package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/cache"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/sqlite"
	model2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
	"gotest.tools/v3/assert"
)

// The boot-time sweep is the whole body of the -init oneshot docker is ordered
// behind, so a sweep that never returns used to mean a box that never finished
// booting. The caller has to be able to walk away from it.
func TestRunDiskSweepWithDeadlineGivesUpOnStuckSweep(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	finished, err := RunDiskSweepWithDeadline(func() error { <-release; return nil }, 100*time.Millisecond)
	elapsed := time.Since(start)

	assert.Assert(t, !finished, "a sweep that never returned must be reported as unfinished")
	assert.NilError(t, err)
	assert.Assert(t, elapsed < 5*time.Second, "waited %s on a sweep that never returns", elapsed)
}

func TestRunDiskSweepWithDeadlineReportsCompletion(t *testing.T) {
	swept := false

	finished, err := RunDiskSweepWithDeadline(func() error { swept = true; return nil }, 5*time.Second)

	assert.Assert(t, finished)
	assert.NilError(t, err)
	assert.Assert(t, swept)
}

// A sweep that finished but failed is not a successful boot. The oneshot decides
// its exit code from this error, so swallowing it here is what would let the box
// report a clean storage start with nothing mounted.
func TestRunDiskSweepWithDeadlinePropagatesSweepFailure(t *testing.T) {
	sweepErr := errors.New("nothing could be enumerated")

	finished, err := RunDiskSweepWithDeadline(func() error { return sweepErr }, 5*time.Second)

	assert.Assert(t, finished)
	assert.Assert(t, errors.Is(err, sweepErr), "the sweep failure must reach the caller, got %v", err)
}

// The core invariant of the boot sweep: a failure to enumerate is not an empty
// machine. If this ever returns (nil, nil) again, CheckSerialDiskMount walks an
// empty list, mounts nothing, reports success, and docker starts on an empty
// /DATA.
func TestEnumerateDisksWithRetryFailsWhenEnumerationNeverSucceeds(t *testing.T) {
	attempts := 0
	slept := 0

	list, err := enumerateDisksWithRetry(
		func() ([]model.LSBLKModel, error) {
			attempts++
			return nil, errors.New("lsblk: context deadline exceeded")
		},
		true,
		4,
		time.Millisecond,
		func(time.Duration) { slept++ },
	)

	assert.Assert(t, err != nil, "a failed enumeration must be an error, never an empty disk list")
	assert.Assert(t, errors.Is(err, ErrDiskEnumerationFailed), "expected ErrDiskEnumerationFailed, got %v", err)
	assert.Assert(t, list == nil)
	assert.Equal(t, attempts, 4, "the enumeration must be retried within its budget")
	assert.Equal(t, slept, 3, "retries must back off between attempts")
}

// Disks routinely take tens of seconds to settle after a cold boot. A single
// failed look must not condemn the sweep.
func TestEnumerateDisksWithRetryRecoversFromTransientFailure(t *testing.T) {
	attempts := 0
	expected := []model.LSBLKModel{{Path: "/dev/sda"}}

	list, err := enumerateDisksWithRetry(
		func() ([]model.LSBLKModel, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("not settled yet")
			}
			return expected, nil
		},
		true,
		5,
		time.Millisecond,
		func(time.Duration) {},
	)

	assert.NilError(t, err)
	assert.Equal(t, len(list), 1)
	assert.Equal(t, attempts, 3)
}

// lsblk answering "no block devices at all" on a box whose database holds
// volumes is not an answer, it is a broken probe: believing it skips the remount
// of every disk.
func TestEnumerateDisksWithRetryRejectsEmptyInventoryWhenVolumesAreExpected(t *testing.T) {
	list, err := enumerateDisksWithRetry(
		func() ([]model.LSBLKModel, error) { return []model.LSBLKModel{}, nil },
		true,
		2,
		time.Millisecond,
		func(time.Duration) {},
	)

	assert.Assert(t, errors.Is(err, ErrDiskEnumerationFailed), "an empty inventory on a box with recorded volumes must fail, got %v", err)
	assert.Assert(t, list == nil)
}

// ...but a machine with no volumes recorded (a fresh install, or one with only
// the system disk) legitimately enumerates nothing to remount, and must not be
// turned into a failed boot.
func TestEnumerateDisksWithRetryAcceptsEmptyInventoryWhenNoVolumesAreExpected(t *testing.T) {
	attempts := 0

	list, err := enumerateDisksWithRetry(
		func() ([]model.LSBLKModel, error) {
			attempts++
			return []model.LSBLKModel{}, nil
		},
		false,
		3,
		time.Millisecond,
		func(time.Duration) {},
	)

	assert.NilError(t, err)
	assert.Equal(t, len(list), 0)
	assert.Equal(t, attempts, 1, "a valid empty inventory must be accepted on the first look")
}

// One dying disk must not cost the healthy ones their mounts: enabling S.M.A.R.T.
// is telemetry, and its failure - timeout included - may not cut the sweep short.
func TestSweepDisksContinuesAfterSMARTFailure(t *testing.T) {
	list := []model.LSBLKModel{
		{Path: "/dev/sdb", Children: []model.LSBLKModel{{Path: "/dev/sdb1", UUID: "uuid-b"}}},
		{Path: "/dev/sda", Children: []model.LSBLKModel{{Path: "/dev/sda1", UUID: "uuid-a"}}},
	}

	mountPointMap := map[string]string{
		"uuid-a": "/DATA/Storage_1",
		"uuid-b": "/DATA/Storage_2",
	}

	remounted := []string{}

	sweepDisks(
		list,
		mountPointMap,
		func(path string) ([]byte, error) {
			if path == "/dev/sdb" {
				return nil, context.DeadlineExceeded
			}
			return []byte("ok"), nil
		},
		func(child model.LSBLKModel, recordedMountPoint string) {
			remounted = append(remounted, child.Path+"->"+recordedMountPoint)
		},
	)

	assert.DeepEqual(t, remounted, []string{"/dev/sdb1->/DATA/Storage_2", "/dev/sda1->/DATA/Storage_1"})
}

// A partition already sitting on its recorded mount point must be left alone -
// remounting it would umount a live filesystem for no reason.
func TestSweepDisksSkipsVolumesAlreadyMountedWhereRecorded(t *testing.T) {
	list := []model.LSBLKModel{
		{Path: "/dev/sda", Children: []model.LSBLKModel{{Path: "/dev/sda1", UUID: "uuid-a", MountPoint: "/DATA/Storage_1"}}},
	}

	remounted := 0

	sweepDisks(
		list,
		map[string]string{"uuid-a": "/DATA/Storage_1"},
		func(string) ([]byte, error) { return []byte("ok"), nil },
		func(model.LSBLKModel, string) { remounted++ },
	)

	assert.Equal(t, remounted, 0)
}

// The wiring that matters at boot: when the inventory cannot be obtained, the
// sweep must abort with an error and remount NOTHING - not walk an empty list,
// report success and let docker start on top of an unmounted /DATA.
func TestCheckSerialDiskMountAbortsWhenDisksCannotBeEnumerated(t *testing.T) {
	d := newSweepTestService(t)
	assert.NilError(t, d.SaveMountPointToDB(model2.Volume{UUID: "uuid-a", MountPoint: "/DATA/Storage_1"}))

	compressEnumerationRetries(t, 3, time.Millisecond)

	attempts := 0
	stubDiskEnumerator(t, func(*diskService) ([]model.LSBLKModel, error) {
		attempts++
		return nil, errors.New("lsblk: context deadline exceeded")
	})

	err := d.CheckSerialDiskMount()

	assert.Assert(t, errors.Is(err, ErrDiskEnumerationFailed), "a sweep that could not enumerate disks must fail loudly, got %v", err)
	assert.Equal(t, attempts, 3, "the enumeration must be retried before giving up")
}

// The same wiring in the healthy direction: a machine whose database holds no
// volumes has nothing to remount and must report a clean sweep.
func TestCheckSerialDiskMountSucceedsOnAMachineWithNoRecordedVolumes(t *testing.T) {
	d := newSweepTestService(t)

	compressEnumerationRetries(t, 2, time.Millisecond)
	stubDiskEnumerator(t, func(*diskService) ([]model.LSBLKModel, error) {
		return []model.LSBLKModel{}, nil
	})

	assert.NilError(t, d.CheckSerialDiskMount())
}

func newSweepTestService(t *testing.T) *diskService {
	t.Helper()

	// The package-level cache is wired up by main(); the sweep touches it, so a
	// test that drives the sweep has to provide it.
	if Cache == nil {
		Cache = cache.Init()
	}

	return &diskService{db: sqlite.GetDBByFile(filepath.Join(t.TempDir(), "local-storage.db"))}
}

func stubDiskEnumerator(t *testing.T, enumerate func(*diskService) ([]model.LSBLKModel, error)) {
	t.Helper()

	original := diskEnumerator
	diskEnumerator = enumerate
	t.Cleanup(func() { diskEnumerator = original })
}

func compressEnumerationRetries(t *testing.T, attempts int, delay time.Duration) {
	t.Helper()

	originalAttempts, originalDelay := serialDiskEnumerationAttempts, serialDiskEnumerationBaseDelay
	serialDiskEnumerationAttempts, serialDiskEnumerationBaseDelay = attempts, delay
	t.Cleanup(func() {
		serialDiskEnumerationAttempts, serialDiskEnumerationBaseDelay = originalAttempts, originalDelay
	})
}
