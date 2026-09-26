package downloadHelper

import (
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"
	"gotest.tools/v3/assert"
)

// reachable reports whether a TCP connection to host:443 can be made. Same
// dial-based check as service/appstore_management_test.go's helper of the
// same name (different package, can't share the unexported test helper) --
// a dial exercises the same resolver and egress path Download uses, so it
// reports a missing network as a missing network instead of blaming the
// product for what a DNS/proxy-less sandbox will always fail.
func reachable(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestDownload(t *testing.T) {
	src := "https://github.com/IceWhaleTech/get/archive/refs/heads/main.zip"

	if !reachable("github.com") {
		t.Skipf("skipping: github.com is not reachable from this machine, so nothing here " +
			"can be exercised. This is a missing network, not a failing product.")
	}

	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	dst := t.TempDir()

	err := Download(src, dst)
	assert.NilError(t, err)
}
