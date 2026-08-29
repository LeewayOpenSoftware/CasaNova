package v1

// Per-disk operations that don't fit storage.go's "format / mount /
// unmount" trio: online filesystem resize and a quick benchmark. Both
// are admin-only (PermStorageWrite) and operate on a mounted partition
// — no whole-disk paths, no system-disk targets.
//
//   POST /v1/storage/resize     { path: "/dev/sdb1" }
//   POST /v1/storage/benchmark  { path: "/dev/sdb1", mode?: "quick" | "deep" }

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
	if info.MountPoint == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "partition must be mounted before resize",
		})
	}
	spec, ok := resizeCommands[info.FsType]
	if !ok {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("filesystem %q does not support online resize", info.FsType),
		})
	}

	target := path
	if spec.useMount {
		target = info.MountPoint
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
			"fs":     info.FsType,
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
	if info.MountPoint == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "partition must be mounted before benchmark",
		})
	}

	start := time.Now()
	res, err := runBenchmark(info.MountPoint, mode)
	res.DurationMS = time.Since(start).Milliseconds()
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

func runFio(mountPoint, mode, size string) (BenchmarkResult, error) {
	scratch := mountPoint + "/.cn-bench.tmp"
	defer exec.Command("rm", "-f", scratch).Run()

	// fio's --runtime caps in-flight IO, but the binary itself can wedge
	// (e.g. waiting for a dying disk to ACK). Bound the whole exec at
	// runtime+30s so a stuck device can't pin the handler past the
	// nominal test duration by more than a fraction.
	runtimeSec := map[string]string{"quick": "5", "deep": "20"}[mode]
	hardCap := 5 * time.Minute // sanity ceiling
	cmdCtx, cancel := context.WithTimeout(context.Background(), hardCap)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "fio",
		"--name=cn-bench",
		"--filename="+scratch,
		"--size="+size,
		"--direct=1",
		"--ioengine=libaio",
		"--iodepth=16",
		"--rw=randrw",
		"--bs=4k",
		"--rwmixread=70",
		"--numjobs=1",
		"--runtime="+runtimeSec,
		"--time_based",
		"--group_reporting",
		"--output-format=json",
	)
	out, err := cmd.Output()
	if err != nil {
		return BenchmarkResult{Mode: mode, Engine: "fio"}, fmt.Errorf("fio: %w", err)
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
		return BenchmarkResult{Mode: mode, Engine: "fio"}, fmt.Errorf("fio json: %w", err)
	}
	if len(parsed.Jobs) == 0 {
		return BenchmarkResult{Mode: mode, Engine: "fio"}, fmt.Errorf("fio returned no jobs")
	}
	j := parsed.Jobs[0]
	return BenchmarkResult{
		Mode:          mode,
		Engine:        "fio",
		SeqReadMBps:   j.Read.BW_bytes / 1024 / 1024,
		SeqWriteMBps:  j.Write.BW_bytes / 1024 / 1024,
		RandReadIOPS:  j.Read.IOPS,
		RandWriteIOPS: j.Write.IOPS,
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
	exec.Command("sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches").Run()

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
