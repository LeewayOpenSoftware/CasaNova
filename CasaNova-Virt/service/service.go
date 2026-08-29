package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
)

// MyService is the process-wide service registry, mirroring
// CasaNova-LocalStorage/service.MyService.
var MyService Services

// Services aggregates the domain, storage and host services plus the gateway
// management client used for route registration.
type Services interface {
	Domain() *DomainService
	Storage() *StorageService
	Host() *HostService
	Gateway() external.ManagementService
}

type store struct {
	domain  *DomainService
	storage *StorageService
	host    *HostService
	gateway external.ManagementService
}

// NewService wires the libvirt-backed services and the gateway client.
func NewService() Services {
	conn := libvirtconn.New(config.ServerInfo.LibvirtSocket)

	gateway, err := external.NewManagementService(config.CommonInfo.RuntimePath)
	if err != nil {
		panic(err)
	}

	return &store{
		domain:  NewDomainService(conn, config.ServerInfo.DiskDir),
		storage: NewStorageService(conn),
		host:    NewHostService(conn),
		gateway: gateway,
	}
}

func (s *store) Domain() *DomainService              { return s.domain }
func (s *store) Storage() *StorageService            { return s.storage }
func (s *store) Host() *HostService                  { return s.host }
func (s *store) Gateway() external.ManagementService { return s.gateway }
