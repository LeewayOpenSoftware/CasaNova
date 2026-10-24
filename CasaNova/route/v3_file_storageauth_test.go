package route

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// v3FileFixture puts the storage authority in force over a temp data root and
// returns that root plus a directory OUTSIDE it that the blocked-prefix list
// does not name (a temp dir, like /home or /tmp on a real box).
func v3FileFixture(t *testing.T) (string, string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "DATA")
	outside := t.TempDir()
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	storageauth.SetStateDir(t.TempDir())
	t.Cleanup(func() { storageauth.SetStateDir("/var/lib/casanova") })
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: []string{data}}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return data, outside
}

func getV3File(p storageauth.Principal, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v3/file?path="+url.QueryEscape(path), nil)
	serveV3File(rec, req, p, path)
	return rec
}

var (
	v3Admin = storageauth.Principal{ID: "1", Role: "admin", Admin: true}
	v3User  = storageauth.Principal{ID: "42", Role: "user"}
)

// A plain user with no storage grant is refused by v1 everywhere; /v3/file
// handed them any path the blocked-prefix list did not name.
func TestV3FileAsksTheStorageAuthority(t *testing.T) {
	data, _ := v3FileFixture(t)
	f := filepath.Join(data, "bob", "payroll.xlsx")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("BOB-PAYROLL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := getV3File(v3User, f); strings.Contains(rec.Body.String(), "BOB-PAYROLL") {
		t.Fatalf("a user with no storage grant downloaded %s (status %d)", f, rec.Code)
	}
	if rec := getV3File(v3Admin, f); rec.Code != http.StatusOK || rec.Body.String() != "BOB-PAYROLL" {
		t.Fatalf("admin download of a storage file: status %d body %q", rec.Code, rec.Body.String())
	}
}

// Outside authorized storage nothing is served, admin included: that is what
// the v1 download answers too.
func TestV3FileRefusesPathsOutsideStorage(t *testing.T) {
	_, outside := v3FileFixture(t)
	key := filepath.Join(outside, "id_ed25519")
	if err := os.WriteFile(key, []byte("SSH-PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []storageauth.Principal{v3User, v3Admin} {
		if rec := getV3File(p, key); strings.Contains(rec.Body.String(), "SSH-PRIVATE-KEY") {
			t.Fatalf("%s downloaded %s outside authorized storage", p.Role, key)
		}
	}
}

// A symlink under the data root is judged on where it lands.
func TestV3FileDoesNotFollowSymlinksOutOfStorage(t *testing.T) {
	data, outside := v3FileFixture(t)
	secret := filepath.Join(outside, "shadow")
	if err := os.WriteFile(secret, []byte("ROOT-HASH"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if rec := getV3File(v3Admin, link); strings.Contains(rec.Body.String(), "ROOT-HASH") {
		t.Fatalf("a symlink under the data root was followed out of storage")
	}
}
