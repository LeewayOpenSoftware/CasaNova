package v2

// The Docker Hub search broke because the upstream response SHAPE changed and
// nothing here noticed: /v2/search/repositories/ started answering 403 to this
// client (verified from the production box with a Go binary — every
// User-Agent and Accept combination got 403, while curl and python got 200
// from the same host), and /api/search/v4, which Hub's own web UI calls,
// answers 200 but with different field names and types.
//
// The fixture below is a real v4 payload captured from the box, trimmed to
// the fields we read. If Hub changes the shape again these fail with the new
// reality instead of the search silently returning nothing.

import (
	"strings"
	"testing"
)

const v4Fixture = `{
 "total": 291835,
 "results": [
  {"slug":"dhi/nginx","name":"Nginx","type":"dhi","badge":"hardened",
   "short_description":"Hardened nginx image.","star_count":0,"pull_count":"5M+"},
  {"slug":"nginx","name":"nginx","type":"image","badge":"official",
   "short_description":"Official build of Nginx.","star_count":21363,"pull_count":"1B+"},
  {"slug":"nginx/nginx-ingress","name":"nginx-ingress","type":"image","badge":"verified_publisher",
   "short_description":"NGINX Ingress Controller","star_count":122,"pull_count":"1B+"},
  {"slug":"nginx/docker-extension","name":"docker-extension","type":"extension","badge":"verified_publisher",
   "short_description":"An extension.","star_count":0,"pull_count":"50K+"}
 ]}`

func TestParseDockerHubV4RealPayload(t *testing.T) {
	got, err := parseDockerHubV4(strings.NewReader(v4Fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Two of the four are installable images; the hardened listing and the
	// Docker Extension are not, and offering them would fail at pull time
	// with an error the user cannot act on.
	if len(got) != 2 {
		names := []string{}
		for _, r := range got {
			names = append(names, r.Name)
		}
		t.Fatalf("got %d results (%s), want 2 — non-image types must be filtered", len(got), strings.Join(names, ", "))
	}

	// The official image must come back with the namespace made explicit,
	// because "nginx" alone is not a valid pull name in every context.
	official := got[0]
	if official.Name != "library/nginx" {
		t.Fatalf("official name = %q, want library/nginx", official.Name)
	}
	if !official.Official {
		t.Fatal("badge 'official' did not set Official — v4 replaced the old is_official bool with this string")
	}
	if official.Stars != 21363 {
		t.Fatalf("stars = %d, want 21363", official.Stars)
	}
	// Hub formats the count itself; decoding it as a number would fail the
	// entire response and empty the search.
	if official.Pulls != "1B+" {
		t.Fatalf("pulls = %q, want the string Hub sent (\"1B+\")", official.Pulls)
	}

	// A namespaced third-party image keeps its slug untouched.
	if got[1].Name != "nginx/nginx-ingress" || got[1].Official {
		t.Fatalf("third-party entry mangled: %+v", got[1])
	}
}

func TestParseDockerHubV4RejectsGarbage(t *testing.T) {
	if _, err := parseDockerHubV4(strings.NewReader("<html>403 Forbidden</html>")); err == nil {
		t.Fatal("an HTML error page parsed as a result list; the handler would report success with zero apps")
	}
}

func TestParseDockerHubV4ToleratesMissingType(t *testing.T) {
	// Defensive: if Hub ever drops the field, entries must still be offered
	// rather than every result silently vanishing.
	got, err := parseDockerHubV4(strings.NewReader(
		`{"results":[{"slug":"foo/bar","short_description":"x","star_count":1,"pull_count":"10K+"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "foo/bar" {
		t.Fatalf("entry without a type was dropped: %+v", got)
	}
}
