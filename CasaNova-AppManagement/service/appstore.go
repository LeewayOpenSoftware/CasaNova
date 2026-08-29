package service

import (
	"crypto/md5" // nolint: gosec
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/utils/downloadHelper"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

type AppStore interface {
	Catalog() (map[string]*ComposeApp, error)
	CategoryMap() (map[string]codegen.CategoryInfo, error)
	ComposeApp(id string) (*ComposeApp, error)
	Recommend() ([]string, error)
	UpdateCatalog() error
	WorkDir() (string, error)
}

type appStore struct {
	categoryMap   map[string]codegen.CategoryInfo
	catalog       map[string]*ComposeApp
	catalogLoaded bool // true once catalog has been built at least once
	recommend     []string
	url           string

	lastAPPStoreSize int64
}

var (
	appStoreMapMu sync.RWMutex
	appStoreMap   = make(map[string]*appStore)

	ErrNotAppStore             = fmt.Errorf("not an appstore")
	ErrDefaultAppStoreNotFound = fmt.Errorf("default appstore not found")
)

func (s *appStore) CategoryMap() (map[string]codegen.CategoryInfo, error) {
	if s.categoryMap != nil {
		return s.categoryMap, nil
	}

	workdir, err := s.WorkDir()
	if err != nil {
		return nil, err
	}

	storeRoot, err := StoreRoot(workdir)
	if err != nil {
		return nil, err
	}

	categoryMap := LoadCategoryMap(storeRoot)

	s.categoryMap = categoryMap

	return s.categoryMap, nil
}

func (s *appStore) UpdateCatalog() error {
	isSuccessful := false

	if _, err := url.Parse(s.url); err != nil {
		return err
	}

	// Wave 35-X: recover from a previous interrupted UpdateCatalog.
	// The non-transactional rename dance below (workdir → workdir.backup
	// → tmpDir → workdir) leaves the system with an orphaned
	// workdir.backup if the process was kill-9'd / OOM'd / power-lost
	// between `os.Rename(workdir, backupDir)` and `os.Rename(tmpDir,
	// workdir)`. defers don't run for these terminations. On restart,
	// workdir is missing and the entire store silently disappears from
	// the UI even though .backup has the previous content. Detect this
	// here and restore.
	if wd, err := s.WorkDir(); err == nil {
		backupDir := wd + ".backup"
		if file.Exists(backupDir) && !file.Exists(wd) {
			logger.Info("recovering orphaned appstore backup from prior crash",
				zap.String("backup", backupDir), zap.String("workdir", wd))
			if rerr := os.Rename(backupDir, wd); rerr != nil {
				logger.Error("failed to recover appstore backup; continuing with fresh download",
					zap.Error(rerr), zap.String("backup", backupDir))
			}
		}
	}

	// check wether the zip package size change
	// if not, skip the update
	{
		// Use a local client instead of mutating the global http.DefaultClient.
		headClient := &http.Client{Timeout: 5 * time.Second}
		res, err := headClient.Head(s.url)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to get appstore size, status code: %d", res.StatusCode)
		}
		if res.ContentLength == s.lastAPPStoreSize {
			logger.Info("appstore size not changed", zap.String("url", s.url))
			return nil
		}
		logger.Info("appstore size changed, update app store", zap.String("url", s.url))

		defer func() {
			if isSuccessful {
				s.lastAPPStoreSize = res.ContentLength
			}
		}()
	}

	workdir, err := s.WorkDir()
	if err != nil {
		return err
	}
	tmpDir := workdir + ".tmp"

	defer func() {
		if err := file.RMDir(tmpDir); err != nil {
			logger.Error("failed to remove temp appstore workdir", zap.Error(err), zap.String("tmpDir", tmpDir))
		}
	}()

	if err := downloadHelper.Download(s.url, tmpDir); err != nil {
		return err
	}

	// make a backup of existing workdir
	if file.Exists(workdir) {
		backupDir := workdir + ".backup"

		if err := file.RMDir(backupDir); err != nil {
			return err
		}

		if err := os.Rename(workdir, backupDir); err != nil {
			return err
		}

		defer func() {
			if isSuccessful {
				if err := file.RMDir(backupDir); err != nil {
					logger.Error("failed to remove backup appstore workdir", zap.Error(err), zap.String("backupDir", backupDir))
				}
				return
			}

			if err := file.RMDir(workdir); err != nil {
				logger.Error("failed to remove appstore workdir", zap.Error(err), zap.String("workdir", workdir))
			}

			if err := os.Rename(backupDir, workdir); err != nil {
				logger.Error("failed to restore backup appstore workdir", zap.Error(err), zap.String("backupDir", backupDir), zap.String("workdir", workdir))
			}
		}()
	}

	if err := os.Rename(tmpDir, workdir); err != nil {
		return err
	}

	storeRoot, err := StoreRoot(workdir)
	if err != nil {
		return err
	}

	placeholderFile := filepath.Join(storeRoot, ".casaos-appstore")
	if err := file.CreateFileAndWriteContent(placeholderFile, s.url); err != nil {
		return err
	}

	newCatalog, err := BuildCatalog(storeRoot)
	if err != nil {
		return err
	}

	// Guard against replacing a valid catalog with an empty one due to
	// transient download/parse failures.  If the new catalog is empty but
	// we already had apps, keep the old catalog and log a warning.
	if len(newCatalog) == 0 && len(s.catalog) > 0 {
		logger.Info("BuildCatalog returned 0 apps after update — keeping previous catalog",
			zap.Int("previousCount", len(s.catalog)),
			zap.String("storeRoot", storeRoot))
	} else {
		s.catalog = newCatalog
		s.catalogLoaded = true
	}

	s.categoryMap = LoadCategoryMap(storeRoot)

	s.recommend = LoadRecommend(storeRoot)

	isSuccessful = true

	return nil
}

func (s *appStore) Recommend() ([]string, error) {
	if s.recommend != nil && len(s.recommend) > 0 {
		return s.recommend, nil
	}

	workdir, err := s.WorkDir()
	if err != nil {
		return nil, err
	}

	storeRoot, err := StoreRoot(workdir)
	if err != nil {
		return nil, err
	}

	return LoadRecommend(storeRoot), nil
}

func (s *appStore) Catalog() (map[string]*ComposeApp, error) {
	if s.catalogLoaded {
		return s.catalog, nil
	}

	workdir, err := s.WorkDir()
	if err != nil {
		return nil, err
	}

	storeRoot, err := StoreRoot(workdir)
	if err != nil {
		return nil, err
	}

	catalog, err := BuildCatalog(storeRoot)
	if err != nil {
		return nil, err
	}

	s.catalog = catalog
	s.catalogLoaded = true

	return s.catalog, nil
}

func (s *appStore) ComposeApp(appStoreID string) (*ComposeApp, error) {
	catalog, err := s.Catalog()
	if err != nil {
		return nil, err
	}

	if composeApp, ok := catalog[appStoreID]; ok {
		return composeApp, nil
	}

	return nil, nil
}

func (s *appStore) WorkDir() (string, error) {
	if s.url == "default" {
		return filepath.Join(config.AppInfo.AppStorePath, s.url), nil
	}

	parsedURL, err := url.Parse(s.url)
	if err != nil {
		return "", err
	}

	appstoreKey := strings.ToLower(parsedURL.Path)

	hash := fmt.Sprintf("%x", md5.Sum([]byte(appstoreKey))) //nolint: gosec

	return filepath.Join(config.AppInfo.AppStorePath, parsedURL.Host, hash), nil
}

func AppStoreByURL(appstoreURL string) (AppStore, error) {
	_, err := url.Parse(appstoreURL)
	if err != nil {
		return nil, err
	}

	// a appstoreKey is a normalized appstore url where everything is in lowercase
	appstoreKey := strings.ToLower(appstoreURL)

	// Check with read lock first (fast path).
	appStoreMapMu.RLock()
	if appstore, ok := appStoreMap[appstoreKey]; ok {
		appStoreMapMu.RUnlock()
		return appstore, nil
	}
	appStoreMapMu.RUnlock()

	// Slow path: acquire write lock, double-check, then create.
	appStoreMapMu.Lock()
	defer appStoreMapMu.Unlock()
	if appstore, ok := appStoreMap[appstoreKey]; ok {
		return appstore, nil
	}
	appStoreMap[appstoreKey] = &appStore{
		url:     appstoreURL,
		catalog: map[string]*ComposeApp{},
	}

	return appStoreMap[appstoreKey], nil
}

func NewDefaultAppStore() (AppStore, error) {
	storeRoot := filepath.Join(config.AppInfo.AppStorePath, "default")

	if !file.Exists(storeRoot) {
		return nil, ErrDefaultAppStoreNotFound
	}

	categoryMap := LoadCategoryMap(storeRoot)

	catalog, err := BuildCatalog(storeRoot)
	if err != nil {
		return nil, err
	}

	recommend := LoadRecommend(storeRoot)

	return &appStore{
		url:         "default",
		categoryMap: categoryMap,
		catalog:     catalog,
		recommend:   recommend,
	}, nil
}

func LoadCategoryMap(storeRoot string) map[string]codegen.CategoryInfo {
	categoryListFile := filepath.Join(storeRoot, common.CategoryListFileName)

	// unmarsal category list
	categoryList := []codegen.CategoryInfo{}

	if !file.Exists(categoryListFile) {
		return map[string]codegen.CategoryInfo{}
	}

	buf := file.ReadFullFile(categoryListFile)

	if err := json.Unmarshal(buf, &categoryList); err != nil {
		logger.Error("failed to unmarshal category list", zap.Error(err), zap.String("categoryListFile", categoryListFile))
		return map[string]codegen.CategoryInfo{}
	}

	categoryList = lo.Filter(categoryList, func(category codegen.CategoryInfo, i int) bool {
		return category.Name != nil && *category.Name != ""
	})

	categoryList = lo.Map(categoryList, func(category codegen.CategoryInfo, i int) codegen.CategoryInfo {
		if category.Font == nil || *category.Font == "" {
			category.Font = lo.ToPtr(common.DefaultCategoryFont)
		}

		if category.Description == nil {
			category.Description = lo.ToPtr("")
		}

		return category
	})

	return lo.SliceToMap(categoryList, func(category codegen.CategoryInfo) (string, codegen.CategoryInfo) {
		return *category.Name, category
	})
}

func LoadRecommend(storeRoot string) []string {
	recommendListFile := filepath.Join(storeRoot, common.RecommendListFileName)

	// unmarsal recommend list
	recommendList := []interface{}{}

	if !file.Exists(recommendListFile) {
		logger.Info("recommend list file not found", zap.String("recommendListFile", recommendListFile))
		return []string{}
	}

	buf := file.ReadFullFile(recommendListFile)
	if err := json.Unmarshal(buf, &recommendList); err != nil {
		logger.Error("failed to unmarshal recommend list", zap.Error(err), zap.String("recommendListFile", recommendListFile))
		return []string{}
	}

	result := lo.Map(recommendList, func(item interface{}, i int) string {
		recommendItem, ok := item.(map[string]interface{})
		if !ok {
			return ""
		}

		storeAppID, ok := recommendItem["appid"]
		if !ok {
			return ""
		}

		if s, ok := storeAppID.(string); ok {
			return s
		}
		return fmt.Sprint(storeAppID) // handle non-string appid (e.g., int)
	})

	return result
}

func BuildCatalog(storeRoot string) (map[string]*ComposeApp, error) {
	catalog := map[string]*ComposeApp{}

	// walk through each folder under storeRoot/Apps and build the catalog
	if err := filepath.WalkDir(filepath.Join(storeRoot, common.AppsDirectoryName), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			return nil
		}

		composeFile := filepath.Join(path, common.ComposeYAMLFileName)
		if !file.Exists(composeFile) {
			// retry with ".yaml" extension
			composeFile = strings.TrimSuffix(composeFile, ".yml") + ".yaml"
			if !file.Exists(composeFile) {
				return nil
			}
		}

		composeYAML := file.ReadFullFile(composeFile)
		if len(composeYAML) == 0 {
			return nil
		}

		// Wave 35-T: catalog loads MUST skip the ValidateComposeSecurity
		// gate. That gate is an INSTALL-time policy; applying it at
		// catalog-build time silently drops every app that bind-mounts
		// /var/run/docker.sock (watchtower, portainer, dockge, diun, …)
		// or exposes /dev/* (scrutiny, plex/jellyfin HW transcoding)
		// from the browse UI. Users who click Install on one of those
		// still hit the gate via NewComposeAppFromYAML downstream.
		composeApp, err := NewComposeAppFromYAMLForCatalog(composeYAML)
		if err != nil {
			logger.Info("failed to parse compose app - contact the contributor of this app to fix it", zap.Error(err), zap.String("composeFile", composeFile))
			return fs.SkipDir // skip invalid compose app
		}

		catalog[composeApp.Name] = composeApp

		return nil
	}); err != nil {
		return nil, err
	}

	return catalog, nil
}

func StoreRoot(workdir string) (string, error) {
	storeRoot := ""

	// locate the path that contains the Apps directory
	if err := filepath.WalkDir(workdir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && d.Name() == common.AppsDirectoryName {
			storeRoot = filepath.Dir(path)
			return filepath.SkipDir
		}

		return nil
	}); err != nil {
		return "", err
	}

	if storeRoot != "" {
		return storeRoot, nil
	}

	return "", ErrNotAppStore
}
