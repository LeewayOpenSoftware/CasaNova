package route

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/labstack/echo/v4"
	"gotest.tools/assert"
)

const healthURL = "/v2/message_bus" + HealthPathSuffix

// asTrustedLocal stamps the request the way the unix-socket http.Server
// does, so the JWT middleware skips it exactly as it does in production
// for root-only callers on /run/casanova/internal/message-bus.sock.
func asTrustedLocal(req *http.Request) *http.Request {
	return req.WithContext(UnixSocketConnContext(req.Context(), nil))
}

// Wiring test. The liveness endpoint is not part of the generated
// server surface, so it can only work if BOTH the echo route and the
// OpenAPI-validator skip are in place — miss either and the request
// dies with 404 in middleware before reaching the handler. This is the
// whole chain: router construction -> JWT skip for the unix socket ->
// validator skip -> handler -> JSON body.
func TestHealthEndpointIsServedByTheRealRouter(t *testing.T) {
	swagger, err := codegen.GetSwagger()
	assert.NilError(t, err)

	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := service.NewServices(&repo)

	ctx, cancel := context.WithCancel(context.Background())
	services.Start(&ctx)
	defer func() {
		cancel()
		services.Wait()
	}()

	handler, err := NewAPIRouter(swagger, &services)
	assert.NilError(t, err)

	deadline := time.Now().Add(5 * time.Second)
	var rec *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, asTrustedLocal(httptest.NewRequest(http.MethodGet, healthURL, nil)))
		if rec.Code == http.StatusOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	assert.Equal(t, rec.Code, http.StatusOK, "body: %s", rec.Body.String())

	var health service.BusHealth
	assert.NilError(t, json2.UnmarshalFromString(rec.Body.String(), &health))
	assert.Equal(t, health.Healthy, true)
	assert.Equal(t, len(health.Goroutines), 3)
	assert.Equal(t, health.Events.Accepting, true)
}

// The endpoint must answer 503 (not 200, not a connection error) when
// the bus is not delivering — that is the signal a monitoring probe
// acts on, and the body still has to carry the reason.
func TestHealthEndpointReports503WhenDispatchersAreDown(t *testing.T) {
	swagger, err := codegen.GetSwagger()
	assert.NilError(t, err)

	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := service.NewServices(&repo)

	handler, err := NewAPIRouter(swagger, &services)
	assert.NilError(t, err)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, asTrustedLocal(httptest.NewRequest(http.MethodGet, healthURL, nil)))

	assert.Equal(t, rec.Code, http.StatusServiceUnavailable)

	var health service.BusHealth
	assert.NilError(t, json2.UnmarshalFromString(rec.Body.String(), &health))
	assert.Equal(t, health.Healthy, false)
	assert.Assert(t, len(health.Problems) > 0)
}

// The bus is reachable from the internet through the gateway tunnel, so
// the liveness endpoint must not be a new unauthenticated surface.
func TestHealthEndpointRequiresAuthWhenNotOnTheUnixSocket(t *testing.T) {
	swagger, err := codegen.GetSwagger()
	assert.NilError(t, err)

	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := service.NewServices(&repo)

	handler, err := NewAPIRouter(swagger, &services)
	assert.NilError(t, err)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthURL, nil))

	assert.Equal(t, rec.Code, http.StatusUnauthorized)
}

// B11 core contract: a publish that the dispatcher never took must not
// be answered with 200 OK.
//
// Mutation that must fail this test: ignore the error from
// EventServiceWS.Publish and return http.StatusOK unconditionally (the
// pre-B11 handler). Every producer on the box then believes the bus
// delivered the event.
func TestPublishEventDoesNotReport200WhenTheEventIsNotAccepted(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	// Services deliberately NOT started: this is the state the bus is
	// left in when the dispatcher dies and nothing restarts it.
	services := service.NewServices(&repo)
	apiRoute := NewAPIRoute(&services)

	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"foo":"bar"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	assert.NilError(t, apiRoute.PublishEvent(e.NewContext(req, rec), "b11", "b11:event:test"))
	assert.Equal(t, rec.Code, http.StatusServiceUnavailable)
}

// ...and the healthy path must keep answering 200, or the fix would
// just trade a silent failure for a permanent false alarm.
func TestPublishEventStillReports200WhenTheDispatcherAcceptsIt(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := service.NewServices(&repo)

	ctx, cancel := context.WithCancel(context.Background())
	services.Start(&ctx)
	defer func() {
		cancel()
		services.Wait()
	}()

	_, err = services.EventTypeService.RegisterEventType(model.EventType{
		SourceID: "b11",
		Name:     "b11:event:test",
	})
	assert.NilError(t, err)

	deadline := time.Now().Add(5 * time.Second)
	for !services.Health().Events.Accepting && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	apiRoute := NewAPIRoute(&services)

	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"foo":"bar"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	assert.NilError(t, apiRoute.PublishEvent(e.NewContext(req, rec), "b11", "b11:event:test"))
	assert.Equal(t, rec.Code, http.StatusOK, "body: %s", rec.Body.String())
}
