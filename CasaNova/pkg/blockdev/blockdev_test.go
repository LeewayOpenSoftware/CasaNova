package blockdev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic sysfs/proc tree. Discovery reads the kernel's own attributes, so
// a fixture tree exercises the real code paths without hardware and without
// root — which is what lets the protection classifier be tested at all.
type fakeHost struct {
	t    *testing.T
	root string
	sys  string
	proc string
	dev  string

	mounts [][2]string
	swaps  []string
}

func newHost(t *testing.T) *fakeHost {
	t.Helper()
	root := t.TempDir()
	h := &fakeHost{
		t: t, root: root,
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
	SetRoots(h.sys, h.proc, h.dev)
	// This package OWNS SetProtectedMountPrefixes and its doc comment names
	// this exact hazard: root is t.TempDir(), which resolves under $TMPDIR,
	// and if $TMPDIR happens to sit under a default prefix (e.g. "/home"),
	// every fixture mountpoint nested under root would falsely match that
	// prefix no matter what the test actually mounts there. Same form as
	// storagemaint's fxNewHost (format_test.go), which needed this for the
	// same reason.
	SetProtectedMountPrefixes(protectedMountPrefixesExcluding(root))
	t.Cleanup(func() {
		SetRoots("/sys", "/proc", "/dev")
		SetProtectedMountPrefixes(DefaultProtectedMountPrefixes)
	})
	return h
}

// protectedMountPrefixesExcluding returns the default protected-mount list
// with any prefix that is an ancestor of (or equal to) base removed. Fixtures
// mount their fake partitions under real, test-chosen paths like "/srv/db" or
// "/var/lib/casanova" ON PURPOSE, to prove the guard refuses them — that must
// keep working regardless of where $TMPDIR resolves. But base itself is
// real too (t.TempDir(), needed for real file I/O), and it resolves under
// $TMPDIR: if $TMPDIR happens to sit under one of the default prefixes (e.g.
// "/home"), every fixture mountpoint nested under base would falsely match
// that prefix no matter what the test actually mounts there. Removing only
// the prefixes that are ancestors of base leaves every other prefix (and any
// literal, non-base-derived mountpoint a test uses) fully active.
func protectedMountPrefixesExcluding(base string) []string {
	var out []string
	for _, p := range DefaultProtectedMountPrefixes {
		if base == p {
			continue
		}
		if p != "/" && strings.HasPrefix(base, strings.TrimSuffix(p, "/")+"/") {
			continue
		}
		out = append(out, p)
	}
	return out
}

type diskSpec struct {
	name       string
	sizeBytes  uint64
	topology   string // the bus path the device hangs off
	model      string
	serial     string
	wwn        string
	removable  bool
	rotational bool
	readOnly   bool
	discard    bool
	holders    []string
}

func (h *fakeHost) addDisk(s diskSpec) {
	h.t.Helper()
	// The real sysfs entry lives under devices/ and /sys/block/<n> is a
	// symlink to it. Discovery resolves that link to learn the transport, so
	// the fixture has to have the same shape.
	realDir := filepath.Join(h.sys, "devices", s.topology, s.name)
	if err := os.MkdirAll(filepath.Join(realDir, "device"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realDir, "queue"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	write := func(rel, val string) {
		if err := os.WriteFile(filepath.Join(realDir, rel), []byte(val+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	write("size", itoa(s.sizeBytes/512))
	write("dev", "8:0")
	write("removable", boolStr(s.removable))
	write("ro", boolStr(s.readOnly))
	write("queue/rotational", boolStr(s.rotational))
	if s.discard {
		write("queue/discard_max_bytes", "2147483648")
	} else {
		write("queue/discard_max_bytes", "0")
	}
	if s.model != "" {
		write("device/model", s.model)
	}
	if s.serial != "" {
		write("device/serial", s.serial)
	}
	if s.wwn != "" {
		write("wwid", s.wwn)
	}
	if len(s.holders) > 0 {
		if err := os.MkdirAll(filepath.Join(realDir, "holders"), 0o755); err != nil {
			h.t.Fatal(err)
		}
		for _, hold := range s.holders {
			if err := os.MkdirAll(filepath.Join(realDir, "holders", hold), 0o755); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(realDir, filepath.Join(h.sys, "block", s.name)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) addPartition(disk, part string, sizeBytes uint64) {
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
		"partition": "1", "size": itoa(sizeBytes / 512), "dev": "8:1", "ro": "0",
	} {
		if err := os.WriteFile(filepath.Join(pdir, rel), []byte(val+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *fakeHost) mount(devName, mountpoint string) {
	h.t.Helper()
	h.mounts = append(h.mounts, [2]string{devName, mountpoint})
	h.writeMounts()
}

func (h *fakeHost) swapOn(devName string) {
	h.t.Helper()
	h.swaps = append(h.swaps, devName)
	h.writeSwaps()
}

func (h *fakeHost) writeMounts() {
	var b strings.Builder
	for i, m := range h.mounts {
		// mountinfo: id parent maj:min root MOUNTPOINT opts - FSTYPE SOURCE super
		b.WriteString(itoa(uint64(i+20)) + " 1 8:1 / " + m[1] +
			" rw,relatime shared:1 - ext4 /dev/" + m[0] + " rw\n")
	}
	if err := os.WriteFile(filepath.Join(h.proc, "self", "mountinfo"), []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) writeSwaps() {
	b := "Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n"
	for _, s := range h.swaps {
		b += "/dev/" + s + "\tpartition\t1000\t0\t-2\n"
	}
	if err := os.WriteFile(filepath.Join(h.proc, "swaps"), []byte(b), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) discover() Inventory {
	h.t.Helper()
	inv, err := Discover()
	if err != nil {
		h.t.Fatalf("Discover: %v", err)
	}
	return inv
}

func itoa(v uint64) string {
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

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------------------------------------------------------------------------
// PROTECTION — the invariant that stops the appliance wiping itself.
// ---------------------------------------------------------------------------

// The finding this whole classifier exists for: /dev/sda1 must not be offered
// merely because /dev/sda2 is the actual root partition. Wiping the parent's
// partition table destroys root just as thoroughly.
//
// Mutation that must fail this test: remove the upward propagation pass.
func TestProtectionPropagatesFromPartitionToWholeDiskAndBack(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sda", sizeBytes: 500 << 30, topology: "pci0000:00/ata1",
		model: "SYSTEM-SSD", serial: "SYS123", rotational: false})
	h.addPartition("sda", "sda1", 512<<20)
	h.addPartition("sda", "sda2", 499<<30)
	h.mount("sda2", "/")

	inv := h.discover()

	root, _ := inv.ByName("sda2")
	if !root.Protected || !hasReason(root, ReasonRootFilesystem) {
		t.Fatalf("the root partition is not protected: %+v", root.ProtectionReasons)
	}
	disk, _ := inv.ByName("sda")
	if !disk.Protected || !hasReason(disk, ReasonChildProtected) {
		t.Fatalf("the disk CONTAINING root is not protected: %+v", disk.ProtectionReasons)
	}
	sibling, _ := inv.ByName("sda1")
	if !sibling.Protected || !hasReason(sibling, ReasonParentProtected) {
		t.Fatalf("a sibling partition on the system disk is not protected: %+v; "+
			"formatting it destroys the same physical disk", sibling.ProtectionReasons)
	}
	for _, n := range []string{"sda", "sda1", "sda2"} {
		d, _ := inv.ByName(n)
		if d.Eligible() {
			t.Fatalf("%s is eligible for a destructive operation", n)
		}
	}
}

func TestBootAndCasaNovaStateAreProtected(t *testing.T) {
	for mountpoint, want := range map[string]ProtectionReason{
		"/boot":             ReasonBootFilesystem,
		"/boot/efi":         ReasonBootFilesystem,
		"/var/lib/casanova": ReasonCasaNovaState,
		"/var":              ReasonMountedSystem,
		"/home":             ReasonMountedSystem,
	} {
		t.Run(mountpoint, func(t *testing.T) {
			h := newHost(t)
			h.addDisk(diskSpec{name: "sdx", sizeBytes: 64 << 30, topology: "pci0000:00/usb1",
				model: "USB", serial: "U1", removable: true})
			h.addPartition("sdx", "sdx1", 64<<30)
			h.mount("sdx1", mountpoint)

			inv := h.discover()
			p, _ := inv.ByName("sdx1")
			if !p.Protected {
				t.Fatalf("%s is not protected", mountpoint)
			}
			if !hasReason(p, want) {
				t.Fatalf("%s protected for %v, want %s", mountpoint, p.ProtectionReasons, want)
			}
			if d, _ := inv.ByName("sdx"); !d.Protected {
				t.Fatalf("the disk holding %s is not protected", mountpoint)
			}
		})
	}
}

func TestActiveSwapIsProtected(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdc", sizeBytes: 32 << 30, topology: "pci0000:00/usb1",
		model: "STICK", serial: "S9", removable: true})
	h.addPartition("sdc", "sdc1", 32<<30)
	h.swapOn("sdc1")

	inv := h.discover()
	p, _ := inv.ByName("sdc1")
	if !p.Protected || !hasReason(p, ReasonActiveSwap) {
		t.Fatalf("active swap is not protected: %+v", p.ProtectionReasons)
	}
	if d, _ := inv.ByName("sdc"); !d.Protected {
		t.Fatal("the disk backing active swap is not protected")
	}
}

// A device consumed by LVM, mdraid or dm-crypt has holders. Discovery cannot
// see through the mapping to know what is layered on top, so holders alone
// disqualify it.
func TestDeviceWithHoldersIsProtected(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdd", sizeBytes: 100 << 30, topology: "pci0000:00/usb1",
		model: "DISK", serial: "D1", removable: true, holders: []string{"dm-0"}})

	inv := h.discover()
	d, _ := inv.ByName("sdd")
	if !d.Protected || !hasReason(d, ReasonHasHolders) {
		t.Fatalf("a device with holders is not protected: %+v", d.ProtectionReasons)
	}
}

// FALSE NEGATIVES BEAT WRONG DISKS: anything unidentifiable is refused.
func TestUnidentifiableDeviceIsRefused(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sde", sizeBytes: 0, topology: "pci0000:00/usb1", removable: true})

	inv := h.discover()
	d, _ := inv.ByName("sde")
	if !d.Protected || !hasReason(d, ReasonUnidentifiable) {
		t.Fatalf("a device with no identifying facts was not refused: %+v", d.ProtectionReasons)
	}
	if d.Identity.Strength() != StrengthWeak {
		t.Fatalf("identity strength = %s, want weak", d.Identity.Strength())
	}
}

func TestReadOnlyDeviceIsRefused(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sr0", sizeBytes: 700 << 20, topology: "pci0000:00/usb1",
		model: "CDROM", serial: "C1", removable: true, readOnly: true})

	inv := h.discover()
	d, _ := inv.ByName("sr0")
	if !d.Protected || !hasReason(d, ReasonReadOnly) {
		t.Fatalf("a read-only device was not refused: %+v", d.ProtectionReasons)
	}
}

// ---------------------------------------------------------------------------
// ELIGIBILITY
// ---------------------------------------------------------------------------

// A USB enclosure reporting removable=0 — the common case for USB-to-SATA and
// USB-to-NVMe bridges — must still be recognised as external, or the formatter
// refuses every external SSD people actually own.
func TestUsbEnclosureReportingNonRemovableIsStillExternal(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdf", sizeBytes: 1 << 40, topology: "pci0000:00/usb2/2-1",
		model: "Portable SSD T7", serial: "S4B7NX0", removable: false})

	inv := h.discover()
	d, _ := inv.ByName("sdf")
	if d.Transport != TransportUSB {
		t.Fatalf("transport = %s, want usb (derived from topology, not the removable flag)", d.Transport)
	}
	if !d.External() {
		t.Fatal("a USB-attached disk reporting removable=0 was not treated as external")
	}
	if !d.Eligible() {
		t.Fatalf("an unprotected external USB disk is not eligible: %+v", d.ProtectionReasons)
	}
}

// Internal disks do not silently become format targets.
func TestInternalDiskIsNotEligibleEvenWhenUnprotected(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdg", sizeBytes: 2 << 40, topology: "pci0000:00/ata2",
		model: "SPARE-HDD", serial: "SP1", rotational: true})

	inv := h.discover()
	d, _ := inv.ByName("sdg")
	if d.Protected {
		t.Fatalf("precondition: this spare disk should be unprotected, got %+v", d.ProtectionReasons)
	}
	if d.Eligible() {
		t.Fatal("an internal SATA disk became a format target without an explicit advanced authority")
	}
}

// ---------------------------------------------------------------------------
// IDENTITY — the replug attack from PART K.
// ---------------------------------------------------------------------------

// PART K scenario 1-4: prepare on USB A, unplug it, plug USB B which takes the
// same /dev/sdX, execute the old operation. It must not match.
func TestReplugUnderTheSameKernelNameIsNotTheSameDevice(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdb", sizeBytes: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	first, _ := h.discover().ByName("sdb")

	// Same slot, same kernel name, different hardware.
	h2 := newHost(t)
	h2.addDisk(diskSpec{name: "sdb", sizeBytes: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "BBBB2222", removable: true})
	second, _ := h2.discover().ByName("sdb")

	if first.Name != second.Name {
		t.Fatal("precondition: both should occupy the same kernel name")
	}
	if first.Identity.Equal(second.Identity) {
		t.Fatal("a replugged DIFFERENT device matched the prepared identity; an operation " +
			"prepared against the first would execute against the second")
	}
	if first.Identity.Fingerprint() == second.Identity.Fingerprint() {
		t.Fatal("two different devices share a fingerprint; the job lock key would collide")
	}
}

// The same physical device moved to another port must still match, or every
// replug would invalidate a legitimate prepared operation.
func TestSameDeviceOnAnotherPortStillMatches(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdb", sizeBytes: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	first, _ := h.discover().ByName("sdb")

	h2 := newHost(t)
	h2.addDisk(diskSpec{name: "sdd", sizeBytes: 64 << 30, topology: "pci0000:00/usb3/3-2",
		model: "Cruzer", serial: "AAAA1111", removable: true})
	moved, _ := h2.discover().ByName("sdd")

	if !first.Identity.Equal(moved.Identity) {
		t.Fatal("the same device on another port did not match its own prepared identity")
	}
}

// Two unidentifiable devices are never "the same one".
func TestWeakIdentitiesNeverCompareEqual(t *testing.T) {
	a := DeviceIdentity{SysfsPath: "/sys/devices/x"}
	b := DeviceIdentity{SysfsPath: "/sys/devices/x"}
	if a.Equal(b) {
		t.Fatal("two weak identities compared equal; an operation could bind to nothing in particular")
	}
}

// ---------------------------------------------------------------------------
// FAIL-CLOSED DISCOVERY
// ---------------------------------------------------------------------------

// A device whose attributes cannot be read is protected, and the inventory
// says it is incomplete. "I could not tell" is never "safe to destroy".
func TestUnreadableDeviceIsProtectedAndReported(t *testing.T) {
	h := newHost(t)
	// A block entry with no attributes at all.
	if err := os.MkdirAll(filepath.Join(h.sys, "devices", "broken", "sdz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.sys, "devices", "broken", "sdz"),
		filepath.Join(h.sys, "block", "sdz")); err != nil {
		t.Fatal(err)
	}

	inv := h.discover()
	d, ok := inv.ByName("sdz")
	if !ok {
		t.Fatal("the unreadable device vanished from the inventory instead of being reported")
	}
	if !d.Protected {
		t.Fatal("a device whose attributes could not be read was not protected")
	}
	if inv.Complete {
		t.Fatal("the inventory claims completeness while a device could not be read")
	}
	if len(inv.Incomplete) == 0 {
		t.Fatal("the inventory does not say WHAT it could not read")
	}
}

func hasReason(d Device, r ProtectionReason) bool {
	for _, x := range d.ProtectionReasons {
		if x == r {
			return true
		}
	}
	return false
}

// TestUnreadableDeviceIsProtectedAndReported passes even with the fail-closed
// branch removed, because a device with no attributes at all is ALSO
// unidentifiable and gets protected for that instead — the right outcome for
// the wrong reason. Verified by mutation.
//
// This is the case the fail-closed branch alone covers: every identifying fact
// is present and strong, and only a required attribute is unreadable. "I could
// not finish reading this device" must protect it on its own.
//
// Mutation that must fail this test: drop `d.Protected = true` beside
// ReasonDiscoveryFailed.
func TestPartiallyUnreadableButIdentifiableDeviceIsStillProtected(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdh", sizeBytes: 256 << 30, topology: "pci0000:00/usb1/1-4",
		model: "Extreme Pro", serial: "SN-STRONG-1", removable: true})

	// Make one required attribute unreadable, leaving identity intact.
	real, err := filepath.EvalSymlinks(filepath.Join(h.sys, "block", "sdh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(real, "dev")); err != nil {
		t.Fatal(err)
	}

	inv := h.discover()
	d, ok := inv.ByName("sdh")
	if !ok {
		t.Fatal("the device vanished from the inventory")
	}
	if d.Identity.Strength() != StrengthStrong {
		t.Fatalf("precondition: identity should still be strong, got %s", d.Identity.Strength())
	}
	if !d.Protected {
		t.Fatal("a device whose discovery did not complete was left ELIGIBLE; " +
			"'I could not finish reading it' must never mean 'safe to destroy'")
	}
	if !hasReason(d, ReasonDiscoveryFailed) {
		t.Fatalf("protected, but not for the incomplete discovery: %+v", d.ProtectionReasons)
	}
	if d.Eligible() {
		t.Fatal("a partially-read device is a format target")
	}
}

// addPartUUID registers a udev by-partuuid symlink for a partition.
func (h *fakeHost) addPartUUID(part, uuid string) {
	h.t.Helper()
	dir := filepath.Join(h.dev, "disk", "by-partuuid")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink("../../"+part, filepath.Join(dir, uuid)); err != nil {
		h.t.Fatal(err)
	}
}

// THE CRITICAL a certifier proved: a physically different drive swapped into
// the same port between prepare and execute WAS accepted as the same device,
// because the only discriminator the moderate branch had — PartitionUUIDs —
// was declared, compared, and never assigned. Every real device compared
// equalStrings(nil, nil) == true.
//
// This is the codebase's signature defect (a field that exists and is never
// wired) landing in the one place where being wrong means formatting somebody
// else's drive.
//
// Mutation that must fail this test: stop assigning PartitionUUIDs in Discover.
func TestTwoIdenticalSticksWithDifferentDataDoNotMatch(t *testing.T) {
	mk := func(t *testing.T, uuid string) DeviceIdentity {
		h := newHost(t)
		// No serial and no WWN: the norm for cheap USB sticks and card readers,
		// which is exactly the population this has to distinguish.
		h.addDisk(diskSpec{name: "sdb", sizeBytes: 32 << 30, topology: "pci0000:00/usb1/1-1",
			model: "Ultra Fit", removable: true})
		h.addPartition("sdb", "sdb1", 32<<30)
		h.addPartUUID("sdb1", uuid)
		d, ok := h.discover().ByName("sdb")
		if !ok {
			t.Fatal("device vanished")
		}
		return d.Identity
	}

	family := mk(t, "1111aaaa-0000-0000-0000-000000000001")
	work := mk(t, "2222bbbb-0000-0000-0000-000000000002")

	if family.Strength() != StrengthModerate {
		t.Fatalf("strength = %s, want moderate — the partition table should be the discriminator",
			family.Strength())
	}
	if family.Equal(work) {
		t.Fatal("two same-model same-size sticks holding DIFFERENT data compared equal; " +
			"a format prepared against one would execute against the other")
	}
	if !family.Equal(family) {
		t.Fatal("a device stopped matching itself")
	}
}

// Without a discriminator the honest answer is "I cannot tell", and every
// destructive operation must refuse — not fall back to model-and-size, which
// matches any identical stick.
func TestNoSerialAndNoPartitionTableIsWeakAndRefused(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdc", sizeBytes: 32 << 30, topology: "pci0000:00/usb1/1-2",
		model: "Ultra Fit", removable: true})

	d, _ := h.discover().ByName("sdc")
	if d.Identity.Strength() != StrengthWeak {
		t.Fatalf("strength = %s, want weak: no serial, no WWN and no partition table leaves "+
			"nothing that distinguishes this from any identical stick", d.Identity.Strength())
	}
	if d.Eligible() {
		t.Fatal("an indistinguishable device is a destructive-operation target")
	}
}

// A serial still wins: a device that identifies itself does not need the table.
func TestSerialStillOutranksThePartitionTable(t *testing.T) {
	h := newHost(t)
	h.addDisk(diskSpec{name: "sdd", sizeBytes: 1 << 40, topology: "pci0000:00/usb2/2-1",
		model: "Portable SSD T7", serial: "S4B7NX0", removable: true})

	d, _ := h.discover().ByName("sdd")
	if d.Identity.Strength() != StrengthStrong {
		t.Fatalf("strength = %s, want strong", d.Identity.Strength())
	}
	if !d.Eligible() {
		t.Fatalf("a strongly-identified external disk is not eligible: %+v", d.ProtectionReasons)
	}
}

// The protection list became injectable (SetProtectedMountPrefixes) so a
// fixture's own real t.TempDir() backing directory can never collide with a
// real production safety path merely by coincidence of where $TMPDIR
// resolves. Injectable means a caller CAN clear or shrink it — including,
// by mistake, in production. This pins the built-in default so that ever
// shipping with an empty or gutted list is a compile-time-visible test
// failure, not a silent hole in the guard that decides what a destructive
// operation may touch.
func TestDefaultProtectedMountPrefixesCoverTheCriticalPaths(t *testing.T) {
	if len(DefaultProtectedMountPrefixes) == 0 {
		t.Fatal("DefaultProtectedMountPrefixes is empty — nothing would ever be refused as a mounted system path")
	}
	want := []string{
		"/", "/boot", "/usr", "/var", "/etc", "/home",
		"/var/lib/casanova", "/var/lib/docker",
	}
	have := map[string]bool{}
	for _, p := range DefaultProtectedMountPrefixes {
		have[p] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("DefaultProtectedMountPrefixes is missing %q — a destructive operation targeting it would not be refused", w)
		}
	}
}

// SetProtectedMountPrefixes must actually change what isProtectedMount does,
// or the fixtures that rely on it to avoid the $TMPDIR collision are silently
// still exposed to it.
func TestSetProtectedMountPrefixesChangesClassification(t *testing.T) {
	t.Cleanup(func() { SetProtectedMountPrefixes(DefaultProtectedMountPrefixes) })

	SetProtectedMountPrefixes([]string{"/only/this"})
	if isProtectedMount("/home") {
		t.Fatal("/home was classified protected after the list was overridden to exclude it")
	}
	if !isProtectedMount("/only/this/nested") {
		t.Fatal("a path beneath the overridden list's own prefix was not classified protected")
	}

	SetProtectedMountPrefixes(DefaultProtectedMountPrefixes)
	if !isProtectedMount("/home") {
		t.Fatal("/home was not classified protected after restoring the default list")
	}
}
