package service

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupUploadEnv(t *testing.T) string {
	t.Helper()
	orig := uploadSessionsFile
	uploadSessionsFile = filepath.Join(t.TempDir(), "upload-sessions.json")
	t.Cleanup(func() { uploadSessionsFile = orig })
	return t.TempDir() // destination dir for uploads
}

// chunkHeader builds a real *multipart.FileHeader carrying data, the same way
// echo's FormFile would produce it.
func chunkHeader(t *testing.T, data []byte) *multipart.FileHeader {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "chunk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	req := httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	_, fh, err := req.FormFile("file")
	if err != nil {
		t.Fatal(err)
	}
	return fh
}

func TestValidateChunkParams(t *testing.T) {
	cases := []struct {
		name                                                             string
		chunkNumber, chunkSize, currentChunkSize, totalChunks, totalSize int64
		ok                                                               bool
	}{
		{"valid middle chunk", 1, 10, 10, 3, 25, true},
		{"valid last chunk remainder", 3, 10, 5, 3, 25, true},
		{"wrong middle size", 1, 10, 5, 3, 25, false},
		{"oversized last chunk", 3, 10, 10, 3, 25, false},
		{"totalSize exceeds grid", 1, 10, 10, 3, 31, false},
		{"totalSize fits fewer chunks", 1, 10, 10, 3, 20, false},
		{"zero chunkNumber", 0, 10, 10, 3, 25, false},
	}
	for _, c := range cases {
		err := validateChunkParams(c.chunkNumber, c.chunkSize, c.currentChunkSize, c.totalChunks, c.totalSize)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestChunkedUploadAssemblesFile(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()
	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC") // 25 bytes: 10+10+5
	id := "25-test.bin"

	for i := 0; i < 3; i++ {
		start := i * 10
		end := start + 10
		if end > len(content) {
			end = len(content)
		}
		part := content[start:end]
		err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(len(part)), 3, 25,
			id, "test.bin", "test.bin", chunkHeader(t, part))
		if err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "test.bin"))
	if err != nil {
		t.Fatalf("assembled file missing: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("assembled = %q, want %q", got, content)
	}
	if _, err := os.Stat(filepath.Join(dest, "test.bin.tmp")); !os.IsNotExist(err) {
		t.Fatal(".tmp must be renamed away on completion")
	}
	// Registry must be empty after completion.
	if raw, err := os.ReadFile(uploadSessionsFile); err == nil && bytes.Contains(raw, []byte(id)) {
		t.Fatal("session must be removed from the registry on completion")
	}
}

func TestChunkBodySizeMismatchRejected(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()
	// Declares 10 bytes but body carries 12 — must be rejected before writing.
	err := svc.UploadFile(nil, dest, 1, 10, 10, 2, 15,
		"15-x.bin", "x.bin", "x.bin", chunkHeader(t, bytes.Repeat([]byte("z"), 12)))
	if err == nil {
		t.Fatal("oversized chunk body must be rejected")
	}
}

func TestUploadResumeSurvivesRestart(t *testing.T) {
	dest := setupUploadEnv(t)
	svc1 := NewFileUploadService()
	defer svc1.Close()
	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC")
	id := "25-resume.bin"

	// Upload chunks 1 and 2, then "restart" the service.
	for i := 0; i < 2; i++ {
		part := content[i*10 : (i+1)*10]
		if err := svc1.UploadFile(nil, dest, int64(i+1), 10, 10, 3, 25,
			id, "resume.bin", "resume.bin", chunkHeader(t, part)); err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
	}

	svc2 := NewFileUploadService() // fresh instance, same registry file
	defer svc2.Close()
	// Chunks 1-2 must be reported present, 3 absent.
	if err := svc2.TestChunk(nil, id, 1); err != nil {
		t.Fatalf("chunk 1 should be present after restart: %v", err)
	}
	if err := svc2.TestChunk(nil, id, 3); err == nil {
		t.Fatal("chunk 3 should be absent after restart")
	}
	// Complete the upload on the new instance.
	if err := svc2.UploadFile(nil, dest, 3, 10, 5, 3, 25,
		id, "resume.bin", "resume.bin", chunkHeader(t, content[20:])); err != nil {
		t.Fatalf("final chunk after restart: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "resume.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("assembled after restart = %q err=%v, want %q", got, err, content)
	}
}

func TestStaleSessionsReaped(t *testing.T) {
	dest := setupUploadEnv(t)
	tmp := uploadTmpPath(filepath.Join(dest, "old.bin"), "stale-id")
	if err := os.WriteFile(tmp, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	sessions := map[string]*uploadSession{
		"stale-id": {TmpPath: tmp, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, false, false}, UpdatedAt: old},
	}
	persistUploadSessions(sessions)

	svc := NewFileUploadService()
	defer svc.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("stale .tmp must be reaped on service start")
	}
	if err := svc.TestChunk(nil, "stale-id", 1); err == nil {
		t.Fatal("stale session must not be resumable")
	}
}

func TestSuspiciousTmpPathDroppedNotDeleted(t *testing.T) {
	dest := setupUploadEnv(t)
	// A registry entry pointing at an arbitrary file (wrong shape) must be
	// DROPPED without deleting the file — the reaper is not an arbitrary
	// unlink primitive.
	victim := filepath.Join(dest, "victim.txt")
	if err := os.WriteFile(victim, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	persistUploadSessions(map[string]*uploadSession{
		"evil": {TmpPath: victim, TotalChunks: 1, ChunkSize: 10, TotalSize: 9,
			Uploaded: []bool{false}, UpdatedAt: old},
	})

	defer NewFileUploadService().Close()
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim file must survive a suspicious registry entry: %v", err)
	}
}

func TestFreshSessionSurvivesReaping(t *testing.T) {
	dest := setupUploadEnv(t)
	id := fmt.Sprintf("%d-fresh.bin", 25)
	tmp := uploadTmpPath(filepath.Join(dest, "fresh.bin"), id)
	// sessionTmpValid requires the exact pre-allocated size.
	if err := os.WriteFile(tmp, bytes.Repeat([]byte("x"), 25), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := map[string]*uploadSession{
		id: {TmpPath: tmp, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, true, false}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)},
	}
	persistUploadSessions(sessions)

	svc := NewFileUploadService()
	defer svc.Close()
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("fresh .tmp must survive: %v", err)
	}
	if err := svc.TestChunk(nil, id, 2); err != nil {
		t.Fatalf("fresh session chunk 2 should be present: %v", err)
	}
}

func TestVanishedTmpForcesSessionReset(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()
	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC")
	id := "25-vanish.bin"

	// Upload 2 of 3 chunks, then delete the tmp out from under the session
	// (e.g. the user removed the visible temp file in the browser).
	for i := 0; i < 2; i++ {
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, 10, 3, 25,
			id, "vanish.bin", "vanish.bin", chunkHeader(t, content[i*10:(i+1)*10])); err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
	}
	tmp := uploadTmpPath(filepath.Join(dest, "vanish.bin"), id)
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}

	// Preflight must now report every chunk absent (bitmap no longer backed
	// by reality) …
	if err := svc.TestChunk(nil, id, 1); err == nil {
		t.Fatal("TestChunk must not report chunks present after the tmp vanished")
	}
	// … and the next POST must refuse with ErrSessionReset instead of
	// recreating an empty tmp and assembling zeros.
	err := svc.UploadFile(nil, dest, 3, 10, 5, 3, 25,
		id, "vanish.bin", "vanish.bin", chunkHeader(t, content[20:]))
	if !errors.Is(err, ErrSessionReset) {
		t.Fatalf("want ErrSessionReset, got %v", err)
	}

	// The client restarts from scratch — the fresh session must work.
	for i := 0; i < 3; i++ {
		part := content[i*10 : min(25, (i+1)*10)]
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(len(part)), 3, 25,
			id, "vanish.bin", "vanish.bin", chunkHeader(t, part)); err != nil {
			t.Fatalf("restart chunk %d: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "vanish.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("assembled after reset = %q err=%v", got, err)
	}
}

func TestConcurrentSessionsSameDestDoNotShareTmp(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()
	// Two different files, same destination name, distinct identifiers —
	// first chunks of both in flight: they must land in DIFFERENT tmps.
	a := bytes.Repeat([]byte("A"), 25)
	b := bytes.Repeat([]byte("B"), 15)
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25,
		"idA", "clash.bin", "clash.bin", chunkHeader(t, a[:10])); err != nil {
		t.Fatal(err)
	}
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 2, 15,
		"idB", "clash.bin", "clash.bin", chunkHeader(t, b[:10])); err != nil {
		t.Fatal(err)
	}
	tmpA := uploadTmpPath(filepath.Join(dest, "clash.bin"), "idA")
	tmpB := uploadTmpPath(filepath.Join(dest, "clash.bin"), "idB")
	if tmpA == tmpB {
		t.Fatal("per-session tmp paths must differ")
	}
	stA, errA := os.Stat(tmpA)
	stB, errB := os.Stat(tmpB)
	if errA != nil || errB != nil {
		t.Fatalf("both tmps must exist: %v %v", errA, errB)
	}
	if stA.Size() != 25 || stB.Size() != 15 {
		t.Fatalf("preallocation clobbered: A=%d (want 25) B=%d (want 15)", stA.Size(), stB.Size())
	}
}

// A session fully uploaded but whose SafeRename never ran (crash between the
// last bitmap write and the rename) must be finalized on service restart —
// otherwise the client preflights all chunks, gets 200, and reports success
// for a file that does not exist. Regression for the rehydrate-finalize fix.
func TestRehydrateFinalizesCompletedUpload(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()
	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC") // 25B = 10+10+5
	id := "25-crashfinal.bin"

	// Upload all 3 chunks.
	for i := 0; i < 3; i++ {
		part := content[i*10 : min(25, (i+1)*10)]
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(len(part)), 3, 25,
			id, "crashfinal.bin", "crashfinal.bin", chunkHeader(t, part)); err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
	}
	final := filepath.Join(dest, "crashfinal.bin")
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("normal path should have produced the file: %v", err)
	}

	// Simulate the crash-before-rename state: recreate the tmp at full size,
	// mark every chunk uploaded in the persisted registry, remove the dest.
	os.Remove(final)
	tmp := uploadTmpPath(final, id)
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatal(err)
	}
	persistUploadSessions(map[string]*uploadSession{
		// Root is what a session created by this build records; rehydration
		// refuses to rename without it (see rehydrateDestination).
		id: {TmpPath: tmp, Root: dest, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded: []bool{true, true, true}, UpdatedAt: nowRFC()},
	})

	// A fresh service must finalize on rehydrate.
	defer NewFileUploadService().Close()
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("rehydrate must finalize the completed upload: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("finalized content = %q, want %q", got, content)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("tmp must be renamed away after finalize")
	}
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }
