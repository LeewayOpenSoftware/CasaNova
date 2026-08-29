// Package v1 exposes the driver API under the legacy /v1 prefix. It delegates to
// the canonical v2 handler set so there is a single implementation.
package v1

import (
	v2 "github.com/LeewayOpenSoftware/CasaNova-Driver/route/v2"
	"github.com/labstack/echo/v4"
)

type DriverAPI struct {
	inner *v2.DriverAPI
}

func NewDriverAPI() *DriverAPI {
	return &DriverAPI{inner: v2.NewDriverAPI()}
}

func (a *DriverAPI) RegisterUnder(g *echo.Group) {
	a.inner.RegisterUnder(g)
}
