package v1

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ---- Migration assistant (CasaOS → CasaNova) ----
//
// CasaNova's install.sh already copies legacy CasaOS configs from
// /etc/casaos/ into /etc/casanova/, disables the legacy systemd
// units, and the Go services keep reading from /var/lib/casaos/ for
// upgrade compatibility. What's missing is a user-visible *report* of
// what got migrated so the user has confidence the upgrade landed.
//
// This endpoint scans the filesystem for CasaOS markers and returns a
// structured report. No mutations — pure read-only inspection. The
// UI's Settings → Migration page renders this; a YSK card on first
// boot can prompt the user to review.

type migrationConfig struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"` // bytes; -1 if not regular file or stat failed
}

type migrationLegacyUnit struct {
	Name    string `json:"name"`    // systemd unit name (casaos-foo.service)
	Active  bool   `json:"active"`  // currently active (would be a fight)
	Enabled bool   `json:"enabled"` // enabled at boot (we disable on install)
}

// MigrationReport summarises what CasaOS state was detected on this
// host. A user upgrading from upstream CasaOS sees populated fields;
// a fresh CasaNova install sees all-zero / empty.
type MigrationReport struct {
	// True iff /etc/casaos/ exists with at least one config file. This
	// is the strongest single signal that an upgrade happened.
	LegacyConfigDir bool `json:"legacy_config_dir"`

	// Config files copied from /etc/casaos/ to /etc/casanova/ by
	// install.sh. We re-scan rather than tracking the copy itself so a
	// user who runs the report again after manually editing one file
	// sees the current state, not a stale snapshot.
	ConfigsMigrated []migrationConfig `json:"configs_migrated"`

	// Compose stack working directories under /var/lib/casaos/apps/
	// that still exist. CasaNova reads compose projects from the same
	// path, so no migration is needed beyond detection.
	LegacyComposeAppsDir string   `json:"legacy_compose_apps_dir"`
	LegacyComposeApps    []string `json:"legacy_compose_apps"` // project names

	// Containers carrying the upstream `casaos=casaos` label. They
	// continue to work without changes (our list endpoints look for
	// this exact label), but reporting the count helps the user trust
	// that nothing was orphaned.
	LegacyContainers int `json:"legacy_containers"`

	// systemd units named casaos-* that are still installed. install.sh
	// disables them; this list is for human verification.
	LegacyUnits []migrationLegacyUnit `json:"legacy_units"`

	// True after the user clicks "Mark migration complete" — purely
	// cosmetic, used to hide the YSK card. Stored at
	// /var/lib/casanova/migration-completed (touch file).
	Acknowledged bool `json:"acknowledged"`

	// When the report was generated (UTC RFC3339). Useful for cache
	// invalidation in the UI.
	GeneratedAt string `json:"generated_at"`
}

const migrationAckPath = "/var/lib/casanova/migration-completed"

// Config files we expect install.sh to have migrated. Listing them
// explicitly (rather than glob-walking /etc/casanova/) keeps the
// report focused on the upstream-shaped files and ignores
// CasaNova-specific configs that may have been added since.
var migrationConfigNames = []string{
	"casanova.conf",
	"gateway.ini",
	"user-service.conf",
	"local-storage.conf",
	"app-management.conf",
	"message-bus.conf",
	"job-management.conf",
}

// GetMigrationReport returns a one-shot inspection of CasaOS-→
// CasaNova migration state on this host.
//
// GET /v1/sys/migration/report
func GetMigrationReport(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	report := MigrationReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// 1. Legacy /etc/casaos/ presence.
	if entries, err := os.ReadDir("/etc/casaos"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			report.LegacyConfigDir = true
			break
		}
	}

	// 2. Configs surviving in /etc/casanova/.
	for _, name := range migrationConfigNames {
		path := filepath.Join("/etc/casanova", name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		size := int64(-1)
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		report.ConfigsMigrated = append(report.ConfigsMigrated, migrationConfig{
			Name: name,
			Path: path,
			Size: size,
		})
	}

	// 3. Compose stack working dirs under the legacy apps path.
	report.LegacyComposeAppsDir = "/var/lib/casaos/apps"
	if entries, err := os.ReadDir(report.LegacyComposeAppsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			// A compose project directory must contain a
			// docker-compose.{yml,yaml}. Skip anything else (a
			// stray temp dir, a `.tmp` artifact).
			composeFound := false
			for _, candidate := range []string{"docker-compose.yml", "docker-compose.yaml"} {
				if _, err := os.Stat(filepath.Join(report.LegacyComposeAppsDir, e.Name(), candidate)); err == nil {
					composeFound = true
					break
				}
			}
			if composeFound {
				report.LegacyComposeApps = append(report.LegacyComposeApps, e.Name())
			}
		}
	}

	// 4. Containers labeled casaos=casaos. Use docker CLI rather than
	//    the Docker SDK here because we don't want to take a hard
	//    runtime dependency on docker.SharedClient() from this read
	//    handler. 2s timeout is plenty for a single ps call.
	report.LegacyContainers = countLegacyContainers()

	// 5. systemd unit inventory.
	report.LegacyUnits = scanLegacyUnits()

	// 6. Acknowledgement marker.
	if _, err := os.Stat(migrationAckPath); err == nil {
		report.Acknowledged = true
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    report,
	})
}

// PostMigrationAcknowledge marks the migration as complete by
// touching /var/lib/casanova/migration-completed. The UI uses this
// to hide the migration YSK card.
//
// POST /v1/sys/migration/acknowledge
func PostMigrationAcknowledge(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(migrationAckPath), 0o755); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create state dir: " + err.Error(),
		})
	}
	if err := os.WriteFile(migrationAckPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to record acknowledgement: " + err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// countLegacyContainers shells out to docker ps with a label filter.
// Returns 0 on any error — failures here must not break the report
// (docker may not be running on a host that's mid-migration).
func countLegacyContainers() int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=casaos=casaos",
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// scanLegacyUnits queries systemd for casaos-* units. Returns empty
// list when systemctl isn't available or returns nothing — fresh
// installs that never had CasaOS see an empty list, which is correct.
func scanLegacyUnits() []migrationLegacyUnit {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// list-unit-files captures *enabled* state; list-units catches
	// *active* state. We need both, so we run both and merge.
	enabledMap := map[string]bool{}
	if out, err := exec.CommandContext(ctx, "systemctl", "list-unit-files",
		"--type=service", "--no-pager", "--no-legend", "casaos-*",
	).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			enabledMap[fields[0]] = fields[1] == "enabled"
		}
	}

	activeMap := map[string]bool{}
	if out, err := exec.CommandContext(ctx, "systemctl", "list-units",
		"--type=service", "--no-pager", "--no-legend", "--all", "casaos-*",
	).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			activeMap[fields[0]] = fields[2] == "active"
		}
	}

	// Union the two key sets.
	all := map[string]struct{}{}
	for k := range enabledMap {
		all[k] = struct{}{}
	}
	for k := range activeMap {
		all[k] = struct{}{}
	}
	out := make([]migrationLegacyUnit, 0, len(all))
	for name := range all {
		out = append(out, migrationLegacyUnit{
			Name:    name,
			Active:  activeMap[name],
			Enabled: enabledMap[name],
		})
	}
	return out
}
