package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/backup"
)

// jamStore makes the schedule file unwritable in a way that does not depend on
// the uid running the tests: schedules.json is replaced by a DIRECTORY, so the
// atomic write's rename fails for root and for everybody else alike.
//
// Nothing outside the test's own temporary directory is touched — no real
// schedule file, and /var/lib/casaos is never opened, not even to read.
func jamStore(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "schedules.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func storeIn(t *testing.T, dir string, sch backup.Schedule) *backup.ScheduleStore {
	t.Helper()
	store, err := backup.NewScheduleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sch); err != nil {
		t.Fatal(err)
	}
	return store
}

func plexSchedule() backup.Schedule {
	return backup.Schedule{
		AppName: "plex",
		Enabled: true,
		Paths:   []string{"/DATA/AppData/plex"},
		Cron:    "0 3 * * *",
		Repo:    backup.ResticRepo{URL: "/srv/repo", Password: "pw"},
	}
}

// THE DEFECT: RunNow answered with the PREVIOUS run's result.
//
// runOnce returns early when a backup for the same app is already in flight.
// RunNow then re-read the schedule and reported whatever LastError was sitting
// there — which described the run before. With the previous run successful, a
// manual "back up now" during a running backup answered 200 "backup complete"
// for a backup that never started. The screen could not tell "it ran and came
// out like this" from "it did not run at all".
//
// This is the tripwire for that: it goes red if the outcome is ever inferred
// from persisted state again.
func TestRunNowDoesNotReportASkippedRunAsTheLastRunsSuccess(t *testing.T) {
	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	// The previous run succeeded, so LastError is empty — the state RunNow
	// used to read back and report as this run's outcome.
	if err := store.UpdateLast("plex", time.Now(), "abcdef01", ""); err != nil {
		t.Fatal(err)
	}

	sched := &backupScheduler{store: store}
	// A backup for plex is already in flight.
	sched.running.Store("plex", true)

	err := sched.RunNow(context.Background(), "plex")
	if err == nil {
		t.Fatal("a run that never started was reported as a completed backup; the handler would answer 200 \"backup complete\"")
	}
	if !errors.Is(err, ErrBackupAlreadyRunning) {
		t.Fatalf("want ErrBackupAlreadyRunning, got %v", err)
	}
}

// The mirror image: with the previous run FAILED, a skipped run used to
// surface that stale failure as though this run had just failed.
func TestRunNowDoesNotReportAStalePreviousFailure(t *testing.T) {
	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	if err := store.UpdateLast("plex", time.Now(), "", "restic backup: connection refused"); err != nil {
		t.Fatal(err)
	}

	sched := &backupScheduler{store: store}
	sched.running.Store("plex", true)

	err := sched.RunNow(context.Background(), "plex")
	if err == nil {
		t.Fatal("expected the skip to be reported")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the previous run's failure was presented as this run's result: %v", err)
	}
	if !errors.Is(err, ErrBackupAlreadyRunning) {
		t.Fatalf("want ErrBackupAlreadyRunning, got %v", err)
	}
}

// THE SECOND HALF: with schedules.json unwritable, a FAILED backup used to
// report success.
//
// recordFailure wrote the failure with `_ = s.store.UpdateLast(...)`. When that
// write failed, the failure never reached the store, and RunNow — reading the
// outcome back out of the store — found the previous (empty) LastError and
// returned nil. The backup failed, the record failed, and the user was told it
// worked.
func TestRunNowReportsAFailureEvenWhenItCannotBeRecorded(t *testing.T) {
	stubResticOnPath(t, `case "$*" in
  "cat config") echo "Fatal: unable to open repository at /srv/repo: no such file or directory" >&2; exit 1 ;;
  *) exit 0 ;;
esac`)

	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	jamStore(t, dir)

	sched := &backupScheduler{store: store}

	err := sched.RunNow(context.Background(), "plex")
	if err == nil {
		t.Fatal("a backup that failed AND could not be recorded was reported as a success")
	}
	if !strings.Contains(err.Error(), "/srv/repo") {
		t.Fatalf("the failure lost the reason it failed: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be recorded") {
		t.Fatalf("the failure does not mention that it could not be persisted, so nobody would know the stored state is stale: %v", err)
	}
}

// A backup that succeeds but whose result cannot be written down is not a
// clean success either: every screen from here on shows the previous run.
func TestRunNowReportsASuccessThatCouldNotBeRecorded(t *testing.T) {
	stubResticOnPath(t, `case "$*" in
  "cat config") echo '{"version":2}' ;;
  backup*) echo '{"message_type":"summary","snapshot_id":"abcdef0123456789"}' ;;
  *) exit 0 ;;
esac`)

	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	jamStore(t, dir)

	sched := &backupScheduler{store: store}

	err := sched.RunNow(context.Background(), "plex")
	if err == nil {
		t.Fatal("the result could not be recorded and the caller was told nothing")
	}
	if !strings.Contains(err.Error(), "abcdef01") {
		t.Fatalf("the error does not name the snapshot that WAS created, which is what makes it recoverable: %v", err)
	}
}

// Over-correction guard: a plain successful run must still be a plain success,
// or every one of the above could be satisfied by returning an error always.
func TestRunNowStillReportsAnOrdinarySuccess(t *testing.T) {
	stubResticOnPath(t, `case "$*" in
  "cat config") echo '{"version":2}' ;;
  backup*) echo '{"message_type":"summary","snapshot_id":"abcdef0123456789"}' ;;
  *) exit 0 ;;
esac`)

	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	sched := &backupScheduler{store: store}

	if err := sched.RunNow(context.Background(), "plex"); err != nil {
		t.Fatalf("a healthy backup was reported as a failure: %v", err)
	}

	after, _ := store.Get("plex")
	if after.LastError != "" || after.LastSnapshot != "abcdef01" {
		t.Fatalf("the run was not recorded: %#v", after)
	}
}

// And a genuine failure must still be a failure carrying its own reason, not
// ErrBackupAlreadyRunning and not a stale message.
func TestRunNowStillReportsAGenuineFailure(t *testing.T) {
	stubResticOnPath(t, `case "$*" in
  "cat config") echo "Fatal: repository does not exist" >&2; exit 10 ;;
  init*) echo "Fatal: create repository at /srv/repo failed: read-only file system" >&2; exit 1 ;;
  *) exit 0 ;;
esac`)

	dir := t.TempDir()
	store := storeIn(t, dir, plexSchedule())
	sched := &backupScheduler{store: store}

	err := sched.RunNow(context.Background(), "plex")
	if err == nil {
		t.Fatal("a failed backup was reported as a success")
	}
	if errors.Is(err, ErrBackupAlreadyRunning) {
		t.Fatalf("a real failure was reported as a skip: %v", err)
	}
	if !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("the failure lost restic's own reason: %v", err)
	}

	after, _ := store.Get("plex")
	if after.LastError == "" {
		t.Fatal("the failure was not recorded for the UI to show")
	}
}
