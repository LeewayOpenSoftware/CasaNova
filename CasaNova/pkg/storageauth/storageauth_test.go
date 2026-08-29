package storageauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture builds a realistic appliance layout in a temp tree and provisions the
// authority against it. Everything is relative to the temp root so the tests
// exercise real symlinks and real directories without touching the host.
type fixture struct {
	t       *testing.T
	root    string
	data    string // canonical storage root
	mount   string // a disk the storage service reports
	share   string // a configured share
	appdata string
	outside string // legitimately outside every storage area — stands in for /home
	state   string

	mounts []string
	users  []Principal

	mountsErr error
	usersErr  error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:       t,
		root:    root,
		data:    filepath.Join(root, "DATA"),
		mount:   filepath.Join(root, "mnt", "disk1"),
		share:   filepath.Join(root, "srv", "share"),
		appdata: filepath.Join(root, "DATA", "AppData"),
		outside: filepath.Join(root, "home", "victim"),
		state:   filepath.Join(root, "var", "lib", "casanova"),
	}
	for _, d := range []string{f.data, f.mount, f.share, f.appdata, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.mounts = []string{f.mount}
	SetStateDir(f.state)
	t.Cleanup(func() { SetStateDir("/var/lib/casanova") })
	return f
}

func (f *fixture) deps() Deps {
	return Deps{
		DataRoots: []string{f.data},
		Mounts:    func() ([]string, error) { return f.mounts, f.mountsErr },
		Shares:    func() ([]string, error) { return []string{f.share}, nil },
		AppData:   func() (string, error) { return f.appdata, nil },
		NonAdminPrincipals: func() ([]Principal, error) {
			return f.users, f.usersErr
		},
	}
}

func (f *fixture) ensure() {
	f.t.Helper()
	if err := Ensure(f.deps()); err != nil {
		f.t.Fatalf("Ensure: %v", err)
	}
}

func admin() Principal { return Principal{ID: "1", Role: "admin", Admin: true} }
func user(id string) Principal {
	return Principal{ID: id, Role: "user"}
}

// ---------------------------------------------------------------------------
// BOUNDARY 1 — the ceiling. This is the finding that started it: the inherited
// sanitizer accepted /home, /tmp, / and every unlisted top-level directory.
// ---------------------------------------------------------------------------

// Mutation that must fail this test: drop the withinAny(ceiling, ...) check in
// Authorize, restoring the deny-list model.
func TestPathOutsideEveryStorageAreaIsRefusedEvenForAdmin(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	target := filepath.Join(f.outside, ".ssh", "authorized_keys")
	if _, err := Authorize(admin(), target, OpWrite); err == nil {
		t.Fatal("an administrator was allowed to write outside every storage area; the ceiling is not a boundary")
	} else if !IsDenial(err) {
		t.Fatalf("refusal is not a *Denial: %v", err)
	}

	// The ceiling is a maximum, not a user-level rule: it must bind the most
	// privileged principal too.
	if _, err := Authorize(admin(), filepath.Join(f.data, "movies", "a.mkv"), OpWrite); err != nil {
		t.Fatalf("an administrator was refused inside real storage: %v", err)
	}
}

// The appliance must still serve everything it is configured to serve. A
// hardcoded {/DATA,/mnt,/media} would refuse the share and the external disk,
// which is worse than the defect being fixed.
func TestEveryConfiguredStorageAreaRemainsReachable(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	for name, p := range map[string]string{
		"canonical data root": filepath.Join(f.data, "x"),
		"mounted disk":        filepath.Join(f.mount, "x"),
		"configured share":    filepath.Join(f.share, "x"),
		"application data":    filepath.Join(f.appdata, "x"),
	} {
		if _, err := Authorize(admin(), p, OpRead); err != nil {
			t.Fatalf("%s became unreachable: %v", name, err)
		}
	}
}

// A disk plugged in after boot has to become reachable without a restart, or
// the boundary is a snapshot of history rather than of what is served.
func TestDiskMountedAfterBootBecomesReachable(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	late := filepath.Join(f.root, "mnt", "disk2")
	if err := os.MkdirAll(late, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Authorize(admin(), filepath.Join(late, "x"), OpRead); err == nil {
		t.Fatal("a path was allowed before its disk was reported mounted")
	}

	f.mounts = append(f.mounts, late)
	forceRederive()

	if _, err := Authorize(admin(), filepath.Join(late, "x"), OpRead); err != nil {
		t.Fatalf("a newly mounted disk stayed unreachable: %v", err)
	}
}

// forceRederive expires the refresh window so the next call re-derives.
func forceRederive() {
	global.mu.Lock()
	global.derivedAt = time.Now().Add(-2 * refreshInterval)
	global.mu.Unlock()
}

// ---------------------------------------------------------------------------
// SYMLINKS AND BIND MOUNTS — the escape that made the inherited deny-list
// decorative. A lexical boundary is not a boundary.
// ---------------------------------------------------------------------------

// Mutation that must fail this test: judge only the lexical path (drop the
// resolved check from Authorize).
func TestSymlinkOutOfStorageIsRefused(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	link := filepath.Join(f.data, "escape")
	if err := os.Symlink(f.outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	target := filepath.Join(link, ".ssh", "authorized_keys")
	if _, err := Authorize(admin(), target, OpWrite); err == nil {
		t.Fatal("a write through a symlink pointing out of storage was allowed")
	}
	if _, err := Authorize(admin(), target, OpRead); err == nil {
		t.Fatal("a read through a symlink pointing out of storage was allowed")
	}
}

// A link between two storage areas is routine on a NAS (/DATA/media pointing at
// a disk) and must keep working — refusing it would be the mitigation becoming
// the outage.
func TestSymlinkBetweenStorageAreasStillWorks(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	link := filepath.Join(f.data, "media")
	if err := os.Symlink(f.mount, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Authorize(admin(), filepath.Join(link, "film.mkv"), OpRead); err != nil {
		t.Fatalf("a link between two storage areas was refused: %v", err)
	}
}

// Deleting a symlink that points outside must remain possible: unlinking it
// never touches the target, and refusing would leave the user unable to clean
// up a link they can plainly see. The PARENT is still resolved, so a link in
// the directory chain cannot smuggle the entry elsewhere.
func TestDeletingALinkThatPointsOutsideIsAllowedButWritingThroughItIsNot(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	link := filepath.Join(f.data, "stray")
	if err := os.Symlink(f.outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Authorize(admin(), link, OpDelete); err != nil {
		t.Fatalf("a stray link inside storage could not be removed: %v", err)
	}
	if _, err := Authorize(admin(), filepath.Join(link, "file"), OpWrite); err == nil {
		t.Fatal("writing through the stray link was allowed")
	}
}

// ---------------------------------------------------------------------------
// BOUNDARY 2 — per-principal entitlement. The role model promised "their own
// files" and nothing implemented it.
// ---------------------------------------------------------------------------

// Mutation that must fail this test: return clean,nil for every principal after
// the ceiling check, i.e. delete the grant loop.
func TestUserCannotReachAnotherUsersData(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	alice, bob := user("10"), user("11")
	aliceDir := filepath.Join(f.data, "users", "alice")
	bobDir := filepath.Join(f.data, "users", "bob")
	for _, d := range []string{aliceDir, bobDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mustGrant(t, Grant{PrincipalID: alice.ID, Path: aliceDir, Ops: OpRead | OpWrite | OpDelete, Origin: OriginOwner})
	mustGrant(t, Grant{PrincipalID: bob.ID, Path: bobDir, Ops: OpRead | OpWrite | OpDelete, Origin: OriginOwner})

	if _, err := Authorize(alice, filepath.Join(aliceDir, "notes.txt"), OpWrite); err != nil {
		t.Fatalf("a user was refused inside their own area: %v", err)
	}
	if _, err := Authorize(alice, filepath.Join(bobDir, "notes.txt"), OpRead); err == nil {
		t.Fatal("a user read another user's data; the per-principal boundary does not exist")
	}
	if _, err := Authorize(alice, filepath.Join(bobDir, "notes.txt"), OpDelete); err == nil {
		t.Fatal("a user could delete another user's data")
	}
}

// A grant carries operations, not blanket access: read does not imply write.
func TestGrantOperationsAreEnforcedSeparately(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	reader := user("20")
	dir := filepath.Join(f.data, "readonly")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, Grant{PrincipalID: reader.ID, Path: dir, Ops: OpRead, Origin: OriginShare})

	if _, err := Authorize(reader, filepath.Join(dir, "f"), OpRead); err != nil {
		t.Fatalf("read was refused despite a read grant: %v", err)
	}
	if _, err := Authorize(reader, filepath.Join(dir, "f"), OpWrite); err == nil {
		t.Fatal("a read-only grant permitted a write")
	}
	if _, err := Authorize(reader, filepath.Join(dir, "f"), OpDelete); err == nil {
		t.Fatal("a read-only grant permitted a delete")
	}
}

// An explicit share is how one principal reaches data it does not own. It must
// work, and it must not widen beyond its subtree.
func TestExplicitShareGrantsExactlyItsSubtree(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	guestish := user("30")
	shared := filepath.Join(f.data, "photos", "holiday")
	sibling := filepath.Join(f.data, "photos", "private")
	for _, d := range []string{shared, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustGrant(t, Grant{PrincipalID: guestish.ID, Path: shared, Ops: OpRead, Origin: OriginShare})

	if _, err := Authorize(guestish, filepath.Join(shared, "a.jpg"), OpRead); err != nil {
		t.Fatalf("the shared subtree was not reachable: %v", err)
	}
	if _, err := Authorize(guestish, filepath.Join(sibling, "a.jpg"), OpRead); err == nil {
		t.Fatal("a share leaked into its sibling directory")
	}
	// Prefix-without-separator must not be treated as containment.
	if _, err := Authorize(guestish, shared+"-other/a.jpg", OpRead); err == nil {
		t.Fatal("a sibling whose name merely shares a prefix was treated as inside the grant")
	}
}

// A symlink inside one user's own area pointing into another's must not let the
// first read the second through their own legitimate grant.
func TestUserCannotEscapeTheirGrantThroughASymlink(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	alice, bob := user("40"), user("41")
	aliceDir := filepath.Join(f.data, "u", "alice")
	bobDir := filepath.Join(f.data, "u", "bob")
	for _, d := range []string{aliceDir, bobDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bobDir, "secret"), []byte("bob's"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, Grant{PrincipalID: alice.ID, Path: aliceDir, Ops: OpRead | OpWrite, Origin: OriginOwner})
	mustGrant(t, Grant{PrincipalID: bob.ID, Path: bobDir, Ops: OpRead | OpWrite, Origin: OriginOwner})

	link := filepath.Join(aliceDir, "peek")
	if err := os.Symlink(bobDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Authorize(alice, filepath.Join(link, "secret"), OpRead); err == nil {
		t.Fatal("a user read another user's file through a symlink inside their own granted area")
	}
}

// A grant cannot promise more than the ceiling covers, or a later ceiling
// change would silently widen access.
func TestGrantOutsideTheCeilingIsRefused(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	err := AddGrant(Grant{PrincipalID: "50", Path: f.outside, Ops: OpRead, Origin: OriginOperator})
	if err == nil {
		t.Fatal("a grant was accepted for a path outside every storage area")
	}
}

func mustGrant(t *testing.T, g Grant) {
	t.Helper()
	if err := AddGrant(g); err != nil {
		t.Fatalf("AddGrant(%s): %v", g.Path, err)
	}
}

// ---------------------------------------------------------------------------
// UPGRADE FROM LEGACY STATE
// ---------------------------------------------------------------------------

// The migration must NOT carry the legacy semantic across the upgrade.
//
// "Anything not blacklisted is allowed" meant, at the principal layer, "every
// account reaches everything". Granting each existing account the whole ceiling
// would import exactly that under a new name. So the migration grants nothing,
// existing accounts are denied with an explainable reason, and health reports
// how many are waiting so an administrator sees it rather than a user
// discovering it.
//
// Mutation that must fail this test: have the migration grant the ceiling to
// every enumerated principal.
func TestUpgradeDoesNotGrandfatherLegacyReach(t *testing.T) {
	f := newFixture(t)
	f.users = []Principal{user("60"), user("61")}
	f.ensure()

	if got := len(Grants()); got != 0 {
		t.Fatalf("the migration created %d grants; it must not import legacy reach", got)
	}

	d := AuthorizeStorage(user("60"), OpWrite, filepath.Join(f.data, "existing.txt"))
	if d.Allowed {
		t.Fatal("an account kept unrestricted reach across the upgrade")
	}
	if d.Reason != ReasonPrincipalNotAuthorized {
		t.Fatalf("reason = %q, want %q so the UI can explain it", d.Reason, ReasonPrincipalNotAuthorized)
	}

	// The compatibility consequence has to be visible, not discovered.
	h := CurrentHealth()
	if h.PendingPrincipals != 2 {
		t.Fatalf("PendingPrincipals = %d, want 2", h.PendingPrincipals)
	}
	if h.Detail == "" {
		t.Fatal("health does not explain that accounts are awaiting storage grants")
	}

	// And an administrator can resolve it explicitly.
	mustGrant(t, Grant{PrincipalID: "60", Path: f.data, Ops: OpRead | OpWrite, Origin: OriginOperator})
	if d := AuthorizeStorage(user("60"), OpWrite, filepath.Join(f.data, "existing.txt")); !d.Allowed {
		t.Fatalf("an explicit administrative grant did not take effect: %+v", d)
	}
}

// A fresh appliance with no non-admin accounts is strict from its first boot,
// with nothing for the administrator to review.
func TestFreshInstallHasNoLegacyGrants(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if h := CurrentHealth(); h.LegacyGrants != 0 {
		t.Fatalf("a fresh install created %d legacy grants", h.LegacyGrants)
	}
	if _, err := Authorize(user("70"), filepath.Join(f.data, "x"), OpRead); err == nil {
		t.Fatal("an ungranted principal was allowed on a fresh install")
	}
}

// The migration must not run twice and must not disturb grants made since.
func TestMigrationIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.users = []Principal{user("80")}
	f.ensure()
	first := len(Grants())

	mustGrant(t, Grant{PrincipalID: "81", Path: f.data, Ops: OpRead, Origin: OriginOperator})

	SetStateDir(f.state)
	f.ensure()

	if got := len(Grants()); got != first+1 {
		t.Fatalf("grants after re-provisioning = %d, want %d; the migration ran again or dropped a grant", got, first+1)
	}
	if _, err := Authorize(user("81"), filepath.Join(f.data, "x"), OpRead); err != nil {
		t.Fatalf("a grant made after migration was lost on restart: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RESTART AND FAIL-SAFE
// ---------------------------------------------------------------------------

func TestGrantsSurviveRestart(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	dir := filepath.Join(f.data, "persistent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, Grant{PrincipalID: "90", Path: dir, Ops: OpRead, Origin: OriginOwner})

	// A restart: drop every scrap of in-memory state and come up again.
	SetStateDir(f.state)
	f.ensure()

	if _, err := Authorize(user("90"), filepath.Join(dir, "f"), OpRead); err != nil {
		t.Fatalf("a grant was forgotten across a restart: %v", err)
	}
	// And a second restart — fail-closed logic has turned fail-open there before.
	SetStateDir(f.state)
	f.ensure()
	if _, err := Authorize(user("91"), filepath.Join(dir, "f"), OpRead); err == nil {
		t.Fatal("an ungranted principal was allowed after a second restart")
	}
}

func TestUnprovisionedAuthorityRefusesEveryone(t *testing.T) {
	f := newFixture(t)
	SetStateDir(f.state) // provisioning deliberately not run

	if _, err := Authorize(admin(), filepath.Join(f.data, "x"), OpRead); err == nil {
		t.Fatal("an unprovisioned authority allowed an operation; it must fail closed")
	}
	h := CurrentHealth()
	if h.Enforcing || h.Status != StatusUnavailable || h.Invariant == "" {
		t.Fatalf("health = %+v, want unavailable, not enforcing, with a named invariant", h)
	}
}

// An unreadable grant store must not read as "nobody is entitled" and must not
// read as "everyone is". It reads as broken.
func TestCorruptGrantStoreFailsClosed(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, grantsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(f.deps()); err == nil {
		t.Fatal("provisioning accepted a corrupt grant store")
	}
	if _, err := Authorize(user("99"), filepath.Join(f.data, "x"), OpRead); err == nil {
		t.Fatal("a corrupt grant store left non-admin access open")
	}
	if h := CurrentHealth(); h.Enforcing || h.Invariant == "" {
		t.Fatalf("health = %+v, want not enforcing with a named invariant", h)
	}
}

// Losing sight of the mounts must narrow the boundary, never widen it.
func TestDerivationFailureFallsBackToTheFloorAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	f.mountsErr = os.ErrPermission
	forceRederive()

	h := CurrentHealth()
	if h.Status != StatusDegraded {
		t.Fatalf("status = %q, want %q", h.Status, StatusDegraded)
	}
	if h.Invariant == "" || h.Detail == "" {
		t.Fatalf("degraded health does not explain itself: %+v", h)
	}
	// Narrower, not wider: the canonical root still works, the lost mount does not.
	if _, err := Authorize(admin(), filepath.Join(f.data, "x"), OpRead); err != nil {
		t.Fatalf("the fail-safe floor was lost: %v", err)
	}
	if _, err := Authorize(admin(), f.outside, OpRead); err == nil {
		t.Fatal("derivation failure widened the boundary")
	}
}

// The operator escape hatch must be explicit, durable, and incapable of
// producing "allow everything".
func TestOperatorRootIsExplicitDurableAndCannotBeRoot(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if err := AddOperatorRoot("/", "everything"); err == nil {
		t.Fatal("the filesystem root was accepted as a storage root")
	}

	extra := filepath.Join(f.root, "storage")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Authorize(admin(), filepath.Join(extra, "x"), OpRead); err == nil {
		t.Fatal("an undeclared root was reachable")
	}
	if err := AddOperatorRoot(extra, "big disk"); err != nil {
		t.Fatalf("AddOperatorRoot: %v", err)
	}
	if _, err := Authorize(admin(), filepath.Join(extra, "x"), OpRead); err != nil {
		t.Fatalf("a declared root stayed unreachable: %v", err)
	}

	// Durable across a restart.
	SetStateDir(f.state)
	f.ensure()
	if _, err := Authorize(admin(), filepath.Join(extra, "x"), OpRead); err != nil {
		t.Fatalf("an operator root did not survive a restart: %v", err)
	}
}

// EffectiveRoots is what a UI should show. It must never show a principal a
// place they cannot act on.
func TestEffectiveRootsMatchWhatIsActuallyAuthorized(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	u := user("100")
	dir := filepath.Join(f.data, "u100")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, Grant{PrincipalID: u.ID, Path: dir, Ops: OpRead, Origin: OriginOwner})

	roots := EffectiveRoots(u)
	if len(roots) != 1 || roots[0].Path != dir {
		t.Fatalf("EffectiveRoots = %+v, want exactly %s", roots, dir)
	}
	for _, r := range roots {
		if _, err := Authorize(u, filepath.Join(r.Path, "probe"), OpRead); err != nil {
			t.Fatalf("EffectiveRoots advertised %s but authorization refuses it: %v", r.Path, err)
		}
	}
	if len(EffectiveRoots(admin())) == 0 {
		t.Fatal("an administrator was shown no roots at all")
	}
}

func TestConcurrentAuthorizeAndGrantMutation(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_, _ = Authorize(user("110"), filepath.Join(f.data, "x"), OpRead)
			_ = CurrentHealth()
		}
	}()
	for i := 0; i < 20; i++ {
		_ = AddGrant(Grant{PrincipalID: "110", Path: f.data, Ops: OpRead, Origin: OriginOperator})
	}
	<-done
}

// The fail-closed gate must be load-bearing on its own.
//
// TestUnprovisionedAuthorityRefusesEveryone passes even with the gate removed,
// because an unprovisioned authority also has an empty ceiling and the ceiling
// check refuses anyway — the right outcome for the wrong reason. This exercises
// the state the gate alone covers: provisioning succeeded, a real ceiling was
// derived, and then self-validation failed. Access inside that ceiling must
// still be refused.
//
// Mutation that must fail this test: force `enforcing` true in Authorize.
func TestFailedSelfValidationRefusesEvenInsideARealCeiling(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	inside := filepath.Join(f.data, "real.txt")
	if _, err := Authorize(admin(), inside, OpRead); err != nil {
		t.Fatalf("precondition: admin should reach storage while healthy: %v", err)
	}

	// Self-validation fails the way a real defect would, leaving the derived
	// ceiling in place.
	global.mu.Lock()
	global.validated = false
	global.failLocked("the negative boundary must hold", os.ErrInvalid)
	ceilingSize := len(global.ceiling)
	global.mu.Unlock()

	if ceilingSize == 0 {
		t.Fatal("precondition: the ceiling should still be populated")
	}
	if _, err := Authorize(admin(), inside, OpRead); err == nil {
		t.Fatal("a failed self-validation still allowed access inside the ceiling; the fail-closed gate is not load-bearing")
	}
	h := CurrentHealth()
	if h.Enforcing || h.Status != StatusValidationFailed || h.Invariant == "" {
		t.Fatalf("health = %+v, want validation_failed, not enforcing, with a named invariant", h)
	}
}

// ---------------------------------------------------------------------------
// PRECEDENCE AND SYSTEM HARD BOUNDARIES
// ---------------------------------------------------------------------------

// A system location is not configurable. Letting an explicit ALLOW override it
// would turn the Permissions panel into a disable-security switch.
//
// Mutation that must fail this test: evaluate the explicit rules before the
// system check.
func TestSystemLocationCannotBeOverriddenByAnExplicitRoot(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	pol := loadPolicy()
	if pol == nil || !pol.mountinfoOK || len(pol.systemMounts) == 0 {
		t.Skip("no pseudo-filesystem mounts visible in this environment")
	}
	sys := pol.systemMounts[0].point

	d := AuthorizeStorage(admin(), OpRead, filepath.Join(sys, "probe"))
	if d.Allowed {
		t.Fatalf("a %s location was authorized", pol.systemMounts[0].fstype)
	}
	if d.Reason != ReasonSystemLocation {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonSystemLocation)
	}
	if d.Remediable {
		t.Fatal("a system location was advertised as fixable by configuration; the UI would offer a button that cannot work")
	}

	// Even after an operator declares it, the hard boundary stands.
	if err := AddOperatorRoot(sys, "deliberate override attempt"); err == nil {
		if d := AuthorizeStorage(admin(), OpRead, filepath.Join(sys, "probe")); d.Allowed {
			t.Fatal("an explicit operator root overrode a system hard boundary")
		}
	}
}

// An explicit DENY outranks any ALLOW covering the same subtree, whichever is
// more specific, and it cannot be escaped through a link.
func TestExplicitDenyOutranksAllow(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	secret := filepath.Join(f.data, "payroll")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AddOperatorDeny(secret, "1", "sensitive"); err != nil {
		t.Fatalf("AddOperatorDeny: %v", err)
	}

	d := AuthorizeStorage(admin(), OpRead, filepath.Join(secret, "x.csv"))
	if d.Allowed {
		t.Fatal("an explicit DENY was overridden by the derived ALLOW that contains it")
	}
	if d.Reason != ReasonExplicitlyDenied {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonExplicitlyDenied)
	}

	// A link inside allowed storage pointing at the denied subtree must not
	// launder it.
	link := filepath.Join(f.data, "shortcut")
	if err := os.Symlink(secret, link); err == nil {
		if d := AuthorizeStorage(admin(), OpRead, filepath.Join(link, "x.csv")); d.Allowed {
			t.Fatal("a symlink laundered an explicit DENY")
		}
	}
}

// Denials must be distinguishable by code, because the UI reacts differently:
// one offers "add this location", another says "ask your administrator".
func TestReasonCodesDistinguishTheDenials(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if d := AuthorizeStorage(admin(), OpRead, filepath.Join(f.outside, "x")); d.Reason != ReasonOutsideAuthorizedStorage || !d.Remediable {
		t.Fatalf("outside storage: %+v, want OUTSIDE_AUTHORIZED_STORAGE and remediable", d)
	}
	if d := AuthorizeStorage(user("200"), OpRead, filepath.Join(f.data, "x")); d.Reason != ReasonPrincipalNotAuthorized {
		t.Fatalf("ungranted principal: %+v, want PRINCIPAL_NOT_AUTHORIZED", d)
	}
	if d := AuthorizeStorage(admin(), OpRead, "relative/path"); d.Reason != ReasonInvalidPath {
		t.Fatalf("relative path: %+v, want INVALID_PATH", d)
	}
	// A principal denial must not disclose whether the location exists.
	d := AuthorizeStorage(user("200"), OpRead, filepath.Join(f.data, "does-not-exist"))
	if strings.Contains(d.Detail, "does-not-exist") {
		t.Fatalf("the denial disclosed the requested path to an unauthorized principal: %q", d.Detail)
	}
}

// Renaming across the authority boundary must fail, and validating only the
// source is how a move becomes an escape.
func TestRenameValidatesBothEndpoints(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	src := filepath.Join(f.data, "file.txt")
	if _, _, err := AuthorizeRename(admin(), src, filepath.Join(f.outside, "file.txt")); err == nil {
		t.Fatal("a rename whose destination is outside authorized storage was allowed")
	}
	if _, _, err := AuthorizeRename(admin(), filepath.Join(f.outside, "x"), filepath.Join(f.data, "x")); err == nil {
		t.Fatal("a rename whose source is outside authorized storage was allowed")
	}
	if _, _, err := AuthorizeRename(admin(), src, filepath.Join(f.mount, "file.txt")); err != nil {
		t.Fatalf("a legitimate cross-root move was refused: %v", err)
	}
}

// Authorization sits on filesystem hot paths. Authorizing many paths must not
// re-read the mount table or re-parse policy per path — the structural
// invariant, not a wall-clock budget, because a clock budget tight enough to
// catch the regression also trips on a loaded CI box.
func TestAuthorizingManyPathsDoesNotRereadTheMountTable(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	before := PolicyVersion()
	for i := 0; i < 5000; i++ {
		_ = AuthorizeStorage(admin(), OpRead, filepath.Join(f.data, "dir", "file"))
	}
	if after := PolicyVersion(); after != before {
		t.Fatalf("policy was rebuilt %d times during 5000 authorizations; the snapshot is not being reused", after-before)
	}
}

// A reporting failure must never take down the authority.
//
// This shipped. On the production box the account list could not be enumerated
// from the main service, the migration treated that as fatal, the authority
// never provisioned, and AuthorizeStorage failed closed — so every file
// operation answered 503 POLICY_UNAVAILABLE. Listing /DATA in the file manager
// returned "the storage authority is not in force".
//
// The enumeration feeds ONE health counter. The migration grants nothing
// either way, so no security decision depends on it. Refusing to answer a
// reporting question must not disable the thing being reported on.
//
// Mutation that must fail this test: return the enumeration error from
// migrateLegacyLocked again.
func TestAnUnreadableAccountListDoesNotDisableTheAuthority(t *testing.T) {
	f := newFixture(t)
	d := f.deps()
	d.NonAdminPrincipals = func() ([]Principal, error) {
		return nil, errors.New("account enumeration is not wired in this service")
	}

	if err := Ensure(d); err != nil {
		t.Fatalf("an unreadable account list disabled the whole authority: %v", err)
	}

	// The authority is in force: real storage is reachable.
	if d := AuthorizeStorage(admin(), OpRead, filepath.Join(f.data, "movies")); !d.Allowed {
		t.Fatalf("file operations are refused because a health counter could not be filled: %+v", d)
	}
	// And the boundary still holds.
	if d := AuthorizeStorage(admin(), OpRead, filepath.Join(f.outside, "x")); d.Allowed {
		t.Fatal("the boundary was relaxed while degrading the counter")
	}

	// Health says "unknown" rather than claiming zero.
	h := CurrentHealth()
	if h.Status != StatusHealthy || !h.Enforcing {
		t.Fatalf("health = %+v, want healthy and enforcing", h)
	}
	if h.PendingPrincipals != pendingUnknown {
		t.Fatalf("PendingPrincipals = %d, want %d (unknown, not a claim of zero)",
			h.PendingPrincipals, pendingUnknown)
	}
	if h.Detail == "" {
		t.Fatal("health does not say WHY the account count is unknown")
	}
}
