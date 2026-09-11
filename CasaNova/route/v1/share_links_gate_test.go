package v1

import (
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// GET /v1/file/share-links returns every live share link's ID, its absolute
// path on disk, and the ready-made /public/share/<id> URL. The entire security
// model of a public share is that the ID is an unguessable secret handed only
// to the recipient, so listing them IS handing out every share.
//
// It was gated on PermSystemRead, which guestPerms HOLDS, while its own create
// and delete siblings required PermSystemConfig. A read whose CONTENT is the
// credential belongs at the write bit.
//
// anonContext/identifiedContext live in system_diagnostic_gate_test.go.
func TestGetShareLinksRefusesEveryNonAdmin(t *testing.T) {
	cases := []struct {
		name string
		ctx  func() echo.Context
	}{
		{"anonymous (fails closed to guest)", func() echo.Context { c, _ := anonContext(); return c }},
		{"guest", func() echo.Context { c, _ := identifiedContext(42, rbac.RoleGuest); return c }},
		{"user", func() echo.Context { c, _ := identifiedContext(7, rbac.RoleUser); return c }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GetShareLinks(tc.ctx())
			if err == nil {
				t.Fatal("GetShareLinks answered a non-admin. Every share ID it returns " +
					"is the secret that authorises an anonymous download of that file.")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("returned %T (%v); the gate must return an *echo.HTTPError so "+
					"echo aborts the chain — a denial written with ctx.JSON returns nil "+
					"and the handler runs on anyway", err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("refused with %d; want 403", he.Code)
			}
		})
	}
}

// Class pin, the same shape as cron.go's: all three share-link handlers must
// sit at PermSystemConfig. The defect was that ONE of a set of three reached
// for the "read" bit by name; a spot check on the one we just fixed would not
// notice the next one.
func TestEveryShareLinkHandlerGatesOnSystemConfig(t *testing.T) {
	src, err := os.ReadFile("file_share_links.go")
	if err != nil {
		t.Fatalf("cannot read file_share_links.go: %v", err)
	}
	gate := regexp.MustCompile(`requirePerm\(ctx, rbac\.(Perm\w+)\)`)
	found := gate.FindAllStringSubmatch(string(src), -1)
	if len(found) != 3 {
		t.Fatalf("found %d requirePerm calls, want 3 (create, list, delete) — this test "+
			"scans source, so a renamed helper or a moved handler makes it vacuous "+
			"rather than failing; re-point it", len(found))
	}
	for _, m := range found {
		if m[1] != "PermSystemConfig" {
			t.Errorf("file_share_links.go gates on rbac.%s; all three share-link "+
				"endpoints must require PermSystemConfig, because the list returns the "+
				"share secrets themselves and guest holds PermSystemRead", m[1])
		}
	}
}
