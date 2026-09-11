package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// ComposeAppLogs returns up to 1000 lines of raw container stdout/stderr —
// startup banners, connection strings, and any secret the app itself printed
// routinely end up there. It had no permission check at all, unlike its
// sibling MyComposeApp (compose_app_gate_test.go), which already requires
// PermAppConfigure for a materially similar secret-bearing payload (env
// vars). Credential-content rule: a read whose content can BE a credential
// belongs at the write bit, not the lowest tidy-looking one — so this is
// gated the same way as MyComposeApp, not PermAppView/PermAppStart.
//
// service.MyService is nil in this test binary, so a handler that reached
// past its gate would panic instead of returning cleanly.
func TestComposeAppLogsDeniesNonAdminRoles(t *testing.T) {
	a := &AppManagement{}
	for _, role := range []string{"", "guest", "user"} {
		t.Run(role, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/compose/myapp/logs", nil), rec)
			if role != "" {
				commonmw.SetRoleInContext(ctx, role, 7)
			}

			err := a.ComposeAppLogs(ctx, "myapp", codegen.ComposeAppLogsParams{})
			if err == nil {
				t.Fatal("expected a refusal, got nil (service.MyService would have panicked if reached)")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("want *echo.HTTPError, got %T (%v)", err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403", role, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("the gate wrote the response itself: %q", rec.Body.String())
			}
		})
	}
}

// admin (the only role holding PermAppConfigure) must still pass the gate.
func TestComposeAppLogsAllowsPermAppConfigure(t *testing.T) {
	e := echo.New()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/compose/myapp/logs", nil), httptest.NewRecorder())
	commonmw.SetRoleInContext(ctx, "admin", 7)
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		t.Fatalf("admin (holds PermAppConfigure) was refused: %v", err)
	}
}
