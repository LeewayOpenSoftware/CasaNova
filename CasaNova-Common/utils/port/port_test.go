package port_test

import (
	"bufio"
	"fmt"
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/port"
	"github.com/stretchr/testify/assert"
)

// procNetHasEntries reports whether filename (a /proc/net/{tcp,udp}[6] file)
// lists at least one connection/listener beyond its header line. A sandboxed
// environment serves these files with only the header, which is not the same
// as "no ports in use" on a real host.
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

func TestPortAvailable(t *testing.T) {
	//	fmt.Println(PortAvailable())
	// fmt.Println(IsPortAvailable(6881,"tcp"))
	p, _ := port.GetAvailablePort("udp")
	fmt.Println("udp", p)
	fmt.Println(port.IsPortAvailable(p, "udp"))

	t1, _ := port.GetAvailablePort("tcp")
	fmt.Println("tcp", t1)
	fmt.Println(port.IsPortAvailable(t1, "tcp"))
}

func TestPorts(t *testing.T) {
	tcpAvailable := procNetHasEntries("/proc/net/tcp") || procNetHasEntries("/proc/net/tcp6")
	udpAvailable := procNetHasEntries("/proc/net/udp") || procNetHasEntries("/proc/net/udp6")
	if !tcpAvailable || !udpAvailable {
		t.Skip("environment reports no entries in /proc/net/{tcp,udp}[6] (common inside a sandbox); ListPortsInUse cannot be measured here")
	}

	tcpPorts, udpPorts, err := port.ListPortsInUse()
	assert.NoError(t, err)

	assert.NotEmpty(t, tcpPorts)
	assert.NotEmpty(t, udpPorts)
}
