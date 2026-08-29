package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/robfig/cron/v3"
)

// Schedule is one persisted backup plan — which paths, where to store
// them, how often, and what to keep. Schedules are keyed by AppName so
// the scheduler can find an app's plan at install/uninstall time.
type Schedule struct {
	// AppName is the compose app ID; also used as a Restic tag so
	// retention can be scoped per app without affecting others.
	AppName string `json:"app_name"`

	// Enabled toggles the schedule without deleting it. A disabled
	// schedule is skipped by the scheduler loop but still listed in
	// the UI so the user can re-enable with one click.
	Enabled bool `json:"enabled"`

	// Paths is the set of directories to back up. Typically this is
	// the app's data volume (e.g. /DATA/AppData/plex) but users can
	// add additional paths like /DATA/Media for bundled backups.
	Paths []string `json:"paths"`

	// Cron is a standard 5-field cron expression in the server's local
	// timezone. "0 3 * * *" is 3am daily, the recommended default for
	// home servers when nothing else is busy.
	Cron string `json:"cron"`

	// Repo is the destination repository (Restic URL + password).
	Repo ResticRepo `json:"repo"`

	// Retention enforces how many snapshots to keep after each backup
	// run. Leave fields at zero to keep everything forever (not
	// recommended — Restic repos grow unbounded without forget/prune).
	Retention RetentionPolicy `json:"retention"`

	// LastRun is updated by the scheduler after every attempt (success
	// or failure). Zero value means "never run".
	LastRun time.Time `json:"last_run,omitempty"`

	// LastError captures the most recent failure, empty on success.
	// Displayed in the UI as "last run failed: <reason>".
	LastError string `json:"last_error,omitempty"`

	// LastSnapshot holds the most recently created snapshot ID so the
	// UI can deep-link to "view the latest snapshot of this app".
	LastSnapshot string `json:"last_snapshot,omitempty"`
}

// Validate returns an error describing the first problem with the
// schedule. Called by the HTTP handlers before persistence so clients
// get actionable errors instead of a cryptic scheduler failure later.
func (s *Schedule) Validate() error {
	if s.AppName == "" {
		return errors.New("app_name is required")
	}
	if len(s.Paths) == 0 {
		return errors.New("at least one backup path is required")
	}
	// Every path must be absolute, non-empty, and free of "..".
	// Without these checks a caller could POST a relative path like
	// "../../etc" which restic would resolve against its own working
	// directory (typically /) — the backup would succeed but the
	// stored paths in the snapshot would be confusingly wrong, and
	// the subsequent restore handler's target-path guard would
	// trivially bypass by inheriting the same traversal.
	for i, p := range s.Paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("path[%d] is empty", i)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path[%d] %q must be absolute", i, p)
		}
		if strings.Contains(p, "..") {
			return fmt.Errorf("path[%d] %q must not contain '..'", i, p)
		}
	}
	if s.Cron == "" {
		return errors.New("cron expression is required")
	}
	if _, err := cron.ParseStandard(s.Cron); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	if s.Repo.URL == "" {
		return errors.New("repo URL is required")
	}
	if s.Repo.Password == "" {
		return errors.New("repo password is required")
	}
	return nil
}

// ScheduleStore persists backup schedules to a single JSON file.
// Thread-safe; the file is rewritten atomically on every Save.
type ScheduleStore struct {
	path string
	mu   sync.RWMutex
	// keyed by app name so per-app lookups are O(1)
	byApp map[string]Schedule
}

// NewScheduleStore constructs a store rooted at the given directory.
// The file is created on first Save — a missing file on read is treated
// as "no schedules yet".
func NewScheduleStore(dir string) (*ScheduleStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("schedule store mkdir: %w", err)
	}
	s := &ScheduleStore{
		path:  filepath.Join(dir, "schedules.json"),
		byApp: make(map[string]Schedule),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ScheduleStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read schedules: %w", err)
	}
	var list []Schedule
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("parse schedules: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sch := range list {
		s.byApp[sch.AppName] = sch
	}
	return nil
}

// saveLocked writes the full map to disk. Caller must hold s.mu.
//
// Wave 35-Q: previously this was os.WriteFile(tmp) + os.Rename — neither
// of which calls fsync(2). The schedules.json file holds plaintext
// Restic repository passwords (Schedule.Repo.Password is a plain
// string), and on btrfs/xfs/zfs the rename and the data can land in
// different transactions. A power loss between them yielded a
// renamed-but-empty file and every backup schedule disappeared at
// once. commonfile.WriteFileAtomic does the full write-tmp + fsync(file) +
// rename + fsync(parent dir) sequence.
func (s *ScheduleStore) saveLocked() error {
	list := make([]Schedule, 0, len(s.byApp))
	for _, sch := range s.byApp {
		list = append(list, sch)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return commonfile.WriteFileAtomic(s.path, raw, 0o600)
}

// Put inserts or replaces a schedule by app name.
func (s *ScheduleStore) Put(sch Schedule) error {
	if err := sch.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byApp[sch.AppName] = sch
	return s.saveLocked()
}

// Get returns a schedule by app name. Second return is false when
// no schedule is configured for the app.
func (s *ScheduleStore) Get(appName string) (Schedule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sch, ok := s.byApp[appName]
	return sch, ok
}

// Delete removes a schedule. Idempotent — safe to call when nothing
// is configured for the app.
func (s *ScheduleStore) Delete(appName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byApp[appName]; !ok {
		return nil
	}
	delete(s.byApp, appName)
	return s.saveLocked()
}

// All returns every schedule, ordered by app name for stable UI display.
// The previous implementation iterated the map directly, which in Go
// has deliberately randomised key order — so the schedule list flickered
// in the UI on every refresh and any UI-side diff-by-index highlighting
// (e.g. "this row was just updated") pointed at the wrong schedule.
func (s *ScheduleStore) All() []Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Schedule, 0, len(s.byApp))
	for _, sch := range s.byApp {
		out = append(out, sch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppName < out[j].AppName })
	return out
}

// UpdateLast records the outcome of a run attempt. Called by the
// scheduler after every backup; callers don't need to re-send the
// rest of the schedule fields.
func (s *ScheduleStore) UpdateLast(appName string, t time.Time, snapshotID, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.byApp[appName]
	if !ok {
		return fmt.Errorf("schedule for %q not found", appName)
	}
	sch.LastRun = t
	sch.LastSnapshot = snapshotID
	sch.LastError = errMsg
	s.byApp[appName] = sch
	return s.saveLocked()
}
