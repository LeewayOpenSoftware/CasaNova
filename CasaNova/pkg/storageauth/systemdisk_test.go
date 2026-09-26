package storageauth

import (
	"os"
	"path/filepath"
	"testing"
)

// The per-account system-disk switch. Every test below names ONE rule from the
// specification, because the value of this feature is entirely in which of them
// hold simultaneously: a switch that lets an account browse the whole disk is
// only safe while the boundaries it does NOT move stay put.

func guest(id string) Principal { return Principal{ID: id, Role: "guest"} }

// RULE: off by default, for EVERYONE, administrators included.
//
// This is the rule most likely to be lost by a later "convenience" change, and
// the one the user stated most emphatically. An administrator already passes
// the principal layer unconditionally, so the only thing standing between them
// and the whole disk is the ceiling — which is exactly what this switch moves.
func TestSystemDiskIsOffByDefaultForEveryoneIncludingAdmin(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	for _, p := range []Principal{admin(), user("2"), guest("3")} {
		if SystemDiskAccessFor(p.ID) {
			t.Errorf("%s: the switch is on before anybody set it", p.Role)
		}
		d := AuthorizeStorage(p, OpRead, "/")
		if d.Allowed {
			t.Errorf("%s: browsing / was allowed with the switch off", p.Role)
		}
		if d.Reason != ReasonOutsideAuthorizedStorage {
			t.Errorf("%s: reason = %q, want %q", p.Role, d.Reason, ReasonOutsideAuthorizedStorage)
		}
	}
}

// RULE: an administrator turns their own switch on, and then / lists.
func TestAdminCanSwitchTheirOwnSystemDiskAccessOn(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	a := admin()
	if err := SetSystemDiskAccess(a.ID, a.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}

	d := AuthorizeStorage(a, OpRead, "/")
	if !d.Allowed {
		t.Fatalf("/ was still refused with the switch on: %s (%s)", d.Reason, d.Detail)
	}
	if d.Reason != ReasonAllowedSystemDisk {
		t.Errorf("reason = %q, want %q so an audit log can tell this apart from an operator-declared root", d.Reason, ReasonAllowedSystemDisk)
	}
	if d.ConfineRoot != "/" {
		t.Errorf("ConfineRoot = %q, want \"/\"; the open must be confined to what was actually authorized", d.ConfineRoot)
	}
}

// RULE: an administrator turns it on for ANOTHER account, and that account —
// which holds no grant at all — can then read the whole disk.
//
// The non-administrator case is the one that exercises BOTH halves of the
// implementation: rule 6.5 for the parts outside authorized storage, and the
// principal layer for the parts inside it.
func TestAdminCanSwitchAnotherAccountOnAndThatAccountCanRead(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	u := user("7")
	if err := SetSystemDiskAccess(u.ID, u.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}

	// Outside authorized storage: rule 6.5.
	if d := AuthorizeStorage(u, OpRead, f.outside); !d.Allowed {
		t.Errorf("a switched-on account could not read outside authorized storage: %s", d.Reason)
	}
	// Inside it, with no grant: the principal layer.
	if d := AuthorizeStorage(u, OpRead, filepath.Join(f.data, "movies")); !d.Allowed {
		t.Errorf("a switched-on account was refused INSIDE authorized storage for want of a grant (%s); "+
			"it could read the rest of the disk, which is incoherent", d.Reason)
	}
	// And a second account is unaffected: the switch is per account.
	if d := AuthorizeStorage(user("8"), OpRead, f.outside); d.Allowed {
		t.Error("switching one account on switched another one on too")
	}
}

// RULE: guest NEVER, and not even an administrator can hand it over.
// Refused when set AND when read, because the two catch different mistakes:
// the setter catches the administrator, the decision catches a switched-on
// account that was demoted to guest afterwards.
func TestGuestCanNeverReceiveSystemDiskAccess(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if err := SetSystemDiskAccess("9", "guest", true); err == nil {
		t.Fatal("the guest account was given system-disk access; the specification forbids it outright")
	}
	if SystemDiskAccessFor("9") {
		t.Fatal("the refused switch was persisted anyway")
	}

	// Now the demotion case: switched on as a user, then read as a guest.
	if err := SetSystemDiskAccess("10", "user", true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}
	if d := AuthorizeStorage(guest("10"), OpRead, f.outside); d.Allowed {
		t.Error("an account switched on while it was a user kept whole-disk access after becoming a guest")
	}
}

// RULE: the switch is READ-ONLY. Writing outside authorized storage stays
// refused, with the switch on, for an administrator.
func TestSystemDiskSwitchGrantsNoWriteAnywhere(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	a := admin()
	if err := SetSystemDiskAccess(a.ID, a.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}

	target := filepath.Join(f.outside, ".ssh", "authorized_keys")
	for _, op := range []Op{OpWrite, OpCreate, OpDelete, OpUpload, OpRename} {
		if d := AuthorizeStorage(a, op, target); d.Allowed {
			t.Errorf("%s outside authorized storage was allowed by a READ switch", op)
		}
	}
	// Reading the same place is the whole point, so it must still work.
	if d := AuthorizeStorage(a, OpRead, f.outside); !d.Allowed {
		t.Errorf("reading was refused too, so the switch does nothing: %s", d.Reason)
	}
}

// RULE: rule 3 stays above the switch. /proc and friends are refused with
// SYSTEM_LOCATION even for a switched-on administrator, because the package's
// contract says an allow must never override it — a switch that could would be
// a disable-security control.
func TestSystemDiskSwitchDoesNotReachSystemLocations(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	pol := loadPolicy()
	if pol == nil || !pol.mountinfoOK || len(pol.systemMounts) == 0 {
		t.Skip("no pseudo-filesystem mounts visible in this environment")
	}
	sys := pol.systemMounts[0].point

	a := admin()
	if err := SetSystemDiskAccess(a.ID, a.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}

	d := AuthorizeStorage(a, OpRead, filepath.Join(sys, "probe"))
	if d.Allowed {
		t.Fatalf("%s was authorized for a switched-on administrator", sys)
	}
	if d.Reason != ReasonSystemLocation {
		t.Fatalf("reason = %q, want %q: the switch must not be able to promote a system location", d.Reason, ReasonSystemLocation)
	}
}

// RULE: an operator DENY (rule 4) also stays above the switch, so a subtree an
// administrator excluded stays excluded for switched-on accounts.
func TestSystemDiskSwitchDoesNotReachExplicitlyDeniedPaths(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	denied := filepath.Join(f.data, "payroll")
	if err := AddOperatorDeny(denied, "1", "sensitive"); err != nil {
		t.Fatalf("AddOperatorDeny: %v", err)
	}

	u := user("11")
	if err := SetSystemDiskAccess(u.ID, u.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}

	d := AuthorizeStorage(u, OpRead, filepath.Join(denied, "salaries.csv"))
	if d.Allowed {
		t.Fatal("a switched-on account read inside an operator DENY; the switch outranked a rule above it")
	}
	if d.Reason != ReasonExplicitlyDenied {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonExplicitlyDenied)
	}
}

// RULE: existing accounts gain nothing from the upgrade. A box provisioned with
// no switch file behaves exactly as one where every switch is off — the
// migration must not hand anybody whole-disk access.
func TestUpgradeWithNoSwitchFileGivesNobodyAccess(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if _, err := os.Stat(filepath.Join(f.state, systemDiskFileName)); err == nil {
		t.Fatal("provisioning created a switch file; an absent file must be the off state, not a written one")
	}
	if got := SystemDiskPrincipals(); len(got) != 0 {
		t.Fatalf("switched-on accounts after a fresh provision: %v", got)
	}
	if d := AuthorizeStorage(admin(), OpRead, "/"); d.Allowed {
		t.Fatal("/ was readable straight after an upgrade")
	}
}

// Turning it off again takes effect on the next decision. The switch lives in
// the authority's own state rather than in a token precisely so that revoking
// does not wait for a session to expire.
func TestSwitchingOffRevokesImmediately(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	a := admin()
	if err := SetSystemDiskAccess(a.ID, a.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess: %v", err)
	}
	if d := AuthorizeStorage(a, OpRead, "/"); !d.Allowed {
		t.Fatalf("precondition: / should be readable with the switch on (%s)", d.Reason)
	}

	if err := SetSystemDiskAccess(a.ID, a.Role, false); err != nil {
		t.Fatalf("SetSystemDiskAccess(off): %v", err)
	}
	if d := AuthorizeStorage(a, OpRead, "/"); d.Allowed {
		t.Fatal("/ was still readable after the switch was turned off")
	}
}
