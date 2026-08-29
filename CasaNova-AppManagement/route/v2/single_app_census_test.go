package v2

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A handler that serves ONE app must not build the whole collection.
//
// Compose().List() enumerates every stack AND parses each one's compose YAML
// off disk. Measured on the production box with 23 apps installed:
//
//	GET /v2/app_management/compose        (all 23)   156 ms
//	GET /v2/app_management/compose/{id}   (one app)  150 ms
//
// Indistinguishable, because the per-app handler did the collection's work and
// then threw 22 parsed files away. `docker compose ls` accounts for about
// 40 ms of that; the rest is 113 KB of YAML parsed for nothing.
//
// Eight handlers had the shape. This census is a COUNT and it is structural, so
// it fails when a ninth is added — which is the realistic regression, since
// "call List and index it" is the obvious thing to write.
func TestNoPerAppHandlerRebuildsTheWholeCollection(t *testing.T) {
	const file = "compose_app.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%s is unreadable, so this census went blind: %v", file, err)
	}
	body := string(src)

	// The shape, in two passes because Go's regexp is RE2 and has no
	// backreferences: find each List() assignment, then look ahead for that
	// same variable being indexed by an id.
	listAssign := regexp.MustCompile(`(\w+), err := service\.MyService\.Compose\(\)\.List\(`)
	for _, m := range listAssign.FindAllStringSubmatchIndex(body, -1) {
		coll := body[m[2]:m[3]]
		line := strings.Count(body[:m[0]], "\n") + 1

		// Look at the next 40 lines only: an index further away is a different
		// statement, not this handler picking one app out of the collection.
		rest := body[m[1]:]
		if idx := nthNewline(rest, 40); idx > 0 {
			rest = rest[:idx]
		}
		if strings.Contains(rest, ", ok := "+coll+"[") {
			t.Errorf("%s:%d: this handler calls Compose().List() and then indexes "+
				"one app out of %q. That parses every installed app's compose YAML "+
				"to serve one — 150 ms instead of ~45 ms with 23 apps installed. "+
				"Use Compose().Get(ctx, id).", file, line, coll)
		}
	}

	// And the census must be able to SEE the thing it audits: if the file no
	// longer contains any Compose() call at all, the pattern has drifted and a
	// green result means nothing.
	if !strings.Contains(body, "service.MyService.Compose()") {
		t.Fatal("no Compose() calls found in compose_app.go — this census is not " +
			"looking at the code it claims to audit")
	}
	if n := strings.Count(body, "Compose().Get("); n < 8 {
		t.Errorf("only %d handlers use Compose().Get(); eight were migrated, so "+
			"some have been reverted to the collection-rebuilding shape", n)
	}
}

// List itself must still exist and still be used where the WHOLE collection is
// genuinely wanted — the fix is to stop misusing it, not to delete it.
func TestTheListEndpointStillListsEverything(t *testing.T) {
	src, err := os.ReadFile("compose_app.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "service.MyService.Compose().List(") {
		t.Fatal("no handler calls Compose().List() any more: the collection " +
			"endpoint cannot work, so the migration went one step too far")
	}
}

// nthNewline returns the offset just past the nth newline, or -1.
func nthNewline(s string, n int) int {
	for i, c := range s {
		if c == '\n' {
			n--
			if n == 0 {
				return i
			}
		}
	}
	return -1
}
