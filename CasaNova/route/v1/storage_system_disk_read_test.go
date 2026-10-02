package v1

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/labstack/echo/v4"
)

// The file API side of the system-disk switch. The authority already allowed
// "/" for a switched-on account; these pin that the HANDLERS let it through —
// before this, the legacy blocked-prefix sanitizer answered 400 for /etc, /usr,
// /var… and validatePath asked for read|write, which a READ switch never
// widens, so even "/" answered 403.

func switchOnFor(t *testing.T, id, role string) {
	t.Helper()
	if err := storageauth.SetSystemDiskAccess(id, role, true); err != nil {
		t.Fatalf("SetSystemDiskAccess(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = storageauth.SetSystemDiskAccess(id, role, false) })
}

func httpOutcome(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		return -1, err.Error()
	}
	if r, ok := he.Message.(model.Result); ok {
		return he.Code, r.Message
	}
	return he.Code, ""
}

// THE BUG: switched on, an administrator lists and reads the system disk.
func TestSystemDiskReadPathOpensTheSystemDisk(t *testing.T) {
	switchOnFor(t, "1", "admin")

	for _, path := range []string{"/", "/etc", "/usr", "/var", "/boot", "/home", "/root", "/proc", "/sys"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, _, err := readPath(ctxWith(rbac.RoleAdmin, 1), path, true); err != nil {
			code, msg := httpOutcome(err)
			t.Errorf("listing %s with the switch on: %d %s", path, code, msg)
		}
	}
	if _, err := os.Stat("/etc/hostname"); err == nil {
		if _, d, err := readPath(ctxWith(rbac.RoleAdmin, 1), "/etc/hostname", false); err != nil {
			code, msg := httpOutcome(err)
			t.Errorf("reading /etc/hostname with the switch on: %d %s", code, msg)
		} else if d.Reason != storageauth.ReasonAllowedSystemDisk {
			t.Errorf("/etc/hostname reason = %q, want %q", d.Reason, storageauth.ReasonAllowedSystemDisk)
		}
	}
}

// RULE: the switch is read-only at the handler too. Every write path still
// goes through validatePath / authorizePath with a mutating op, and both still
// refuse /etc for a switched-on administrator.
func TestSystemDiskSwitchOpensNoWriteHandler(t *testing.T) {
	switchOnFor(t, "1", "admin")
	c := ctxWith(rbac.RoleAdmin, 1)

	if _, err := validatePath(c, "/etc/hostname"); err == nil {
		t.Error("validatePath (the write handlers' check) accepted /etc/hostname with the switch on")
	}
	for _, op := range []storageauth.Op{storageauth.OpWrite, storageauth.OpCreate, storageauth.OpDelete, storageauth.OpUpload} {
		if _, err := authorizePath(c, "/etc/hostname", op); err == nil {
			t.Errorf("%s on /etc/hostname accepted with the switch on", op)
		}
	}
}

// RULE: switch OFF, readPath is validatePath — same status, same message.
// Guest with a stale record too: the switch never applies to guest.
func TestSystemDiskReadPathUnchangedWhenOff(t *testing.T) {
	// Account 3 was switched on as a user and is a guest now.
	switchOnFor(t, "3", "user")

	cases := []struct {
		role rbac.Role
		uid  int
	}{{rbac.RoleAdmin, 1}, {rbac.RoleUser, 2}, {rbac.RoleGuest, 3}}
	for _, cs := range cases {
		for _, path := range []string{"/", "/etc", "/etc/hostname", "/proc", "/var/lib/casaos", ""} {
			for _, listing := range []bool{true, false} {
				_, _, got := readPath(ctxWith(cs.role, cs.uid), path, listing)
				_, want := validatePath(ctxWith(cs.role, cs.uid), path)
				gc, gm := httpOutcome(got)
				wc, wm := httpOutcome(want)
				if gc != wc || gm != wm {
					t.Errorf("%s %q listing=%v: readPath = %d %s, validatePath = %d %s",
						cs.role, path, listing, gc, gm, wc, wm)
				}
			}
		}
	}
}

// RULE: a FOLDER outside authorized storage is not archived — the archive
// walks without per-file checks and would carry /proc/kcore and the signing
// key — while a single file there downloads.
func TestSystemDiskDownloadRefusesFolderArchives(t *testing.T) {
	switchOnFor(t, "1", "admin")

	get := func(files string) int {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/v1/file/download?format=zip&files="+url.QueryEscape(files), nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.Set(commonmw.ContextKeyRole, rbac.RoleAdmin)
		c.Set(commonmw.ContextKeyUserID, 1)
		err := GetDownloadFile(c)
		code, _ := httpOutcome(err)
		if err == nil {
			return rec.Code
		}
		return code
	}
	if code := get("/etc"); code != http.StatusForbidden {
		t.Errorf("archiving /etc through the switch: status %d, want 403", code)
	}
	if _, err := os.Stat("/etc/hostname"); err == nil {
		if code := get("/etc/hostname"); code != http.StatusOK {
			t.Errorf("downloading /etc/hostname through the switch: status %d, want 200", code)
		}
	}
}
