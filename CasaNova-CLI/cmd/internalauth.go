package cmd

import (
	"fmt"
	"net/http"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
)

// These seams keep the privilege boundary testable without weakening it in
// production. The CLI is an operator tool: it may act as a local peer only
// when invoked as root, who can already read the root-only peer secret.
var (
	cliEUID           = os.Geteuid
	cliInternalHeader = internalauth.Header
)

type cliAuthTransport struct{ base http.RoundTripper }

func (t cliAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if cliEUID() != 0 {
		return nil, fmt.Errorf("casaos-cli must be run as root (use sudo); refusing to use the internal service credential")
	}
	headers, err := cliInternalHeader()
	if err != nil {
		return nil, fmt.Errorf("read internal service credential: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func cliDirectHTTPClient() *http.Client {
	return &http.Client{Transport: cliAuthTransport{base: http.DefaultTransport}}
}
