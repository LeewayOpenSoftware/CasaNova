package service

import (
	"errors"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"hash/fnv"
	"io"
	"math"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	pkgfile "github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// Upload safety limits. These cap user-controlled inputs so a malicious
// client can't exhaust memory, disk, or arithmetic range.
const (
	// MaxTotalChunks caps the uploaded[] bool slice to ~1MB of RAM per
	// concurrent upload. 1M chunks at 100MB each = 100TB, which is far
	// beyond any realistic file.
	MaxTotalChunks int64 = 1_000_000

	// MaxChunkSize bounds per-chunk memory use and file offset arithmetic.
	// 1 GiB is more than any chunked uploader should ever need.
	MaxChunkSize int64 = 1 << 30

	// MaxTotalSize is a global cap on a single file (16 TiB). Pre-allocation
	// with Truncate respects this so ENOSPC happens early.
	MaxTotalSize int64 = 16 << 40

	// MaxLiveUploadSessions caps how many INCOMPLETE sessions may exist at
	// once. Every session costs RAM (a chunk bitmap), a registry entry, and —
	// because the registry is rewritten as one document — a share of every
	// other session's chunk write. Nothing bounded that before: POST
	// /v2/casaos/file/upload only needs PermFileWrite, and this box answers
	// from the internet through the Cloudflare tunnel.
	//
	// 256 is far above any real client. A browser uploads a handful of files
	// in parallel and a session disappears the moment its file lands, so the
	// only way to accumulate them is to abandon uploads on purpose. The cap
	// gates session CREATION only: an upload already in flight (the 120 GB
	// tunnel transfer this project validated) keeps taking chunks no matter
	// how full the registry is.
	MaxLiveUploadSessions = 256

	// MaxLiveUploadChunks is the aggregate bitmap budget across live sessions.
	// The per-session cap alone is not enough: 256 sessions each declaring
	// MaxTotalChunks would be 256 MB of bitmaps in RAM and a ~34 MB registry
	// rewritten on every single chunk. Budgeting the SUM bounds both, and at
	// 2M chunks it is still absurdly generous — with the client's 32 MB chunks
	// that is 64 TB of uploads in flight at the same time.
	MaxLiveUploadChunks int64 = 2_000_000

	// MaxUploadIdentifierLen bounds the resume key. It is a map key, a
	// registry key and part of the .tmp name, all attacker-chosen and all
	// retained for uploadSessionMaxAge. Real identifiers are "<size>-<file
	// name>", so 512 bytes leaves room for the longest legal file name.
	MaxUploadIdentifierLen = 512
)

// uploadTouchInterval is how stale a session's recorded timestamp may get
// before a chunk that changes nothing still refreshes it. See the persist
// decision in UploadFile.
const uploadTouchInterval = 5 * time.Minute

// uploadReapIntervalOverride shortens the reaper's tick for tests. Atomic, not
// a plain var: it is read while other services' goroutines are alive, so a
// plain global would be a genuine data race. Zero means "use the default".
//
// It is read EXACTLY ONCE, synchronously, inside NewFileUploadService — never
// from the reaper goroutine. Reading it from the goroutine was a real defect:
// `go s.reapLoop()` schedules asynchronously, so a service constructed by test
// A could still be sitting un-started when test B set the override to 20 ms,
// and would then run a 50 Hz reaper for the rest of its life even after B's
// t.Cleanup restored the global. Measured symptom: one run in seventeen took
// 106 s, with TestBackgroundReaperRunsWithoutBeingAsked failing at 15.02 s and
// nearly every other test paying exactly 10.00 s (the Close() ceiling below),
// while the other sixteen runs finished the whole file in 0.49 s. Capturing the
// value per SERVICE makes the interval a property of the construction site, so
// no later write to the global can reach a service that already exists.
var uploadReapIntervalOverride atomic.Int64

// reapInterval is how often the background reaper runs. Hourly is far below the
// 48h retention window and costs one pass over a map that is normally empty.
func reapInterval() time.Duration {
	if v := uploadReapIntervalOverride.Load(); v > 0 {
		return time.Duration(v)
	}
	return time.Hour
}

// ErrSessionReset tells the route layer to answer 409: the resume state no
// longer matches reality (tmp vanished/resized, grid mismatch) and the client
// must restart the file from chunk 1. Resuming into a recreated-empty tmp
// would silently assemble zeros where "present" chunks used to be.
var ErrSessionReset = errors.New("upload session reset required")

// ErrTooManyUploadSessions tells the route layer to answer 429: the registry is
// at its admission limit. It is deliberately NOT reachable for a session that
// already exists — only the creation of a new one is refused — so a transfer in
// progress can never be knocked over by someone else's flood.
var ErrTooManyUploadSessions = errors.New("too many upload sessions in progress")

// ErrUploadPathRejected marks a destination the path validator refused. It is a
// client error (400), not a server failure: the request named a path that
// resolves somewhere the file API must never write.
var ErrUploadPathRejected = errors.New("upload path rejected")

type FileInfo struct {
	init             bool
	uploaded         []bool
	uploadedChunkNum int64
	finalizing       bool // finalize in progress — blocks a duplicate retry
}

type FileUploadService struct {
	uploadStatus sync.Map
	lock         sync.RWMutex
	// sessions mirrors uploadStatus durably (upload_sessions.go) so resume
	// survives a service restart and stale .tmp files get reaped.
	sessions map[string]*uploadSession
	// stop shuts the background reaper down and done reports that it is gone.
	// Production never closes it (the service lives as long as the process);
	// tests do, and they need the loop to be finished before they restore the
	// globals it reads.
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	// reapEvery is this service's own reaper period, captured once at
	// construction. See uploadReapIntervalOverride for why it must NOT be read
	// from inside the goroutine.
	reapEvery time.Duration
}

func NewFileUploadService() *FileUploadService {
	s := &FileUploadService{
		uploadStatus: sync.Map{},
		lock:         sync.RWMutex{},
		sessions:     loadUploadSessions(),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		// Read the interval HERE, on the constructing goroutine, so it is fixed
		// before NewFileUploadService returns.
		reapEvery: reapInterval(),
	}
	// Rehydrate the in-RAM resume state from the surviving sessions so
	// TestChunk answers correctly and clients skip already-uploaded chunks.
	var complete []string
	for id, sess := range s.sessions {
		uploaded := make([]bool, len(sess.Uploaded))
		copy(uploaded, sess.Uploaded)
		n := int64(0)
		for _, u := range uploaded {
			if u {
				n++
			}
		}
		s.uploadStatus.Store(id, &FileInfo{init: true, uploaded: uploaded, uploadedChunkNum: n})
		if n == int64(len(uploaded)) && len(uploaded) > 0 {
			complete = append(complete, id)
		}
	}
	// A session that is ALREADY fully uploaded when we rehydrate means the
	// service died between the last chunk's bitmap write and the SafeRename
	// (a crash, or the self-updater's restart). The client would preflight
	// every chunk, get 200, skip all POSTs and report success — but no file
	// exists at the destination and the reaper would delete the .tmp in 48h.
	// Finish the rename now so the upload actually lands.
	for _, id := range complete {
		s.finalizeRehydrated(id)
	}
	// Start the reaper HERE, at the single place the service is constructed
	// (route/v2/route.go::NewCasaOS), so it can't be forgotten by a caller.
	go s.reapLoop()
	return s
}

// reapLoop is the half of the session registry that was missing.
// uploadSessionMaxAge, the decision table and ReapUploadSessions all existed,
// but nothing ever drove them: the reaper ran once at service start and then
// only if a human opened the maintenance screen. An abandoned session — and the
// pre-allocated .tmp it owns — therefore lived until the next reboot, so any
// PermFileWrite caller (i.e. anyone reachable through the tunnel) could park
// registry entries and temp files forever. The pass itself is unchanged; this
// only makes the age limit mean something.
func (s *FileUploadService) reapLoop() {
	defer close(s.done)
	every := s.reapEvery
	if every <= 0 {
		// A zero-value service (never through the constructor) must not panic
		// in time.NewTicker; fall back to the production period.
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			// A closed stop channel must ALWAYS beat a pending tick. Go's
			// select picks uniformly among ready cases, so with a short period
			// the loop could keep starting sweeps after Close() had already
			// asked it to quit — each sweep takes s.lock and writes the
			// registry, which is how Close() ended up waiting out its 10 s
			// ceiling and how a stopped service kept rewriting the registry
			// file of whoever came next. This non-blocking check makes the
			// shutdown edge deterministic.
			select {
			case <-s.stop:
				return
			default:
			}
			s.SweepSessions(false)
		}
	}
}

// Close stops the background reaper and waits for it to finish the pass it may
// be in, so a test can restore the globals the loop reads. Safe to call more
// than once.
//
// The wait is bounded and the channels are nil-checked: Close must never be the
// thing that hangs a caller, whether the loop is mid-sweep, was never started
// (a zero-value service), or is wedged.
func (s *FileUploadService) Close() {
	s.stopOnce.Do(func() {
		if s.stop == nil {
			return
		}
		close(s.stop)
		if s.done == nil {
			return
		}
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			logger.Error("upload session reaper did not stop in time; continuing")
		}
	})
}

// sanitizeUploadPath re-validates one of the paths this upload builds by
// concatenation, and is the fix for the symlink escape described in UploadFile.
//
// It returns the path UNCHANGED on success. A path whose canonical form differs
// from what we were about to open is refused rather than rewritten, because the
// property that makes the check meaningful is "the string we validated is the
// string we open". utils.SanitizeFilePath returns FixAndCleanPath's output and
// that rewrites "\" into "/": a name like "AC\DC.mp3" would be CHECKED as two
// path components and WRITTEN as one, so a directory component carrying a
// backslash could be a symlink the check never looks at. The v1 twin refuses
// the same input up front (route/v1/file.go::validateUploadNames) for exactly
// this reason.
func sanitizeUploadPath(p string) (string, error) {
	clean, err := utils.SanitizeFilePath(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUploadPathRejected, err.Error())
	}
	if clean != p {
		return "", fmt.Errorf(`%w: upload path is not canonical (a name cannot contain a backslash)`, ErrUploadPathRejected)
	}
	// And through the shared storage authority's ceiling.
	//
	// requireUnderUploadRoot below already confines the write to the root the
	// request named, which is the stronger of the two checks. This adds the
	// question that root confinement cannot ask: whether that root is inside
	// storage CasaNova is authorized to manage at all. Keeping both means the
	// upload path and every other file endpoint answer to ONE ceiling instead
	// of drifting apart the way the sanitiser and the upload blocklist did.
	//
	// Ceiling only: the per-principal decision was already made at the route
	// layer, which is where the identity lives.
	if d := storageauth.AuthorizeSystem(storageauth.OpUpload|storageauth.OpCreate|storageauth.OpWrite, clean); !d.Allowed {
		return "", fmt.Errorf("%w: %s", ErrUploadPathRejected, string(d.Reason))
	}
	return clean, nil
}

// --- containment -------------------------------------------------------------
//
// sanitizeUploadPath alone delivers a WEAKER guarantee than "the upload stays
// in the upload area": all utils.SanitizeFilePath promises is "the write does
// not land inside utils.BlockedSystemPrefixes". /home, /tmp, /srv/../x and /
// itself are not on that list, so with /DATA/Uploads/link -> /home planted in
// the allowed area, relativePath="link/victim/.ssh/authorized_keys" was
// ACCEPTED (err == nil, measured against the already-patched code) and wrote
// attacker bytes to /home/victim/.ssh/authorized_keys as root — outside the
// upload root, and a straight path to remote login.
//
// The functions below add the guarantee that was actually claimed: every path
// this upload opens, creates or renames must still resolve UNDER the upload
// root the request named. That is one comparison per path on top of the
// resolution the sanitizer already performs.

// deepestLstatAncestor splits p into the deepest ancestor that exists on disk
// and the components that do not exist yet. os.Lstat, not os.Stat, so a
// DANGLING symlink counts as existing and gets expanded by uploadResolve rather
// than walked past — /DATA/Uploads/link -> /home/newdir (not yet created) is
// exactly the shape that would otherwise slip through, because EvalSymlinks
// fails on it and a naive resolver would then answer with the unresolved path.
func deepestLstatAncestor(p string) (string, []string) {
	var missing []string
	for {
		if _, err := os.Lstat(p); err == nil {
			for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
				missing[i], missing[j] = missing[j], missing[i]
			}
			return p, missing
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil
		}
		missing = append(missing, filepath.Base(p))
		p = parent
	}
}

// uploadResolve reports where p really lands on disk, for a path that may not
// exist yet (every upload target is in that state on the first chunk).
//
// It mirrors the resolution utils.SanitizeFilePath performs internally, which
// this package cannot reuse because that function deliberately returns the
// LEXICAL path — callers must open the link, not its target. Here we need the
// resolved target itself, to compare it against the upload root.
//
// The one deliberate difference: this returns an ERROR when it cannot resolve,
// where the utils version falls back to the unresolved path. A path we cannot
// place is a path we refuse to write to — fail closed, no fallback to the old
// permissive behaviour.
func uploadResolve(p string) (string, error) {
	cur := filepath.Clean(p)
	// The hop bound is only here so a symlink loop can never spin a request
	// thread; a real chain is one or two hops.
	for hop := 0; hop < 16; hop++ {
		dir, missing := deepestLstatAncestor(cur)
		if dir == "" {
			return "", fmt.Errorf("%w: %s cannot be located on disk", ErrUploadPathRejected, p)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		// dir survives Lstat but not EvalSymlinks ⇒ dir IS a dangling symlink.
		// Expand it by hand and retry.
		target, err := os.Readlink(dir)
		if err != nil {
			return "", fmt.Errorf("%w: %s cannot be resolved", ErrUploadPathRejected, p)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(dir), target)
		}
		cur = filepath.Join(append([]string{target}, missing...)...)
	}
	return "", fmt.Errorf("%w: %s resolves through too many symlinks", ErrUploadPathRejected, p)
}

// withinServedStorageRoot reports whether a resolved path lands inside one of
// the storage areas the file API already serves (utils.DataRoots).
//
// This exemption is what keeps the containment check from breaking a normal NAS
// layout, where a folder inside the upload area is a symlink onto another disk
// (/DATA/Uploads/media -> /mnt/disk2/media is routine, and utils.DataRoots
// exists precisely because a storage root is not always a plain host
// directory). It grants no capability: a caller that can upload here already
// holds PermFileWrite, and PostUploadFile accepts any path under those same
// roots directly, so redirecting through a link reaches nothing new. /home,
// /tmp and / are NOT storage roots, which is exactly the escape being closed.
//
// Consulted only after the strict check has already failed, so the path every
// legitimate request takes pays nothing for it.
func withinServedStorageRoot(resolved string) bool {
	for _, root := range utils.DataRoots {
		if _, err := os.Lstat(root); err != nil {
			continue // not an install that has this root
		}
		target, err := uploadResolve(root)
		if err != nil || target == "/" {
			// A root that resolved to "/" would exempt the entire filesystem.
			continue
		}
		if resolved == target || strings.HasPrefix(resolved, target+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// requireUnderUploadRoot is the containment assertion itself: p, once every
// symlink on it has been followed, must still be inside resolvedRoot (or inside
// a served storage root — see withinServedStorageRoot).
func requireUnderUploadRoot(resolvedRoot, p string) error {
	resolved, err := uploadResolve(p)
	if err != nil {
		return err
	}
	if resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return nil
	}
	if withinServedStorageRoot(resolved) {
		return nil
	}
	return fmt.Errorf("%w: %s resolves to %s, which is outside the upload root %s",
		ErrUploadPathRejected, p, resolved, resolvedRoot)
}

// chunkBudgetExceeded reports whether admitting a session of `want` chunks
// would push the live bitmaps past MaxLiveUploadChunks.
func chunkBudgetExceeded(sessions map[string]*uploadSession, want int64) bool {
	total := want
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		total += sess.TotalChunks
		if total > MaxLiveUploadChunks {
			return true
		}
	}
	return total > MaxLiveUploadChunks
}

// sessionNeedsTouch reports whether a chunk that changed no bits should still
// refresh the session's timestamp. Keeps a long retry storm from aging a live
// session out while still collapsing the writes it would otherwise cause.
func sessionNeedsTouch(sess *uploadSession) bool {
	t, err := time.Parse(time.RFC3339, sess.UpdatedAt)
	return err != nil || time.Since(t) > uploadTouchInterval
}

// dropSession forgets a session in both halves of the state (registry + in-RAM
// bitmap) and persists the result. removeTmp additionally deletes the recorded
// temp file, but only through removeSessionTmp's gate.
func (s *FileUploadService) dropSession(id string, removeTmp bool) {
	s.lock.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.uploadStatus.Delete(id)
	persistUploadSessions(s.sessions)
	s.lock.Unlock()
	if removeTmp && sess != nil {
		removeSessionTmp(id, sess.Root, sess.TmpPath)
	}
}

// rehydrateDestination re-derives the destination of a session that is being
// finalized from the REGISTRY instead of from a live request, and re-validates
// every path involved.
//
// This is the deploy-safety guard. Until now the rehydrate path took
// sess.TmpPath straight from the JSON on disk, derived a destination with
// TrimSuffix/LastIndex and handed both to SafeRename with no sanitisation and
// no shape check at all — while the live request path had just been hardened.
// The registry currently on the box was written by the VULNERABLE build, which
// recorded whatever path the request produced; an attacker can therefore have
// already parked a session whose TmpPath is /etc/cron.d/x.upload-XXXXXXXX.tmp.
// Installing the hardened build would then have made the FIX itself perform the
// escape, on its very first start, before any request arrived.
//
// Three things must hold, and any failure discards the session instead of
// renaming anything:
//
//  1. the recorded temp path is one of ours (shape) and passes the same
//     sanitiser as a live upload;
//  2. the session records the upload root it was created under. Entries written
//     by the vulnerable build have no Root, cannot be proven to belong to any
//     upload area, and are therefore never finalized — that is the one-time
//     migration, and it is what makes deploying this patch safe;
//  3. both the temp file and the derived destination still RESOLVE under that
//     root, so a symlink planted since does not move the landing site.
func rehydrateDestination(sess *uploadSession) (string, error) {
	if !sessionTmpAcceptableIn(sess.Root, sess.TmpPath) {
		return "", fmt.Errorf("%w: recorded temp path %q is not one of our upload temp files", ErrUploadPathRejected, sess.TmpPath)
	}
	if sess.Root == "" {
		return "", fmt.Errorf("%w: session records no upload root (written by an older build); it cannot be proven to belong to an upload area", ErrUploadPathRejected)
	}
	root, err := sanitizeUploadPath(sess.Root)
	if err != nil {
		return "", err
	}
	// uploadTmpPath is "<dest>.upload-<hash>.tmp" — strip the whole suffix.
	destPath := strings.TrimSuffix(sess.TmpPath, ".tmp")
	i := strings.LastIndex(destPath, ".upload-")
	if i < 0 {
		return "", fmt.Errorf("%w: recorded temp path %q has no destination in it", ErrUploadPathRejected, sess.TmpPath)
	}
	destPath, err = sanitizeUploadPath(destPath[:i])
	if err != nil {
		return "", err
	}
	resolvedRoot, err := uploadResolve(root)
	if err != nil {
		return "", err
	}
	for _, p := range []string{sess.TmpPath, destPath} {
		if err := requireUnderUploadRoot(resolvedRoot, p); err != nil {
			return "", err
		}
	}
	return destPath, nil
}

// finalizeRehydrated completes an upload whose chunks all arrived before a
// crash but whose SafeRename never ran. Best-effort: on any error the session
// stays put and the reaper's staleness check is the backstop.
func (s *FileUploadService) finalizeRehydrated(id string) {
	s.lock.Lock()
	sess := s.sessions[id]
	s.lock.Unlock()
	if sess == nil || sess.TmpPath == "" {
		return
	}
	// Validate BEFORE anything is renamed or removed: an unvalidated registry
	// entry must not be able to drive either operation.
	destPath, err := rehydrateDestination(sess)
	if err != nil {
		logger.Error("refusing to finalize an upload session whose recorded paths do not validate; entry dropped, nothing on disk touched",
			zap.String("identifier", id), zap.String("tmp", sess.TmpPath), zap.Error(err))
		s.dropSession(id, false)
		return
	}
	// The tmp must still be the exact pre-allocated file (right size); a
	// truncated/missing tmp is unrecoverable — drop the session so the
	// client restarts instead of resuming into a bad file.
	if !sessionTmpValid(sess) {
		s.dropSession(id, true)
		return
	}
	if err := pkgfile.SafeRename(sess.TmpPath, destPath); err != nil {
		logger.Error("rehydrate finalize rename failed", zap.String("id", id), zap.Error(err))
		return
	}
	s.dropSession(id, false)
	logger.Info("finalized upload that completed just before a restart", zap.String("dest", destPath))
}

// validateChunkParams guards every user-controlled int parameter before it
// touches a slice index or a file offset. Invalid values return a clear
// error instead of panicking with index-out-of-range or silently wrapping
// to a negative seek offset.
func validateChunkParams(chunkNumber, chunkSize, currentChunkSize, totalChunks, totalSize int64) error {
	if totalChunks <= 0 || totalChunks > MaxTotalChunks {
		return fmt.Errorf("totalChunks %d out of range [1..%d]", totalChunks, MaxTotalChunks)
	}
	if chunkNumber <= 0 || chunkNumber > totalChunks {
		return fmt.Errorf("chunkNumber %d out of range [1..%d]", chunkNumber, totalChunks)
	}
	if chunkSize <= 0 || chunkSize > MaxChunkSize {
		return fmt.Errorf("chunkSize %d out of range [1..%d]", chunkSize, MaxChunkSize)
	}
	if totalSize < 0 || totalSize > MaxTotalSize {
		return fmt.Errorf("totalSize %d out of range [0..%d]", totalSize, MaxTotalSize)
	}
	// Overflow check on the seek offset: (chunkNumber-1)*chunkSize must fit
	// in int64. Without this, a huge chunkNumber * chunkSize wraps to a
	// negative offset and writes happen at arbitrary file positions.
	if chunkNumber-1 > math.MaxInt64/chunkSize {
		return fmt.Errorf("chunk offset overflow for chunk %d size %d", chunkNumber, chunkSize)
	}
	// The declared layout must be self-consistent: totalSize fits the grid.
	if totalSize <= (totalChunks-1)*chunkSize || totalSize > totalChunks*chunkSize {
		return fmt.Errorf("totalSize %d inconsistent with %d chunks of %d", totalSize, totalChunks, chunkSize)
	}
	// currentChunkSize is exact: chunkSize for every chunk except the last,
	// which carries the remainder. An oversized chunk would otherwise overrun
	// into the next chunk's region of the sparse .tmp.
	want := chunkSize
	if chunkNumber == totalChunks {
		want = totalSize - (totalChunks-1)*chunkSize
	}
	if currentChunkSize != want {
		return fmt.Errorf("currentChunkSize %d != expected %d for chunk %d/%d", currentChunkSize, want, chunkNumber, totalChunks)
	}
	return nil
}

// uploadTmpPath derives the per-SESSION temp file. Unique per identifier so
// two concurrent sessions to the same destination can never share (and
// Truncate) one inode, and so it can never collide with a legitimate user
// file named "<dest>.tmp".
func uploadTmpPath(destPath, identifier string) string {
	h := fnv.New32a()
	h.Write([]byte(identifier))
	return fmt.Sprintf("%s.upload-%08x.tmp", destPath, h.Sum32())
}

// sessionTmpValid reports whether the session's tmp is still the pre-allocated
// file the bitmap describes (exists, regular, exact size).
func sessionTmpValid(sess *uploadSession) bool {
	if sess == nil {
		return false
	}
	st, err := os.Lstat(sess.TmpPath)
	return err == nil && st.Mode().IsRegular() && st.Size() == sess.TotalSize
}

func (s *FileUploadService) TestChunk(
	c echo.Context,
	identifier string,
	chunkNumber int64,
) error {
	fileInfoTemp, ok := s.uploadStatus.Load(identifier)

	if !ok {
		return errors.New("file not found")
	}

	fileInfo, assertOk := fileInfoTemp.(*FileInfo)
	if !assertOk {
		return errors.New("upload state corrupt")
	}

	if !fileInfo.init {
		return errors.New("file not init")
	}

	// A bitmap entry is only meaningful while the session's tmp still exists
	// at its pre-allocated size. If it vanished (user deleted the visible
	// .tmp, disk swap, …) answer "absent" for everything: the client then
	// re-POSTs and the first UploadFile resets the broken session.
	s.lock.RLock()
	sess := s.sessions[identifier]
	s.lock.RUnlock()
	if !sessionTmpValid(sess) {
		return errors.New("session tmp missing")
	}

	// Bounds-check chunkNumber BEFORE indexing — untrusted client input.
	if chunkNumber <= 0 || chunkNumber > int64(len(fileInfo.uploaded)) {
		return errors.New("chunkNumber out of range")
	}

	if !fileInfo.uploaded[chunkNumber-1] {
		return errors.New("file not found")
	}

	return nil
}

func (s *FileUploadService) UploadFile(
	c echo.Context,
	path string,
	chunkNumber int64,
	chunkSize int64,
	currentChunkSize int64,
	totalChunks int64,
	totalSize int64,
	identifier string,
	relativePath string,
	fileName string,
	bin *multipart.FileHeader,
) error {
	// Validate every user-controlled number up-front. Before this guard,
	// a malicious client could trigger array-OOB panics, int64 overflow on
	// the seek offset, or multi-GB memory allocation via totalChunks.
	if err := validateChunkParams(chunkNumber, chunkSize, currentChunkSize, totalChunks, totalSize); err != nil {
		return err
	}
	// The identifier is a map key, a registry key and part of the .tmp name,
	// all chosen by the caller and all retained for uploadSessionMaxAge.
	if len(identifier) == 0 || len(identifier) > MaxUploadIdentifierLen {
		return fmt.Errorf("identifier must be 1..%d bytes, got %d", MaxUploadIdentifierLen, len(identifier))
	}
	// The uploaded part must BE the size it declares — a mismatched body
	// would silently overwrite the next chunk's region (or leave a hole).
	if bin.Size != currentChunkSize {
		return fmt.Errorf("chunk %d body is %d bytes, declared %d", chunkNumber, bin.Size, currentChunkSize)
	}

	// The upload root itself is validated FIRST and everything below is built
	// from the validated string, so the base can never be one thing for the
	// check and another for the write.
	basePath, err := sanitizeUploadPath(filepath.Clean(path))
	if err != nil {
		return err
	}

	// Bound the path BEFORE anything walks it. Every path guard on this
	// request — the sanitizer's resolution and the containment check below —
	// costs one Lstat per component that does not exist yet, and the component
	// count is chosen by the caller. Without a bound, a single small POST
	// carrying a relativePath of a few hundred thousand segments buys hundreds
	// of thousands of syscalls: the guard becomes the amplifier.
	//
	// Neither limit can refuse an upload the filesystem would have accepted:
	// the kernel rejects any path over PATH_MAX (4096) at open() time, and it
	// does so only AFTER we would have paid for the walk. 128 components is far
	// past the deepest real folder upload.
	if len(relativePath) > 4096 {
		return fmt.Errorf("relative path is %d bytes, over the %d-byte limit", len(relativePath), 4096)
	}
	if n := strings.Count(relativePath, "/"); n > 128 {
		return fmt.Errorf("relative path has %d components, over the %d-component limit", n+1, 128)
	}

	// Sanitize relativePath to prevent path traversal (e.g. "../../etc/shadow").
	cleanRel := filepath.Clean(relativePath)
	if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return fmt.Errorf("invalid relative path: %q", relativePath)
	}
	// Verify the resolved destination stays within the target directory.
	destPath := filepath.Join(basePath, cleanRel)
	if !strings.HasPrefix(destPath, basePath+string(filepath.Separator)) {
		return fmt.Errorf("relative path escapes target directory")
	}

	// Re-validate EVERY path built above, now that the concatenation happened.
	// basePath was sanitized on entry, but the checks on relativePath
	// are purely lexical (no "..", not absolute, stays under path) and say
	// nothing about a SYMLINKED directory component planted inside the allowed
	// area: any container with a bind-mount of /DATA can create
	// /DATA/Uploads/link -> /etc, and then relativePath=link/cron.d/x made this
	// handler MkdirAll and write /etc/cron.d/x as root. utils.SanitizeFilePath
	// resolves the deepest existing ancestor through EvalSymlinks and applies
	// the canonical blocklist, so the check runs against where the write really
	// lands. Same fix, same helper and same reasoning as the v1 twin
	// (route/v1/file.go::PostFileUpload re-validates each concatenation).
	destPath, err = sanitizeUploadPath(destPath)
	if err != nil {
		return err
	}
	// The parent is created with MkdirAll below: a link on any of its
	// components would create directories outside the data area even when the
	// file itself never lands.
	folderPath, err := sanitizeUploadPath(filepath.Dir(destPath))
	if err != nil {
		return err
	}
	// The .tmp is the file actually opened, written and fsynced; validating
	// only the final destination would leave every byte of the transfer
	// unchecked.
	tmpPath, err := sanitizeUploadPath(uploadTmpPath(destPath, identifier))
	if err != nil {
		return err
	}

	// CONTAINMENT. The three checks above only prove "not inside a blocked
	// system prefix", which is a strictly weaker property than the one the file
	// API owes its users. Measured against the code as patched: with
	// /DATA/Uploads/link -> <any directory outside every blocked prefix>,
	// relativePath="link/victim/.ssh/authorized_keys" returned err == nil and
	// wrote the attacker's bytes to <that directory>/victim/.ssh/
	// authorized_keys as root — /home, /tmp and / are not on the blocklist.
	// Requiring the RESOLVED paths to stay under the resolved upload root is
	// what makes "the upload stays in the upload area" true rather than
	// merely claimed.
	resolvedRoot, err := uploadResolve(basePath)
	if err != nil {
		return err
	}
	for _, p := range []string{folderPath, destPath, tmpPath} {
		if err := requireUnderUploadRoot(resolvedRoot, p); err != nil {
			return err
		}
	}

	s.lock.Lock()
	fileInfoTemp, ok := s.uploadStatus.Load(identifier)
	var fileInfo *FileInfo
	var file *os.File

	if cleanRel != fileName {
		// uploaded file is folder — folderPath was validated above.
		if _, err := os.Stat(folderPath); os.IsNotExist(err) {
			err := os.MkdirAll(folderPath, os.ModePerm)
			if err != nil {
				s.lock.Unlock()
				return err
			}
		}
	}

	if !ok {
		// Admission control for a NEW session only. Nothing here can touch an
		// upload already in flight: a session that exists takes the else branch
		// and keeps accepting chunks whatever the registry looks like, so a
		// flood cannot knock over a legitimate multi-hour transfer.
		//
		// Reclaim first — an entry that is already past the retention window
		// must not keep a real upload out. We hold the lock, so call the
		// in-lock form; the decision table is the same one every other pass
		// uses.
		if len(s.sessions) >= MaxLiveUploadSessions || chunkBudgetExceeded(s.sessions, totalChunks) {
			s.sweepLocked()
		}
		if len(s.sessions) >= MaxLiveUploadSessions {
			s.lock.Unlock()
			return fmt.Errorf("%w: %d sessions already in progress", ErrTooManyUploadSessions, len(s.sessions))
		}
		if chunkBudgetExceeded(s.sessions, totalChunks) {
			s.lock.Unlock()
			return fmt.Errorf("%w: adding %d chunks would pass the %d-chunk budget", ErrTooManyUploadSessions, totalChunks, MaxLiveUploadChunks)
		}
		// Fresh session. A leftover tmp from a lost registry is unusable
		// (its bitmap is gone) — clear it and create exclusively.
		os.Remove(tmpPath)
		f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			s.lock.Unlock()
			return err
		}
		// Pre-allocate the destination to its final size so the filesystem
		// reserves blocks — faster writes and early ENOSPC detection.
		if err := f.Truncate(totalSize); err != nil {
			f.Close()
			os.Remove(tmpPath)
			s.lock.Unlock()
			return fmt.Errorf("preallocate %d bytes: %w", totalSize, err)
		}
		fileInfo = &FileInfo{
			init:             true,
			uploaded:         make([]bool, totalChunks),
			uploadedChunkNum: 0,
		}
		s.uploadStatus.Store(identifier, fileInfo)
		// Register the session BEFORE any data lands: a crash mid-first-chunk
		// must never leave an untracked pre-allocated tmp behind.
		s.sessions[identifier] = &uploadSession{
			TmpPath: tmpPath, TotalChunks: totalChunks,
			ChunkSize: chunkSize, TotalSize: totalSize,
			// Record the validated upload root. It is what lets a LATER start
			// of the daemon re-run this request's containment check before it
			// renames anything (see rehydrateDestination); a session without it
			// can never be finalized from the registry.
			Root:      basePath,
			Uploaded:  make([]bool, totalChunks),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		persistUploadSessions(s.sessions)
		file = f
	} else {
		fi, assertOk := fileInfoTemp.(*FileInfo)
		if !assertOk {
			s.lock.Unlock()
			return fmt.Errorf("upload state for %s is corrupt", identifier)
		}
		// Resume validation: the request's grid must match the recorded
		// session AND the tmp must still be the exact pre-allocated file the
		// bitmap describes. Anything else ⇒ drop the session and make the
		// client restart (ErrSessionReset → 409).
		sess := s.sessions[identifier]
		valid := int64(len(fi.uploaded)) == totalChunks &&
			sess != nil && sess.ChunkSize == chunkSize &&
			sess.TotalSize == totalSize && sess.TmpPath == tmpPath &&
			sessionTmpValid(sess)
		if !valid {
			s.uploadStatus.Delete(identifier)
			if sess != nil {
				// Gated like every other deletion of a registry-supplied path:
				// this one used to unlink sess.TmpPath unconditionally, so a
				// registry entry written by the vulnerable build (or edited by
				// hand) turned a mismatched resume into an arbitrary-file
				// delete, reachable by anyone holding PermFileWrite.
				removeSessionTmp(identifier, sess.Root, sess.TmpPath)
			}
			delete(s.sessions, identifier)
			persistUploadSessions(s.sessions)
			s.lock.Unlock()
			return fmt.Errorf("%w: state mismatch for %s", ErrSessionReset, identifier)
		}
		// Carry the upload root forward for sessions created before it was
		// recorded. It can only be backfilled once the session has passed the
		// full live validation above, so a poisoned entry never acquires one.
		if sess.Root != basePath {
			sess.Root = basePath
			persistUploadSessions(s.sessions)
		}
		// NO O_CREATE on resume: the tmp was just validated to exist; letting
		// the open recreate a vanished file would resurrect the zeros bug.
		// O_NOFOLLOW closes the last-component swap: sessionTmpValid Lstat'd a
		// regular file a moment ago, and without this flag a symlink dropped in
		// that window would be followed and written through.
		f, err := os.OpenFile(tmpPath, os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			s.lock.Unlock()
			return err
		}
		fileInfo = fi
		file = f
	}
	s.lock.Unlock()

	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	if _, err := file.Seek((chunkNumber-1)*chunkSize, io.SeekStart); err != nil {
		return err
	}

	src, err := bin.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	// LimitReader caps the write at the declared (validated) size, so even a
	// crafted multipart part larger than its header claims cannot overrun
	// into the next chunk's region. Short bodies are a hard error — the
	// region would keep pre-allocated zeros and corrupt the file silently.
	n, err := io.Copy(file, io.LimitReader(src, currentChunkSize))
	if err != nil {
		return err
	}
	if n != currentChunkSize {
		return fmt.Errorf("chunk %d short write: got %d bytes, want %d", chunkNumber, n, currentChunkSize)
	}

	// Durability barrier: the chunk's bytes must be ON DISK before the bitmap
	// (RAM + registry) says "present". Without this, the small session JSON
	// can commit before the 32-80MB of data still sitting in page cache — a
	// power cut then resumes past a chunk whose region is pre-allocated
	// zeros, and nothing downstream would ever notice.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync chunk %d: %w", chunkNumber, err)
	}

	s.lock.Lock()
	// Idempotent: a duplicate chunk (retry) must not double-count.
	isComplete := false
	newChunk := false
	if !fileInfo.uploaded[chunkNumber-1] {
		fileInfo.uploaded[chunkNumber-1] = true
		fileInfo.uploadedChunkNum++
		newChunk = true
	}
	if fileInfo.uploadedChunkNum == totalChunks && !fileInfo.finalizing {
		// Claim finalization under the lock so a parallel duplicate retry
		// can't double-rename; the session itself is deleted only AFTER the
		// rename succeeds (crash between = session survives, client retries).
		fileInfo.finalizing = true
		isComplete = true
	}
	// Mirror progress durably (small JSON; one write per 32-80MB chunk).
	//
	// Only when the bitmap actually MOVED, though. The registry is written as
	// one document, so the cost of a write is proportional to every live
	// session — and a client re-POSTing a chunk it already sent changes nothing
	// on disk, which made "one byte in, whole registry out" a free amplifier
	// with no upload behind it. The timestamp is still refreshed periodically
	// so a long retry storm can never age a live session into the reaper.
	if sess := s.sessions[identifier]; sess != nil && (newChunk || sessionNeedsTouch(sess)) {
		sess.Uploaded = append(sess.Uploaded[:0], fileInfo.uploaded...)
		sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		persistUploadSessions(s.sessions)
	}
	s.lock.Unlock()

	// Close explicitly so the fd is released before the rename. Errors are a
	// hard failure — and must release the finalizing claim for the retry.
	if closeErr := file.Close(); closeErr != nil {
		closed = true
		if isComplete {
			s.lock.Lock()
			fileInfo.finalizing = false
			s.lock.Unlock()
		}
		return fmt.Errorf("close upload chunk: %w", closeErr)
	}
	closed = true

	if isComplete {
		// Finalize: move the tmp into place (EXDEV-safe), THEN drop the
		// session. If the rename fails or we crash first, the session and
		// tmp survive intact and the client's retry finalizes again.
		if err := pkgfile.SafeRename(tmpPath, destPath); err != nil {
			logger.Error("finalize upload rename", zap.Error(err), zap.String("path", cleanRel))
			s.lock.Lock()
			fileInfo.finalizing = false
			s.lock.Unlock()
			return err
		}
		s.lock.Lock()
		s.uploadStatus.Delete(identifier)
		delete(s.sessions, identifier)
		persistUploadSessions(s.sessions)
		s.lock.Unlock()
	}

	return nil
}
