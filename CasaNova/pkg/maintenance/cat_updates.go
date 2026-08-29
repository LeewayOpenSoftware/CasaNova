package maintenance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ─── 3.1 casanova-update-packages ───────────────────────────────────────────
//
// Superseded release tarballs in /var/lib/casanova/updates_archive (1.25 GiB on
// this box) and /var/lib/casanova/updates (319 MiB), plus the `.part` stubs a
// cancelled package upload leaves behind.
//
// Four independent guards, ALL required (design §3.1):
//
//  1. the tarball's version is strictly older than what manifest.json says is
//     installed — compared with the ordering comparator, never lexically and
//     never by mtime (a `touch` used to resurrect an old tarball);
//  2. it is not the package of the version now running: POST /update/repair
//     needs exactly that file, and without it a repair means re-uploading
//     320 MB;
//  3. it is not the N-1, which is the one-step rollback;
//  4. no update is staged or being applied. If one is, the whole category is
//     disabled with that reason — there IS a staged update on this box's
//     history, so the case is real rather than theoretical.
//
// changelog.json is never a candidate: only names that match the canonical
// package regexp are ever considered.

// partStubMinAge is how long a `.part` upload stub must sit untouched before we
// call it abandoned. The upload handler writes to it continuously, so an hour
// of silence means the request is gone.
const partStubMinAge = time.Hour

type packageEntry struct {
	path    string
	base    string
	version string
	dir     string
	mtime   time.Time
}

func scanUpdatePackages(e *Engine) *Finding {
	f := newFinding(CatUpdatePackages)

	installed := installedVersion(e.env.ManifestFile)
	if installed == "" {
		f.Summary = "The installed version could not be read from manifest.json."
		f.block(BlockInconsistent, "manifest.json is missing or unreadable, so no package can be proven superseded")
		return f
	}
	if stage := e.updateStage(); isUpdateBusyStage(stage) {
		f.Summary = "An update is being applied right now."
		f.block(BlockUpdateRunning, "an update is in progress ("+stage+")")
		return f
	}

	pkgs := listPackages(e.env.UpdatesDir)
	pkgs = append(pkgs, listPackages(e.env.UpdatesArchiveDir)...)

	// Guard 4: anything NEWER than what is installed is a staged update waiting
	// to be applied. That is the rollback-critical state, so the category
	// closes entirely rather than trying to delete "the other ones".
	for _, p := range pkgs {
		if CompareBuildVersions(p.version, installed) > 0 {
			f.Summary = fmt.Sprintf("A newer package (%s) is staged and has not been applied yet.", p.version)
			f.block(BlockUpdateStaged, "there is a staged update waiting to be applied ("+p.version+")")
			return f
		}
	}

	// Guards 2 and 3, by IDENTITY: collect the exact paths that must survive.
	keep := map[string]string{}
	for _, p := range pkgs {
		if p.version == installed {
			keep[p.path] = "installed version"
		}
		if e.env.RunningVersion != "" && p.version == e.env.RunningVersion {
			keep[p.path] = "version currently running"
		}
	}
	if nm1 := previousVersion(pkgs, installed); nm1 != "" {
		for _, p := range pkgs {
			if p.version == nm1 {
				keep[p.path] = "one-step rollback (N-1)"
			}
		}
	}

	sz := newSizer()
	lim := newPlanLimits()
	cutoff := e.env.now().Add(-partStubMinAge)

	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].base < pkgs[j].base })
	for _, p := range pkgs {
		if why, held := keep[p.path]; held {
			f.Notes = append(f.Notes, p.base+" kept: "+why)
			continue
		}
		if CompareBuildVersions(p.version, installed) >= 0 {
			continue // not strictly older: not our business
		}
		detail := fmt.Sprintf("version %s, older than the installed %s", p.version, installed)
		it, err := e.item(CatUpdatePackages, p.path, p.base, detail, time.Time{}, sz, lim)
		if err != nil {
			f.Notes = append(f.Notes, p.base+" skipped: "+err.Error())
			continue
		}
		f.addItem(it)
	}

	// The `.part` / `.tgz.part` stubs a cancelled package upload leaves in the
	// updates directory. Shape is ours (leading dot + trailing .part, written
	// by PostUpdateUploadPackage), and an hour of no writes means nobody is
	// still streaming into it.
	for _, st := range listPartStubs(e.env.UpdatesDir, cutoff) {
		detail := "interrupted package upload, untouched since " + st.mtime.Format(time.RFC3339)
		it, err := e.item(CatUpdatePackages, st.path, st.base, detail, cutoff, sz, lim)
		if err != nil {
			f.Notes = append(f.Notes, st.base+" skipped: "+err.Error())
			continue
		}
		f.addItem(it)
	}

	// Never preselected. Nothing in the repository creates updates_archive, so
	// "it is ours and we know it is spare" does not apply: the user sees the
	// version and the date and ticks the boxes.
	f.Preselect = false
	f.Summary = fmt.Sprintf("%d package(s) older than the installed %s.", f.ItemCount, installed)
	if f.ItemCount == 0 {
		f.Summary = "No superseded packages. The installed version is " + installed + "."
	}
	return f
}

func precheckUpdatePackages(e *Engine, items []Item) *PreflightError {
	if stage := e.updateStage(); isUpdateBusyStage(stage) {
		return &PreflightError{Code: BlockUpdateRunning, Reason: "an update is in progress (" + stage + ")"}
	}
	installed := installedVersion(e.env.ManifestFile)
	if installed == "" {
		return &PreflightError{Code: BlockInconsistent, Reason: "manifest.json is unreadable"}
	}
	// Re-prove guards 1-3 against the paths we are about to delete: minutes
	// have passed and an update may have been staged in the meantime.
	for _, it := range items {
		v := ExtractVersionFromPackage(it.Path)
		if v == "unknown" {
			continue // a .part stub; its own age check runs at unlink time
		}
		if CompareBuildVersions(v, installed) >= 0 {
			return &PreflightError{
				Code:   BlockInconsistent,
				Reason: "the plan contains " + v + ", which is no longer older than the installed " + installed,
			}
		}
		if e.env.RunningVersion != "" && v == e.env.RunningVersion {
			return &PreflightError{Code: BlockInconsistent, Reason: "the plan contains the running version"}
		}
	}
	return nil
}

// installedVersion reads the version manifest.json declares. That file is the
// product's own record of what was applied, which is why the design picks it
// over the binary's compiled-in version for the "superseded" test.
func installedVersion(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Version)
}

// listPackages returns the release tarballs in one directory. Regular files
// only, name must match the canonical regexp AND carry an orderable version:
// anything else is somebody's file, not our artefact.
func listPackages(dir string) []packageEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []packageEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version, ok := isReleasePackageName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		st, err := lstatNoFollow(path)
		if err != nil || !isRegular(&st) {
			continue
		}
		out = append(out, packageEntry{
			path:    path,
			base:    entry.Name(),
			version: version,
			dir:     dir,
			mtime:   mtimeOf(&st),
		})
	}
	return out
}

// previousVersion returns the highest version strictly below installed — the
// one-step rollback that guard 3 protects.
func previousVersion(pkgs []packageEntry, installed string) string {
	best := ""
	for _, p := range pkgs {
		if CompareBuildVersions(p.version, installed) >= 0 {
			continue
		}
		if best == "" || CompareBuildVersions(p.version, best) > 0 {
			best = p.version
		}
	}
	return best
}

type partStub struct {
	path  string
	base  string
	mtime time.Time
}

// listPartStubs finds the `.<name>.part` / `.<name>.tgz.part` files the package
// upload handler creates. The shape is checked, not guessed: leading dot,
// trailing ".part", regular file, in a directory only our handler writes to.
func listPartStubs(dir string, cutoff time.Time) []partStub {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []partStub
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".part") {
			continue
		}
		path := filepath.Join(dir, name)
		st, err := lstatNoFollow(path)
		if err != nil || !isRegular(&st) {
			continue
		}
		mt := mtimeOf(&st)
		if !mt.Before(cutoff) {
			continue // still being written
		}
		out = append(out, partStub{path: path, base: name, mtime: mt})
	}
	return out
}

// ─── 3.2 casanova-upgrade-backups ───────────────────────────────────────────
//
// /var/lib/casanova/backups/upgrade-<ts>/ trees. Everything EXCEPT the current
// one is offered; the current one is identified two ways that must agree — the
// .last-upgrade-backup pointer and the newest update-report.json, which is
// exactly what feeds the rollback button. If they disagree, that is a broken
// invariant, not a cleanup opportunity, and the category refuses.
//
// The user's retention policy (one upgrade backup) is implemented as "keep the
// last one", never as "keep N": if the pointer is ever missing, "keep N" would
// delete the only backup that exists.

func scanUpgradeBackups(e *Engine) *Finding {
	f := newFinding(CatUpgradeBackups)

	current, err := currentUpgradeBackup(e.env.BackupsDir)
	if err != nil {
		f.Summary = "The current rollback point could not be identified."
		f.block(BlockInconsistent, err.Error())
		return f
	}

	dirs, _ := filepath.Glob(filepath.Join(e.env.BackupsDir, "upgrade-*"))
	sort.Strings(dirs)

	sz := newSizer()
	lim := newPlanLimits()
	for _, dir := range dirs {
		st, err := lstatNoFollow(dir)
		if err != nil || !isDir(&st) {
			continue
		}
		if dir == current {
			f.Notes = append(f.Notes, filepath.Base(dir)+" kept: current rollback point")
			continue
		}
		it, err := e.item(CatUpgradeBackups, dir, filepath.Base(dir),
			"superseded rollback point", time.Time{}, sz, lim)
		if err != nil {
			f.Notes = append(f.Notes, filepath.Base(dir)+" skipped: "+err.Error())
			continue
		}
		if holder, held := e.anyTargetHeldOpen(it); held {
			f.Notes = append(f.Notes, filepath.Base(dir)+" skipped: still open by "+holder)
			continue
		}
		f.addItem(it)
	}

	f.Preselect = false
	switch f.ItemCount {
	case 0:
		f.Summary = "Only the current rollback point exists. Nothing to reclaim."
	default:
		f.Summary = fmt.Sprintf("%d superseded rollback point(s). The current one (%s) is never offered.",
			f.ItemCount, filepath.Base(current))
	}
	return f
}

func precheckUpgradeBackups(e *Engine, items []Item) *PreflightError {
	current, err := currentUpgradeBackup(e.env.BackupsDir)
	if err != nil {
		return &PreflightError{Code: BlockInconsistent, Reason: err.Error()}
	}
	for _, it := range items {
		if it.Path == current {
			return &PreflightError{
				Code:   BlockInconsistent,
				Reason: "the plan contains the current rollback point",
			}
		}
	}
	return nil
}

// currentUpgradeBackup identifies the live rollback point through both routes
// the product already uses, and requires them to agree.
func currentUpgradeBackup(backupsDir string) (string, error) {
	pointer := strings.TrimSpace(readTrimmedFile(filepath.Join(backupsDir, ".last-upgrade-backup")))
	if pointer != "" {
		pointer = filepath.Clean(pointer)
	}
	fromReport := backupFromNewestReport(backupsDir)

	switch {
	case pointer == "" && fromReport == "":
		return "", fmt.Errorf("neither .last-upgrade-backup nor an update-report.json identifies the current rollback point")
	case pointer == "":
		return fromReport, nil
	case fromReport == "":
		return pointer, nil
	case pointer != fromReport:
		return "", fmt.Errorf(".last-upgrade-backup (%s) and the newest update-report.json (%s) disagree about the current rollback point",
			pointer, fromReport)
	}
	return pointer, nil
}

// backupFromNewestReport mirrors readLatestReport (route/v1/update.go): the
// newest update-report.json by mtime, and the backup_location it names. That
// is the same file PostUpdateRollback restores from, so this is identity, not
// a heuristic about names or dates.
func backupFromNewestReport(backupsDir string) string {
	matches, err := filepath.Glob(filepath.Join(backupsDir, "upgrade-*", "update-report.json"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, m := range matches {
		st, err := lstatNoFollow(m)
		if err != nil || !isRegular(&st) {
			continue
		}
		if mt := mtimeOf(&st); mt.After(newestTime) {
			newestTime, newest = mt, m
		}
	}
	if newest == "" {
		return ""
	}
	raw, err := os.ReadFile(newest)
	if err != nil {
		return ""
	}
	var rep struct {
		BackupLocation string `json:"backup_location"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return ""
	}
	loc := strings.TrimSpace(rep.BackupLocation)
	if loc == "" {
		// A report with no backup_location cannot vouch for anything; fall
		// back to the directory the report itself lives in, which IS the
		// backup by construction.
		return filepath.Dir(newest)
	}
	return filepath.Clean(loc)
}
