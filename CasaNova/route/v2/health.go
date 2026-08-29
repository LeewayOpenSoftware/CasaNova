package v2

import (
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/LeewayOpenSoftware/CasaNova/codegen"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
	"github.com/mholt/archiver/v3"
)

// requireHealthPerm gates the v2 health handlers.
//
// The G-03 fix closed the v1 log tail and the system profile, and left these
// three — which are strictly LARGER. GetHealthlogs streams a ZIP of everything
// under /var/log/casaos, every file and every rotation; GetHealthPorts returns
// the complete TCP and UDP listening inventory; GetHealthServices the running
// service inventory. All three were reachable by a tokenless caller, and the
// gateway proxies the internet over loopback, so "reachable" meant reachable
// from the internet.
//
// Identity first, then permission: a request that carries no credential
// resolves to guest, and guest holding a read permission would otherwise be
// enough. The log ZIP asks for PermSystemConfig because a log archive is a
// configuration-grade disclosure; the two inventories ask for PermSystemRead.
func requireHealthPerm(ctx echo.Context, p rbac.Permission) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, map[string]string{
			"message": "authentication required",
		})
	}
	if !rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return echo.NewHTTPError(http.StatusForbidden, map[string]string{
			"message": "permission denied",
		})
	}
	return nil
}

func (s *CasaOS) GetHealthServices(ctx echo.Context) error {
	if err := requireHealthPerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	services, err := service.MyService.Health().Services()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	return ctx.JSON(http.StatusOK, codegen.GetHealthServicesOK{
		Data: &codegen.HealthServices{
			Running:    services[true],
			NotRunning: services[false],
		},
	})
}

func (s *CasaOS) GetHealthPorts(ctx echo.Context) error {
	if err := requireHealthPerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	tcpPorts, udpPorts, err := service.MyService.Health().Ports()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	return ctx.JSON(http.StatusOK, codegen.GetHealthPortsOK{
		Data: &codegen.HealthPorts{
			TCP: &tcpPorts,
			UDP: &udpPorts,
		},
	})
}
func (c *CasaOS) GetHealthlogs(ctx echo.Context) error {
	if err := requireHealthPerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var name, currentPath, commonDir, extension string
	var err error
	var ar archiver.Writer
	fileList, err := os.ReadDir("/var/log/casaos")
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}
	extension, ar, err = file.GetCompressionAlgorithm("zip")
	if err != nil {
		ctx.Response().Header().Set("Content-Type", "application/json")
		message := err.Error()
		return ctx.JSON(http.StatusNotFound, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}
	err = ar.Create(ctx.Response().Writer)
	if err != nil {
		ctx.Response().Header().Set("Content-Type", "application/json")
		message := err.Error()
		return ctx.JSON(http.StatusNotFound, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}
	defer ar.Close()

	commonDir = "/var/log/casaos"

	currentPath = filepath.Base(commonDir)

	name = currentPath
	name += extension
	ctx.Response().Header().Add("Content-Type", "application/octet-stream")
	ctx.Response().Header().Add("Content-Transfer-Encoding", "binary")
	ctx.Response().Header().Add("Cache-Control", "no-cache")
	ctx.Response().Header().Add("Content-Disposition", "attachment; filename*=utf-8''"+url.PathEscape(name))

	for _, fname := range fileList {
		err := file.AddFile(ar, filepath.Join("/var/log/casaos", fname.Name()), commonDir)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

	}
	return nil
}
