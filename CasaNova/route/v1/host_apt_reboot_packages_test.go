package v1

// Two rules pinned here, both about the SAME field in TWO twin handlers:
//
//   1. rebootPackages is CONTENT (`linux-image-6.8.0-45-generic` is an
//      installed version, i.e. a target), so it needs PermSystemConfig — while
//      the rebootRequired BOOLEAN is the alarm and stays readable for anyone
//      authenticated. Gating it in only one of the two handlers would leave
//      the other one answering the same question, so both are pinned together.
//
//   2. The slice is never nil on the wire. The tag has no omitempty, so a nil
//      serialized as `"rebootPackages":null` against an api/host.ts that
//      declares it non-nullable `string[]` — in BOTH response types.

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

// pointRebootMarkersAt redirects the /run markers at a temp dir. pkgs == nil
// means "no reboot pending at all" (neither file exists).
func pointRebootMarkersAt(t *testing.T, pkgs []string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "reboot-required")
	pkgsFile := marker + ".pkgs"
	if pkgs != nil {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pkgsFile, []byte(strings.Join(pkgs, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prevM, prevP := rebootMarkerFiles, rebootPkgsFiles
	// Both slots point into the temp dir: the real code probes /var/run then
	// /run, and a test that only redirected the first would still read the
	// host's real /run on a box that has one.
	rebootMarkerFiles = []string{marker, marker}
	rebootPkgsFiles = []string{pkgsFile, pkgsFile}
	t.Cleanup(func() { rebootMarkerFiles, rebootPkgsFiles = prevM, prevP })
}

// The kernel package name carries its ABI version — that is the target.
var rebootPkgFixture = []string{"linux-image-6.8.0-45-generic", "libc6"}

func aptStatusBodyFor(t *testing.T, role string) string {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/host/apt/updates", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, role, 1)
	if err := GetAptUpdates(ctx); err != nil {
		t.Fatalf("GetAptUpdates returned an error: %v", err)
	}
	return rec.Body.String()
}

func aptUpgradeBodyFor(t *testing.T, role string) string {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/host/apt/upgrade/status", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, role, 1)
	if err := GetAptUpgradeStatus(ctx); err != nil {
		t.Fatalf("GetAptUpgradeStatus returned an error: %v", err)
	}
	return rec.Body.String()
}

// THE GAP, in BOTH twins. A table so neither handler can be fixed alone.
func TestRebootPackagesHiddenFromNonAdminInBothHandlers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(*testing.T, string) string
	}{
		{"GetAptUpdates", aptStatusBodyFor},
		{"GetAptUpgradeStatus", aptUpgradeBodyFor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pointRebootMarkersAt(t, rebootPkgFixture)
			body := tc.body(t, string(rbac.RoleUser))
			if strings.Contains(body, "linux-image-6.8.0-45-generic") {
				t.Fatalf("a non-admin received the kernel package pending a reboot (an installed version): %s", body)
			}
			if strings.Contains(body, `"rebootPackages":null`) {
				t.Fatalf("rebootPackages serialized as null; api/host.ts declares it a non-nullable string[]: %s", body)
			}
			if !strings.Contains(body, `"rebootPackages":[]`) {
				t.Fatalf("rebootPackages is not an empty array for a non-admin: %s", body)
			}
		})
	}
}

// Positive control on the ALARM: the boolean must survive for a non-admin in
// both handlers. Without this, blanking rebootRequired too would pass the test
// above while destroying the only reboot signal a non-admin has.
func TestRebootRequiredBooleanStaysReadableForNonAdmin(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(*testing.T, string) string
	}{
		{"GetAptUpdates", aptStatusBodyFor},
		{"GetAptUpgradeStatus", aptUpgradeBodyFor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pointRebootMarkersAt(t, rebootPkgFixture)
			body := tc.body(t, string(rbac.RoleUser))
			if !strings.Contains(body, `"rebootRequired":true`) {
				t.Fatalf("a non-admin lost the reboot alarm, which is state and not configuration: %s", body)
			}
		})
	}
}

// Positive control on the CONTENT: an admin still gets the names in both.
// Without this, a handler that always emitted [] would pass everything above.
func TestRebootPackagesVisibleToAdminInBothHandlers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(*testing.T, string) string
	}{
		{"GetAptUpdates", aptStatusBodyFor},
		{"GetAptUpgradeStatus", aptUpgradeBodyFor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pointRebootMarkersAt(t, rebootPkgFixture)
			body := tc.body(t, string(rbac.RoleAdmin))
			if !strings.Contains(body, "linux-image-6.8.0-45-generic") {
				t.Fatalf("an admin did not receive the pending reboot packages: %s", body)
			}
		})
	}
}

// The type lie, which is NOT about permission: with no reboot pending at all,
// the field must still be [] — for an ADMIN, the caller no redaction touches.
func TestRebootPackagesIsEmptyArrayNotNullWhenNoRebootPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(*testing.T, string) string
	}{
		{"GetAptUpdates", aptStatusBodyFor},
		{"GetAptUpgradeStatus", aptUpgradeBodyFor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pointRebootMarkersAt(t, nil) // no marker files at all
			body := tc.body(t, string(rbac.RoleAdmin))
			if strings.Contains(body, `"rebootPackages":null`) {
				t.Fatalf("rebootPackages serialized as null with nothing pending, against a non-nullable string[] in api/host.ts: %s", body)
			}
			if !strings.Contains(body, `"rebootPackages":[]`) {
				t.Fatalf("rebootPackages is not an empty array with nothing pending: %s", body)
			}
			if !strings.Contains(body, `"rebootRequired":false`) {
				t.Fatalf("no marker files, yet rebootRequired is not false: %s", body)
			}
		})
	}
}
