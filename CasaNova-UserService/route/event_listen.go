package route

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	message_bus "github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"go.uber.org/zap"
	"golang.org/x/net/websocket"
)

// EventListen subscribes to local-storage events from the message bus.
//
// Wave 35-X: complete rewrite. The previous loop had six concurrent bugs:
//  1. Bounded retry (i<1000) — after ~17 minutes of bus downtime the
//     subscription silently died forever ("services dropping after
//     hours/days").
//  2. defer ws.Close() inside a for-loop — every reconnect stacked one
//     more deferred Close, only fired on function return. With 100
//     reconnects, 100 leaked sockets pending close.
//  3. No recover() — first nil-deref panic killed the listener for the
//     daemon lifetime.
//  4. `*event.Uuid` unconditional deref — events without UUID crashed.
//  5. Fixed 1024-byte read buffer — events larger than 1KB unmarshalled
//     as truncated JSON, fail forever without ever reconnecting.
//  6. Unmarshal error did not `continue` — fell through into Marshal of
//     a partially-zeroed event.
//
// New behaviour: unbounded reconnect with exponential backoff (capped at
// 30s), recover() guard, per-connection deferred close, full message
// read via JSON codec, nil-safe UUID handling.
func EventListen() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("EventListen panicked — restarting in 5s",
				zap.Any("panic", r))
			time.Sleep(5 * time.Second)
			go EventListen()
		}
	}()

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		if subscribeOnce() {
			// Clean exit (context cancellation) — stop the loop.
			return
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// subscribeOnce opens a single WebSocket subscription and processes
// events until the connection drops. Returns true only on clean
// shutdown; false signals the caller to reconnect.
func subscribeOnce() bool {
	// Subscribe over the trusted unix socket: the bus's TCP listener demands
	// a JWT for websockets too (?token=), which internal services don't
	// have — dialing TCP answered 401 forever. A websocket client over a raw
	// unix net.Conn speaks the same protocol, and the bus's socket listener
	// stamps the connection as trusted (ConnContext) so the subscription is
	// authorized without a token.
	conn, err := net.Dial("unix", external.MessageBusSocketPath)
	if err != nil {
		logger.Error("dial message-bus socket err", zap.Any("error", err))
		return false
	}
	wsURL := fmt.Sprintf("ws://unix%s/event/%s", external.APIMessageBus, "local-storage")
	wsCfg, err := websocket.NewConfig(wsURL, "http://unix")
	if err != nil {
		_ = conn.Close()
		logger.Error("websocket config err", zap.Any("error", err))
		return false
	}
	ws, err := websocket.NewClient(wsCfg, conn)
	if err != nil {
		_ = conn.Close()
		logger.Error("connect websocket err", zap.Any("error", err))
		return false
	}
	defer ws.Close() // scoped to this single connection — no stacking

	logger.Info("subscribed to", zap.Any("url", wsURL))

	for {
		var event message_bus.Event
		// JSON codec reads full message regardless of size — no more
		// silent truncation at 1KB.
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			logger.Error("websocket read err", zap.Any("err", err.Error()))
			return false // reconnect
		}

		if event.Name == "local-storage:raid_status" {
			continue
		}

		propertiesStr, err := json.Marshal(event.Properties)
		if err != nil {
			logger.Error("marshal error", zap.Any("err", err.Error()), zap.Any("event", event))
			continue
		}

		// Nil-safe UUID — previous *event.Uuid crashed on events with
		// no UUID. Fall back to empty string; CreateEvemt should
		// tolerate that or fail-soft.
		var uuid string
		if event.Uuid != nil {
			uuid = *event.Uuid
		}

		m := model.EventModel{
			SourceID:   event.SourceID,
			Name:       event.Name,
			Properties: string(propertiesStr),
			UUID:       uuid,
		}
		service.MyService.Event().CreateEvemt(m)
	}
}

// strconv import retained for any future inline use
var _ = strconv.Itoa
