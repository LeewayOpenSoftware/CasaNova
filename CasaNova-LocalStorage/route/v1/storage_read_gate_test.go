package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// This module's Wave 26 hardening pass gated every WRITE route
// (PostAddStorage, PutFormatStorage, DeleteStorage, DeleteDisksUmount,
// UmountStorage, DeleteDiskUSB, PutSystemUSBAutoMount) behind PermStorageWrite
// but never touched the matching reads: they had no permission check at all,
// so any authenticated caller — including guest — could enumerate the full
// disk/partition/serial inventory, USB drives, storage mounts, cloud-storage
// mounts, and driver info. This asserts each read now denies a caller
// without PermStorageRead, matching the read/write split already used by
// this module's own siblings (and by CasaNova/route/v1's samba.go/smart.go/
// smb.go). Grouped as one test over the whole read-route family rather than
// one test per route, per the fix mandate.
//
// service.MyService is nil in this test binary, so a handler that reached
// past its gate would panic instead of returning a clean 403 — the absence
// of a panic here is itself part of the proof that the gate runs first.
func TestStorageReadRoutesDenyNonAdminRoles(t *testing.T) {
	routes := []struct {
		name    string
		method  string
		path    string
		handler echo.HandlerFunc
	}{
		{"GetDiskList", http.MethodGet, "/v1/disks", GetDiskList},
		{"GetDiskSize", http.MethodGet, "/v1/disks/size?path=/mnt/x", GetDiskSize},
		{"GetDisksUSBList", http.MethodGet, "/v1/disks/usb", GetDisksUSBList},
		{"GetSystemUSBAutoMount", http.MethodGet, "/v1/usb/usb-auto-mount", GetSystemUSBAutoMount},
		{"GetStorageList", http.MethodGet, "/v1/storage", GetStorageList},
		{"ListStorages", http.MethodGet, "/v1/cloud", ListStorages},
		{"ListDriverInfo", http.MethodGet, "/v1/driver", ListDriverInfo},
	}

	// "user" and "admin" both hold PermStorageRead (see rbac.go), so they
	// belong on the other side of this gate — asserted separately below via
	// requirePerm directly, without invoking the full handler, which would
	// otherwise reach the nil service.MyService in this test binary.
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			for _, role := range []string{"", "guest"} {
				t.Run(role, func(t *testing.T) {
					e := echo.New()
					rec := httptest.NewRecorder()
					ctx := e.NewContext(httptest.NewRequest(rt.method, rt.path, nil), rec)
					if role != "" {
						commonmw.SetRoleInContext(ctx, role, 7)
					}

					err := rt.handler(ctx)
					if err == nil {
						t.Fatalf("%s/role=%q: expected a refusal, got nil (service.MyService would have panicked if reached)", rt.name, role)
					}
					he, ok := err.(*echo.HTTPError)
					if !ok {
						t.Fatalf("%s/role=%q: want *echo.HTTPError, got %T (%v)", rt.name, role, err, err)
					}
					if he.Code != http.StatusForbidden {
						t.Fatalf("%s/role=%q got %d, want 403", rt.name, role, he.Code)
					}
					if rec.Body.Len() != 0 {
						t.Fatalf("%s/role=%q: the gate wrote the response itself: %q", rt.name, role, rec.Body.String())
					}
				})
			}
		})
	}
}

// The gate must not be stricter than the module's own write-side scheme
// either: "user" and "admin" both hold PermStorageRead and must pass.
func TestStorageReadRoutesAllowRolesHoldingPermStorageRead(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			e := echo.New()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/disks", nil), httptest.NewRecorder())
			commonmw.SetRoleInContext(ctx, role, 7)
			if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
				t.Fatalf("role %q holding PermStorageRead was refused: %v", role, err)
			}
		})
	}
}
