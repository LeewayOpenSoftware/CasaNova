package v2

import (
	"errors"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/fstab"
	"go.uber.org/zap"
)

func (s *LocalStorageService) SaveToFStab(m codegen.Mount) error {
	ft := fstab.Get()

	if err := ft.Add(fstab.Entry{
		MountPoint: m.MountPoint,

		Source:  *m.Source,
		FSType:  *m.Fstype,
		Options: *m.Options,
		Dump:    0,
		Pass:    fstab.PassDoNotCheck,
	}, true); err != nil {
		logger.Error("Error when trying to persist mount", zap.Error(err), zap.Any("mount", m))
		return err
	}
	return nil
}

func (s *LocalStorageService) RemoveFromFStab(mountpoint string) error {
	ft := fstab.Get()

	if err := ft.RemoveByMountPoint(mountpoint, false); err != nil {
		// A refusal is not the same as a failure, but it must NOT be reported
		// as success: the entry is still in /etc/fstab and the mount will come
		// back on the next boot. Surface it with its own message so the
		// operator knows the line is theirs to remove, instead of watching an
		// "unmounted" filesystem reappear after a reboot.
		if errors.Is(err, fstab.ErrEntryNotManaged) {
			logger.Error("refusing to remove an fstab entry we did not add — it will persist across reboots",
				zap.String("mount point", mountpoint))
			return err
		}
		logger.Error("Error when trying to unpersist mount", zap.Error(err), zap.String("mount point", mountpoint))
		return err
	}
	return nil
}
