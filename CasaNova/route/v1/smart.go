package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/labstack/echo/v4"
)

// smartExec runs a storage tool with a hard deadline. Without CommandContext a
// smartctl against a wedged/dying disk (exactly the disk you are asking about)
// blocks in uninterruptible I/O forever, pinning the HTTP handler and its
// goroutine for the life of the process — and the SMART page is polled.
func smartExec(timeout time.Duration, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).Output()
}

func smartExecCombined(timeout time.Duration, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

// ---- JSON shapes returned by lsblk -J and smartctl -j ----

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name   string  `json:"name"`
	Size   string  `json:"size"`
	Type   string  `json:"type"`
	Model  *string `json:"model"`
	Serial *string `json:"serial"`
	Rota   bool    `json:"rota"`
	Tran   *string `json:"tran"`
}

type smartctlJSON struct {
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	AtaSmartAttributes *struct {
		Table []smartAttribute `json:"table"`
	} `json:"ata_smart_attributes"`
	Smartctl *struct {
		ExitStatus int        `json:"exit_status"`
		Messages   []smartMsg `json:"messages"`
	} `json:"smartctl"`
}

type smartMsg struct {
	String   string `json:"string"`
	Severity string `json:"severity"`
}

type smartAttribute struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Value  int    `json:"value"`
	Worst  int    `json:"worst"`
	Thresh int    `json:"thresh"`
	Raw    struct {
		Value int    `json:"value"`
		Str   string `json:"string"`
	} `json:"raw"`
}

// ---- Response types ----

type SmartAttributeResponse struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Value  int    `json:"value"`
	Worst  int    `json:"worst"`
	Thresh int    `json:"thresh"`
	Raw    int    `json:"raw"`
}

type SmartDiskInfo struct {
	Device             string                   `json:"device"`
	Model              string                   `json:"model"`
	Serial             string                   `json:"serial"`
	Size               string                   `json:"size"`
	Type               string                   `json:"type"`
	Transport          string                   `json:"transport"`
	SmartAvailable     bool                     `json:"smart_available"`
	SmartHealthy       bool                     `json:"smart_healthy"`
	TemperatureC       *int                     `json:"temperature_c"`
	PowerOnHours       int                      `json:"power_on_hours"`
	ReallocatedSectors int                      `json:"reallocated_sectors"`
	PendingSectors     int                      `json:"pending_sectors"`
	Attributes         []SmartAttributeResponse `json:"attributes"`
}

// GET /v1/sys/smart — Collect SMART health data for all physical disks.
func GetSmartSummary(ctx echo.Context) error {
	// Gated: drive models, serials and health are an inventory of the array.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}

	// Check if smartctl is installed.
	if _, err := exec.LookPath("smartctl"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "smartctl not found",
			Data:    []SmartDiskInfo{},
		})
	}

	// List block devices.
	lsblkOut, err := smartExec(10*time.Second, "lsblk", "-J", "-o", "NAME,SIZE,TYPE,MODEL,SERIAL,ROTA,TRAN")
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to list block devices: " + err.Error(),
			Data:    []SmartDiskInfo{},
		})
	}

	var lsblk lsblkOutput
	if err := json.Unmarshal(lsblkOut, &lsblk); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to parse lsblk output",
			Data:    []SmartDiskInfo{},
		})
	}

	var results []SmartDiskInfo
	for _, dev := range lsblk.Blockdevices {
		if dev.Type != "disk" {
			continue
		}

		devPath := "/dev/" + dev.Name
		info := SmartDiskInfo{
			Device:     devPath,
			Model:      derefStr(dev.Model),
			Serial:     derefStr(dev.Serial),
			Size:       dev.Size,
			Transport:  derefStr(dev.Tran),
			Attributes: []SmartAttributeResponse{},
		}

		// Determine SSD vs HDD.
		if dev.Rota {
			info.Type = "hdd"
		} else {
			info.Type = "ssd"
		}

		// Run smartctl -j -a /dev/NAME. Smartctl may exit non-zero even
		// when it produces valid JSON (e.g. exit 4 = SMART error logged),
		// so we parse stdout regardless of error.
		smartOut, _ := smartExec(30*time.Second, "smartctl", "-j", "-a", devPath)
		if len(smartOut) == 0 {
			// No output at all — smartctl couldn't access the device
			// (permission denied, device unsupported, etc.).
			info.SmartAvailable = false
			results = append(results, info)
			continue
		}

		var sj smartctlJSON
		if err := json.Unmarshal(smartOut, &sj); err != nil {
			info.SmartAvailable = false
			results = append(results, info)
			continue
		}

		info.SmartAvailable = true

		if sj.SmartStatus != nil {
			info.SmartHealthy = sj.SmartStatus.Passed
		}

		if sj.Temperature != nil {
			temp := sj.Temperature.Current
			info.TemperatureC = &temp
		}

		if sj.PowerOnTime != nil {
			info.PowerOnHours = sj.PowerOnTime.Hours
		}

		// Extract individual attributes.
		if sj.AtaSmartAttributes != nil {
			for _, attr := range sj.AtaSmartAttributes.Table {
				resp := SmartAttributeResponse{
					ID:     attr.ID,
					Name:   attr.Name,
					Value:  attr.Value,
					Worst:  attr.Worst,
					Thresh: attr.Thresh,
					Raw:    attr.Raw.Value,
				}
				info.Attributes = append(info.Attributes, resp)

				// Pull out well-known attributes by ID.
				switch attr.ID {
				case 5: // Reallocated_Sector_Ct
					info.ReallocatedSectors = attr.Raw.Value
				case 197: // Current_Pending_Sector
					info.PendingSectors = attr.Raw.Value
				}
			}
		}

		results = append(results, info)
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    results,
	})
}

// POST /v1/sys/smart/test — Start a SMART self-test on a device.
func PostSmartTest(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("smartctl"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "smartctl is not installed",
		})
	}

	var body struct {
		Device string `json:"device"`
		Type   string `json:"type"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	// Validate device path — must match /dev/[alphanumeric] only.
	if !validSmartDevice.MatchString(body.Device) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid device path: must match /dev/[a-zA-Z0-9]+",
		})
	}

	// Validate test type.
	testType := body.Type
	if testType != "short" && testType != "long" {
		testType = "short"
	}

	// Run smartctl -t <type> <device>. This returns immediately; the
	// drive performs the self-test in the background.
	out, err := smartExecCombined(30*time.Second, "smartctl", "-t", testType, body.Device)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: fmt.Sprintf("smartctl exited with error (test may still have started): %s", strings.TrimSpace(string(out))),
			Data:    map[string]string{"status": "error", "output": string(out)},
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Test started",
		Data:    map[string]string{"status": "started", "type": testType, "device": body.Device},
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- SMART Test Scheduling ----

const smartSchedulePath = "/var/lib/casanova/smart_schedule.json"

// validSmartDevice matches block device paths like /dev/sda, /dev/nvme0n1.
var validSmartDevice = regexp.MustCompile(`^/dev/[a-zA-Z0-9]+$`)

var smartScheduleMu sync.Mutex

type SmartSchedule struct {
	Device    string `json:"device"`
	TestType  string `json:"test_type"`
	Interval  string `json:"interval"`
	DayOfWeek int    `json:"day_of_week"`
	Hour      int    `json:"hour"`
	Enabled   bool   `json:"enabled"`
	LastRun   string `json:"last_run"`
	NextRun   string `json:"next_run"`
}

type SmartScheduleFile struct {
	Schedules []SmartSchedule `json:"schedules"`
}

func loadSchedules() (SmartScheduleFile, error) {
	var sf SmartScheduleFile
	data, err := os.ReadFile(smartSchedulePath)
	if err != nil {
		if os.IsNotExist(err) {
			return SmartScheduleFile{Schedules: []SmartSchedule{}}, nil
		}
		return sf, err
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		return sf, err
	}
	if sf.Schedules == nil {
		sf.Schedules = []SmartSchedule{}
	}
	return sf, nil
}

func saveSchedules(sf SmartScheduleFile) error {
	dir := filepath.Dir(smartSchedulePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	// Wave 35-R: was write-tmp + fsync(file) + rename; add the parent-
	// dir fsync that commonfile.WriteFileAtomic gives us so a power
	// loss between rename and metadata commit cannot wipe the SMART
	// schedule set. Same rationale as alerts.go / cloud_backup.go.
	return commonfile.WriteFileAtomic(smartSchedulePath, data, 0o644)
}

// calcNextRun computes the next run time from now, based on interval,
// day_of_week (Sunday=0, only used for weekly), and hour (0-23).
func calcNextRun(interval string, dayOfWeek, hour int) time.Time {
	now := time.Now()
	loc := now.Location()

	switch interval {
	case "daily":
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next

	case "weekly":
		// Find the next occurrence of dayOfWeek at the given hour.
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
		daysUntil := (dayOfWeek - int(next.Weekday()) + 7) % 7
		if daysUntil == 0 && !next.After(now) {
			daysUntil = 7
		}
		next = next.AddDate(0, 0, daysUntil)
		return next

	case "monthly":
		next := time.Date(now.Year(), now.Month(), 1, hour, 0, 0, 0, loc)
		if !next.After(now) {
			next = next.AddDate(0, 1, 0)
		}
		return next

	default:
		// Fallback to daily.
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next
	}
}

// GET /v1/sys/smart/schedule — List all SMART test schedules.
func GetSmartSchedule(ctx echo.Context) error {
	// Gated: names the drives it is scheduled against.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}

	smartScheduleMu.Lock()
	defer smartScheduleMu.Unlock()

	sf, err := loadSchedules()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load schedules: " + err.Error(),
		})
	}

	// Recalculate next_run for all enabled schedules.
	for i := range sf.Schedules {
		if sf.Schedules[i].Enabled {
			sf.Schedules[i].NextRun = calcNextRun(
				sf.Schedules[i].Interval,
				sf.Schedules[i].DayOfWeek,
				sf.Schedules[i].Hour,
			).UTC().Format(time.RFC3339)
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    sf.Schedules,
	})
}

// POST /v1/sys/smart/schedule — Create or update a schedule.
func PostSmartSchedule(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Device    string `json:"device"`
		TestType  string `json:"test_type"`
		Interval  string `json:"interval"`
		DayOfWeek int    `json:"day_of_week"`
		Hour      int    `json:"hour"`
		Enabled   bool   `json:"enabled"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	// Validate device path.
	if !strings.HasPrefix(body.Device, "/dev/") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "device must start with /dev/",
		})
	}
	if strings.Contains(body.Device, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "path traversal not allowed",
		})
	}

	// Validate test type.
	if body.TestType != "short" && body.TestType != "long" {
		body.TestType = "short"
	}

	// Validate interval.
	if body.Interval != "daily" && body.Interval != "weekly" && body.Interval != "monthly" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "interval must be daily, weekly, or monthly",
		})
	}

	// Validate day_of_week and hour.
	if body.DayOfWeek < 0 || body.DayOfWeek > 6 {
		body.DayOfWeek = 0
	}
	if body.Hour < 0 || body.Hour > 23 {
		body.Hour = 3
	}

	smartScheduleMu.Lock()
	defer smartScheduleMu.Unlock()

	sf, err := loadSchedules()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load schedules: " + err.Error(),
		})
	}

	nextRun := ""
	if body.Enabled {
		nextRun = calcNextRun(body.Interval, body.DayOfWeek, body.Hour).UTC().Format(time.RFC3339)
	}

	schedule := SmartSchedule{
		Device:    body.Device,
		TestType:  body.TestType,
		Interval:  body.Interval,
		DayOfWeek: body.DayOfWeek,
		Hour:      body.Hour,
		Enabled:   body.Enabled,
		NextRun:   nextRun,
	}

	// Update existing or append new.
	found := false
	for i, s := range sf.Schedules {
		if s.Device == body.Device {
			schedule.LastRun = s.LastRun // preserve last_run
			sf.Schedules[i] = schedule
			found = true
			break
		}
	}
	if !found {
		sf.Schedules = append(sf.Schedules, schedule)
	}

	if err := saveSchedules(sf); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save schedule: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    schedule,
	})
}

// DELETE /v1/sys/smart/schedule — Remove a schedule.
// Device passed as query param: ?device=/dev/sda
func DeleteSmartSchedule(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	device := ctx.QueryParam("device")
	if device == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "device query parameter is required",
		})
	}

	smartScheduleMu.Lock()
	defer smartScheduleMu.Unlock()

	sf, err := loadSchedules()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load schedules: " + err.Error(),
		})
	}

	found := false
	filtered := make([]SmartSchedule, 0, len(sf.Schedules))
	for _, s := range sf.Schedules {
		if s.Device == device {
			found = true
			continue
		}
		filtered = append(filtered, s)
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: fmt.Sprintf("no schedule found for device %s", device),
		})
	}

	sf.Schedules = filtered
	if err := saveSchedules(sf); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save schedules: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// ---- SMART Test Results ----

type smartSelfTestLog struct {
	AtaSmartSelfTestLog *struct {
		Standard *struct {
			Table []smartTestEntry `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
}

type smartTestEntry struct {
	Type *struct {
		String string `json:"string"`
	} `json:"type"`
	Status *struct {
		String    string `json:"string"`
		Passed    bool   `json:"passed"`
		Remaining struct {
			Percent int `json:"percent"`
		} `json:"remaining"`
	} `json:"status"`
	LifetimeHours int `json:"lifetime_hours"`
}

type SmartTestResult struct {
	Type          string `json:"type"`
	Status        string `json:"status"`
	Passed        bool   `json:"passed"`
	Remaining     string `json:"remaining"`
	LifetimeHours int    `json:"lifetime_hours"`
}

type SmartDeviceResults struct {
	Device string            `json:"device"`
	Tests  []SmartTestResult `json:"tests"`
}

// GET /v1/sys/smart/results — Get last self-test results for all disks.
func GetSmartResults(ctx echo.Context) error {
	// Gated: per-drive SMART attributes; same inventory, in detail.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}

	if _, err := exec.LookPath("smartctl"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "smartctl not found",
			Data:    []SmartDeviceResults{},
		})
	}

	// List block devices.
	lsblkOut, err := smartExec(10*time.Second, "lsblk", "-J", "-o", "NAME,TYPE")
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to list block devices: " + err.Error(),
			Data:    []SmartDeviceResults{},
		})
	}

	var lsblk lsblkOutput
	if err := json.Unmarshal(lsblkOut, &lsblk); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to parse lsblk output",
			Data:    []SmartDeviceResults{},
		})
	}

	var results []SmartDeviceResults
	for _, dev := range lsblk.Blockdevices {
		if dev.Type != "disk" {
			continue
		}

		devPath := "/dev/" + dev.Name
		dr := SmartDeviceResults{
			Device: devPath,
			Tests:  []SmartTestResult{},
		}

		smartOut, _ := smartExec(30*time.Second, "smartctl", "-j", "-l", "selftest", devPath)
		if len(smartOut) == 0 {
			results = append(results, dr)
			continue
		}

		var stl smartSelfTestLog
		if err := json.Unmarshal(smartOut, &stl); err != nil {
			results = append(results, dr)
			continue
		}

		if stl.AtaSmartSelfTestLog != nil && stl.AtaSmartSelfTestLog.Standard != nil {
			for _, entry := range stl.AtaSmartSelfTestLog.Standard.Table {
				tr := SmartTestResult{
					LifetimeHours: entry.LifetimeHours,
				}
				if entry.Type != nil {
					tr.Type = entry.Type.String
				}
				if entry.Status != nil {
					tr.Status = entry.Status.String
					tr.Passed = entry.Status.Passed
					tr.Remaining = fmt.Sprintf("%d%%", entry.Status.Remaining.Percent)
				}
				dr.Tests = append(dr.Tests, tr)
			}
		}

		results = append(results, dr)
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    results,
	})
}
