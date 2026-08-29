package route

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/common"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/route/adapter/in"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/route/adapter/out"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (r *APIRoute) GetEventTypes(ctx echo.Context) error {
	eventTypes, err := r.services.EventTypeService.GetEventTypes()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	results := make([]codegen.EventType, 0)

	for _, eventType := range eventTypes {
		results = append(results, out.EventTypeAdapter(eventType))
	}

	return ctx.JSON(http.StatusOK, results)
}

func (r *APIRoute) RegisterEventTypes(ctx echo.Context) error {
	var eventTypes []codegen.EventType
	if err := ctx.Bind(&eventTypes); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	for _, eventType := range eventTypes {
		_, err := r.services.EventTypeService.RegisterEventType(in.EventTypeAdapter(eventType))
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}
	}

	return ctx.JSON(http.StatusOK, codegen.ResponseOK{})
}

func (r *APIRoute) GetEventTypesBySourceID(ctx echo.Context, sourceID codegen.SourceID) error {
	results, err := r.services.EventTypeService.GetEventTypesBySourceID(sourceID)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return ctx.JSON(http.StatusOK, results)
}

func (r *APIRoute) GetEventType(ctx echo.Context, sourceID codegen.SourceID, name codegen.EventName) error {
	result, err := r.services.EventTypeService.GetEventType(sourceID, name)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	if result == nil {
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: utils.Ptr("not found")})
	}

	return ctx.JSON(http.StatusOK, result)
}

func (r *APIRoute) PublishEvent(ctx echo.Context, sourceID codegen.SourceID, name codegen.EventName) error {
	var properties map[string]string
	body, err := io.ReadAll(io.LimitReader(ctx.Request().Body, 1<<20)) // 1MB max
	if err != nil {

		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if err = json.Unmarshal(body, &properties); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	uuidStr := uuid.New().String()
	event := in.EventAdapter(codegen.Event{
		SourceID:   sourceID,
		Name:       name,
		Properties: properties,
		Timestamp:  utils.Ptr(time.Now()),
		Uuid:       &uuidStr,
	})

	r.services.SocketIOService.Publish(event)

	// B11: the old code ignored the outcome and always answered 200 OK.
	// A dead or saturated dispatcher was therefore invisible to every
	// producer on the box — the exact "green dashboard, no events"
	// failure mode. 503 tells the caller the truth: the WebSocket
	// dispatcher did NOT take this event.
	//
	// Note the asymmetry: the socket.io broadcast above already
	// happened and has its own delivery path, so a 503 here means
	// "not fully delivered", not "nothing happened". Reporting the
	// partial failure is still strictly better than claiming success —
	// producers log it, and the operator can correlate it with
	// /health's restart and drop counters.
	if err := r.services.EventServiceWS.Publish(event); err != nil {
		logger.Error("event not accepted by the bus dispatcher",
			zap.Error(err),
			zap.String("source_id", sourceID),
			zap.String("name", name))

		message := "event not accepted by the message bus: " + err.Error()
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, out.EventAdapter(event))
}

func (r *APIRoute) SubscribeEventWS(c echo.Context, sourceID codegen.SourceID, params codegen.SubscribeEventWSParams) error {
	var eventNames []string
	if params.Names != nil {
		for _, eventName := range *params.Names {
			eventType, err := r.services.EventTypeService.GetEventType(sourceID, eventName)
			if err != nil {
				message := err.Error()
				return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}

			if eventType == nil {
				return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: utils.Ptr(fmt.Sprintf("event type `%s` of source ID `%s` not found", eventName, sourceID))})
			}

			eventNames = append(eventNames, eventName)
		}
	} else {
		eventTypes, err := r.services.EventTypeService.GetEventTypesBySourceID(sourceID)
		if err != nil || len(eventTypes) == 0 {
			if err != nil {
				message := err.Error()
				return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}
			message := "event types not found"
			return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		for _, eventType := range eventTypes {
			eventNames = append(eventNames, eventType.Name)
		}
	}

	conn, _, _, err := ws.UpgradeHTTP(c.Request(), c.Response())
	if err != nil {
		message := err.Error()
		return c.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	channel, err := r.services.EventServiceWS.Subscribe(sourceID, eventNames)
	if err != nil {
		conn.Close() // need to close connection here, instead of defer, because of the goroutine
		logger.Error("error when subscribing to event via websocket", zap.Error(err), zap.String("source_id", sourceID))
		return nil // connection already upgraded — can't send JSON
	}

	// Wave 33-A MEDIUM: previously the goroutine had no read pump, no
	// write deadlines, and trusted the kernel to eventually surface a
	// dead peer. In practice a half-open TCP connection (peer's network
	// disappeared, no FIN) would pin this goroutine forever — the
	// channel kept buffering events and `wsutil.WriteServerText` would
	// block on a TCP write that never got an ACK. The Subscribe channel
	// is also unbounded on the dispatch side, so a slow consumer could
	// pile up unlimited events in memory until OOM. Fix: install a hard
	// write deadline on every frame (so a stuck send returns), and run a
	// parallel read pump that detects peer EOF and force-closes the
	// connection (so the write goroutine sees ErrClosed and exits).
	go func(conn net.Conn, channel chan model.Event, eventNames []string) {
		defer conn.Close()
		// NOTE: do NOT close(channel) here — the dispatch loop may still
		// hold a snapshot reference and sending on a closed channel panics.
		// After Unsubscribe, no more sends target this channel; GC reclaims it.
		defer func(eventNames []string) {
			for _, name := range eventNames {
				if err := r.services.EventServiceWS.Unsubscribe(sourceID, name, channel); err != nil {
					logger.Error("error when trying to unsubscribe an event type via websocket", zap.Error(err), zap.String("source_id", sourceID), zap.String("name", name))
				}
			}
		}(eventNames)

		logger.Info("a websocket connection has started for events", zap.String("remote_addr", conn.RemoteAddr().String()))

		// Read pump: drain inbound frames so we notice peer EOF/close.
		// On any error we close the conn, which unblocks the write loop.
		go func() {
			for {
				if _, _, err := wsutil.ReadClientData(conn); err != nil {
					_ = conn.Close()
					return
				}
			}
		}()

		const writeDeadline = 10 * time.Second

		for {
			event, ok := <-channel
			if !ok {
				logger.Info("websocket channel for events is closed")
				return
			}

			if err := conn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
				logger.Info("websocket set write deadline failed", zap.Error(err))
				return
			}

			if event.SourceID == common.MessageBusSourceID && event.Name == common.MessageBusHeartbeatName {
				if err := wsutil.WriteServerMessage(conn, ws.OpPing, []byte{}); err != nil {
					logger.Error("error when trying to send ping message via websocket", zap.Error(err))
					return
				}
				continue
			}

			message, err := json.Marshal(out.EventAdapter(event))
			if err != nil {
				logger.Error("error when trying to marshal event for websocket", zap.Error(err))
				continue
			}

			logger.Info("sending event via websocket", zap.String("remote_addr", conn.RemoteAddr().String()), zap.String("message", string(message)))

			if err := wsutil.WriteServerText(conn, message); err != nil {
				if _, ok := err.(*net.OpError); ok {
					logger.Info("websocket connection ended", zap.String("error", err.Error()))
				} else {
					logger.Error("error when sending event via websocket", zap.String("error", err.Error()))
				}
				return
			}
		}
	}(conn, channel, eventNames)

	return nil
}
