package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-CLI/codegen/app_management"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
)

const fakePeerSecret = "test-peer-secret"

// stubPeerCredential makes the CLI believe it is root and holds the peer
// secret, without touching the real root-only file.
func stubPeerCredential(t *testing.T) {
	t.Helper()
	oldEUID, oldHeader := cliEUID, cliInternalHeader
	t.Cleanup(func() { cliEUID, cliInternalHeader = oldEUID, oldHeader })
	cliEUID = func() int { return 0 }
	cliInternalHeader = func() (map[string]string, error) {
		return map[string]string{internalauth.HeaderName: fakePeerSecret}, nil
	}
}

// captureHeader stands in for a service and records what arrived.
func captureHeader(t *testing.T, got *string, seen *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get(internalauth.HeaderName)
		*seen = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// THE PROPERTY, IN TWO ARMS: the peer secret travels on the direct channel and
// is NOT ATTACHED to a request aimed at the gateway.
//
// The second arm is the one that matters and it is NOT satisfied by "the
// gateway strips it anyway". That is a fact about today's gateway; a future
// version that stopped stripping would silently turn every gateway-routed CLI
// command into an offer of local-peer authority over a network path. The
// credential must never be put on the wire in that direction in the first
// place, which is a property of the CLI and is what this test pins.

func TestPeerSecretTravelsOnTheDirectChannel(t *testing.T) {
	stubPeerCredential(t)

	var got string
	var seen bool
	service := captureHeader(t, &got, &seen)

	// The service publishes its own address; the CLI reads it, exactly as a
	// peer service does.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, appManagementURLFilename), []byte(service.URL+"\n"), 0o644); err != nil {
		t.Fatalf("could not write the address fixture: %v", err)
	}
	oldPath := cliRuntimePath
	t.Cleanup(func() { cliRuntimePath = oldPath })
	cliRuntimePath = dir

	client, err := cliDirectAppManagementClient()
	if err != nil {
		t.Fatalf("could not build the direct client: %v", err)
	}
	if _, err := client.GetGlobalSettingsWithResponse(context.Background()); err != nil {
		t.Fatalf("direct call failed: %v", err)
	}

	if !seen {
		t.Fatal("the service was never reached; the direct channel is not wired up")
	}
	if got != fakePeerSecret {
		t.Fatalf("%s = %q on the direct channel, want the peer secret. Without it "+
			"AppManagement has no JWT and no peer proof, which is the deployed 401",
			internalauth.HeaderName, got)
	}
}

func TestPeerSecretIsNeverAttachedTowardTheGateway(t *testing.T) {
	stubPeerCredential(t)

	var got string
	var seen bool
	// Stands in for the gateway. It does NOT strip anything: the point is that
	// nothing arrives to be stripped.
	gateway := captureHeader(t, &got, &seen)

	client, err := app_management.NewClientWithResponses(
		gateway.URL+"/"+BasePathAppManagement,
		app_management.WithHTTPClient(cliGatewayHTTPClient()),
	)
	if err != nil {
		t.Fatalf("could not build the gateway client: %v", err)
	}
	if _, err := client.GetGlobalSettingsWithResponse(context.Background()); err != nil {
		t.Fatalf("gateway call failed: %v", err)
	}

	if !seen {
		t.Fatal("the stand-in gateway was never reached; this arm proved nothing")
	}
	if got != "" {
		t.Fatalf("%s = %q was sent toward the gateway. The peer secret is the "+
			"authority of a LOCAL PEER, and a request crossing the public proxy is by "+
			"definition a network client — exactly who must never be able to present "+
			"it. That today's Director deletes it is not a defence: a version that "+
			"stopped deleting would make this an offer of peer authority over the "+
			"network", internalauth.HeaderName, got)
	}
}

// The direct resolver must FAIL rather than fall back to the gateway. A silent
// fallback would send the request down the proxied path, get the header
// stripped, and return the same 401 this change exists to remove — with nothing
// in the error to explain why.
func TestDirectURLHasNoGatewayFallback(t *testing.T) {
	oldPath := cliRuntimePath
	t.Cleanup(func() { cliRuntimePath = oldPath })

	t.Run("missing address file", func(t *testing.T) {
		cliRuntimePath = t.TempDir() // exists, but holds no address file
		url, err := cliAppManagementDirectURL()
		if err == nil {
			t.Fatalf("resolved %q with no address file; a fallback here reintroduces "+
				"the 401 wearing the face of robustness", url)
		}
	})

	t.Run("empty address file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, appManagementURLFilename), []byte("  \n"), 0o644); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		cliRuntimePath = dir
		url, err := cliAppManagementDirectURL()
		if err == nil {
			t.Fatalf("resolved %q from a whitespace-only address file; that builds "+
				"\"http:///v2/...\" and fails cryptically inside net/http", url)
		}
	})
}

// Positive control for the resolver: a normal address file resolves, so the
// failure tests above are not passing because the resolver never works.
func TestDirectURLResolvesANormalAddressFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, appManagementURLFilename), []byte("http://127.0.0.1:41047\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	oldPath := cliRuntimePath
	t.Cleanup(func() { cliRuntimePath = oldPath })
	cliRuntimePath = dir

	got, err := cliAppManagementDirectURL()
	if err != nil {
		t.Fatalf("a normal address file did not resolve: %v", err)
	}
	if got != "http://127.0.0.1:41047" {
		t.Fatalf("resolved %q, want the published address with surrounding whitespace trimmed", got)
	}
}
