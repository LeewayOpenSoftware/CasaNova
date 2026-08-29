package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupSessionRegistry points the registry at a temp file and returns a
// destination directory. Deliberately independent of the upload-service test
// helpers: these tests are about the reaper, not about uploading.
func setupSessionRegistry(t *testing.T) string {
	t.Helper()
	orig := uploadSessionsFile
	uploadSessionsFile = filepath.Join(t.TempDir(), "upload-sessions.json")
	t.Cleanup(func() { uploadSessionsFile = orig })
	return t.TempDir()
}

func ago(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// The decision table
//
// For a v2 session the criterion is the REGISTRY, not the clock: the bitmap
// says whether the file is one rename from landing, and no amount of elapsed
// time makes deleting such a file correct.
// ---------------------------------------------------------------------------

func TestReapUploadSessionVerdicts(t *testing.T) {
	dest := setupSessionRegistry(t)

	// A real per-session tmp, pre-allocated to its declared size.
	tmpFor := func(name string, id string, size int) string {
		p := uploadTmpPath(filepath.Join(dest, name), id)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	victim := filepath.Join(dest, "victim.txt")
	if err := os.WriteFile(victim, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := map[string]*uploadSession{
		"active": {TmpPath: tmpFor("a.bin", "active", 25), TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, false, false}, UpdatedAt: ago(1 * time.Hour)},
		"stale": {TmpPath: tmpFor("b.bin", "stale", 25), TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, false, false}, UpdatedAt: ago(72 * time.Hour)},
		// Every chunk arrived and the tmp is exactly the pre-allocated file the
		// bitmap describes. Ancient, and still not ours to delete.
		"complete": {TmpPath: tmpFor("c.bin", "complete", 25), TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, true, true}, UpdatedAt: ago(30 * 24 * time.Hour)},
		"missing": {TmpPath: uploadTmpPath(filepath.Join(dest, "gone.bin"), "missing"), TotalChunks: 1, ChunkSize: 10, TotalSize: 9,
			Uploaded: []bool{false}, UpdatedAt: ago(1 * time.Hour)},
		// A hand-edited / corrupted entry pointing at somebody's file.
		"suspicious": {TmpPath: victim, TotalChunks: 1, ChunkSize: 10, TotalSize: 9,
			Uploaded: []bool{false}, UpdatedAt: ago(72 * time.Hour)},
		// An unparseable timestamp cannot be resumed against, so it counts as
		// abandoned — but only because the bitmap is incomplete.
		"undated": {TmpPath: tmpFor("d.bin", "undated", 25), TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, false, false}, UpdatedAt: "not a timestamp"},
	}
	want := map[string]UploadSessionVerdict{
		"active":     UploadSessionActive,
		"stale":      UploadSessionStale,
		"complete":   UploadSessionPendingFinalize,
		"missing":    UploadSessionMissingTmp,
		"suspicious": UploadSessionSuspicious,
		"undated":    UploadSessionStale,
	}

	plan := reapUploadSessions(sessions, true)
	if plan.Sessions != len(want) {
		t.Fatalf("plan.Sessions = %d, want %d", plan.Sessions, len(want))
	}
	got := map[string]UploadSessionVerdict{}
	for _, it := range plan.Items {
		got[it.Identifier] = it.Verdict
		if it.Removed {
			t.Errorf("%s is marked Removed on a dry run", it.Identifier)
		}
		if it.Reason == "" {
			t.Errorf("%s has a verdict with no reason; the UI has nothing to show", it.Identifier)
		}
	}
	for id, wantVerdict := range want {
		if got[id] != wantVerdict {
			t.Errorf("%s = %q, want %q", id, got[id], wantVerdict)
		}
	}
	if len(sessions) != len(want) {
		t.Errorf("a dry run mutated the registry: %d entries left of %d", len(sessions), len(want))
	}

	// Now for real.
	rep := reapUploadSessions(sessions, false)
	if rep.Reaped != 4 { // stale, undated, missing, suspicious
		t.Errorf("rep.Reaped = %d, want 4", rep.Reaped)
	}
	if _, ok := sessions["active"]; !ok {
		t.Error("the active session was dropped from the registry")
	}
	if _, ok := sessions["complete"]; !ok {
		t.Error("the completed session was dropped from the registry")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("a suspicious entry must never delete the file it names: %v", err)
	}
	if _, err := os.Lstat(sessions["complete"].TmpPath); err != nil {
		t.Errorf("a completed session's tmp must survive: %v", err)
	}
}

// The data-loss case the age-only reaper had: a session whose chunks all
// arrived but whose rename never ran (crash, or the self-updater's restart)
// used to be deleted once it aged past the resume window — destroying a file
// the user's client had already reported at 100%. It must instead survive and
// be finalized on the next start.
func TestCompletedSessionIsFinalizedNotReaped(t *testing.T) {
	dest := setupSessionRegistry(t)
	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC") // 25 bytes
	id := "25-crashfinal.bin"
	final := filepath.Join(dest, "crashfinal.bin")
	tmp := uploadTmpPath(final, id)
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatal(err)
	}
	persistUploadSessions(map[string]*uploadSession{
		// Root is what a session created by this build records; rehydration
		// refuses to rename without it (see rehydrateDestination).
		id: {TmpPath: tmp, Root: dest, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, true, true}, UpdatedAt: ago(30 * 24 * time.Hour)},
	})

	defer NewFileUploadService().Close()

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("a completed upload was destroyed instead of finalized: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("finalized content = %q, want %q", got, content)
	}
}

// ---------------------------------------------------------------------------
// On-demand entry points
// ---------------------------------------------------------------------------

// withLiveService registers svc for the duration of the test. The pointer is
// process-global, so it has to be put back.
func withLiveService(t *testing.T, svc *FileUploadService) {
	t.Helper()
	prev := liveUploadService.Load()
	liveUploadService.Store(svc)
	t.Cleanup(func() { liveUploadService.Store(prev) })
}

// stageLiveSession injects one session into a running service exactly as an
// interrupted upload would leave it: registry entry, in-RAM bitmap, and a
// pre-allocated tmp on disk.
func stageLiveSession(t *testing.T, svc *FileUploadService, dest, id, name string, updatedAt string) string {
	t.Helper()
	tmp := uploadTmpPath(filepath.Join(dest, name), id)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(25); err != nil {
		t.Fatal(err)
	}
	f.Close()

	svc.lock.Lock()
	svc.sessions[id] = &uploadSession{TmpPath: tmp, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
		Uploaded: []bool{true, false, false}, UpdatedAt: updatedAt}
	svc.uploadStatus.Store(id, &FileInfo{init: true, uploaded: []bool{true, false, false}, uploadedChunkNum: 1})
	persistUploadSessions(svc.sessions)
	svc.lock.Unlock()
	return tmp
}

func TestReapUploadSessionsRefusesWithoutTheLiveService(t *testing.T) {
	dest := setupSessionRegistry(t)
	withLiveService(t, nil)

	svc := NewFileUploadService()
	defer svc.Close()
	tmp := stageLiveSession(t, svc, dest, "orphan", "orphan.bin", ago(72*time.Hour))

	// Deleting a .tmp from outside the service means deleting a file whose
	// session is still live in another goroutine's map. We refuse instead.
	if _, err := ReapUploadSessions(); !errors.Is(err, ErrUploadServiceUnavailable) {
		t.Fatalf("ReapUploadSessions err = %v, want ErrUploadServiceUnavailable", err)
	}
	if _, err := os.Lstat(tmp); err != nil {
		t.Fatalf("nothing may be deleted when the reap is refused: %v", err)
	}

	// A scan still answers, from the registry file, and says it is not live.
	scan := ScanUploadSessions()
	if scan.Live {
		t.Error("a scan without the running service must not claim to be live")
	}
	if scan.Sessions != 1 || len(scan.Items) != 1 || scan.Items[0].Verdict != UploadSessionStale {
		t.Errorf("scan = %+v, want one stale session", scan)
	}
	if _, err := os.Lstat(tmp); err != nil {
		t.Fatalf("a scan must not delete anything: %v", err)
	}
}

func TestReapUploadSessionsUnderTheServiceLock(t *testing.T) {
	dest := setupSessionRegistry(t)
	withLiveService(t, nil)

	svc := NewFileUploadService()
	defer svc.Close()
	stale := stageLiveSession(t, svc, dest, "stale", "stale.bin", ago(72*time.Hour))
	live := stageLiveSession(t, svc, dest, "live", "live.bin", ago(1*time.Hour))
	RegisterUploadService(svc)

	scan := ScanUploadSessions()
	if !scan.Live {
		t.Error("a scan with the running service registered must be live")
	}
	if scan.Reaped != 1 || scan.Sessions != 2 {
		t.Errorf("scan = %d reapable of %d, want 1 of 2", scan.Reaped, scan.Sessions)
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("a scan must not delete anything: %v", err)
	}

	rep, err := ReapUploadSessions()
	if err != nil {
		t.Fatalf("ReapUploadSessions: %v", err)
	}
	if !rep.Live || rep.Reaped != 1 {
		t.Errorf("rep = %+v, want one reaped session from the live service", rep)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale tmp should have been reaped (err=%v)", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Errorf("a session inside the resume window must survive: %v", err)
	}
	// The in-RAM resume state has to go with it, or TestChunk keeps answering
	// "present" for a file that no longer exists.
	if err := svc.TestChunk(nil, "stale", 1); err == nil {
		t.Error("a reaped session must not stay resumable")
	}
	if err := svc.TestChunk(nil, "live", 1); err != nil {
		t.Errorf("the surviving session must stay resumable: %v", err)
	}
	// And the registry on disk has to agree with the map.
	raw, err := os.ReadFile(uploadSessionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"stale"`)) {
		t.Error("the reaped session is still in the persisted registry")
	}
}

// The .tmp is pre-allocated to the final size with Truncate, so a session that
// uploaded 10 bytes of a 64 MiB file is holding 10 bytes, not 64 MiB. Reporting
// the apparent size would promise space the run cannot free — rule 14.
func TestReapReportsAllocatedBytesNotApparentSize(t *testing.T) {
	dest := setupSessionRegistry(t)
	const apparent = 64 << 20

	id := "sparse"
	tmp := uploadTmpPath(filepath.Join(dest, "sparse.bin"), id)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(apparent); err != nil {
		t.Fatal(err)
	}
	f.Close()

	plan := reapUploadSessions(map[string]*uploadSession{
		id: {TmpPath: tmp, TotalChunks: 8, ChunkSize: 8 << 20, TotalSize: apparent,
			Uploaded: make([]bool, 8), UpdatedAt: ago(72 * time.Hour)},
	}, true)

	if len(plan.Items) != 1 {
		t.Fatalf("plan = %+v, want one item", plan)
	}
	it := plan.Items[0]
	if it.ApparentBytes != apparent {
		t.Errorf("ApparentBytes = %d, want %d", it.ApparentBytes, apparent)
	}
	if it.Bytes > it.ApparentBytes {
		t.Fatalf("reported %d reclaimable bytes for a %d-byte file", it.Bytes, it.ApparentBytes)
	}
	if plan.Bytes != it.Bytes {
		t.Errorf("plan.Bytes = %d, want the item's %d", plan.Bytes, it.Bytes)
	}
	if it.Bytes == it.ApparentBytes {
		t.Skip("this filesystem allocates sparse files in full; nothing to compare")
	}
}
