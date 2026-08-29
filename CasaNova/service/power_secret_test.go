package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// The invariant: the shared UPS credential never leaves the daemon through a
// read endpoint.
//
// GET /v1/sys/host/power/config is gated on PermSystemRead, which the guest
// role holds — and a tokenless request resolves to guest — so the full config
// struct was served to anyone who could reach the box. On this deployment that
// is the public internet through the Cloudflare tunnel: the 64-character token
// came back to a plain curl with no header at all. Verified live before the fix.
//
// Mutation that must fail this test: have the read handler return Config()
// instead of RedactedConfig(), or drop the blanking in RedactedConfig.
func TestRedactedConfigNeverCarriesTheToken(t *testing.T) {
	// Same shape as a real Forza token (64 hex chars), deliberately fake:
	// this test once carried the production token as its fixture, which put
	// the real credential into every copy of the source. A redaction test
	// only needs a value with the right silhouette.
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	p := &PowerOrchestrator{}
	p.cfg = PowerConfig{ForzaURL: "http://192.168.3.214:9090", ForzaToken: secret}

	got := p.RedactedConfig()

	if got.ForzaToken != "" {
		t.Fatalf("RedactedConfig leaked the token: %q", got.ForzaToken)
	}
	if !got.ForzaTokenSet {
		t.Fatal("the UI can no longer tell that a token is configured")
	}

	// The wire form is what actually reaches the client, so assert on it: a
	// field added later that happens to carry the secret would slip past a
	// struct-field check but not past this one.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the serialized response still contains the token: %s", raw)
	}
}

// Because the read path now blanks the token, the settings form loads with an
// empty field. Saving it must not wipe the stored credential — otherwise the
// first save after this change silently disconnects the box from its UPS.
func TestSetConfigTreatsAnEmptyTokenAsUnchanged(t *testing.T) {
	const secret = "shared-forza-credential"

	p := &PowerOrchestrator{}
	p.cfg = PowerConfig{ForzaURL: "http://192.168.3.214:9090", ForzaToken: secret}

	// What the UI sends back after the user edits an unrelated field.
	incoming := p.RedactedConfig()
	incoming.IdleWatts = 42

	if err := p.SetConfig(incoming); err != nil {
		// Persisting may fail in a sandbox without /var/lib/casanova; the
		// in-memory decision is what this test is about.
		t.Logf("SetConfig persistence failed (expected off-box): %v", err)
	}

	if p.Config().ForzaToken != secret {
		t.Fatalf("saving the form wiped the token: %q", p.Config().ForzaToken)
	}
	if p.Config().IdleWatts != 42 {
		t.Fatal("the rest of the config was not applied")
	}
}

// A genuinely new token must still replace the old one, or the operator can
// never rotate the credential.
func TestSetConfigAcceptsANewToken(t *testing.T) {
	p := &PowerOrchestrator{}
	p.cfg = PowerConfig{ForzaToken: "old-token"}

	cfg := p.Config()
	cfg.ForzaToken = "rotated-token"
	if err := p.SetConfig(cfg); err != nil {
		t.Logf("SetConfig persistence failed (expected off-box): %v", err)
	}

	if p.Config().ForzaToken != "rotated-token" {
		t.Fatalf("token rotation was ignored: %q", p.Config().ForzaToken)
	}
}
