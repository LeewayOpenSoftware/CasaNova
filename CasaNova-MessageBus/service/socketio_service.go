package service

import (
	"context"
	"net/http"
	"net/url"

	socketio "github.com/CorrectRoadH/go-socket.io"
	"github.com/CorrectRoadH/go-socket.io/engineio"
	"github.com/CorrectRoadH/go-socket.io/engineio/transport"
	"github.com/CorrectRoadH/go-socket.io/engineio/transport/polling"
	"github.com/CorrectRoadH/go-socket.io/engineio/transport/websocket"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"go.uber.org/zap"
)

type SocketIOService struct {
	server *socketio.Server
}

func (s *SocketIOService) Publish(message interface{}) {
	if event, ok := message.(model.Event); ok {
		s.server.BroadcastToRoom("/", "event", event.Name, event)
		return
	}

	if action, ok := message.(model.Action); ok {
		s.server.BroadcastToRoom("/", "action", action.Name, action)
		return
	}

	logger.Error("unknown message type", zap.Any("message", message))
}

func (s *SocketIOService) Start(ctx *context.Context) {
	go func() {
		if err := s.server.Serve(); err != nil {
			logger.Error("error when serving socketio for events", zap.Error(err))
		}
	}()

	<-(*ctx).Done()
	logger.Info("socketio service shutting down due to context cancellation")
	if err := s.server.Close(); err != nil {
		logger.Error("error when closing socketio server", zap.Error(err))
	}
}

func (s *SocketIOService) Server() *socketio.Server {
	return s.server
}

func NewSocketIOService() *SocketIOService {
	return &SocketIOService{
		server: buildServer(),
	}
}

func buildServer() *socketio.Server {
	// Same-origin enforcement: a malicious page on a third-party origin
	// must NOT be able to drive a victim's browser to open a socket.io
	// connection here (CSWSH). Even though the message bus listens on
	// localhost, the gateway proxies it for in-browser subscribers, so
	// requests still arrive with browser Origin headers. Allow only:
	//   - no Origin header (non-browser client, e.g. internal Go svc)
	//   - Origin host equals the request Host (same-origin)
	originOK := func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}

	websocketTransport := websocket.Default
	websocketTransport.CheckOrigin = originOK

	pollingTransport := polling.Default
	pollingTransport.CheckOrigin = originOK

	server := socketio.NewServer(&engineio.Options{
		Transports: []transport.Transport{
			websocketTransport,
			pollingTransport,
		},
	})

	server.OnConnect("/", func(s socketio.Conn) error {
		// TODO add connector info. we need to know who is connecting
		s.SetContext("")
		logger.Info("a socketio connection has started", zap.Any("remote_addr", s.RemoteAddr()))

		s.Join("event")
		s.Join("action")

		return nil
	})

	server.OnError("/", func(s socketio.Conn, e error) {
		// TODO add connector info. we need to know who is disconnecting
		logger.Error("error in socketio connnection", zap.Any("error", e))
	})

	server.OnDisconnect("/", func(s socketio.Conn, reason string) {
		server.Remove(s.ID())
		// TODO add connector info. we need to know who is disconnecting
		logger.Info("a socketio connection is disconnected", zap.Any("reason", reason))
	})

	return server
}
