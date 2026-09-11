package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-CLI/codegen/app_management"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
)

// THE DIRECT CHANNEL, AND WHY THE PEER SECRET MAY ONLY TRAVEL ON IT.
//
// The three read-only diagnostics that AppManagement lets a local peer call
// without a JWT (GetGlobalSettings, MyComposeApp, ComposeAppLogs — see
// CasaNova-AppManagement/route/v2/rbac_gate.go) are reached by talking to the
// service's OWN address, the one it publishes in app-management.url, exactly
// as every other peer service does. They do NOT go through the gateway.
//
// This is not a workaround for the gateway stripping the header. The gateway's
// Director deletes X-CasaNova-Internal at the public proxy boundary, and that
// strip is CORRECT: the secret is the authority of a LOCAL PEER, not a network
// credential. A caller arriving over the gateway is by definition a network
// client, which is precisely who must never be able to present it. The strip is
// the boundary telling the truth, so the CLI stops pretending to be a network
// client for these three calls instead of trying to smuggle the header past it.
//
// The property this buys is STRONGER than the one the CLI had before: the peer
// secret now works ONLY on the direct channel and is never even attached to a
// request aimed at the gateway. See cliGatewayHTTPClient for the other side.
//
// A var, not a const, so tests can point it at a fixture directory.
var cliRuntimePath = constants.DefaultRuntimePath

// appManagementURLFilename mirrors external.AppManageURLFilename. It is spelled
// out here rather than imported because CasaNova-Common/external pulls gjson and
// the jwt package into the CLI's module graph, and neither is in this module's
// go.sum — importing it for one string would mean editing go.mod/go.sum, which a
// regression fix on a frozen line has no business doing. Kept next to the
// canonical name so a rename shows up in the same grep.
const appManagementURLFilename = "app-management.url"

// cliAppManagementDirectURL returns the base URL AppManagement published for
// itself. It DELIBERATELY HAS NO FALLBACK TO THE GATEWAY.
//
// Falling back would send the request down the proxied path, where the header
// is stripped and the service answers 401 — the exact failure this indirection
// exists to remove, except now wearing the face of robustness and impossible to
// diagnose from the error. A missing or empty address file means the service is
// not running or has not finished publishing its address, and saying so is the
// useful answer.
func cliAppManagementDirectURL() (string, error) {
	addressFile := filepath.Join(cliRuntimePath, appManagementURLFilename)

	buf, err := os.ReadFile(addressFile)
	if err != nil {
		return "", fmt.Errorf("cannot read the app-management address file %s: %w "+
			"(is the app-management service running?)", addressFile, err)
	}

	// An empty file is a real state: the address is written on startup, and a
	// crash mid-write used to leave a zero-byte file behind. Treat it as "no
	// address" rather than building "http:///v2/..." and failing cryptically
	// deep inside net/http.
	address := strings.TrimSpace(string(buf))
	if address == "" {
		return "", fmt.Errorf("the app-management address file %s is empty "+
			"(the service may still be starting up)", addressFile)
	}

	return address, nil
}

// cliDirectAppManagementClient builds the ONLY client that carries the peer
// secret, and binds it to the direct address in the same step.
//
// The two halves are deliberately not separable: if the credential-bearing
// transport could be obtained on its own, someone would eventually hand it a
// gateway URL and the CLI would start offering peer authority over a network
// channel. Returning a client that is already pointed at the service makes that
// combination unrepresentable rather than merely discouraged.
func cliDirectAppManagementClient() (*app_management.ClientWithResponses, error) {
	address, err := cliAppManagementDirectURL()
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(address, "/") + "/" + BasePathAppManagement

	return app_management.NewClientWithResponses(url, app_management.WithHTTPClient(cliDirectHTTPClient()))
}

// cliGatewayHTTPClient is for every command that legitimately talks to the
// gateway. It attaches NO peer credential.
//
// Not "because the gateway strips it anyway" — that is a fact about today's
// gateway, and a future version that stopped stripping would silently turn each
// of these commands into an offer of peer authority over a network path. The
// credential is simply not attached to a request that is not going straight to
// the service.
//
// The root requirement is kept as it was. It is unrelated to this change, and
// relaxing who may run the gateway-routed commands is a separate decision.
func cliGatewayHTTPClient() *http.Client {
	return &http.Client{Transport: cliRootOnlyTransport{base: http.DefaultTransport}}
}

// cliRootOnlyTransport enforces the existing root requirement without carrying
// any credential.
type cliRootOnlyTransport struct{ base http.RoundTripper }

func (t cliRootOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if cliEUID() != 0 {
		return nil, fmt.Errorf("casaos-cli must be run as root (use sudo)")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
