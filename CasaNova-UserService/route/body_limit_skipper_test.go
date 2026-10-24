package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// Upload routes enforce their own larger caps in the handler and must not be
// cut off at 1M; ordinary JSON routes in the same group must be.
func TestJSONBodyLimitSkipperExemptsUploads(t *testing.T) {
	e := echo.New()
	for path, want := range map[string]bool{
		"/v1/users/avatar":              true,
		"/v1/users/current/custom/:key": true,
		"/v1/users/current/image/:key":  true,
		"/v1/users/login":               false,
		"/v1/users/current":             false,
		"/v1/users/current/password":    false,
		"/v1/users/:id/role":            false,
	} {
		c := e.NewContext(httptest.NewRequest(http.MethodPut, "/", nil), httptest.NewRecorder())
		c.SetPath(path)
		if got := jsonBodyLimitSkipper(c); got != want {
			t.Errorf("jsonBodyLimitSkipper(%s) = %v, want %v", path, got, want)
		}
	}
}
