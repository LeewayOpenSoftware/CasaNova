package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"github.com/labstack/echo/v4"
)

// Four mutating management routes carried NO RBAC check at all, while PUT /tls
// in the same file did. A non-admin the JWT middleware admitted could rewrite
// the internal routing table (every user's traffic), drop the global IP
// whitelist that is enforced on every inbound request, or move the public port.
//
// GET /tls carried a comment claiming "Admin-only via the group JWT + an
// in-handler role check for defense-in-depth" and no check at all — the
// comment described its neighbor PUT /tls, not itself. A comment naming a
// control is not evidence the control exists; only the entry in the table
// below, and the census in TestPeerAllowedRoutesStillCarryTheAdminBranch, are.
// Its caller was verified independently of PUT's (repo-wide grep,
// CasaNova-UI-v2/src/views/settings/NetworkSettings.vue is the only one) —
// browser UI, admin, no peer, same as PUT, but checked rather than assumed.
//
// WHAT THESE TESTS CAN AND CANNOT REACH, stated plainly so nobody reads more
// into a green run than it earns. The handlers are unexported closures behind
// the group's JWT middleware, so a request can only reach them two ways: with a
// real signed token, or by presenting the internal secret (which makes
// authskip skip the JWT). Minting a real token here would mean standing up a
// JWKS server and writing the user-service address file, and it would poison a
// process-wide public-key cache shared with the other tests in this package.
// So these drive the handlers via the secret and set the role directly, which
// covers the whitelist pair completely. For the two peer-allowed routes the
// secret is itself sufficient by design, so their role branch is pinned at the
// source instead — see TestPeerAllowedRoutesStillCarryTheAdminBranch.

func mgmtGateCtx(method, path string, role rbac.Role, hdr map[string]string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath(path)
	if role != "" {
		commonmw.SetRoleInContext(c, string(role), 7)
	}
	return c, rec
}

// mgmtHandler pulls the handler out of the REAL route tree, so a gate that was
// added to the wrong method or path fails here rather than passing on a
// hand-written call.
func mgmtHandler(t *testing.T, method, path string) echo.HandlerFunc {
	t.Helper()
	m := NewManagementRoute(service.NewManagementService(service.NewState()))
	e := echo.New()
	m.buildV1Group(e)
	c := e.NewContext(httptest.NewRequest(method, path, nil), httptest.NewRecorder())
	e.Router().Find(method, path, c)
	h := c.Handler()
	if h == nil {
		t.Fatalf("no handler registered for %s %s — the route moved and this test "+
			"would otherwise pass by testing nothing", method, path)
	}
	return h
}

var mutatingManagementRoutes = []struct {
	method    string
	path      string
	peerAllow bool // a genuine peer service legitimately calls this tokenless
}{
	{http.MethodPost, "/v1/gateway/routes", true},     // every service registers at boot
	{http.MethodPut, "/v1/gateway/port", true},        // CasaNova relays the operator's change
	{http.MethodPost, "/v1/gateway/whitelist", false}, // browser UI only
	{http.MethodDelete, "/v1/gateway/whitelist", false},
	{http.MethodPut, "/v1/gateway/tls", false}, // the pre-existing gate, pinned with the rest
	{http.MethodGet, "/v1/gateway/tls", false}, // read counterpart: no gate at all until this fix
	{http.MethodGet, "/v1/gateway/whitelist", false}, // read counterpart of POST/DELETE: no gate at all until this fix
}

// The whitelist and TLS routes must refuse a non-admin EVEN WHEN the caller
// holds the internal secret. The secret proves "a process on this box", not
// "an administrator", and these three have no peer caller at all.
func TestAdminOnlyManagementRoutesRefuseNonAdminsDespiteTheSecret(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	hdr, err := internalauth.Header()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}

	for _, r := range mutatingManagementRoutes {
		if r.peerAllow {
			continue
		}
		for _, role := range []rbac.Role{"", rbac.RoleGuest, rbac.RoleUser} {
			label := string(role)
			if label == "" {
				label = "no-role"
			}
			t.Run(r.method+" "+r.path+" as "+label, func(t *testing.T) {
				h := mgmtHandler(t, r.method, r.path)
				ctx, rec := mgmtGateCtx(r.method, r.path, role, hdr)
				_ = h(ctx)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("answered %d, want 403. The whitelist is enforced on every "+
						"inbound request and TLS config owns the whole inbound surface; "+
						"neither may be changed by a non-admin. Body: %s",
						rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// An admin must still get through — a gate that refuses everyone is not a gate.
func TestAdminOnlyManagementRoutesAdmitAnAdmin(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	hdr, _ := internalauth.Header()

	for _, r := range mutatingManagementRoutes {
		if r.peerAllow {
			continue
		}
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			h := mgmtHandler(t, r.method, r.path)
			ctx, rec := mgmtGateCtx(r.method, r.path, rbac.RoleAdmin, hdr)
			_ = h(ctx)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("an admin was refused 403 on %s %s", r.method, r.path)
			}
		})
	}
}

// THE BREAKAGE THIS FIX NEARLY SHIPPED. POST /routes and PUT /port have a
// legitimate TOKENLESS caller: every CasaNova service registers itself at boot
// through CasaNova-Common/external/gateway.go, which sends the internal secret
// and no JWT — so nothing sets a role and RoleFromContext yields guest. A bare
// admin check answers 403 to every service's route registration and takes the
// box down at startup. This test is the regression guard for that.
func TestPeerServicesCanStillRegisterRoutesAndChangePort(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	hdr, err := internalauth.Header()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}

	for _, r := range mutatingManagementRoutes {
		if !r.peerAllow {
			continue
		}
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			h := mgmtHandler(t, r.method, r.path)
			ctx, rec := mgmtGateCtx(r.method, r.path, "", hdr) // peer: no role at all
			_ = h(ctx)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("a genuine peer bearing the internal secret was refused 403. "+
					"This is how every service registers its routes at boot; refusing it "+
					"takes the whole box down. Body: %s", rec.Body.String())
			}
		})
	}
}

// The role branch of the two peer-allowed routes cannot be reached through the
// router without a real signed token (see the note at the top), so it is pinned
// at the source. Anti-vacuity first: an unreadable file or a missing anchor
// fails rather than passing silently.
func TestPeerAllowedRoutesStillCarryTheAdminBranch(t *testing.T) {
	src, err := os.ReadFile("management_route.go")
	if err != nil {
		t.Fatalf("cannot read management_route.go: %v", err)
	}
	text := string(src)

	gate := regexp.MustCompile(`!managementAdmin\(ctx\) && !managementPeer\(ctx\)`)
	if n := len(gate.FindAllString(text, -1)); n != 2 {
		t.Errorf("found %d admin-or-peer gates, want exactly 2 (POST /routes and "+
			"PUT /port). A mutating route either lost its gate or gained a peer "+
			"exemption it has no caller for.", n)
	}
	adminOnly := regexp.MustCompile(`if !managementAdmin\(ctx\) \{`)
	if n := len(adminOnly.FindAllString(text, -1)); n != 3 {
		t.Errorf("found %d admin-only gates, want exactly 3 (GET, POST and DELETE "+
			"/whitelist)", n)
	}
	// Count, not Contains: a boolean check on the bare string stays true even
	// if one of the two /tls gates is deleted, because managementAdmin's own
	// body ("return rbac.Allows(...)") also contains it — a Contains check
	// here would never go red. Anchored to the "if !...{" CALL-SITE form
	// instead, which excludes the helper's definition and matches only an
	// actual gate in a handler.
	inlineAdminCheck := regexp.MustCompile(`if !rbac\.Allows\(commonmw\.RoleFromContext\(ctx\), rbac\.PermSystemConfig\) \{`)
	if n := len(inlineAdminCheck.FindAllString(text, -1)); n != 2 {
		t.Errorf("found %d inline PermSystemConfig call sites, want exactly 2 (GET /tls and "+
			"PUT /tls). One of the pair lost its gate.", n)
	}
}
