package mount

import (
	"context"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
)

func Mount(source string, mountpoint string, fstype *string, options *string) error {
	args := []string{"--verbose"}

	if fstype != nil && *fstype != "" {
		args = append(args, "-t", *fstype)
	}

	if options != nil && *options != "" {
		args = append(args, "-o", *options)
	}

	args = append(args, source, mountpoint)

	// Bounded: mount against a dead NFS/SMB server otherwise blocks in the
	// kernel forever, and this is reachable from an HTTP handler. 90s is
	// generous for any healthy mount; a hung one is turned into an error
	// instead of a stuck worker.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := command.ExecuteCommandContext(ctx, "mount", args...); err != nil {
		return err
	}

	return nil
}

func UmountByMountPoint(mountpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := command.ExecuteCommandContext(ctx, "umount", "--force", "--verbose", "--quiet", mountpoint); err != nil {
		return err
	}

	return nil
}

func UmountByDevice(device string) error {
	if _, err := command.ExecuteCommand("umount", "--force", "--verbose", "--quiet", "--recursive", device); err != nil {
		return err
	}

	return nil
}
