package docker_test

import (
	"fmt"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
)

func TestGetDir(t *testing.T) {
	fmt.Println(docker.GetDir("", "config"))
}
