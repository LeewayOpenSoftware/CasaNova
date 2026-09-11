package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// WsContainerLogs streams a container's raw stdout/stderr, which routinely
// echoes secrets (startup banners, connection strings) — the handler's own
// comment already says so. It was gated on PermAppView, which guest holds:
// credential-content rule (a read whose content can BE a credential belongs
// at the write bit) means PermAppView was never the right bit here, the same
// reasoning that put ContainerUpdateInfo/ToComposeYAML on PermAppConfigure.
//
// The handler can't be driven all the way through a real Docker call or a
// real WebSocket upgrade in a unit test, so this uses a container id that
// fails validContainerID's regex — denied roles must be refused by the
// PERMISSION gate (403) before that check even runs; allowed roles reach the
// id-format check next and get a clean 400, never Docker or the socket
// upgrade. That divergence (403 vs. anything else) is what proves the gate
// ran first, without needing a live Docker daemon in this test binary.
func TestWsContainerLogsDeniesNonAdminRoles(t *testing.T) {
	for _, role := range []string{"", "guest", "user"} {
		t.Run(role, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/container/!!invalid!!/logs", nil), rec)
			if role != "" {
				commonmw.SetRoleInContext(ctx, role, 7)
			}
			ctx.SetParamNames("id")
			ctx.SetParamValues("!!invalid!!")

			err := WsContainerLogs(ctx)
			if err == nil {
				t.Fatalf("role %q: expected a refusal, got nil", role)
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("role %q: want *echo.HTTPError, got %T (%v)", role, err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403 (the permission gate must run before the container-id format check)", role, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("role %q: the gate wrote the response itself: %q", role, rec.Body.String())
			}
		})
	}
}

// admin (the only role holding PermAppConfigure) must still pass the gate.
func TestWsContainerLogsAllowsPermAppConfigure(t *testing.T) {
	e := echo.New()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/container/!!invalid!!/logs", nil), httptest.NewRecorder())
	commonmw.SetRoleInContext(ctx, "admin", 7)
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		t.Fatalf("admin (holds PermAppConfigure) was refused: %v", err)
	}
}
