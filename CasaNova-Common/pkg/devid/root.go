package devid

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RootStatus says how much a root-disk probe was able to determine.
//
// These three values exist because the API they replace could not tell them
// apart. It returned (string, error), and the case that matters most -- "the
// root filesystem is on a disk, but I could not work out which disk" -- came
// back as ("", nil): an empty name and a NIL error. Callers could not detect
// it even if they checked the error, and IsSameOrChild("") is false for every
// device, so the format guards read it as "not the system disk" and allowed
// the format. The one guard whose entire job is to stop the operator wiping
// the disk the OS is running from failed open, and it did it silently.
type RootStatus int

const (
	// RootUnknown means the probe could not determine the root disk, and
	// therefore cannot rule out that the device being asked about is part of
	// it.
	//
	// It is the zero value deliberately. A caller that reads Disks without
	// looking at Status, or that adds a case and forgets to handle it, gets
	// the answer that protects data rather than the one that destroys it.
	RootUnknown RootStatus = iota

	// RootOnDisks means the root filesystem is carried by Disks.
	RootOnDisks

	// RootDiskless means the root filesystem is carried by no block device at
	// all: a live image running on an overlay, an initramfs on a ramdisk, a
	// netbooted root. There is no boot disk to protect, and refusing to
	// format would only lock the operator out of every disk in the machine.
	//
	// This is the case the original fail-open was written for, and the only
	// one that keeps it.
	RootDiskless
)

func (s RootStatus) String() string {
	switch s {
	case RootOnDisks:
		return "resolved"
	case RootDiskless:
		return "diskless"
	default:
		return "unknown"
	}
}

// Root is the answer to "which disk did this system boot from?".
type Root struct {
	Status RootStatus

	// Disks holds whole-disk device nodes, and is meaningful only when Status
	// is RootOnDisks. It is a slice because one root filesystem can sit on
	// several disks: an LVM volume group or an md array is backed by all of
	// its members, and every one of them is unsafe to format.
	Disks []string

	// Reason states what the probe saw, in words meant both for a log line
	// and for the refusal shown to the operator. A guard that refuses without
	// saying why is the other half of the silent failure this type ends: the
	// operator on a live USB needs to be told that the answer was "I cannot
	// tell", not left guessing why their disk is suddenly protected.
	Reason string
}

// Protects reports whether dev must not be formatted or repartitioned.
//
// The unknown case answers true. That is the decision this type encodes: on a
// guard whose only purpose is to stop the operator destroying the filesystem
// the machine is running from, "I could not tell" has to mean "refuse". The
// cost of being wrong is not symmetric -- a refusal is an error message and a
// support question, an unguarded format is an unbootable box and lost data --
// and the old behaviour paid the expensive side of that trade to save the
// cheap one.
func (r Root) Protects(dev string) bool {
	switch r.Status {
	case RootOnDisks:
		for _, disk := range r.Disks {
			if IsSameOrChild(disk, dev) {
				return true
			}
		}
		return false
	case RootDiskless:
		return false
	default:
		return true
	}
}

// disklessRootFS are the filesystem types that carry a root filesystem with no
// block device behind it. A live USB boots onto an overlay, a netboot onto
// NFS, an initramfs-only system onto rootfs or tmpfs. On such a machine there
// is no disk to protect and every disk is a legitimate format target, so this
// list is what keeps "refuse when unsure" from becoming "refuse on live media".
//
// It is a list of the cases we can positively identify, not a catch-all: a
// filesystem type that is not here resolves as unknown, which refuses. Adding
// a type to this list widens what may be formatted, so each entry has to be a
// filesystem that genuinely cannot live on a local disk.
var disklessRootFS = map[string]struct{}{
	"overlay":   {},
	"overlayfs": {},
	"aufs":      {},
	"tmpfs":     {},
	"ramfs":     {},
	"rootfs":    {},
	"nfs":       {},
	"nfs4":      {},
	"cifs":      {},
	"smb3":      {},
	"9p":        {},
	"virtiofs":  {},
}

// maxStackDepth bounds how far the backing-device walk follows one stacked
// device into another. LUKS on LVM on an md array is three levels; the bound
// exists so a cycle in a malformed /sys tree ends in a refusal rather than in
// a hung request.
const maxStackDepth = 8

// Resolver supplies the two filesystem lookups that root resolution needs. It
// is an interface for the same reason RootFrom takes a reader: so the device
// layouts this build machine will never boot -- LVM, LUKS, an md array, a
// /dev/root symlink -- can be tested against a fixture tree instead of
// assumed.
type Resolver interface {
	// Resolve follows symlinks for a device path and returns the path it
	// actually names. /dev/root, and everything under /dev/disk/by-uuid and
	// its siblings, are symlinks to the real node.
	Resolve(path string) (string, error)

	// Slaves lists the base names of the devices backing the stacked device
	// named by base, the way the entries of /sys/block/<base>/slaves do. A
	// device that is not stacked has no such directory; that is not an error,
	// and returns no names.
	Slaves(base string) ([]string, error)

	// PoolMembers lists the device paths ZFS reports as belonging to pool.
	//
	// ZFS is the one layout whose membership the kernel does not publish: a
	// pool's vdevs appear nowhere in /sys/block, so unlike LVM or md there is
	// no slaves directory to walk and the only authority is ZFS itself. An
	// error here has to end in a refusal, never in an empty list treated as
	// "no disks" -- that would be the ("", nil) mistake this file exists to
	// undo, wearing a different type.
	PoolMembers(pool string) ([]string, error)
}

// SysResolver answers from the running kernel: symlinks from the filesystem,
// device stacking from /sys/block.
type SysResolver struct {
	// Root prefixes both filesystem lookups, so a test can point them at a
	// fixture tree. Empty means the real filesystem.
	Root string

	// ZpoolStatus returns the output of `zpool status -P <pool>`. Nil means
	// run the real command.
	//
	// It is a field rather than a fixture path because ZFS pool membership is
	// not a file: it comes back from a command, and no directory tree can
	// stand in for it. What this seam buys is the part that can actually be
	// wrong -- the parser -- exercised against verbatim zpool output for
	// layouts (mirror, raidz) that no build machine here boots from.
	ZpoolStatus func(pool string) ([]byte, error)
}

func (s SysResolver) Resolve(path string) (string, error) {
	target, err := filepath.EvalSymlinks(filepath.Join(s.Root, path))
	if err != nil {
		return "", err
	}
	if s.Root == "" {
		return target, nil
	}
	// Give the answer back in the device namespace the caller asked in, not in
	// the fixture tree's.
	rel, err := filepath.Rel(s.Root, target)
	if err != nil {
		return "", err
	}
	return "/" + filepath.ToSlash(rel), nil
}

func (s SysResolver) Slaves(base string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "/sys/block", base, "slaves"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// ProbeRoot reads the running system's mount table and reports which disks
// carry the root filesystem.
func ProbeRoot() Root {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return Root{Reason: fmt.Sprintf("the mount table could not be read: %v", err)}
	}
	defer f.Close()
	return ProbeRootFrom(f, SysResolver{})
}

// ProbeRootFrom determines the root disks from a /proc/mounts-formatted
// stream, using res for the symlink and stacking lookups. Parsing is separated
// from reading so the layouts this machine will never boot can be tested
// rather than assumed; res may be nil, which answers unknown for any root that
// needs a lookup.
func ProbeRootFrom(r io.Reader, res Resolver) Root {
	source, fstype, err := rootMount(r)
	switch {
	case err != nil:
		return Root{Reason: fmt.Sprintf("the mount table could not be read: %v", err)}
	case source == "":
		return Root{Reason: "the mount table has no entry for /"}
	}

	if _, ok := disklessRootFS[fstype]; ok {
		return Root{
			Status: RootDiskless,
			Reason: fmt.Sprintf("/ is a %s mount (source %q), which no block device backs", fstype, source),
		}
	}

	if fstype == zfsFSType {
		return zfsRoot(source, res)
	}

	disks, err := disksBehind([]string{source}, source, res)
	if err != nil {
		return Root{Reason: fmt.Sprintf("/ is on %q (%s) but the disk behind it could not be determined: %v", source, fstype, err)}
	}
	return Root{
		Status: RootOnDisks,
		Disks:  disks,
		Reason: fmt.Sprintf("/ is on %s (%s), carried by %s", source, fstype, strings.Join(disks, ", ")),
	}
}

// disksBehind reduces the devices carrying / to the whole disks they live on.
//
// Two things stand between a device name and a disk node that IsSameOrChild
// can compare. The name may be a symlink -- /dev/root, /dev/disk/by-uuid/<uuid>,
// or the /dev/disk/by-id path a ZFS pool usually records -- and the device may
// be stacked: /dev/mapper/vg-root is a device-mapper node whose real disks are
// its slaves in /sys/block, and asking IsSameOrChild about it directly answers
// false for every disk in the machine, which is precisely how an LVM or LUKS
// root went unprotected.
//
// It takes a LIST of starting nodes because a root filesystem can be carried
// by several devices that are not stacked under one node. An md or LVM root
// arrives as a single node and fans out through slaves; a ZFS root arrives
// already fanned out, as the pool's vdev list, and every one of those has to
// go through the same resolution. The two cases share this walk so that a
// mirror vdev on LUKS is handled by the code that already knows how to walk
// LUKS, rather than by a second implementation that would learn it later.
//
// what names what is being resolved, for error messages: the caller knows
// whether these paths came from the mount table or from a pool's vdev list.
func disksBehind(sources []string, what string, res Resolver) ([]string, error) {
	var (
		out  []string
		seen = map[string]bool{}
		walk func(node string, depth int) error
	)
	walk = func(node string, depth int) error {
		if depth > maxStackDepth {
			return fmt.Errorf("stacked devices below %q are still stacked after %d levels", what, maxStackDepth)
		}
		disk := WholeDisk(node)
		_, base, ok := splitNode(disk)
		if !ok {
			return fmt.Errorf("%q is not a kernel device node", disk)
		}
		if isStacked(base) {
			if res == nil {
				return fmt.Errorf("%q is a stacked device and no resolver was given to list its disks", disk)
			}
			slaves, err := res.Slaves(base)
			if err != nil {
				return fmt.Errorf("the disks backing %q could not be listed: %w", disk, err)
			}
			if len(slaves) == 0 {
				return fmt.Errorf("%q is a stacked device but lists no backing disks", disk)
			}
			for _, s := range slaves {
				if err := walk("/dev/"+s, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if !seen[disk] {
			seen[disk] = true
			out = append(out, disk)
		}
		return nil
	}
	for _, source := range sources {
		node := source
		if res != nil {
			if target, err := res.Resolve(source); err == nil && target != "" {
				node = target
			}
			// A failed symlink resolution is not fatal on its own: a plain
			// /dev/sda2 does not need resolving, and splitNode below is what
			// decides whether the name we ended up with is usable.
		}
		if err := walk(node, 0); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// isStacked reports whether base names a device built on top of other devices,
// whose backing disks have to be looked up rather than derived from the name.
// Device-mapper covers LVM, LUKS and multipath; md covers software RAID.
func isStacked(base string) bool {
	return strings.HasPrefix(base, "dm-") || strings.HasPrefix(base, "md")
}

// rootMount returns the source device and filesystem type of the entry for /
// in a /proc/mounts-formatted stream.
func rootMount(r io.Reader) (source, fstype string, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "/" {
			continue
		}
		return fields[0], fields[2], nil
	}
	return "", "", sc.Err()
}
