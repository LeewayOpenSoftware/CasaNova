package v2

import (
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/labstack/echo/v4"
)

// THE PROPERTY: a query parameter may become an event property ONLY if it is
// DECLARED. Anything else stays out of the payload.
//
// PropertiesFromQueryParams used to copy EVERY incoming query parameter into
// the map that becomes the bus event's payload, which made any HTTP client the
// author of arbitrary fields on an internal event. Two things make that worse
// than it first looks:
//
//   - PublishEventWrapper merges the context properties OVER the publisher's
//     own map (service/service.go: `properties[k] = v` runs after the explicit
//     properties are in place), so a caller-supplied key does not merely get
//     added — it OVERWRITES a value the service set. `?message=...` rewrote the
//     error text of an error event.
//   - the bus does not filter either: PublishEvent in CasaNova-MessageBus takes
//     the properties verbatim and relays them to every subscriber.
//
// So the declared set is deliberately NARROW. It is NOT "every declared
// property type": app:name, app:icon and message are declared property types,
// but they are set BY THE SERVICE and must never be author-able by a client.
// Only the query parameters genuinely meant to ride along belong here.

// qs builds a one-parameter query string, escaping the key so names containing
// ':' (app:name) survive the round trip.
func qs(key, value string) string {
	return neturl.Values{key: []string{value}}.Encode()
}

func propsFor(t *testing.T, rawQuery string) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/app_management/anything?"+rawQuery, nil)
	ctx := echo.New().NewContext(req, httptest.NewRecorder())
	return PropertiesFromQueryParams(ctx)
}

// THE PROPERTY. Undeclared keys must not reach the payload — including keys
// that impersonate a declared, service-owned property.
func TestUndeclaredQueryParamsNeverEnterTheEventPayload(t *testing.T) {
	undeclared := []string{
		"evil",                 // an invented field
		"app:name",             // declared, but SERVICE-owned: spoofs the app identity
		"app:icon",             // ditto: would point the UI at an attacker's image
		"message",              // ditto: overwrites the error text of an error event
		"docker:container:id",  // ditto
		"force",                // a real query param of these routes, not an event property
		"url",                  // ditto
		"container_ids",        // ditto
		"delete_config_folder", // ditto
	}
	for _, key := range undeclared {
		t.Run(key, func(t *testing.T) {
			props := propsFor(t, qs(key, "attacker-value"))
			if got, ok := props[key]; ok {
				t.Fatalf("query parameter %q reached the event payload as %q. Any HTTP "+
					"client can therefore author that field on an internal bus event, and "+
					"because PublishEventWrapper merges context properties OVER the "+
					"publisher's own map it does not just add the field — it overwrites "+
					"what the service set", key, got)
			}
		})
	}
}

// POSITIVE CONTROL. Without this, a function that returns an empty map always
// passes the test above, which would be an outage rather than a fix: these two
// parameters are the reason the merge exists at all.
func TestDeclaredQueryParamsStillReachTheEventPayload(t *testing.T) {
	props := propsFor(t, "dry_run=true&check_port_conflict=false")

	if got := props[common.PropertyTypeDryRun.Name]; got != "true" {
		t.Fatalf("%s = %q, want \"true\" — this is a DECLARED event property and the "+
			"install flow reports it to subscribers", common.PropertyTypeDryRun.Name, got)
	}
	if got := props[common.PropertyTypeCheckPortConflict.Name]; got != "false" {
		t.Fatalf("%s = %q, want \"false\"", common.PropertyTypeCheckPortConflict.Name, got)
	}
}

// The declared keys keep their EXACT wire form — this is not a redesign of the
// bus. An explicitly empty value is a value, and was carried before, so it is
// still carried: the filter decides WHICH keys pass, never how they look.
func TestDeclaredKeysKeepTheirExactValue(t *testing.T) {
	props := propsFor(t, "dry_run=")
	got, ok := props[common.PropertyTypeDryRun.Name]
	if !ok {
		t.Fatalf("an explicitly-sent %s= was dropped; the filter must select keys, "+
			"not reinterpret their values", common.PropertyTypeDryRun.Name)
	}
	if got != "" {
		t.Fatalf("%s = %q, want the empty string it was sent as", common.PropertyTypeDryRun.Name, got)
	}
}

// A parameter that is simply absent must not be invented with a zero value:
// subscribers distinguish "not sent" from "sent as empty".
func TestAbsentDeclaredParamsAreNotInvented(t *testing.T) {
	props := propsFor(t, "dry_run=true")
	if _, ok := props[common.PropertyTypeCheckPortConflict.Name]; ok {
		t.Fatalf("%s was added to the payload although the request never sent it",
			common.PropertyTypeCheckPortConflict.Name)
	}
}

// The declared set must stay tied to the real PropertyType declarations. If
// someone re-spells a name as a string literal, the payload key silently stops
// matching what subscribers were told to expect — and widening this set is a
// security decision, not a refactor.
func TestDeclaredSetIsTiedToTheDeclaredPropertyTypes(t *testing.T) {
	if len(common.QueryParamEventProperties) == 0 {
		t.Fatal("the declared set is empty; the positive controls above would then be " +
			"passing for the wrong reason")
	}
	known := map[string]bool{
		common.PropertyTypeCheckPortConflict.Name: true,
		common.PropertyTypeDryRun.Name:            true,
	}
	for _, p := range common.QueryParamEventProperties {
		if p.Name == "" {
			t.Fatal("a declared query-param property has an empty name")
		}
		if !known[p.Name] {
			t.Fatalf("query-param property %q is not one of the service's declared "+
				"property types — widening this set is a security decision and must be "+
				"made deliberately, with the spoofing note at the top of this file "+
				"re-read first", p.Name)
		}
	}
}
