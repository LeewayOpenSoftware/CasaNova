package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/utils/downloadHelper"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// NOTE: json is a package-level var in service.go (jsoniter), not encoding/json.
// All json.Unmarshal / json.MarshalIndent calls use that binding.

// StoreEntry represents one app store in the registry. Local stores are
// directories already on disk; remote stores are zip URLs that get
// downloaded and extracted into their path.
type StoreEntry struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Type              string  `json:"type"` // "local" or "remote"
	URL               string  `json:"url,omitempty"`
	Path              string  `json:"path"` // relative to AppStorePath
	Enabled           bool    `json:"enabled"`
	Priority          int     `json:"priority"` // lower = higher priority
	SyncIntervalHours int     `json:"sync_interval_hours,omitempty"`
	LastSynced        *string `json:"last_synced"` // ISO timestamp, null if never
}

// StoreRegistry holds the complete store list and provides thread-safe
// read/write access to stores.json.
type StoreRegistry struct {
	Stores []StoreEntry `json:"stores"`
	mu     sync.RWMutex

	// Memoized merged catalog. Building it walks every enabled store's Apps
	// tree and parses every docker-compose.yml with compose-go — ~150 full
	// YAML loads for the default stores. Callers hit MergedCatalog inside
	// per-app loops, so an uncached build turned one request into thousands
	// of parses. Invalidated explicitly on every mutation that can change
	// what a store contains (register/unregister/update/sync).
	catalogMu    sync.RWMutex
	catalogCache map[string]*ComposeApp
}

// invalidateCatalog drops the memoized merged catalog. Call after ANY change
// to the set of stores or to their on-disk contents.
func (r *StoreRegistry) invalidateCatalog() {
	r.catalogMu.Lock()
	r.catalogCache = nil
	r.catalogMu.Unlock()
}

var (
	registry     *StoreRegistry
	registryOnce sync.Once

	ErrStoreNotFound      = fmt.Errorf("store not found")
	ErrStoreAlreadyExists = fmt.Errorf("store already exists")
)

// LoadStoreRegistry reads stores.json from the appstore directory. If the
// file doesn't exist, it creates a default registry with just the built-in
// "default" store entry.
//
// Wave 35-Q: previously a malformed stores.json (truncated by a power
// loss mid-write, or corrupted by a concurrent writer in AppLibrary
// pointing at the same path) was silently replaced with defaultStores()
// — the user's entire third-party store list disappeared and only the
// built-in "default" survived. The sysadmin would not notice until they
// tried to install something they remembered was there. We now refuse
// to overwrite a corrupt file: the bad copy is renamed to .corrupt
// alongside, the registry is loaded from the most recent .bak if one
// exists, and only as a last resort do we fall back to defaults.
func LoadStoreRegistry() *StoreRegistry {
	registryOnce.Do(func() {
		registry = &StoreRegistry{}
		storesFile := filepath.Join(config.AppInfo.AppStorePath, common.StoresFileName)

		if file.Exists(storesFile) {
			buf := file.ReadFullFile(storesFile)
			if err := json.Unmarshal(buf, registry); err != nil {
				logger.Error("stores.json is corrupt — quarantining and trying .bak", zap.Error(err), zap.String("file", storesFile))
				_ = os.Rename(storesFile, storesFile+".corrupt")
				bakFile := storesFile + ".bak"
				if file.Exists(bakFile) {
					if bakBuf := file.ReadFullFile(bakFile); len(bakBuf) > 0 {
						err2 := json.Unmarshal(bakBuf, registry)
						if err2 == nil {
							logger.Info("recovered stores.json from .bak", zap.String("file", bakFile))
							return
						}
						logger.Error(".bak is also corrupt, falling back to defaults", zap.Error(err2))
					}
				}
				registry.Stores = defaultStores()
			}
		} else {
			registry.Stores = defaultStores()
			// Cannot propagate: LoadStoreRegistry returns only the registry,
			// and the in-memory defaults are usable either way. Logged inside
			// save(); discarded here on purpose, not by omission.
			_ = registry.save()
		}
	})
	return registry
}

func defaultStores() []StoreEntry {
	return []StoreEntry{
		{
			ID:       "default",
			Name:     "CasaNova Official",
			Type:     "local",
			Path:     "default",
			Enabled:  true,
			Priority: 1,
		},
		// CasaOS community catalog (Apache-2.0), merged below CasaNova's own.
		// ponytail: hardcoded default so fresh installs ship with a populated
		// store; disable/remove via the stores API if unwanted. URL must be the
		// gh-pages ZIP (has Apps/) — raw.githubusercontent won't work.
		{
			ID:       "casaos-official",
			Name:     "CasaOS App Store",
			Type:     "remote",
			URL:      "https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@gh-pages/store/main.zip",
			Path:     "casaos-official",
			Enabled:  true,
			Priority: 2,
		},
	}
}

// save persists the registry to stores.json.
//
// Wave 35-Q: previously this was a naked os.WriteFile with mode 0644
// — no temp file, no fsync, no rename, no inter-process lock. Two
// failure modes for a sysadmin:
//   - AppManagement and AppLibrary run as separate processes but
//     write the same file: one race truncates the other and
//     MarkSynced timestamps / RegisterStore entries silently vanish.
//   - A power loss mid-write left a half-written file that next-boot
//     load fell back to defaults on (see LoadStoreRegistry above).
//
// New behaviour:
//  1. Snapshot the previous file as stores.json.bak before writing
//     (best-effort) so a corruption recovery has somewhere to go.
//  2. Use commonfile.WriteFileAtomic for the durable write sequence.
//  3. Mode 0640 (was 0644) — the registry is service-only state, no
//     reason for it to be world-readable.
//
// Inter-process locking is still TODO; the long-term fix is to make
// AppLibrary stop writing this file at all and let AppManagement own
// it. For now, callers in both processes coexist via "last writer
// wins on a per-write basis" — atomic writes mean each side either
// sees the new full content or the previous full content, never a
// torn intermediate state.
// save writes the registry to disk AND REPORTS WHETHER IT WORKED.
//
// It used to return nothing. A marshal failure or a read-only/full disk was
// logged and forgotten, so RegisterStore, UnregisterStore and UpdateStore —
// each of which already returns an error to an HTTP handler — answered success
// for a change that existed only in this process's memory and disappeared at
// the next restart. Nobody could find out: not the API, not the UI, only the
// daemon log.
//
// The two callers that genuinely cannot propagate now discard the error
// EXPLICITLY, with a reason, so a reader can tell a considered discard from a
// forgotten one.
func (r *StoreRegistry) save() error {
	storesFile := filepath.Join(config.AppInfo.AppStorePath, common.StoresFileName)
	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		logger.Error("failed to marshal stores.json", zap.Error(err))
		return fmt.Errorf("marshal stores.json: %w", err)
	}
	// Snapshot to .bak (best effort) for the next LoadStoreRegistry to
	// recover from if this write somehow corrupts despite the atomic
	// write sequence (filesystem bugs, hardware failure).
	if file.Exists(storesFile) {
		_ = copyFileBestEffort(storesFile, storesFile+".bak")
	}
	if err := file.WriteFileAtomic(storesFile, buf, 0o640); err != nil {
		logger.Error("failed to write stores.json", zap.Error(err))
		return fmt.Errorf("write stores.json: %w", err)
	}
	return nil
}

// copyFileBestEffort makes a snapshot copy of src at dst. Errors are
// ignored — this is purely for the .bak fallback in LoadStoreRegistry.
func copyFileBestEffort(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o640)
}

// ListStores returns all registered stores.
func (r *StoreRegistry) ListStores() []StoreEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StoreEntry, len(r.Stores))
	copy(out, r.Stores)
	return out
}

// GetStore returns a store by ID or ErrStoreNotFound.
func (r *StoreRegistry) GetStore(id string) (StoreEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.Stores {
		if s.ID == id {
			return s, nil
		}
	}
	return StoreEntry{}, ErrStoreNotFound
}

// RegisterStore adds a new store entry. For remote stores, the caller
// should follow up with SyncRemoteStore to download the content.
func (r *StoreRegistry) RegisterStore(entry StoreEntry) error {
	defer r.invalidateCatalog()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.Stores {
		if s.ID == entry.ID {
			return ErrStoreAlreadyExists
		}
	}
	// Ensure the directory exists
	absPath := filepath.Join(config.AppInfo.AppStorePath, entry.Path)
	if err := os.MkdirAll(absPath, 0755); err != nil {
		return fmt.Errorf("failed to create store directory: %w", err)
	}
	before := append([]StoreEntry(nil), r.Stores...)
	r.Stores = append(r.Stores, entry)
	if err := r.save(); err != nil {
		// Put the registry back. A store that is "registered" until the next
		// restart is the same defect as one that reports success and was never
		// written — it just takes longer to notice.
		r.Stores = before
		return err
	}
	return nil
}

// UnregisterStore removes a store by ID. Does NOT delete the directory.
func (r *StoreRegistry) UnregisterStore(id string) error {
	defer r.invalidateCatalog()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.Stores {
		if s.ID == id {
			if id == "default" {
				return fmt.Errorf("cannot unregister the default store")
			}
			before := append([]StoreEntry(nil), r.Stores...)
			r.Stores = append(r.Stores[:i], r.Stores[i+1:]...)
			if err := r.save(); err != nil {
				r.Stores = before
				return err
			}
			return nil
		}
	}
	return ErrStoreNotFound
}

// UpdateStore modifies an existing store's mutable fields (name, enabled,
// priority, sync_interval_hours). The ID and type are immutable.
func (r *StoreRegistry) UpdateStore(id string, update StoreEntry) error {
	defer r.invalidateCatalog()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.Stores {
		if s.ID == id {
			// Snapshot before the in-place field edits, so a failed write can
			// put the entry back instead of leaving memory ahead of disk.
			before := append([]StoreEntry(nil), r.Stores...)
			if update.Name != "" {
				r.Stores[i].Name = update.Name
			}
			r.Stores[i].Enabled = update.Enabled
			if update.Priority > 0 {
				r.Stores[i].Priority = update.Priority
			}
			if update.SyncIntervalHours > 0 {
				r.Stores[i].SyncIntervalHours = update.SyncIntervalHours
			}
			if err := r.save(); err != nil {
				r.Stores = before
				return err
			}
			return nil
		}
	}
	return ErrStoreNotFound
}

// MarkSynced updates the last_synced timestamp for a store.
func (r *StoreRegistry) MarkSynced(id string) {
	defer r.invalidateCatalog()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.Stores {
		if s.ID == id {
			now := time.Now().UTC().Format(time.RFC3339)
			r.Stores[i].LastSynced = &now
			// Cannot propagate: MarkSynced reports nothing to anyone, and a
			// lost timestamp costs one extra sync rather than a wrong answer.
			// Logged inside save(); discarded here on purpose.
			_ = r.save()
			return
		}
	}
}

// SyncRemoteStore downloads a remote registry store into its configured
// registry path. This path is what MergedCatalog reads, so do not use the
// legacy AppStoreByURL hashed workdir here.
func SyncRemoteStore(entry StoreEntry) error {
	if entry.Type != "remote" {
		return nil
	}
	if entry.URL == "" {
		return fmt.Errorf("remote store %q has no URL", entry.ID)
	}
	if entry.Path == "" {
		return fmt.Errorf("remote store %q has no path", entry.ID)
	}

	workdir := filepath.Join(config.AppInfo.AppStorePath, entry.Path)
	downloadDir := workdir + ".tmp"
	readyDir := workdir + ".ready"
	backupDir := workdir + ".backup"
	isSuccessful := false

	// Wave 35-X: recover orphaned backup from previous interrupted sync.
	// If process was killed between rename(workdir, backupDir) and
	// rename(readyDir, workdir), the store silently disappears from
	// MergedCatalog on the next boot. Restore the backup so the store
	// stays browsable while this fresh sync runs.
	if file.Exists(backupDir) && !file.Exists(workdir) {
		logger.Info("recovering orphaned remote-store backup from prior crash",
			zap.String("backup", backupDir), zap.String("workdir", workdir),
			zap.String("store", entry.ID))
		if rerr := os.Rename(backupDir, workdir); rerr != nil {
			logger.Error("failed to recover remote-store backup; continuing with fresh sync",
				zap.Error(rerr), zap.String("store", entry.ID))
		}
	}

	if err := file.RMDir(downloadDir); err != nil {
		return err
	}
	if err := file.RMDir(readyDir); err != nil {
		return err
	}
	defer func() {
		if err := file.RMDir(downloadDir); err != nil {
			logger.Error("failed to remove temp appstore registry workdir", zap.Error(err), zap.String("tmpDir", downloadDir))
		}
		if err := file.RMDir(readyDir); err != nil {
			logger.Error("failed to remove ready appstore registry workdir", zap.Error(err), zap.String("readyDir", readyDir))
		}
	}()

	if err := downloadHelper.Download(entry.URL, downloadDir); err != nil {
		return err
	}
	storeRoot, err := StoreRoot(downloadDir)
	if err != nil {
		return err
	}
	if err := os.Rename(storeRoot, readyDir); err != nil {
		return err
	}

	if file.Exists(workdir) {
		if err := file.RMDir(backupDir); err != nil {
			return err
		}
		if err := os.Rename(workdir, backupDir); err != nil {
			return err
		}
		defer func() {
			if isSuccessful {
				if err := file.RMDir(backupDir); err != nil {
					logger.Error("failed to remove backup appstore registry workdir", zap.Error(err), zap.String("backupDir", backupDir))
				}
				return
			}
			if err := file.RMDir(workdir); err != nil {
				logger.Error("failed to remove failed appstore registry workdir", zap.Error(err), zap.String("workdir", workdir))
			}
			if err := os.Rename(backupDir, workdir); err != nil {
				logger.Error("failed to restore appstore registry backup", zap.Error(err), zap.String("backupDir", backupDir), zap.String("workdir", workdir))
			}
		}()
	}

	if err := os.Rename(readyDir, workdir); err != nil {
		return err
	}
	isSuccessful = true
	return nil
}

// EnabledStores returns all stores with enabled=true, sorted by priority.
func (r *StoreRegistry) EnabledStores() []StoreEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []StoreEntry
	for _, s := range r.Stores {
		if s.Enabled {
			out = append(out, s)
		}
	}
	// Sort by priority (lower number = higher priority)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Priority < out[i].Priority {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// MergedCatalog builds a unified catalog from all enabled stores. Apps
// from higher-priority stores (lower number) win on name collision.
func (r *StoreRegistry) MergedCatalog() (map[string]*ComposeApp, error) {
	r.catalogMu.RLock()
	if r.catalogCache != nil {
		cached := r.catalogCache
		r.catalogMu.RUnlock()
		return cached, nil
	}
	r.catalogMu.RUnlock()

	merged := map[string]*ComposeApp{}

	for _, store := range r.EnabledStores() {
		storeRoot := filepath.Join(config.AppInfo.AppStorePath, store.Path)
		if !file.Exists(storeRoot) {
			logger.Info("store directory not found, skipping", zap.String("store", store.ID), zap.String("path", storeRoot))
			continue
		}

		catalog, err := BuildCatalog(storeRoot)
		if err != nil {
			logger.Error("failed to build catalog for store", zap.String("store", store.ID), zap.Error(err))
			continue
		}

		for name, app := range catalog {
			if _, exists := merged[name]; !exists {
				merged[name] = app
			}
			// If name already exists, the higher-priority store's version wins
			// (it was added first because we iterate in priority order).
		}
	}

	r.catalogMu.Lock()
	r.catalogCache = merged
	r.catalogMu.Unlock()

	return merged, nil
}

// MergedCategoryMap builds a unified category map from all enabled stores.
func (r *StoreRegistry) MergedCategoryMap() map[string]codegen.CategoryInfo {
	merged := map[string]codegen.CategoryInfo{}

	for _, store := range r.EnabledStores() {
		storeRoot := filepath.Join(config.AppInfo.AppStorePath, store.Path)
		for k, v := range LoadCategoryMap(storeRoot) {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}

	return merged
}

// MergedRecommend builds a unified recommend list from all enabled stores.
func (r *StoreRegistry) MergedRecommend() []string {
	var merged []string
	seen := map[string]bool{}

	for _, store := range r.EnabledStores() {
		storeRoot := filepath.Join(config.AppInfo.AppStorePath, store.Path)
		for _, id := range LoadRecommend(storeRoot) {
			if !seen[id] {
				merged = append(merged, id)
				seen[id] = true
			}
		}
	}

	return merged
}
