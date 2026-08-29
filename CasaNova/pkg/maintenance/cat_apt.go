package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ─── 3.5 apt-package-cache ──────────────────────────────────────────────────
//
// /var/cache/apt/archives/*.deb — 217 MB in 47 packages on this box. Equivalent
// to `apt-get clean`, but WITHOUT invoking apt: these are plain file removals
// with the locks verified first.
//
// archives/partial/ is never listed and never touched (that is a download in
// flight), and neither is /var/lib/apt/lists (design §2: without it apt can
// neither search nor install until the next update, on a box with a documented
// history of losing its internet while the LAN stayed up).
//
// The safeguard is the lock check. Deleting a .deb that apt is about to
// install breaks the transaction and can leave dpkg demanding
// `dpkg --configure -a` over SSH. With apt idle the risk is zero: it
// re-downloads whatever it needs.

func scanAptCache(e *Engine) *Finding {
	f := newFinding(CatAptCache)

	sz := newSizer()
	lim := newPlanLimits()
	debs, err := filepath.Glob(filepath.Join(e.env.AptArchivesDir, "*.deb"))
	if err != nil {
		f.Summary = "The apt archive directory could not be listed."
		f.block(BlockScanError, err.Error())
		return f
	}
	sort.Strings(debs)

	for _, path := range debs {
		st, lerr := lstatNoFollow(path)
		if lerr != nil || !isRegular(&st) {
			continue
		}
		name := filepath.Base(path)
		it, ierr := e.item(CatAptCache, path, name, "already downloaded package", time.Time{}, sz, lim)
		if ierr != nil {
			f.Notes = append(f.Notes, name+" skipped: "+ierr.Error())
			continue
		}
		it.Preselected = true
		f.addItem(it)
	}

	// The size stays visible even when blocked: the user should see what is
	// being withheld and why, not just an absence.
	if perr := aptBusy(e); perr != nil {
		f.Summary = fmt.Sprintf("%d cached package(s), but apt is busy right now.", f.ItemCount)
		f.block(perr.Code, perr.Reason)
		return f
	}

	f.Preselect = f.ItemCount > 0
	if f.ItemCount == 0 {
		f.Summary = "The apt cache is already empty."
	} else {
		f.Summary = fmt.Sprintf("%d package file(s) apt has already installed.", f.ItemCount)
	}
	f.Notes = append(f.Notes, "archives/partial/ and /var/lib/apt/lists are never touched.")
	return f
}

func precheckAptCache(e *Engine, _ []Item) *PreflightError {
	// Re-checked at run time on purpose: unattended-upgrades can start between
	// the scan and the click, and it is the one apt user nobody remembers.
	return aptBusy(e)
}

// aptBusy reports whether apt/dpkg is mid-transaction, with a reason a human
// can act on.
func aptBusy(e *Engine) *PreflightError {
	// Our own apt runs are worth naming specifically: the host-admin module
	// knows when WE started an upgrade, so the user gets "CasaNova is applying
	// system updates" instead of a generic lock message.
	if e.env.AptSelfManaged != nil {
		if busy, why := e.env.AptSelfManaged(); busy {
			return &PreflightError{Code: BlockDpkgLocked, Reason: why}
		}
	}
	for _, lock := range []string{e.env.DpkgLockFile, e.env.AptLockFile} {
		if lock == "" {
			continue
		}
		if pid, held := fileLockHolder(lock); held {
			reason := "apt or dpkg is running"
			if pid > 0 {
				reason += " (pid " + strconv.Itoa(pid) + ": " + processName(e.env.ProcRoot, pid) + ")"
			}
			return &PreflightError{Code: BlockDpkgLocked, Reason: reason}
		}
	}
	return nil
}

// processName resolves a pid to its comm, for the message only. Best effort:
// an unknown name never changes the decision, only the wording.
func processName(procRoot string, pid int) string {
	if procRoot == "" {
		procRoot = "/proc"
	}
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}
