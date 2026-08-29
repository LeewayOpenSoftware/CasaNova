package service

import (
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"go.uber.org/zap"
)

type StorageService interface {
	MountStorage(mountPoint, fs string) error
	UnmountStorage(mountPoint string) error
	GetStorages() (httper.MountList, error)
	CreateConfig(data map[string]string, name string, t string) error
	CheckAndMountByName(name string) error
	CheckAndMountAll() error
	GetConfigByName(name string) (map[string]string, error)
	DeleteConfigByName(name string) error
	GetConfig() (httper.RemotesResult, error)
}

type storageStruct struct {
}

func (s *storageStruct) MountStorage(mountPoint, fs string) error {
	file.IsNotExistMkDir(mountPoint)
	return httper.Mount(mountPoint, fs)
}
func (s *storageStruct) UnmountStorage(mountPoint string) error {
	err := httper.Unmount(mountPoint)
	if err == nil {
		dir, _ := os.ReadDir(mountPoint)

		if len(dir) == 0 {
			file.RMDir(mountPoint)
		}
		return nil
	}
	return err
}
func (s *storageStruct) GetStorages() (httper.MountList, error) {
	return httper.GetMountList()
}

// CreateConfig persists an rclone remote.
//
// It used to call httper.CreateConfig and `return nil`, discarding the result.
// Its only caller is the cloud OAuth callback, which now treats
// credential-write + mount as one transaction and rolls the credential back
// when the mount fails — a swallowed error here made that transaction a no-op
// and made the callback announce success for a remote that was never written.
func (s *storageStruct) CreateConfig(data map[string]string, name string, t string) error {
	return httper.CreateConfig(data, name, t)
}
func (s *storageStruct) CheckAndMountByName(name string) error {
	storages, _ := MyService.Storage().GetStorages()
	currentRemote, _ := httper.GetConfigByName(name)
	mountPoint := currentRemote["mount_point"]
	isMount := false
	for _, v := range storages.MountPoints {
		if v.MountPoint == mountPoint {
			isMount = true
			break
		}
	}
	if !isMount {
		return MyService.Storage().MountStorage(mountPoint, name+":")
	}
	return nil
}
func (s *storageStruct) CheckAndMountAll() error {
	storages, err := MyService.Storage().GetStorages()
	if err != nil {
		return err
	}
	logger.Info("when CheckAndMountAll storages", zap.Any("storages", storages))
	section, err := httper.GetAllConfigName()
	if err != nil {
		return err
	}
	logger.Info("when CheckAndMountAll section", zap.Any("section", section))
	for _, v := range section.Remotes {
		currentRemote, _ := httper.GetConfigByName(v)
		mountPoint := currentRemote["mount_point"]
		if len(mountPoint) == 0 {
			continue
		}
		isMount := false
		for _, v := range storages.MountPoints {
			if v.MountPoint == mountPoint {
				isMount = true
				break
			}
		}
		if !isMount {
			logger.Info("when CheckAndMountAll MountStorage", zap.String("mountPoint", mountPoint), zap.String("fs", v))
			err := MyService.Storage().MountStorage(mountPoint, v+":")
			if err != nil {
				logger.Error("when CheckAndMountAll then", zap.String("mountPoint", mountPoint), zap.String("fs", v), zap.Error(err))
			}
		}
	}
	return nil
}

func (s *storageStruct) GetConfigByName(name string) (map[string]string, error) {
	return httper.GetConfigByName(name)
}
func (s *storageStruct) DeleteConfigByName(name string) error {
	return httper.DeleteConfigByName(name)
}
func (s *storageStruct) GetConfig() (httper.RemotesResult, error) {
	section, err := httper.GetAllConfigName()
	if err != nil {
		return httper.RemotesResult{}, err
	}
	return section, nil
}
func NewStorageService() StorageService {
	return &storageStruct{}
}
