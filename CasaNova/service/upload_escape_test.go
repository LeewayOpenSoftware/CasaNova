package service

// upload_escape_test.go — regressions for the three defects the adversarial
// verification found IN THE PREVIOUS PATCH, not in the original code:
//
//	(1) the registry-driven paths (rehydrate finalize, resume-mismatch cleanup)
//	    skipped every guard the patch added, so installing the patch on a box
//	    whose registry had been poisoned by the vulnerable build would have made
//	    the FIX perform the escape on its first start;
//	(2) the delivered guarantee was "the write is not inside
//	    utils.BlockedSystemPrefixes", not "the write stays inside the upload
//	    root" — /home, /tmp and / are not on that list;
//	(3) the reaper's period was read from a process-global INSIDE the loop
//	    goroutine, so a service could inherit another test's 20 ms tick.
//
// Every test asserts the invariant (nothing lands or disappears outside the
// upload root; the period is a property of the service), every wait is bounded,
// and none of them shares mutable process state with another test beyond the
// two globals they restore themselves.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
)

// ---------------------------------------------------------------------------
// (1) registry-driven paths
// ---------------------------------------------------------------------------

// The deploy hazard itself, made observable: a session that was recorded by the
// vulnerable build with a temp path OUTSIDE any upload area, already complete,
// so the very first NewFileUploadService would rename it into place. The
// directory is a writable temp dir rather than /etc precisely so the escape is
// visible without root — the mechanism is identical.
func TestPrePlantedRegistryEntryIsNotFinalizedOnStart(t *testing.T) {
	setupUploadEnv(t)
	outside := t.TempDir()

	const id = "1-escape"
	tmp := filepath.Join(outside, "escape.upload-deadbeef.tmp")
	landed := filepath.Join(outside, "escape")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	persistUploadSessions(map[string]*uploadSession{
		// No Root: exactly what the vulnerable build wrote.
		id: {TmpPath: tmp, TotalChunks: 1, ChunkSize: 1, TotalSize: 1,
			Uploaded: []bool{true}, UpdatedAt: nowRFC()},
	})

	svc := NewFileUploadService()
	defer svc.Close()

	if _, err := os.Lstat(landed); err == nil {
		t.Fatalf("the patched build executed a pre-planted escape: %s was created on start", landed)
	}
	// …and it must not have "cleaned up" a file it cannot prove it owns either.
	if _, err := os.Lstat(tmp); err != nil {
		t.Fatalf("a session it refused to finalize had its file deleted anyway: %v", err)
	}
	svc.lock.RLock()
	_, still := svc.sessions[id]
	svc.lock.RUnlock()
	if still {
		t.Fatal("an unverifiable registry entry survived the migration; it would be retried on every start")
	}
}

// The decision table behind that, unit-tested so it holds at any privilege
// level and for the shapes a temp dir cannot reproduce (/etc).
func TestRehydrateDestinationRefusesEveryUnprovablePath(t *testing.T) {
	dest := setupUploadEnv(t)
	good := filepath.Join(dest, "ok.bin.upload-deadbeef.tmp")
	outside := t.TempDir()

	bad := []struct {
		name string
		sess uploadSession
	}{
		{"blocked system prefix (the CN escape that was proven live pre-patch)",
			uploadSession{TmpPath: "/etc/cron.d/x.upload-deadbeef.tmp", Root: dest}},
		{"no recorded upload root — written by the vulnerable build",
			uploadSession{TmpPath: good}},
		{"temp file outside the recorded root",
			uploadSession{TmpPath: filepath.Join(outside, "x.upload-deadbeef.tmp"), Root: dest}},
		{"recorded root is itself a blocked prefix",
			uploadSession{TmpPath: good, Root: "/etc"}},
		{"not one of our temp files at all",
			uploadSession{TmpPath: filepath.Join(dest, "victim.txt"), Root: dest}},
		{"relative path", uploadSession{TmpPath: "x.upload-deadbeef.tmp", Root: dest}},
	}
	for _, c := range bad {
		sess := c.sess
		if got, err := rehydrateDestination(&sess); err == nil {
			t.Errorf("%s: rehydrate accepted it and would rename into %s", c.name, got)
		}
	}

	// The positive case must still work, or crash recovery is dead.
	ok := uploadSession{TmpPath: good, Root: dest}
	got, err := rehydrateDestination(&ok)
	if err != nil {
		t.Fatalf("a well-formed session must still be finalizable: %v", err)
	}
	if want := filepath.Join(dest, "ok.bin"); got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
}

// The removal gate itself. isUploadTmpPath only checks the SHAPE, and
// "/etc/cron.d/x.upload-deadbeef.tmp" is a perfectly well-formed shape — so the
// reaper would happily unlink a registry path no live request could ever
// produce. sessionTmpAcceptable adds the sanitiser, and this proves it is
// load-bearing rather than ornamental.
//
// utils.BlockedSystemPrefixes is extended with a writable temp dir for the
// duration, which is the only way to make the deletion OBSERVABLE without
// running as root (the same trick pkg/utils/path_test.go uses). Restored on
// exit; the tests in this package are sequential and none is t.Parallel.
func TestReaperNeverDeletesAPathTheSanitizerRejects(t *testing.T) {
	setupUploadEnv(t)
	blocked := t.TempDir()
	prev := utils.BlockedSystemPrefixes
	utils.BlockedSystemPrefixes = append(append([]string{}, prev...), blocked)
	t.Cleanup(func() { utils.BlockedSystemPrefixes = prev })

	tmp := filepath.Join(blocked, "planted.upload-deadbeef.tmp")
	if err := os.WriteFile(tmp, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := map[string]*uploadSession{
		"planted": {TmpPath: tmp, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
			Uploaded:  make([]bool, 3),
			UpdatedAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)},
	}

	rep := reapUploadSessions(sessions, false)

	if _, err := os.Lstat(tmp); err != nil {
		t.Fatalf("the reaper deleted a path the upload sanitizer would refuse: %v", err)
	}
	if _, still := sessions["planted"]; still {
		t.Fatal("the unusable entry must be dropped from the registry")
	}
	if len(rep.Items) != 1 || rep.Items[0].Verdict != UploadSessionSuspicious {
		t.Fatalf("verdict = %+v, want a single %q", rep.Items, UploadSessionSuspicious)
	}
}

// The second unguarded os.Remove: a resume whose recorded state no longer
// matches used to unlink sess.TmpPath verbatim, so a registry entry naming a
// user's file turned a 409 into an arbitrary delete.
func TestResumeMismatchNeverDeletesAPathThatIsNotOurs(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	victim := filepath.Join(dest, "important-user-file.txt")
	if err := os.WriteFile(victim, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	const id = "25-victim.bin"
	svc.lock.Lock()
	svc.uploadStatus.Store(id, &FileInfo{init: true, uploaded: make([]bool, 3)})
	svc.sessions[id] = &uploadSession{
		TmpPath: victim, Root: dest, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
		Uploaded: make([]bool, 3), UpdatedAt: nowRFC(),
	}
	svc.lock.Unlock()

	err := svc.UploadFile(nil, dest, 1, 10, 10, 3, 25, id, "victim.bin", "victim.bin",
		chunkHeader(t, bytes.Repeat([]byte("A"), 10)))
	if !errors.Is(err, ErrSessionReset) {
		t.Fatalf("want ErrSessionReset for a mismatched resume, got %v", err)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("a mismatched resume deleted a file that was never ours: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// (2) containment — the guarantee that was claimed but not delivered
// ---------------------------------------------------------------------------

// PT-01, honestly. The old assertion only proved the write missed
// utils.BlockedSystemPrefixes. This one points the planted symlink at a
// perfectly ordinary writable directory that no blocklist covers — the shape
// that was measured returning err == nil and writing attacker bytes to
// <base>/home/victim/.ssh/authorized_keys against the ALREADY-PATCHED code.
func TestUploadCannotEscapeTheUploadRootThroughASymlink(t *testing.T) {
	dest := setupUploadEnv(t)
	outside := t.TempDir()
	svc := NewFileUploadService()
	defer svc.Close()

	if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	body := []byte("pwned")
	err := svc.UploadFile(nil, dest, 1, 16, int64(len(body)), 1, int64(len(body)),
		"5-authorized_keys", "link/victim/.ssh/authorized_keys", "authorized_keys",
		chunkHeader(t, body))
	if !errors.Is(err, ErrUploadPathRejected) {
		t.Fatalf("want ErrUploadPathRejected for a write that leaves the upload root, got %v", err)
	}
	assertEmpty(t, outside, "the upload escaped its root")
}

// A DANGLING symlink is the same escape with the target not created yet: the
// naive way to resolve a path gives up on ENOENT and answers with the
// unresolved string, which passes containment and then lets MkdirAll create the
// directory through the link.
func TestUploadCannotEscapeThroughADanglingSymlink(t *testing.T) {
	dest := setupUploadEnv(t)
	outside := t.TempDir()
	svc := NewFileUploadService()
	defer svc.Close()

	if err := os.Symlink(filepath.Join(outside, "not-created-yet"), filepath.Join(dest, "link")); err != nil {
		t.Fatalf("plant dangling symlink: %v", err)
	}

	body := []byte("pwned")
	err := svc.UploadFile(nil, dest, 1, 16, int64(len(body)), 1, int64(len(body)),
		"5-dangling.txt", "link/deep/pwned.txt", "pwned.txt", chunkHeader(t, body))
	if !errors.Is(err, ErrUploadPathRejected) {
		t.Fatalf("want ErrUploadPathRejected through a dangling symlink, got %v", err)
	}
	assertEmpty(t, outside, "a dangling symlink let the upload out of its root")
}

// LESSON 1 GUARD. Containment must not cost the legitimate case it looks like:
// a symlinked folder INSIDE the upload root (users reorganise their own data
// area constantly) still resolves inside the root, so the upload must land.
func TestSymlinkedFolderInsideTheRootStillWorks(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	real := filepath.Join(dest, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dest, "link")); err != nil {
		t.Fatal(err)
	}

	body := []byte("legit")
	if err := svc.UploadFile(nil, dest, 1, 16, int64(len(body)), 1, int64(len(body)),
		"5-legit.txt", "link/legit.txt", "legit.txt", chunkHeader(t, body)); err != nil {
		t.Fatalf("a symlinked folder inside the upload root must still accept uploads: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(real, "legit.txt"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("upload through an in-root symlink = %q (err %v), want %q", got, err, body)
	}
}

// The guards resolve one path component at a time, and the component count is
// caller-chosen: without a bound, one small POST buys hundreds of thousands of
// syscalls and the protection becomes the amplifier. The bound must refuse the
// absurd shape and leave a realistically deep folder upload alone.
func TestAbsurdlyDeepRelativePathIsRefusedButRealDepthWorks(t *testing.T) {
	dest := setupUploadEnv(t)
	svc := NewFileUploadService()
	defer svc.Close()

	deep := strings.Repeat("a/", 5000) + "x.bin"
	body := []byte("x")
	if err := svc.UploadFile(nil, dest, 1, 16, 1, 1, 1, "1-deep", deep, "x.bin",
		chunkHeader(t, body)); err == nil {
		t.Fatal("a 5000-component relative path must be refused before anything walks it")
	}
	svc.lock.RLock()
	n := len(svc.sessions)
	svc.lock.RUnlock()
	if n != 0 {
		t.Fatalf("the refused request still created %d session(s)", n)
	}

	// LESSON 1 GUARD: an ordinary deep tree still uploads.
	rel := filepath.Join("a", "b", "c", "d", "e", "f", "g", "h", "real.bin")
	if err := svc.UploadFile(nil, dest, 1, 16, 1, 1, 1, "1-real", rel, "real.bin",
		chunkHeader(t, body)); err != nil {
		t.Fatalf("a normal nested folder upload was broken by the depth bound: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("nested upload = %q (err %v), want %q", got, err, body)
	}
}

func assertEmpty(t *testing.T, dir, msg string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) == 0 {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Fatalf("%s: %s now contains %v", msg, dir, names)
}

// ---------------------------------------------------------------------------
// (3) reaper period determinism
// ---------------------------------------------------------------------------

// The period must be decided when the service is BUILT, not whenever its
// goroutine happens to get scheduled. Pinning to a single P makes the exact
// interleaving the bug needed deterministic: `go s.reapLoop()` provably has not
// run yet when the store below lands, so a loop that reads the global at start
// picks up 5 ms and reaps, while a loop that uses the captured field stays on
// the hourly period it was built with.
func TestReaperPeriodIsFixedWhenTheServiceIsBuilt(t *testing.T) {
	dest := setupUploadEnv(t)

	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	uploadReapIntervalOverride.Store(int64(time.Hour))
	t.Cleanup(func() { uploadReapIntervalOverride.Store(0) })

	svc := NewFileUploadService()
	uploadReapIntervalOverride.Store(int64(5 * time.Millisecond))
	defer svc.Close()

	if svc.reapEvery != time.Hour {
		t.Fatalf("reapEvery = %v, want the 1h period in effect at construction", svc.reapEvery)
	}

	const id = "25-notmine.bin"
	tmp := filepath.Join(dest, "notmine.bin.upload-0000c0de.tmp")
	if err := os.WriteFile(tmp, bytes.Repeat([]byte("x"), 25), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.lock.Lock()
	svc.sessions[id] = &uploadSession{
		TmpPath: tmp, Root: dest, TotalChunks: 3, ChunkSize: 10, TotalSize: 25,
		Uploaded:  make([]bool, 3),
		UpdatedAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
	}
	svc.lock.Unlock()

	// 60 periods of the 5 ms global go by. A service built for the hourly
	// period must not have reaped anything.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("a service built with the hourly period adopted a 5ms global set after it existed: %v", err)
	}
}

// Bounded-latency guard for the other half of the same finding: Close() paying
// its 10 s ceiling was the measured symptom (nearly every test in the file
// billed exactly 10.00 s on the bad run). Twenty-five stop cycles on a 2 ms
// reaper must cost a tiny fraction of one ceiling.
func TestClosingAFastReaperIsAlwaysCheap(t *testing.T) {
	setupUploadEnv(t)
	uploadReapIntervalOverride.Store(int64(2 * time.Millisecond))
	t.Cleanup(func() { uploadReapIntervalOverride.Store(0) })

	start := time.Now()
	for i := 0; i < 25; i++ {
		svc := NewFileUploadService()
		time.Sleep(3 * time.Millisecond) // let at least one tick be pending
		svc.Close()
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("25 stop cycles took %v; Close() is paying its timeout instead of stopping the loop", elapsed)
	}
}
