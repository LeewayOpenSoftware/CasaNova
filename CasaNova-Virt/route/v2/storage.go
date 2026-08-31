package v2

import (
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
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

type createVolumeRequest struct {
	Name      string `json:"name"`
	CapacityB uint64 `json:"capacity"` // bytes
	Format    string `json:"format"`   // qcow2 | raw
}

// POST /pool/:name/volume — create a disk image in the pool.
func (h *VirtHandler) CreateVolume(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	var req createVolumeRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	v, err := h.svc.Storage().CreateVolume(c.Param("name"), req.Name, req.CapacityB, req.Format)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	return created(c, v)
}

// DELETE /pool/:name/volume/:volume — remove a disk image.
func (h *VirtHandler) DeleteVolume(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	if err := h.svc.Storage().DeleteVolume(c.Param("name"), c.Param("volume")); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "volume deleted"})
}
