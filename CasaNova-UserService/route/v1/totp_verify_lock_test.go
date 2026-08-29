package v1

import (
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
	"golang.org/x/crypto/bcrypt"
)

// 2FA BACKUP CODE CONSUMPTION — CROSS-PROCESS.
//
// The claim under test is a security claim, not a tidiness one: a backup
// code that UserService accepts at login is consumed, so it cannot be
// accepted again — including when the OTHER process that owns the same file
// (CasaNova's /v1/sys/2fa/* handlers) is doing the same read-modify-write at
// the same moment.
//
// This claim CANNOT be tested with goroutines. totpVerifyMu already
// serialises goroutines inside this process, so an in-process test passes
// whether or not the cross-process lock exists — it would pass for the wrong
// reason. It also could not have caught the original defect: UserService's
// old flock on the target inode did exclude a second UserService goroutine
// perfectly well. What it did not exclude was a peer that publishes by
// rename, because the rename replaces the inode the flock is attached to.
//
// So these tests fork real processes. Two of them run the production path
// (totpEnforceLogin); two run casaNovaStylePeerConsume, a deliberately
// independent re-implementation of what CasaNova/route/v1/totp.go does —
// read the JSON, mark the code used, publish by tmp + fsync + rename. The
// peer is written from scratch rather than by calling the helpers under test,
// so the test cannot pass merely because both sides share a bug.
//
// Two scenarios, because they fail differently:
//
//	SameCode      — everyone races for ONE code. Without exclusion two
//	                processes both match it before either writes, and two
//	                logins are accepted on one second factor.
//	DistinctCodes — everyone consumes a DIFFERENT code. Without exclusion
//	                each writes a full copy of the version it read, so the
//	                last rename wins and the other consumptions vanish from
//	                disk while their logins were already accepted.

const (
	// Environment contract between the parent test and its forked children.
	envTOTPChildRole  = "CASANOVA_TEST_TOTP_CHILD_ROLE"
	envTOTPChildFile  = "CASANOVA_TEST_TOTP_CHILD_FILE"
	envTOTPChildCode  = "CASANOVA_TEST_TOTP_CHILD_CODE"
	envTOTPChildGate  = "CASANOVA_TEST_TOTP_CHILD_GATE"
	envTOTPChildReady = "CASANOVA_TEST_TOTP_CHILD_READY"

	roleUserService = "userservice"
	roleCasaNova    = "casanova"

	// Child exit codes. Distinct values so the parent can tell "the code was
	// accepted" from "the code was refused" from "something broke", instead
	// of reading a failure as a success.
	exitConsumed = 0
	exitRejected = 3
	exitFailed   = 4
)

// ---------- the forked child ----------

// TestChildTOTPConsumer is not a test. It is the entry point the parent
// re-executes this binary at, so the production consume path runs in a
// process of its own. Without the environment contract set it does nothing,
// so an ordinary `go test ./...` run skips straight past it.
func TestChildTOTPConsumer(t *testing.T) {
	role := os.Getenv(envTOTPChildRole)
	if role == "" {
		t.Skip("not a forked consumer child")
	}

	userTOTPConfigFile = os.Getenv(envTOTPChildFile)
	// Several children queue on one lock and each holds it for a couple of
	// bcrypt comparisons, so the production 5s bound is deliberately widened
	// here: a timeout would be a slow test machine, not the defect.
	userTOTPLockTimeout = 60 * time.Second

	code := os.Getenv(envTOTPChildCode)

	// Announce readiness, then spin on the start gate. Forking costs tens of
	// milliseconds and varies; the gate collapses that spread so every child
	// reaches its first read within well under a millisecond of the others.
	// Without it, a broken build could pass simply because the children never
	// overlapped.
	if err := os.WriteFile(os.Getenv(envTOTPChildReady), []byte("1"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "child: ready file:", err)
		os.Exit(exitFailed)
	}
	if !waitForStartGate(os.Getenv(envTOTPChildGate), 30*time.Second) {
		fmt.Fprintln(os.Stderr, "child: start gate never opened")
		os.Exit(exitFailed)
	}

	var err error
	switch role {
	case roleUserService:
		err = totpEnforceLogin(code)
	case roleCasaNova:
		err = casaNovaStylePeerConsume(code)
	default:
		fmt.Fprintln(os.Stderr, "child: unknown role", role)
		os.Exit(exitFailed)
	}

	switch {
	case err == nil:
		os.Exit(exitConsumed)
	case errors.Is(err, errTOTPInvalid):
		os.Exit(exitRejected)
	default:
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(exitFailed)
	}
}

func waitForStartGate(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(200 * time.Microsecond)
	}
	return false
}

// casaNovaStylePeerConsume stands in for the CasaNova service consuming the
// same backup code through PostTOTPValidate. It is written independently of
// the helpers in totp_verify.go on purpose, and it publishes by rename —
// which is precisely the behaviour that made the old flock-on-the-target
// scheme worthless.
//
// It takes the same sidecar lock, because the fix is only a fix if BOTH
// processes take it; removing the Acquire here is one of the two mutations
// that must break these tests.
func casaNovaStylePeerConsume(code string) error {
	lk, err := filelock.Acquire(userTOTPConfigFile, 60*time.Second)
	if err != nil {
		return err
	}
	defer lk.Release()

	raw, err := os.ReadFile(userTOTPConfigFile)
	if err != nil {
		return err
	}
	var cfg struct {
		Enabled     bool   `json:"enabled"`
		Secret      string `json:"secret"`
		Verified    bool   `json:"verified"`
		BackupCodes []struct {
			Hash string `json:"hash,omitempty"`
			Code string `json:"code,omitempty"`
			Used bool   `json:"used"`
		} `json:"backup_codes"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}

	upper := strings.ToUpper(strings.TrimSpace(code))
	for i := range cfg.BackupCodes {
		bc := &cfg.BackupCodes[i]
		if bc.Used || bc.Hash == "" {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(bc.Hash), []byte(upper)) != nil {
			continue
		}
		bc.Used = true
		out, mErr := json.MarshalIndent(&cfg, "", "  ")
		if mErr != nil {
			return mErr
		}
		return peerPublishAtomic(userTOTPConfigFile, out)
	}
	return errTOTPInvalid
}

// peerPublishAtomic is commonfile.WriteFileAtomic's sequence — write tmp,
// fsync, rename — with a PID-unique temp name.
//
// The shared helper uses one fixed temp name for a given target. Two
// unserialized writers therefore collide on that name and one of them fails
// with an I/O error, which would make an unlocked build fail these tests for
// an incidental reason and hide the double-acceptance they are actually
// about. Removing that accidental collision leaves the exclusion under test
// as the only thing standing between two writers.
func peerPublishAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.peer%d.tmp", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------- parent-side fixture ----------

type diskBackupCode struct {
	Hash string `json:"hash,omitempty"`
	Code string `json:"code,omitempty"`
	Used bool   `json:"used"`
}

type diskConfig struct {
	Enabled     bool             `json:"enabled"`
	Secret      string           `json:"secret"`
	Verified    bool             `json:"verified"`
	BackupCodes []diskBackupCode `json:"backup_codes"`
}

// writeCrossProcessFixture creates a 2FA config with n unused backup codes in
// a temp directory and returns its path plus the plaintext codes.
//
// The hashes are real bcrypt.DefaultCost hashes, matching production. That
// matters for more than realism: the ~60ms each comparison costs is the
// window between a process reading the file and writing it back, and it is
// what makes an unserialized build overlap reliably instead of only under
// unlucky timing.
func writeCrossProcessFixture(t *testing.T, n int) (path string, plain []string) {
	t.Helper()

	path = filepath.Join(t.TempDir(), "2fa.json")
	cfg := diskConfig{
		Enabled:  true,
		Verified: true,
		Secret:   "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
	}
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("BKUPCOD%d", i)
		h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("hash fixture code: %v", err)
		}
		cfg.BackupCodes = append(cfg.BackupCodes, diskBackupCode{Hash: string(h)})
		plain = append(plain, code)
	}

	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path, plain
}

func readCrossProcessFixture(t *testing.T, path string) diskConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg diskConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config %q: %v", string(data), err)
	}
	return cfg
}

type childSpec struct {
	role string
	code string
}

type childResult struct {
	spec     childSpec
	exitCode int
	stderr   string
}

// runConsumersConcurrently forks one process per spec, releases them all at
// the same instant, and returns what each one decided.
func runConsumersConcurrently(t *testing.T, configPath string, specs []childSpec) []childResult {
	t.Helper()

	gateDir := t.TempDir()
	gate := filepath.Join(gateDir, "go")

	type running struct {
		spec childSpec
		cmd  *exec.Cmd
		errC chan error
		out  *strings.Builder
	}

	procs := make([]*running, 0, len(specs))
	readyFiles := make([]string, 0, len(specs))

	for i, spec := range specs {
		ready := filepath.Join(gateDir, "ready"+strconv.Itoa(i))
		readyFiles = append(readyFiles, ready)

		cmd := exec.Command(os.Args[0], "-test.run=^TestChildTOTPConsumer$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(),
			envTOTPChildRole+"="+spec.role,
			envTOTPChildFile+"="+configPath,
			envTOTPChildCode+"="+spec.code,
			envTOTPChildGate+"="+gate,
			envTOTPChildReady+"="+ready,
		)
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}

		errC := make(chan error, 1)
		go func(c *exec.Cmd) { errC <- c.Wait() }(cmd)
		procs = append(procs, &running{spec: spec, cmd: cmd, errC: errC, out: &out})
	}

	t.Cleanup(func() {
		for _, p := range procs {
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}
	})

	// Wait until every child is parked on the gate before opening it.
	deadline := time.Now().Add(60 * time.Second)
	for _, ready := range readyFiles {
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child never signalled ready (%s)", ready)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := os.WriteFile(gate, []byte("1"), 0o600); err != nil {
		t.Fatalf("open start gate: %v", err)
	}

	results := make([]childResult, 0, len(procs))
	for _, p := range procs {
		var code int
		select {
		case err := <-p.errC:
			var ee *exec.ExitError
			switch {
			case err == nil:
				code = 0
			case errors.As(err, &ee):
				code = ee.ExitCode()
			default:
				t.Fatalf("child %v: %v\n%s", p.spec, err, p.out.String())
			}
		case <-time.After(120 * time.Second):
			t.Fatalf("child %v never exited\n%s", p.spec, p.out.String())
		}
		results = append(results, childResult{spec: p.spec, exitCode: code, stderr: p.out.String()})
	}
	return results
}

// countConsumed returns how many children reported the code accepted.
//
// A child that neither accepted nor rejected is reported with t.Errorf and
// NOT with t.Fatalf, deliberately: an unserialized build also produces
// genuine I/O errors (two writers collide on the same tmp file name before
// the rename), and aborting here would hide the assertion that actually
// states the security claim behind an incidental symptom.
func countConsumed(t *testing.T, results []childResult) int {
	t.Helper()
	n := 0
	for _, r := range results {
		switch r.exitCode {
		case exitConsumed:
			n++
		case exitRejected:
			// Expected loser: the code was already used by the time this
			// child got the lock.
		default:
			t.Errorf("child %v neither accepted nor rejected (exit %d):\n%s", r.spec, r.exitCode, r.stderr)
		}
	}
	return n
}

func countUsed(cfg diskConfig) int {
	n := 0
	for _, bc := range cfg.BackupCodes {
		if bc.Used {
			n++
		}
	}
	return n
}

// ---------- the invariants ----------

// TestBackupCodeAcceptedByExactlyOneProcess is the security invariant: one
// backup code, one accepted login. Four processes — two on the UserService
// login path, two on the CasaNova handler path — race for the same code.
//
// Without the shared sidecar lock, two of them match the code before either
// writes and both report success: a second factor used twice.
func TestBackupCodeAcceptedByExactlyOneProcess(t *testing.T) {
	path, plain := writeCrossProcessFixture(t, 3)
	target := plain[0]

	results := runConsumersConcurrently(t, path, []childSpec{
		{role: roleUserService, code: target},
		{role: roleCasaNova, code: target},
		{role: roleUserService, code: target},
		{role: roleCasaNova, code: target},
	})

	if got := countConsumed(t, results); got != 1 {
		t.Fatalf("%d processes accepted the same backup code, want exactly 1", got)
	}

	cfg := readCrossProcessFixture(t, path)
	if got := countUsed(cfg); got != 1 {
		t.Fatalf("on disk %d backup codes are used, want exactly 1", got)
	}
	if !cfg.BackupCodes[0].Used {
		t.Fatalf("the accepted code is not the one marked used on disk: %+v", cfg.BackupCodes)
	}
}

// TestConcurrentConsumptionsAllReachDisk is the lost-update invariant.
//
// Every process consumes a DIFFERENT code, so every one of them is entitled
// to succeed — and every one of those consumptions has to survive on disk.
// Each writer publishes a whole file, so without exclusion they all write the
// version they read and the last rename silently erases the rest: logins
// already accepted, codes still unused, no error anywhere.
//
// This is the case an atomic rename alone cannot cover, which is why the lock
// wraps the read as well as the write.
func TestConcurrentConsumptionsAllReachDisk(t *testing.T) {
	path, plain := writeCrossProcessFixture(t, 4)

	results := runConsumersConcurrently(t, path, []childSpec{
		{role: roleUserService, code: plain[0]},
		{role: roleCasaNova, code: plain[1]},
		{role: roleUserService, code: plain[2]},
		{role: roleCasaNova, code: plain[3]},
	})

	if got := countConsumed(t, results); got != 4 {
		t.Fatalf("%d of 4 distinct backup codes were accepted, want 4", got)
	}

	cfg := readCrossProcessFixture(t, path)
	if got := countUsed(cfg); got != 4 {
		t.Fatalf("on disk %d of 4 consumed backup codes are marked used — %d consumption(s) were lost",
			got, 4-got)
	}
}

// ---------- single-process behaviour the guard must not break ----------

// TestTOTPPathTakesNoCrossProcessLock pins the deliberate asymmetry. An
// ordinary TOTP login only reads, and the reader is already safe because
// every writer publishes by rename. Putting a cross-process wait in front of
// every login would let one wedged peer turn into a login outage — a guard
// that can deny service to the flow it protects is worse than the race.
//
// The test holds the lock and requires a valid TOTP to be accepted anyway.
func TestTOTPPathTakesNoCrossProcessLock(t *testing.T) {
	path, _ := writeCrossProcessFixture(t, 2)

	secretB32 := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretB32)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	code := generateUserTOTP(raw, time.Now().Unix()/int64(userTOTPPeriod))

	prev := userTOTPConfigFile
	userTOTPConfigFile = path
	t.Cleanup(func() { userTOTPConfigFile = prev })

	prevTimeout := userTOTPLockTimeout
	userTOTPLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { userTOTPLockTimeout = prevTimeout })

	// An advisory flock is per open file description, so this hold excludes
	// a second Acquire from this very process exactly as another process
	// would.
	held, err := filelock.Acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("take the peer lock: %v", err)
	}
	defer held.Release()

	if err := totpEnforceLogin(code); err != nil {
		t.Fatalf("a valid TOTP was refused while a peer held the write lock: %v", err)
	}
}

// TestBackupCodeRefusedWhenLockUnavailable is the fail-closed half. If the
// lock cannot be taken the code cannot be consumed, and accepting a code we
// did not consume is the reusable-credential bug itself. The login must fail
// and the code must still be unused.
func TestBackupCodeRefusedWhenLockUnavailable(t *testing.T) {
	path, plain := writeCrossProcessFixture(t, 2)

	prev := userTOTPConfigFile
	userTOTPConfigFile = path
	t.Cleanup(func() { userTOTPConfigFile = prev })

	prevTimeout := userTOTPLockTimeout
	userTOTPLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { userTOTPLockTimeout = prevTimeout })

	held, err := filelock.Acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("take the peer lock: %v", err)
	}
	defer held.Release()

	if err := totpEnforceLogin(plain[0]); err == nil {
		t.Fatal("a backup code was accepted while the lock was unavailable — it could not have been consumed")
	}
	if got := countUsed(readCrossProcessFixture(t, path)); got != 0 {
		t.Fatalf("%d backup code(s) were written without holding the lock", got)
	}

	// And the same code still works once the peer lets go, so the test above
	// is not passing because the path is simply broken.
	held.Release()
	userTOTPLockTimeout = prevTimeout
	if err := totpEnforceLogin(plain[0]); err != nil {
		t.Fatalf("backup code refused after the peer released the lock: %v", err)
	}
	if got := countUsed(readCrossProcessFixture(t, path)); got != 1 {
		t.Fatalf("used backup codes = %d after a successful consume, want 1", got)
	}
}

// TestTOTPConfigPathContract pins the string this module shares with
// CasaNova/route/v1/totp.go. Both derive their sidecar from it with
// filelock.Suffix; if one path moves and the other does not, the two
// processes lock different files and exclude nothing — silently.
func TestTOTPConfigPathContract(t *testing.T) {
	if userTOTPConfigFile != "/var/lib/casanova/2fa.json" {
		t.Fatalf("userTOTPConfigFile = %q; CasaNova hardcodes /var/lib/casanova/2fa.json", userTOTPConfigFile)
	}
	if want := "/var/lib/casanova/2fa.json.lock"; userTOTPConfigFile+filelock.Suffix != want {
		t.Fatalf("sidecar = %q, want %q", userTOTPConfigFile+filelock.Suffix, want)
	}
}
