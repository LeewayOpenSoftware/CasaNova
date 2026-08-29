package v1

import (
	"crypto/hmac"
	cryptoRand "crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

// totpConfigFile is a var, not a const, only so tests can redirect it to a
// temp directory. Nothing in production reassigns it.
//
// UserService reads and writes the same path (see
// CasaNova-UserService/route/v1/totp_verify.go). Both sides must agree on
// this string AND on the locking scheme below, or the exclusion is gone.
var totpConfigFile = "/var/lib/casanova/2fa.json"

const (
	totpPeriod    = 30
	totpDigits    = 6
	totpIssuer    = "CasaNova"
	backupCodeLen = 8
	backupCodeNum = 8
)

// totpConfig is the on-disk representation of 2FA state.
type totpConfig struct {
	Enabled     bool         `json:"enabled"`
	Secret      string       `json:"secret"`
	Verified    bool         `json:"verified"`
	BackupCodes []backupCode `json:"backup_codes"`
}

// backupCode is a single recovery code stored on disk.
//
// Wave 26: only the bcrypt hash is persisted. The plaintext is shown to
// the user exactly once at setup time and never written anywhere — read
// access to /var/lib/casanova/2fa.json no longer hands an attacker every
// recovery code on a plate. The legacy `Code` field is kept for backward
// compatibility with files written by previous versions; it is migrated
// to `Hash` on the first read and cleared on the next save.
type backupCode struct {
	Hash string `json:"hash,omitempty"`
	Code string `json:"code,omitempty"` // legacy plaintext, migrated on read
	Used bool   `json:"used"`
}

// totpMu serialises access this process makes to 2fa.json. It orders
// nothing against UserService — see lockTOTPState.
var totpMu sync.Mutex

// totpLockTimeout bounds the wait for the cross-process lock. The guarded
// section is one small read plus one small write, so real contention clears
// in milliseconds; the bound exists so a wedged peer turns into a failed
// request instead of a hung one. A var so tests can shorten it.
var totpLockTimeout = 5 * time.Second

// lockTOTPState takes BOTH locks guarding 2fa.json and returns the release.
//
// Why two: totpMu orders goroutines inside this process (an advisory flock
// is per open file description, so two goroutines here would each open
// their own descriptor and neither would block). filelock orders this
// process against UserService, which performs the same read-modify-write on
// the same file at login time when a backup code is consumed.
//
// Why a sidecar and not the file: every write here publishes by rename, so
// the target's inode is replaced on each save. A lock taken on the target
// stops excluding anything the moment someone renames over it — that is the
// exact defect this replaces. filelock.Acquire locks "<path>.lock", which
// is never renamed and never unlinked.
//
// The whole read-modify-write must run inside one hold: locking only the
// write would still let two processes read the same version, edit their own
// copies, and have the later rename erase the earlier one silently.
//
// On error NEITHER lock is held and the caller must fail the request. A
// mutation that could not be serialized has not happened, and reporting it
// as done is the bug this exists to prevent.
func lockTOTPState() (func(), error) {
	totpMu.Lock()
	// filelock deliberately does not create the state directory (the owner
	// of the directory owns its mode), so create it here — on a box where
	// 2FA was never set up neither the file nor the directory exists yet
	// and the very first setup call would otherwise fail to lock.
	// 0o755, matching the rest of the tree.
	//
	// /var/lib/casanova is SHARED: tokenrevocation creates it 0755 and depends
	// on every service being able to read its list — "they do not all run as
	// the same user" — and repairs the mode when it drifts. Creating it 0700
	// here means whichever subsystem wins the race decides whether the others
	// can read their own state. The secret lives in 2fa.json, written 0600.
	if err := os.MkdirAll(filepath.Dir(totpConfigFile), 0o755); err != nil {
		totpMu.Unlock()
		return nil, err
	}
	lk, err := filelock.Acquire(totpConfigFile, totpLockTimeout)
	if err != nil {
		totpMu.Unlock()
		return nil, err
	}
	return func() {
		lk.Release()
		totpMu.Unlock()
	}, nil
}

// ---------- TOTP brute-force rate limiting ----------

var (
	totpAttempts        = make(map[string]int)
	totpLockouts        = make(map[string]time.Time)
	totpRateMu          sync.Mutex
	maxTOTPAttempts     = 5
	totpLockoutDuration = 15 * time.Minute
	totpMaxMapSize      = 10000
)

// checkTOTPRateLimit returns true if the IP is allowed to attempt TOTP
// verification, false if it is currently locked out.
func checkTOTPRateLimit(ip string) bool {
	totpRateMu.Lock()
	defer totpRateMu.Unlock()
	if lockUntil, ok := totpLockouts[ip]; ok && time.Now().Before(lockUntil) {
		return false // still locked out
	}
	delete(totpLockouts, ip) // lockout expired
	return true
}

// recordTOTPAttempt tracks a failed or successful TOTP attempt. On success
// the counter is reset; after maxTOTPAttempts failures the IP is locked out.
func recordTOTPAttempt(ip string, success bool) {
	totpRateMu.Lock()
	defer totpRateMu.Unlock()
	if success {
		delete(totpAttempts, ip)
		return
	}
	totpAttempts[ip]++
	if totpAttempts[ip] >= maxTOTPAttempts {
		totpLockouts[ip] = time.Now().Add(totpLockoutDuration)
		delete(totpAttempts, ip)
	}
	// Evict half the maps when they grow too large to bound memory.
	evictTOTPMapsLocked()
}

// evictTOTPMapsLocked removes roughly half the entries when either map
// exceeds the threshold. Must be called with totpRateMu held.
func evictTOTPMapsLocked() {
	if len(totpAttempts) > totpMaxMapSize {
		i := 0
		for k := range totpAttempts {
			if i >= len(totpAttempts)/2 {
				break
			}
			delete(totpAttempts, k)
			i++
		}
	}
	if len(totpLockouts) > totpMaxMapSize {
		now := time.Now()
		// First pass: remove expired entries.
		for k, v := range totpLockouts {
			if now.After(v) {
				delete(totpLockouts, k)
			}
		}
		// Second pass if still too large: remove half.
		if len(totpLockouts) > totpMaxMapSize {
			i := 0
			for k := range totpLockouts {
				if i >= len(totpLockouts)/2 {
					break
				}
				delete(totpLockouts, k)
				i++
			}
		}
	}
}

// ---------- TOTP core (RFC 6238) ----------

// generateTOTP computes a 6-digit TOTP code for the given secret and
// time step. The secret must be the raw bytes (not base32-encoded).
func generateTOTP(secret []byte, timeStep int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(timeStep))
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf)
	hash := mac.Sum(nil)
	offset := hash[len(hash)-1] & 0x0f
	code := binary.BigEndian.Uint32(hash[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000)
}

// validateTOTP checks the code against the current time step and ±1
// neighbours to tolerate clock drift (up to 30 seconds each way).
func validateTOTP(secret []byte, code string) bool {
	now := time.Now().Unix() / int64(totpPeriod)
	for _, offset := range []int64{-1, 0, 1} {
		if generateTOTP(secret, now+offset) == code {
			return true
		}
	}
	return false
}

// ---------- persistence ----------

// readTOTPConfig parses the config file and migrates any legacy plaintext
// backup codes into hashed form IN MEMORY ONLY. It never writes, so it is
// safe to call without holding the state lock — writers publish by rename,
// so a reader always sees one whole version of the file.
//
// The second return value reports whether the in-memory copy now differs
// from disk and is worth persisting.
func readTOTPConfig() (*totpConfig, bool, error) {
	data, err := os.ReadFile(totpConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &totpConfig{}, false, nil
		}
		return nil, false, err
	}
	var cfg totpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false, err
	}
	// Wave 26: migrate any legacy plaintext backup codes from disk into
	// hashed form on first read. The plaintext field is cleared so the
	// next saveTOTPConfig() call writes only the hash.
	migrated := false
	for i := range cfg.BackupCodes {
		bc := &cfg.BackupCodes[i]
		if bc.Hash == "" && bc.Code != "" {
			h, herr := bcrypt.GenerateFromPassword([]byte(strings.ToUpper(bc.Code)), bcrypt.DefaultCost)
			if herr != nil {
				// On hashing failure leave the entry alone — better a
				// stale plaintext on disk than a corrupted entry that
				// silently invalidates a backup code.
				continue
			}
			bc.Hash = string(h)
			bc.Code = ""
			migrated = true
		}
	}
	return &cfg, migrated, nil
}

// loadTOTPConfig reads the config and persists the legacy-plaintext
// migration if there was one.
//
// PRECONDITION: the caller holds the state lock from lockTOTPState. The
// migration write-back is a write, and an unserialized write here would
// erase a backup code UserService consumed a moment earlier. Callers that
// only want to look at the config (GetTOTPStatus) use readTOTPConfig and
// leave the write-back to the next locked caller.
func loadTOTPConfig() (*totpConfig, error) {
	cfg, migrated, err := readTOTPConfig()
	if err != nil {
		return nil, err
	}
	if migrated {
		_ = saveTOTPConfig(cfg) // best-effort; the in-memory copy is correct either way.
	}
	return cfg, nil
}

// saveTOTPConfig persists 2FA state to disk.
//
// Wave 35-Q: previously this was a naked os.WriteFile — no temp file,
// no fsync, no rename. Three failure modes for an admin who has
// actually enabled 2FA:
//  1. PostTOTPVerify writes the file under the totpMu lock, but a
//     power loss mid-write truncated the file and on next boot
//     loadTOTPConfig succeeded with Enabled=false (zero value) — the
//     admin was silently dropped out of 2FA and any attacker who
//     phished the password walked in.
//  2. PostTOTPValidate consumes a backup code (line 540) and crashes
//     between marking Used=true and the rename: the backup code can
//     be replayed forever, or every backup code is lost.
//  3. A concurrent reader during the write window saw a partial file
//     and json.Unmarshal failed.
//
// commonfile.WriteFileAtomic does the full write-tmp + fsync(file) +
// rename + fsync(parent dir) sequence, so the file is now either the
// previous content or the new content — never a half-written torn
// file.
//
// The rename is what makes a READER safe. It does nothing for a WRITER:
// two processes that both read version A and both rename leave whichever
// renamed last. PRECONDITION: the caller holds the state lock from
// lockTOTPState, taken before the read that produced cfg.
func saveTOTPConfig(cfg *totpConfig) error {
	// 0o755, matching the rest of the tree.
	//
	// /var/lib/casanova is SHARED: tokenrevocation creates it 0755 and depends
	// on every service being able to read its list — "they do not all run as
	// the same user" — and repairs the mode when it drifts. Creating it 0700
	// here means whichever subsystem wins the race decides whether the others
	// can read their own state. The secret lives in 2fa.json, written 0600.
	if err := os.MkdirAll(filepath.Dir(totpConfigFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return commonfile.WriteFileAtomic(totpConfigFile, data, 0o600)
}

// ---------- helpers ----------

func randomBase32Secret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// generateBackupCodes returns a fresh batch of recovery codes. The caller
// receives the persistent (hashed) form to store on disk and the plaintext
// list to show to the user *once*. The plaintext is never written back.
func generateBackupCodes(count int) ([]backupCode, []string) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	codes := make([]backupCode, count)
	plain := make([]string, count)
	for i := range codes {
		buf := make([]byte, backupCodeLen)
		for j := range buf {
			n, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(int64(len(charset))))
			if err != nil {
				// Extremely unlikely; degrade to first char rather than panic.
				buf[j] = charset[0]
				continue
			}
			buf[j] = charset[n.Int64()]
		}
		code := string(buf)
		plain[i] = code
		hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			// If bcrypt fails (process out of memory) the safer fallback
			// is an unusable code rather than a plaintext one.
			codes[i] = backupCode{Used: true}
			continue
		}
		codes[i] = backupCode{Hash: string(hash), Used: false}
	}
	return codes, plain
}

// matchBackupCode constant-time-compares a user-supplied code against the
// stored hash. Empty / never-set hashes never match.
func matchBackupCode(stored backupCode, candidate string) bool {
	if stored.Used || stored.Hash == "" || candidate == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(stored.Hash), []byte(candidate)) == nil
}

func buildOTPAuthURI(secret, account string) string {
	return fmt.Sprintf(
		"otpauth://totp/%s:%s?secret=%s&issuer=%s&digits=%d&period=%d",
		totpIssuer, account, secret, totpIssuer, totpDigits, totpPeriod,
	)
}

// ---------- HTTP handlers ----------

// GET /v1/sys/2fa/status — Check if 2FA is enabled.
func GetTOTPStatus(ctx echo.Context) error {
	totpMu.Lock()
	defer totpMu.Unlock()

	// Read-only: deliberately does NOT take the cross-process lock. This is
	// a report, and a report must not be able to fail because a peer holds
	// the lock. The atomic rename already gives the reader a whole file.
	cfg, _, err := readTOTPConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read 2FA config: " + err.Error(),
		})
	}

	remaining := 0
	for _, bc := range cfg.BackupCodes {
		if !bc.Used {
			remaining++
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"enabled":                cfg.Enabled && cfg.Verified,
			"backup_codes_remaining": remaining,
		},
	})
}

// POST /v1/sys/2fa/setup — Generate a new TOTP secret (pending verification).
//
// 2FA config is server-global (single /var/lib/casanova/2fa.json). Without
// this gate, any authenticated user — including a freshly registered guest
// — could overwrite the admin's pending secret, locking the admin out of
// their own setup. Restrict to PermSystemConfig (admin only).
func PostTOTPSetup(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	release, err := lockTOTPState()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "2FA config is busy, try again: " + err.Error(),
		})
	}
	defer release()

	cfg, err := loadTOTPConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read 2FA config: " + err.Error(),
		})
	}

	if cfg.Enabled && cfg.Verified {
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "2FA is already enabled. Disable it first.",
		})
	}

	secret, err := randomBase32Secret(20)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to generate secret: " + err.Error(),
		})
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "server"
	}

	cfg.Secret = secret
	cfg.Verified = false
	cfg.Enabled = false
	cfg.BackupCodes = nil

	if err := saveTOTPConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to save 2FA config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"secret": secret,
			"qr_uri": buildOTPAuthURI(secret, "admin@"+hostname),
		},
	})
}

// POST /v1/sys/2fa/verify — Verify a TOTP code and activate 2FA.
//
// Admin-only: pairs with PostTOTPSetup. A non-admin must never be able to
// flip the global 2FA Enabled flag.
func PostTOTPVerify(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	clientIP := ctx.RealIP()
	if !checkTOTPRateLimit(clientIP) {
		return ctx.JSON(http.StatusTooManyRequests, model.Result{
			Success: common_err.TOO_MANY_REQUEST,
			Message: "Too many attempts. Try again later.",
		})
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Invalid request body",
		})
	}

	body.Code = strings.TrimSpace(body.Code)
	if len(body.Code) != totpDigits {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Code must be 6 digits",
		})
	}

	release, err := lockTOTPState()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "2FA config is busy, try again: " + err.Error(),
		})
	}
	defer release()

	cfg, err := loadTOTPConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read 2FA config: " + err.Error(),
		})
	}

	if cfg.Secret == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "No pending 2FA setup. Call /v1/sys/2fa/setup first.",
		})
	}

	secretBytes, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(cfg.Secret)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Corrupt secret in config",
		})
	}

	if !validateTOTP(secretBytes, body.Code) {
		recordTOTPAttempt(clientIP, false)
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "Invalid code. Please try again.",
			Data: map[string]interface{}{
				"success": false,
			},
		})
	}

	recordTOTPAttempt(clientIP, true)

	// Code is valid — activate 2FA and generate backup codes.
	// Wave 26: generateBackupCodes returns hashed entries for disk and the
	// plaintext list to show the user exactly once.
	cfg.Enabled = true
	cfg.Verified = true
	hashed, plainCodes := generateBackupCodes(backupCodeNum)
	cfg.BackupCodes = hashed

	if err := saveTOTPConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to save 2FA config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"success":      true,
			"backup_codes": plainCodes,
		},
	})
}

// POST /v1/sys/2fa/validate — Validate a TOTP code (used at login).
func PostTOTPValidate(ctx echo.Context) error {
	clientIP := ctx.RealIP()
	if !checkTOTPRateLimit(clientIP) {
		return ctx.JSON(http.StatusTooManyRequests, model.Result{
			Success: common_err.TOO_MANY_REQUEST,
			Message: "Too many attempts. Try again later.",
		})
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Invalid request body",
		})
	}

	body.Code = strings.TrimSpace(body.Code)

	// The lock is taken before the read even though most calls only read:
	// a call that matches a backup code MUST consume it, and deciding on a
	// snapshot taken before the lock would let two callers accept the same
	// code. On lock failure the answer is "not valid" rather than a
	// wave-through — an unverifiable code must never be accepted.
	release, err := lockTOTPState()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "2FA config is busy, try again: " + err.Error(),
		})
	}
	defer release()

	cfg, err := loadTOTPConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read 2FA config: " + err.Error(),
		})
	}

	// Wave 26: do NOT silently return valid:true when 2FA is disabled.
	//
	// The previous behavior leaked the boundary: any caller could submit
	// any code (including the empty string) and receive `valid:true` as
	// long as 2FA wasn't enabled. A login flow that consumed this endpoint
	// after the password check would happily wave the user through. The
	// "is 2FA enabled" question is a separate concern from "is this code
	// correct" — surface it as its own field so the login flow can decide.
	if !cfg.Enabled || !cfg.Verified {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data: map[string]interface{}{
				"valid":   false,
				"enabled": false,
			},
		})
	}

	secretBytes, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(cfg.Secret)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Corrupt secret in config",
		})
	}

	// Check TOTP code first.
	if validateTOTP(secretBytes, body.Code) {
		recordTOTPAttempt(clientIP, true)
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    map[string]interface{}{"valid": true},
		})
	}

	// Check backup codes (case-insensitive). Wave 26: bcrypt-compare against
	// the stored hash; legacy plaintext entries were migrated on load.
	upper := strings.ToUpper(body.Code)
	for i, bc := range cfg.BackupCodes {
		if matchBackupCode(bc, upper) {
			cfg.BackupCodes[i].Used = true
			if err := saveTOTPConfig(cfg); err != nil {
				return ctx.JSON(http.StatusInternalServerError, model.Result{
					Success: common_err.SERVICE_ERROR,
					Message: "Failed to consume backup code: " + err.Error(),
				})
			}
			recordTOTPAttempt(clientIP, true)
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    map[string]interface{}{"valid": true},
			})
		}
	}

	recordTOTPAttempt(clientIP, false)
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]interface{}{"valid": false},
	})
}

// DELETE /v1/sys/2fa — Disable 2FA (requires a valid code).
//
// Admin-only on top of the TOTP code requirement: belt-and-suspenders, and
// keeps non-admin users from probing the disable endpoint at all.
func DeleteTOTP(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	clientIP := ctx.RealIP()
	if !checkTOTPRateLimit(clientIP) {
		return ctx.JSON(http.StatusTooManyRequests, model.Result{
			Success: common_err.TOO_MANY_REQUEST,
			Message: "Too many attempts. Try again later.",
		})
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Invalid request body",
		})
	}

	body.Code = strings.TrimSpace(body.Code)

	release, err := lockTOTPState()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "2FA config is busy, try again: " + err.Error(),
		})
	}
	defer release()

	cfg, err := loadTOTPConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read 2FA config: " + err.Error(),
		})
	}

	if !cfg.Enabled || !cfg.Verified {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "2FA is not enabled",
		})
	}

	secretBytes, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(cfg.Secret)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Corrupt secret in config",
		})
	}

	valid := validateTOTP(secretBytes, body.Code)
	if !valid {
		// Also check backup codes (Wave 26: bcrypt match).
		upper := strings.ToUpper(body.Code)
		for i, bc := range cfg.BackupCodes {
			if matchBackupCode(bc, upper) {
				cfg.BackupCodes[i].Used = true
				valid = true
				break
			}
		}
	}

	if !valid {
		recordTOTPAttempt(clientIP, false)
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "Invalid code. Please try again.",
		})
	}

	recordTOTPAttempt(clientIP, true)

	// Wipe 2FA state.
	cfg.Enabled = false
	cfg.Verified = false
	cfg.Secret = ""
	cfg.BackupCodes = nil

	if err := saveTOTPConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to save 2FA config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Two-factor authentication disabled",
	})
}
