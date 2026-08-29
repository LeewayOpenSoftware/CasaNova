package service

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

// resetUsageState clears the memoised probe results between tests. The cache is
// process-wide by design (it is what keeps a polled endpoint from re-arming a
// probe of a wedged mount), so tests have to start from a known state.
func resetUsageState(t *testing.T) {
	t.Helper()

	clear := func() {
		usageCacheMu.Lock()
		defer usageCacheMu.Unlock()

		usageCache = map[string]usageCacheEntry{}
	}

	clear()
	t.Cleanup(clear)
}

// shrinkUsageBudgets compresses the production deadlines so a test proving the
// timeout fires does not take seconds.
func shrinkUsageBudgets(t *testing.T, d time.Duration) {
	t.Helper()

	originalLocal, originalRemote := localUsageBudget, remoteUsageBudget
	localUsageBudget, remoteUsageBudget = d, d
	t.Cleanup(func() { localUsageBudget, remoteUsageBudget = originalLocal, originalRemote })
}

// stubMountTable presents a mount table without mounting anything.
func stubMountTable(t *testing.T, entries ...mountEntry) {
	t.Helper()

	original := mountTable
	mountTable = func() ([]mountEntry, error) { return entries, nil }
	t.Cleanup(func() { mountTable = original })
}

// wedgedStatfs stands in for a filesystem that stopped answering: statfs(2) on a
// dead NFS/CIFS mount never returns and cannot be interrupted, so the stub
// blocks until the test is over rather than returning an error. Every call is
// counted, because "how many probes did one wedged mount cost us" is the whole
// question.
func wedgedStatfs(t *testing.T) *atomic.Int32 {
	t.Helper()

	var (
		calls   atomic.Int32
		release = make(chan struct{})
	)

	original := statfsUsage
	statfsUsage = func(string) (FSUsage, error) {
		calls.Add(1)

		<-release

		return FSUsage{}, errors.New("filesystem never answered")
	}

	t.Cleanup(func() {
		statfsUsage = original
		close(release)
	})

	return &calls
}

// A probe of a filesystem that never answers has to give the caller back its
// goroutine. Before the bound, `df`/statfs on a dead mount held the HTTP handler
// for the life of the process.
//
// Mutation that must fail this test: call statfsUsage(key) directly in
// MountUsage instead of going through the bounded probe.
func TestMountUsageGivesUpOnUnresponsiveFilesystem(t *testing.T) {
	resetUsageState(t)
	shrinkUsageBudgets(t, 200*time.Millisecond)
	stubMountTable(t, mountEntry{Source: "/dev/sda1", FSType: "ext4", MountPoint: "/"})
	wedgedStatfs(t)

	done := make(chan error, 1)
	go func() {
		_, err := MountUsage("/mnt/wedged-gives-up")
		done <- err
	}()

	select {
	case err := <-done:
		assert.Assert(t, err != nil, "a filesystem that never answered must not report success")
	case <-time.After(5 * time.Second):
		t.Fatal("MountUsage never returned - the probe is unbounded again")
	}
}

// The golden rule: a probe that did not answer is not an empty filesystem. Zero
// bytes with a nil error is indistinguishable from a mount holding nothing, and
// that is exactly how an unreachable share was rendered as a disk with no data
// on it.
//
// Mutation that must fail this test: return FSUsage{}, nil on the failure path
// of MountUsage (or drop ErrUsageUnavailable from the wrapping).
func TestMountUsageReportsUnknownRatherThanEmpty(t *testing.T) {
	resetUsageState(t)
	shrinkUsageBudgets(t, 200*time.Millisecond)
	stubMountTable(t, mountEntry{Source: "//nas/share", FSType: "cifs", MountPoint: "/mnt/unknown-not-empty"})
	wedgedStatfs(t)

	usage, err := MountUsage("/mnt/unknown-not-empty")

	assert.Assert(t, errors.Is(err, ErrUsageUnavailable), "expected ErrUsageUnavailable, got %v", err)
	assert.Equal(t, usage.TotalBytes, uint64(0))
	assert.Equal(t, usage.UsedBytes, uint64(0))
	assert.Equal(t, usage.AvailBytes, uint64(0))
}

// This is the leak the previous round of bounding introduced: the deadline let
// the caller go, but the expired probe stayed parked in the syscall - with the
// OS thread under it - and the storage page polls, so each poll armed another
// one. A single dead share turned into one stranded thread every few seconds
// for as long as the box ran.
//
// The negative cache alone does not close it: that entry expires every few
// seconds by design, and the mount is still parked when it does. What must stop
// the next poll from arming probe number two is the breaker - one outstanding
// stalled probe per mount point, refused until it finally returns. The test
// therefore expires the negative cache before every poll, which is what a box
// running for months does thousands of times a day.
//
// Mutation that must fail this test: raise maxStalledProbesPerKey, or bypass
// RunBoundedProbe - the stub then counts one stranded probe per poll.
func TestMountUsageDoesNotRelaunchProbeOfAWedgedFilesystem(t *testing.T) {
	resetUsageState(t)
	shrinkUsageBudgets(t, 100*time.Millisecond)
	stubMountTable(t, mountEntry{Source: "//nas/share", FSType: "cifs", MountPoint: "/mnt/no-relaunch"})
	calls := wedgedStatfs(t)

	for poll := 0; poll < 8; poll++ {
		_, err := MountUsage("/mnt/no-relaunch")
		assert.Assert(t, err != nil, "poll %d unexpectedly succeeded", poll)
		expireUsageFailure(t, "/mnt/no-relaunch")
	}

	assert.Equal(t, calls.Load(), int32(1),
		"a wedged filesystem was probed %d times - each expired probe strands a thread that nothing reclaims", calls.Load())
}

// expireUsageFailure backdates the remembered failure so the next call would
// probe again, standing in for the passage of usageFailureTTL.
func expireUsageFailure(t *testing.T, key string) {
	t.Helper()

	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()

	entry := usageCache[key]
	entry.failedAt = time.Now().Add(-usageFailureTTL - time.Second)
	usageCache[key] = entry
}

// The other half of the failure story, and the one the breaker does not cover:
// a dead network mount very often does not hang at all, it fails immediately
// (ESTALE, ENOTCONN, EHOSTDOWN). Nothing stalls, so no breaker trips, and a
// polled endpoint would issue a fresh syscall per mount per poll forever. The
// failure has to be remembered for a while, like the successful value is.
//
// Mutation that must fail this test: remove the failure branch from
// cachedUsage, or stop recording it in recordUsageFailure.
func TestMountUsageRemembersAFastFailureBetweenPolls(t *testing.T) {
	resetUsageState(t)
	shrinkUsageBudgets(t, time.Second)
	stubMountTable(t, mountEntry{Source: "//nas/gone", FSType: "cifs", MountPoint: "/mnt/fast-failure"})

	var calls atomic.Int32

	original := statfsUsage
	statfsUsage = func(string) (FSUsage, error) {
		calls.Add(1)

		return FSUsage{}, errors.New("stale file handle")
	}
	t.Cleanup(func() { statfsUsage = original })

	for poll := 0; poll < 8; poll++ {
		_, err := MountUsage("/mnt/fast-failure")
		assert.Assert(t, errors.Is(err, ErrUsageUnavailable), "poll %d: %v", poll, err)
	}

	assert.Equal(t, calls.Load(), int32(1),
		"a filesystem that fails fast was probed %d times over 8 polls - the failure is not being remembered", calls.Load())
}

// A filesystem that answered a minute ago and has gone quiet since is better
// described by its last measurement than by "unknown" - as long as the caller is
// told the numbers are not current.
//
// Mutation that must fail this test: drop the Stale flag, or return
// ErrUsageUnavailable while a recent good value exists.
func TestMountUsageServesLastGoodValueAsStale(t *testing.T) {
	resetUsageState(t)
	shrinkUsageBudgets(t, 100*time.Millisecond)
	stubMountTable(t, mountEntry{Source: "/dev/sdb1", FSType: "ext4", MountPoint: "/mnt/stale"})

	original := statfsUsage
	statfsUsage = func(string) (FSUsage, error) {
		return FSUsage{TotalBytes: 1 << 40, UsedBytes: 1 << 39, AvailBytes: 1 << 39, At: time.Now()}, nil
	}
	t.Cleanup(func() { statfsUsage = original })

	first, err := MountUsage("/mnt/stale")
	assert.NilError(t, err)
	assert.Assert(t, !first.Stale)

	// The filesystem goes away, and the fresh window expires.
	wedgedStatfs(t)
	expireUsageFreshness(t, "/mnt/stale")

	stale, err := MountUsage("/mnt/stale")
	assert.NilError(t, err, "a mount with a recent measurement must not be reported as unknown")
	assert.Assert(t, stale.Stale, "stale numbers were served without being flagged as stale")
	assert.Equal(t, stale.UsedBytes, first.UsedBytes)
}

// expireUsageFreshness backdates the cached measurement so the next call has to
// probe, without making the test sleep for the real TTL.
func expireUsageFreshness(t *testing.T, key string) {
	t.Helper()

	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()

	entry := usageCache[key]
	entry.goodAt = time.Now().Add(-usageFreshTTL - time.Second)
	usageCache[key] = entry
}

// Remote mounts are classified from the kernel's mount table - a memory read -
// before anything touches them, and an unrecognised mount is treated as remote.
// Deciding after the fact is useless: by then the thread is already gone.
//
// Mutation that must fail this test: return localUsageBudget unconditionally
// from usageBudget.
func TestUsageBudgetIsDecidedFromTheMountTable(t *testing.T) {
	stubMountTable(t,
		mountEntry{Source: "/dev/sda1", FSType: "ext4", MountPoint: "/"},
		mountEntry{Source: "//nas/media", FSType: "cifs", MountPoint: "/mnt/media"},
		mountEntry{Source: "pool", FSType: "fuse.mergerfs", MountPoint: "/DATA"},
	)

	assert.Equal(t, usageBudget("/var/lib"), localUsageBudget, "a path on the local root got the remote budget")
	assert.Equal(t, usageBudget("/DATA/movies"), localUsageBudget, "our own mergerfs pool is local storage, not somebody's share")
	assert.Equal(t, usageBudget("/mnt/media"), remoteUsageBudget, "a CIFS share got the local budget")
	assert.Equal(t, usageBudget("/mnt/media/tv"), remoteUsageBudget, "a path inside a CIFS share got the local budget")
}

func TestUsageBudgetTreatsUnknownMountsAsRemote(t *testing.T) {
	original := mountTable
	mountTable = func() ([]mountEntry, error) { return nil, errors.New("cannot read the mount table") }
	t.Cleanup(func() { mountTable = original })

	assert.Equal(t, usageBudget("/mnt/whatever"), remoteUsageBudget,
		"with no mount table the conservative budget is the short one")
}

// GetSystemDf must look at the root filesystem and nothing else. It used to run
// `df -kPT`, which statfs(2)s every mount on the machine before printing a
// single line, and then discarded every row but "/" - so one unreachable share
// hung a call that had no business touching that share at all.
//
// Mutation that must fail this test: probe every entry of the mount table (what
// `df` with no argument does) instead of only "/".
func TestGetSystemDfProbesOnlyTheRootFilesystem(t *testing.T) {
	resetUsageState(t)
	stubMountTable(t,
		mountEntry{Source: "/dev/sda2", FSType: "ext4", MountPoint: "/"},
		mountEntry{Source: "//nas/dead", FSType: "cifs", MountPoint: "/mnt/dead"},
		mountEntry{Source: "server:/export", FSType: "nfs4", MountPoint: "/mnt/alsodead"},
	)

	var (
		mu     sync.Mutex
		probed []string
	)

	original := systemUsageProbe
	systemUsageProbe = func(path string) (FSUsage, error) {
		mu.Lock()
		probed = append(probed, path)
		mu.Unlock()

		return FSUsage{TotalBytes: 100, UsedBytes: 40, AvailBytes: 60, At: time.Now()}, nil
	}
	t.Cleanup(func() { systemUsageProbe = original })

	d := &diskService{}

	df, err := d.GetSystemDf()
	assert.NilError(t, err)
	assert.Equal(t, df.FileSystem, "/dev/sda2")
	assert.Equal(t, df.Type, "ext4")
	assert.Equal(t, df.MountedOn, "/")
	assert.Equal(t, strings.Join(probed, ","), "/",
		"the root filesystem report touched filesystems it was not asked about: %v", probed)
}

// A root filesystem that is slow to answer must not also erase the facts we
// already have. GetStorageList needs only the device carrying "/" to mark the
// system disk, and blanking it there means the storage page can no longer tell
// the system disk from a data disk.
//
// Mutation that must fail this test: return model.DFDiskSpace{} on the usage
// failure path of GetSystemDf.
func TestGetSystemDfKeepsRootIdentityWhenUsageIsUnavailable(t *testing.T) {
	resetUsageState(t)
	stubMountTable(t, mountEntry{Source: "/dev/sda2", FSType: "ext4", MountPoint: "/"})

	original := systemUsageProbe
	systemUsageProbe = func(string) (FSUsage, error) { return FSUsage{}, ErrUsageUnavailable }
	t.Cleanup(func() { systemUsageProbe = original })

	d := &diskService{}

	df, err := d.GetSystemDf()
	assert.Assert(t, err != nil, "unavailable usage must be reported, not swallowed")
	assert.Equal(t, df.FileSystem, "/dev/sda2", "the root device is known from procfs and must survive a failed statfs")
	assert.Equal(t, df.Blocks, "", "no numbers must be invented when the probe failed")
}

// Not being able to read the mount table is "we do not know", never "there is
// no root filesystem".
func TestGetSystemDfFailsLoudlyWhenTheMountTableIsUnreadable(t *testing.T) {
	resetUsageState(t)

	original := mountTable
	mountTable = func() ([]mountEntry, error) { return nil, errors.New("procfs unreadable") }
	t.Cleanup(func() { mountTable = original })

	d := &diskService{}

	_, err := d.GetSystemDf()
	assert.Assert(t, err != nil, "an unreadable mount table must not be reported as a successful empty answer")
}

// Everything above drives the seams. These two exercise the real
// implementations behind them, so a stubbed-out test suite cannot stay green
// over a mount-table parser or a statfs wrapper that does not actually work.
func TestRootMountReadsTheRealMountTable(t *testing.T) {
	if _, err := os.Stat("/proc/self/mountinfo"); err != nil {
		t.Skip("no /proc/self/mountinfo on this host")
	}

	root, err := rootMount()
	assert.NilError(t, err)
	assert.Assert(t, root.Source != "", "the real mount table produced no source device for /")
	assert.Assert(t, root.FSType != "", "the real mount table produced no filesystem type for /")
}

func TestStatfsUsageMeasuresARealFilesystem(t *testing.T) {
	usage, err := statfsUsage(t.TempDir())
	assert.NilError(t, err)
	assert.Assert(t, usage.TotalBytes > 0, "a real filesystem reported zero total bytes")
	assert.Assert(t, usage.UsedBytes+usage.AvailBytes <= usage.TotalBytes,
		"used+available exceeds the total - the block arithmetic is wrong")
}

// The percentage keeps df's semantics: rounded up, so a filesystem with bytes
// on it never reads as 0%, and "-" for one that reports no usable space rather
// than a fabricated 0%.
func TestFormatUsePercentMatchesDF(t *testing.T) {
	assert.Equal(t, formatUsePercent(FSUsage{UsedBytes: 0, AvailBytes: 100}), "0%")
	assert.Equal(t, formatUsePercent(FSUsage{UsedBytes: 1, AvailBytes: 999}), "1%")
	assert.Equal(t, formatUsePercent(FSUsage{UsedBytes: 50, AvailBytes: 50}), "50%")
	assert.Equal(t, formatUsePercent(FSUsage{UsedBytes: 100, AvailBytes: 0}), "100%")
	assert.Equal(t, formatUsePercent(FSUsage{}), "-")
}
