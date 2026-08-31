package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/labstack/echo/v4"
)

// ListISOs — GET /iso — the installer/boot images available to attach to a VM.
func (h *VirtHandler) ListISOs(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	isos, err := h.svc.Domain().ListISOs()
	if err != nil {
		return fail(c, http.StatusInternalServerError, err.Error())
	}
	return ok(c, isos)
}

type isoDownloadRequest struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

// DownloadISO — POST /iso/download — fetch an ISO from a URL into the image
// store in the background (the VM create wizard's "add an ISO by URL"). Admin.
func (h *VirtHandler) DownloadISO(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	var req isoDownloadRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	if req.URL == "" {
		return fail(c, http.StatusBadRequest, "url is required")
	}
	if err := h.svc.Domain().DownloadISO(req.URL, req.Name); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "download started"})
}

// DELETE /iso/:name — remove an installation image.
func (h *VirtHandler) DeleteISO(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	if err := h.svc.Domain().DeleteISO(c.Param("name")); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "iso deleted"})
}

// GET /iso/downloads — transfers in flight, plus the ones that finished or
// failed recently. A read, so it is gated as one: this is what the sidebar
// polls, and requiring configure rights would blank the panel for a viewer
// who can already see the resulting ISOs.
func (h *VirtHandler) ListISODownloads(c echo.Context) error {
	if err := requireRead(c); err != nil {
		return err
	}
	return ok(c, h.svc.Domain().ISODownloads())
}

// DELETE /iso/downloads/:name — stop a transfer. The partial file is removed
// by the transfer's own cleanup, which runs on every exit path.
func (h *VirtHandler) CancelISODownload(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}
	if err := h.svc.Domain().CancelISODownload(c.Param("name")); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	return ok(c, model.BaseResponse{Message: "download cancelled"})
}
