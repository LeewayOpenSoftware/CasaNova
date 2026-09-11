package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// These are the entire peer exception, not a permission-default change. A
// peer gets past each selected GET's gate; a request without the secret still
// sees the same PermAppConfigure refusal. Empty IDs deliberately make the two
// compose handlers stop at their normal 400 validation, proving the test never
// needs a Docker daemon merely to verify authorization.
func TestInternalPeerMayReachOnlyApprovedSensitiveReads(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	hdr, err := internalauth.Header()
	if err != nil {
		t.Fatalf("provision internal peer secret: %v", err)
	}

	config.GlobalMu.Lock()
	config.Global["OPENAI_API_KEY"] = "test-secret"
	config.GlobalMu.Unlock()
	t.Cleanup(func() {
		config.GlobalMu.Lock()
		delete(config.Global, "OPENAI_API_KEY")
		config.GlobalMu.Unlock()
	})

	tests := []struct {
		name     string
		path     string
		approved func(echo.Context) error
	}{
		{"global", "/v2/app_management/global", func(c echo.Context) error {
			return (&AppManagement{}).GetGlobalSettings(c)
		}},
		{"compose", "/v2/app_management/compose/", func(c echo.Context) error {
			return (&AppManagement{}).MyComposeApp(c, "")
		}},
		{"logs", "/v2/app_management/compose//logs", func(c echo.Context) error {
			return (&AppManagement{}).ComposeAppLogs(c, "", codegen.ComposeAppLogsParams{})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/genuine-peer", func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for key, value := range hdr {
				req.Header.Set(key, value)
			}
			if err := tt.approved(e.NewContext(req, rec)); err != nil {
				t.Fatalf("valid internal peer was refused before the handler responded: %v", err)
			}
			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Fatalf("valid internal peer got auth status %d", rec.Code)
			}
		})

		t.Run(tt.name+"/no-secret", func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			err := tt.approved(e.NewContext(httptest.NewRequest(http.MethodGet, tt.path, nil), rec))
			he, ok := err.(*echo.HTTPError)
			if !ok || he.Code != http.StatusForbidden {
				t.Fatalf("request without the internal secret got %T %v, want 403 permission denial", err, err)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("permission gate wrote the response itself: %q", rec.Body.String())
			}
		})
	}
}
