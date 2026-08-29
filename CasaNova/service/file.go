/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2021-12-20 14:15:46
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-04 16:18:23
 * @FilePath: /CasaOS/service/file.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/moby/sys/mountinfo"
	"go.uber.org/zap"
)

var FileQueue sync.Map

// OpMu protects OpStrArr from concurrent read/write across HTTP
// handlers and background goroutines (ExecOpFile, CheckFileStatus).
// Without this, concurrent POST /v1/batch/task and DELETE /v1/batch/:id/task
// requests race on the slice and corrupt in-flight operations.
var OpMu sync.Mutex
var OpStrArr []string

// RemoveOp drops one operation id from the queue by identity.
//
// The notify loop iterates a *snapshot* of OpStrArr and, when an entry
// finishes, has to remove that entry — not whichever one happens to sit
// at the front. Popping OpStrArr[0] instead dropped an unrelated
// in-flight copy whenever operations completed out of order, and
// panicked on a slice bound once the queue had already drained.
func RemoveOp(id string) {
	OpMu.Lock()
	defer OpMu.Unlock()
	if len(OpStrArr) == 0 {
		return
	}
	kept := OpStrArr[:0:0]
	for _, v := range OpStrArr {
		if v != id {
			kept = append(kept, v)
		}
	}
	OpStrArr = kept
}

// opProgress tracks live byte counts per in-flight operation ID so the
// status endpoint can publish progress without polling the destination
// filesystem every 3 seconds (which caused 0 → 100% jumps for any file
// that copied faster than the poll interval).
var opProgress sync.Map // map[opID]*file.ProgressCounter

type reader struct {
	ctx context.Context
	r   io.Reader
}

// NewReader wraps an io.Reader to handle context cancellation.
//
// Context state is checked BEFORE every Read.
func NewReader(ctx context.Context, r io.Reader) io.Reader {
	if r, ok := r.(*reader); ok && ctx == r.ctx {
		return r
	}
	return &reader{ctx: ctx, r: r}
}

func (r *reader) Read(p []byte) (n int, err error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

type writer struct {
	ctx context.Context
	w   io.Writer
}

type copier struct {
	writer
}

func NewWriter(ctx context.Context, w io.Writer) io.Writer {
	if w, ok := w.(*copier); ok && ctx == w.ctx {
		return w
	}
	return &copier{writer{ctx: ctx, w: w}}
}

// Write implements io.Writer, but with context awareness.
func (w *writer) Write(p []byte) (n int, err error) {
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	default:
		return w.w.Write(p)
	}
}

// opControl is the cancellation handle that lives alongside an operation's
// FileQueue entry. Before it existed, cancelling an operation only mutated the
// tracking (FileQueue + OpStrArr): the copy goroutine kept running, so a
// cancelled 600 GB copy went on filling the disk while the dashboard showed
// nothing, a "cancelled" move still deleted its source once it completed, and
// the surviving goroutine's final FileQueue.Store resurrected an entry the UI
// had already dropped and could never clear again.
type opControl struct {
	ctx    context.Context
	cancel context.CancelFunc

	// claimed is won exactly once, by whoever takes responsibility for the
	// operation: the worker that runs it, or the canceller that guarantees no
	// worker ever will. The winner is the one that closes done.
	claimed atomic.Bool

	// done is closed once the operation is guaranteed not to touch the
	// filesystem any more. The queue must not advance before that: two workers
	// running at once is what "cancel then start the next one" used to do.
	done chan struct{}

	// cancelRequested marks operations the operator (or the staleness path)
	// asked to stop. It survives the worker's unwind window so the notify loop
	// can report CANCELLING instead of re-issuing a cancel every tick.
	cancelRequested atomic.Bool

	// removed marks tracking that has already been dropped, so a late writer
	// cannot re-Store the entry and resurrect a ghost operation.
	removed atomic.Bool

	// retired is set inside finishOp's critical section, before done is closed.
	// It is what lets a late wedge decision tell "the worker is still in the
	// kernel" from "the worker just left": a reservation recorded for a worker
	// that already let go would never be cleaned up, and would block its tree
	// for the lifetime of the daemon.
	retired atomic.Bool

	// wedged is won once, by whoever decides this worker is parked rather than
	// slow. See the "Parked workers" block below.
	wedged atomic.Bool

	// probing guards the one in-flight liveness probe per operation, so a
	// stalled operation cannot spawn a probe goroutine on every 3s tick.
	probing atomic.Bool

	// paths is the set of paths this worker owns, captured when it claimed the
	// operation. It is captured at claim time rather than read from FileQueue on
	// demand because the tracking entry can be dropped while the worker is still
	// running (an early FINISHED retire), and a reservation that does not know
	// what it is reserving is worse than none.
	//
	// ATOMIC, AND PUBLISHED AFTER THE CLAIM — NOT BEFORE.
	//
	// The doc here used to say it was written before the claim became visible,
	// and beginOp's comment said the same. Neither was true: the claim CAS is
	// what confers ownership, so writing this first would mean writing into a
	// control this goroutine had not yet won. The order is correct and the
	// comments were wrong, which left a plain []string being written by beginOp
	// while considerWedge read it on the monitor goroutine with nothing
	// ordering the two.
	//
	// So the ordering is stated as it is: claim, then publish. Readers must
	// tolerate the window in between (see considerWedge), and the field is
	// atomic so that tolerating it is a defined operation rather than a race.
	paths atomic.Pointer[[]string]

	// lastBytes / stillSince track byte movement so a worker that is stuck can
	// be told from one that is merely slow, without putting a clock budget on
	// the copy itself. stillSince is unix nanoseconds; zero means "not started".
	lastBytes  atomic.Int64
	stillSince atomic.Int64
}

// noteProgress records the operation's byte count and answers how long it has
// been sitting at the same number.
func (c *opControl) noteProgress(done int64, now time.Time) time.Duration {
	since := c.stillSince.Load()
	if since == 0 {
		c.lastBytes.Store(done)
		c.stillSince.Store(now.UnixNano())
		return 0
	}
	if c.lastBytes.Swap(done) != done {
		c.stillSince.Store(now.UnixNano())
		return 0
	}
	return now.Sub(time.Unix(0, since))
}

// opTrackMu serializes every mutation of an operation's tracking state
// (FileQueue entry, OpStrArr membership, opControls, activeOp) so that
// "cancel drops the entry" and "the worker publishes progress" cannot
// interleave into a resurrected entry.
//
// Lock ordering is opTrackMu -> OpMu (RemoveOp). Never the reverse.
var opTrackMu sync.Mutex

// opControls maps operation id -> *opControl for every registered operation.
var opControls sync.Map

// activeOp is the control of the worker currently touching the filesystem.
// One operation runs at a time, and a cancelled worker keeps that slot until it
// has unwound.
//
// Exception, and the only one: a worker declared WEDGED (see below) is demoted
// out of this slot while it is still inside the kernel. It does not stop owning
// its files — its tree stays reserved through wedgedOps — but it stops owning
// the whole queue.
var activeOp *opControl

// CancelUnwindWait bounds how long a cancel request waits for the worker to
// actually stop before answering the caller. It is deliberately short: an
// HTTP handler must not hang on a write to a dead network mount. Timing out
// does NOT mean "cancelled" — the entry stays visible as CANCELLING and the
// queue stays blocked until the worker really unwinds, because reporting a
// stop that did not happen is exactly the lie this whole change removes.
const CancelUnwindWait = 5 * time.Second

func newOpControl() *opControl {
	ctx, cancel := context.WithCancel(context.Background())
	return &opControl{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func loadOpControl(id string) (*opControl, bool) {
	raw, ok := opControls.Load(id)
	if !ok {
		return nil, false
	}
	ctl, ok := raw.(*opControl)
	return ctl, ok
}

// RegisterOp attaches a cancellation handle to an operation that has just been
// put in FileQueue. It must be called before the enqueuing request returns, so
// that a cancel can never arrive for an operation that has no way to be
// stopped.
func RegisterOp(id string) {
	opTrackMu.Lock()
	defer opTrackMu.Unlock()
	if _, exists := loadOpControl(id); exists {
		return
	}
	opControls.Store(id, newOpControl())
}

// dropOpTrackingLocked removes an operation from every tracking structure.
// Caller holds opTrackMu.
func dropOpTrackingLocked(id string) {
	if ctl, ok := loadOpControl(id); ok {
		ctl.removed.Store(true)
	}
	FileQueue.Delete(id)
	RemoveOp(id)
}

// ReapOp drops the tracking of an operation that has finished on its own.
func ReapOp(id string) {
	opTrackMu.Lock()
	defer opTrackMu.Unlock()
	dropOpTrackingLocked(id)
}

// storeOpIfLive publishes an updated operation entry, but only while the
// operation is still tracked. A plain FileQueue.Store from the worker or the
// progress poller re-created entries that had just been cancelled: the UI got
// an operation it could not cancel again (its control was gone) and which no
// longer had anything running behind it.
func storeOpIfLive(id string, v model.FileOperate) bool {
	opTrackMu.Lock()
	defer opTrackMu.Unlock()
	if ctl, ok := loadOpControl(id); ok && ctl.removed.Load() {
		return false
	}
	if _, ok := FileQueue.Load(id); !ok {
		return false
	}
	FileQueue.Store(id, v)
	return true
}

// beginOp claims an operation for execution. It answers one of three states:
// claimed (run it), busy (another worker owns the filesystem — wait on its
// control and retry) or gone (the entry was cancelled or already taken).
func beginOp(id string) (entry model.FileOperate, ctl *opControl, busy *opControl, ok bool) {
	opTrackMu.Lock()
	defer opTrackMu.Unlock()

	if activeOp != nil {
		return entry, nil, activeOp, false
	}
	raw, exists := FileQueue.Load(id)
	if !exists {
		return entry, nil, nil, false
	}
	queued, isOp := raw.(model.FileOperate)
	if !isOp {
		return entry, nil, nil, false
	}
	paths := opPathsOf(queued)
	// A parked worker still owns its tree even though it no longer owns the
	// queue. Refusing here is what keeps "the queue may go on with operations
	// that do NOT touch that tree" from becoming "two workers on one
	// destination". The entry stays queued and is retried whenever the queue is
	// pumped, so it starts on its own once the worker lets go.
	if reserved, blocked := wedgeConflictLocked(paths); blocked {
		logger.Info("file operation stays queued: its tree is still owned by a parked worker",
			zap.String("id", id), zap.String("reserved", reserved))
		return entry, nil, nil, false
	}
	ctl, exists = loadOpControl(id)
	if !exists {
		// Defensive: an entry queued by a path that did not register a control
		// must still be runnable, otherwise the whole queue stalls behind it.
		ctl = newOpControl()
		opControls.Store(id, ctl)
	}
	if ctl.removed.Load() || !ctl.claimed.CompareAndSwap(false, true) {
		return entry, nil, nil, false
	}
	// The claim above is the ownership edge; only now is it safe to write here.
	// Until this store lands, a wedge declaration cannot name the tree it would
	// have to reserve, and considerWedge is required to back off rather than
	// record a reservation over nothing.
	ctl.paths.Store(&paths)
	ctl.stillSince.Store(time.Now().UnixNano())
	activeOp = ctl
	return queued, ctl, nil, true
}

// finishOp is the worker's only exit path. It releases the filesystem slot,
// retires the control, and — for a cancelled operation — drops the tracking
// and lets the queue move on now that nothing is running any more.
func finishOp(id string, ctl *opControl) {
	opTrackMu.Lock()
	// Publish "this worker is out of the kernel" before anything else can
	// observe the control, so a wedge decision racing with this exit refuses
	// instead of recording a reservation nobody would ever clear.
	ctl.retired.Store(true)
	if activeOp == ctl {
		activeOp = nil
	}
	opControls.Delete(id)
	aborted := ctl.cancelRequested.Load()
	// The worker has let go of the filesystem, so its tree is free again. This
	// is the ONLY place a reservation is released: the dashboard entry can be
	// retired long before the worker exits, and the reservation must outlive it.
	wasWedged := ctl.wedged.Load()
	wedgedOps.Delete(id)
	if aborted {
		dropOpTrackingLocked(id)
	}
	opTrackMu.Unlock()

	close(ctl.done)

	if wasWedged {
		logger.Info("parked file operation worker came back and let go of its tree",
			zap.String("id", id))
	}
	if aborted || wasWedged {
		go ExecOpFile()
	}
}

// CancelOp stops an in-flight or queued operation for real.
//
// It returns true only when the operation is guaranteed to be off the
// filesystem and its tracking has been dropped. false means "cancellation
// requested, worker still unwinding": the entry deliberately stays in the
// queue (reported as CANCELLING) and the queue does NOT advance, because the
// worker still owns the destination. The worker's own unwind finishes the job.
func CancelOp(id string, wait time.Duration) bool {
	opTrackMu.Lock()
	ctl, ok := loadOpControl(id)
	if !ok {
		// Nothing is running behind this entry (already reaped, or it never
		// had a worker): dropping the tracking is the whole cancellation.
		dropOpTrackingLocked(id)
		opTrackMu.Unlock()
		go ExecOpFile()
		return true
	}
	ctl.cancelRequested.Store(true)
	ctl.cancel()
	unclaimed := ctl.claimed.CompareAndSwap(false, true)
	if unclaimed {
		// We won the claim, so no worker will ever run this operation (and
		// none is running it now — activeOp is only set by a winning claim),
		// which also means nobody else will close done. Retire it here.
		opControls.Delete(id)
		dropOpTrackingLocked(id)
	}
	opTrackMu.Unlock()

	if unclaimed {
		close(ctl.done)
		// Nothing was running, so no worker unwind will do it: the queue has
		// to be pushed forward from here or a cancel of the head entry would
		// leave the rest of the queue parked for ever.
		go ExecOpFile()
		return true
	}

	select {
	case <-ctl.done:
		// finishOp already advanced the queue; harmless to insist, since
		// starting an operation is guarded by the same claim.
		go ExecOpFile()
		return true
	case <-time.After(wait):
		logger.Error("file operation did not stop within the cancellation window; it stays visible as CANCELLING while the queue waits for it to unwind",
			zap.String("id", id), zap.Duration("waited", wait))
		// The caller must not be kept waiting, but the queue must not wait for
		// ever either: escalate in the background. If the worker is still in the
		// kernel once the whole window has passed, it is reported as WEDGED and
		// the queue moves on with everything that does not touch its tree.
		watchForWedge(id, ctl, WedgeAfter-wait)
		return false
	}
}

// CancelAllOps cancels every tracked operation. Used by the admin-only queue
// reset, which used to wipe the tracking while the workers kept running.
func CancelAllOps(wait time.Duration) {
	ids := map[string]struct{}{}
	OpMu.Lock()
	for _, id := range OpStrArr {
		ids[id] = struct{}{}
	}
	OpMu.Unlock()
	FileQueue.Range(func(k, _ any) bool {
		if id, ok := k.(string); ok {
			ids[id] = struct{}{}
		}
		return true
	})
	for id := range ids {
		CancelOp(id, wait)
	}
}

// OpCancelPending reports an operation whose cancellation has been requested
// but whose worker has not finished unwinding yet.
func OpCancelPending(id string) bool {
	ctl, ok := loadOpControl(id)
	return ok && ctl.cancelRequested.Load()
}

// ---------------------------------------------------------------------------
// Parked ("wedged") workers.
//
// A file worker can end up blocked in a syscall that no context can interrupt:
// a hard-mounted NFS/SMB share whose server leaves the network parks read(2)
// and write(2) in uninterruptible sleep, with no timeout at all. This box mounts
// remote shares, so that is an ordinary event, not an exotic one.
//
// The queue is deliberately conservative about such a worker: it still owns its
// destination, so nothing may run over the same tree, and reporting it as
// stopped would be a lie (see CancelOp). What was missing is a BOUND. A worker
// that never unwound held the single execution slot for ever, so every other
// file operation on the box — including operations on unrelated local disks —
// stayed parked behind it, showing CANCELLING for ever, with no operator action
// able to change that.
//
// Past that bound the queue stops predicting that the worker is about to stop
// and starts reporting a fact:
//   - the operation is reported as WEDGED, naming the path and the mount point
//     that stopped answering, instead of CANCELLING for ever;
//   - the worker is demoted out of the execution slot, so unrelated operations
//     run again;
//   - the tree it owns — and the unresponsive mount holding it — stay RESERVED.
//     This is the part that must not be weakened: the worker can return from its
//     syscall at any instant and go on writing, so anything that could touch
//     those paths keeps waiting exactly as it did before.
//
// Declaring an operation wedged NEVER cancels it and never touches data. It is a
// reporting and scheduling decision only: a stuck report must not be allowed to
// break a working operation.
const (
	// WedgeReasonCancelUnwind: a cancellation was requested and the worker did
	// not let go of the filesystem within WedgeAfter.
	WedgeReasonCancelUnwind = "cancel-unwind"

	// WedgeReasonUnresponsivePath: the operation stopped moving bytes and a
	// plain stat(2) on the path it works on does not come back either.
	WedgeReasonUnresponsivePath = "unresponsive-path"
)

// WedgeAfter bounds how long a requested cancellation may stay "about to take
// effect" before the queue treats the worker as parked. It is generous on
// purpose: a worker unwinding a large staged copy on a healthy but busy disk
// must be allowed to finish, because declaring it wedged releases the queue slot
// and there is no way to take that back.
//
// Variables, not constants, so tests can compress the windows. Nothing outside
// tests writes them.
var (
	WedgeAfter = 60 * time.Second

	// OpStallProbeAfter is how long an operation may sit at the same byte count
	// before its paths are probed for liveness. Zero byte movement on its own
	// proves nothing — retiring a large destination to the recycle bin walks the
	// whole tree without copying a byte — so this only triggers the probe, never
	// a verdict.
	OpStallProbeAfter = 60 * time.Second

	// wedgeProbeTimeout bounds the liveness probe itself.
	wedgeProbeTimeout = 3 * time.Second
)

// WedgedOp describes an operation whose worker is parked inside the kernel. It
// is what the transfer panel and the health/diagnosis surface report instead of
// an operation that says CANCELLING for ever.
type WedgedOp struct {
	ID     string    `json:"id"`
	Type   string    `json:"type"`
	Holder string    `json:"holder"` // path the parked worker still owns
	Mount  string    `json:"mount"`  // mount point holding it, "" if unknown
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// wedgeRecord is the reservation behind a WedgedOp: the paths that stay off
// limits until the worker really lets go.
type wedgeRecord struct {
	info  WedgedOp
	paths []string
}

// wedgedOps maps operation id -> *wedgeRecord. Mutated only under opTrackMu,
// and only ever removed by finishOp — the worker's single exit path. It is NOT
// removed when the tracking entry is dropped: an operation can be retired from
// the dashboard while its worker is still in the kernel, and the reservation has
// to outlive the dashboard entry or the next operation would be started on top
// of a tree somebody still owns.
var wedgedOps sync.Map

// opPathsOf lists every path an operation can touch: its destination directory
// and each source item. Destinations of individual items live under To, so To
// covers them.
func opPathsOf(op model.FileOperate) []string {
	paths := make([]string, 0, len(op.Item)+1)
	if op.To != "" {
		paths = append(paths, filepath.Clean(op.To))
	}
	for _, item := range op.Item {
		if item.From != "" {
			paths = append(paths, filepath.Clean(item.From))
		}
	}
	return paths
}

// wedgeConflictLocked reports the reserved path that would collide with the
// given paths, if any. Overlap is tested in both directions: a parked worker
// writing into /DATA/media conflicts with an operation on /DATA/media/films
// (below it) just as much as with one on /DATA (above it).
//
// Caller holds opTrackMu.
func wedgeConflictLocked(paths []string) (string, bool) {
	conflict := ""
	found := false
	wedgedOps.Range(func(_, v any) bool {
		rec, ok := v.(*wedgeRecord)
		if !ok {
			return true
		}
		for _, reserved := range rec.paths {
			for _, p := range paths {
				if pathContains(reserved, p) || pathContains(p, reserved) {
					conflict, found = reserved, true
					return false
				}
			}
		}
		return true
	})
	return conflict, found
}

// mountHolding returns the deepest mount point that contains path, so the
// operator is told which mount stopped answering rather than just which file.
//
// Reading /proc/self/mountinfo does not touch the dead filesystem, so this is
// safe to call about a mount that is not answering — which is the only time it
// is called. A variable so tests can describe a mount table without one.
var mountHolding = func(path string) string {
	entries, err := mountinfo.GetMounts(nil)
	if err != nil {
		return ""
	}
	best := ""
	for _, m := range entries {
		if pathContains(m.Mountpoint, path) && len(m.Mountpoint) > len(best) {
			best = m.Mountpoint
		}
	}
	return best
}

// probePathAlive answers whether the filesystem holding path still responds,
// within timeout. "Responds" means it answered at all: ENOENT or EACCES are
// answers, and a filesystem that answers is not the problem being detected here.
//
// The probe goroutine is deliberately abandoned when it times out. It is blocked
// in the same uninterruptible syscall as the worker and can only return if the
// mount ever comes back — one parked goroutine per dead mount is the price of
// asking the question at all, and the alternative (guessing from byte counters)
// mistakes a slow operation for a dead one.
//
// A variable so tests can simulate a dead mount without mounting one.
var probePathAlive = func(path string, timeout time.Duration) bool {
	answered := make(chan struct{})
	go func() {
		_, _ = os.Stat(path)
		close(answered)
	}()
	select {
	case <-answered:
		return true
	case <-time.After(timeout):
		return false
	}
}

// markWedged records that a worker is parked and releases the execution slot it
// was holding. It returns whether the declaration was made.
//
// It refuses when the operation was already declared parked (the decision is
// taken once), and in two cases that must fail closed — keeping the slot held is
// the old, conservative behaviour and is never worse than what we had:
//   - the worker has already retired (it let go while this decision was being
//     made): a reservation nobody will ever clear would block its tree for the
//     lifetime of the daemon;
//   - the worker never recorded which paths it owns: releasing the queue without
//     being able to reserve the tree is exactly how two workers end up on one
//     destination.
//
// The mount is looked up for the report even when the path is on a local disk;
// only the reservation treats "/" specially.
func markWedged(id string, ctl *opControl, reason, holder string) bool {
	// One load, used for every decision below. Re-reading the pointer per use
	// would let the "is it published" test and the reservation disagree.
	owned := []string(nil)
	if pp := ctl.paths.Load(); pp != nil {
		owned = *pp
	}
	if holder == "" && len(owned) > 0 {
		holder = owned[0]
	}
	// Off the lock: this reads /proc, and opTrackMu is on the path of every
	// progress publication.
	mount := ""
	if holder != "" {
		mount = mountHolding(holder)
	}

	opTrackMu.Lock()
	if ctl.retired.Load() || len(owned) == 0 || !ctl.wedged.CompareAndSwap(false, true) {
		opTrackMu.Unlock()
		return false
	}
	reserved := append([]string{}, owned...)
	// Reserve the whole mount as well, because a worker parked on it is evidence
	// that the mount is not answering and any other operation sent there would
	// park too. "/" is excluded: everything lives under it, so reserving it would
	// re-create the very stall this is here to end, and a wedge on a local root
	// filesystem is better served by reserving just the tree.
	if mount != "" && mount != string(os.PathSeparator) {
		reserved = append(reserved, mount)
	}
	rec := &wedgeRecord{
		info: WedgedOp{
			ID:     id,
			Holder: holder,
			Mount:  mount,
			Reason: reason,
			Since:  time.Now(),
		},
		paths: reserved,
	}
	if raw, ok := FileQueue.Load(id); ok {
		if op, isOp := raw.(model.FileOperate); isOp {
			rec.info.Type = op.Type
		}
	}
	if activeOp == ctl {
		activeOp = nil
	}
	wedgedOps.Store(id, rec)
	opTrackMu.Unlock()

	logger.Error("file operation worker is parked inside the kernel; releasing the queue and reserving its tree",
		zap.String("id", id), zap.String("reason", reason), zap.String("holder", holder),
		zap.String("mount", mount), zap.Strings("reserved", reserved))

	// Unrelated work can run again. Anything targeting the reserved tree stays
	// queued, exactly as it did while the worker held the slot.
	go ExecOpFile()
	return true
}

// watchForWedge escalates a cancellation that never lands. It is started when a
// cancel request has already outlived its unwind window, and it waits for the
// rest of WedgeAfter before declaring the worker parked. At most one watcher
// runs per operation.
func watchForWedge(id string, ctl *opControl, after time.Duration) {
	if !ctl.probing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		if after > 0 {
			select {
			case <-ctl.done:
				ctl.probing.Store(false)
				return
			case <-time.After(after):
			}
		}
		select {
		case <-ctl.done:
			ctl.probing.Store(false)
			return
		default:
		}
		if !markWedged(id, ctl, WedgeReasonCancelUnwind, "") {
			ctl.probing.Store(false)
		}
	}()
}

// considerWedge probes an operation that has stopped moving bytes, and declares
// it wedged only if the filesystem it works on does not answer.
//
// The probe is what separates this from the staleness rule above it: a healthy
// move can sit at zero bytes for minutes (trashDestination walks the whole
// destination tree before the payload is published), and treating that as a
// wedge would take a working operation out of the queue slot on the strength of
// a stopwatch. Asking the filesystem directly is the difference between "slow"
// and "gone" — and this path still never cancels anything, so the worst a false
// positive can do is let an unrelated operation start early.
//
// Only a claimed worker is considered: a queued operation moves no bytes by
// definition, and has no worker holding anything to report.
func considerWedge(id string) {
	ctl, ok := loadOpControl(id)
	if !ok || !ctl.claimed.Load() || ctl.retired.Load() || ctl.wedged.Load() {
		return
	}
	if !ctl.probing.CompareAndSwap(false, true) {
		return
	}
	// Sample the probe policy here rather than inside the goroutine, so one
	// attempt uses one policy end to end (and so the goroutine reads nothing
	// that could change under it while it is blocked on a dead mount).
	probe, timeout := probePathAlive, wedgeProbeTimeout

	// Between the claim and the publish, this worker owns a tree it has not
	// named yet. Releasing the probe slot and returning is what makes that
	// window harmless: the next tick sees the paths and probes properly.
	//
	// Proceeding instead would iterate an empty list, declare nothing, and
	// leave probing latched at true forever — so a worker that really did wedge
	// on a dead mount would never be examined again. A missed detection is the
	// expensive failure here, not a skipped tick.
	pp := ctl.paths.Load()
	if pp == nil || len(*pp) == 0 {
		ctl.probing.Store(false)
		return
	}
	paths := *pp
	go func() {
		for _, p := range paths {
			if probe(p, timeout) {
				continue
			}
			if markWedged(id, ctl, WedgeReasonUnresponsivePath, p) {
				return
			}
			break
		}
		// Everything answered (or the declaration was refused): let a later tick
		// ask again, since the mount may die after this probe.
		ctl.probing.Store(false)
	}()
}

// OpWedged returns the parked-worker record of one operation.
func OpWedged(id string) (WedgedOp, bool) {
	raw, ok := wedgedOps.Load(id)
	if !ok {
		return WedgedOp{}, false
	}
	rec, ok := raw.(*wedgeRecord)
	if !ok {
		return WedgedOp{}, false
	}
	return rec.info, true
}

// DiscardWedgeReservation releases the tree a parked worker is holding, on an
// operator's explicit say-so. It returns the record that was discarded.
//
// WHY THIS HAS TO EXIST.
//
// A reservation is removed in exactly one place — finishOp, the worker's only
// exit path — and a worker parked in the kernel on a mount that never comes
// back never reaches it. That is correct while the mount might return, and
// permanent when it will not: an NFS export that was decommissioned leaves
// every file operation under that mountpoint refused for the lifetime of the
// process, with no way out but restarting the service. Making the operator
// restart CasaNova to move a file is not a recovery path, it is the absence of
// one.
//
// WHY IT CANCELS BEFORE IT RELEASES.
//
// Releasing the tree lets a NEW operation start on paths the parked worker
// still believes it owns, which is precisely what the reservation exists to
// prevent. So the operation is cancelled FIRST — cancelRequested and the
// context, in that order, matching cancelOp — and only then is the tree freed.
// A worker that later wakes up finds itself cancelled and unwinds through
// finishOp instead of resuming its copy. The delete there becomes a no-op,
// which is why the control is deliberately left intact here.
//
// THE RESIDUAL, STATED PLAINLY.
//
// This does not kill the worker: a thread blocked in the kernel cannot be
// interrupted from user space. If the mount returns, the single syscall already
// in flight completes before the worker can observe the cancellation, so one
// in-flight chunk may still be written to the old destination. That window is
// bounded by one operation and cannot be closed without killing the process.
// It is the reason this is an explicit operator action with its own endpoint
// rather than something a timer does quietly.
func DiscardWedgeReservation(id string) (WedgedOp, bool) {
	raw, ok := wedgedOps.Load(id)
	if !ok {
		return WedgedOp{}, false
	}
	rec, ok := raw.(*wedgeRecord)
	if !ok {
		return WedgedOp{}, false
	}

	// Cancel first. See above: the order is the whole safety argument.
	if ctl, live := loadOpControl(id); live {
		ctl.cancelRequested.Store(true)
		ctl.cancel()
	}

	opTrackMu.Lock()
	wedgedOps.Delete(id)
	// Drop the dashboard entry too, in the SAME critical section.
	//
	// Releasing only the reservation left the row behind: OpWedged then missed,
	// the snapshot fell through to OpCancelPending, and the operation showed as
	// CANCELLING for ever with no ProcessingPath — so the discard destroyed the
	// one place the box named the dead mount, and nothing could clear the row
	// because ctl.wedged and ctl.probing stay latched and finishOp is exactly
	// the path that will never run. Trading a truthful WEDGED row for a
	// permanent lying one is not an improvement.
	dropOpTrackingLocked(id)
	opTrackMu.Unlock()
	return rec.info, true
}

// WedgedFileOps lists every operation whose worker is currently parked, oldest
// first, for the health and diagnosis surfaces. It is a snapshot: a worker can
// come back at any moment, and its record disappears when it does.
func WedgedFileOps() []WedgedOp {
	out := []WedgedOp{}
	wedgedOps.Range(func(_, v any) bool {
		if rec, ok := v.(*wedgeRecord); ok {
			out = append(out, rec.info)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			return out[i].ID < out[j].ID
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}

// OpStalledFor records an operation's byte count and reports how long it has
// been sitting at the same number. Unclaimed operations report zero: they are
// waiting, not stalled.
func OpStalledFor(id string, processed int64) time.Duration {
	ctl, ok := loadOpControl(id)
	if !ok || !ctl.claimed.Load() {
		return 0
	}
	return ctl.noteProgress(processed, time.Now())
}

// OpQueueIdle reports that no worker holds the execution slot.
func OpQueueIdle() bool {
	opTrackMu.Lock()
	defer opTrackMu.Unlock()
	return activeOp == nil
}

func FileOperate(k string) {
	var temp model.FileOperate
	var ctl *opControl
	for {
		entry, claimed, busy, ok := beginOp(k)
		if ok {
			temp, ctl = entry, claimed
			break
		}
		if busy == nil {
			// Cancelled, already running, or the entry is gone.
			return
		}
		// Never two workers on the filesystem at once: wait for the operation
		// that currently owns it — including one that is unwinding a cancel —
		// before taking the slot.
		<-busy.done
	}
	defer finishOp(k, ctl)
	ctx := ctl.ctx

	if temp.Finished || temp.ProcessedSize > 0 {
		return
	}

	// Work on a private item list. `temp` is a struct copy of the queue entry,
	// but Item is a slice: its backing array is the SAME one the progress
	// poller and the notify broadcast read on every tick, and marking items
	// finished in place is an unsynchronised write under those reads (a data
	// race the race detector flags, and torn state the dashboard can read).
	// The finished flags reach the map when the operation publishes its result.
	items := make([]model.FileItem, len(temp.Item))
	copy(items, temp.Item)
	temp.Item = items

	// Set up a live byte counter for this operation. CheckFileStatus will
	// read this counter atomically instead of stat'ing the destination
	// every 3 seconds — the old approach made progress jump 0 → 100% for
	// any file faster than the poll interval.
	counter := &file.ProgressCounter{}
	opProgress.Store(k, counter)
	defer opProgress.Delete(k)

	for i := 0; i < len(temp.Item); i++ {
		// Stop between items too: a tree of many small files must react to a
		// cancel as fast as a single huge one.
		if ctx.Err() != nil {
			break
		}
		v := temp.Item[i]
		if temp.Type == "move" {
			lastPath := v.From[strings.LastIndex(v.From, "/")+1:]
			dst := temp.To + "/" + lastPath
			// Lstat, not CheckNotExist: a dangling symlink at the
			// destination still occupies the name and must honour "skip"
			// like any other existing entry.
			if _, statErr := os.Lstat(dst); statErr == nil && temp.Style == "skip" {
				temp.Item[i].Finished = true
				continue
			}
			if err := moveItem(ctx, v.From, dst, v.Size, counter); err != nil {
				if errors.Is(err, context.Canceled) {
					// An operator-requested stop is not a failure, and logging
					// it as "file move error" sent an admin hunting a disk
					// fault that never happened. moveItem only honours a cancel
					// while the payload is still staged, so at this point the
					// source and any pre-existing destination are both intact —
					// say that, and stop walking the rest of the items.
					logger.Info("file move cancelled before the destination was touched; source and destination are both intact",
						zap.String("from", v.From), zap.String("to", dst))
					break
				}
				logger.Error("file move error",
					zap.String("from", v.From), zap.String("to", dst), zap.Error(err))
				continue
			}
			temp.Item[i].Finished = true

		} else if temp.Type == "copy" {
			lastPath := v.From[strings.LastIndex(v.From, "/")+1:]
			dst := temp.To + "/" + lastPath
			if err := copyTreeCtx(ctx, v.From, dst, counter); err != nil {
				logger.Error("file copy error", zap.String("from", v.From), zap.String("to", dst), zap.Error(err))
				if errors.Is(err, context.Canceled) {
					// Say where the partial copy is instead of deleting it: a
					// copy merges into an existing destination, so removing
					// the tree would take pre-existing data with it. The
					// interrupted file itself is already gone (copyFileCtx
					// discards its temporary), so what is left is whole files.
					logger.Error("file copy cancelled; the completely copied files are left in place for the operator to review",
						zap.String("partial", dst))
				}
				continue
			}
			temp.Item[i].Finished = true
		} else {
			continue
		}

	}

	if ctx.Err() != nil {
		// The tracking entry is being (or has been) dropped by the canceller.
		// Publishing "finished" here is what used to resurrect a ghost
		// operation the UI could never get rid of.
		return
	}
	temp.Finished = true
	temp.LastUpdate = time.Now()
	storeOpIfLive(k, temp)
}

// copyTreeCtx is the cancellable twin of file.CopyDirWithProgress.
//
// It lives here rather than in pkg/utils/file because cancellation is a
// property of an *operation* (see opControl), and because the context has to
// reach through the io.Copy of a single multi-gigabyte file — the pkg helper
// is used by callers that have no operation behind them and must not gain a
// context they cannot honour. The wrappers it composes (NewReader/NewWriter)
// were already here and, until now, unused by anything.
//
// Durability matches the pkg helper exactly (tmp file, fsync, rename), because
// moveItem's cross-mount path relies on it: the staged copy must be durable
// before it is published at the destination.
func copyTreeCtx(ctx context.Context, src, dst string, p *file.ProgressCounter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !info.IsDir() {
		return copyFileCtx(ctx, src, dst, info.Mode(), p)
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTreeCtx(ctx, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), p); err != nil {
			return err
		}
	}
	return nil
}

// copyTmpSuffix names the transient file a cancellable copy writes into. It
// mirrors pkg/utils/file's own suffix so the janitor patterns that already
// recognise interrupted copies keep working.
const copyTmpSuffix = ".casanova-tmp"

func copyFileCtx(ctx context.Context, src, dst string, mode os.FileMode, p *file.ProgressCounter) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + copyTmpSuffix
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// Bytes are counted on the way to the file, and the context is checked on
	// both sides of the copy: a cancel takes effect within one 32 KiB chunk
	// instead of at the end of a 600 GB file.
	dest := NewWriter(ctx, &countingWriter{w: out, p: p})
	if _, err := io.Copy(dest, NewReader(ctx, in)); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// countingWriter credits every byte that actually reached the destination to
// the operation's progress counter.
type countingWriter struct {
	w io.Writer
	p *file.ProgressCounter
}

func (cw *countingWriter) Write(b []byte) (int, error) {
	n, err := cw.w.Write(b)
	if n > 0 && cw.p != nil {
		cw.p.Add(int64(n))
	}
	return n, err
}

// moveStagePrefix names the transient staging directory that holds a move's
// payload while it is in flight. It lives INSIDE the destination directory so
// that publishing is a same-directory rename(2): the final destination is
// never visible half-written. A crash leaves one of these behind, recognisable
// by the prefix, and never a damaged destination.
const moveStagePrefix = ".casanova-move-"

// stageRename performs the source → staging rename probe. It is a variable so
// tests can force the EXDEV (cross-mount copy) branch: that branch is the one
// that used to destroy data, and it cannot otherwise be exercised without a
// second real filesystem mounted under the test.
var stageRename = os.Rename

// errDestInsideSource rejects a move whose destination sits under its own
// source. rename(2) answers EINVAL for that within one mount, but a nested
// mount below the source (this box stacks mounts under /DATA and /mnt) sends
// it down the copy path, where the walk would recurse into its own output
// until the filesystem fills.
var errDestInsideSource = errors.New("move: destination is inside the source directory")

// trashDestination sends an about-to-be-overwritten destination to the recycle
// bin. It is a variable for two reasons: it is the single seam a test can make
// fail, so the "recycle bin rejected it → refuse the overwrite" branch (the
// fail-closed guarantee) is exercisable at all; and it is the one call site to
// repoint once pkg/trash grows a size-free entry point. Today Trash() computes
// Entry.Size with a full recursive walk of the tree it just trashed, which this
// caller throws away — on a media library that walk stalls the move for minutes
// with the payload sitting in a hidden staging directory.
var trashDestination = func(path string) error {
	_, err := trash.NewManager().Trash(path)
	return err
}

// moveItem moves src to dst so that the user is never left with fewer than one
// intact copy of the data.
//
// What this replaces: the old move did RemoveAll(dst) BEFORE copying, so an
// overwrite whose copy then failed (ENOSPC, network mount dropping, dying
// disk) destroyed the pre-existing destination for good, with no trash entry.
// And if the copy succeeded but removing the source failed (EBUSY, permissions,
// file held open), it "rolled back" by RemoveAll(dst) — deleting the freshly
// written good copy while the source had already been partially unlinked, a
// path that ends with ZERO copies. It also never tried rename(2), so even a
// move within one mount paid a full copy+delete and opened that same window.
//
// Order of operations here:
//  1. stage the payload inside the destination directory — rename(2) when src
//     and dst share a mount (instant, no temporary double space), copy+fsync
//     only when the kernel answers EXDEV;
//  2. an existing destination goes to the RECYCLE BIN, never RemoveAll, and
//     only once the replacement is fully staged and durable;
//  3. a same-directory rename publishes the destination atomically;
//  4. the source is unlinked LAST. If that fails we KEEP the destination — it
//     is the surviving copy — and report the error. A completed copy is never
//     rolled back.
//
// size is the item's pre-computed byte count, used to credit the progress
// counter on the rename fast path (see below).
//
// OPERATING CHARACTERISTIC — space, not a defect. Because the replacement is
// staged before the old destination leaves its place, and because the old
// destination is RETAINED by the recycle bin (30 days, pkg/trash
// DefaultRetention) instead of being unlinked, an overwrite-move is no longer
// space-neutral:
//   - a cross-mount overwrite needs room for source AND destination at the same
//     time on the target filesystem, so a case that used to just fit can now
//     fail with ENOSPC. That failure is safe (nothing is destroyed) but it is a
//     real behaviour change on a nearly full disk;
//   - even the instant same-mount path grows disk usage on every overwrite
//     until the trash janitor purges, and pkg/trash puts the bin at the mount
//     root of the DESTINATION — for a destination on "/" that is
//     /.Trash-casanova, so repeated overwrite-moves eat root filesystem space.
//
// This is the deliberate trade: an accidental overwrite stays undoable, and
// running out of space fails the move instead of destroying the target. Trash
// occupancy is the operator's lever (Settings → Maintenance / trash janitor).
//
// CANCELLATION. ctx has exactly one effect, and it is placed where it cannot
// break the invariant above: the move aborts only while the payload is still
// in the staging directory, i.e. before the destination is touched at all.
// Aborting there unwinds through the same deferred cleanup as any staging
// failure — the source is put back (rename fast path) or was never touched
// (cross-mount path), and the existing destination is left exactly as it was.
// Past that point the operation runs to completion even if the operator
// cancels: from the moment the old destination goes to the recycle bin, the
// staged payload is the only copy, and stopping half way is precisely how you
// end up with zero.
func moveItem(ctx context.Context, src, dst string, size int64, counter *file.ProgressCounter) error {
	srcClean := filepath.Clean(src)
	dstClean := filepath.Clean(dst)

	// A move onto itself must be a no-op. The old code answered it by
	// RemoveAll'ing the destination — which IS the source — and then copying
	// from a path that no longer existed: the item vanished.
	if srcClean == dstClean {
		return nil
	}
	// Refuse before anything touches the source.
	if pathWithin(dstClean, srcClean) {
		return fmt.Errorf("%w: %s -> %s", errDestInsideSource, srcClean, dstClean)
	}
	if _, err := os.Lstat(srcClean); err != nil {
		return err
	}

	dstDir := filepath.Dir(dstClean)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp(dstDir, moveStagePrefix+"*")
	if err != nil {
		return err
	}
	staged := filepath.Join(stageDir, filepath.Base(dstClean))

	published := false
	renamed := false
	defer func() {
		if published {
			_ = os.Remove(stageDir) // empty now
			return
		}
		if renamed {
			// The fast path already unlinked the source, so the stage holds
			// the ONLY copy: put it back rather than delete it. If even that
			// fails, LEAVE the stage on disk and say where it is — losing the
			// data to a cleanup path is not an acceptable outcome.
			if err := os.Rename(staged, srcClean); err != nil {
				logger.Error("move aborted after the source was renamed away — data left in the staging directory, NOT deleted",
					zap.String("staging", staged), zap.String("source", srcClean), zap.Error(err))
				return
			}
			_ = os.Remove(stageDir)
			return
		}
		// Cross-mount path: the source is untouched, the stage holds at most
		// a partial copy. Only that copy is discarded; the live destination
		// is never touched by this cleanup.
		_ = os.RemoveAll(stageDir)
	}()

	// Try rename(2) first and fall back only on EXDEV. Probing beats trying
	// to decide "same filesystem" ourselves: /DATA, /mnt and /media are bind
	// mounts inside the service's namespace (systemd ReadWritePaths), so they
	// share st_dev with / while rename(2) still answers EXDEV — the exact trap
	// that broke the recycle bin (see pkg/trash mountPointOf).
	if renameErr := stageRename(srcClean, staged); renameErr == nil {
		renamed = true
	} else if !errors.Is(renameErr, syscall.EXDEV) {
		return renameErr
	} else {
		if err := copyTreeCtx(ctx, srcClean, staged, counter); err != nil {
			return err
		}
		// Make the copy durable BEFORE it becomes reachable at dst, so a
		// power cut cannot expose a destination whose files are empty.
		if err := fsyncTree(staged); err != nil {
			return err
		}
	}

	// Last point at which stopping is free: everything is still in staging and
	// the destination has not been touched. See CANCELLATION above.
	if err := ctx.Err(); err != nil {
		return err
	}

	// Replace, don't erase: the previous destination goes to the recycle bin
	// so a wrong overwrite stays undoable, and only now that its replacement
	// is complete on disk.
	if _, err := os.Lstat(dstClean); err == nil {
		if trashErr := trashDestination(dstClean); trashErr != nil {
			// Fail closed. The deferred cleanup puts the source back; the
			// existing destination is left exactly as we found it.
			return fmt.Errorf("move: refusing to overwrite %s, recycle bin rejected it: %w", dstClean, trashErr)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Rename(staged, dstClean); err != nil {
		return err
	}
	published = true
	// Persist the directory entry itself: without this the rename above can
	// still be lost on power cut even though the data blocks are durable.
	//
	// How a failure here is reported depends on which path we took, because the
	// two differ in whether a second copy still exists:
	//   - cross-mount: the source has NOT been removed yet, so failing closed
	//     costs the user a retry and nothing else. Report the error.
	//   - rename fast path: stageRename already unlinked the source. The payload
	//     is at the destination and durable; only the durability of the parent's
	//     directory entry across a power cut is unproven. Reporting failure would
	//     mark a completed move as failed in the transfer panel, the user retries,
	//     the retry fails with ENOENT on a source that is gone, and it reads as
	//     data loss. Warn and report success instead. The trigger is realistic:
	//     fsyncDir only tolerates EINVAL/ENOTSUP/EOPNOTSUPP, and an rclone or SMB
	//     mount can answer EIO or ENOTCONN here.
	if err := fsyncDir(dstDir); err != nil {
		if !renamed {
			return fmt.Errorf("move: %s copied to %s but the directory entry is not durable (source kept): %w", srcClean, dstClean, err)
		}
		logger.Error("move completed but the destination directory entry could not be fsync'd; the payload is at the destination and the source is already gone, so the move is reported as successful",
			zap.String("from", srcClean), zap.String("to", dstClean), zap.Error(err))
	}

	if renamed {
		// rename(2) moved the payload without a single byte flowing through
		// the progress counter. Credit the item's whole size so an instant
		// move shows as completed instead of stuck at 0% in the transfer
		// panel, which reads this counter.
		if counter != nil && size > 0 {
			counter.Add(size)
		}
		return nil
	}
	if err := os.RemoveAll(srcClean); err != nil {
		// The destination is complete and durable, so it is now the surviving
		// copy. Deleting it to "roll back" (what the old code did) could leave
		// zero copies, since RemoveAll may already have unlinked part of the
		// source. Keep the destination and report.
		return fmt.Errorf("move: %s copied to %s but the source could not be removed (destination kept): %w", srcClean, dstClean, err)
	}
	return nil
}

// pathWithin reports whether p sits at or under dir.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}

// pathContains is the containment test used by the parked-worker reservations.
//
// It is separate from pathWithin because pathWithin cannot express the root
// filesystem: "/" + separator is "//", which is a prefix of nothing, so "/"
// would contain no path at all. That silence is the wrong default here — a
// reservation that quietly covers nothing would let an operation start on a tree
// somebody still owns — so root is spelled out. pathWithin itself is left alone:
// it guards moveItem's "destination inside its own source" refusal, and that
// refusal must not change behaviour under a queue change.
func pathContains(dir, p string) bool {
	if dir == string(os.PathSeparator) {
		return strings.HasPrefix(p, dir)
	}
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}

// fsyncTree flushes directory metadata for a freshly copied tree. File
// contents are already fsync'd by CopyFileWithProgress, but the directory
// entries naming them are not, and a rename that publishes a tree of
// unreferenced inodes is worth nothing after a power cut.
func fsyncTree(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fsyncDir(filepath.Dir(root))
	}
	if !info.IsDir() {
		return fsyncDir(filepath.Dir(root))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := fsyncTree(filepath.Join(root, e.Name())); err != nil {
				return err
			}
		}
	}
	return fsyncDir(root)
}

// fsyncDir fsyncs a directory so creates and renames inside it survive a power
// cut. Filesystems that do not implement directory fsync (some network and
// FUSE mounts this box exposes through rclone/SMB) answer EINVAL/ENOTSUP —
// treat that as "nothing to flush" rather than failing an otherwise good move.
//
// A variable so tests can force the untolerated-errno case (EIO from a flaky
// remote mount); the two paths of moveItem must react to it differently and
// that difference is exactly what needs pinning.
var fsyncDir = func(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	return nil
}

// ExecOpFile starts the first operation in the queue that a worker could
// actually claim, in queue order.
//
// It used to take OpStrArr[0] unconditionally, which is right while the head is
// either running or startable — FileOperate refuses anything else and returns.
// But an entry that can never be claimed stays at the head on purpose: a
// cancelled operation is kept visible until its worker unwinds, and a parked one
// (see WedgeAfter) is kept visible for as long as it is parked. Pointing the
// pump at such a head meant it started nothing at all, and the whole queue sat
// behind one dead mount.
//
// Serialization is unchanged and still enforced by beginOp: whatever is started
// here waits for the current worker's control before touching the filesystem.
func ExecOpFile() {
	OpMu.Lock()
	ids := make([]string, len(OpStrArr))
	copy(ids, OpStrArr)
	OpMu.Unlock()

	opTrackMu.Lock()
	next, ok := nextStartableLocked(ids)
	opTrackMu.Unlock()
	if !ok {
		return
	}
	go FileOperate(next)
}

// nextStartableLocked picks the first queue entry worth handing to a worker.
// Caller holds opTrackMu.
//
// The skips mirror the reasons FileOperate/beginOp would refuse the entry
// anyway; the point of testing them here is that refusing INSIDE the worker
// leaves the queue with nobody running, whereas skipping here moves on to the
// next candidate.
func nextStartableLocked(ids []string) (string, bool) {
	for _, id := range ids {
		raw, exists := FileQueue.Load(id)
		if !exists {
			continue
		}
		op, isOp := raw.(model.FileOperate)
		if !isOp {
			continue
		}
		// Already ran or is running: FileOperate's own guard refuses these.
		if op.Finished || op.ProcessedSize > 0 {
			continue
		}
		if ctl, ok := loadOpControl(id); ok {
			if ctl.removed.Load() || ctl.cancelRequested.Load() || ctl.claimed.Load() {
				continue
			}
		}
		if _, blocked := wedgeConflictLocked(opPathsOf(op)); blocked {
			continue
		}
		return id, true
	}
	return "", false
}

// CheckFileStatus publishes byte-level progress for every in-flight file
// operation by reading the atomic counter that the copy loop updates as
// bytes flow through it. No filesystem polling — the old implementation
// du-walked the destination every 3 seconds and jumped 0 → 100%.
//
// Poll interval is 500ms since we're now doing cheap atomic loads, not
// filesystem walks.
func CheckFileStatus() {
	for {
		// Snapshot the current operation list under the lock so we
		// don't race with concurrent append/reset from HTTP handlers.
		OpMu.Lock()
		if len(OpStrArr) == 0 {
			OpMu.Unlock()
			return
		}
		snapshot := make([]string, len(OpStrArr))
		copy(snapshot, OpStrArr)
		OpMu.Unlock()

		for _, v := range snapshot {
			publishOpProgress(v)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// publishOpProgress writes one operation's live byte count back into its queue
// entry. Split out of the poll loop so the "never re-publish an operation that
// has been cancelled" rule is reachable by a test: the loop itself never
// returns, and this is the write that used to resurrect cancelled entries
// twice a second — leaving a FileQueue entry no longer in OpStrArr, which kept
// the notify loop spinning forever on an operation the UI could not see.
func publishOpProgress(id string) bool {
	item, ok := FileQueue.Load(id)
	if !ok {
		return false
	}
	temp := item.(model.FileOperate)

	var done int64
	if raw, ok := opProgress.Load(id); ok {
		if counter, assertOk := raw.(*file.ProgressCounter); assertOk {
			done = counter.Done()
		}
	}
	temp.ProcessedSize = done

	// Best-effort per-item breakdown: distribute the running byte
	// count proportionally across items based on their known sizes.
	// Finished items keep their own Size as ProcessedSize.
	if temp.TotalSize > 0 {
		// Deep-copy the Item slice first. `temp` is a struct copy from
		// the map, but Item is a slice — it shares the SAME backing
		// array the copier goroutine (ExecOpFile) writes Item[i].Finished
		// into. Writing ProcessedSize here on the shared array races with
		// that and, on Store, clobbers the copier's Finished back to false
		// (copy stuck <100%, never marked FINISHED). Work on a private copy.
		items := make([]model.FileItem, len(temp.Item))
		copy(items, temp.Item)
		temp.Item = items
		remaining := done
		for i := range temp.Item {
			if temp.Item[i].Finished {
				temp.Item[i].ProcessedSize = temp.Item[i].Size
				remaining -= temp.Item[i].Size
				if remaining < 0 {
					remaining = 0
				}
				continue
			}
			if remaining >= temp.Item[i].Size {
				temp.Item[i].ProcessedSize = temp.Item[i].Size
				remaining -= temp.Item[i].Size
			} else {
				temp.Item[i].ProcessedSize = remaining
				remaining = 0
			}
		}
	}
	temp.LastUpdate = time.Now()
	// Never re-Store an entry a cancel has already dropped.
	return storeOpIfLive(id, temp)
}

func IsMounted(path string) bool {
	mounted, _ := mountinfo.Mounted(path)
	if mounted {
		return true
	}
	connections := MyService.Connections().GetConnectionsList()
	for _, v := range connections {
		if v.MountPoint == path {
			return true
		}
	}
	return false
}

// SnapshotMountedPaths returns the union of "currently mounted by the
// kernel" and "tracked by rclone connections" as a path → true map.
// Used by per-directory listings under /mnt and /media so we read the
// global mount state ONCE per request instead of once per entry.
//
// v0.1 perf fix — see CasaNova/route/v1/file.go DirPath handler.
func SnapshotMountedPaths() map[string]bool {
	out := map[string]bool{}
	// Pull the full kernel mount table once via PeekMounts; cheaper
	// than calling mountinfo.Mounted(path) for every entry (which
	// re-reads /proc/self/mountinfo from scratch each call). Falls
	// back to the per-path API if the helper isn't available on this
	// platform / version.
	if entries, err := mountinfo.GetMounts(nil); err == nil {
		for _, m := range entries {
			out[m.Mountpoint] = true
		}
	}
	connections := MyService.Connections().GetConnectionsList()
	for _, v := range connections {
		out[v.MountPoint] = true
	}
	return out
}
