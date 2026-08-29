package v1

// Archive operations — compress a selection to a new archive on disk,
// and extract an existing archive in place.
//
//   POST /v1/file/compress  { paths: [...], destination: "/DATA/out.zip", format: "zip"|"targz" }
//   POST /v1/file/extract   { path: "/DATA/foo.zip", destination: "/DATA/foo" }
//
// Both are synchronous for now (the UI shows a spinner). A future iteration
// moves these onto the batch-task queue alongside copy/move for live progress.

import (
	"archive/tar"
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/labstack/echo/v4"
	"github.com/mholt/archiver/v3"
)

// archiveTimeout is the wall-clock cap on COMPRESS. archiver.Archive takes no
// context and cannot be interrupted, so the handler still answers on time and
// the abandoned worker is cleaned up after it finishes (see PostFileCompress).
// Compress output is bounded by the real files on disk, so there is no bomb to
// contain here — only a caller that would otherwise wait forever.
const archiveTimeout = 15 * time.Minute

// archiveStallTimeout bounds EXTRACT, and it is deliberately a *stall* budget,
// not a wall clock. Extraction is now genuinely cancelable, which means a
// wall-clock cap would abort a perfectly legitimate multi-hour restore of a
// large backup — the previous code only *appeared* to survive that because its
// timeout abandoned the worker instead of stopping it (the extraction finished
// anyway, the user just got a 504). Cancelling on "no bytes moved for 15
// minutes" keeps honest long extractions alive and still bounds a hung read.
// Size is bounded separately, by free space, which is what stops a bomb.
const archiveStallTimeout = 15 * time.Minute

// maxArchiveEntries caps how many members one extraction may create. A few
// hundred bytes of zip can declare millions of empty entries, which exhausts
// inodes and directory space without ever tripping a byte budget. One million
// is far above any real payload we have seen (a full Linux rootfs tar is
// ~100k entries, a large photo library ~200k) so no legitimate restore is hit.
const maxArchiveEntries = 1_000_000

// archiveSpaceReserve is the most free space an extraction leaves untouched.
// The byte budget is derived from the destination filesystem rather than being
// a fixed number precisely because a fixed number is either too small for a
// legitimate 50 GB backup or too large to protect a small disk. The reserve is
// 5% of what is free, capped here, so a nearly-empty disk still permits small
// extractions while a bomb is stopped before it takes the last byte.
const archiveSpaceReserve = 1 << 30 // 1 GiB

type compressRequest struct {
	Paths       []string `json:"paths"`
	Destination string   `json:"destination"`
	Format      string   `json:"format"` // zip | targz
}

func PostFileCompress(ctx echo.Context) error {
	// Writing a new archive to disk is a write op; a read-only operator role
	// must not be able to create files (or, via the source read, stage a
	// copy of anything readable into a downloadable archive).
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	var req compressRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if len(req.Paths) == 0 || req.Destination == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "paths and destination are required",
		})
	}

	// Validate every source path + the destination through the same
	// traversal guard the download handler uses.
	srcs := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		clean, errResp := validatePath(ctx, p)
		if errResp != nil {
			return errResp
		}
		if !file.Exists(clean) {
			return ctx.JSON(http.StatusNotFound, model.Result{
				Success: common_err.FILE_DOES_NOT_EXIST,
				Message: clean + " does not exist",
			})
		}
		srcs = append(srcs, clean)
	}
	dest, errResp := validatePath(ctx, req.Destination)
	if errResp != nil {
		return errResp
	}
	if file.Exists(dest) {
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "destination already exists",
		})
	}

	var arch archiver.Archiver
	switch strings.ToLower(req.Format) {
	case "", "zip":
		z := archiver.NewZip()
		// The archive is built at a private temp path that we create first,
		// so archiver has to be allowed to write over that (empty) file.
		z.OverwriteExisting = true
		arch = z
		if !strings.HasSuffix(dest, ".zip") {
			dest += ".zip"
		}
	case "targz", "tar.gz":
		tg := archiver.NewTarGz()
		tg.OverwriteExisting = true
		arch = tg
		if !strings.HasSuffix(dest, ".tar.gz") {
			dest += ".tar.gz"
		}
	default:
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "format must be zip or targz",
		})
	}

	// SECURITY / correctness: build into a private temp file in the SAME
	// directory (same filesystem, so the final rename is atomic) instead of
	// writing straight to the user-visible destination. archiver.Archive has
	// no context, so a timeout can only abandon the worker — and the old code
	// abandoned it while deleting `dest`, after which the still-running worker
	// re-created `dest` behind our back. The user was told the job failed and
	// was left with a file that looked complete. Now the abandoned worker can
	// only ever touch the temp path, which a detached reaper removes once the
	// worker actually stops, and `dest` never appears unless the archive is
	// whole.
	tmpFile, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".part-*")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "compress failed: " + err.Error(),
		})
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	cmdCtx, cancel := context.WithTimeout(context.Background(), archiveTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- arch.Archive(srcs, tmpPath) }()
	select {
	case err := <-done:
		if err != nil {
			_ = os.Remove(tmpPath)
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "compress failed: " + err.Error(),
			})
		}
	case <-cmdCtx.Done():
		// Reap the temp file only after the worker has let go of it.
		go func() { <-done; _ = os.Remove(tmpPath) }()
		return ctx.JSON(http.StatusGatewayTimeout, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("compress timed out after %s", archiveTimeout),
		})
	}

	if file.Exists(dest) {
		// Something claimed the name while we were packing; os.Rename would
		// silently clobber it, which the pre-flight check above promised not
		// to do.
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "destination already exists",
		})
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "compress failed: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]string{"archive": dest},
	})
}

type extractRequest struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
}

func PostFileExtract(ctx echo.Context) error {
	// Extraction writes files to disk — gate it on file-write like the rest
	// of the mutating file API (a non-admin user could otherwise reach it,
	// and the per-entry refusal below is the second layer of defense).
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	var req extractRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	src, errResp := validatePath(ctx, req.Path)
	if errResp != nil {
		return errResp
	}
	if !file.Exists(src) {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
		})
	}
	// Default destination: a sibling folder named after the archive
	// minus its extension — matches what every desktop file manager does.
	dest := req.Destination
	if dest == "" {
		base := filepath.Base(src)
		for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".tar", ".tgz", ".rar", ".7z"} {
			if strings.HasSuffix(strings.ToLower(base), ext) {
				base = base[:len(base)-len(ext)]
				break
			}
		}
		dest = filepath.Join(filepath.Dir(src), base)
	}
	cleanDest, errResp := validatePath(ctx, dest)
	if errResp != nil {
		return errResp
	}

	budget, err := archiveSpaceBudget(cleanDest)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "extract failed: cannot measure free space at the destination: " + err.Error(),
		})
	}

	status, err := extractArchive(context.Background(), src, cleanDest, archiveLimits{
		MaxBytes:   budget,
		MaxEntries: maxArchiveEntries,
		Stall:      archiveStallTimeout,
	})
	if err != nil {
		code := common_err.SERVICE_ERROR
		if status == http.StatusBadRequest {
			code = common_err.INVALID_PARAMS
		}
		return ctx.JSON(status, model.Result{
			Success: code,
			Message: "extract failed: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]string{"destination": cleanDest},
	})
}

// archiveLimits bounds one extraction.
type archiveLimits struct {
	MaxBytes   int64         // total uncompressed bytes that may be written
	MaxEntries int           // total members that may be created
	Stall      time.Duration // abort if no bytes move for this long (0 = never)
}

// extractArchive expands src into dest, member by member, and is the whole of
// the extraction path. It returns the HTTP status that fits the failure, and
// (0, nil) on success.
//
// It replaces "pre-scan the archive with archiver.Walk, then hand the same
// PATH to archiver.Unarchive", which was broken in three ways:
//
//   - TOCTOU (PT-03): the guard read the file and Unarchive re-opened it.
//     Anything with write access to /DATA — including any container — could
//     swap the file between the two reads, so the symlink/traversal refusal
//     was applied to bytes that were never extracted. Walking once means the
//     bytes we validate are, by construction, the bytes we write: a single
//     open, and each member is inspected and then written from that same
//     stream before the next header is read.
//   - unbounded output (CN-DOS-05): Unarchive expands whatever the headers
//     declare, so a few KB of zip could fill a 5.4 TB array.
//   - uncancelable (CN-DOS-05): Unarchive is all-or-nothing and takes no
//     context, so the deadline abandoned the goroutine and it kept extracting.
//     Copying per entry through a context-checked reader makes the deadline
//     real, and lets the handler run it inline with no orphan worker at all.
//
// Failure policy: a rejection removes the partial output ONLY when that output
// is the harm — an unsafe archive, or a blown byte/entry budget. A stall, an
// I/O error or a truncated archive keeps what already landed, so an interrupted
// restore is resumable instead of being deleted from under the user.
func extractArchive(parent context.Context, src, dest string, lim archiveLimits) (int, error) {
	// Whether we own the destination decides whether we may roll back.
	destExisted := file.Exists(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("creating destination: %w", err)
	}

	ctx, touch, stop := newStallGuard(parent, lim.Stall)
	defer stop()

	var (
		entries   int
		declared  int64
		written   int64
		reclaimed int64
		refusal   error
		status    int
		discard   bool
	)
	// archiver.Walk wraps a returned error with %v, which destroys errors.Is,
	// so the reason is carried out of band and the walk is stopped cleanly.
	//
	// refuse stops the walk and KEEPS whatever already landed. That is the
	// default because the previous shape rolled back on *every* rejection: a
	// read stall on a failing USB or mechanical disk 40 minutes into a restore
	// answered 504 and then deleted every byte extracted so far, which is
	// strictly worse than the bug it replaced (the old code merely abandoned
	// the worker and the partial tree survived). Everything already on disk
	// passed the per-entry link/traversal/special-bit refusal and was written
	// with O_NOFOLLOW, so keeping it is safe — it is incomplete, not hostile,
	// and the user can resume or delete it.
	refuse := func(code int, err error) error {
		status, refusal = code, err
		return archiver.ErrStopWalk
	}
	// refuseAndDiscard is refuse for the rejections where the partial output IS
	// the damage: an unsafe archive (we do not want half of a hostile payload
	// materialised) and a blown byte/entry budget (leaving the bytes behind
	// would leave the disk or the inode table as full as the bomb wanted it).
	// Rolling those back is what makes the budget a recovery and not just a
	// stop.
	refuseAndDiscard := func(code int, err error) error {
		discard = true
		return refuse(code, err)
	}
	// stalled says what the user has to know to act: the work stopped, and the
	// partial tree is still there. Silently keeping it would be as confusing as
	// silently deleting it was destructive.
	stalled := func() error {
		return refuse(http.StatusGatewayTimeout, fmt.Errorf(
			"extraction made no progress for %s — what was already extracted has been kept at %s; extract again to resume over it, or delete it",
			lim.Stall, dest))
	}
	overBudget := func(net int64) error {
		if net < 0 {
			net = 0
		}
		return refuseAndDiscard(http.StatusBadRequest, fmt.Errorf(
			"archive expands to more than %s of new data, which is the space free at the destination minus a small reserve (%s already written; bytes reclaimed by replacing files of the same name in the destination are credited back) — free up space or extract to another disk",
			archiveHumanBytes(lim.MaxBytes), archiveHumanBytes(net)))
	}

	walkErr := archiver.Walk(src, func(f archiver.File) error {
		// Many small members are progress too, not just bytes copied.
		touch()
		if ctx.Err() != nil {
			return stalled()
		}

		entries++
		if entries > lim.MaxEntries {
			return refuseAndDiscard(http.StatusBadRequest, fmt.Errorf(
				"archive declares more than %d entries", lim.MaxEntries))
		}

		name := archiveEntryName(f)
		skip, err := checkUnsafeArchiveEntry(f, name)
		if err != nil {
			return refuseAndDiscard(http.StatusBadRequest, fmt.Errorf("refusing unsafe archive: %w", err))
		}
		if skip {
			return nil
		}

		target := filepath.Join(dest, name)
		// Belt and braces after the lexical check: the path we are about to
		// open must still be under dest once Join has cleaned it.
		if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
			return refuseAndDiscard(http.StatusBadRequest, fmt.Errorf(
				"refusing unsafe archive: entry escapes the destination (%s)", name))
		}

		if f.IsDir() {
			if err := os.MkdirAll(target, f.Mode().Perm()); err != nil {
				return refuse(http.StatusInternalServerError, err)
			}
			return nil
		}

		// The budget is free space, so what it has to bound is NET NEW bytes,
		// not bytes written. writeArchiveEntry opens with O_TRUNC, so a member
		// landing on top of a file of the same name hands the old copy's blocks
		// back to the filesystem as the new copy lands. Counting gross writes
		// made re-extracting a 50 GB backup into the folder that already held
		// it fail with "expands to more than 39 GiB" on a NAS with 40 GB free,
		// even though the operation needs no new space at all.
		//
		// The credit is taken here, immediately before the write, and the only
		// way out of this callback that does NOT truncate target is a refusal
		// that ends the walk — so a credited byte is always a byte the O_TRUNC
		// below really did release.
		reclaimed += archiveReclaimableBytes(target)

		// The declared size is checked first so a bomb that is honest in its
		// headers is refused before a single byte of it reaches the disk.
		if size := f.Size(); size > 0 {
			declared += size
			if declared-reclaimed > lim.MaxBytes {
				return overBudget(written - reclaimed)
			}
		}
		// ...and the copy is capped anyway, because a header may lie.
		remaining := lim.MaxBytes - (written - reclaimed)
		if remaining < 0 {
			remaining = 0
		}
		n, err := writeArchiveEntry(ctx, target, f, f.Mode().Perm(), remaining+1, touch)
		written += n
		if err != nil {
			if ctx.Err() != nil {
				return stalled()
			}
			return refuse(http.StatusInternalServerError, fmt.Errorf("writing %s: %w", name, err))
		}
		if written-reclaimed > lim.MaxBytes {
			return overBudget(written - reclaimed)
		}
		return nil
	})

	if refusal == nil && walkErr != nil {
		status, refusal = http.StatusInternalServerError, walkErr
	}
	if refusal != nil {
		if !destExisted {
			// Roll back only a destination we created ourselves AND only for
			// the refusals whose partial output is itself the harm (see
			// refuseAndDiscard). A stall, a read error or a corrupt archive
			// keeps what landed: destroying a multi-hour restore because the
			// last 5% of it stalled is a worse outcome than an incomplete
			// folder the user can resume over or delete.
			if discard {
				_ = os.RemoveAll(dest)
			} else {
				// Nothing landed at all: don't leave a stray empty directory
				// behind. os.Remove refuses a non-empty directory, so this can
				// never delete extracted data.
				_ = os.Remove(dest)
			}
		}
		return status, refusal
	}
	return 0, nil
}

// archiveReclaimableBytes reports how much space the filesystem gets back when
// an extracted member truncates whatever already sits at target. It is the
// credit that makes the extraction budget count NET NEW bytes.
//
// Two ways this could be turned into free budget, both closed here:
//
//   - a SYMLINK planted at target. Lstat never follows it, so a link pointing
//     at a 4 TB file credits nothing (and O_NOFOLLOW refuses to write through
//     it anyway); only a regular file is credited.
//   - a SPARSE file planted at target. `truncate -s 100T` costs no blocks at
//     all, so crediting the apparent size would buy 100 TB of budget for free
//     and let one extraction take the whole array — an outage for all 27
//     containers, not just a failed restore. Allocated blocks are what the
//     filesystem actually returns, so the smaller of the two wins. For an
//     ordinary file blocks*512 >= size, so this is exactly the file size and
//     the accounting stays symmetric with the logical bytes counted on the way
//     in; only sparse (and transparently compressed) files credit less, and
//     erring low there merely refuses, it never overfills.
func archiveReclaimableBytes(target string) int64 {
	st, err := os.Lstat(target)
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	size := st.Size()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if alloc := int64(sys.Blocks) * 512; alloc < size {
			size = alloc
		}
	}
	if size < 0 {
		return 0
	}
	return size
}

// writeArchiveEntry copies at most limit bytes of one member to target and
// reports how many bytes it wrote.
func writeArchiveEntry(ctx context.Context, target string, src io.Reader, perm os.FileMode, limit int64, touch func()) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	if perm == 0 {
		// Some producers ship zero mode bits; a 0000 file is useless to the
		// user and archiver's own behaviour here is an accident, not a rule.
		perm = 0o644
	}
	// Write beside, publish by rename. The previous shape opened target with
	// O_TRUNC directly, which destroys the pre-existing file the moment the
	// member starts — so a corrupt archive, a cancel, or a full disk left the
	// user with a partial file where their old copy used to be. Extraction
	// over existing data must be all-or-nothing per member: the old file
	// survives every failure, and only a fully-written member replaces it.
	//
	// A symlink already sitting at target is refused outright, preserving the
	// contract the old O_NOFOLLOW enforced (and the test that pins it): a
	// planted link in the destination is an attack shape, and refusing loudly
	// beats both writing through it and silently deleting it. ELOOP semantics
	// via error, not via the kernel flag, because the actual write now goes
	// to a fresh CreateTemp name where nothing can be planted.
	if st, lerr := os.Lstat(target); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
		return 0, &os.PathError{Op: "extract", Path: target, Err: syscall.ELOOP}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".extract-*")
	if err != nil {
		return 0, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return 0, err
	}

	n, err := io.Copy(tmp, io.LimitReader(&stallReader{ctx: ctx, r: src, touch: touch}, limit))
	if err != nil {
		cleanup()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return n, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return n, err
	}
	return n, nil
}

// stallReader makes a plain archive stream cancelable and reports progress.
type stallReader struct {
	ctx   context.Context
	r     io.Reader
	touch func()
}

func (s *stallReader) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	if n > 0 && s.touch != nil {
		s.touch()
	}
	return n, err
}

// newStallGuard returns a context cancelled once no progress has been reported
// for d, the touch function that reports progress, and a stop function.
// touch is only ever called from the single goroutine driving the walk.
func newStallGuard(parent context.Context, d time.Duration) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancel(parent)
	if d <= 0 {
		return ctx, func() {}, cancel
	}
	timer := time.AfterFunc(d, cancel)
	last := time.Now()
	touch := func() {
		now := time.Now()
		// Rearming on every 32 KiB read would be pure overhead; a tenth of the
		// budget is fine-grained enough and still cannot expire mid-transfer.
		if now.Sub(last) < d/10 {
			return
		}
		last = now
		timer.Reset(d)
	}
	return ctx, touch, func() { timer.Stop(); cancel() }
}

// archiveSpaceBudget is how many bytes an extraction into dest may write.
//
// A fixed byte cap cannot work here: small enough to protect a nearly full
// disk it would refuse a legitimate 50 GB restore, and large enough for that
// restore it protects nothing. What actually matters is that an archive can
// never take the last byte of the filesystem, so the cap is what is free right
// now minus a reserve, and the reserve scales down on small volumes so that
// extracting a 10 MB zip onto a disk with 900 MB left still works.
func archiveSpaceBudget(dest string) (int64, error) {
	// dest itself usually does not exist yet; measure the nearest ancestor
	// that does, which is on the same filesystem.
	probe := dest
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(probe, &st); err != nil {
		return 0, err
	}
	avail := int64(st.Bavail) * st.Bsize
	if avail < 0 {
		avail = 0
	}
	reserve := avail / 20
	if reserve > archiveSpaceReserve {
		reserve = archiveSpaceReserve
	}
	return avail - reserve, nil
}

func archiveHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// archiveEntryName is the member's full path inside the archive. f.Name() is
// the FileInfo name, which for tar and zip is only the last component — using
// it as the extraction path would flatten every directory.
func archiveEntryName(f archiver.File) string {
	switch h := f.Header.(type) {
	case *tar.Header:
		return h.Name
	case zip.FileHeader:
		return h.Name
	case *zip.FileHeader:
		return h.Name
	}
	return f.Name()
}

// checkUnsafeArchiveEntry refuses a member we must not materialise, and
// reports skip=true for members that carry no file at all. This is the same
// refusal set the old pre-scan applied, moved inline so that it runs against
// the bytes actually being extracted rather than against a separate read.
//
// SECURITY: archiver's zip-slip protection validates entry NAMES but not
// symlink/hardlink TARGETS — a crafted archive with a symlink entry (e.g.
// "pwn" -> "/") followed by a write through it ("pwn/etc/cron.d/x") escapes
// the destination and writes anywhere as root. Special bits are rejected as
// defense in depth: nothing legitimate in a user upload needs setuid/setgid/
// sticky.
func checkUnsafeArchiveEntry(f archiver.File, name string) (bool, error) {
	// Mode-based check FIRST, for every archive format. The tar branch used to
	// be the only one rejecting links, but ZIP carries symlinks too (mode bits
	// in the external attrs) and they materialise just the same.
	mode := f.Mode()
	if mode&os.ModeSymlink != 0 {
		return false, fmt.Errorf("contains a link entry (%s)", name)
	}
	if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false, fmt.Errorf("contains an entry with special permission bits (%s)", name)
	}
	if mode&(os.ModeDevice|os.ModeCharDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeIrregular) != 0 {
		return false, fmt.Errorf("contains a special file entry (%s)", name)
	}

	if h, ok := f.Header.(*tar.Header); ok {
		switch h.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			// A hardlink entry has a plain regular-file mode, so only the
			// typeflag catches it; it is just as good an escape as a symlink.
			return false, fmt.Errorf("contains a link entry (%s)", h.Name)
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// pax metadata, not a member. archiver ignores these too; writing
			// them out would litter every git-generated tarball's output with
			// a stray "pax_global_header" file.
			return true, nil
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeGNUSparse:
			// supported
		default:
			return false, fmt.Errorf("contains an unsupported entry type (%s)", h.Name)
		}
	}

	if unsafeArchiveName(name) {
		return false, fmt.Errorf("contains an unsafe path (%s)", name)
	}
	return false, nil
}

// unsafeArchiveName reports whether an in-archive path escapes its root.
func unsafeArchiveName(name string) bool {
	if name == "" {
		return false
	}
	if filepath.IsAbs(name) {
		return true
	}
	clean := filepath.Clean(name)
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
