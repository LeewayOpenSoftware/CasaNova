// Package oauthflow is the callback authority for provider OAuth flows whose
// completion has a privileged, persistent effect on this box.
//
// WHY THIS EXISTS (the trust model that was missing)
//
// GET /v1/recover/:type was written as if "whoever arrives here is the user who
// started the flow from the dashboard". Nothing enforced that. The provider does
// not call us back directly: the authorization URL sets
// redirect_uri=https://cloudoauth.files.casaos.app and puts the box's own URL in
// the OAuth `state` parameter, so `state` was a *redirect target consumed by a
// third party*, not a nonce. The browser that finally hits this box carries only
// `?code=…`, which means the callback had:
//
//   - no proof that any flow was ever started on this box,
//   - no binding to a principal,
//   - no binding to a provider (`:type` is a free path parameter),
//   - no single-use property, no expiry, no replay rejection.
//
// Combined with the v1 JWT skipper (loopback && no Authorization header) and a
// gateway that reverse-proxies every internet request over 127.0.0.1, the whole
// state machine was reachable unauthenticated from the internet: an attacker who
// authorizes their OWN cloud account against this build's client id gets a code,
// replays it at https://<box>/v1/recover/GoogleDrive?code=…, and the box persists
// the ATTACKER's refresh token into rclone's config and FUSE-mounts the
// attacker's drive under /mnt as root with AllowOther — re-mounted on every boot
// by CheckAndMountAll.
//
// This package supplies the missing half of the state machine:
//
//	Begin()   — an AUTHENTICATED principal registers an intent. Returns a
//	            256-bit state token; only its SHA-256 is ever stored.
//	Consume() — the callback presents the token. One-shot (removed under the
//	            same lock that reads it), TTL-bounded, provider-bound,
//	            principal-attributed.
//
// DURABILITY IS NOT AN AUTHORIZATION INPUT. The pending-flow set is mirrored to
// disk so a service restart mid-flow does not strand a user. If the store cannot
// be proven to be ours (ownership/mode/regular-file check, not an existence
// check), durability is switched off and the mirror is removed — the authority
// keeps running from memory, which is strictly less exposed, never more. No code
// path treats an unreadable, foreign, or absent store as a reason to accept a
// callback. Health names the invariant that failed.
package oauthflow

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
)

// DefaultStateDir is where the pending-flow mirror lives in production.
//
// It is a subdirectory of its own, NOT /var/lib/casanova itself, because
// provisioning asserts mode 0700 on the directory it owns. Asserting that on the
// shared state directory would silently re-permission every other subsystem's
// store (share links, alerts, TOTP) as a side effect of this one starting up.
const DefaultStateDir = "/var/lib/casanova/oauth"

const storeFileName = "oauth-flows.json"

const (
	// DefaultTTL bounds how long an authorization can sit in flight. A browser
	// round-trip through a consent screen is seconds to a couple of minutes;
	// ten minutes is generous and keeps the replay window small.
	DefaultTTL = 10 * time.Minute

	// stateBytes is the entropy of the state token. 256 bits — guessing is not
	// a threat model, and the token never leaves the initiating client except
	// through the provider round-trip.
	stateBytes = 32

	// maxPendingPerPrincipal bounds one principal's in-flight flows. Reaching it
	// evicts that principal's OWN oldest flow rather than refusing: a UI that
	// re-renders the "add cloud storage" dialog mints flows routinely, and a cap
	// that refuses would break the legitimate feature. Eviction is deliberately
	// scoped to the same principal so it can never be used to knock out someone
	// else's in-flight authorization.
	maxPendingPerPrincipal = 8

	// maxPendingTotal bounds the whole set. At the global cap we REFUSE the new
	// initiation instead of evicting, precisely so one principal cannot evict
	// another's flow. Refusal only ever hits an authenticated caller, never a
	// callback in flight, and it self-heals within DefaultTTL.
	maxPendingTotal = 256
)

// Provider is a canonical cloud provider identifier. The canonical spelling is
// the one that appears in the /v1/recover/:type path, because the authorization
// URL we mint is what puts it there.
type Provider string

const (
	ProviderGoogleDrive Provider = "GoogleDrive"
	ProviderDropbox     Provider = "Dropbox"
	ProviderOnedrive    Provider = "Onedrive"
)

// providerAliases maps every spelling any caller uses to the canonical one. The
// UI speaks "gdrive"/"dropbox"/"onedrive"; the legacy dashboard and the minted
// authorization URL speak "GoogleDrive"/"Dropbox"/"Onedrive". Matching is
// case-insensitive so a provider or relay that normalises case cannot turn a
// legitimate callback into a refusal.
var providerAliases = map[string]Provider{
	"googledrive":  ProviderGoogleDrive,
	"google_drive": ProviderGoogleDrive,
	"gdrive":       ProviderGoogleDrive,
	"drive":        ProviderGoogleDrive,
	"dropbox":      ProviderDropbox,
	"onedrive":     ProviderOnedrive,
	"one_drive":    ProviderOnedrive,
}

// CanonicalProvider resolves a caller-supplied provider string. The bool is
// false for anything unrecognised — callers must refuse rather than guess,
// because ":type" reaching a default branch is how an attacker picks the
// credential store they want to write to.
func CanonicalProvider(s string) (Provider, bool) {
	p, ok := providerAliases[strings.ToLower(strings.TrimSpace(s))]
	return p, ok
}

// RcloneType is the rclone backend name for a provider ("drive", "dropbox",
// "onedrive"). It is what the existing config records carry in their "type".
func (p Provider) RcloneType() string {
	switch p {
	case ProviderGoogleDrive:
		return "drive"
	case ProviderDropbox:
		return "dropbox"
	case ProviderOnedrive:
		return "onedrive"
	}
	return ""
}

// Slug is the provider fragment used in generated remote names. It reproduces
// the names the pre-authority handler generated ("_google_drive_", "_dropbox_",
// "_onedrive_") so existing mounts and generated ones stay in one namespace.
func (p Provider) Slug() string {
	switch p {
	case ProviderGoogleDrive:
		return "google_drive"
	case ProviderDropbox:
		return "dropbox"
	case ProviderOnedrive:
		return "onedrive"
	}
	return "unknown"
}

// Errors returned by Begin and Consume. Callers at the HTTP edge must collapse
// every Consume error into ONE indistinguishable refusal: the distinctions
// below are for logs and health counters, not for the caller of a callback.
var (
	ErrUnknownProvider  = errors.New("oauthflow: unknown provider")
	ErrNoPrincipal      = errors.New("oauthflow: no authenticated principal")
	ErrNoOrigin         = errors.New("oauthflow: no request origin")
	ErrTooManyPending   = errors.New("oauthflow: too many pending flows")
	ErrMalformedState   = errors.New("oauthflow: malformed state")
	ErrUnknownState     = errors.New("oauthflow: unknown or already-consumed state")
	ErrExpiredState     = errors.New("oauthflow: state expired")
	ErrProviderMismatch = errors.New("oauthflow: state was not issued for this provider")
)

// Flow is the record of one registered intent. Hash is the only representation
// of the state token that is ever stored or written to disk; copies handed back
// to callers have it cleared.
type Flow struct {
	ID            string    `json:"id"`
	Hash          string    `json:"hash"`
	Provider      Provider  `json:"provider"`
	PrincipalID   int       `json:"principal_id"`
	PrincipalRole string    `json:"principal_role"`
	Origin        string    `json:"origin"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// public returns a copy safe to hand outside the authority.
func (f Flow) public() Flow {
	f.Hash = ""
	return f
}

// persisted is the on-disk shape: pending flows plus the remote-provenance
// ledger, in one file so a single atomic write covers both.
type persisted struct {
	Flows  []Flow `json:"flows"`
	Ledger ledger `json:"ledger"`
}

// Authority is the callback authority. The zero value is not usable; use New or
// Default.
type Authority struct {
	mu    sync.Mutex
	dir   string
	flows map[string]*Flow // sha256-hex of state token -> flow

	// durable is false when the on-disk mirror could not be proven to be ours.
	// It gates PERSISTENCE ONLY. No authorization decision reads it.
	durable      bool
	storeFailure string

	ledger ledger

	issued   uint64
	consumed uint64
	rejected uint64
	evicted  uint64

	now func() time.Time
}

var (
	defaultMu   sync.Mutex
	defaultDir  = DefaultStateDir
	defaultAuth *Authority
)

// Configure points the process-wide authority at a different state directory
// and drops any authority already built. It is intended for tests and for
// early process start-up, before the first Default() call. Calling it while
// flows are in flight discards them; nothing in production does that.
func Configure(dir string) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultDir = dir
	defaultAuth = nil
}

// Default returns the process-wide authority, provisioning it on first use.
// Provisioning is idempotent: it creates the state directory if absent and
// re-asserts its mode if present, so a box that has never run this code and a
// box that has both end up in the same state with no operator action.
func Default() *Authority {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultAuth == nil {
		defaultAuth = New(defaultDir)
	}
	return defaultAuth
}

// New builds an authority over dir. It never fails: a directory that cannot be
// created, or that exists but is not exclusively ours, degrades the authority to
// memory-only and is reported by Health under the "state_store_ownership"
// invariant. It does not degrade any authorization check.
func New(dir string) *Authority {
	a := &Authority{
		dir:   dir,
		flows: make(map[string]*Flow),
		now:   time.Now,
		ledger: ledger{
			Created: make(map[string]RemoteRecord),
		},
	}
	if err := secureDir(dir); err != nil {
		a.storeFailure = err.Error()
		return a
	}
	a.durable = true
	a.loadLocked()
	return a
}

// secureDir provisions dir and PROVES it is ours — a regular directory, not a
// symlink, owned by this euid, with no group/other bits. Existence is not the
// property we need: a pre-created world-writable /var/lib/casanova would let a
// local unprivileged process seed pending flows for us to load at boot.
func secureDir(dir string) error {
	if dir == "" {
		return errors.New("empty state directory")
	}
	// Parents keep the conventional mode; only the leaf we own is locked down.
	if parent := filepath.Dir(dir); parent != dir {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", parent, err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	// From here on the leaf is addressed by DESCRIPTOR, never again by path.
	//
	// os.Chmod re-resolves the path and follows a final symlink, so doing it
	// before the check meant a symlink pre-planted at dir got the 0700 applied
	// to its TARGET: we re-permissioned a directory we had not yet decided to
	// trust, and only then refused it. Reordering the same two path calls would
	// merely narrow that window. Opening once with O_NOFOLLOW|O_DIRECTORY and
	// driving every check and the mode change through that one descriptor
	// removes it, because they all name the same open object — no second
	// resolution for anyone to redirect in between.
	//
	// Scope, stated honestly: O_NOFOLLOW covers the FINAL component only. The
	// parents are still resolved normally, so this defends dir against a
	// symlink planted at dir, not against a compromised /var/lib.
	//
	// A leaf we cannot open this way (symlink, not a directory, unreadable)
	// returns an error, which New() turns into a memory-only authority with
	// Health naming the invariant. That costs durability, never an
	// authorization check — no caller reads a.durable to decide a callback.
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", dir, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", dir, err)
	}
	// O_DIRECTORY already refused a non-directory and O_NOFOLLOW a symlink;
	// this restates it on the descriptor we actually hold rather than trusting
	// the open flags to have been honoured.
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Ownership is proven BEFORE we modify anything: a directory that is not
	// ours is one we refuse, not one we re-permission on the way to refusing.
	if err := ownedByUs(fi); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	// Mkdir applies umask on a new directory and is a no-op on an existing one,
	// so neither case is guaranteed to carry the mode we need. fchmod cannot be
	// redirected — it names the descriptor, not a path.
	if err := f.Chmod(0o700); err != nil {
		return fmt.Errorf("cannot set mode on %s: %w", dir, err)
	}
	fi, err = f.Stat()
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", dir, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to group/other (%#o)", dir, fi.Mode().Perm())
	}
	return nil
}

func (a *Authority) storePath() string {
	return filepath.Join(a.dir, storeFileName)
}

// loadLocked restores the mirror. Any failure — missing, foreign, malformed —
// leaves the authority empty rather than partially trusted. A file we cannot
// prove is ours is not read AND not written: durability is switched off so we
// never merge our state into someone else's file.
func (a *Authority) loadLocked() {
	path := a.storePath()
	fi, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			a.disableDurabilityLocked("state_store_unreadable: " + err.Error())
		}
		return
	}
	if !fi.Mode().IsRegular() {
		a.disableDurabilityLocked("state_store_not_regular_file")
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		a.disableDurabilityLocked(fmt.Sprintf("state_store_permissive_mode (%#o)", fi.Mode().Perm()))
		return
	}
	if err := ownedByUs(fi); err != nil {
		a.disableDurabilityLocked("state_store_foreign_owner: " + err.Error())
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		a.disableDurabilityLocked("state_store_unreadable: " + err.Error())
		return
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		// A corrupt mirror is discarded, not repaired. Pending flows are worth
		// at most DefaultTTL; guessing at a half-parsed one is not worth the
		// risk of resurrecting a record we do not understand.
		a.rewriteLocked()
		return
	}
	now := a.now()
	for i := range p.Flows {
		f := p.Flows[i]
		if f.Hash == "" || !f.ExpiresAt.After(now) {
			continue
		}
		a.flows[f.Hash] = &f
	}
	if p.Ledger.Created == nil {
		p.Ledger.Created = make(map[string]RemoteRecord)
	}
	a.ledger = p.Ledger
}

// disableDurabilityLocked turns off the mirror and removes it. Removal is the
// point: a stale file we refuse to write must not be reloaded on the next boot
// and treated as live pending flows.
func (a *Authority) disableDurabilityLocked(reason string) {
	a.durable = false
	if a.storeFailure == "" {
		a.storeFailure = reason
	}
	_ = os.Remove(a.storePath())
}

// rewriteLocked flushes the in-memory state to the mirror. A write failure
// disables durability (and removes the file) rather than leaving a mirror that
// disagrees with memory — a mirror holding a flow we have already consumed is
// exactly the replay window this package exists to close.
func (a *Authority) rewriteLocked() {
	if !a.durable {
		return
	}
	p := persisted{Ledger: a.ledger}
	for _, f := range a.flows {
		p.Flows = append(p.Flows, *f)
	}
	sort.Slice(p.Flows, func(i, j int) bool { return p.Flows[i].CreatedAt.Before(p.Flows[j].CreatedAt) })
	raw, err := json.Marshal(p)
	if err != nil {
		a.disableDurabilityLocked("state_store_marshal: " + err.Error())
		return
	}
	if err := commonfile.WriteFileAtomic(a.storePath(), raw, 0o600); err != nil {
		a.disableDurabilityLocked("state_store_write: " + err.Error())
	}
}

// pruneLocked drops expired flows. Called on every entry point so the set is
// bounded by DefaultTTL even if nothing ever completes.
func (a *Authority) pruneLocked() int {
	now := a.now()
	dropped := 0
	for h, f := range a.flows {
		if !f.ExpiresAt.After(now) {
			delete(a.flows, h)
			dropped++
		}
	}
	return dropped
}

// Begin registers an intent for an authenticated principal and returns the
// state token that binds the eventual callback to it. The token is returned
// once and never stored; only its SHA-256 is kept.
func (a *Authority) Begin(p Provider, principalID int, role, origin string) (string, Flow, error) {
	if p.RcloneType() == "" {
		return "", Flow{}, ErrUnknownProvider
	}
	if principalID <= 0 {
		return "", Flow{}, ErrNoPrincipal
	}
	if strings.TrimSpace(origin) == "" {
		return "", Flow{}, ErrNoOrigin
	}

	raw := make([]byte, stateBytes)
	if _, err := rand.Read(raw); err != nil {
		// Refuse rather than fall back to a weaker token. There is no
		// acceptable degraded state token.
		return "", Flow{}, fmt.Errorf("oauthflow: cannot generate state: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	idRaw := make([]byte, 8)
	if _, err := rand.Read(idRaw); err != nil {
		return "", Flow{}, fmt.Errorf("oauthflow: cannot generate flow id: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()

	a.evictOverflowForLocked(principalID)
	if len(a.flows) >= maxPendingTotal {
		// Deliberately a refusal and not a global eviction: evicting the oldest
		// flow across all principals would hand any principal holding
		// storage:write a way to cancel an administrator's in-flight
		// authorization. Refusing costs one authenticated caller a retry.
		return "", Flow{}, ErrTooManyPending
	}

	now := a.now()
	f := &Flow{
		ID:            hex.EncodeToString(idRaw),
		Hash:          hashState(token),
		Provider:      p,
		PrincipalID:   principalID,
		PrincipalRole: role,
		Origin:        origin,
		CreatedAt:     now,
		ExpiresAt:     now.Add(DefaultTTL),
	}
	a.flows[f.Hash] = f
	a.issued++
	a.rewriteLocked()
	return token, f.public(), nil
}

// evictOverflowForLocked keeps one principal's pending set under
// maxPendingPerPrincipal by dropping that principal's own oldest flows.
func (a *Authority) evictOverflowForLocked(principalID int) {
	var mine []*Flow
	for _, f := range a.flows {
		if f.PrincipalID == principalID {
			mine = append(mine, f)
		}
	}
	if len(mine) < maxPendingPerPrincipal {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt.Before(mine[j].CreatedAt) })
	for i := 0; i <= len(mine)-maxPendingPerPrincipal; i++ {
		delete(a.flows, mine[i].Hash)
		a.evicted++
	}
}

// Consume validates a callback's state token and burns it.
//
// The removal happens under the same lock as the lookup and BEFORE any
// validation verdict is returned, so a token is spendable exactly once even
// under concurrent callbacks, and a provider mismatch burns it too — a
// mismatch means the state was tampered with in transit, and leaving it live
// would let an attacker probe which provider a token belongs to.
func (a *Authority) Consume(state string, p Provider) (Flow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prunedByThisCall := a.pruneLocked()

	if !WellFormedState(state) {
		a.rejected++
		return Flow{}, ErrMalformedState
	}
	h := hashState(state)
	f, ok := a.flows[h]
	if !ok {
		a.rejected++
		// Persist ONLY if the prune above actually dropped something.
		//
		// This branch is reached by every forged callback, and GET
		// /v1/recover/:type is unauthenticated and internet-reachable: an
		// attacker needs nothing but a 43-character string. Rewriting the
		// durable mirror here meant each of those requests performed a full
		// atomic replace — temp file, write, fsync(file), rename, fsync(dir) —
		// while holding this mutex. Measured 10.97 ms/request against 1.86 us
		// for the same rejection without it: a 5884x remote amplification
		// primitive, introduced by the fix that closed the original hole.
		//
		// Correctness does not need it. An unknown state changes nothing; the
		// only possible change is the removal of flows that had ALREADY
		// expired, and an expired flow is refused on load anyway, so deferring
		// its erasure to the next real operation costs no authorization
		// property.
		if prunedByThisCall > 0 {
			a.rewriteLocked()
		}
		return Flow{}, ErrUnknownState
	}
	delete(a.flows, h)
	a.rewriteLocked()

	if !f.ExpiresAt.After(a.now()) {
		a.rejected++
		return Flow{}, ErrExpiredState
	}
	if f.Provider != p {
		a.rejected++
		return Flow{}, ErrProviderMismatch
	}
	a.consumed++
	return f.public(), nil
}

// Pending reports how many flows are in flight. Test and health use only.
func (a *Authority) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()
	return len(a.flows)
}

// WellFormedState is the cheap shape check applied before any lookup: 43
// base64url characters, which is what 32 random bytes encode to. It exists so a
// megabyte of junk in a query parameter never reaches the hash function.
func WellFormedState(s string) bool {
	if len(s) != base64.RawURLEncoding.EncodedLen(stateBytes) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
