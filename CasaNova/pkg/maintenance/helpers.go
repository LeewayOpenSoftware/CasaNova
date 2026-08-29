package maintenance

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/service"
	"golang.org/x/sys/unix"
)

// defaultUploadSessionsFile keeps the registry path in ONE place: the service
// package that writes it.
func defaultUploadSessionsFile() string { return service.UploadSessionsPath() }

// item turns one path into a client-visible Item plus its removal plan.
//
// Every item in every category is built here, which is what makes "the plan is
// leaves-first and was produced by the no-follow walker" true by construction
// rather than by convention. A path the guard refuses never becomes an item —
// the category logs it and moves on, because a finding the executor would
// reject is worse than a finding that is missing.
func (e *Engine) item(category, path, label, detail string, cutoff time.Time, sz *sizer, lim *planLimits) (Item, error) {
	targets, err := e.guard.expandTree(path, cutoff, sz, lim)
	if err != nil {
		return Item{}, err
	}
	var bytes int64
	for _, t := range targets {
		bytes += t.Bytes
	}
	// expandTree emits the entry itself last: the file, or the container
	// directory after its contents.
	head := targets[len(targets)-1]
	return Item{
		ID:      itemID(category, path, head.Ino),
		Label:   label,
		Path:    path,
		Bytes:   bytes,
		MTime:   head.MTime,
		Detail:  detail,
		targets: targets,
	}, nil
}

// anyTargetHeldOpen reports whether any file in an item's plan is still open by
// some process. Used at SCAN time so a whole tree can be withheld with an
// explanation, rather than half-deleted at run time.
func (e *Engine) anyTargetHeldOpen(it Item) (string, bool) {
	if e.fdx == nil {
		return "", false
	}
	for _, t := range it.targets {
		if t.Kind == KindDir {
			continue
		}
		if holder, held := e.fdx.Holder(t.Dev, t.Ino); held {
			return holder, true
		}
	}
	return "", false
}

// fdIndexUsable reports whether the open-descriptor check is actually working.
//
// It matters because "no process has this open" is the load-bearing guard for
// the log categories, and an unreadable /proc produces exactly the same answer
// as a genuinely idle file. When we cannot tell the two apart we refuse.
func (e *Engine) fdIndexUsable() bool {
	p, ok := e.fdx.(interface{ usable() bool })
	if !ok {
		return true // an injected lookup is authoritative by definition
	}
	return p.usable()
}

// updateStage reports what the self-updater is doing.
func (e *Engine) updateStage() string {
	if e.env.UpdateStage != nil {
		return e.env.UpdateStage()
	}
	raw, err := os.ReadFile(e.env.UpdateStatusFile)
	if err != nil {
		return "" // no file: no update has run since boot
	}
	var s struct {
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s.Stage
}

// isUpdateBusyStage lists the stages during which the updater owns the update
// directories. Deleting a package out from under an apply is how you end up
// with a half-installed system and no way back.
func isUpdateBusyStage(stage string) bool {
	switch stage {
	case "downloading", "verifying", "applying", "restarting":
		return true
	}
	return false
}

// readTrimmedFile reads a small text file and trims it. Empty on any error:
// callers treat "cannot read" and "empty" the same way, which is "refuse".
func readTrimmedFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// fileLockHolder reports whether another process holds a write lock on path,
// and its pid.
//
// fcntl(F_GETLK), NOT flock(2). apt and dpkg take their locks with
// fcntl(F_SETLK) (apt-pkg/contrib/fileutl.cc, dpkg lib/dpkg/lock.c), and on
// Linux the two lock families do not see each other at all: an flock probe
// would succeed happily while dpkg is mid-transaction, i.e. it would be a
// guard that can never fire. F_GETLK also asks without taking anything, so
// probing can never itself become the thing that blocks apt.
func fileLockHolder(path string) (pid int, held bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		// No lock file means apt has never run here, or is not installed.
		return 0, false
	}
	defer unix.Close(fd)

	lk := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: 0, // SEEK_SET
		Start:  0,
		Len:    0, // whole file
	}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_GETLK, &lk); err != nil {
		// If we cannot ask, we cannot claim the file is free.
		return 0, true
	}
	if lk.Type == unix.F_UNLCK {
		return 0, false
	}
	return int(lk.Pid), true
}
