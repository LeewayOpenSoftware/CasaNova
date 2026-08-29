package maintenance

import (
	"os"
	"path/filepath"
	"testing"
)

// The open-descriptor index is the guard the log categories rest on, so it is
// tested against the real /proc of the test process: the test opens a file and
// the index must find it.

func TestProcFDIndexFindsAnOpenFileByInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.log")
	writeFile(t, path, "content")

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	st, err := lstatNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}

	x := newProcFDIndex("/proc")
	if _, held := x.Holder(st.Dev, st.Ino); !held {
		t.Fatal("an open file was reported as unused")
	}
}

// THE reason the check is by inode and not by path: logrotate renames the file
// while the writer keeps the same descriptor. A path-based check would report
// the renamed file as idle and delete the one rsyslogd is still appending to —
// which frees nothing, because the inode outlives the name.
func TestProcFDIndexFollowsARenamedFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "syslog")
	rotated := filepath.Join(dir, "syslog.1")
	writeFile(t, live, "log line")

	f, err := os.OpenFile(live, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// What logrotate does: rename, writer untouched.
	if err := os.Rename(live, rotated); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("still writing\n"); err != nil {
		t.Fatal(err)
	}

	st, err := lstatNoFollow(rotated)
	if err != nil {
		t.Fatal(err)
	}
	x := newProcFDIndex("/proc")
	holder, held := x.Holder(st.Dev, st.Ino)
	if !held {
		t.Fatal("the renamed file was reported as having no writer")
	}
	if holder == "" {
		t.Fatal("no holder was named")
	}
}

func TestProcFDIndexIgnoresClosedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.log")
	writeFile(t, path, "content")
	st, err := lstatNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}

	x := newProcFDIndex("/proc")
	x.Refresh()
	if _, held := x.Holder(st.Dev, st.Ino); held {
		t.Fatal("a file nobody has open was reported as held")
	}
}

// An unreadable /proc must not look like "nothing is open": the categories
// that depend on the check refuse to run instead.
func TestProcFDIndexReportsItselfUnusableWithoutProc(t *testing.T) {
	x := newProcFDIndex(filepath.Join(t.TempDir(), "no-such-proc"))
	if x.usable() {
		t.Fatal("an index built from a missing /proc claimed to be usable")
	}
}
