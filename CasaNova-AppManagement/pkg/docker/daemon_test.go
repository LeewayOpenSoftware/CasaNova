package docker_test

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"gotest.tools/v3/assert"
)

func TestCurrentArchitecture(t *testing.T) {
	if !docker.IsDaemonRunning() {
		t.Skip("Docker daemon is not running")
	}

	a, err := docker.CurrentArchitecture()
	assert.NilError(t, err)
	assert.Assert(t, a != "")
}
