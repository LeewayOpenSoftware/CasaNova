package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
)

// trustedPolicyDoc returns the bytes of a policy file an administrator would
// have written, carrying one restriction we can look for again afterwards.
func trustedPolicyDoc(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(routePolicyDoc{
		Version: routePolicyDocVersion,
		Routes:  map[string]persistedRoutePolicy{"/v1/files": {AllowedCIDRs: []string{"192.168.1.0/24"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A FRESH BOX AND A BOX WHOSE POLICY WE COULD NOT READ ARE DIFFERENT STATES,
// AND THE STORE HAS TO TELL THEM APART.
//
// Both end up with an empty acls map, which is why the enforcement path treats
// them identically: every route comes back undeclared and everyone is allowed.
// That is correct for the fresh box — nobody has decided anything yet — and it
// is the silent loss of an administrator's decision for the other one.
//
// The difference has to survive into what the store DOES, not just into what it
// logs. The observable consequence is the write: a store that never read the
// decision on disk must refuse to overwrite it, while a fresh box must be able
// to record its first policy normally. `unreadContent` is what carries that,
// and before this change it was set on ONE of the three degradation branches.
func TestAFreshBoxAndAnUnreadablePolicyFileAreNotTheSameState(t *testing.T) {
	t.Run("fresh box records its first decision", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "route-policy.json")

		store := newRoutePolicyStore(path)
		h := store.Health()
		if !h.OK {
			t.Fatalf("a box with no policy file is not degraded, it is undecided: %+v", h)
		}
		if h.UnreadContent {
			t.Fatalf("a box with no policy file has no unread content: %+v", h)
		}
		if err := store.Set("/v1/files", []string{"10.0.0.0/8"}); err != nil {
			t.Fatalf("a fresh box could not record its first policy: %v", err)
		}
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the first policy was not persisted: %v", err)
		}
		if !strings.Contains(string(written), "10.0.0.0/8") {
			t.Fatalf("the first policy is not in the file: %s", written)
		}
	})

	// Branch :197-198 of load() -- the bytes were read but are not parseable.
	// The file certainly exists: we read it. Overwriting it with the empty
	// in-memory set destroys a decision that a partial write or a typo may
	// still let the operator recover.
	t.Run("unparseable file is not overwritten", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "route-policy.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"routes":{"/v1/files":`), 0o600); err != nil {
			t.Fatal(err)
		}

		store := newRoutePolicyStore(path)
		h := store.Health()
		if h.OK {
			t.Fatal("an unparseable policy file was reported healthy")
		}
		if h.FailedInvariant != invRoutePolicyDurable {
			t.Fatalf("health does not name the failed invariant: %+v", h)
		}
		if !h.UnreadContent {
			t.Fatalf("health does not report that a policy file exists unread, so an operator "+
				"cannot tell this from a fresh box: %+v", h)
		}

		if err := store.Set("/v1/files", []string{"10.0.0.0/8"}); err == nil {
			t.Fatal("a write overwrote a policy file whose contents could not be parsed")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the policy file was removed: %v", err)
		}
		if !strings.Contains(string(after), "/v1/files") {
			t.Fatalf("the administrator's decision was destroyed; file now reads: %s", after)
		}
	})

	// Branch :188 of load() -- the file is ours and not writable by others, so
	// it passes the trust check, but it cannot be read. Trust deliberately
	// ignores read bits ("readability is not a property we depend on"), which
	// is exactly how a mode-0000 file reaches ReadFile and fails there.
	t.Run("unreadable file is not overwritten", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-0000 file, so this branch is unreachable as root")
		}
		path := filepath.Join(t.TempDir(), "route-policy.json")
		if err := os.WriteFile(path, trustedPolicyDoc(t), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		store := newRoutePolicyStore(path)
		h := store.Health()
		if h.OK {
			t.Fatal("a policy file that could not be read was reported healthy")
		}
		if !h.UnreadContent {
			t.Fatalf("health does not report that a policy file exists unread, so an operator "+
				"cannot tell this from a fresh box: %+v", h)
		}

		if err := store.Set("/v1/files", []string{"10.0.0.0/8"}); err == nil {
			t.Fatal("a write overwrote a policy file whose contents could not be read")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the policy file was removed: %v", err)
		}
		if !strings.Contains(string(after), "192.168.1.0/24") {
			t.Fatalf("the administrator's decision was destroyed; file now reads: %s", after)
		}
	})
}

// THE BOX MUST NOT FALL OFF THE NETWORK BECAUSE ITS POLICY FILE BROKE.
//
// Refusing to write over unread content is a refusal inside Set(). Peer
// services re-register their routes constantly (the registration watchdog),
// and CreateRoute reaches Set() only when the payload actually says something
// about authorization -- availabilityRoute() in CasaNova-Common leaves
// AllowedCIDRs nil precisely so a liveness loop cannot talk about policy.
//
// This test pins that: with a store that is degraded AND holding unread
// content, a peer's availability re-registration still succeeds. If a future
// change routes registration through the policy write path, this goes red
// rather than bricking route registration on every box whose policy file has
// the wrong owner.
func TestPeerRouteRegistrationSurvivesAnUnreadablePolicyFile(t *testing.T) {
	policyPath := isolatePolicyFile(t)
	if err := os.WriteFile(policyPath, []byte(`{"version":1,"routes":{`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtimeDir := t.TempDir()
	m := newTestManagement(t, runtimeDir)

	if h := m.routePolicy.Health(); h.OK {
		t.Fatalf("precondition: the store should be degraded, got %+v", h)
	}

	if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatalf("a peer could not register its route while the policy file was unreadable: %v", err)
	}
	if m.GetProxy("/v1/files") == nil {
		t.Fatal("the route was not registered, so the box just lost that service")
	}

	// And the availability payload must still not have touched policy.
	var reg *model.Route
	for _, r := range m.GetRoutes() {
		if r.Path == "/v1/files" {
			reg = r
		}
	}
	if reg == nil {
		t.Fatal("the registered route is not in GetRoutes")
	}
	if reg.AllowedCIDRs != nil {
		t.Fatalf("an availability re-registration authored policy: %+v", reg)
	}
}
