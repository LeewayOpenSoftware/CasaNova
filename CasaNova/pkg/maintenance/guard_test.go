package maintenance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The chokepoint's tests. Each one was written by breaking the guard it
// covers, watching the test fail, and putting the guard back — a test that
// still passes with the check removed is not a test, it is decoration.

func newTestGuard(t *testing.T, roots ...string) *Guard {
	t.Helper()
	g, err := NewGuard(roots, nil)
	if err != nil {
		t.Fatalf("NewGuard(%v): %v", roots, err)
	}
	return g
}

// A symlink inside an allowed root must not let a removal act on what it
// points at. There is one on this box today (a mysql.sock under /DATA aimed at
// /var/run) and anyone with an SMB share can plant another.
func TestCheckRefusesSymlinkOutOfRoot(t *testing.T) {
	tmp := t.TempDir()
	allowed := filepath.Join(tmp, "allowed")
	outside := filepath.Join(tmp, "outside")
	mkdirAll(t, allowed, outside)
	writeFile(t, filepath.Join(outside, "victim"), "do not delete me")

	if err := os.Symlink(outside, filepath.Join(allowed, "escape")); err != nil {
		t.Fatal(err)
	}

	g := newTestGuard(t, allowed)
	_, err := g.Check(filepath.Join(allowed, "escape", "victim"))
	if !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("symlinked parent accepted: err=%v", err)
	}
}

// Same escape, but through Remove: the file at the other end must survive even
// when the caller hands us the identity of the real inode.
func TestRemoveRefusesSymlinkedParent(t *testing.T) {
	tmp := t.TempDir()
	allowed := filepath.Join(tmp, "allowed")
	outside := filepath.Join(tmp, "outside")
	mkdirAll(t, allowed, outside)
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "do not delete me")
	if err := os.Symlink(outside, filepath.Join(allowed, "escape")); err != nil {
		t.Fatal(err)
	}

	st, err := lstatNoFollow(victim)
	if err != nil {
		t.Fatal(err)
	}
	tgt := target{
		Path:  filepath.Join(allowed, "escape", "victim"),
		Kind:  KindFile,
		Dev:   st.Dev,
		Ino:   st.Ino,
		MTime: mtimeOf(&st),
	}

	g := newTestGuard(t, allowed)
	if err := g.Remove(tgt, nil); err == nil {
		t.Fatal("Remove followed a symlinked parent out of the root")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("victim outside the root was deleted: %v", err)
	}
}

// Rule 3's minimum depth: never a first- or second-level component.
func TestCheckEnforcesMinimumDepth(t *testing.T) {
	g := newTestGuard(t, "/DATA")
	for _, p := range []string{"/DATA/anything", "/DATA/x"} {
		if _, err := g.Check(p); !errors.Is(err, ErrTooShallow) {
			t.Errorf("Check(%q) = %v, want ErrTooShallow", p, err)
		}
	}
}

// The hard deny-list: storage roots, our own state directory, the trash and
// lost+found, at any depth.
func TestCheckEnforcesDenyList(t *testing.T) {
	g := newTestGuard(t, "/DATA", "/var/log", "/var/lib/casanova/updates")
	cases := []struct {
		path string
		want error
	}{
		{"/DATA", ErrTooShallow},                      // a storage root is never a target
		{"/var/lib/casanova", ErrDenied},              // our own state directory
		{"/DATA/disk1/lost+found", ErrDenied},         // component
		{"/DATA/disk1/lost+found/file", ErrDenied},    // component, deeper
		{"/DATA/.Trash-casanova/files/x", ErrDenied},  // the undo layer of another feature
		{"/var/lib/casaos/apps/foo", ErrOutsideRoots}, // not a root at all
		{"/etc/ssh/sshd_config", ErrOutsideRoots},     // outside every root
		{"/var/log/../lib/casanova/db/x", ErrDenied},  // non-canonical input
		{"/var/logger/x/y", ErrOutsideRoots},          // boundary: not /var/log
	}
	for _, c := range cases {
		_, err := g.Check(c.path)
		if !errors.Is(err, c.want) {
			t.Errorf("Check(%q) = %v, want %v", c.path, err, c.want)
		}
	}
}

// The reconciliation with utils.BlockedSystemPrefixes: a path may sit under a
// blocked prefix only when an allowed root strictly inside that prefix covers
// it. This is what lets /var/log work without reopening /var/lib/casaos/db.
func TestCheckReconcilesWithBlockedSystemPrefixes(t *testing.T) {
	g := newTestGuard(t, "/var/log", "/var/cache/apt/archives")

	// Under a blocked prefix but covered by a declared root.
	if _, err := g.Check("/var/log/syslog.1"); err != nil {
		t.Fatalf("Check(/var/log/syslog.1) = %v, want it accepted", err)
	}
	// Under the same blocked prefix, NOT covered by any root.
	if _, err := g.Check("/var/lib/casaos/db/jwt_ec_private.pem"); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("a /var path with no covering root was accepted: %v", err)
	}
}

// A root that is a blocked system prefix as a whole would exempt everything
// under it and silently disable the canonical blocklist. Same shape as
// utils.withinDataRoot's own guard.
func TestNewGuardRefusesRootThatIsASystemPrefix(t *testing.T) {
	for _, root := range []string{"/", "/var", "/etc", "/usr"} {
		if _, err := NewGuard([]string{root}, nil); err == nil {
			t.Errorf("NewGuard(%q) accepted a root that must never be one", root)
		}
	}
}

func TestNewGuardRejectsEmptyRootSet(t *testing.T) {
	if _, err := NewGuard(nil, nil); err == nil {
		t.Fatal("a guard with no roots was accepted")
	}
}

// Roots are matched longest-first so a path under /var/log/journal descends
// from the journal root, not from /var/log.
func TestRootForPrefersTheLongestRoot(t *testing.T) {
	g := newTestGuard(t, "/var/log", "/var/log/journal")
	if got := g.rootFor("/var/log/journal/abc/def"); got != "/var/log/journal" {
		t.Fatalf("rootFor = %q, want /var/log/journal", got)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func mkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A storage root that is itself a symlink is a documented layout (the
// /var/lib/casaos/files fallback). Paths under it must keep working, while an
// escape from inside it must still be refused.
func TestGuardToleratesASymlinkedRoot(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-storage")
	outside := filepath.Join(tmp, "outside")
	mkdirAll(t, filepath.Join(real, "sub"), outside)
	writeFile(t, filepath.Join(real, "sub", "leftover.casanova-tmp"), "scratch")
	writeFile(t, filepath.Join(outside, "victim"), "keep me")

	root := filepath.Join(tmp, "DATA")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "escape")); err != nil {
		t.Fatal(err)
	}

	g := newTestGuard(t, root)

	inside := filepath.Join(root, "sub", "leftover.casanova-tmp")
	if _, err := g.Check(inside); err != nil {
		t.Fatalf("a path under a symlinked root was rejected: %v", err)
	}
	tgt := scanTarget(t, inside, time.Time{})
	if err := g.Remove(tgt, nil); err != nil {
		t.Fatalf("Remove under a symlinked root failed: %v", err)
	}

	if _, err := g.Check(filepath.Join(root, "escape", "victim")); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("an escape from inside a symlinked root was accepted: %v", err)
	}
}
