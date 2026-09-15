package v2

import (
	"fmt"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
	"gopkg.in/yaml.v3"
)

func (a *AppManagement) Convert(ctx echo.Context, params codegen.ConvertParams) error {
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

	fileType := codegen.Appfile
	if params.Type != nil {
		fileType = *params.Type
	}

	switch fileType {

	case codegen.Appfile:
		var legacyFile model.CustomizationPostData
		if err := ctx.Bind(&legacyFile); err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		compose := legacyFile.Compose()

		yaml, err := yaml.Marshal(compose)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		return ctx.String(http.StatusOK, string(yaml))

	default:
		message := fmt.Sprintf("unsupported file type: %s", fileType)
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
}
