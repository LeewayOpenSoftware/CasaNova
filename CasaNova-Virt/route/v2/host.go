package v2

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// GET /host — full hypervisor host info.
func (h *VirtHandler) GetHost(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	info, err := h.svc.Host().Info()
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, info)
}

// GET /info — dashboard summary (currently the same host payload the oracle's
// /info endpoint returned).
func (h *VirtHandler) GetInfo(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	info, err := h.svc.Host().Info()
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, info)
}
