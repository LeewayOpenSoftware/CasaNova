package v1

import (
	"net/http"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// reachesSensitiveHandler reports whether authorization let the handler reach
// its first downstream dependency. A panic counts as reached: these focused
// gate tests intentionally do not initialize the system-service repository.
func reachesSensitiveHandler(h func(echo.Context) error, ctx echo.Context) (reached bool) {
	defer func() {
		if recover() != nil {
			reached = true
		}
	}()
	err := h(ctx)
	if he, ok := err.(*echo.HTTPError); ok && (he.Code == http.StatusUnauthorized || he.Code == http.StatusForbidden) {
		return false
	}
	return true
}

func requireGuestDeniedAndAdminAdmitted(t *testing.T, h func(echo.Context) error) {
	t.Helper()

	guest, _ := identifiedContext(42, rbac.RoleGuest)
	if reachesSensitiveHandler(h, guest) {
		t.Fatal("authenticated guest reached admin-grade log/audit content; want 403 before downstream work")
	}

	admin, _ := identifiedContext(1, rbac.RoleAdmin)
	if !reachesSensitiveHandler(h, admin) {
		t.Fatal("authenticated admin was refused; positive control did not reach the handler")
	}
}

func TestDaemonLogTailRequiresAdminPrincipal(t *testing.T) {
	requireGuestDeniedAndAdminAdmitted(t, GetCasaOSErrorLogs)
}

func TestJournalLogsRequireAdminPrincipal(t *testing.T) {
	// Keep the positive control deterministic: once admitted, the handler takes
	// its documented journalctl-not-installed response rather than reading the
	// build host's journal.
	t.Setenv("PATH", t.TempDir())
	requireGuestDeniedAndAdminAdmitted(t, GetSystemLogs)
}

func TestAuditEventsRequireAdminPrincipal(t *testing.T) {
	requireGuestDeniedAndAdminAdmitted(t, GetAuditEvents)
}
