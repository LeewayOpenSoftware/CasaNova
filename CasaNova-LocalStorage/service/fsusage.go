package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
	"github.com/moby/sys/mountinfo"
)

// ErrUsageUnavailable means the filesystem did not answer within its budget and
// we have no recent value for it either.
//
// It exists so that "this mount did not answer" can never be rendered as "this
// mount holds zero bytes". The two look identical once a failed probe is
// collapsed into a zero-valued struct, and that confusion is what turns a dead
// network share into a storage page that quietly claims the disk is empty.
// Callers must show "unknown", not 0.
var ErrUsageUnavailable = errors.New("filesystem usage unavailable")

// FSUsage is the result of one bounded statfs(2).
type FSUsage struct {
	TotalBytes uint64
	UsedBytes  uint64
	AvailBytes uint64

	// Stale is set when these numbers come from an earlier successful probe
	// because the current one did not answer. The value is still the best
	// information available - far better than zeros - but a caller that renders
	// it must say so, otherwise the operator reads month-old numbers as live.
	Stale bool

	// At is when the numbers were actually measured.
	At time.Time
}

var (
	// localUsageBudget bounds statfs(2) on a locally-backed filesystem. A
	// healthy one answers in microseconds; anything near this budget is a disk
	// in trouble, and we would rather report it degraded than hold an HTTP
	// handler goroutine.
	localUsageBudget = 3 * time.Second

	// remoteUsageBudget bounds the network-backed mounts. They are the ones
	// that hang (a server that stops answering leaves every statfs parked
	// forever), and each stranded probe costs an OS thread that nothing can
	// reclaim - so they get a tighter leash than local disks.
	remoteUsageBudget = 1 * time.Second

	// usageFreshTTL is how long a successful measurement is reused. The storage
	// page and the dashboard widget both poll every few seconds and there are
	// several mount points per box, so without this every poll fired one
	// statfs per mounted filesystem.
	usageFreshTTL = 5 * time.Second

	// usageFailureTTL is the negative cache, and it is the half that actually
	// prevents the leak. The breaker below refuses to stack a *second*
	// concurrent probe of a wedged mount, but as soon as a stranded probe is
	// reaped the next poll would arm another one - so a mount that answers in
	// 30s while the UI polls every 5s still accumulates threads forever. A
	// failure is therefore remembered for a while and answered from memory.
	usageFailureTTL = 15 * time.Second

	// usageStaleGrace is how long the last good measurement may still be served
	// (flagged Stale) after the filesystem stopped answering. Beyond it we
	// admit we do not know, because numbers this old describe a filesystem that
	// has been unreachable for minutes and are no longer evidence of anything.
	usageStaleGrace = 5 * time.Minute

	// usageEntryRetention drops cache entries for mount points nobody has asked
	// about. Mount points come and go (USB drives, temporary shares) over the
	// months this box stays up, and a map keyed by path would otherwise only
	// ever grow.
	usageEntryRetention = 30 * time.Minute
)

// statfsUsage is the raw syscall, isolated as a seam so the timeout and
// degradation paths can be tested without an actual dead NFS server.
var statfsUsage = func(path string) (FSUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSUsage{}, err
	}

	blockSize := uint64(st.Bsize)

	// Same arithmetic df(1) does: "used" counts the blocks the filesystem has
	// spent (including the root-reserved ones), "available" counts only what an
	// unprivileged writer may still use, which is why they do not add up to
	// Total on ext4.
	return FSUsage{
		TotalBytes: st.Blocks * blockSize,
		UsedBytes:  (st.Blocks - st.Bfree) * blockSize,
		AvailBytes: st.Bavail * blockSize,
		At:         time.Now(),
	}, nil
}

// mountEntry is one line of the kernel's mount table, reduced to what the
// storage probes need.
type mountEntry struct {
	Source     string
	FSType     string
	MountPoint string
}

// mountTable reads what is mounted. It is a procfs read (/proc/self/mountinfo
// via moby/sys/mountinfo, the parser this module already depends on) precisely
// because it cannot block: asking the kernel what is mounted is a memory read,
// while asking a mount anything about itself - statfs, stat, df - is a
// round-trip that a dead server never completes. Every decision about which
// filesystems are safe to probe has to be taken from this side of the line.
//
// A seam so tests can present a mount table without mounting anything.
var mountTable = func() ([]mountEntry, error) {
	mounts, err := mountinfo.GetMounts(nil)
	if err != nil {
		return nil, err
	}

	entries := make([]mountEntry, 0, len(mounts))
	for _, m := range mounts {
		entries = append(entries, mountEntry{
			Source:     m.Source,
			FSType:     m.FSType,
			MountPoint: filepath.Clean(m.Mountpoint),
		})
	}

	return entries, nil
}

// remoteFSTypes are the filesystem types whose probes go over a wire and can
// therefore hang forever rather than fail.
//
// This list is deliberately identical in intent to service.UnwalkableMounts in
// the CasaNova module (temp_sweeper.go), which solved the same problem for the
// janitor's directory walk. It is repeated rather than imported because the two
// live in separate Go modules with no shared home for it today: CasaNova-Common
// is the only module both import and it carries no filesystem knowledge. That
// duplication is a known debt - the predicate belongs in CasaNova-Common so
// there is one answer to "is this mount remote".
//
// `fuseblk` is deliberately absent: that is ntfs-3g and friends, a plain local
// disk. autofs is included because touching an autofs path is what triggers the
// mount - the probe itself would cause the very I/O we are trying to bound.
var remoteFSTypes = map[string]bool{
	"9p": true, "afs": true, "afpfs": true, "autofs": true, "beegfs": true,
	"ceph": true, "cifs": true, "coda": true, "davfs": true, "davfs2": true,
	"fuse": true, "glusterfs": true, "lustre": true, "ncpfs": true,
	"nfs": true, "nfs4": true, "orangefs": true, "smb2": true, "smb3": true,
	"smbfs": true,
}

// localFUSETypes are the `fuse.<backend>` mounts backed by a local disk, which
// therefore behave like local filesystems. fuse.mergerfs is the one that
// matters here: it is our own combined-storage pool, i.e. the box's primary
// storage, and treating it as remote would put the main volume on the short
// leash meant for other people's servers. Unknown FUSE backends stay classified
// as remote on purpose - a wire-backed one we have not heard of (juicefs,
// gcsfuse, a new rclone alias) is exactly the case the short budget is for.
var localFUSETypes = map[string]bool{
	"fuse.mergerfs": true,
	"fuse.ntfs-3g":  true, "fuse.ntfs": true, "fuse.ntfs3": true,
	"fuse.exfat": true, "fuse.exfat-fuse": true,
	"fuse.ext4fuse": true, "fuse.lklfuse": true,
	"fuse.hfsfuse": true, "fuse.apfs-fuse": true,
}

func isRemoteFSType(fsType string) bool {
	if strings.HasPrefix(fsType, "fuse.") {
		return !localFUSETypes[fsType]
	}

	return remoteFSTypes[fsType]
}

// owningMount returns the mount table entry that owns path - the longest mount
// point that is a prefix of it - so a path deep inside a share is judged by the
// share it lives on.
func owningMount(path string) (mountEntry, bool) {
	entries, err := mountTable()
	if err != nil {
		return mountEntry{}, false
	}

	var best mountEntry
	found := false

	for _, e := range entries {
		if !isUnder(path, e.MountPoint) {
			continue
		}
		// Longest match wins, and on a tie the later line does: two candidates
		// of equal length that both contain path are the same mount point, and
		// mountinfo is ordered, so the later one is the over-mount that
		// actually serves it.
		if !found || len(e.MountPoint) >= len(best.MountPoint) {
			best = e
			found = true
		}
	}

	return best, found
}

func isUnder(path, mountPoint string) bool {
	if path == mountPoint {
		return true
	}
	if mountPoint == "/" {
		return strings.HasPrefix(path, "/")
	}

	return strings.HasPrefix(path, mountPoint+"/")
}

// usageBudget picks the deadline for a path from the mount table, before any
// syscall touches it. Classifying the mount from procfs is the whole point:
// once we have called statfs on a dead share it is already too late, the thread
// is gone. An unknown mount is treated as remote - the conservative side is the
// short leash, never the long one.
func usageBudget(path string) time.Duration {
	m, ok := owningMount(path)
	if !ok || isRemoteFSType(m.FSType) {
		return remoteUsageBudget
	}

	return localUsageBudget
}

type usageCacheEntry struct {
	good     FSUsage
	goodAt   time.Time // zero ⇒ never measured successfully
	failedAt time.Time // zero ⇒ no recent failure
	failure  error
	touched  time.Time
}

var (
	usageCacheMu sync.Mutex
	usageCache   = map[string]usageCacheEntry{}
)

// MountUsage reports how full a filesystem is, without ever letting one wedged
// filesystem block the caller.
//
// It is the single entry point for filesystem usage in this service. Everything
// that used to call statfs(2)/disk.Usage/df directly from an HTTP handler now
// goes through here, because each of those did the same three things wrong: no
// deadline (a dead CIFS share parks the handler goroutine forever), no rate
// limit (a polled endpoint arms a fresh probe every few seconds and each one
// that never returns strands an OS thread), and no way to say "I do not know"
// (a failed probe became a zero-valued struct, i.e. a filesystem reported as
// empty).
//
// The contract: on success the numbers are fresh or cached; on a filesystem
// that stopped answering the last good numbers come back flagged Stale; and
// when there is nothing trustworthy to report it returns ErrUsageUnavailable.
// It never returns zeros with a nil error.
func MountUsage(path string) (FSUsage, error) {
	key := filepath.Clean(path)
	now := time.Now()

	if usage, err, handled := cachedUsage(key, now); handled {
		return usage, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), usageBudget(key))
	defer cancel()

	// The seam is captured here, in the caller's goroutine; the syscall itself
	// runs inside RunBoundedProbe's goroutine, which is the whole point — an
	// abandoned probe outlives this call, so it must not touch anything the
	// caller also reads.
	probe := statfsUsage

	// RunBoundedProbe returns the moment ctx expires even though the statfs is
	// still parked in the kernel, and its per-key breaker refuses to arm a
	// second probe of the same wedged mount while the first has not returned —
	// which is what stops a polled storage page from stranding one OS thread
	// per poll on a dead share. A bare `probe(key)` here (the previous stub)
	// had neither: a dead CIFS/NFS mount hung this handler goroutine forever,
	// i.e. exactly the failure this file exists to prevent.
	usage, err := command.RunBoundedProbe(ctx, "statfs "+key, func() (FSUsage, error) {
		return probe(key)
	})
	if err != nil {
		return recordUsageFailure(key, err, time.Now())
	}

	return recordUsageSuccess(key, usage, time.Now())
}

// cachedUsage answers from memory when it can. handled=false means the caller
// must go and probe.
func cachedUsage(key string, now time.Time) (usage FSUsage, err error, handled bool) {
	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()

	entry, ok := usageCache[key]
	if !ok {
		return FSUsage{}, nil, false
	}

	entry.touched = now
	usageCache[key] = entry

	if !entry.goodAt.IsZero() && now.Sub(entry.goodAt) < usageFreshTTL {
		return entry.good, nil, true
	}

	if !entry.failedAt.IsZero() && now.Sub(entry.failedAt) < usageFailureTTL {
		u, e := degradedUsage(entry, now)
		return u, e, true
	}

	return FSUsage{}, nil, false
}

func recordUsageSuccess(key string, usage FSUsage, now time.Time) (FSUsage, error) {
	if usage.At.IsZero() {
		usage.At = now
	}

	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()

	usageCache[key] = usageCacheEntry{good: usage, goodAt: now, touched: now}
	pruneUsageCacheLocked(now)

	return usage, nil
}

func recordUsageFailure(key string, cause error, now time.Time) (FSUsage, error) {
	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()

	entry := usageCache[key]
	entry.failedAt = now
	entry.failure = cause
	entry.touched = now
	usageCache[key] = entry
	pruneUsageCacheLocked(now)

	return degradedUsage(entry, now)
}

// degradedUsage is the honest answer for a filesystem that is not responding:
// the last measurement if it is recent enough to still mean something, flagged
// so the caller can say it is stale, and otherwise an explicit "unknown".
func degradedUsage(entry usageCacheEntry, now time.Time) (FSUsage, error) {
	if !entry.goodAt.IsZero() && now.Sub(entry.goodAt) < usageStaleGrace {
		stale := entry.good
		stale.Stale = true
		return stale, nil
	}

	if entry.failure != nil {
		return FSUsage{}, fmt.Errorf("%w: %v", ErrUsageUnavailable, entry.failure)
	}

	return FSUsage{}, ErrUsageUnavailable
}

func pruneUsageCacheLocked(now time.Time) {
	for k, e := range usageCache {
		if now.Sub(e.touched) > usageEntryRetention {
			delete(usageCache, k)
		}
	}
}

// rootMount returns the mount table entry carrying "/". The error case is "we
// could not read the mount table", which is never the same as "there is no root
// filesystem".
func rootMount() (mountEntry, error) {
	entries, err := mountTable()
	if err != nil {
		return mountEntry{}, fmt.Errorf("cannot read the mount table: %w", err)
	}

	found := false
	var root mountEntry

	for _, e := range entries {
		if e.MountPoint == "/" {
			// Last one wins: a later line over-mounts an earlier one.
			root = e
			found = true
		}
	}

	if !found {
		return mountEntry{}, errors.New("no root filesystem in the mount table")
	}

	return root, nil
}
