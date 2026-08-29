// Package v2 holds the HTTP handlers for the virtualization API. The route
// shapes mirror the ZimaOS-VirtManagement oracle (echo router, /domain,
// /pool, /host, /info groups); see ANALYSIS.md for the recovered route table.
package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/service"
	"github.com/labstack/echo/v4"
)

// VirtHandler carries the service dependencies for the handlers.
type VirtHandler struct {
	svc service.Services
}

func NewVirtHandler(svc service.Services) *VirtHandler {
	return &VirtHandler{svc: svc}
}

func ok(c echo.Context, data interface{}) error {
	return c.JSON(http.StatusOK, data)
}

func created(c echo.Context, data interface{}) error {
	return c.JSON(http.StatusCreated, data)
}

func fail(c echo.Context, code int, msg string) error {
	return c.JSON(code, model.BaseResponse{Message: msg})
}
