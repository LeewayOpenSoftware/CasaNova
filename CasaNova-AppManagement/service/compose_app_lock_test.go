package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bluele/gcache"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/applock"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"gopkg.in/yaml.v3"
)

// lockTestServices is a minimal Services implementation. The compose-app write
// gate only ever reaches AppLock() and AppStoreManagement(); wiring the real
// NewService() would demand a gateway runtime socket that no test host has.
type lockTestServices struct {
	appLock  *applock.Manager
	appStore *AppStoreManagement
}

func (s *lockTestServices) AppStoreManagement() *AppStoreManagement { return s.appStore }
func (s *lockTestServices) AppLock() *applock.Manager               { return s.appLock }
func (s *lockTestServices) Compose() *ComposeService                { return nil }
func (s *lockTestServices) Docker() DockerService                   { return nil }
func (s *lockTestServices) Gateway() external.ManagementService     { return nil }
func (s *lockTestServices) Notify() external.NotifyService          { return nil }

func (s *lockTestServices) MessageBus() *message_bus.ClientWithResponses {
	client, _ := message_bus.NewClientWithResponses(
		external.MessageBusSocketServer,
		message_bus.WithHTTPClient(external.SocketHTTPClient()),
	)
	return client
}

// newLockTestApp writes a real compose file into a temp dir and returns the
// installed app bound to it, plus the YAML a writer would try to apply.
func newLockTestApp(t *testing.T) (app *ComposeApp, composeFile string, newComposeYAML []byte) {
	t.Helper()
	logger.LogInitConsoleOnly()

	previous := MyService
	MyService = &lockTestServices{
		appLock: applock.NewManager(),
		// Same upgradable cache the real constructor builds — FinishUpgrade
		// evicts from it, and a nil cache would panic instead of clearing.
		appStore: &AppStoreManagement{
			isAppUpgradable: gcache.New(100).LRU().Expiration(1 * time.Hour).Build(),
		},
	}
	t.Cleanup(func() { MyService = previous })

	app, err := NewComposeAppFromYAML([]byte(common.SampleComposeAppYAML), true, false)
	if err != nil {
		t.Fatalf("load sample compose app: %v", err)
	}

	newComposeYAML, err = yaml.Marshal(app)
	if err != nil {
		t.Fatalf("marshal compose app: %v", err)
	}

	composeFile = filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(composeFile, newComposeYAML, 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	app.ComposeFiles = []string{composeFile}

	return app, composeFile, newComposeYAML
}

// assertNoBackupLeftovers fails if any rollback snapshot survived the
// operation. An orphan here means the app directory slowly fills with stale
// copies of old configs, and — worse — that some failure path returned without
// running its cleanup.
func assertNoBackupLeftovers(t *testing.T, composeFile string) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(composeFile))
	if err != nil {
		t.Fatalf("read compose dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".bak") {
			t.Fatalf("rollback snapshot %q was left behind", entry.Name())
		}
	}
}

// TestPullAndApplyRejectsConcurrentWriters is the core B7 regression: the app
// lock must cover the WHOLE of PullAndApply, not just some caller's synchronous
// prologue. A writer holding the lock here is exactly the state a real in-flight
// PullAndApply leaves the manager in for up to 30 minutes, so every other writer
// (a second manual update, a settings apply, the nightly auto-update sweep) must
// bounce off before touching the compose file or its snapshot.
func TestPullAndApplyRejectsConcurrentWriters(t *testing.T) {
	app, composeFile, newComposeYAML := newLockTestApp(t)

	original, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}

	release, err := MyService.AppLock().Acquire(app.Name)
	if err != nil {
		t.Fatalf("acquire app lock: %v", err)
	}

	const writers = 8
	results := make(chan error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func() {
			<-start
			results <- app.PullAndApply(context.Background(), newComposeYAML)
		}()
	}
	close(start)

	for i := 0; i < writers; i++ {
		err := <-results
		if !errors.Is(err, ErrComposeAppBusy) {
			t.Errorf("concurrent writer %d entered PullAndApply instead of being rejected (err=%v) - the compose file has more than one writer", i, err)
		}
	}

	release()

	current, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file after rejection: %v", err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("rejected writers mutated the compose file")
	}
	assertNoBackupLeftovers(t, composeFile)

	// The rejected writers must not have consumed the holder's lock either:
	// once the real writer releases, the app is usable again.
	next, err := MyService.AppLock().Acquire(app.Name)
	if err != nil {
		t.Fatalf("app stayed locked after the holder released - a rejected writer released a lock it never held: %v", err)
	}
	next()
}

// TestPullAndApplyHoldsTheLockUntilTheWorkIsDone is the test the other lock
// tests are NOT: they hold the lock from the test body and watch other writers
// bounce, which proves only that Acquire is called. The defect B7 fixed is a
// lock taken and released in the prologue, and every "second writer bounces"
// assertion stays green against it.
//
// This one measures TENURE. It suspends PullAndApply at the instant the new
// compose file has landed on disk — the state a real update spends most of its
// 30 minutes in — and asserts from the test goroutine that the app is still
// locked and that a competing writer still cannot get in. Turning the
// `defer release()` in PullAndApply into a bare `release()` fails it.
func TestPullAndApplyHoldsTheLockUntilTheWorkIsDone(t *testing.T) {
	app, composeFile, newComposeYAML := newLockTestApp(t)

	original, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}

	written := make(chan struct{})
	proceed := make(chan struct{})
	realWrite := writeComposeFile
	writeComposeFile = func(composeYAML []byte, path string) error {
		if err := realWrite(composeYAML, path); err != nil {
			return err
		}
		close(written)
		<-proceed
		return nil
	}
	t.Cleanup(func() { writeComposeFile = realWrite })

	// An already-cancelled context: everything downstream of the write talks to
	// Docker, and a unit test must neither pull images nor start containers.
	// Cancellation makes those steps fail immediately, which exercises the
	// rollback path as a bonus. Nothing before the write consults the context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- app.PullAndApply(ctx, newComposeYAML) }()

	select {
	case <-written:
	case err := <-done:
		t.Fatalf("PullAndApply returned before the new compose file was written: %v", err)
	case <-time.After(2 * time.Minute):
		close(proceed)
		t.Fatal("PullAndApply never reached the compose write")
	}

	if !MyService.AppLock().IsLocked(app.Name) {
		t.Error("the app is not locked while its compose file is half-applied - the lock covers the prologue, not the work")
	}

	if release, err := MyService.AppLock().Acquire(app.Name); err == nil {
		release()
		t.Error("a second writer acquired the app lock in the middle of an update - two writers now own one compose file")
	}

	close(proceed)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PullAndApply reported success on a cancelled context")
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("PullAndApply did not return after being released")
	}

	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("the app lock outlived PullAndApply - the app would be permanently unmanageable")
	}

	// The failed update must have put the original configuration back.
	current, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("the app has NO compose file after a failed update: %v", err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("a failed update left the app running on a configuration that never came up")
	}
}

// TestRestoreComposeFileNeverUnlinksTheDestination covers the rollback's own
// safety. file.CopySingleFile deletes the destination BEFORE it copies, so a
// restore that fails half way left the app with no compose file at all — the
// recovery path destroying the thing it was recovering. Both cases below fail
// the copy and demand the live file survive.
func TestRestoreComposeFileNeverUnlinksTheDestination(t *testing.T) {
	const liveCompose = "name: live\nservices:\n  app:\n    image: busybox\n"

	t.Run("missing snapshot", func(t *testing.T) {
		dir := t.TempDir()
		composeFile := filepath.Join(dir, "docker-compose.yml")
		if err := os.WriteFile(composeFile, []byte(liveCompose), 0o600); err != nil {
			t.Fatalf("write compose file: %v", err)
		}

		if err := restoreComposeFile(filepath.Join(dir, "docker-compose.yml.bak-gone"), composeFile); err == nil {
			t.Fatal("restoring from a snapshot that does not exist must fail")
		}

		got, err := os.ReadFile(composeFile)
		if err != nil {
			t.Fatalf("the app was left with no compose file at all: %v", err)
		}
		if string(got) != liveCompose {
			t.Fatalf("a failed restore mutated the app's compose file: %q", string(got))
		}
	})

	t.Run("empty snapshot", func(t *testing.T) {
		dir := t.TempDir()
		composeFile := filepath.Join(dir, "docker-compose.yml")
		if err := os.WriteFile(composeFile, []byte(liveCompose), 0o600); err != nil {
			t.Fatalf("write compose file: %v", err)
		}
		// A snapshot truncated by a crash. Restoring it would leave an app that
		// cannot be loaded, which is worse than the configuration that failed.
		backup := filepath.Join(dir, "docker-compose.yml.bak-truncated")
		if err := os.WriteFile(backup, nil, 0o600); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}

		if err := restoreComposeFile(backup, composeFile); err == nil {
			t.Fatal("restoring an empty snapshot must fail")
		}

		got, err := os.ReadFile(composeFile)
		if err != nil {
			t.Fatalf("the app was left with no compose file at all: %v", err)
		}
		if string(got) != liveCompose {
			t.Fatalf("a failed restore mutated the app's compose file: %q", string(got))
		}
	})
}

// TestSnapshotComposeFileWritesThroughItsReservedHandle pins the invariant the
// snapshot's comment promises: the name os.CreateTemp reserved is held for the
// whole call, so no concurrent CreateTemp in the same directory can be handed
// it. Copying through the PATH (file.CopySingleFile) unlinks the placeholder
// and recreates it, which frees the name in between.
//
// The observable trace is the mode. CreateTemp's file is 0600; a file
// recreated from the source's metadata carries the source's mode. Umask is
// pinned to 0 for the call so the assertion means the same thing on any host.
// The mode is worth pinning on its own account too: the snapshot is a verbatim
// copy of a compose file that carries env vars and credentials, and it can sit
// in the app directory for months after an interrupted update.
func TestSnapshotComposeFileWritesThroughItsReservedHandle(t *testing.T) {
	_, composeFile, _ := newLockTestApp(t)

	if err := os.Chmod(composeFile, 0o666); err != nil {
		t.Fatalf("chmod compose file: %v", err)
	}

	previousUmask := syscall.Umask(0)
	backup, err := snapshotComposeFile(composeFile)
	syscall.Umask(previousUmask)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer os.Remove(backup)

	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("the snapshot is not at the name that was reserved for it: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the snapshot is not the file CreateTemp reserved (mode %04o, want 0600) - the reserved name was released mid-copy, and the backup is more permissive than the compose file it copies", info.Mode().Perm())
	}

	original, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("the snapshot does not hold the compose file it was taken from")
	}
}

// TestPullAndApplyBusyErrorIsSkippableByAutoUpdate pins the contract the
// auto-update sweep depends on: a rejection is recognisable as ErrComposeAppBusy
// so the sweep logs a skip instead of flagging the stack as a failed update.
func TestPullAndApplyBusyErrorIsSkippableByAutoUpdate(t *testing.T) {
	app, _, newComposeYAML := newLockTestApp(t)

	release, err := MyService.AppLock().Acquire(app.Name)
	if err != nil {
		t.Fatalf("acquire app lock: %v", err)
	}
	defer release()

	err = app.PullAndApply(context.Background(), newComposeYAML)
	if !errors.Is(err, ErrComposeAppBusy) {
		t.Fatalf("auto-update cannot tell a busy app from a broken one: got %v", err)
	}
}

// TestUpdateAndApplyRejectWhenAppBusy covers the advisory fast-fail on the two
// HTTP-facing entry points. Both spawn detached goroutines, so without this the
// caller gets a 200 and the real rejection only ever appears in a log line.
func TestUpdateAndApplyRejectWhenAppBusy(t *testing.T) {
	app, _, newComposeYAML := newLockTestApp(t)

	release, err := MyService.AppLock().Acquire(app.Name)
	if err != nil {
		t.Fatalf("acquire app lock: %v", err)
	}
	defer release()

	ctx := common.WithProperties(context.Background(), map[string]string{})

	if err := app.Update(ctx); !errors.Is(err, ErrComposeAppBusy) {
		t.Errorf("Update did not reject a busy app: %v", err)
	}
	if err := app.Apply(ctx, newComposeYAML); !errors.Is(err, ErrComposeAppBusy) {
		t.Errorf("Apply did not reject a busy app: %v", err)
	}
}

// TestUpdateRejectsWhileUpgradeInFlight covers the other half of the advisory
// check: StartUpgrade/FinishUpgrade were set but never consulted, so a second
// "Update" click sailed straight through to a detached goroutine.
func TestUpdateRejectsWhileUpgradeInFlight(t *testing.T) {
	app, _, _ := newLockTestApp(t)

	MyService.AppStoreManagement().StartUpgrade(app.Name)
	defer MyService.AppStoreManagement().FinishUpgrade(app.Name)

	ctx := common.WithProperties(context.Background(), map[string]string{})
	if err := app.Update(ctx); !errors.Is(err, ErrComposeAppBusy) {
		t.Fatalf("a second update was accepted while one is already upgrading: %v", err)
	}
}

// TestSnapshotComposeFileIsUniquePerOperation guards the shared-backup half of
// B7: with the old fixed "<compose>.bak" path, two operations snapshotted over
// each other and a rollback restored a config that had never run.
func TestSnapshotComposeFileIsUniquePerOperation(t *testing.T) {
	_, composeFile, _ := newLockTestApp(t)

	original, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}

	first, err := snapshotComposeFile(composeFile)
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	defer os.Remove(first)

	second, err := snapshotComposeFile(composeFile)
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	defer os.Remove(second)

	if first == second {
		t.Fatalf("two operations share the same backup path %q - each would overwrite the other's rollback", first)
	}
	for _, path := range []string{first, second} {
		if path == composeFile+".bak" {
			t.Fatalf("snapshot fell back to the fixed shared path %q", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read snapshot %q: %v", path, err)
		}
		if !bytes.Equal(original, got) {
			t.Fatalf("snapshot %q does not hold the original compose file", path)
		}
	}
}

// TestPullAndApplyCleansUpSnapshotWhenNothingWasWritten: an operation that dies
// before the new YAML lands on disk leaves the compose file untouched, so its
// snapshot is pure garbage and must go. The pre-B7 code returned from the
// apiService() failure path before any cleanup was even registered, orphaning a
// backup on every attempt.
func TestPullAndApplyCleansUpSnapshotWhenNothingWasWritten(t *testing.T) {
	app, composeFile, newComposeYAML := newLockTestApp(t)

	original, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}

	// WriteToFullPath stages through "<compose>.tmp"; a directory sitting on
	// that name makes the write fail deterministically, without needing a
	// docker daemon to reject anything.
	if err := os.Mkdir(composeFile+".tmp", 0o700); err != nil {
		t.Fatalf("block compose write: %v", err)
	}

	if err := app.PullAndApply(context.Background(), newComposeYAML); err == nil {
		t.Fatal("expected PullAndApply to fail when the compose file cannot be written")
	}

	current, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read compose file after failure: %v", err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("failed operation left the compose file modified")
	}
	assertNoBackupLeftovers(t, composeFile)

	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("app lock was not released after a failed PullAndApply - the app would be permanently unmanageable")
	}
}

// TestPullAndApplyMissingComposeFile: the auto-update sweep calls PullAndApply
// directly, without the len(ComposeFiles) check Update and Apply do, so a
// project whose compose file vanished used to panic the sweep goroutine on
// ComposeFiles[0].
func TestPullAndApplyMissingComposeFile(t *testing.T) {
	app, _, newComposeYAML := newLockTestApp(t)
	app.ComposeFiles = nil

	if err := app.PullAndApply(context.Background(), newComposeYAML); !errors.Is(err, ErrComposeFileNotFound) {
		t.Fatalf("expected ErrComposeFileNotFound, got %v", err)
	}
}
