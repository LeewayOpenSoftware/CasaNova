// Package scheduled implements user-defined cron-like jobs for CasaNova.
// Users want to run things like:
//
//   - "restart Plex every Sunday at 4am"
//   - "dump Postgres every night inside the db container"
//   - "ping the watchtower URL every 5 minutes"
//
// The plan from the roadmap is:
//
//   - CRUD API for user-defined cron
//   - Run on host OR inside a specific Docker container
//   - Capture logs, keep run history, support manual trigger + cancel
//
// Tasks and their history persist as JSON on disk so an operator can
// inspect them outside the running service.
package scheduled

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/robfig/cron/v3"
)

// shellMetaRe is the **single source of truth** for "what is a dangerous
// character in a user-supplied shell command". It is enforced both at
// Validate() time (so a hostile task cannot be persisted at all) and a
// second time inside the runner before exec — defense in depth in case
// a future code path skips Validate.
//
// Wave 26: previously this regex only existed inside the runner, which
// meant a malicious admin / compromised account could *store* a task
// containing arbitrary shell injection sequences and the failure was
// only surfaced at fire time. Stored hostile state is a footgun even
// when it never executes — it shows up in backups, in audit dumps, and
// in any future "run task X" code path that forgets to re-validate.
//
// The character class blocks: command separators (;|&), subshells / cmd
// substitution (`...`, $(...)), redirects (<>), background (&),
// expansions ($, ~, *, ?, [], {}), comments (#), history (!), backslash
// escapes, and CR/LF (multi-line injection).
var shellMetaRe = regexp.MustCompile("[;|&`$()<>\\n\\r!~*?\\[\\]{}#\\\\]")

// containerNameRe matches the legal Docker container name/ID grammar.
// Same regex as the runner — kept in lockstep so create-time validation
// matches run-time validation byte-for-byte.
var containerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// containerUserRe matches `name`, `name:group`, `uid`, or `uid:gid`.
// Letters, digits, underscores, hyphens — same grammar Docker accepts
// for `--user`. The colon is optional and may appear at most once.
var containerUserRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*(:[a-zA-Z0-9_][a-zA-Z0-9_.-]*)?$`)

// Target describes where the command runs. Tasks targeting a container
// use `docker exec` under the hood, so the container must already exist
// and be running at fire time.
type Target struct {
	// Host is set to true for commands that run directly on the host
	// with /bin/sh -c. Mutually exclusive with Container.
	Host bool `json:"host"`

	// Container is the name or ID of a Docker container. When non-empty,
	// the runner shell-execs the command inside that container instead
	// of on the host.
	Container string `json:"container,omitempty"`

	// WorkingDir overrides the CWD for the command. Optional; defaults
	// to / for host commands and the container's WORKDIR for exec.
	WorkingDir string `json:"working_dir,omitempty"`

	// User is the unix user (or `user:group` form) the in-container
	// command should run as. Wave 34-X residual: previously every
	// container task ran as the container's default user — almost
	// always root inside the image — even when the operator just
	// wanted to invoke a maintenance script in the application's own
	// service account. Forcing a non-root user is the only way to
	// keep a hostile cron task from compromising the host via a
	// container that mounts /var/run/docker.sock.
	//
	// Empty string keeps Docker's default behavior. Validate() rejects
	// anything that doesn't match a strict user[:group] regex.
	User string `json:"user,omitempty"`
}

// Task is a user-configured scheduled job.
type Task struct {
	// ID is assigned at Create time — clients don't pick it. Keeps the
	// scheduler's internal map simple and prevents collisions.
	ID string `json:"id"`

	// Name is the human label shown in the UI.
	Name string `json:"name"`

	// Description is an optional longer explanation.
	Description string `json:"description,omitempty"`

	// Cron is a 5-field cron expression evaluated in the server's
	// local timezone.
	Cron string `json:"cron"`

	// Command is the shell command to execute. It's passed to
	// /bin/sh -c exactly as-is — users are responsible for quoting
	// and escaping their own arguments.
	Command string `json:"command"`

	// Target decides whether this runs on the host or inside a container.
	Target Target `json:"target"`

	// Enabled toggles the task without deleting it.
	Enabled bool `json:"enabled"`

	// Timeout bounds each run. Tasks that overshoot are killed and the
	// run is marked as TimedOut. Zero means "no timeout" (not recommended).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Wave 26: ownership / audit fields stamped by the store on Put. The
	// scheduler service runs as root, so every task is effectively a root
	// shell. Recording who created and last touched each task gives an
	// auditor a place to start when a hostile task shows up — the alone-
	// in-the-room admin model is no longer good enough now that an
	// attacker who steals an admin token can quietly install a persistent
	// backdoor via the task API.
	CreatedBy int `json:"created_by,omitempty"`
	UpdatedBy int `json:"updated_by,omitempty"`

	// Created + Updated are stamped by the store. Clients cannot set them.
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Validate catches obvious misconfigurations up-front so clients see
// actionable errors instead of scheduler failures later.
func (t *Task) Validate() error {
	if t.Name == "" {
		return errors.New("name is required")
	}
	if t.Cron == "" {
		return errors.New("cron expression is required")
	}
	if _, err := cron.ParseStandard(t.Cron); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	if t.Command == "" {
		return errors.New("command is required")
	}
	// Wave 26: shell-metachar gate at storage time, not just at run
	// time. Stored hostile state shows up in backups, in audit dumps,
	// and in any future "run task" path that forgets to re-validate.
	if shellMetaRe.MatchString(t.Command) {
		return errors.New("command contains disallowed shell metacharacters")
	}
	if t.Target.Host && t.Target.Container != "" {
		return errors.New("target cannot be both host and container")
	}
	if !t.Target.Host && t.Target.Container == "" {
		return errors.New("target must be host or container")
	}
	if t.Target.Container != "" && !containerNameRe.MatchString(t.Target.Container) {
		return errors.New("invalid container name")
	}
	if t.Target.User != "" {
		if t.Target.Host {
			return errors.New("user is only valid for container targets")
		}
		if !containerUserRe.MatchString(t.Target.User) {
			return errors.New("invalid container user")
		}
	}
	if t.Target.WorkingDir != "" {
		// Reject anything that contains shell metachars *or* a traversal
		// sequence. WorkingDir is passed as argv to docker exec / set as
		// cmd.Dir on host (neither is interpreted by a shell), so the
		// metachar check is belt-and-suspenders against future changes.
		if shellMetaRe.MatchString(t.Target.WorkingDir) {
			return errors.New("working_dir contains disallowed characters")
		}
		clean := filepath.Clean(t.Target.WorkingDir)
		if strings.Contains(clean, "..") {
			return errors.New("working_dir must not contain traversal sequences")
		}
	}
	if t.TimeoutSeconds < 0 {
		return errors.New("timeout must be >= 0")
	}
	return nil
}

// RunStatus enumerates outcomes of a single task execution.
type RunStatus string

const (
	StatusSuccess  RunStatus = "success"   // exited 0 inside timeout
	StatusFailed   RunStatus = "failed"    // exited non-zero
	StatusTimedOut RunStatus = "timed_out" // hit TimeoutSeconds
	StatusError    RunStatus = "error"     // runner itself failed to start/spawn
)

// Run is one historical execution of a task. The history is capped
// per task (see TaskStore.MaxHistory) so a chatty cron doesn't grow
// the store file without bound.
type Run struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    RunStatus `json:"status"`
	ExitCode  int       `json:"exit_code"`
	// Output is stdout + stderr combined, truncated to a safe cap so
	// a runaway task can't fill the store file.
	Output string `json:"output"`
}

// TaskStore persists tasks + per-task run history to disk.
// Thread-safe via mu.
type TaskStore struct {
	path       string
	maxHistory int
	mu         sync.RWMutex

	tasks   map[string]*Task
	history map[string][]Run // newest first, capped at maxHistory
}

// NewTaskStore opens or creates a store under dir.
func NewTaskStore(dir string) (*TaskStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("task store mkdir: %w", err)
	}
	ts := &TaskStore{
		path:       filepath.Join(dir, "tasks.json"),
		maxHistory: 50,
		tasks:      make(map[string]*Task),
		history:    make(map[string][]Run),
	}
	if err := ts.load(); err != nil {
		return nil, err
	}
	return ts, nil
}

type persistedStore struct {
	Tasks   map[string]*Task `json:"tasks"`
	History map[string][]Run `json:"history"`
}

func (ts *TaskStore) load() error {
	raw, err := os.ReadFile(ts.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read tasks: %w", err)
	}
	var p persistedStore
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse tasks: %w", err)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if p.Tasks != nil {
		ts.tasks = p.Tasks
	}
	if p.History != nil {
		ts.history = p.History
	}
	return nil
}

// saveMu serializes file-level writes so concurrent callers from different
// TaskStore instances (or future code paths) cannot interleave writes.
var saveMu sync.Mutex

// saveLocked serialises the task store to disk with crash safety.
//
// Wave 35-R: previously this was write-tmp + fsync(file) + rename,
// which leaves a window on btrfs/xfs/zfs where the rename commits to
// the parent directory's metadata log in a different transaction than
// the file's data. A power loss between them yielded a renamed-but-
// empty tasks.json and every scheduled task disappeared at once — the
// scheduler came back with zero tasks and the user's cron history was
// lost. commonfile.WriteFileAtomic does the full write-tmp + fsync(file)
// + rename + fsync(parent dir) sequence so the rename is durable before
// we return success, matching what we already do for the backup
// schedule store.
func (ts *TaskStore) saveLocked() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	p := persistedStore{
		Tasks:   ts.tasks,
		History: ts.history,
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return commonfile.WriteFileAtomic(ts.path, raw, 0o600)
}

// Put inserts or updates a task. For inserts (ID empty) a new ID is
// generated and the resulting task (with ID populated) is returned.
//
// Wave 26: actorUserID, when non-zero, is recorded as the task's
// UpdatedBy. For brand-new tasks (no prior ID, or no existing entry
// for that ID) it is also recorded as CreatedBy. Pass 0 only for
// trusted internal callers (loopback / tests) where the action is not
// attributable to any user.
func (ts *TaskStore) Put(t *Task, actorUserID int) (*Task, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := time.Now()
	existing, existed := ts.tasks[t.ID]
	if t.ID == "" {
		t.ID = newTaskID()
		t.Created = now
		t.CreatedBy = actorUserID
	} else if !existed {
		t.Created = now
		t.CreatedBy = actorUserID
	} else {
		// Edits never reset the original creator — preserve it even if
		// the caller forgot to round-trip the field.
		t.Created = existing.Created
		t.CreatedBy = existing.CreatedBy
	}
	t.Updated = now
	if actorUserID != 0 {
		t.UpdatedBy = actorUserID
	}
	ts.tasks[t.ID] = t
	if err := ts.saveLocked(); err != nil {
		return nil, err
	}
	return t, nil
}

// Get returns a task by ID.
func (ts *TaskStore) Get(id string) (*Task, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.tasks[id]
	return t, ok
}

// Delete removes a task and wipes its run history.
func (ts *TaskStore) Delete(id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.tasks, id)
	delete(ts.history, id)
	return ts.saveLocked()
}

// All returns every task in a stable, deterministic order (by name,
// then by ID as a tiebreaker for tasks with duplicate names).
//
// Go map iteration is intentionally randomised, so the previous
// implementation returned tasks in a different order on every call.
// The task list in the UI was re-rendered from this slice on each
// poll, which made rows flicker between positions and broke any diff-
// by-index highlighting ("this row was just updated" arrows ended up
// pointing at a different task). Sorting here fixes the UI stability
// without asking callers to re-sort.
func (ts *TaskStore) All() []*Task {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]*Task, 0, len(ts.tasks))
	for _, t := range ts.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AppendRun records a run outcome. The history for the task is capped
// at maxHistory entries; oldest are dropped first.
//
// Wave 33: refuse to append for a task that no longer exists in the
// store. Without this guard a long-running job whose task was deleted
// mid-run finishes, calls AppendRun, and writes a Run record into
// `history[taskID]` for an ID that has nothing on the other side. The
// next saveLocked() persists those orphan history entries to disk and
// they linger forever — there is no UI to view them and no Delete
// path to clear them, so they accumulate every time an admin removes
// a misbehaving cron task while it's executing. Returning a typed
// not-found error here lets the caller log it without aborting the
// rest of its bookkeeping.
func (ts *TaskStore) AppendRun(r Run) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, ok := ts.tasks[r.TaskID]; !ok {
		return ErrTaskNotFound
	}
	runs := ts.history[r.TaskID]
	runs = append([]Run{r}, runs...)
	if len(runs) > ts.maxHistory {
		runs = runs[:ts.maxHistory]
	}
	ts.history[r.TaskID] = runs
	return ts.saveLocked()
}

// ErrTaskNotFound is returned by store mutators when the referenced
// task ID is not present. Callers (notably AppendRun's logging in the
// scheduledManager) use this to suppress noisy errors during the
// normal "task removed during a run" race.
var ErrTaskNotFound = errTaskNotFound{}

type errTaskNotFound struct{}

func (errTaskNotFound) Error() string { return "scheduled: task not found" }

// History returns the run records for a task, newest first.
func (ts *TaskStore) History(taskID string) []Run {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	runs := ts.history[taskID]
	out := make([]Run, len(runs))
	copy(out, runs)
	return out
}

// newTaskID generates a short, sortable identifier for a new task.
// We use the current nanosecond timestamp in base36 — short enough
// to display, unique enough for a single-host service.
func newTaskID() string {
	return fmt.Sprintf("task_%x", time.Now().UnixNano())
}
