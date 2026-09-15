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
	data, err := os.ReadFile(statusPath)
	switch {
	case err == nil:
		currentStatus := strings.TrimSpace(string(data))
		if currentStatus == "v0.5.0" || currentStatus >= "v0.5" {
			_logger.Info("Already migrated to v0.5.0 or newer.")
			return false, nil
		}
	case os.IsNotExist(err):
		// Never migrated on this box. Nothing to report.
	default:
		// The status file is there and could not be read. "I could not check"
		// is not "not migrated", and it is not "already migrated" either — so
		// say so, and let the caller decide. Answering true here still runs
		// the migration, which is the safe direction (it is idempotent), but
		// the caller needs to know the answer was a fallback and not a
		// measurement.
		return true, fmt.Errorf("could not read migration status at %s: %w", statusPath, err)
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
		// ONLY "IT IS NOT THERE" MEANS THERE IS NOTHING TO MIGRATE.
		//
		// This used to take the not-found branch for ANY error — EACCES on a
		// config owned by another user, EROFS on a read-only rootfs, EIO on a
		// failing disk, ENOTDIR on a broken path — and return nil. main() then
		// ran PostMigrate, which stamps "v0.5.0" into the status file, and
		// IsMigrationNeeded answers false from that moment on. A SINGLE FAILED
		// RUN BECAME PERMANENT: the config was never migrated, nothing said
		// so, and no later run would ever try again.
		//
		// A genuinely absent config is still fine — the service writes it on
		// first start, and there is nothing to convert. Everything else is a
		// failure, and returning it stops main() before the stamp, so the next
		// run retries.
		if os.IsNotExist(err) {
			_logger.Info("Config file not found, will be created on first run: %s", err)
			return nil
		}
		return fmt.Errorf("failed to open config for migration: %w", err)
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
