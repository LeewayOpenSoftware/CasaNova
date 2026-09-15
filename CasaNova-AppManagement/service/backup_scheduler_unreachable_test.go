package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/backup"
)

// stubResticOnPath puts a fake `restic` first on PATH and logs every
// invocation, so a test can assert which subcommands the scheduler ran.
//
// Everything lives in the test's own temporary directory: no real repository
// is opened, nothing is mounted or unmounted, and /var/lib/casaos is never
// touched. The scheduler cannot tell the difference, which is the point — the
// defect is about how it reads restic's answers, not about restic.
func stubResticOnPath(t *testing.T, script string) (argvLog string) {
	t.Helper()

	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argvLog + "'\n" + script
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog
}

func schedulerWithStore(t *testing.T, sch backup.Schedule) (*backupScheduler, *backup.ScheduleStore) {
	t.Helper()

	store, err := backup.NewScheduleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sch); err != nil {
		t.Fatal(err)
	}
	return &backupScheduler{store: store, running: sync.Map{}}, store
}

// THE CONSEQUENCE, at the place where it actually costs the user data.
//
// The repository lives on a disk that is not mounted, so `restic cat config`
// fails to open it. The old Exists() turned that into a plain "false" and
// runOnce answered it by initialising a brand new, empty repository at the
// bare mountpoint on the rootfs. Every nightly run then succeeded into the
// wrong disk, ListSnapshots showed nothing, and retention pruned the phantom
// while the real backups sat unreachable.
//
// The scheduler must now stop and say which repository it could not reach.
// This is the tripwire: it goes red the moment "could not tell" is allowed to
// mean "not there yet" again.
func TestRunOnceRefusesToInitialiseAnUnreachableRepository(t *testing.T) {
	argv := stubResticOnPath(t, `case "$*" in
  "cat config") echo "Fatal: unable to open repository at /mnt/usb/repo: stat /mnt/usb/repo: no such file or directory" >&2; exit 1 ;;
  *) exit 0 ;;
esac`)

	sched, store := schedulerWithStore(t, backup.Schedule{
		AppName: "plex",
		Enabled: true,
		Paths:   []string{"/DATA/AppData/plex"},
		Cron:    "0 3 * * *",
		Repo:    backup.ResticRepo{URL: "/mnt/usb/repo", Password: "pw"},
	})
	sch, _ := store.Get("plex")

	sched.runOnce(context.Background(), sch)

	for _, line := range readLines(t, argv) {
		if strings.HasPrefix(line, "init") {
			t.Fatal("the scheduler created a NEW repository over an unmounted mountpoint; the nightly backups would go to the wrong disk from here on")
		}
		if strings.HasPrefix(line, "backup") {
			t.Fatal("the scheduler backed up into a repository it could not even open")
		}
	}

	after, _ := store.Get("plex")
	if after.LastError == "" {
		t.Fatal("an unreachable repository was recorded as a successful run")
	}
	if !strings.Contains(after.LastError, "/mnt/usb/repo") {
		t.Fatalf("the recorded failure does not name the repository that could not be reached: %q", after.LastError)
	}
}

// The other direction has to keep working, or the very first backup of a new
// app could never create its repository: a repository restic positively
// reports as missing is still initialised.
func TestRunOnceStillInitialisesAGenuinelyMissingRepository(t *testing.T) {
	argv := stubResticOnPath(t, `case "$*" in
  "cat config") echo "Fatal: repository does not exist" >&2; exit 10 ;;
  backup*) echo '{"message_type":"summary","snapshot_id":"abcdef0123456789"}' ;;
  *) exit 0 ;;
esac`)

	sched, store := schedulerWithStore(t, backup.Schedule{
		AppName: "plex",
		Enabled: true,
		Paths:   []string{"/DATA/AppData/plex"},
		Cron:    "0 3 * * *",
		Repo:    backup.ResticRepo{URL: "/srv/repo", Password: "pw"},
	})
	sch, _ := store.Get("plex")

	sched.runOnce(context.Background(), sch)

	var inited bool
	for _, line := range readLines(t, argv) {
		if strings.HasPrefix(line, "init") {
			inited = true
		}
	}
	if !inited {
		t.Fatal("a genuinely missing repository was never initialised, so the app's first backup could never run")
	}

	after, _ := store.Get("plex")
	if after.LastError != "" {
		t.Fatalf("a healthy run recorded an error: %q", after.LastError)
	}
	if after.LastSnapshot != "abcdef01" {
		t.Fatalf("last snapshot = %q, want the parsed short ID", after.LastSnapshot)
	}
}

func readLines(t *testing.T, argvLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}
