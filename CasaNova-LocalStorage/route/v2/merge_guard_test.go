package v2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	"github.com/labstack/echo/v4"
)

// SEGURIDAD DEL TEST: se fuerza EnableMergerFS="true" para que la rama
// destructiva de InitMerge (RMDir de /var/lib/casaos/files + SafeRename del
// punto de montaje) quede DESACTIVADA por configuracion. Con la guarda puesta
// el handler contesta 400; sin ella cae al `else` y contesta 200. Rojo y verde
// distinguibles sin que exista ninguna ruta que borre o mueva nada.
func TestInitMergeRefusesAHostDirectory(t *testing.T) {
	prev := config.ServerInfo.EnableMergerFS
	config.ServerInfo.EnableMergerFS = "true"
	t.Cleanup(func() { config.ServerInfo.EnableMergerFS = prev })

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v2/local_storage/merge/init", strings.NewReader(`{"mount_point":"/etc"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)

	s := &LocalStorage{}
	if err := s.InitMerge(ctx); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("InitMerge acepto /etc (status=%d): con EnableMergerFS!=true habria movido /etc a /var/lib/casaos/files. body=%s", rec.Code, rec.Body.String())
	}
}

// Anti-vacuidad: el punto de merge legitimo tiene que pasar la guarda. Si este
// se pone rojo, el arreglo rompio la inicializacion del almacenamiento.
func TestInitMergeStillAcceptsTheRealMergePoint(t *testing.T) {
	prev := config.ServerInfo.EnableMergerFS
	config.ServerInfo.EnableMergerFS = "true"
	t.Cleanup(func() { config.ServerInfo.EnableMergerFS = prev })

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v2/local_storage/merge/init", strings.NewReader(`{"mount_point":"/DATA"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)

	s := &LocalStorage{}
	if err := s.InitMerge(ctx); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("InitMerge rechaza /DATA, que es EL punto de merge: %s", rec.Body.String())
	}
}
