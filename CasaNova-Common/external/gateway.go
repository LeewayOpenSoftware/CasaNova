// The commmon package provides structs and functions for external code to interact with this gateway service.
package external

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	http2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
)

const (
	ManagementURLFilename = "management.url"
	StaticURLFilename     = "static.url"
	APIGatewayRoutes      = "/v1/gateway/routes"
	APIGatewayPort        = "/v1/gateway/port"
)

type ManagementService interface {
	CreateRoute(route *model.Route) error
	ChangePort(request *model.ChangePortRequest) error
	GetPort() (error, string)
}

type managementService struct {
	addressFile string
	address     string
	mu          sync.RWMutex
}

func (m *managementService) CreateRoute(route *model.Route) error {
	address, err := m.getAddress()
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(address, "/") + "/" + strings.TrimPrefix(APIGatewayRoutes, "/")
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}

	response, err := http2.Post(url, body, 30*time.Second)
	if err != nil {
		m.clearAddress(address)
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		return errors.New("failed to create route (status code: " + fmt.Sprint(response.StatusCode) + ")")
	}

	return nil
}

func (m *managementService) ChangePort(request *model.ChangePortRequest) error {
	address, err := m.getAddress()
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(address, "/") + "/" + strings.TrimPrefix(APIGatewayPort, "/")
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	response, err := http2.Put(url, body, 30*time.Second)
	if err != nil {
		m.clearAddress(address)
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to change port (status code: " + fmt.Sprint(response.StatusCode) + ")")
	}

	return nil
}

func (m *managementService) GetPort() (error, string) {
	address, err := m.getAddress()
	if err != nil {
		return err, ""
	}
	url := strings.TrimSuffix(address, "/") + "/" + strings.TrimPrefix(APIGatewayPort, "/")

	response, err := http2.Get(url, 30*time.Second)
	if err != nil {
		m.clearAddress(address)
		return err, ""
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to change port (status code: " + fmt.Sprint(response.StatusCode) + ")"), ""
	}
	str, err := io.ReadAll(response.Body)
	if err != nil {
		return err, ""
	}
	return nil, string(str)
}

func (m *managementService) getAddress() (string, error) {
	m.mu.RLock()
	if m.address != "" {
		address := m.address
		m.mu.RUnlock()
		return address, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.address != "" {
		return m.address, nil
	}

	address, err := getAddress(m.addressFile)
	if err != nil {
		return "", err
	}
	m.address = address
	return address, nil
}

func (m *managementService) clearAddress(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.address == address {
		m.address = ""
	}
}

func NewManagementService(RuntimePath string) (ManagementService, error) {
	return &managementService{
		addressFile: filepath.Join(RuntimePath, ManagementURLFilename),
	}, nil
}
