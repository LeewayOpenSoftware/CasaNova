package v2

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/mergerfs"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/mount"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/partition"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
	model2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

var (
	ErrMergeMountPointAlreadyExists  = errors.New("merge mount point already exists")
	ErrMergeMountPointDoesNotExist   = errors.New("merge mount point does not exist")
	ErrMergeMountPointSourceConflict = errors.New("source mount point should not be a child path of the merge mount point")
	ErrNilReference                  = errors.New("reference is nil")
)

var (
	// How long pool assembly waits for the disks the database expects before
	// it gives up on them. Sized for the boot race it exists to cover: USB and
	// slow SATA enumeration plus the remount oneshot routinely land a disk
	// tens of seconds after this service starts. This is an ABSOLUTE budget:
	// the probes of every pass run under one deadline computed at entry, so a
	// blkid or partx stuck on a dying disk is cut off mid-flight instead of
	// stretching the wait to minutes. Variables rather than constants so tests
	// can compress them.
	mergeAssemblyWaitTimeout  = 60 * time.Second
	mergeAssemblyPollInterval = 3 * time.Second

	// mergeAssemblyProbeFloor guarantees at least one honest probing pass even
	// when the wait budget is tiny: deciding a disk is missing because the
	// budget expired before its probe could run would fence a perfectly healthy
	// pool.
	mergeAssemblyProbeFloor = 15 * time.Second

	// mergeProbeTimeout bounds the presence probes on the paths that are not
	// the assembly loop (the InitMerge HTTP handler, GetMerges). Those used to
	// inherit the unbounded ExecuteCommand, so one wedged disk pinned a server
	// goroutine for the life of the process, once per admin retry.
	mergeProbeTimeout = 20 * time.Second
)

// The fence installed on a pool mount point whose disks did not show up.
//
// Refusing to assemble the pool is necessary but not sufficient: /DATA is then
// a plain directory on the root filesystem, and nothing orders docker behind
// this service, so containers bind-mount /DATA/AppData/* and write their data
// into the rootfs - invisible, unbacked, and filling the system disk. A
// read-only tmpfs over the mount point is the least destructive way to make
// that fail loudly instead: it writes nothing, destroys nothing, hides no data
// permanently (whatever is underneath reappears on umount), and turns every
// wrong write into EROFS, which container logs and the journal show. It is
// lifted automatically the moment the volumes are all present again.
const (
	poolFenceSource  = "casanova-pool-unavailable"
	poolFenceFSType  = "tmpfs"
	poolFenceOptions = "ro,nosuid,nodev,noexec,size=1m,mode=0555"
)

var (
	// Seams for tests: fencing shells out to mount(8), which a unit test must
	// not do.
	poolFenceMount  = mount.Mount
	poolFenceUmount = mount.UmountByMountPoint

	// volumePresence is the presence probe, replaceable in tests so a pool can
	// be driven through "disk missing" and "disk showed up" without real block
	// devices.
	volumePresence = isVolumeMountedAsRecorded
)

// Make sure the serial disk is removed from the merge list when it is deleted from database, to keep the database consistent.
func hookAfterDeleteVolume(db *gorm.DB, model interface{}) error {
	var targetVolumes []model2.Volume

	switch t := model.(type) {
	case model2.Volume:
		targetVolumes = []model2.Volume{t}
	case *model2.Volume:
		targetVolumes = []model2.Volume{*t}
	case []model2.Volume:
		targetVolumes = t
	case *[]model2.Volume:
		targetVolumes = *t
	default:
		return nil
	}

	var merges []model2.Merge

	if err := db.Model(&model2.Merge{}).Preload(model2.MergeSourceVolumes).Find(&merges).Error; err != nil {
		logger.Error("failed to get merge list from database", zap.Error(err))
		return err
	}

	for i := range merges {
		updatedVolumes := make([]*model2.Volume, 0)
		for _, sourceVolume := range merges[i].SourceVolumes {
			shouldKeep := true
			for _, targetVolume := range targetVolumes {
				if sourceVolume.ID == targetVolume.ID {
					shouldKeep = false
					break
				}
			}
			if shouldKeep {
				updatedVolumes = append(updatedVolumes, sourceVolume)
			}
		}

		if err := db.Model(&merges[i]).Association(model2.MergeSourceVolumes).Error; err != nil {
			logger.Error("failed to enter association mode between merges and volumes", zap.Error(err), zap.Any("merge", merges[i]))
			return err
		}

		if err := db.Model(&merges[i]).Association(model2.MergeSourceVolumes).Replace(updatedVolumes); err != nil {
			logger.Error("failed to update merge source volumes", zap.Error(err), zap.Any("merge", merges[i]), zap.Any("updatedVolumes", updatedVolumes))
			return err
		}
	}

	return nil
}

func (s *LocalStorageService) GetMerges(mountPoint *string) ([]model2.Merge, error) {
	mergesFromDB, err := s.GetMergeAllFromDB(mountPoint)
	if err != nil {
		return nil, err
	}

	for _, merge := range mergesFromDB {
		merge.SourceVolumes = excludeVolumesWithWrongMountPointAndUUID(merge.SourceVolumes)
	}

	return mergesFromDB, nil
}

func (s *LocalStorageService) CreateMerge(merge *model2.Merge) error {
	if merge == nil {
		logger.Error("`merge` should not be nil")
		return ErrNilReference
	}

	if err := file.IsNotExistMkDir(merge.MountPoint); err != nil {
		return err
	}

	merge.SourceVolumes = excludeVolumesWithWrongMountPointAndUUID(merge.SourceVolumes)

	sources, err := buildSources(merge)
	if err != nil {
		logger.Error("failed to build sources", zap.Error(err))
		return err
	}

	// check if the mount point is empty before creating a new mergerfs mount
	if bool, err := file.IsDirEmpty(merge.MountPoint); err != nil {
		logger.Error("failed to check if the mount point is empty", zap.Error(err))
		return err
	} else if !bool {
		logger.Error("mount point is not empty", zap.String("mountPoint", merge.MountPoint))
		return ErrMountPointIsNotEmpty
	}

	// create a new merge by mounting mergerfs
	source := strings.Join(sources, ":")
	if _, err := s.Mount(codegen.Mount{
		MountPoint: merge.MountPoint,
		Fstype:     &merge.FSType,
		Source:     &source,
	}); err != nil {
		logger.Error("failed to mount mergerfs", zap.Error(err), zap.String("mountPoint", merge.MountPoint), zap.String("source", source))
		return err
	}

	return nil
}

func (s *LocalStorageService) UpdateMerge(merge *model2.Merge) error {
	if merge == nil {
		logger.Error("`merge` should not be nil")
		return ErrNilReference
	}

	if !file.Exists(merge.MountPoint) {
		return ErrMergeMountPointDoesNotExist
	}

	merge.SourceVolumes = excludeVolumesWithWrongMountPointAndUUID(merge.SourceVolumes)

	sources, err := buildSources(merge)
	if err != nil {
		logger.Error("failed to build sources", zap.Error(err))
		return err
	}

	// if it is already a merge point, check if the mount point is a mergerfs mount with the same sources
	existingSources, err := mergerfs.GetSource(merge.MountPoint)
	if err != nil {
		logger.Error("failed to get mergerfs sources", zap.Error(err), zap.String("mountPoint", merge.MountPoint))
		return err
	}

	if !utils.CompareStringSlices(sources, existingSources) {
		// update the mergerfs sources if different sources
		if err := mergerfs.SetSource(merge.MountPoint, sources); err != nil {
			logger.Error("failed to set mergerfs sources", zap.Error(err), zap.String("mountPoint", merge.MountPoint), zap.Any("sources", sources))
			return err
		}
	}

	return nil
}

// CheckMergeMount returns an error when at least one pool was left unassembled.
// The caller that matters is the casanova-local-storage-first oneshot, which
// docker is ordered behind: turning a refusal into a non-zero exit makes it show
// up in `systemctl --failed` instead of the box coming up looking healthy with a
// fenced /DATA. It is ordering-only (no Requires=), so a failure never keeps
// docker or the boot target from starting.
//
// It is idempotent: a pool the oneshot already assembled comes back as
// "merge already exists" and is only refreshed, so the daemon can safely call it
// again as a fallback on boxes where the oneshot is not enabled.
func (s *LocalStorageService) CheckMergeMount() error {
	// Assembly must never run on a half-populated snapshot of the system. Both
	// branches below rewrite the pool's branch list from whatever volumes look
	// mounted right now - CreateMerge builds it, UpdateMerge pushes it into a
	// live mergerfs via SetSource - so a disk that has not finished coming up
	// (slow USB/SATA enumeration, the remount oneshot still running) silently
	// disappears from /DATA: the user sees a pool missing part of their data
	// and new writes land on the wrong branch. Wait for the disks the database
	// expects, and if they are still absent when the wait runs out, leave the
	// pool exactly as it is. Losing the availability of a disk is a destructive
	// outcome, so not touching anything is the correct failure.
	mergesFromDB, missingByMerge, err := s.waitForMergeSourceVolumes()
	if err != nil {
		logger.Error("failed to get merge list from database", zap.Error(err))
		return fmt.Errorf("merge list unavailable: %w", err)
	}

	mounts, err := s.GetMounts(codegen.GetMountsParams{})
	if err != nil {
		logger.Error("failed to get mount list from system", zap.Error(err))
		return fmt.Errorf("mount list unavailable: %w", err)
	}

	// Count pools we could not bring up, so the caller can report a degraded
	// storage stack rather than silent success.
	unassembled := 0

	for i := range mergesFromDB {

		if missing, incomplete := missingByMerge[i]; incomplete {
			unassembled++
			logger.Error("refusing to assemble merge point: source volume(s) not present - pool left untouched",
				zap.String("mountPoint", mergesFromDB[i].MountPoint),
				zap.Strings("missingVolumes", describeVolumes(missing)),
			)
			s.fencePoolMountPoint(mergesFromDB[i].MountPoint, mounts)
			continue
		}

		isMergeExist := false

		// for each merge from database by mount point, check if it already mounted, i.e. a mergerfs mount
		for _, mount := range mounts {
			if mount.MountPoint == mergesFromDB[i].MountPoint {
				if mount.Fstype != nil && *mount.Fstype == mergesFromDB[i].FSType {
					logger.Info("merge already exists", zap.Any("merge", mergesFromDB[i]))
					isMergeExist = true
					break
				}
				// A fence from an earlier pass sits exactly here and is not a
				// mergerfs mount. Now that every volume is back, lift it -
				// otherwise the pool could never assemble again and the fence,
				// meant to be temporary, would become the outage.
				if isPoolFence(mount) {
					logger.Info("all source volumes are present again - lifting the read-only fence on the pool mount point",
						zap.String("mountPoint", mergesFromDB[i].MountPoint))
					if err := poolFenceUmount(mergesFromDB[i].MountPoint); err != nil {
						logger.Error("failed to lift the fence on the pool mount point - the pool cannot be assembled until it is unmounted by hand",
							zap.Error(err), zap.String("mountPoint", mergesFromDB[i].MountPoint))
					}
					continue
				}
				logger.Error("not a mergerfs mount point", zap.Any("mount", mount))
			}
		}

		if isMergeExist {
			if err := s.UpdateMerge(&mergesFromDB[i]); err != nil {
				logger.Error("failed to update merge", zap.Error(err), zap.Any("merge", mergesFromDB[i]))
				unassembled++
			}
			continue
		} else {
			if err := s.CreateMerge(&mergesFromDB[i]); err != nil {
				logger.Error("failed to create merge", zap.Error(err), zap.Any("merge", mergesFromDB[i]))
				unassembled++
			}
		}
	}

	if unassembled > 0 {
		return fmt.Errorf("%d of %d merge point(s) could not be assembled", unassembled, len(mergesFromDB))
	}
	return nil
}

// waitForMergeSourceVolumes polls until every volume the database associates
// with a merge is actually there, and returns the merges together with what is
// still missing per merge (keyed by index into the returned slice) once the
// budget is spent. An empty map means the snapshot is complete and safe to act
// on. Retrying is what covers the normal boot race: disks routinely appear
// seconds after this service does.
//
// The merge list is re-read from the database on every pass. It cannot be a
// snapshot: casanova-local-storage-first is not ordered before this service, and
// while we poll it may remount a disk on a different mount point (m + "-1") and
// write that into the database. Probing a stale MountPoint would then fail
// forever and reject a pool whose disks are all present and healthy - the exact
// race this wait exists to survive.
func (s *LocalStorageService) waitForMergeSourceVolumes() ([]model2.Merge, map[int][]*model2.Volume, error) {
	start := time.Now()

	waitUntil := start.Add(mergeAssemblyWaitTimeout)

	// The probes may outlive the polling budget by the floor, so that a wait
	// configured shorter than a single probe still gets one honest look at the
	// disks instead of declaring them missing by arithmetic.
	deadline := waitUntil
	if floor := start.Add(mergeAssemblyProbeFloor); floor.After(deadline) {
		deadline = floor
	}

	// One absolute deadline for the whole wait, handed to every probe, so a
	// probe that hangs is cut off mid-flight rather than adding its own timeout
	// on top of the budget. Without this the "60 second" wait was several
	// minutes with a handful of volumes, and unbounded against a dying disk.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	for attempt := 1; ; attempt++ {
		merges, err := s.GetMergeAllFromDB(nil)
		if err != nil {
			return nil, nil, err
		}

		missingByMerge := make(map[int][]*model2.Volume)

		for i := range merges {
			if _, missing := partitionVolumesByPresence(ctx, merges[i].SourceVolumes); len(missing) > 0 {
				missingByMerge[i] = missing
			}
		}

		if len(missingByMerge) == 0 || !time.Now().Before(waitUntil) {
			return merges, missingByMerge, nil
		}

		logger.Info("waiting for merge source volume(s) to appear before assembling the pool",
			zap.Int("attempt", attempt),
			zap.Int("incompleteMerges", len(missingByMerge)),
		)

		select {
		case <-ctx.Done():
			return merges, missingByMerge, nil
		case <-time.After(mergeAssemblyPollInterval):
		}
	}
}

// fencePoolMountPoint makes a pool mount point whose disks are missing reject
// writes instead of absorbing them into the root filesystem. See the fence
// constants for why this is the least destructive option available here.
//
// It only ever runs for a pool the database says should exist and whose source
// volumes are missing, so a fresh install (a merge with no source volumes) never
// reaches it.
func (s *LocalStorageService) fencePoolMountPoint(mountPoint string, mounts []codegen.Mount) {
	// Paranoia about the one thing worse than an unfenced pool: fencing
	// something that is not the pool.
	if mountPoint == "" || !filepath.IsAbs(mountPoint) || filepath.Clean(mountPoint) == "/" {
		logger.Error("refusing to fence an implausible pool mount point", zap.String("mountPoint", mountPoint))
		return
	}

	// Anything already mounted here is either a live pool or the fence from an
	// earlier pass; stacking another mount on top of it would only hide it.
	for _, existing := range mounts {
		if existing.MountPoint == mountPoint {
			logger.Info("pool mount point already carries a mount - leaving it as is",
				zap.String("mountPoint", mountPoint), zap.Any("mount", existing))
			return
		}
	}

	if err := file.IsNotExistMkDir(mountPoint); err != nil {
		logger.Error("failed to create the pool mount point before fencing it", zap.Error(err), zap.String("mountPoint", mountPoint))
		return
	}

	fsType, options := poolFenceFSType, poolFenceOptions
	if err := poolFenceMount(poolFenceSource, mountPoint, &fsType, &options); err != nil {
		logger.Error("FAILED to fence the unassembled pool mount point - applications writing here will fill the root filesystem with data that is not on any disk",
			zap.Error(err), zap.String("mountPoint", mountPoint))
		return
	}

	logger.Error("pool mount point fenced read-only until its disks come back - writes to it will fail with EROFS instead of landing on the root filesystem",
		zap.String("mountPoint", mountPoint), zap.String("source", poolFenceSource))
}

// isPoolFence reports whether a mount is one of ours.
func isPoolFence(m codegen.Mount) bool {
	return m.Fstype != nil && *m.Fstype == poolFenceFSType &&
		m.Source != nil && *m.Source == poolFenceSource
}

// describeVolumes renders volumes for logs without dereferencing nil entries -
// a nil source volume is itself one of the reasons a snapshot gets rejected.
func describeVolumes(volumes []*model2.Volume) []string {
	descriptions := make([]string, 0, len(volumes))

	for _, volume := range volumes {
		if volume == nil {
			descriptions = append(descriptions, "<nil volume reference>")
			continue
		}
		descriptions = append(descriptions, volume.UUID+" -> "+volume.MountPoint)
	}

	return descriptions
}

// filter out any volume that are not mounted based on its UUID and mount point (in reality, could have a different disk mounted on the same path)
//
// The probes get their own deadline because this runs on request-serving paths
// too (GetMerges, and CreateMerge/UpdateMerge from the InitMerge handler): an
// admin retrying an operation against a wedged disk must not leak a pinned
// goroutine per attempt.
func excludeVolumesWithWrongMountPointAndUUID(volumes []*model2.Volume) []*model2.Volume {
	ctx, cancel := context.WithTimeout(context.Background(), mergeProbeTimeout)
	defer cancel()

	present, _ := partitionVolumesByPresence(ctx, volumes)

	return present
}

// partitionVolumesByPresence splits volumes into the ones confirmed mounted
// where the database says they are and the ones that are not. Callers that only
// need the surviving branches go through excludeVolumesWithWrongMountPointAndUUID;
// CheckMergeMount needs the missing half too, because "a disk we expected is not
// here" is exactly the condition under which it must not touch the pool.
func partitionVolumesByPresence(ctx context.Context, volumes []*model2.Volume) (present, missing []*model2.Volume) {
	for _, volume := range volumes {
		if volumePresence(ctx, volume) {
			present = append(present, volume)
			continue
		}
		missing = append(missing, volume)
	}

	return present, missing
}

// isVolumeMountedAsRecorded reports whether the disk behind this volume is
// present and mounted where the database recorded it. Every probe it runs obeys
// ctx, so the caller's budget is the real bound - including in the middle of a
// blkid or partx that a dying disk would otherwise block forever.
func isVolumeMountedAsRecorded(ctx context.Context, v *model2.Volume) bool {
	if v == nil {
		logger.Error("nil source volume in merge - treating it as missing")
		return false
	}

	path, err := partition.GetDevicePathContext(ctx, v.UUID)
	if err != nil {
		logger.Error("failed to corresponding device path by volume UUID", zap.Error(err), zap.String("uuid", v.UUID))
		logger.Info("WARNING: merge pool volume excluded — drive may be missing",
			zap.String("uuid", v.UUID),
			zap.String("mountPoint", v.MountPoint))
		return false
	}

	par, err := command.ExecLSBLKByPathContext(ctx, path)
	if err != nil {
		// Could not read the device: that is "unknown", and unknown counts as
		// missing here on purpose - the pool must not be rebuilt around a disk
		// we failed to inspect.
		logger.Error("failed to inspect the device behind a merge source volume", zap.Error(err), zap.String("path", path))
		logger.Info("WARNING: merge pool volume excluded — drive may be missing",
			zap.String("uuid", v.UUID),
			zap.String("mountPoint", v.MountPoint))
		return false
	}

	pttype := gjson.GetBytes(par, "blockdevices.0.pttype")
	if pttype.String() != "gpt" {
		mountPoint := gjson.GetBytes(par, "blockdevices.0.mountpoint")
		if mountPoint.String() != v.MountPoint {
			logger.Error("mount point does not match actual", zap.Any("volume", v), zap.String("actual mount point", mountPoint.String()))
			logger.Info("WARNING: merge pool volume excluded — drive may be missing",
				zap.String("uuid", v.UUID),
				zap.String("mountPoint", v.MountPoint))
			return false
		}
		return true

	}

	partitions, err := partition.GetPartitionsContext(ctx, path)
	if err != nil {
		logger.Error("failed to corresponding partition of volume", zap.Error(err), zap.String("path", path))
		logger.Info("WARNING: merge pool volume excluded — drive may be missing",
			zap.String("uuid", v.UUID),
			zap.String("mountPoint", v.MountPoint))
		return false
	}

	if len(partitions) != 1 {
		logger.Error("there should be exactly one partition corresponding to the volume", zap.String("path", path), zap.Int("partitions", len(partitions)))
		logger.Info("WARNING: merge pool volume excluded — drive may be missing",
			zap.String("uuid", v.UUID),
			zap.String("mountPoint", v.MountPoint))
		return false
	}

	if partitions[0].LSBLKProperties["MOUNTPOINT"] != v.MountPoint {
		logger.Error("mount point does not match actual", zap.Any("volume", v), zap.String("actual mount point", partitions[0].LSBLKProperties["MOUNTPOINT"]))
		logger.Info("WARNING: merge pool volume excluded — drive may be missing",
			zap.String("uuid", v.UUID),
			zap.String("mountPoint", v.MountPoint))
		return false
	}

	return true
}

func buildSources(merge *model2.Merge) ([]string, error) {
	sources := make([]string, 0)

	if merge.SourceBasePath != nil && *merge.SourceBasePath != "" {
		// check if sourceBasePath is under mount point (use trailing slash to avoid prefix false positives)
		mountWithSlash := filepath.Clean(merge.MountPoint) + "/"
		sourceWithSlash := filepath.Clean(*merge.SourceBasePath) + "/"
		if strings.HasPrefix(sourceWithSlash, mountWithSlash) || strings.HasPrefix(mountWithSlash, sourceWithSlash) {
			logger.Error(
				"source base path should not be a child path of the merge mount point",
				zap.String("sourceBasePath", *merge.SourceBasePath),
				zap.String("merge.MountPoint", merge.MountPoint),
			)
			return nil, ErrMergeMountPointSourceConflict
		}

		// create source path if it does not exists
		if err := file.IsNotExistMkDir(*merge.SourceBasePath); err != nil {
			return nil, err
		}

		sources = append(sources, *merge.SourceBasePath)
	}

	for _, sourceVolume := range merge.SourceVolumes {
		if sourceVolume == nil {
			logger.Error("one of the source volumes is nil", zap.Any("sourceVolumes", merge.SourceVolumes))
			return nil, ErrNilReference
		}

		// check if sourceBasePath is under mount point (use trailing slash to avoid prefix false positives)
		volMountWithSlash := filepath.Clean(sourceVolume.MountPoint) + "/"
		mergeMountWithSlash := filepath.Clean(merge.MountPoint) + "/"
		if strings.HasPrefix(volMountWithSlash, mergeMountWithSlash) || strings.HasPrefix(mergeMountWithSlash, volMountWithSlash) {
			logger.Error(
				"mount point of source volume should not be a child path of the mount point",
				zap.Any("sourceVolume.MountPoint", sourceVolume.MountPoint),
				zap.Any("merge.MountPoint", merge.MountPoint),
			)
			return nil, ErrMergeMountPointSourceConflict
		}

		sources = append(sources, sourceVolume.MountPoint)
	}

	return sources, nil
}
