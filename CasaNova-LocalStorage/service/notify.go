package service

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"go.uber.org/zap"
)

type NotifyServer interface {
	SendNotify(name string, message map[string]interface{}) error
}

type notifyServer struct {
}

func (i *notifyServer) SendNotify(name string, message map[string]interface{}) error {
	msg := make(map[string]string)
	for k, v := range message {
		bt, _ := json.Marshal(v)
		msg[k] = string(bt)
	}
	// Wave 35-X: bound the publish so a wedged message-bus cannot
	// freeze every disk-event publisher goroutine forever. Background()
	// + no deadline meant udev events queued up indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := MyService.MessageBus().PublishEventWithResponse(ctx, common.ServiceName, name, msg)
	if err != nil {
		logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", msg))
		return err
	}
	if response != nil && response.StatusCode() != http.StatusOK {
		logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
	}
	// SocketServer.BroadcastToRoom("/", "public", path, message)
	return nil
}

func NewNotifyService() NotifyServer {
	return &notifyServer{}
}
