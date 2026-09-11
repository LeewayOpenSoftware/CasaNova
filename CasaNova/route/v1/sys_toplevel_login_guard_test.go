package v1

import (
	"net/http"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// Permgate tail #13, batch A — antivirus.go, backup.go, certs.go, events.go
// (notifications only — GetAuditEvents is already fixed), logs.go
// (GetLogUnits only — GetSystemLogs is already fixed).
//
// Same claim as system_diagnostic_gate_test.go's G-03 suite, applied to the
// six handlers in this batch that were skipped when that pattern was rolled
// out: requirePerm(PermSystemRead) is a no-op against a tokenless caller
// because RoleFromContext fails closed to guest, and guest holds
// PermSystemRead. requireSystemPrincipal closes it by checking
// UserIDFromContext first.

var batchASysHandlers = map[string]func(echo.Context) error{
	"GetAntivirusStatus": GetAntivirusStatus, // GET /v1/sys/antivirus/status
	"GetBackupList":      GetBackupList,      // GET /v1/sys/backup/list
	"GetCertsStatus":     GetCertsStatus,     // GET /v1/sys/certs/status
	"GetCertsCheck":      GetCertsCheck,      // GET /v1/sys/certs/check
	"GetNotifications":   GetNotifications,   // GET /v1/sys/notifications
	"GetLogUnits":        GetLogUnits,        // GET /v1/sys/logs/units
}

func TestBatchASysHandlersRefuseAnonymousCallers(t *testing.T) {
	for name, h := range batchASysHandlers {
		t.Run(name, func(t *testing.T) {
			ctx, rec := anonContext()
			err := callGuardingAgainstPanic(t, name, h, ctx)
			if err == nil {
				t.Fatalf("%s answered an anonymous caller (status %d, body %q). "+
					"Every caller that presented no token must be refused.",
					name, rec.Code, rec.Body.String())
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("%s returned %T (%v); the gate must return an *echo.HTTPError so "+
					"echo aborts the chain — a helper that writes the response with ctx.JSON "+
					"returns nil and the handler runs on", name, err, err)
			}
			if he.Code != http.StatusUnauthorized {
				t.Fatalf("%s refused an anonymous caller with %d; want 401. A 403 here would "+
					"mean the refusal came from the permission table, which guest satisfies "+
					"for PermSystemRead", name, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("%s wrote a body (%q) while denying; the denial must not commit the "+
					"response", name, rec.Body.String())
			}
		})
	}
}

// Positive control: an authenticated guest account (the population these
// routes actually serve — dashboard widgets for every signed-in role) must
// keep working. A zero-count assertion above proves nothing on its own if
// every path also happens to be broken for a legitimate caller.
func TestBatchASysHandlersAllowIdentifiedGuest(t *testing.T) {
	for name, h := range batchASysHandlers {
		t.Run(name, func(t *testing.T) {
			ctx, rec := identifiedContext(7, rbac.RoleGuest)
			err := callGuardingAgainstPanic(t, name, h, ctx)
			if err != nil {
				t.Fatalf("%s refused an identified guest account: %v (status %d, body %q). "+
					"guest holds PermSystemRead; the login guard must not remove that",
					name, err, rec.Code, rec.Body.String())
			}
		})
	}
}
