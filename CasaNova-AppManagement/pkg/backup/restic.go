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

// resticBinary is the name of the binary every operation shells out to.
// Kept indirect so a test can point it at a stub and exercise this file's
// exit-code classification without a real repository — the alternative is a
// test that silently passes on any box where restic is not installed, which
// is the same class of lie this file is being fixed for.
var resticBinary = "restic"

// available reports whether the restic binary exists on PATH.
// Called as a pre-flight for every operation so the error is deterministic.
func available() error {
	if _, err := exec.LookPath(resticBinary); err != nil {
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

// resticRepoMissing matches the ways restic says "there is no repository
// here yet", across the versions we support. Restic 0.17 introduced exit
// code 10 for exactly this; older builds only say it in the message, so both
// are recognised and anything else is explicitly NOT recognised.
var resticRepoMissing = regexp.MustCompile(`(?i)(repository does not exist|no repository config file|unable to open config file)`)

// resticExitRepoDoesNotExist is restic >= 0.17's exit code for "repository
// does not exist". Its siblings (11 = cannot lock, 12 = wrong password) are
// deliberately NOT treated as absence.
const resticExitRepoDoesNotExist = 10

// Exists reports whether the repository is present and initialized.
//
// The bool is only meaningful when the error is nil. This shape is the whole
// point of the function: the previous version was `return cmd.Run() == nil`,
// which collapsed an unreachable repo, a wrong password, a network timeout
// and an unmounted disk all into the same `false` that means "not created
// yet" — while the doc-comment above it claimed it distinguished exactly
// those cases.
//
// That mattered because of the one caller. backupScheduler.runOnce asks this
// to decide whether to Init, and Init on a false negative creates a brand new
// empty repository at the mountpoint of a disk that simply was not mounted.
// From then on the nightly runs "succeed" into the rootfs, the real history
// is invisible to ListSnapshots, and retention prunes the phantom. The right
// answer to "I could not tell" is to stop, so this now says so out loud
// instead of guessing "no".
//
// Absence is reported only on a POSITIVE identification of absence: restic's
// dedicated exit code, or its own wording. Everything else is an error.
func (r *ResticManager) Exists(ctx context.Context) (bool, error) {
	if err := available(); err != nil {
		return false, err
	}
	cmd := r.command(ctx, "cat", "config")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}

	text := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == resticExitRepoDoesNotExist {
		return false, nil
	}
	if resticRepoMissing.MatchString(text) {
		return false, nil
	}
	return false, fmt.Errorf("restic cat config: %w (output: %s)", err, text)
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
	// restic exited 0 but emitted no parseable summary, so the snapshot
	// exists and we do not know its ID.
	//
	// This used to return a Snapshot with an empty ID "so the caller knows
	// the operation worked". The caller — backupScheduler.runOnce — persists
	// that ID as the app's last snapshot, and an empty one later reads back
	// as "restore the latest snapshot in the repository", which with several
	// apps sharing a repo is somebody else's data. An ID we never learned is
	// not a snapshot we can offer to restore, so this is an error.
	return nil, fmt.Errorf("restic backup: completed but emitted no parseable summary, so the snapshot ID is unknown (output: %s)", strings.TrimSpace(string(out)))
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
// can be a short ID (first 8 chars) or "latest" for the most recent
// snapshot CARRYING THE GIVEN TAGS.
//
// The tags are not optional decoration: they are the only thing that scopes
// a repository to one app. CasaNova tags every scheduled snapshot
// "app:<name>", several apps share one repository, and Snapshots() has
// always filtered by tag. Restore did not — it ran `restore <id> --target`
// with no filter at all, so "latest" meant the newest snapshot in the whole
// repository. Restoring app A could therefore unpack app B's data over A's
// directory and answer "restore complete". That asymmetry between the two
// functions is what gives the bug away as an oversight rather than a design.
//
// An empty ID is no longer quietly promoted to "latest". It used to be, and
// combined with BackupPaths persisting an empty ID after an unparseable
// summary, that promotion was the whole path from "we lost track of one
// snapshot" to "we restored the wrong app's data".
//
// validResticSnapshotID matches restic snapshot IDs (hex) or "latest".
var validResticSnapshotID = regexp.MustCompile(`^(?:latest|[a-f0-9]{8,64})$`)

func (r *ResticManager) Restore(ctx context.Context, snapshotID, target string, tags ...string) error {
	if err := available(); err != nil {
		return err
	}
	if snapshotID == "" {
		return errors.New("restic restore: no snapshot ID given (refusing to guess: the newest snapshot in a shared repository may belong to another app)")
	}
	// Validate snapshotID to prevent argument injection via crafted IDs.
	if !validResticSnapshotID.MatchString(snapshotID) {
		return fmt.Errorf("invalid snapshot ID: %q", snapshotID)
	}

	args := []string{"restore", snapshotID}
	if snapshotID == "latest" {
		// --tag is restic's documented filter for resolving "latest", and it
		// is accepted ONLY together with "latest": passing it alongside an
		// explicit ID is a fatal error on restic >= 0.17. Hence the split:
		// filter here, verify below.
		for _, t := range tags {
			args = append(args, "--tag", t)
		}
	} else if len(tags) > 0 {
		// An explicit ID bypasses any filter restic would apply, so the
		// scoping has to happen before the command runs: an ID belonging to
		// another app is exactly the confusion being fixed, and it is just as
		// destructive when a caller supplies it by hand.
		if err := r.assertSnapshotHasTags(ctx, snapshotID, tags); err != nil {
			return err
		}
	}
	args = append(args, "--target", target)

	cmd := r.command(ctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restic restore: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	logger.Info("restic restore complete",
		zap.String("snapshot", snapshotID),
		zap.Strings("tags", tags),
		zap.String("target", target))
	return nil
}

// assertSnapshotHasTags refuses a restore whose snapshot does not carry every
// one of the given tags.
//
// A lookup that fails is a refusal, not a pass: "I could not check whether
// this snapshot belongs to this app" must not read the same as "it does",
// which is the mistake Exists() made one function up.
func (r *ResticManager) assertSnapshotHasTags(ctx context.Context, snapshotID string, tags []string) error {
	snaps, err := r.Snapshots(ctx, tags...)
	if err != nil {
		return fmt.Errorf("restic restore: cannot verify that snapshot %s belongs to %s: %w", snapshotID, strings.Join(tags, ","), err)
	}
	for _, s := range snaps {
		if s.ID == snapshotID || s.ShortID == snapshotID || strings.HasPrefix(s.ID, snapshotID) {
			return nil
		}
	}
	return fmt.Errorf("restic restore: snapshot %s does not carry %s; refusing to restore another app's data over this target", snapshotID, strings.Join(tags, ","))
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
	cmd := exec.CommandContext(ctx, resticBinary, args...)
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
