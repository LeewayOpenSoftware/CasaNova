package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// 26.10.1 audit (container routes): GetDockerNetworks and AppUsageList
// had no explicit requirePerm call at all, unlike every sibling handler in
// this file (ContainerUpdateInfo, ToComposeYAML, WsContainerLogs, ...). They
// were not exploitable today — v1Group's JWT middleware already refuses any
// request without a valid, unrevoked token, and PermAppView (the bit this
// test checks for) is held by every role including guest — but a context
// with no principal at all (RoleFromContext failing closed to guest with no
// UserID set, exactly the scenario rbac_gate.go's own doc comment warns
// about) must still be refused, not silently treated as guest. This asserts
// that refusal, the same way docker_gate_test.go asserts ContainerUpdateInfo's.
func TestDockerNetworksAndAppUsageDenyUnauthenticatedContext(t *testing.T) {
	cases := []struct {
		name    string
		handler func(echo.Context) error
	}{
		{"GetDockerNetworks", GetDockerNetworks},
		{"AppUsageList", AppUsageList},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/container/usage", nil), rec)
			// Deliberately no SetRoleInContext call: this is the "nobody
			// authenticated" context, not an explicit guest session.

			err := tc.handler(ctx)
			if err == nil {
				t.Fatal("expected a refusal, got nil (service.MyService would have panicked if reached)")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("want *echo.HTTPError, got %T (%v)", err, err)
			}
			if he.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401 (PermAppView is held by guest, so this must fail on the \"no principal\" check, not the permission check)", he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("the gate wrote the response itself: %q", rec.Body.String())
			}
		})
	}
}
