package storageauth

import (
	"os"
	"path/filepath"
	"testing"
)

// The whole-filesystem SEARCH switch: separate from the system-disk switch,
// off for everyone by default, never for a guest, and in effect only together
// with system-disk access.

func TestSystemSearchIsOffByDefaultForEveryoneIncludingAdmin(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	for _, p := range []Principal{admin(), user("7")} {
		if WholeFilesystemSearch(p) {
			t.Errorf("%s: whole-filesystem search on by default", p.Role)
		}
		// Even with system-disk access: the search is its own switch.
		switchOn(t, p)
		if WholeFilesystemSearch(p) {
			t.Errorf("%s: system-disk access alone turned whole-filesystem search on", p.Role)
		}
	}
	if _, err := os.Stat(filepath.Join(f.state, systemSearchFileName)); err == nil {
		t.Error("provisioning wrote a search switch file; absent must be the off state")
	}
}

// DECISION: the search switch requires the system-disk switch, refused at set
// time and re-checked at decision time.
func TestSystemSearchRequiresSystemDiskAccess(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	a := admin()

	if err := SetSystemSearchAccess(a.ID, a.Role, true); err == nil {
		t.Fatal("search switch turned on for an account without system-disk access")
	}
	switchOn(t, a)
	if err := SetSystemSearchAccess(a.ID, a.Role, true); err != nil {
		t.Fatalf("SetSystemSearchAccess with system-disk on: %v", err)
	}
	if !WholeFilesystemSearch(a) {
		t.Fatal("both switches on, but whole-filesystem search is not in effect")
	}

	// Turning system-disk access off later disables the search on the next
	// decision, though the search switch stays recorded.
	if err := SetSystemDiskAccess(a.ID, a.Role, false); err != nil {
		t.Fatal(err)
	}
	if WholeFilesystemSearch(a) {
		t.Fatal("whole-filesystem search still in effect after system-disk access was turned off")
	}
	if !SystemSearchAccessFor(a.ID) {
		t.Fatal("the recorded search switch was silently dropped")
	}
	// Off is always accepted.
	if err := SetSystemSearchAccess(a.ID, a.Role, false); err != nil {
		t.Fatalf("turning the search switch off: %v", err)
	}
}

func TestSystemSearchNeverForGuest(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	if err := SetSystemSearchAccess("9", "guest", true); err == nil {
		t.Fatal("the guest account was given whole-filesystem search")
	}
	// A record that became wrong later (an account demoted to guest) is refused
	// at decision time too.
	u := user("9")
	switchOn(t, u)
	if err := SetSystemSearchAccess(u.ID, u.Role, true); err != nil {
		t.Fatal(err)
	}
	if WholeFilesystemSearch(guest("9")) {
		t.Fatal("a demoted guest kept whole-filesystem search")
	}
}

// One account's switch never applies to another.
func TestSystemSearchIsPerAccount(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	a := admin()
	switchOn(t, a)
	if err := SetSystemSearchAccess(a.ID, a.Role, true); err != nil {
		t.Fatal(err)
	}
	if WholeFilesystemSearch(user("7")) {
		t.Fatal("account 7 inherited account 1's search switch")
	}
}

// The switch survives a restart: it is persisted in the authority's state.
func TestSystemSearchPersistsAcrossReprovision(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	a := admin()
	switchOn(t, a)
	if err := SetSystemSearchAccess(a.ID, a.Role, true); err != nil {
		t.Fatal(err)
	}
	SetStateDir(f.state)
	f.ensure()
	if !WholeFilesystemSearch(a) {
		t.Fatal("whole-filesystem search lost across a re-provision")
	}
}

// An unreadable switch file fails the authority rather than reading as off.
func TestSystemSearchCorruptFileFailsClosed(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, systemSearchFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(f.deps()); err == nil {
		t.Fatal("the authority provisioned over a corrupt search switch file")
	}
}

// The walk guard: kernel filesystems are not descended, an operator DENY is
// not descended, and the withheld credentials are not returned as hits.
func TestSearchScopeSkipsKernelFSDeniedAndWithheld(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	requireKernelFS(t)
	a := admin()
	switchOn(t, a)
	if err := SetSystemSearchAccess(a.ID, a.Role, true); err != nil {
		t.Fatal(err)
	}
	s := NewSearchScope(a)

	if !s.Descend("/", "/") || !s.Descend("/", "/etc") {
		t.Fatal("the walk may not enter / or /etc with both switches on")
	}
	for _, dir := range []string{"/proc", "/sys", "/sys/class"} {
		if s.Descend("/", dir) {
			t.Errorf("the walk descends into kernel filesystem %s", dir)
		}
	}
	if err := AddOperatorDeny(f.outside, "1", "test"); err != nil {
		t.Fatal(err)
	}
	if s.Descend("/", f.outside) {
		t.Errorf("the walk descends into operator-denied %s", f.outside)
	}
	for _, p := range []string{"/etc/shadow", "/etc/gshadow", "/var/lib/casaos/jwt_ec_private.pem", "/etc/ssh/ssh_host_ed25519_key"} {
		if !s.Hide(p) {
			t.Errorf("withheld %s would be returned as a search hit", p)
		}
	}
	if s.Hide("/etc/hostname") {
		t.Error("an ordinary file is hidden from search")
	}

	// Another account with no switch gets nothing from this scope's principal.
	if NewSearchScope(guest("9")).Descend("/", "/etc") {
		t.Error("a guest's scope may enter /etc")
	}
}
