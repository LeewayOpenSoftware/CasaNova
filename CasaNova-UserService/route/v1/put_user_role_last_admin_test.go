package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	dbmodel "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
)

// fakeUserServicePutRole implements only what PutUserRole calls.
type fakeUserServicePutRole struct {
	service.UserService
	roster     []dbmodel.UserDBModel
	byID       map[string]dbmodel.UserDBModel
	updateSeen *dbmodel.UserDBModel
}

func (f *fakeUserServicePutRole) GetAllUserName() []dbmodel.UserDBModel { return f.roster }

func (f *fakeUserServicePutRole) GetUserAllInfoById(id string) dbmodel.UserDBModel {
	return f.byID[id]
}

func (f *fakeUserServicePutRole) UpdateUser(u dbmodel.UserDBModel) {
	f.updateSeen = &u
}

type fakeRepoPutRole struct {
	service.Repository
	user *fakeUserServicePutRole
}

func (f fakeRepoPutRole) User() service.UserService { return f.user }

func putUserRoleCtx(targetID, authedID, newRole string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	body := `{"role":"` + newRole + `"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/users/"+targetID+"/role", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("user_id", authedID)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	ctx.SetParamNames("id")
	ctx.SetParamValues(targetID)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 7)
	return ctx, rec
}

func withPutUserRoleFake(byID map[string]dbmodel.UserDBModel) *fakeUserServicePutRole {
	roster := make([]dbmodel.UserDBModel, 0, len(byID))
	for _, u := range byID {
		roster = append(roster, u)
	}
	return &fakeUserServicePutRole{roster: roster, byID: byID}
}

// Baseline: demoting an ordinary user must keep working.
func TestPutUserRoleAllowsDemotingANonAdmin(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	fake := withPutUserRoleFake(map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"8": {Id: 8, Role: "user"},
	})
	prev := service.MyService
	service.MyService = fakeRepoPutRole{user: fake}
	defer func() { service.MyService = prev }()

	ctx, rec := putUserRoleCtx("8", "7", "guest")
	if err := PutUserRole(ctx); err != nil {
		t.Fatalf("PutUserRole returned an error: %v", err)
	}
	if rec.Code == http.StatusForbidden {
		t.Fatalf("PutUserRole refused an ordinary demotion (status %d, body %q)", rec.Code, rec.Body.String())
	}
	if fake.updateSeen == nil {
		t.Fatal("PutUserRole did not persist the role change")
	}
}

// Baseline: demoting one of two admins (both real "admin" strings) must
// keep working — the pre-existing behavior this guard must not break.
func TestPutUserRoleAllowsDemotingAnAdminWhenAnotherRemains(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	fake := withPutUserRoleFake(map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"9": {Id: 9, Role: "admin"},
	})
	prev := service.MyService
	service.MyService = fakeRepoPutRole{user: fake}
	defer func() { service.MyService = prev }()

	ctx, rec := putUserRoleCtx("9", "7", "user")
	if err := PutUserRole(ctx); err != nil {
		t.Fatalf("PutUserRole returned an error: %v", err)
	}
	if rec.Code == http.StatusForbidden {
		t.Fatalf("PutUserRole refused to demote an admin even though another admin remains "+
			"(status %d, body %q)", rec.Code, rec.Body.String())
	}
	if fake.updateSeen == nil {
		t.Fatal("PutUserRole did not persist the role change")
	}
}
