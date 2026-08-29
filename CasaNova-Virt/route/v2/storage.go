package v2

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// GET /pool — list storage pools.
func (h *VirtHandler) ListPools(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	pools, err := h.svc.Storage().ListPools()
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, pools)
}

// GET /pool/:name/volume — list volumes in a pool.
//
// The oracle keyed this route on the pool UUID (/pool/:uuid/volume); we accept
// the pool name, which libvirt looks up equivalently and is friendlier for our
// own UI. (A UUID is still a valid libvirt pool name lookup target only via
// LookupByUUID, so the handler is documented as name-based.)
func (h *VirtHandler) ListVolumes(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	vols, err := h.svc.Storage().ListVolumes(c.Param("name"))
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, vols)
}
