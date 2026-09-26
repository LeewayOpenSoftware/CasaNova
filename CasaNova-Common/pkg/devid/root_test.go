package devid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureTree builds the two pieces of /dev and /sys that root resolution
// reads, so the layouts this build machine will never boot can be exercised
// for real rather than mocked at the seam that matters.
//
// links maps a device path to the path it is a symlink to; every target is
// created as a plain file so EvalSymlinks can follow it. slaves maps a stacked
// device's base name to the base names of the devices backing it, the way
// /sys/block/<base>/slaves does.
func fixtureTree(t *testing.T, links map[string]string, slaves map[string][]string) SysResolver {
	t.Helper()
	root := t.TempDir()

	touch := func(rel string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, nil, 0o644); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}

	for link, target := range links {
		touch(target)
		abs := filepath.Join(root, link)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, target), abs); err != nil {
			t.Fatal(err)
		}
	}
	for base, members := range slaves {
		for _, m := range members {
			touch(filepath.Join("sys/block", base, "slaves", m))
		}
	}
	return SysResolver{Root: root}
}

func mountsRoot(source, fstype string) string {
	return "sysfs /sys sysfs rw 0 0\n" + source + " / " + fstype + " rw,relatime 0 0\n"
}

// THE BUG, in the form that loses data. A root filesystem on LVM (or on LUKS,
// or on multipath -- all device-mapper) is named /dev/mapper/vg-root in the
// mount table. That name carries no partition rule, so the old probe handed it
// straight to the comparison, where it matched no disk in the machine: the
// guard on POST /v1/storage {"format":true,"path":"/dev/sda"} answered "not
// the system disk" about the disk the OS was running from, and repartitioned
// it. Nothing in the old signature could have caught this -- it was a
// successful probe returning a name, with a nil error.
func TestRootOnLVMProtectsTheDiskUnderneath(t *testing.T) {
	res := fixtureTree(t,
		map[string]string{"dev/mapper/vg-root": "dev/dm-0"},
		map[string][]string{"dm-0": {"sda3"}},
	)

	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/mapper/vg-root", "ext4")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if strings.Join(root.Disks, ",") != "/dev/sda" {
		t.Fatalf("disks = %v, want [/dev/sda]", root.Disks)
	}
	for _, dev := range []string{"/dev/sda", "/dev/sda1", "/dev/sda3"} {
		if !root.Protects(dev) {
			t.Errorf("formatting %q was ALLOWED while / is on LVM over /dev/sda3 -- that wipes the boot disk", dev)
		}
	}
	if root.Protects("/dev/sdb") {
		t.Error("/dev/sdb was refused; it has nothing to do with the root filesystem")
	}
}

// An md array root is backed by every member, and every member is unsafe.
// Resolving to one disk and stopping would leave the mirror half-protected.
func TestRootOnMDArrayProtectsEveryMember(t *testing.T) {
	res := fixtureTree(t, nil, map[string][]string{"md0": {"sda1", "sdb1"}})

	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/md0", "ext4")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if strings.Join(root.Disks, ",") != "/dev/sda,/dev/sdb" {
		t.Fatalf("disks = %v, want [/dev/sda /dev/sdb]", root.Disks)
	}
	for _, dev := range []string{"/dev/sda", "/dev/sdb", "/dev/sdb2"} {
		if !root.Protects(dev) {
			t.Errorf("formatting %q was allowed while / is on an md array over sda1+sdb1", dev)
		}
	}
}

// LUKS on LVM on md is three levels of stacking, and the disks are only at the
// bottom. The walk has to keep going, and has to stop at the disks rather than
// reporting an intermediate dm node it cannot compare.
func TestRootOnStackedDevicesWalksToTheDisks(t *testing.T) {
	res := fixtureTree(t,
		map[string]string{"dev/mapper/crypt-root": "dev/dm-1"},
		map[string][]string{
			"dm-1": {"dm-0"},
			"dm-0": {"md0"},
			"md0":  {"nvme0n1p2", "nvme1n1p2"},
		},
	)

	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/mapper/crypt-root", "ext4")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if strings.Join(root.Disks, ",") != "/dev/nvme0n1,/dev/nvme1n1" {
		t.Fatalf("disks = %v, want [/dev/nvme0n1 /dev/nvme1n1]", root.Disks)
	}
}

// /dev/root is what an initramfs leaves in the mount table on plenty of
// systems. It is a symlink, and unresolved it names no disk at all.
func TestRootSymlinkIsFollowed(t *testing.T) {
	res := fixtureTree(t, map[string]string{"dev/root": "dev/mmcblk0p2"}, nil)

	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/root", "ext4")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if strings.Join(root.Disks, ",") != "/dev/mmcblk0" {
		t.Fatalf("disks = %v, want [/dev/mmcblk0]", root.Disks)
	}
}

// THE DECISION. When the probe cannot tell, every device is protected. This is
// the behaviour the user asked for over the old fail-open, and it is what the
// zero value of Root has to mean: a caller that forgets to check Status, or a
// case added to RootStatus and not handled, must land on the answer that keeps
// the data.
func TestUnknownRootProtectsEverything(t *testing.T) {
	for _, tc := range []struct {
		name string
		root Root
	}{
		{"the zero value", Root{}},
		{"no root line in the mount table", ProbeRootFrom(strings.NewReader("sysfs /sys sysfs rw 0 0\n"), SysResolver{Root: t.TempDir()})},
		{"a dm root whose slaves cannot be listed", ProbeRootFrom(strings.NewReader(mountsRoot("/dev/dm-0", "ext4")), SysResolver{Root: t.TempDir()})},
		{"a mapper name with no resolver at all", ProbeRootFrom(strings.NewReader(mountsRoot("/dev/mapper/vg-root", "ext4")), nil)},
		{"a root on zfs, whose pool members we do not resolve", ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/casanova", "zfs")), nil)},
	} {
		if tc.root.Status != RootUnknown {
			t.Errorf("%s: status = %s, want unknown", tc.name, tc.root.Status)
		}
		for _, dev := range []string{"/dev/sda", "/dev/nvme0n1", "/dev/mmcblk0p1"} {
			if !tc.root.Protects(dev) {
				t.Errorf("%s: formatting %q was allowed although the root disk is unknown", tc.name, dev)
			}
		}
	}
}

// THE OVER-CORRECTION CONTROL. "Refuse when unsure" must not become "refuse
// always". On a live USB the root really is on no disk, and an operator
// installing CasaNova from that USB has to be able to format the internal
// drive -- which is the entire job they booted it for. A change that turns
// this green test red has traded a data-loss bug for a lockout bug.
func TestDisklessRootProtectsNothing(t *testing.T) {
	for _, tc := range []struct{ source, fstype string }{
		{"overlay", "overlay"},
		{"none", "tmpfs"},
		{"rootfs", "rootfs"},
		{"10.0.0.1:/export", "nfs4"},
	} {
		root := ProbeRootFrom(strings.NewReader(mountsRoot(tc.source, tc.fstype)), SysResolver{Root: t.TempDir()})
		if root.Status != RootDiskless {
			t.Errorf("%s root: status = %s, want diskless (reason: %s)", tc.fstype, root.Status, root.Reason)
			continue
		}
		for _, dev := range []string{"/dev/sda", "/dev/sda1", "/dev/nvme0n1"} {
			if root.Protects(dev) {
				t.Errorf("%s root: formatting %q was refused, so a live-media install cannot use the internal disk", tc.fstype, dev)
			}
		}
	}
}

// The layouts that already worked keep working, and an unrelated disk stays
// formattable. /dev/sda is a string prefix of /dev/sdaa, the 27th disk a NAS
// with an expander really can have.
func TestResolvedRootStillProtectsOnlyItsOwnDisk(t *testing.T) {
	res := SysResolver{Root: t.TempDir()}
	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/sda2", "ext4")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	for _, dev := range []string{"/dev/sda", "/dev/sda1", "/dev/sda2"} {
		if !root.Protects(dev) {
			t.Errorf("%q was allowed; / is on /dev/sda2", dev)
		}
	}
	for _, dev := range []string{"/dev/sdaa", "/dev/sdaa1", "/dev/sdb", "/dev/nvme0n1"} {
		if root.Protects(dev) {
			t.Errorf("%q was refused; / is on /dev/sda and has nothing to do with it", dev)
		}
	}
}

// A cycle in /sys must end in a refusal, not in a hung request.
func TestStackingCycleIsBoundedAndRefuses(t *testing.T) {
	res := fixtureTree(t, nil, map[string][]string{
		"dm-0": {"dm-1"},
		"dm-1": {"dm-0"},
	})

	root := ProbeRootFrom(strings.NewReader(mountsRoot("/dev/dm-0", "ext4")), res)

	if root.Status != RootUnknown {
		t.Fatalf("status = %s, want unknown for a cyclic /sys tree", root.Status)
	}
	if !root.Protects("/dev/sda") {
		t.Error("a cyclic stacking tree allowed the format")
	}
}
