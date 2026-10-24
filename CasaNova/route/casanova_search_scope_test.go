package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/labstack/echo/v4"
)

// POST /v2/search/file walks the storage roots and returns names, sizes and
// mtimes. It checked only PermFileRead, which every `user` holds, so an
// account with no storage grant enumerated every other user's files. It must
// answer only what the storage authority lets the account list.
func setupSearchScope(t *testing.T) (root, alice, bob string) {
	t.Helper()
	root = t.TempDir()
	alice = filepath.Join(root, "alice")
	bob = filepath.Join(root, "bob", "private")
	for _, d := range []string{alice, bob} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(alice, "alice-payroll.xlsx"), filepath.Join(bob, "bob-payroll.xlsx")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	storageauth.SetStateDir(t.TempDir())
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storageauth.SetStateDir("/var/lib/casanova") })

	saved := searchStorageRoots
	searchStorageRoots = []string{root}
	t.Cleanup(func() { searchStorageRoots = saved })
	return root, alice, bob
}

func runSearch(t *testing.T, role rbac.Role, uid int, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/search/file", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(commonmw.ContextKeyRole, role)
	c.Set(commonmw.ContextKeyUserID, uid)
	if err := searchFile(c); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestV2SearchHonoursStorageAuthority(t *testing.T) {
	_, alice, bob := setupSearchScope(t)

	// No grant: nothing from anyone.
	rec := runSearch(t, rbac.RoleUser, 42, `{"keyword":"payroll"}`)
	if strings.Contains(rec.Body.String(), "payroll.xlsx") {
		t.Errorf("user without a storage grant enumerated files: %s", rec.Body.String())
	}

	// Grant on alice only: alice's file, never bob's.
	if err := storageauth.AddGrant(storageauth.Grant{ID: "g-alice", PrincipalID: "42", Path: alice,
		Ops: storageauth.OpRead, Origin: storageauth.OriginOperator}); err != nil {
		t.Fatal(err)
	}
	rec = runSearch(t, rbac.RoleUser, 42, `{"keyword":"payroll"}`)
	if !strings.Contains(rec.Body.String(), "alice-payroll.xlsx") {
		t.Errorf("granted user lost their own file from global search: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bob-payroll.xlsx") {
		t.Errorf("granted user enumerated another user's file: %s", rec.Body.String())
	}

	// Naming a dir the account cannot list is refused outright.
	rec = runSearch(t, rbac.RoleUser, 42, `{"keyword":"payroll","dir":"`+bob+`"}`)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "bob-payroll.xlsx") {
		t.Errorf("dir outside the grant: status %d body %s, want 403", rec.Code, rec.Body.String())
	}
}

func TestV2SearchAdminStillSeesAllStorage(t *testing.T) {
	_, alice, _ := setupSearchScope(t)
	rec := runSearch(t, rbac.RoleAdmin, 1, `{"keyword":"payroll"}`)
	for _, want := range []string{"alice-payroll.xlsx", "bob-payroll.xlsx"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("admin global search missing %s: %s", want, rec.Body.String())
		}
	}
	rec = runSearch(t, rbac.RoleAdmin, 1, `{"keyword":"payroll","dir":"`+alice+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice-payroll.xlsx") {
		t.Errorf("admin scoped search: status %d body %s", rec.Code, rec.Body.String())
	}
}
