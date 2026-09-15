package v1

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/labstack/echo/v4"
)

// withFakeUserDataPath points config.AppInfo.UserDataPath at a fresh
// t.TempDir() for the duration of the test and restores it afterward, so
// GetUserImage's path arithmetic never touches a real filesystem (never
// /var/lib/casaos).
func withFakeUserDataPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := config.AppInfo.UserDataPath
	config.AppInfo.UserDataPath = dir
	t.Cleanup(func() { config.AppInfo.UserDataPath = prev })
	return dir
}

// getUserImageCtx builds a request with NO Authorization and NO user_id
// header -- GetUserImage is registered outside v1Group in InitRouter
// specifically so it can be reached before anyone has a token, and a
// legitimate caller of this route never carries either.
func getUserImageCtx(rawQuery string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/image?"+rawQuery, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// THE BUG. GetUserImage's old contract took an arbitrary `path` query
// param and only checked it fell somewhere under UserDataPath -- the data
// directory of EVERY user, not the caller's (there is no caller: this
// route carries no JWT). PostUserCustomConf/DeleteUserCustomConf write
// each user's own config at <UserDataPath>/<id>/<name>.json in that same
// tree, so any caller who could guess an id and a config name read it
// with zero authentication -- not just images.
//
// Run against the unfixed handler this FAILS (old code answers 200 with
// the secret JSON body). After the fix it PASSES: the `path` query param
// is no longer read at all, so this now looks like a malformed request.
func TestGetUserImage_DoesNotServeArbitraryPathToAnotherUsersConfig(t *testing.T) {
	dir := withFakeUserDataPath(t)
	secretDir := filepath.Join(dir, "2")
	if err := os.MkdirAll(secretDir, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	secretPath := filepath.Join(secretDir, "settings.json")
	const secret = `{"totp_backup_codes":["AAAA-BBBB"]}`
	if err := os.WriteFile(secretPath, []byte(secret), 0o640); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, rec := getUserImageCtx("path=" + url.QueryEscape(secretPath))
	if err := GetUserImage(ctx); err != nil {
		t.Fatalf("GetUserImage returned an error: %v", err)
	}

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "totp_backup_codes") {
		t.Fatalf("GetUserImage served another user's config file with no token at all -- got %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for a request naming no id/key, got %d body %q", rec.Code, rec.Body.String())
	}
}

// THE LEGITIMATE CASE THAT MUST SURVIVE. The pre-login screen shows every
// account's avatar before anyone has a token -- that reachability is
// deliberate (route/v1.go names "image" explicitly among the routes
// registered outside the JWT group), not the bug. A fix that closes the
// route entirely instead of narrowing its contract passes the RED test
// above by satisfying nothing.
func TestGetUserImage_ServesLegitimateAvatarPreLogin(t *testing.T) {
	dir := withFakeUserDataPath(t)
	userDir := filepath.Join(dir, "42")
	if err := os.MkdirAll(userDir, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	avatarPath := filepath.Join(userDir, "avatar.png")
	if err := os.WriteFile(avatarPath, []byte("not a real png, extension is all that's checked here"), 0o640); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, rec := getUserImageCtx("id=42&key=avatar")
	if err := GetUserImage(ctx); err != nil {
		t.Fatalf("GetUserImage returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate pre-login avatar fetch by id+key should succeed, got %d body %q", rec.Code, rec.Body.String())
	}
}

// Even once a caller supplies a syntactically valid id, the handler must
// still refuse to serve a file that isn't a recognized image extension --
// otherwise `key=settings` against a real user id would still reach
// settings.json, just through the new contract instead of the old one.
func TestGetUserImage_RejectsNonImageKeyEvenForARealUser(t *testing.T) {
	dir := withFakeUserDataPath(t)
	userDir := filepath.Join(dir, "2")
	if err := os.MkdirAll(userDir, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "settings.json"), []byte(`{"secret":true}`), 0o640); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, rec := getUserImageCtx("id=2&key=settings")
	if err := GetUserImage(ctx); err != nil {
		t.Fatalf("GetUserImage returned an error: %v", err)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("GetUserImage must not serve a non-image file even when id+key resolve to it, got 200 body %q", rec.Body.String())
	}
}

// The key must not be allowed to smuggle a path separator or "..": it is
// concatenated directly into a filesystem path, so it has to be validated
// BEFORE that concatenation, not cleaned after.
func TestGetUserImage_RejectsKeyWithPathSeparators(t *testing.T) {
	withFakeUserDataPath(t)

	ctx, rec := getUserImageCtx("id=2&key=" + url.QueryEscape("../../etc/passwd"))
	if err := GetUserImage(ctx); err != nil {
		t.Fatalf("GetUserImage returned an error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for a key containing path separators, got %d body %q", rec.Code, rec.Body.String())
	}
}
