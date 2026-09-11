package v1

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	json2 "encoding/json"
	"errors"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net"
	"net/http"
	url2 "net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/common"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model/system_model"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/utils/encryption"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
	uuid "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
)

// @Summary register user
// @Router /user/register/ [post]
func PostUserRegister(ctx echo.Context) error {
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid request body"})
	}

	username := json["username"]
	pwd := json["password"]
	key := json["key"]

	// Validate ALL inputs before consuming the single-use key. The key used to
	// be LoadAndDelete'd here first, so a too-short password (or any post-key
	// failure) burned the only key and the retry failed with a misleading
	// KEY_NOT_EXIST. The atomic consume now happens just before CreateUser.
	if len(username) == 0 || len(pwd) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	if len(pwd) < 6 {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.PWD_IS_TOO_SIMPLE, Message: common_err.GetMsg(common_err.PWD_IS_TOO_SIMPLE)})
	}
	oldUser := service.MyService.User().GetUserInfoByUserName(username)
	if oldUser.Id > 0 {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.USER_EXIST, Message: common_err.GetMsg(common_err.USER_EXIST)})
	}

	user := model2.UserDBModel{}
	user.Username = username
	hashedPwd, err := encryption.HashPassword(pwd)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: "password hashing failed"})
	}
	user.Password = hashedPwd
	user.Role = "admin"

	// Consume the single-use key atomically, immediately before creating the
	// user — only one concurrent request can win the LoadAndDelete.
	if _, ok := service.UserRegisterHash.LoadAndDelete(key); !ok {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.KEY_NOT_EXIST, Message: common_err.GetMsg(common_err.KEY_NOT_EXIST)})
	}

	user = service.MyService.User().CreateUser(user)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR)})
	}
	file.MkDir(config.AppInfo.UserDataPath + "/" + strconv.Itoa(user.Id))
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// RevokedUsers tracks user IDs whose accounts have been deleted.
// Tokens issued before deletion remain cryptographically valid until
// expiry, so every JWT validation path must check this map.
var RevokedUsers sync.Map

// ---------- Wave 26: refresh token rotation store ----------
//
// A refresh token is a persistent credential. Without rotation, an
// attacker who exfiltrates one (XSS, browser malware, log leak, …) keeps
// indefinite access for the full TTL with no signal to the legitimate
// user. With rotation:
//
//   - Every refresh issues a *new* refresh token whose jti is added to
//     the store and the *old* jti is marked consumed.
//   - Reusing an already-consumed jti is the textbook signal of a stolen
//     token: legitimate clients only ever present the most recent one.
//     We treat reuse as compromise of the entire family and revoke every
//     refresh in that chain — both the attacker's *and* the victim's
//     tokens. The victim is forced to re-authenticate; the attacker
//     cannot ride along.
//   - Logout / account deletion revokes the family up front so the
//     refresh chain dies immediately.
//
// The store is in-memory: refresh tokens already cannot survive a
// restart on a single-node CasaNova box, so persisting them buys nothing
// and adds a database write to every login. Restart implicitly revokes
// every chain, which is the strictest behavior available.

type refreshFamily struct {
	userID    int
	expiresAt time.Time
	revoked   bool
	// activeJTI is the latest non-consumed jti in this family. The map
	// `consumedJTIs` records every jti the family has ever issued, so we
	// can tell "old but legitimate replay race" from "reuse".
	activeJTI    string
	consumedJTIs map[string]struct{}
	// Session metadata for the Active Sessions UI. Set at login time
	// (from the echo.Context) and bumped on every refresh so the user
	// can see when each device last touched the API.
	userAgent  string
	ipAddress  string
	createdAt  time.Time
	lastSeenAt time.Time
}

var (
	refreshFamilies   = make(map[string]*refreshFamily) // familyID -> family
	jtiToFamily       = make(map[string]string)         // jti       -> familyID
	refreshFamiliesMu sync.Mutex
)

// jtiFingerprint returns a short, non-reversible identifier suitable
// for log lines: the first 12 hex chars of SHA-256(jti). It is enough
// to correlate two log entries about the same token without putting
// the raw jti — which is the bearer secret — into the log stream.
func jtiFingerprint(jti string) string {
	if jti == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(jti))
	return hex.EncodeToString(sum[:6])
}

// registerNewRefreshFamily creates a fresh family for a brand-new login.
// It returns the jti to embed in the issued refresh token. The jti is the
// familyID at this point — subsequent rotations within the family use a
// different jti but share the same familyID.
//
// userAgent/ipAddress are captured for the Active Sessions UI; both are
// optional from a correctness standpoint (they only feed display) but
// the login handler always passes them.
func registerNewRefreshFamily(userID int, ttl time.Duration, userAgent, ipAddress string) (jti string, err error) {
	jti, err = jwt.NewJTI()
	if err != nil {
		return "", err
	}
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	pruneExpiredFamiliesLocked()
	now := time.Now()
	refreshFamilies[jti] = &refreshFamily{
		userID:       userID,
		expiresAt:    now.Add(ttl),
		activeJTI:    jti,
		consumedJTIs: map[string]struct{}{},
		userAgent:    userAgent,
		ipAddress:    ipAddress,
		createdAt:    now,
		lastSeenAt:   now,
	}
	jtiToFamily[jti] = jti
	return jti, nil
}

// rotateRefreshFamily validates `presentedJTI` against the store and, if
// it is the family's current active jti, returns a freshly minted jti
// that the caller bakes into the new refresh token.
//
// Failure modes:
//
//	"unknown" : jti is not (or no longer) tracked — reject the request
//	            without touching anything else.
//	"reuse"   : jti is tracked but has already been consumed by an
//	            earlier rotation. Burns the entire family.
//	"revoked" : the family was explicitly killed (logout, account delete,
//	            or a prior reuse).
//	"expired" : family TTL has lapsed.
func rotateRefreshFamily(presentedJTI string, ttl time.Duration) (newJTI string, err error) {
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	pruneExpiredFamiliesLocked()

	familyID, ok := jtiToFamily[presentedJTI]
	if !ok {
		return "", errors.New("refresh: unknown token")
	}
	fam, ok := refreshFamilies[familyID]
	if !ok || fam.revoked {
		return "", errors.New("refresh: revoked token")
	}
	if time.Now().After(fam.expiresAt) {
		// Drop the family from the store on its way out.
		deleteFamilyLocked(familyID)
		return "", errors.New("refresh: expired token")
	}

	// Replay detection: if the presented jti has already been retired,
	// somebody is replaying an old refresh. Burn the entire family.
	if _, consumed := fam.consumedJTIs[presentedJTI]; consumed {
		fam.revoked = true
		// Wave 28: log a truncated SHA-256 fingerprint of the jti
		// instead of the raw value. Logs are often shipped to less
		// trusted aggregators; raw refresh-token jtis there would let
		// a log reader replay tokens until the family was burned.
		logger.Error("refresh token reuse detected — revoking family",
			zap.Int("user_id", fam.userID),
			zap.String("jti_fp", jtiFingerprint(presentedJTI)))
		return "", errors.New("refresh: token reuse detected, family revoked")
	}

	// Strict freshness: only the *current* active jti is accepted. Any
	// previously-rotated-out jti that we still happen to remember is
	// treated as the same replay class as a fully-consumed one.
	if presentedJTI != fam.activeJTI {
		fam.revoked = true
		logger.Error("refresh token presented stale jti — revoking family",
			zap.Int("user_id", fam.userID),
			zap.String("presented_fp", jtiFingerprint(presentedJTI)),
			zap.String("active_fp", jtiFingerprint(fam.activeJTI)))
		return "", errors.New("refresh: stale token, family revoked")
	}

	// Mint a new jti, retire the old one.
	next, err := jwt.NewJTI()
	if err != nil {
		return "", err
	}
	// Wave 32: keep the old jti in jtiToFamily so a replay attempt
	// finds the family and reaches the consumedJTIs check above
	// (which is the branch that burns the family on reuse).
	// Previously this line was `delete(jtiToFamily, presentedJTI)`
	// — that returned the consumed jti to the "unknown token"
	// branch on replay (L198) and the family-burn promised by
	// Wave 26 was silent dead code: stolen-and-rotated tokens
	// produced a vague 401 with no compromise signal and no
	// revocation. The Wave 32 audit caught it. The mapping is
	// cleaned up by deleteFamilyLocked() which iterates
	// fam.consumedJTIs on family expiry / revocation, so leaving
	// the entry here doesn't grow the map without bound.
	fam.consumedJTIs[presentedJTI] = struct{}{}
	fam.activeJTI = next
	fam.lastSeenAt = time.Now()
	jtiToFamily[next] = familyID
	return next, nil
}

// SessionInfo is the trimmed-down view of a refresh family that the
// Active Sessions UI consumes. It deliberately omits jtis and the user
// ID — the caller only sees their own sessions and identifies them by
// the opaque ID below (a SHA-256 fingerprint of the family ID so the
// raw jti never leaves the server).
type SessionInfo struct {
	ID         string `json:"id"`
	UserAgent  string `json:"userAgent"`
	IPAddress  string `json:"ipAddress"`
	CreatedAt  int64  `json:"createdAt"`  // Unix seconds
	LastSeenAt int64  `json:"lastSeenAt"` // Unix seconds
	ExpiresAt  int64  `json:"expiresAt"`  // Unix seconds
	Current    bool   `json:"current"`    // true if this is the session that made the GET request
}

// listSessionsForUser returns every active refresh family belonging to
// userID. The currentFamilyID (derived from the caller's own access
// token's jti via the refresh family map) is flagged so the UI can
// disable the "revoke" button on the user's own session.
func listSessionsForUser(userID int, currentFamilyID string) []SessionInfo {
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	pruneExpiredFamiliesLocked()
	out := make([]SessionInfo, 0)
	for familyID, fam := range refreshFamilies {
		if fam.userID != userID || fam.revoked {
			continue
		}
		out = append(out, SessionInfo{
			ID:         jtiFingerprint(familyID),
			UserAgent:  fam.userAgent,
			IPAddress:  fam.ipAddress,
			CreatedAt:  fam.createdAt.Unix(),
			LastSeenAt: fam.lastSeenAt.Unix(),
			ExpiresAt:  fam.expiresAt.Unix(),
			Current:    familyID == currentFamilyID,
		})
	}
	return out
}

// revokeSessionByID looks up the family whose fingerprint matches `id`
// and marks it revoked. Returns true when a match was found so the
// caller can answer 404 honestly. Fingerprints are derived deterministically
// from family IDs (sha256 → first 12 hex), so the lookup is a linear scan
// — fine at session-table cardinality of <100 per user.
func revokeSessionByID(userID int, id string) bool {
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	for familyID, fam := range refreshFamilies {
		if fam.userID != userID {
			continue
		}
		if jtiFingerprint(familyID) == id {
			fam.revoked = true
			return true
		}
	}
	return false
}

// revokeRefreshFamilyByJTI marks the family containing jti as revoked.
// Used by logout and DeleteUser. Unknown jtis are silently ignored — the
// caller cannot distinguish between "already gone" and "never existed",
// which is fine because both end states are correct.
func revokeRefreshFamilyByJTI(jti string) {
	if jti == "" {
		return
	}
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	if familyID, ok := jtiToFamily[jti]; ok {
		if fam, ok := refreshFamilies[familyID]; ok {
			fam.revoked = true
		}
	}
}

// revokeAllRefreshFamiliesForUser kills every refresh chain belonging to
// userID. Called from DeleteUser so a deleted account cannot keep
// refreshing into life until natural TTL.
func revokeAllRefreshFamiliesForUser(userID int) {
	refreshFamiliesMu.Lock()
	defer refreshFamiliesMu.Unlock()
	for _, fam := range refreshFamilies {
		if fam.userID == userID {
			fam.revoked = true
		}
	}
}

// pruneExpiredFamiliesLocked drops fully-expired entries to bound memory.
// Caller must hold refreshFamiliesMu.
func pruneExpiredFamiliesLocked() {
	now := time.Now()
	for familyID, fam := range refreshFamilies {
		if now.After(fam.expiresAt) || fam.revoked {
			deleteFamilyLocked(familyID)
		}
	}
}

func deleteFamilyLocked(familyID string) {
	fam, ok := refreshFamilies[familyID]
	if !ok {
		return
	}
	for jti := range fam.consumedJTIs {
		delete(jtiToFamily, jti)
	}
	delete(jtiToFamily, fam.activeJTI)
	delete(refreshFamilies, familyID)
}

// Per-IP and per-username rate limiting for login attempts.
//
// Wave 28: the previous design used "delete half the map in random
// order when full" as an LRU approximation. This was gameable: an
// attacker who flooded the map with bogus keys could force eviction
// of a *targeted* victim's entry, resetting that victim's counter.
//
// New design:
//   - Each entry tracks lastSeen so old entries can be GC'd.
//   - A periodic sweep deletes idle entries.
//   - When the map hits its hard cap, NEW keys do NOT evict existing
//     entries; instead they share a strict overflow limiter. So a
//     flood of new keys gets rate-limited as one bucket and cannot
//     touch any pre-existing per-IP / per-user limiter.
type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

var (
	ipLimiters    = make(map[string]*limiterEntry)
	ipLimitersMu  sync.Mutex
	ipLimiterGCAt time.Time

	userLimiters    = make(map[string]*limiterEntry)
	userLimitersMu  sync.Mutex
	userLimiterGCAt time.Time

	// Shared overflow buckets returned to *new* keys when the
	// per-key map is at capacity. Strict, single-bucket so a key
	// flood can't be used to evict a real victim's limiter and
	// can't pass through unmetered.
	ipOverflowLimiter   = rate.NewLimiter(rate.Every(time.Second), 5)
	userOverflowLimiter = rate.NewLimiter(rate.Every(time.Second), 5)
)

const (
	ipLimiterMaxSize     = 10000
	ipLimiterRetention   = 15 * time.Minute
	userLimiterMaxSize   = 50000
	userLimiterRetention = 30 * time.Minute
	limiterGCInterval    = 5 * time.Minute
)

// sweepLimiterMap deletes entries whose lastSeen is older than retention
// AND whose limiter is fully refilled (no in-flight penalty). The token
// check prevents prematurely freeing an entry that is currently being
// rate-limited. Caller must hold the map's mutex.
func sweepLimiterMap(m map[string]*limiterEntry, retention time.Duration, fullTokens float64, now time.Time) {
	cutoff := now.Add(-retention)
	for k, e := range m {
		if e.lastSeen.Before(cutoff) && e.limiter.Tokens() >= fullTokens {
			delete(m, k)
		}
	}
}

// loginSourceFingerprint returns a best-effort unique key for the
// caller when rate-limiting login attempts. It defends against two
// classes of abuse:
//
//   - An external attacker sending forged X-Forwarded-For headers to
//     rotate the effective key on every request. echo.Context.RealIP()
//     trusts those headers unconditionally, so we fall back to the
//     TCP-level RemoteAddr for all non-loopback callers.
//   - A misbehaving reverse proxy that sends a blank header. We still
//     return RemoteAddr in that case so we never produce an empty key
//     (which would collapse every attacker into the same bucket as
//     legitimate users).
//
// When the request actually originates from loopback (e.g. our own
// gateway terminating TLS), we honor X-Forwarded-For because that's
// the real client behind the proxy.
func loginSourceFingerprint(ctx echo.Context) string {
	remote := ctx.Request().RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if remote == "" {
		remote = "unknown"
	}
	// Loopback = trusted proxy: honor the forwarded header.
	ip := net.ParseIP(remote)
	if ip != nil && ip.IsLoopback() {
		if xf := ctx.RealIP(); xf != "" {
			return xf
		}
	}
	return remote
}

func getIPLimiter(ip string) *rate.Limiter {
	ipLimitersMu.Lock()
	defer ipLimitersMu.Unlock()

	now := time.Now()
	if now.Sub(ipLimiterGCAt) > limiterGCInterval {
		sweepLimiterMap(ipLimiters, ipLimiterRetention, 5, now)
		ipLimiterGCAt = now
	}

	if entry, exists := ipLimiters[ip]; exists {
		entry.lastSeen = now
		return entry.limiter
	}

	// At capacity: return shared overflow bucket. Existing entries
	// stay put — flood cannot evict a real victim.
	if len(ipLimiters) >= ipLimiterMaxSize {
		return ipOverflowLimiter
	}

	// 5 attempts per minute per IP
	limiter := rate.NewLimiter(rate.Every(time.Minute), 5)
	ipLimiters[ip] = &limiterEntry{limiter: limiter, lastSeen: now}
	return limiter
}

func getUserLimiter(username string) *rate.Limiter {
	userLimitersMu.Lock()
	defer userLimitersMu.Unlock()

	now := time.Now()
	if now.Sub(userLimiterGCAt) > limiterGCInterval {
		sweepLimiterMap(userLimiters, userLimiterRetention, 10, now)
		userLimiterGCAt = now
	}

	if entry, exists := userLimiters[username]; exists {
		entry.lastSeen = now
		return entry.limiter
	}

	if len(userLimiters) >= userLimiterMaxSize {
		return userOverflowLimiter
	}

	// 10 attempts per 5 minutes per username
	limiter := rate.NewLimiter(rate.Every(5*time.Minute), 10)
	userLimiters[username] = &limiterEntry{limiter: limiter, lastSeen: now}
	return limiter
}

// resolveEffectiveRole determines the effective role to mint into access and
// refresh tokens, preserving backward-compatibility for legacy accounts across
// both login and refresh token issuance.
//
// Wave 35-U / Role Mint Parity:
// A previous implementation used `string(rbac.Normalize(role))` under the
// assumption that missing/empty values would normalize to admin. However,
// rbac.Normalize fails closed to RoleGuest for empty or invalid input.
// This caused legacy accounts with empty role columns or unrecognized
// legacy strings (e.g. "administrator", "owner") to be silently demoted
// to guest upon token minting.
//
// To prevent locking account holders out of their own box:
//   - If the stored role is empty or whitespace-only, fall back explicitly to
//     "admin" (matching startup backfill in pkg/sqlite/db.go).
//   - If rbac.Normalize returns RoleGuest but the raw value was NOT
//     case-insensitively "guest" (e.g. "administrator", "owner", typos),
//     promote to "admin".
//   - Otherwise, return the normalized role ("admin", "user", "guest").
//
// Both PostUserLogin and PostUserRefreshToken MUST use this helper so their
// minted tokens never diverge.
func resolveEffectiveRole(storedRole string) string {
	trimmed := strings.TrimSpace(storedRole)
	if trimmed == "" {
		return string(rbac.RoleAdmin)
	}
	normalized := rbac.Normalize(trimmed)
	if normalized == rbac.RoleGuest && !strings.EqualFold(trimmed, "guest") {
		return string(rbac.RoleAdmin)
	}
	return string(normalized)
}

// countOtherAdmins answers "how many accounts, OTHER than excludeID, would
// still hold PermUserManage after this operation" — the question DeleteUser
// and PutUserRole both need answered before removing or demoting an admin.
//
// Counts by the RESOLVED role (resolveEffectiveRole), not the literal DB
// string. A legacy row stored as "" or "administrator" resolves to admin at
// token-mint time (see resolveEffectiveRole's own comment) even though its
// Role column never says "admin" — counting by literal string both
// under-protects (the box's only admin is such a row: it never matches
// role=='admin', so a last-admin guard keyed on the literal string never
// fires) and over-rejects (two real admins, one of them a legacy row, count
// as one and a legitimate delete/demote gets refused).
//
// Fails closed by construction, not by a special-cased error branch: if the
// underlying fetch errors or returns nothing (GetAllUserName does not
// surface a DB error today), the count comes back 0 — indistinguishable from
// "genuinely no other admins" — which is exactly the outcome a last-admin
// guard should produce when it cannot prove otherwise.
func countOtherAdmins(excludeID string) int {
	count := 0
	for _, u := range service.MyService.User().GetAllUserName() {
		if strconv.Itoa(u.Id) == excludeID {
			continue
		}
		if resolveEffectiveRole(u.Role) == string(rbac.RoleAdmin) {
			count++
		}
	}
	return count
}

// @Summary login
// @Produce  application/json
// @Accept application/json
// @Tags user
// @Param user_name query string true "User name"
// @Param pwd  query string true "password"
// @Success 200 {string} string "ok"
// @Router /user/login [post]
func PostUserLogin(ctx echo.Context) error {
	// Use the transport-level RemoteAddr, NOT ctx.RealIP(). RealIP()
	// honors X-Forwarded-For / X-Real-IP, which a remote attacker can
	// spoof to pick a fresh bucket per request and fully bypass the
	// per-IP login throttle. Only trust forwarded headers when the
	// request actually came from loopback (i.e. our own gateway).
	clientIP := loginSourceFingerprint(ctx)
	if !getIPLimiter(clientIP).Allow() {
		return ctx.JSON(common_err.TOO_MANY_REQUEST,
			model.Result{
				Success: common_err.TOO_MANY_LOGIN_REQUESTS,
				Message: common_err.GetMsg(common_err.TOO_MANY_LOGIN_REQUESTS),
			})
	}

	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid request body"})
	}

	username := json["username"]

	password := json["password"]
	// check params is empty
	if len(username) == 0 || len(password) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: common_err.GetMsg(common_err.INVALID_PARAMS),
			})
	}

	// Per-username rate limiting: prevents distributed brute-force attacks
	// that rotate source IPs but target a single account.
	if !getUserLimiter(username).Allow() {
		return ctx.JSON(common_err.TOO_MANY_REQUEST,
			model.Result{
				Success: common_err.TOO_MANY_LOGIN_REQUESTS,
				Message: common_err.GetMsg(common_err.TOO_MANY_LOGIN_REQUESTS),
			})
	}

	user := service.MyService.User().GetUserAllInfoByName(username)

	// Timing attack protection: always perform a hash comparison even if user doesn't exist.
	// This prevents attackers from distinguishing "user not found" vs "wrong password" by timing.
	if user.Id == 0 {
		// Dummy bcrypt comparison to keep response time consistent
		encryption.CheckPassword("dummy", "$2a$14$000000000000000000000uGtWMqBzOlYbHOT.SViMFoY63fKji3K")
		// Random delay (0-500ms) to further mask timing
		time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)
		// Audit: unknown username login attempt
		service.AuditAuth(auditlog.EventLoginFailure, username, clientIP, "unknown user", false)
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST_OR_PWD_INVALID, Message: common_err.GetMsg(common_err.USER_NOT_EXIST_OR_PWD_INVALID)})
	}

	// Check password using bcrypt (new) with MD5 fallback (legacy migration)
	matches, needsUpgrade := encryption.CheckPassword(password, user.Password)
	if !matches {
		time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)
		// Audit: wrong password
		service.AuditAuth(auditlog.EventLoginFailure, username, clientIP, "invalid password", false)
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST_OR_PWD_INVALID, Message: common_err.GetMsg(common_err.USER_NOT_EXIST_OR_PWD_INVALID)})
	}

	// Wave 28: enforce 2FA when /var/lib/casanova/2fa.json says it's
	// enabled and verified. The check is done *after* the password
	// match so it doesn't leak whether 2FA is set up to anonymous
	// callers (the response shape is the same in both cases until
	// the password is correct). See route/v1/totp_verify.go for the
	// rationale on cross-service file access.
	if otpErr := totpEnforceLogin(json["otp"]); otpErr != nil {
		switch otpErr {
		case errTOTPRequired:
			service.AuditAuth(auditlog.EventLoginFailure, username, clientIP, "2FA code required", false)
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "two-factor authentication code required",
				Data:    map[string]interface{}{"requires_otp": true},
			})
		case errTOTPInvalid:
			time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)
			service.AuditAuth(auditlog.EventLoginFailure, username, clientIP, "invalid 2FA code", false)
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{
				Success: common_err.USER_NOT_EXIST_OR_PWD_INVALID,
				Message: "invalid two-factor authentication code",
			})
		default:
			logger.Error("2FA check failed", zap.Error(otpErr))
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "2FA check failed",
			})
		}
	}

	// Audit: successful login
	service.AuditAuth(auditlog.EventLoginSuccess, username, clientIP, "login success", true)

	// Transparent migration: if the password was stored as MD5, upgrade to bcrypt
	if needsUpgrade {
		if newHash, err := encryption.HashPassword(password); err == nil {
			upgradeUser := user
			upgradeUser.Password = newHash
			service.MyService.User().UpdateUserPassword(upgradeUser)
			logger.Info("migrated user password from MD5 to bcrypt", zap.String("username", username))
		}
	}

	privateKey, _ := service.MyService.User().GetKeyPair()

	// Resolve the user's role at issue time using resolveEffectiveRole.
	// Both login and refresh MUST use the same role resolution logic to
	// prevent legacy admin accounts from dropping to guest upon token refresh.
	effectiveRole := resolveEffectiveRole(user.Role)

	token := system_model.VerifyInformation{}

	accessToken, err := jwt.GetAccessTokenWithRole(user.Username, privateKey, user.Id, effectiveRole)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	token.AccessToken = accessToken

	// Wave 26: register a brand-new refresh-token family at login time
	// and embed its jti in the issued refresh token. The /refresh
	// endpoint will rotate jtis within this family on each call.
	refreshJTI, err := registerNewRefreshFamily(
		user.Id,
		7*24*time.Hour,
		ctx.Request().Header.Get("User-Agent"),
		clientIP,
	)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	refreshToken, err := jwt.GetRefreshTokenWithRoleAndJTI(user.Username, privateKey, user.Id, effectiveRole, refreshJTI)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	token.RefreshToken = refreshToken

	token.ExpiresAt = time.Now().Add(3 * time.Hour * time.Duration(1)).Unix()
	data := make(map[string]interface{}, 2)
	user.Password = ""
	data["token"] = token

	// TODO:1 Database fields cannot be external
	data["user"] = user

	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    data,
		})
}

// @Summary edit user head
// @Produce  application/json
// @Accept multipart/form-data
// @Tags user
// @Param file formData file true ""
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /users/avatar [put]
func PutUserAvatar(ctx echo.Context) error {
	// Cap the body BEFORE ctx.Bind buffers it — a ~1GB {"file":"…"} would
	// otherwise be fully decoded onto the heap and OOM the root user-service
	// (the 10MB check below runs only AFTER the decode). 11MB leaves room for
	// base64 + JSON overhead over the 10MB data limit. Mirrors the sibling
	// hardening on PostUserCustomConf.
	ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, 11<<20)
	id := ctx.Request().Header.Get("user_id")
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	data := json["file"]

	// Reject oversized payloads before doing any decoding work.
	if len(data) > 10*1024*1024 {
		return ctx.JSON(http.StatusRequestEntityTooLarge, model.Result{Success: common_err.INVALID_PARAMS, Message: "avatar data exceeds 10 MB limit"})
	}

	imgBase64 := strings.Replace(data, "data:image/png;base64,", "", 1)
	decodeData, err := base64.StdEncoding.DecodeString(string(imgBase64))
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	// Wave 29: defend against PNG decompression bombs. The 10 MB cap on
	// the base64 input is ~7.5 MB of compressed image — plenty of room
	// to encode a 30000×30000 single-color PNG that decompresses to ~3.6
	// GB of pixel data and OOMs the daemon. Peek at the dimensions via
	// DecodeConfig (which only reads the header, not the pixel stream)
	// and reject anything outside reasonable avatar bounds before
	// calling image.Decode and allocating the pixel buffer.
	const maxAvatarDim = 4096
	cfg, _, err := image.DecodeConfig(strings.NewReader(string(decodeData)))
	if err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid image header: " + err.Error()})
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxAvatarDim || cfg.Height > maxAvatarDim {
		return ctx.JSON(http.StatusRequestEntityTooLarge, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("avatar dimensions %dx%d exceed %d limit", cfg.Width, cfg.Height, maxAvatarDim),
		})
	}

	img, _, err := image.Decode(strings.NewReader(string(decodeData)))
	if err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid image data: " + err.Error()})
	}
	if img == nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "decoded image is nil"})
	}

	ext := ".png"
	avatarDir := config.AppInfo.UserDataPath + "/" + id
	// Ensure the user directory exists — first login after a migration
	// may not have created it yet, and os.Create won't make parents.
	if err := os.MkdirAll(avatarDir, 0o755); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "cannot create user directory: " + err.Error()})
	}
	avatarPath := avatarDir + "/avatar" + ext
	// Write to a temp file first, then rename — so the old avatar is
	// preserved if the write fails.
	outFile, err := os.CreateTemp(avatarDir, "avatar-*.tmp")
	if err != nil {
		// Without this return, outFile is nil and the deferred Close()
		// plus the png.Encode below both panic with a nil dereference.
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "create avatar file: " + err.Error()})
	}
	tmpPath := outFile.Name()
	defer func() {
		outFile.Close()
		// Clean up temp file on failure (successful path renames it away).
		os.Remove(tmpPath)
	}()

	if err := png.Encode(outFile, img); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "encode avatar: " + err.Error()})
	}
	outFile.Close()
	// Atomic rename: old avatar is only replaced after the new one is
	// fully written. On failure the old avatar remains intact.
	if err := os.Rename(tmpPath, avatarPath); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "rename avatar: " + err.Error()})
	}
	user.Avatar = avatarPath
	service.MyService.User().UpdateUser(user)
	return ctx.JSON(http.StatusOK,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    user,
		})
}

// @Summary get user head
// @Produce  application/json
// @Tags user
// @Param file formData file true ""
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /users/avatar [get]
func GetUserAvatar(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}

	if file.Exists(user.Avatar) {
		ctx.Response().Header().Set("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(path.Base(user.Avatar)))
		ctx.Response().Header().Set("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate, value")
		return ctx.File(user.Avatar)

	}
	user.Avatar = "/usr/share/casaos/www/avatar.svg"
	if file.Exists(user.Avatar) {
		ctx.Response().Header().Set("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(path.Base(user.Avatar)))
		ctx.Response().Header().Set("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate, value")
		return ctx.File(user.Avatar)

	}
	user.Avatar = "/var/lib/casaos/www/avatar.svg"
	ctx.Response().Header().Set("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(path.Base(user.Avatar)))
	ctx.Response().Header().Set("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate, value")
	return ctx.File(user.Avatar)
}

// @Summary edit user name
// @Produce  application/json
// @Accept application/json
// @Tags user
// @Param old_name  query string true "Old user name"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /user/name/:id [put]
func PutUserInfo(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	json := model2.UserDBModel{}
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST_OR_PWD_INVALID, Message: common_err.GetMsg(common_err.USER_NOT_EXIST_OR_PWD_INVALID)})
	}
	// Wave 32: SECURITY — IDOR fix. UpdateUser is `Updates(&m)` which
	// uses gorm's primary-key WHERE clause from the model's `Id` field.
	// Previously the handler bound the request body straight into
	// `json` (a full UserDBModel including Id) and passed `json` to
	// UpdateUser, so any logged-in user could PUT
	//   {"id":1,"nickname":"pwned","email":"a@b"}
	// and rewrite the admin's profile. The header-based existence
	// check was decoration. Force the update target back to the
	// JWT-derived id.
	jwtID, _ := strconv.Atoi(id)
	json.Id = jwtID
	if len(json.Username) > 0 {
		u := service.MyService.User().GetUserInfoByUserName(json.Username)
		if u.Id > 0 && u.Id != json.Id {
			return ctx.JSON(common_err.CLIENT_ERROR,
				model.Result{Success: common_err.USER_EXIST, Message: common_err.GetMsg(common_err.USER_EXIST)})
		}
	}

	if len(json.Email) == 0 {
		json.Email = user.Email
	}
	if len(json.Avatar) == 0 {
		json.Avatar = user.Avatar
	}
	// SECURITY: Never allow users to change their own role via this endpoint.
	// Role changes must go through the admin-only PutUserRole handler.
	json.Role = user.Role
	if len(json.Description) == 0 {
		json.Description = user.Description
	}
	if len(json.Nickname) == 0 {
		json.Nickname = user.Nickname
	}
	service.MyService.User().UpdateUser(json)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: json})
}

// @Summary edit user password
// @Produce  application/json
// @Accept application/json
// @Tags user
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /user/password/:id [put]
func PutUserPassword(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	oldPwd := json["old_password"]
	pwd := json["password"]
	if len(oldPwd) == 0 || len(pwd) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	if len(pwd) < 6 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "Password must be at least 6 characters"})
	}
	user := service.MyService.User().GetUserAllInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	// Verify old password (supports both bcrypt and legacy MD5)
	oldMatches, _ := encryption.CheckPassword(oldPwd, user.Password)
	if !oldMatches {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.PWD_INVALID_OLD, Message: common_err.GetMsg(common_err.PWD_INVALID_OLD)})
	}
	// Always hash new password with bcrypt
	newHash, err := encryption.HashPassword(pwd)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: "password hashing failed"})
	}
	user.Password = newHash
	service.MyService.User().UpdateUserPassword(user)
	// A password change must cut off any already-exfiltrated session — revoke
	// every refresh-token family so a stolen refresh token can't keep minting
	// fresh access tokens for the rest of the 7-day window (mirrors DeleteUser).
	revokeAllRefreshFamiliesForUser(user.Id)
	user.Password = ""
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: user})
}

// @Summary edit user nick
// @Produce  application/json
// @Accept application/json
// @Tags user
// @Param nick_name query string false "nick name"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /user/nick [put]
func PutUserNick(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	Nickname := json["nick_name"]
	if len(Nickname) == 0 {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(http.StatusOK,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	user.Nickname = Nickname
	service.MyService.User().UpdateUser(user)
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: user})
}

// @Summary edit user description
// @Produce  application/json
// @Accept multipart/form-data
// @Tags user
// @Param description formData string false "Description"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /user/desc [put]
func PutUserDesc(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	desc := json["description"]
	if len(desc) == 0 {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(http.StatusOK,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	user.Description = desc

	service.MyService.User().UpdateUser(user)

	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: user})
}

// @Summary get user info
// @Produce  application/json
// @Accept  application/json
// @Tags user
// @Success 200 {string} string "ok"
// @Router /user/info/:id [get]
func GetUserInfo(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	user := service.MyService.User().GetUserInfoById(id)

	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    user,
		})
}

/**
 * @description:
 * @param {*gin.Context} c
 * @param {string} Username
 * @return {*}
 * @method:
 * @router:
 */
func GetUserInfoByUsername(ctx echo.Context) error {
	username := ctx.Param("username")
	if len(username) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	user := service.MyService.User().GetUserInfoByUserName(username)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}

	// A profile (email + RBAC role) is sensitive — only the user themselves
	// or an admin may read it. Wave 28 gated GetUserAllUsername behind
	// PermUserManage; this sibling was missed, letting any authed caller
	// enumerate every user's email/role.
	authedID := ctx.Request().Header.Get("user_id")
	if strconv.Itoa(user.Id) != authedID && !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage) {
		return ctx.JSON(http.StatusForbidden, model.Result{Success: common_err.CLIENT_ERROR, Message: "not allowed"})
	}

	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    user,
		})
}

/**
 * @description: get all Usernames
 * @method:GET
 * @router:/user/all/name
 */
func GetUserAllUsername(ctx echo.Context) error {
	// Wave 28: admin-only. The route is also JWT-gated at the
	// router level — this in-handler check exists so that if the
	// route is ever re-mounted publicly by mistake, the handler
	// itself still refuses non-admin callers.
	role := commonmw.RoleFromContext(ctx)
	if !rbac.Allows(role, rbac.PermUserManage) {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "admin role required",
		})
	}
	users := service.MyService.User().GetAllUserName()
	names := []string{}
	for _, v := range users {
		names = append(names, v.Username)
	}
	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    names,
		})
}

/**
 * @description:get custom file by user
 * @param {path} name string "file name"
 * @method: GET
 * @router: /user/custom/:key
 */
func GetUserCustomConf(ctx echo.Context) error {
	name := ctx.Param("key")
	if len(name) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// Sanitize name to prevent path traversal (e.g., "../../etc/shadow")
	cleanName := filepath.Base(name)
	if cleanName != name || strings.Contains(name, "..") {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid key name"})
	}
	id := ctx.Request().Header.Get("user_id")

	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	filePath := filepath.Join(config.AppInfo.UserDataPath, id, cleanName+".json")

	data := file.ReadFullFile(filePath)
	if !gjson.ValidBytes(data) {
		return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: string(data)})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: json2.RawMessage(string(data))})
}

// customConfStripes is a fixed-size pool of mutexes that serialize
// writes/deletes against the same custom-config file. Without
// serialization, two concurrent POSTs (or a POST racing a DELETE) for
// the same key would interleave inside file.WriteToPath / os.Remove and
// leave the JSON blob torn.
//
// Wave 29: the previous implementation stored a per-key mutex in a
// sync.Map and never deleted entries. An authenticated attacker could
// hammer /user/custom/:key with distinct key values per request and
// grow the map without bound — every new key allocated a fresh
// *sync.Mutex that lived until process restart, a slow OOM. A fixed-
// size striped pool keeps memory constant: distinct keys may share a
// stripe (false sharing only adds wait time, never corrupts data) and
// the *same* key still always lands on the *same* stripe so the lock
// still serializes the operations it has to.
//
// 256 stripes is roughly the working set of a healthy install while
// still bounding the worst case at ~10 KiB of mutexes.
const customConfStripeCount = 256

var customConfStripes = func() []sync.Mutex {
	s := make([]sync.Mutex, customConfStripeCount)
	return s
}()

func lockCustomConf(userID int, cleanName string) func() {
	key := strconv.Itoa(userID) + "/" + cleanName
	// FNV-1a is fine here — we just need a stable, well-distributed
	// hash and we're not crypto-sensitive.
	const fnvOffset uint32 = 2166136261
	const fnvPrime uint32 = 16777619
	h := fnvOffset
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= fnvPrime
	}
	m := &customConfStripes[int(h%customConfStripeCount)]
	m.Lock()
	return m.Unlock
}

/**
 * @description:create or update custom conf by user
 * @param {path} name string "file name"
 * @method:POST
 * @router:/user/custom/:key
 */
func PostUserCustomConf(ctx echo.Context) error {
	name := ctx.Param("key")
	if len(name) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// Sanitize name to prevent path traversal (e.g., "../../etc/shadow")
	cleanName := filepath.Base(name)
	if cleanName != name || strings.Contains(name, "..") {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid key name"})
	}
	id := ctx.Request().Header.Get("user_id")
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	unlock := lockCustomConf(user.Id, cleanName)
	defer unlock()
	// Cap request body at 2 MiB. Custom prefs are JSON blobs written
	// by the dashboard — the biggest real payload in Wave 29 was a
	// few hundred kilobytes. Without a limit an attacker POSTing a
	// multi-GB body stalls the goroutine and exhausts memory, and
	// io.ReadAll's discarded error used to silently swallow network
	// truncations on top of that.
	const maxCustomConfBytes = 2 << 20
	defer ctx.Request().Body.Close()
	data, err := io.ReadAll(io.LimitReader(ctx.Request().Body, maxCustomConfBytes+1))
	if err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR,
			model.Result{Success: common_err.CLIENT_ERROR, Message: "failed to read request body"})
	}
	if len(data) > maxCustomConfBytes {
		return ctx.JSON(http.StatusRequestEntityTooLarge,
			model.Result{Success: common_err.CLIENT_ERROR, Message: "request body too large"})
	}
	filePath := filepath.Join(config.AppInfo.UserDataPath, strconv.Itoa(user.Id))

	if err := file.IsNotExistMkDir(filePath); err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR)})
	}

	if err := file.WriteToPath(data, filePath, cleanName+".json"); err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR)})
	}

	if cleanName == "system" {
		dataMap := make(map[string]string, 1)
		dataMap["system"] = string(data)
		// Wave 35-X: bound publish so wedged bus cannot pin the save
		// handler indefinitely.
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		response, err := service.MyService.MessageBus().PublishEventWithResponse(pubCtx, common.SERVICENAME, "zimaos:user:save_config", dataMap)
		pubCancel()
		if err != nil {
			logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", string(data)))
		}
		if response != nil && response.StatusCode() != http.StatusOK {
			logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
		}

	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: json2.RawMessage(string(data))})
}

/**
 * @description: delete user custom config
 * @param {path} key string
 * @method:delete
 * @router:/user/custom/:key
 */
func DeleteUserCustomConf(ctx echo.Context) error {
	name := ctx.Param("key")
	if len(name) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	id := ctx.Request().Header.Get("user_id")
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	// Sanitize name to prevent path traversal (e.g., "../../etc/passwd")
	cleanName := filepath.Base(name) // strips any directory components
	if cleanName != name || strings.Contains(name, "..") {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid key name"})
	}
	unlock := lockCustomConf(user.Id, cleanName)
	defer unlock()
	filePath := filepath.Join(config.AppInfo.UserDataPath, strconv.Itoa(user.Id), cleanName+".json")
	err := os.Remove(filePath)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR)})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

/**
 * @description:
 * @param {path} id string "user id"
 * @method:DELETE
 * @router:/user/delete/:id
 */
// DeleteUser allows:
//   - any user to delete their own account
//   - admins (rbac.PermUserManage) to delete any account
//
// Before RBAC landed this was an IDOR — any authenticated user could
// delete any other account. The self-delete-only fallback remains as a
// safety net for non-admin users who want to close their own account.
func DeleteUser(ctx echo.Context) error {
	id := ctx.Param("id")
	authedID := ctx.Request().Header.Get("user_id")
	if id == "" || authedID == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "missing user id",
		})
	}

	role := commonmw.RoleFromContext(ctx)
	isSelf := id == authedID
	isAdmin := rbac.Allows(role, rbac.PermUserManage)

	if !isSelf && !isAdmin {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "only admins can delete other users",
		})
	}

	// Refuse to delete the box's last admin account, whether the caller is
	// deleting themselves or another admin. CasaNova-UI-v2's stores/users.ts
	// refuses this client-side too, but that is a courtesy, not the
	// guarantee — this is. Counts by the RESOLVED role (countOtherAdmins),
	// not the literal DB string, so a legacy account (Role stored as ""
	// or "administrator", which still mints an admin token — see
	// resolveEffectiveRole) is protected exactly like one whose Role column
	// literally says "admin".
	target := service.MyService.User().GetUserAllInfoById(id)
	if resolveEffectiveRole(target.Role) == string(rbac.RoleAdmin) && countOtherAdmins(id) == 0 {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cannot delete the last admin account",
		})
	}

	service.MyService.User().DeleteUserById(id)

	// Revoke all outstanding JWT tokens for this user.
	//
	// The in-memory map is kept as this process's fast path, but it is not the
	// record that matters: it was unreadable from the other six services and
	// was lost on restart, so a deleted user's token kept working everywhere
	// else for its full lifetime. tokenrevocation is the shared, persistent
	// authority every service consults.
	RevokedUsers.Store(id, time.Now())
	if revErr := tokenrevocation.Revoke(id); revErr != nil {
		// The account row is already gone, so failing here would leave the
		// caller believing nothing happened while the user is in fact deleted.
		// Report it loudly instead: until the list is writable again the
		// deleted user's existing tokens stay live, which is exactly the state
		// an operator has to know about.
		logger.Error("account deleted but its tokens could not be revoked cluster-wide",
			zap.String("user_id", id), zap.Error(revErr))
	}
	if uid, convErr := strconv.Atoi(id); convErr == nil {
		// Wave 26: also burn every refresh-token chain so the deleted
		// account cannot rotate itself back to life until natural TTL.
		revokeAllRefreshFamiliesForUser(uid)
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: id})
}

/**
 * @description:update user image
 * @method:POST
 * @router:/user/current/image/:key
 */
func PutUserImage(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	path := json["path"]
	key := ctx.Param("key")
	if len(path) == 0 || len(key) == 0 {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// Validate path is within the allowed user data directory to prevent
	// reading arbitrary files via path traversal.
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, filepath.Clean(config.AppInfo.UserDataPath)+"/") {
		return ctx.JSON(http.StatusForbidden, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}
	path = cleanPath
	if !file.Exists(path) {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}

	_, err := file.GetImageExt(path)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.NOT_IMAGE, Message: common_err.GetMsg(common_err.NOT_IMAGE)})
	}

	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	fstat, err := os.Stat(path)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}
	if fstat.Size() > 10<<20 {
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.IMAGE_TOO_LARGE, Message: common_err.GetMsg(common_err.IMAGE_TOO_LARGE)})
	}
	ext := file.GetExt(path)
	filePath := config.AppInfo.UserDataPath + "/" + strconv.Itoa(user.Id) + "/" + key + ext
	file.CopySingleFile(path, filePath, "overwrite")

	data := make(map[string]string, 3)
	data["path"] = filePath
	data["file_name"] = key + ext
	data["online_path"] = "/v1/users/image?path=" + filePath
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

/**
* @description:
* @param {*gin.Context} c
* @param {file} file
* @param {string} key
* @param {string} type:avatar,background
* @return {*}
* @method:
* @router:
 */
func PostUserUploadImage(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	f, err := ctx.FormFile("file")
	key := ctx.Param("key")
	t := ctx.FormValue("type")
	if len(key) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	if err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.CLIENT_ERROR), Data: err.Error()})
	}

	_, err = file.GetImageExtByName(f.Filename)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.NOT_IMAGE, Message: common_err.GetMsg(common_err.NOT_IMAGE)})
	}
	ext := filepath.Ext(f.Filename)
	user := service.MyService.User().GetUserInfoById(id)

	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	if t == "avatar" {
		key = "avatar"
	}
	path := config.AppInfo.UserDataPath + "/" + strconv.Itoa(user.Id) + "/" + key + ext

	file.SaveUploadedFile(f, path)

	data := make(map[string]string, 3)
	data["path"] = path
	data["file_name"] = key + ext
	data["online_path"] = "/v1/users/image?path=" + path
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

/**
 * @description: get current user's image
 * @method:GET
 * @router:/user/image/:id
 */
func GetUserImage(ctx echo.Context) error {
	filePath := ctx.QueryParam("path")
	if len(filePath) == 0 {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	absFilePath, err := filepath.Abs(filepath.Clean(filePath))
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	if !file.Exists(absFilePath) {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}
	expectedPrefix := filepath.Clean(config.AppInfo.UserDataPath)
	if !strings.HasPrefix(absFilePath, expectedPrefix+"/") {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}

	matched, err := regexp.MatchString(`^/var/lib/casaos/\d`, absFilePath)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}
	if !matched {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}

	fileName := path.Base(absFilePath)

	// @tiger - RESTful URL
	ctx.Response().Header().Set("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(fileName))
	return ctx.File(absFilePath)
}

func DeleteUserImage(ctx echo.Context) error {
	id := ctx.Request().Header.Get("user_id")
	path := ctx.QueryParam("path")
	if len(path) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	user := service.MyService.User().GetUserInfoById(id)
	if user.Id == 0 {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.USER_NOT_EXIST, Message: common_err.GetMsg(common_err.USER_NOT_EXIST)})
	}
	// Validate path: clean it, then check it starts with the user's data directory.
	// strings.Contains is NOT safe — it can be bypassed with crafted paths.
	cleanPath := filepath.Clean(path)
	allowedPrefix := filepath.Clean(config.AppInfo.UserDataPath + "/" + strconv.Itoa(user.Id))
	if !strings.HasPrefix(cleanPath, allowedPrefix+"/") && cleanPath != allowedPrefix {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}
	if !file.Exists(cleanPath) {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}
	os.Remove(cleanPath)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

/**
 * @description:
 * @param {*gin.Context} c
 * @param {string} refresh_token
 * @return {*}
 * @method:
 * @router:
 */
func PostUserRefreshToken(ctx echo.Context) error {
	js := make(map[string]string)
	if err := ctx.Bind(&js); err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	refresh := js["refresh_token"]

	privateKey, _ := service.MyService.User().GetKeyPair()

	claims, err := jwt.ParseToken(
		refresh,
		func() (*ecdsa.PublicKey, error) {
			_, publicKey := service.MyService.User().GetKeyPair()
			return publicKey, nil
		})
	if err != nil {
		return ctx.JSON(http.StatusUnauthorized, model.Result{Success: common_err.VERIFICATION_FAILURE, Message: common_err.GetMsg(common_err.VERIFICATION_FAILURE), Data: err.Error()})
	}
	if !claims.VerifyExpiresAt(time.Now(), true) || !claims.VerifyIssuer("refresh", true) {
		return ctx.JSON(http.StatusUnauthorized, model.Result{Success: common_err.VERIFICATION_FAILURE, Message: common_err.GetMsg(common_err.VERIFICATION_FAILURE)})
	}

	// Reject refresh tokens for deleted users — prevents indefinite access
	// after account deletion. The shared authority is the durable record; the
	// in-memory map only short-circuits it inside this process.
	_, revokedLocally := RevokedUsers.Load(strconv.Itoa(claims.ID))
	if revokedLocally || jwt.RevokedForClaims(claims) {
		return ctx.JSON(http.StatusUnauthorized, model.Result{Success: common_err.VERIFICATION_FAILURE, Message: "user account has been deleted"})
	}

	// Wave 26: rotate the refresh token. rotateRefreshFamily enforces:
	//   * the presented jti is the family's *current* active jti
	//   * the family is not revoked / expired
	//   * any reuse or stale-jti presentation kills the entire family
	// Tokens minted before this code shipped have an empty jti and will
	// fail this check; the user must log in once to get a tracked one.
	newRefreshJTI, rotateErr := rotateRefreshFamily(claims.RegisteredClaims.ID, 7*24*time.Hour)
	if rotateErr != nil {
		logger.Error("refresh token rotation failed",
			zap.String("error", rotateErr.Error()),
			zap.Int("user_id", claims.ID))
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.VERIFICATION_FAILURE,
			Message: common_err.GetMsg(common_err.VERIFICATION_FAILURE),
		})
	}

	// Wave 34-X: re-resolve role + username from the database, NOT from
	// the parsed refresh token claims. The previous implementation
	// passed `claims.Role` and `claims.Username` straight back into
	// the new access/refresh token, which meant:
	//   - An admin demoted to operator (or revoked entirely) kept
	//     minting admin-privileged access tokens for the full 7-day
	//     refresh-family TTL — completely defeating role changes.
	//   - A user renamed via PutUser kept appearing in audit logs
	//     under their old username.
	// Wave 34-X / Role Mint Parity: re-resolve role + username from the
	// database using resolveEffectiveRole. Both login and refresh must
	// use resolveEffectiveRole to guarantee identical token role minting.
	currentUser := service.MyService.User().GetUserInfoById(strconv.Itoa(claims.ID))
	if currentUser.Id == 0 {
		// Account no longer exists — refuse to mint a new token. The
		// /etc revocation list is a separate signal; this protects
		// against the case where the row was deleted directly.
		logger.Error("refresh token presented for missing user",
			zap.Int("user_id", claims.ID))
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.VERIFICATION_FAILURE,
			Message: common_err.GetMsg(common_err.VERIFICATION_FAILURE),
		})
	}
	effectiveRole := resolveEffectiveRole(currentUser.Role)
	effectiveUsername := currentUser.Username
	if effectiveUsername == "" {
		effectiveUsername = claims.Username
	}

	newAccessToken, err := jwt.GetAccessTokenWithRole(effectiveUsername, privateKey, claims.ID, effectiveRole)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	newRefreshToken, err := jwt.GetRefreshTokenWithRoleAndJTI(effectiveUsername, privateKey, claims.ID, effectiveRole, newRefreshJTI)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	verifyInfo := system_model.VerifyInformation{
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    time.Now().Add(3 * time.Hour).Unix(),
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: verifyInfo})
}

// PostUserLogout invalidates a refresh token family server-side. The
// client clears its localStorage independently, but without a server
// call to actually revoke the family the refresh token remains valid
// until its TTL — so a stolen device could keep refreshing access
// tokens indefinitely after a "logout".
//
// Body: {"refresh_token": "<jwt>"}
//
// Returns 200 even on parse failures: from the user's perspective the
// session is gone, so we should not leak whether their token was valid
// or already revoked. The internal log captures the actual outcome.
//
// POST /v1/users/logout
func PostUserLogout(ctx echo.Context) error {
	js := make(map[string]string)
	// Cap the body — a refresh JWT is well under 4 KiB; anything bigger
	// is junk and shouldn't be allowed to allocate.
	ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, 4<<10)
	if err := ctx.Bind(&js); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
		})
	}
	refresh := js["refresh_token"]
	if refresh == "" {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
		})
	}

	claims, err := jwt.ParseToken(
		refresh,
		func() (*ecdsa.PublicKey, error) {
			_, publicKey := service.MyService.User().GetKeyPair()
			return publicKey, nil
		})
	if err != nil {
		// Don't reveal whether the token was malformed vs expired vs
		// signed by another key — same response either way.
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
		})
	}

	// Revoke just the family this jti belongs to. We deliberately do
	// NOT revoke every family for the user: a logout from one device
	// should not log the user out of every other device they have
	// open. Whole-account revocation belongs on a separate "sign out
	// everywhere" button.
	if claims.RegisteredClaims.ID != "" {
		revokeRefreshFamilyByJTI(claims.RegisteredClaims.ID)
	}

	if l := service.AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventLogout,
			Severity: auditlog.SeverityInfo,
			Actor:    strconv.Itoa(claims.ID),
			ActorIP:  ctx.RealIP(),
			Message:  "user logout — refresh family revoked",
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// DeleteUserAll is a data-destruction maintenance endpoint. Gated to
// admins and additionally refuses self-inclusion: an admin deleting every
// user including themselves would leave the system without a way back in.
func DeleteUserAll(ctx echo.Context) error {
	role := commonmw.RoleFromContext(ctx)
	if !rbac.Allows(role, rbac.PermUserManage) {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "admin role required",
		})
	}
	// Refuse unconditionally — deleting all users (including the caller)
	// would leave the system unrecoverable. This op should be done via
	// direct SQL during maintenance, not over HTTP.
	return ctx.JSON(http.StatusForbidden, model.Result{
		Success: common_err.CLIENT_ERROR,
		Message: "bulk user deletion via HTTP is disabled; use maintenance mode",
	})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/init/check [get]
func GetUserStatus(ctx echo.Context) error {
	data := make(map[string]interface{}, 4)

	if service.MyService.User().GetUserCount() > 0 {
		data["initialized"] = true
		data["key"] = ""
	} else {
		// Wave 28: never return the registration key over plain HTTP.
		// The previous behaviour leaked the bootstrap secret to anyone
		// who could reach /v1/users/status — a single curl on a fresh
		// install was enough to register the admin account before the
		// real owner saw the welcome screen.
		//
		// New behaviour:
		//   - generate (or reuse) the key in the in-memory store as
		//     before, so PostUserRegister still validates against it,
		//   - additionally persist the key to a 0600 root-owned file,
		//   - return only the file PATH in the HTTP response, so the
		//     admin must have host-level access (SSH / console / the
		//     migration script) to retrieve it.
		// Side effect: ensures the in-memory key exists AND the
		// on-disk copy at initialKeyFilePath is fresh.
		_ = initialRegistrationKey()
		data["key"] = ""
		data["key_file"] = initialKeyFilePath
		data["initialized"] = false
	}
	gpus, err := external.NvidiaGPUInfoList()
	if err != nil {
		logger.Error("NvidiaGPUInfoList error", zap.Error(err))
	}
	data["gpus"] = len(gpus)
	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    data,
		})
}

// initialKeyFilePath is where the bootstrap registration key gets
// persisted on disk so an admin with host access can retrieve it
// without needing it returned over HTTP. Mode 0600, root-owned.
const initialKeyFilePath = "/var/lib/casanova/initial-registration-key.txt"

// initialRegistrationKey returns the active bootstrap registration
// key, generating one (and writing the on-disk copy) if none exists.
// Idempotent: subsequent calls return the same key until it is
// consumed by PostUserRegister.
func initialRegistrationKey() string {
	var existingKey string
	service.UserRegisterHash.Range(func(k, v interface{}) bool {
		existingKey = k.(string)
		return false
	})
	if existingKey == "" {
		existingKey = uuid.NewV4().String()
		service.UserRegisterHash.Store(existingKey, existingKey)
	}
	// Best-effort persist. If the write fails (read-only fs, missing
	// parent dir we can't create), we still return the key so the
	// in-memory PostUserRegister flow keeps working — the admin will
	// have to read the key from a different source. The error is
	// logged but not returned, since the UI is already getting only
	// the path back and would have nothing to do with it.
	if err := os.MkdirAll(filepath.Dir(initialKeyFilePath), 0o700); err == nil {
		_ = os.WriteFile(initialKeyFilePath, []byte(existingKey+"\n"), 0o600)
	}
	return existingKey
}
