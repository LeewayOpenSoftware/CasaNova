package devid

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// zfsFSType is the filesystem type /proc/mounts reports for a ZFS dataset.
const zfsFSType = "zfs"

// zpoolTimeout bounds the pool query.
//
// `zpool status` is not guaranteed to return. On a pool with a faulted or
// unreachable vdev -- an iSCSI target that went away, a USB enclosure that
// dropped off -- it can block in the kernel waiting on I/O, and this probe runs
// inside an HTTP handler for POST /v1/storage. Without a bound, the guard that
// protects the boot disk becomes the thing that hangs the request that was
// asking about it.
//
// A timeout is an error, and an error here refuses. That is the right way
// round: a degraded root pool is precisely when the operator is most likely to
// be poking at disks and least able to afford a wrong answer.
const zpoolTimeout = 5 * time.Second

// zfsRoot resolves a root filesystem on ZFS to the disks its pool sits on.
//
// WHY THIS EXISTS. A ZFS root appears in the mount table as a DATASET name --
// `rpool/ROOT/ubuntu_a1b2c3 / zfs rw,...` -- and a dataset name is not a device
// path. It has no /dev prefix, no partition suffix and no node in /sys/block,
// so the block-device walk rejected it and the probe answered RootUnknown.
// Unknown refuses, so the machine was safe but unusable: with / on a pool, the
// format guard protected every disk in the box, including the empty one the
// operator had just plugged in to hold backups. That is the opposite failure
// from the one this package was written to fix, and it is still a failure.
//
// The way out is the same one that fixed LVM and md: stop guessing from the
// name and ask what is underneath. ZFS is the awkward case because the kernel
// publishes nothing -- a pool's membership is not in /sys/block the way an md
// array's is -- so the authority has to be ZFS itself.
func zfsRoot(source string, res Resolver) Root {
	pool := poolName(source)
	if pool == "" {
		return Root{Reason: fmt.Sprintf("/ is a zfs mount whose source %q does not name a pool", source)}
	}
	if res == nil {
		return Root{Reason: fmt.Sprintf("/ is on the zfs pool %q and no resolver was given to list its disks", pool)}
	}

	members, err := res.PoolMembers(pool)
	if err != nil {
		return Root{Reason: fmt.Sprintf("/ is on the zfs pool %q but its disks could not be listed: %v", pool, err)}
	}
	if len(members) == 0 {
		return Root{Reason: fmt.Sprintf("/ is on the zfs pool %q but zfs reported no devices for it", pool)}
	}

	disks, err := disksBehind(members, "the zfs pool "+pool, res)
	if err != nil {
		return Root{Reason: fmt.Sprintf("/ is on the zfs pool %q but the disks behind its devices could not be determined: %v", pool, err)}
	}
	return Root{
		Status: RootOnDisks,
		Disks:  disks,
		Reason: fmt.Sprintf("/ is on the zfs pool %s (dataset %s), carried by %s", pool, source, strings.Join(disks, ", ")),
	}
}

// poolName takes the pool out of a dataset name. `rpool/ROOT/ubuntu_a1b2c3`
// belongs to `rpool`; a bare `rpool` is its own pool's root dataset. A pool
// name cannot contain a slash, so the first component is the whole answer.
func poolName(source string) string {
	pool := source
	if i := strings.IndexByte(pool, '/'); i >= 0 {
		pool = pool[:i]
	}
	// A leading slash would mean this was a path, not a dataset, and a
	// dataset name is never empty or relative-looking.
	if pool == "" || strings.HasPrefix(source, "/") {
		return ""
	}
	return pool
}

// PoolMembers answers from ZFS, by running `zpool status -P` for the one pool
// asked about.
//
// SCOPED TO ONE POOL ON PURPOSE. `zpool status` with no argument reports every
// imported pool, and using that would protect the disks of the data pools too.
// Those are exactly the disks an operator legitimately reformats, and quietly
// refusing them would trade this card's lockout for a smaller one. The pool
// named here is the pool carrying /, and nothing else is the boot disk.
func (s SysResolver) PoolMembers(pool string) ([]string, error) {
	run := s.ZpoolStatus
	if run == nil {
		run = runZpoolStatus
	}
	out, err := run(pool)
	if err != nil {
		return nil, err
	}
	return parseZpoolStatus(out), nil
}

// runZpoolStatus is the real command. -P makes ZFS print vdevs as full paths
// rather than the bare `sda3` it otherwise abbreviates to, which is what makes
// the output resolvable at all.
func runZpoolStatus(pool string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), zpoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "zpool", "status", "-P", pool)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("zpool status -P %s did not return within %s", pool, zpoolTimeout)
	}
	if err != nil {
		// A missing zpool binary lands here too, and it must: a machine whose
		// / is a zfs mount and which cannot run zpool is a machine this probe
		// cannot answer for. Reporting that as "no disks" would allow the
		// format.
		return nil, fmt.Errorf("zpool status -P %s: %w", pool, err)
	}
	return out, nil
}

// parseZpoolStatus pulls the device paths out of `zpool status -P` output.
//
// The output is a tree, indented, with a header block above it and an errors
// line below. Every line that names a device names it as an absolute path
// because of -P; every other row is a pool name, a vdev type (mirror-0,
// raidz1-0), a section label (logs, cache, spares) or prose. So "first field
// starts with a slash" is the whole rule, and it does not need to know the
// shape of the tree -- which is the point, because that shape is the part that
// differs between a mirror, a raidz and a stripe, and the part ZFS is free to
// change.
//
// CACHE, LOG AND SPARE DEVICES ARE INCLUDED, deliberately. A separate log
// device holds writes the pool has acknowledged and not yet committed, so
// formatting one can lose data that / believes is on disk. An L2ARC cache
// device cannot, and a spare is not in use yet. Telling them apart would mean
// tracking the section a row sits under -- more parser, in exchange for
// permission to format a disk that belongs to the root pool. The asymmetry
// this package is built on says no: refusing a cache disk costs an error
// message, and being wrong about a log disk costs the pool.
func parseZpoolStatus(out []byte) []string {
	var (
		devices []string
		seen    = map[string]bool{}
	)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		dev := fields[0]
		if !strings.HasPrefix(dev, "/") || seen[dev] {
			continue
		}
		seen[dev] = true
		devices = append(devices, dev)
	}
	sort.Strings(devices)
	return devices
}
