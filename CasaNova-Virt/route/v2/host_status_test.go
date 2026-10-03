package v2

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
)

// No libvirt socket on the host is "unavailable here", not an internal error.
func TestHostInfoStatus(t *testing.T) {
	noSock := fmt.Errorf("%w: tried /a, /b", libvirtconn.ErrNoSocket)
	if got := hostInfoStatus(noSock); got != http.StatusServiceUnavailable {
		t.Errorf("no socket -> %d, want 503", got)
	}
	if got := hostInfoStatus(errors.New("rpc broke")); got != http.StatusInternalServerError {
		t.Errorf("other failure -> %d, want 500", got)
	}
}
