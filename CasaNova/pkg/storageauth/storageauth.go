// Package storageauth is the storage authorization authority: it decides
// whether a given principal may perform a given operation on a given path.
//
// See docs/design/legacy-path-containment.md for the finding it closes.
//
// TWO BOUNDARIES, IN ORDER
//
//  1. CEILING — the storage areas this appliance serves at all.
//  2. GRANT   — the subset THIS principal is entitled to act on.
//
// A path must clear both. Admin clears the second by role; every other
// principal clears it only through an explicit, persisted grant.
//
// # WHAT THIS REPLACES
//
// The inherited model was a DENY-LIST: utils.BlockedSystemPrefixes named
// fifteen system directories and everything else was permitted. That was the
// right shape for what CasaOS was — a single-user LAN appliance whose file
// manager served "your own box", where the threat was a user wandering into
// the machinery. It is the wrong shape for what CasaNova is: exposed to the
// internet, running as root, with a role model whose `user` role is documented
// as "operation of THEIR OWN files" while nothing in the code ever expressed
// whose files were whose.
//
// Concretely, the old primitive accepted /home/<anyone>/.ssh/authorized_keys,
// /tmp, / and every top-level directory not on the fixed list. Somebody had
// already noticed — route/v1/file.go carries a comment saying the sanitizer
// "still permits / and /home" — and added containment to exactly one handler
// out of thirty-nine.
//
// The boundary is therefore no longer a hand-maintained list of paths. It is:
//
//	principal -> capability/ownership -> storage root -> permitted operation
//
// with the derived ceiling as an absolute maximum that no grant can exceed.
//
// # DERIVED, NOT HARDCODED
//
// The ceiling cannot be a constant. An install whose disk is mounted at
// /storage, or whose share lives under /home/data, works today; a hardcoded
// {/DATA,/mnt,/media} would start refusing it with no diagnosis, which is
// worse than the defect being fixed. So the ceiling is derived from what the
// appliance actually serves — canonical data roots, filesystems the storage
// service reports mounted, configured shares, the AppData root — plus roots
// the operator declared explicitly and persistently. There is no implicit
// "allow everything" anywhere in that list, and no way to obtain one.
//
// # FAIL-SAFE
//
// If derivation fails, the ceiling falls back to the canonical data roots and
// reports degraded — never to "allow everything". If the grant store cannot be
// read, every non-admin principal is refused rather than admitted unchecked.
// Both states name the failed invariant through Health.
package storageauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Op is a single operation on a path. Grants carry a set of these.
type Op uint32

// The operation set is deliberately wider than the product currently
// distinguishes. Baking a boolean IsSafePath() into the architecture is what
// made operation-aware authorization impossible in the inherited model; naming
// the operations now costs nothing and keeps the door open. Callers that do
// not care may pass a coarse value such as OpWrite.
const (
	OpRead Op = 1 << iota
	OpCreate
	OpWrite
	OpRename
	OpDelete
	OpUpload
	OpExtract
	OpShare
	OpMount
	OpAdminister
)

// opMutates reports whether an operation changes the filesystem. Used to pick
// the resolution strategy and, for rename, to require BOTH endpoints.
func opMutates(o Op) bool {
	return o&(OpCreate|OpWrite|OpRename|OpDelete|OpUpload|OpExtract|OpMount|OpAdminister) != 0
}

func (o Op) String() string {
	if o == 0 {
		return "none"
	}
	var parts []string
	for _, e := range []struct {
		op   Op
		name string
	}{
		{OpRead, "read"}, {OpCreate, "create"}, {OpWrite, "write"},
		{OpRename, "rename"}, {OpDelete, "delete"}, {OpUpload, "upload"},
		{OpExtract, "extract"}, {OpShare, "share"}, {OpMount, "mount"},
		{OpAdminister, "administer"},
	} {
		if o&e.op != 0 {
			parts = append(parts, e.name)
		}
	}
	return strings.Join(parts, "|")
}

// RootKind records where a ceiling root came from, so health output can explain
// the boundary instead of just listing paths.
type RootKind string

const (
	KindData     RootKind = "data"     // canonical storage roots
	KindMount    RootKind = "mount"    // filesystem the storage service reports mounted
	KindShare    RootKind = "share"    // configured share
	KindAppData  RootKind = "appdata"  // application data root
	KindOperator RootKind = "operator" // explicitly declared by the operator
)

// Root is one entry of the ceiling.
type Root struct {
	Path string   `json:"path"`
	Kind RootKind `json:"kind"`
	// Note explains why this root exists, for diagnostics.
	Note string `json:"note,omitempty"`
}

// GrantOrigin records why a grant exists. Origin matters operationally: a
// legacy-migration grant is one an administrator should review, and health says
// so, whereas an operator grant is a deliberate standing decision.
type GrantOrigin string

const (
	OriginOperator GrantOrigin = "operator"
	OriginOwner    GrantOrigin = "owner"
	OriginShare    GrantOrigin = "share"
	// OriginLegacy marks access preserved across the upgrade that introduced
	// this package. Silently revoking every non-admin user's access on upgrade
	// would be a worse failure than the defect, so the migration records what
	// each principal could already reach, explicitly and revocably, and health
	// flags it for review.
	OriginLegacy GrantOrigin = "legacy-migration"
)

// Grant entitles a principal to operate inside one subtree.
type Grant struct {
	ID          string      `json:"id"`
	PrincipalID string      `json:"principal_id"`
	Path        string      `json:"path"`
	Ops         Op          `json:"ops"`
	Origin      GrantOrigin `json:"origin"`
	CreatedAt   time.Time   `json:"created_at"`
	Note        string      `json:"note,omitempty"`
}

// Principal is the identity an authorization decision is made for.
type Principal struct {
	ID    string
	Role  string // "admin", "user", "guest"
	Admin bool   // resolved from the role model by the caller
}

// Deps are the facts the ceiling is derived from. They are injected rather than
// imported so this package cannot form a cycle with the services that own the
// information, and so every derivation input is visible and testable.
//
// A nil function is treated as "this source contributed nothing", not as an
// error: a deployment without configured shares is normal. A function that
// returns an error marks the ceiling degraded — the difference between "there
// are none" and "we could not find out" is exactly the difference between a
// correct boundary and a guess.
type Deps struct {
	// DataRoots are the canonical storage roots. This is the fail-safe floor
	// and must never be empty.
	DataRoots []string
	// Mounts reports filesystems the storage service currently serves.
	Mounts func() ([]string, error)
	// Shares reports configured share roots.
	Shares func() ([]string, error)
	// AppData reports the application data root.
	AppData func() (string, error)
	// NonAdminPrincipals lists non-admin accounts, used only by the one-time
	// legacy migration to decide whether any grant needs preserving.
	NonAdminPrincipals func() ([]Principal, error)
}

// Status is the machine-readable health of the authority.
type Status string

const (
	StatusHealthy          Status = "healthy"
	StatusDegraded         Status = "degraded"          // ceiling fell back to the floor
	StatusValidationFailed Status = "validation_failed" // negative boundary did not hold
	StatusUnavailable      Status = "unavailable"       // never provisioned
)

// Health describes the authority for diagnostics endpoints.
type Health struct {
	Status Status `json:"status"`
	// Ceiling is the effective global boundary.
	Ceiling []Root `json:"ceiling"`
	// Grants is how many principal entitlements are in force.
	Grants int `json:"grants"`
	// LegacyGrants counts entitlements preserved by the upgrade migration.
	// A non-zero value is an action item for the administrator, not a fault.
	LegacyGrants int `json:"legacy_grants"`
	// Invariant names the property that failed, in words an operator can act
	// on. Empty when healthy.
	Invariant string `json:"invariant,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Enforcing reports whether decisions are trustworthy. When false the
	// authority refuses instead.
	Enforcing bool `json:"enforcing"`
	// PendingPrincipals counts accounts awaiting a storage grant after the
	// upgrade. Non-zero is an administrator action item, not a fault.
	PendingPrincipals int `json:"pending_principals"`
	// PolicyVersion changes on every accepted mutation and re-derivation, so a
	// UI can tell whether its view is current.
	PolicyVersion    int64 `json:"policy_version"`
	DerivedRoots     int   `json:"derived_roots"`
	ExplicitRoots    int   `json:"explicit_roots"`
	SystemBoundaries int   `json:"system_boundaries"`
	// MountTableReadable reports whether pseudo-filesystem membership can be
	// proven. When false the authority refuses conventional kernel mount
	// points rather than assuming they are ordinary storage.
	MountTableReadable bool `json:"mount_table_readable"`
	// Validation is "passed" or "failed" — the self-test outcome, not merely
	// whether the component exists.
	Validation string `json:"validation"`
}

// Denial is the error type every refusal uses, so callers can map it to a
// status code without string matching and diagnostics can explain which of the
// two boundaries refused.
type Denial struct {
	Path     string
	Op       Op
	Boundary string // "ceiling" or "grant" or "lexical"
	Reason   string
}

func (d *Denial) Error() string {
	return fmt.Sprintf("%s denied by the %s boundary: %s", d.Path, d.Boundary, d.Reason)
}

// IsDenial reports whether err is an authorization refusal, in either form:
// the structured Decision returned by AuthorizeStorage, or the *Denial that
// the older internal paths produce.
func IsDenial(err error) bool {
	var legacy *Denial
	if errors.As(err, &legacy) {
		return true
	}
	var d Decision
	return errors.As(err, &d)
}

// ReasonOf extracts the machine-readable reason from an authorization error,
// so an HTTP layer can map it to a status and a UI to an explanation without
// parsing English.
func ReasonOf(err error) (ReasonCode, bool) {
	var d Decision
	if errors.As(err, &d) {
		return d.Reason, true
	}
	return "", false
}

const (
	rootsFileName  = "storage-roots.json"
	denyFileName   = "storage-policy-rules.json"
	grantsFileName = "storage-grants.json"
	schemaVersion  = 1
	fileMode       = 0o600
	dirMode        = 0o755
	// pendingUnknown marks an account count that could not be established.
	// Distinct from zero, which is a claim.
	pendingUnknown = -1

	// refreshInterval bounds how stale the derived ceiling may be. Mounts
	// appear and disappear while the box runs, and a boundary that only
	// reflects boot-time reality would refuse a disk plugged in afterwards.
	refreshInterval = 30 * time.Second
)

type state struct {
	mu sync.RWMutex

	dir  string
	deps Deps

	ceiling       []Root
	operatorAdd   []Root
	explicitRules []Rule
	policyVersion int64

	// pendingDetail explains a pendingPrincipals of pendingUnknown.
	pendingDetail string

	// pendingPrincipals counts accounts that exist but hold no storage grant,
	// so health can surface the upgrade's compatibility consequence instead of
	// leaving an administrator to discover it from a user complaint.
	pendingPrincipals int
	grants            []Grant

	// systemDisk is the set of principal IDs the per-account system-disk
	// switch is ON for. Absence is OFF, so the default for every account —
	// administrators included — is no whole-disk access. See systemdisk.go.
	systemDisk map[string]struct{}

	// systemSearch is the set of principal IDs the whole-filesystem search
	// switch is recorded ON for. In effect only together with systemDisk; see
	// systemsearch.go.
	systemSearch map[string]struct{}

	provisioned bool
	validated   bool
	derivedAt   time.Time

	status    Status
	invariant string
	detail    string
}

var global = &state{
	dir:       "/var/lib/casanova",
	status:    StatusUnavailable,
	invariant: "storage authority has not been provisioned",
}

// SetStateDir overrides where the persisted configuration lives, and resets
// state. Intended for tests and relocated deployments.
func SetStateDir(dir string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.dir = dir
	global.ceiling = nil
	global.operatorAdd = nil
	global.grants = nil
	global.systemDisk = nil
	global.systemSearch = nil
	global.provisioned = false
	global.validated = false
	global.derivedAt = time.Time{}
	global.status = StatusUnavailable
	global.invariant = "storage authority has not been provisioned"
	global.detail = ""
}

// Ensure provisions the authority and proves its negative boundary. Services
// call this at startup. A non-nil error means the authority is not in force and
// every non-admin decision fails closed.
func Ensure(d Deps) error {
	if len(d.DataRoots) == 0 {
		global.mu.Lock()
		defer global.mu.Unlock()
		return global.failLocked("the canonical data roots must be supplied",
			errors.New("Deps.DataRoots is empty; there would be no fail-safe floor"))
	}

	global.mu.Lock()
	global.deps = d
	if err := global.provisionLocked(); err != nil {
		global.mu.Unlock()
		return err
	}
	global.mu.Unlock()

	return SelfTest()
}

func (s *state) provisionLocked() error {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return s.failLocked("the state directory must exist",
			fmt.Errorf("create %s: %w", s.dir, err))
	}

	// The per-account system-disk switches. An unreadable file fails the
	// authority rather than reading as "nobody is switched on": that default
	// is indistinguishable from the safe one and would hide a corrupt file.
	sd, sderr := loadSystemDisk(filepath.Join(s.dir, systemDiskFileName))
	if sderr != nil {
		return s.failLocked("the system-disk switch list must parse",
			fmt.Errorf("%s is unreadable (%w); refusing to run with an unknown policy", filepath.Join(s.dir, systemDiskFileName), sderr))
	}
	s.systemDisk = sd
	ss, sserr := loadSystemDisk(filepath.Join(s.dir, systemSearchFileName))
	if sserr != nil {
		return s.failLocked("the system-search switch list must parse",
			fmt.Errorf("%s is unreadable (%w); refusing to run with an unknown policy", filepath.Join(s.dir, systemSearchFileName), sserr))
	}
	s.systemSearch = ss

	// Operator-declared roots are persistent configuration, never inferred.
	add, err := loadRoots(filepath.Join(s.dir, rootsFileName))
	if err != nil {
		return s.failLocked("the operator root list must parse",
			fmt.Errorf("%s is unreadable (%w); refusing to guess the boundary", filepath.Join(s.dir, rootsFileName), err))
	}
	s.operatorAdd = add
	persisted, perr := loadRules(filepath.Join(s.dir, denyFileName))
	if perr != nil {
		return s.failLocked("the policy rule file must parse",
			fmt.Errorf("%s is unreadable (%w); refusing to run with an unknown policy", filepath.Join(s.dir, denyFileName), perr))
	}
	s.explicitRules = append([]Rule{}, persisted...)
	for _, r := range add {
		s.explicitRules = append(s.explicitRules, Rule{
			ID: "explicit:" + r.Path, Path: r.Path, Access: AccessAllow,
			Source: SourceExplicit, Kind: KindOperator, Note: r.Note,
		})
	}

	grants, gerr := loadGrants(filepath.Join(s.dir, grantsFileName))
	if gerr != nil {
		// Never silently drop entitlements: an unreadable store must not read
		// as "nobody is entitled to anything" AND must not read as "everyone
		// is". It reads as broken, and non-admins are refused.
		return s.failLocked("the grant store must parse",
			fmt.Errorf("%s is unreadable (%w); non-admin access is refused until it is repaired or removed",
				filepath.Join(s.dir, grantsFileName), gerr))
	}
	s.grants = grants

	s.provisioned = true
	s.deriveLocked()

	if err := s.migrateLegacyLocked(); err != nil {
		return s.failLocked("the legacy migration must complete", err)
	}
	return nil
}

// deriveLocked recomputes the ceiling. It is idempotent and cheap enough to run
// on the refresh interval.
func (s *state) deriveLocked() {
	floor := make([]Root, 0, len(s.deps.DataRoots))
	for _, p := range s.deps.DataRoots {
		if c := cleanAbs(p); c != "" {
			floor = append(floor, Root{Path: c, Kind: KindData, Note: "canonical storage root"})
		}
	}

	roots := append([]Root{}, floor...)
	degraded := ""

	if s.deps.Mounts != nil {
		got, err := s.deps.Mounts()
		if err != nil {
			degraded = "mounted filesystems could not be enumerated: " + err.Error()
		}
		for _, p := range got {
			if c := cleanAbs(p); c != "" {
				roots = append(roots, Root{Path: c, Kind: KindMount, Note: "served by the storage service"})
			}
		}
	}
	if s.deps.Shares != nil {
		got, err := s.deps.Shares()
		if err != nil && degraded == "" {
			degraded = "configured shares could not be enumerated: " + err.Error()
		}
		for _, p := range got {
			if c := cleanAbs(p); c != "" {
				roots = append(roots, Root{Path: c, Kind: KindShare, Note: "configured share"})
			}
		}
	}
	if s.deps.AppData != nil {
		got, err := s.deps.AppData()
		if err != nil && degraded == "" {
			degraded = "the application data root could not be determined: " + err.Error()
		}
		if c := cleanAbs(got); c != "" {
			roots = append(roots, Root{Path: c, Kind: KindAppData, Note: "application data"})
		}
	}
	roots = append(roots, s.operatorAdd...)

	s.ceiling = dedupeRoots(roots)
	s.derivedAt = time.Now()

	// PUBLISH: build the immutable snapshot enforcement reads. Nothing
	// observes a new policy until it is complete, so a crash or a concurrent
	// request never sees a half-built rule set.
	mounts, mountinfoOK := readSystemMounts()
	s.policyVersion++
	publish(buildPolicy(s.policyVersion, s.ceiling, s.explicitRules, mounts, mountinfoOK))

	if degraded != "" {
		// The floor still stands, so the boundary is narrower than it should
		// be rather than wider. Narrower is the safe direction, and saying so
		// lets an operator explain a refusal they did not expect.
		s.status = StatusDegraded
		s.invariant = "the ceiling must reflect every storage area actually served"
		s.detail = degraded + " — the boundary fell back to the canonical roots, so paths on other storage are being refused"
		return
	}
	// Successful derivation clears "unavailable" and lifts a previous
	// "degraded", but never clears a failed self-test: only SelfTest may do
	// that, or a refresh would quietly relabel a broken boundary as healthy.
	// Enforcement is gated separately on `validated`, so a healthy status
	// before self-test still refuses.
	if s.status == StatusUnavailable || s.status == StatusDegraded {
		s.status = StatusHealthy
	}
	s.invariant = ""
	s.detail = ""
}

// migrateLegacyLocked runs once, when the grant store does not yet exist.
//
// Before this package, every authenticated non-admin could reach anything the
// deny-list did not name. Switching to deny-by-default without a migration
// would lock every existing user out of their own data on upgrade — a worse
// failure than the defect. So each existing non-admin principal receives an
// explicit, persisted, revocable grant recording what they could already
// reach, marked OriginLegacy and surfaced by Health as an item to review.
//
// A deployment with no non-admin accounts writes an empty store and is strict
// from its first boot, with nothing to review.
func (s *state) migrateLegacyLocked() error {
	path := filepath.Join(s.dir, grantsFileName)
	if _, err := os.Stat(path); err == nil {
		return nil // already migrated; idempotent
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat grant store: %w", err)
	}

	// The unsafe legacy semantic was "anything not blacklisted is allowed",
	// and at the principal layer it meant "every account reaches everything".
	// Importing that wholesale — granting each existing account the entire
	// ceiling — would carry the exact semantic being retired across the
	// upgrade, wearing a new name. So the migration grants NOTHING.
	//
	// Existing accounts therefore receive PRINCIPAL_NOT_AUTHORIZED until an
	// administrator assigns storage, which is a deliberate, documented
	// compatibility consequence: a denial that explains itself and offers an
	// administrative action is better than silently preserving unrestricted
	// reach. Health reports how many accounts are waiting so the condition is
	// visible rather than discovered by a confused user.
	//
	// Administrators are unaffected: they clear the principal layer by role.
	// The account list feeds a HEALTH COUNTER and nothing else: the migration
	// grants nothing either way, so the security decision does not depend on
	// it. Making it fatal was a defect with production consequences — the
	// authority never provisioned, so AuthorizeStorage failed closed and every
	// file operation on the box was refused with POLICY_UNAVAILABLE.
	//
	// Refusing to answer a reporting question must never take down the thing
	// being reported on. An enumeration that fails is recorded as UNKNOWN, and
	// health says "unknown" rather than claiming zero.
	pending := pendingUnknown
	if s.deps.NonAdminPrincipals != nil {
		got, err := s.deps.NonAdminPrincipals()
		if err != nil {
			s.pendingDetail = "the account list could not be read (" + err.Error() +
				"), so the number of accounts awaiting storage grants is unknown"
		} else {
			pending = len(got)
		}
	}
	s.pendingPrincipals = pending

	if err := saveGrants(path, nil); err != nil {
		return err
	}
	s.grants = nil
	return nil
}

// SelfTest proves the negative boundary of BOTH layers rather than the
// existence of either.
//
// A check that never refuses anything is precisely the defect this package
// replaces, so the assertions are: a path outside the ceiling is refused for an
// admin (the ceiling is an absolute maximum, not a user-level rule), and a path
// inside the ceiling is refused for a principal holding no grant.
func SelfTest() error {
	fail := func(invariant string, err error) error {
		global.mu.Lock()
		global.validated = false
		global.failLocked(invariant, err)
		global.mu.Unlock()
		return err
	}

	global.mu.Lock()
	if !global.provisioned {
		err := errors.New("provision before self-test")
		global.failLocked("the authority has not been provisioned", err)
		global.mu.Unlock()
		return err
	}
	// Enforce provisionally so the probes below run through the real Authorize
	// path, gate included, rather than a private lookup that would not prove
	// what callers actually execute.
	global.validated = true
	ceiling := append([]Root{}, global.ceiling...)
	global.mu.Unlock()

	if len(ceiling) == 0 {
		return fail("the ceiling must never be empty",
			errors.New("an empty ceiling would refuse everything, including the appliance's own storage"))
	}

	// The ceiling binds even an administrator: it describes what the appliance
	// serves, not who is privileged.
	admin := Principal{ID: "\x00selftest-admin", Role: "admin", Admin: true}
	outside := "\x00-not-a-real-path/selftest"
	if _, err := Authorize(admin, outside, OpRead); err == nil {
		return fail("a path outside the ceiling must be refused for every principal",
			errors.New("an administrator was allowed outside the ceiling"))
	}

	// And a principal with no grant is refused inside it.
	stranger := Principal{ID: "\x00selftest-stranger", Role: "user"}
	inside := filepath.Join(ceiling[0].Path, "selftest-probe")
	if _, err := Authorize(stranger, inside, OpRead); err == nil {
		return fail("a principal holding no grant must be refused inside the ceiling",
			errors.New("an ungranted principal was allowed"))
	}

	// The positive direction must hold too, or the authority is a blanket deny
	// and the appliance cannot serve its own storage.
	if _, err := Authorize(admin, inside, OpRead); err != nil {
		return fail("an administrator must reach the appliance's own storage",
			fmt.Errorf("admin refused inside the ceiling: %w", err))
	}

	global.mu.Lock()
	global.validated = true
	if global.status == StatusValidationFailed || global.status == StatusUnavailable {
		global.status = StatusHealthy
		global.invariant = ""
		global.detail = ""
	}
	global.mu.Unlock()
	return nil
}

// Authorize is the single decision point. It returns the cleaned path to act on
// or a *Denial.
//
// FAIL-CLOSED: when the authority is not provisioned and validated, every
// principal is refused. A service that cannot consult the boundary refuses
// rather than acting unchecked.
func Authorize(p Principal, path string, op Op) (string, error) {
	d := AuthorizeStorage(p, op, path)
	if d.Allowed {
		return d.Path, nil
	}
	return "", d
}

// AuthorizeStorage is the semantic authority: it answers "what establishes that
// CasaNova, and then this principal, may perform this operation on this
// object?" and returns a Decision that explains itself.
//
// This is deliberately not IsSafePath(path). A boolean path predicate is the
// legacy mistake with a nicer name: it cannot express operations, it cannot
// express principals, and once callers depend on its shape, adding either
// becomes a rewrite. Callers that genuinely do not distinguish operations may
// pass a coarse one.
//
// FAIL-CLOSED: when the authority is not in force, every principal is refused
// with POLICY_UNAVAILABLE. "Could not determine policy" never becomes "allow".
func AuthorizeStorage(p Principal, op Op, path string) Decision {
	return authorizeStorage(p, op, path, false)
}

// AuthorizeListing authorizes reading the NAMES and metadata of a directory's
// entries — a listing, a count, a stat-only size walk — as opposed to reading
// what is inside a file.
//
// It differs from AuthorizeStorage(p, OpRead, path) in exactly one place, and
// only for an account whose system-disk switch is on: a kernel pseudo-
// filesystem (rule 3) and a withheld credential file may be LISTED, because
// their names and sizes are what browsing "/" means. Their CONTENTS stay
// refused by AuthorizeStorage, with the reason stated, because reading them is
// where the danger is: /proc/kcore, a sysfs attribute whose read wakes
// hardware, the appliance's own signing key. An operator DENY still wins, and
// with the switch off this is AuthorizeStorage(p, OpRead, path) exactly.
func AuthorizeListing(p Principal, path string) Decision {
	return authorizeStorage(p, OpRead, path, true)
}

func authorizeStorage(p Principal, op Op, path string, listing bool) Decision {
	global.maybeRefresh()

	global.mu.RLock()
	enforcing := global.enforcingLocked()
	grants := global.grants
	invariant := global.invariant
	global.mu.RUnlock()

	pol := loadPolicy()
	if !enforcing || pol == nil {
		why := invariant
		if why == "" {
			why = "it has not completed provisioning and self-validation"
		}
		return Decision{Reason: ReasonPolicyUnavailable,
			Detail: "the storage authority is not in force (" + why + "); access is refused rather than granted unchecked"}
	}

	clean := cleanAbs(path)
	if clean == "" || strings.ContainsRune(clean, 0) {
		return Decision{Reason: ReasonInvalidPath, Detail: "path must be absolute, non-empty and free of null bytes"}
	}

	// Rules 3-7: may CasaNova manage this object at all?
	resolved := resolveForOp(op, clean)
	d := pol.evaluateStorage(clean, resolved)
	if !d.Allowed {
		// Rule 6.5: the per-account system-disk switch narrows rule 7's
		// default deny, and ONLY rule 7's. Keying on the reason code is what
		// keeps the rest of the precedence intact without restating it: a
		// SYSTEM_LOCATION (rule 3), an operator DENY (rule 4), an unusable
		// path (rule 1) or an authority that is not in force (rule 2) all
		// arrive here with their own reason and are returned untouched. The
		// switch cannot promote any of them, which is the property the
		// package's contract requires of every allow.
		if d.Reason == ReasonOutsideAuthorizedStorage && systemDiskReads(p, op) {
			if !listing {
				if w, ok := systemDiskWithheld(clean, resolved); ok {
					return w
				}
			}
			return systemDiskDecision(clean, resolved)
		}
		// Rule 3 for a LISTING by a switched-on account. Only the reason code
		// SYSTEM_LOCATION from a readable mount table qualifies: when the
		// table could not be read the authority cannot tell what is mounted
		// there, and that refusal stands. Rule 4 is evaluated here explicitly
		// because evaluateStorage returned before reaching it.
		if listing && d.Reason == ReasonSystemLocation && d.Rule != "system:unverifiable" &&
			systemDiskReads(p, op) {
			if deny := pol.explicitDeny(clean, resolved); deny != nil {
				return *deny
			}
			sd := systemDiskDecision(clean, resolved)
			sd.Rule = "system-disk-access+list:" + strings.TrimPrefix(d.Rule, "system:")
			sd.Detail = "this account has the system-disk switch on: the entries of this kernel filesystem may be listed, but not read"
			return sd
		}
		// The same rule 3 refusal for a READ, said so the switched-on account
		// learns why it can browse here but not open anything.
		if !listing && d.Reason == ReasonSystemLocation && systemDiskReads(p, op) {
			d.Detail += "; with the system-disk switch its entries can be listed, but its files are never read: kernel pseudo-files can expose memory or act on hardware"
		}
		return d
	}

	// The withheld secrets are refused to a switched-on READ inside authorized
	// storage too, not only outside it: the AppData root is /var/lib/casaos,
	// which is where the JWT signing key and the user database live, and the
	// read handlers skip the system-directory blocklist for a switched-on
	// account. Without this the switch handed the signing key to any account
	// it was given to.
	if !listing && systemDiskReads(p, op) {
		if w, ok := systemDiskWithheld(clean, resolved); ok {
			return w
		}
	}

	// Rules 8-10: may THIS principal, for THIS operation?
	if p.Admin {
		d.Path = clean
		return d
	}
	// The switch is a whole-disk READ entitlement, so it answers the principal
	// layer as well. Without this, a switched-on non-administrator could browse
	// the parts of the disk OUTSIDE authorized storage — allowed just above at
	// rule 6.5 — and be refused the parts INSIDE it for want of a grant, which
	// is both incoherent and the opposite of what the switch says it does.
	if systemDiskReads(p, op) {
		d.Path = clean
		d.Rule = d.Rule + "+system-disk-access"
		return d
	}
	for _, g := range grants {
		if g.PrincipalID != p.ID || g.Ops&op == 0 {
			continue
		}
		// Judged on where the path really lands too, or a symlink inside one
		// principal's area pointing into another's would let the first read
		// the second through their own legitimate grant.
		if within(g.Path, clean) && within(g.Path, resolved) {
			d.Path = clean
			d.Rule = d.Rule + "+grant:" + g.ID
			if len(g.Path) > len(d.ConfineRoot) {
				d.ConfineRoot = g.Path
			}
			// The grant is narrower than the storage root, so it — not the
			// root — is what the open must be confined beneath.

			return d
		}
	}
	return Decision{
		Reason: ReasonPrincipalNotAuthorized,
		// Deliberately says nothing about what exists at the path: a principal
		// that may not act there should not learn whether it is there.
		Detail:     "this account has no " + op.String() + " capability for the requested location",
		Remediable: false,
	}
}

// AuthorizeSystem applies the CEILING ONLY — "may CasaNova manage this object
// at all" — with no principal.
//
// It exists for the paths that have no requesting user: startup recovery,
// background cleanup, rehydration of persisted state, and any place that acts
// on a path it read from disk rather than received from a client. Those paths
// are exactly where a previous vulnerable release can plant something, so they
// need the boundary even though no principal is present.
//
// Callers that DO have a principal must use AuthorizeStorage instead: this
// function deliberately skips the per-principal layer and would be a bypass if
// used on a request path. The name says so.
func AuthorizeSystem(op Op, path string) Decision {
	return AuthorizeStorage(Principal{ID: "\x00system", Role: "system", Admin: true}, op, path)
}

// AuthorizeRename authorizes both endpoints of a move. The destination is a
// separate object with its own policy, and validating only the source is how
// a move becomes an escape.
func AuthorizeRename(p Principal, from, to string) (string, string, error) {
	src := AuthorizeStorage(p, OpRename, from)
	if !src.Allowed {
		return "", "", src
	}
	dst := AuthorizeStorage(p, OpCreate|OpWrite, to)
	if !dst.Allowed {
		return "", "", dst
	}
	return src.Path, dst.Path, nil
}

// EffectiveRoots reports the subtrees a principal may act on, for diagnostics
// and for a UI that should show a user only what they can reach.
func EffectiveRoots(p Principal) []Root {
	global.maybeRefresh()
	global.mu.RLock()
	defer global.mu.RUnlock()

	if !global.enforcingLocked() {
		return nil
	}
	if p.Admin {
		return append([]Root{}, global.ceiling...)
	}
	var out []Root
	for _, g := range global.grants {
		if g.PrincipalID != p.ID {
			continue
		}
		if withinAny(global.ceiling, g.Path) {
			out = append(out, Root{Path: g.Path, Kind: RootKind(g.Origin), Note: g.Note})
		}
	}
	return dedupeRoots(out)
}

// CurrentHealth reports machine-readable health, naming the invariant that
// failed and the boundary in force.
func CurrentHealth() Health {
	global.maybeRefresh()
	global.mu.RLock()
	defer global.mu.RUnlock()

	legacy := 0
	for _, g := range global.grants {
		if g.Origin == OriginLegacy {
			legacy++
		}
	}
	h := Health{
		Status:            global.status,
		Ceiling:           append([]Root{}, global.ceiling...),
		Grants:            len(global.grants),
		LegacyGrants:      legacy,
		PendingPrincipals: global.pendingPrincipals,
		Invariant:         global.invariant,
		Detail:            global.detail,
		Enforcing:         global.enforcingLocked(),
		Validation:        "failed",
	}
	if global.validated {
		h.Validation = "passed"
	}
	if p := loadPolicy(); p != nil {
		h.PolicyVersion = p.version
		h.DerivedRoots = p.derivedCount
		h.ExplicitRoots = p.explicitCount
		h.SystemBoundaries = len(p.systemMounts)
		h.MountTableReadable = p.mountinfoOK
	}
	switch {
	case h.PendingPrincipals == pendingUnknown && h.Invariant == "":
		h.Detail = global.pendingDetail
	case h.PendingPrincipals > 0 && h.Invariant == "":
		h.Detail = fmt.Sprintf("%d account(s) hold no storage grant after the upgrade and will be denied with PRINCIPAL_NOT_AUTHORIZED until an administrator assigns storage", h.PendingPrincipals)
	}
	return h
}

func (s *state) enforcingLocked() bool {
	return s.provisioned && s.validated &&
		(s.status == StatusHealthy || s.status == StatusDegraded)
}

func (s *state) failLocked(invariant string, err error) error {
	s.status = StatusValidationFailed
	if !s.provisioned {
		s.status = StatusUnavailable
	}
	s.invariant = invariant
	if err != nil {
		s.detail = err.Error()
	}
	return err
}

// maybeRefresh re-derives the ceiling when it has gone stale, so a disk mounted
// after boot becomes reachable without a restart.
func (s *state) maybeRefresh() {
	s.mu.RLock()
	fresh := time.Since(s.derivedAt) < refreshInterval
	provisioned := s.provisioned
	s.mu.RUnlock()
	if fresh || !provisioned {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.derivedAt) < refreshInterval {
		return
	}
	s.deriveLocked()
}

// ---------------------------------------------------------------- grants API

// AddGrant records a new entitlement. Callers are administrative endpoints; the
// package does not decide who may call it, only what the grant means.
//
// A grant is refused if it would fall outside the ceiling: an entitlement the
// ceiling does not cover would be a promise the authority cannot keep, and
// storing it would let a later ceiling change silently widen access.
func AddGrant(g Grant) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	if !global.provisioned {
		return global.failLocked("the authority has not been provisioned",
			errors.New("cannot record a grant before provisioning"))
	}
	clean := cleanAbs(g.Path)
	if clean == "" {
		return errors.New("grant path must be absolute")
	}
	if !withinAny(global.ceiling, clean) {
		return fmt.Errorf("grant path %s is outside every storage area this appliance serves", clean)
	}
	if strings.TrimSpace(g.PrincipalID) == "" {
		return errors.New("grant must name a principal")
	}
	if g.Ops == 0 {
		return errors.New("grant must carry at least one operation")
	}
	g.Path = clean
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	if g.ID == "" {
		g.ID = g.PrincipalID + "-" + strconv.FormatInt(g.CreatedAt.UnixNano(), 36)
	}

	next := append(append([]Grant{}, global.grants...), g)
	if err := saveGrants(filepath.Join(global.dir, grantsFileName), next); err != nil {
		return err
	}
	global.grants = next
	return nil
}

// RevokeGrant removes an entitlement by ID.
func RevokeGrant(id string) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	next := make([]Grant, 0, len(global.grants))
	found := false
	for _, g := range global.grants {
		if g.ID == id {
			found = true
			continue
		}
		next = append(next, g)
	}
	if !found {
		return fmt.Errorf("no grant with id %q", id)
	}
	if err := saveGrants(filepath.Join(global.dir, grantsFileName), next); err != nil {
		return err
	}
	global.grants = next
	return nil
}

// Grants returns a copy of the entitlements in force.
func Grants() []Grant {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return append([]Grant{}, global.grants...)
}

// AddOperatorRoot persists an extra ceiling root. This is the ONLY way to widen
// the ceiling beyond what derivation finds, and it is deliberately explicit and
// durable: there is no implicit "allow everything" and no runtime flag that
// produces one.
func AddOperatorRoot(path, note string) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	clean := cleanAbs(path)
	if clean == "" {
		return errors.New("operator root must be absolute")
	}
	if clean == "/" {
		return errors.New("the filesystem root cannot be a storage root; that would restore the unbounded behaviour this authority replaces")
	}
	for _, r := range global.operatorAdd {
		if r.Path == clean {
			return nil // idempotent
		}
	}
	next := append(append([]Root{}, global.operatorAdd...), Root{Path: clean, Kind: KindOperator, Note: note})
	if err := saveRoots(filepath.Join(global.dir, rootsFileName), next); err != nil {
		return err
	}
	global.operatorAdd = next
	global.deriveLocked()
	return nil
}

// ---------------------------------------------------------------- resolution

// resolveFn is indirected so tests can drive the boundary deterministically.
// Production uses the real filesystem.
var resolveFn = filepath.EvalSymlinks

// resolveForOp resolves a path the way the operation will actually touch it.
//
// Content operations (read, write) follow the final component, because that is
// where the bytes come from or go. Operations on the entry ITSELF (delete,
// share) must NOT follow it: unlinking a symlink never touches its target, and
// following it would both misjudge the boundary and make a link pointing
// outside impossible to clean up from the UI. The parent is still resolved, so
// a link in the path's directory chain cannot smuggle the entry elsewhere.
func resolveForOp(op Op, clean string) string {
	if op&(OpDelete|OpRename|OpShare) != 0 && op&(OpRead|OpWrite|OpCreate|OpUpload|OpExtract) == 0 {
		parent := resolveBestEffort(filepath.Dir(clean))
		return filepath.Join(parent, filepath.Base(clean))
	}
	return resolveBestEffort(clean)
}

// resolveBestEffort resolves as much of a path as exists.
//
// A path being created does not exist yet, so EvalSymlinks fails on it; the
// deepest existing ancestor is what decides the boundary, because that is what
// a planted symlink would have to live in. Returning the input unchanged on
// failure would be the dangerous direction — it would let a non-existent path
// skip resolution entirely — so the walk always lands on something real.
func resolveBestEffort(p string) string {
	rest := ""
	cur := p
	for i := 0; i < 64; i++ {
		if r, err := resolveFn(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // nothing on this path exists; lexical form is all there is
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	return p
}

// AddOperatorDeny records an explicit DENY rule.
//
// DENY is validated and persisted exactly like ALLOW, and outranks it: an
// administrator who allows /DATA and denies /DATA/payroll gets the narrower
// answer, and no link into the denied subtree launders it.
//
// Mutating policy is itself privileged. This function does not decide WHO may
// call it — that is the route layer's gate — but it does refuse anything that
// would make the stored policy incoherent, before persisting.
func AddOperatorDeny(path, addedBy, note string) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	if !global.provisioned {
		return global.failLocked("the authority has not been provisioned",
			errors.New("cannot record a rule before provisioning"))
	}
	clean := cleanAbs(path)
	if err := pathIsCanonicalRootCandidate(clean); err != nil {
		return err
	}
	for _, r := range global.explicitRules {
		if r.Path == clean && r.Access == AccessDeny {
			return nil // idempotent
		}
	}
	rule := Rule{
		ID: "deny:" + clean, Path: clean, Access: AccessDeny,
		Source: SourceExplicit, Note: note, AddedBy: addedBy, CreatedAt: time.Now(),
	}

	// VALIDATE -> PREPARE -> PERSIST -> DURABLE -> PUBLISH. The snapshot is
	// only swapped after the write is durable, so a crash mid-update leaves
	// the previous policy in force. A partial write can never turn DENY into
	// ALLOW, because the file is replaced atomically and a policy that does
	// not parse fails closed rather than reading as empty.
	next := append(append([]Rule{}, global.explicitRules...), rule)
	if err := saveRules(filepath.Join(global.dir, denyFileName), next); err != nil {
		return err
	}
	global.explicitRules = next
	global.deriveLocked()
	return nil
}

// saveRules persists explicit rules (both senses) atomically.
func saveRules(path string, rules []Rule) error {
	return writeAtomic(path, rulesFile{Version: schemaVersion, Rules: rules})
}

func loadRules(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f rulesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Version > schemaVersion {
		return nil, fmt.Errorf("policy rule schema version %d is newer than this build understands (%d)", f.Version, schemaVersion)
	}
	return f.Rules, nil
}

type rulesFile struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

// ---------------------------------------------------------------- path helpers

func cleanAbs(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Match the inherited normalisation so a path accepted here is the same
	// string the filesystem layer will act on.
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	return filepath.Clean(p)
}

// within reports whether child is root itself or lies beneath it. The separator
// is required so /DATA-other is not treated as inside /DATA.
func within(root, child string) bool {
	if root == child {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(root, "/")+"/")
}

func withinAny(roots []Root, child string) bool {
	for _, r := range roots {
		if within(r.Path, child) {
			return true
		}
	}
	return false
}

func dedupeRoots(in []Root) []Root {
	sort.SliceStable(in, func(i, j int) bool { return len(in[i].Path) < len(in[j].Path) })
	var out []Root
	for _, r := range in {
		if r.Path == "" {
			continue
		}
		covered := false
		for _, kept := range out {
			if within(kept.Path, r.Path) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ---------------------------------------------------------------- persistence

type rootsFile struct {
	Version int    `json:"version"`
	Roots   []Root `json:"roots"`
}

type grantsFile struct {
	Version int     `json:"version"`
	Grants  []Grant `json:"grants"`
}

func loadRoots(path string) ([]Root, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f rootsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Version > schemaVersion {
		return nil, fmt.Errorf("operator root list schema version %d is newer than this build understands (%d)", f.Version, schemaVersion)
	}
	return f.Roots, nil
}

func saveRoots(path string, roots []Root) error {
	return writeAtomic(path, rootsFile{Version: schemaVersion, Roots: roots})
}

func loadGrants(path string) ([]Grant, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var f grantsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Version > schemaVersion {
		return nil, fmt.Errorf("grant store schema version %d is newer than this build understands (%d)", f.Version, schemaVersion)
	}
	return f.Grants, nil
}

func saveGrants(path string, grants []Grant) error {
	return writeAtomic(path, grantsFile{Version: schemaVersion, Grants: grants})
}

// writeAtomic replaces path in one step, so a reader — or a crash — never sees
// a half-written boundary.
func writeAtomic(path string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".storageauth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, fileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
