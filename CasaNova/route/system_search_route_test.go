package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// The file manager's "Buscar…" box through the real router:
//
//	GET /v1/file/search?root=…&q=…   (CasaNova-UI-v2 src/api/files.ts fileSearch)
//
// with the whole-filesystem search switch the Users & roles page sends as
//
//	PUT /v1/sys/system-search-access/:id {enabled, role}

type searchBody struct {
	Message string `json:"message"`
	Data    struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
		Scope string `json:"scope"`
	} `json:"data"`
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) searchBody {
	t.Helper()
	var b searchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("search body is not JSON: %v: %s", err, rec.Body.String())
	}
	return b
}

func TestFileSearchFromRootNeedsTheSearchSwitchThroughTheRouter(t *testing.T) {
	if _, err := os.Stat("/etc/hostname"); err != nil {
		t.Skip("no /etc/hostname to find here")
	}
	fx := newSysdiskFixture(t)
	admin := fx.token(t, 1, "admin")
	t.Cleanup(func() {
		_ = storageauth.SetSystemSearchAccess("1", "admin", false)
		_ = storageauth.SetSystemDiskAccess("1", "admin", false)
	})
	search := "/v1/file/search?root=/&q=hostname&limit=1"

	// OFF, as today: a search from / is refused, and /etc is refused by the
	// legacy sanitizer.
	if rec := fx.do(t, http.MethodGet, search, admin, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("switches off: search from / = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodGet, "/v1/file/search?root=/etc&q=host", admin, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("switches off: search from /etc = %d %s, want 400", rec.Code, rec.Body.String())
	}

	// The search switch cannot be turned on without system-disk access.
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-search-access/1", admin, `{"enabled":true,"role":"admin"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("search switch on without system-disk access = %d %s, want 400", rec.Code, rec.Body.String())
	}

	// System-disk access ALONE does not widen the search.
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-disk-access/1", admin, `{"enabled":true,"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT system-disk-access = %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodGet, search, admin, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("system-disk only: search from / = %d %s, want 403", rec.Code, rec.Body.String())
	}

	// Both on.
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-search-access/1", admin, `{"enabled":true,"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT system-search-access = %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodGet, "/v1/sys/system-search-access", admin, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"1"`) {
		t.Fatalf("GET system-search-access = %d %s, want id 1", rec.Code, rec.Body.String())
	}

	// THE FEATURE: a search from / finds a file outside authorized storage.
	rec := fx.do(t, http.MethodGet, search, admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("both switches on: search from / = %d %s, want 200", rec.Code, rec.Body.String())
	}
	b := decodeSearch(t, rec)
	if b.Data.Scope != "filesystem" {
		t.Errorf("scope = %q, want filesystem", b.Data.Scope)
	}
	found := false
	for _, h := range b.Data.Hits {
		if h.Path == "/etc/hostname" {
			found = true
		}
		for _, k := range []string{"/proc/", "/sys/", "/dev/"} {
			if strings.HasPrefix(h.Path, k) {
				t.Errorf("hit inside a kernel filesystem: %s", h.Path)
			}
		}
	}
	if !found {
		t.Errorf("search from / for hostname did not find /etc/hostname: %s", rec.Body.String())
	}

	// The withheld credentials are not returned, though their names match.
	rec = fx.do(t, http.MethodGet, "/v1/file/search?root=/etc&q=shadow", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search /etc for shadow = %d %s", rec.Code, rec.Body.String())
	}
	for _, h := range decodeSearch(t, rec).Data.Hits {
		switch h.Path {
		case "/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-":
			t.Errorf("withheld credential returned as a hit: %s", h.Path)
		}
	}

	// A kernel filesystem is not walked even when asked for directly.
	if _, err := os.Stat("/proc/self"); err == nil {
		if rec := fx.do(t, http.MethodGet, "/v1/file/search?root=/proc&q=status", admin, ""); rec.Code == http.StatusOK {
			t.Errorf("search rooted at /proc answered 200: %s", rec.Body.String())
		}
	}

	// Another account does not inherit the switch, and cannot set its own.
	user := fx.token(t, 7, "user")
	if rec := fx.do(t, http.MethodGet, search, user, ""); rec.Code == http.StatusOK {
		t.Errorf("account 7 searched / through account 1's switch: %s", rec.Body.String())
	}
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-search-access/7", user, `{"enabled":true,"role":"user"}`); rec.Code == http.StatusOK {
		t.Errorf("a non-admin turned its own search switch on: %s", rec.Body.String())
	}

	// Turning system-disk access off takes the search with it.
	if rec := fx.do(t, http.MethodPut, "/v1/sys/system-disk-access/1", admin, `{"enabled":false,"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT system-disk-access off = %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodGet, search, admin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("system-disk off, search switch still recorded: search from / = %d, want 403", rec.Code)
	}
}
