//go:build linux

package storageauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests exercise the window itself, not the intention. Each one performs
// the swap an attacker would perform, at the moment it would matter, and
// asserts the kernel refused rather than that the code meant well.

// The core race: authorization sees a legitimate directory, an attacker
// replaces it with a symlink pointing out of storage, and the open happens
// afterwards. Lexical validation cannot catch this — the string never changed.
//
// Mutation that must fail this test: drop RESOLVE_BENEATH from the Resolve
// mask, or fall back to os.OpenFile when confinement is available.
func TestAncestorSwappedAfterAuthorizationCannotEscape(t *testing.T) {
	if ok, why := ConfinementAvailable(); !ok {
		t.Skipf("kernel confinement unavailable: %s", why)
	}
	f := newFixture(t)
	f.ensure()

	victim := filepath.Join(f.outside, "secret")
	if err := os.WriteFile(victim, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A legitimate directory inside storage, with a file in it.
	legit := filepath.Join(f.data, "workdir")
	if err := os.MkdirAll(legit, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(legit, "secret")
	if err := os.WriteFile(target, []byte("yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Authorization happens now, against the honest tree.
	d := AuthorizeStorage(admin(), OpRead, target)
	if !d.Allowed {
		t.Fatalf("precondition: the honest path should be authorized: %+v", d)
	}
	if d.ConfineRoot == "" {
		t.Fatal("the decision named no confinement boundary; the open has nothing to be held to")
	}

	// THE SWAP. Between deciding and opening, the ancestor becomes a symlink
	// pointing out of storage. Every string involved is unchanged.
	if err := os.RemoveAll(legit); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, legit); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Open with the decision made BEFORE the swap. This is the actual TOCTOU
	// shape and the only one that exercises the kernel: calling OpenAuthorized
	// here would re-authorize, the authority would re-resolve, and the swap
	// would be caught before the syscall — a pass that proves nothing about
	// confinement. The first version of this test did exactly that, and said
	// so in its own log line.
	fh, err := openConfined(d, os.O_RDONLY, 0)
	if err == nil {
		defer fh.Close()
		got := make([]byte, 32)
		n, _ := fh.Read(got)
		t.Fatalf("a stale authorization followed the swapped ancestor and read %q — "+
			"the kernel is not confining resolution", string(got[:n]))
	}
	if !strings.Contains(err.Error(), "escaped its authorized boundary") {
		t.Fatalf("refused, but not by the confinement path, so this test is not exercising "+
			"openat2: %v", err)
	}
}

// The same swap, but the attacker points INSIDE authorized storage. That must
// keep working: the authority already decided the subtree is usable, and
// refusing would make the mitigation an outage for ordinary symlinks.
func TestSwapWithinTheBoundaryStillOpens(t *testing.T) {
	if ok, _ := ConfinementAvailable(); !ok {
		t.Skip("kernel confinement unavailable")
	}
	f := newFixture(t)
	f.ensure()

	real := filepath.Join(f.data, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(f.data, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fh, _, err := OpenAuthorized(admin(), OpRead, filepath.Join(link, "f"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("a symlink staying inside authorized storage was refused: %v", err)
	}
	defer fh.Close()
	buf := make([]byte, 16)
	n, _ := fh.Read(buf)
	if string(buf[:n]) != "content" {
		t.Fatalf("read %q, want %q", string(buf[:n]), "content")
	}
}

// A magic link (/proc/self/fd/N) is not an ordinary symlink: following it
// reopens an arbitrary descriptor, which would sidestep the boundary entirely.
func TestMagicLinksAreRefused(t *testing.T) {
	if ok, _ := ConfinementAvailable(); !ok {
		t.Skip("kernel confinement unavailable")
	}
	f := newFixture(t)
	f.ensure()

	outside, err := os.CreateTemp(f.outside, "target-*")
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()

	magic := filepath.Join(f.data, "magic")
	if err := os.Symlink(filepath.Join("/proc/self/fd", itoaFD(int(outside.Fd()))), magic); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if fh, _, err := OpenAuthorized(admin(), OpRead, magic, os.O_RDONLY, 0); err == nil {
		fh.Close()
		t.Fatal("a magic link resolved; RESOLVE_NO_MAGICLINKS is not in force")
	}
}

// ".." must not walk out even though the authority's lexical cleaning would
// already have collapsed it — the kernel enforces this during resolution,
// which is what makes it race-proof.
func TestParentTraversalIsRefusedAtOpenTime(t *testing.T) {
	if ok, _ := ConfinementAvailable(); !ok {
		t.Skip("kernel confinement unavailable")
	}
	f := newFixture(t)
	f.ensure()

	d := AuthorizeStorage(admin(), OpRead, filepath.Join(f.data, "x"))
	if !d.Allowed {
		t.Fatalf("precondition: %+v", d)
	}
	// Hand openConfined a decision whose target sits outside its own boundary.
	bad := d
	bad.Resolved = filepath.Join(f.outside, "escape")
	if _, err := openConfined(bad, os.O_RDONLY, 0); err == nil {
		t.Fatal("a target outside its own boundary was opened")
	}
}

// The confinement boundary must be the principal's GRANT when one applied, not
// the storage root: confining to the root would leave a swap free to move the
// target into another user's area while staying inside authorized storage.
func TestConfinementUsesTheGrantNotTheCeiling(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	alice := user("700")
	aliceDir := filepath.Join(f.data, "u", "alice")
	if err := os.MkdirAll(aliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, Grant{PrincipalID: alice.ID, Path: aliceDir, Ops: OpRead, Origin: OriginOwner})

	d := AuthorizeStorage(alice, OpRead, filepath.Join(aliceDir, "f"))
	if !d.Allowed {
		t.Fatalf("precondition: %+v", d)
	}
	if d.ConfineRoot != aliceDir {
		t.Fatalf("ConfineRoot = %q, want the grant %q — confining to the ceiling would let a "+
			"swap cross into another user's area", d.ConfineRoot, aliceDir)
	}
}

// A denied request must never reach the open at all.
func TestOpenAuthorizedRefusesBeforeOpening(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	probe := filepath.Join(f.outside, "should-not-be-created")
	fh, d, err := OpenAuthorized(admin(), OpCreate|OpWrite, probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fh.Close()
		t.Fatal("an unauthorized path was opened")
	}
	if d.Allowed {
		t.Fatal("the decision claims allowed while the open refused")
	}
	if _, statErr := os.Stat(probe); statErr == nil {
		t.Fatal("the refused open still created the file")
	}
}

// Hammering the swap while opens run concurrently: every attempt must either
// open something inside the boundary or fail. Never something outside.
func TestConcurrentSwapNeverYieldsAnEscape(t *testing.T) {
	if ok, _ := ConfinementAvailable(); !ok {
		t.Skip("kernel confinement unavailable")
	}
	f := newFixture(t)
	f.ensure()

	if err := os.WriteFile(filepath.Join(f.outside, "loot"), []byte("ESCAPED"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.data, "hot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loot"), []byte("SAFE"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "loot")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the attacker
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.RemoveAll(dir)
			os.Symlink(f.outside, dir)
			os.Remove(dir)
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "loot"), []byte("SAFE"), 0o600)
		}
	}()

	escapes := 0
	for i := 0; i < 800; i++ {
		fh, _, err := OpenAuthorized(admin(), OpRead, target, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		buf := make([]byte, 16)
		n, _ := fh.Read(buf)
		fh.Close()
		if strings.Contains(string(buf[:n]), "ESCAPED") {
			escapes++
		}
	}
	close(stop)
	wg.Wait()

	if escapes > 0 {
		t.Fatalf("%d of 800 opens read data from outside authorized storage while an ancestor "+
			"was being swapped — the confinement is not atomic", escapes)
	}
}

// When the kernel cannot confine, the caller must be TOLD, not silently given a
// weaker guarantee. The fallback is the pre-existing behaviour, and a caller
// that requires the guarantee can refuse on this error.
func TestUnavailableConfinementIsReportedNotHidden(t *testing.T) {
	f := newFixture(t)
	f.ensure()

	d := AuthorizeStorage(admin(), OpRead, filepath.Join(f.data, "x"))
	if !d.Allowed {
		t.Fatalf("precondition: %+v", d)
	}
	noBoundary := d
	noBoundary.ConfineRoot = ""

	if err := os.WriteFile(filepath.Join(f.data, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := openConfined(noBoundary, os.O_RDONLY, 0)
	if fh != nil {
		defer fh.Close()
	}
	if !errors.Is(err, ErrConfinementUnavailable) {
		t.Fatalf("a decision with no boundary opened without reporting the missing guarantee: %v", err)
	}
}

func itoaFD(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
