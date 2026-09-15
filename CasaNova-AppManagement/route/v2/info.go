package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/labstack/echo/v4"
)

func (a *AppManagement) Info(ctx echo.Context) error {
	// Wave 34 (ported from 27.04, docs/design/per-app-grants.md §7): this
	// handler had NO authorization gate at all — not the wrong bit, none.
	// The router's JWT middleware still refused an anonymous caller, so it
	// was AUTHENTICATED but UNAUTHORIZED: every logged-in account reached
	// it, and so did any context that resolved to guest without a principal
	// behind it. PermAppView is the bit the whole family gets — guest
	// already holds it, so no role loses anything it has today.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	architecture, err := docker.CurrentArchitecture()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: utils.Ptr(err.Error()),
		})
	}

	return ctx.JSON(http.StatusOK, codegen.InfoOK{
		Architecture: utils.Ptr(architecture),
	})
}
