package v1

// GetAptUpgradeStatus used to return the raw stdout/stderr tail of a
// privileged apt-get run — package names and exact versions, plus dpkg
// maintainer-script output — to ANY authenticated caller holding
// PermSystemRead. That is the same reconnaissance value as this package's
// package-list gate (an exact map of which known-vulnerable versions this
// box just ran). Triggering the upgrade already requires the stricter
// PermSystemConfig (PostAptUpgrade); reading its full raw output should
// require the same tier. A merely-authenticated non-admin still gets
// state/exit-code/reboot info, just not the raw log.
//
// This is NOT because the log could carry embedded Basic Auth credentials
// from a private repo URL: reproduced directly (see host_apt.go's Tail
// comment), apt's own URI printer strips userinfo before it reaches
// output, so a real run's log never contains one. The fixture below still
// includes a credential-shaped string anyway, checked as a belt-and-
// suspenders property test ("whatever ends up in this log, a non-admin
// gets none of it") rather than as a claim about what apt actually emits.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

func aptUpgradeStatusCtx(role string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/host/apt/upgrade/status", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, role, 1)
	return ctx, rec
}

func pointAptUpgradeLogAt(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "apt-upgrade.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := aptUpgradeLog
	aptUpgradeLog = path
	t.Cleanup(func() { aptUpgradeLog = prev })
}

const aptUpgradeLogFixture = `Get:1 https://user:s3cr3t@private-mirror.example.com/ubuntu noble/main amd64 foo-pkg amd64 1.2.3 [45 kB]
Unpacking foo-pkg (1.2.3) over (1.2.2) ...
Setting up foo-pkg (1.2.3) ...`

// THE GAP: a non-admin ("user" role) authenticated caller must not see the
// raw log — package/version reconnaissance at minimum, and (belt and
// suspenders, see the file comment above) anything else that ends up in it.
func TestGetAptUpgradeStatusHidesTailFromNonAdmin(t *testing.T) {
	pointAptUpgradeLogAt(t, aptUpgradeLogFixture)

	ctx, rec := aptUpgradeStatusCtx(string(rbac.RoleUser))
	if err := GetAptUpgradeStatus(ctx); err != nil {
		t.Fatalf("GetAptUpgradeStatus returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a non-admin should still get coarse status), body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatalf("response to a non-admin caller leaked the raw log: %s", rec.Body.String())
	}
	// Tail must be [] , not omitted/null: the JSON tag has no omitempty, and
	// the Vue card does `upgrade.tail.length` with no optional chaining —
	// a null there is a client-side crash for every non-admin viewer.
	if !strings.Contains(rec.Body.String(), `"tail":[]`) {
		t.Fatalf("tail is not an empty array for a non-admin caller (would be `null`, crashing the UI's "+
			"unconditional upgrade.tail.length read): %s", rec.Body.String())
	}
}

// Positive control: an admin caller must still get the raw tail — this is a
// visibility restriction, not a removal of the feature.
func TestGetAptUpgradeStatusShowsTailToAdmin(t *testing.T) {
	pointAptUpgradeLogAt(t, aptUpgradeLogFixture)

	ctx, rec := aptUpgradeStatusCtx(string(rbac.RoleAdmin))
	if err := GetAptUpgradeStatus(ctx); err != nil {
		t.Fatalf("GetAptUpgradeStatus returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "foo-pkg") {
		t.Fatalf("admin caller did not receive the log tail: %s", rec.Body.String())
	}
}
