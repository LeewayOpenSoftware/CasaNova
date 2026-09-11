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

// OLA 3 — duplicated security decision that was never actually duplicated:
// CasaNova-UI-v2/src/stores/users.ts:194 refuses to delete the box's last
// admin account, with a comment claiming "the backend has the same check".
// DeleteUser (this file's sibling, user.go) carried no such check at all —
// any caller reaching the API directly (not through the UI) could delete the
// sole admin account and permanently lock the box out of every
// PermUserManage-gated capability.

// fakeUserServiceLastAdmin implements only what DeleteUser calls. Embedding
// the real interface satisfies the compiler for anything else and panics
// loudly (nil pointer) if DeleteUser starts calling a method this fake
// doesn't expect — the same anti-vacuity shape used elsewhere in this repo's
// gate tests.
type fakeUserServiceLastAdmin struct {
	service.UserService
	target       dbmodel.UserDBModel
	adminCount   int64
	deleteCalled bool
}

func (f *fakeUserServiceLastAdmin) GetUserAllInfoById(id string) dbmodel.UserDBModel {
	return f.target
}

func (f *fakeUserServiceLastAdmin) CountUsersByRole(role string) int64 {
	if role == string(rbac.RoleAdmin) {
		return f.adminCount
	}
	return 0
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

func TestDeleteUserRefusesToDeleteTheLastAdmin(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "revoked.json"))

	fakeUser := &fakeUserServiceLastAdmin{
		target:     dbmodel.UserDBModel{Id: 7, Role: string(rbac.RoleAdmin)},
		adminCount: 1,
	}
	prev := service.MyService
	service.MyService = fakeRepoLastAdmin{user: fakeUser}
	defer func() { service.MyService = prev }()

	ctx, rec := deleteUserCtx("7", "7") // self-delete, the sole admin
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if fakeUser.deleteCalled {
		t.Fatal("DeleteUser deleted the box's only admin account. The frontend already refuses " +
			"this with a comment claiming the backend has the same check — it did not, until now.")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// Positive control: deleting a non-admin, or an admin when there are others
// left, must still work — a last-admin guard that refuses every delete is
// not a guard, it's a lockout of its own.
func TestDeleteUserAllowsDeletingWhenNotTheLastAdmin(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "revoked.json"))

	fakeUser := &fakeUserServiceLastAdmin{
		target:     dbmodel.UserDBModel{Id: 8, Role: string(rbac.RoleUser)},
		adminCount: 1,
	}
	prev := service.MyService
	service.MyService = fakeRepoLastAdmin{user: fakeUser}
	defer func() { service.MyService = prev }()

	ctx, _ := deleteUserCtx("8", "7") // admin deleting a different, non-admin account
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if !fakeUser.deleteCalled {
		t.Fatal("DeleteUser refused to delete a non-admin target; the last-admin check must not apply to it")
	}
}

func TestDeleteUserAllowsDeletingAnAdminWhenOthersRemain(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "revoked.json"))

	fakeUser := &fakeUserServiceLastAdmin{
		target:     dbmodel.UserDBModel{Id: 9, Role: string(rbac.RoleAdmin)},
		adminCount: 2, // caller + target, so deleting target still leaves one
	}
	prev := service.MyService
	service.MyService = fakeRepoLastAdmin{user: fakeUser}
	defer func() { service.MyService = prev }()

	ctx, _ := deleteUserCtx("9", "7")
	if err := DeleteUser(ctx); err != nil {
		t.Fatalf("DeleteUser returned an error: %v", err)
	}
	if !fakeUser.deleteCalled {
		t.Fatal("DeleteUser refused to delete an admin account even though another admin remains")
	}
}
