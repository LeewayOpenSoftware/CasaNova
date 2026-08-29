package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/labstack/echo/v4"
)

// GET /domain — list all domains.
func (h *VirtHandler) ListDomains(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	domains, err := h.svc.Domain().List()
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, domains)
}

// POST /domain — define a new domain from a VirtualMachineConfiguration.
func (h *VirtHandler) CreateDomain(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	var cfg model.VirtualMachineConfiguration
	if err := c.Bind(&cfg); err != nil {
		return fail(c, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	if cfg.Name == "" {
		return fail(c, http.StatusBadRequest, "name is required")
	}
	d, err := h.svc.Domain().Define(&cfg)
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return created(c, d)
}

// GET /domain/:name — full detail for one domain.
func (h *VirtHandler) GetDomain(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	d, err := h.svc.Domain().Get(c.Param("name"))
	if err != nil {
		return fail(c, http.StatusNotFound, err.Error())
	}
	return ok(c, d)
}

// DELETE /domain/:name — undefine (and stop if running).
func (h *VirtHandler) DeleteDomain(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	if err := h.svc.Domain().Undefine(c.Param("name")); err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "domain undefined"})
}

// GET /domain/:name/status — current libvirt state.
func (h *VirtHandler) GetDomainStatus(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	st, err := h.svc.Domain().Status(c.Param("name"))
	if err != nil {
		return fail(c, http.StatusNotFound, err.Error())
	}
	return ok(c, st)
}

type actionRequest struct {
	Action model.DomainAction `json:"action"`
}

// PUT /domain/:name/status — apply a lifecycle action
// (start|shutdown|reboot|stop|suspend|resume).
func (h *VirtHandler) SetDomainStatus(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	var req actionRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	if err := h.svc.Domain().Action(c.Param("name"), req.Action); err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "action applied: " + string(req.Action)})
}

type autostartRequest struct {
	Enabled bool `json:"enabled"`
}

// POST /domain/:name/autostart — toggle start-on-boot.
func (h *VirtHandler) SetAutostart(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	var req autostartRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	if err := h.svc.Domain().SetAutostart(c.Param("name"), req.Enabled); err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "autostart updated"})
}

// GET /domain/:name/vnc — remote-console (VNC/SPICE) coordinates.
func (h *VirtHandler) GetConsole(c echo.Context) error {
	// Console access = full control of the guest → admin-gated.
	if err := requireConfig(c); err != nil {
		return err
	}
	info, err := h.svc.Domain().Console(c.Param("name"))
	if err != nil {
		return fail(c, http.StatusNotFound, err.Error())
	}
	return ok(c, info)
}
