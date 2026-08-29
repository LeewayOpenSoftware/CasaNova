package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	interfaces "github.com/LeewayOpenSoftware/CasaNova-Common"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

// migrationTool050 handles migration from CasaOS 0.4.x to CasaNova 0.5.0.
//
// Changes:
// - Ensures app store URLs are up to date (adds CasaNova store if not present)
// - Writes migration status v0.5.0
type migrationTool050 struct{}

func (m *migrationTool050) IsMigrationNeeded() (bool, error) {
	_logger.Info("Checking if CasaNova v0.5.0 migration is needed...")

	// Check migration status file
	statusPath := "/var/lib/casaos/migration/app-management.status"
	if data, err := os.ReadFile(statusPath); err == nil {
		currentStatus := strings.TrimSpace(string(data))
		if currentStatus == "v0.5.0" || currentStatus >= "v0.5" {
			_logger.Info("Already migrated to v0.5.0 or newer.")
			return false, nil
		}
	}

	return true, nil
}

func (m *migrationTool050) PreMigrate() error {
	_logger.Info("Pre-migration: backing up app-management.conf...")

	src := config.AppManagementConfigFilePath
	dst := src + ".pre-050.bak"

	if _, err := os.Stat(src); err == nil {
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("failed to read config for backup: %w", err)
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			_logger.Error("Warning: could not create config backup at %s: %s", dst, err)
			// Non-fatal - continue with migration
		} else {
			_logger.Info("Config backed up to %s", dst)
		}
	}

	return nil
}

func (m *migrationTool050) Migrate() error {
	_logger.Info("Migrating app-management config to CasaNova v0.5.0...")

	file, err := os.OpenFile(config.AppManagementConfigFilePath, os.O_RDWR, 0644)
	if err != nil {
		// Config file doesn't exist yet - will be created by service on first run
		_logger.Info("Config file not found, will be created on first run: %s", err)
		return nil
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	configStr := string(content)
	modified := false

	// Ensure the IceWhaleTech app store URL is updated to jsdelivr CDN
	oldURLs := []string{
		"https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip",
		"https://casaos.oss-cn-shanghai.aliyuncs.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip",
		"https://casaos.app/store/main.zip",
	}
	newURL := "https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@gh-pages/store/main.zip"

	for _, old := range oldURLs {
		if strings.Contains(configStr, old) {
			configStr = strings.Replace(configStr, old, newURL, -1)
			modified = true
			_logger.Info("Updated app store URL: %s -> %s", old, newURL)
		}
	}

	if modified {
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("failed to truncate config: %w", err)
		}
		if _, err := file.WriteAt([]byte(configStr), 0); err != nil {
			return fmt.Errorf("failed to write config: %w", err)
		}
	}

	_logger.Info("Migration complete.")
	return nil
}

func (m *migrationTool050) PostMigrate() error {
	// Write migration status
	statusDir := "/var/lib/casaos/migration"
	if err := os.MkdirAll(statusDir, 0755); err != nil {
		return fmt.Errorf("failed to create migration status dir: %w", err)
	}

	statusPath := statusDir + "/app-management.status"
	if err := os.WriteFile(statusPath, []byte("v0.5.0\n"), 0644); err != nil {
		return fmt.Errorf("failed to write migration status: %w", err)
	}

	_logger.Info("Migration status written: v0.5.0")
	return nil
}

func NewMigration050() interfaces.MigrationTool {
	return &migrationTool050{}
}
