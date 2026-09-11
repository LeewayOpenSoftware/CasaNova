package v1

// GetAptUpdates used to hand ANY authenticated caller holding PermSystemRead
// the full cached package list — name, INSTALLED version, candidate version
// and a per-row security flag — i.e. a precise, machine-readable map of which
// known-vulnerable versions this box is running, plus the inventory of its
// private APT sources. Its twin GetAptUpgradeStatus already draws the line at
// PermSystemConfig for the privileged CONTENT while leaving the coarse state
// readable; these tests pin the same line here, and pin the wire SHAPE that
// keeps the fix from crashing the non-admin UI.

import (
	"encoding/json"
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

func aptUpdatesCtx(role string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/host/apt/updates", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, role, 1)
	return ctx, rec
}

// pointAptCacheAt writes a cache snapshot with one ordinary update and one
// SECURITY update whose installed version is the interesting part.
func pointAptCacheAt(t *testing.T) {
	t.Helper()
	st := AptStatus{
		Supported:   true,
		CheckedAt:   "2026-09-11T00:00:00Z",
		Total:       2,
		Security:    1,
		FailedRepos: []string{"https://private-mirror.example.com/ubuntu"},
		Packages: []AptPackage{
			{Name: "foo-pkg", CurrentVersion: "1.2.2", NewVersion: "1.2.3"},
			{Name: "openssl", CurrentVersion: "3.0.2-0ubuntu1.15", NewVersion: "3.0.2-0ubuntu1.19", Security: true},
		},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apt-status.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	prev := aptCacheFile
	aptCacheFile = path
	t.Cleanup(func() { aptCacheFile = prev })
}

// THE GAP: a merely-authenticated non-admin must not receive the per-package
// rows. The assertion is on the vulnerable INSTALLED version, not on the
// package name, because the name alone is what a count would already imply —
// the installed version is what makes the disclosure actionable.
func TestGetAptUpdatesHidesPackageRowsFromNonAdmin(t *testing.T) {
	pointAptCacheAt(t)

	ctx, rec := aptUpdatesCtx(string(rbac.RoleUser))
	if err := GetAptUpdates(ctx); err != nil {
		t.Fatalf("GetAptUpdates returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a non-admin still gets coarse status), body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "3.0.2-0ubuntu1.15") {
		t.Fatalf("response to a non-admin leaked the installed version of a package with a pending SECURITY update: %s", body)
	}
	if strings.Contains(body, "private-mirror.example.com") {
		t.Fatalf("response to a non-admin leaked the private APT source inventory: %s", body)
	}
}

// The counts deliberately SURVIVE for a non-admin: the needs-attention
// indicator is the whole reason this handler is readable below admin. Without
// this case, "redact everything" would pass the test above and silently blank
// the card.
func TestGetAptUpdatesKeepsCountsForNonAdmin(t *testing.T) {
	pointAptCacheAt(t)

	ctx, rec := aptUpdatesCtx(string(rbac.RoleUser))
	if err := GetAptUpdates(ctx); err != nil {
		t.Fatalf("GetAptUpdates returned an error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"total":2`) || !strings.Contains(body, `"security":1`) {
		t.Fatalf("a non-admin lost the update counts, which are the needs-attention indicator: %s", body)
	}
}

// THE SHAPE, and its own RED: a field made conditional by permission must
// serialize [] and never null. AptStatus.Packages has no omitempty, and
// AptUpdateCard.vue's dockerInUpgrade computed does apt.packages.some(...)
// with no optional chaining, on /app/settings/updates — a route that is NOT
// admin-only. A null here would turn this security fix into a client-side
// crash for every non-admin viewer.
func TestGetAptUpdatesSerializesEmptyPackagesNotNullForNonAdmin(t *testing.T) {
	pointAptCacheAt(t)

	ctx, rec := aptUpdatesCtx(string(rbac.RoleUser))
	if err := GetAptUpdates(ctx); err != nil {
		t.Fatalf("GetAptUpdates returned an error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"packages":null`) {
		t.Fatalf("packages serialized as null for a non-admin; the UI reads apt.packages.some() unguarded: %s", body)
	}
	if !strings.Contains(body, `"packages":[]`) {
		t.Fatalf("packages is not an empty array for a non-admin: %s", body)
	}
}

// Positive control: an admin still gets the rows AND the failed-repo
// inventory. This is a visibility restriction, not a removal of the feature —
// and without this case a handler that always returned an empty list would
// pass every assertion above.
func TestGetAptUpdatesShowsPackageRowsToAdmin(t *testing.T) {
	pointAptCacheAt(t)

	ctx, rec := aptUpdatesCtx(string(rbac.RoleAdmin))
	if err := GetAptUpdates(ctx); err != nil {
		t.Fatalf("GetAptUpdates returned an error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "3.0.2-0ubuntu1.15") {
		t.Fatalf("admin caller did not receive the package rows: %s", body)
	}
	if !strings.Contains(body, "private-mirror.example.com") {
		t.Fatalf("admin caller did not receive the failed-repo inventory: %s", body)
	}
}

// An anonymous caller (no user id in context) is refused outright — the
// handler is gated by requireSystemPrincipal, not a bare requirePerm, because
// guest holds PermSystemRead. Pins that the redaction above did not become
// the ONLY control by someone relaxing the gate to "redact for non-admins".
func TestGetAptUpdatesRefusesAnonymous(t *testing.T) {
	pointAptCacheAt(t)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/host/apt/updates", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)

	err := GetAptUpdates(ctx)
	if err == nil && rec.Code == http.StatusOK {
		t.Fatalf("a tokenless caller received a 200: %s", rec.Body.String())
	}
}
