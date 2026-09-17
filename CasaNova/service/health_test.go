package service_test

import (
	"bufio"
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/stretchr/testify/assert"
)

// procNetHasEntries reports whether filename (a /proc/net/{tcp,udp}[6] file)
// lists at least one connection/listener beyond its header line. A sandboxed
// environment serves these files with only the header, which is not the same
// as "no ports in use" on a real host. Same declared precondition as the
// sibling TestPorts in CasaNova-Common/utils/port, whose ListPortsInUse is
// exactly what the health service delegates to.
func procNetHasEntries(filename string) bool {
	file, err := os.Open(filename)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return false // couldn't even read the header
	}
	return scanner.Scan() // at least one entry line after the header
}

func TestPorts(t *testing.T) {
	tcpAvailable := procNetHasEntries("/proc/net/tcp") || procNetHasEntries("/proc/net/tcp6")
	udpAvailable := procNetHasEntries("/proc/net/udp") || procNetHasEntries("/proc/net/udp6")
	if !tcpAvailable || !udpAvailable {
		t.Skip("environment reports no entries in /proc/net/{tcp,udp}[6] (common inside a sandbox); the health service's Ports() cannot be measured here")
	}

	healthService := service.NewHealthService()

	tcpPorts, udpPorts, err := healthService.Ports()
	assert.NoError(t, err)

	assert.NotEmpty(t, tcpPorts)
	assert.NotEmpty(t, udpPorts)
}
