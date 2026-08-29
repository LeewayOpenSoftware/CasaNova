package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

// 2FA STATE LOCK — CasaNova side.
//
// The claim: a backup code is consumed at most once, even though a second
// PROCESS (UserService, at login) performs the same read-modify-write on
// /var/lib/casanova/2fa.json.
//
// What used to be there was two mutually incompatible schemes. This side
// published by rename; UserService held flock on the target inode. flock
// belongs to an open file description of one inode, so a rename swapped the
// inode out from under UserService's lock — neither side excluded the other,
// and a consumed backup code could stay unused on disk. A recovery code that
// survives its own use is a reusable second factor.
//
// The tests below pin the two halves of the fix that live in this module:
//
//   - the legitimate flow still works (a guard that breaks the feature it
//     protects is worse than the bug);
//   - the consume path refuses to write when it cannot take the shared
//     cross-process lock, instead of writing unserialized.
//
// The second test holds the lock from the test goroutine on its OWN
// descriptor. That is a real exclusion, not a formality: an advisory flock
// is per open file description, so a second open() inside this very process
// blocks exactly as another process would. That is also why totpMu alone
// cannot stand in for it.

// writeTOTPFixture points the package at a temp config file containing an
// enabled 2FA setup with `n` unused backup codes, and returns the plaintext
// codes. The caller gets the config path back.
func writeTOTPFixture(t *testing.T, n int) (dir string, plain []string) {
	t.Helper()

	dir = t.TempDir()
	path := filepath.Join(dir, "2fa.json")

	prev := totpConfigFile
	totpConfigFile = path
	t.Cleanup(func() { totpConfigFile = prev })

	// Rate-limit state is package-global and shared with every other test in
	// this package; start from a clean slate so a lockout from elsewhere
	// cannot make these tests pass for the wrong reason.
	totpRateMu.Lock()
	totpAttempts = make(map[string]int)
	totpLockouts = make(map[string]time.Time)
	totpRateMu.Unlock()

	codes, plainCodes := generateBackupCodes(n)
	cfg := &totpConfig{
		Enabled:  true,
		Verified: true,
		// A syntactically valid base32 secret that will not match the
		// 6-digit codes used below, so only the backup-code path can accept.
		Secret:      "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
		BackupCodes: codes,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir, plainCodes
}

// readTOTPFixture reads the config straight off disk, bypassing every helper
// under test.
func readTOTPFixture(t *testing.T) *totpConfig {
	t.Helper()
	data, err := os.ReadFile(totpConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg totpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return &cfg
}

func usedBackupCodeCount(cfg *totpConfig) int {
	n := 0
	for _, bc := range cfg.BackupCodes {
		if bc.Used {
			n++
		}
	}
	return n
}

// callValidate drives PostTOTPValidate with the given code and returns the
// status and the decoded `data` map.
func callValidate(t *testing.T, code string) (int, map[string]interface{}) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/sys/2fa/validate",
		strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := PostTOTPValidate(e.NewContext(req, rec)); err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}

	var body struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body.Data
}

// TestBackupCodeConsumeStillWorksUncontended is the counterweight to the
// refusal test below. A lock that made the legitimate recovery path fail
// would be worse than the race it closes: an admin locked out of their own
// box. Uncontended, the code must be accepted AND marked used on disk.
func TestBackupCodeConsumeStillWorksUncontended(t *testing.T) {
	_, plain := writeTOTPFixture(t, 3)

	status, data := callValidate(t, plain[1])
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if valid, _ := data["valid"].(bool); !valid {
		t.Fatalf("valid = %v, want true (data=%v)", data["valid"], data)
	}

	cfg := readTOTPFixture(t)
	if got := usedBackupCodeCount(cfg); got != 1 {
		t.Fatalf("used backup codes = %d, want exactly 1", got)
	}
	if !cfg.BackupCodes[1].Used {
		t.Fatalf("the wrong backup code was consumed: %+v", cfg.BackupCodes)
	}

	// And it must not be reusable.
	status, data = callValidate(t, plain[1])
	if status != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", status)
	}
	if valid, _ := data["valid"].(bool); valid {
		t.Fatalf("a consumed backup code was accepted a second time")
	}
}

// TestBackupCodeConsumeRefusesWithoutCrossProcessLock is the isolating test.
//
// The peer (UserService) is represented by a held lock on the shared sidecar
// — the only thing that stands between the two processes. If the consume
// path does not take that lock, it will happily read, match and rename while
// the peer is mid-cycle, which is the lost update this whole change exists
// to stop.
//
// The assertion is on the FILE, not on the status code: whatever the handler
// answers, the backup code must still be unused, because a write that could
// not be serialized has not happened.
func TestBackupCodeConsumeRefusesWithoutCrossProcessLock(t *testing.T) {
	_, plain := writeTOTPFixture(t, 3)

	// Short timeout so the test does not sit through the production wait.
	prevTimeout := totpLockTimeout
	totpLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { totpLockTimeout = prevTimeout })

	peer, err := filelock.Acquire(totpConfigFile, 2*time.Second)
	if err != nil {
		t.Fatalf("test could not take the peer lock: %v", err)
	}
	defer peer.Release()

	status, data := callValidate(t, plain[0])
	if valid, _ := data["valid"].(bool); valid {
		t.Fatalf("handler reported valid:true while the peer held the lock (status=%d)", status)
	}

	cfg := readTOTPFixture(t)
	if got := usedBackupCodeCount(cfg); got != 0 {
		t.Fatalf("handler consumed %d backup code(s) without holding the cross-process lock", got)
	}

	// The lock must be released again for the ordinary path: release the
	// peer and confirm the same code is still usable. Without this the test
	// would also pass if the handler had simply stopped working.
	peer.Release()
	totpLockTimeout = prevTimeout

	status, data = callValidate(t, plain[0])
	if status != http.StatusOK {
		t.Fatalf("post-contention status = %d, want 200", status)
	}
	if valid, _ := data["valid"].(bool); !valid {
		t.Fatalf("backup code was rejected after the peer released the lock (data=%v)", data)
	}
	if got := usedBackupCodeCount(readTOTPFixture(t)); got != 1 {
		t.Fatalf("used backup codes = %d after the peer released, want 1", got)
	}
}

// TestGetTOTPStatusDoesNotDependOnTheLock pins the deliberate asymmetry.
//
// A status read must never be able to fail because a peer holds the write
// lock: it is a report, and a report that can take the feature down with it
// is the failure mode that took the file API to 503 in this project once
// already. The status endpoint reads the renamed file and answers.
func TestGetTOTPStatusDoesNotDependOnTheLock(t *testing.T) {
	writeTOTPFixture(t, 4)

	prevTimeout := totpLockTimeout
	totpLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { totpLockTimeout = prevTimeout })

	peer, err := filelock.Acquire(totpConfigFile, 2*time.Second)
	if err != nil {
		t.Fatalf("test could not take the peer lock: %v", err)
	}
	defer peer.Release()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/2fa/status", nil)
	rec := httptest.NewRecorder()
	if err := GetTOTPStatus(e.NewContext(req, rec)); err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d while a peer held the write lock, want 200", rec.Code)
	}

	var body struct {
		Data struct {
			Enabled   bool `json:"enabled"`
			Remaining int  `json:"backup_codes_remaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if !body.Data.Enabled || body.Data.Remaining != 4 {
		t.Fatalf("status = %+v, want enabled with 4 remaining", body.Data)
	}
}

// TestTOTPStateLockPathContract pins the two strings both services have to
// agree on. UserService derives its lock from the same config path with the
// same filelock.Suffix; if this module's path moves and the other's does not,
// the two processes lock different sidecars and exclude nothing — silently,
// which is why it is asserted rather than left to review.
func TestTOTPStateLockPathContract(t *testing.T) {
	if totpConfigFile != "/var/lib/casanova/2fa.json" {
		t.Fatalf("totpConfigFile = %q; UserService hardcodes /var/lib/casanova/2fa.json", totpConfigFile)
	}
	if want := "/var/lib/casanova/2fa.json.lock"; totpConfigFile+filelock.Suffix != want {
		t.Fatalf("sidecar = %q, want %q", totpConfigFile+filelock.Suffix, want)
	}
}

// TestLockTOTPStateCreatesStateDirectory covers the first-run case: on a box
// where 2FA was never set up, neither the file nor /var/lib/casanova exists.
// filelock deliberately does not create the directory, so lockTOTPState must
// — otherwise the very first PostTOTPSetup fails to lock and 2FA can never be
// enabled.
func TestLockTOTPStateCreatesStateDirectory(t *testing.T) {
	prev := totpConfigFile
	totpConfigFile = filepath.Join(t.TempDir(), "never", "created", "2fa.json")
	t.Cleanup(func() { totpConfigFile = prev })

	release, err := lockTOTPState()
	if err != nil {
		t.Fatalf("lockTOTPState on a fresh box: %v", err)
	}
	release()

	if _, err := os.Stat(filepath.Dir(totpConfigFile)); err != nil {
		t.Fatalf("state directory was not created: %v", err)
	}
}

// bcryptCostSanity keeps the fixtures honest: generateBackupCodes uses
// bcrypt.DefaultCost, and a fixture that silently dropped to a cheaper cost
// would make the timing-sensitive parts of these tests meaningless.
func TestBackupCodeFixtureUsesRealHashes(t *testing.T) {
	codes, plain := generateBackupCodes(1)
	if codes[0].Hash == "" || codes[0].Code != "" {
		t.Fatalf("backup code fixture is not hash-only: %+v", codes[0])
	}
	if err := bcrypt.CompareHashAndPassword([]byte(codes[0].Hash), []byte(plain[0])); err != nil {
		t.Fatalf("generated hash does not match its plaintext: %v", err)
	}
}
