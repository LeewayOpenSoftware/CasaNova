package maintenance

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Open-descriptor detection (design rule 6).
//
// The check is by INODE, never by path. logrotate does rename(2), not
// close+create: rsyslogd keeps writing to the same inode under its new name
// until it is told to reopen. A path-based check would look at
// /var/log/syslog.1, see no process with that path open, and delete the file
// the logger is still appending to. The unlink then frees nothing (the inode
// stays alive as long as the fd does), logging keeps flowing into a file with
// no name, and the user sees "297 MB reclaimed" next to a df that did not
// move. The fd beats the mtime, always.

type devIno struct {
	dev uint64
	ino uint64
}

// fdLookup answers "does any process hold this inode open". An interface so
// tests can pin the answer without needing a real process to cooperate.
type fdLookup interface {
	Holder(dev, ino uint64) (string, bool)
}

// procFDIndex is a snapshot of every (dev, ino) reachable through /proc/*/fd.
//
// Building it costs one stat per open descriptor on the box — a few tens of
// thousands on a NAS with 27 containers, which is tens of milliseconds. It is
// therefore built once and reused, but never for longer than maxAge: rule 5
// wants the fd re-checked immediately before each unlink, and a snapshot that
// is two seconds old is the honest compromise between "immediately" and
// re-walking /proc once per .deb.
type procFDIndex struct {
	procRoot string
	maxAge   time.Duration

	mu      sync.Mutex
	taken   map[devIno]string
	builtAt time.Time
}

func newProcFDIndex(procRoot string) *procFDIndex {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &procFDIndex{procRoot: procRoot, maxAge: 2 * time.Second}
}

// Holder reports whether some process has the inode open, and who.
func (x *procFDIndex) Holder(dev, ino uint64) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.taken == nil || time.Since(x.builtAt) > x.maxAge {
		x.rebuild()
	}
	h, ok := x.taken[devIno{dev, ino}]
	return h, ok
}

// Refresh forces the next Holder call to rebuild. Used at the start of a run
// so the first check of the run never reuses the scan's snapshot.
func (x *procFDIndex) Refresh() {
	x.mu.Lock()
	x.taken = nil
	x.mu.Unlock()
}

func (x *procFDIndex) rebuild() {
	taken := map[devIno]string{}

	pids, err := os.ReadDir(x.procRoot)
	if err != nil {
		// A /proc we cannot read means we cannot prove a file is unused.
		// Leaving the previous snapshot in place would be worse than useless,
		// so keep an EMPTY map and let the caller's other guards decide: the
		// categories that depend on this check refuse to run without it.
		x.taken, x.builtAt = taken, time.Now()
		return
	}

	for _, p := range pids {
		if !p.IsDir() {
			continue
		}
		name := p.Name()
		if _, err := strconv.Atoi(name); err != nil {
			continue // not a pid: cpuinfo, self, sys…
		}
		fdDir := x.procRoot + "/" + name + "/fd"
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process exited, or not ours to look at
		}
		for _, e := range entries {
			// Stat (not Lstat) on /proc/<pid>/fd/<n> follows the magic link to
			// the object itself, which is how a deleted-but-open file still
			// answers with its real (dev, ino).
			var st unix.Stat_t
			if err := unix.Stat(fdDir+"/"+e.Name(), &st); err != nil {
				continue
			}
			if st.Mode&unix.S_IFMT != unix.S_IFREG {
				continue // sockets, pipes and epolls cannot be what we delete
			}
			key := devIno{st.Dev, st.Ino}
			if _, dup := taken[key]; !dup {
				taken[key] = x.describe(name)
			}
		}
	}

	x.taken, x.builtAt = taken, time.Now()
}

// describe turns a pid into something a support ticket can use.
func (x *procFDIndex) describe(pid string) string {
	comm, err := os.ReadFile(x.procRoot + "/" + pid + "/comm")
	if err != nil {
		return "pid " + pid
	}
	return "pid " + pid + " (" + strings.TrimSpace(string(comm)) + ")"
}

// usable reports whether the index actually saw a running system. An empty
// /proc means we have no evidence either way, and categories whose safety
// rests on the fd check must refuse instead of assuming "nobody has it open".
func (x *procFDIndex) usable() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.taken == nil || time.Since(x.builtAt) > x.maxAge {
		x.rebuild()
	}
	return len(x.taken) > 0
}
