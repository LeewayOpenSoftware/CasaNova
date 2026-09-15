package route

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"gotest.tools/v3/assert"
)

var (
	_router http.Handler
	_state  *service.State
)

func init() {
	logger.LogInitConsoleOnly()
}

// _internalHdr is the header a peer service presents to prove it is one.
// These tests used to reach the management API on the strength of
// `req.RemoteAddr = "127.0.0.1:0"` alone, which is exactly the waiver that
// made the whole API reachable without a credential. They now authenticate
// the way CasaNova-Common/external/gateway.go does.
var _internalHdr map[string]string

// _management is the same service instance the router serves from, so a test
// can read back what a request wrote without going through an endpoint whose
// authorization it is not trying to exercise.
var _management *service.Management

func setup(t *testing.T) func(t *testing.T) {
	tmpdir, _ := os.MkdirTemp("", "casaos-gateway-route-test")

	internalauth.SetPathsForTest(t.TempDir())
	hdr, err := internalauth.Header()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}
	_internalHdr = hdr

	_state = service.NewState()
	if err := _state.SetRuntimePath(tmpdir); err != nil {
		t.Fatal(err)
	}

	management := service.NewManagementService(_state)
	_management = management
	managementRoute := NewManagementRoute(management)
	_router = managementRoute.GetRoute()

	return func(t *testing.T) {
		management = nil
		_router = nil
		os.RemoveAll(tmpdir)
	}
}

func TestPing(t *testing.T) {
	defer setup(t)(t)

	w := httptest.NewRecorder()

	req, _ := http.NewRequest(http.MethodGet, "/ping", nil)

	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateRoute(t *testing.T) {
	defer setup(t)(t)

	route := &model.Route{
		Path:   "test",
		Target: "http://127.0.0.1:8080",
	}

	body, err := json.Marshal(route)
	assert.NilError(t, err)

	req, _ := http.NewRequest(http.MethodPost, "/v1/gateway/routes", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	w := httptest.NewRecorder()
	_router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	// Registering is a peer's job; reading the whole routing table is not.
	// The internal secret proves "a process on this box", not "an
	// administrator", and GET /routes is admin-only — so the same header that
	// just created a route does not get to enumerate them.
	req, _ = http.NewRequest(http.MethodGet, "/v1/gateway/routes", nil)
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	w = httptest.NewRecorder()
	_router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// What this test is actually about — that the POST persisted the route —
	// read from the service the router serves from, rather than through an
	// endpoint whose authorization is pinned elsewhere.
	routes := _management.GetRoutes()
	assert.Equal(t, 1, len(routes))
	assert.Equal(t, route.Path, routes[0].Path)
	assert.Equal(t, route.Target, routes[0].Target)
}

func TestChangePort(t *testing.T) {
	defer setup(t)(t)

	actualPort := ""

	_state.OnGatewayPortChange(func(s string) error {
		actualPort = s
		return nil
	})

	expectedPort := "123"

	// set
	request := &model.ChangePortRequest{
		Port: expectedPort,
	}

	body, err := json.Marshal(request)
	assert.NilError(t, err)

	req, _ := http.NewRequest(http.MethodPut, "/v1/gateway/port", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	w := httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, expectedPort, actualPort)

	// get
	req, _ = http.NewRequest(http.MethodGet, "/v1/gateway/port", nil)
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}

	w = httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var result *model.Result
	decoder := json.NewDecoder(w.Body)

	err = decoder.Decode(&result)
	assert.NilError(t, err)
	assert.Equal(t, expectedPort, result.Data)
}

func TestChangePortNegative(t *testing.T) {
	defer setup(t)(t)

	expectedPort := "123"

	// set
	request := &model.ChangePortRequest{
		Port: expectedPort,
	}

	body, err := json.Marshal(request)
	assert.NilError(t, err)

	req, _ := http.NewRequest(http.MethodPut, "/v1/gateway/port", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	w := httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, expectedPort, "123")

	// get
	req, _ = http.NewRequest(http.MethodGet, "/v1/gateway/port", nil)
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	w = httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var result *model.Result
	decoder := json.NewDecoder(w.Body)

	err = decoder.Decode(&result)
	assert.NilError(t, err)
	assert.Equal(t, expectedPort, result.Data)

	// emulate error
	_state.OnGatewayPortChange(func(_ string) error {
		return errors.New("error")
	})

	// set
	request.Port = "456"

	body, err = json.Marshal(request)
	assert.NilError(t, err)

	req, _ = http.NewRequest(http.MethodPut, "/v1/gateway/port", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	w = httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, expectedPort, "123")

	// get
	req, _ = http.NewRequest(http.MethodGet, "/v1/gateway/port", nil)
	for k, v := range _internalHdr {
		req.Header.Set(k, v)
	}

	w = httptest.NewRecorder()
	_router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	decoder = json.NewDecoder(w.Body)

	err = decoder.Decode(&result)
	assert.NilError(t, err)
	assert.Equal(t, expectedPort, result.Data)
}
