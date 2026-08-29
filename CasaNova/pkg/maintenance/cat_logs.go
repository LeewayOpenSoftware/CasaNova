package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ─── 3.3 system-rotated-logs ────────────────────────────────────────────────
//
// Rotated log files that no process is still writing to: ~297 MB on this box
// (syslog.1 alone is 244 MB uncompressed, thanks to delaycompress).
//
// Detection is a shape, not a name pattern: `<base>.<N>` or `<base>.<N>.gz`
// where `<base>` exists in the same directory as a live regular file. There is
// no glob for "*log*", no "*.old", no directory called "temp".
//
// The load-bearing guard is the open-descriptor check, and it is by inode
// (procfd.go). If we grabbed the file rsyslogd is still writing to, the unlink
// would free nothing at all — the inode stays alive as long as the fd does —
// and the user would read "297 MB reclaimed" next to a df that never moved.

const (
	// rotatedLogMinAge is the settling time after a rotation. logrotate
	// renames first and signals the daemon afterwards; until the writer
	// reopens, the file it holds open is the one we would be deleting.
	rotatedLogMinAge = 2 * time.Hour

	// logScanMaxDepth keeps the walk to /var/log/* and /var/log/*/*, which
	// covers the per-service directories (casaos/, casanova/, apt/) without
	// wandering into anything deeper that we have no rule for.
	logScanMaxDepth = 2
)

// rotatedNameRe matches `<base>.<N>` and `<base>.<N>.gz`. The base is captured
// so it can be checked for existence — that check, not the regexp, is what
// makes this safe.
var rotatedNameRe = regexp.MustCompile(`^(.+)\.([0-9]+)(\.gz)?$`)

// securityLogBases are login/accounting records, not disposable service logs.
// wtmp is the login history, btmp the failed-login history, faillog/lastlog the
// per-user counters — the exact trail an administrator investigating an
// intrusion needs, and worth kilobytes at most. A rotated wtmp.1 satisfies the
// "base alive, old, no writer" test just like any other, so without this set it
// was not only listed but PRE-TICKED, and the default Analyse→Clean flow would
// have shredded the login audit trail. Excluding by name here only ADDS
// protection (rule 2 forbids deleting by a name match, never protecting by
// one), consistent with how /var/log/audit is already excluded wholesale.
var securityLogBases = map[string]bool{
	"wtmp":    true,
	"btmp":    true,
	"faillog": true,
	"lastlog": true,
}

func scanRotatedLogs(e *Engine) *Finding {
	f := newFinding(CatRotatedLogs)

	if !e.fdIndexUsable() {
		f.Summary = "Open files could not be enumerated."
		f.block(BlockNoProcAccess, "/proc could not be read, so we cannot prove a log has no writer")
		return f
	}

	now := e.env.now()
	cutoff := now.Add(-rotatedLogMinAge)
	sz := newSizer()
	lim := newPlanLimits()

	dirs := logDirsToScan(e)
	sort.Strings(dirs)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		live := liveRegularFiles(dir, entries)

		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)

		for _, name := range names {
			m := rotatedNameRe.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			if securityLogBases[m[1]] {
				// A security/accounting record, never a disposable log.
				continue
			}
			baseMTime, baseOK := live[m[1]]
			if !baseOK {
				// No live `<base>` next to it: this is not a rotation of
				// anything we can see, so it is not ours to judge. The dead
				// service logs the design mentions land here on purpose —
				// deleting less is the correct failure.
				continue
			}
			path := filepath.Join(dir, name)
			st, err := lstatNoFollow(path)
			if err != nil || !isRegular(&st) {
				continue
			}
			mt := mtimeOf(&st)
			if !mt.Before(cutoff) {
				continue // rotated too recently to be sure the writer moved on
			}
			if baseMTime.Before(mt) {
				// The live file has not been written since the rotation, so
				// the writer has not reopened yet and may still hold this one.
				continue
			}
			detail := fmt.Sprintf("rotated copy of %s, last written %s", m[1], mt.Format(time.RFC3339))
			it, err := e.item(CatRotatedLogs, path, name, detail, cutoff, sz, lim)
			if err != nil {
				f.Notes = append(f.Notes, name+" skipped: "+err.Error())
				continue
			}
			if holder, held := e.anyTargetHeldOpen(it); held {
				f.Notes = append(f.Notes, name+" skipped: still open by "+holder)
				continue
			}
			it.Preselected = true
			f.addItem(it)
		}
	}

	f.Preselect = f.ItemCount > 0
	if f.ItemCount == 0 {
		f.Summary = "No rotated logs without a writer."
	} else {
		f.Summary = fmt.Sprintf("%d rotated log file(s) with no process holding them open.", f.ItemCount)
	}
	f.Notes = append(f.Notes, "Live logs, /var/log/audit and login records (wtmp/btmp) are never included.")
	return f
}

func precheckRotatedLogs(e *Engine, _ []Item) *PreflightError {
	if !e.fdIndexUsable() {
		return &PreflightError{Code: BlockNoProcAccess, Reason: "/proc could not be read"}
	}
	return nil
}

// logDirsToScan lists /var/log and its immediate subdirectories, minus the two
// that are somebody else's business.
//
//   - audit/ is forensic evidence and is excluded by prefix (design §2).
//   - journal/ belongs to the orphan-journal category, which deletes by
//     machine-id identity. A rotation-shaped name inside it must never be
//     matched by THIS detector.
func logDirsToScan(e *Engine) []string {
	root := filepath.Clean(e.env.LogDir)
	out := []string{root}
	remote := e.guard.mounts()

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth >= logScanMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			child := filepath.Join(dir, entry.Name())
			if child == filepath.Join(root, "audit") || child == filepath.Clean(e.env.JournalDir) {
				continue
			}
			if remote[child] {
				continue
			}
			// os.ReadDir reports a symlink to a directory as a non-directory,
			// so a planted link never becomes a scan root here.
			st, err := lstatNoFollow(child)
			if err != nil || !isDir(&st) {
				continue
			}
			out = append(out, child)
			walk(child, depth+1)
		}
	}
	walk(root, 0)
	return out
}

// liveRegularFiles indexes the regular files in a directory by name, with
// their mtime. The mtime is what tells us whether a writer has already moved
// on to the new file after a rotation.
func liveRegularFiles(dir string, entries []os.DirEntry) map[string]time.Time {
	out := map[string]time.Time{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		st, err := lstatNoFollow(filepath.Join(dir, entry.Name()))
		if err != nil || !isRegular(&st) {
			continue
		}
		out[entry.Name()] = mtimeOf(&st)
	}
	return out
}

// ─── 3.4 orphan-journal ─────────────────────────────────────────────────────
//
// /var/log/journal/<machine-id>/ for a machine-id that is NOT this machine's:
// 180 MB of real blocks on this box (375 MB apparent — the files are sparse,
// which is exactly why rule 14 counts st_blocks).
//
// This is the category that justifies the whole module. `journalctl
// --disk-usage` reports 40.3 MB and cannot see these 180 MB at all, because
// they do not belong to the current machine-id: a generic cleaner running
// `--vacuum-size` here reports success and frees precisely nothing.
//
// Deleted BY IDENTITY: the subdirectory whose name equals /etc/machine-id is
// untouchable by construction. If journald's Storage were persistent/auto,
// that directory would be the LIVE store, and the only correct way to shrink
// it is journalctl --vacuum-*, never rm — removing mapped .journal files
// corrupts the index.

const (
	// journalMinAge is a deliberately generous floor instead of a parse of
	// journald.conf's MaxRetentionSec. systemd time spans ("1day", "2 weeks",
	// "4h 30min") are their own little grammar, and a parser that gets one
	// form wrong under-estimates the retention and deletes something still
	// covered by it. A month is longer than any default retention and the
	// orphan on this box is 16 months old.
	journalMinAge = 30 * 24 * time.Hour
)

// machineIDRe is the shape of a machine-id: 32 lowercase hex digits. A
// directory that does not look like one is not something we recognise, so it
// is left alone rather than assumed to be junk.
var machineIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func scanOrphanJournal(e *Engine) *Finding {
	f := newFinding(CatOrphanJournal)

	current := currentMachineID(e.env.MachineIDFile)
	if current == "" {
		f.Summary = "This machine's ID could not be read."
		f.block(BlockInconsistent, "/etc/machine-id is missing or unreadable, so no journal can be proven orphaned")
		return f
	}
	if !e.fdIndexUsable() {
		f.Summary = "Open files could not be enumerated."
		f.block(BlockNoProcAccess, "/proc could not be read, so we cannot prove journald is not using these files")
		return f
	}

	entries, err := os.ReadDir(e.env.JournalDir)
	if err != nil {
		f.Summary = "No persistent journal directory."
		return f
	}

	cutoff := e.env.now().Add(-journalMinAge)
	sz := newSizer()
	lim := newPlanLimits()

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !machineIDRe.MatchString(name) {
			continue
		}
		if name == current {
			f.Notes = append(f.Notes, name+" kept: this machine's journal")
			continue
		}
		path := filepath.Join(e.env.JournalDir, name)
		st, err := lstatNoFollow(path)
		if err != nil || !isDir(&st) {
			continue
		}
		it, err := e.item(CatOrphanJournal, path, name,
			"journal of machine-id "+name+", which is not this machine ("+current+")",
			time.Time{}, sz, lim)
		if err != nil {
			f.Notes = append(f.Notes, name+" skipped: "+err.Error())
			continue
		}
		if newest := newestTargetMTime(it); !newest.Before(cutoff) {
			f.Notes = append(f.Notes, name+" skipped: its newest entry is more recent than the retention floor")
			continue
		}
		if holder, held := e.anyTargetHeldOpen(it); held {
			f.Notes = append(f.Notes, name+" skipped: still open by "+holder)
			continue
		}
		it.Preselected = true
		f.addItem(it)
	}

	f.Preselect = f.ItemCount > 0
	if f.ItemCount == 0 {
		f.Summary = "No journal from a previous installation. Current machine-id: " + current + "."
	} else {
		f.Summary = fmt.Sprintf("%d journal directory/ies belonging to a previous installation of this box.", f.ItemCount)
	}
	return f
}

func precheckOrphanJournal(e *Engine, items []Item) *PreflightError {
	current := currentMachineID(e.env.MachineIDFile)
	if current == "" {
		return &PreflightError{Code: BlockInconsistent, Reason: "/etc/machine-id is unreadable"}
	}
	// Identity again, right before acting: the machine-id can change under us
	// (a clone, a reinstall of the id file), and if it now matches a planned
	// directory that directory is the LIVE journal.
	for _, it := range items {
		if filepath.Base(it.Path) == current {
			return &PreflightError{Code: BlockInconsistent, Reason: "the plan contains this machine's own journal"}
		}
	}
	if !e.fdIndexUsable() {
		return &PreflightError{Code: BlockNoProcAccess, Reason: "/proc could not be read"}
	}
	return nil
}

func currentMachineID(path string) string {
	id := strings.ToLower(readTrimmedFile(path))
	if !machineIDRe.MatchString(id) {
		return ""
	}
	return id
}

// newestTargetMTime is the most recent mtime in an item's plan — the age test
// for a whole tree, since one fresh file is enough to mean "still in use".
func newestTargetMTime(it Item) time.Time {
	var newest time.Time
	for _, t := range it.targets {
		if t.MTime.After(newest) {
			newest = t.MTime
		}
	}
	return newest
}
