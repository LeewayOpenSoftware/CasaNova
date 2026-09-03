package v1

import (
	"testing"
)

// A real /proc/self/mountinfo root line, one per device family. Only the
// fields the parser reads matter: [4] is the mount point and the source
// device follows the " - <fstype>" marker.
func mountinfoRoot(dev, fstype string) string {
	return "26 1 179:2 / / rw,relatime shared:1 - " + fstype + " " + dev + " rw\n" +
		"27 26 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:2 - proc proc rw\n"
}

// zfsRefusesSystemDisk is exactly the decision both ZFS guards make:
// derive the root disk, then ask whether the requested device is that disk
// or one of its partitions.
func zfsRefusesSystemDisk(mountinfo, requested string) bool {
	rootDev := zfsRootDeviceFrom(mountinfo)
	if rootDev == "" {
		return false
	}
	return zfsIsSystemDisk(rootDev, requested)
}

// THE BUG. The whole-disk name was derived by trimming trailing digits, so
// /dev/mmcblk0p2 -- the root filesystem of every eMMC and SD-card box, the
// Raspberry-Pi-class hardware this product is aimed at -- reduced to
// "/dev/mmcblk0p" rather than "/dev/mmcblk0". Asked to build a pool on
// /dev/mmcblk0, the guard compared it against "/dev/mmcblk0p", found neither
// equality nor a prefix, and let the request through onto the boot disk.
func TestZFSGuardRefusesTheBootDiskOnEMMC(t *testing.T) {
	mi := mountinfoRoot("/dev/mmcblk0p2", "ext4")

	if got := zfsRootDeviceFrom(mi); got != "/dev/mmcblk0" {
		t.Errorf("root disk for /dev/mmcblk0p2 = %q, want %q", got, "/dev/mmcblk0")
	}
	for _, requested := range []string{"/dev/mmcblk0", "/dev/mmcblk0p1", "/dev/mmcblk0p2"} {
		if !zfsRefusesSystemDisk(mi, requested) {
			t.Errorf("a pool on %q was ALLOWED while the system booted from "+
				"/dev/mmcblk0p2 — that repartitions the card the OS runs from", requested)
		}
	}
}

// The other direction. /dev/sda is a string prefix of /dev/sdaa, the 27th
// disk a NAS with an expander really can have, so the old guard refused to
// use a disk that has nothing to do with the root filesystem.
func TestZFSGuardDoesNotRefuseAnUnrelatedDisk(t *testing.T) {
	mi := mountinfoRoot("/dev/sda1", "ext4")

	for _, requested := range []string{"/dev/sdaa", "/dev/sdaa1", "/dev/sdb"} {
		if zfsRefusesSystemDisk(mi, requested) {
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
		mi := mountinfoRoot(tc.root, "ext4")
		if got := zfsRootDeviceFrom(mi); got != tc.disk {
			t.Errorf("root disk for %s = %q, want %q", tc.root, got, tc.disk)
		}
		for _, requested := range []string{tc.disk, tc.part} {
			if !zfsRefusesSystemDisk(mi, requested) {
				t.Errorf("a pool on %q was allowed while the system booted from %s", requested, tc.root)
			}
		}
	}
}

// A root that is not a device node at all (overlay, a UUID= entry) means
// "cannot tell". The guards already treat "" as skip-the-check rather than
// refuse-everything, so nothing must suddenly count as the system disk.
func TestZFSGuardTreatsAnUndiscoverableRootAsUnknown(t *testing.T) {
	if zfsRefusesSystemDisk(mountinfoRoot("overlay", "overlay"), "/dev/sdb") {
		t.Error("/dev/sdb was refused as the system disk although the root device could not be determined")
	}
}
