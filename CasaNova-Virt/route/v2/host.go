package v2

import (
	"errors"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
	"github.com/labstack/echo/v4"
)

// hostInfoStatus maps a host-info failure to a status: no libvirt socket on
// this machine is "service unavailable", not an internal error, and the
// message names the sockets that were tried.
func hostInfoStatus(err error) int {
	if errors.Is(err, libvirtconn.ErrNoSocket) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// GET /host — full hypervisor host info.
func (h *VirtHandler) GetHost(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	info, err := h.svc.Host().Info()
	if err != nil {
		return fail(c, hostInfoStatus(err), err.Error())
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
		return fail(c, hostInfoStatus(err), err.Error())
	}
	return ok(c, info)
}
