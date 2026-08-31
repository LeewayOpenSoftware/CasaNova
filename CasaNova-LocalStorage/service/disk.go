package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	command2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/command"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/shell"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/fstab"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/mount"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/partition"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/smart"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
	model2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
	v2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2/fs"
	jsoniter "github.com/json-iterator/go"
	"github.com/moby/sys/mountinfo"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type DiskService interface {
	EnsureDefaultMergePoint() bool
	AddPartition(path string) error
	DeletePartition(path string) error
	CheckSerialDiskMount() error
	FormatDisk(path string) error
	FormatDiskAs(path, fsType string) error
	GetDiskInfo(path string) model.LSBLKModel
	GetPersistentTypeByUUID(uuid string) string
	GetUSBDriveStatusList() []model.USBDriveStatus
	LSBLK(isUseCache bool) []model.LSBLKModel
	MountDisk(path, volume string) (string, error)
	RemoveLSBLKCache()
	SmartCTL(path string) model.SmartctlA
	UmountPointAndRemoveDir(m model.LSBLKModel) error
	UmountUSB(path string) error

	UpdateMountPointInDB(m model2.Volume) error
	DeleteMountPointFromDB(path, mountPoint string) error
	GetSerialAllFromDB() ([]model2.Volume, error)
	SaveMountPointToDB(m model2.Volume) error
	InitCheck()
	GetSystemDf() (model.DFDiskSpace, error)
}

type diskService struct {
	db *gorm.DB
}

const (
	PersistentTypeNone   = "none"
	PersistentTypeFStab  = "fstab"
	PersistentTypeCasaOS = "casaos"
)

var (
	ErrVolumeWithEmptyUUID = errors.New("cannot save volume with empty uuid")
	json2                  = jsoniter.ConfigCompatibleWithStandardLibrary
)

func (d *diskService) EnsureDefaultMergePoint() bool {
	mountPoint := common.DefaultMountPoint
	sourceBasePath := constants.DefaultFilePath

	existingMerges, err := MyService.LocalStorage().GetMergeAllFromDB(&mountPoint)
	if err != nil {
		// Don't panic on a DB read failure during startup — log and bail so
		// the rest of the service can still come up. A crash loop here would
		// wedge the entire stack before any recovery is possible.
		logger.Error("EnsureDefaultMergePoint: GetMergeAllFromDB failed",
			zap.String("mount_point", mountPoint), zap.Error(err))
		return false
	}

	// check if /DATA is already a merge point
	if len(existingMerges) > 0 {
		if len(existingMerges) > 1 {
			logger.Error("more than one merge point with the same mount point found", zap.String("mount point", mountPoint))
		}
		config.ServerInfo.EnableMergerFS = "true"
		return true
	}

	merge := &model2.Merge{
		FSType:         fs.MergerFSFullName,
		MountPoint:     mountPoint,
		SourceBasePath: &sourceBasePath,
	}
	if err := MyService.LocalStorage().CreateMerge(merge); err != nil {
		if errors.Is(err, v2.ErrMergeMountPointAlreadyExists) {
			logger.Info(err.Error(), zap.String("mount point", mountPoint))
		} else if errors.Is(err, v2.ErrMountPointIsNotEmpty) {
			logger.Error("Mount point "+mountPoint+" is not empty", zap.String("mount point", mountPoint))
			return false
		} else {
			// CreateMerge failure is non-fatal — log and report via return.
			logger.Error("EnsureDefaultMergePoint: CreateMerge failed",
				zap.String("mount_point", mountPoint), zap.Error(err))
			return false
		}
	}

	// mounts, err := MyService.LocalStorage().GetMounts(codegen.GetMountsParams{})
	// if err != nil {
	// 	logger.Error("failed to get mount list from system", zap.Error(err))
	// 	return false
	// }
	// isExist := false
	// for _, v := range mounts {
	// 	if v.MountPoint == mountPoint {
	// 		config.ServerInfo.EnableMergerFS = "true"
	// 		isExist = true
	// 		merge.SourceBasePath = v.Source
	// 		break
	// 	}
	// }

	// if !isExist {
	// 	if err := MyService.LocalStorage().CreateMerge(merge); err != nil {
	// 		if errors.Is(err, v2.ErrMergeMountPointAlreadyExists) {
	// 			logger.Info(err.Error(), zap.String("mount point", mountPoint))
	// 		} else if errors.Is(err, v2.ErrMountPointIsNotEmpty) {
	// 			logger.Error("Mount point "+mountPoint+" is not empty", zap.String("mount point", mountPoint))
	// 			return false
	// 		} else {
	// 			panic(err)
	// 		}
	// 	}
	// }

	if err := MyService.LocalStorage().CreateMergeInDB(merge); err != nil {
		// DB write failure is non-fatal at startup — log and report via return.
		logger.Error("EnsureDefaultMergePoint: CreateMergeInDB failed",
			zap.String("mount_point", mountPoint), zap.Error(err))
		return false
	}
	config.ServerInfo.EnableMergerFS = "true"
	return true
}

func (d *diskService) RemoveLSBLKCache() {
	key := "system_lsblk"
	Cache.Delete(key)
	// The negative entry is part of the same cache: leaving it behind would keep
	// answering "enumeration failed" to a caller that explicitly asked for a
	// fresh look.
	Cache.Delete(lsblkFailureCacheKey)
}

func (d *diskService) UmountUSB(path string) error {
	safePath := shell.SanitizeArg(path)
	if safePath == "" {
		return errors.New("invalid device path")
	}
	_, err := command2.ExecResultStr("source " + config.AppInfo.ShellPath + "/local-storage-helper.sh ;UDEVILUmount " + safePath)
	return err
}

func (d *diskService) SmartCTL(path string) model.SmartctlA {
	key := "system_smart_" + path
	if result, ok := Cache.Get(key); ok {

		res, ok := result.(model.SmartctlA)
		if ok {
			return res
		}
	}
	var m model.SmartctlA
	buf := command.ExecSmartCTLByPath(path)
	if buf == nil {
		if err := Cache.Add(key, m, time.Minute*10); err != nil {
			logger.Error("failed to add cache", zap.Error(err), zap.String("key", key))
		}
		return m
	}

	err := json2.Unmarshal(buf, &m)
	if err != nil {
		logger.Error("failed to unmarshal json", zap.Error(err), zap.String("json", string(buf)))
	}
	if !reflect.DeepEqual(m, model.SmartctlA{}) {
		if err := Cache.Add(key, m, time.Hour*24); err != nil {
			logger.Error("failed to add cache", zap.Error(err), zap.String("key", key))
		}
	}
	return m
}

func (d *diskService) FormatDisk(path string) error {
	return d.FormatDiskAs(path, "ext4")
}

// FormatDiskAs is the multi-filesystem variant. fsType is one of the keys
// in partition.supportedMkfs; an empty string defaults to ext4 so older
// callers (and the V1 handler) keep working unchanged.
func (d *diskService) FormatDiskAs(path, fsType string) error {
	if fsType == "" {
		fsType = "ext4"
	}
	// wait for partition path to be ready
	count := 5
	for count > 0 {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				time.Sleep(1 * time.Second)
				count--
				continue
			}
			logger.Error("error when checking partition path", zap.Error(err), zap.String("path", path))
			return err
		}
		break
	}

	logger.Info("formatting partition...", zap.String("path", path), zap.String("fs", fsType))
	if err := partition.FormatPartitionAs(path, fsType); err != nil {
		logger.Error("failed to format partition", zap.Error(err), zap.String("path", path), zap.String("fs", fsType))
		return err
	}

	return nil
}

// ,
func (d *diskService) UmountPointAndRemoveDir(m model.LSBLKModel) error {
	if len(m.MountPoint) > 0 {
		if err := mount.UmountByMountPoint(m.MountPoint); err != nil {
			logger.Error("error when umounting partition", zap.Error(err), zap.String("path", m.Path), zap.String("mount point", m.MountPoint))
			return err
		}
		if err := file.RMDir(m.MountPoint); err != nil {
			logger.Error("error when removing mount point directory", zap.Error(err), zap.String("path", m.Path), zap.String("mount point", m.MountPoint))
			return err
		}
	}
	for _, p := range m.Children {
		if len(p.MountPoint) > 0 {

			if err := mount.UmountByMountPoint(p.MountPoint); err != nil {
				logger.Error("error when umounting partition", zap.Error(err), zap.String("path", p.Path), zap.String("mount point", p.MountPoint))
				return err
			}
			if err := file.RMDir(p.MountPoint); err != nil {
				logger.Error("error when removing mount point directory", zap.Error(err), zap.String("path", p.Path), zap.String("mount point", p.MountPoint))
				return err
			}
		}
	}

	return nil
}

// part
func (d *diskService) AddPartition(path string) error {
	logger.Info("creating partition table...", zap.String("path", path))
	if err := partition.CreatePartitionTable(path); err != nil {
		logger.Error("failed to create partition table", zap.Error(err), zap.String("path", path))
		return err
	}

	logger.Info("creating partition...", zap.String("path", path))
	partitions, err := partition.AddPartition(path)
	if err != nil {
		logger.Error("failed to create partition", zap.Error(err), zap.String("path", path))
		return err
	}

	for _, p := range partitions {
		partitionPath := p.LSBLKProperties["PATH"]

		// wait for partition path to be ready
		count := 5
		for count > 0 {
			if _, err := os.Stat(partitionPath); err != nil {
				if os.IsNotExist(err) {
					time.Sleep(1 * time.Second)
					count--
					continue
				}
				logger.Error("error when checking partition path", zap.Error(err), zap.String("path", partitionPath))
				return err
			}
			break
		}

		logger.Info("formatting partition...", zap.String("path", partitionPath))
		if err := partition.FormatPartition(partitionPath); err != nil {
			logger.Error("failed to format partition", zap.Error(err), zap.String("path", partitionPath))
			return err
		}
	}

	return nil
}

func (d *diskService) DeletePartition(path string) error {
	// check if path exists
	if !file.Exists(path) {
		return errors.New("device " + path + " does not exists")
	}

	logger.Info("trying to get all partitions of device...", zap.String("path", path))
	partitions, err := partition.GetPartitions(path)
	if err != nil {
		logger.Error("error when getting all partitions of device", zap.Error(err), zap.String("path", path))
		return err
	}

	for _, p := range partitions {

		n, err := strconv.Atoi(p.PARTXProperties["NR"])
		if err != nil {
			logger.Error("error when converting partition number to int", zap.Error(err), zap.String("path", path), zap.String("partition number", p.PARTXProperties["NR"]))
			return err
		}

		logger.Info("trying to delete partition...", zap.String("path", p.LSBLKProperties["PATH"]))
		if err := partition.DeletePartition(path, n); err != nil {
			logger.Error("error when deleting partition", zap.Error(err), zap.String("path", p.LSBLKProperties["PATH"]))
			return err
		}
	}

	return nil
}

// LSBLK keeps the historical shape (nil on failure) for the read-only callers
// that only render a device list. Anything that decides whether a disk should be
// mounted must use lsblkList instead: an empty list and a failed enumeration are
// the same value here, and confusing the two is how a single dying drive left
// every healthy disk unmounted.
func (d *diskService) LSBLK(isUseCache bool) []model.LSBLKModel {
	list, err := d.lsblkList(isUseCache)
	if err != nil {
		return nil
	}

	return list
}

// lsblkList is the enumeration of block devices with its failure preserved.
func (d *diskService) lsblkList(isUseCache bool) ([]model.LSBLKModel, error) {
	key := "system_lsblk"

	if isUseCache {
		if result, ok := Cache.Get(key); ok {
			if res, ok := result.([]model.LSBLKModel); ok {
				return res, nil
			}
		}

		// A recent failure is cached too, and returned as a failure. lsblk is a
		// global probe: while one device is wedged every call hangs for the full
		// timeout and leaves a child behind, and the storage UI polls. The
		// negative entry keeps that poll from re-arming a new lsblk every few
		// seconds; it is deliberately short-lived, and the boot sweep bypasses
		// the cache entirely so recovery is never delayed by it.
		if result, ok := Cache.Get(lsblkFailureCacheKey); ok {
			if cachedErr, ok := result.(error); ok {
				return nil, cachedErr
			}
		}
	}

	str, err := command.ExecLSBLK()
	if err != nil {
		logger.Error("Failed to exec shell - lsblk exec error", zap.Error(err))
		Cache.Set(lsblkFailureCacheKey, err, lsblkFailureCacheTTL)
		return nil, err
	}

	Cache.Delete(lsblkFailureCacheKey)

	blkList, err := ParseBlockDevices(str)
	if err != nil {
		// Unparseable output is a failed enumeration, not an empty machine: the
		// caller must not read it as "there are no disks to mount".
		logger.Error("Failed to parse block devices from output of lsblk", zap.Error(err))
		return nil, err
	}

	var fsused uint64

	result := make([]model.LSBLKModel, 0)

	for _, blk := range blkList {

		if blk.Type == "loop" || blk.RO {
			continue
		}

		fsused = 0

		var blkChildren []model.LSBLKModel
		smart := MyService.Disk().SmartCTL(blk.Path)
		for _, child := range blk.Children {
			if child.RM {

				// if strings.ToLower(strings.TrimSpace(child.State)) != "ok" {
				// 	health = false
				// }
				f, _ := strconv.ParseUint(child.FSUsed.String(), 10, 64)
				fsused += f
			}
			blkChildren = append(blkChildren, child)
		}
		if smart.SmartStatus.Passed {
			blk.Health = "OK"
		} else {
			for _, v := range smart.Smartctl.Messages {
				if strings.Contains(v.String, "STANDBY") {
					blk.Health = "OK"
					break
				}
			}
		}

		blk.FSUsed = json.Number(fmt.Sprintf("%d", fsused))
		blk.Children = blkChildren
		if fsused > 0 {
			blk.UsedPercent, err = strconv.ParseFloat(fmt.Sprintf("%.4f", float64(fsused)/float64(blk.Size)), 64)
			if err != nil {
				logger.Error("Failed to parse float", zap.Error(err))
			}
		}
		result = append(result, blk)
	}

	if len(result) > 0 {
		Cache.Set(key, result, time.Second*100)
	}

	return result, nil
}

func (d *diskService) GetDiskInfo(path string) model.LSBLKModel {
	str, err := command.ExecLSBLKByPath(path)
	if err != nil {
		logger.Error("Failed to exec shell - lsblk exec error", zap.Error(err), zap.String("path", path))
		return model.LSBLKModel{}
	}

	blkList, err := ParseBlockDevices(str)
	if err != nil {
		logger.Error("Failed to parse block devices from output of lsblk", zap.Error(err))
		return model.LSBLKModel{}
	}

	blk := model.LSBLKModel{}
	if len(blkList) > 0 {
		blk = blkList[0]
	}
	return blk
}

func (d *diskService) MountDisk(path, mountPoint string) (string, error) {
	logger.Info("trying to mount...", zap.String("path", path), zap.String("mountPoint", mountPoint))

	// check if path is already mounted at mountPoint
	if mountInfoList, err := mountinfo.GetMounts(func(i *mountinfo.Info) (skip bool, stop bool) {
		if i.Source == path && i.Mountpoint == mountPoint {
			return false, true
		}
		return true, false
	}); err != nil {
		logger.Error("error when trying to get mount info", zap.Error(err))
		return "", err
	} else if len(mountInfoList) > 0 {
		logger.Info("already mounted", zap.String("path", path), zap.String("mount point", mountPoint))
		return "", nil
	}

	if err := file.IsNotExistMkDir(mountPoint); err != nil {
		logger.Error("error when checking if mount point already exists, or when creating the mount point if it does not exists", zap.Error(err), zap.String("mount point", mountPoint))
		return "", err
	}

	// Resolve device path to UUID for stable mounting.
	// /dev/sdX paths change when SATA ports are swapped — UUID doesn't.
	mountSource := path
	if uuid := getDeviceUUID(path); uuid != "" {
		uuidPath := "/dev/disk/by-uuid/" + uuid
		if _, err := os.Stat(uuidPath); err == nil {
			logger.Info("mounting by UUID for stability",
				zap.String("device", path),
				zap.String("uuid", uuid),
				zap.String("uuid_path", uuidPath),
			)
			mountSource = uuidPath
		}
	}

	// Detect SSD for TRIM support. Wave 34-X residual: previously the
	// helper computed `mountOpts` and then immediately discarded it via
	// `_ = mountOpts`, so SSDs were mounted without `discard` and TRIM
	// never ran. The shell helper has been extended to accept a 3rd
	// argument for extra ext-family mount options; forward it here.
	deviceName := filepath.Base(path)
	mountOpts := ""
	if isSSD(deviceName) {
		// Pass only `discard` — `noatime` is already applied by the
		// shell helper for the ext family, so passing it twice would
		// just be noise.
		mountOpts = "discard"
		logger.Info("SSD detected — mounting with TRIM support", zap.String("device", path))
	}

	// Sanitize and mount
	safePath := shell.SanitizeArg(mountSource)
	safeMountPoint := shell.SanitizeArg(mountPoint)
	cmd := "source " + config.AppInfo.ShellPath + "/local-storage-helper.sh ;do_mount " + safePath + " " + safeMountPoint
	if mountOpts != "" {
		cmd += " " + shell.SanitizeArg(mountOpts)
	}
	if out, err := command2.OnlyExec(cmd); err != nil {
		logger.Error("error when mounting", zap.Error(err), zap.String("path", path), zap.String("mount point", mountPoint), zap.String("output", string(out)))
		return out, err
	}

	return "", nil
}

// deviceProbeTimeout bounds the small per-device identity probes (blkid,
// udevadm). They run inside MountDisk and the boot remount loop, one per
// device: unbounded, a single drive stuck in uninterruptible I/O - or a udev
// daemon still settling after a module reload - held the whole sweep. The
// budget matches the one isUSBDevice already used for udevadm; a healthy answer
// takes milliseconds.
const deviceProbeTimeout = 5 * time.Second

// logDeviceProbeStall separates "the tool answered and had nothing to say" from
// "the tool never answered". Only the second is a symptom of a sick device, and
// only the second must be visible in the journal - the identity probes return
// an empty string either way, which is how a dying disk used to degrade the
// mount silently.
func logDeviceProbeStall(tool, devicePath string, err error) {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, command.ErrProbeStalled) {
		return
	}

	logger.Error("device identity probe did not answer",
		zap.String("tool", tool),
		zap.String("device", devicePath),
		zap.Error(err),
	)
}

// getDeviceUUID returns the filesystem UUID for a block device.
// Uses blkid which is available on all Linux systems.
// Returns empty string if UUID cannot be determined (unformatted disk, etc.)
func getDeviceUUID(devicePath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), deviceProbeTimeout)
	defer cancel()

	out, err := command.ExecuteCommandContext(ctx, "blkid", "-s", "UUID", "-o", "value", devicePath)
	if err != nil {
		// A device with no filesystem legitimately has no UUID, so an error is
		// not by itself news - but a probe that never answered is, because the
		// caller will silently fall back to mounting by /dev/sdX, which is the
		// unstable name we introduced UUIDs to avoid.
		logDeviceProbeStall("blkid", devicePath, err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getDeviceSerial returns the serial number of the physical disk.
// Uses udevadm to query the device's ID_SERIAL_SHORT property.
func getDeviceSerial(devicePath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), deviceProbeTimeout)
	defer cancel()

	out, err := command.ExecuteCommandContext(ctx, "udevadm", "info", "--query=property", "--name="+devicePath)
	if err != nil {
		logDeviceProbeStall("udevadm", devicePath, err)
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ID_SERIAL_SHORT=") {
			return strings.TrimPrefix(line, "ID_SERIAL_SHORT=")
		}
	}
	return ""
}

func (d *diskService) SaveMountPointToDB(m model2.Volume) error {
	if m.UUID == "" {
		return ErrVolumeWithEmptyUUID
	}

	var existing model2.Volume

	result := d.db.Where(&model2.Volume{UUID: m.UUID}).Limit(1).Find(&existing)

	if result.Error != nil {
		logger.Error("error when querying volume by UUID", zap.Error(result.Error), zap.Any("uuid", m.UUID))
		return result.Error
	}

	if result.RowsAffected > 0 {
		m.ID = existing.ID
	}

	if result := d.db.Save(&m); result.Error != nil {
		logger.Error("error when saving volume to db", zap.Error(result.Error), zap.Any("volume", m))
		return result.Error
	}

	return nil
}

func (d *diskService) UpdateMountPointInDB(m model2.Volume) error {
	result := d.db.Model(&model2.Volume{}).Where(&model2.Volume{UUID: m.UUID}).Update("mount_point", m.MountPoint)
	if result.Error != nil {
		logger.Error("error when updating mount point in db by UUID", zap.Error(result.Error), zap.String("uuid", m.UUID), zap.String("mount point", m.MountPoint))
		return result.Error
	}

	logger.Info(strconv.Itoa(int(result.RowsAffected))+" volume(s) with mount point updated in db by UUID", zap.String("uuid", m.UUID), zap.String("mount point", m.MountPoint))

	return nil
}

func (d *diskService) DeleteMountPointFromDB(path, mountPoint string) error {
	// Wave 28: hard refusal for empty arguments. Previously this
	// function built a Volume{} filter struct from whatever was
	// passed in and let gorm's "non-zero fields only" Where rule
	// generate the SQL. If both mountPoint AND the resolved UUID
	// were empty (which happens when partition.GetPartitions
	// returned nothing for `path` and the caller also passed an
	// empty mountPoint), the WHERE clause was empty, .Find pulled
	// the first row in the table, and the subsequent Delete
	// silently dropped a *random* unrelated volume row. Refuse
	// any call that lacks the discriminating fields.
	if mountPoint == "" && path == "" {
		return errors.New("DeleteMountPointFromDB: refusing call with empty path and empty mountPoint")
	}

	partitions, err := partition.GetPartitions(path)
	if err != nil {
		logger.Error("error when getting partitions by path", zap.Error(err), zap.String("path", path))
		return err
	}

	if len(partitions) != 1 {
		logger.Error("there should be only 1 partition returned", zap.Any("partitions", partitions))
	}

	uuid := ""
	if len(partitions) > 0 {
		uuid = partitions[0].LSBLKProperties[`UUID`]
		logger.Info("trying to delete volume by path and mount point", zap.String("path", path), zap.String("mount point", mountPoint), zap.String("uuid", uuid), zap.Any("partitons", partitions))
	}

	// Build the WHERE clause with explicit columns rather than a
	// struct filter, so an empty value is a *condition* (column = '')
	// not "no condition at all". We require at minimum that
	// mountPoint be non-empty, or that we have a UUID — refusing the
	// call otherwise rather than risk a wildcard delete.
	if mountPoint == "" && uuid == "" {
		return errors.New("DeleteMountPointFromDB: refusing call — no mountPoint and no resolvable UUID for path " + path)
	}

	q := d.db.Model(&model2.Volume{})
	switch {
	case mountPoint != "" && uuid != "":
		q = q.Where("mount_point = ? AND uuid = ?", mountPoint, uuid)
	case mountPoint != "":
		q = q.Where("mount_point = ?", mountPoint)
	default:
		q = q.Where("uuid = ?", uuid)
	}

	var existingVolumes []model2.Volume
	result := q.Limit(1).Find(&existingVolumes)
	logger.Info("result", zap.Any("result", result))
	if result.Error != nil {
		logger.Error("error when finding the volume by path and mount point", zap.Error(result.Error), zap.String("path", path), zap.String("mount point", mountPoint))
	}

	if result.RowsAffected == 0 {
		logger.Info("no volume found by path and mount point", zap.String("path", path), zap.String("mount point", mountPoint))
		return nil
	}

	if result := d.db.Delete(&existingVolumes); result.Error != nil {
		logger.Error("error when deleting volume", zap.Error(result.Error), zap.Any("volume", existingVolumes))
		return result.Error
	}

	return nil
}

func (d *diskService) GetSerialAllFromDB() ([]model2.Volume, error) {
	var volumes []model2.Volume

	result := d.db.Find(&volumes)
	if result.Error != nil {
		logger.Error("error when querying all volumes from db", zap.Error(result.Error))
		return nil, result.Error
	}

	return volumes, nil
}

func (d *diskService) GetPersistentTypeByUUID(uuid string) string {
	// check if path is in database
	var m model2.Volume

	if result := d.db.Where(&model2.Volume{UUID: uuid}).Limit(1).Find(&m); result.Error != nil {
		logger.Error("error when finding the volume by uuid in database", zap.Error(result.Error), zap.String("uuid", uuid))
	} else if result.RowsAffected > 0 {
		return PersistentTypeCasaOS
	}

	// check if it is in fstab
	if entry, err := fstab.Get().GetEntryBySource(uuid); err != nil {
		logger.Error("error when finding the volume by uuid in fstab", zap.Error(err), zap.String("uuid", uuid))
	} else if entry != nil {
		return PersistentTypeFStab
	}

	// return none if not found
	return PersistentTypeNone
}

func (d *diskService) CheckSerialDiskMount() error {
	logger.Info("Checking serial disk mount...")

	// check mount point
	dbList, err := d.GetSerialAllFromDB()
	if err != nil {
		logger.Error("error when getting all volumes from db", zap.Error(err))
		return fmt.Errorf("cannot read persisted volumes: %w", err)
	}

	// The enumeration is retried and, if it never succeeds, this sweep aborts.
	// It must never fall through to the loop below with an empty list: lsblk is
	// global, so one drive in uninterruptible I/O fails the whole inventory, and
	// treating that as "this machine has no disks" silently skips the remount of
	// every healthy disk. The box then boots with an empty /DATA and docker
	// starts on top of it, which is far worse than the hang this replaced -
	// nobody sees it until the data is already being written to the rootfs.
	list, err := enumerateDisksWithRetry(
		func() ([]model.LSBLKModel, error) { return diskEnumerator(d) },
		len(dbList) > 0,
		serialDiskEnumerationAttempts,
		serialDiskEnumerationBaseDelay,
		time.Sleep,
	)
	if err != nil {
		logger.Error("aborting the boot-time remount sweep: block devices could not be enumerated - NO disk was remounted, /DATA is not backed by its volumes and must not be written to",
			zap.Error(err),
			zap.Int("expectedVolumes", len(dbList)),
		)
		return err
	}

	mountPointMap := make(map[string]string, len(dbList))

	defer d.RemoveLSBLKCache()

	// remount
	for _, v := range dbList {
		logger.Info("previously persisted mount point", zap.Any("volume", v))
		mountPointMap[v.UUID] = v.MountPoint
	}

	sweepDisks(list, mountPointMap, command.ExecEnabledSMART, d.remountVolume)

	return nil
}

// sweepDisks remounts every partition the database expects, one disk at a time.
// Dependencies are injected so the ordering guarantee below can be tested
// without real block devices.
func sweepDisks(
	list []model.LSBLKModel,
	mountPointMap map[string]string,
	enableSMART func(path string) ([]byte, error),
	remount func(child model.LSBLKModel, recordedMountPoint string),
) {
	for _, currentDisk := range list {
		// Enabling S.M.A.R.T. is best-effort telemetry, never a precondition
		// for mounting. This loop runs from the -init oneshot that docker is
		// ordered behind, so a drive that never answers used to hang the whole
		// boot right here and the box came up without /DATA. The call is
		// bounded now, and a failure only ever logs: neither `continue` nor
		// `return` belongs in this error path, because the sweep must reach the
		// remount of the healthy disks that come after the sick one.
		output, err := enableSMART(currentDisk.Path)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, command.ErrProbeStalled):
				logger.Error("timed out enabling S.M.A.R.T - skipping this disk and continuing the sweep", zap.Error(err), zap.String("path", currentDisk.Path))
			case output != nil:
				logger.Error("failed to enable S.M.A.R.T: "+string(output), zap.Error(err), zap.String("path", currentDisk.Path))
			default:
				logger.Error("failed to enable S.M.A.R.T", zap.Error(err), zap.String("path", currentDisk.Path))
			}
		}

		for _, blkChild := range currentDisk.Children {
			m, ok := mountPointMap[blkChild.UUID]
			if !ok {
				continue
			}
			if blkChild.MountPoint == m {
				continue
			}
			remount(blkChild, m)
		}
	}
}

// remountVolume restores one partition to the mount point the database recorded
// for it, falling back to a sibling path when the recorded one is occupied.
func (d *diskService) remountVolume(blkChild model.LSBLKModel, m string) {
	logger.Info("trying to re-mount...", zap.String("path", blkChild.Path), zap.String("mount point", m))
	// mount point check
	mountPoint := m
	mount.UmountByMountPoint(m)
	dir, _ := os.ReadDir(m)
	if len(dir) > 0 {
		i := 1
		for {
			mountPoint = m + "-" + strconv.Itoa(i)
			if file.CheckNotExist(mountPoint) {
				break
			}
			i++
		}
		logger.Info("mount point already exists, using new mount point", zap.String("path", blkChild.Path), zap.String("mount point", mountPoint))
	}

	if output, err := d.MountDisk(blkChild.Path, mountPoint); err != nil {
		logger.Error(output, zap.Error(err), zap.String("path", blkChild.Path), zap.String("volume", mountPoint))
	}

	// obtain the actual mount path (just in case)
	partitions, err := partition.GetPartitions(blkChild.Path)
	if err != nil {
		logger.Error("error when getting partitions by path", zap.Error(err), zap.String("path", blkChild.Path))
		return
	}

	if len(partitions) == 0 {
		return
	}
	mountPoint = partitions[0].LSBLKProperties["MOUNTPOINT"]

	if mountPoint != m {
		v := model2.Volume{
			UUID:       blkChild.UUID,
			MountPoint: mountPoint,
		}
		if err := d.UpdateMountPointInDB(v); err != nil {
			logger.Error("error when updating mount point in db", zap.Error(err), zap.Any("volume", v))
		}
	}
}

// ErrDiskEnumerationFailed marks a sweep that never obtained a usable inventory
// of block devices. It is the signal for "we do not know what disks exist", which
// callers must keep strictly apart from "there are no disks".
var ErrDiskEnumerationFailed = errors.New("block device enumeration failed")

// DefaultSerialDiskMountDeadline caps how long the boot-time remount sweep may
// run before the caller stops waiting for it.
const DefaultSerialDiskMountDeadline = 5 * time.Minute

const (
	// lsblkFailureCacheKey / lsblkFailureCacheTTL hold the short-lived negative
	// cache that keeps a polling UI from re-arming a new global lsblk every
	// couple of seconds while a device is wedged.
	lsblkFailureCacheKey = "system_lsblk_error"
	lsblkFailureCacheTTL = 15 * time.Second
)

var (
	// Enumeration retry budget: 2s, 4s, 8s, 16s, 32s between attempts, so about
	// a minute of patience for a device that is merely slow to settle (USB
	// enclosures and some SATA controllers routinely take tens of seconds after
	// a cold boot), well inside DefaultSerialDiskMountDeadline so the mounting
	// itself still has time. Variables rather than constants so tests can
	// compress them.
	serialDiskEnumerationAttempts  = 6
	serialDiskEnumerationBaseDelay = 2 * time.Second

	// diskEnumerator is the boot sweep's inventory source, and the seam that
	// lets the sweep's abort-on-failure path be tested without a block device
	// stuck in uninterruptible I/O. The cache is bypassed on purpose: the answer
	// has to be current, and a cached failure must not shorten the retries.
	diskEnumerator = func(d *diskService) ([]model.LSBLKModel, error) { return d.lsblkList(false) }
)

// enumerateDisksWithRetry retries the inventory with exponential backoff and
// returns ErrDiskEnumerationFailed if it never becomes usable.
//
// expectDisks says whether the database holds volumes, i.e. whether this machine
// is known to have data disks. When it does, an empty inventory is treated as a
// failed enumeration rather than as an answer: a box that has volumes recorded
// but reports zero block devices is a box whose lsblk we should not believe, and
// believing it means skipping the remount of every disk in silence.
func enumerateDisksWithRetry(
	enumerate func() ([]model.LSBLKModel, error),
	expectDisks bool,
	attempts int,
	baseDelay time.Duration,
	sleep func(time.Duration),
) ([]model.LSBLKModel, error) {
	var lastErr error

	delay := baseDelay

	for attempt := 1; attempt <= attempts; attempt++ {
		list, err := enumerate()

		switch {
		case err != nil:
			lastErr = err
		case len(list) == 0 && expectDisks:
			lastErr = errors.New("lsblk reported no block devices while the database expects volumes")
		default:
			return list, nil
		}

		logger.Error("failed to enumerate block devices - retrying",
			zap.Error(lastErr),
			zap.Int("attempt", attempt),
			zap.Int("attempts", attempts),
		)

		if attempt < attempts {
			sleep(delay)
			delay *= 2
		}
	}

	return nil, fmt.Errorf("%w: %v", ErrDiskEnumerationFailed, lastErr)
}

// RunDiskSweepWithDeadline runs a disk sweep and reports whether it finished
// within timeout, along with whatever the sweep itself reported. It cannot abort
// the sweep - nothing interrupts a thread parked in uninterruptible disk I/O -
// but it lets a boot-critical caller stop waiting on it. Every probe inside the
// sweep is individually bounded; this is the backstop that keeps a pathological
// set of disks from holding the -init oneshot (and docker, ordered behind it)
// forever. Coming up degraded and saying so out loud beats never coming up at
// all - but "degraded" has to reach the caller, which is why the sweep's error
// is returned instead of being swallowed into a boolean.
func RunDiskSweepWithDeadline(sweep func() error, timeout time.Duration) (bool, error) {
	done := make(chan error, 1)
	go func() {
		done <- sweep()
	}()

	select {
	case err := <-done:
		return true, err
	case <-time.After(timeout):
		return false, nil
	}
}

func (d *diskService) GetUSBDriveStatusList() []model.USBDriveStatus {
	blockList := d.LSBLK(false)
	statusList := []model.USBDriveStatus{}
	for _, v := range blockList {
		if v.Tran != "usb" {
			continue
		}

		isMount := false
		status := model.USBDriveStatus{Model: v.Model, Name: v.Name, Size: v.Size}
		for _, child := range v.Children {
			if len(child.MountPoint) > 0 {
				isMount = true
				avail, _ := strconv.ParseUint(child.FSAvail.String(), 10, 64)
				status.Avail += avail
			}
		}
		if !isMount && len(v.MountPoint) > 0 {
			isMount = true
			avail, _ := strconv.ParseUint(v.FSAvail.String(), 10, 64)
			status.Avail += avail
		}

		if isMount {
			statusList = append(statusList, status)
		}
	}
	return statusList
}

func (d *diskService) InitCheck() {
	time.Sleep(time.Second * 5)
	var fileName string = "local-storage.json"
	diskMap := make(map[string]model.LSBLKModel)
	diskMapNew := make(map[string]model.LSBLKModel)
	diskTempFilePath := filepath.Join(config.AppInfo.DBPath, fileName)
	if file.Exists(diskTempFilePath) {
		tempData := file.ReadFullFile(diskTempFilePath)
		err := json.Unmarshal(tempData, &diskMap)
		if err != nil {
			os.Remove(diskTempFilePath)
		}
	}

	diskList := MyService.Disk().LSBLK(false)
	for _, v := range diskList {
		if IsDiskSupported(v) {
			if _, ok := diskMap[v.Serial]; !ok {
				properties := common.AdditionalProperties(v)
				eventModel := message_bus.Event{
					SourceID:   "local-storage",
					Name:       "local-storage:disk:added",
					Properties: properties,
				}
				// add UI properties to applicable events so that CasaOS UI can render it
				event := common.EventAdapterWithUIProperties(&eventModel)

				bk := false
				for _, k := range v.Children {
					if k.MountPoint == "/" {
						bk = true
						break
					}
					for _, s := range k.Children {
						if s.MountPoint == "/" {
							bk = true
							break
						}
					}
					if bk {
						break
					}
				}
				if bk {
					continue
				}

				logger.Info("disk added", zap.Any("eventModel", eventModel))

				// Wave 35-X: bound the publish so a wedged message-bus
				// cannot pin this udev-event goroutine forever.
				pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
				response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, event.SourceID, event.Name, event.Properties)
				pubCancel()
				if err != nil {
					logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", event))
					continue
				}

				if response != nil && response.StatusCode() != http.StatusOK {
					logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
				}

			}
			diskMapNew[v.Serial] = v
		}
	}
	for k, v := range diskMap {
		if _, ok := diskMapNew[k]; !ok {
			logger.Info("disk removed", zap.Any("disk", v))
			properties := common.AdditionalProperties(v)
			eventModel := message_bus.Event{
				SourceID:   "local-storage",
				Name:       "local-storage:disk:removed",
				Properties: properties,
			}
			event := common.EventAdapterWithUIProperties(&eventModel)
			logger.Info("InitCheck disk removed", zap.Any("eventModel", eventModel))
			// Wave 35-X: bounded publish (see disk-added site above).
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, event.SourceID, event.Name, event.Properties)
			pubCancel()
			if err != nil {
				logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", event))
				continue
			}

			if response != nil && response.StatusCode() != http.StatusOK {
				logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
			}
		}
	}
	data, err := json.Marshal(diskMapNew)
	if err != nil {
		return
	}
	file.WriteToPath(data, config.AppInfo.DBPath, fileName)
}

// systemUsageProbe is the seam GetSystemDf measures the root filesystem
// through, so its degraded paths can be tested without a wedged disk.
var systemUsageProbe = MountUsage

// GetSystemDf describes the filesystem mounted on "/".
//
// It used to shell out to `df -kPT` and then throw away every row but "/". That
// was the bug: df statfs(2)s *every* mount on the machine before printing
// anything, so a single unreachable NFS/CIFS share (this box has had them go
// away with the router rule) hung the command forever. The call had no
// deadline, and GetStorageList runs it on every poll of the storage page, so
// each poll added one more permanently blocked goroutine plus a `df` process in
// D-state that SIGKILL cannot reap, with its pipes and descriptors. Over the
// months this box stays up that ends in resource exhaustion - and it takes out
// the storage page precisely when a mount has gone bad.
//
// The replacement never looks at a filesystem it was not asked about: the
// device and type come from the kernel's mount table (a procfs read, which
// cannot block), and only "/" is measured, through the bounded probe.
func (d *diskService) GetSystemDf() (model.DFDiskSpace, error) {
	root, err := rootMount()
	if err != nil {
		return model.DFDiskSpace{}, err
	}

	df := model.DFDiskSpace{
		FileSystem: root.Source,
		Type:       root.FSType,
		MountedOn:  "/",
	}

	usage, err := systemUsageProbe("/")
	if err != nil {
		// The identity of the root device comes from procfs and is still true;
		// only the numbers are missing. GetStorageList needs nothing but
		// FileSystem to mark the system disk, so it is handed back populated
		// rather than blanked - a probe that timed out must not also erase the
		// facts we do know.
		return df, fmt.Errorf("root filesystem usage unavailable: %w", err)
	}

	if usage.Stale {
		logger.Error("serving stale root filesystem usage - / did not answer statfs",
			zap.Time("measured_at", usage.At))
	}

	df.Blocks = strconv.FormatUint(usage.TotalBytes, 10)
	df.Used = strconv.FormatUint(usage.UsedBytes, 10)
	df.Available = strconv.FormatUint(usage.AvailBytes, 10)
	df.UsePercent = formatUsePercent(usage)

	return df, nil
}

// formatUsePercent reproduces df(1)'s percentage: used over the space a normal
// writer can account for (used + available), rounded up, so a filesystem with
// any bytes spent never reads as 0%.
func formatUsePercent(usage FSUsage) string {
	denominator := usage.UsedBytes + usage.AvailBytes
	if denominator == 0 {
		// df prints "-" for filesystems that report no usable space at all
		// (pseudo filesystems); saying "0%" would claim it is empty.
		return "-"
	}

	percent := (usage.UsedBytes*100 + denominator - 1) / denominator

	return strconv.FormatUint(percent, 10) + "%"
}

func NewDiskService(db *gorm.DB) DiskService {
	return &diskService{db: db}
}

func IsDiskSupported(d model.LSBLKModel) bool {
	return d.Tran == "sata" ||
		d.Tran == "nvme" ||
		d.Tran == "spi" ||
		d.Tran == "sas" ||
		strings.Contains(d.SubSystems, "virtio") ||
		strings.Contains(d.SubSystems, "block:scsi:vmbus:acpi") || // Microsoft Hyper-V
		strings.Contains(d.SubSystems, "block:mmc:mmc_host:pci") ||
		strings.Contains(d.SubSystems, "block:mmc:mmc_host:platform") ||
		strings.Contains(d.SubSystems, "block:scsi:pci") || d.Tran == "usb"
}

// supportedFsTypes lists every filesystem CasaNova will both recognise on
// existing media and offer in the format picker. lsblk reports "ntfs"
// (not "ntfs-3g" — that's the FUSE driver name); the prior list typo
// meant real NTFS drives were never detected. btrfs / xfs / f2fs / zfs
// added so modern NAS workloads aren't silently blocked.
var supportedFsTypes = map[string]struct{}{
	"vfat":    {},
	"exfat":   {},
	"ext2":    {},
	"ext3":    {},
	"ext4":    {},
	"ntfs":    {},
	"btrfs":   {},
	"xfs":     {},
	"f2fs":    {},
	"zfs":     {},
	"iso9660": {},
}

func IsFormatSupported(d model.LSBLKModel) bool {
	_, ok := supportedFsTypes[d.FsType]
	return ok
}

func WalkDisk(rootBlk model.LSBLKModel, depth uint, shouldStopAt func(blk model.LSBLKModel) bool) *model.LSBLKModel {
	if shouldStopAt(rootBlk) {
		return &rootBlk
	}

	if depth == 0 {
		return nil
	}

	for _, blkChild := range rootBlk.Children {
		if blk := WalkDisk(blkChild, depth-1, shouldStopAt); blk != nil {
			return blk
		}
	}

	return nil
}

func ParseBlockDevices(str []byte) ([]model.LSBLKModel, error) {
	var blkList []model.LSBLKModel
	if err := json2.Unmarshal([]byte(jsoniter.Get(str, "blockdevices").ToString()), &blkList); err != nil {
		return nil, err
	}

	return blkList, nil
}

// isSSD checks if a block device is a solid-state drive.
// Reads the kernel's rotational flag from sysfs.
// Returns true for SSDs, false for HDDs, false on error.
func isSSD(deviceName string) bool {
	// deviceName is "sda", "nvme0n1", etc. (no /dev/ prefix)
	data, err := os.ReadFile("/sys/block/" + deviceName + "/queue/rotational")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "0"
}

// isUSBDevice performs multi-check USB detection (inspired by OpenMediaVault).
// Checks 5 different indicators — if ANY match, the device is USB.
func isUSBDevice(devicePath string) bool {
	// Wave 27: bound udevadm. A stalled udev daemon (which we've seen
	// during early boot or after a kernel module reload) used to hang
	// every disk-list refresh forever, because LSBLK calls this in a
	// loop and held the global storage mutex while doing it.
	// Routed through the bounded helper so it shares the abandoned-probe
	// breaker: LSBLK calls this in a loop on every disk-list refresh, and a
	// deadline alone still lets each refresh strand one more udevadm.
	ctx, cancel := context.WithTimeout(context.Background(), deviceProbeTimeout)
	defer cancel()

	out, err := command.ExecuteCommandContext(ctx, "udevadm", "info", "--query=property", "--name="+devicePath)
	if err != nil {
		logDeviceProbeStall("udevadm", devicePath, err)
		return false
	}
	props := string(out)

	// Check 1: ID_BUS=usb
	if strings.Contains(props, "ID_BUS=usb") {
		return true
	}
	// Check 2: ID_USB_DRIVER=usb-storage
	if strings.Contains(props, "ID_USB_DRIVER=usb-storage") {
		return true
	}
	// Check 3: ID_DRIVE_THUMB=1
	if strings.Contains(props, "ID_DRIVE_THUMB=1") {
		return true
	}
	// Check 4: ID_PATH contains -usb-
	for _, line := range strings.Split(props, "\n") {
		if strings.HasPrefix(line, "ID_PATH=") && strings.Contains(line, "-usb-") {
			return true
		}
	}

	// Check 5: sysfs removable flag
	deviceName := filepath.Base(devicePath)
	data, err := os.ReadFile("/sys/block/" + deviceName + "/removable")
	if err == nil && strings.TrimSpace(string(data)) == "1" {
		return true
	}

	return false
}

// EnrichDiskInfo adds SMART health, SSD detection, and USB detection
// to an LSBLKModel. Call this after parsing lsblk output.
func EnrichDiskInfo(disk *model.LSBLKModel) {
	deviceName := filepath.Base(disk.Path)

	// SSD detection
	disk.IsSSD = isSSD(deviceName) || !disk.Rota

	// USB detection (multi-check)
	disk.IsUSB = isUSBDevice(disk.Path)

	// SMART health (cached, won't hammer disks)
	health, err := smart.Query(disk.Path)
	if err == nil && health.Supported {
		disk.SmartSupported = true
		disk.SmartHealth = string(health.Assessment)
		disk.Temperature = health.Temperature
		disk.PowerOnHours = health.PowerOnHours
	}
}
