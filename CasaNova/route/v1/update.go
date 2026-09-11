package v1

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ─── Types matching the frontend contract in api/updates.ts ─────────────────

type UpdateStage string

const (
	StageIdle        UpdateStage = "idle"
	StageChecking    UpdateStage = "checking"
	StageDownloading UpdateStage = "downloading"
	StageVerifying   UpdateStage = "verifying"
	StageApplying    UpdateStage = "applying"
	StageRestarting  UpdateStage = "restarting"
	StageCompleted   UpdateStage = "completed"
	StageFailed      UpdateStage = "failed"
)

type UpdateChannel string

const (
	ChannelStable  UpdateChannel = "stable"
	ChannelBeta    UpdateChannel = "beta"
	ChannelNightly UpdateChannel = "nightly"
)

type UpdateStatus struct {
	CurrentVersion  string        `json:"currentVersion"`
	LatestVersion   *string       `json:"latestVersion"`
	Channel         UpdateChannel `json:"channel"`
	UpdateAvailable bool          `json:"updateAvailable"`
	Stage           UpdateStage   `json:"stage"`
	ProgressPercent int           `json:"progressPercent"`
	Message         string        `json:"message"`
	LastCheckedAt   *string       `json:"lastCheckedAt"`
	LastAppliedAt   *string       `json:"lastAppliedAt"`
	SizeBytes       *int64        `json:"sizeBytes"`
	RequiresReboot  bool          `json:"requiresReboot"`
	LastReport      *UpdateReport `json:"lastReport,omitempty"`
}

// UpdateReport mirrors the JSON that casanova-update.sh writes to
// /var/lib/casanova/upgrade-backup-<ts>/update-report.json after every
// attempt. Surfaced through the status endpoint so the UI can show
// which services failed and offer a rollback button.
type UpdateReport struct {
	Status           string   `json:"status"`
	FromVersion      string   `json:"from_version"`
	ToVersion        string   `json:"to_version"`
	InstalledVersion string   `json:"installed_version"`
	Channel          string   `json:"channel"`
	ServicesOK       int      `json:"services_ok"`
	ServicesTotal    int      `json:"services_total"`
	ServicesFailed   []string `json:"services_failed"`
	GatewayPort      int      `json:"gateway_port"`
	GatewayPing      bool     `json:"gateway_ping"`
	FrontendOK       bool     `json:"frontend_ok"`
	ContainersBefore int      `json:"containers_before"`
	ContainersAfter  int      `json:"containers_after"`
	BackupLocation   string   `json:"backup_location"`
	Timestamp        string   `json:"timestamp"`
}

type ChangelogEntry struct {
	Version     string `json:"version"`
	PublishedAt string `json:"publishedAt"`
	Body        string `json:"body"`
}

type StartUpdateRequest struct {
	TargetVersion *string `json:"targetVersion"`
	AllowReboot   bool    `json:"allowReboot"`
}

type StartUpdateResponse struct {
	JobID string      `json:"jobId"`
	Stage UpdateStage `json:"stage"`
}

// ─── Global update state (single system-wide state machine) ─────────────────
// Protected by a mutex since HTTP handlers run concurrently.

var (
	updateMu    sync.RWMutex
	updateState = UpdateStatus{
		CurrentVersion:  common.FullVersion(),
		Channel:         UpdateChannel(common.CHANNEL),
		Stage:           StageIdle,
		ProgressPercent: 0,
		Message:         "",
	}
	updateJobID    string
	updateCancel   chan struct{}
	statusFilePath = "/var/run/casanova/update-status.json"
	updateScript   = "/usr/share/casanova/tools/casanova-update.sh"
)

// statusFileState is the JSON written by casanova-update.sh --status-file
type statusFileState struct {
	Stage           string `json:"stage"`
	ProgressPercent int    `json:"progressPercent"`
	Message         string `json:"message"`
	UpdatedAt       string `json:"updatedAt"`
}

// ─── Handlers ───────────────────────────────────────────────────────────────

// GET /v1/sys/update/status — poll-friendly endpoint the UI hits every 2-60s
func GetUpdateStatus(ctx echo.Context) error {
	updateMu.RLock()
	s := updateState
	updateMu.RUnlock()

	// If an update is in progress, read the status file written by the
	// shell script for real-time progress
	if s.Stage == StageDownloading || s.Stage == StageVerifying ||
		s.Stage == StageApplying || s.Stage == StageRestarting {
		if fs, err := readStatusFile(); err == nil {
			s.Stage = UpdateStage(fs.Stage)
			s.ProgressPercent = fs.ProgressPercent
			s.Message = fs.Message
		}
	} else {
		// Idle/terminal: recompute availability against the RUNNING build so
		// the panel self-corrects the instant an apply changes the version —
		// no lingering "update available" after a successful update, and no
		// re-offer of a same/older cached package. Operates on the copy `s`.
		evaluateAvailability(&s)
	}

	// Attach the most-recent post-update report so the UI can show which
	// services failed (if any) and offer a rollback button. Cheap — one
	// glob + one file read per status poll.
	if rep, _ := readLatestReport(); rep != nil {
		s.LastReport = rep
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    s,
	})
}

// readLatestReport finds the newest update-report.json the update script
// writes and parses it. The script writes it to
// ${UPGRADE_BACKUP_BASE}/upgrade-<ts>/update-report.json where
// UPGRADE_BACKUP_BASE=/var/lib/casanova/backups (casanova-update.sh). This
// glob MUST match that path — an earlier mismatch left /update/status with
// an always-empty LastReport and made /update/rollback always 400.
// Returns (nil, nil) when no report exists yet.
func readLatestReport() (*UpdateReport, error) {
	matches, err := filepath.Glob("/var/lib/casanova/backups/upgrade-*/update-report.json")
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	var newest string
	var newestTime time.Time
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if fi.ModTime().After(newestTime) {
			newestTime = fi.ModTime()
			newest = m
		}
	}
	if newest == "" {
		return nil, nil
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		return nil, err
	}
	var rep UpdateReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// PostUpdateRollback triggers a rollback to the most recent backup. It spawns
// /usr/share/casanova/tools/casaos-to-casanova.sh with --rollback-upgrade and
// lets it write its own progress through the shared status file. Requires
// PermSystemConfig.
//
// The comment here used to say upgrade.sh does the restore. It does not, and
// has not for some time — the path a few lines below is the truth. upgrade.sh
// still exists and is still shipped inside the package, so a reader who
// believed this comment would have gone and edited a script this endpoint never
// runs. A comment naming the wrong file is worse than no comment.
func PostUpdateRollback(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	rep, _ := readLatestReport()
	if rep == nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "no previous update found to roll back from",
		})
	}
	if rep.BackupLocation == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "report missing backup_location",
		})
	}

	rollbackScript := "/usr/share/casanova/tools/casaos-to-casanova.sh"
	if _, err := os.Stat(rollbackScript); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "rollback script not found at " + rollbackScript,
		})
	}

	// Fire and forget — rollback will restart casanova itself, so the
	// HTTP response races the service shutdown. Caller polls /status to
	// see when it comes back.
	//
	// MUST run as a TRANSIENT SERVICE (PID 1, fresh namespace), NOT a scope.
	// This is the exact lesson runUpdateScript already documents: rollback
	// stops casanova.service, so it has to live outside that cgroup (a scope
	// solves only that). But a `--scope` also runs as a child of THIS process
	// and inherits casanova.service's mount namespace — PrivateTmp and, fatally,
	// ProtectSystem=full, which mounts /usr read-only. The rollback's
	// `install .../usr/bin/casanova` then fails EROFS; with `set -euo pipefail`
	// and no ERR trap the script aborts right after stopping all nine services
	// and before restoring anything, leaving the box off with the tunnel down —
	// the recovery path bricking the box. Dropping --scope makes systemd-run
	// create a transient service run by PID 1 with a clean namespace, /usr
	// writable, like the update path. --collect reaps the unit so the fixed
	// --unit name is reusable. Output goes to the update log via the property,
	// never os.Stdout (casanova's journal fd closes on stop → SIGPIPE).
	prepareLogRotation("/var/lib/casanova/logs/update.log", "CASANOVA_UPDATE_LOG", "casanova-self-rollback")
	os.MkdirAll("/var/lib/casanova/logs", 0o755)
	cmd := exec.Command(
		"systemd-run",
		"--unit=casanova-self-rollback",
		"--description=CasaNova self-rollback",
		"--collect",
		"--property=StandardOutput=append:/var/lib/casanova/logs/update.log",
		"--property=StandardError=append:/var/lib/casanova/logs/update.log",
		"--setenv=CASANOVA_BACKUP_DIR="+rep.BackupLocation,
		"--",
		"/bin/bash", rollbackScript, "--rollback-upgrade",
	)
	if logF, err := os.OpenFile("/var/lib/casanova/logs/update.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout = logF
		cmd.Stderr = logF
	}
	if err := cmd.Start(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to start rollback: " + err.Error(),
		})
	}
	go cmd.Wait() // reap so we don't leave a zombie

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "rollback started",
		Data: map[string]interface{}{
			"backupLocation": rep.BackupLocation,
			"fromVersion":    rep.ToVersion,
			"toVersion":      rep.FromVersion,
		},
	})
}

// GET /v1/sys/update/check — force a check against the release channel.
// For now, checks for update packages in /var/lib/casanova/updates/.
// The future version will query a remote manifest URL.
func GetUpdateCheck(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	updateMu.Lock()
	updateState.Stage = StageChecking
	updateState.Message = "Checking for updates..."
	now := time.Now().UTC().Format(time.RFC3339)
	updateState.LastCheckedAt = &now
	updateMu.Unlock()

	updateMu.Lock()
	defer updateMu.Unlock()
	// Availability = the newest cached package is a genuine UPGRADE over the
	// running build (isVersionNewer), never merely "different". The old code
	// used `ver != BUILD_VERSION`, so a same-or-OLDER cached package (e.g. a
	// dev box running a hand-deployed binary newer than the staged tarball)
	// reported "update available" forever and re-offered a downgrade.
	updateState.Stage = StageIdle
	evaluateAvailability(&updateState)
	if updateState.UpdateAvailable && updateState.LatestVersion != nil {
		updateState.Message = fmt.Sprintf("Update available: %s", *updateState.LatestVersion)
	} else {
		updateState.Message = "CasaNova is up to date."
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    updateState,
	})
}

// POST /v1/sys/update/clear-cache — delete staged .tar.gz packages to free
// space. Offered once the system is up to date. The changelog.json is kept.
// Note: repairing afterwards needs a matching package re-staged.
func PostUpdateClearCache(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	dir := "/var/lib/casanova/updates"
	entries, _ := os.ReadDir(dir)
	removed := 0
	var freed int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tar.gz") || strings.HasSuffix(e.Name(), ".tgz") {
			p := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(p); err == nil {
				freed += fi.Size()
			}
			if os.Remove(p) == nil {
				removed++
			}
		}
	}
	updateMu.Lock()
	evaluateAvailability(&updateState)
	updateMu.Unlock()
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: fmt.Sprintf("Removed %d package(s), freed %dMiB", removed, freed/(1<<20)),
		Data:    updateState,
	})
}

// POST /v1/sys/update/repair — reinstall the CURRENT version's system files
// from its cached package (a system-file restore, à la Windows repair). It
// requires a cached package that MATCHES the running version, so it can never
// silently downgrade; if none is cached it asks the user to re-stage one.
func PostUpdateRepair(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	updateMu.Lock()
	if updateState.Stage != StageIdle && updateState.Stage != StageCompleted && updateState.Stage != StageFailed {
		updateMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{Success: common_err.SERVICE_ERROR, Message: "An update is already in progress"})
	}
	pkg := findPackageForVersion("/var/lib/casanova/updates", common.BUILD_VERSION)
	if pkg == "" {
		updateMu.Unlock()
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "No cached package matches the current version (" + common.BUILD_VERSION + "). Re-stage it via \"Load from disk\" to enable repair.",
		})
	}
	if free, need, ok := hasFreeSpaceForUpdate(pkg); !ok {
		updateMu.Unlock()
		return ctx.JSON(http.StatusInsufficientStorage, model.Result{Success: common_err.SERVICE_ERROR, Message: fmt.Sprintf("Not enough free space: need ~%dMiB, have %dMiB free", need/(1<<20), free/(1<<20))})
	}
	os.Remove(statusFilePath)
	jobID := fmt.Sprintf("repair-%d", time.Now().UnixMilli())
	updateJobID = jobID
	updateCancel = make(chan struct{})
	updateState.Stage = StageDownloading
	updateState.ProgressPercent = 0
	updateState.Message = "Reinstalling system files (repair)…"
	go runUpdateScript(pkg, jobID)
	updateMu.Unlock()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    StartUpdateResponse{JobID: jobID, Stage: StageDownloading},
	})
}

// POST /v1/sys/update/start — begin applying an update
func PostUpdateStart(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	var req StartUpdateRequest
	if err := ctx.Bind(&req); err != nil {
		// Bind is optional — start with defaults if body is empty
		req = StartUpdateRequest{}
	}

	updateMu.Lock()
	if updateState.Stage != StageIdle && updateState.Stage != StageCompleted && updateState.Stage != StageFailed {
		updateMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "An update is already in progress",
		})
	}

	// Find the package to apply
	updatesDir := "/var/lib/casanova/updates"
	pkg := findLatestPackage(updatesDir)
	if pkg == "" {
		updateMu.Unlock()
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "No update package found in " + updatesDir,
		})
	}

	// Preflight: refuse to start if the filesystem can't hold the extraction +
	// backup. The script extracts the tarball (~pkg size) AND backs up the old
	// binaries/www before swapping, so a mid-apply ENOSPC would leave a
	// half-installed system. Require free >= 3x the package size as headroom.
	if free, need, ok := hasFreeSpaceForUpdate(pkg); !ok {
		updateMu.Unlock()
		return ctx.JSON(http.StatusInsufficientStorage, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("Not enough free space to apply safely: need ~%dMiB, have %dMiB free in %s", need/(1<<20), free/(1<<20), updatesDir),
		})
	}

	// Remove the status file from any PREVIOUS update before spawning the
	// script. GetUpdateStatus prefers the file over in-memory state, so a
	// stale "failed" from a prior attempt would be reported as THIS update's
	// result during the window before the script writes its first entry.
	os.Remove(statusFilePath)

	jobID := fmt.Sprintf("update-%d", time.Now().UnixMilli())
	updateJobID = jobID
	updateCancel = make(chan struct{})
	updateState.Stage = StageDownloading
	updateState.ProgressPercent = 0
	updateState.Message = "Starting update..."

	// Run the update script in the background — spawned while still
	// holding the lock so no concurrent request can see "downloading"
	// state before the goroutine is actually running.
	go runUpdateScript(pkg, jobID)
	updateMu.Unlock()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: StartUpdateResponse{
			JobID: jobID,
			Stage: StageDownloading,
		},
	})
}

// POST /v1/sys/update/cancel — abort an in-flight update
func PostUpdateCancel(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	updateMu.Lock()
	defer updateMu.Unlock()

	if updateCancel != nil {
		close(updateCancel)
		updateCancel = nil
	}
	updateState.Stage = StageIdle
	updateState.ProgressPercent = 0
	updateState.Message = "Update cancelled."

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// GET /v1/sys/update/changelog — changelog for the target version
func GetUpdateChangelog(ctx echo.Context) error {
	// For now return a placeholder. In production this reads from the
	// package's manifest or a remote changelog endpoint.
	entries := []ChangelogEntry{}

	changelogFile := "/var/lib/casanova/updates/changelog.json"
	if data, err := os.ReadFile(changelogFile); err == nil {
		_ = json.Unmarshal(data, &entries)
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    entries,
	})
}

// POST /v1/sys/update/channel — switch release channel
func PostUpdateChannel(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	body := struct {
		Channel string `json:"channel"`
	}{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	ch := UpdateChannel(body.Channel)
	if ch != ChannelStable && ch != ChannelBeta && ch != ChannelNightly {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Invalid channel. Use: stable, beta, nightly",
		})
	}

	updateMu.Lock()
	updateState.Channel = ch
	updateState.Message = fmt.Sprintf("Switched to %s channel", ch)
	updateMu.Unlock()

	// Re-check for updates on the new channel
	// In production, this would query a different manifest URL
	updateMu.RLock()
	s := updateState
	updateMu.RUnlock()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    s,
	})
}

// POST /v1/sys/update/from-disk — apply an update from a .tar.gz already on
// the server's filesystem. The user provides the absolute path. This is the
// "sneaker-net" workflow: copy a package to a USB, mount it, and point the UI
// at the file.
func PostUpdateFromDisk(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	body := struct {
		Path        string `json:"path"`
		AllowReboot bool   `json:"allowReboot"`
	}{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "Invalid request body: " + err.Error(),
		})
	}

	pkg := strings.TrimSpace(body.Path)
	if pkg == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "path is required",
		})
	}

	// Must be an absolute path
	if !filepath.IsAbs(pkg) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "path must be absolute (e.g. /media/usb/casanova-5.0.0.tar.gz)",
		})
	}

	// Must end in .tar.gz or .tgz
	lower := strings.ToLower(pkg)
	if !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "file must be a .tar.gz or .tgz archive",
		})
	}

	// Must exist and be a regular file
	fi, err := os.Stat(pkg)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "File not found: " + pkg,
		})
	}
	if fi.IsDir() {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "path is a directory, not a file",
		})
	}

	// Refuse if an update is already running
	updateMu.Lock()
	if updateState.Stage != StageIdle && updateState.Stage != StageCompleted && updateState.Stage != StageFailed {
		updateMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "An update is already in progress",
		})
	}

	jobID := fmt.Sprintf("disk-update-%d", time.Now().UnixMilli())
	updateJobID = jobID
	updateCancel = make(chan struct{})
	updateState.Stage = StageApplying
	updateState.ProgressPercent = 5
	updateState.Message = fmt.Sprintf("Applying update from %s …", filepath.Base(pkg))
	ver := extractVersionFromPackage(pkg)
	updateState.LatestVersion = &ver
	sz := fi.Size()
	updateState.SizeBytes = &sz

	// Run the update script in the background — spawned while still
	// holding the lock so no concurrent request can see "applying"
	// state before the goroutine is actually running.
	go runUpdateScript(pkg, jobID)
	updateMu.Unlock()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: StartUpdateResponse{
			JobID: jobID,
			Stage: StageApplying,
		},
	})
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// runUpdateScript executes casanova-update.sh in the background.
// It writes progress to the status file which GetUpdateStatus polls.
func runUpdateScript(packagePath, jobID string) {
	// Ensure status file directory exists
	os.MkdirAll(filepath.Dir(statusFilePath), 0755)

	// Find the update script — check multiple locations
	script := ""
	candidates := []string{
		"/usr/share/casanova/tools/casanova-update.sh",
		"/var/lib/casanova/tools/casanova-update.sh",
		// During development, the script may be next to the binaries
		filepath.Join(filepath.Dir(os.Args[0]), "..", "share", "casanova", "tools", "casanova-update.sh"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			script = c
			break
		}
	}

	// If the script isn't installed yet (e.g. upgrading from dev9/dev10
	// which shipped without the tools/ directory), try to bootstrap it
	// from the update package itself. The tarball includes
	// <root>/tools/casanova-update.sh at depth 1 or 2.
	if script == "" {
		extracted, extractErr := extractUpdateScript(packagePath)
		if extractErr != nil || extracted == "" {
			updateMu.Lock()
			defer updateMu.Unlock()
			updateState.Stage = StageFailed
			updateState.ProgressPercent = 0
			msg := "Update failed: casanova-update.sh not found and could not be extracted from the package."
			if extractErr != nil {
				msg += " " + extractErr.Error()
			}
			updateState.Message = msg
			return
		}
		script = extracted
	}

	// The update script MUST run in a fresh transient systemd SERVICE, NOT
	// in casanova.service's cgroup or mount namespace. Three failure modes,
	// all fixed by the same choice:
	//
	//  1. cgroup kill — Phase 3 runs "systemctl stop casanova"; systemd's
	//     default KillMode=control-group would SIGTERM every process in the
	//     service cgroup, including this update script. A transient service
	//     lives in its OWN cgroup, so stopping casanova leaves it running.
	//
	//  2. PrivateTmp=yes — casanova.service gets a private /tmp on a tmpfs
	//     that systemd DESTROYS the instant the service stops. The script
	//     extracts the 300MB package into /tmp and verifies it in Phase 1;
	//     then Phase 3 stops casanova, the private tmpfs is torn down, and
	//     the extracted tree VANISHES mid-update — Phase 4 finds no binaries
	//     and no www ("cannot stat" for every asset) and rolls back. A
	//     transient service has no PrivateTmp: it uses the real host /tmp,
	//     which survives the stop.
	//
	//  3. ProtectSystem=full — casanova.service mounts /usr read-only in its
	//     namespace, so "install .../usr/bin/casanova" fails with EROFS. A
	//     transient service has no ProtectSystem: /usr is writable.
	//
	// An earlier version used `systemd-run --scope`, which only solved (1):
	// a scope runs the command as a child of the CALLER and INHERITS its
	// mount namespace, so it dragged PrivateTmp and ProtectSystem along and
	// broke every real update. Dropping --scope makes systemd-run create a
	// transient service run by PID 1 with a fresh namespace — no inheritance.
	//
	// We use cmd.Start() (not Run) because this Go process is killed when the
	// script stops casanova. systemd-run returns as soon as the unit starts,
	// so Start() suffices. Progress is tracked via the status file, which the
	// NEW casanova process reads after the restart.
	//
	// StandardOutput/Error=append pins the script's own stdout to a persistent
	// file owned by the transient unit (independent of casanova's journal fd,
	// which Phase 3 closes — writing to it would SIGPIPE the script, the
	// 2026-07-16 incident). --collect reaps the unit after it exits so the
	// fixed --unit name is reusable next time.
	prepareLogRotation("/var/lib/casanova/logs/update.log", "CASANOVA_UPDATE_LOG", "casanova-self-update")
	os.MkdirAll("/var/lib/casanova/logs", 0o755)
	cmd := exec.Command(
		"systemd-run",
		"--unit=casanova-self-update",
		"--description=CasaNova self-update",
		"--collect",
		"--property=StandardOutput=append:/var/lib/casanova/logs/update.log",
		"--property=StandardError=append:/var/lib/casanova/logs/update.log",
		"--",
		"bash", script,
		"--status-file="+statusFilePath,
		packagePath,
	)
	// cmd.Stdout here only captures systemd-run's own launch line ("Running
	// as unit: …") and any launch error — the script's real output goes to
	// the transient unit's log via the StandardOutput property above.
	if logF, err := os.OpenFile("/var/lib/casanova/logs/update.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout = logF
		cmd.Stderr = logF
	}

	if err := cmd.Start(); err != nil {
		updateMu.Lock()
		defer updateMu.Unlock()
		updateState.Stage = StageFailed
		updateState.ProgressPercent = 0
		updateState.Message = fmt.Sprintf("Failed to launch update script: %v", err)
		return
	}

	// Don't cmd.Wait() — the casanova process will be killed during
	// the update. The script writes progress to the status file;
	// after restart the new casanova process picks it up via
	// GetUpdateStatus which polls readStatusFile().
}

// readStatusFile parses the JSON status file written by the update script
func readStatusFile() (*statusFileState, error) {
	data, err := os.ReadFile(statusFilePath)
	if err != nil {
		return nil, err
	}
	var s statusFileState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// hasFreeSpaceForUpdate checks the filesystem holding the backup/extraction
// area has room for the apply. Requires 3x the package size (extract + backup +
// headroom). Fails OPEN (ok=true) on any stat error so a preflight glitch never
// blocks a legitimate update.
func hasFreeSpaceForUpdate(pkgPath string) (free int64, need int64, ok bool) {
	fi, err := os.Stat(pkgPath)
	if err != nil {
		return 0, 0, true
	}
	need = fi.Size() * 3
	var st syscall.Statfs_t
	if err := syscall.Statfs("/var/lib/casanova", &st); err != nil {
		return 0, need, true
	}
	free = int64(st.Bavail) * int64(st.Bsize)
	return free, need, free >= need
}

// findLatestPackage returns the HIGHEST-VERSIONED .tar.gz in the given
// directory (filename version token, compared with the same logic the
// updater uses). It used to pick by mtime — a `touch` on an old tarball
// made it "the latest", and while evaluateAvailability's newer-than-current
// check hid the downgrade offer, PostUpdateStart would still apply that
// stale file. Files without a parseable version token lose to any file
// with one; mtime only breaks exact-version ties.
func findLatestPackage(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var latest, latestVer string
	var latestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ver := extractVersionFromPackage(path)
		if ver == "unknown" {
			ver = ""
		}
		better := false
		switch {
		case latest == "":
			better = true
		case ver != "" && latestVer == "":
			better = true
		case ver == "" && latestVer != "":
			better = false
		case ver != "" && latestVer != "":
			if cmp := compareBuildVersions(ver, latestVer); cmp != 0 {
				better = cmp > 0
				break
			}
			fallthrough
		default:
			if info, ierr := e.Info(); ierr == nil && info.ModTime().After(latestTime) {
				better = true
			}
		}
		if better {
			latest, latestVer = path, ver
			if info, ierr := e.Info(); ierr == nil {
				latestTime = info.ModTime()
			}
		}
	}
	return latest
}

// versionTokenRe matches a plausible version token — a leading digit or
// "v" followed by a digit, then dots / alphanumerics / dashes. Anything
// failing this is treated as unparseable, so a malformed
// `casanova-x86_64.tar.gz` returns "unknown" instead of "x86_64".
var versionTokenRe = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.]*$`)

// extractVersionFromPackage tries to extract the version string from
// the package filename (casanova-v5.0.0-x86_64.tar.gz → v5.0.0). When
// the token doesn't look like a version, returns "unknown" rather than
// surfacing an arch / channel marker as a fake version.
func extractVersionFromPackage(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	parts := strings.SplitN(base, "-", 3)
	if len(parts) >= 2 && versionTokenRe.MatchString(parts[1]) {
		return parts[1]
	}
	return "unknown"
}

// ── Version comparison ───────────────────────────────────────────────────
// Build versions look like "18.6.1.dev1.1707261644": MAJOR.MINOR.PATCH .
// <channel><num> . <timestamp>. We order by semver first, then by the build
// timestamp. NOTE the Makefile stamps the timestamp as `date +%d%m%y%H%M`
// (DDMMYYHHMM), which is NOT numerically monotonic across month boundaries —
// so we reorder it to YYMMDDHHMM before comparing.

type buildVersion struct {
	nums []int // [18,6,1]
	ts   int64 // sortable YYMMDDHHMM, 0 if absent
}

func sortableTimestamp(s string) int64 {
	if len(s) == 10 { // DDMMYYHHMM → YYMMDDHHMM
		reordered := s[4:6] + s[2:4] + s[0:2] + s[6:8] + s[8:10]
		if n, err := strconv.ParseInt(reordered, 10, 64); err == nil {
			return n
		}
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func parseBuildVersion(v string) buildVersion {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	bv := buildVersion{}
	for _, p := range strings.Split(v, ".") {
		if p == "" {
			continue
		}
		if len(p) >= 8 { // the timestamp segment
			bv.ts = sortableTimestamp(p)
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			bv.nums = append(bv.nums, n) // a semver component
		}
		// a channel token like "dev1" carries no ordering weight → ignore
	}
	return bv
}

// compareBuildVersions returns -1/0/1 for a<b / a==b / a>b.
func compareBuildVersions(a, b string) int {
	pa, pb := parseBuildVersion(a), parseBuildVersion(b)
	for i := 0; i < len(pa.nums) || i < len(pb.nums); i++ {
		var x, y int
		if i < len(pa.nums) {
			x = pa.nums[i]
		}
		if i < len(pb.nums) {
			y = pb.nums[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if pa.ts != pb.ts {
		if pa.ts < pb.ts {
			return -1
		}
		return 1
	}
	return 0
}

// isVersionNewer reports whether candidate is a genuine upgrade over running —
// the ONLY condition under which we surface "update available". A same or older
// cached package is NOT an update (that's what "reinstall/repair" is for).
func isVersionNewer(candidate, running string) bool {
	return compareBuildVersions(candidate, running) > 0
}

// evaluateAvailability (re)computes CurrentVersion/LatestVersion/UpdateAvailable
// /SizeBytes from the newest cached package vs the RUNNING build. Called by both
// the explicit check and every idle status poll so the panel self-corrects the
// instant the running version changes after an apply (no stale "available").
func evaluateAvailability(s *UpdateStatus) {
	s.CurrentVersion = common.BUILD_VERSION
	latestPkg := findLatestPackage("/var/lib/casanova/updates")
	if latestPkg == "" {
		s.LatestVersion = nil
		s.UpdateAvailable = false
		s.SizeBytes = nil
		return
	}
	ver := extractVersionFromPackage(latestPkg)
	s.LatestVersion = &ver
	s.UpdateAvailable = ver != "" && ver != "unknown" && isVersionNewer(ver, common.BUILD_VERSION)
	if fi, _ := os.Stat(latestPkg); fi != nil {
		sz := fi.Size()
		s.SizeBytes = &sz
	}
}

// findPackageForVersion returns the cached package whose version matches the
// given one exactly, or "" if none. Used by repair so it reinstalls the CURRENT
// version's files (never a silent downgrade to whatever the newest cache is).
func findPackageForVersion(dir, version string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if extractVersionFromPackage(full) == version {
			return full
		}
	}
	return ""
}

// extractUpdateScript opens a CasaNova update tarball and extracts
// tools/casanova-update.sh to /usr/share/casanova/tools/. This
// bootstraps the update engine on servers that were installed before
// the tools/ directory was included in install.sh (dev9 and dev10).
//
// The tarball layout is either flat (tools/casanova-update.sh at root)
// or nested one level deep (casanova-full-5.0.0.dev11/tools/casanova-update.sh).
// We accept both by matching any entry whose base path ends in
// "tools/casanova-update.sh".
func extractUpdateScript(tarballPath string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read: %w", err)
		}

		// Match any entry ending in tools/casanova-update.sh
		name := filepath.Clean(hdr.Name)
		if !strings.HasSuffix(name, "tools/casanova-update.sh") &&
			!strings.HasSuffix(name, "tools"+string(filepath.Separator)+"casanova-update.sh") {
			continue
		}

		// Found it — extract to the canonical location.
		destDir := "/usr/share/casanova/tools"
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		destPath := filepath.Join(destDir, "casanova-update.sh")

		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", fmt.Errorf("create: %w", err)
		}
		// Cap the read at 1 MiB — the real script is ~15 KiB.
		if _, err := io.Copy(out, io.LimitReader(tr, 1<<20)); err != nil {
			out.Close()
			os.Remove(destPath)
			return "", fmt.Errorf("write: %w", err)
		}
		out.Close()
		return destPath, nil
	}
	return "", fmt.Errorf("tools/casanova-update.sh not found in archive")
}

// ─── Unused import suppressors ──────────────────────────────────────────────
var _ = strconv.Itoa
