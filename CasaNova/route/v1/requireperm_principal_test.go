package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// THE PROPERTY, NOW ENFORCED BY THE DEFAULT RATHER THAN PER ROUTE.
//
// requirePerm used to ask one question — "does the role hold the permission" —
// and a request carrying no principal at all passed it whenever the permission
// was one guest holds. Around forty routes were closed by hand before the
// pattern was recognised as a fail-open DEFAULT rather than a run of individual
// oversights. These tests pin the closed default so the class cannot return by
// someone simply writing a new handler the obvious way.
//
// guestBits is the whole reachable surface of the old hole: those are the only
// permissions an unauthenticated context could ever satisfy.
var guestBits = map[string]rbac.Permission{
	"PermSystemRead": rbac.PermSystemRead,
	"PermAppView":    rbac.PermAppView,
}

// Precondition, asserted rather than assumed. If guest ever loses these bits
// the tests below would pass for the wrong reason — they would be proving that
// a permission nobody holds is refused, which is not the property.
func TestGuestStillHoldsTheBitsThatMadeTheHoleReachable(t *testing.T) {
	for name, p := range guestBits {
		if !rbac.Allows(rbac.RoleGuest, p) {
			t.Fatalf("guest no longer holds %s — the premise of the requirePerm "+
				"principal check changed; re-read the gate before trusting these tests", name)
		}
	}
}

// THE PROPERTY: no principal, no authorization — even for a permission the
// guest role genuinely holds.
func TestRequirePermRefusesAContextWithNoPrincipal(t *testing.T) {
	for name, p := range guestBits {
		t.Run(name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/sys/anything", nil), rec)

			err := requirePerm(ctx, p)
			if err == nil {
				t.Fatalf("requirePerm allowed a caller with no principal to use %s — "+
					"the role fell back to guest and borrowed its permission, which is "+
					"exactly the hole this default closes", name)
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("denial was %T (%v); it must be an *echo.HTTPError so echo aborts "+
					"the handler chain — a gate that writes the response with ctx.JSON "+
					"returns nil and the handler runs on", err, err)
			}
			if he.Code != http.StatusUnauthorized {
				t.Fatalf("denial was %d, want 401: the refusal must say the caller is "+
					"unidentified, not that the permission table rejected them — guest "+
					"satisfies %s", he.Code, name)
			}
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Fatalf("the gate committed the response itself (code=%d, body=%q); the "+
					"error handler owns the body", rec.Code, rec.Body.String())
			}
		})
	}
}

// POSITIVE CONTROL. Without this the test above is satisfied by a gate that
// refuses everyone, which would be an outage rather than a fix.
func TestRequirePermStillAllowsRealPrincipals(t *testing.T) {
	for name, p := range guestBits {
		t.Run(name+"/identified-guest-account", func(t *testing.T) {
			if err := requirePerm(ctxWith(rbac.RoleGuest, 7), p); err != nil {
				t.Fatalf("a real signed-in guest account was refused %s: %v — a guest "+
					"ACCOUNT is a principal; only the absent one is being rejected", name, err)
			}
		})
		t.Run(name+"/admin", func(t *testing.T) {
			if err := requirePerm(ctxWith(rbac.RoleAdmin, 1), p); err != nil {
				t.Fatalf("an authenticated admin was refused %s: %v", name, err)
			}
		})
		t.Run(name+"/user", func(t *testing.T) {
			if err := requirePerm(ctxWith(rbac.RoleUser, 42), p); err != nil {
				t.Fatalf("an authenticated ordinary user was refused %s: %v", name, err)
			}
		})
	}
}

// MONOTONICITY — the property that made this safe to land on a frozen line.
//
// The principal test runs AFTER the permission test, so it can only ever turn
// an ALLOW into a DENY. Every refusal that already happened must still happen
// at the same branch, with the same 403 and the same body. If someone reorders
// the two checks, tokenless callers start getting 401 where clients and tests
// expect 403 across 200+ call sites — this is what catches that.
func TestRequirePermLeavesExistingDenialsByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		ctx  echo.Context
		perm rbac.Permission
	}{
		{"no-principal/non-guest-bit", ctxWith("", 0), rbac.PermFileDelete},
		{"identified-guest/admin-bit", ctxWith(rbac.RoleGuest, 7), rbac.PermUserManage},
		{"identified-user/admin-bit", ctxWith(rbac.RoleUser, 42), rbac.PermSystemConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requirePerm(tc.ctx, tc.perm)
			if err == nil {
				t.Fatal("a refusal that predates this change stopped happening")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("denial was %T, want *echo.HTTPError", err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("denial was %d, want 403 — this refusal comes from the PERMISSION "+
					"table and must keep its original shape. A 401 here means the principal "+
					"check was moved ahead of the permission check, which is not a monotone "+
					"change: it rewrites denials clients already parse", he.Code)
			}
		})
	}
}

// THE NAMED EXEMPTION MUST SURVIVE, or inter-service calls break.
//
// A genuine peer service has no user token and therefore no user id — it proves
// itself with the per-boot internal secret instead. Its exemption lives in
// requirePermInternal, which answers BEFORE the strict default is consulted.
// This is the shape the change is arguing for: the exemption is visible in the
// route's own gate, not buried as a tolerance in the default.
func TestPeerServiceExemptionSurvivesTheStrictDefault(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	secret, err := internalauth.Secret()
	if err != nil {
		t.Fatalf("could not mint the peer secret: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/notify/system_status", nil)
	req.Header.Set(internalauth.HeaderName, secret)
	ctx := e.NewContext(req, httptest.NewRecorder())

	// No principal at all — exactly what the JWT skipper leaves behind for a peer.
	if commonmw.UserIDFromContext(ctx) != 0 {
		t.Fatal("setup wrong: this context was supposed to carry no principal")
	}
	if err := requirePermInternal(ctx, rbac.PermSystemRead); err != nil {
		t.Fatalf("a peer service presenting the internal secret was refused: %v — "+
			"inter-service notifications and share teardown run through this path", err)
	}

	// Negative control: the same tokenless request WITHOUT the secret is refused.
	// Without this, the test above would pass even if the exemption were
	// unconditional, which is the loopback-trust bug this codebase already shipped.
	bare := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/notify/system_status", nil), httptest.NewRecorder())
	if err := requirePermInternal(bare, rbac.PermSystemRead); err == nil {
		t.Fatal("a caller presenting NO secret and no token was allowed — the peer " +
			"exemption is unconditional, which means it is not a credential check")
	}
}
