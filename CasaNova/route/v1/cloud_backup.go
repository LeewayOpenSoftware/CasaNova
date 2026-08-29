package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

const cloudBackupConfigPath = "/var/lib/casanova/cloud.json"

var cloudRemoteNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ---- persistent config ----

type cloudRemoteEntry struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Configured bool   `json:"configured"`
	LastSync   string `json:"last_sync,omitempty"`
}

type cloudBackupConfig struct {
	Remotes    []cloudRemoteEntry `json:"remotes"`
	SyncPaths  []string           `json:"sync_paths"`
	Schedule   string             `json:"schedule"`
	RemotePath string             `json:"remote_path"`
}

// ---- sync state (in-memory) ----

type cloudSyncState struct {
	Running   bool   `json:"running"`
	Progress  string `json:"progress"`
	StartedAt string `json:"started_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

var (
	currentCloudSync   cloudSyncState
	currentCloudSyncMu sync.Mutex
)

// ---- provider list ----

type cloudProviderInfo struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

var cloudProvidersList = []cloudProviderInfo{
	{Type: "drive", Name: "Google Drive", Icon: "google-drive"},
	{Type: "dropbox", Name: "Dropbox", Icon: "dropbox"},
	{Type: "s3", Name: "Amazon S3", Icon: "aws"},
	{Type: "onedrive", Name: "OneDrive", Icon: "onedrive"},
	{Type: "b2", Name: "Backblaze B2", Icon: "backblaze"},
	{Type: "sftp", Name: "SFTP Server", Icon: "server"},
}

// ---- rclone execution ----

const (
	// `config create` only writes rclone's config file for the backends this
	// dialog can actually drive (S3/B2/SFTP key=value pairs), so a minute is
	// already generous. It is deliberately not longer: the OAuth backends
	// need `rclone authorize` from a terminal — the dialog says as much — and
	// that flow parks on a browser callback, so stretching the deadline here
	// would only hold an HTTP handler open for a wait that cannot finish
	// through this endpoint.
	cloudRemoteCreateTimeout = 60 * time.Second

	// Budget for the `lsd` connection test. This is a real network round
	// trip, and a perfectly healthy remote routinely needs more than the 30s
	// this used to allow: Drive refreshes its OAuth token and lists a root
	// with hundreds of folders, S3/B2 answer from a distant region and back
	// off under rate limits, SFTP crosses a WAN. The remote is already
	// written to rclone's config and to cloud.json by the time we get here,
	// so a deadline that fires early does not undo anything — it just tells
	// the user their working remote is broken.
	cloudRemoteTestTimeout = 120 * time.Second
)

// rclone talks to the network on the request path: `config create` runs the
// provider handshake for OAuth backends, `lsd` is the connection test, and
// even `listremotes` can sit on a config file that lives on a stalled mount.
// Without a deadline an unresponsive provider pins the HTTP handler and its
// goroutine for the life of the process, and all the user sees is the Backups
// page spinning with no way to cancel. Bound every child and surface the
// timeout as a real error.
func rcloneExec(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "rclone", args...).Output()
	return out, rcloneTimeoutErr(ctx, timeout, err)
}

func rcloneExecCombined(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "rclone", args...).CombinedOutput()
	return out, rcloneTimeoutErr(ctx, timeout, err)
}

// rcloneTimeout is the error a child killed by our own deadline reports, as
// opposed to one rclone itself failed on. It carries the same text the plain
// error used to, so nothing that only prints it changes; callers that can say
// "we could not verify this" instead of "this is broken" pick it out with
// errors.As.
type rcloneTimeout struct {
	timeout time.Duration
}

func (e rcloneTimeout) Error() string {
	return fmt.Sprintf("rclone did not respond within %s", e.timeout)
}

// rcloneTimeoutErr replaces the bare "signal: killed" a cancelled child
// reports with something the UI can show. It never names the arguments:
// `config create` carries the provider's tokens and passwords and these
// messages are handed straight back to the browser.
func rcloneTimeoutErr(ctx context.Context, timeout time.Duration, err error) error {
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return rcloneTimeout{timeout: timeout}
	}
	return err
}

// rcloneFailure renders a failed run for the UI: rclone's own message when it
// managed to print one, otherwise the error — which is what a deadline looks
// like, since the child is killed before it writes anything.
func rcloneFailure(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}

// ---- config helpers ----

func loadCloudBackupConfig() (cloudBackupConfig, error) {
	cfg := cloudBackupConfig{
		Remotes:    []cloudRemoteEntry{},
		SyncPaths:  []string{"/etc/casaos", "/var/lib/casaos/db"},
		Schedule:   "manual",
		RemotePath: "CasaNova-Backup",
	}

	data, err := os.ReadFile(cloudBackupConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Remotes == nil {
		cfg.Remotes = []cloudRemoteEntry{}
	}
	if cfg.SyncPaths == nil {
		cfg.SyncPaths = []string{}
	}
	return cfg, nil
}

func saveCloudBackupConfig(cfg cloudBackupConfig) error {
	if err := os.MkdirAll("/var/lib/casanova", 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Wave 35-R: previously this was write-tmp + fsync(file) + rename,
	// which on btrfs/xfs/zfs lets the rename land in a different
	// transaction from the data. A power loss between them yields a
	// renamed-but-empty cloud.json, which wipes every configured
	// remote and every saved schedule in one go — worse, the rclone
	// config itself survives so the server silently forgets about
	// the backups it was supposed to be running without any visible
	// error. commonfile.WriteFileAtomic adds the parent-dir fsync so
	// the rename is durable before we return success.
	return commonfile.WriteFileAtomic(cloudBackupConfigPath, data, 0o644)
}

// ---- handlers ----

// GET /v1/sys/cloud/status — cloud backup status overview.
func GetCloudBackupStatus(ctx echo.Context) error {
	_, rcloneErr := exec.LookPath("rclone")
	installed := rcloneErr == nil

	cfg, _ := loadCloudBackupConfig()

	// Query rclone for live configured remotes.
	var liveRemotes []string
	if installed {
		out, err := rcloneExec(10*time.Second, "listremotes")
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				line = strings.TrimSpace(strings.TrimSuffix(line, ":"))
				if line != "" {
					liveRemotes = append(liveRemotes, line)
				}
			}
		}
	}

	// Find the most recent sync across all remotes.
	lastSync := ""
	for _, r := range cfg.Remotes {
		if r.LastSync != "" && r.LastSync > lastSync {
			lastSync = r.LastSync
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"rclone_installed": installed,
			"remotes":          cfg.Remotes,
			"sync_paths":       cfg.SyncPaths,
			"schedule":         cfg.Schedule,
			"remote_path":      cfg.RemotePath,
			"last_sync":        lastSync,
			"live_remotes":     liveRemotes,
		},
	})
}

// GET /v1/sys/cloud/providers — list available cloud provider types.
func GetCloudBackupProviders(ctx echo.Context) error {
	// Gated: names the configured cloud accounts; PermSystemRead is held by guest.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    cloudProvidersList,
	})
}

// POST /v1/sys/cloud/remotes — add/configure a new rclone remote.
func PostCloudBackupRemote(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Name   string            `json:"name"`
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if !cloudRemoteNameRe.MatchString(body.Name) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid remote name: use alphanumeric characters, underscores, and dashes only",
		})
	}

	// Validate type — must be a simple alphanumeric rclone provider name.
	if !cloudRemoteNameRe.MatchString(body.Type) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid provider type: use alphanumeric characters only",
		})
	}

	if _, err := exec.LookPath("rclone"); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "rclone is not installed",
		})
	}

	// Build: rclone config create <name> <type> key=value ...
	// Reject config keys/values that start with "--" to prevent argument injection.
	args := []string{"config", "create", body.Name, body.Type}
	for k, v := range body.Config {
		if strings.HasPrefix(k, "-") || strings.HasPrefix(v, "-") {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: fmt.Sprintf("invalid config key/value: must not start with dash: %s", k),
			})
		}
		args = append(args, fmt.Sprintf("%s=%s", k, v))
	}

	out, err := rcloneExecCombined(cloudRemoteCreateTimeout, args...)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create remote: " + rcloneFailure(out, err),
		})
	}

	// Test the connection.
	testOut, testErr := rcloneExecCombined(cloudRemoteTestTimeout, "lsd", body.Name+":")
	testOk := testErr == nil
	testMessage := strings.TrimSpace(string(testOut))
	var testTimeout rcloneTimeout
	switch {
	case testOk:
		// Keep rclone's own listing as the message, empty root included.
	case errors.As(testErr, &testTimeout):
		// Our deadline fired, which says nothing about the remote: it is
		// already created and persisted, and a slow-but-healthy provider
		// reaches this path routinely. test_ok stays false because we did
		// not verify anything and must not claim we did, but the reason has
		// to read as "unverified" rather than "broken" — that boolean is all
		// the response carries, so the distinction lives in the message the
		// UI prints under it.
		testMessage = fmt.Sprintf("Connection not verified: the remote was created and saved, but listing it did not finish within %s. Slow or rate-limited providers hit this even when they work — run a sync to confirm.", testTimeout.timeout)
	case testMessage == "":
		// A killed child prints nothing, so without this the UI reported a
		// failed connection test with an empty reason.
		testMessage = testErr.Error()
	}

	// Persist to cloud.json.
	cfg, _ := loadCloudBackupConfig()
	filtered := make([]cloudRemoteEntry, 0, len(cfg.Remotes))
	for _, r := range cfg.Remotes {
		if r.Name != body.Name {
			filtered = append(filtered, r)
		}
	}
	cfg.Remotes = append(filtered, cloudRemoteEntry{
		Name:       body.Name,
		Type:       body.Type,
		Configured: true,
	})
	saveCloudBackupConfig(cfg)

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"name":         body.Name,
			"type":         body.Type,
			"test_ok":      testOk,
			"test_message": testMessage,
		},
	})
}

// DELETE /v1/sys/cloud/remotes/:name — remove a configured remote.
func DeleteCloudBackupRemote(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	name := ctx.Param("name")
	if !cloudRemoteNameRe.MatchString(name) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid remote name",
		})
	}

	// Remove from rclone config.
	if _, err := exec.LookPath("rclone"); err == nil {
		rcloneExecCombined(10*time.Second, "config", "delete", name)
	}

	// Remove from cloud.json.
	cfg, _ := loadCloudBackupConfig()
	filtered := make([]cloudRemoteEntry, 0, len(cfg.Remotes))
	for _, r := range cfg.Remotes {
		if r.Name != name {
			filtered = append(filtered, r)
		}
	}
	cfg.Remotes = filtered
	saveCloudBackupConfig(cfg)

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// POST /v1/sys/cloud/sync — trigger a sync now (runs in background).
func PostCloudBackupSync(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Remote     string   `json:"remote"`
		Paths      []string `json:"paths"`
		RemotePath string   `json:"remote_path"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if body.Remote == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "remote name is required",
		})
	}

	if _, err := exec.LookPath("rclone"); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "rclone is not installed",
		})
	}

	currentCloudSyncMu.Lock()
	if currentCloudSync.Running {
		currentCloudSyncMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "a sync is already running",
		})
	}
	currentCloudSync = cloudSyncState{
		Running:   true,
		Progress:  "Starting...",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	currentCloudSyncMu.Unlock()

	// Fall back to config values when body fields are empty.
	paths := body.Paths
	if len(paths) == 0 {
		cfg, _ := loadCloudBackupConfig()
		paths = cfg.SyncPaths
	}
	remotePath := body.RemotePath
	if remotePath == "" {
		cfg, _ := loadCloudBackupConfig()
		remotePath = cfg.RemotePath
	}

	remote := body.Remote // capture before handler returns
	go func() {
		var lastErr string
		// Wave 35-X: cleanup runs regardless of panic. Without this,
		// any panic inside the rclone exec wrapper or config save
		// stranded `currentCloudSync.Running = true` forever — the
		// user could never start another sync until daemon restart.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("cloud backup sync panicked",
					zap.Any("panic", r), zap.String("remote", remote))
				lastErr = fmt.Sprintf("internal error: %v", r)
			}
			currentCloudSyncMu.Lock()
			currentCloudSync.Running = false
			if lastErr != "" {
				currentCloudSync.Progress = "Failed"
				currentCloudSync.LastError = lastErr
			} else {
				currentCloudSync.Progress = "Completed"
				currentCloudSync.LastError = ""
			}
			currentCloudSyncMu.Unlock()
		}()
		// Wave 35-X: bound each rclone child to 6h. A wedged rclone
		// process (stuck mount, dead remote, hung token refresh) used
		// to run forever — sysadmin had to manually kill the process
		// to clear the sync state.
		for i, localPath := range paths {
			parts := strings.Split(strings.TrimSuffix(localPath, "/"), "/")
			dirName := parts[len(parts)-1]
			dest := fmt.Sprintf("%s:%s/%s/", remote, remotePath, dirName)

			currentCloudSyncMu.Lock()
			currentCloudSync.Progress = fmt.Sprintf("Syncing %d/%d: %s", i+1, len(paths), localPath)
			currentCloudSyncMu.Unlock()

			rcloneCtx, rcloneCancel := context.WithTimeout(context.Background(), 6*time.Hour)
			out, err := exec.CommandContext(rcloneCtx, "rclone", "sync", localPath, dest, "--stats-one-line", "-v").CombinedOutput()
			rcloneCancel()
			if err != nil {
				lastErr = fmt.Sprintf("%s: %s", localPath, strings.TrimSpace(string(out)))
			}
		}

		// Update last_sync timestamp under the sync mutex to prevent
		// concurrent config read-modify-write races.
		currentCloudSyncMu.Lock()
		now := time.Now().UTC().Format(time.RFC3339)
		cfg, _ := loadCloudBackupConfig()
		for i, r := range cfg.Remotes {
			if r.Name == remote {
				cfg.Remotes[i].LastSync = now
				break
			}
		}
		saveCloudBackupConfig(cfg)
		currentCloudSyncMu.Unlock()
	}()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"status": "syncing",
		},
	})
}

// GET /v1/sys/cloud/sync/status — current sync progress.
func GetCloudBackupSyncStatus(ctx echo.Context) error {
	currentCloudSyncMu.Lock()
	state := currentCloudSync
	currentCloudSyncMu.Unlock()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    state,
	})
}

// POST /v1/sys/cloud/config — update sync configuration.
func PostCloudBackupConfig(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		SyncPaths  []string `json:"sync_paths"`
		Schedule   string   `json:"schedule"`
		RemotePath string   `json:"remote_path"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	switch body.Schedule {
	case "manual", "daily", "weekly":
		// valid
	default:
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "schedule must be one of: manual, daily, weekly",
		})
	}

	cfg, _ := loadCloudBackupConfig()
	if body.SyncPaths != nil {
		cfg.SyncPaths = body.SyncPaths
	}
	cfg.Schedule = body.Schedule
	if body.RemotePath != "" {
		cfg.RemotePath = body.RemotePath
	}

	if err := saveCloudBackupConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    cfg,
	})
}
