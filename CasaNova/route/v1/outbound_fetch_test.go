package v1

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// newAuthedContext builds a request context the way the JWT middleware would
// after validating a token for the given role.
func newAuthedContext(t *testing.T, method, target, role string, userID int) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(method, target, nil), rec)
	if role != "" {
		commonmw.SetRoleInContext(ctx, role, userID)
	}
	return ctx, rec
}

func httpErrCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("want *echo.HTTPError, got %T (%v)", err, err)
	}
	return he.Code
}

// The caller gate must run before ANY client-supplied input is looked at.
//
// This is the ordering the confirmed attack turned on: an unauthenticated
// GET /v1/sys/proxy?url=… made the box resolve and fetch an attacker URL. The
// request here carries NO url parameter at all, so the pre-fix handler's first
// action (missing-parameter check) answered 400. A 401 proves the gate is
// ahead of the parser, and therefore ahead of DNS and the socket.
func TestSystemProxyGateRunsBeforeAnyInputIsParsed(t *testing.T) {
	ctx, rec := newAuthedContext(t, http.MethodGet, "/v1/sys/proxy", "", 0)

	err := GetSystemProxy(ctx)
	if code := httpErrCode(t, err); code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller got %d, want 401 (gate must precede url parsing)", code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("the handler committed a body on the deny path: %q", rec.Body.String())
	}
}

// No anonymous caller may make this process originate outbound traffic.
// The upstream server counts hits; it must never be reached.
func TestSystemProxyDeniesAnonymousWithoutTouchingUpstream(t *testing.T) {
	var hits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte("pwned"))
	}))
	defer upstream.Close()

	ctx, _ := newAuthedContext(t, http.MethodGet, "/v1/sys/proxy?url="+upstream.URL, "", 0)

	// 401 (not 403) is what distinguishes "refused by the caller gate" from
	// "refused by the private-address check" — the test server is loopback,
	// so without this distinction the assertion would pass either way.
	if code := httpErrCode(t, GetSystemProxy(ctx)); code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller got %d, want 401", code)
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Fatalf("upstream was contacted %d time(s) for an anonymous request", n)
	}
}

// Logged-in but under-privileged roles are refused too: guest holds
// PermSystemRead, so a gate built on a read permission would have let the
// tokenless case through. Only PermSystemConfig (admin) may spend egress.
func TestSystemProxyDeniesNonAdminRoles(t *testing.T) {
	for _, role := range []string{"guest", "user"} {
		t.Run(role, func(t *testing.T) {
			ctx, _ := newAuthedContext(t, http.MethodGet, "/v1/sys/proxy?url=http://example.com/", role, 7)
			if code := httpErrCode(t, GetSystemProxy(ctx)); code != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403", role, code)
			}
		})
	}
}

// The login half of the gate is what removes the anonymous caller, and a
// permission check cannot stand in for it: guest is the role a TOKENLESS
// request resolves to, and guest deliberately holds PermSystemRead. So a
// future outbound handler gated on a read permission would be wide open again
// unless requireOutboundFetchCaller refuses missing logins on its own.
func TestOutboundFetchCallerRejectsTokenlessEvenForAPermissionGuestHolds(t *testing.T) {
	ctx, _ := newAuthedContext(t, http.MethodGet, "/v1/sys/anything", "", 0)

	// Sanity: guest really does hold this permission, so the PERMISSION TABLE
	// alone cannot refuse the anonymous caller — which is why this gate has to
	// check the login separately. Asserted against the table directly: it used
	// to be asserted by expecting requirePerm to allow the caller, but the
	// default now refuses a context with no principal too, so that phrasing
	// tested the wrong thing.
	if !rbac.Allows(rbac.RoleGuest, rbac.PermSystemRead) {
		t.Fatal("premise broken: guest no longer holds PermSystemRead")
	}

	if code := httpErrCode(t, requireOutboundFetchCaller(ctx, rbac.PermSystemRead)); code != http.StatusUnauthorized {
		t.Fatalf("tokenless caller got %d for a guest-held permission, want 401", code)
	}

	// A real login with that permission is still allowed through.
	authed, _ := newAuthedContext(t, http.MethodGet, "/v1/sys/anything", "user", 7)
	if err := requireOutboundFetchCaller(authed, rbac.PermSystemRead); err != nil {
		t.Fatalf("a logged-in user holding the permission was refused: %v", err)
	}
}

// The docker tag lookup is the same primitive with a narrower destination and
// had the same missing caller check.
func TestDockerImageTagsDeniesAnonymousWithoutTouchingUpstream(t *testing.T) {
	var hits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer upstream.Close()

	ctx, _ := newAuthedContext(t, http.MethodGet, "/v1/container/image/tags?image=nginx", "", 0)
	if code := httpErrCode(t, GetDockerImageTags(ctx)); code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller got %d, want 401", code)
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Fatalf("upstream was contacted %d time(s)", n)
	}

	ctx, _ = newAuthedContext(t, http.MethodGet, "/v1/container/image/tags?image=nginx", "user", 7)
	if code := httpErrCode(t, GetDockerImageTags(ctx)); code != http.StatusForbidden {
		t.Fatalf("non-admin role got %d, want 403", code)
	}
}

// No proxied body may be interpretable as an active document on CasaNova's
// origin. text/html was the confirmed XSS vector; image/svg+xml is the same
// class (an SVG can carry <script>) and must not survive either.
func TestInertResponseNeverServesActiveContentTypes(t *testing.T) {
	cases := []struct {
		upstream string
		want     string
	}{
		{"text/html", "application/octet-stream"},
		{"text/html; charset=utf-8", "application/octet-stream"},
		{"image/svg+xml", "application/octet-stream"},
		{"application/xhtml+xml", "application/octet-stream"},
		{"text/javascript", "application/octet-stream"},
		{"", "application/octet-stream"},
		{"IMAGE/PNG", "image/png"},
		{"image/jpeg", "image/jpeg"},
		{"application/json", "application/json; charset=utf-8"},
	}

	for _, tc := range cases {
		t.Run(tc.upstream, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/sys/proxy", nil), rec)

			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("<script>alert(document.cookie)</script>")),
			}
			if tc.upstream != "" {
				resp.Header.Set("Content-Type", tc.upstream)
			}

			if err := writeInertUpstreamResponse(ctx, resp, outboundMaxResponseBytes); err != nil {
				t.Fatalf("relay failed: %v", err)
			}

			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Fatalf("upstream %q served as %q, want %q", tc.upstream, got, tc.want)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
				t.Fatalf("Content-Disposition = %q, want attachment", got)
			}
			if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
				t.Fatalf("Content-Security-Policy = %q, want a sandbox directive", got)
			}
		})
	}
}

// Relaying the upstream Content-Length while truncating at the limit promises
// the client more bytes than are sent — a hung or broken response instead of a
// short one. Either the declared length matches what we may send, or no length
// is declared at all.
func TestInertResponseDropsUnservableContentLength(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/sys/proxy", nil), rec)

	body := strings.Repeat("A", 64)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "application/octet-stream")
	resp.Header.Set("Content-Length", "999999999")

	if err := writeInertUpstreamResponse(ctx, resp, 16); err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want it dropped (we may only send 16 bytes)", got)
	}
	if n := rec.Body.Len(); int64(n) != 16 {
		t.Fatalf("relayed %d bytes, want the 16-byte cap", n)
	}
}

// A declared length that fits under the cap is still relayed.
func TestInertResponseKeepsServableContentLength(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/sys/proxy", nil), rec)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("0123456789")),
	}
	resp.Header.Set("Content-Length", "10")

	if err := writeInertUpstreamResponse(ctx, resp, 1024); err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if got := rec.Header().Get("Content-Length"); got != "10" {
		t.Fatalf("Content-Length = %q, want 10", got)
	}
}

// The destination half must keep refusing internal targets and non-http
// schemes. Kept here so moving it out of system.go cannot silently drop it.
func TestValidateOutboundURLRefusesInternalAndNonHTTP(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", http.StatusBadRequest},
		{"file:///etc/shadow", http.StatusBadRequest},
		{"gopher://example.com/", http.StatusBadRequest},
		{"http://127.0.0.1:85/ping", http.StatusForbidden},
		{"http://0.0.0.0:85/ping", http.StatusForbidden},
		{"http://192.168.1.1:85/ping", http.StatusForbidden},
		{"http://[::1]:85/", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			_, err := validateOutboundURL(tc.raw)
			if code := httpErrCode(t, err); code != tc.want {
				t.Fatalf("%q got %d, want %d", tc.raw, code, tc.want)
			}
		})
	}
}

// The concurrency budget must actually bound in-flight fetches, must hand the
// slot back, and must give up when the caller goes away rather than pinning a
// slot for the full wait. Uses its own limiter instance: the package-level one
// is process state and must not be shared between tests.
func TestOutboundLimiterBoundsAndReleases(t *testing.T) {
	l := newOutboundLimiter(2, 50*time.Millisecond)

	if err := l.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if err := l.acquire(context.Background()); err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	// Capacity is 2; the third must be refused after the wait, not admitted.
	if err := l.acquire(context.Background()); !errors.Is(err, errOutboundBusy) {
		t.Fatalf("third acquire = %v, want errOutboundBusy", err)
	}

	l.release()
	if err := l.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}

	// A caller that hangs up while queued gets its own error and does not
	// occupy the slot it never got.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.acquire(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
	}

	l.release()
	l.release()
}

// Failure injection on the one piece of shared state this change adds: a slot
// taken must come back on EVERY exit path, or the endpoint bricks itself after
// outboundMaxInFlight refused requests and every later caller waits out
// outboundQueueWait for nothing. Both handlers are driven through a refusal
// that happens AFTER the slot is taken:
//   - the proxy: an authorised admin aiming at a private address (403)
//   - the tag lookup: a registry outside the allow-list (handled error)
//
// Neither opens a socket, so this stays hermetic. Capacity is then checked by
// taking every slot with a short deadline: a leak shows up as a fast
// DeadlineExceeded instead of a 15s stall.
func TestOutboundSlotIsReleasedOnRefusalPaths(t *testing.T) {
	for i := 0; i < outboundMaxInFlight*2; i++ {
		proxyCtx, _ := newAuthedContext(t, http.MethodGet, "/v1/sys/proxy?url=http://127.0.0.1:85/ping", "admin", 1)
		if code := httpErrCode(t, GetSystemProxy(proxyCtx)); code != http.StatusForbidden {
			t.Fatalf("iteration %d: proxy to a private address got %d, want 403", i, code)
		}

		tagCtx, _ := newAuthedContext(t, http.MethodGet, "/v1/container/image/tags?image=evil.example.com/a/b", "admin", 1)
		if err := GetDockerImageTags(tagCtx); err != nil {
			t.Fatalf("iteration %d: tag lookup returned %v, want the handled-error payload", i, err)
		}
	}

	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	taken := 0
	defer func() {
		for ; taken > 0; taken-- {
			outboundFetchLimiter.release()
		}
	}()
	for taken < outboundMaxInFlight {
		if err := outboundFetchLimiter.acquire(deadline); err != nil {
			t.Fatalf("only %d of %d slots were free after the refusal paths: %v (slots leaked)",
				taken, outboundMaxInFlight, err)
		}
		taken++
	}
}
