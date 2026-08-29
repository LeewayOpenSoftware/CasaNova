package v2

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/service"
	"github.com/labstack/echo/v4"
)

// GET /tuning/cpu-governor — current + available CPU frequency governors.
func (a *DriverAPI) GetCPUGovernor(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	info, err := service.MyService.Tuning().CPUGovernor()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, info)
}

// PUT /tuning/cpu-governor  body: {"governor":"ondemand"}
func (a *DriverAPI) SetCPUGovernor(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Governor string `json:"governor"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	// ?persist=false → transient apply (power-saving ladder): the value is
	// NOT recorded as the user's boot-reapplied preference.
	if err := service.MyService.Tuning().SetCPUGovernor(body.Governor, ctx.QueryParam("persist") != "false"); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	info, _ := service.MyService.Tuning().CPUGovernor()
	return ok(ctx, info)
}

// GET /tuning/swappiness — vm.swappiness + read-only swap context.
func (a *DriverAPI) GetSwappiness(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	info, err := service.MyService.Tuning().Swappiness()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, info)
}

// PUT /tuning/swappiness  body: {"value":10}
func (a *DriverAPI) SetSwappiness(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Value int `json:"value"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if err := service.MyService.Tuning().SetSwappiness(body.Value); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	info, _ := service.MyService.Tuning().Swappiness()
	return ok(ctx, info)
}

// PUT /tuning/epp  body: {"epp":"balance_performance"}
func (a *DriverAPI) SetEPP(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		EPP string `json:"epp"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if err := service.MyService.Tuning().SetEPP(body.EPP, ctx.QueryParam("persist") != "false"); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	info, _ := service.MyService.Tuning().CPUGovernor()
	return ok(ctx, info)
}

// GET /tuning/io-scheduler — per-disk I/O scheduler + read-ahead.
func (a *DriverAPI) GetIOSchedulers(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	disks, err := service.MyService.Tuning().IOSchedulers()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, disks)
}

// PUT /tuning/io-scheduler  body: {"disk":"sda","scheduler":"bfq","readAheadKiB":256}
func (a *DriverAPI) SetIOScheduler(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Disk         string `json:"disk"`
		Scheduler    string `json:"scheduler"`
		ReadAheadKiB *int   `json:"readAheadKiB"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if body.Scheduler != "" {
		if err := service.MyService.Tuning().SetIOScheduler(body.Disk, body.Scheduler); err != nil {
			return fail(ctx, common_err.SERVICE_ERROR, err)
		}
	}
	if body.ReadAheadKiB != nil {
		if err := service.MyService.Tuning().SetReadAhead(body.Disk, *body.ReadAheadKiB); err != nil {
			return fail(ctx, common_err.SERVICE_ERROR, err)
		}
	}
	disks, _ := service.MyService.Tuning().IOSchedulers()
	return ok(ctx, disks)
}

// PUT /tuning/spindown  body: {"disk":"sda","timeout":"30m"}
func (a *DriverAPI) SetSpindown(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Disk    string `json:"disk"`
		Timeout string `json:"timeout"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if err := service.MyService.Tuning().SetSpindown(body.Disk, body.Timeout); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	disks, _ := service.MyService.Tuning().IOSchedulers()
	return ok(ctx, disks)
}

// POST /tuning/spindown/standby  body: {"disk":"sda"} — immediate standby
// (hdparm -y). Transient: no persistence, the next access wakes the disk.
func (a *DriverAPI) StandbyDisk(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Disk string `json:"disk"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if err := service.MyService.Tuning().StandbyNow(body.Disk); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, map[string]string{"disk": body.Disk, "state": "standby"})
}

// GET /tuning/sysctl — the compiled sysctl allowlist + current values.
func (a *DriverAPI) GetSysctls(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	entries, err := service.MyService.Tuning().Sysctls()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, entries)
}

// PUT /tuning/sysctl  body: {"key":"vfs_cache_pressure","value":"50"}
func (a *DriverAPI) SetSysctl(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	if err := service.MyService.Tuning().SetSysctl(body.Key, body.Value); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	entries, _ := service.MyService.Tuning().Sysctls()
	return ok(ctx, entries)
}

// GET /tuning/zram — zram swap status.
func (a *DriverAPI) GetZram(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	info, err := service.MyService.Tuning().Zram()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, info)
}

// PUT /tuning/zram  body: {"enable":true,"algorithm":"zstd","sizeMiB":4096}
func (a *DriverAPI) SetZram(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	var body struct {
		Enable    bool   `json:"enable"`
		Algorithm string `json:"algorithm"`
		SizeMiB   int    `json:"sizeMiB"`
	}
	if err := ctx.Bind(&body); err != nil {
		return fail(ctx, common_err.CLIENT_ERROR, err)
	}
	info, err := service.MyService.Tuning().SetZram(body.Enable, body.Algorithm, body.SizeMiB)
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, info)
}

// GET /sensors — hwmon temperature/fan report (read-only).
func (a *DriverAPI) GetSensors(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	report, err := service.MyService.Sensors().Read()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, report)
}
