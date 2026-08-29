package route

// The truth table of requireBusMutator, plus the one property that keeps the
// heartbeat alive: internal socket publishers must pass with no token at all,
// because that is exactly how every in-house service reaches the bus.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/labstack/echo/v4"
)

func gateCtx(t *testing.T, role string, viaSocket bool) echo.Context {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v2/message_bus/event/src/name", nil)
	if viaSocket {
		req = req.WithContext(context.WithValue(req.Context(), unixSocketCtxKey, true))
	}
	c := e.NewContext(req, httptest.NewRecorder())
	if role != "" {
		commonmw.SetRoleInContext(c, role, 1)
	}
	return c
}

func TestBusMutatorGate(t *testing.T) {
	cases := []struct {
		name      string
		role      string
		viaSocket bool
		allowed   bool
	}{
		{"socket interno sin token", "", true, true},
		{"admin por TCP", "admin", false, true},
		{"user por TCP", "user", false, false},
		{"guest por TCP", "guest", false, false},
		{"sin rol por TCP (token invalido no llega, pero fail-closed igual)", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireBusMutator(gateCtx(t, tc.role, tc.viaSocket))
			if tc.allowed && err != nil {
				t.Fatalf("legitimate caller blocked: %v — internal publishers dying means no heartbeat, no events, no UI notifications", err)
			}
			if !tc.allowed {
				if err == nil {
					t.Fatal("forger admitted: a non-admin can inject events/actions with arbitrary sourceIDs")
				}
				he, ok := err.(*echo.HTTPError)
				if !ok || he.Code != http.StatusForbidden {
					t.Fatalf("refusal must be an aborting 403 HTTPError, got %v — a ctx.JSON-style nil return would let the handler continue (the gate-return defect class)", err)
				}
			}
		})
	}
}
