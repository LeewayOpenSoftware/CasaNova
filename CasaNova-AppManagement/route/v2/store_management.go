package v2

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// StoreList returns all registered stores from stores.json.
// GET /v2/app_management/stores
func (a *AppManagement) StoreList(ctx echo.Context) error {
	// Wave 34 (ported from 27.04, docs/design/per-app-grants.md §7): this
	// handler had NO authorization gate at all — not the wrong bit, none.
	// The router's JWT middleware still refused an anonymous caller, so it
	// was AUTHENTICATED but UNAUTHORIZED: every logged-in account reached
	// it, and so did any context that resolved to guest without a principal
	// behind it. PermAppView is the bit the whole family gets — guest
	// already holds it, so no role loses anything it has today.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	registry := service.LoadStoreRegistry()
	stores := registry.ListStores()
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data":    stores,
	})
}

// StoreRegister adds a new store entry to the registry.
// POST /v2/app_management/stores
//
// Wave 25: registering a store points the entire box at a remote catalog
// that the resulting installs trust by default. PermAppInstall lets a
// regular user choose where the apps come from — that's a system-level
// decision, not an application-level one. Restrict to admin (PermSystemConfig).
func (a *AppManagement) StoreRegister(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	var entry service.StoreEntry
	if err := ctx.Bind(&entry); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid request body: " + err.Error(),
		})
	}

	if entry.ID == "" || entry.Name == "" || entry.Path == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "id, name, and path are required",
		})
	}

	// Prevent path traversal — only allow relative paths that stay inside AppStorePath.
	cleanPath := filepath.Clean(entry.Path)
	if strings.Contains(cleanPath, "..") || filepath.IsAbs(cleanPath) {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "path must be relative and must not contain '..'",
		})
	}
	entry.Path = cleanPath

	// Validate URL for remote stores to prevent SSRF.
	if entry.URL != "" {
		if err := validateStoreURL(entry.URL); err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"success": 400,
				"message": "invalid store URL: " + err.Error(),
			})
		}
	}

	if entry.Type == "" {
		entry.Type = "local"
	}

	registry := service.LoadStoreRegistry()
	if err := registry.RegisterStore(entry); err != nil {
		status := http.StatusConflict
		if err != service.ErrStoreAlreadyExists {
			status = http.StatusInternalServerError
		}
		return ctx.JSON(status, map[string]interface{}{
			"success": status,
			"message": err.Error(),
		})
	}

	logger.Info("registered new app store", zap.String("id", entry.ID), zap.String("type", entry.Type))
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data":    entry,
	})
}

// StoreUpdate modifies an existing store's mutable fields.
// PUT /v2/app_management/stores/:id
//
// Wave 25: same admin-only requirement as StoreRegister — re-pointing an
// existing store at a hostile URL is identical in impact to creating a
// new one.
func (a *AppManagement) StoreUpdate(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "store id is required",
		})
	}

	var update service.StoreEntry
	if err := ctx.Bind(&update); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid request body: " + err.Error(),
		})
	}

	registry := service.LoadStoreRegistry()
	if err := registry.UpdateStore(id, update); err != nil {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"success": 404,
			"message": err.Error(),
		})
	}

	logger.Info("updated app store", zap.String("id", id))
	updated, _ := registry.GetStore(id)
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data":    updated,
	})
}

// StoreUnregister removes a store from the registry.
// DELETE /v2/app_management/stores/:id
//
// Wave 25: removing the trusted store registry is also admin-only —
// otherwise a non-admin could delete the only known-good source and force
// fall-back to attacker-controlled stores.
func (a *AppManagement) StoreUnregister(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "store id is required",
		})
	}

	registry := service.LoadStoreRegistry()
	if err := registry.UnregisterStore(id); err != nil {
		status := http.StatusNotFound
		if id == "default" {
			status = http.StatusForbidden
		}
		return ctx.JSON(status, map[string]interface{}{
			"success": status,
			"message": err.Error(),
		})
	}

	logger.Info("unregistered app store", zap.String("id", id))
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"message": "store unregistered",
	})
}

// StoreSync forces a re-sync of a remote store. For local stores
// this just rebuilds the catalog.
// POST /v2/app_management/stores/:id/sync
func (a *AppManagement) StoreSync(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "store id is required",
		})
	}

	registry := service.LoadStoreRegistry()
	store, err := registry.GetStore(id)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"success": 404,
			"message": err.Error(),
		})
	}

	// For remote stores, trigger the download+extract. For local stores,
	// just rebuild the catalog (which happens on next Catalog() call).
	if store.Type == "remote" && store.URL != "" {
		// Validate the URL to prevent SSRF before making any outbound request.
		if err := validateStoreURL(store.URL); err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"success": 400,
				"message": "invalid store URL: " + err.Error(),
			})
		}

		if err := service.SyncRemoteStore(store); err != nil {
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"success": 500,
				"message": "sync failed: " + err.Error(),
			})
		}
	}

	registry.MarkSynced(id)
	logger.Info("synced app store", zap.String("id", id), zap.String("type", store.Type))
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"message": "store synced",
	})
}

// validateStoreURL checks that a store URL uses an allowed scheme (http/https)
// and does not resolve to a private/reserved IP address (SSRF protection).
func validateStoreURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must contain a host")
	}

	// Resolve the hostname and reject private/reserved IPs.
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if isPrivateIP(ip) {
			return fmt.Errorf("host %q resolves to private/reserved IP %s", host, ipStr)
		}
	}

	return nil
}

// isPrivateIP returns true if the IP falls in a private or reserved range
// that should not be reachable from externally-supplied URLs.
func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
