package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/port"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/systemctl"
)

type HealthService interface {
	Services() (map[bool]*[]string, error)
	Ports() ([]int, []int, error)
}

type service struct{}

func (s *service) Services() (map[bool]*[]string, error) {
	// The units were renamed casaos-* → casanova-* generations ago, but this
	// query kept the old pattern: it matched zero units, so the health panel
	// reported an empty service list on every production box — a monitor
	// that watched nothing and never said so. Both patterns are queried so a
	// half-migrated install still reports its leftovers.
	services, err := systemctl.ListServices("casanova*")
	if err != nil {
		return nil, err
	}
	if legacy, err := systemctl.ListServices("casaos*"); err == nil {
		services = append(services, legacy...)
	}

	var running, notRunning []string

	for _, service := range services {
		if service.Running {
			running = append(running, service.Name)
		} else {
			notRunning = append(notRunning, service.Name)
		}
	}

	result := map[bool]*[]string{
		true:  &running,
		false: &notRunning,
	}

	return result, nil
}

func (s *service) Ports() ([]int, []int, error) {
	return port.ListPortsInUse()
}

func NewHealthService() HealthService {
	return &service{}
}
