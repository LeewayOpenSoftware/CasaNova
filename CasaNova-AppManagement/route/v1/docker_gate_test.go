package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/labstack/echo/v4"
)

// ContainerUpdateInfo returns GetCustomizationPostData's full payload for any
// origin=="local" container, which includes every environment variable —
// database passwords and API keys among them. It was reachable with no
// permission check at all, right next to ToComposeYAML (same file), which
// already requires PermAppConfigure for the identical data. This asserts the
// deny path actually aborts before service.MyService is ever touched: package
// service's MyService is nil in this test binary, so if the gate did not
// abort first, the handler would panic instead of returning cleanly.
func TestContainerUpdateInfoDeniesNonAdminRoles(t *testing.T) {
	for _, role := range []string{"", "guest", "user"} {
		t.Run(role, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/container/abc", nil), rec)
			if role != "" {
				commonmw.SetRoleInContext(ctx, role, 7)
			}
			ctx.SetParamNames("id")
			ctx.SetParamValues("abc")

			err := ContainerUpdateInfo(ctx)
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
