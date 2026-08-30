package v1

// The storage page's "benchmark" and "resize" actions were unreachable: both
// answered "partition must be mounted" for a disk whose partition was mounted.
// The handlers read MountPoint off the device the caller named, and the page
// only ever names the DISK — which is never itself mounted. These pin the
// resolution that replaces that read.
//
// The fixture mirrors the test box exactly: /dev/sda is a disk with no
// mountpoint, and its single child /dev/sda1 is ext4 at /mnt/UNIT5.

import (
	"testing"

	model1 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
)

func sda() model1.LSBLKModel {
	return model1.LSBLKModel{
		Path: "/dev/sda", Type: "disk",
		Children: []model1.LSBLKModel{
			{Path: "/dev/sda1", Type: "part", FsType: "ext4", MountPoint: "/mnt/UNIT5"},
		},
	}
}

func TestMountedFilesystems_ResolvesWholeDiskToItsPartition(t *testing.T) {
	// This is the bug, stated directly: the disk carries no mountpoint of its
	// own, so anything reading it finds "" and refuses. The partition beneath
	// it is mounted and is what both operations must act on.
	got := mountedFilesystems(sda())
	if len(got) != 1 {
		t.Fatalf("want 1 mounted filesystem under /dev/sda, got %d: %+v", len(got), got)
	}
	if got[0].Device != "/dev/sda1" || got[0].MountPoint != "/mnt/UNIT5" || got[0].FsType != "ext4" {
		t.Errorf("wrong target: %+v", got[0])
	}
}

func TestMountedFilesystems_AcceptsAPartitionNamedDirectly(t *testing.T) {
	// The API contract still allows naming the partition. Resolution must be a
	// widening, not a replacement — an API client passing /dev/sda1 keeps working.
	got := mountedFilesystems(model1.LSBLKModel{
		Path: "/dev/sda1", Type: "part", FsType: "ext4", MountPoint: "/mnt/UNIT5",
	})
	if len(got) != 1 || got[0].Device != "/dev/sda1" {
		t.Fatalf("a mounted partition must resolve to itself, got %+v", got)
	}
}

func TestMountedFilesystems_WalksThroughLVMAndLUKS(t *testing.T) {
	// disk → partition → crypt → lv. A one-level lookup would find nothing
	// here and report an encrypted or LVM disk as unmountable.
	got := mountedFilesystems(model1.LSBLKModel{
		Path: "/dev/sdb", Type: "disk",
		Children: []model1.LSBLKModel{{
			Path: "/dev/sdb1", Type: "part",
			Children: []model1.LSBLKModel{{
				Path: "/dev/mapper/crypt", Type: "crypt",
				Children: []model1.LSBLKModel{
					{Path: "/dev/mapper/vg-data", Type: "lvm", FsType: "xfs", MountPoint: "/srv/data"},
				},
			}},
		}},
	})
	if len(got) != 1 || got[0].MountPoint != "/srv/data" || got[0].FsType != "xfs" {
		t.Fatalf("want the leaf filesystem at /srv/data, got %+v", got)
	}
}

func TestMountedFilesystems_SkipsSwap(t *testing.T) {
	// lsblk reports swap as mounted at "[SWAP]". It is not a directory: a
	// benchmark scratch file cannot be written there and an online grow is
	// meaningless, so it must never be selectable as a target.
	got := mountedFilesystems(model1.LSBLKModel{
		Path: "/dev/sdc", Type: "disk",
		Children: []model1.LSBLKModel{
			{Path: "/dev/sdc1", Type: "part", FsType: "swap", MountPoint: "[SWAP]"},
			{Path: "/dev/sdc2", Type: "part", FsType: "ext4", MountPoint: "/data"},
		},
	})
	if len(got) != 1 || got[0].Device != "/dev/sdc2" {
		t.Fatalf("swap must not be a target; got %+v", got)
	}
}

func TestMountedFilesystems_ReportsNothingWhenNothingIsMounted(t *testing.T) {
	// The refusal still has to happen when it is true — an unmounted disk cannot
	// be benchmarked or grown. Widening the lookup must not make the guard
	// vacuous.
	got := mountedFilesystems(model1.LSBLKModel{
		Path: "/dev/sdd", Type: "disk",
		Children: []model1.LSBLKModel{{Path: "/dev/sdd1", Type: "part", FsType: "ext4"}},
	})
	if len(got) != 0 {
		t.Fatalf("an unmounted disk must yield no target, got %+v", got)
	}
}

func TestMountedFilesystems_ReportsEveryCandidateSoResizeCanRefuse(t *testing.T) {
	// Resize needs to distinguish "one obvious target" from "ambiguous".
	// That decision is only possible if the resolver returns all of them
	// rather than stopping at the first.
	got := mountedFilesystems(model1.LSBLKModel{
		Path: "/dev/sde", Type: "disk",
		Children: []model1.LSBLKModel{
			{Path: "/dev/sde1", Type: "part", FsType: "ext4", MountPoint: "/a"},
			{Path: "/dev/sde2", Type: "part", FsType: "ext4", MountPoint: "/b"},
		},
	})
	if len(got) != 2 {
		t.Fatalf("want both candidates so the caller can refuse, got %+v", got)
	}
}
