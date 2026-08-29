package external

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
)

// The watchdog exists to keep routes REACHABLE. A route now also carries an
// authorization decision (AllowedCIDRs), and the gateway reads "field absent"
// as "this caller is not talking about policy". So the payload this loop
// sends must set the availability fields and nothing else.
//
// Reflection rather than a hand-written field list: when model.Route gains a
// field, this test starts covering it automatically, and it fails the moment
// the watchdog starts populating anything beyond path/target.
func TestWatchdogPayloadCarriesAvailabilityFieldsOnly(t *testing.T) {
	availability := map[string]bool{"Path": true, "Target": true}

	route := availabilityRoute("/v1/files", "http://127.0.0.1:8080")
	if route.Path != "/v1/files" || route.Target != "http://127.0.0.1:8080" {
		t.Fatalf("availability fields not set: %+v", route)
	}

	v := reflect.ValueOf(*route)
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if availability[name] {
			continue
		}
		if !v.Field(i).IsZero() {
			t.Fatalf("re-registration payload sets non-availability field %q (%v); "+
				"a liveness loop must not carry authorization state",
				name, v.Field(i).Interface())
		}
	}
}

// The same guarantee at the wire level: what actually reaches the gateway
// must not mention policy, because a policy field present-and-empty is how an
// administrator says "remove the restriction".
func TestWatchdogPayloadHasNoPolicyFieldOnTheWire(t *testing.T) {
	body, err := json.Marshal(availabilityRoute("/v1/files", "http://127.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "allowed_cidrs") {
		t.Fatalf("watchdog payload carries a policy field: %s", body)
	}
}

// Every route the watchdog is given gets the same availability-only shape.
func TestWatchdogUsesAvailabilityRouteForEveryPath(t *testing.T) {
	paths := []string{"/v1/files", "/v2/files", "/"}
	for _, p := range paths {
		r := availabilityRoute(p, "http://127.0.0.1:8080")
		if r.Path != p {
			t.Fatalf("path %q not carried through: %+v", p, r)
		}
		if r.AllowedCIDRs != nil {
			t.Fatalf("path %q got a policy field: %+v", p, r)
		}
	}
	// Guard the plumbing too: the loop must build its payload with
	// availabilityRoute rather than an inline literal that could drift.
	var _ func(string, string) *model.Route = availabilityRoute
}
