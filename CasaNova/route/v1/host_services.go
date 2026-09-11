package v1

import (
	"net/http"
	"os/exec"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// Curated host-service panel. The client sends a KEY from this compiled map,
// never a systemd unit name — so `systemctl restart/enable/disable` only ever
// receives a unit WE chose. ssh has canToggle=false (restart only) so the
// panel can never disable the operator's own way back in. The casanova-*/
// gateway units are deliberately absent (the Troubleshooter owns those).
type curatedService struct {
	Unit      string
	Label     string
	CanToggle bool   // enable/disable allowed (false => restart-only)
	Warn      string // shown before a restart, e.g. "cuts active shares"
}

var curatedServices = map[string]curatedService{
	"samba":     {Unit: "smbd", Label: "Samba (SMB shares)", CanToggle: true, Warn: "restart cuts active SMB sessions"},
	"samba-nmb": {Unit: "nmbd", Label: "Samba NetBIOS", CanToggle: true},
	"nfs":       {Unit: "nfs-kernel-server", Label: "NFS server", CanToggle: true, Warn: "restart cuts active NFS mounts"},
	"docker":    {Unit: "docker", Label: "Docker engine", CanToggle: false, Warn: "restart bounces all containers"},
	"cron":      {Unit: "cron", Label: "Cron scheduler", CanToggle: true},
	"avahi":     {Unit: "avahi-daemon", Label: "Avahi (mDNS discovery)", CanToggle: true},
	"ssh":       {Unit: "ssh", Label: "SSH server", CanToggle: false, Warn: "restart-only by design (cannot be disabled from here)"},
}

// order for stable display
var curatedOrder = []string{"docker", "samba", "samba-nmb", "nfs", "ssh", "cron", "avahi"}

type HostServiceStatus struct {
	Key       string `json:"key"`
	Unit      string `json:"unit"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Active    bool   `json:"active"`
	Enabled   bool   `json:"enabled"`
	CanToggle bool   `json:"canToggle"`
	Warn      string `json:"warn,omitempty"`
}

// GET /v1/sys/host/services
func GetHostServices(ctx echo.Context) error {
	// requireSystemPrincipal: see route/v1/antivirus.go's GetAntivirusStatus
	// comment — guest holds PermSystemRead, so a bare requirePerm never
	// refused a tokenless caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	out := make([]HostServiceStatus, 0, len(curatedOrder))
	for _, key := range curatedOrder {
		svc := curatedServices[key]
		unit := svc.Unit + ".service"
		st := HostServiceStatus{Key: key, Unit: svc.Unit, Label: svc.Label, CanToggle: svc.CanToggle, Warn: svc.Warn}
		// `is-active` returns "active"; `is-enabled` returns "enabled". A unit
		// that doesn't exist reports "inactive"/nonzero — treat as not installed.
		load := showUnitProp(unit, "LoadState")
		st.Installed = load == "loaded"
		st.Active = showUnitProp(unit, "ActiveState") == "active"
		enabled := showUnitProp(unit, "UnitFileState")
		st.Enabled = enabled == "enabled" || enabled == "enabled-runtime" || enabled == "static"
		out = append(out, st)
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    out,
	})
}

// POST /v1/sys/host/services/:key/:action   action ∈ restart|enable|disable
func PostHostServiceAction(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	key := ctx.Param("key")
	action := ctx.Param("action")

	svc, ok := curatedServices[key]
	if !ok {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.SERVICE_ERROR, Message: "unknown service"})
	}
	// Build the argv from the MAP, never from the raw params. enable/disable
	// are gated by CanToggle so ssh can't be turned off from the panel.
	var args []string
	switch action {
	case "restart":
		args = []string{"restart", svc.Unit}
	case "enable":
		if !svc.CanToggle {
			return ctx.JSON(http.StatusForbidden, model.Result{Success: common_err.SERVICE_ERROR, Message: "this service cannot be toggled"})
		}
		args = []string{"enable", "--now", svc.Unit}
	case "disable":
		if !svc.CanToggle {
			return ctx.JSON(http.StatusForbidden, model.Result{Success: common_err.SERVICE_ERROR, Message: "this service cannot be toggled"})
		}
		args = []string{"disable", "--now", svc.Unit}
	default:
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: "invalid action"})
	}

	cmd := exec.Command("systemctl", args...)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "systemctl " + action + " failed: " + err.Error()})
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return ctx.JSON(http.StatusGatewayTimeout, model.Result{Success: common_err.SERVICE_ERROR, Message: "systemctl timed out"})
	}
	return GetHostServices(ctx)
}
