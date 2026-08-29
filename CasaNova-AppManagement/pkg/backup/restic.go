// This file extends the backup package with a Restic wrapper. Restic is
// a deduplicating, encrypted, cross-platform backup tool that handles:
//
//   - Content-addressed storage (same bytes across backups stored once)
//   - AES-256 encryption at rest
//   - Incremental snapshots that are mountable via `restic mount`
//   - Arbitrary backends: local path, S3, B2, Azure, GCS, SFTP, Rclone
//   - Retention via `restic forget --keep-daily N --keep-weekly N ...`
//
// We shell out to the `restic` binary rather than importing the library —
// upstream exposes its internals as `internal/` packages and breaks them
// across releases. The binary interface is stable and well-documented.
//
// Restic is expected to be installed on the host (apt install restic,
// or `brew install restic`). If it's missing, every operation returns
// ErrResticNotInstalled so the UI can show a clear setup prompt instead
// of a cryptic exec error.

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// ErrResticNotInstalled is returned when the `restic` binary is not on PATH.
// The UI should catch this and prompt the user to install it.
var ErrResticNotInstalled = errors.New("restic binary not found in PATH; install it with your package manager")

// ResticRepo describes a single Restic repository the user has configured.
// Repositories map 1:1 to a backend URL and a password.
type ResticRepo struct {
	// URL is the Restic repository URL: /path for local, s3:... for S3,
	// b2:... for Backblaze, sftp:user@host:/path, rclone:remote:path, etc.
	// The restic binary routes based on the scheme prefix.
	URL string `json:"url"`

	// Password encrypts every object in the repo. Restic refuses to init
	// a repo without one. The plaintext is stored in the schedule JSON;
	// an operator running a multi-user system should consider encrypting
	// the schedule file with a machine-local key (future work).
	Password string `json:"password"`
}

// Snapshot is a single point-in-time backup inside a repo.
// Matches the shape Restic emits from `restic snapshots --json`.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags,omitempty"`
}

// RetentionPolicy mirrors Restic's forget flags. A zero value means "keep
// forever" for that bucket — the default policy keeps 7 daily, 4 weekly,
// 6 monthly snapshots which is a reasonable default for home use.
type RetentionPolicy struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepHourly  int `json:"keep_hourly,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

// DefaultRetention returns a sensible policy for home-server use.
// 7 days + 4 weeks + 6 months of history, no hourly or yearly.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
}

// ResticManager wraps the restic CLI against a specific repository.
// It's cheap to construct — there's no connection to keep open.
type ResticManager struct {
	repo ResticRepo
}

// NewRestic returns a manager bound to a single repository.
func NewRestic(repo ResticRepo) *ResticManager {
	return &ResticManager{repo: repo}
}

// available reports whether the restic binary exists on PATH.
// Called as a pre-flight for every operation so the error is deterministic.
func available() error {
	if _, err := exec.LookPath("restic"); err != nil {
		return ErrResticNotInstalled
	}
	return nil
}

// Init creates a new Restic repository at the configured URL. Fails if
// the repo already exists (Restic refuses to re-init over a live repo).
// Safe to call repeatedly — the caller can ignore "already initialized"
// errors by checking Exists() first.
func (r *ResticManager) Init(ctx context.Context) error {
	if err := available(); err != nil {
		return err
	}
	cmd := r.command(ctx, "init")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restic init: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	logger.Info("restic repo initialized", zap.String("url", r.repo.URL))
	return nil
}

// Exists returns true if the repository is reachable and initialized.
// Implemented as `restic cat config` which is the cheapest round-trip
// that distinguishes "missing" from "wrong password" from "unreachable".
func (r *ResticManager) Exists(ctx context.Context) bool {
	if available() != nil {
		return false
	}
	cmd := r.command(ctx, "cat", "config")
	return cmd.Run() == nil
}

// BackupPaths runs a backup of the given filesystem paths into the repo.
// Tags are free-form labels applied to the snapshot — CasaNova uses them
// to scope retention per app ("app:plex", "kind:scheduled").
func (r *ResticManager) BackupPaths(ctx context.Context, paths []string, tags ...string) (*Snapshot, error) {
	if err := available(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("restic backup: no paths supplied")
	}

	args := []string{"backup", "--json"}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	// "--" terminates flag parsing so user-supplied paths can't be
	// interpreted as restic flags (e.g., "--exclude=...").
	args = append(args, "--")
	args = append(args, paths...)

	cmd := r.command(ctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("restic backup: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	// Restic --json emits a stream of events; the last line with
	// message_type:summary has the snapshot id.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var summary struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &summary); err == nil && summary.MessageType == "summary" {
			return &Snapshot{
				ID:      summary.SnapshotID,
				ShortID: shortID(summary.SnapshotID),
				Time:    time.Now(),
				Paths:   paths,
				Tags:    tags,
			}, nil
		}
	}
	// Backup succeeded but we couldn't parse the summary — return a
	// minimal snapshot so the caller knows the operation worked.
	return &Snapshot{Time: time.Now(), Paths: paths, Tags: tags}, nil
}

// Snapshots lists every snapshot in the repo. Optional tag filters narrow
// to snapshots bearing all the given tags (AND semantics, not OR).
func (r *ResticManager) Snapshots(ctx context.Context, tags ...string) ([]Snapshot, error) {
	if err := available(); err != nil {
		return nil, err
	}
	args := []string{"snapshots", "--json"}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	cmd := r.command(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	// Populate ShortID for display (Restic's own CLI shows the first 8 chars).
	for i := range snaps {
		snaps[i].ShortID = shortID(snaps[i].ID)
	}
	return snaps, nil
}

// Restore extracts a snapshot to a target filesystem path. Target must
// be writable; Restic will create missing directories. The snapshot ID
// can be a short ID (first 8 chars) or "latest" for the most recent.
// validResticSnapshotID matches restic snapshot IDs (hex) or "latest".
var validResticSnapshotID = regexp.MustCompile(`^(?:latest|[a-f0-9]{8,64})$`)

func (r *ResticManager) Restore(ctx context.Context, snapshotID, target string) error {
	if err := available(); err != nil {
		return err
	}
	if snapshotID == "" {
		snapshotID = "latest"
	}
	// Validate snapshotID to prevent argument injection via crafted IDs.
	if !validResticSnapshotID.MatchString(snapshotID) {
		return fmt.Errorf("invalid snapshot ID: %q", snapshotID)
	}
	cmd := r.command(ctx, "restore", snapshotID, "--target", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restic restore: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	logger.Info("restic restore complete",
		zap.String("snapshot", snapshotID),
		zap.String("target", target))
	return nil
}

// Forget applies a retention policy and prunes freed data. This is the
// single call that enforces "keep 7 daily, 4 weekly" etc. — pass tags to
// scope retention per app (otherwise the policy applies repo-wide).
func (r *ResticManager) Forget(ctx context.Context, policy RetentionPolicy, tags ...string) error {
	if err := available(); err != nil {
		return err
	}
	args := []string{"forget", "--prune"}
	if policy.KeepLast > 0 {
		args = append(args, "--keep-last", fmt.Sprintf("%d", policy.KeepLast))
	}
	if policy.KeepHourly > 0 {
		args = append(args, "--keep-hourly", fmt.Sprintf("%d", policy.KeepHourly))
	}
	if policy.KeepDaily > 0 {
		args = append(args, "--keep-daily", fmt.Sprintf("%d", policy.KeepDaily))
	}
	if policy.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", fmt.Sprintf("%d", policy.KeepWeekly))
	}
	if policy.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", fmt.Sprintf("%d", policy.KeepMonthly))
	}
	if policy.KeepYearly > 0 {
		args = append(args, "--keep-yearly", fmt.Sprintf("%d", policy.KeepYearly))
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	cmd := r.command(ctx, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restic forget: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stats returns repository size, snapshot count, and dedup ratio, parsed
// from `restic stats --json`. Used by the UI to show how much space each
// repository is consuming.
type Stats struct {
	TotalSize      uint64 `json:"total_size"`
	TotalFileCount uint64 `json:"total_file_count"`
	SnapshotCount  uint64 `json:"snapshots_count"`
}

func (r *ResticManager) Stats(ctx context.Context) (*Stats, error) {
	if err := available(); err != nil {
		return nil, err
	}
	cmd := r.command(ctx, "stats", "--json", "--mode=raw-data")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic stats: %w", err)
	}
	var s Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parse stats: %w", err)
	}
	return &s, nil
}

// command builds an exec.Cmd with the repo URL and password wired through
// environment variables. Using env vars (not argv) keeps the password out
// of `ps` listings visible to other local users.
func (r *ResticManager) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+r.repo.URL,
		"RESTIC_PASSWORD="+r.repo.Password,
	)
	return cmd
}

// shortID truncates a snapshot ID for display. Restic's convention is
// the first 8 hex characters of the SHA-256.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
