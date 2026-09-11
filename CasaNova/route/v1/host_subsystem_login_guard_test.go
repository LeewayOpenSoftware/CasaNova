package v1

import (
	"net/http"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// Permgate tail #13, batch B — host_apt.go, host_memory.go, host_power.go,
// host_processes.go, host_services.go, host_ups.go. Same claim as
// sys_toplevel_login_guard_test.go (batch A) and system_diagnostic_gate_test.go's
// G-03 suite: requirePerm(PermSystemRead) does not refuse a tokenless caller,
// because guest holds PermSystemRead and RoleFromContext fails closed to
// guest. requireSystemPrincipal closes it.

var batchBHostHandlers = map[string]func(echo.Context) error{
	"GetAptUpdates":       GetAptUpdates,       // GET /v1/sys/host/apt/updates
	"GetAptUpgradeStatus": GetAptUpgradeStatus, // GET /v1/sys/host/apt/upgrade/status
	"GetAptAuto":          GetAptAuto,          // GET /v1/sys/host/apt/auto
	"GetHostMemory":       GetHostMemory,       // GET /v1/sys/host/memory
	"GetHostPower":        GetHostPower,        // GET /v1/sys/host/power
	"GetHostPowerConfig":  GetHostPowerConfig,  // GET /v1/sys/host/power/config
	"GetHostProcesses":    GetHostProcesses,    // GET /v1/sys/host/processes
	"GetHostServices":     GetHostServices,     // GET /v1/sys/host/services
	"GetUPSConfig":        GetUPSConfig,        // GET /v1/sys/host/ups/config
	"GetHostUPS":          GetHostUPS,          // GET /v1/sys/host/ups
}

func TestBatchBHostHandlersRefuseAnonymousCallers(t *testing.T) {
	for name, h := range batchBHostHandlers {
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

// Positive control: an authenticated guest account must keep working.
func TestBatchBHostHandlersAllowIdentifiedGuest(t *testing.T) {
	for name, h := range batchBHostHandlers {
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
