package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	dbmodel "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
)

// fakeUserServiceLastAdmin implements only what DeleteUser calls. Embedding
// the real interface satisfies the compiler for anything else and panics
// loudly (nil pointer) if DeleteUser starts calling a method this fake
// doesn't expect.
type fakeUserServiceLastAdmin struct {
	service.UserService
	roster       []dbmodel.UserDBModel
	byID         map[string]dbmodel.UserDBModel
	deleteCalled bool
}

func (f *fakeUserServiceLastAdmin) GetAllUserName() []dbmodel.UserDBModel { return f.roster }

func (f *fakeUserServiceLastAdmin) GetUserAllInfoById(id string) dbmodel.UserDBModel {
	return f.byID[id]
}

func (f *fakeUserServiceLastAdmin) DeleteUserById(id string) {
	f.deleteCalled = true
}

type fakeRepoLastAdmin struct {
	service.Repository
	user *fakeUserServiceLastAdmin
}

func (f fakeRepoLastAdmin) User() service.UserService { return f.user }

func deleteUserCtx(targetID, authedID string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/user/delete/"+targetID, nil)
	req.Header.Set("user_id", authedID)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	ctx.SetParamNames("id")
	ctx.SetParamValues(targetID)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 7)
	return ctx, rec
}

func withDeleteUserFake(t *testing.T, byID map[string]dbmodel.UserDBModel) *fakeUserServiceLastAdmin {
	t.Helper()
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "revoked.json"))
	roster := make([]dbmodel.UserDBModel, 0, len(byID))
	for _, u := range byID {
		roster = append(roster, u)
	}
	fake := &fakeUserServiceLastAdmin{roster: roster, byID: byID}
	prev := service.MyService
	service.MyService = fakeRepoLastAdmin{user: fake}
	t.Cleanup(func() { service.MyService = prev })
	return fake
}

// THE GAP THE REVIEW FOUND: a sole admin stored as the legacy string "administrator"
// (not literally "admin") must still be protected — resolveEffectiveRole
// mints an admin token for that row, so it IS the box's only admin
// operationally, even though a literal role=='admin' comparison would never
// see it.
func TestDeleteUserRefusesToDeleteTheLastAdminEvenWithLegacyRoleString(t *testing.T) {
	fake := withDeleteUserFake(t, map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "administrator"},
	})
	ctx, rec := deleteUserCtx("7", "7") // self-delete, the sole (legacy-role) admin
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if fake.deleteCalled {
		t.Fatal("DeleteUser deleted the box's only admin account because its Role column said " +
			"\"administrator\" instead of \"admin\" — resolveEffectiveRole mints an admin token " +
			"for this row regardless")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// THE MIRROR FAILURE MODE: two real admins, one of them a legacy row, must
// NOT collapse into "one admin" and refuse a legitimate delete.
func TestDeleteUserAllowsDeletingAnAdminWhenAnotherIsALegacyAdminRow(t *testing.T) {
	fake := withDeleteUserFake(t, map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"9": {Id: 9, Role: "administrator"}, // a second, legacy-stored admin
	})
	ctx, _ := deleteUserCtx("9", "7")
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeleteUser refused to delete an admin account even though another admin " +
			"(a legacy \"administrator\" row) remains")
	}
}

// Baseline: the ordinary case (both admins stored as literal "admin") must
// keep working exactly as before.
func TestDeleteUserAllowsDeletingAnAdminWhenOthersRemain(t *testing.T) {
	fake := withDeleteUserFake(t, map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"9": {Id: 9, Role: "admin"},
	})
	ctx, _ := deleteUserCtx("9", "7")
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeleteUser refused to delete an admin account even though another admin remains")
	}
}

// Positive control: deleting a non-admin must never trip the last-admin
// guard, regardless of how many admins exist.
func TestDeleteUserAllowsDeletingWhenNotTheLastAdmin(t *testing.T) {
	fake := withDeleteUserFake(t, map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"8": {Id: 8, Role: "user"},
	})
	ctx, _ := deleteUserCtx("8", "7")
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeleteUser refused to delete a non-admin target; the last-admin check must not apply to it")
	}
}
