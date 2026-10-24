package route

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/gobwas/ws"
	"github.com/labstack/echo/v4"
)

// dialEventWS starts an in-memory message bus with one event type and returns
// a client connection to its event websocket.
func dialEventWS(t *testing.T) net.Conn {
	t.Helper()
	repo, err := repository.NewDatabaseRepositoryInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	services := service.NewServices(&repo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	services.Start(&ctx)
	api := NewAPIRoute(&services)
	e := echo.New()

	types, _ := json2.Marshal([]model.EventType{{SourceID: "wslimit", Name: "evt"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(types))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if err := api.RegisterEventTypes(e.NewContext(asInternalPublisher(req), rec)); err != nil || rec.Code != 200 {
		t.Fatalf("register: %v %d", err, rec.Code)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := e.NewContext(r, echo.NewResponse(w, e))
		_ = api.SubscribeEventWS(c, "wslimit", codegen.SubscribeEventWSParams{})
	}))
	t.Cleanup(srv.Close)

	conn, _, _, err := ws.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func writeClientFrame(t *testing.T, conn net.Conn, op ws.OpCode, payload []byte) {
	t.Helper()
	f := ws.MaskFrameInPlace(ws.NewFrame(op, true, append([]byte(nil), payload...)))
	if err := ws.WriteFrame(conn, f); err != nil {
		t.Fatalf("write %v frame: %v", op, err)
	}
}

// expectPong reads frames until the server answers our ping.
func expectPong(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		f, err := ws.ReadFrame(conn)
		if err != nil {
			t.Fatalf("waiting for pong: %v", err)
		}
		if f.Header.OpCode == ws.OpPong && string(f.Payload) == payload {
			return
		}
	}
}

// A small client frame is tolerated and pings are still answered.
func TestEventWSToleratesSmallClientFrames(t *testing.T) {
	conn := dialEventWS(t)
	writeClientFrame(t, conn, ws.OpPing, []byte("p1"))
	expectPong(t, conn, "p1")
	writeClientFrame(t, conn, ws.OpText, []byte(`{"hello":"bus"}`))
	writeClientFrame(t, conn, ws.OpPing, []byte("p2"))
	expectPong(t, conn, "p2")
}

// A client announcing a frame far larger than any legitimate message must be
// disconnected at the header, not buffered until the frame completes.
func TestEventWSClosesOnOversizedClientFrame(t *testing.T) {
	conn := dialEventWS(t)
	hdr := ws.Header{Fin: true, OpCode: ws.OpText, Length: 64 << 20, Masked: true, Mask: ws.NewMask()}
	if err := ws.WriteHeader(conn, hdr); err != nil {
		t.Fatal(err)
	}
	// Send no payload: an unbounded reader keeps waiting for the 64 MiB,
	// a bounded one closes the connection right after the header.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, err := ws.ReadFrame(conn)
		if err == nil {
			continue // a heartbeat ping may race the close
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("server kept the connection open after a 64 MiB frame header; want it closed")
		}
		return
	}
}
