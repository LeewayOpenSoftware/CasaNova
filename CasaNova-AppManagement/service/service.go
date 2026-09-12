/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2022-07-12 09:48:56
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-09-02 22:10:05
 *@FilePath: /CasaOS/service/service.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/applock"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	jsoniter "github.com/json-iterator/go"
	"go.uber.org/zap"
)

var (
	MyService Services

	json = jsoniter.ConfigCompatibleWithStandardLibrary

	// eventSemaphore limits the number of concurrent PublishEventWrapper goroutines
	// to prevent unbounded goroutine accumulation when many events fire at once.
	eventSemaphore = make(chan struct{}, 50)
)

type Services interface {
	AppStoreManagement() *AppStoreManagement
	AppLock() *applock.Manager

	// Git() *GitService
	Compose() *ComposeService
	Docker() DockerService
	Gateway() external.ManagementService
	Notify() external.NotifyService
	MessageBus() *message_bus.ClientWithResponses
}

func NewService(RuntimePath string) Services {
	gatewayManagement, err := external.NewManagementService(RuntimePath)
	if err != nil {
		panic(err)
	}

	return &store{
		gateway: gatewayManagement,
		notify:  external.NewNotifyService(RuntimePath),

		appStoreManagement: NewAppStoreManagement(),
		appLockManager:     applock.NewManager(),

		compose: NewComposeService(),
		docker:  NewDockerService(),
	}
}

type store struct {
	appStoreManagement *AppStoreManagement
	appLockManager     *applock.Manager

	// git     *GitService
	compose *ComposeService
	docker  DockerService
	gateway external.ManagementService
	notify  external.NotifyService
}

func (c *store) Gateway() external.ManagementService {
	return c.gateway
}

func (c *store) Notify() external.NotifyService {
	return c.notify
}

func (c *store) AppStoreManagement() *AppStoreManagement {
	return c.appStoreManagement
}

func (c *store) AppLock() *applock.Manager {
	return c.appLockManager
}

func (c *store) Compose() *ComposeService {
	return c.compose
}

func (c *store) Docker() DockerService {
	return c.docker
}

func (c *store) MessageBus() *message_bus.ClientWithResponses {
	// All service→bus traffic rides the trusted unix socket: the TCP
	// listener requires a JWT (loopback trust was removed in the auth
	// hardening) and services hold no user token, so the old address-file/
	// TCP path answered 401 to every register/publish. The socket transport
	// also drops the boot-order dependency on the message-bus.url file.
	client, _ := message_bus.NewClientWithResponses(
		external.MessageBusSocketServer,
		message_bus.WithHTTPClient(external.SocketHTTPClient()),
	)

	return client
}

func PublishEventWrapper(ctx context.Context, eventType message_bus.EventType, properties map[string]string) {
	// Limit concurrent event publishes to prevent goroutine leaks.
	select {
	case eventSemaphore <- struct{}{}:
		defer func() { <-eventSemaphore }()
	case <-ctx.Done():
		return
	}

	// Prevent goroutine accumulation: bound the total time this function can
	// spend on HTTP calls to 5 seconds regardless of the parent context.
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if MyService == nil {
		fmt.Println("failed to publish event - messsage bus service not initialized")
		return
	}

	if properties == nil {
		properties = map[string]string{}
	}

	// Merge with the properties from the context, over a SNAPSHOT: this ranges
	// on a goroutine of its own at almost every call site, while handlers write
	// into the context's map, and ranging a map someone else is writing is a
	// fatal the daemon cannot recover from.
	for k, v := range common.PropertiesSnapshot(timeoutCtx) {
		properties[k] = v
	}

	resp, err := external.PublishEventInSocket(timeoutCtx, eventType.SourceID, eventType.Name, properties)
	if err != nil {
		logger.Error("failed to publish event", zap.Error(err))

		response, err := MyService.MessageBus().PublishEventWithResponse(timeoutCtx, common.AppManagementServiceName, eventType.Name, properties)
		if err != nil {
			logger.Error("failed to publish event", zap.Error(err))
			return
		}
		if response == nil {
			logger.Error("failed to publish event", zap.String("status code", "empty response"))
			return
		}
		defer response.HTTPResponse.Body.Close()

		if response.StatusCode() != http.StatusOK {
			logger.Error("failed to publish event", zap.String("status code", response.Status()))
		}
	} else {
		if resp.StatusCode != http.StatusOK {
			logger.Error("failed to publish event", zap.String("status code", resp.Status))
		}
	}
}
