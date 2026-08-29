package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	v2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2/wrapper"
	"github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

var Cache *cache.Cache

var MyService Services

type Services interface {
	Disk() DiskService
	USB() USBService
	LocalStorage() *v2.LocalStorageService
	Gateway() external.ManagementService
	Notify() NotifyServer
	NotifySystem() external.NotifyService
	Shares() external.ShareService
	MessageBus() *message_bus.ClientWithResponses
	Storage() StorageService
}

func NewService(db *gorm.DB) Services {
	gatewayManagement, err := external.NewManagementService(config.CommonInfo.RuntimePath)
	if err != nil {
		panic(err)
	}

	notifySystem := external.NewNotifyService(config.CommonInfo.RuntimePath)
	sharesService := external.NewShareService(config.CommonInfo.RuntimePath)

	return &store{
		usb:          NewUSBService(),
		disk:         NewDiskService(db),
		localStorage: v2.NewLocalStorageService(db, wrapper.NewMountInfo()),
		gateway:      gatewayManagement,
		notify:       NewNotifyService(),
		notifySystem: notifySystem,
		shares:       sharesService,
		storage:      NewStorageService(),
	}
}

type store struct {
	usb          USBService
	disk         DiskService
	localStorage *v2.LocalStorageService
	gateway      external.ManagementService
	notify       NotifyServer
	notifySystem external.NotifyService
	shares       external.ShareService
	storage      StorageService
}

func (c *store) NotifySystem() external.NotifyService {
	return c.notifySystem
}

func (c *store) Storage() StorageService {
	return c.storage
}

func (c *store) Gateway() external.ManagementService {
	return c.gateway
}

func (c *store) USB() USBService {
	return c.usb
}

func (c *store) Disk() DiskService {
	return c.disk
}

func (c *store) LocalStorage() *v2.LocalStorageService {
	return c.localStorage
}

func (c *store) Notify() NotifyServer {
	return c.notify
}

func (c *store) Shares() external.ShareService {
	return c.shares
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
