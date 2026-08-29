package v1

import (
	"github.com/labstack/echo/v4"
	"gopkg.in/yaml.v3"
)

func YAML(ctx echo.Context, code int, i interface{}) error {
	// Set Content-Type BEFORE WriteHeader — headers sent after WriteHeader are ignored.
	ctx.Response().Header().Set(echo.HeaderContentType, "text/yaml")
	ctx.Response().WriteHeader(code)

	return yaml.NewEncoder(ctx.Response()).Encode(i)
}
