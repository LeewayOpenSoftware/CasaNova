package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

// liveAppComposeFile plants an installed app's directory under AppsPath and
// returns the path of its compose file plus the exact bytes it holds. Every
// assertion in this file is about those bytes still being there afterwards:
// Install mkdirs AppsPath/<name> and overwrites its docker-compose.yml before
// anything asynchronous happens, so any rejection that arrives late has already
// destroyed the configuration of whatever was installed under that name.
func liveAppComposeFile(t *testing.T, appName string) (composeFile string, content []byte) {
	t.Helper()

	previous := config.AppInfo.AppsPath
	config.AppInfo.AppsPath = t.TempDir()
	t.Cleanup(func() { config.AppInfo.AppsPath = previous })

	workingDir := filepath.Join(config.AppInfo.AppsPath, appName)
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatalf("create live app dir: %v", err)
	}

	content = []byte("name: " + appName + "\nservices:\n  app:\n    image: busybox\n    volumes:\n      - /mnt/data:/data\n")
	composeFile = filepath.Join(workingDir, common.ComposeYAMLFileName)
	if err := os.WriteFile(composeFile, content, 0o600); err != nil {
		t.Fatalf("write live compose file: %v", err)
	}

	return composeFile, content
}

func assertLiveAppUntouched(t *testing.T, composeFile string, want []byte) {
	t.Helper()

	got, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("the installed app's compose file is gone: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the installed app's compose file was overwritten:\n got: %q\nwant: %q", string(got), string(want))
	}
}

// TestInstallRejectsBeforeTouchingTheAppDirectoryWhenBusy is the CRITICAL
// regression. Install used to take the app lock inside the goroutine that does
// the pull, i.e. AFTER it had already mkdir'd AppsPath/<name> and overwritten
// its docker-compose.yml. With the lock now held for a whole update, a store
// install aimed at the name of an app the nightly sweep was updating would
// clobber the live compose file first and only then be told the app was busy.
func TestInstallRejectsBeforeTouchingTheAppDirectoryWhenBusy(t *testing.T) {
	app, _, _ := newLockTestApp(t)
	composeFile, content := liveAppComposeFile(t, app.Name)

	// Stand in for the in-flight operation that owns the app: an update, the
	// auto-update sweep, an uninstall.
	release, err := MyService.AppLock().Acquire(app.Name)
	if err != nil {
		t.Fatalf("acquire app lock: %v", err)
	}
	defer release()

	listComposeAppsCalled := false
	previousList := listComposeApps
	listComposeApps = func(ctx context.Context, s *ComposeService) (map[string]*ComposeApp, error) {
		listComposeAppsCalled = true
		return map[string]*ComposeApp{}, nil
	}
	t.Cleanup(func() { listComposeApps = previousList })

	s := &ComposeService{}
	err = s.Install(context.Background(), app)
	if !errors.Is(err, ErrComposeAppBusy) {
		t.Fatalf("Install did not recognise the app as busy: %v", err)
	}
	if listComposeAppsCalled {
		t.Error("Install went on working after the app lock rejected it")
	}

	assertLiveAppUntouched(t, composeFile, content)

	// A rejection must not consume the holder's lock, and must not leave the
	// install slot claimed: the user has to be able to retry once the other
	// operation finishes.
	release()
	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("the app stayed locked after the holder released - the rejected install released a lock it never held")
	}
	if !s.TryAcquireInstall(app.Name) {
		t.Fatal("the rejected install left the install slot claimed - the app can never be installed again")
	}
}

// TestInstallRefusesToOverwriteAnInstalledApp: an idle installed app holds no
// lock, so the lock alone does not protect it. Install is not an update path —
// it rewrites the compose file from the store's YAML — so pointing it at a
// name Docker already runs would silently discard that app's ports, env,
// bind-mount paths and x-casaos metadata while its containers keep running.
func TestInstallRefusesToOverwriteAnInstalledApp(t *testing.T) {
	app, _, _ := newLockTestApp(t)
	composeFile, content := liveAppComposeFile(t, app.Name)

	previousList := listComposeApps
	listComposeApps = func(ctx context.Context, s *ComposeService) (map[string]*ComposeApp, error) {
		return map[string]*ComposeApp{app.Name: app}, nil
	}
	t.Cleanup(func() { listComposeApps = previousList })

	s := &ComposeService{}
	err := s.Install(context.Background(), app)
	if !errors.Is(err, ErrComposeAppAlreadyInstalled) {
		t.Fatalf("Install accepted a name that is already installed: %v", err)
	}

	assertLiveAppUntouched(t, composeFile, content)

	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("the rejected install kept the app lock - every later update of that app would be rejected as busy")
	}
}

// TestPullAndInstallRefusesToRunWithoutTheAppLock: the install work must never
// start unless the caller has been holding the app lock since before it touched
// AppsPath/<name>. Checking it here means a future caller that takes the lock
// too late (or not at all) fails loudly on its first run instead of silently
// racing whoever else is writing that app's compose file.
func TestPullAndInstallRefusesToRunWithoutTheAppLock(t *testing.T) {
	app, _, _ := newLockTestApp(t)

	err := app.pullAndInstall(context.Background())
	if err == nil {
		t.Fatal("the install ran without its caller holding the app lock")
	}
	// Pin the reason: the refusal has to come from the precondition, not from
	// some later step that happened to fail on this host.
	if !strings.Contains(err.Error(), "does not hold the app lock") {
		t.Fatalf("the install got past the lock precondition and failed later instead: %v", err)
	}
}

// TestInstallNeverDeletesAnAppDirectoryItDidNotCreate: every failure path in
// Install deletes AppsPath/<name> recursively, which is only ever safe for a
// directory this install made. A pre-existing one holds someone else's compose
// file, config folder and — for apps that bind-mount ./data — the user's data.
// Leaving a stale directory behind costs a few KB; deleting the wrong one is
// unrecoverable.
func TestInstallNeverDeletesAnAppDirectoryItDidNotCreate(t *testing.T) {
	app, _, _ := newLockTestApp(t)
	composeFile, _ := liveAppComposeFile(t, app.Name)
	workingDir := filepath.Dir(composeFile)

	userData := filepath.Join(workingDir, "config", "secret.env")
	if err := os.MkdirAll(filepath.Dir(userData), 0o755); err != nil {
		t.Fatalf("create app config dir: %v", err)
	}
	if err := os.WriteFile(userData, []byte("TOKEN=keep-me\n"), 0o600); err != nil {
		t.Fatalf("write app config: %v", err)
	}

	// Force the compose write to fail after the working directory has been
	// prepared: a directory where the compose file belongs cannot be renamed
	// over. This is the cheapest deterministic stand-in for the real failures
	// on that line (ENOSPC, EROFS, EIO on a dying disk).
	if err := os.Remove(composeFile); err != nil {
		t.Fatalf("remove compose file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(composeFile, "blocker"), 0o755); err != nil {
		t.Fatalf("block compose write: %v", err)
	}

	previousList := listComposeApps
	listComposeApps = func(ctx context.Context, s *ComposeService) (map[string]*ComposeApp, error) {
		return map[string]*ComposeApp{}, nil
	}
	t.Cleanup(func() { listComposeApps = previousList })

	s := &ComposeService{}
	if err := s.Install(context.Background(), app); err == nil {
		t.Fatal("expected Install to fail when the compose file cannot be written")
	}

	if _, err := os.Stat(userData); err != nil {
		t.Fatalf("a failed install deleted an app directory it did not create, taking the user's data with it: %v", err)
	}

	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("the failed install kept the app lock")
	}
}

// TestInstallFailsClosedWhenTheAppInventoryIsUnavailable: with no answer from
// Docker there is no way to tell a free name from a live app's name, and being
// wrong here overwrites a running app. Nothing has been written at that point,
// so refusing costs the caller a retry and nothing else.
func TestInstallFailsClosedWhenTheAppInventoryIsUnavailable(t *testing.T) {
	app, _, _ := newLockTestApp(t)
	composeFile, content := liveAppComposeFile(t, app.Name)

	previousList := listComposeApps
	listComposeApps = func(ctx context.Context, s *ComposeService) (map[string]*ComposeApp, error) {
		return nil, errors.New("cannot connect to the Docker daemon")
	}
	t.Cleanup(func() { listComposeApps = previousList })

	s := &ComposeService{}
	if err := s.Install(context.Background(), app); err == nil {
		t.Fatal("Install proceeded without knowing whether the app name was free")
	}

	assertLiveAppUntouched(t, composeFile, content)

	if MyService.AppLock().IsLocked(app.Name) {
		t.Fatal("the rejected install kept the app lock")
	}
}
