package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/message_bus"
	"gorm.io/gorm"
)

var MyService Repository

type Repository interface {
	Gateway() external.ManagementService
	User() UserService
	MessageBus() *message_bus.ClientWithResponses
	Event() EventService
}

func NewService(db *gorm.DB, RuntimePath string) Repository {

	gatewayManagement, err := external.NewManagementService(RuntimePath)
	if err != nil {
		panic(err)
	}

	return &store{
		gateway: gatewayManagement,
		user:    NewUserService(db),
		event:   NewEventService(db),
	}
}

type store struct {
	gateway external.ManagementService
	user    UserService
	event   EventService
}

func (c *store) Event() EventService {
	return c.event
}
func (c *store) Gateway() external.ManagementService {
	return c.gateway
}

func (c *store) User() UserService {
	return c.user
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
