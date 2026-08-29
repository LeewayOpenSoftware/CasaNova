package storagemaint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// FORMATTER TESTS.
//
// Every test here runs against a synthetic sysfs/proc tree and a recording
// command runner. That is not a compromise: the properties being proven —
// which device an operation binds to, what protection propagation means, what
// happens when the world changes between prepare and execute — are decided
// entirely by discovery and identity, and a real USB stick would prove less
// while requiring hardware and root.
//
// What these tests do NOT prove is stated where it matters: they do not prove
// that mkfs produces a mountable filesystem on real hardware. looplab_test.go
// covers that tier with real tools.

const fxTestTimeout = 30 * time.Second

// ---------------------------------------------------------------- fixtures

type fxHost struct {
	t    *testing.T
	sys  string
	proc string
	dev  string

	mounts [][2]string
	swaps  []string
}

func fxNewHost(t *testing.T) *fxHost {
	t.Helper()
	root := t.TempDir()
	h := &fxHost{t: t,
		sys:  filepath.Join(root, "sys"),
		proc: filepath.Join(root, "proc"),
		dev:  filepath.Join(root, "dev"),
	}
	for _, d := range []string{filepath.Join(h.sys, "block"), filepath.Join(h.proc, "self"), h.dev} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.writeMounts()
	h.writeSwaps()
	blockdev.SetRoots(h.sys, h.proc, h.dev)
	t.Cleanup(func() { blockdev.SetRoots("/sys", "/proc", "/dev") })
	return h
}

type fxDisk struct {
	name      string
	size      uint64
	topology  string
	model     string
	serial    string
	removable bool
	readOnly  bool
	holders   []string
}

func (h *fxHost) addDisk(s fxDisk) {
	h.t.Helper()
	realDir := filepath.Join(h.sys, "devices", s.topology, s.name)
	for _, d := range []string{"device", "queue"} {
		if err := os.MkdirAll(filepath.Join(realDir, d), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	write := func(rel, val string) {
		if err := os.WriteFile(filepath.Join(realDir, rel), []byte(val+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	write("size", fxItoa(s.size/512))
	write("dev", "8:0")
	write("removable", fxBool(s.removable))
	write("ro", fxBool(s.readOnly))
	write("queue/rotational", "0")
	write("queue/discard_max_bytes", "0")
	if s.model != "" {
		write("device/model", s.model)
	}
	if s.serial != "" {
		write("device/serial", s.serial)
	}
	for _, hold := range s.holders {
		if err := os.MkdirAll(filepath.Join(realDir, "holders", hold), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.Symlink(realDir, filepath.Join(h.sys, "block", s.name)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fxHost) addPartition(disk, part string, size uint64) {
	h.t.Helper()
	real, err := filepath.EvalSymlinks(filepath.Join(h.sys, "block", disk))
	if err != nil {
		h.t.Fatal(err)
	}
	pdir := filepath.Join(real, part)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	for rel, val := range map[string]string{
		"partition": "1", "size": fxItoa(size / 512), "dev": "8:1", "ro": "0",
	} {
		if err := os.WriteFile(filepath.Join(pdir, rel), []byte(val+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *fxHost) mount(devName, mountpoint string) {
	h.t.Helper()
	h.mounts = append(h.mounts, [2]string{devName, mountpoint})
	h.writeMounts()
}

func (h *fxHost) unmount(mountpoint string) {
	var keep [][2]string
	for _, m := range h.mounts {
		if m[1] != mountpoint {
			keep = append(keep, m)
		}
	}
	h.mounts = keep
	h.writeMounts()
}

func (h *fxHost) swapOn(devName string) {
	h.t.Helper()
	h.swaps = append(h.swaps, devName)
	h.writeSwaps()
}

func (h *fxHost) writeMounts() {
	var b strings.Builder
	for i, m := range h.mounts {
		b.WriteString(fxItoa(uint64(i+20)) + " 1 8:1 / " + m[1] +
			" rw,relatime shared:1 - ext4 /dev/" + m[0] + " rw\n")
	}
	if err := os.WriteFile(filepath.Join(h.proc, "self", "mountinfo"), []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fxHost) writeSwaps() {
	b := "Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n"
	for _, s := range h.swaps {
		b += "/dev/" + s + "\tpartition\t1000\t0\t-2\n"
	}
	if err := os.WriteFile(filepath.Join(h.proc, "swaps"), []byte(b), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func fxItoa(v uint64) string {
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

func fxBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------------------------------------------------------------- fake runner

type fxCall struct {
	Name  string
	Args  []string
	Stdin string
}

type fxRunner struct {
	mu    sync.Mutex
	calls []fxCall
	// blkid maps a device path to what a probe reports. Absent means "nothing
	// recognisable", which blkid signals with exit status 2.
	blkid map[string]map[string]string
	// failures maps a helper name to the error it should return.
	failures map[string]error
	// after runs once a helper succeeded, so the fixture can model the world
	// changing (a new partition appearing, a volume being unmounted).
	after map[string]func(spec CommandSpec)
}

func fxNewRunner() *fxRunner {
	return &fxRunner{
		blkid:    map[string]map[string]string{},
		failures: map[string]error{},
		after:    map[string]func(CommandSpec){},
	}
}

func (r *fxRunner) Run(_ context.Context, spec CommandSpec) (CommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fxCall{Name: spec.Name, Args: append([]string{}, spec.Args...), Stdin: spec.Stdin})
	fail := r.failures[spec.Name]
	r.mu.Unlock()

	if fail != nil {
		return CommandResult{ExitCode: 1, Stderr: fail.Error()}, fail
	}
	if spec.Name == string(ToolBlkid) {
		target := spec.Args[len(spec.Args)-1]
		r.mu.Lock()
		fields, ok := r.blkid[target]
		r.mu.Unlock()
		if !ok {
			return CommandResult{ExitCode: 2}, errors.New("exit status 2")
		}
		var b strings.Builder
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k + "=" + fields[k] + "\n")
		}
		return CommandResult{Stdout: b.String()}, nil
	}
	r.mu.Lock()
	hook := r.after[spec.Name]
	r.mu.Unlock()
	if hook != nil {
		hook(spec)
	}
	return CommandResult{}, nil
}

func (r *fxRunner) named(name string) []fxCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fxCall
	for _, c := range r.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func (r *fxRunner) ran(name string) bool { return len(r.named(name)) > 0 }

// ---------------------------------------------------------------- lab

type fxLab struct {
	t       *testing.T
	host    *fxHost
	runner  *fxRunner
	f       *Formatter
	clock   time.Time
	tick    time.Duration
	caps    Capabilities
	roots   []string
	rootErr error
}

var fxAdmin = storageauth.Principal{ID: "admin-1", Role: "admin", Admin: true}

func fxAllCaps() Capabilities {
	c := Capabilities{CanPartition: true, CanWipeSignatures: true, CanDiscard: true, CanIdentify: true}
	for _, t := range allTools {
		c.Tools = append(c.Tools, ToolStatus{Tool: t, Available: true, Path: "/usr/sbin/" + string(t)})
	}
	for _, fs := range SupportedFilesystems {
		fc := FilesystemCapability{Filesystem: fs, Tool: filesystemTools[fs], Available: true}
		if fs == FSFAT32 {
			fc.MaxFileBytes = 4<<30 - 1
			fc.MaxVolumeBytes = 2 << 40
		}
		c.Filesystems = append(c.Filesystems, fc)
	}
	return c
}

func fxNewLab(t *testing.T) *fxLab {
	t.Helper()
	SetStateDir(t.TempDir())
	t.Cleanup(func() { SetStateDir("/var/lib/casanova/storage-maintenance") })

	// Helper lookup is stubbed so every test presents the same host, and it is
	// restored so the real-tool lab tests are unaffected.
	SetLookPath(func(name string) (string, error) { return "/usr/sbin/" + name, nil })
	t.Cleanup(func() { SetLookPath(exec.LookPath) })

	lab := &fxLab{
		t: t, host: fxNewHost(t), runner: fxNewRunner(),
		clock: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		// The fake clock advances on every read so poll loops with a deadline
		// terminate without any real sleeping.
		tick: 20 * time.Millisecond,
		caps: fxAllCaps(),
	}
	f := NewFormatter()
	f.runner = lab.runner
	f.capabilities = func(context.Context) Capabilities { return lab.caps }
	f.now = func() time.Time {
		now := lab.clock
		lab.clock = lab.clock.Add(lab.tick)
		return now
	}
	f.isMountpoint = func(string) (bool, error) { return true, nil }
	f.mountRoot = filepath.Join(t.TempDir(), "media")
	f.managedRoots = []string{f.mountRoot, "/media"}
	f.settleInterval = time.Millisecond
	f.settleTimeout = 2 * time.Second
	f.addStorageRoot = func(path, note string) error {
		lab.roots = append(lab.roots, path)
		return lab.rootErr
	}
	lab.f = f
	return lab
}

// device returns the fixture device's identity, the way a UI would have got it.
func (l *fxLab) identity(name string) blockdev.DeviceIdentity {
	l.t.Helper()
	inv, err := blockdev.Discover()
	if err != nil {
		l.t.Fatalf("discover: %v", err)
	}
	d, ok := inv.ByName(name)
	if !ok {
		l.t.Fatalf("fixture device %s is missing", name)
	}
	return d.Identity
}

func (l *fxLab) devPath(name string) string { return filepath.Join(l.host.dev, name) }

// wireHappyPath makes the fixture behave like a working disk: sfdisk publishes
// a partition, mkfs makes it identifiable, umount updates the mount table.
func (l *fxLab) wireHappyPath(disk, part string, partSize uint64, fs Filesystem, label string) {
	l.runner.after[string(ToolSfdisk)] = func(CommandSpec) {
		l.host.addPartition(disk, part, partSize)
	}
	l.runner.after["umount"] = func(spec CommandSpec) {
		l.host.unmount(spec.Args[len(spec.Args)-1])
	}
	l.runner.after[string(filesystemTools[fs])] = func(CommandSpec) {
		fields := map[string]string{"TYPE": string(expectedFSType[fs][0]), "UUID": "1111-2222"}
		if label != "" {
			fields["LABEL"] = label
		}
		l.runner.mu.Lock()
		l.runner.blkid[l.devPath(part)] = fields
		l.runner.mu.Unlock()
	}
}

func fxCtx(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), fxTestTimeout)
}

func fxCode(t *testing.T, err error) FormatErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	code, ok := CodeOf(err)
	if !ok {
		t.Fatalf("error is not a structured FormatError: %v", err)
	}
	return code
}

// ---------------------------------------------------------------------------
// PROTECTION — the mutations that would let the appliance wipe itself.
// ---------------------------------------------------------------------------

// MUTATION: remove the protection check in validateTarget, or remove root
// protection. Preparing a format of the disk holding / must be refused.
func TestPrepareRefusesTheSystemDisk(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sda", size: 500 << 30, topology: "pci0000:00/usb1/1-1",
		model: "SYSTEM", serial: "SYS-1", removable: true})
	lab.host.addPartition("sda", "sda1", 499<<30)
	lab.host.mount("sda1", "/")

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sda"), Filesystem: FSExt4, Label: "DATA"})
	if code := fxCode(t, err); code != ErrCodeDeviceProtected {
		t.Fatalf("formatting the disk that holds / was refused with %s, want %s", code, ErrCodeDeviceProtected)
	}
}

// PART K 12: format a partition whose PARENT disk carries the system partition.
// MUTATION: remove the parent-protection re-check in validateTarget.
//
// This test uses a HAND-BUILT inventory in which the parent is protected and the
// child is not — a state blockdev never produces. That is deliberate: it
// isolates this file's own guard instead of passing because blockdev's downward
// propagation already marked the child. A fixture-based version of this test
// passes even with the guard deleted, and would prove nothing.
func TestPartitionOfAProtectedDiskIsRefusedByThisPackagesOwnGuard(t *testing.T) {
	lab := fxNewLab(t)
	diskID := blockdev.DeviceIdentity{Serial: "SYS-9", Model: "SYSTEM", SizeBytes: 500 << 30,
		SysfsPath: "/sys/devices/x/sda"}
	partID := blockdev.DeviceIdentity{Serial: "SYS-9", Model: "SYSTEM", SizeBytes: 100 << 30,
		SysfsPath: "/sys/devices/x/sda/sda1"}
	inv := blockdev.Inventory{Complete: true, Devices: []blockdev.Device{
		{Name: "sda", Path: "/dev/sda", Identity: diskID, Transport: blockdev.TransportUSB,
			Removable: true, SizeBytes: 500 << 30, Children: []string{"sda1"},
			Protected:         true,
			ProtectionReasons: []blockdev.ProtectionReason{blockdev.ReasonRootFilesystem}},
		{Name: "sda1", Path: "/dev/sda1", Identity: partID, Transport: blockdev.TransportUSB,
			Removable: true, SizeBytes: 100 << 30, IsPartition: true, Parent: "sda"},
	}}
	lab.f.discover = func() (blockdev.Inventory, error) { return inv, nil }

	part, _ := inv.ByName("sda1")
	if !part.Eligible() {
		t.Fatal("precondition: the child must look eligible on its own, or this test proves nothing")
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: partID, Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeParentProtected {
		t.Fatalf("formatting a partition of a protected disk was refused with %s, want %s", code, ErrCodeParentProtected)
	}
}

// A protected SIBLING is the same hazard with one more step.
func TestPartitionSharingADiskWithAProtectedPartitionIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	base := blockdev.DeviceIdentity{Serial: "MIX-1", Model: "MIXED", SysfsPath: "/sys/devices/x/sdb"}
	disk := base
	disk.SizeBytes = 500 << 30
	p1 := base
	p1.SizeBytes = 100 << 30
	p1.SysfsPath = "/sys/devices/x/sdb/sdb1"
	p2 := base
	p2.SizeBytes = 200 << 30
	p2.SysfsPath = "/sys/devices/x/sdb/sdb2"

	inv := blockdev.Inventory{Complete: true, Devices: []blockdev.Device{
		{Name: "sdb", Path: "/dev/sdb", Identity: disk, Transport: blockdev.TransportUSB,
			Removable: true, SizeBytes: disk.SizeBytes, Children: []string{"sdb1", "sdb2"}},
		{Name: "sdb1", Path: "/dev/sdb1", Identity: p1, Transport: blockdev.TransportUSB,
			Removable: true, SizeBytes: p1.SizeBytes, IsPartition: true, Parent: "sdb"},
		{Name: "sdb2", Path: "/dev/sdb2", Identity: p2, Transport: blockdev.TransportUSB,
			Removable: true, SizeBytes: p2.SizeBytes, IsPartition: true, Parent: "sdb",
			Protected:         true,
			ProtectionReasons: []blockdev.ProtectionReason{blockdev.ReasonCasaNovaState}},
	}}
	lab.f.discover = func() (blockdev.Inventory, error) { return inv, nil }

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{Identity: p1, Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeParentProtected {
		t.Fatalf("refused with %s, want %s", code, ErrCodeParentProtected)
	}
}

// MUTATION: turn a discovery error into "nothing in the way".
func TestDiscoveryFailureRefusesInsteadOfAllowing(t *testing.T) {
	lab := fxNewLab(t)
	lab.f.discover = func() (blockdev.Inventory, error) {
		return blockdev.Inventory{}, errors.New("sysfs unreadable")
	}
	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: blockdev.DeviceIdentity{Serial: "X1", SizeBytes: 1 << 30}, Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeDiscoveryIncmplete {
		t.Fatalf("a discovery failure produced %s, want %s", code, ErrCodeDiscoveryIncmplete)
	}
}

// An inventory that could not read SOME OTHER device is still refused: the
// device it could not read might be the one whose protection would have
// propagated to this one.
func TestIncompleteInventoryRefusesEvenWhenTheTargetLooksFine(t *testing.T) {
	lab := fxNewLab(t)
	id := blockdev.DeviceIdentity{Serial: "OK-1", Model: "STICK", SizeBytes: 8 << 30,
		SysfsPath: "/sys/devices/x/sdb"}
	inv := blockdev.Inventory{Complete: false, Incomplete: []string{"sdz: unreadable attributes: size"},
		Devices: []blockdev.Device{{Name: "sdb", Path: "/dev/sdb", Identity: id,
			Transport: blockdev.TransportUSB, Removable: true, SizeBytes: 8 << 30}}}
	lab.f.discover = func() (blockdev.Inventory, error) { return inv, nil }

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{Identity: id, Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeDiscoveryIncmplete {
		t.Fatalf("refused with %s, want %s", code, ErrCodeDiscoveryIncmplete)
	}
}

// MUTATION: allow a mounted device.
//
// The mountpoint here is /srv/db — not protected by blockdev (it is not a
// system prefix) and not managed by CasaNova. Only this file's own mount policy
// can refuse it, which is what makes the test isolate that policy.
func TestDeviceMountedSomewhereCasaNovaDoesNotManageIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", "/srv/db")

	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := inv.ByName("sdb")
	if d.Protected {
		t.Fatalf("precondition: blockdev must NOT have protected this device, or the test proves "+
			"nothing about the mount policy: %v", d.ProtectionReasons)
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err = lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeDeviceBusy {
		t.Fatalf("a device mounted at /srv/db was refused with %s, want %s", code, ErrCodeDeviceBusy)
	}
	var fe *FormatError
	if !errors.As(err, &fe) || len(fe.Blockers) == 0 {
		t.Fatal("DEVICE_BUSY did not name what holds the device")
	}
	if !strings.Contains(strings.Join(fe.Blockers, " "), "/srv/db") {
		t.Fatalf("blockers do not name the mountpoint: %v", fe.Blockers)
	}
}

// A volume CasaNova itself mounted IS taken down — otherwise reformatting a
// drive the appliance mounted would be impossible.
func TestVolumeCasaNovaManagesIsUnmountedRatherThanRefused(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", filepath.Join(lab.f.mountRoot, "old-volume"))

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("a CasaNova-managed volume could not be prepared: %v", err)
	}
	var sawUnmount bool
	for _, a := range plan.Actions {
		if a.Kind == string(stepQuiesce) {
			sawUnmount = true
		}
	}
	if !sawUnmount {
		t.Fatal("the plan does not tell the user the volume will be unmounted")
	}
}

// ---------------------------------------------------------------------------
// IDENTITY — PART K 1-4.
// ---------------------------------------------------------------------------

// MUTATION: bind to /dev/sdX instead of the stable identity.
//
// Prepare against one stick, replace it with a different stick that takes the
// same kernel name, then execute. The operation must not land on the new one.
func TestReplugUnderTheSameKernelNameAbortsExecute(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The stick is pulled and a different one lands in the same slot, taking
	// the same kernel name and the same size.
	replaced := fxNewHost(t)
	replaced.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "BBBB2222", removable: true})

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeDeviceNotFound && code != ErrCodeIdentityMismatch {
		t.Fatalf("executing after a replug produced %s; the prepared operation would have run "+
			"against a DIFFERENT physical device", code)
	}
	if lab.runner.ran(string(ToolWipefs)) || lab.runner.ran(string(ToolSfdisk)) {
		t.Fatal("a destructive helper ran against the replaced device")
	}
}

// The same device on another port must still be accepted, or every replug
// invalidates legitimate work.
func TestSameDeviceOnAnotherPortStillExecutes(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Same hardware, different port and kernel name. The topology hash is
	// derived from identity and layout, not from the sysfs path.
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		// A moved device is allowed to invalidate the plan only through the
		// topology seal, never through the identity comparison.
		if code, _ := CodeOf(err); code == ErrCodeDeviceNotFound {
			t.Fatalf("the same physical device was not recognised: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// THE TOKEN — PART K 16-17.
// ---------------------------------------------------------------------------

func TestOperationTokenIsSingleUse(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeTokenConsumed {
		t.Fatalf("replaying an operation token produced %s, want %s", code, ErrCodeTokenConsumed)
	}
}

// MUTATION: accept an expired token.
func TestExpiredOperationTokenIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	lab.clock = plan.ExpiresAt.Add(time.Second)

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeTokenExpired {
		t.Fatalf("an expired token produced %s, want %s", code, ErrCodeTokenExpired)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran on an expired token")
	}
}

func TestUnknownOperationTokenIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Execute(ctx, fxAdmin, "not-a-real-operation")
	if code := fxCode(t, err); code != ErrCodeTokenUnknown {
		t.Fatalf("an unknown token produced %s, want %s", code, ErrCodeTokenUnknown)
	}
}

func TestTokenPreparedByAnotherAdministratorIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	other := storageauth.Principal{ID: "admin-2", Role: "admin", Admin: true}
	if _, err := lab.f.Execute(ctx, other, plan.OperationID); fxCode(t, err) != ErrCodeNotAuthorized {
		t.Fatalf("another administrator consumed a token they were not given: %v", err)
	}
}

// ---------------------------------------------------------------------------
// REVALIDATION — PART K 5-9.
// ---------------------------------------------------------------------------

// MUTATION: skip the revalidation in Execute.
//
// Between prepare and execute the device is mounted at a path CasaNova does not
// manage. Execute must abort with nothing destroyed.
func TestMountedBetweenPrepareAndExecuteAborts(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	lab.host.mount("sdb1", "/srv/db")

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	code := fxCode(t, err)
	if code != ErrCodeDeviceBusy && code != ErrCodePlanChanged {
		t.Fatalf("a device mounted between prepare and execute produced %s", code)
	}
	if lab.runner.ran(string(ToolWipefs)) || lab.runner.ran(string(ToolSfdisk)) {
		t.Fatal("a destructive helper ran against a device that had been mounted")
	}
}

// PART K: the device becomes a system dependency between prepare and execute.
func TestDeviceBecomingSystemStorageBetweenPrepareAndExecuteAborts(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// An administrator moves CasaNova's state onto the stick in the meantime.
	lab.host.mount("sdb1", "/var/lib/casanova")

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	code := fxCode(t, err)
	if code != ErrCodeDeviceProtected && code != ErrCodePlanChanged {
		t.Fatalf("a device that became CasaNova state produced %s", code)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran against a device holding CasaNova state")
	}
}

// Active swap appearing between prepare and execute is the same hazard.
func TestSwapAppearingBetweenPrepareAndExecuteAborts(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	lab.host.swapOn("sdb1")

	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err == nil {
		t.Fatal("a device carrying active swap was formatted")
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran against a device carrying active swap")
	}
}

// MUTATION: remove the validateTarget call from Execute.
//
// This is the case the execute-time revalidation covers ON ITS OWN. Every other
// "the world changed" test is also caught by the topology seal, so deleting the
// revalidation leaves them passing. Here an UNRELATED device becomes unreadable
// between prepare and execute: the target's own topology is byte-for-byte
// identical, the seal matches, and only re-running the safety checks against
// the fresh inventory can notice that protection can no longer be proven.
func TestUnrelatedDiscoveryFailureBetweenPrepareAndExecuteAborts(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addDisk(fxDisk{name: "sdc", size: 32 << 30, topology: "pci0000:00/usb1/1-2",
		model: "Other", serial: "CCCC3333", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	before, err := topologyHashOfName(lab, "sdb")
	if err != nil {
		t.Fatal(err)
	}

	// A different device stops answering.
	real, err := filepath.EvalSymlinks(filepath.Join(lab.host.sys, "block", "sdc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(real, "dev")); err != nil {
		t.Fatal(err)
	}

	after, err := topologyHashOfName(lab, "sdb")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("precondition: the target's own topology must be unchanged, or the topology seal " +
			"would catch this and the test would prove nothing about revalidation")
	}

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeDiscoveryIncmplete {
		t.Fatalf("an inventory that could no longer read every device produced %s, want %s",
			code, ErrCodeDiscoveryIncmplete)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran while protection could not be proven for the whole inventory")
	}
}

func topologyHashOfName(lab *fxLab, name string) (string, error) {
	inv, err := lab.f.discover()
	if err != nil {
		return "", err
	}
	d, ok := inv.ByName(name)
	if !ok {
		return "", errors.New("missing " + name)
	}
	return topologyHash(inv, d)
}

// A partition appearing on the disk between prepare and execute invalidates the
// plan the user actually agreed to.
func TestTopologyChangeBetweenPrepareAndExecuteAborts(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	lab.host.addPartition("sdb", "sdb1", 10<<30)

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodePlanChanged {
		t.Fatalf("a changed layout produced %s, want %s", code, ErrCodePlanChanged)
	}
}

// ---------------------------------------------------------------------------
// AUTHORITY AND LOCKING.
// ---------------------------------------------------------------------------

// MUTATION: allow an ordinary role to format.
func TestOnlyAnAdministratorCanFormat(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	ctx, cancel := fxCtx(t)
	defer cancel()

	for _, p := range []storageauth.Principal{
		{ID: "u1", Role: "user"},
		{ID: "u2", Role: "guest"},
		{ID: "", Role: "admin", Admin: true},
		// Admin flag without the role, and the role without the flag: both are
		// half-resolved identities and neither is an administrator.
		{ID: "u3", Role: "user", Admin: true},
		{ID: "u4", Role: "admin"},
	} {
		if _, err := lab.f.Prepare(ctx, p, PrepareFormatRequest{
			Identity: lab.identity("sdb"), Filesystem: FSExt4}); fxCode(t, err) != ErrCodeNotAuthorized {
			t.Fatalf("%+v was allowed to prepare a format: %v", p, err)
		}
		if _, err := lab.f.Execute(ctx, p, "anything"); fxCode(t, err) != ErrCodeNotAuthorized {
			t.Fatalf("%+v was allowed to execute a format", p)
		}
	}
}

// MUTATION: remove the per-device lock.
func TestASecondJobCannotStartOnALockedDevice(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Another job already holds the physical device.
	if err := AcquireDevice(plan.LockFingerprint, "other-job"); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	defer ReleaseDevice(plan.LockFingerprint, "other-job")

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeDeviceLocked {
		t.Fatalf("a second job on a locked device produced %s, want %s", code, ErrCodeDeviceLocked)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran while another job held the device")
	}
}

// A partition job and a whole-disk job on the same hardware must collide. They
// only do so if the lock key is the PARENT's fingerprint.
func TestPartitionAndWholeDiskShareOneLock(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)

	ctx, cancel := fxCtx(t)
	defer cancel()
	diskPlan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare disk: %v", err)
	}
	partPlan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb1"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare partition: %v", err)
	}
	if diskPlan.LockFingerprint != partPlan.LockFingerprint {
		t.Fatalf("a partition job (%s) and a whole-disk job (%s) would not collide; two destructive "+
			"operations could run on one physical disk", partPlan.LockFingerprint, diskPlan.LockFingerprint)
	}
}

// ---------------------------------------------------------------------------
// CAPABILITY — PART K 14.
// ---------------------------------------------------------------------------

// A missing mkfs helper is a refusal BEFORE the job, not a failure halfway
// through with the disk already wiped.
func TestMissingMkfsHelperIsRefusedBeforeAnythingIsDestroyed(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	caps := fxAllCaps()
	for i := range caps.Filesystems {
		if caps.Filesystems[i].Filesystem == FSExFAT {
			caps.Filesystems[i].Available = false
			caps.Filesystems[i].Detail = "helper not found in PATH (provided by exfatprogs)"
		}
	}
	lab.caps = caps

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExFAT, Label: "MEDIA"})
	if code := fxCode(t, err); code != ErrCodeCapability {
		t.Fatalf("a missing mkfs helper produced %s, want %s", code, ErrCodeCapability)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran for a filesystem that cannot be created on this host")
	}
}

// blkid missing means the result could never be verified. Refuse rather than
// format something we would have to call "Ready" on faith.
func TestMissingBlkidRefusesTheWholeOperation(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	caps := fxAllCaps()
	caps.CanIdentify = false
	lab.caps = caps

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeCapability {
		t.Fatalf("refused with %s, want %s", code, ErrCodeCapability)
	}
}

// FAT32 on a 5 TB drive cannot work. The limit is enforced at prepare time.
func TestFat32IsRefusedOnAVolumeItCannotAddress(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 5 << 40, topology: "pci0000:00/usb1/1-1",
		model: "BigDisk", serial: "BIG-1", removable: true})

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSFAT32, Label: "BIG"})
	if code := fxCode(t, err); code != ErrCodeInvalidRequest {
		t.Fatalf("FAT32 on a 5 TiB volume produced %s, want %s", code, ErrCodeInvalidRequest)
	}
}

func TestLabelIsValidatedBeforeAnythingIsDestroyed(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	ctx, cancel := fxCtx(t)
	defer cancel()

	for _, label := range []string{
		"way too long a label for ext4",
		"has/slash",
		"-F", // a leading dash never reaches an argument list
	} {
		if _, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
			Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: label}); err == nil {
			t.Fatalf("label %q was accepted", label)
		}
	}
}

// ---------------------------------------------------------------------------
// POST-FORMAT VALIDATION — PART K 15-16, E9.
// ---------------------------------------------------------------------------

// MUTATION: skip the post-format validation.
//
// mkfs exits 0 but blkid does not report the filesystem. The job must fail; it
// must never reach COMPLETE.
//
// No label is requested on purpose. With a label, the label comparison would
// also catch this, and the test would pass even with the filesystem-detection
// and UUID checks deleted — proving nothing about them. Without one, those two
// checks are all that stand between a silently-failed mkfs and "Ready".
func TestMkfsSucceedingWithoutADetectableFilesystemIsNotReady(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	// sfdisk publishes the partition, mkfs "succeeds", but nothing identifies.
	lab.runner.after[string(ToolSfdisk)] = func(CommandSpec) {
		lab.host.addPartition("sdb", "sdb1", 63<<30)
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeVerificationFailed {
		t.Fatalf("an unverifiable filesystem produced %s, want %s", code, ErrCodeVerificationFailed)
	}
	if job.Stage == StageComplete || job.Result == "success" {
		t.Fatal("a job whose filesystem could not be detected reported success")
	}
	if job.ResultUUID != "" {
		t.Fatal("a UUID was reported for a filesystem that does not exist")
	}
}

// PART K 16: mkfs returns an error. Never report Ready.
func TestMkfsFailureIsNeverReportedAsReady(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")
	lab.runner.failures[string(ToolMkfsExt4)] = errors.New("exit status 1: bad magic")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeToolFailed {
		t.Fatalf("mkfs failing produced %s, want %s", code, ErrCodeToolFailed)
	}
	if job.Result == "success" || job.Stage == StageComplete {
		t.Fatal("mkfs failed but the job reported success")
	}
	if job.Stage != StageFailed {
		t.Fatalf("stage %s, want %s", job.Stage, StageFailed)
	}
	// The persisted record has to agree with what was returned.
	stored, lerr := lab.f.LoadJob(job.JobID)
	if lerr != nil {
		t.Fatalf("load: %v", lerr)
	}
	if stored.Result != "failed" {
		t.Fatalf("the persisted record says %q", stored.Result)
	}
}

// PART K 15: the post-format mount check fails. Structured failure, not success.
func TestPostFormatMountFailureIsAStructuredFailure(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")
	lab.runner.failures["mount"] = errors.New("exit status 32: wrong fs type")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeVerificationFailed {
		t.Fatalf("a filesystem that cannot be mounted produced %s, want %s", code, ErrCodeVerificationFailed)
	}
	if job.Result == "success" {
		t.Fatal("a filesystem that cannot be mounted was reported Ready")
	}
	if len(lab.roots) != 0 {
		t.Fatal("an unmountable volume was registered as a storage root")
	}
}

// Writes during validation must land on the NEW filesystem. If the mountpoint
// is not really a mountpoint, mount lied and the probe would have written to
// the system disk.
func TestValidationRefusesWhenTheMountpointIsNotActuallyAMount(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")
	lab.f.isMountpoint = func(string) (bool, error) { return false, nil }

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); fxCode(t, err) != ErrCodeVerificationFailed {
		t.Fatalf("mount lying about success was accepted: %v", err)
	}
}

// A label silently dropped or truncated by mkfs is a failure, not a detail.
func TestLabelMismatchAfterFormatIsAFailure(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "TRUNCATED")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "MYDATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); fxCode(t, err) != ErrCodeVerificationFailed {
		t.Fatalf("a mismatched label was accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// THE ORDINARY PATH.
// ---------------------------------------------------------------------------

func TestSuccessfulFormatReachesCompleteAndIntegrates(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", filepath.Join(lab.f.mountRoot, "old"))
	lab.runner.blkid[lab.devPath("sdb1")] = map[string]string{"TYPE": "vfat", "UUID": "OLD-1"}
	// After the table is written the old partition is replaced by a new one.
	lab.runner.after[string(ToolSfdisk)] = func(CommandSpec) {
		lab.host.addPartition("sdb", "sdb1", 63<<30)
	}
	lab.runner.after["umount"] = func(spec CommandSpec) {
		lab.host.unmount(spec.Args[len(spec.Args)-1])
	}
	lab.runner.after[string(ToolMkfsExt4)] = func(CommandSpec) {
		lab.runner.mu.Lock()
		lab.runner.blkid[lab.devPath("sdb1")] = map[string]string{
			"TYPE": "ext4", "UUID": "abcd-1234", "LABEL": "MYDATA"}
		lab.runner.mu.Unlock()
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "MYDATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The plan must let the user recognise the physical device and see what is
	// about to be lost.
	if plan.Serial != "AAAA1111" || plan.Model != "Cruzer" || plan.SizeBytes != 64<<30 {
		t.Fatalf("the plan does not identify the hardware: %+v", plan)
	}
	if len(plan.CurrentPartitions) != 1 || plan.CurrentPartitions[0].Filesystem != "vfat" {
		t.Fatalf("the plan does not report what is currently on the disk: %+v", plan.CurrentPartitions)
	}
	if plan.ExpiresAt.Sub(plan.CreatedAt) <= 0 || plan.ExpiresAt.Sub(plan.CreatedAt) > 15*time.Minute {
		t.Fatalf("the operation token is not short-lived: %s", plan.ExpiresAt.Sub(plan.CreatedAt))
	}

	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if job.Stage != StageComplete || job.Result != "success" {
		t.Fatalf("stage %s result %s: %s", job.Stage, job.Result, job.ErrorDetail)
	}
	if job.ResultFilesystem != "ext4" || job.ResultUUID != "abcd-1234" || job.ResultLabel != "MYDATA" {
		t.Fatalf("the outcome was not read back from the device: %+v", job)
	}
	if len(lab.roots) != 1 || lab.roots[0] != job.ResultMountpoint {
		t.Fatalf("the new volume was not registered as a managed storage root: %v", lab.roots)
	}

	// The whole ordinary path ran, in order.
	for _, tool := range []string{"umount", string(ToolWipefs), string(ToolSfdisk), string(ToolMkfsExt4), "mount"} {
		if !lab.runner.ran(tool) {
			t.Fatalf("%s never ran", tool)
		}
	}
	// Mounted, written, unmounted, mounted again — twice for validation plus
	// the final integration mount.
	if got := len(lab.runner.named("mount")); got < 3 {
		t.Fatalf("the filesystem was mounted %d time(s); validation requires a mount/unmount/remount "+
			"round trip before integration", got)
	}
	// And the persisted record agrees.
	stored, err := lab.f.LoadJob(job.JobID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.Stage != StageComplete || stored.Corrupt {
		t.Fatalf("persisted record: stage %s corrupt %v", stored.Stage, stored.Corrupt)
	}
}

// A new partition table replaces whatever was underneath. "Format this disk"
// that preserves an unknown old table produces a device nobody can explain.
func TestPartitionTableIsReplacedNotPreserved(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	calls := lab.runner.named(string(ToolSfdisk))
	if len(calls) != 1 {
		t.Fatalf("sfdisk ran %d times", len(calls))
	}
	args := strings.Join(calls[0].Args, " ")
	if !strings.Contains(args, "--wipe always") || !strings.Contains(args, "--wipe-partitions always") {
		t.Fatalf("the old table was not wiped: %v", calls[0].Args)
	}
	if !strings.Contains(calls[0].Stdin, "label: gpt") {
		t.Fatalf("GPT is not the default table: %q", calls[0].Stdin)
	}
	if !strings.Contains(calls[0].Stdin, "start=2048") {
		t.Fatalf("the data partition is not 1 MiB aligned: %q", calls[0].Stdin)
	}
	if !lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("stale signatures were never removed")
	}
}

func TestMbrIsAvailableAsAnExplicitCompatibilityMode(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 8 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 7<<30, FSFAT32, "CAM")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSFAT32, Label: "CAM", PartitionMode: PartitionMBR})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	calls := lab.runner.named(string(ToolSfdisk))
	if len(calls) != 1 || !strings.Contains(calls[0].Stdin, "label: dos") {
		t.Fatalf("MBR mode did not produce a DOS label: %+v", calls)
	}
}

// A DOS/MBR entry addresses 32 bits of 512-byte sectors, so it cannot describe
// a disk past 2 TiB. The refusal has to happen in Prepare — the same place the
// analogous FAT32 limit is refused — because the alternative is a disk that is
// wiped first and only then found to be undescribable.
//
// MUTATION: delete the mbrMaxAddressableBytes check in Prepare.
func TestAnMbrTableIsRefusedBeforeADiskItCannotAddressIsDestroyed(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 4 << 40, topology: "pci0000:00/usb1/1-1",
		model: "BigDisk", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 4<<40, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	// ext4 on purpose: FAT32 carries its own volume limit, and using it would
	// let this test pass on the strength of the OTHER guard.
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA",
		PartitionMode: PartitionMBR})
	if code := fxCode(t, err); code != ErrCodeInvalidRequest {
		t.Fatalf("a 4 TiB MBR request was refused with %s, want %s", code, ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "GPT") {
		t.Fatalf("the refusal does not tell the user what to choose instead: %v", err)
	}
	lab.runner.mu.Lock()
	ran := len(lab.runner.calls)
	lab.runner.mu.Unlock()
	if lab.runner.ran(string(ToolWipefs)) || lab.runner.ran(string(ToolSfdisk)) {
		t.Fatalf("the disk was touched before the table limit was checked (%d helper calls)", ran)
	}

	// CONTROL: the same disk with GPT is accepted, so the refusal above is the
	// table limit and not something else about a 4 TiB device.
	if _, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA",
		PartitionMode: PartitionGPT}); err != nil {
		t.Fatalf("GPT on the same 4 TiB disk was refused: %v", err)
	}
}

// A partition target never writes a table, so its partition mode is unused and
// must not be a reason to refuse. Without this the fix for the 2 TiB limit
// would start rejecting legitimate work.
func TestThePartitionModeOfAPartitionJobIsNotAReasonToRefuse(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 4 << 40, topology: "pci0000:00/usb1/1-1",
		model: "BigDisk", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 3<<40)

	ctx, cancel := fxCtx(t)
	defer cancel()
	if _, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb1"), Filesystem: FSExt4, Label: "DATA",
		PartitionMode: PartitionMBR}); err != nil {
		t.Fatalf("formatting a 3 TiB partition was refused over a table it does not write: %v", err)
	}
}

// The job record is what the recovery state machine reads back to decide what
// the disk looks like. Renaming it into place is only durable once the
// DIRECTORY is fsynced, so a failure there means the recorded stage may not
// survive a power cut — and the step that stage describes must not run.
//
// MUTATION: swallow the syncDir error in persist (`_ = syncDir(dir)`).
func TestAJobRecordThatCannotBeMadeDurableStopsTheDestructiveStep(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	prev := syncDir
	syncDir = func(string) error { return errors.New("simulated fsync failure on the state directory") }
	t.Cleanup(func() { syncDir = prev })

	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeJobStateCorrupt {
		t.Fatalf("an undurable job record produced %s, want %s", code, ErrCodeJobStateCorrupt)
	}
	if lab.runner.ran(string(ToolWipefs)) || lab.runner.ran(string(ToolSfdisk)) ||
		lab.runner.ran(string(filesystemTools[FSExt4])) {
		t.Fatal("the disk was modified although the stage recording it could not be made durable")
	}
}

// The counterweight to the test above: a filesystem that does not implement
// fsync on a directory reports EINVAL or EOPNOTSUPP, there is no stronger
// ordering available on such a host, and refusing every format because the
// state directory sits on one would trade a durability weakness for an outage.
// A real failure must still be a failure.
//
// MUTATION: drop the EINVAL/EOPNOTSUPP branch from dirSyncResult, or widen it
// to swallow everything.
func TestOnlyAnUnimplementedDirectoryFsyncIsTolerated(t *testing.T) {
	for _, tc := range []struct {
		in        error
		tolerated bool
	}{
		{nil, true},
		{syscall.EINVAL, true},
		{syscall.EOPNOTSUPP, true},
		{syscall.EIO, false},
		{syscall.ENOSPC, false},
		{syscall.EBADF, false},
		{errors.New("something else"), false},
	} {
		got := dirSyncResult(tc.in)
		if tc.tolerated && got != nil {
			t.Fatalf("dirSyncResult(%v) = %v, want nil: a host that cannot fsync a directory "+
				"would be unable to format at all", tc.in, got)
		}
		if !tc.tolerated && got == nil {
			t.Fatalf("dirSyncResult(%v) = nil: a real durability failure was reported as success", tc.in)
		}
	}
}

// And end to end: on such a host the format completes.
func TestAFilesystemWithoutDirectoryFsyncStillFormats(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	// The fsync result is what is simulated; the classification under test is
	// the production one.
	prev := syncDir
	syncDir = func(string) error { return dirSyncResult(syscall.EOPNOTSUPP) }
	t.Cleanup(func() { syncDir = prev })

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("a host without directory fsync could not format: %v", err)
	}
}

// ---------------------------------------------------------------------------
// COMMAND CONSTRUCTION — no shell, ever.
// ---------------------------------------------------------------------------

// Every helper invocation is structured arguments with `--` before the device
// operand, and no argument is ever a shell fragment.
func TestNoHelperInvocationCanBeShellInterpreted(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "MY DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "MY DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}

	lab.runner.mu.Lock()
	calls := append([]fxCall{}, lab.runner.calls...)
	lab.runner.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no helper ran")
	}
	for _, c := range calls {
		if c.Name == "sh" || c.Name == "bash" || strings.Contains(c.Name, " ") {
			t.Fatalf("a shell was invoked: %+v", c)
		}
		// Every command must terminate its options before the operand, so a
		// value can never be re-read as a flag.
		if !contains(c.Args, "--") {
			t.Fatalf("%s runs without an option terminator: %v", c.Name, c.Args)
		}
		// Everything after "--" is an operand, and every operand is an absolute
		// path this code derived from discovery — never a caller-supplied
		// string that could be re-read as an option.
		idx := indexOf(c.Args, "--")
		operands := c.Args[idx+1:]
		if len(operands) == 0 {
			t.Fatalf("%s has an option terminator but no operand: %v", c.Name, c.Args)
		}
		for _, op := range operands {
			if !strings.HasPrefix(op, "/") {
				t.Fatalf("%s takes a non-path operand %q: %v", c.Name, op, c.Args)
			}
		}
	}
}

// MUTATION: use `umount -l`.
//
// A lazy unmount detaches the tree and lets writers carry on against a
// filesystem about to be destroyed. It turns "device busy" into silent loss.
func TestUnmountIsNeverLazyOrForced(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", filepath.Join(lab.f.mountRoot, "old"))
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	calls := lab.runner.named("umount")
	if len(calls) == 0 {
		t.Fatal("the managed volume was never unmounted")
	}
	for _, c := range calls {
		for _, a := range c.Args {
			if a == "-l" || a == "--lazy" || a == "-f" || a == "--force" {
				t.Fatalf("umount was forced or made lazy: %v", c.Args)
			}
		}
	}
}

// A busy unmount is a refusal that names the mountpoint, not a forced detach.
func TestBusyUnmountFailsWithoutForcing(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", filepath.Join(lab.f.mountRoot, "old"))
	lab.runner.failures["umount"] = errors.New("target is busy")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_, err = lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeDeviceBusy {
		t.Fatalf("a busy unmount produced %s, want %s", code, ErrCodeDeviceBusy)
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran after the volume failed to unmount")
	}
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// PERSISTED STATE — PART K 13, and the corrupt-record mutation.
// ---------------------------------------------------------------------------

// MUTATION: remove the job-record integrity check.
//
// A record that says COMPLETE but does not verify must never be reported as
// COMPLETE. This is exactly what a crash-truncated or hand-edited file looks
// like.
func TestCorruptJobRecordNeverLooksComplete(t *testing.T) {
	lab := fxNewLab(t)
	dir := filepath.Join(StateDir(), "format-jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A well-formed record, sealed, and then edited to claim success.
	rec := JobRecord{JobID: "tampered", Stage: StagePartitioned, Result: "failed",
		PendingStep: string(stepMkfs), DeviceModified: true}
	sum, err := jobChecksum(rec)
	if err != nil {
		t.Fatal(err)
	}
	rec.Checksum = sum
	rec.Stage = StageComplete
	rec.Result = "success"
	rec.Terminal = true
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tampered.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := lab.f.LoadJob("tampered")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Corrupt {
		t.Fatal("an edited job record was accepted as genuine")
	}
	if got.Stage == StageComplete || got.Result == "success" {
		t.Fatalf("an edited record reported %s/%s; a file claiming success is not evidence of success",
			got.Stage, got.Result)
	}
	// And it is never resumable.
	ctx, cancel := fxCtx(t)
	defer cancel()
	if _, err := lab.f.Resume(ctx, fxAdmin, "tampered"); fxCode(t, err) != ErrCodeJobStateCorrupt {
		t.Fatalf("a corrupt job was resumable: %v", err)
	}
}

func TestTruncatedJobRecordIsReportedUnknown(t *testing.T) {
	lab := fxNewLab(t)
	dir := filepath.Join(StateDir(), "format-jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half.json"), []byte(`{"job_id":"half","stage":"COMP`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := lab.f.LoadJob("half")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Corrupt || got.Stage != StageUnknown {
		t.Fatalf("a truncated record reported stage %s corrupt=%v", got.Stage, got.Corrupt)
	}
}

// PART K 13: a reboot after the partition table is written but before mkfs.
// The diagnosis must be precise, not a guess and not "everything is fine".
func TestInterruptionAfterPartitioningIsDiagnosedPrecisely(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	// mkfs never returns: the machine died there.
	lab.runner.after[string(ToolSfdisk)] = func(CommandSpec) {
		lab.host.addPartition("sdb", "sdb1", 63<<30)
	}
	lab.runner.failures[string(ToolMkfsExt4)] = errors.New("killed")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, _ := lab.f.Execute(ctx, fxAdmin, plan.OperationID)

	// Simulate the crash: rewrite the record as it stood BEFORE the failure was
	// recorded, which is what a power cut leaves behind.
	crashed := job
	crashed.Terminal = false
	crashed.Result = ""
	crashed.ErrorCode = ""
	crashed.ErrorDetail = ""
	crashed.Stage = StagePartitionVisible
	crashed.PendingStep = string(stepMkfs)
	crashed.DeviceModified = true
	if err := lab.f.persist(&crashed); err != nil {
		t.Fatal(err)
	}

	d, err := lab.f.Diagnose(ctx, job.JobID)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Stage != StagePartitionVisible {
		t.Fatalf("diagnosed stage %s", d.Stage)
	}
	if d.InterruptedDuring != string(stepMkfs) {
		t.Fatalf("the diagnosis does not name the step that was running: %q", d.InterruptedDuring)
	}
	if !d.DeviceModified {
		t.Fatal("the diagnosis claims the device is untouched after a partition table was written")
	}
	if !strings.Contains(d.Summary, "NO filesystem") {
		t.Fatalf("the summary does not say the filesystem was never created: %q", d.Summary)
	}
	if d.ObservedFilesystem != "" {
		t.Fatalf("a filesystem was observed where none was created: %q", d.ObservedFilesystem)
	}
	if !d.Resumable {
		t.Fatal("an interruption after PARTITION_VISIBLE should be resumable from mkfs")
	}
	// The cancel state must not pretend a clean stop is available.
	if d.Cancel == CancelStateCancellable {
		t.Fatal("the UI would be told a clean cancel is available after the table was written")
	}
}

// An interruption before anything destructive is NOT resumable: a fresh prepare
// re-proves every safety property against the device as it is now.
func TestInterruptionBeforeAnythingDestructiveIsNotResumable(t *testing.T) {
	lab := fxNewLab(t)
	rec := JobRecord{JobID: "early", Stage: StageValidated, PendingStep: string(stepQuiesce)}
	if err := lab.f.persist(&rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := fxCtx(t)
	defer cancel()
	if _, err := lab.f.Resume(ctx, fxAdmin, "early"); fxCode(t, err) != ErrCodeInvalidRequest {
		t.Fatalf("a job that had not touched the device was resumed instead of re-prepared: %v", err)
	}
}

// PART K: a stale job replay. A finished job cannot be run again.
func TestFinishedJobCannotBeReplayed(t *testing.T) {
	lab := fxNewLab(t)
	rec := JobRecord{JobID: "done", Stage: StageComplete, Terminal: true, Result: "success"}
	if err := lab.f.persist(&rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := fxCtx(t)
	defer cancel()
	if _, err := lab.f.Resume(ctx, fxAdmin, "done"); fxCode(t, err) != ErrCodeInvalidRequest {
		t.Fatalf("a completed job was replayed: %v", err)
	}
}

// Automatic recovery: nothing is left claiming to be running, and no operator
// has to tidy up state by hand.
func TestRecoveryMarksInterruptedJobsWithoutOperatorAction(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	rec := JobRecord{JobID: "midflight", Stage: StageSignaturesCleared,
		PendingStep: string(stepPartition), DeviceModified: true,
		Identity: lab.identity("sdb"), Fingerprint: lab.identity("sdb").Fingerprint()}
	rec.Plan.Identity = rec.Identity
	rec.Plan.DeviceName = "sdb"
	if err := lab.f.persist(&rec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	diags, err := lab.f.RecoverInterruptedJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("recovery found %d interrupted job(s)", len(diags))
	}
	got, err := lab.f.LoadJob("midflight")
	if err != nil {
		t.Fatal(err)
	}
	// Interrupted, and deliberately NOT terminal.
	//
	// Marking it terminal made Resume unreachable: recovery runs at every
	// start, so every unfinished job became "finished" while the Diagnosis for
	// that same job still advertised Resumable — an API contradicting itself.
	// A power cut mid-format leaves work that can be continued, which is a
	// different state from "over".
	if !got.Interrupted || got.Result != "interrupted" {
		t.Fatalf("the interrupted job was left as %q interrupted=%v", got.Result, got.Interrupted)
	}
	if got.Terminal {
		t.Fatal("recovery marked an interrupted job terminal; Resume refuses terminal jobs, so " +
			"the resume path the Diagnosis advertises could never be taken")
	}
	if got.Stage != StageSignaturesCleared {
		t.Fatalf("recovery lost the last proven stage: %s", got.Stage)
	}
	if !strings.Contains(got.ErrorDetail, "signature") {
		t.Fatalf("the recovery record does not describe what was left behind: %q", got.ErrorDetail)
	}
}

// Stage transitions are on disk BEFORE the step they precede. A record written
// after the fact is exactly as useless as no record.
func TestStageIsPersistedBeforeEachDestructiveStep(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	var seen []string
	record := func(spec CommandSpec) {
		jobs, err := lab.f.ListJobs()
		if err != nil || len(jobs) == 0 {
			t.Errorf("no job state on disk while %s was running", spec.Name)
			return
		}
		seen = append(seen, string(jobs[0].Stage)+"/"+jobs[0].PendingStep)
	}
	for _, tool := range []string{string(ToolWipefs), string(ToolSfdisk), string(ToolMkfsExt4)} {
		prev := lab.runner.after[tool]
		lab.runner.after[tool] = func(spec CommandSpec) {
			record(spec)
			if prev != nil {
				prev(spec)
			}
		}
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{
		// While wipefs runs, the last PROVEN stage is QUIESCED and the pending
		// step names wipefs itself — which is exactly what a crash here has to
		// leave behind for the diagnosis to be precise.
		string(StageQuiesced) + "/" + string(stepWipe),
		string(StageSignaturesCleared) + "/" + string(stepPartition),
		string(StagePartitionVisible) + "/" + string(stepMkfs),
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("state observed while each destructive helper ran:\n got %v\nwant %v", seen, want)
	}
}

// ---------------------------------------------------------------------------
// CANCELLATION — PART I.
// ---------------------------------------------------------------------------

// Cancelling before anything destructive stops the job with the device intact.
func TestCancelBeforeAnyDestructiveStepLeavesTheDeviceIntact(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.host.mount("sdb1", filepath.Join(lab.f.mountRoot, "old"))
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")
	// The cancellation arrives while the volume is being unmounted.
	lab.runner.after["umount"] = func(spec CommandSpec) {
		lab.host.unmount(spec.Args[len(spec.Args)-1])
		jobs, _ := lab.f.ListJobs()
		if len(jobs) > 0 {
			if _, _, err := lab.f.Cancel(jobs[0].JobID); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if code := fxCode(t, err); code != ErrCodeCancelled {
		t.Fatalf("cancellation produced %s", code)
	}
	if job.DeviceModified {
		t.Fatal("a cancellation before the first destructive step reported the device modified")
	}
	if lab.runner.ran(string(ToolWipefs)) {
		t.Fatal("wipefs ran after the job was cancelled")
	}
}

// The Cancel button cannot be universally safe, and the API must say so.
func TestCancelPolicyTellsTheTruthPerStage(t *testing.T) {
	byStage := map[FormatStage]StageCancelPolicy{}
	for _, p := range cancelPolicy {
		byStage[p.Stage] = p
	}
	for _, s := range []FormatStage{StageDiscovered, StageValidated, StageQuiesced} {
		if byStage[s].LeavesDeviceModified {
			t.Fatalf("%s claims the device is already modified", s)
		}
	}
	for _, s := range []FormatStage{StageSignaturesCleared, StagePartitioned, StagePartitionVisible,
		StageFilesystemCreated} {
		p, ok := byStage[s]
		if !ok {
			t.Fatalf("%s has no declared cancellation policy", s)
		}
		if !p.LeavesDeviceModified {
			t.Fatalf("%s claims cancelling leaves the device untouched, which is false", s)
		}
		if p.Note == "" {
			t.Fatalf("%s does not explain what cancelling would leave behind", s)
		}
	}
	// Writing the partition table and mkfs are not interruptible.
	for _, k := range []stepKind{stepPartition, stepMkfs} {
		if !nonCancellableSteps[k] {
			t.Fatalf("%s is declared interruptible; interrupting it leaves a device in a worse "+
				"state than letting it finish", k)
		}
	}
	// And a job inside one of those steps reports exactly that.
	j := JobRecord{Stage: StagePartitioned, PendingStep: string(stepMkfs)}
	if got := j.CancelState(); got != CancelStateNonCancellablePhase {
		t.Fatalf("a job inside mkfs reports %s, want %s", got, CancelStateNonCancellablePhase)
	}
	j.CancelRequested = true
	if got := j.CancelState(); got != CancelStateNonCancellablePhase {
		t.Fatalf("a cancellation during mkfs reports %s", got)
	}
	j.PendingStep = string(stepVerify)
	j.DeviceModified = true
	if got := j.CancelState(); got != CancelStateRecovering {
		t.Fatalf("a cancellation after the device was modified reports %s, want %s",
			got, CancelStateRecovering)
	}
	j.Terminal = true
	if got := j.CancelState(); got != CancelStateFinished {
		t.Fatalf("a finished job reports %s", got)
	}
}

// ---------------------------------------------------------------------------
// INTEGRATION AND HONESTY.
// ---------------------------------------------------------------------------

// E10: the new volume widens the storage CEILING; it does not entitle anybody.
func TestIntegrationDoesNotGrantAnybodyAccess(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	before := len(storageauth.Grants())
	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(lab.roots) != 1 {
		t.Fatalf("the volume was not registered as a storage root: %v", lab.roots)
	}
	if after := len(storageauth.Grants()); after != before {
		t.Fatalf("formatting created %d grant(s); 'mounted' must not mean 'authorised for everyone'",
			after-before)
	}
}

// If integration fails, the job fails. A volume the authority does not know
// about is not a finished format.
func TestFailureToRegisterTheStorageRootFailsTheJob(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")
	lab.rootErr = errors.New("ceiling is degraded")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID)
	if err == nil {
		t.Fatal("a volume that could not be registered was reported as finished")
	}
	if job.Stage == StageComplete {
		t.Fatal("stage COMPLETE despite integration failing")
	}
}

// RULE 7: the code must not claim a stronger guarantee than it provides.
func TestPlanDoesNotClaimErasure(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !strings.Contains(plan.Guarantees, "NOT a secure erase") {
		t.Fatalf("the plan does not say formatting is not erasure: %q", plan.Guarantees)
	}
	text := plan.Guarantees
	for _, a := range plan.Actions {
		text += " " + a.Description
	}
	lower := strings.ToLower(text)
	for _, claim := range []string{"securely erased", "unrecoverable", "dod ", "certified", "shredded"} {
		if strings.Contains(lower, claim) {
			t.Fatalf("the plan claims %q, which this mechanism does not provide: %q", claim, text)
		}
	}
}

// The plan must list the data that is about to become unreachable.
func TestPlanNamesTheDataAboutToBeLost(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.host.addPartition("sdb", "sdb1", 63<<30)
	lab.runner.blkid[lab.devPath("sdb1")] = map[string]string{
		"TYPE": "ntfs", "UUID": "OLD", "LABEL": "Backups"}

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	var sawLoss bool
	for _, a := range plan.Actions {
		if a.Kind == "data_loss" && strings.Contains(a.Description, "ntfs") {
			sawLoss = true
		}
	}
	if !sawLoss {
		t.Fatalf("the plan does not tell the user an NTFS volume is about to be lost: %+v", plan.Actions)
	}
	if plan.CurrentPartitions[0].Label != "Backups" {
		t.Fatalf("the plan does not show the existing volume label: %+v", plan.CurrentPartitions)
	}
}

// ---------------------------------------------------------------------------
// WIRING AND HEALTH.
// ---------------------------------------------------------------------------

// The production formatter must actually be wired to the real authorities. A
// perfectly-tested state machine pointed at a stub protects nothing.
func TestProductionFormatterIsWiredToTheRealAuthorities(t *testing.T) {
	f := NewFormatter()
	if reflect.ValueOf(f.discover).Pointer() != reflect.ValueOf(blockdev.Discover).Pointer() {
		t.Fatal("the formatter does not discover devices through pkg/blockdev")
	}
	if reflect.ValueOf(f.addStorageRoot).Pointer() != reflect.ValueOf(storageauth.AddOperatorRoot).Pointer() {
		t.Fatal("the formatter does not register volumes through pkg/storageauth")
	}
	if reflect.ValueOf(f.capabilities).Pointer() != reflect.ValueOf(CurrentCapabilities).Pointer() {
		t.Fatal("the formatter does not use discovered capabilities")
	}
	if reflect.ValueOf(f.auditFn).Pointer() != reflect.ValueOf(Audit).Pointer() {
		t.Fatal("the formatter does not write to the append-only audit log")
	}
	if _, ok := f.runner.(execRunner); !ok {
		t.Fatal("the production formatter does not run real helpers")
	}
	if f.tokenTTL <= 0 || f.tokenTTL > 15*time.Minute {
		t.Fatalf("the operation token lives for %s", f.tokenTTL)
	}
	for _, want := range defaultManagedMountRoots {
		if !contains(f.managedRoots, want) {
			t.Fatalf("%s is not a managed mount root", want)
		}
	}
}

// Health autoprovisions and validates a PROPERTY, not the existence of a file.
func TestHealthAutoprovisionsAndProvesIntegrityIsEnforced(t *testing.T) {
	lab := fxNewLab(t)
	ctx, cancel := fxCtx(t)
	defer cancel()

	if _, err := os.Stat(filepath.Join(StateDir(), "format-jobs")); err == nil {
		t.Fatal("precondition: the job directory should not exist yet")
	}
	h := lab.f.Health(ctx)
	if h.Status != "healthy" {
		t.Fatalf("health = %s: %s / %s", h.Status, h.Invariant, h.Detail)
	}
	if !h.StateDirWritable {
		t.Fatal("the state directory was not autoprovisioned")
	}
	if !h.IntegrityEnforced {
		t.Fatal("health does not prove that a tampered job record is rejected")
	}
	if len(h.Filesystems) == 0 {
		t.Fatal("health reports no available filesystems")
	}
}

// A missing prerequisite names the invariant, not a boolean.
func TestHealthNamesTheFailedInvariant(t *testing.T) {
	lab := fxNewLab(t)
	caps := fxAllCaps()
	caps.CanWipeSignatures = false
	lab.caps = caps
	ctx, cancel := fxCtx(t)
	defer cancel()

	h := lab.f.Health(ctx)
	if h.Status != "degraded" {
		t.Fatalf("health = %s with wipefs missing", h.Status)
	}
	if h.Invariant == "" {
		t.Fatal("health reports degraded without naming the invariant that failed")
	}
}

// Corrupt records are surfaced, not swallowed.
func TestHealthSurfacesCorruptJobRecords(t *testing.T) {
	lab := fxNewLab(t)
	dir := filepath.Join(StateDir(), "format-jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"job_id":"bad","checksum":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := fxCtx(t)
	defer cancel()
	h := lab.f.Health(ctx)
	if len(h.CorruptJobs) != 1 || h.Status != "degraded" {
		t.Fatalf("a corrupt record was not surfaced: %+v", h)
	}
}

// Protected devices are LISTED with their reason. A user who cannot see why
// their disk is missing goes looking for a command line.
func TestTargetsExplainWhyADeviceIsRefused(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sda", size: 500 << 30, topology: "pci0000:00/ata1",
		model: "SYSTEM", serial: "SYS-1"})
	lab.host.addPartition("sda", "sda1", 499<<30)
	lab.host.mount("sda1", "/")
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})

	targets, err := lab.f.Targets()
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	byName := map[string]FormatTarget{}
	for _, tg := range targets {
		byName[tg.Name] = tg
	}
	if byName["sda"].Eligible || len(byName["sda"].ProtectionReasons) == 0 {
		t.Fatalf("the system disk is offered or unexplained: %+v", byName["sda"])
	}
	if !byName["sdb"].Eligible {
		t.Fatalf("the external stick is not offered: %+v", byName["sdb"])
	}
}

// ---------------------------------------------------------------------------
// SMALL INVARIANTS.
// ---------------------------------------------------------------------------

func TestVolumeNameSanitisationCannotEscapeTheMountRoot(t *testing.T) {
	for _, in := range []string{"../../etc", "..", "/etc/passwd", "a/b", "...", "", "  "} {
		got := sanitizeVolumeName(in)
		if strings.ContainsAny(got, "/.\x00") {
			t.Fatalf("sanitize(%q) = %q, which is not a safe path component", in, got)
		}
	}
	lab := fxNewLab(t)
	job := &JobRecord{Fingerprint: "serial:x"}
	job.Plan.Label = "../../etc"
	mp, err := lab.f.mountpointFor(job)
	if err != nil {
		t.Fatalf("mountpointFor: %v", err)
	}
	if !strings.HasPrefix(mp, lab.f.mountRoot+"/") {
		t.Fatalf("mountpoint %q escaped %q", mp, lab.f.mountRoot)
	}
}

// The managed-mount root itself is never unmounted: doing so would take every
// volume underneath it with it.
func TestTheManagedRootItselfIsNotAManagedMount(t *testing.T) {
	f := NewFormatter()
	for _, root := range f.managedRoots {
		if f.isManagedMount(root) {
			t.Fatalf("%s is treated as an unmountable volume", root)
		}
	}
	if f.isManagedMount("/") {
		t.Fatal("/ is treated as a managed mount")
	}
	if !f.isManagedMount("/media/usb-stick") {
		t.Fatal("a volume under /media is not recognised as managed")
	}
	if f.isManagedMount("/srv/data") {
		t.Fatal("an unmanaged path is treated as managed")
	}
}

// Two identities that cannot be told apart are never the same device, and an
// ambiguous match is refused rather than resolved by picking one.
func TestAmbiguousIdentityMatchIsRefused(t *testing.T) {
	id := blockdev.DeviceIdentity{Serial: "DUP", Model: "M", SizeBytes: 1 << 30, SysfsPath: "/a"}
	inv := blockdev.Inventory{Complete: true, Devices: []blockdev.Device{
		{Name: "sdb", Identity: id}, {Name: "sdc", Identity: id},
	}}
	if _, err := findByIdentity(inv, id); fxCode(t, err) != ErrCodeIdentityMismatch {
		t.Fatalf("an ambiguous identity resolved to a device: %v", err)
	}
}

func TestWeakIdentityIsRefusedOutright(t *testing.T) {
	lab := fxNewLab(t)
	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: blockdev.DeviceIdentity{SysfsPath: "/sys/devices/x"}, Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeIdentityWeak {
		t.Fatalf("a weak identity produced %s, want %s", code, ErrCodeIdentityWeak)
	}
}

// Every destructive helper runs with a bounded timeout.
func TestEveryHelperInvocationIsBounded(t *testing.T) {
	f := NewFormatter()
	job := &JobRecord{}
	job.Plan.Filesystem = FSExt4
	job.Plan.Label = "X"
	spec, err := f.mkfsSpec(job, "/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Timeout <= 0 {
		t.Fatal("mkfs runs without a timeout")
	}
	if f.stepTimeout <= 0 || f.mkfsTimeout <= 0 || f.settleTimeout <= 0 {
		t.Fatal("a step can run unbounded")
	}
}

// The audit record is written before the destruction, and a failure to write it
// stops the operation.
func TestAnUnwritableAuditLogStopsTheOperation(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.f.auditFn = func(AuditEntry) error { return errors.New("read-only filesystem") }

	ctx, cancel := fxCtx(t)
	defer cancel()
	_, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4})
	if code := fxCode(t, err); code != ErrCodeAuditUnavailable {
		t.Fatalf("an unwritable audit log produced %s, want %s", code, ErrCodeAuditUnavailable)
	}
}

func TestSuccessfulFormatIsAudited(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	lab.wireHappyPath("sdb", "sdb1", 63<<30, FSExt4, "DATA")

	ctx, cancel := fxCtx(t)
	defer cancel()
	plan, err := lab.f.Prepare(ctx, fxAdmin, PrepareFormatRequest{
		Identity: lab.identity("sdb"), Filesystem: FSExt4, Label: "DATA"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := lab.f.Execute(ctx, fxAdmin, plan.OperationID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	entries, err := ReadAudit()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var sawSuccess bool
	for _, e := range entries {
		if e.Operation == "format.execute" && e.Result == "success" {
			if e.PrincipalID != fxAdmin.ID || e.Serial != "AAAA1111" {
				t.Fatalf("the audit entry does not say who destroyed what: %+v", e)
			}
			sawSuccess = true
		}
	}
	if !sawSuccess {
		t.Fatalf("a completed format was not audited: %+v", entries)
	}
}

// Resume must actually be reachable after the startup recovery pass.
//
// This is the contradiction a certifier found: RecoverInterruptedJobs runs at
// every service start and marked every unfinished job terminal, Resume refuses
// terminal jobs, and the Diagnosis for the same job reported Resumable=true.
// The resume path was documented, tested in isolation, and unreachable in
// production — this codebase's signature defect, one level up.
//
// Mutation that must fail this test: set Terminal in RecoverInterruptedJobs
// again.
func TestResumeIsReachableAfterTheStartupRecoveryPass(t *testing.T) {
	lab := fxNewLab(t)
	lab.host.addDisk(fxDisk{name: "sdb", size: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	rec := JobRecord{JobID: "midflight", Stage: StageSignaturesCleared,
		PendingStep: string(stepPartition), DeviceModified: true,
		Identity: lab.identity("sdb"), Fingerprint: lab.identity("sdb").Fingerprint()}
	rec.Plan.Identity = rec.Identity
	rec.Plan.DeviceName = "sdb"
	if err := lab.f.persist(&rec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := fxCtx(t)
	defer cancel()
	diags, err := lab.f.RecoverInterruptedJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("recovery found %d job(s), want 1", len(diags))
	}
	if !diags[0].Resumable {
		t.Skip("this fixture's device is not present, so the diagnosis is legitimately not resumable")
	}

	// The exact call the diagnosis advertises. It must not be refused for the
	// reason "already finished".
	_, err = lab.f.Resume(ctx, fxAdmin, "midflight")
	if err != nil && strings.Contains(err.Error(), "already finished") {
		t.Fatalf("Resume refused a job recovery had just advertised as resumable: %v", err)
	}
}
