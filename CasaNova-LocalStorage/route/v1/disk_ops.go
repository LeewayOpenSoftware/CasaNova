package v1

// Per-disk operations that don't fit storage.go's "format / mount /
// unmount" trio: online filesystem resize and a quick benchmark. Both
// are admin-only (PermStorageWrite) and act on a mounted filesystem.
//
// The caller may name either the partition or the whole disk. The
// storage page only has disk rows, so in practice it always sends the
// disk; mountedFilesystems below resolves that to the filesystem the
// operation can actually touch.
//
//   POST /v1/storage/resize     { path: "/dev/sdb"  | "/dev/sdb1" }
//   POST /v1/storage/benchmark  { path: "/dev/sda"  | "/dev/sda1", mode?: "quick" | "deep" }

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	model1 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	"github.com/labstack/echo/v4"
)

// resizeCommands maps each supported filesystem to the argv that grows
// it to fill the partition. Only filesystems with online-grow support
// are listed — shrinking ext is offline, btrfs and xfs are online-grow
// only, vfat/exfat don't support resize at all.
//
// Each entry has two argv layouts: one that accepts a mount point, one
// that accepts a device path. We try mount-point first because btrfs
// and xfs require the mounted target, not the device.
var resizeCommands = map[string]struct {
	useMount bool
	argv     []string // ends with the path placeholder, appended at call time
}{
	"ext4":  {false, []string{"resize2fs"}},
	"ext3":  {false, []string{"resize2fs"}},
	"ext2":  {false, []string{"resize2fs"}},
	"xfs":   {true, []string{"xfs_growfs"}},
	"btrfs": {true, []string{"btrfs", "filesystem", "resize", "max"}},
	"f2fs":  {false, []string{"resize.f2fs", "-s"}},
}

// mountedFS is a filesystem an operation can act on: where it lives, where
// it is mounted, and what it is formatted as.
type mountedFS struct {
	Device     string
	MountPoint string
	FsType     string
}

// mountedFilesystems returns every mounted filesystem at or beneath a device.
//
// WHY THIS EXISTS. Both handlers below used to read info.MountPoint straight
// off the device the caller named, and reject the request when it was empty.
// A whole disk is never itself mounted — its partitions are — and the storage
// page has no partition rows: every menu entry passes the disk. So the check
// was true on every real click, and both operations answered "partition must
// be mounted" about partitions that were mounted the whole time. On the test
// box, /dev/sda reports no mountpoint while its only child /dev/sda1 is
// mounted at /mnt/UNIT5.
//
// The walk is recursive rather than one level deep so a filesystem sitting on
// LVM or LUKS (disk → partition → crypt → lv) is still found.
func mountedFilesystems(node model1.LSBLKModel) []mountedFS {
	found := []mountedFS{}
	// A swap partition reports "[SWAP]" as its mountpoint. It is not a
	// directory, so writing a scratch file there or growing it online is
	// meaningless — excluded so it can never be picked as a target.
	if node.MountPoint != "" && strings.HasPrefix(node.MountPoint, "/") {
		found = append(found, mountedFS{
			Device:     node.Path,
			MountPoint: node.MountPoint,
			FsType:     node.FsType,
		})
	}
	for _, child := range node.Children {
		found = append(found, mountedFilesystems(child)...)
	}
	return found
}

func PostResizeDisk(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	body := map[string]string{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	path := strings.TrimSpace(body["path"])
	if err := validateDevicePath(ctx, path); err != nil {
		return err
	}

	// The partition must (a) be a real block device we recognise and
	// (b) currently be mounted — online-grow needs a live target.
	info := service.MyService.Disk().GetDiskInfo(path)
	if info.Path == "" {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "unknown device",
		})
	}
	// Resizing is a real mutation, so it resolves only when the answer is
	// unambiguous. A disk holding two mounted filesystems gives no basis for
	// choosing one, and guessing would grow a filesystem the operator never
	// named — so it says which candidates it found and stops.
	mounts := mountedFilesystems(info)
	if len(mounts) == 0 {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("%s has no mounted filesystem; mount it before resizing", path),
		})
	}
	if len(mounts) > 1 {
		names := make([]string, 0, len(mounts))
		for _, m := range mounts {
			names = append(names, m.Device)
		}
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("%s holds several mounted filesystems (%s); name the one to resize",
				path, strings.Join(names, ", ")),
		})
	}
	fs := mounts[0]

	spec, ok := resizeCommands[fs.FsType]
	if !ok {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("filesystem %q does not support online resize", fs.FsType),
		})
	}

	// resize2fs takes the device, xfs_growfs and btrfs take the mount point.
	// Either way it is the resolved filesystem's own path, never the disk the
	// caller named — resize2fs /dev/sda would fail on a partitioned disk.
	target := fs.Device
	if spec.useMount {
		target = fs.MountPoint
	}
	argv := append([]string{}, spec.argv...)
	argv = append(argv, target)

	// resize2fs / xfs_growfs / btrfs fs resize complete in seconds for
	// typical NAS drives; 10 minutes is the paranoid upper bound that
	// catches a wedged kernel without locking out the operator.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, argv[0], argv[1:]...).CombinedOutput()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return ctx.JSON(http.StatusGatewayTimeout, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: argv[0] + " timed out after 10m and was killed",
		})
	}
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("%s failed: %s: %s", argv[0], err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]string{
			"fs":     fs.FsType,
			"target": target,
			"output": strings.TrimSpace(string(out)),
		},
	})
}

// BenchmarkResult is the per-operation summary the UI renders. We don't
// return raw fio JSON — it's enormous and most of it is uninteresting.
type BenchmarkResult struct {
	Mode          string  `json:"mode"`   // "quick" | "deep"
	Engine        string  `json:"engine"` // "fio" | "dd"
	DurationMS    int64   `json:"durationMs"`
	SeqReadMBps   float64 `json:"seqReadMBps"`
	SeqWriteMBps  float64 `json:"seqWriteMBps"`
	RandReadIOPS  float64 `json:"randReadIOPS"`
	RandWriteIOPS float64 `json:"randWriteIOPS"`
	Notes         string  `json:"notes,omitempty"`
}

func PostBenchmarkDisk(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	body := map[string]string{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	path := strings.TrimSpace(body["path"])
	mode := strings.TrimSpace(body["mode"])
	if mode == "" {
		mode = "quick"
	}
	if mode != "quick" && mode != "deep" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "mode must be 'quick' or 'deep'",
		})
	}
	if err := validateDevicePath(ctx, path); err != nil {
		return err
	}

	info := service.MyService.Disk().GetDiskInfo(path)
	if info.Path == "" {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "unknown device",
		})
	}
	// Unlike resize, several candidates are not ambiguous here: every
	// filesystem on the device sits on the same hardware, and the benchmark
	// only writes a scratch file it deletes. So it takes the first and names
	// it in the result, because "180 MB/s" without a target is not a
	// measurement anyone can repeat.
	mounts := mountedFilesystems(info)
	if len(mounts) == 0 {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("%s has no mounted filesystem to benchmark; mount it first", path),
		})
	}
	fs := mounts[0]

	start := time.Now()
	res, err := runBenchmark(fs.MountPoint, mode)
	res.DurationMS = time.Since(start).Milliseconds()
	where := fmt.Sprintf("measured on %s (%s)", fs.MountPoint, fs.Device)
	if res.Notes == "" {
		res.Notes = where
	} else {
		res.Notes += "; " + where
	}
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "benchmark failed: " + err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    res,
	})
}

// runBenchmark dispatches to fio if present (richer JSON output, real
// 4k random IO) or falls back to dd for a sequential-only quick number.
// The mode argument controls how big the test file is — "quick" uses a
// 256 MiB scratch, "deep" uses 1 GiB. Always writes to the mount point
// (NOT the device); raw-device benchmarking would clobber data.
func runBenchmark(mountPoint, mode string) (BenchmarkResult, error) {
	size := "256M"
	if mode == "deep" {
		size = "1G"
	}
	if _, err := exec.LookPath("fio"); err == nil {
		return runFio(mountPoint, mode, size)
	}
	return runDD(mountPoint, mode, size)
}

// fioPhase is one fio invocation's numbers. fio reports bandwidth and IOPS
// for reads and writes separately; which of the four a caller cares about
// depends on what workload it just ran.
type fioPhase struct {
	ReadBps   float64
	WriteBps  float64
	ReadIOPS  float64
	WriteIOPS float64
}

// runFioPhase runs one workload against the scratch file and parses its
// summary. Everything that is the same for every phase — direct IO, the
// engine, the scratch path, JSON output — lives here; the caller supplies
// only what makes its workload that workload.
func runFioPhase(ctx context.Context, scratch, size, runtimeSec string, workload ...string) (fioPhase, error) {
	argv := []string{
		"--name=cn-bench",
		"--filename=" + scratch,
		"--size=" + size,
		"--direct=1",
		"--ioengine=libaio",
		"--numjobs=1",
		"--runtime=" + runtimeSec,
		"--time_based",
		"--group_reporting",
		"--output-format=json",
	}
	argv = append(argv, workload...)

	out, err := exec.CommandContext(ctx, "fio", argv...).Output()
	if err != nil {
		return fioPhase{}, fmt.Errorf("fio: %w", err)
	}
	var parsed struct {
		Jobs []struct {
			Read struct {
				BW_bytes float64 `json:"bw_bytes"`
				IOPS     float64 `json:"iops"`
			} `json:"read"`
			Write struct {
				BW_bytes float64 `json:"bw_bytes"`
				IOPS     float64 `json:"iops"`
			} `json:"write"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return fioPhase{}, fmt.Errorf("fio json: %w", err)
	}
	if len(parsed.Jobs) == 0 {
		return fioPhase{}, fmt.Errorf("fio returned no jobs")
	}
	j := parsed.Jobs[0]
	return fioPhase{
		ReadBps:   j.Read.BW_bytes,
		WriteBps:  j.Write.BW_bytes,
		ReadIOPS:  j.Read.IOPS,
		WriteIOPS: j.Write.IOPS,
	}, nil
}

// runPhase is the seam the mapping test writes through. runFio's bug was not
// in how it talked to fio, it was in which workload each result field was fed
// from — so the test needs to substitute the workloads, not the parsing.
var runPhase = runFioPhase

// runFio measures the two things the result claims to report, in two passes.
//
// WHY TWO. This used to run a single 4 KiB random job and fill in all four
// result fields from it: its bandwidth became "sequential throughput" and its
// IOPS became "random IOPS". Those are the same measurement counted two ways —
// 90.9 IOPS x 4 KiB is exactly the 0.36 MB/s that got reported as sequential —
// so a 5.5 TB drive that streams around 180 MB/s was shown as doing 0.36, and
// the dialog's promise of "sequential + random" was true of neither number.
// Anyone comparing two disks on that figure was comparing random IOPS while
// reading a label that said throughput.
//
// So: one pass with a large block size for streaming bandwidth, one with 4 KiB
// at depth for random IOPS. Each field now comes from the workload that field
// names.
func runFio(mountPoint, mode, size string) (BenchmarkResult, error) {
	scratch := mountPoint + "/.cn-bench.tmp"
	defer exec.Command("rm", "-f", scratch).Run()

	// fio's --runtime caps in-flight IO, but the binary itself can wedge
	// (e.g. waiting for a dying disk to ACK), so the exec is bounded too.
	//
	// The bound is proportional to the mode rather than a blanket ceiling.
	// Two phases at runtimeSec each, plus fio laying out the scratch file and
	// opening it O_DIRECT, is the real cost; the slack above that is for a
	// slow spinning disk, not for a dead one. A flat five minutes was both too
	// loose to bound a wedged device usefully and far longer than any caller
	// is willing to wait — see BENCHMARK_TIMEOUT_MS in the UI's storage API,
	// which has to exceed whatever this is.
	runtimeSec, ceiling := "5", 90*time.Second
	if mode == "deep" {
		runtimeSec, ceiling = "20", 3*time.Minute
	}
	cmdCtx, cancel := context.WithTimeout(context.Background(), ceiling)
	defer cancel()

	// Sequential: 1 MiB blocks, modest depth. This is the number that answers
	// "how fast does a large file copy go".
	seq, err := runPhase(cmdCtx, scratch, size, runtimeSec,
		"--rw=rw", "--bs=1M", "--iodepth=8", "--rwmixread=50")
	if err != nil {
		return BenchmarkResult{Mode: mode, Engine: "fio"}, err
	}

	// Random: 4 KiB at depth 16, read-weighted. This is the number that
	// answers "how does it behave under a database or many small files".
	rnd, err := runPhase(cmdCtx, scratch, size, runtimeSec,
		"--rw=randrw", "--bs=4k", "--iodepth=16", "--rwmixread=70")
	if err != nil {
		return BenchmarkResult{Mode: mode, Engine: "fio"}, err
	}

	return BenchmarkResult{
		Mode:          mode,
		Engine:        "fio",
		SeqReadMBps:   seq.ReadBps / 1024 / 1024,
		SeqWriteMBps:  seq.WriteBps / 1024 / 1024,
		RandReadIOPS:  rnd.ReadIOPS,
		RandWriteIOPS: rnd.WriteIOPS,
	}, nil
}

// runDD is the fallback for hosts that don't ship fio. Sequential only —
// no random IOPS — but a single dd write+read pair is enough to surface
// "the disk feels slow today" without depending on a non-default tool.
func runDD(mountPoint, mode, size string) (BenchmarkResult, error) {
	scratch := mountPoint + "/.cn-bench.tmp"
	defer exec.Command("rm", "-f", scratch).Run()

	// dd has no built-in timeout and writes until count is reached.
	// Bound each phase at 5 minutes — a dead disk shouldn't pin the
	// handler past that.
	ddCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Write phase
	writeCmd := exec.CommandContext(ddCtx, "dd",
		"if=/dev/zero", "of="+scratch, "bs=1M",
		"count="+strings.TrimSuffix(size, "M"), "conv=fdatasync,notrunc", "status=progress")
	wOut, err := writeCmd.CombinedOutput()
	if err != nil {
		return BenchmarkResult{Mode: mode, Engine: "dd"}, fmt.Errorf("dd write: %w", err)
	}
	writeMBps := parseDDSpeed(string(wOut))

	// Drop cache before read so we measure real disk IO.
	func() {
		// `sync` flushes EVERY filesystem, including whatever disk is being
		// benchmarked precisely because it misbehaves — a wedged one parks
		// sync in D-state. The write/read phases around this are already
		// deadline-bound; without a bound here the whole benchmark handler
		// could still hang on the one unbounded step between them.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches")
		cmd.WaitDelay = 5 * time.Second
		_ = cmd.Run()
	}()

	readCmd := exec.CommandContext(ddCtx, "dd", "if="+scratch, "of=/dev/null", "bs=1M", "status=progress")
	rOut, err := readCmd.CombinedOutput()
	if err != nil {
		return BenchmarkResult{Mode: mode, Engine: "dd"}, fmt.Errorf("dd read: %w", err)
	}
	readMBps := parseDDSpeed(string(rOut))

	return BenchmarkResult{
		Mode:         mode,
		Engine:       "dd",
		SeqReadMBps:  readMBps,
		SeqWriteMBps: writeMBps,
		Notes:        "fio not installed — sequential numbers only",
	}, nil
}

// parseDDSpeed walks dd's stderr (delivered through CombinedOutput) for
// the final line of the form "... 12.3 MB/s" or "... 1.2 GB/s" and
// returns the throughput in MiB/s. Returns 0 when the format doesn't
// match — dd's wording varies across coreutils versions.
func parseDDSpeed(out string) float64 {
	scanner := bufio.NewScanner(strings.NewReader(out))
	last := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "B/s") {
			last = line
		}
	}
	if last == "" {
		return 0
	}
	fields := strings.Fields(last)
	for i, f := range fields {
		if strings.HasSuffix(f, "B/s") && i > 0 {
			val, err := strconv.ParseFloat(fields[i-1], 64)
			if err != nil {
				return 0
			}
			switch f {
			case "kB/s", "KB/s":
				return val / 1024
			case "MB/s":
				return val
			case "GB/s":
				return val * 1024
			}
		}
	}
	return 0
}
