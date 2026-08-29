/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-12 09:48:56
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-09-02 22:10:05
 * @FilePath: /CasaOS/service/service.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"sync"
	"sync/atomic"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova/codegen/message_bus"
	"github.com/gorilla/websocket"
	"github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

var Cache *cache.Cache

var (
	MyService Repository
)

const MaxWebSocketConns = 100

var (
	WebSocketMu    sync.Mutex
	WebSocketConns []*websocket.Conn
	SocketRun      atomic.Bool
)

type Repository interface {
	Casa() CasaService
	Connections() ConnectionsService
	Gateway() external.ManagementService
	Health() HealthService
	Notify() NotifyServer
	Rely() RelyService
	Shares() SharesService
	System() SystemService
	Storage() StorageService
	MessageBus() *message_bus.ClientWithResponses
	Peer() PeerService
	Other() OtherService
}

func NewService(db *gorm.DB, RuntimePath string) Repository {
	gatewayManagement, err := external.NewManagementService(RuntimePath)
	if err != nil && len(RuntimePath) > 0 {
		panic(err)
	}

	return &store{
		casa:        NewCasaService(),
		connections: NewConnectionsService(db),
		gateway:     gatewayManagement,
		notify:      NewNotifyService(db),
		rely:        NewRelyService(db),
		system:      NewSystemService(),
		health:      NewHealthService(),
		shares:      NewSharesService(db),
		storage:     NewStorageService(),
		other:       NewOtherService(),

		peer: NewPeerService(db),
	}
}

type store struct {
	peer        PeerService
	db          *gorm.DB
	casa        CasaService
	notify      NotifyServer
	rely        RelyService
	system      SystemService
	shares      SharesService
	connections ConnectionsService
	gateway     external.ManagementService
	storage     StorageService
	health      HealthService
	other       OtherService
}

func (c *store) Storage() StorageService {
	return c.storage
}

func (c *store) Peer() PeerService {
	return c.peer
}

func (c *store) Other() OtherService {
	return c.other
}

func (c *store) Gateway() external.ManagementService {
	return c.gateway
}

func (s *store) Connections() ConnectionsService {
	return s.connections
}

func (s *store) Shares() SharesService {
	return s.shares
}

func (c *store) Rely() RelyService {
	return c.rely
}

func (c *store) System() SystemService {
	return c.system
}

func (c *store) Notify() NotifyServer {
	return c.notify
}

func (c *store) Casa() CasaService {
	return c.casa
}

func (c *store) Health() HealthService {
	return c.health
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
