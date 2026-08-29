// Package tokenrevocation is the revocation authority: it makes "this
// principal's tokens are no longer valid" true on every CasaNova service and
// across restarts, instead of only inside the process that decided it.
//
// See docs/design/token-revocation-authority.md for the decision record.
//
// # THE DEFECT THIS EXISTS TO FIX
//
// Deleting a user recorded the decision in a sync.Map inside
// CasaNova-UserService. Only that service ever read it, so a deleted user's
// token kept working against the main daemon, AppManagement, LocalStorage,
// Driver, Virt and JobManagement for the rest of its lifetime. And the map was
// in memory while the signing key is persisted on purpose, so a restart forgot
// every revocation while the tokens it was meant to reject stayed valid.
//
// jwt.IsRevokedFunc was declared for this and is assigned by nobody.
//
// # SHAPE
//
// One small file, written atomically by the user service, read by everyone:
// principal ID -> revocation instant. A token is refused when it was issued at
// or before that instant, so one entry covers every token a principal holds
// while a later login is unaffected.
//
// # THE SHARED-FILE DEFECT THIS ALSO FIXES
//
// The list is shared by nine PROCESSES. Its read-modify-write used to be
// guarded only by this package's sync.RWMutex, which exists once per process
// and orders nothing between them. Measured: twelve processes each revoking a
// distinct principal, all twelve reporting success, one or two entries left on
// disk; and in the realistic shape — one service restarting while an operator
// deletes a user — 27 of 40 revocations reported successful were absent from
// the file afterwards. The atomic rename made readers safe and writers no
// safer: both writers read version A, both edited a private copy, both
// renamed, and the loser's edit vanished with no error anywhere.
//
// Every update now holds a cross-process flock (see pkg/filelock) across the
// whole read-modify-write, and the file's own contents are re-read inside it.
// Consequently: if Revoke returns nil, the entry is in the file, and no entry
// that was there before the call is missing after it.
//
// SelfTest no longer writes the shared file at all. It used to record and then
// delete a probe principal in the authoritative list on every start of every
// service — eighteen read-modify-write cycles per boot on the one file the box
// least wants to lose, and a process killed between the two left the probe
// behind. It now proves the same two things separately: that this filesystem
// really takes a revocation (on a private throwaway file, through the real
// write and read code), and that the real IsRevoked refuses and accepts on the
// right side of the boundary (through an in-memory probe entry).
//
// # LIFECYCLE — this subsystem cares for itself
//
// Provision() is idempotent and is called at service startup: it creates the
// directory and file, repairs a wrong mode, migrates an older schema, and does
// nothing when the state is already correct. SelfTest() then proves the
// security property rather than the component's existence. Status() reports
// machine-readable health naming the invariant that failed. A provisioning
// failure is retried in the background from the read path, so a transient one
// (a full disk during boot) recovers on its own instead of leaving the process
// refusing every token until somebody restarts it.
//
// # FAIL-CLOSED
//
// If provisioning or self-validation fails, IsRevoked answers "revoked" for
// every principal: requests are refused rather than accepted unchecked. There
// is no path back to the old unvalidated behaviour. A corrupt file is
// quarantined and reported, never silently rewritten — rewriting it would
// erase real revocations, which is precisely the failure being fixed.
//
// # UPGRADE: THE FIRST START AFTER THIS FIX SIGNS EVERYONE OUT, ONCE
//
// A list written by a build that could lose revocations cannot be trusted to
// list every principal that was actually revoked, and nothing on disk says
// which entries went missing. The first start that can tell the difference
// therefore refuses every token issued at or before the instant it adopts such
// a file (see sanitizeLegacyList). On an existing box that means every signed-in
// session has to sign in again — once, on that one start.
//
// That is deliberate, and it is ANNOUNCED rather than left to be discovered:
// the migrating process writes a line to stderr saying what it did and why, and
// CurrentHealth reports legacy_cutoff_applied / legacy_cutoff_at for that
// process plus revoked_before for as long as the floor is in force. An operator
// asked "why was I logged out?" can answer from the journal or from health
// instead of suspecting a break-in.
//
// # DOWNGRADE
//
// The file carries min_reader_version: the lowest reader this writer's content
// is safe for. Readers refuse on THAT, not on the version number, so a later
// build may add fields and still be readable by this one — which is what makes
// the Rollback button survivable, since nothing in the update path backs this
// file up or restores it. Raising min_reader_version is the deliberate act of
// saying "older builds must refuse this file", and it is required of any change
// that alters what an existing field MEANS.
package tokenrevocation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
)

// DefaultPath is where the shared list lives, beside the other persistent
// state so it survives restarts like the signing key does.
const DefaultPath = "/var/lib/casanova/token-revocations.json"

const (
	// schemaVersion is the format this build writes.
	schemaVersion = 1

	// minReaderVersion is the compatibility contract this build stamps on every
	// file it writes: the oldest reader that can interpret this content
	// correctly.
	//
	// RAISE IT, in the same commit, for any change that alters the MEANING of
	// an existing field — different units on the instants, a key that no longer
	// names a principal, revocations recorded somewhere other than "revoked".
	// Do NOT raise it merely to add a field: unknown fields are ignored, and an
	// older build that can still read the list is an older build that can still
	// administer the box after a rollback.
	//
	// The pinned-format test in this package fails when the on-disk shape
	// changes, which is where that decision gets forced.
	minReaderVersion = 1

	// fileMode: every service reads this file and they do not all run as the
	// same user. It holds no secrets — principal IDs and timestamps only.
	fileMode = 0o644
	dirMode  = 0o755

	// retentionPeriod bounds the file. Refresh tokens are the longest-lived
	// credential at 7 days, so an older entry can no longer refuse anything.
	// Pruning on write stops repeated revocations growing the file forever.
	retentionPeriod = 8 * 24 * time.Hour

	// refreshInterval bounds how stale a reader's view may be. The public key
	// cache already accepts 10s for a value of comparable weight; 1s keeps
	// propagation well inside that budget at one stat per second per process.
	refreshInterval = time.Second

	// lockWait bounds how long an update waits for a peer process to finish
	// its own update. The guarded section is one small read plus one small
	// write, so this is orders of magnitude more than a healthy box needs; it
	// exists so a wedged peer produces a failed revocation the caller can
	// report, not a request that never returns.
	lockWait = 5 * time.Second

	// provisionRetryInterval paces the background repair attempt made from the
	// read path while the authority is not enforcing. Every attempt is one stat
	// and at most one small read, and the process is refusing every token
	// meanwhile, so a slow pace costs nothing but a fast one buys nothing.
	provisionRetryInterval = 5 * time.Second

	// probePrincipal is the reserved ID SelfTest uses to exercise the negative
	// boundary. It is not a valid user ID (real IDs are integers), so it can
	// never collide with a real principal. Builds before the self-test stopped
	// writing the shared list could leave it on disk; it is filtered out of the
	// reported entry count and expires with everything else.
	probePrincipal = "\x00selftest-probe"
)

// Status is the machine-readable health of the authority.
type Status string

const (
	// StatusHealthy means the list is provisioned, readable and validated.
	StatusHealthy Status = "healthy"
	// StatusDegraded means a refresh failed but a previously validated
	// snapshot is still being enforced. Revocations made during this window
	// may not be visible yet; everything already revoked is still refused.
	StatusDegraded Status = "degraded"
	// StatusValidationFailed means self-validation did not hold: the property
	// this package exists to provide is not in force. Fails closed.
	StatusValidationFailed Status = "validation_failed"
	// StatusUnavailable means the authority was never successfully
	// provisioned. Fails closed.
	StatusUnavailable Status = "unavailable"
)

// Health describes the authority for diagnostics endpoints.
type Health struct {
	Status Status `json:"status"`
	Path   string `json:"path"`
	// Invariant names the property that failed, in words an operator can act
	// on. Empty when healthy.
	Invariant string `json:"invariant,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Entries   int    `json:"entries"`
	// Enforcing reports whether token refusal is actually being applied. False
	// only in states that fail closed, where every token is refused instead.
	Enforcing bool `json:"enforcing"`
	// RevokedBefore, when set, is the instant before which every token is
	// refused for every principal, in RFC3339. It is what tells an operator why
	// a box asked everyone to log in again; see sanitizeLegacyList.
	RevokedBefore string `json:"revoked_before,omitempty"`
	// LegacyCutoffApplied reports that THIS process is the one that adopted a
	// pre-contract list and imposed the floor above, i.e. that the sign-everyone
	// -out happened on this start rather than being inherited from the file.
	// It is per-process and not persisted: later starts read an already-migrated
	// file and report false while RevokedBefore is still in force, which is what
	// RevokedBefore is for. Always emitted, so "no cutoff" is an answer an
	// operator can read rather than an absent field they must interpret.
	LegacyCutoffApplied bool `json:"legacy_cutoff_applied"`
	// LegacyCutoffAt is the instant of that cutoff, RFC3339, when it happened
	// in this process.
	LegacyCutoffAt string `json:"legacy_cutoff_at,omitempty"`
}

// list is the whole content of the shared file.
type list struct {
	// entries maps principal ID to the instant of revocation.
	entries map[string]time.Time

	// before, when non-zero, refuses every token issued at or before it, for
	// every principal. It is a floor under the per-principal entries, not a
	// replacement for them.
	before time.Time

	// readerContract is the min_reader_version the file declared. Zero means
	// the file predates the contract, which is also how a list written by a
	// build without cross-process serialization is recognised.
	readerContract int
}

type state struct {
	// mu guards every field below. It is taken for the shortest possible time
	// and NEVER while disk I/O is in flight: every request consults this lock,
	// and refresh used to hold it exclusively across a stat plus a full read
	// and unmarshal — measured at 79ms for 50k entries and 217ms for 200k, with
	// every request in the process stopped for the duration. On this box that
	// is not hypothetical: the host-admin module spins disks down, so a stat
	// can block for a spin-up.
	//
	// Lock order, where more than one is held: ioMu -> writeMu -> mu.
	mu sync.RWMutex

	// writeMu serializes updates from THIS process before they contend for the
	// cross-process lock. It is not a substitute for that lock — that mistake
	// is what this package was losing revocations to — it just keeps one
	// process from queueing against itself.
	writeMu sync.Mutex

	// ioMu single-flights the refresh and the background repair. Callers use
	// TryLock and move on: a reader that finds a refresh already running serves
	// the snapshot it has rather than waiting on someone else's disk.
	ioMu sync.Mutex

	path string

	// revoked maps principal ID to the instant of revocation.
	revoked map[string]time.Time

	// revokedBefore mirrors list.before for the loaded snapshot.
	revokedBefore time.Time

	// legacyCutoffApplied/legacyCutoffAt record that this process performed the
	// pre-contract migration, so health can say so instead of leaving the
	// operator to guess why the box asked everyone to sign in again. They are
	// reporting only: nothing reads them to make a refusal decision.
	legacyCutoffApplied bool
	legacyCutoffAt      time.Time

	// generation counts snapshots published by THIS process. A refresh that
	// read the file before a local write landed must not publish its stale map
	// over the newer one; it compares the generation it started with.
	generation uint64

	// provisioned and validated gate the fail-closed decision. Until both
	// hold, this process cannot vouch for any token.
	provisioned bool
	validated   bool

	// bootstrapped records that lazy provisioning has been attempted, so it
	// runs at most once per configured path.
	bootstrapped bool

	// nextRepairAttempt paces the background re-provision while the authority
	// is not enforcing.
	nextRepairAttempt time.Time

	status    Status
	invariant string
	detail    string

	// probeAt/probeActive hold SelfTest's in-memory probe revocation. It is
	// deliberately not in `revoked`, so a refresh cannot drop it mid-probe and
	// fail a healthy box closed, and it never reaches the shared file.
	probeAt     time.Time
	probeActive bool

	lastCheck time.Time
	lastMod   time.Time
	lastSize  int64
}

// duringDiskIO is a test seam, nil in production. It is called at the points
// where this package is about to touch the disk, so a test can assert
// STRUCTURALLY — by taking the lock itself, not by timing anything — that the
// lock every request consults is not being held across that I/O.
var duringDiskIO func(stage string)

func atDiskIO(stage string) {
	if duringDiskIO != nil {
		duringDiskIO(stage)
	}
}

var global = &state{
	path:    DefaultPath,
	revoked: map[string]time.Time{},
	status:  StatusUnavailable,
	// Before Provision runs, the authority cannot answer. Saying so up front
	// means there is no window where it reports "not revoked" merely because
	// it has not loaded yet.
	invariant: "revocation list has not been provisioned",
}

// SetPath overrides the file location and resets state. Intended for tests and
// for deployments that relocate persistent state; production uses DefaultPath.
func SetPath(p string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.path = p
	global.revoked = map[string]time.Time{}
	global.revokedBefore = time.Time{}
	global.legacyCutoffApplied = false
	global.legacyCutoffAt = time.Time{}
	global.generation++
	global.provisioned = false
	global.validated = false
	global.bootstrapped = false
	global.probeActive = false
	global.probeAt = time.Time{}
	global.nextRepairAttempt = time.Time{}
	global.status = StatusUnavailable
	global.invariant = "revocation list has not been provisioned"
	global.detail = ""
	global.lastCheck = time.Time{}
	global.lastMod = time.Time{}
	global.lastSize = 0
}

// Path reports the file currently in use.
func Path() string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.path
}

// Provision brings the on-disk state to the shape this package requires. It is
// idempotent and safe to call on every start:
//
//	absent      -> create an empty, correctly-moded list
//	correct     -> no-op
//	wrong mode  -> repair
//	old schema  -> migrate in place
//	pre-contract-> migrate and sanitize (see sanitizeLegacyList)
//	corrupt     -> quarantine, report, and fail (never silently rewrite: an
//	               empty list would un-revoke everyone, failing open)
//	incompatible-> refuse and diagnose rather than guess
//
// The whole inspect-and-repair runs under the cross-process lock: without it,
// two services starting at once both see "absent" and both write an empty list,
// and a revocation made between the two writes is gone.
//
// A non-nil error means the authority is not in force and callers fail closed.
func Provision() error {
	global.writeMu.Lock()
	defer global.writeMu.Unlock()

	global.mu.Lock()
	// An explicit Provision means the caller is driving the lifecycle, so the
	// lazy safety net must stand down. Without this, the first IsRevoked issued
	// from inside SelfTest re-enters autoProvision, which runs a NESTED
	// SelfTest whose cleanup retracts the outer one's probe — and the outer
	// self-test then fails on a perfectly healthy box, taking it fail-closed.
	global.bootstrapped = true
	path := global.path
	global.mu.Unlock()

	l, mod, size, legacyCutoff, perr := provisionOnDisk(path)

	global.mu.Lock()
	if global.path != path {
		// SetPath moved the target while we worked. Publishing the old file's
		// content under the new path would be a lie; leave the state as the
		// reset left it, unprovisioned and failing closed.
		global.mu.Unlock()
		return errors.New("revocation list path changed during provisioning")
	}
	if perr != nil {
		global.nextRepairAttempt = time.Now().Add(provisionRetryInterval)
		err := global.failLocked(perr.invariant, perr.err)
		global.mu.Unlock()
		return err
	}

	global.adoptLocked(l, mod, size)
	global.provisioned = true
	global.status = StatusHealthy
	global.invariant = ""
	global.detail = ""
	if legacyCutoff {
		global.legacyCutoffApplied = true
		global.legacyCutoffAt = l.before
	}
	kept := countEntries(l.entries)
	global.mu.Unlock()

	// Announce after releasing the lock every request consults: the notice is a
	// write to stderr, and no report is worth putting the request path behind a
	// disk. Provisioning has already succeeded at this point, so nothing the
	// announcement does can turn a working authority into a failed one.
	if legacyCutoff {
		announceLegacyCutoff(path, l.before, kept)
	}
	return nil
}

// provisionError carries the operator-facing invariant alongside the cause, so
// the disk work can happen with no lock held and still report precisely.
type provisionError struct {
	invariant string
	err       error
}

// provisionOnDisk does every filesystem operation Provision needs, holding no
// state lock. It returns the adopted list, the file identity to stamp, and
// whether adopting the file imposed the legacy cutoff — which the caller has to
// announce, because that is the start on which everybody gets signed out.
func provisionOnDisk(path string) (list, time.Time, int64, bool, *provisionError) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return list{}, time.Time{}, 0, false, &provisionError{
			"revocation state directory must exist",
			fmt.Errorf("create %s: %w", dir, err),
		}
	}

	lk, err := filelock.Acquire(path, lockWait)
	if err != nil {
		return list{}, time.Time{}, 0, false, &provisionError{
			"revocation list updates must be serialized across services",
			fmt.Errorf("lock %s: %w", path, err),
		}
	}
	defer lk.Release()

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// First run. An empty list is the correct initial state: nothing has
		// been revoked yet, and there is no earlier build's loss to compensate
		// for, so no sanitizing floor is set.
		l := list{entries: map[string]time.Time{}, readerContract: minReaderVersion}
		if werr := writeFile(path, l); werr != nil {
			return list{}, time.Time{}, 0, false, &provisionError{"revocation list must be writable", werr}
		}
		mod, size := identity(path)
		return l, mod, size, false, nil

	case err != nil:
		return list{}, time.Time{}, 0, false, &provisionError{
			"revocation list must be readable",
			fmt.Errorf("stat %s: %w", path, err),
		}
	}

	if info.IsDir() {
		return list{}, time.Time{}, 0, false, &provisionError{
			"revocation list must be a regular file",
			fmt.Errorf("%s is a directory", path),
		}
	}

	l, rerr := readFile(path)
	if rerr != nil {
		// Deliberately leave the file exactly where it is.
		//
		// The first draft moved it aside and let the next start create a
		// fresh empty list. That self-heals into the precise failure this
		// package exists to prevent: an empty list un-revokes every
		// principal, so a corrupt file would silently restore access for
		// deleted accounts one restart later.
		//
		// Leaving it in place makes the refusal stable across restarts.
		// Recovery is an explicit human act — repair or delete the file —
		// which is the documented emergency path, not the normal one.
		return list{}, time.Time{}, 0, false, &provisionError{
			"revocation list must parse",
			fmt.Errorf("%s is unreadable (%w); every token is being refused until it is repaired or removed "+
				"(removing it resets the box to no revocations)", path, rerr),
		}
	}

	// A zero-length file carries no revocations, so nothing can be lost by
	// giving it the shape provisioning would have created. It is also not
	// evidence of an earlier build, so it gets no sanitizing floor.
	rewrite := info.Size() == 0
	legacyCutoff := false
	if info.Size() > 0 && l.readerContract < minReaderVersion {
		legacyCutoff = sanitizeLegacyList(&l)
		rewrite = true
	}
	if rewrite {
		l.readerContract = minReaderVersion
		l.prune(time.Now())
		if werr := writeFile(path, l); werr != nil {
			return list{}, time.Time{}, 0, false, &provisionError{"revocation list must be writable", werr}
		}
		mod, size := identity(path)
		// Report the cutoff only once it is durable: announcing a floor that a
		// failed write never persisted would send the operator after a logout
		// that did not happen.
		return l, mod, size, legacyCutoff, nil
	}

	// Repair a mode that drifted — a list other services cannot read would
	// silently take every one of them fail-closed.
	if info.Mode().Perm() != fileMode {
		if cerr := os.Chmod(path, fileMode); cerr != nil {
			return list{}, time.Time{}, 0, false, &provisionError{
				"revocation list must be readable by every service",
				fmt.Errorf("chmod %s to %#o: %w", path, fileMode, cerr),
			}
		}
	}
	mod, size := identity(path)
	return l, mod, size, false, nil
}

// sanitizeLegacyList repairs the one thing this fix cannot recover on its own.
//
// The builds that guarded the shared file with a process mutex lost revocations
// silently: the API reported "user deleted" and the entry never reached the
// file. Nothing on disk records which ones were lost, so the fixed build reads
// the same file and inherits the same hostile state — a deleted account whose
// refresh token keeps working, from the internet, for up to seven days, with no
// way to notice.
//
// So the first build that can tell the difference does the only sound thing
// available: it refuses every token issued at or before the moment it adopts a
// pre-contract file. Everyone signs in again once; every credential minted
// while revocations could go missing stops working. This is the automatic form
// of the manual remedy (re-revoke every deleted user, or rotate the signing
// key), and it happens exactly once — the rewritten file declares the contract,
// so the next start, and every other service, sees a current file and does
// nothing.
//
// The floor expires with retentionPeriod like any other entry, because after
// that every token it could refuse has expired on its own.
//
// It returns whether it moved the floor forward, because the one thing this
// remedy must not be is silent: a box that signs everyone out and says nothing
// is indistinguishable, from the operator's chair, from a box that has been
// broken into. Callers announce it (announceLegacyCutoff) and record it for
// health. The announcement is REPORTING ONLY — it cannot fail the migration,
// and no refusal decision reads it.
func sanitizeLegacyList(l *list) bool {
	now := time.Now()
	if l.before.Before(now) {
		l.before = now
		return true
	}
	// The file already carries a floor at or beyond now, so adopting it refuses
	// nothing that was not already refused: nothing to announce.
	return false
}

// announceLegacyCutoff tells the operator, in the service journal, that this
// start is the one that signed everybody out and why.
//
// It writes to stderr rather than through utils/logger on purpose. It is
// emitted from Provision (and from Revoke, when a revocation is what migrates
// the list), and provisioning does not happen after logger initialization in
// every service — CasaNova-AppManagement provisions the authority before it
// calls LogInit — so routing it through a logger that may not exist yet would
// drop the notice in exactly the process that performed the migration. systemd
// captures stderr for every unit, and utils/logger tees to stdout anyway, so
// either way it lands in the journal.
//
// It is a var so tests can capture the notice; production never reassigns it.
var announceLegacyCutoff = func(path string, at time.Time, revocations int) {
	fmt.Fprintf(os.Stderr,
		"tokenrevocation: adopted a pre-contract revocation list (%s) and set a global cutoff at %s. "+
			"Every token issued at or before that instant is now refused, so signed-in sessions must sign in again — "+
			"this once, on this start. Reason: builds before this one could lose a revocation without reporting it, "+
			"and the file does not record which were lost, so credentials minted while that was possible cannot be "+
			"trusted. Per-principal revocations on the list afterwards: %d. The cutoff expires on its own after %s.\n",
		path, at.Format(time.RFC3339), revocations, retentionPeriod)
}

// countEntries counts real principals, leaving out the reserved self-test probe
// an older build could have left in the file. Used for both health and the
// cutoff notice so the two never disagree.
func countEntries(entries map[string]time.Time) int {
	n := 0
	for id := range entries {
		if id != probePrincipal {
			n++
		}
	}
	return n
}

// SelfTest proves the security property rather than the component's existence,
// WITHOUT writing to the shared list.
//
// Two assertions, because the property has two halves:
//
//  1. recordability — this filesystem, at this path, really accepts a written
//     revocation and reads the same one back. Exercised through the real
//     writeFile/readFile on a private throwaway file, so a boot cannot damage
//     or churn the authoritative list. The previous version recorded and then
//     deleted a probe principal in the authoritative file, which is eighteen
//     extra read-modify-write cycles per boot on the one file the box cannot
//     reconstruct.
//  2. enforcement — the real IsRevoked, fail-closed gate included, refuses a
//     token from before the revocation and accepts one from after, and leaves
//     other principals alone. The probe entry lives in memory for the duration:
//     a refresh cannot drop it (which would fail a healthy box closed) and it
//     can never leak to disk.
//
// Call it after Provision at startup.
func SelfTest() error {
	fail := func(invariant string, err error) error {
		global.mu.Lock()
		global.validated = false
		global.failLocked(invariant, err)
		global.mu.Unlock()
		return err
	}

	global.mu.Lock()
	global.bootstrapped = true // same reason as in Provision: no nested self-test
	provisioned := global.provisioned
	path := global.path
	global.mu.Unlock()

	if !provisioned {
		err := errors.New("provision before self-test")
		global.mu.Lock()
		global.failLocked("revocation list has not been provisioned", err)
		global.mu.Unlock()
		return err
	}

	if err := proveRecordable(path); err != nil {
		return fail("a revocation must be recordable", err)
	}

	// Enforce provisionally so the assertions below run through the real
	// IsRevoked path — fail-closed gate included — rather than a private
	// lookup that would not prove the function callers actually use. Any
	// failure below reverts this via fail().
	base := time.Now()
	global.mu.Lock()
	global.validated = true
	global.probeAt = base
	global.probeActive = true
	// A token has to be issued after the global floor as well as after the
	// probe's own instant to be acceptable, and the floor is set to "now" on
	// the boot that adopts a pre-contract file. Assert the positive cases with
	// an instant that clears both, or a correct box fails its own self-test on
	// exactly the boot that sanitizes it.
	after := base.Add(time.Minute)
	if !after.After(global.revokedBefore) {
		after = global.revokedBefore.Add(time.Minute)
	}
	global.mu.Unlock()

	// Always retract the probe, including on the failure paths.
	defer func() {
		global.mu.Lock()
		global.probeActive = false
		global.probeAt = time.Time{}
		global.mu.Unlock()
	}()

	// The negative boundary: a credential predating the revocation must be
	// refused. This is the assertion that matters — a check that never
	// refuses anything is the bug being fixed.
	if !IsRevoked(probePrincipal, base.Add(-time.Minute)) {
		return fail("a token issued before revocation must be refused",
			errors.New("a pre-revocation probe token was accepted"))
	}

	// And the positive one: revocation must not be a blanket deny, or a
	// principal could never authenticate again after re-login.
	if IsRevoked(probePrincipal, after) {
		return fail("a token issued after revocation must be accepted",
			errors.New("a post-revocation probe token was refused"))
	}

	// An unrelated principal must be unaffected.
	if IsRevoked("\x00selftest-untouched", after) {
		return fail("revocation must not affect other principals",
			errors.New("an unrelated probe principal was refused"))
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

// proveRecordable writes a revocation through the real serialization code and
// reads it back, on a private file in the real state directory. It proves the
// property that matters — this filesystem durably records a revocation here —
// without touching the list every service depends on.
func proveRecordable(path string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".token-revocations-probe-*")
	if err != nil {
		return fmt.Errorf("create a probe file in %s: %w", dir, err)
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	// Seconds, because that is the resolution the file format keeps.
	at := time.Now().Truncate(time.Second)
	if err := writeFile(name, list{
		entries:        map[string]time.Time{probePrincipal: at},
		readerContract: minReaderVersion,
	}); err != nil {
		return err
	}

	got, err := readFile(name)
	if err != nil {
		return fmt.Errorf("a written revocation did not read back: %w", err)
	}
	back, present := got.entries[probePrincipal]
	if !present || !back.Equal(at) {
		return fmt.Errorf("a revocation written under %s read back as %v (present=%t), want %v",
			dir, back, present, at)
	}
	return nil
}

// Ensure provisions and self-validates in one call. Services call this at
// startup; a non-nil error means the authority is not in force, and every
// token will be refused until it is repaired.
func Ensure() error {
	global.mu.Lock()
	global.bootstrapped = true
	global.mu.Unlock()

	if err := Provision(); err != nil {
		return err
	}
	return SelfTest()
}

// autoProvision is the safety net. Services call Ensure at startup so failures
// are logged and visible in health, but a service that forgets must not end up
// refusing every token forever — and, worse, must not be the reason someone
// removes the fail-closed behaviour. So the first consultation provisions and
// self-validates on its own, at most once per configured path.
//
// This is the lifecycle the subsystem owes its operator: absent state gets
// created without anyone being told to create it. A failure here still fails
// closed, CurrentHealth still names the invariant that broke, and repair is
// retried from the read path (see state.repair).
func autoProvision() {
	global.mu.RLock()
	done := global.bootstrapped
	global.mu.RUnlock()
	if done {
		return
	}

	global.mu.Lock()
	if global.bootstrapped {
		global.mu.Unlock()
		return
	}
	global.bootstrapped = true
	global.mu.Unlock()

	if err := Provision(); err != nil {
		return // status already records the failed invariant; callers fail closed
	}
	_ = SelfTest()
}

// CurrentStatus reports current health, refreshing the view first.
func CurrentStatus() Status {
	autoProvision()
	global.refresh()
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.status
}

// CurrentHealth reports machine-readable health for diagnostics endpoints,
// naming the invariant that failed.
func CurrentHealth() Health {
	autoProvision()
	global.refresh()
	global.mu.RLock()
	defer global.mu.RUnlock()

	h := Health{
		Status:              global.status,
		Path:                global.path,
		Invariant:           global.invariant,
		Detail:              global.detail,
		Entries:             countEntries(global.revoked),
		Enforcing:           global.enforcingLocked(),
		LegacyCutoffApplied: global.legacyCutoffApplied,
	}
	if !global.revokedBefore.IsZero() {
		h.RevokedBefore = global.revokedBefore.Format(time.RFC3339)
	}
	if !global.legacyCutoffAt.IsZero() {
		h.LegacyCutoffAt = global.legacyCutoffAt.Format(time.RFC3339)
	}
	return h
}

// enforcingLocked reports whether refusal decisions are trustworthy. When
// false the caller fails closed instead.
func (s *state) enforcingLocked() bool {
	return s.provisioned && s.validated &&
		(s.status == StatusHealthy || s.status == StatusDegraded)
}

// failLocked records a failed invariant and returns the error unchanged.
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

// adoptLocked publishes a snapshot this process produced. The file identity is
// passed in rather than stat'ed here, because this runs under the lock every
// request consults.
func (s *state) adoptLocked(l list, mod time.Time, size int64) {
	if l.entries == nil {
		l.entries = map[string]time.Time{}
	}
	s.revoked = l.entries
	s.revokedBefore = l.before
	s.generation++
	s.lastMod = mod
	s.lastSize = size
	s.lastCheck = time.Now()
}

// identity reads the file identity used to decide whether a refresh must
// reload. A failed stat yields the zero identity, which simply makes the next
// refresh reload from disk.
func identity(path string) (time.Time, int64) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, 0
	}
	return info.ModTime(), info.Size()
}

// IsRevoked reports whether a token for principal issued at issuedAt must be
// refused.
//
// FAIL-CLOSED: when the authority is not provisioned and validated, this
// returns true for every principal. A service that cannot consult the
// revocation list refuses tokens rather than accepting them unchecked.
//
// A zero issuedAt means the token carries no iat claim. Those are refused
// whenever the principal has any revocation on record: without an issue time
// there is no way to distinguish a pre- from a post-revocation token, and for
// a credential belonging to a principal someone deliberately revoked, refusing
// is the correct side to err on. (Every token CasaNova mints carries iat.)
func IsRevoked(principal string, issuedAt time.Time) bool {
	autoProvision()
	global.refresh()

	global.mu.RLock()
	enforcing := global.enforcingLocked()
	before := global.revokedBefore
	at, found := global.revoked[principal]
	if !found && global.probeActive && principal == probePrincipal {
		at, found = global.probeAt, true
	}
	global.mu.RUnlock()

	if !enforcing {
		return true
	}
	if !before.IsZero() && !issuedAt.After(before) {
		// Covers the zero instant too: a token that cannot be placed relative
		// to the floor is refused while the floor is in force.
		return true
	}
	if !found {
		return false
	}
	if issuedAt.IsZero() {
		return true
	}
	return !issuedAt.After(at)
}

// IsRevokedID is the int-keyed form, matching the ID on jwt.Claims.
func IsRevokedID(principal int, issuedAt time.Time) bool {
	return IsRevoked(strconv.Itoa(principal), issuedAt)
}

// Revoke refuses every token issued to principal at or before now.
//
// A nil return means the entry is in the shared file: the read-modify-write ran
// under the cross-process lock, the file was re-read inside it, and the
// replacement was renamed into place with its directory synced. A non-nil error
// means the revocation did NOT happen and the caller must not report success —
// the deleted user still has access.
func Revoke(principal string) error {
	return revokeAt(principal, time.Now())
}

// RevokeID is the int-keyed form.
func RevokeID(principal int) error {
	return Revoke(strconv.Itoa(principal))
}

// revokeAt performs the update with NO state lock held: it takes the
// cross-process lock, re-reads, edits, writes, and only then takes s.mu for as
// long as it takes to publish the result. Holding the request-path lock across
// the file work would put every reader in this process behind a peer's disk.
func revokeAt(principal string, at time.Time) error {
	s := global

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("create revocation list dir: %w", err)
	}

	// Between processes this is the only thing that makes the cycle below a
	// cycle rather than a race. Without it two services read the same list,
	// each add their own entry, and the second rename erases the first — which
	// is exactly how a revocation the API called successful went missing.
	atDiskIO("revoke")
	lk, err := filelock.Acquire(path, lockWait)
	if err != nil {
		return fmt.Errorf("serialize revocation list update: %w", err)
	}
	defer lk.Release()

	// Re-read INSIDE the lock so entries written before a restart, or by
	// another service since we last loaded, are not dropped by this write.
	current, err := readFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Refusing to write leaves the principal live, but overwriting an
		// unreadable file could discard other principals' revocations. Report
		// it: the caller must not treat the revocation as done.
		return fmt.Errorf("read revocation list before update: %w", err)
	}
	if current.entries == nil {
		current.entries = map[string]time.Time{}
	}

	legacyCutoff := false
	if existed && current.readerContract < minReaderVersion {
		// This list was written by a build that could lose revocations, and the
		// write below is about to stamp the current contract on it — erasing
		// the only evidence that the sanitizing floor is owed. Apply it here
		// too, so a revocation that reaches an unmigrated list cannot be the
		// thing that makes the migration never happen.
		legacyCutoff = sanitizeLegacyList(&current)
	}
	current.readerContract = minReaderVersion
	current.entries[principal] = at
	current.prune(at)

	if err := writeFile(path, current); err != nil {
		return err
	}
	mod, size := identity(path)

	// Adopt immediately so this process enforces without waiting for refresh.
	s.mu.Lock()
	if s.path != path {
		// SetPath moved the target mid-write. The write landed on the old file
		// and is durable there; publishing it against the new path would be
		// wrong, so leave the fresh state alone.
		s.mu.Unlock()
		return nil
	}
	s.adoptLocked(current, mod, size)
	if legacyCutoff {
		s.legacyCutoffApplied = true
		s.legacyCutoffAt = current.before
	}
	if s.status == StatusDegraded {
		s.status = StatusHealthy
		s.invariant = ""
		s.detail = ""
	}
	kept := countEntries(current.entries)
	s.mu.Unlock()

	// Same reason as in Provision: a revocation can be the operation that
	// migrates the list, and that is still the moment everybody gets signed out.
	// Announced with no state lock held, after the write is durable.
	if legacyCutoff {
		announceLegacyCutoff(path, current.before, kept)
	}
	return nil
}

// prune drops entries whose covered tokens have all expired on their own, and
// retires the global floor once it can no longer refuse a live token.
func (l *list) prune(now time.Time) {
	cutoff := now.Add(-retentionPeriod)
	for id, at := range l.entries {
		if at.Before(cutoff) {
			delete(l.entries, id)
		}
	}
	if !l.before.IsZero() && l.before.Before(cutoff) {
		l.before = time.Time{}
	}
}

// refresh keeps this process's view current and, while the authority is not
// enforcing, retries provisioning. It performs no disk I/O itself and never
// waits on another goroutine's.
func (s *state) refresh() {
	now := time.Now()

	s.mu.RLock()
	// enforcing implies provisioned, so reaching reload() below means there is
	// a file to reload.
	enforcing := s.enforcingLocked()
	fresh := now.Sub(s.lastCheck) < refreshInterval
	repairDue := !enforcing && !now.Before(s.nextRepairAttempt)
	s.mu.RUnlock()

	if !enforcing {
		// A process that failed to provision — a full disk at boot, a state
		// directory not mounted yet — used to stay fail-closed until somebody
		// restarted it, because nothing ever tried again. Retry, paced, so a
		// transient fault heals itself.
		if repairDue {
			s.repair()
		}
		return
	}
	if fresh {
		return
	}
	s.reload()
}

// repair re-runs provisioning and self-validation for a process that is not
// currently enforcing. It is paced and single-flighted; a permanently broken
// state (a corrupt file) costs one stat and one read every few seconds and
// keeps failing closed, exactly as before.
//
// What it costs when it fires on a legitimate request: the one goroutine that
// wins the TryLock does the provisioning work inline, and if a peer service
// holds the cross-process lock it can wait up to lockWait for it. Every other
// caller fails the TryLock and returns at once. That is bounded, it happens on
// a process that is already refusing every token, and the alternative is the
// behaviour being fixed — refusing every token until a human restarts the
// service.
func (s *state) repair() {
	if !s.ioMu.TryLock() {
		return
	}
	defer s.ioMu.Unlock()

	s.mu.Lock()
	if s.enforcingLocked() || time.Now().Before(s.nextRepairAttempt) {
		s.mu.Unlock()
		return
	}
	s.nextRepairAttempt = time.Now().Add(provisionRetryInterval)
	s.mu.Unlock()

	if err := Provision(); err != nil {
		return // status already names the invariant; still failing closed
	}
	_ = SelfTest()
}

// reload re-reads the file when it has changed. The stat, read and unmarshal
// happen with NO lock held; s.mu is taken only to publish, so a slow disk
// delays this refresh instead of stopping every request in the process.
func (s *state) reload() {
	// Never wait: a caller that finds a refresh already running serves the
	// snapshot it has. Staleness stays bounded by refreshInterval either way,
	// and queueing behind someone else's disk is the cost this avoids.
	if !s.ioMu.TryLock() {
		return
	}
	defer s.ioMu.Unlock()

	s.mu.RLock()
	path := s.path
	gen := s.generation
	lastMod, lastSize := s.lastMod, s.lastSize
	fresh := time.Since(s.lastCheck) < refreshInterval
	s.mu.RUnlock()
	if fresh {
		return
	}

	atDiskIO("reload")
	info, serr := os.Stat(path)
	unchanged := false
	var l list
	var rerr error
	if serr == nil {
		unchanged = info.ModTime().Equal(lastMod) && info.Size() == lastSize
		if !unchanged {
			l, rerr = readFile(path)
		}
	}

	atDiskIO("reload-publish")

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != path || s.generation != gen {
		// SetPath ran, or this process wrote a newer snapshot while we were
		// reading. Publishing what we read would move the view backwards — a
		// revocation this process just made would disappear from it.
		return
	}
	s.lastCheck = time.Now()

	switch {
	case serr != nil && errors.Is(serr, os.ErrNotExist):
		// The list was provisioned and has since disappeared. That is not
		// the fresh-install case — something removed it — so do not treat
		// it as "nothing is revoked". Keep enforcing what we hold and say so.
		s.status = StatusDegraded
		s.invariant = "revocation list must remain present"
		s.detail = fmt.Sprintf("%s disappeared after provisioning", path)

	case serr != nil:
		s.status = StatusDegraded
		s.invariant = "revocation list must remain readable"
		s.detail = serr.Error()

	case unchanged:
		if s.status == StatusDegraded {
			s.status = StatusHealthy
			s.invariant = ""
			s.detail = ""
		}

	case rerr != nil:
		// Never silently un-revoke on a bad read: keep the last good snapshot
		// and report degraded.
		s.status = StatusDegraded
		s.invariant = "revocation list must parse"
		s.detail = rerr.Error()

	default:
		if l.entries == nil {
			l.entries = map[string]time.Time{}
		}
		s.revoked = l.entries
		// The cutoff may only ever move FORWARD from a refresh.
		//
		// Provisioning applies a sanitizing floor to a file written by a build
		// that predates the reader contract, because such a file cannot be
		// interpreted safely. This is the same file, re-read on the request
		// path every refreshInterval — and it had no contract check at all, so
		// a pre-contract file replaced on disk would silently LOWER the floor
		// a second later and hand back sessions provisioning had just cut off.
		//
		// The obvious caller was protected and the equivalent primitive was
		// left open. Refusing to lower it here closes the pair: a refresh can
		// tighten revocation, never loosen it.
		if l.readerContract >= minReaderVersion || l.before.After(s.revokedBefore) {
			s.revokedBefore = l.before
		}
		s.lastMod = info.ModTime()
		s.lastSize = info.Size()
		s.status = StatusHealthy
		s.invariant = ""
		s.detail = ""
	}
}

type fileFormat struct {
	// Version is the schema this file was written by. Absent (0) means the
	// unversioned first draft, which is read as version 1 — same field shape.
	// It is informational: compatibility is decided by MinReaderVersion, so
	// that a later build can add fields and still be readable here.
	Version int `json:"version"`

	// MinReaderVersion is the oldest reader the writer vouches for. A reader
	// that is older refuses the file rather than misreading it. Absent (0)
	// means the file predates the contract, which also identifies a list
	// written before updates were serialized across processes.
	MinReaderVersion int `json:"min_reader_version,omitempty"`

	// RevokedBefore refuses every token issued at or before this unix second,
	// for every principal. Zero means no such floor.
	RevokedBefore int64 `json:"revoked_before,omitempty"`

	// Revoked holds unix seconds keyed by principal ID. Seconds are enough:
	// token lifetimes are hours and days, and a coarser stamp errs toward
	// refusing slightly more.
	Revoked map[string]int64 `json:"revoked"`
}

func readFile(path string) (list, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return list{}, err
	}
	if len(data) == 0 {
		// A zero-length file is what an interrupted non-atomic write would
		// leave. Ours are atomic, so treat it as an empty list rather than
		// corruption — it is the same content Provision would create. It is
		// reported as current-contract because it holds nothing that an older
		// build could have lost.
		return list{entries: map[string]time.Time{}, readerContract: minReaderVersion}, nil
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return list{}, err
	}
	if f.MinReaderVersion > minReaderVersion {
		// The writer said this content needs a newer reader. Refuse rather
		// than guess: misreading a revocation list fails open.
		return list{}, fmt.Errorf("revocation list requires reader version %d, this build is %d",
			f.MinReaderVersion, minReaderVersion)
	}
	out := list{
		entries:        make(map[string]time.Time, len(f.Revoked)),
		readerContract: f.MinReaderVersion,
	}
	if f.RevokedBefore > 0 {
		out.before = time.Unix(f.RevokedBefore, 0)
	}
	for id, sec := range f.Revoked {
		out.entries[id] = time.Unix(sec, 0)
	}
	return out, nil
}

func writeFile(path string, l list) error {
	f := fileFormat{
		Version:          schemaVersion,
		MinReaderVersion: minReaderVersion,
		Revoked:          make(map[string]int64, len(l.entries)),
	}
	if !l.before.IsZero() {
		f.RevokedBefore = l.before.Unix()
	}
	for id, at := range l.entries {
		f.Revoked[id] = at.Unix()
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create revocation list dir: %w", err)
	}

	// Atomic replace: a reader sees the whole old file or the whole new one,
	// never a truncated one. On any failure below the temp file is removed and
	// the existing list is untouched, so a full or read-only disk costs the
	// caller an error and costs the box no revocation it already had.
	tmp, err := os.CreateTemp(dir, ".token-revocations-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp revocation list: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp revocation list: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp revocation list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp revocation list: %w", err)
	}
	if err := os.Chmod(tmpName, fileMode); err != nil {
		return fmt.Errorf("chmod temp revocation list: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace revocation list: %w", err)
	}

	// Sync the directory so the rename itself survives a power cut, not just
	// the bytes it points at. Without this a revocation reported as durable
	// could be missing after an unclean shutdown. Best effort: on a filesystem
	// that refuses to open a directory the write has still happened.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
