package route

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/glebarez/sqlite"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// THE EXACT CALL THE FILE EXPLORER MAKES, through the router this service
// serves. The handler-level tests in route/v1 call readPath with a hand-built
// context; this one goes through what a browser goes through: the v1 group's
// JWT middleware (real ES256 token, verified against the JWKS the user service
// publishes, checked against the revocation authority), the PUT the Users &
// roles switch sends, and then
//
//	GET /v1/folder?path=/        (CasaNova-UI-v2 src/api/files.ts listDir)
//
// It exists because a human with the switch on still saw
// "No se pudo abrir esta carpeta / OUTSIDE_AUTHORIZED_STORAGE" on "/". If the
// identity the middleware stashes ever stops being the identity the switch is
// keyed on, the handler tests stay green and this one goes red.

type sysdiskRepo struct {
	service.Repository
	system service.SystemService
	shares service.SharesService
}

func (r sysdiskRepo) System() service.SystemService { return r.system }
func (r sysdiskRepo) Shares() service.SharesService { return r.shares }

// One signing key for the whole package: external.GetPublicKey caches the key
// it fetched, so a fresh key per fixture would be refused by the second test.
var (
	sysdiskKeyOnce sync.Once
	sysdiskKey     *ecdsa.PrivateKey
	sysdiskKeyErr  error
)

func sysdiskSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	sysdiskKeyOnce.Do(func() { sysdiskKey, sysdiskKeyErr = ecdsa.GenerateKey(elliptic.P256(), rand.Reader) })
	if sysdiskKeyErr != nil {
		t.Fatal(sysdiskKeyErr)
	}
	return sysdiskKey
}

type sysdiskFixture struct {
	router http.Handler
	key    *ecdsa.PrivateKey
}

func newSysdiskFixture(t *testing.T) *sysdiskFixture {
	t.Helper()
	dir := t.TempDir()

	// Storage authority with one data root that is NOT "/", so "/" is
	// OUTSIDE_AUTHORIZED_STORAGE unless the switch opens it.
	dataRoot := filepath.Join(dir, "DATA")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	storageauth.SetStateDir(filepath.Join(dir, "storageauth"))
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: []string{dataRoot}}); err != nil {
		t.Fatalf("provision storage authority: %v", err)
	}

	tokenrevocation.SetPath(filepath.Join(dir, "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	// The user service's JWKS, found the way production finds it: through
	// <RuntimePath>/user-service.url.
	key := sysdiskSigningKey(t)
	jwks, err := commonjwt.GenerateJwksJSON(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/"+commonjwt.JWKSPath, commonjwt.JWKSHandler(jwks))
	us := httptest.NewServer(mux)
	t.Cleanup(us.Close)
	runtime := filepath.Join(dir, "run")
	if err := os.MkdirAll(runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, external.UserServiceAddressFilename), []byte(us.URL), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRuntime := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = prevRuntime })

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(dir, "casaos.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(model2.SharesDBModel{}); err != nil {
		t.Fatal(err)
	}
	prev := service.MyService
	service.MyService = sysdiskRepo{system: service.NewSystemService(), shares: service.NewSharesService(db)}
	t.Cleanup(func() { service.MyService = prev })

	return &sysdiskFixture{router: InitV1Router(), key: key}
}

func (fx *sysdiskFixture) token(t *testing.T, id int, role string) string {
	t.Helper()
	tok, err := commonjwt.GenerateToken("u"+strconv.Itoa(id), fx.key, id, role, "casaos", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (fx *sysdiskFixture) do(t *testing.T, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderAuthorization, token)
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	// The gateway's hop, as in production.
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	return rec
}

func folderNames(t *testing.T, rec *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var out struct {
		Data struct {
			Content []struct {
				Name string `json:"name"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("listing body is not JSON: %v: %s", err, rec.Body.String())
	}
	names := map[string]bool{}
	for _, e := range out.Data.Content {
		names[e.Name] = true
	}
	return names
}

func TestFileExplorerOpensRootWhenSwitchIsOnThroughTheRouter(t *testing.T) {
	fx := newSysdiskFixture(t)
	admin := fx.token(t, 1, "admin")

	// Switch OFF: the explorer's call on "/" is the signature the human saw.
	rec := fx.do(t, http.MethodGet, "/v1/folder?path=/", admin, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(storageauth.ReasonOutsideAuthorizedStorage)) {
		t.Fatalf("precondition, switch off: GET /v1/folder?path=/ = %d %s, want 403 %s",
			rec.Code, rec.Body.String(), storageauth.ReasonOutsideAuthorizedStorage)
	}

	// The Users & roles switch, exactly as CasaNova-UI-v2 stores/users.ts sends it.
	rec = fx.do(t, http.MethodPut, "/v1/sys/system-disk-access/1", admin, `{"enabled":true,"role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /v1/sys/system-disk-access/1 = %d %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { _ = storageauth.SetSystemDiskAccess("1", "admin", false) })

	// And it reads back the way the switch renders it.
	rec = fx.do(t, http.MethodGet, "/v1/sys/system-disk-access", admin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"1"`) {
		t.Fatalf("GET /v1/sys/system-disk-access = %d %s, want id 1 listed", rec.Code, rec.Body.String())
	}

	// THE BUG: the same call, same token, now lists "/".
	rec = fx.do(t, http.MethodGet, "/v1/folder?path=/", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("switch on: GET /v1/folder?path=/ = %d %s, want 200", rec.Code, rec.Body.String())
	}
	names := folderNames(t, rec)
	for _, want := range []string{"etc", "usr"} {
		if _, err := os.Stat("/" + want); err == nil && !names[want] {
			t.Errorf("listing of / is missing %q: %v", want, names)
		}
	}

	// And a folder the legacy blocked-prefix list used to answer 400 for.
	if _, err := os.Stat("/etc"); err == nil {
		rec = fx.do(t, http.MethodGet, "/v1/folder?path=/etc", admin, "")
		if rec.Code != http.StatusOK {
			t.Errorf("switch on: GET /v1/folder?path=/etc = %d %s, want 200", rec.Code, rec.Body.String())
		}
	}

	// Read-only: creating a folder under "/" is still refused.
	rec = fx.do(t, http.MethodPost, "/v1/folder", admin, `{"path":"/cn-sysdisk-route-probe"}`)
	if rec.Code == http.StatusOK {
		_ = os.Remove("/cn-sysdisk-route-probe")
		t.Errorf("switch on: POST /v1/folder under / answered 200; the switch must stay read-only")
	}
}

// Another account's switch never opens "/" for this one, and a guest's PUT is
// refused before it can be recorded.
func TestFileExplorerRootStaysClosedForOtherAccountsThroughTheRouter(t *testing.T) {
	fx := newSysdiskFixture(t)
	admin := fx.token(t, 1, "admin")
	user := fx.token(t, 7, "user")

	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-disk-access/1", admin, `{"enabled":true,"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT switch for 1 = %d %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { _ = storageauth.SetSystemDiskAccess("1", "admin", false) })

	rec := fx.do(t, http.MethodGet, "/v1/folder?path=/", user, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("account 7 listed / through account 1's switch: %s", rec.Body.String())
	}
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-disk-access/7", user, `{"enabled":true,"role":"user"}`); rec.Code == http.StatusOK {
		t.Fatalf("a non-admin turned its own switch on: %s", rec.Body.String())
	}
}
