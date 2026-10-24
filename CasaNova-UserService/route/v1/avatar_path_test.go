package v1

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
)

// avatarUserService keeps one mutable user row so PutUserInfo's write is
// visible to GetUserAvatar's read, as the DB would make it.
type avatarUserService struct {
	mockParityUserService
	row model2.UserDBModel
}

func (f *avatarUserService) GetUserInfoById(id string) model2.UserDBModel { return f.row }
func (f *avatarUserService) GetUserInfoByUserName(n string) model2.UserDBModel {
	return model2.UserDBModel{}
}
func (f *avatarUserService) UpdateUser(u model2.UserDBModel) {
	// mirrors gorm Updates(&m): non-zero fields overwrite the row
	if u.Avatar != "" {
		f.row.Avatar = u.Avatar
	}
	if u.Nickname != "" {
		f.row.Nickname = u.Nickname
	}
}

// withAvatarFixture points UserDataPath at a temp tree, installs a guest row
// with id 9 and returns that tree plus a "secret" file outside it.
func withAvatarFixture(t *testing.T, avatar string) (*avatarUserService, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	prevPath := config.AppInfo.UserDataPath
	config.AppInfo.UserDataPath = dataDir
	t.Cleanup(func() { config.AppInfo.UserDataPath = prevPath })

	secret := filepath.Join(t.TempDir(), "jwt_ec_private.pem")
	if err := os.WriteFile(secret, []byte("SECRET-KEY-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "9"), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := &avatarUserService{row: model2.UserDBModel{Id: 9, Username: "guestuser", Role: "guest", Avatar: avatar}}
	prev := service.MyService
	service.MyService = &mockParityRepo{userSvc: fake}
	t.Cleanup(func() { service.MyService = prev })
	return fake, dataDir, secret
}

func getAvatarBody(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/avatar", nil)
	req.Header.Set("user_id", "9")
	rec := httptest.NewRecorder()
	_ = GetUserAvatar(echo.New().NewContext(req, rec))
	return rec.Body.String()
}

// Any logged-in account (role guest included) could store an arbitrary path
// as its avatar through PUT /v1/users/current and read the file back through
// GET /v1/users/avatar — as root, so the JWT signing key too.
func TestPutUserInfoIgnoresAvatarPath(t *testing.T) {
	fake, _, secret := withAvatarFixture(t, "")

	req := httptest.NewRequest(http.MethodPut, "/v1/users/current", strings.NewReader(`{"avatar":"`+secret+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("user_id", "9")
	if err := PutUserInfo(echo.New().NewContext(req, httptest.NewRecorder())); err != nil {
		t.Fatal(err)
	}
	if fake.row.Avatar == secret {
		t.Fatalf("PutUserInfo stored a caller-supplied avatar path %q", secret)
	}
	if strings.Contains(getAvatarBody(t), "SECRET-KEY-BYTES") {
		t.Fatal("GET /v1/users/avatar returned the contents of a file outside the user's directory")
	}
}

// A path already stored before the fix (or written any other way) must not
// be served when it lies outside the owner's directory.
func TestGetUserAvatarRefusesStoredPathOutsideUserDir(t *testing.T) {
	fake, _, secret := withAvatarFixture(t, "")
	fake.row.Avatar = secret
	if strings.Contains(getAvatarBody(t), "SECRET-KEY-BYTES") {
		t.Fatal("GET /v1/users/avatar served a stored path outside the user's directory")
	}
}

func TestGetUserAvatarRefusesSymlinkOutOfUserDir(t *testing.T) {
	fake, dataDir, secret := withAvatarFixture(t, "")
	link := filepath.Join(dataDir, "9", "avatar.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	fake.row.Avatar = link
	if strings.Contains(getAvatarBody(t), "SECRET-KEY-BYTES") {
		t.Fatal("GET /v1/users/avatar followed a symlink out of the user's directory")
	}
}

func TestGetUserAvatarServesOwnAvatar(t *testing.T) {
	fake, dataDir, _ := withAvatarFixture(t, "")
	own := filepath.Join(dataDir, "9", "avatar.png")
	if err := os.WriteFile(own, []byte("PNG-OWN-AVATAR"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.row.Avatar = own
	if body := getAvatarBody(t); !strings.Contains(body, "PNG-OWN-AVATAR") {
		t.Fatalf("own avatar not served, got %q", body)
	}
}
