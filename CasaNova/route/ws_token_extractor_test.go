package route

// The group-level JWT extractor and strictJWT's must agree on where a
// WebSocket's token lives.
//
// They diverged: strictJWT learned to read ?jwt= (browsers cannot set headers
// on a WS handshake, so /v1/sys/wsssh passes the JWT there and keeps ?token=
// for the opaque SSH session id), but the extractor on v1Group — which runs
// FIRST — still read only ?token=. It handed the hex session id to
// jwt.Validate, which of course failed, and every integrated-terminal
// connection died with a 401 the handler never saw. The route-level fix was
// dead code behind a group-level refusal.
//
// This asserts the property directly on the extractors rather than through a
// live handshake: no PTY, no SSH daemon, no timing.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// extractorsUnderTest returns the two lookup functions the router builds, in
// the order a request meets them.
func extractorsUnderTest(t *testing.T) map[string]echo_middleware.ValuesExtractor {
	t.Helper()
	// One shared function now serves both middlewares — which is itself the
	// fix. Kept as a map so the test still reads as "every extractor agrees"
	// if they are ever split again.
	return map[string]echo_middleware.ValuesExtractor{
		"shared": wsTokenExtractor,
	}
}

func extractWith(t *testing.T, f echo_middleware.ValuesExtractor, url string, header string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if header != "" {
		req.Header.Set(echo.HeaderAuthorization, header)
	}
	c := echo.New().NewContext(req, httptest.NewRecorder())
	vals, err := f(c)
	if err != nil || len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func TestBothExtractorsPreferJwtParamOverToken(t *testing.T) {
	// The wsssh shape: ?jwt=<JWT>&token=<opaque ssh session id>.
	const jwt = "eyJhbGciOiJFUzI1NiJ9.payload.sig"
	const sshSession = "9f2c1ab4de7740aa"
	url := "/v1/sys/wsssh?jwt=" + jwt + "&token=" + sshSession

	for name, f := range extractorsUnderTest(t) {
		got := extractWith(t, f, url, "")
		if got == sshSession {
			t.Fatalf("%s returned the SSH session id where a JWT belongs: jwt.Validate will reject it "+
				"and the terminal 401s before the handler runs", name)
		}
		if got != jwt {
			t.Fatalf("%s extracted %q, want the ?jwt= value", name, got)
		}
	}
}

func TestBothExtractorsStillHonourHeaderAndPlainToken(t *testing.T) {
	// The ordinary paths must not regress: Authorization wins over query, and
	// a lone ?token= (used by other WS endpoints) is still read.
	for name, f := range extractorsUnderTest(t) {
		if got := extractWith(t, f, "/v1/anything?jwt=fromquery", "Bearer fromheader"); got != "fromheader" {
			t.Fatalf("%s: header must win, got %q", name, got)
		}
		if got := extractWith(t, f, "/v1/anything?token=plain", ""); got != "plain" {
			t.Fatalf("%s: lone ?token= must still be read, got %q", name, got)
		}
	}
}
