package v1

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"golang.org/x/crypto/bcrypt"
)

// Wave 28: 2FA enforcement at login.
//
// The TOTP config lives at /var/lib/casanova/2fa.json and is owned by the
// CasaNova service (which provides the setup/enable/disable HTTP handlers
// and the /v1/sys/2fa/* endpoints). UserService and CasaNova both run as
// root on the same host, so UserService can read — and, when consuming a
// backup code, write — that file directly. Cross-service HTTP coupling
// for an at-login check would be slower and would create a circular
// startup dependency between UserService and CasaNova.
//
// # Concurrency (fixed: the two services used to disagree on the lock)
//
// This file used to take flock(LOCK_EX) on 2fa.json ITSELF and hold an
// O_RDWR descriptor across read/verify/write. CasaNova's totp.go writes
// the same file with WriteFileAtomic — write-tmp + fsync + rename. flock
// is attached to an open file description of one inode, so the rename
// swapped the inode out from under this lock: the two schemes excluded
// nothing at all, and after a peer rename the descriptor held here pointed
// at an orphaned inode, so a consumed backup code was written to a file
// nobody would ever read again. A second-factor recovery code that stays
// unused on disk is a reusable credential.
//
// Both processes now use the ONE scheme that survives a rename:
// CasaNova-Common/pkg/filelock, an advisory flock on the stable sidecar
// "/var/lib/casanova/2fa.json.lock", taken before the read and held until
// after the rename. See CasaNova/route/v1/totp.go::lockTOTPState for the
// other half — the two must stay in agreement or the exclusion is gone.
//
// Where the lock is and is NOT taken, and why:
//
//   - The pure-TOTP login path takes NO cross-process lock. It only reads,
//     and a reader is already safe: every writer publishes by rename, so a
//     reader sees the whole old file or the whole new one, never a torn
//     one. Locking there would put a cross-process wait in front of every
//     single login for no correctness gain — a stuck peer would become a
//     login outage.
//   - The backup-code path takes the lock, because it is a
//     read-modify-write and it must re-read under the lock: the pre-lock
//     read is stale by construction, and the very code being checked may
//     have been consumed by a peer while we waited.
//   - If the lock cannot be taken the backup-code login is REFUSED. We
//     cannot mark the code used, and accepting a code we did not consume
//     is exactly the reusable-credential bug above. This fails closed on a
//     rare recovery path only; ordinary TOTP logins are unaffected.
//
// Schema must match CasaNova/route/v1/totp.go::totpConfig and ::backupCode
// exactly. If you change one, change the other.

// userTOTPConfigFile is a var, not a const, only so tests can redirect it
// to a temp directory. Nothing in production reassigns it.
var userTOTPConfigFile = "/var/lib/casanova/2fa.json"

const userTOTPPeriod = 30

// userTOTPLockTimeout bounds the wait for the cross-process lock. The guarded
// section is one small read plus one small write, so real contention clears in
// milliseconds; the bound exists so a wedged peer turns into a failed login
// attempt instead of a hung request holding a connection open. A var so tests
// can widen it when they deliberately pile several processes onto the lock.
var userTOTPLockTimeout = 5 * time.Second

type userTOTPBackupCode struct {
	Hash string `json:"hash,omitempty"`
	Code string `json:"code,omitempty"` // legacy plaintext (read-only here)
	Used bool   `json:"used"`
}

type userTOTPConfig struct {
	Enabled     bool                 `json:"enabled"`
	Secret      string               `json:"secret"`
	Verified    bool                 `json:"verified"`
	BackupCodes []userTOTPBackupCode `json:"backup_codes"`
}

// totpVerifyMu serialises access this process makes to the file. The
// filelock guards against the *other* process; this mutex guards two
// goroutines inside this process, which flock alone would not — an
// advisory lock is per open file description, and two goroutines here
// would each open their own.
var totpVerifyMu sync.Mutex

// totpLastStep / totpLastSecret remember the newest time step accepted at
// login (and for which secret), so a TOTP code cannot be replayed within its
// ±1-step window. Guarded by totpVerifyMu. Kept in memory on purpose: the
// TOTP login path must stay lock-free and write-free on 2fa.json (see the
// concurrency notes above), and a code's whole window is ~90 s, so losing
// this on a restart only reopens one window.
var (
	totpLastStep   int64
	totpLastSecret string
)

// errTOTPRequired signals "the password was right but a 2FA code is
// required and was not supplied". The login handler maps it to a
// distinct response so the UI can prompt for an OTP without retrying
// the password.
var errTOTPRequired = errors.New("totp: code required")

// errTOTPInvalid signals a wrong/expired TOTP code or unknown backup
// code. Treated as a login failure but distinguishable from a bad
// password in audit logs.
var errTOTPInvalid = errors.New("totp: invalid code")

// totpEnforceLogin checks the on-disk 2FA config and, if 2FA is enabled,
// validates the supplied code (either a TOTP or an unused backup code).
//
// Returns:
//   - nil                : 2FA disabled OR code accepted
//   - errTOTPRequired    : 2FA enabled but no code supplied
//   - errTOTPInvalid     : code rejected
//   - any other error    : I/O / lock / decode failure (treat as 500)
func totpEnforceLogin(suppliedCode string) error {
	totpVerifyMu.Lock()
	defer totpVerifyMu.Unlock()

	cfg, err := readUserTOTPConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil // 2FA never set up → nothing to enforce
	}
	if !cfg.Enabled || !cfg.Verified || cfg.Secret == "" {
		return nil
	}

	code := strings.TrimSpace(suppliedCode)
	if code == "" {
		return errTOTPRequired
	}

	// Try TOTP first. This decides on the snapshot we just read and never
	// writes, so it needs no cross-process lock.
	secret, derr := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(cfg.Secret))
	if derr == nil {
		var minStep int64 = -1 << 63
		if totpLastSecret == cfg.Secret {
			minStep = totpLastStep + 1
		}
		if step, ok := matchUserTOTPStep(secret, code, minStep); ok {
			totpLastStep, totpLastSecret = step, cfg.Secret
			return nil
		}
		if _, ok := matchUserTOTPStep(secret, code, -1<<63); ok {
			return errTOTPInvalid // valid code, already used
		}
	}

	// Fall back to backup codes. That path consumes the code, so it runs
	// under the cross-process lock and re-reads the file there.
	return consumeUserBackupCode(strings.ToUpper(code))
}

// readUserTOTPConfig parses the config file. A missing or empty file means
// "2FA was never set up" and yields (nil, nil).
//
// The read is deliberately unlocked: writers publish by atomic rename, so
// a reader gets one whole version of the file.
func readUserTOTPConfig() (*userTOTPConfig, error) {
	data, err := os.ReadFile(userTOTPConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var cfg userTOTPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// consumeUserBackupCode marks the first unused backup code matching
// codeUpper as used and persists the file, with the whole read-verify-write
// cycle under the cross-process lock on the sidecar.
//
// Returns nil if a code was consumed, errTOTPInvalid if none matched, and
// any other error if the lock or the I/O failed — a caller that cannot
// consume must not accept.
func consumeUserBackupCode(codeUpper string) error {
	lk, err := filelock.Acquire(userTOTPConfigFile, userTOTPLockTimeout)
	if err != nil {
		return err
	}
	defer lk.Release()

	// Re-read UNDER the lock. Whatever we read before acquiring it is a
	// snapshot from before every writer that ran while we waited, and one
	// of them may have consumed this exact code.
	cfg, err := readUserTOTPConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		// The config vanished while we waited (2FA reset). Nothing to
		// enforce, but nothing to consume either — reject rather than
		// silently letting an unverifiable code through.
		return errTOTPInvalid
	}
	if !cfg.Enabled || !cfg.Verified {
		return nil // 2FA was disabled while we waited
	}

	for i := range cfg.BackupCodes {
		bc := &cfg.BackupCodes[i]
		if bc.Used || bc.Hash == "" {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(bc.Hash), []byte(codeUpper)) != nil {
			continue
		}
		bc.Used = true
		out, mErr := json.MarshalIndent(cfg, "", "  ")
		if mErr != nil {
			return mErr
		}
		// Same publish mechanism as CasaNova's saveTOTPConfig: tmp +
		// fsync + rename, so a reader never sees a half-written file and
		// a crash never leaves the code neither used nor usable.
		if wErr := commonfile.WriteFileAtomic(userTOTPConfigFile, out, 0o600); wErr != nil {
			return wErr
		}
		return nil
	}

	return errTOTPInvalid
}

// validateUserTOTP is the same RFC 6238 validator used by CasaNova's
// totp.go, duplicated here so UserService doesn't import the CasaNova
// service package (which would create a circular module dependency).
// Tolerates ±1 time step (30s) of clock drift. Returns the matching step,
// ignoring steps below minStep (already accepted ones).
func matchUserTOTPStep(secret []byte, code string, minStep int64) (int64, bool) {
	now := time.Now().Unix() / int64(userTOTPPeriod)
	for _, off := range []int64{-1, 0, 1} {
		if now+off >= minStep && generateUserTOTP(secret, now+off) == code {
			return now + off, true
		}
	}
	return 0, false
}

func generateUserTOTP(secret []byte, timeStep int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(timeStep))
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf)
	hash := mac.Sum(nil)
	offset := hash[len(hash)-1] & 0x0f
	codeInt := binary.BigEndian.Uint32(hash[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", codeInt%1000000)
}
