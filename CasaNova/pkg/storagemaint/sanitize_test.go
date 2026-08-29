//go:build linux

package storagemaint

// TEST STRATEGY, AND WHAT THESE TESTS DO NOT PROVE
//
// Free-space sanitization is destructive by construction, so the tests run
// against a filesystem the test lab builds rather than against the developer's
// disk. There are two tiers, and the boundary between them is stated rather
// than blurred:
//
//	TIER 1 — SIMULATED CAPACITY, REAL FILESYSTEM (always runs)
//	  A real directory on a real filesystem, real files, real write(2) calls,
//	  real fsync, real unlink — but statfs is answered by the test so the
//	  "filesystem" is 128 MiB instead of the host's whole disk. This is what
//	  makes it possible to prove the reserve, the cleanup, the file-intactness
//	  and the crash recovery without a 400 GB test run. Everything except the
//	  size of the volume is genuine.
//
//	TIER 2 — LOOP DEVICE, REAL EVERYTHING (skips without privileges)
//	  A real ext4 image, a real loop device, a real mount, real statfs. It
//	  needs root, so it SKIPS on an unprivileged machine with a message saying
//	  what was not exercised.
//
//	NOT COVERED BY ANY TIER — DISCARD REACHING THE MEDIA
//	  A loop device does not reproduce a real SSD's discard behaviour, and no
//	  host-side test can observe what a controller does with a deallocated
//	  page. The TRIM tests therefore exercise the CONTROL FLOW and the
//	  CAPABILITY handling only: that the right helper is chosen, that arguments
//	  are structured, that progress is reported as indeterminate, and that the
//	  result never claims an erase. Whether a given drive actually erases
//	  anything is a hardware property and is deliberately not asserted.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

const (
	testFSTotal = 128 << 20
	testFSAvail = 96 << 20
	// With a 128 MiB volume the reserve lands on the absolute floor (64 MiB),
	// so a pass writes 32 MiB. Small enough to be fast, large enough to need
	// many chunks and several artifacts.
	testExpectedReserve = 64 << 20
	testExpectedTarget  = testFSAvail - testExpectedReserve
)

// ---------------------------------------------------------------- fixture

type sanitizeFixture struct {
	t       *testing.T
	base    string
	root    string // the filesystem's mount point and the authorized storage root
	proc    string
	sys     string
	dev     string
	devName string

	rotational  bool
	discard     bool
	fstype      string
	serial      string
	alsoAtSlash bool
	// partName, when set, puts the filesystem on a PARTITION of devName rather
	// than on the whole disk.
	partName string

	total uint64
	avail uint64
}

type fixtureOpt func(*sanitizeFixture)

func solidState() fixtureOpt     { return func(f *sanitizeFixture) { f.rotational = false } }
func withoutDiscard() fixtureOpt { return func(f *sanitizeFixture) { f.discard = false } }
func withFSType(s string) fixtureOpt {
	return func(f *sanitizeFixture) { f.fstype = s }
}

// alsoMountedAtRoot makes the same physical device carry "/" as well, which is
// how blockdev decides a disk is the system disk.
func alsoMountedAtRoot() fixtureOpt { return func(f *sanitizeFixture) { f.alsoAtSlash = true } }

// onAPartitionOfADiskWithNoSerial models the cheap USB stick: the filesystem
// sits on a partition, and the drive reports neither a serial nor a WWN, so
// every identity in play falls back to sysfs topology plus size. That is the
// only shape in which a partition's fingerprint and its parent disk's differ —
// with a serial they are identical, because a partition inherits it.
func onAPartitionOfADiskWithNoSerial() fixtureOpt {
	return func(f *sanitizeFixture) {
		f.serial = ""
		f.partName = f.devName + "1"
	}
}

func newSanitizeFixture(t *testing.T, opts ...fixtureOpt) *sanitizeFixture {
	t.Helper()
	if ok, why := storageauth.ConfinementAvailable(); !ok {
		t.Skip("kernel path confinement (openat2/RESOLVE_BENEATH) unavailable: " + why)
	}

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &sanitizeFixture{
		t: t, base: base,
		root:    filepath.Join(base, "disk1"),
		proc:    filepath.Join(base, "proc"),
		sys:     filepath.Join(base, "sys"),
		dev:     filepath.Join(base, "dev"),
		devName: "sdz", rotational: true, discard: true,
		fstype: "ext4", serial: "TESTSERIAL0001",
		total: testFSTotal, avail: testFSAvail,
	}
	for _, o := range opts {
		o(f)
	}

	for _, d := range []string{f.root, f.dev, filepath.Join(f.proc, "self"),
		filepath.Join(f.proc, "sys", "kernel", "random")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.writeSysfs()
	f.writeMountinfo(f.fsDevName(), f.fstype)
	write(t, filepath.Join(f.proc, "sys", "kernel", "random", "boot_id"), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n")

	blockdev.SetRoots(f.sys, f.proc, f.dev)
	SetSanitizeProcRoot(f.proc)
	t.Cleanup(func() {
		blockdev.SetRoots("/sys", "/proc", "/dev")
		SetSanitizeProcRoot("/proc")
	})

	storageauth.SetStateDir(filepath.Join(base, "authstate"))
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: []string{f.root}}); err != nil {
		t.Fatalf("provision the storage authority: %v", err)
	}
	t.Cleanup(func() { storageauth.SetStateDir("/var/lib/casanova") })

	SetStateDir(filepath.Join(base, "maintstate"))
	t.Cleanup(func() { SetStateDir("/var/lib/casanova/storage-maintenance") })

	f.installStatfs()
	f.installTools(nil)

	if h := EnsureSanitizeSubsystem(context.Background()); !h.Enforcing {
		t.Fatalf("subsystem not enforcing: %s / %s", h.Invariant, h.Detail)
	}
	return f
}

func (f *sanitizeFixture) writeSysfs() {
	f.t.Helper()
	base := filepath.Join(f.sys, "block", f.devName)
	if err := os.MkdirAll(filepath.Join(base, "queue"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "device"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	write(f.t, filepath.Join(base, "size"), "2097152\n") // 1 GiB in 512-byte sectors
	write(f.t, filepath.Join(base, "removable"), "1\n")
	write(f.t, filepath.Join(base, "ro"), "0\n")
	write(f.t, filepath.Join(base, "dev"), "8:100\n")
	rot := "0\n"
	if f.rotational {
		rot = "1\n"
	}
	write(f.t, filepath.Join(base, "queue", "rotational"), rot)
	disc := "0\n"
	if f.discard {
		disc = "2147450880\n"
	}
	write(f.t, filepath.Join(base, "queue", "discard_max_bytes"), disc)
	write(f.t, filepath.Join(base, "device", "model"), "CASANOVA TESTDISK\n")
	write(f.t, filepath.Join(base, "device", "vendor"), "CasaNova\n")
	write(f.t, filepath.Join(base, "device", "serial"), f.serial+"\n")

	if f.partName == "" {
		return
	}
	pbase := filepath.Join(base, f.partName)
	write(f.t, filepath.Join(pbase, "partition"), "1\n")
	write(f.t, filepath.Join(pbase, "size"), "2095104\n") // the disk minus the alignment gap
	write(f.t, filepath.Join(pbase, "ro"), "0\n")
	write(f.t, filepath.Join(pbase, "dev"), "8:101\n")
	// udev's partuuid farm. Without it a serial-less partition is only weakly
	// identified and every destructive operation refuses it, so the case this
	// fixture exists to reach would never be entered.
	farm := filepath.Join(f.dev, "disk", "by-partuuid")
	if err := os.MkdirAll(farm, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink("../../"+f.partName,
		filepath.Join(farm, "11111111-2222-3333-4444-555555555555")); err != nil {
		f.t.Fatal(err)
	}
}

// fsDevName is the kernel name of the device the test filesystem lives on.
func (f *sanitizeFixture) fsDevName() string {
	if f.partName != "" {
		return f.partName
	}
	return f.devName
}

func (f *sanitizeFixture) writeMountinfo(devName, fstype string) {
	f.t.Helper()
	var b strings.Builder
	if f.alsoAtSlash {
		fmt.Fprintf(&b, "21 1 8:100 / / rw,relatime shared:1 - %s /dev/%s rw\n", fstype, devName)
	}
	fmt.Fprintf(&b, "36 21 8:100 / %s rw,relatime shared:2 - %s /dev/%s rw\n", f.root, fstype, devName)
	write(f.t, filepath.Join(f.proc, "self", "mountinfo"), b.String())
}

// installStatfs presents a filesystem of a chosen size on an ordinary temp
// directory. Free space shrinks by exactly the bytes the sanitizer's own
// artifacts occupy, so the reserve, the stopping condition and the reclaim
// check are all exercised for real.
func (f *sanitizeFixture) installStatfs() {
	f.t.Helper()
	prev := sanitizeStatfs
	sanitizeStatfs = func(path string) (sanitizeFSStat, error) {
		used := f.artifactBytes()
		avail := f.avail
		if used >= avail {
			avail = 0
		} else {
			avail -= used
		}
		return sanitizeFSStat{TotalBytes: f.total, AvailBytes: avail, FreeBytes: avail, BlockSize: 4096}, nil
	}
	f.t.Cleanup(func() { sanitizeStatfs = prev })
}

func (f *sanitizeFixture) artifactBytes() uint64 {
	var total uint64
	entries, err := os.ReadDir(f.root)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), sanitizeArtifactPrefix) {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(f.root, e.Name()))
		if err != nil {
			continue
		}
		for _, i := range inner {
			if fi, err := i.Info(); err == nil {
				total += uint64(fi.Size())
			}
		}
	}
	return total
}

// installTools presents a host carrying exactly the named helpers.
func (f *sanitizeFixture) installTools(paths map[Tool]string) {
	f.t.Helper()
	SetLookPath(func(name string) (string, error) {
		if p, ok := paths[Tool(name)]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	})
	f.t.Cleanup(func() { SetLookPath(exec.LookPath) })
	DiscoverCapabilities(context.Background())
}

func (f *sanitizeFixture) plan(method SanitizeMethod, mutate func(*SanitizeRequest)) (*SanitizePlan, error) {
	f.t.Helper()
	req := SanitizeRequest{Root: f.root, Method: method, PrincipalID: "test"}
	req.Options.ChunkBytes = 1 << 20
	req.Options.ProgressInterval = time.Millisecond
	if mutate != nil {
		mutate(&req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return PlanSanitize(ctx, req)
}

func (f *sanitizeFixture) mustPlan(method SanitizeMethod, mutate func(*SanitizeRequest)) *SanitizePlan {
	f.t.Helper()
	p, err := f.plan(method, mutate)
	if err != nil {
		f.t.Fatalf("PlanSanitize(%s): %v", method, err)
	}
	return p
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type knownFile struct {
	path  string
	body  []byte
	mtime time.Time
}

// seedKnownFiles puts allocated data on the filesystem, with content and mtime
// recorded, so "the sanitizer did not touch the user's files" is a measurement
// and not a hope.
func seedKnownFiles(t *testing.T, root string) []knownFile {
	t.Helper()
	var out []knownFile
	for i := 0; i < 6; i++ {
		p := filepath.Join(root, fmt.Sprintf("user-data-%d.bin", i))
		body := []byte(strings.Repeat(fmt.Sprintf("payload-%d;", i), 4096))
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC).Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		out = append(out, knownFile{path: p, body: body, mtime: mt})
	}
	// A nested directory too: nothing about the writer may recurse into user
	// data, and an empty assertion set would not notice if it did.
	sub := filepath.Join(root, "photos", "2019")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "holiday.jpg")
	body := []byte(strings.Repeat("JPEGDATA", 2048))
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2019, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return append(out, knownFile{path: p, body: body, mtime: mt})
}

func assertKnownFilesIntact(t *testing.T, files []knownFile) {
	t.Helper()
	for _, kf := range files {
		got, err := os.ReadFile(kf.path)
		if err != nil {
			t.Fatalf("allocated file %s disappeared: %v", kf.path, err)
		}
		if string(got) != string(kf.body) {
			t.Fatalf("allocated file %s was modified by free-space sanitization", kf.path)
		}
		fi, err := os.Stat(kf.path)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(kf.mtime) {
			t.Fatalf("allocated file %s had its mtime changed: want %s got %s",
				kf.path, kf.mtime, fi.ModTime())
		}
	}
}

func assertNoArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), sanitizeArtifactPrefix) {
			t.Fatalf("sanitization artifact %s was left behind", e.Name())
		}
	}
}

func refusalCode(t *testing.T, err error) SanitizeRefusalCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	code, ok := SanitizeRefusalOf(err)
	if !ok {
		t.Fatalf("expected a structured refusal, got %T: %v", err, err)
	}
	return code
}

// ---------------------------------------------------------------- protection

func TestSanitizeRefusesAProtectedFilesystem(t *testing.T) {
	// Everything else about this fixture is valid — authorized root, a
	// characterized filesystem, a strongly identified rotational disk — so the
	// only thing that can refuse is the protection classifier. Without that
	// isolation the test would pass even if the guard were deleted.
	f := newSanitizeFixture(t, alsoMountedAtRoot())

	_, err := f.plan(SanitizeZeroFill, nil)
	if got := refusalCode(t, err); got != RefuseDeviceProtected {
		t.Fatalf("want %s, got %s (%v)", RefuseDeviceProtected, got, err)
	}
	var ref *SanitizeRefusal
	if !errors.As(err, &ref) {
		t.Fatal("expected *SanitizeRefusal")
	}
	found := false
	for _, r := range ref.DeviceReasons {
		if r == blockdev.ReasonRootFilesystem {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal does not name the root filesystem: %v", ref.DeviceReasons)
	}
}

func TestSanitizeRefusesAPathOutsideAuthorizedStorage(t *testing.T) {
	f := newSanitizeFixture(t)
	outside := filepath.Join(f.base, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// Present it as a mount point of the same eligible device, so only the
	// storage authority can refuse it.
	write(t, filepath.Join(f.proc, "self", "mountinfo"), fmt.Sprintf(
		"36 21 8:100 / %s rw,relatime - ext4 /dev/%s rw\n"+
			"37 21 8:100 / %s rw,relatime - ext4 /dev/%s rw\n", f.root, f.devName, outside, f.devName))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := PlanSanitize(ctx, SanitizeRequest{Root: outside, Method: SanitizeZeroFill})
	if got := refusalCode(t, err); got != RefuseUnauthorizedPath {
		t.Fatalf("want %s, got %s (%v)", RefuseUnauthorizedPath, got, err)
	}
}

func TestSanitizeRefusesAnUncharacterizedFilesystem(t *testing.T) {
	// btrfs keeps deleted data referenced by snapshots, so overwriting free
	// space there would report success while the data is still present. A
	// filesystem this package has not characterized is refused, not guessed at.
	f := newSanitizeFixture(t, withFSType("btrfs"))
	_, err := f.plan(SanitizeZeroFill, nil)
	if got := refusalCode(t, err); got != RefuseFSNotCharacterized {
		t.Fatalf("want %s, got %s (%v)", RefuseFSNotCharacterized, got, err)
	}
}

func TestSanitizeRefusesATargetThatIsNotAMountPoint(t *testing.T) {
	f := newSanitizeFixture(t)
	sub := filepath.Join(f.root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := PlanSanitize(ctx, SanitizeRequest{Root: sub, Method: SanitizeZeroFill})
	if got := refusalCode(t, err); got != RefuseNotAMountPoint {
		t.Fatalf("want %s, got %s (%v)", RefuseNotAMountPoint, got, err)
	}
}

func TestSanitizeRefusesWhenTheMountTableCannotBeRead(t *testing.T) {
	// FAIL-CLOSED: not knowing which filesystem serves a path is a refusal.
	f := newSanitizeFixture(t)
	if err := os.Remove(filepath.Join(f.proc, "self", "mountinfo")); err != nil {
		t.Fatal(err)
	}
	_, err := f.plan(SanitizeZeroFill, nil)
	if got := refusalCode(t, err); got != RefuseMountTableUnreadable {
		t.Fatalf("want %s, got %s (%v)", RefuseMountTableUnreadable, got, err)
	}
}

// ---------------------------------------------------------------- semantics

func TestMultiPassIsNeverOfferedForSolidState(t *testing.T) {
	ssd := newSanitizeFixture(t, solidState())
	_, err := ssd.plan(SanitizeMultiPass, func(r *SanitizeRequest) { r.Passes = 3 })
	if got := refusalCode(t, err); got != RefuseMultiPassOnSolidState {
		t.Fatalf("want %s, got %s (%v)", RefuseMultiPassOnSolidState, got, err)
	}
	// Positive control: the identical request on rotational media is accepted,
	// so the refusal above is about the media and not about multi-pass being
	// broken in general.
	hdd := newSanitizeFixture(t)
	p := hdd.mustPlan(SanitizeMultiPass, func(r *SanitizeRequest) { r.Passes = 3 })
	if len(p.Passes) != 3 {
		t.Fatalf("want 3 passes, got %d", len(p.Passes))
	}
	if !p.Semantics.RotationalOnly {
		t.Fatal("multi-pass semantics must be marked rotational-only")
	}
}

func TestZeroFillOnSolidStateIsNeverAutomatic(t *testing.T) {
	f := newSanitizeFixture(t, solidState())

	_, err := f.plan(SanitizeZeroFill, nil)
	if got := refusalCode(t, err); got != RefuseOverwriteNotAcked {
		t.Fatalf("want %s, got %s (%v)", RefuseOverwriteNotAcked, got, err)
	}

	p := f.mustPlan(SanitizeZeroFill, func(r *SanitizeRequest) { r.AcknowledgeSolidStateWriteCost = true })
	joined := strings.ToLower(strings.Join(p.Warnings, " "))
	if !strings.Contains(joined, "endurance") {
		t.Fatalf("an acknowledged solid-state overwrite must still warn about the write cost: %v", p.Warnings)
	}
	if !strings.Contains(strings.ToLower(p.Semantics.WriteCostNote), "not stronger") {
		t.Fatalf("zero-fill must not be presented as stronger than a device sanitize: %q", p.Semantics.WriteCostNote)
	}
}

func TestRecommendationNeverPicksAnOverwriteForSolidState(t *testing.T) {
	caps := Capabilities{Tools: []ToolStatus{{Tool: ToolFstrim, Available: true}}}

	ssd := blockdev.Device{Name: "nvme0n1", Rotational: false, SupportsDiscard: true}
	if m, why := RecommendSanitizeMethod(ssd, caps); m != SanitizeTrim {
		t.Fatalf("want trim for solid-state, got %q (%s)", m, why)
	}
	noDiscard := blockdev.Device{Name: "sdb", Rotational: false, SupportsDiscard: false}
	if m, _ := RecommendSanitizeMethod(noDiscard, caps); m == SanitizeZeroFill || m == SanitizeMultiPass {
		t.Fatalf("an overwrite must never be recommended automatically for solid-state media, got %q", m)
	}
	hdd := blockdev.Device{Name: "sda", Rotational: true}
	if m, _ := RecommendSanitizeMethod(hdd, caps); m != SanitizeZeroFill {
		t.Fatalf("want zero_fill for rotational media, got %q", m)
	}
}

func TestTrimIsNeverDescribedAsSecureErase(t *testing.T) {
	trim := sanitizeSemanticsFor(SanitizeTrim)
	if trim.HostOverwrites {
		t.Fatal("TRIM writes nothing; describing it as an overwrite is a false guarantee")
	}
	if trim.DeterminateProgress {
		t.Fatal("neither the kernel nor the device reports discard progress; an ETA would be invented")
	}
	if !strings.Contains(strings.ToLower(strings.Join(trim.DoesNotReach, " ")), "not a cryptographic erase") {
		t.Fatalf("TRIM must state plainly that it is not a cryptographic erase: %v", trim.DoesNotReach)
	}
	for _, s := range SanitizeMethodCatalog() {
		if s.CryptographicErase {
			t.Fatalf("method %s claims a cryptographic erase this package does not perform", s.Method)
		}
		if s.DeviceLevelPrimitive && s.Implemented {
			t.Fatalf("method %s claims to use a device primitive it does not use", s.Method)
		}
	}
}

func TestDeviceSanitizeIsRefusedRatherThanSubstituted(t *testing.T) {
	// The defect this prevents: "secure erase" quietly becoming a TRIM.
	f := newSanitizeFixture(t)
	_, err := f.plan(SanitizeDeviceErase, nil)
	if got := refusalCode(t, err); got != RefuseDeviceEraseNotHere {
		t.Fatalf("want %s, got %s (%v)", RefuseDeviceEraseNotHere, got, err)
	}
}

func TestMultiPassSequencesAreExplicitAndEndClean(t *testing.T) {
	for n := MinSanitizePasses; n <= MaxSanitizePasses; n++ {
		seq, err := sanitizePassSequence(SanitizeMultiPass, n)
		if err != nil {
			t.Fatalf("%d passes: %v", n, err)
		}
		if len(seq) != n {
			t.Fatalf("%d passes produced %d specs", n, len(seq))
		}
		for i, s := range seq {
			if s.Index != i+1 {
				t.Fatalf("pass %d is numbered %d", i+1, s.Index)
			}
			if strings.TrimSpace(s.Label) == "" {
				t.Fatalf("pass %d of the %d-pass sequence has no label; an unnamed pass is exactly "+
					"the 'DoD wipe' vagueness this table replaces", i+1, n)
			}
		}
		if last := seq[len(seq)-1]; last.Kind != passZero {
			t.Fatalf("the %d-pass sequence ends on %q instead of a zero clean pass", n, last.Label)
		}
	}
	if _, err := sanitizePassSequence(SanitizeMultiPass, 1); err == nil {
		t.Fatal("a single pass is the zero-fill method and must not be reachable as multi-pass")
	}
	if _, err := sanitizePassSequence(SanitizeMultiPass, MaxSanitizePasses+1); err == nil {
		t.Fatal("pass counts beyond the table must be refused")
	}
}

// ---------------------------------------------------------------- the reserve

func TestTheSafetyReserveIsNeverZeroAndNeverLowerable(t *testing.T) {
	for _, total := range []uint64{1 << 20, 100 << 20, 1 << 30, 1 << 40, 1 << 50} {
		r := sanitizeReserveBytes(total, 0)
		if r == 0 {
			t.Fatalf("a %s filesystem got a zero reserve: the writer could fill it to 100%%", humanBytes(total))
		}
		if r < minSanitizeReserveBytes && total >= minSanitizeReserveBytes {
			t.Fatalf("a %s filesystem got a %s reserve, below the floor", humanBytes(total), humanBytes(r))
		}
		// An option may RAISE the reserve; it must never lower it.
		if lowered := sanitizeReserveBytes(total, 1); lowered < r {
			t.Fatalf("an override lowered the reserve from %s to %s", humanBytes(r), humanBytes(lowered))
		}
		// Raising only has room where the reserve is not already the whole
		// filesystem — the reserve is clamped to the volume it protects.
		if r < total {
			if raised := sanitizeReserveBytes(total, r+(1<<20)); raised <= r {
				t.Fatalf("an override could not raise the %s reserve on a %s filesystem",
					humanBytes(r), humanBytes(total))
			}
		}
	}
	if got := sanitizeTargetBytes(100, 200); got != 0 {
		t.Fatalf("with less available than the reserve the target must be zero, got %d", got)
	}
}

func TestSanitizeSelfTestProvesTheSafetyProperties(t *testing.T) {
	if err := sanitizeSelfTest(); err != nil {
		t.Fatalf("the production tables do not satisfy the safety invariants: %v", err)
	}
}

func TestZeroFillNeverConsumesTheSafetyReserve(t *testing.T) {
	f := newSanitizeFixture(t)
	seedKnownFiles(t, f.root)

	plan := f.mustPlan(SanitizeZeroFill, nil)
	if plan.ReserveBytes != testExpectedReserve {
		t.Fatalf("reserve: want %s got %s", humanBytes(testExpectedReserve), humanBytes(plan.ReserveBytes))
	}
	if plan.TargetBytesPerPass != testExpectedTarget {
		t.Fatalf("target: want %s got %s", humanBytes(testExpectedTarget), humanBytes(plan.TargetBytesPerPass))
	}

	var lowWater uint64 = ^uint64(0)
	plan.Options.Progress = func(p SanitizeProgress) {
		if p.FreeBytesNow < lowWater {
			lowWater = p.FreeBytesNow
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}

	// THE INVARIANT: the filesystem never came closer to full than the reserve.
	if lowWater < plan.ReserveBytes {
		t.Fatalf("free space fell to %s, below the %s reserve",
			humanBytes(lowWater), humanBytes(plan.ReserveBytes))
	}
	if res.Final.BytesWrittenTotal > testExpectedTarget {
		t.Fatalf("wrote %s, more than the %s budget above the reserve",
			humanBytes(res.Final.BytesWrittenTotal), humanBytes(testExpectedTarget))
	}
	if res.Final.BytesWrittenTotal == 0 {
		t.Fatal("nothing was written; the test would pass even with the writer removed")
	}
	if !res.StoppedAtReserve {
		t.Fatal("the pass should have ended by reaching its own safety limit")
	}
}

func TestTheWriterStopsWhenAnotherProcessConsumesTheHeadroom(t *testing.T) {
	// This isolates the RUNTIME bound from the planned budget. On a quiet
	// filesystem the budget alone keeps the writer inside the reserve, so a
	// test that only checks the budget would still pass with the per-chunk
	// free-space re-check deleted. Here another process takes 20 MiB right
	// after the plan was made: the budget is now wrong by exactly that much,
	// and only re-reading free space every chunk can stop in time.
	const neighbourTakes = 20 << 20
	f := newSanitizeFixture(t)
	prev := sanitizeStatfs
	sanitizeStatfs = func(path string) (sanitizeFSStat, error) {
		mine := f.artifactBytes()
		consumed := mine
		if mine > 0 {
			// The neighbour appears once the run has started, i.e. after the
			// plan's budget was computed.
			consumed += neighbourTakes
		}
		avail := f.avail
		if consumed >= avail {
			avail = 0
		} else {
			avail -= consumed
		}
		return sanitizeFSStat{TotalBytes: f.total, AvailBytes: avail, FreeBytes: avail, BlockSize: 4096}, nil
	}
	t.Cleanup(func() { sanitizeStatfs = prev })

	plan := f.mustPlan(SanitizeZeroFill, nil)
	if plan.TargetBytesPerPass != testExpectedTarget {
		t.Fatalf("the plan budget must still be the quiet-filesystem figure, got %s",
			humanBytes(plan.TargetBytesPerPass))
	}

	var lowWater uint64 = ^uint64(0)
	plan.Options.Progress = func(p SanitizeProgress) {
		if p.FreeBytesNow < lowWater {
			lowWater = p.FreeBytesNow
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if lowWater < plan.ReserveBytes {
		t.Fatalf("free space fell to %s under concurrent load, below the %s reserve",
			humanBytes(lowWater), humanBytes(plan.ReserveBytes))
	}
	if res.Final.BytesWrittenTotal >= testExpectedTarget {
		t.Fatalf("the writer spent its whole %s budget while a neighbour was consuming the same "+
			"space; it must stop at the reserve, not at the budget",
			humanBytes(testExpectedTarget))
	}
	if !res.StoppedAtReserve {
		t.Fatal("the run must record that it stopped at its own safety limit")
	}
	if res.Final.BytesWrittenTotal == 0 {
		t.Fatal("nothing was written; the assertion above would be vacuous")
	}
}

func TestTheWriteBufferCannotBeSizedToHoldTheFreeSpace(t *testing.T) {
	// The allocation choke point itself, not just option validation: any code
	// path that tries to hold a filesystem's worth of free space in memory is
	// refused where the memory would be taken.
	if _, err := newSanitizeBuffer(1 << 30); err == nil {
		t.Fatal("a 1 GiB write buffer was allocated; the writer must stream")
	}
	if _, err := newSanitizeBuffer(maxSanitizeChunkBytes + 1); err == nil {
		t.Fatalf("a buffer above the %s ceiling was allocated", humanBytes(maxSanitizeChunkBytes))
	}
	b, err := newSanitizeBuffer(defaultSanitizeChunkBytes)
	if err != nil || uint64(len(b)) != defaultSanitizeChunkBytes {
		t.Fatalf("the default chunk must be allocatable: %v", err)
	}
}

// ---------------------------------------------------------------- the writer

func TestZeroFillLeavesAllocatedFilesIntactAndCleansUp(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	plan := f.mustPlan(SanitizeZeroFill, nil)
	before, err := sanitizeStatfs(f.root)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if res.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s", res.Outcome, res.Detail)
	}

	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, f.root)
	if !res.ArtifactsRemoved {
		t.Fatal("the result does not report the workspace as removed")
	}
	if !res.SpaceReclaimed {
		t.Fatalf("free space was not reclaimed: %s -> %s",
			humanBytes(res.FreeBytesAtStart), humanBytes(res.FreeBytesAtEnd))
	}
	after, err := sanitizeStatfs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailBytes != before.AvailBytes {
		t.Fatalf("free space did not return: %s -> %s",
			humanBytes(before.AvailBytes), humanBytes(after.AvailBytes))
	}
	if len(res.Unexplained) != 0 {
		t.Fatalf("a clean run reported unexplained artifacts: %v", res.Unexplained)
	}
	if res.Final.PartsCreated == 0 || res.Final.PassesCompleted != 1 {
		t.Fatalf("progress is not populated: %+v", res.Final)
	}
	if res.Final.ThroughputBytesPerSec <= 0 || res.Final.Elapsed <= 0 {
		t.Fatalf("throughput and elapsed time must be reported: %+v", res.Final)
	}
	if !res.Final.Determinate {
		t.Fatal("an overwrite has byte-accurate progress and must be reported as determinate")
	}
}

func TestMultiPassRunsEveryPassAndFreesSpaceBetweenThem(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	plan := f.mustPlan(SanitizeMultiPass, func(r *SanitizeRequest) { r.Passes = 3 })
	if plan.TotalWriteBytes != plan.TargetBytesPerPass*3 {
		t.Fatalf("the plan must state the full write volume: %s", humanBytes(plan.TotalWriteBytes))
	}
	if plan.CapacityWrites <= 0 {
		t.Fatal("the plan must state how many capacity-writes multi-pass implies")
	}

	var seenLabels []string
	var peak uint64
	plan.Options.Progress = func(p SanitizeProgress) {
		if p.CurrentPassLabel != "" && (len(seenLabels) == 0 || seenLabels[len(seenLabels)-1] != p.CurrentPassLabel) {
			seenLabels = append(seenLabels, p.CurrentPassLabel)
		}
		if p.BytesWrittenThisPass > peak {
			peak = p.BytesWrittenThisPass
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if res.Final.PassesCompleted != 3 {
		t.Fatalf("completed %d of 3 passes", res.Final.PassesCompleted)
	}
	// Roughly three times one pass: the space must have been released between
	// passes, or the later passes would have had nothing to write into.
	if res.Final.BytesWrittenTotal < 2*testExpectedTarget {
		t.Fatalf("only %s written across three passes; the space was not freed between them",
			humanBytes(res.Final.BytesWrittenTotal))
	}
	if len(seenLabels) < 2 {
		t.Fatalf("progress never named more than one pass: %v", seenLabels)
	}
	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, f.root)
}

func TestArtifactsRespectThePerFileLimit(t *testing.T) {
	// FAT32 cannot hold a 4 GiB file, so free space must be consumed with
	// several artifacts. The clamp is unit-checked at the real limit, and the
	// multi-artifact path is exercised for real at a size a test can reach.
	fat := sanitizeFSProfiles[FSFAT32]
	if got := sanitizePartSizeFor(fat, 8<<30); got != fat.MaxFileBytes {
		t.Fatalf("a FAT32 artifact was allowed to reach %s (limit %s)",
			humanBytes(got), humanBytes(fat.MaxFileBytes))
	}
	if fat.MaxFileBytes >= 4<<30 {
		t.Fatalf("the FAT32 per-file limit is wrong: %d", fat.MaxFileBytes)
	}
	opts, err := SanitizeOptions{MaxPartBytes: 8 << 30, ChunkBytes: 1 << 20}.normalized(fat)
	if err != nil {
		t.Fatal(err)
	}
	if opts.MaxPartBytes > fat.MaxFileBytes {
		t.Fatalf("normalization left MaxPartBytes at %s on FAT32", humanBytes(opts.MaxPartBytes))
	}
	ext4 := sanitizeFSProfiles[FSExt4]
	if got := sanitizePartSizeFor(ext4, 8<<30); got != 8<<30 {
		t.Fatalf("ext4 has no 4 GiB per-file limit but the size was clamped to %s", humanBytes(got))
	}

	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, func(r *SanitizeRequest) { r.Options.MaxPartBytes = 4 << 20 })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if res.Final.PartsCreated < 4 {
		t.Fatalf("a %s budget with %s artifacts produced only %d files",
			humanBytes(plan.TargetBytesPerPass), humanBytes(plan.MaxPartBytes), res.Final.PartsCreated)
	}
}

func TestTheWriterStreamsAndDoesNotBufferFreeSpaceInMemory(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeMultiPass, func(r *SanitizeRequest) {
		r.Passes = 2
		r.Options.ChunkBytes = 1 << 20
	})

	before := sanitizeBufferBytes.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	allocated := sanitizeBufferBytes.Load() - before

	if res.Final.BytesWrittenTotal < testExpectedTarget {
		t.Fatalf("the run wrote only %s; the memory check would be vacuous",
			humanBytes(res.Final.BytesWrittenTotal))
	}
	// One buffer for the whole run, whatever the volume of data.
	if allocated > 2*plan.Options.ChunkBytes {
		t.Fatalf("the writer allocated %s of buffer to write %s; it must stream",
			humanBytes(allocated), humanBytes(res.Final.BytesWrittenTotal))
	}
	if _, err := (SanitizeOptions{ChunkBytes: 1 << 30}).normalized(sanitizeFSProfiles[FSExt4]); err == nil {
		t.Fatal("an unbounded write buffer must be rejected by option validation")
	}
}

func TestENOSPCIsATerminalConditionNotAFailure(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	// The reserve normally stops the writer first. This injects the case where
	// another writer consumed the headroom concurrently and the filesystem
	// reports full before the reserve is reached.
	var written atomic.Uint64
	prev := sanitizeWriteChunk
	sanitizeWriteChunk = func(fh *os.File, b []byte) (int, error) {
		if written.Load() >= 8<<20 {
			return 0, &os.PathError{Op: "write", Path: fh.Name(), Err: unix.ENOSPC}
		}
		n, err := fh.Write(b)
		written.Add(uint64(n))
		return n, err
	}
	t.Cleanup(func() { sanitizeWriteChunk = prev })

	plan := f.mustPlan(SanitizeZeroFill, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("ENOSPC where the algorithm expects it must not be an error: %v", err)
	}
	if !res.HitENOSPC {
		t.Fatal("the result must record that the filesystem reported full")
	}
	if res.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s", res.Outcome)
	}
	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, f.root)
}

func TestPacingHoldsTheConfiguredThroughputCeiling(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, func(r *SanitizeRequest) {
		r.Options.MaxBytesPerSecond = 64 << 20
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	elapsed := time.Since(start)
	// 32 MiB at 64 MiB/s cannot legitimately finish in under a quarter second.
	if elapsed < 250*time.Millisecond {
		t.Fatalf("wrote %s in %s despite a %s/s ceiling; pacing is not applied",
			humanBytes(res.Final.BytesWrittenTotal), elapsed, humanBytes(64<<20))
	}
}

// ---------------------------------------------------------------- cancellation

func TestCancellationCleansUpItsArtifacts(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	plan := f.mustPlan(SanitizeMultiPass, func(r *SanitizeRequest) { r.Passes = 3 })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var sawArtifacts atomic.Bool
	plan.Options.Progress = func(p SanitizeProgress) {
		if p.BytesWrittenTotal > 2<<20 {
			// Prove there was something to clean up at the moment of
			// cancellation, or the assertion below would be vacuous.
			if entries, err := os.ReadDir(f.root); err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), sanitizeArtifactPrefix) {
						sawArtifacts.Store(true)
					}
				}
			}
			cancel()
		}
	}

	res, err := RunSanitize(ctx, plan.JobID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if res.Outcome != OutcomeCancelled {
		t.Fatalf("outcome %s", res.Outcome)
	}
	if !sawArtifacts.Load() {
		t.Fatal("no artifacts existed when the run was cancelled; the cleanup assertion would prove nothing")
	}
	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, f.root)
	if !res.ArtifactsRemoved {
		t.Fatal("a cancelled run must report its workspace removed")
	}
	if !res.SpaceReclaimed {
		t.Fatal("a cancelled run must leave the free space back where it found it")
	}
}

// ---------------------------------------------------------------- revalidation

func TestRunAbortsWhenTheBackingDeviceChangesAfterPlanning(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)

	// A replug that reused the device node: same name, same size, different
	// hardware. Binding to the kernel name would sail straight through this.
	f.serial = "SOMEOTHERDISK9999"
	f.writeSysfs()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if got := refusalCode(t, err); got != RefusePlanChanged {
		t.Fatalf("want %s, got %s (%v)", RefusePlanChanged, got, err)
	}
	if res.Outcome != OutcomeAborted {
		t.Fatalf("outcome %s", res.Outcome)
	}
	assertNoArtifacts(t, f.root)
}

func TestRunAbortsWhenTheFilesystemMovesToAnotherDevice(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)
	f.writeMountinfo("sdy", "ext4")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := RunSanitize(ctx, plan.JobID)
	if got := refusalCode(t, err); got != RefusePlanChanged {
		t.Fatalf("want %s, got %s (%v)", RefusePlanChanged, got, err)
	}
	assertNoArtifacts(t, f.root)
}

func TestRunAbortsAndCleansUpWhenTheTargetChangesBetweenPasses(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)
	plan := f.mustPlan(SanitizeMultiPass, func(r *SanitizeRequest) { r.Passes = 3 })

	var flipped atomic.Bool
	plan.Options.Progress = func(p SanitizeProgress) {
		if p.BytesWrittenTotal > 2<<20 && flipped.CompareAndSwap(false, true) {
			// The disk is remounted read-only mid-operation.
			write(t, filepath.Join(f.proc, "self", "mountinfo"), fmt.Sprintf(
				"36 21 8:100 / %s ro,relatime - ext4 /dev/%s ro\n", f.root, f.devName))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if got := refusalCode(t, err); got != RefusePlanChanged {
		t.Fatalf("want %s, got %s (%v)", RefusePlanChanged, got, err)
	}
	if res.Outcome != OutcomeAborted {
		t.Fatalf("outcome %s", res.Outcome)
	}
	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, f.root)
}

// ---------------------------------------------------------------- provenance

func TestCleanupRefusesArtifactsItCannotProveAreOurs(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	type lookalike struct {
		dir      string
		manifest string
		victim   string
	}
	cases := []lookalike{
		{ // no manifest at all
			dir: sanitizeArtifactPrefix + "0123456789abcdef", manifest: "",
			victim: "important-user-file.bin"},
		{ // a manifest, but not this subsystem's
			dir:      sanitizeArtifactPrefix + "fedcba9876543210",
			manifest: `{"magic":"some.other.tool","version":1,"job_id":"fedcba9876543210"}`,
			victim:   "another-users-data.bin"},
		{ // our magic, but it claims a different filesystem root
			dir: sanitizeArtifactPrefix + "aaaabbbbccccdddd",
			manifest: fmt.Sprintf(`{"magic":%q,"version":1,"job_id":"aaaabbbbccccdddd","root":"/somewhere/else"}`,
				sanitizeArtifactMagic),
			victim: "restored-from-backup.bin"},
		{ // our magic and our root, but the job id contradicts the directory name
			dir: sanitizeArtifactPrefix + "1111222233334444",
			manifest: fmt.Sprintf(`{"magic":%q,"version":1,"job_id":"9999888877776666","root":%q}`,
				sanitizeArtifactMagic, f.root),
			victim: "someone-elses-job.bin"},
		{ // no manifest, and the content is named exactly like our own artifacts.
			// This case isolates the DIRECTORY-level proof: the per-entry name
			// check cannot save this file, so only the manifest requirement
			// stops it being deleted.
			dir: sanitizeArtifactPrefix + "5555666677778888", manifest: "",
			victim: sanitizePartName("5555666677778888", 1)},
	}

	for _, c := range cases {
		dir := filepath.Join(f.root, c.dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, c.victim), "irreplaceable")
		if c.manifest != "" {
			write(t, filepath.Join(dir, sanitizeManifestName), c.manifest)
		}
	}

	rep, err := RecoverSanitizeAtRoot(f.root)
	if err != nil {
		t.Fatalf("RecoverSanitizeAtRoot: %v", err)
	}

	for _, c := range cases {
		dir := filepath.Join(f.root, c.dir)
		if _, err := os.Stat(filepath.Join(dir, c.victim)); err != nil {
			t.Fatalf("%s was deleted although its provenance could not be proven: %v",
				filepath.Join(c.dir, c.victim), err)
		}
		found := false
		for _, u := range rep.Unexplained {
			if strings.HasPrefix(u, dir) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was neither cleaned nor reported; a crash artifact must never be silently ignored",
				c.dir)
		}
	}
	assertKnownFilesIntact(t, files)
}

func TestCleanupRemovesArtifactsItCanProve(t *testing.T) {
	// Positive control for the test above: with a valid manifest the very same
	// code path DOES remove the workspace, so the refusals there are about
	// provenance and not about cleanup being broken.
	f := newSanitizeFixture(t)
	jobID := "0123456789abcdef0123456789abcdef"
	dir := filepath.Join(f.root, sanitizeArtifactDirName(jobID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, sanitizeManifestName), fmt.Sprintf(
		`{"magic":%q,"version":%d,"job_id":%q,"root":%q,"method":"zero_fill"}`,
		sanitizeArtifactMagic, sanitizeManifestVersion, jobID, f.root))
	write(t, filepath.Join(dir, sanitizePartName(jobID, 1)), strings.Repeat("x", 4096))
	write(t, filepath.Join(dir, sanitizePartName(jobID, 2)), strings.Repeat("x", 4096))

	rep, err := RecoverSanitizeAtRoot(f.root)
	if err != nil {
		t.Fatalf("RecoverSanitizeAtRoot: %v", err)
	}
	if rep.ArtifactsRemove != 3 {
		t.Fatalf("removed %d entries, want 3", rep.ArtifactsRemove)
	}
	if len(rep.Unexplained) != 0 {
		t.Fatalf("a provable workspace was reported as unexplained: %v", rep.Unexplained)
	}
	assertNoArtifacts(t, f.root)
}

func TestCleanupLeavesAForeignFileInsideOurOwnWorkspace(t *testing.T) {
	// Our manifest proves the DIRECTORY is ours. It does not prove that every
	// file inside it is, so anything this writer would not have created stays.
	f := newSanitizeFixture(t)
	jobID := "abcdefabcdefabcdefabcdefabcdefab"
	dir := filepath.Join(f.root, sanitizeArtifactDirName(jobID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, sanitizeManifestName), fmt.Sprintf(
		`{"magic":%q,"version":%d,"job_id":%q,"root":%q}`,
		sanitizeArtifactMagic, sanitizeManifestVersion, jobID, f.root))
	write(t, filepath.Join(dir, sanitizePartName(jobID, 1)), strings.Repeat("x", 4096))
	foreign := filepath.Join(dir, "not-ours.txt")
	write(t, foreign, "someone put this here")

	rep, err := RecoverSanitizeAtRoot(f.root)
	if err != nil {
		t.Fatalf("RecoverSanitizeAtRoot: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("a file this writer never creates was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sanitizePartName(jobID, 1))); !os.IsNotExist(err) {
		t.Fatal("our own artifact should still have been removed")
	}
	found := false
	for _, u := range rep.Unexplained {
		if u == foreign {
			found = true
		}
	}
	if !found {
		t.Fatalf("the foreign file was not reported: %v", rep.Unexplained)
	}
}

// ---------------------------------------------------------------- recovery

func TestCrashRecoveryReclaimsSpaceAndMarksTheJobInterrupted(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)

	// Exactly what a power cut leaves: a persisted record still in "running",
	// a manifest, and artifacts occupying the disk.
	jobID := "beefbeefbeefbeefbeefbeefbeefbeef"
	rec := SanitizeJobRecord{
		JobID: jobID, State: JobRunning, Method: SanitizeZeroFill, Root: f.root,
		Filesystem: FSExt4, DeviceName: f.devName, DeviceFingerprint: "serial:testserial0001",
		ArtifactDir: filepath.Join(f.root, sanitizeArtifactDirName(jobID)),
		BootID:      "an-older-boot", PID: 12345, PassesPlanned: 1,
		FreeBytesAtStart: f.avail, ReserveBytes: testExpectedReserve,
		StartedAt: time.Now().Add(-time.Hour),
	}
	if err := writeSanitizeJobRecord(rec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, sanitizeArtifactDirName(jobID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, sanitizeManifestName), fmt.Sprintf(
		`{"magic":%q,"version":%d,"job_id":%q,"root":%q,"method":"zero_fill"}`,
		sanitizeArtifactMagic, sanitizeManifestVersion, jobID, f.root))
	for i := 1; i <= 3; i++ {
		write(t, filepath.Join(dir, sanitizePartName(jobID, i)), strings.Repeat("0", 8<<20))
	}

	occupied, err := sanitizeStatfs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if occupied.AvailBytes >= f.avail {
		t.Fatal("the simulated filesystem does not reflect the leftover artifacts")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := EnsureSanitizeSubsystem(ctx)

	if h.InterruptedRecovered != 1 {
		t.Fatalf("recovered %d interrupted jobs, want 1", h.InterruptedRecovered)
	}
	if h.BytesReclaimed == 0 {
		t.Fatal("recovery did not report the space it reclaimed")
	}
	assertNoArtifacts(t, f.root)
	assertKnownFilesIntact(t, files)

	after, err := sanitizeStatfs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailBytes != f.avail {
		t.Fatalf("a crash left the disk looking full: %s available, want %s",
			humanBytes(after.AvailBytes), humanBytes(f.avail))
	}

	jobs, err := ReadSanitizeJobs()
	if err != nil {
		t.Fatal(err)
	}
	var got *SanitizeJobRecord
	for i := range jobs {
		if jobs[i].JobID == jobID {
			got = &jobs[i]
		}
	}
	if got == nil {
		t.Fatal("the job record disappeared")
	}
	if got.State != JobInterrupted {
		t.Fatalf("job state %q, want %q", got.State, JobInterrupted)
	}
	if got.FinishedAt == nil || got.Detail == "" {
		t.Fatal("an interrupted job must record when and how it ended")
	}
}

// seedInterruptedJob leaves exactly what a power cut leaves: a record still in
// "running", a manifest, and artifacts occupying the disk.
func seedInterruptedJob(t *testing.T, f *sanitizeFixture, jobID string) {
	t.Helper()
	rec := SanitizeJobRecord{
		JobID: jobID, State: JobRunning, Method: SanitizeZeroFill, Root: f.root,
		Filesystem: FSExt4, DeviceName: f.fsDevName(), DeviceFingerprint: "serial:testserial0001",
		ArtifactDir: filepath.Join(f.root, sanitizeArtifactDirName(jobID)),
		BootID:      "an-older-boot", PID: 12345, PassesPlanned: 1,
		FreeBytesAtStart: f.avail, ReserveBytes: testExpectedReserve,
		StartedAt: time.Now().Add(-time.Hour),
	}
	if err := writeSanitizeJobRecord(rec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, sanitizeArtifactDirName(jobID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, sanitizeManifestName), fmt.Sprintf(
		`{"magic":%q,"version":%d,"job_id":%q,"root":%q,"method":"zero_fill"}`,
		sanitizeArtifactMagic, sanitizeManifestVersion, jobID, f.root))
	for i := 1; i <= 3; i++ {
		write(t, filepath.Join(dir, sanitizePartName(jobID, i)), strings.Repeat("0", 8<<20))
	}
}

// A kernel that cannot confine path resolution stops NEW sanitizations. It must
// not stop the recovery of an OLD one: the artifacts of an interrupted job hold
// real disk space, and skipping recovery leaves the operator with a filesystem
// that looks full and nothing anywhere saying why. That is the exact failure
// this recovery code exists to prevent.
//
// WHAT THIS DOES NOT PROVE: only the health verdict is simulated, so the
// cleanup below still runs on this machine's real openat2. On a kernel that
// genuinely lacks it the cleanup would report errors instead of freeing space.
// What is proven is the ORDER — that the confinement verdict no longer returns
// before recovery is even attempted.
//
// MUTATION: move the `if !confined { ... return h }` block back above the
// RecoverSanitizeJobs call in EnsureSanitizeSubsystem.
func TestRecoveryIsNotSkippedOnAKernelWithoutConfinement(t *testing.T) {
	f := newSanitizeFixture(t)
	files := seedKnownFiles(t, f.root)
	jobID := "abadabadabadabadabadabadabadabad"
	seedInterruptedJob(t, f, jobID)

	prev := confinementAvailable
	confinementAvailable = func() (bool, string) { return false, "simulated kernel without openat2" }
	t.Cleanup(func() { confinementAvailable = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := EnsureSanitizeSubsystem(ctx)

	// The subsystem still refuses to START anything. Recovering is not the same
	// permission as running.
	if h.Enforcing {
		t.Fatal("sanitization is enforcing on a kernel that cannot confine path resolution")
	}
	if h.Status != "degraded" {
		t.Fatalf("health status %q, want degraded", h.Status)
	}
	if h.InterruptedRecovered != 1 {
		t.Fatalf("recovery was skipped: %d interrupted jobs seen, want 1", h.InterruptedRecovered)
	}
	assertNoArtifacts(t, f.root)
	assertKnownFilesIntact(t, files)

	after, err := sanitizeStatfs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailBytes != f.avail {
		t.Fatalf("the disk still looks full after recovery: %s available, want %s",
			humanBytes(after.AvailBytes), humanBytes(f.avail))
	}
}

// A record whose cleanup failed must stay actionable. Retiring it would leave
// the artifacts on disk with nothing pointing at them: later passes skip
// non-running records, so that root would never be swept again and the space
// would be lost until somebody found the directory by hand.
//
// MUTATION: call markRecordInterrupted unconditionally in RecoverSanitizeJobs.
func TestARecordIsNotRetiredWhileItsArtifactsAreStillOnDisk(t *testing.T) {
	f := newSanitizeFixture(t)
	jobID := "cafecafecafecafecafecafecafecafe"
	seedInterruptedJob(t, f, jobID)

	// Cleanup cannot open the workspace: the directory is replaced by a file,
	// which proveSanitizeArtifact reports as an error rather than as a
	// finished cleanup.
	dir := filepath.Join(f.root, sanitizeArtifactDirName(jobID))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep := RecoverSanitizeJobs(ctx)
	if len(rep.Errors) == 0 && len(rep.Unexplained) == 0 {
		t.Fatal("a workspace that could not be cleaned was reported as a clean recovery")
	}

	jobs, err := ReadSanitizeJobs()
	if err != nil {
		t.Fatal(err)
	}
	var got *SanitizeJobRecord
	for i := range jobs {
		if jobs[i].JobID == jobID {
			got = &jobs[i]
		}
	}
	if got == nil {
		t.Fatal("the job record disappeared")
	}
	if got.State != JobRunning {
		t.Fatalf("job state %q after a failed cleanup, want %q so the next pass retries",
			got.State, JobRunning)
	}

	// And the next pass does retry it, rather than walking past.
	if again := RecoverSanitizeJobs(ctx); again.InterruptedJobs != 1 {
		t.Fatalf("the second pass saw %d interrupted jobs, want 1", again.InterruptedJobs)
	}
}

func TestRecoveryIsIdempotent(t *testing.T) {
	f := newSanitizeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first := RecoverSanitizeJobs(ctx)
	second := RecoverSanitizeJobs(ctx)
	if len(first.Errors) != 0 || len(second.Errors) != 0 {
		t.Fatalf("recovery reported errors on a clean tree: %v / %v", first.Errors, second.Errors)
	}
	if second.InterruptedJobs != 0 {
		t.Fatalf("the second pass found %d interrupted jobs", second.InterruptedJobs)
	}
	assertNoArtifacts(t, f.root)
}

func TestARunningJobRecordIsPersistedBeforeAnyLargeWrite(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)

	var recordSeen atomic.Bool
	plan.Options.Progress = func(p SanitizeProgress) {
		if recordSeen.Load() {
			return
		}
		jobs, err := ReadSanitizeJobs()
		if err != nil {
			return
		}
		for _, j := range jobs {
			if j.JobID == plan.JobID && j.State == JobRunning && j.ArtifactDir != "" {
				recordSeen.Store(true)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := RunSanitize(ctx, plan.JobID); err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if !recordSeen.Load() {
		t.Fatal("no durable job record existed while the writer was running; a crash would be undiagnosable")
	}
}

// ---------------------------------------------------------------- trim

// TestTrimControlFlow exercises the discard path with a stub helper.
//
// HARDWARE VALIDATION BOUNDARY: this proves the helper is selected from the
// capability table, invoked with structured arguments and a "--" separator,
// bounded by a timeout, and reported as indeterminate progress that claims no
// erase. It proves NOTHING about whether a real controller erases anything —
// that is not observable from the host and no test in this file claims it.
func TestTrimControlFlowAndArguments(t *testing.T) {
	f := newSanitizeFixture(t)
	argsFile := filepath.Join(f.base, "fstrim-args")
	stub := filepath.Join(f.base, "fstrim-stub")
	write(t, stub, fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\n"+
		"echo '%s: 100 MiB (104857600 bytes) trimmed'\n", argsFile, f.root))
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.installTools(map[Tool]string{ToolFstrim: stub})

	plan := f.mustPlan(SanitizeTrim, nil)
	if plan.Semantics.DeterminateProgress {
		t.Fatal("discard progress must be reported as indeterminate")
	}
	if len(plan.Passes) != 0 {
		t.Fatal("TRIM has no overwrite passes")
	}
	if plan.TotalWriteBytes != 0 {
		t.Fatal("TRIM writes nothing; the plan must say so")
	}
	warned := false
	for _, w := range plan.Warnings {
		if strings.Contains(strings.ToLower(w), "not a secure erase") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a TRIM plan must state that it is not a secure erase: %v", plan.Warnings)
	}

	// The discard heartbeat runs on its own goroutine, so the observations it
	// records must be safe to read from the test goroutine afterwards.
	var sawIndeterminate atomic.Bool
	var fabricatedETA atomic.Bool
	plan.Options.Progress = func(p SanitizeProgress) {
		if !p.Determinate && p.EstimatedRemaining == 0 {
			sawIndeterminate.Store(true)
		}
		if p.EstimatedRemaining != 0 {
			fabricatedETA.Store(true)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if !sawIndeterminate.Load() {
		t.Fatal("no indeterminate progress was reported")
	}
	if fabricatedETA.Load() {
		t.Fatal("an estimated time was fabricated for an operation whose progress nothing reports")
	}
	if res.Final.BytesDiscarded != 104857600 {
		t.Fatalf("discarded range not reported: %d", res.Final.BytesDiscarded)
	}
	if res.Final.BytesWrittenTotal != 0 {
		t.Fatal("TRIM must not report host writes")
	}
	assertNoArtifacts(t, f.root)

	body, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(body)), "\n")
	// Structured arguments with "--" before the derived value, never a shell
	// string: the mount point must arrive as its own argv entry.
	if !containsSeq(args, []string{"-v", "--", f.root}) {
		t.Fatalf("fstrim was not invoked with a separator before the derived path: %v", args)
	}
	t.Log("hardware validation boundary: the stub proves argument handling and progress semantics only; " +
		"whether a device erases discarded blocks is a firmware property and is not asserted here")
}

func containsSeq(hay, needle []string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestTrimRefusedWhenTheDeviceOrHelperCannotSupportIt(t *testing.T) {
	noDiscard := newSanitizeFixture(t, withoutDiscard())
	noDiscard.installTools(map[Tool]string{ToolFstrim: "/bin/true"})
	_, err := noDiscard.plan(SanitizeTrim, nil)
	if got := refusalCode(t, err); got != RefuseMethodUnavailable {
		t.Fatalf("want %s, got %s (%v)", RefuseMethodUnavailable, got, err)
	}

	noHelper := newSanitizeFixture(t)
	noHelper.installTools(nil) // no fstrim on this host
	_, err = noHelper.plan(SanitizeTrim, nil)
	if got := refusalCode(t, err); got != RefuseMethodUnavailable {
		t.Fatalf("want %s, got %s (%v)", RefuseMethodUnavailable, got, err)
	}
}

// ---------------------------------------------------------------- locking

func TestOneDestructiveJobPerPhysicalDevice(t *testing.T) {
	f := newSanitizeFixture(t)
	first := f.mustPlan(SanitizeZeroFill, nil)
	second := f.mustPlan(SanitizeZeroFill, nil)

	if err := AcquireDevice(first.LockFingerprint, first.JobID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseDevice(first.LockFingerprint, first.JobID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := RunSanitize(ctx, second.JobID)
	if got := refusalCode(t, err); got != RefuseDeviceBusy {
		t.Fatalf("want %s, got %s (%v)", RefuseDeviceBusy, got, err)
	}
	assertNoArtifacts(t, f.root)
}

// The formatter and the sanitizer must exclude each other on the HARDWARE. The
// formatter keys its lock on the parent disk; if the sanitizer keys its own on
// the partition it was pointed at, the two never collide and a format and a
// sanitization can run on one drive at the same time.
//
// The drive here reports no serial and no WWN, which is the only shape where
// the two keys can differ: with a serial a partition inherits it and both
// fingerprints are already the same string, so a fixture with one would let
// this test pass with the bug still present.
//
// MUTATION: lock on plan.Fingerprint instead of plan.lockKey() in RunSanitize,
// or make PlanSanitize set LockFingerprint from dev.Identity.Fingerprint().
func TestSanitizeAndFormatExcludeEachOtherOnADriveWithNoSerial(t *testing.T) {
	f := newSanitizeFixture(t, onAPartitionOfADiskWithNoSerial())
	plan := f.mustPlan(SanitizeZeroFill, nil)

	// Precondition: this fixture really is the divergent case. If the two keys
	// were equal here the test would prove nothing.
	if plan.LockFingerprint == plan.Fingerprint {
		t.Fatalf("fixture is not exercising the divergent case: the partition and its disk "+
			"share the fingerprint %q", plan.Fingerprint)
	}

	// The key a whole-disk format of the same hardware would take, derived the
	// formatter's way from live discovery rather than copied from the plan.
	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := inv.ByName(f.devName)
	if !ok {
		t.Fatalf("fixture disk %s is missing from the inventory", f.devName)
	}
	formatterKey := lockFingerprint(inv, disk)

	if err := AcquireDevice(formatterKey, "format-job"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseDevice(formatterKey, "format-job") })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = RunSanitize(ctx, plan.JobID)
	if got := refusalCode(t, err); got != RefuseDeviceBusy {
		t.Fatalf("a sanitization started while a format held the same physical disk: got %s (%v)", got, err)
	}
	assertNoArtifacts(t, f.root)
}

// AcquireDevice refuses an empty key, so a plan that predates the separate
// lock field must fall back to the device's own fingerprint rather than
// failing to lock at all. Narrowing a lock is not worth disabling the feature.
func TestAPlanWithoutTheLockFieldStillLocksSomething(t *testing.T) {
	p := &SanitizePlan{Fingerprint: "serial:abc"}
	if got := p.lockKey(); got != "serial:abc" {
		t.Fatalf("lockKey() = %q with no lock fingerprint, want the device fingerprint", got)
	}
	p.LockFingerprint = "wwn:0x1"
	if got := p.lockKey(); got != "wwn:0x1" {
		t.Fatalf("lockKey() = %q, want the physical-device key", got)
	}
}

// ---------------------------------------------------------------- tier 2

// TestLoopDeviceEndToEnd is the tier-2 test: a real ext4 image on a real loop
// device with a real mount and real statfs. It needs privileges to attach the
// loop device, so it SKIPS rather than pretending to have run.
func TestLoopDeviceEndToEnd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to attach a loop device and mount it; the writer, the reserve, the " +
			"cleanup and the recovery are covered against a simulated-capacity filesystem in the " +
			"other tests in this file")
	}
	for _, tool := range []string{"losetup", "mkfs.ext4", "mount", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if ok, why := storageauth.ConfinementAvailable(); !ok {
		t.Skip(why)
	}

	base := t.TempDir()
	img := filepath.Join(base, "disk.img")
	mnt := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 512<<20); err != nil {
		f, cerr := os.Create(img)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := f.Truncate(512 << 20); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	run := func(name string, args ...string) string {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v: %s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	loop := run("losetup", "--find", "--show", "--", img)
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loop).Run() })
	run("mkfs.ext4", "-q", "-F", "--", loop)
	run("mount", "--", loop, mnt)
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })

	storageauth.SetStateDir(filepath.Join(base, "authstate"))
	t.Cleanup(func() { storageauth.SetStateDir("/var/lib/casanova") })
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: []string{mnt}}); err != nil {
		t.Fatalf("provision the storage authority: %v", err)
	}
	SetStateDir(filepath.Join(base, "maintstate"))
	t.Cleanup(func() { SetStateDir("/var/lib/casanova/storage-maintenance") })
	if h := EnsureSanitizeSubsystem(ctx); !h.Enforcing {
		t.Fatalf("subsystem not enforcing: %s", h.Invariant)
	}

	files := seedKnownFiles(t, mnt)
	before, err := sanitizeStatfs(mnt)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanSanitize(ctx, SanitizeRequest{Root: mnt, Method: SanitizeZeroFill, PrincipalID: "test"})
	if err != nil {
		t.Fatalf("PlanSanitize: %v", err)
	}
	res, err := RunSanitize(ctx, plan.JobID)
	if err != nil {
		t.Fatalf("RunSanitize: %v", err)
	}
	if res.Final.BytesWrittenTotal == 0 {
		t.Fatal("nothing was written")
	}
	assertKnownFilesIntact(t, files)
	assertNoArtifacts(t, mnt)
	after, err := sanitizeStatfs(mnt)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailBytes+(8<<20) < before.AvailBytes {
		t.Fatalf("free space was not reclaimed on a real filesystem: %s -> %s",
			humanBytes(before.AvailBytes), humanBytes(after.AvailBytes))
	}
	if after.AvailBytes <= plan.ReserveBytes {
		t.Fatalf("the real filesystem was left at or below the reserve: %s", humanBytes(after.AvailBytes))
	}
}

// A prepared plan is an authorization, not a document the caller may edit.
//
// RunSanitize used to accept any *SanitizePlan handed to it, with every field
// exported and the runtime safety bound read from w.plan.ReserveBytes — so a
// caller could pass ReserveBytes: 0 and fill the filesystem to capacity, which
// is the single outcome the reserve exists to prevent.
//
// Mutation that must fail this test: have RunSanitize take a *SanitizePlan
// again, or trust plan.ReserveBytes without re-deriving it.
func TestATamperedPlanCannotLowerTheSafetyReserve(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)

	honest := plan.ReserveBytes
	if honest == 0 {
		t.Fatal("precondition: a prepared plan should carry a non-zero reserve")
	}

	// The caller edits its copy, the way a compromised or careless one would.
	plan.ReserveBytes = 0
	plan.TargetBytesPerPass = 1 << 62

	res, err := RunSanitize(context.Background(), plan.JobID)
	if err != nil {
		// A refusal is an acceptable outcome; silently honouring the tampered
		// values is not.
		return
	}
	if res.ReserveBytes < honest {
		t.Fatalf("the run honoured a tampered reserve (%d) instead of re-deriving it (>= %d); "+
			"a safety bound the caller can set to zero is not a safety bound",
			res.ReserveBytes, honest)
	}
}

// A plan executes once. Two concurrent executions must not both win.
//
// Mutation that must fail this test: read the plan without deleting it under
// the same lock.
func TestPreparedPlanIsSingleUse(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)

	if _, err := RunSanitize(context.Background(), plan.JobID); err != nil {
		t.Logf("first run returned %v (acceptable in this fixture)", err)
	}
	if _, err := RunSanitize(context.Background(), plan.JobID); err == nil {
		t.Fatal("the same prepared plan executed twice; a destructive authorization must be " +
			"single-use")
	}
}

// An unknown id is refused rather than treated as an empty plan.
func TestUnknownPlanIDIsRefused(t *testing.T) {
	if _, err := RunSanitize(context.Background(), "not-a-real-job"); err == nil {
		t.Fatal("an unknown plan id was accepted")
	}
}

// A plan that lapsed is refused: the filesystem may have changed since it was
// measured, and the plan's numbers describe a world that no longer exists.
func TestExpiredPlanIsRefused(t *testing.T) {
	f := newSanitizeFixture(t)
	plan := f.mustPlan(SanitizeZeroFill, nil)

	sealedMu.Lock()
	if p, ok := sealedPlans[plan.JobID]; ok {
		p.PreparedAt = time.Now().Add(-2 * sanitizePlanTTL)
	}
	sealedMu.Unlock()

	if _, err := RunSanitize(context.Background(), plan.JobID); err == nil {
		t.Fatal("an expired plan executed")
	}
}
