//go:build integration

// These tests exercise the deprecated Mod Management SDK against a live
// third-party/store endpoint and are excluded from the default suite. The
// store now returns a ZimaOS envelope with data as an object containing
// `modules`, while the generated SDK model expects data to be an array of
// Module values; the store also requires authentication that this client
// does not send. Do not repair that model spec drift here: if the SDK is
// adopted again, update the contract and its client deliberately.
package modmanagement_test

import (
	"os"
	"testing"

	modmanagement "github.com/LeewayOpenSoftware/CasaNova-Common/pkg/mod_management"
	"github.com/stretchr/testify/assert"
)

func TestInstallableModules(t *testing.T) {
	// skip in GitHub Actions
	if os.Getenv("CI") != "" {
		t.Skip()
	}
	client, err := modmanagement.NewClient(modmanagement.ModManagementClientOpts{})
	assert.NoError(t, err)
	modules, err := client.InstallableModules()
	assert.NoError(t, err)

	t.Log(modules)
}

func TestInstallModule(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip()
	}
	err := modmanagement.RequireModule("doconverter", "/var/run/casaos")
	assert.NoError(t, err)
}

func TestInstallNoExistModule(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip()
	}
	err := modmanagement.RequireModule("abc", "/var/run/casaos")
	assert.ErrorIs(t, err, modmanagement.ErrModuleNoInStore)
}
