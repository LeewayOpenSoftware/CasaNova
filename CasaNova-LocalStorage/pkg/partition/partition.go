package partition

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
	"unicode"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/devid"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/command"
)

type Partition struct {
	LSBLKProperties map[string]string
	PARTXProperties map[string]string
}

var (
	ErrNoPartitionFound    = errors.New("no partition found after partition creation")
	ErrNotAPartitionDevice = errors.New("device path does not look like a partition (must end with a digit)")
	ErrFormatRootDevice    = errors.New("cannot format root/system device")
	ErrNotABlockDevice     = errors.New("path is not a block device")
)

// isBlockDevice returns nil if path is an existing block device on
// disk. Without this check FormatPartition would happily mkfs a
// regular file or symlink the caller pointed at — exactly the kind
// of footgun that turns a small input-validation slip in an upstream
// route handler into "we wrote ext4 over /etc/passwd". stat + the
// device-mode bit is the same check OpenMediaVault uses.
func isBlockDevice(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode()&os.ModeDevice == 0 {
		return ErrNotABlockDevice
	}
	return nil
}

func GetDevicePath(uuid string) (string, error) {
	out, err := command.ExecuteCommand("blkid", "--uuid", uuid)
	if err != nil {
		return "", err
	}

	return string(bytes.TrimSpace(out)), nil
}

// path - device path, e.g. /dev/sda
func GetPartitions(path string) ([]Partition, error) {
	var partitions []Partition

	// lsblk
	out, err := command.ExecuteCommand("lsblk", "--pairs", "--bytes", "--output-all", path)
	if err != nil {
		return nil, err
	}
	lsblkPartitions := parseLSBLKOutput(out)

	if len(lsblkPartitions) == 0 {
		return partitions, nil
	}

	// partx
	out, err = command.ExecuteCommand("partx", "--pairs", "--bytes", "--output-all", path)
	if err != nil {
		return nil, err
	}
	partxPartitions := parsePARTXOutput(out)

	if len(partxPartitions) == 0 {
		return partitions, nil
	}

	// merge
	partitions = mergeOutputs(lsblkPartitions, partxPartitions)

	return partitions, nil
}

// inform the operating system about partition table changes
func ProbePartition(device string) error {
	if _, err := command.ExecuteCommand("partprobe", "-s", device); err != nil {
		return err
	}

	return nil
}

// rootDevice - root device, e.g. /dev/sda
func AddPartition(rootDevice string) ([]Partition, error) {
	// add partition
	if _, err := command.ExecuteCommand("parted", "-s", rootDevice, "mkpart", "primary", "0", "100%"); err != nil {
		return nil, err
	}

	if err := ProbePartition(rootDevice); err != nil {
		return nil, err
	}

	var partitions []Partition
	count := 5
	for count > 0 {
		// wait for partition to appear
		result, err := GetPartitions(rootDevice)
		if err != nil {
			return nil, err
		}
		if len(result) > 0 {
			partitions = result
			break
		}

		time.Sleep(1 * time.Second)
		count--
	}

	if len(partitions) == 0 {
		return nil, ErrNoPartitionFound
	}

	return partitions, nil
}

func CreatePartitionTable(rootDevice string) error {
	// create partition table
	if _, err := command.ExecuteCommand("parted", "-s", rootDevice, "mklabel", "gpt"); err != nil {
		return err
	}
	return nil
}

// partitionDevice - partition device, e.g. /dev/sda1
// supportedMkfs lists the filesystems FormatPartitionAs will accept and
// the argv it shells out to. Adding a new entry is the only change
// needed to expose another filesystem in the UI picker — provided the
// matching mkfs.<type> binary is installed on the host.
//
// xfs / btrfs / f2fs all use "-f" to force-overwrite an existing fs.
// ntfs uses "--fast" + "--force" for the same effect via ntfs-3g's
// mkntfs. exfat is mkexfatfs (or mkfs.exfat on newer util-linux).
// zfs is intentionally absent — pool creation is a multi-disk wizard
// flow, not a single-partition mkfs, and lives in its own code path.
var supportedMkfs = map[string][]string{
	"ext4":  {"mkfs.ext4", "-v", "-m", "1", "-F"},
	"ext3":  {"mkfs.ext3", "-v", "-m", "1", "-F"},
	"ext2":  {"mkfs.ext2", "-v", "-m", "1", "-F"},
	"xfs":   {"mkfs.xfs", "-f"},
	"btrfs": {"mkfs.btrfs", "-f"},
	"f2fs":  {"mkfs.f2fs", "-f"},
	"vfat":  {"mkfs.vfat", "-F", "32"},
	"exfat": {"mkfs.exfat"},
	"ntfs":  {"mkfs.ntfs", "--fast", "--force"},
}

// FormatPartition formats a partition as ext4 (legacy behaviour, kept
// for callers that don't pass a filesystem choice).
func FormatPartition(partitionDevice string) error {
	return FormatPartitionAs(partitionDevice, "ext4")
}

// FormatPartitionAs formats a partition as the requested filesystem. The
// caller is responsible for unmounting the device first; we validate
// path safety here so a malformed device path can't be passed through.
func FormatPartitionAs(partitionDevice, fsType string) error {
	if len(partitionDevice) == 0 || !unicode.IsDigit(rune(partitionDevice[len(partitionDevice)-1])) {
		return ErrNotAPartitionDevice
	}

	// Wave 27: confirm the path actually points at a block device.
	// The trailing-digit check above only validates the *string*; an
	// attacker (or a buggy caller) could still pass a regular file
	// like /tmp/x9, a symlink, or a FIFO. mkfs is more than happy to
	// format any of those, which has destroyed user data in the wild.
	if err := isBlockDevice(partitionDevice); err != nil {
		return err
	}

	// Refuse to format the disk the system booted from -- and refuse just as
	// firmly when which disk that is could not be determined.
	//
	// The error check this replaces looked like it covered that, and did not.
	// devid.RootDevice returned ("", nil) -- a NIL error -- for a root it could
	// not name, such as one on LVM or behind a /dev/root symlink, so the only
	// thing left to catch it was IsSameOrChild against an empty string, which
	// is false for every device. The guard passed and mkfs ran on the boot
	// disk. ProbeRoot names that outcome instead of encoding it as an empty
	// string, and Protects answers true for it.
	root := devid.ProbeRoot()
	if root.Protects(partitionDevice) {
		return fmt.Errorf("%w: %s", ErrFormatRootDevice, root.Reason)
	}

	argv, ok := supportedMkfs[fsType]
	if !ok {
		return fmt.Errorf("unsupported filesystem %q", fsType)
	}
	// Build the full argv: mkfs.<fs> [opts...] <device>
	full := make([]string, 0, len(argv)+1)
	full = append(full, argv...)
	full = append(full, partitionDevice)
	if _, err := command.ExecuteCommand(full[0], full[1:]...); err != nil {
		return err
	}
	return nil
}

// rootDevice - root device, e.g. /dev/sda
//
// number - partition number, e.g. 1
func DeletePartition(rootDevice string, number int) error {
	n := strconv.Itoa(number)

	// delete partition
	if _, err := command.ExecuteCommand("sfdisk", "--delete", rootDevice, n); err != nil {
		return err
	}

	return ProbePartition(rootDevice)
}

func parsePARTXOutput(out []byte) map[string]map[string]string {
	partitions := map[string]map[string]string{}
	for _, buf := range bytes.Split(out, []byte("\n")) {
		if len(buf) == 0 {
			continue
		}

		partition := parsePairs(buf)
		if partition["UUID"] == "" {
			continue
		}

		partitions[partition["UUID"]] = partition
	}
	return partitions
}

func parseLSBLKOutput(out []byte) map[string]map[string]string {
	partitions := map[string]map[string]string{}
	for _, buf := range bytes.Split(out, []byte("\n")) {
		if len(buf) == 0 {
			continue
		}

		partition := parsePairs(buf)
		if partition["PARTUUID"] == "" {
			continue
		}

		partitions[partition["PARTUUID"]] = partition
	}
	return partitions
}

func mergeOutputs(lsblkPartitions, partxPartitions map[string]map[string]string) []Partition {
	partitions := []Partition{}
	for uuid, partxPartition := range partxPartitions {
		lsblkPartition, ok := lsblkPartitions[uuid]
		if !ok {
			continue
		}
		partitions = append(partitions, Partition{
			LSBLKProperties: lsblkPartition,
			PARTXProperties: partxPartition,
		})
	}

	return partitions
}

func parsePairs(buf []byte) map[string]string {
	pairs := map[string]string{}
	for _, field := range bytes.Fields(buf) {
		kv := bytes.Split(field, []byte("="))
		if len(kv) != 2 {
			continue
		}
		pairs[string(kv[0])] = string(bytes.Trim(kv[1], "\""))
	}

	return pairs
}
