package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

func adminCtx(e *echo.Echo, req *http.Request, rec *httptest.ResponseRecorder) echo.Context {
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)
	return ctx
}

// Lo que se puede CREAR y lo que se puede GESTIONAR tienen que ser el mismo
// conjunto. Un id libre se aceptaba al crear y se rechazaba al borrar: la tarea
// corria como root para siempre y no habia forma de quitarla por API.
func TestPutScheduledTaskRefusesAnIDItCouldNotManageLater(t *testing.T) {
	e := echo.New()
	body := `{"id":"backup_diario","name":"Backup","cron":"0 2 * * *","command":"echo hola","target":{"host":true}}`
	req := httptest.NewRequest(http.MethodPut, "/v2/job_management/tasks", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := PutScheduledTask(adminCtx(e, req, rec)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("un id libre fue ACEPTADO al crear (status=%d): quedaria sin poder borrarse. body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task_") {
		t.Fatalf("el 400 no dice que forma se espera: %s", rec.Body.String())
	}
}

// Contraprueba anti-vacuidad: un id BIEN formado tiene que pasar la puerta.
// Sin scheduler detras contesta 503, y ese 503 es la prueba de que el rechazo
// de arriba vino de la validacion del id y no de que el handler rechace todo.
func TestPutScheduledTaskLetsAWellFormedIDThrough(t *testing.T) {
	e := echo.New()
	body := `{"id":"task_deadbeef","name":"Backup","cron":"0 2 * * *","command":"echo hola","target":{"host":true}}`
	req := httptest.NewRequest(http.MethodPut, "/v2/job_management/tasks", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := PutScheduledTask(adminCtx(e, req, rec)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("un id bien formado fue rechazado por la validacion de id: %s", rec.Body.String())
	}
}

// Las tareas que YA existan con id libre tienen que poder gestionarse, o
// cerrar la puerta de creacion las dejaria inmanejables para siempre.
func TestValidateTaskIDCanAddressATaskThatAlreadyExistsWithAFreeFormID(t *testing.T) {
	e := echo.New()
	for _, id := range []string{"backup_diario", "task_deadbeef", "mi.tarea-2"} {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		ctx := e.NewContext(req, httptest.NewRecorder())
		ctx.SetParamNames("id")
		ctx.SetParamValues(id)
		if got, err := validateTaskID(ctx); err != nil || got != id {
			t.Fatalf("validateTaskID(%q) = %q, %v: una tarea existente seguiria sin poder borrarse", id, got, err)
		}
	}
	for _, bad := range []string{"", "con\x00nulo", "salto\nlinea", strings.Repeat("x", 129)} {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		ctx := e.NewContext(req, httptest.NewRecorder())
		ctx.SetParamNames("id")
		ctx.SetParamValues(bad)
		if _, err := validateTaskID(ctx); err == nil {
			t.Fatalf("validateTaskID acepto un id imposible: %q", bad)
		}
	}
}
