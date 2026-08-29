package partition

import (
	"bytes"
	"context"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
)

// The bounded twins of GetDevicePath / GetPartitions.
//
// They exist as separate entry points instead of a deadline baked into the
// originals because the unbounded versions share command.ExecuteCommand with
// mkfs/parted/sfdisk, where a deadline would be actively harmful. Callers that
// poll these probes in a loop - pool assembly waiting for its disks, the InitMerge
// HTTP handler - need the opposite guarantee: the probe must die with the
// caller's budget, even mid-flight, because blkid and partx block forever on a
// drive in uninterruptible I/O and would otherwise pin the goroutine for the
// life of the process.

// GetDevicePathContext resolves a filesystem UUID to its device path, giving up
// when ctx expires.
func GetDevicePathContext(ctx context.Context, uuid string) (string, error) {
	out, err := command.ExecuteCommandContext(ctx, "blkid", "--uuid", uuid)
	if err != nil {
		return "", err
	}

	return string(bytes.TrimSpace(out)), nil
}

// GetPartitionsContext lists the partitions of a device, giving up when ctx
// expires. Kept in lockstep with GetPartitions: same two probes, same
// short-circuit when either reports nothing.
func GetPartitionsContext(ctx context.Context, path string) ([]Partition, error) {
	var partitions []Partition

	out, err := command.ExecuteCommandContext(ctx, "lsblk", "--pairs", "--bytes", "--output-all", path)
	if err != nil {
		return nil, err
	}

	lsblkPartitions := parseLSBLKOutput(out)
	if len(lsblkPartitions) == 0 {
		return partitions, nil
	}

	out, err = command.ExecuteCommandContext(ctx, "partx", "--pairs", "--bytes", "--output-all", path)
	if err != nil {
		return nil, err
	}

	partxPartitions := parsePARTXOutput(out)
	if len(partxPartitions) == 0 {
		return partitions, nil
	}

	return mergeOutputs(lsblkPartitions, partxPartitions), nil
}
