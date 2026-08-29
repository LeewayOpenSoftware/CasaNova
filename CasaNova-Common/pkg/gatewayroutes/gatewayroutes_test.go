package gatewayroutes

import (
	"strings"
	"testing"
)

// clone deep-copies the manifest so a mutation test cannot leak into the
// other tests through the shared map.
func clone() map[Owner]Claim {
	out := make(map[Owner]Claim, len(manifest))
	for owner, claim := range manifest {
		c := Claim{
			Prefixes: append([]string(nil), claim.Prefixes...),
			Derived:  append([]string(nil), claim.Derived...),
		}
		out[owner] = c
	}
	return out
}

func TestManifestIsValid(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatalf("the shipped route manifest is invalid: %v", err)
	}
}

// TestDuplicateClaimIsRejected reproduces the defect measured on the box on
// 2026-08-11: casanova published "/v1/driver" (cloud storage providers) and
// casanova-driver published the same prefix for the hardware driver API. The
// gateway keeps one target per prefix and both services re-register every 30s,
// so /run/casaos/routes.json alternated between 127.0.0.1:39719 and
// 127.0.0.1:42815 on a 30s cycle and half of the requests hit the wrong
// backend. Nothing in either service could notice.
func TestDuplicateClaimIsRejected(t *testing.T) {
	m := clone()
	driver := m[OwnerDriver]
	driver.Derived = append(driver.Derived, "/v1/driver")
	m[OwnerDriver] = driver

	err := validate(m, carveouts)
	if err == nil {
		t.Fatal("two owners claiming /v1/driver must be rejected; the route would flap between backends")
	}
	if !strings.Contains(err.Error(), "/v1/driver") {
		t.Fatalf("error should name the disputed prefix, got: %v", err)
	}
}

// TestUndeclaredCarveoutIsRejected covers the other half of the B8 defect: a
// service quietly taking over part of another service's subtree. Removing the
// declaration (but keeping the claim) must fail.
func TestUndeclaredCarveoutIsRejected(t *testing.T) {
	kept := make([]carveout, 0, len(carveouts))
	for _, c := range carveouts {
		if c.Prefix == "/v1/container/all" {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == len(carveouts) {
		t.Fatal("fixture error: /v1/container/all is expected to be a declared carve-out")
	}

	err := validate(clone(), kept)
	if err == nil {
		t.Fatal("an undeclared prefix nested in another owner's subtree must be rejected")
	}
	if !strings.Contains(err.Error(), "/v1/container/all") {
		t.Fatalf("error should name the offending prefix, got: %v", err)
	}
}

func TestStaleCarveoutIsRejected(t *testing.T) {
	stale := append(append([]carveout(nil), carveouts...), carveout{
		Prefix: "/v1/sys",
		Owner:  OwnerCasaNova,
		Reason: "documents a boundary that does not exist",
	})
	if err := validate(clone(), stale); err == nil {
		t.Fatal("a carve-out for a prefix that is not nested in anyone else's subtree must be rejected")
	}
}

// TestPathParameterPrefixIsRejected pins the constraint that makes the whole
// ownership question unavoidable: the gateway routes on literal prefixes, so
// "/v1/container/:id/logs" can never be published. A service that wants such a
// route has to own "/v1/container" itself.
func TestPathParameterPrefixIsRejected(t *testing.T) {
	m := clone()
	c := m[OwnerCasaNova]
	c.Prefixes = append(c.Prefixes, "/v1/container/:id/logs")
	m[OwnerCasaNova] = c

	if err := validate(m, carveouts); err == nil {
		t.Fatal("a prefix containing a path parameter must be rejected")
	}
}

// TestContainerFamilyResolution is the routing question B8 got wrong, frozen
// as a test. These are the exact URLs the web UI builds (see
// CasaNova-UI-v2/src/api/containers.ts).
func TestContainerFamilyResolution(t *testing.T) {
	cases := []struct {
		path string
		want Owner
	}{
		{"/v1/container/all", OwnerCasaNova},
		{"/v1/container/stats", OwnerCasaNova},
		{"/v1/container/image/tags", OwnerCasaNova},
		{"/v1/container/usage", OwnerAppManagement},
		{"/v1/container/networks", OwnerAppManagement},
		{"/v1/container/9b1deadbeef0/logs", OwnerAppManagement},
		{"/v1/container/9b1deadbeef0/terminal", OwnerAppManagement},
		{"/v1/container/9b1deadbeef0/start", OwnerAppManagement},
		{"/v1/container/9b1deadbeef0", OwnerAppManagement},
		{"/v1/driver", OwnerCasaNova},
		{"/v2/driver/sensors", OwnerDriver},
		{"/v1/users/login", OwnerUserService},
		{"/v1/disks", OwnerLocalStorage},
		// Not an API path at all: the static UI catch-all must take it.
		{"/index.html", OwnerStaticUI},
	}
	for _, tc := range cases {
		got, ok := Resolve(tc.path)
		if !ok {
			t.Errorf("Resolve(%q): no owner", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestHasPrefixRespectsSegmentBoundaries mirrors the gateway's pathHasPrefix.
// "/v1foo" must not be served by the owner of "/v1".
func TestHasPrefixRespectsSegmentBoundaries(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/v1/container", "/v1/container", true},
		{"/v1/container/all", "/v1/container", true},
		{"/v1/containerfoo", "/v1/container", false},
		{"/v1/container", "/v1/container/all", false},
		{"/anything", RootPrefix, true},
	}
	for _, tc := range cases {
		if got := HasPrefix(tc.path, tc.prefix); got != tc.want {
			t.Errorf("HasPrefix(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
		}
	}
}

// TestPrefixesReturnsACopy guards the registration path: main() appends its
// runtime-derived paths to the returned slice, and must not be able to grow
// the manifest's backing array into another service's claim.
func TestPrefixesReturnsACopy(t *testing.T) {
	before := len(manifest[OwnerAppManagement].Prefixes)
	got := Prefixes(OwnerAppManagement)
	got = append(got, "/v1/mutation")
	got[0] = "/v1/mutation"
	if len(manifest[OwnerAppManagement].Prefixes) != before {
		t.Fatal("append to the returned slice changed the manifest")
	}
	if manifest[OwnerAppManagement].Prefixes[0] == "/v1/mutation" {
		t.Fatal("writing to the returned slice changed the manifest")
	}
}
