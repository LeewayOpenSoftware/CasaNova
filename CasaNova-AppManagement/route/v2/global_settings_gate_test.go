package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/labstack/echo/v4"
)

// GetGlobalSettings and GetGlobalSetting return global environment variables
// that are injected into every compose container — including sensitive credentials
// such as OPENAI_API_KEY. They must not answer any caller who lacks PermAppConfigure.
//
// The tests below assert that:
// 1. Unauthenticated callers, guests, and normal users are refused with 403
//    (echo.HTTPError with StatusForbidden) before reading config.Global.
// 2. An authenticated admin principal is permitted (positive control).
func TestGlobalSettingsDeniesNonAdminRoles(t *testing.T) {
	config.GlobalMu.Lock()
	config.Global["OPENAI_API_KEY"] = "sk-testsecretkey12345"
	config.GlobalMu.Unlock()

	defer func() {
		config.GlobalMu.Lock()
		delete(config.Global, "OPENAI_API_KEY")
		config.GlobalMu.Unlock()
	}()

	a := &AppManagement{}

	for _, role := range []string{"", "guest", "user"} {
		t.Run("GetGlobalSettings/"+role, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/global", nil), rec)
			if role != "" {
				commonmw.SetRoleInContext(ctx, role, 7)
			}

			err := a.GetGlobalSettings(ctx)
			if err == nil {
				t.Fatalf("role %q was allowed to read global settings (status %d); want 403", role, rec.Code)
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("want *echo.HTTPError, got %T (%v)", err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403", role, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("the gate wrote the response itself: %q", rec.Body.String())
			}
		})

		t.Run("GetGlobalSetting/"+role, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/global/OPENAI_API_KEY", nil), rec)
			if role != "" {
				commonmw.SetRoleInContext(ctx, role, 7)
			}

			err := a.GetGlobalSetting(ctx, "OPENAI_API_KEY")
			if err == nil {
				t.Fatalf("role %q was allowed to read OPENAI_API_KEY (status %d); want 403", role, rec.Code)
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("want *echo.HTTPError, got %T (%v)", err, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403", role, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("the gate wrote the response itself: %q", rec.Body.String())
			}
		})
	}
}

func TestGlobalSettingsAllowsAdmin(t *testing.T) {
	config.GlobalMu.Lock()
	config.Global["OPENAI_API_KEY"] = "sk-testsecretkey12345"
	config.GlobalMu.Unlock()

	defer func() {
		config.GlobalMu.Lock()
		delete(config.Global, "OPENAI_API_KEY")
		config.GlobalMu.Unlock()
	}()

	a := &AppManagement{}

	t.Run("GetGlobalSettings/admin", func(t *testing.T) {
		e := echo.New()
		rec := httptest.NewRecorder()
		ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/global", nil), rec)
		commonmw.SetRoleInContext(ctx, "admin", 1)

		err := a.GetGlobalSettings(ctx)
		if err != nil {
			t.Fatalf("admin was denied: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp codegen.GlobalSettingsOK
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Data == nil || len(*resp.Data) == 0 {
			t.Fatal("expected at least 1 global setting in response")
		}
	})

	t.Run("GetGlobalSetting/admin", func(t *testing.T) {
		e := echo.New()
		rec := httptest.NewRecorder()
		ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/app_management/global/OPENAI_API_KEY", nil), rec)
		commonmw.SetRoleInContext(ctx, "admin", 1)

		err := a.GetGlobalSetting(ctx, "OPENAI_API_KEY")
		if err != nil {
			t.Fatalf("admin was denied: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp codegen.GlobalSettingOK
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Data == nil || resp.Data.Value != "sk-testsecretkey12345" {
			t.Fatalf("expected value sk-testsecretkey12345, got %v", resp.Data)
		}
	})
}
