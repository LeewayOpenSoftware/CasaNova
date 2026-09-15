package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRestic installs a fake `restic` on disk and points the package at it.
//
// Why a stub and not the real binary: the behaviour under test is how THIS
// package reads restic's answers — an exit code and a line of text — and a
// test that needed a real repository would be skipped on every box without
// restic installed, which is the silent pass this whole file is about. The
// stub answers exactly what restic answers, and it never touches anything
// outside the test's own temporary directory: no /var/lib/casaos, no real
// repository, nothing mounted or unmounted.
//
// argvLog receives one line per invocation so the arguments themselves can be
// asserted — the cross-app restore bug was entirely a missing argument.
func stubRestic(t *testing.T, script string) (argvLog string) {
	t.Helper()

	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	path := filepath.Join(dir, "restic-stub")

	// argvLog is quoted: t.TempDir() bakes the (sub)test name into the path,
	// and a name containing shell metacharacters would otherwise break the
	// stub and read exactly like a failure of the code under test.
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argvLog + "'\n" + script
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	old := resticBinary
	resticBinary = path
	t.Cleanup(func() { resticBinary = old })
	return argvLog
}

func readArgv(t *testing.T, argvLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("stub was never invoked: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func testRepo() *ResticManager {
	return NewRestic(ResticRepo{URL: "/mnt/backup-disk/repo", Password: "pw"})
}

// ---------------------------------------------------------------- BACKUP-1

// THE DEFECT: an unreachable repository must not be reported as "not created
// yet", because the only caller answers that by creating one.
//
// backupScheduler.runOnce does `if !Exists { Init }`. With the repository on a
// disk that is not mounted, the old `return cmd.Run() == nil` said false, Init
// created a fresh empty repository at the bare mountpoint on the rootfs, and
// every nightly run after that "succeeded" into it while the real history sat
// unreachable on the unmounted disk.
func TestExistsReportsUnreachableAsAnErrorNotAsAbsence(t *testing.T) {
	stubRestic(t, `echo "Fatal: unable to open repository at /mnt/backup-disk/repo: stat /mnt/backup-disk/repo: no such file or directory" >&2
exit 1`)

	exists, err := testRepo().Exists(context.Background())
	if err == nil {
		t.Fatal("an unreachable repository reported no error: the scheduler would now Init a new repo over the unmounted mountpoint")
	}
	if exists {
		t.Fatal("exists = true for an unreachable repository")
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("the error does not carry restic's own reason: %v", err)
	}
}

// A wrong password is not absence either — it is the same repository, still
// there, that we simply cannot read.
func TestExistsReportsWrongPasswordAsAnError(t *testing.T) {
	stubRestic(t, `echo "Fatal: wrong password or no key found" >&2
exit 12`)

	exists, err := testRepo().Exists(context.Background())
	if err == nil || exists {
		t.Fatalf("wrong password reported as absence (exists=%v, err=%v)", exists, err)
	}
}

// The other half of the same coin: a genuinely missing repository must still
// come back as a clean "no", or the scheduler could never create the first
// one. Both of restic's ways of saying it are recognised.
func TestExistsReportsGenuineAbsenceCleanly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"exit code 10 (restic >= 0.17)", `echo "Fatal: repository does not exist" >&2
exit 10`},
		{"message only (older restic)", `echo "Fatal: unable to open config file: no repository config file found" >&2
exit 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRestic(t, tc.script)
			exists, err := testRepo().Exists(context.Background())
			if err != nil {
				t.Fatalf("a missing repository reported an error, so the first backup could never initialise one: %v", err)
			}
			if exists {
				t.Fatal("exists = true for a missing repository")
			}
		})
	}
}

func TestExistsReportsAPresentRepository(t *testing.T) {
	stubRestic(t, `echo '{"version":2,"id":"deadbeef"}'
exit 0`)

	exists, err := testRepo().Exists(context.Background())
	if err != nil || !exists {
		t.Fatalf("a healthy repository was not recognised (exists=%v, err=%v)", exists, err)
	}
}

// ---------------------------------------------------------------- BACKUP-2

// THE DEFECT, first half: a backup whose summary could not be parsed must not
// hand back a Snapshot with an empty ID. The scheduler persists that ID, and
// an empty ID later reads back as "restore the newest snapshot in the
// repository" — which in a repository shared by several apps is somebody
// else's data.
func TestBackupWithoutAParseableSummaryIsAnErrorNotAnIDLessSnapshot(t *testing.T) {
	stubRestic(t, `echo '{"message_type":"status","percent_done":1}'
exit 0`)

	snap, err := testRepo().BackupPaths(context.Background(), []string{"/data/app"}, "app:plex")
	if err == nil {
		t.Fatalf("a backup with no parseable summary was reported as success: %#v", snap)
	}
	if snap != nil {
		t.Fatalf("an ID-less snapshot was handed to the caller to persist: %#v", snap)
	}
	if !strings.Contains(err.Error(), "snapshot ID is unknown") {
		t.Fatalf("the error does not say what went wrong: %v", err)
	}
}

// The success path must keep working unchanged: a parseable summary still
// yields the snapshot with its ID.
func TestBackupStillReturnsTheSnapshotOnASummary(t *testing.T) {
	stubRestic(t, `echo '{"message_type":"summary","snapshot_id":"abcdef0123456789"}'
exit 0`)

	snap, err := testRepo().BackupPaths(context.Background(), []string{"/data/app"}, "app:plex")
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID != "abcdef0123456789" || snap.ShortID != "abcdef01" {
		t.Fatalf("snapshot = %#v", snap)
	}
}

// THE DEFECT, second half: "latest" must be resolved WITHIN this app's tags.
//
// Snapshots() has always passed --tag; Restore did not, so "latest" meant the
// newest snapshot in the whole repository. That asymmetry is the bug. This is
// the tripwire: it goes red the moment --tag stops reaching the command line.
func TestRestoreLatestIsScopedToTheAppsTag(t *testing.T) {
	argv := stubRestic(t, `exit 0`)

	if err := testRepo().Restore(context.Background(), "latest", "/data/plex", "app:plex"); err != nil {
		t.Fatal(err)
	}

	line := readArgv(t, argv)[0]
	if !strings.Contains(line, "--tag app:plex") {
		t.Fatalf("restore ran unscoped, so \"latest\" could be another app's snapshot: restic %s", line)
	}
	if !strings.Contains(line, "--target /data/plex") {
		t.Fatalf("restore lost its target: restic %s", line)
	}
}

// An empty ID is no longer silently promoted to "latest".
func TestRestoreRefusesAnEmptySnapshotID(t *testing.T) {
	argv := stubRestic(t, `exit 0`)

	err := testRepo().Restore(context.Background(), "", "/data/plex", "app:plex")
	if err == nil {
		t.Fatal("an empty snapshot ID was accepted and promoted to \"latest\"")
	}
	if _, statErr := os.Stat(argv); statErr == nil {
		t.Fatal("restic was invoked despite the caller not naming a snapshot")
	}
}

// An explicit ID bypasses restic's own filter, so the ownership check has to
// happen before the command runs. A snapshot belonging to another app must be
// refused, not unpacked over this app's directory.
func TestRestoreRefusesASnapshotBelongingToAnotherApp(t *testing.T) {
	argv := stubRestic(t, `case "$*" in
  snapshots*) echo '[{"id":"1111111111111111","short_id":"11111111","tags":["app:plex"]}]' ;;
  *) exit 0 ;;
esac`)

	// 2222… is sonarr's snapshot; the caller asked for it while restoring plex.
	err := testRepo().Restore(context.Background(), "2222222222222222", "/data/plex", "app:plex")
	if err == nil {
		t.Fatal("another app's snapshot was restored over this app's target")
	}
	if !strings.Contains(err.Error(), "does not carry") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, line := range readArgv(t, argv) {
		if strings.HasPrefix(line, "restore ") {
			t.Fatalf("restic restore ran anyway: restic %s", line)
		}
	}
}

// The app's own snapshot still restores.
func TestRestoreAllowsTheAppsOwnSnapshot(t *testing.T) {
	argv := stubRestic(t, `case "$*" in
  snapshots*) echo '[{"id":"1111111111111111","short_id":"11111111","tags":["app:plex"]}]' ;;
  *) exit 0 ;;
esac`)

	if err := testRepo().Restore(context.Background(), "1111111111111111", "/data/plex", "app:plex"); err != nil {
		t.Fatalf("the app's own snapshot was refused: %v", err)
	}

	var ran bool
	for _, line := range readArgv(t, argv) {
		if strings.HasPrefix(line, "restore ") {
			ran = true
			// --tag alongside an explicit ID is fatal on restic >= 0.17.
			if strings.Contains(line, "--tag") {
				t.Fatalf("--tag passed with an explicit ID: restic %s", line)
			}
		}
	}
	if !ran {
		t.Fatal("restic restore never ran")
	}
}

// A lookup that fails must refuse, not wave the restore through: "I could not
// check whether this snapshot is yours" is not "it is".
func TestRestoreRefusesWhenOwnershipCannotBeChecked(t *testing.T) {
	stubRestic(t, `case "$*" in
  snapshots*) echo "Fatal: unable to open repository" >&2; exit 1 ;;
  *) exit 0 ;;
esac`)

	err := testRepo().Restore(context.Background(), "1111111111111111", "/data/plex", "app:plex")
	if err == nil {
		t.Fatal("restore proceeded although ownership could not be verified")
	}
	if !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("unexpected error: %v", err)
	}
}
