// Package v2 holds the driver-service HTTP handlers. They are framework-level
// thin: parse/validate, call service.MyService, wrap in the shared model.Result
// envelope used across CasaNova services.
package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/service"
	"github.com/labstack/echo/v4"
)

type DriverAPI struct{}

func NewDriverAPI() *DriverAPI { return &DriverAPI{} }

func ok(ctx echo.Context, data interface{}) error {
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    data,
	})
}

func fail(ctx echo.Context, code int, err error) error {
	return ctx.JSON(code, model.Result{
		Success: code,
		Message: err.Error(),
	})
}

// GET /devices/pci
func (a *DriverAPI) ListPCI(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	devices, err := service.MyService.Device().ListPCI()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, devices)
}

// GET /devices/usb
func (a *DriverAPI) ListUSB(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	devices, err := service.MyService.Device().ListUSB()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, devices)
}

// GET /modules
func (a *DriverAPI) ListModules(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	mods, err := service.MyService.Module().List()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, mods)
}

// POST /modules/:name/load
func (a *DriverAPI) LoadModule(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	name := ctx.Param("name")
	if err := service.MyService.Module().Load(name); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, map[string]string{"name": name, "state": "loaded"})
}

// POST /modules/:name/unload  (also wired to DELETE /modules/:name)
func (a *DriverAPI) UnloadModule(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	name := ctx.Param("name")
	if err := service.MyService.Module().Unload(name); err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, map[string]string{"name": name, "state": "unloaded"})
}

// GET /gpu
func (a *DriverAPI) ListGPU(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	gpus, err := service.MyService.GPU().List()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, gpus)
}

// POST /gpu/select?load=true|false
func (a *DriverAPI) SelectGPU(ctx echo.Context) error {
	if err := requireConfig(ctx); err != nil {
		return err
	}
	load := ctx.QueryParam("load") != "false" // default: load modules
	out, err := service.MyService.GPU().Select(load)
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, map[string]string{"output": out})
}

// GET /drivers  (installed out-of-tree driver packages)
func (a *DriverAPI) ListDrivers(ctx echo.Context) error {
	if err := requireRead(ctx); err != nil {
		return err
	}
	drivers, err := service.MyService.Driver().ListInstalled()
	if err != nil {
		return fail(ctx, common_err.SERVICE_ERROR, err)
	}
	return ok(ctx, drivers)
}

// RegisterUnder wires every handler under the given base path (e.g. /v2/driver
// or /v1/driver) so v1 and v2 share one implementation.
func (a *DriverAPI) RegisterUnder(g *echo.Group) {
	g.GET("/devices/pci", a.ListPCI)
	g.GET("/devices/usb", a.ListUSB)
	g.GET("/modules", a.ListModules)
	g.POST("/modules/:name/load", a.LoadModule)
	g.POST("/modules/:name/unload", a.UnloadModule)
	g.DELETE("/modules/:name", a.UnloadModule)
	g.GET("/gpu", a.ListGPU)
	g.POST("/gpu/select", a.SelectGPU)
	g.GET("/drivers", a.ListDrivers)
	// Kernel tunables (governor, swappiness). Reads → requireRead,
	// mutations → requireConfig; validated against sysfs in the service.
	g.GET("/tuning/cpu-governor", a.GetCPUGovernor)
	g.PUT("/tuning/cpu-governor", a.SetCPUGovernor)
	g.PUT("/tuning/epp", a.SetEPP)
	g.GET("/tuning/swappiness", a.GetSwappiness)
	g.PUT("/tuning/swappiness", a.SetSwappiness)
	g.GET("/tuning/io-scheduler", a.GetIOSchedulers)
	g.PUT("/tuning/io-scheduler", a.SetIOScheduler)
	g.PUT("/tuning/spindown", a.SetSpindown)
	g.POST("/tuning/spindown/standby", a.StandbyDisk)
	g.GET("/tuning/sysctl", a.GetSysctls)
	g.PUT("/tuning/sysctl", a.SetSysctl)
	g.GET("/tuning/zram", a.GetZram)
	g.PUT("/tuning/zram", a.SetZram)
	g.GET("/sensors", a.GetSensors)
}
