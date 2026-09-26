package v1

import (
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/devid"
)

// A /proc/mounts root line, one per device family. Only the first three fields
// matter to the probe: the source device, the mount point, and the filesystem
// type.
func mountsRoot(dev, fstype string) string {
	return "sysfs /sys sysfs rw 0 0\n" +
		dev + " / " + fstype + " rw,relatime 0 0\n"
}

// zfsRefusesSystemDisk is exactly the decision both ZFS guards make: probe for
// the root disk, then ask whether the requested device is protected by it.
//
// It used to be three helpers local to this package -- the fourth hand-rolled
// copy of that question in the tree. The guards now call
// devid.ProbeRoot().Protects, so this test drives the same code the handlers
// do, with the mount table supplied rather than read from /proc.
func zfsRefusesSystemDisk(mounts, requested string) bool {
	return devid.ProbeRootFrom(strings.NewReader(mounts), nil).Protects(requested)
}

// THE BUG. The whole-disk name was derived by trimming trailing digits, so
// /dev/mmcblk0p2 -- the root filesystem of every eMMC and SD-card box, the
// Raspberry-Pi-class hardware this product is aimed at -- reduced to
// "/dev/mmcblk0p" rather than "/dev/mmcblk0". Asked to build a pool on
// /dev/mmcblk0, the guard compared it against "/dev/mmcblk0p", found neither
// equality nor a prefix, and let the request through onto the boot disk.
func TestZFSGuardRefusesTheBootDiskOnEMMC(t *testing.T) {
	m := mountsRoot("/dev/mmcblk0p2", "ext4")

	for _, requested := range []string{"/dev/mmcblk0", "/dev/mmcblk0p1", "/dev/mmcblk0p2"} {
		if !zfsRefusesSystemDisk(m, requested) {
			t.Errorf("a pool on %q was ALLOWED while the system booted from "+
				"/dev/mmcblk0p2 — that repartitions the card the OS runs from", requested)
		}
	}
}

// The other direction. /dev/sda is a string prefix of /dev/sdaa, the 27th
// disk a NAS with an expander really can have, so the old guard refused to
// use a disk that has nothing to do with the root filesystem.
func TestZFSGuardDoesNotRefuseAnUnrelatedDisk(t *testing.T) {
	m := mountsRoot("/dev/sda1", "ext4")

	for _, requested := range []string{"/dev/sdaa", "/dev/sdaa1", "/dev/sdb"} {
		if zfsRefusesSystemDisk(m, requested) {
			t.Errorf("a pool on %q was refused as the system disk; the system is on /dev/sda1", requested)
		}
	}
}

// The layouts that already worked must keep working.
func TestZFSGuardStillCoversSATAAndNVMe(t *testing.T) {
	for _, tc := range []struct{ root, disk, part string }{
		{"/dev/sda1", "/dev/sda", "/dev/sda2"},
		{"/dev/nvme0n1p2", "/dev/nvme0n1", "/dev/nvme0n1p1"},
	} {
		m := mountsRoot(tc.root, "ext4")
		for _, requested := range []string{tc.disk, tc.part} {
			if !zfsRefusesSystemDisk(m, requested) {
				t.Errorf("a pool on %q was allowed while the system booted from %s", requested, tc.root)
			}
		}
	}
}

// THE DECISION THIS CARD CHANGED. A root the probe cannot name used to mean
// "skip the check": the guard was written as `if rootDev != ""`, and a root on
// LVM, on an md array, or behind a name the probe could not reduce to a disk
// all produced an empty string. `zpool create` on the boot disk was allowed in
// precisely the cases where the guard was needed. It now refuses.
func TestZFSGuardRefusesWhenTheRootDiskIsUnknown(t *testing.T) {
	for _, m := range []string{
		mountsRoot("/dev/mapper/vg-root", "ext4"), // LVM or LUKS, no resolver here
		mountsRoot("rpool/ROOT/casanova", "zfs"),  // root on a pool we do not resolve
		"sysfs /sys sysfs rw 0 0\n",               // no entry for / at all
	} {
		if !zfsRefusesSystemDisk(m, "/dev/sdb") {
			t.Errorf("a pool on /dev/sdb was allowed although the root disk could not be "+
				"determined; mount table:\n%s", m)
		}
	}
}

// THE OVER-CORRECTION CONTROL. Refusing when unsure must not become refusing
// always. A live image really has no boot disk, and an operator who booted it
// to build a pool on the internal drives has to be able to. If this test goes
// red, a data-loss bug has been traded for a lockout bug.
func TestZFSGuardStillAllowsPoolsWhenRootIsOnNoDisk(t *testing.T) {
	for _, fstype := range []string{"overlay", "tmpfs", "nfs4"} {
		if zfsRefusesSystemDisk(mountsRoot("none", fstype), "/dev/sdb") {
			t.Errorf("a pool on /dev/sdb was refused with / on %s, which is backed by no disk at all", fstype)
		}
	}
}
