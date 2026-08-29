package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/pci"
)

// MyService is the global service container, mirroring CasaNova-LocalStorage.
var MyService Services

// Services aggregates the driver-service capabilities plus the gateway client
// used to register routes with the CasaNova gateway (:85) over management.url.
type Services interface {
	Gateway() external.ManagementService
	Device() DeviceService
	Module() ModuleService
	GPU() GPUService
	Driver() DriverService
	Tuning() TuningService
	Sensors() SensorService
}

type store struct {
	gateway external.ManagementService
	device  DeviceService
	module  ModuleService
	gpu     GPUService
	driver  DriverService
	tuning  TuningService
	sensors SensorService
}

// NewService builds the container. The gateway client resolves the management
// URL from RuntimePath/management.url, exactly like every other CasaNova service.
func NewService() Services {
	gatewayManagement, err := external.NewManagementService(config.CommonInfo.RuntimePath)
	if err != nil {
		panic(err)
	}

	idDB := pci.NewDB(config.ServerInfo.PCIIDsPath)

	return &store{
		gateway: gatewayManagement,
		device:  NewDeviceService(idDB),
		module:  NewModuleService(),
		gpu:     NewGPUService(idDB),
		driver:  NewDriverService(),
		tuning:  NewTuningService(),
		sensors: NewSensorService(),
	}
}

func (s *store) Gateway() external.ManagementService { return s.gateway }
func (s *store) Device() DeviceService               { return s.device }
func (s *store) Module() ModuleService               { return s.module }
func (s *store) GPU() GPUService                     { return s.gpu }
func (s *store) Driver() DriverService               { return s.driver }
func (s *store) Tuning() TuningService               { return s.tuning }
func (s *store) Sensors() SensorService              { return s.sensors }
