package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/labstack/echo/v4"
)

// See route/v1/storage_read_gate_test.go for the full rationale: this
// module's Wave 26 hardening pass gated the v2 write routes (Mount/Unmount,
// SetMerge/InitMerge, PutStandby) behind PermStorageWrite but never touched
// the matching reads. Grouped as one test over the read-route family.
func TestStorageReadRoutesDenyNonAdminRoles(t *testing.T) {
	s := &LocalStorage{}
	routes := []struct {
		name    string
		method  string
		path    string
		handler echo.HandlerFunc
	}{
		{"GetMounts", http.MethodGet, "/v2/local_storage/mount", func(ctx echo.Context) error {
			return s.GetMounts(ctx, codegen.GetMountsParams{})
		}},
		{"GetMerges", http.MethodGet, "/v2/local_storage/merge", func(ctx echo.Context) error {
			return s.GetMerges(ctx, codegen.GetMergesParams{})
		}},
		{"GetMergeInitStatus", http.MethodGet, "/v2/local_storage/merge/init", s.GetMergeInitStatus},
		{"GetStorageRoots", http.MethodGet, "/v2/local_storage/storage", s.GetStorageRoots},
		{"GetStandby", http.MethodGet, "/v2/local_storage/standby", s.GetStandby},
	}

	// "user" and "admin" both hold PermStorageRead (see rbac.go), so they
	// belong on the other side of this gate — asserted separately below via
	// requirePerm directly, without invoking the full handler, which would
	// otherwise reach the nil service.MyService in this test binary.
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			for _, role := range []string{"", "guest"} {
				t.Run(role, func(t *testing.T) {
					e := echo.New()
					rec := httptest.NewRecorder()
					ctx := e.NewContext(httptest.NewRequest(rt.method, rt.path, nil), rec)
					if role != "" {
						commonmw.SetRoleInContext(ctx, role, 7)
					}

					err := rt.handler(ctx)
					if err == nil {
						t.Fatalf("%s/role=%q: expected a refusal, got nil (service.MyService would have panicked if reached)", rt.name, role)
					}
					he, ok := err.(*echo.HTTPError)
					if !ok {
						t.Fatalf("%s/role=%q: want *echo.HTTPError, got %T (%v)", rt.name, role, err, err)
					}
					if he.Code != http.StatusForbidden {
						t.Fatalf("%s/role=%q got %d, want 403", rt.name, role, he.Code)
					}
					if rec.Body.Len() != 0 {
						t.Fatalf("%s/role=%q: the gate wrote the response itself: %q", rt.name, role, rec.Body.String())
					}
				})
			}
		})
	}
}

// The gate must not be stricter than the module's own write-side scheme
// either: "user" and "admin" both hold PermStorageRead and must pass.
func TestStorageReadRoutesAllowRolesHoldingPermStorageRead(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			e := echo.New()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/local_storage/mount", nil), httptest.NewRecorder())
			commonmw.SetRoleInContext(ctx, role, 7)
			if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
				t.Fatalf("role %q holding PermStorageRead was refused: %v", role, err)
			}
		})
	}
}
