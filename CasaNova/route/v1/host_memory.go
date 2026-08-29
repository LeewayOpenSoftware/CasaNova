package v1

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// Host memory management: cap how much RAM the Docker stack may use, applied
// live via `systemctl set-property docker.service`. systemd (PID 1) applies
// the cgroup-v2 limit in place and persists its own drop-in under
// /etc/systemd/system.control — our process never writes /etc, so this works
// from inside casanova.service's ProtectSystem=full sandbox.
//
// The unit name is a HARDCODED constant. The API never accepts a unit name
// from the client — arbitrary set-property is root-equivalent.
const dockerUnit = "docker.service"

// memSizeRe accepts systemd byte sizes like "4G", "512M". Only M/G — the
// realistic range for a NAS — so a fat-fingered "4" (bytes) can't sneak in.
var memSizeRe = regexp.MustCompile(`^[0-9]+[MG]$`)

type HostMemoryInfo struct {
	MemTotalKiB int64  `json:"memTotalKiB"`
	DockerUsedB int64  `json:"dockerUsedBytes"` // cgroup memory.current, -1 if unknown
	MemoryMax   string `json:"memoryMax"`       // "infinity" when unlimited
	MemoryHigh  string `json:"memoryHigh"`
	FloorMiB    int64  `json:"floorMiB"` // lowest limit the API will accept
}

// GET /v1/sys/host/memory — current Docker memory limits + usage.
func GetHostMemory(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	info := HostMemoryInfo{
		MemTotalKiB: meminfoKiB("MemTotal"),
		DockerUsedB: dockerCgroupCurrent(),
		MemoryMax:   showUnitProp(dockerUnit, "MemoryMax"),
		MemoryHigh:  showUnitProp(dockerUnit, "MemoryHigh"),
		FloorMiB:    memoryFloorMiB(),
	}
	// systemd reports an unset MemoryMax as the max uint64; normalise to the
	// user-facing "infinity" so the UI shows "unlimited" cleanly.
	if info.MemoryMax == "" || info.MemoryMax == "18446744073709551615" {
		info.MemoryMax = "infinity"
	}
	if info.MemoryHigh == "" || info.MemoryHigh == "18446744073709551615" {
		info.MemoryHigh = "infinity"
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    info,
	})
}

// PUT /v1/sys/host/memory  body: {"memoryMax":"4G"}  ("infinity" clears it)
func PutHostMemory(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		MemoryMax string `json:"memoryMax"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: err.Error()})
	}
	val := strings.TrimSpace(body.MemoryMax)

	var maxProp, highProp string
	if val == "infinity" || val == "" {
		maxProp, highProp = "MemoryMax=infinity", "MemoryHigh=infinity"
	} else {
		if !memSizeRe.MatchString(val) {
			return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: "memoryMax must look like 4G or 512M, or be \"infinity\""})
		}
		bytes, err := parseMemSize(val)
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: err.Error()})
		}
		floor := memoryFloorMiB() * 1024 * 1024
		if bytes < floor {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: fmt.Sprintf("limit too low: minimum is %dMiB so the management stack stays alive", memoryFloorMiB()),
			})
		}
		// MemoryHigh (soft throttle) at 90% of the hard cap gives the kernel
		// room to reclaim before the OOM-killing MemoryMax hits.
		high := bytes / 10 * 9
		maxProp = "MemoryMax=" + val
		highProp = "MemoryHigh=" + strconv.FormatInt(high, 10)
	}

	// set-property --runtime=false persists the drop-in so the limit survives
	// a reboot. systemd applies it to the live cgroup immediately.
	out, err := exec.Command("systemctl", "set-property", dockerUnit, maxProp, highProp).CombinedOutput()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "set-property failed: " + strings.TrimSpace(string(out)),
		})
	}
	return GetHostMemory(ctx)
}

// ── helpers ──────────────────────────────────────────────────────────────

func showUnitProp(unit, prop string) string {
	out, err := exec.Command("systemctl", "show", unit, "-p", prop, "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerCgroupCurrent reads the docker.service cgroup-v2 live memory usage.
// Returns -1 when the path is absent (docker not managed / cgroup v1).
func dockerCgroupCurrent() int64 {
	for _, p := range []string{
		"/sys/fs/cgroup/system.slice/docker.service/memory.current",
	} {
		if raw, err := os.ReadFile(p); err == nil {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
				return v
			}
		}
	}
	return -1
}

// memoryFloorMiB is the lowest hard cap the API allows: max(2GiB, 25% of RAM).
// It guarantees the casanova-* management stack (which is NOT in docker's
// cgroup) plus Docker's own daemon have room, so a too-tight limit can never
// lock the user out of the UI that would undo it.
func memoryFloorMiB() int64 {
	quarterMiB := meminfoKiB("MemTotal") / 1024 / 4
	const twoGiB = 2048
	if quarterMiB > twoGiB {
		return quarterMiB
	}
	return twoGiB
}

func meminfoKiB(key string) int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	prefix := key + ":"
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			f := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(f) >= 1 {
				if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

func parseMemSize(s string) (int64, error) {
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch s[len(s)-1] {
	case 'M':
		return n * 1024 * 1024, nil
	case 'G':
		return n * 1024 * 1024 * 1024, nil
	}
	return 0, fmt.Errorf("bad size unit")
}
