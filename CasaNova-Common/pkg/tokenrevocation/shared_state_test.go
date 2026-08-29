package tokenrevocation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TR-CONC-01. The list is shared by nine PROCESSES. A sync.Mutex orders nothing
// between them, and an atomic rename protects readers, not writers.
//
// INVARIANT: if Revoke returns nil, a later read of the shared file — from any
// process — contains that principal, and no entry that was there before the
// call is missing after it.
//
// These tests use real processes on purpose. Goroutines cannot fail the way the
// bug failed: the defect was invisible to every in-process test because the
// process mutex made it look correct.
// ---------------------------------------------------------------------------

// TestCrossProcessRevokeHelper is not a test. It is the body of the child
// process the tests below spawn: one service, one revocation, its own copy of
// this package with its own private mutex.
func TestCrossProcessRevokeHelper(t *testing.T) {
	path := os.Getenv("TR_HELPER_PATH")
	if path == "" {
		t.Skip("not a helper invocation")
	}
	SetPath(path)

	// Every child does its work at the same instant, so the windows actually
	// overlap instead of relying on process startup jitter.
	waitForBarrier(t, os.Getenv("TR_HELPER_START"))

	if os.Getenv("TR_HELPER_ENSURE") == "1" {
		// A service starting up, which is what makes this the realistic
		// scenario: the self-update stops and starts services one at a time
		// while the box stays live.
		if err := Ensure(); err != nil {
			t.Fatalf("helper Ensure: %v", err)
		}
	}

	if p := os.Getenv("TR_HELPER_PRINCIPAL"); p != "" {
		if err := Revoke(p); err != nil {
			// A non-zero exit says "this process did NOT report success", and
			// the parent only holds reported successes to the invariant.
			t.Fatalf("helper Revoke(%s): %v", p, err)
		}
	}
}

func waitForBarrier(t *testing.T, nanos string) {
	t.Helper()
	if nanos == "" {
		return
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		t.Fatalf("bad barrier %q: %v", nanos, err)
	}
	if d := time.Until(time.Unix(0, n)); d > 0 {
		time.Sleep(d)
	}
}

// helperCmd builds a child-process invocation of the helper above.
func helperCmd(t *testing.T, path, principal string, ensure bool, start time.Time) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessRevokeHelper$", "-test.timeout=120s")
	env := append(os.Environ(),
		"TR_HELPER_PATH="+path,
		"TR_HELPER_START="+strconv.FormatInt(start.UnixNano(), 10),
	)
	if principal != "" {
		env = append(env, "TR_HELPER_PRINCIPAL="+principal)
	}
	if ensure {
		env = append(env, "TR_HELPER_ENSURE=1")
	}
	cmd.Env = env
	return cmd
}

// Twelve independent processes, each revoking a distinct principal, all
// released at the same instant. Before the cross-process lock this left one or
// two of twelve entries on disk while all twelve processes reported success.
//
// Mutation that must fail this test: drop the filelock.Acquire in revokeAt.
func TestConcurrentProcessesDoNotLoseRevocations(t *testing.T) {
	const procs = 12
	base := t.TempDir()

	for round := 0; round < 2; round++ {
		path := filepath.Join(base, fmt.Sprintf("round%d", round), "token-revocations.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		start := time.Now().Add(1500 * time.Millisecond)
		reported := make([]bool, procs)
		var wg sync.WaitGroup
		for i := 0; i < procs; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out, err := helperCmd(t, path, "user"+strconv.Itoa(i), true, start).CombinedOutput()
				reported[i] = err == nil
				if err != nil {
					t.Logf("round %d process %d did not report success: %v\n%s", round, i, err, out)
				}
			}(i)
		}
		wg.Wait()

		l, err := readFile(path)
		if err != nil {
			t.Fatalf("round %d: read shared list: %v", round, err)
		}

		var missing []string
		claimed := 0
		for i := 0; i < procs; i++ {
			if !reported[i] {
				continue
			}
			claimed++
			if _, present := l.entries["user"+strconv.Itoa(i)]; !present {
				missing = append(missing, "user"+strconv.Itoa(i))
			}
		}
		if claimed == 0 {
			t.Fatalf("round %d: no process reported success, the test proved nothing", round)
		}
		if len(missing) > 0 {
			t.Fatalf("round %d: %d processes reported the revocation as done and %d are absent from the shared file %v; "+
				"the API answered 200 and the principals still have access", round, claimed, len(missing), missing)
		}
	}
}

// The production shape from the finding: an operator deletes a user while other
// services are restarting. Restarting is not a rare event here — the
// self-update stops and starts the services one at a time on a live box.
//
// A restart runs Provision, which is itself a read-modify-write whenever it has
// to create the list or bring an older one up to the current contract. Both
// states are seeded below, because a start that only reads cannot lose
// anything and would prove nothing.
//
// Mutation that must fail this test: drop the filelock.Acquire in
// provisionOnDisk.
func TestARevocationSurvivesServicesRestartingAroundIt(t *testing.T) {
	const restarts = 8
	base := t.TempDir()

	seeds := []struct {
		name string
		seed func(path string)
	}{
		{"first boot, no list yet", func(string) {}},
		{"a list from a build that predates the contract", func(path string) {
			legacy := `{"version":1,"revoked":{"old":` + itoa(time.Now().Add(-time.Hour).Unix()) + `}}`
			if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}},
	}

	for round, sc := range seeds {
		path := filepath.Join(base, fmt.Sprintf("round%d", round), "token-revocations.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		sc.seed(path)

		start := time.Now().Add(1500 * time.Millisecond)
		var wg sync.WaitGroup
		for i := 0; i < restarts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = helperCmd(t, path, "", true, start).CombinedOutput()
			}()
		}

		var deleteErr error
		var deleteOut []byte
		wg.Add(1)
		go func() {
			defer wg.Done()
			deleteOut, deleteErr = helperCmd(t, path, "deleted-user", false, start).CombinedOutput()
		}()
		wg.Wait()

		if deleteErr != nil {
			t.Logf("round %d (%s): the deletion reported failure, which is honest: %v\n%s",
				round, sc.name, deleteErr, deleteOut)
			continue
		}

		l, err := readFile(path)
		if err != nil {
			t.Fatalf("round %d (%s): read shared list: %v", round, sc.name, err)
		}
		if _, present := l.entries["deleted-user"]; !present {
			t.Fatalf("round %d (%s): the user service reported the deletion as done and the revocation is not in %s; "+
				"the deleted account keeps every service until its refresh token expires", round, sc.name, path)
		}
	}
}

// ---------------------------------------------------------------------------
// TR-CONC-05. No revocation query may wait on another goroutine's disk.
// ---------------------------------------------------------------------------

// Structural, not timed: the hook runs at the moment the package is about to
// touch the disk and asserts that the lock every request consults can still be
// taken. A clock budget here would only measure the machine.
//
// Mutation that must fail this test: take s.mu.Lock() around the stat/read in
// reload, as the original did.
func TestReloadDoesNotHoldTheRequestLockAcrossDiskIO(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	lockFree := make(chan bool, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	duringDiskIO = func(stage string) {
		if stage != "reload" {
			return
		}
		// One observation is enough, and the hook variable is never reassigned
		// from inside the hook: another goroutine may be reading it.
		once.Do(func() {
			ok := global.mu.TryRLock()
			if ok {
				global.mu.RUnlock()
			}
			lockFree <- ok
			<-release
		})
	}
	// Order matters: release the paused goroutine and wait for it to finish
	// before the temp directory is torn down, or it writes into a directory
	// that is being removed.
	t.Cleanup(func() { <-finished })
	t.Cleanup(func() { duringDiskIO = nil; close(release) })

	// Force the next consultation to refresh.
	global.mu.Lock()
	global.lastCheck = time.Time{}
	global.mu.Unlock()

	go func() { defer close(finished); _ = IsRevoked("x", time.Now()) }()

	select {
	case ok := <-lockFree:
		if !ok {
			t.Fatal("refresh held the lock every request consults across disk I/O; " +
				"a slow or spun-down disk stops the whole process")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never reached the disk")
	}

	// And the liveness half: a second query must not queue behind the first
	// one's I/O, which is still in flight.
	done := make(chan struct{})
	go func() { defer close(done); _ = IsRevoked("y", time.Now()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a revocation query waited for another goroutine's disk I/O")
	}
}

// Same invariant for the write path: a revocation takes a cross-process lock
// and rewrites the file, and none of that may stop the request path.
//
// Mutation that must fail this test: hold global.mu across revokeAt's file
// work, as the original revokeAtLocked did.
func TestRevokeDoesNotHoldTheRequestLockAcrossDiskIO(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	lockFree := make(chan bool, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	duringDiskIO = func(stage string) {
		if stage != "revoke" {
			return
		}
		once.Do(func() {
			ok := global.mu.TryRLock()
			if ok {
				global.mu.RUnlock()
			}
			lockFree <- ok
			<-release
		})
	}
	t.Cleanup(func() { <-finished })
	t.Cleanup(func() { duringDiskIO = nil; close(release) })

	go func() { defer close(finished); _ = RevokeID(1234) }()

	select {
	case ok := <-lockFree:
		if !ok {
			t.Fatal("a revocation held the lock every request consults across its file work")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revocation never reached the disk")
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = IsRevoked("y", time.Now()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a revocation query waited for a concurrent revocation's disk I/O")
	}
}

// Moving the disk read out from under the lock introduces a hazard the
// original shape did not have: a refresh that read the file BEFORE a local
// revocation landed must not publish that older map over the newer one. If it
// did, a revocation this process had just reported as done would vanish from
// its own view for up to a second — fail-open for the principal an operator
// just deleted.
//
// Mutation that must fail this test: drop the generation check in reload's
// publish step.
func TestARefreshCannotUndoARevocationThatOvertookIt(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Another service changed the file, so the refresh really reads.
	if err := writeFile(path, list{entries: map[string]time.Time{"other": time.Now()}}); err != nil {
		t.Fatalf("external write: %v", err)
	}

	reading := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	duringDiskIO = func(stage string) {
		if stage != "reload-publish" {
			return
		}
		once.Do(func() {
			close(reading)
			<-release
		})
	}
	t.Cleanup(func() { <-finished })
	t.Cleanup(func() { duringDiskIO = nil })

	global.mu.Lock()
	global.lastCheck = time.Time{}
	global.mu.Unlock()

	go func() { defer close(finished); _ = IsRevoked("z", time.Now()) }()

	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never read the file")
	}

	// The revocation overtakes the in-flight refresh.
	if err := RevokeID(99); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}
	close(release)
	<-finished

	if !IsRevoked("99", time.Now().Add(-time.Hour)) {
		t.Fatal("a refresh that started earlier published its older snapshot and undid a completed revocation")
	}
}

// A provisioning failure used to be permanent: nothing retried, so a full disk
// or a late mount at boot left the process refusing every token until somebody
// noticed and restarted it. The nine mains only log the error and carry on.
//
// Mutation that must fail this test: make refresh return early when the
// authority is not enforcing, instead of calling repair.
func TestTransientProvisioningFailureHealsWithoutARestart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the fault cannot be injected")
	}
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	path := filepath.Join(stateDir, "token-revocations.json")
	SetPath(path)
	t.Cleanup(func() { SetPath(DefaultPath) })

	if err := Ensure(); err == nil {
		t.Fatal("provisioning succeeded on a directory it cannot write")
	}
	if !IsRevoked("1", time.Now()) {
		t.Fatal("a process that could not provision accepted a token")
	}

	// The fault clears. Nobody restarts anything, nobody runs a command.
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	global.mu.Lock()
	global.nextRepairAttempt = time.Time{} // do not make the test wait out the pacing
	global.mu.Unlock()

	if IsRevoked("1", time.Now()) {
		t.Fatal("the process is still refusing every token after the fault cleared; " +
			"a transient disk error is an outage that needs a human")
	}
	if h := CurrentHealth(); h.Status != StatusHealthy || !h.Enforcing {
		t.Fatalf("health after self-repair = %+v, want healthy and enforcing", h)
	}
}

// ---------------------------------------------------------------------------
// Boot cost and debris: the self-test must not touch the authoritative list.
// ---------------------------------------------------------------------------

// Mutation that must fail this test: make SelfTest record its probe in the
// shared file again.
func TestSelfTestNeverWritesTheSharedList(t *testing.T) {
	path := use(t)
	if err := Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := RevokeID(77); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := SelfTest(); err != nil {
			t.Fatalf("SelfTest: %v", err)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the self-test rewrote the authoritative list.\nbefore: %s\nafter:  %s", before, after)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info2.ModTime().Equal(info.ModTime()) {
		t.Fatal("the self-test replaced the authoritative list; every service's refresh reloads for nothing, " +
			"and a process killed between the probe write and its cleanup leaves the probe behind")
	}
	// And it still proves the property, rather than having been hollowed out.
	if !IsRevoked("77", time.Now().Add(-time.Hour)) {
		t.Fatal("enforcement stopped working")
	}
}

// Every temporary the package makes must be cleaned up: the state directory is
// on the box's root filesystem and nine services boot into it.
func TestBootLeavesNoDebrisInTheStateDirectory(t *testing.T) {
	path := use(t)
	for i := 0; i < 3; i++ {
		SetPath(path)
		if err := Ensure(); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	assertNoDebris(t, path)
}

func assertNoDebris(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	want := map[string]bool{
		filepath.Base(path):           true,
		filepath.Base(path) + ".lock": true,
	}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Fatalf("left behind %q in the state directory", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Failure injection on the persistent state machine.
// ---------------------------------------------------------------------------

// A write that cannot happen must cost the caller an error and cost the box
// nothing: no entry lost, no partial file, no debris, and the failure must not
// be reported as a completed revocation.
func TestAFailedWriteLosesNothingAndLeavesNoDebris(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the fault cannot be injected")
	}
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(1); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := RevokeID(2); err == nil {
		_ = os.Chmod(dir, 0o755)
		t.Fatal("Revoke reported success while the state directory refused every write; " +
			"the caller would answer 200 and the account would keep working")
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod back: %v", err)
	}

	l, err := readFile(path)
	if err != nil {
		t.Fatalf("the failed write left the list unreadable: %v", err)
	}
	if _, present := l.entries["1"]; !present {
		t.Fatal("a failed write destroyed a revocation that was already recorded")
	}
	if _, present := l.entries["2"]; present {
		t.Fatal("a revocation reported as failed is on disk; the two disagree")
	}
	assertNoDebris(t, path)
}

// A process killed between writeFile's staging and its rename leaves a temp
// file. The next start must ignore it, not adopt it, and not fail on it.
func TestAnInterruptedWriteLeavesTheAuthorityIntact(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(5); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	// The debris a SIGKILL mid-write leaves: staged content that never got
	// renamed, holding a list that does NOT contain the revocation.
	stale := filepath.Join(filepath.Dir(path), ".token-revocations-orphan.tmp")
	if err := os.WriteFile(stale, []byte(`{"version":1,"min_reader_version":1,"revoked":{}}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	SetPath(path)
	if err := Ensure(); err != nil {
		t.Fatalf("a start after an interrupted write did not converge: %v", err)
	}
	if !IsRevoked("5", time.Now().Add(-time.Hour)) {
		t.Fatal("a start after an interrupted write adopted the staged file and dropped a revocation")
	}
}

// ---------------------------------------------------------------------------
// P5-11 / deployment semantics: the state a vulnerable build leaves behind.
// ---------------------------------------------------------------------------

// A list written by a build whose updates were not serialized across processes
// may be missing revocations that its API already reported as done, and nothing
// on disk says which. The first build that can tell refuses every credential
// minted during that period, exactly once.
//
// Mutation that must fail this test: make sanitizeLegacyList a no-op.
func TestAListFromABuildThatCouldLoseRevocationsIsSanitizedOnce(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Exactly what the vulnerable build wrote: no min_reader_version.
	old := time.Now().Add(-2 * time.Hour).Unix()
	legacy := `{"version":1,"revoked":{"9":` + itoa(old) + `}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Ensure(); err != nil {
		t.Fatalf("Ensure on a pre-contract list: %v", err)
	}

	// Every token minted while revocations could go missing stops working.
	if !IsRevoked("42", time.Now().Add(-time.Minute)) {
		t.Fatal("a credential from the vulnerable build's lifetime still works; " +
			"a user deleted back then keeps access until the refresh token expires")
	}
	// The box stays usable: signing in again works.
	if IsRevoked("42", time.Now().Add(time.Minute)) {
		t.Fatal("nobody can obtain a working token any more")
	}
	// The revocations that DID make it are kept.
	if !IsRevoked("9", time.Unix(old, 0).Add(-time.Hour)) {
		t.Fatal("sanitizing dropped a revocation that was recorded")
	}
	// The operator can see why everyone was signed out.
	if h := CurrentHealth(); h.RevokedBefore == "" {
		t.Fatalf("health does not report the floor that signed everyone out: %+v", h)
	}

	first, err := readFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if first.before.IsZero() {
		t.Fatal("the sanitizing floor was not persisted; every service would apply its own")
	}
	if first.readerContract != minReaderVersion {
		t.Fatalf("the rewritten list does not declare the contract: %d", first.readerContract)
	}

	// Exactly once. The other eight services, and every later restart, must
	// find a current file and leave the floor where it is.
	for i := 0; i < 3; i++ {
		SetPath(path)
		if err := Ensure(); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		again, err := readFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !again.before.Equal(first.before) {
			t.Fatalf("restart %d moved the floor from %v to %v; every restart would sign everyone out again",
				i, first.before, again.before)
		}
	}
}

// A revocation that lands on a pre-contract list must not be the thing that
// cancels the migration. Every write stamps the current contract, so a Revoke
// that reached the file before any Provision did would erase the only marker
// saying the sanitizing floor is still owed.
//
// Mutation that must fail this test: drop the sanitizeLegacyList call in
// revokeAt.
func TestARevocationOntoAPreContractListStillSanitizesIt(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{"version":1,"revoked":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Revoke("deleted"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	l, err := readFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if l.readerContract != minReaderVersion {
		t.Fatalf("the write did not stamp the contract: %d", l.readerContract)
	}
	if l.before.IsZero() {
		t.Fatal("a revocation erased the pre-contract marker without applying the floor it owed; " +
			"the migration can now never happen and the inherited state stays hostile")
	}
}

// A build that never lost anything must not sign everyone out: a fresh install
// gets no floor.
func TestAFreshInstallGetsNoSanitizingFloor(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	l, err := readFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !l.before.IsZero() {
		t.Fatalf("a fresh install set a floor at %v; every user would be signed out for nothing", l.before)
	}
	if IsRevoked("1", time.Now().Add(-time.Hour)) {
		t.Fatal("a fresh install refuses an existing session")
	}
}
