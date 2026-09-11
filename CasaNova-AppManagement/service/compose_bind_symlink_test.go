package service

// filepath.Clean is a string operation. It cannot see that an allowed bind
// source is a SYMLINK into a denied one — and Docker resolves the host path
// when it mounts, so the container gets the link's target, not its name.
//
// The denylist already handles the lexical cases (the path itself, a child of
// it, and an ancestor of it). This pins the one it could not see.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDangerousBindSourceFollowsSymlinks(t *testing.T) {
	tmp := t.TempDir()

	link := filepath.Join(tmp, "hostlink")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink here: %v", err)
	}

	if !isDangerousBindSource(link) {
		t.Error("a symlink to /etc was accepted as a bind source — the container receives /etc")
	}
	// A path UNDER the link resolves the same way: the deepest existing
	// ancestor is the link itself and the tail is re-appended.
	if !isDangerousBindSource(filepath.Join(link, "shadow")) {
		t.Error("a path under a symlink to /etc was accepted — the container receives /etc/shadow")
	}
}

// TestDangerousBindSourceStillAllowsOrdinaryPaths is the positive control. A
// guard that answered "dangerous" to everything would pass the test above.
func TestDangerousBindSourceStillAllowsOrdinaryPaths(t *testing.T) {
	tmp := t.TempDir()

	real := filepath.Join(tmp, "appdata")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if isDangerousBindSource(real) {
		t.Fatal("an ordinary existing directory was refused as a bind source")
	}

	okLink := filepath.Join(tmp, "oklink")
	if err := os.Symlink(real, okLink); err != nil {
		t.Skipf("cannot create symlink here: %v", err)
	}
	if isDangerousBindSource(okLink) {
		t.Fatal("a symlink to a harmless directory was refused — resolution must only ADD denials for paths that land somewhere denied")
	}

	// A source that does not exist yet is the COMMON case: Docker creates it.
	// It cannot be a symlink to anywhere, so it keeps its lexical verdict.
	if isDangerousBindSource(filepath.Join(tmp, "not-created-yet", "sub")) {
		t.Fatal("a not-yet-created bind source was refused; Docker creates these routinely")
	}
}

// TestDangerousBindSourceLexicalCasesUnchanged pins the behaviour that already
// existed, so the added resolution cannot have weakened any of it.
func TestDangerousBindSourceLexicalCasesUnchanged(t *testing.T) {
	for _, src := range []string{"/etc", "/etc/passwd", "/", "/var/run", "/run/docker.sock", "/proc", "/usr/bin"} {
		if !isDangerousBindSource(src) {
			t.Errorf("%q is no longer refused", src)
		}
	}
	for _, src := range []string{"/DATA", "/DATA/AppData/foo", "/mnt/pool/media"} {
		if isDangerousBindSource(src) {
			t.Errorf("%q is now refused but was allowed before", src)
		}
	}
}
