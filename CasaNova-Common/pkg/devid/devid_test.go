package devid

import (
	"strings"
	"testing"
)

func TestWholeDisk(t *testing.T) {
	cases := []struct{ in, want string }{
		// The family the old trailing-digit trim was written for.
		{"/dev/sda", "/dev/sda"},
		{"/dev/sda1", "/dev/sda"},
		{"/dev/sda15", "/dev/sda"},
		{"/dev/sdaa", "/dev/sdaa"},
		{"/dev/sdaa3", "/dev/sdaa"},
		{"/dev/hdb2", "/dev/hdb"},
		{"/dev/vdb", "/dev/vdb"},
		{"/dev/vdb1", "/dev/vdb"},
		{"/dev/xvda2", "/dev/xvda"},

		// p-separated families. A disk name here ENDS in a digit, which is
		// what the trailing-digit trim destroyed.
		{"/dev/nvme0n1", "/dev/nvme0n1"},
		{"/dev/nvme0n1p1", "/dev/nvme0n1"},
		{"/dev/nvme0n1p12", "/dev/nvme0n1"},
		{"/dev/nvme1n2", "/dev/nvme1n2"},
		{"/dev/mmcblk0", "/dev/mmcblk0"},
		{"/dev/mmcblk0p1", "/dev/mmcblk0"},
		{"/dev/mmcblk1p2", "/dev/mmcblk1"},
		{"/dev/loop0", "/dev/loop0"},
		{"/dev/loop0p1", "/dev/loop0"},
		{"/dev/md0", "/dev/md0"},
		{"/dev/md0p1", "/dev/md0"},
		{"/dev/nbd0p3", "/dev/nbd0"},
		{"/dev/dm-0", "/dev/dm-0"},

		// Not the kernel's naming: returned untouched rather than guessed at.
		{"/dev/mapper/vg0-root", "/dev/mapper/vg0-root"},
		{"/dev/disk/by-id/ata-Foo1", "/dev/disk/by-id/ata-Foo1"},
		{"/dev/root", "/dev/root"},
		{"", ""},
		{"/dev/", "/dev/"},

		// Bare names work too, so a caller that already stripped /dev/ is not
		// silently given back a whole-disk answer of the wrong shape.
		{"sda1", "sda"},
		{"mmcblk0p1", "mmcblk0"},
	}
	for _, c := range cases {
		if got := WholeDisk(c.in); got != c.want {
			t.Errorf("WholeDisk(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsSameOrChildCatchesTheEMMCMiss is the regression this package exists
// for. The old guard reduced /dev/mmcblk0p1 to "/dev/mmcblk0p" and then asked
// whether "/dev/mmcblk0" started with it. It does not, so formatting the card
// the system booted from was allowed.
func TestIsSameOrChildCatchesTheEMMCMiss(t *testing.T) {
	root := WholeDisk("/dev/mmcblk0p1")
	if root != "/dev/mmcblk0" {
		t.Fatalf("root of /dev/mmcblk0p1 = %q, want /dev/mmcblk0", root)
	}
	oldGuard := strings.HasPrefix("/dev/mmcblk0", "/dev/mmcblk0p") || "/dev/mmcblk0" == "/dev/mmcblk0p"
	if oldGuard {
		t.Fatal("premise broken: the old guard was supposed to MISS this")
	}
	if !IsSameOrChild(root, "/dev/mmcblk0") {
		t.Error("IsSameOrChild must refuse the disk the root filesystem lives on")
	}
	if !IsSameOrChild(root, "/dev/mmcblk0p2") {
		t.Error("a sibling partition of the root disk is still the root disk")
	}
}

// TestIsSameOrChildDropsTheFalseAlarm is the other direction: /dev/sda is a
// string prefix of /dev/sdaa, so the old guard called the 27th disk the
// system disk.
func TestIsSameOrChildDropsTheFalseAlarm(t *testing.T) {
	if !strings.HasPrefix("/dev/sdaa1", "/dev/sda") {
		t.Fatal("premise broken: the old guard was supposed to MATCH this")
	}
	if IsSameOrChild("/dev/sda", "/dev/sdaa1") {
		t.Error("/dev/sdaa1 is not a partition of /dev/sda")
	}
	if IsSameOrChild("/dev/nvme0n1", "/dev/nvme0n2") {
		t.Error("a second NVMe namespace is a different disk")
	}
}

func TestIsSameOrChild(t *testing.T) {
	cases := []struct {
		disk, dev string
		want      bool
	}{
		{"/dev/sda", "/dev/sda", true},
		{"/dev/sda", "/dev/sda1", true},
		{"/dev/sda1", "/dev/sda3", true}, // both reduce to /dev/sda
		{"/dev/sda", "/dev/sdb", false},
		{"/dev/sda", "/dev/sdb1", false},
		{"/dev/nvme0n1", "/dev/nvme0n1p3", true},
		{"/dev/mmcblk0", "/dev/mmcblk0p1", true},
		{"/dev/mmcblk0", "/dev/mmcblk1p1", false},
		{"/dev/mapper/vg0-root", "/dev/mapper/vg0-root", true},
		{"/dev/mapper/vg0-root", "/dev/sda1", false},

		// An unknown root device must not turn every disk into the root disk.
		{"", "/dev/sda1", false},
		{"/dev/sda", "", false},
	}
	for _, c := range cases {
		if got := IsSameOrChild(c.disk, c.dev); got != c.want {
			t.Errorf("IsSameOrChild(%q, %q) = %v, want %v", c.disk, c.dev, got, c.want)
		}
	}
}

func TestRootDeviceFrom(t *testing.T) {
	cases := []struct{ name, mounts, want string }{
		{
			"sata root",
			"sysfs /sys sysfs rw 0 0\n/dev/sda2 / ext4 rw,relatime 0 0\n/dev/sda1 /boot/efi vfat rw 0 0\n",
			"/dev/sda",
		},
		{
			"emmc root",
			"/dev/mmcblk0p2 / ext4 rw,relatime 0 0\n/dev/mmcblk0p1 /boot vfat rw 0 0\n",
			"/dev/mmcblk0",
		},
		{
			"nvme root",
			"/dev/nvme0n1p3 / btrfs rw 0 0\n",
			"/dev/nvme0n1",
		},
		{
			// A mount point containing a space arrives octal-escaped, so a
			// field split is safe; the point is only that "/" is matched
			// exactly and not by prefix.
			"a mount point that starts with a slash is not the root",
			"/dev/sdb1 /DATA ext4 rw 0 0\n/dev/sdc1 / ext4 rw 0 0\n",
			"/dev/sdc",
		},
		{
			"overlay root is not a device node",
			"overlay / overlay rw 0 0\n",
			"",
		},
		{
			"no root line at all",
			"sysfs /sys sysfs rw 0 0\n",
			"",
		},
	}
	for _, c := range cases {
		got, err := RootDeviceFrom(strings.NewReader(c.mounts))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: RootDeviceFrom = %q, want %q", c.name, got, c.want)
		}
	}
}
