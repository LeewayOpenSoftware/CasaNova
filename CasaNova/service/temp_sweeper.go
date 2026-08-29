package service

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

const (
	// maxSweepDepth is how many levels below each root we still look at.
	//
	// Our leftovers sit right next to the directory the user was uploading
	// into: PostFileUpload builds `<target>/.temp/<hash><chunks>/…`, and the
	// atomic-copy stub is `<destination>.casanova-tmp`. A target the user
	// picked in the file manager is a handful of levels below /DATA or below
	// a disk's mount root — it is never buried inside a photo library. The
	// unbounded walk this replaces charged every boot a full recursive stat
	// of every file on the box, which on a NAS with real disks is minutes.
	maxSweepDepth = 6

	// sweepBudget caps the BOOT sweep. That one is a best-effort janitor, not a
	// correctness-critical step: a box slow enough to blow through the budget
	// is better off leaving the leftovers than grinding its I/O for the first
	// ten minutes of every boot. Anything missed is retried on the next start.
	//
	// An on-demand run (SweepScratch with Budget 0) deliberately has no such
	// cap: the user asked for it and is watching the answer, so a slow disk
	// should make them wait rather than silently hand back a partial report.
	sweepBudget = 2 * time.Minute

	// scratchMinAge is the safety margin for chunk groups, `.temp` shells and
	// copy stubs: anything younger than this might still be live from a
	// freshly-restarted upload. 1 hour is enough to cover the time between a
	// crash and a supervisor restart.
	scratchMinAge = 1 * time.Hour

	// singleShotTmpMinAge is the margin for the v1 single-shot upload
	// leftovers. It is a day rather than an hour because, unlike a chunk group,
	// there is no registry and no second guard behind the name: the only thing
	// that distinguishes an abandoned `<dest>.upload-<UnixNano>.tmp` from one
	// that a slow client is still streaming into is how old it is. A 2 GiB
	// upload over a bad link can take hours, so the margin has to be generous
	// enough that we can never be racing a transfer that is still alive.
	singleShotTmpMinAge = 24 * time.Hour

	// copyStubSuffix mirrors pkg/utils/file.tmpSuffix, which is unexported —
	// this is the only copy of the literal outside that package, and
	// TestSingleShotAndCopyStubShapes pins the two together.
	copyStubSuffix = ".casanova-tmp"

	// uploadTmpMarker/uploadTmpExt bracket the timestamp in the name
	// PostFileUpload's single-shot branch builds (route/v1/file.go).
	uploadTmpMarker = ".upload-"
	uploadTmpExt    = ".tmp"
)

// SweepItemKind names the shapes this sweeper is allowed to remove. They are
// the shapes OUR OWN upload and copy code writes, and nothing else: the kind is
// carried in the report so the maintenance screen can tell the user which of
// our flows left the leftover behind instead of showing a bare path.
type SweepItemKind string

const (
	// SweepChunkGroup is `<target>/.temp/<md5hex><nChunks>/` — the v1 chunked
	// upload staging area.
	SweepChunkGroup SweepItemKind = "chunk-group"
	// SweepTempShell is the `<target>/.temp` directory itself, once nothing we
	// have to keep is left inside it.
	SweepTempShell SweepItemKind = "temp-shell"
	// SweepCopyStub is `<destination>.casanova-tmp` — an interrupted atomic
	// copy (pkg/utils/file.copyFileAtomic).
	SweepCopyStub SweepItemKind = "copy-stub"
	// SweepUploadTmp is `<destination>.upload-<UnixNano>.tmp` — an interrupted
	// v1 single-shot upload (route/v1/file.go PostFileUpload).
	SweepUploadTmp SweepItemKind = "upload-tmp"
)

// SweepItem is one leftover the sweep removed, or would remove on a dry run.
type SweepItem struct {
	Path    string        `json:"path"`
	Kind    SweepItemKind `json:"kind"`
	Bytes   int64         `json:"bytes"`
	ModTime time.Time     `json:"mtime"`
	// Removed is false on a dry run and on a removal that failed.
	Removed bool `json:"removed"`
	// Err is the removal failure, if any. A failed removal is reported but
	// never counted: the byte totals are what we actually reclaimed.
	Err string `json:"err,omitempty"`
}

// SweepReport is what a sweep found. On a dry run every counter reads as
// "would remove"; the guards, the cutoffs and the mount refusals are the same
// either way, so the dry run's plan is the real run's plan.
type SweepReport struct {
	Roots  []string    `json:"roots"`
	DryRun bool        `json:"dryRun"`
	Items  []SweepItem `json:"items"`
	// Dirs counts removed directories: chunk groups plus emptied `.temp`
	// shells, matching what the boot sweep has always logged.
	Dirs int `json:"dirs"`
	// Stubs counts removed `.casanova-tmp` copy stubs.
	Stubs int `json:"stubs"`
	// UploadTmps counts removed `<dest>.upload-<UnixNano>.tmp` leftovers.
	UploadTmps int `json:"uploadTmps"`
	// Failed counts entries we decided to remove but could not.
	Failed int   `json:"failed"`
	Bytes  int64 `json:"bytes"`
	// BudgetExpired says the walk stopped early, so the report is a floor and
	// not a total. The UI has to say so rather than claim "nothing else found".
	BudgetExpired bool `json:"budgetExpired"`
	// SkippedMounts are the mount points we refused to walk into (see
	// unwalkableMounts). Same honesty problem as BudgetExpired: those subtrees
	// were not examined, and the user is entitled to know which.
	SkippedMounts []string `json:"skippedMounts,omitempty"`
}

// Paths lists the item paths in report order. Convenience for callers that
// only need the plan, e.g. to hand the user a confirmation list.
func (r SweepReport) Paths() []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.Path)
	}
	return out
}

// SweepOptions configures a scratch sweep.
type SweepOptions struct {
	// Roots are the trees to walk. "/" is refused: this sweeper decides what
	// to delete from the shape of a name, and a name shape is only trustworthy
	// inside the storage areas our own upload code writes into.
	Roots []string

	// DryRun reports what a real run would remove without touching the disk.
	DryRun bool

	// MinAge lengthens the safety margin for chunk groups, `.temp` shells and
	// copy stubs. Zero means scratchMinAge, and a value below it is ignored:
	// see atLeast.
	MinAge time.Duration

	// SingleShotMinAge lengthens the margin for the v1 single-shot upload
	// leftovers. Zero means singleShotTmpMinAge; shorter is ignored.
	SingleShotMinAge time.Duration

	// Budget stops the walk after this long. Zero means no limit.
	Budget time.Duration

	// MaxDepth overrides maxSweepDepth. Zero means the default.
	MaxDepth int

	// Only restricts removal to this exact set of paths, as reported by an
	// earlier dry run. Nil means "everything the guards allow". It is how a
	// caller honours a per-item confirmation: a leftover that appeared after
	// the plan was shown to the user is skipped entirely and waits for the
	// next run, so the user can never approve one list and have us delete
	// another.
	Only map[string]bool
}

// SweepOrphanTempDirs walks the configured data roots at startup and deletes
// the abandoned upload chunk groups under `.temp` (see sweepTempScratch), the
// `.casanova-tmp` leftovers of an interrupted atomic copy, and the
// `<dest>.upload-<UnixNano>.tmp` files an interrupted single-shot upload
// leaves behind.
//
// Before this sweep existed, a crash mid-upload would orphan a .temp/
// directory forever — the in-process cleanup goroutine (which ran 11
// seconds after each successful splice) would never fire. Users would
// accumulate GBs of abandoned upload fragments.
//
// Called once from main() during service startup. On-demand callers (the
// maintenance module) use SweepScratch, which is the same walk with the same
// guards and no time budget.
func SweepOrphanTempDirs(roots []string) {
	SweepScratch(SweepOptions{Roots: roots, Budget: sweepBudget})
}

// SweepScratch is the on-demand entry point: the identical walk with the
// identical guards, plus a dry-run mode and a report of what it touched.
//
// It is deliberately the SAME code path as the boot sweep rather than a second
// implementation with its own thresholds — the guards below are what stand
// between a RemoveAll and somebody's photo library, and a copy of them would
// drift.
func SweepScratch(opts SweepOptions) SweepReport {
	now := time.Now()
	run := &sweepRun{
		opts:      opts,
		cutoff:    now.Add(-atLeast(opts.MinAge, scratchMinAge)),
		tmpCutoff: now.Add(-atLeast(opts.SingleShotMinAge, singleShotTmpMinAge)),
		blocked:   unwalkableMounts(),
		seen:      map[string]bool{},
		rep:       SweepReport{DryRun: opts.DryRun},
	}
	if opts.Budget > 0 {
		run.deadline = now.Add(opts.Budget)
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = maxSweepDepth
	}

	// Accept the roots first, so the walk can recognise where one root's
	// subtree ends and another's begins.
	rootSet := map[string]bool{}
	for _, root := range opts.Roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		// A root of "/" (or a relative path, which WalkDir would resolve
		// against whatever the process CWD happens to be) would put a
		// name-shape-driven RemoveAll in charge of the whole box. The boot list
		// and the maintenance catalog both name real storage roots.
		if root == "/" || !filepath.IsAbs(root) {
			logger.Error("refusing to sweep an unsafe root", zap.String("root", root))
			continue
		}
		if rootSet[root] {
			continue
		}
		rootSet[root] = true
		run.rep.Roots = append(run.rep.Roots, root)
	}

	for _, root := range run.rep.Roots {
		// Depth is counted in separators so the check costs no syscall.
		rootDepth := strings.Count(root, string(os.PathSeparator))

		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable nodes, don't abort
			}
			if run.expired() {
				run.rep.BudgetExpired = true
				return filepath.SkipAll
			}

			if d.IsDir() {
				// Never step into a share we cannot trust to answer — see
				// unwalkableMounts. /mnt and /media are themselves mount
				// parents, so we cannot just refuse to leave the root's
				// filesystem the way `find -xdev` would: that would stop the
				// sweep from ever reaching an external disk.
				if p != root && run.blocked[p] {
					run.skipMount(p)
					return filepath.SkipDir
				}

				// Another root owns this subtree: stop here and let it be
				// walked from its own root, with its own depth budget. On this
				// box /mnt holds two ext4 disks; folding them into /mnt's
				// budget would leave anything six levels inside a disk
				// unreachable, and walking them twice would be pure waste.
				if p != root && rootSet[p] {
					return filepath.SkipDir
				}

				// `.temp` upload scratch directories (chunk storage).
				if d.Name() == ".temp" {
					run.sweepTempScratch(p)
					// Don't descend either way: sweepTempScratch already
					// inspected everything under here that we may touch, and
					// WalkDir must not report entries it just deleted.
					return filepath.SkipDir
				}

				if strings.Count(p, string(os.PathSeparator))-rootDepth >= maxDepth {
					return filepath.SkipDir
				}
				return nil
			}

			switch {
			// `.casanova-tmp` stub files left by a crashed atomic copy.
			case strings.HasSuffix(d.Name(), copyStubSuffix):
				run.considerStub(p, d, SweepCopyStub, run.cutoff)
			// `<dest>.upload-<UnixNano>.tmp` left by a crashed single-shot
			// upload. Nothing used to clean these: the sweeper only knew
			// `.temp` and `.casanova-tmp`, and the v2 registry's
			// isUploadTmpPath demands exactly 8 hex characters, which a
			// 19-digit timestamp can never match. That made it an unbounded,
			// permanent leak.
			case isSingleShotUploadTmpName(d.Name()):
				run.considerStub(p, d, SweepUploadTmp, run.tmpCutoff)
			}
			return nil
		})

		if run.rep.BudgetExpired {
			break
		}
	}

	sort.Strings(run.rep.SkippedMounts)
	if !opts.DryRun && (run.rep.Dirs > 0 || run.rep.Stubs > 0 || run.rep.UploadTmps > 0) {
		logger.Info("orphan temp sweep complete",
			zap.Int("temp_dirs", run.rep.Dirs),
			zap.Int("tmp_stubs", run.rep.Stubs),
			zap.Int("upload_tmps", run.rep.UploadTmps),
			zap.Int64("bytes_reclaimed", run.rep.Bytes))
	}
	if run.rep.BudgetExpired {
		logger.Info("orphan temp sweep hit its time budget, remaining leftovers wait for the next start",
			zap.Duration("budget", opts.Budget))
	}
	return run.rep
}

// sweepRun carries the decisions of one sweep. It exists so the boot sweep and
// an on-demand run share every guard and differ only in this struct's fields.
type sweepRun struct {
	opts      SweepOptions
	cutoff    time.Time // chunk groups, `.temp` shells, copy stubs
	tmpCutoff time.Time // single-shot upload leftovers
	deadline  time.Time // zero means no budget
	blocked   map[string]bool
	// seen dedupes by path so overlapping roots (say /DATA and a disk mounted
	// under it) cannot count the same leftover twice. A report that
	// double-counts bytes is a report that lies about how much a run will
	// free, and rule 14 of the maintenance design is that we do not promise a
	// number we cannot deliver.
	seen map[string]bool
	rep  SweepReport
}

func (r *sweepRun) expired() bool {
	return !r.deadline.IsZero() && time.Now().After(r.deadline)
}

// allowed applies the caller's per-item confirmation, if there is one.
func (r *sweepRun) allowed(path string) bool {
	return r.opts.Only == nil || r.opts.Only[path]
}

func (r *sweepRun) skipMount(path string) {
	if r.seen["mount:"+path] {
		return
	}
	r.seen["mount:"+path] = true
	r.rep.SkippedMounts = append(r.rep.SkippedMounts, path)
}

// record adds an item to the report and updates the counters. A path already
// recorded is ignored, and a failed removal is listed but never counted.
func (r *sweepRun) record(item SweepItem) {
	if r.seen[item.Path] {
		return
	}
	r.seen[item.Path] = true
	r.rep.Items = append(r.rep.Items, item)
	if item.Err != "" {
		r.rep.Failed++
		return
	}
	switch item.Kind {
	case SweepChunkGroup, SweepTempShell:
		r.rep.Dirs++
	case SweepCopyStub:
		r.rep.Stubs++
	case SweepUploadTmp:
		r.rep.UploadTmps++
	}
	r.rep.Bytes += item.Bytes
}

// considerStub handles the two single-FILE shapes: the atomic-copy stub and the
// single-shot upload leftover.
//
// DirEntry.Info is an lstat, so a symlink or a FIFO wearing one of our suffixes
// shows up as neither regular nor stale-by-our-clock and is left alone. That
// matters: removing a link named `holiday.mkv.casanova-tmp` would destroy a
// name the user can see in the file manager, and we have no trash behind this.
func (r *sweepRun) considerStub(path string, d fs.DirEntry, kind SweepItemKind, cutoff time.Time) {
	info, err := d.Info()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if !info.ModTime().Before(cutoff) {
		return
	}
	if !r.allowed(path) {
		return
	}
	item := SweepItem{Path: path, Kind: kind, Bytes: info.Size(), ModTime: info.ModTime()}
	if !r.opts.DryRun {
		// Revalidate immediately before the unlink: the entry above came from
		// the directory read, which is already in the past, and /DATA is
		// writable over SMB. os.SameFile compares (st_dev, st_ino), so an
		// entry swapped for a link or replaced by a fresh upload since then
		// fails here and stays.
		if cur, cerr := os.Lstat(path); cerr != nil || !cur.Mode().IsRegular() || !os.SameFile(cur, info) {
			return
		}
		if rmErr := os.Remove(path); rmErr != nil {
			item.Err = rmErr.Error()
			r.record(item)
			return
		}
		item.Removed = true
	}
	r.record(item)
}

// remoteFSTypes are the filesystem types the sweeper refuses to walk into.
//
// A healthy CIFS/NFS share answers a stat over the wire in milliseconds and a
// dead one never answers at all, so recursing into a mounted share at boot
// costs minutes of round-trips at best and wedges the sweep at worst. Userspace
// mounts (rclone, sshfs, gvfs — everything that shows up as `fuse.<backend>`)
// behave the same way. `fuseblk` is deliberately absent: that is ntfs-3g and
// friends, i.e. a plain external disk we do want to sweep. autofs is here for a
// different reason — touching an autofs path is what *triggers* the mount.
var remoteFSTypes = map[string]bool{
	"9p": true, "afs": true, "afpfs": true, "autofs": true, "beegfs": true,
	"ceph": true, "cifs": true, "coda": true, "davfs": true, "davfs2": true,
	"fuse": true, "glusterfs": true, "lustre": true, "ncpfs": true,
	"nfs": true, "nfs4": true, "orangefs": true, "smb2": true, "smb3": true,
	"smbfs": true,
}

// localFUSETypes are the `fuse.<backend>` mounts that are backed by a local
// disk and therefore stay sweepable, for the same reason `fuseblk` is not in
// remoteFSTypes: nothing goes over a wire, so a stat cannot hang.
//
// `fuse.mergerfs` is the one that matters. A merged pool is not somebody's
// remote share, it is OUR combined-storage feature (CasaNova-LocalStorage
// pkg/mergerfs, /v2/local_storage/merged) and the API lets the user park the
// mount point anywhere — /mnt/pool, /media/pool. Blanket-blocking `fuse.*`
// meant the user's PRIMARY storage was the one place we never swept, so the
// abandoned upload chunks this janitor exists to reclaim piled up there
// forever. Unknown FUSE backends stay blocked by default: a wire-backed one we
// have not heard of (juicefs, gcsfuse, a new rclone alias) hangs the boot
// sweep, and that failure is worse than leaving leftovers behind.
var localFUSETypes = map[string]bool{
	"fuse.mergerfs": true,
	"fuse.ntfs-3g":  true, "fuse.ntfs": true, "fuse.ntfs3": true,
	"fuse.exfat": true, "fuse.exfat-fuse": true,
	"fuse.ext4fuse": true, "fuse.lklfuse": true,
	"fuse.hfsfuse": true, "fuse.apfs-fuse": true,
}

// mountEntry is one line of /proc/self/mountinfo reduced to what we use.
type mountEntry struct {
	point  string
	fsType string
}

// unwalkableMounts returns the set of mount points SweepOrphanTempDirs must
// not descend into. It reads /proc/self/mountinfo rather than calling
// statfs(2) per directory: procfs is a memory read that cannot block, while
// statfs on a dead NFS mount is exactly the hang we are trying to avoid.
func unwalkableMounts() map[string]bool {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseUnwalkableMounts(string(raw))
}

// parseUnwalkableMounts keeps the remote mount points out of parseMountInfo's
// result.
func parseUnwalkableMounts(raw string) map[string]bool {
	blocked := map[string]bool{}
	for _, m := range parseMountInfo(raw) {
		if isRemoteFSType(m.fsType) {
			blocked[m.point] = true
		}
	}
	return blocked
}

// parseMountInfo pulls the mount points out of mountinfo's format:
// `id parent major:minor root mountpoint options… - fstype source …`.
// The variable-length optional fields before the "-" separator are why the
// fstype has to be located instead of indexed.
func parseMountInfo(raw string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		out = append(out, mountEntry{
			point:  filepath.Clean(unescapeMountPath(fields[4])),
			fsType: fields[sep+1],
		})
	}
	return out
}

func isRemoteFSType(fstype string) bool {
	if strings.HasPrefix(fstype, "fuse.") {
		return !localFUSETypes[fstype]
	}
	return remoteFSTypes[fstype]
}

// LocalSweepRoots turns a list of storage roots into the roots an on-demand
// sweep should actually walk: the ones that exist, minus anything mounted from
// the network, plus the local mount points nested inside them.
//
// This is the "real mount points" half of what makes an on-demand run better
// than the boot one. The boot sweep is handed three fixed strings and walks
// whatever is there; here we ask the kernel what is actually mounted, so a disk
// or a mergerfs pool the user parked under one of those roots becomes a root of
// its own — its own depth budget, and named in the report so the user can see
// what was examined. On this box that is /mnt/UNIT5 and /mnt/UNIT1a_UNIT, two
// ext4 disks whose contents would otherwise have to fit inside /mnt's six
// levels. SweepScratch walks each subtree exactly once (the parent root stops
// at the child root's boundary), so nesting costs nothing.
//
// It never widens beyond what it was given. A mount point outside the caller's
// roots is not added, because deciding that some other mount is ours to sweep
// is a product decision this function has no business taking.
func LocalSweepRoots(base []string) []string {
	blocked := unwalkableMounts()

	var roots []string
	for _, b := range base {
		if b == "" {
			continue
		}
		b = filepath.Clean(b)
		if !filepath.IsAbs(b) || b == "/" || blocked[b] {
			continue
		}
		// Lstat only after the mountinfo check: stat'ing a dead NFS mount is
		// the exact hang unwalkableMounts exists to avoid.
		if st, err := os.Lstat(b); err != nil || !st.IsDir() {
			continue
		}
		roots = append(roots, b)
	}

	if raw, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, m := range parseMountInfo(string(raw)) {
			if isRemoteFSType(m.fsType) || !underAny(m.point, roots) {
				continue
			}
			roots = append(roots, m.point)
		}
	}

	// Exact duplicates only — a base root that is itself a mount point, or a
	// caller that listed the same directory twice. Nested entries are kept on
	// purpose (see above).
	sort.Strings(roots)
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if len(out) > 0 && out[len(out)-1] == r {
			continue
		}
		out = append(out, r)
	}
	return out
}

// underAny reports whether path sits inside one of the given directories.
// Boundary-safe (exact match or prefix + "/") so "/media2" is not treated as
// living under "/media".
func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if path == d || strings.HasPrefix(path, d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// atLeast is why MinAge and SingleShotMinAge are one-way options: they may
// lengthen a safety margin, never shorten it. Those margins are the only thing
// standing between this sweeper and an upload that is still streaming, and a
// caller reaching for a more aggressive threshold is describing a bug, not a
// configuration. Zero therefore means "the default", not "no margin".
func atLeast(v, floor time.Duration) time.Duration {
	if v > floor {
		return v
	}
	return floor
}

// unescapeMountPath decodes the octal escapes mountinfo uses for space, tab,
// newline and backslash. Without this a share mounted at `/media/My Disk`
// would never match the path WalkDir hands us, and we would walk into it.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, perr := strconv.ParseUint(s[i+1:i+4], 8, 8); perr == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dirBytes returns the total size of a directory tree in bytes. Best-effort.
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// sweepTempScratch cleans one `.temp` directory and records what it removed.
//
// The chunk-shape test has to be applied one level DOWN. PostFileUpload writes
// `<target>/.temp/<hash><totalChunks>/…`, so a `.temp` that holds anything at
// all holds a directory. The first version of this sweeper asked whether
// `.temp` itself contained nothing but numeric chunk files — true only for the
// empty leftover — and then skipped the subtree, so it never reached a single
// real chunk group and reclaimed nothing. Fragments kept piling up exactly as
// they did before the sweeper existed.
//
// The per-group decision also replaces the old check on `.temp`'s own mtime:
// creating a group bumps the parent's timestamp, so one upload started today
// used to shield months-old orphans sitting next to it.
func (r *sweepRun) sweepTempScratch(path string) {
	// Read `.temp`'s age BEFORE we delete anything under it — removing a
	// group rewrites the parent's mtime, and we still want to know whether
	// the shell itself was stale when we got here.
	shell, serr := os.Stat(path)
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	remaining := len(entries)

	for _, e := range entries {
		// A loose file directly under `.temp` is not something we wrote, so
		// leave it — and leaving it also keeps `.temp` itself.
		if !e.IsDir() {
			continue
		}
		group := filepath.Join(path, e.Name())
		if !isChunkGroup(group, r.cutoff) {
			continue
		}
		// An item the user did not tick keeps `.temp` alive too: remaining
		// stays above zero, which is the honest outcome — the shell is still
		// in use as far as this run is concerned.
		if !r.allowed(group) {
			continue
		}
		item := SweepItem{Path: group, Kind: SweepChunkGroup, Bytes: dirBytes(group)}
		if info, ierr := os.Lstat(group); ierr == nil {
			item.ModTime = info.ModTime()
		}
		if !r.opts.DryRun {
			if rmErr := os.RemoveAll(group); rmErr != nil {
				logger.Error("failed to sweep orphan upload chunks",
					zap.String("path", group),
					zap.Error(rmErr))
				item.Err = rmErr.Error()
				r.record(item)
				continue
			}
			item.Removed = true
			logger.Info("swept orphan upload chunks",
				zap.String("path", group),
				zap.Int64("bytes", item.Bytes))
		}
		remaining--
		r.record(item)
	}

	// Drop the `.temp` shell only once nothing is left in it and it was
	// already outside the safety margin — an empty `.temp` that appeared in
	// the last hour may belong to an upload that is just getting started.
	if remaining != 0 || serr != nil || !shell.ModTime().Before(r.cutoff) || !r.allowed(path) {
		return
	}
	item := SweepItem{Path: path, Kind: SweepTempShell, ModTime: shell.ModTime()}
	if r.opts.DryRun {
		r.record(item)
		return
	}
	if rmErr := os.Remove(path); rmErr == nil {
		item.Removed = true
		r.record(item)
	}
}

// isChunkGroup reports whether dir is one of our abandoned per-upload chunk
// groups: `<target>/.temp/<hash><totalChunks>` holding the numeric chunk files
// PostFileUpload splices, with nothing in it newer than the safety margin.
//
// Two independent guards, because there is no trash for this path and a
// RemoveAll here is final. First the name: our groups are an md5 hex digest
// with the chunk count appended, which no directory a human created looks
// like. Then the contents: only numeric chunk files, plus the plain
// subdirectories a folder upload mirrors from the source tree (relativePath
// with a dirPath makes the chunks land in `<group>/<subdir>/<n>`). Any
// non-numeric file, symlink or unreadable entry means we are staring at
// somebody's data and we keep our hands off the whole group.
func isChunkGroup(dir string, cutoff time.Time) bool {
	if !isChunkGroupName(filepath.Base(dir)) {
		return false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.Mode().IsDir() || !info.ModTime().Before(cutoff) {
		return false
	}
	return chunksAllStale(dir, cutoff, 0)
}

// maxChunkGroupDepth bounds the recursion into a group. A folder upload nests
// as deep as the tree the user dragged in; past this we decline instead of
// walking, which only costs us the reclaim.
const maxChunkGroupDepth = 8

// chunksAllStale verifies the shape and the age of a group's contents. The age
// is checked on every entry rather than on the group directory alone, because
// writing `<group>/<subdir>/3` touches `<subdir>`, not the group: an upload
// that is still streaming into a nested folder must never be swept out from
// under the client.
func chunksAllStale(dir string, cutoff time.Time, depth int) bool {
	if depth > maxChunkGroupDepth {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		// DirEntry.Info does not follow symlinks, so a planted link shows up
		// as neither a directory nor a regular file and lands in default.
		info, ierr := e.Info()
		if ierr != nil {
			return false
		}
		if !info.ModTime().Before(cutoff) {
			return false
		}
		switch {
		case info.Mode().IsDir():
			if !chunksAllStale(filepath.Join(dir, e.Name()), cutoff, depth+1) {
				return false
			}
		case info.Mode().IsRegular():
			if !isNumericName(e.Name()) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isChunkGroupName matches `<md5 hex><totalChunks>`: hex digits only, and long
// enough to carry a 32-character digest plus at least one digit of chunk
// count. Length and alphabet are checked instead of splitting the two halves
// apart, since the digest's own trailing digits make the boundary ambiguous.
func isChunkGroupName(name string) bool {
	if len(name) < 33 {
		return false
	}
	for _, r := range name {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// SingleShotUploadTmpPath builds the sidecar PostFileUpload's single-shot
// branch writes to before renaming it over the destination.
//
// It lives here, next to the matcher, so the two cannot drift — they already
// did once, and the result was a leftover with no owner: the sweeper knew only
// `.temp` and `.casanova-tmp`, and the v2 registry's isUploadTmpPath demands
// exactly 8 hex characters where this name carries a 19-digit timestamp, so
// nothing on the box ever deleted one. A leak with no ceiling.
func SingleShotUploadTmpPath(dest string) string {
	return dest + uploadTmpMarker + strconv.FormatInt(time.Now().UnixNano(), 10) + uploadTmpExt
}

// isSingleShotUploadTmpName matches `<destination>.upload-<UnixNano>.tmp`, the
// sidecar PostFileUpload's single-shot branch writes before renaming it over
// the destination (route/v1/file.go). The handler removes it on every error
// path it can see; what it cannot see is the process dying mid-copy, and that
// is the leftover this matches.
//
// The digit run is what separates it from the v2 session tmp, which is
// `<destination>.upload-<8 hex>.tmp` and belongs to the upload registry, not to
// us: a UnixNano is 19 digits today and cannot be 8 characters long until the
// year 2286. The 16-20 window is the same one the maintenance design specifies
// — wide enough not to break on a clock set to 1973 or on a distant future,
// narrow enough that the two shapes can never overlap.
//
// A name that is ONLY the marker (no destination in front) is rejected: our
// producer always prefixes the file it is replacing.
func isSingleShotUploadTmpName(name string) bool {
	if !strings.HasSuffix(name, uploadTmpExt) {
		return false
	}
	base := strings.TrimSuffix(name, uploadTmpExt)
	i := strings.LastIndex(base, uploadTmpMarker)
	if i <= 0 {
		return false
	}
	digits := base[i+len(uploadTmpMarker):]
	if len(digits) < 16 || len(digits) > 20 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isNumericName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
