package storageauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole-disk VIEW the switch promises: with it on, every directory from "/"
// can be listed — kernel filesystems included — while what must never be READ
// stays refused with its reason stated, and nothing about writing moves.
//
// These use the host's real /proc and /sys because rule 3 is decided from the
// real mount table; a fixture directory named "proc" is, correctly, just a
// directory.

func requireKernelFS(t *testing.T) {
	t.Helper()
	pol := loadPolicy()
	if pol == nil || !pol.mountinfoOK {
		t.Skip("mount table unreadable here; rule 3 cannot be exercised")
	}
	for _, m := range []string{"/proc", "/sys"} {
		if _, _, ok := pol.systemLocation(m); !ok {
			t.Skipf("%s is not a kernel filesystem in this environment", m)
		}
	}
}

func switchOn(t *testing.T, p Principal) {
	t.Helper()
	if err := SetSystemDiskAccess(p.ID, p.Role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess(%s): %v", p.ID, err)
	}
}

// RULE: switched on, the kernel filesystems are LISTED like any other directory.
// This is the bug: /proc, /sys and /dev answered SYSTEM_LOCATION even to a
// switched-on administrator, so "/" could be opened but not explored.
func TestSystemDiskListsKernelFilesystems(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	requireKernelFS(t)

	for _, p := range []Principal{admin(), user("7")} {
		switchOn(t, p)
		for _, dir := range []string{"/", "/proc", "/sys", "/sys/class"} {
			d := AuthorizeListing(p, dir)
			if !d.Allowed {
				t.Errorf("%s: listing %s refused with the switch on: %s (%s)", p.Role, dir, d.Reason, d.Detail)
				continue
			}
			if d.Reason != ReasonAllowedSystemDisk {
				t.Errorf("%s: listing %s reason = %q, want %q", p.Role, dir, d.Reason, ReasonAllowedSystemDisk)
			}
		}
	}
}

// RULE: listing is not reading. Opening a file in a kernel filesystem stays
// SYSTEM_LOCATION with the switch on — /proc/kcore is the machine's memory and
// a sysfs read can act on hardware — and the refusal says why.
func TestSystemDiskNeverReadsKernelFiles(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	requireKernelFS(t)

	a := admin()
	switchOn(t, a)
	for _, path := range []string{"/proc/kcore", "/proc/cpuinfo", "/sys/kernel/uevent_seqnum"} {
		d := AuthorizeStorage(a, OpRead, path)
		if d.Allowed {
			t.Errorf("%s: READ allowed through the switch", path)
			continue
		}
		if d.Reason != ReasonSystemLocation {
			t.Errorf("%s: reason = %q, want %q", path, d.Reason, ReasonSystemLocation)
		}
		if !strings.Contains(d.Detail, "listed") {
			t.Errorf("%s: detail does not tell a switched-on account why it can list but not read: %q", path, d.Detail)
		}
	}
}

// RULE: with the switch OFF, listing is AuthorizeStorage(OpRead) exactly —
// the kernel filesystems stay SYSTEM_LOCATION and "/" stays outside storage.
func TestSystemDiskOffListingIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	requireKernelFS(t)

	for _, p := range []Principal{admin(), user("2"), guest("3")} {
		for _, path := range []string{"/", "/etc", "/proc", "/sys/class", f.outside, filepath.Join(f.data, "x")} {
			list := AuthorizeListing(p, path)
			read := AuthorizeStorage(p, OpRead, path)
			if list.Allowed != read.Allowed || list.Reason != read.Reason {
				t.Errorf("%s %s: listing (%v %s) differs from read (%v %s) with the switch off",
					p.Role, path, list.Allowed, list.Reason, read.Allowed, read.Reason)
			}
		}
	}
	if d := AuthorizeListing(admin(), "/proc"); d.Reason != ReasonSystemLocation {
		t.Errorf("/proc with the switch off: reason = %q, want %q", d.Reason, ReasonSystemLocation)
	}
}

// RULE: an operator DENY outranks the listing exception exactly as it outranks
// every other allow — including inside a kernel filesystem, where rule 3 had
// already returned before rule 4 could be reached.
func TestSystemDiskListingYieldsToOperatorDeny(t *testing.T) {
	f := newFixture(t)
	f.ensure()
	requireKernelFS(t)

	a := admin()
	switchOn(t, a)
	for _, deny := range []string{"/proc/sys", f.outside} {
		if err := AddOperatorDeny(deny, "1", "test"); err != nil {
			t.Fatalf("AddOperatorDeny(%s): %v", deny, err)
		}
		d := AuthorizeListing(a, deny)
		if d.Allowed || d.Reason != ReasonExplicitlyDenied {
			t.Errorf("listing %s under an operator DENY: allowed=%v reason=%q, want %q",
				deny, d.Allowed, d.Reason, ReasonExplicitlyDenied)
		}
	}
}

// RULE: the appliance's and the host's credentials are listed but not read.
// The switch can be given to a non-administrator, and reading the JWT signing
// key is minting an administrator's token — not a read.
func TestSystemDiskWithholdsCredentials(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	u := user("7")
	switchOn(t, u)

	// A credential by name, wherever DBPath was configured.
	for _, name := range []string{"jwt_ec_private.pem", "user.db", "user.db-wal", "casaOS.db"} {
		p := filepath.Join(f.outside, name)
		if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		d := AuthorizeStorage(u, OpRead, p)
		if d.Allowed || d.Reason != ReasonSystemLocation || !strings.HasPrefix(d.Rule, "withheld:") {
			t.Errorf("%s: read through the switch = allowed=%v reason=%q rule=%q, want withheld", name, d.Allowed, d.Reason, d.Rule)
		}
		if l := AuthorizeListing(u, p); !l.Allowed {
			t.Errorf("%s: its NAME could not even be listed: %s", name, l.Reason)
		}
	}
	// A symlink to one is withheld too: judged on where it lands.
	link := filepath.Join(f.outside, "innocent")
	if err := os.Symlink(filepath.Join(f.outside, "jwt_ec_private.pem"), link); err != nil {
		t.Fatal(err)
	}
	if d := AuthorizeStorage(u, OpRead, link); d.Allowed {
		t.Error("a symlink to the signing key was readable through the switch")
	}
	// Fixed host paths.
	for _, p := range []string{"/etc/shadow", "/etc/gshadow", "/var/lib/casaos/db/x", "/etc/ssh/ssh_host_ed25519_key"} {
		if d := AuthorizeStorage(u, OpRead, p); d.Allowed {
			t.Errorf("%s readable through the switch", p)
		}
	}
	// And what is NOT a credential stays readable: the switch is a view of the
	// whole disk, not a second blocklist.
	for _, p := range []string{"/etc/hostname", "/etc/ssh/ssh_host_ed25519_key.pub", "/usr/bin"} {
		if d := AuthorizeStorage(u, OpRead, p); !d.Allowed {
			t.Errorf("%s refused through the switch: %s (%s)", p, d.Reason, d.Detail)
		}
	}
}

// RULE: reading /etc is allowed through the switch; writing it is not, for
// any write operation, administrator included.
func TestSystemDiskReadsEtcButNeverWritesIt(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	a := admin()
	switchOn(t, a)
	if d := AuthorizeStorage(a, OpRead, "/etc"); !d.Allowed {
		t.Fatalf("reading /etc refused with the switch on: %s", d.Reason)
	}
	if d := AuthorizeListing(a, "/etc"); !d.Allowed {
		t.Fatalf("listing /etc refused with the switch on: %s", d.Reason)
	}
	for _, op := range []Op{OpWrite, OpCreate, OpDelete, OpRename, OpUpload, OpExtract, OpRead | OpWrite} {
		d := AuthorizeStorage(a, op, "/etc/hostname")
		if d.Allowed {
			t.Errorf("%s on /etc/hostname allowed by a READ switch", op)
		}
		if d.Reason != ReasonOutsideAuthorizedStorage {
			t.Errorf("%s on /etc/hostname: reason = %q, want %q", op, d.Reason, ReasonOutsideAuthorizedStorage)
		}
	}
}

// RULE: guest never, listing included.
func TestSystemDiskListingNeverForGuest(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	g := guest("3")
	if err := SetSystemDiskAccess(g.ID, g.Role, true); err == nil {
		t.Fatal("the setter accepted the guest account")
	}
	// A record that became wrong afterwards: switched on as a user, now guest.
	switchOn(t, user("3"))
	for _, p := range []string{"/", "/etc", "/proc"} {
		if d := AuthorizeListing(g, p); d.Allowed {
			t.Errorf("guest listed %s through a stale switch record", p)
		}
	}
}

// RULE: when the mount table could not be read the authority cannot tell what
// is mounted at /proc, and that refusal is NOT converted into a listing — the
// exception is for a kernel filesystem the authority has positively identified.
func TestSystemDiskListingKeepsTheUnverifiableRefusal(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	a := admin()
	switchOn(t, a)
	global.mu.RLock()
	ceiling := append([]Root{}, global.ceiling...)
	global.mu.RUnlock()
	publish(buildPolicy(PolicyVersion(), ceiling, nil, nil, false))

	d := AuthorizeListing(a, "/proc")
	if d.Allowed || d.Rule != "system:unverifiable" {
		t.Errorf("listing /proc with an unreadable mount table: allowed=%v rule=%q, want the unverifiable refusal", d.Allowed, d.Rule)
	}
}
