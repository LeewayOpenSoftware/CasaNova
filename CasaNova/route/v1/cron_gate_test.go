package v1

import (
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// GET /v1/sys/cron returns the ROOT CRONTAB — every line is a command the box
// runs as root on a schedule. It used to be gated on PermSystemRead, which
// guest HOLDS, so the gate could not refuse anybody the JWT middleware let in:
// an ordinary user, and an OAuth2-delegated token minted with role "guest" on
// purpose to de-privilege a relying party, both read it.
//
// These drive the handler with each non-admin principal and require a 403. The
// anonymous case matters as much as the others: RoleFromContext fails closed to
// RoleGuest, so "no role on the context" is not "no permissions" here, it is
// guest — which is exactly how the old bit passed.
//
// anonContext/identifiedContext live in system_diagnostic_gate_test.go.
func TestGetCronJobsRefusesEveryNonAdmin(t *testing.T) {
	cases := []struct {
		name string
		ctx  func() (echo.Context, http.ResponseWriter)
	}{
		{"anonymous (fails closed to guest)", func() (echo.Context, http.ResponseWriter) {
			c, r := anonContext()
			return c, r
		}},
		{"guest", func() (echo.Context, http.ResponseWriter) {
			c, r := identifiedContext(42, rbac.RoleGuest)
			return c, r
		}},
		{"user", func() (echo.Context, http.ResponseWriter) {
			c, r := identifiedContext(7, rbac.RoleUser)
			return c, r
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := tc.ctx()
			err := GetCronJobs(ctx)
			if err == nil {
				t.Fatal("GetCronJobs answered a non-admin caller. The root crontab is " +
					"root's command inventory; only PermSystemConfig may read it.")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("GetCronJobs returned %T (%v); the gate must return an "+
					"*echo.HTTPError so echo aborts the chain — a denial written with "+
					"ctx.JSON returns nil and the handler runs on anyway", err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("refused with %d; want 403", he.Code)
			}
		})
	}
}

// The admin path must NOT be refused by the gate. Asserted as "not a 403"
// rather than "200", because the handler goes on to exec `crontab -l`, whose
// outcome depends on the machine the test runs on; the gate's behaviour does
// not.
func TestGetCronJobsAdmitsAdmin(t *testing.T) {
	ctx, _ := identifiedContext(1, rbac.RoleAdmin)
	err := GetCronJobs(ctx)
	if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusForbidden {
		t.Fatal("GetCronJobs refused an admin with 403; admin holds PermSystemConfig")
	}
}

// Class pin, not a spot check. The four mutating cron handlers were already on
// PermSystemConfig and only the read was open, which is precisely the shape
// that survives review: a new read handler added later reaches for the "read"
// bit by name. Every gate in cron.go must sit at PermSystemConfig, because in
// this file reading and writing are the same privilege.
func TestEveryCronHandlerGatesOnSystemConfig(t *testing.T) {
	src, err := os.ReadFile("cron.go")
	if err != nil {
		t.Fatalf("cannot read cron.go: %v", err)
	}
	gate := regexp.MustCompile(`requirePerm\(ctx, rbac\.(Perm\w+)\)`)
	found := gate.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("found no requirePerm call in cron.go — this test scans source, so a " +
			"renamed helper makes it vacuous rather than failing; re-point it")
	}
	for _, m := range found {
		if m[1] != "PermSystemConfig" {
			t.Errorf("cron.go gates on rbac.%s; every cron endpoint must require "+
				"PermSystemConfig — the crontab is root's command inventory and "+
				"guest holds PermSystemRead", m[1])
		}
	}
}
