package v2

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/sqlite"
	model2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2/fs"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2/wrapper"
	"github.com/moby/sys/mountinfo"
	"gotest.tools/v3/assert"
)

// A UUID no block device on the test machine can possibly carry, so
// isVolumeMountedAsRecorded always reports it as missing - the stand-in for a
// disk that has not finished coming up yet.
const absentVolumeUUID = "00000000-0000-4000-8000-b13b13b13b13"

// fakeMountInfo serves an arbitrary mount table, so the assembly logic can be
// driven over states the fixed mock in mount_test.go does not cover (a pool
// mount point carrying our fence, most of all).
type fakeMountInfo struct {
	mounts []mountinfo.Info
}

func (f *fakeMountInfo) GetMounts(filter mountinfo.FilterFunc) ([]*mountinfo.Info, error) {
	results := make([]*mountinfo.Info, 0, len(f.mounts))

	for i := range f.mounts {
		if filter != nil {
			skip, stop := filter(&f.mounts[i])
			if skip {
				if stop {
					break
				}
				continue
			}
			results = append(results, &f.mounts[i])
			if stop {
				break
			}
			continue
		}
		results = append(results, &f.mounts[i])
	}

	return results, nil
}

func newAssemblyTestService(t *testing.T, mounts ...mountinfo.Info) *LocalStorageService {
	t.Helper()

	db := sqlite.GetDBByFile(filepath.Join(t.TempDir(), "local-storage.db"))

	var mountInfo wrapper.MountInfoWrapper = &MountInfoMock{}
	if len(mounts) > 0 {
		mountInfo = &fakeMountInfo{mounts: mounts}
	}

	return NewLocalStorageService(db, mountInfo)
}

// compressAssemblyTimings keeps the wait from costing the suite a real minute.
func compressAssemblyTimings(t *testing.T, wait, poll time.Duration) {
	t.Helper()

	originalWait, originalPoll, originalFloor := mergeAssemblyWaitTimeout, mergeAssemblyPollInterval, mergeAssemblyProbeFloor
	mergeAssemblyWaitTimeout, mergeAssemblyPollInterval, mergeAssemblyProbeFloor = wait, poll, wait
	t.Cleanup(func() {
		mergeAssemblyWaitTimeout, mergeAssemblyPollInterval, mergeAssemblyProbeFloor = originalWait, originalPoll, originalFloor
	})
}

// stubVolumePresence replaces the real block-device probe.
func stubVolumePresence(t *testing.T, present func(v *model2.Volume) bool) {
	t.Helper()

	original := volumePresence
	volumePresence = func(_ context.Context, v *model2.Volume) bool {
		if v == nil {
			return false
		}
		return present(v)
	}
	t.Cleanup(func() { volumePresence = original })
}

// recordFenceCalls captures the mount/umount the fence would perform, so no test
// ever shells out to mount(8).
func recordFenceCalls(t *testing.T) (fenced *[]string, lifted *[]string) {
	t.Helper()

	mounted := []string{}
	unmounted := []string{}

	originalMount, originalUmount := poolFenceMount, poolFenceUmount
	poolFenceMount = func(source, mountPoint string, fstype, options *string) error {
		mounted = append(mounted, source+" -> "+mountPoint)
		return nil
	}
	poolFenceUmount = func(mountPoint string) error {
		unmounted = append(unmounted, mountPoint)
		return nil
	}
	t.Cleanup(func() { poolFenceMount, poolFenceUmount = originalMount, originalUmount })

	return &mounted, &unmounted
}

// A pool whose disks are not all present must be left alone: rewriting its
// branches would drop the absent disk out of /DATA. Assembly always goes through
// buildSources, which creates the pool's base path, so a base path that was
// never created is proof that no assembly was attempted.
func TestCheckMergeMountLeavesPoolAloneWhenAVolumeIsMissing(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 0, time.Millisecond)
	recordFenceCalls(t)

	poolMountPoint := filepath.Join(t.TempDir(), "DATA")
	sourceBasePath := filepath.Join(t.TempDir(), "files")

	merge := model2.Merge{
		FSType:         fs.MergerFSFullName,
		MountPoint:     poolMountPoint,
		SourceBasePath: &sourceBasePath,
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: filepath.Join(t.TempDir(), "Storage_1")},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	service.CheckMergeMount()

	_, err := os.Stat(sourceBasePath)
	assert.Assert(t, os.IsNotExist(err), "pool was assembled from an incomplete set of disks (base path %s was created)", sourceBasePath)
}

// Refusing to assemble is not enough on its own: nothing orders docker behind
// this service, so an unassembled /DATA is a plain rootfs directory that
// containers happily bind-mount and fill. The mount point must be fenced so
// those writes fail instead of silently landing on the system disk.
func TestCheckMergeMountFencesTheMountPointWhenVolumesAreMissing(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 0, time.Millisecond)
	fenced, _ := recordFenceCalls(t)
	stubVolumePresence(t, func(*model2.Volume) bool { return false })

	poolMountPoint := filepath.Join(t.TempDir(), "DATA")

	merge := model2.Merge{
		FSType:     fs.MergerFSFullName,
		MountPoint: poolMountPoint,
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: filepath.Join(t.TempDir(), "Storage_1")},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	service.CheckMergeMount()

	assert.DeepEqual(t, *fenced, []string{poolFenceSource + " -> " + poolMountPoint})
}

// The fence exists only for a pool the database says should exist and whose
// disks are absent. A healthy pool - and a fresh install, whose merge has no
// source volumes at all - must never be fenced, or the guard becomes the outage.
func TestCheckMergeMountDoesNotFenceAPoolWithNoMissingVolumes(t *testing.T) {
	poolMountPoint := filepath.Join(t.TempDir(), "DATA")

	service := newAssemblyTestService(t, mountinfo.Info{
		Mountpoint: poolMountPoint,
		FSType:     fs.MergerFSFullName,
		Source:     "/var/lib/casaos/files",
	})
	compressAssemblyTimings(t, 0, time.Millisecond)
	fenced, _ := recordFenceCalls(t)

	sourceBasePath := "/var/lib/casaos/files"
	merge := model2.Merge{
		FSType:         fs.MergerFSFullName,
		MountPoint:     poolMountPoint,
		SourceBasePath: &sourceBasePath,
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	service.CheckMergeMount()

	assert.Equal(t, len(*fenced), 0, "a pool with every expected volume present was fenced: %v", *fenced)
}

// The fence is temporary by construction: once the disks are back it has to be
// lifted, otherwise a mount point that was fenced during one bad boot could
// never carry the pool again.
func TestCheckMergeMountLiftsTheFenceWhenVolumesReturn(t *testing.T) {
	poolMountPoint := filepath.Join(t.TempDir(), "DATA")

	service := newAssemblyTestService(t, mountinfo.Info{
		Mountpoint: poolMountPoint,
		FSType:     poolFenceFSType,
		Source:     poolFenceSource,
	})
	compressAssemblyTimings(t, 0, time.Millisecond)
	_, lifted := recordFenceCalls(t)
	stubVolumePresence(t, func(*model2.Volume) bool { return true })

	// A leftover entry under the fenced mount point stops CreateMerge before it
	// can shell out to mount(8) - the same state a real fenced /DATA is in.
	assert.NilError(t, os.MkdirAll(filepath.Join(poolMountPoint, "AppData"), 0o755))

	merge := model2.Merge{
		FSType:     fs.MergerFSFullName,
		MountPoint: poolMountPoint,
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: filepath.Join(t.TempDir(), "Storage_1")},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	service.CheckMergeMount()

	assert.DeepEqual(t, *lifted, []string{poolMountPoint})
}

// The guard must fire on missing disks only. A merge with no source volumes at
// all (a fresh install, whose pool is only the rootfs base path) is a complete
// snapshot and has to stay assemblable.
func TestWaitForMergeSourceVolumesAcceptsCompleteSnapshot(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 0, time.Millisecond)

	sourceBasePath := "/var/lib/casaos/files"
	merge := model2.Merge{FSType: fs.MergerFSFullName, MountPoint: "/DATA", SourceBasePath: &sourceBasePath}
	assert.NilError(t, service._db.Create(&merge).Error)

	merges, missingByMerge, err := service.waitForMergeSourceVolumes()

	assert.NilError(t, err)
	assert.Equal(t, len(merges), 1)
	assert.Equal(t, len(missingByMerge), 0)
}

// Disks routinely show up seconds after this service does, so the wait has to
// be a wait and not a single look.
func TestWaitForMergeSourceVolumesRetriesUntilDeadline(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 150*time.Millisecond, 25*time.Millisecond)
	stubVolumePresence(t, func(*model2.Volume) bool { return false })

	merge := model2.Merge{
		FSType:     fs.MergerFSFullName,
		MountPoint: "/DATA",
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: "/DATA/Storage_1"},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	start := time.Now()
	_, missingByMerge, err := service.waitForMergeSourceVolumes()
	elapsed := time.Since(start)

	assert.NilError(t, err)
	assert.Equal(t, len(missingByMerge), 1)
	assert.Equal(t, len(missingByMerge[0]), 1)
	assert.Assert(t, elapsed >= 150*time.Millisecond, "gave up after %s without waiting out the deadline", elapsed)
}

// The wait must re-read the database on every pass. casanova-local-storage-first
// is not ordered before this service: while we poll it can remount a disk on a
// different mount point and record that in the database. Polling a frozen
// snapshot compares against the stale mount point forever, so the pool is
// rejected - and now fenced - because of the very race the wait exists to cover.
func TestWaitForMergeSourceVolumesRereadsTheDatabaseEachPass(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 2*time.Second, time.Millisecond)

	recordedMountPoint := "/DATA/Storage_1"
	relocatedMountPoint := "/DATA/Storage_1-1"

	merge := model2.Merge{
		FSType:     fs.MergerFSFullName,
		MountPoint: "/DATA",
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: recordedMountPoint},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	probes := 0
	stubVolumePresence(t, func(v *model2.Volume) bool {
		probes++
		if probes == 1 {
			// The oneshot remounts the disk elsewhere and updates the database,
			// exactly as it does when the recorded mount point is occupied.
			assert.NilError(t, service._db.Model(&model2.Volume{}).
				Where("uuid = ?", absentVolumeUUID).
				Update("mount_point", relocatedMountPoint).Error)
			return false
		}
		return v.MountPoint == relocatedMountPoint
	})

	merges, missingByMerge, err := service.waitForMergeSourceVolumes()

	assert.NilError(t, err)
	assert.Equal(t, len(missingByMerge), 0, "the wait kept probing a stale snapshot instead of re-reading the database")
	assert.Equal(t, merges[0].SourceVolumes[0].MountPoint, relocatedMountPoint)
}

// A nil entry is a volume we know nothing about, which is the same as a disk we
// cannot find: it must count as missing (and must not panic on the way there).
func TestPartitionVolumesByPresenceTreatsNilVolumeAsMissing(t *testing.T) {
	present, missing := partitionVolumesByPresence(context.Background(), []*model2.Volume{nil})

	assert.Equal(t, len(present), 0)
	assert.Equal(t, len(missing), 1)
	assert.DeepEqual(t, describeVolumes(missing), []string{"<nil volume reference>"})
}

// The absolute deadline has to cut a probe that is already running, not merely
// decide between passes: with several volumes, a per-probe budget stacks into
// minutes and against a wedged disk it never ends at all.
func TestWaitForMergeSourceVolumesCutsOffProbesAtTheDeadline(t *testing.T) {
	service := newAssemblyTestService(t)
	compressAssemblyTimings(t, 100*time.Millisecond, time.Millisecond)

	merge := model2.Merge{
		FSType:     fs.MergerFSFullName,
		MountPoint: "/DATA",
		SourceVolumes: []*model2.Volume{
			{UUID: absentVolumeUUID, MountPoint: "/DATA/Storage_1"},
			{UUID: "11111111-1111-4111-8111-111111111111", MountPoint: "/DATA/Storage_2"},
		},
	}
	assert.NilError(t, service._db.Create(&merge).Error)

	original := volumePresence
	volumePresence = func(ctx context.Context, _ *model2.Volume) bool {
		// A probe that only ever returns because the caller's deadline reaches
		// into it - a drive parked in uninterruptible I/O.
		<-ctx.Done()
		return false
	}
	t.Cleanup(func() { volumePresence = original })

	start := time.Now()
	_, missingByMerge, err := service.waitForMergeSourceVolumes()
	elapsed := time.Since(start)

	assert.NilError(t, err)
	assert.Equal(t, len(missingByMerge), 1)
	assert.Assert(t, elapsed < 2*time.Second, "the wait ran for %s: its deadline does not reach into a running probe", elapsed)
}
