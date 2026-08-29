package modmanagement

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/codegen/mod_management"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/tidwall/gjson"
)

var (
	ErrNoDataInResponse = fmt.Errorf("no data in response")
	ErrModuleNoInStore  = fmt.Errorf("module not in store")
)

type ModManagementClient struct {
	Client *mod_management.ClientWithResponses
}

type ModManagementClientOpts struct {
	Port *int
}

func NewClient(opts ModManagementClientOpts) (*ModManagementClient, error) {
	port := 80
	if opts.Port != nil {
		port = *opts.Port
	}
	client, err := mod_management.NewClientWithResponses(fmt.Sprintf("http://localhost:%d/v2/mod_management", port))
	if err != nil {
		return nil, err
	}
	return &ModManagementClient{Client: client}, nil
}

func (c *ModManagementClient) InstalledModules() ([]mod_management.Module, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Client.ModuleListWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("failed to get installed modules: empty response")
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("failed to get installed modules: %s", resp.Status())
	}

	// Wave 35-X: oapi-codegen only fills JSON200 when the 200 body
	// decoded cleanly. A 200 with empty/garbage body leaves
	// JSON200=nil — previous code crashed with nil deref.
	if resp.JSON200 == nil || resp.JSON200.Data == nil {
		return []mod_management.Module{}, ErrNoDataInResponse
	}
	return *resp.JSON200.Data, nil
}

func (c *ModManagementClient) InstallableModules() ([]mod_management.RemoteModule, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Client.InstallableModuleListWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("failed to get installable modules: empty response")
	}

	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("failed to get installable modules: %s", resp.Status())
	}

	// Wave 35-X: see InstalledModules — JSON200 may be nil even on 200.
	if resp.JSON200 == nil || resp.JSON200.Data == nil {
		return []mod_management.RemoteModule{}, ErrNoDataInResponse
	}

	return *resp.JSON200.Data, nil
}

func (c *ModManagementClient) InstallModule(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Client.ModuleInstallWithResponse(ctx, mod_management.ModuleInstallJSONRequestBody{
		Name: name,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("failed to install module: empty response")
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("failed to install module: %s, %s", resp.Status(), string(resp.Body))
	}
	return nil
}
func (c *ModManagementClient) InstallModuleAsync(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Client.ModuleInstallAsyncWithResponse(ctx, mod_management.ModuleInstallJSONRequestBody{
		Name: name,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("failed to install module async: empty response")
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("failed to install module: %s, %s", resp.Status(), string(resp.Body))
	}
	return nil
}

func (c *ModManagementClient) UninstallModule(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Client.ModuleUninstallWithResponse(ctx, mod_management.ModuleId{
		Name: name,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("failed to uninstall module: empty response")
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("failed to uninstall module: %s", resp.Status())
	}
	return nil
}

func RequireModule(name string, runtimePath string) error {
	gatway, err := external.NewManagementService(runtimePath)
	if err != nil {
		return err
	}

	port := 80
	if err, portStr := gatway.GetPort(); err == nil {
		newPort := gjson.Get(portStr, "data").Int()
		if newPort != 0 && int(newPort) != port {
			port = int(newPort)
		}
	}

	client, err := NewClient(ModManagementClientOpts{
		Port: &port,
	})
	if err != nil {
		return err
	}

	modules, err := client.InstalledModules()
	if err != nil {
		return err
	}

	// Check if the module is already installed
	for _, module := range modules {
		if module.Name == nil {
			continue
		}
		if *module.Name == name {
			return nil
		}
	}

	// Install module
	err = client.InstallModule(name)
	if err != nil {
		if strings.Contains(err.Error(), "module not exist") {
			return ErrModuleNoInStore
		}
		return err
	}

	return nil
}
