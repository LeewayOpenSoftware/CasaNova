// Package devid answers one question about Linux block device nodes:
// does this device node name the same disk as that one, or a partition of it?
//
// WHY THIS EXISTS. Three separate guards refuse to format or repartition the
// disk the system booted from, and all three asked that question the same
// wrong way:
//
//	rootDev != "" && (strings.HasPrefix(path, rootDev) || path == rootDev)
//
// A prefix match is not a device-identity match, and it is wrong in both
// directions.
//
// The dangerous direction is the miss. The whole-disk name was derived by
// trimming trailing digits, so /dev/mmcblk0p1 (the root filesystem of every
// eMMC and SD-card box — the Raspberry-Pi-class hardware this product is aimed
// at) reduced to "/dev/mmcblk0p", not "/dev/mmcblk0". The guard then compared
// the disk it was asked to format, "/dev/mmcblk0", against "/dev/mmcblk0p":
// not equal, not a prefix, guard passes, and the request repartitions the card
// the OS is running from. The trim was written for /dev/sda1 and carried an
// nvme special case; nothing else in the tree was considered.
//
// The harmless direction is the false alarm. /dev/sda is a string prefix of
// /dev/sdaa — the 27th disk, which a NAS with an expander really can have —
// so on such a box the guard refuses to format a disk that has nothing to do
// with the root filesystem, and says "cannot format the system disk" about a
// disk that is not.
//
// Both come from treating device nodes as strings. The names are not
// arbitrary: the kernel builds a partition node from its disk node by one of
// two rules, and which rule applies is decided by the disk's family. So that
// is what this package encodes — the two rules and the families they belong
// to — rather than a heuristic that happens to hold for /dev/sda.
package devid

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// pSeparatedPrefixes are the families whose partition node is the disk node
// plus "p" plus the partition number: /dev/nvme0n1 -> /dev/nvme0n1p1,
// /dev/mmcblk0 -> /dev/mmcblk0p1. Their disk names already END in a digit,
// which is exactly why trimming trailing digits mangles them.
var pSeparatedPrefixes = []string{"nvme", "mmcblk", "loop", "md", "nbd", "dm-"}

// letteredPrefixes are the families whose disk name is all letters and whose
// partition node just appends the number: sda -> sda1, xvdb -> xvdb2.
// Order matters only in that "xvd" must be tried before "vd"; IsPrefix below
// takes the longest match.
var letteredPrefixes = []string{"xvd", "sd", "hd", "vd"}

// WholeDisk returns the device node of the disk that dev belongs to. Given a
// partition it returns its disk; given a disk, or anything it does not
// recognise, it returns dev unchanged.
//
// Returning the input unchanged is the deliberate answer for the unrecognised
// case. This package cannot know every naming scheme (device-mapper names are
// whatever the administrator called them, and /dev/root is a symlink whose
// target only the filesystem can supply), and guessing at one would produce a
// confident wrong disk. An unchanged name compares equal to itself and to
// nothing else, which is the narrow, honest answer.
//
//	/dev/sda1      -> /dev/sda
//	/dev/sdaa3     -> /dev/sdaa
//	/dev/nvme0n1p2 -> /dev/nvme0n1
//	/dev/nvme0n1   -> /dev/nvme0n1   (a namespace, not a partition)
//	/dev/mmcblk0p1 -> /dev/mmcblk0
//	/dev/mmcblk0   -> /dev/mmcblk0
//	/dev/mapper/vg-root -> /dev/mapper/vg-root
func WholeDisk(dev string) string {
	dir, base, ok := splitNode(dev)
	if !ok {
		return dev
	}

	// Rule 1: <disk>p<N>, for families whose disk name ends in a digit.
	if fam := matchPrefix(base, pSeparatedPrefixes); fam != "" {
		if i := strings.LastIndexByte(base, 'p'); i > len(fam)-1 {
			head, tail := base[:i], base[i+1:]
			// The "p" is a partition separator only if digits follow it and
			// the head is itself a complete disk name ending in a digit.
			// That is what keeps /dev/loop0 (no "p") and the bare namespace
			// /dev/nvme0n1 out of this branch.
			if allDigits(tail) && endsInDigit(head) {
				return dir + head
			}
		}
		return dev
	}

	// Rule 2: <disk><N>, for families whose disk name is all letters.
	if fam := matchPrefix(base, letteredPrefixes); fam != "" {
		head := strings.TrimRight(base, "0123456789")
		// Guard against a name that is nothing but the family and digits
		// (there is no disk "sd"), and against no digits at all (/dev/sda).
		if head != base && len(head) > len(fam) && allLetters(head) {
			return dir + head
		}
		return dev
	}

	return dev
}

// IsSameOrChild reports whether dev is disk itself or one of its partitions.
//
// It is the question the format guards meant to ask. Both sides are reduced to
// their whole disk first, so /dev/sda vs /dev/sda1 matches, /dev/sda vs
// /dev/sdaa does not, and /dev/mmcblk0 vs /dev/mmcblk0p1 does.
//
// An empty disk matches nothing: callers that could not determine the root
// device must not have every device suddenly count as the root device.
func IsSameOrChild(disk, dev string) bool {
	disk, dev = strings.TrimSpace(disk), strings.TrimSpace(dev)
	if disk == "" || dev == "" {
		return false
	}
	return WholeDisk(disk) == WholeDisk(dev)
}

// RootDeviceFrom returns the whole-disk device node carrying "/", read from a
// /proc/mounts-formatted stream. It takes a reader so the parsing can be
// tested against the layouts this box will never have — eMMC, NVMe, LVM —
// instead of only against whatever the build machine booted from.
//
// It returns "" when the root device is not a /dev node (an overlay or a
// UUID= entry). Callers already treat "" as "cannot tell", and a name that is
// not a device node cannot be compared to one.
func RootDeviceFrom(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[1] != "/" {
			continue
		}
		dev := fields[0]
		if !strings.HasPrefix(dev, "/dev/") {
			return "", nil
		}
		return WholeDisk(dev), nil
	}
	return "", sc.Err()
}

// RootDevice reads /proc/mounts and returns the whole-disk device node that
// carries the root filesystem.
func RootDevice() (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return RootDeviceFrom(f)
}

// splitNode separates a device node into its directory (with the trailing
// slash) and its base name. It reports false for anything that is not a plain
// name under a directory — including /dev/mapper/... and /dev/disk/by-id/...,
// whose names carry no partition rule at all.
func splitNode(dev string) (dir, base string, ok bool) {
	dev = strings.TrimSpace(dev)
	if dev == "" || strings.HasSuffix(dev, "/") {
		return "", "", false
	}
	i := strings.LastIndexByte(dev, '/')
	dir, base = dev[:i+1], dev[i+1:]
	if base == "" || base == "." || base == ".." {
		return "", "", false
	}
	// Only /dev itself, or a bare name, follows the kernel's partition naming.
	// /dev/mapper and /dev/disk/by-* are administrator- and udev-chosen names.
	if dir != "" && dir != "/dev/" {
		return "", "", false
	}
	return dir, base, true
}

// matchPrefix returns the longest entry of prefixes that base starts with.
func matchPrefix(base string, prefixes []string) string {
	best := ""
	for _, p := range prefixes {
		if len(p) > len(best) && strings.HasPrefix(base, p) {
			best = p
		}
	}
	return best
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func allLetters(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func endsInDigit(s string) bool {
	return s != "" && s[len(s)-1] >= '0' && s[len(s)-1] <= '9'
}
