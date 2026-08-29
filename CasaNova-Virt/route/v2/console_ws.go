package v2

import (
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// vncUpgrader upgrades the console HTTP request to a WebSocket. Origin is not
// checked here: the request already passed the gateway + this service's JWT
// middleware (console access is admin-gated via requireConfig below), and the
// browser only ever opens same-origin URLs against the CasaNova gateway.
var vncUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

// VncWebSocket is a websockify bridge: it relays raw bytes between the browser
// (noVNC, over a WebSocket) and the VM's VNC TCP socket on the host. This is
// what makes the in-dashboard VM console actually work — browsers can't speak
// raw TCP VNC, so noVNC needs a WS endpoint on the same origin.
//
// Route: GET /v2/virtualization/domain/:name/vnc/ws?token=<jwt>
// Auth: console = full guest control → admin-gated (requireConfig).
func (h *VirtHandler) VncWebSocket(c echo.Context) error {
	if err := requireConfig(c); err != nil {
		return err
	}

	name := c.Param("name")
	info, err := h.svc.Domain().Console(name)
	if err != nil {
		return fail(c, http.StatusNotFound, err.Error())
	}
	if info.Port <= 0 {
		// autoport is only resolved to a concrete port once the guest runs.
		return fail(c, http.StatusConflict, "console not available — is the VM running?")
	}

	addr := net.JoinHostPort(info.Host, strconv.Itoa(info.Port))
	tcp, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fail(c, http.StatusBadGateway, "cannot reach VNC server: "+err.Error())
	}
	defer tcp.Close()

	ws, err := vncUpgrader.Upgrade(c.Response().Writer, c.Request(), nil)
	if err != nil {
		// Upgrade already wrote the HTTP error.
		logger.Error("vnc: websocket upgrade failed", zap.Error(err), zap.String("domain", name))
		return nil
	}
	defer ws.Close()

	logger.Info("vnc: console bridge open", zap.String("domain", name), zap.String("target", addr))

	// Pump both directions until either side closes. noVNC exchanges binary
	// frames; we relay raw bytes verbatim.
	done := make(chan struct{}, 2)

	// TCP (VNC) -> WebSocket (browser)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := tcp.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	// WebSocket (browser) -> TCP (VNC)
	go func() {
		for {
			mt, data, rerr := ws.ReadMessage()
			if rerr != nil {
				break
			}
			if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
				if _, werr := tcp.Write(data); werr != nil {
					break
				}
			}
		}
		done <- struct{}{}
	}()

	<-done // first side to close tears down the bridge (deferred Close on both)
	logger.Info("vnc: console bridge closed", zap.String("domain", name))
	return nil
}
