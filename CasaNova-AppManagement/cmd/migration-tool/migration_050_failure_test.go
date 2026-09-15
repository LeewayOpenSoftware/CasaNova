package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

// A FAILED MIGRATION RUN MUST NOT BECOME PERMANENT.
//
// Migrate() opened the config and, on ANY error, took the branch meaning "the
// file does not exist yet, the service will create it" and returned nil. EACCES
// on a config owned by another user, EROFS on a read-only rootfs, EIO on a
// failing disk, ENOTDIR on a broken path — all of them reported success.
//
// main() then ran PostMigrate, which writes "v0.5.0" into
// /var/lib/casaos/migration/app-management.status, and from that moment
// IsMigrationNeeded answers false. The config was never migrated, nothing said
// so, and NO LATER RUN WOULD EVER TRY AGAIN. One bad minute became the box's
// permanent state.
//
// Returning the error stops main() before PostMigrate, so the stamp is not
// written and the next run retries — which is the whole fix.

// The package logger is built in main(), which tests never run, so every
// method here would nil-panic on its first _logger call. Building one for the
// test package is the whole of the fix — and the panic it replaces was a
// perfectly good signal: it said the test was reaching real code.
func init() {
	if _logger == nil {
		_logger = NewLogger()
	}
}

// configAt points the migration tool at a config path for one test and puts
// the real one back afterwards. Nothing outside t.TempDir() is touched, and in
// particular /etc/casaos and /var/lib/casaos are never opened.
func configAt(t *testing.T, path string) {
	t.Helper()
	prev := config.AppManagementConfigFilePath
	config.AppManagementConfigFilePath = path
	t.Cleanup(func() { config.AppManagementConfigFilePath = prev })
}

// Mutation that must fail this test: go back to returning nil for every
// os.OpenFile error.
func TestAConfigThatCannotBeOpenedIsNotReportedAsMigrated(t *testing.T) {
	// A path whose parent is a regular file: opening it fails with ENOTDIR,
	// which is emphatically not "the file does not exist yet". A read-only
	// filesystem and a permission denial arrive at the same branch.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	configAt(t, filepath.Join(blocker, "app-management.conf"))

	m := &migrationTool050{}
	err := m.Migrate()

	if err == nil {
		t.Fatal("Migrate returned nil for a config it could not open; main() goes on to stamp v0.5.0 and the box " +
			"never attempts this migration again")
	}
	if !strings.Contains(err.Error(), "open config") {
		t.Errorf("err = %v; it should say the config could not be opened", err)
	}
}

// The legitimate case is still legitimate: a config that genuinely is not
// there yet is not a failure — the service writes it on first start and there
// is nothing to convert. Asserted next to the failure case so the fix cannot
// be "return an error always".
func TestAnAbsentConfigIsStillNothingToMigrate(t *testing.T) {
	configAt(t, filepath.Join(t.TempDir(), "does-not-exist.conf"))

	m := &migrationTool050{}
	if err := m.Migrate(); err != nil {
		t.Fatalf("an absent config must be nothing to migrate, got: %v", err)
	}
}

// And a real config is still migrated, so neither test above is passing
// because Migrate does nothing at all.
func TestAReadableConfigIsStillProcessed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-management.conf")
	body := "[app]\nappstore=https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	configAt(t, path)

	m := &migrationTool050{}
	if err := m.Migrate(); err != nil {
		t.Fatalf("Migrate on a readable config: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(after), "github.com/IceWhaleTech/_appstore") {
		t.Errorf("the legacy app store URL was left in place, so this test is not exercising the migration: %q", string(after))
	}
}
