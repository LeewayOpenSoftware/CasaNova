package maintenance

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A fixture of the box's real layout, so the categories can be exercised
// without a root shell and without anything under /var being at risk.

const installedTestVersion = "18.6.2.dev1.0808261615"

type fixture struct {
	root string
	env  *Env
	e    *Engine
	t    *testing.T
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()

	env := &Env{
		UpdatesDir:        filepath.Join(root, "var/lib/casanova/updates"),
		UpdatesArchiveDir: filepath.Join(root, "var/lib/casanova/updates_archive"),
		BackupsDir:        filepath.Join(root, "var/lib/casanova/backups"),
		ManifestFile:      filepath.Join(root, "var/lib/casanova/manifest.json"),
		UpdateStatusFile:  filepath.Join(root, "run/casanova/update-status.json"),

		LogDir:         filepath.Join(root, "var/log"),
		JournalDir:     filepath.Join(root, "var/log/journal"),
		MachineIDFile:  filepath.Join(root, "etc/machine-id"),
		AptArchivesDir: filepath.Join(root, "var/cache/apt/archives"),
		DpkgLockFile:   filepath.Join(root, "var/lib/dpkg/lock-frontend"),
		AptLockFile:    filepath.Join(root, "var/cache/apt/archives/lock"),

		// The scratch detector is exercised here against a controlled temp root,
		// so the unit tests turn it on explicitly. Production leaves it off
		// (DefaultEnv), which TestScratchDisabledByDefaultInProduction pins.
		ScratchEnabled:     true,
		ScratchRoots:       []string{filepath.Join(root, "DATA")},
		UploadSessionsFile: filepath.Join(root, "var/lib/casanova/upload-sessions.json"),

		StateDir:       filepath.Join(root, "var/lib/casanova/maintenance"),
		RunningVersion: installedTestVersion,
		ProcRoot:       "/proc",
	}

	mkdirAll(t,
		env.UpdatesDir, env.UpdatesArchiveDir, env.BackupsDir,
		env.LogDir, env.JournalDir, env.AptArchivesDir, env.StateDir,
		filepath.Join(root, "etc"),
		filepath.Join(root, "var/lib/dpkg"),
		filepath.Join(root, "DATA"),
	)

	f := &fixture{root: root, env: env, t: t}
	f.setManifestVersion(installedTestVersion)
	return f
}

// engine builds the engine lazily so a test can adjust the fixture first.
func (f *fixture) engine() *Engine {
	f.t.Helper()
	if f.e == nil {
		e, err := New(f.env)
		if err != nil {
			f.t.Fatalf("New(env): %v", err)
		}
		f.e = e
	}
	return f.e
}

func (f *fixture) scan(category string) *Finding {
	f.t.Helper()
	return f.engine().scanCategory(category)
}

func (f *fixture) setManifestVersion(v string) {
	writeFile(f.t, f.env.ManifestFile, fmt.Sprintf(`{"version":%q,"channel":"dev"}`, v))
}

// write creates a file relative to the fixture root and back-dates it.
func (f *fixture) write(rel, content string, age time.Duration) string {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	writeFile(f.t, path, content)
	if age > 0 {
		f.age(path, age)
	}
	return path
}

func (f *fixture) age(path string, age time.Duration) {
	f.t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) itemLabels(fi *Finding) []string {
	out := make([]string, 0, len(fi.Items))
	for _, it := range fi.Items {
		out = append(out, it.Label)
	}
	return out
}

func (f *fixture) itemIDs(fi *Finding) []string {
	out := make([]string, 0, len(fi.Items))
	for _, it := range fi.Items {
		out = append(out, it.ID)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ─── fcntl lock helper ──────────────────────────────────────────────────────
//
// apt and dpkg lock with fcntl(F_SETLK), and a process cannot see its own
// fcntl locks through F_GETLK, so proving the apt guard works needs a SECOND
// process. Re-executing the test binary with an environment variable set is
// the cheapest way to get one.

const lockHelperEnv = "CASANOVA_MAINTENANCE_TEST_LOCKPATH"

func TestMain(m *testing.M) {
	if path := os.Getenv(lockHelperEnv); path != "" {
		holdFcntlLock(path)
		return
	}
	os.Exit(m.Run())
}

func holdFcntlLock(path string) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT, 0o640)
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_SETLK, &lk); err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED")
	os.Stdout.Sync()
	select {} // held until the parent kills us
}

// holdLock starts the helper and returns a stop function.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "LOCKED\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("lock helper did not start: %q (%v)", line, err)
	}
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func removeFile(path string) error { return os.Remove(path) }
