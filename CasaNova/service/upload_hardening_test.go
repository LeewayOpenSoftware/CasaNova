package service

// upload_hardening_test.go — regression tests for the two upload findings:
//
//	PT-01      path traversal through a SYMLINKED directory component planted
//	           inside the allowed upload area.
//	UPLOAD-01  disk/memory exhaustion: unbounded sessions, a bitmap persisted
//	           as indented JSON booleans, and a reaper nothing ever called.
//
// Every test here asserts the INVARIANT (nothing lands outside the root; the
// daemon does not write more than it is sent; abandoned state goes away on its
// own), not that some function was called, and every wait is bounded.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// escapeTarget returns a writable directory that lives under one of the
// canonical blocked prefixes (utils.BlockedSystemPrefixes contains "/run", and
// /run/user/<uid> belongs to the user running the test). Having a writable
// target is what makes the escape OBSERVABLE: with the fix reverted the upload
// really does create a file in there. Returns "" when the box has no such
// directory, in which case the caller falls back to asserting the refusal only.
func escapeTarget(t *testing.T) string {
	t.Helper()
	base := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return ""
	}
	dir, err := os.MkdirTemp(base, "casanova-upload-escape-")
	if err != nil {
		return ""
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// PT-01. relativePath is only checked lexically (no "..", not absolute, stays
// under path), which says nothing about a directory component that IS a
// symlink. Any container with a bind-mount of the data area can plant one.
func TestUploadRefusesSymlinkedDirectoryComponent(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	target := escapeTarget(t)
	observable := target != ""
	if !observable {
		target = "/etc"
	}
	if err := os.Symlink(target, filepath.Join(dest, "link")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	body := []byte("pwned")
	err := svc.UploadFile(nil, dest, 1, 16, int64(len(body)), 1, int64(len(body)),
		"5-pwned.txt", "link/pwned.txt", "pwned.txt", chunkHeader(t, body))
	if err == nil {
		t.Fatal("upload through a symlinked directory component must be refused")
	}
	if !errors.Is(err, ErrUploadPathRejected) {
		t.Fatalf("want ErrUploadPathRejected, got %v", err)
	}

	// The invariant: no write of any kind escaped the upload root — not the
	// final file, not the pre-allocated .tmp, not a created directory.
	if observable {
		entries, rerr := os.ReadDir(target)
		if rerr != nil {
			t.Fatalf("read escape target: %v", rerr)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("upload escaped the data area: %s now contains %v", target, names)
		}
	}
}

// The same guard must refuse a destination that lexically lands in a blocked
// prefix even without a symlink (path arrives sanitized from the route, but the
// service is also called directly).
func TestUploadRefusesBlockedDestination(t *testing.T) {
	setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	body := []byte("x")
	err := svc.UploadFile(nil, "/etc/cron.d", 1, 16, 1, 1, 1,
		"1-x", "x", "x", chunkHeader(t, body))
	if !errors.Is(err, ErrUploadPathRejected) {
		t.Fatalf("want ErrUploadPathRejected for /etc destination, got %v", err)
	}
}

// LESSON 1 GUARD: the traversal fix must not cost the feature it protects. A
// normal folder upload creates its directories and assembles its file.
func TestUploadStillCreatesNestedFolders(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC") // 25 bytes: 10+10+5
	id := "25-nested.bin"
	for i := 0; i < 3; i++ {
		start, end := i*10, i*10+10
		if end > len(content) {
			end = len(content)
		}
		part := content[start:end]
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(len(part)), 3, 25,
			id, "sub/deep/nested.bin", "nested.bin", chunkHeader(t, part)); err != nil {
			t.Fatalf("chunk %d of a legitimate folder upload: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "sub", "deep", "nested.bin"))
	if err != nil {
		t.Fatalf("nested upload did not land: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("assembled = %q, want %q", got, content)
	}
}

// floodSessions fills the registry with `n` sessions that the reaper will keep:
// fresh timestamp, incomplete bitmap, and a real .tmp on disk (an entry whose
// .tmp is missing is dropped on sight, so fake paths would not hold the cap).
func floodSessions(t *testing.T, svc *FileUploadService, dir string, n int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	svc.lock.Lock()
	defer svc.lock.Unlock()
	for i := 0; i < n; i++ {
		tmp := filepath.Join(dir, fmt.Sprintf("flood%d.bin.upload-000000%02x.tmp", i, i%256))
		if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
			t.Fatalf("flood tmp: %v", err)
		}
		svc.sessions[fmt.Sprintf("flood-%d", i)] = &uploadSession{
			TmpPath: tmp, TotalChunks: 2, ChunkSize: 1, TotalSize: 2,
			Uploaded: make([]bool, 2), UpdatedAt: now,
		}
	}
}

// UPLOAD-01 (b): a full registry refuses NEW sessions — and, the part that
// makes the cap safe to have at all, never disturbs an upload in flight.
func TestSessionCapRefusesNewButNeverBlocksLiveUpload(t *testing.T) {
	dest := setupUploadEnv(t)
	flood := t.TempDir()
	svc := NewFileUploadService()
	defer svc.Close()

	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC")
	id := "25-live.bin"
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25, id, "live.bin", "live.bin",
		chunkHeader(t, content[0:10])); err != nil {
		t.Fatalf("first chunk of the live upload: %v", err)
	}

	floodSessions(t, svc, flood, MaxLiveUploadSessions)

	// A new session is refused, with the typed error the route turns into 429.
	err := svc.UploadFile(nil, dest, 1, 10, 5, 1, 5, "5-new.bin", "new.bin", "new.bin",
		chunkHeader(t, []byte("hello")))
	if !errors.Is(err, ErrTooManyUploadSessions) {
		t.Fatalf("want ErrTooManyUploadSessions once the registry is full, got %v", err)
	}

	// The upload that was already running finishes untouched.
	for i := 1; i < 3; i++ {
		start, end := i*10, i*10+10
		if end > len(content) {
			end = len(content)
		}
		part := content[start:end]
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(len(part)), 3, 25,
			id, "live.bin", "live.bin", chunkHeader(t, part)); err != nil {
			t.Fatalf("live upload chunk %d was blocked by the cap: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "live.bin"))
	if err != nil {
		t.Fatalf("live upload did not land: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("assembled = %q, want %q", got, content)
	}
}

// UPLOAD-01 (b): the aggregate chunk budget. One session may declare
// MaxTotalChunks, but the SUM of live bitmaps is what costs RAM and makes every
// registry write expensive.
func TestChunkBudgetRefusesNewSession(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	tmp := filepath.Join(dest, "huge.bin.upload-0000000f.tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.lock.Lock()
	svc.sessions["huge"] = &uploadSession{
		TmpPath: tmp, TotalChunks: MaxLiveUploadChunks, ChunkSize: 1 << 20,
		TotalSize: MaxLiveUploadChunks << 20, Uploaded: nil,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	svc.lock.Unlock()

	err := svc.UploadFile(nil, dest, 1, 10, 5, 1, 5, "5-more.bin", "more.bin", "more.bin",
		chunkHeader(t, []byte("hello")))
	if !errors.Is(err, ErrTooManyUploadSessions) {
		t.Fatalf("want ErrTooManyUploadSessions once the chunk budget is spent, got %v", err)
	}
}

// UPLOAD-01 (b): the resume key is attacker-chosen, is a map key, a registry
// key and part of the .tmp name, and is retained for uploadSessionMaxAge.
func TestOversizedIdentifierRejected(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	err := svc.UploadFile(nil, dest, 1, 10, 5, 1, 5,
		strings.Repeat("x", MaxUploadIdentifierLen+1), "big.bin", "big.bin",
		chunkHeader(t, []byte("hello")))
	if err == nil {
		t.Fatal("an oversized identifier must be refused")
	}
	svc.lock.RLock()
	n := len(svc.sessions)
	svc.lock.RUnlock()
	if n != 0 {
		t.Fatalf("rejected identifier still created %d session(s)", n)
	}
}

// UPLOAD-01 (a): the registry write must be proportional to the upload, not to
// the declared chunk count. With the old indented []bool a 1e6-chunk session
// wrote ~13 MB of JSON per chunk — for a chunk that may carry one byte.
func TestRegistryStaysCompactForHugeBitmaps(t *testing.T) {
	setupUploadEnv(t)

	n := int(MaxTotalChunks)
	uploaded := make([]bool, n)
	for i := 0; i < n; i += 3 {
		uploaded[i] = true
	}
	persistUploadSessions(map[string]*uploadSession{
		"1000000-huge.bin": {
			TmpPath: "/DATA/huge.bin.upload-deadbeef.tmp", TotalChunks: int64(n),
			ChunkSize: 1 << 20, TotalSize: int64(n) << 20, Uploaded: uploaded,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		},
	})

	st, err := os.Stat(uploadSessionsFile)
	if err != nil {
		t.Fatalf("registry not written: %v", err)
	}
	// Packed + base64 is ~167 KB for a million chunks. 512 KB is a generous
	// ceiling that still fails loudly if the boolean array (or the indenting)
	// ever comes back.
	const maxRegistry = 512 << 10
	if st.Size() > maxRegistry {
		t.Fatalf("registry is %d bytes for %d chunks (limit %d): the per-chunk write amplification is back",
			st.Size(), n, maxRegistry)
	}

	// Compact must not mean lossy: resume correctness depends on every bit.
	back := readUploadSessions()["1000000-huge.bin"]
	if back == nil {
		t.Fatal("session did not survive the round trip")
	}
	if len(back.Uploaded) != n {
		t.Fatalf("bitmap length %d, want %d", len(back.Uploaded), n)
	}
	for i := range uploaded {
		if back.Uploaded[i] != uploaded[i] {
			t.Fatalf("bit %d = %v, want %v", i, back.Uploaded[i], uploaded[i])
		}
	}
}

// LESSON 1 GUARD for the encoding change: a box upgraded mid-transfer has a
// registry full of the legacy boolean arrays. Refusing them would make the
// whole registry unparseable — every resumable upload lost, every .tmp
// orphaned. It must still load.
func TestLegacyRegistryArrayStillLoads(t *testing.T) {
	setupUploadEnv(t)

	legacy := `{"25-old.bin":{"tmpPath":"/DATA/old.bin.upload-deadbeef.tmp","totalChunks":3,` +
		`"chunkSize":10,"totalSize":25,"uploaded":[true,false,true],"updatedAt":"2026-08-16T00:00:00Z"}}`
	if err := os.WriteFile(uploadSessionsFile, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	sess := readUploadSessions()["25-old.bin"]
	if sess == nil {
		t.Fatal("legacy registry entry was dropped: an upgrade would lose every in-flight upload")
	}
	want := []bool{true, false, true}
	if len(sess.Uploaded) != len(want) {
		t.Fatalf("bitmap length %d, want %d", len(sess.Uploaded), len(want))
	}
	for i := range want {
		if sess.Uploaded[i] != want[i] {
			t.Fatalf("bit %d = %v, want %v", i, sess.Uploaded[i], want[i])
		}
	}
}

// UPLOAD-01 (a): a chunk that changes no bits must not rewrite the registry.
// Re-POSTing one already-stored chunk in a loop was an unbounded write
// amplifier with no upload behind it.
func TestDuplicateChunkDoesNotRewriteRegistry(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC")
	id := "25-dup.bin"
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25, id, "dup.bin", "dup.bin",
		chunkHeader(t, content[0:10])); err != nil {
		t.Fatalf("first chunk: %v", err)
	}

	// Any rewrite replaces the sentinel with JSON.
	const sentinel = "SENTINEL-NOT-REWRITTEN"
	if err := os.WriteFile(uploadSessionsFile, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25, id, "dup.bin", "dup.bin",
		chunkHeader(t, content[0:10])); err != nil {
		t.Fatalf("a duplicate chunk must still succeed (idempotent retry): %v", err)
	}
	raw, err := os.ReadFile(uploadSessionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatalf("a duplicate chunk rewrote the whole registry (%d bytes) while storing nothing", len(raw))
	}

	// And the upload still completes normally afterwards.
	for i := 1; i < 3; i++ {
		start, end := i*10, i*10+10
		if end > len(content) {
			end = len(content)
		}
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(end-start), 3, 25,
			id, "dup.bin", "dup.bin", chunkHeader(t, content[start:end])); err != nil {
			t.Fatalf("chunk %d after the duplicate: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "dup.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("file after a duplicate retry = %q (err %v), want %q", got, err, content)
	}
}

// UPLOAD-01 (c): the reaper was written, exported and never driven — nothing
// called it outside service start and the maintenance screen, so
// uploadSessionMaxAge meant nothing and abandoned sessions lived forever. This
// asserts the WIRING: an abandoned session disappears with nobody asking.
func TestBackgroundReaperRunsWithoutBeingAsked(t *testing.T) {
	dest := setupUploadEnv(t)
	uploadReapIntervalOverride.Store(int64(20 * time.Millisecond))
	t.Cleanup(func() { uploadReapIntervalOverride.Store(0) })

	svc := NewFileUploadService()
	defer svc.Close()

	const id = "25-abandoned.bin"
	tmp := filepath.Join(dest, "abandoned.bin.upload-0000beef.tmp")
	if err := os.WriteFile(tmp, bytes.Repeat([]byte("x"), 25), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.lock.Lock()
	svc.sessions[id] = &uploadSession{
		TmpPath: tmp, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
		Uploaded:  make([]bool, 3),
		UpdatedAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
	}
	svc.lock.Unlock()

	// Bounded wait — a test that hangs is a test that gets deleted.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(tmp); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned .tmp is still there: nothing drives the reaper, so uploadSessionMaxAge is decorative")
		}
		time.Sleep(10 * time.Millisecond)
	}
	svc.lock.RLock()
	_, still := svc.sessions[id]
	svc.lock.RUnlock()
	if still {
		t.Fatal("the .tmp was reaped but its registry entry survived")
	}
}

// A live session must NOT be reaped by the periodic pass — the ticker exists to
// clean up abandonment, not to cancel a slow transfer.
func TestBackgroundReaperLeavesFreshSessionsAlone(t *testing.T) {
	dest := setupUploadEnv(t)
	uploadReapIntervalOverride.Store(int64(20 * time.Millisecond))
	t.Cleanup(func() { uploadReapIntervalOverride.Store(0) })

	svc := NewFileUploadService()
	defer svc.Close()

	content := []byte("AAAAAAAAAABBBBBBBBBBCCCCC")
	id := "25-slow.bin"
	if err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25, id, "slow.bin", "slow.bin",
		chunkHeader(t, content[0:10])); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	// Several ticks go by while the client is quiet.
	time.Sleep(300 * time.Millisecond)

	for i := 1; i < 3; i++ {
		start, end := i*10, i*10+10
		if end > len(content) {
			end = len(content)
		}
		if err := svc.UploadFile(nil, dest, int64(i+1), 10, int64(end-start), 3, 25,
			id, "slow.bin", "slow.bin", chunkHeader(t, content[start:end])); err != nil {
			t.Fatalf("the reaper broke a live upload at chunk %d: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "slow.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("slow upload = %q (err %v), want %q", got, err, content)
	}
}
