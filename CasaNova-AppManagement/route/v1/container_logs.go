package v1

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	v2 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/route/v2"
	modelCommon "github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// maxLogTail bounds the backlog a single client can ask the daemon to replay.
// Without a bound, "?tail=5000000" makes dockerd walk the whole json-log file
// for a container that has been running for months before a single line
// reaches the browser.
const maxLogTail = 10000

// defaultLogTail matches what the web UI asks for.
const defaultLogTail = 500

// wsConnWriter serializes writes to a *websocket.Conn. Gorilla's connection is
// not safe for concurrent writers, and this handler has two of them: the
// keepalive ping ticker and the log-forwarding goroutine. Without the mutex a
// ping landing mid-chunk panics the whole service with "concurrent write to
// websocket connection".
type wsConnWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsConnWriter) writeMessage(messageType int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return w.conn.WriteMessage(messageType, data)
}

// Write makes wsConnWriter an io.Writer so it can be the destination of
// stdcopy/io.Copy. Each call becomes one text frame.
func (w *wsConnWriter) Write(p []byte) (int, error) {
	if err := w.writeMessage(websocket.TextMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// WsContainerLogs streams a container's logs over a WebSocket (read-only).
//
// Route: GET /v1/container/:id/logs?tail=N
//
// This lives in app-management because app-management owns the "/v1/container"
// gateway prefix. The gateway routes on literal path prefixes, so no other
// service can be given "/v1/container/:id/logs": a copy of this handler in the
// main service — which is where it used to live — never received a single
// request and the endpoint answered 404 for every caller.
//
// The stream is read through the Docker API rather than by shelling out to
// `docker logs -f`. On a box that stays up for months, a subprocess per open
// log viewer is a leak waiting for one failed Kill; cancelling the request
// context closes the daemon connection deterministically.
func WsContainerLogs(ctx echo.Context) error {
	// Logs routinely echo secrets (startup banners, connection strings) — the
	// same class of data ContainerUpdateInfo/ToComposeYAML are gated to
	// protect. A read whose content can BE a credential belongs at that
	// same write-ish bit, not at app-view (which guest holds): "can see the
	// app exists" is not "may read anything it ever printed." The route is
	// mounted behind strictJWT() so the ?token= the browser puts on the
	// WebSocket URL is actually validated — the group-level JWT middleware
	// skips tokenless loopback requests, and every gateway-proxied request
	// looks exactly like one.
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	containerID := ctx.Param("id")
	if !validContainerID.MatchString(containerID) || len(containerID) > 64 {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid container id",
		})
	}

	tail, tailNotice := parseLogTail(v2.DefaultQuery(ctx, "tail", strconv.Itoa(defaultLogTail)))

	cli, err := docker.SharedClient()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	// Inspect before upgrading: a missing container has to surface as a real
	// HTTP status, otherwise the browser sees a successful handshake followed
	// by an empty stream and cannot tell "no logs yet" from "no such
	// container". It also tells us whether the stream is multiplexed.
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 10*time.Second)
	info, err := cli.ContainerInspect(inspectCtx, containerID)
	cancelInspect()
	if err != nil {
		logger.Error("container logs: inspect failed", zap.String("id", containerID), zap.Error(err))
		return ctx.JSON(http.StatusNotFound, modelCommon.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	multiplexed := info.Config == nil || !info.Config.Tty

	wsConn, err := upgrader.Upgrade(ctx.Response().Writer, ctx.Request(), nil)
	if err != nil {
		// Upgrade already wrote an HTTP error response.
		logger.Error("container logs: websocket upgrade failed", zap.Error(err))
		return nil
	}
	defer wsConn.Close()

	// Bound what a client can push at us. This socket is write-only from our
	// side; the only inbound traffic is pongs and the occasional client frame.
	wsConn.SetReadLimit(64 * 1024)
	_ = wsConn.SetReadDeadline(time.Now().Add(wsPongWait))
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	w := &wsConnWriter{conn: wsConn}
	if tailNotice != "" {
		// Say so rather than silently serving a different backlog than asked.
		_ = w.writeMessage(websocket.TextMessage, []byte(tailNotice+"\r\n"))
	}

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()

	body, err := cli.ContainerLogs(streamCtx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		logger.Error("container logs: stream open failed", zap.String("id", containerID), zap.Error(err))
		_ = w.writeMessage(websocket.TextMessage, []byte("failed to open log stream: "+err.Error()+"\r\n"))
		return nil
	}
	defer body.Close()

	// Buffered so whichever goroutine finishes first never blocks on a
	// receiver that has already gone away.
	done := make(chan struct{}, 3)

	// Keepalive: browsers and reverse proxies drop idle WebSockets, and a
	// quiet container can be idle for hours.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("container logs: ping goroutine panic", zap.Any("panic", r))
			}
			done <- struct{}{}
		}()
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := w.writeMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-streamCtx.Done():
				return
			}
		}
	}()

	// Docker → WebSocket.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("container logs: forward goroutine panic", zap.Any("panic", r))
			}
			done <- struct{}{}
		}()
		var copyErr error
		if multiplexed {
			// Without a TTY the daemon frames stdout and stderr with an 8-byte
			// header; forwarding the raw stream would show that binary header
			// as garbage in front of every line.
			_, copyErr = stdcopy.StdCopy(w, w, body)
		} else {
			_, copyErr = io.Copy(w, body)
		}
		if copyErr != nil && copyErr != io.EOF && streamCtx.Err() == nil {
			logger.Error("container logs: stream ended", zap.String("id", containerID), zap.Error(copyErr))
		}
	}()

	// Detect client disconnect. Nothing here reads meaningful data — the read
	// loop exists so a closed browser tab unblocks everything else instead of
	// leaving a follow stream attached to the daemon forever.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			if _, _, err := wsConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	<-done

	// Tear down in the order that actually unblocks the others: cancel the
	// daemon request (releases the copy goroutine), then close the socket
	// (releases the reader goroutine).
	cancelStream()
	_ = body.Close()
	_ = wsConn.Close()

	return nil
}

// parseLogTail validates the ?tail= parameter. It returns the value to use and,
// when the request was adjusted, a human-readable notice to send to the client
// so a clamped or rejected value is never silently different from what was
// asked for.
func parseLogTail(raw string) (int, string) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultLogTail, "requested tail is not a number; showing the last " +
			strconv.Itoa(defaultLogTail) + " lines"
	}
	if n < 0 {
		return defaultLogTail, "requested tail is negative; showing the last " +
			strconv.Itoa(defaultLogTail) + " lines"
	}
	if n > maxLogTail {
		return maxLogTail, "requested tail exceeds the " + strconv.Itoa(maxLogTail) +
			"-line limit; showing the last " + strconv.Itoa(maxLogTail) + " lines"
	}
	return n, ""
}
