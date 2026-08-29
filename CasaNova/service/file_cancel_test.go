package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
)

// These tests pin the cancellation contract of file operations. Every one of
// them corresponds to a way the previous implementation lied to the operator:
// cancelling only mutated the tracking (FileQueue/OpStrArr) while the copy
// goroutine kept running, so a "cancelled" 600 GB copy went on filling the
// disk invisibly, a "cancelled" move still deleted its source once it
// completed, the zombie's final write resurrected an entry the UI could no
// longer see or cancel, and the next queued operation started on top of a
// destination the previous worker still owned.

// resetOpState clears the process-wide operation tracking so tests do not
// inherit each other's queues. It also runs on cleanup: a leaked activeOp
// would make every later test block waiting for a worker that no longer runs.
func resetOpState(t *testing.T) {
	t.Helper()
	clear := func() {
		OpMu.Lock()
		OpStrArr = nil
		OpMu.Unlock()
		FileQueue.Range(func(k, _ any) bool { FileQueue.Delete(k); return true })
		opControls.Range(func(k, _ any) bool { opControls.Delete(k); return true })
		opProgress.Range(func(k, _ any) bool { opProgress.Delete(k); return true })
		opTrackMu.Lock()
		activeOp = nil
		opTrackMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitUntil polls cond until it holds, the operation under test finishes, or
// the deadline passes, and reports whether cond was ever observed.
//
// The data-safety tests below use this instead of a Fatal-ing wait on purpose.
// An implementation that touches the destination before the payload is staged
// (the ordering that used to destroy data) returns almost immediately, and a
// Fatal here would report "timed out waiting for bytes" — slow, and it hides
// which invariant actually broke. Falling through lets the destination and
// source assertions run and name the real damage.
func waitUntil(timeout time.Duration, done <-chan struct{}, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-done:
			return cond() // one last look: it may have satisfied cond on its way out
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func closed(ch <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
}

// fifoFeeder drives a named pipe used as a copy source. A pipe is the only way
// to hold a real copy open for the length of a test without writing gigabytes:
// it is a regular non-directory file to the copier, it delivers bytes for as
// long as the test keeps feeding it, and pausing the feed reproduces exactly
// what a hung NFS/SMB mount does to a worker — it blocks inside read(2), where
// cancelling a context cannot reach it.
//
// written counts bytes HANDED TO the pipe, published before the write is
// attempted rather than after it returns; see the loop in startFifoFeeder for
// why the ordering matters. exitErr records why the feed loop ended and is only
// safe to read after stopped is closed (the close is what publishes it).
type fifoFeeder struct {
	paused  atomic.Bool
	eof     atomic.Bool
	written atomic.Int64
	exitErr error
	stopped chan struct{}
}

func mkfifoT(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("this filesystem cannot host a FIFO (%v); the cancellation tests need one", err)
	}
}

func startFifoFeeder(t *testing.T, path string) *fifoFeeder {
	t.Helper()
	f := &fifoFeeder{stopped: make(chan struct{})}
	go func() {
		defer close(f.stopped)
		// Blocks until the copier opens the read end.
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			f.exitErr = err
			return
		}
		defer w.Close()
		buf := make([]byte, 1<<15)
		for {
			if f.eof.Load() {
				return // closing the write end gives the copier a clean EOF
			}
			if f.paused.Load() {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			// Account the chunk BEFORE handing it to the pipe, then give back
			// whatever the pipe refused. Counting after w.Write returned made
			// this counter lag a write the kernel had already accepted in full,
			// so a test that sampled it as a baseline could miss a whole 1<<15
			// chunk and then read the late Add as "the worker kept reading
			// after the cancel completed" — the intermittent failure this file
			// was reported for. Counting up front can only ever overstate the
			// baseline, which is the direction that cannot invent a violation.
			f.written.Add(int64(len(buf)))
			n, err := w.Write(buf)
			if n < len(buf) {
				f.written.Add(int64(n) - int64(len(buf)))
			}
			if err != nil {
				// EPIPE: the copier closed the read end, which is the signal
				// that it really stopped.
				f.exitErr = err
				return
			}
		}
	}()
	t.Cleanup(func() {
		f.eof.Store(true)
		f.paused.Store(false)
		select {
		case <-f.stopped:
		case <-time.After(5 * time.Second):
		}
	})
	return f
}

func (f *fifoFeeder) pause()  { f.paused.Store(true) }
func (f *fifoFeeder) resume() { f.paused.Store(false) }

// requireSourceReleased proves that the worker under test really let go of the
// source, and returns the number of bytes the feeder managed to push into it.
//
// The feed loop only ends on eof (which the cancellation tests never set before
// asserting) or on a failed write, and the only thing that can fail a write to
// a pipe here is EPIPE: every reader closed. So "the feeder saw EPIPE" is a
// structural statement that the copy goroutine closed the read end — it cannot
// be satisfied by a worker that is still reading, nor by one that has merely
// gone quiet while holding the descriptor open.
//
// This replaces the "sample written, sleep 200ms, expect no growth" check the
// two cancellation tests used to do. That check was intermittent (measured:
// 5 failures in 25 runs under CPU contention, reporting a phantom 32768 bytes)
// because the feeder used to account bytes only after w.Write returned, so the
// baseline could be sampled with a completed write not yet counted. It is also
// weaker than what is asserted here: a 200ms quiet window says nothing about
// whether the source descriptor is still held, and an fd the cancelled worker
// never closed is exactly what keeps a "cancelled" transfer pinning a mount.
func (f *fifoFeeder) requireSourceReleased(t *testing.T, timeout time.Duration) int64 {
	t.Helper()
	select {
	case <-f.stopped:
	case <-time.After(timeout):
		t.Fatalf("the cancelled worker still holds the source pipe open after %s (%d bytes fed): "+
			"the copy goroutine never closed the read end, so it can resume reading at any time",
			timeout, f.written.Load())
	}
	// stopped is closed after exitErr is assigned, so this read is ordered.
	if !errors.Is(f.exitErr, syscall.EPIPE) {
		t.Fatalf("the feeder stopped with %v, want EPIPE from the copier closing the read end; "+
			"without EPIPE this test proves nothing about the worker having stopped", f.exitErr)
	}
	return f.written.Load()
}

// finish ends the stream so a healthy copy completes on its own.
func (f *fifoFeeder) finish() {
	f.eof.Store(true)
	f.paused.Store(false)
}

func opCounter(t *testing.T, id string) *file.ProgressCounter {
	t.Helper()
	raw, ok := opProgress.Load(id)
	if !ok {
		return nil
	}
	counter, _ := raw.(*file.ProgressCounter)
	return counter
}

// TestCopyTreeCtxStopsMidStream is the wiring proof for the context-aware
// reader/writer wrappers: they existed in this file for years and nothing used
// them, which is why a copy could not be interrupted once it had started.
func TestCopyTreeCtxStopsMidStream(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "stream")
	mkfifoT(t, src)
	dst := filepath.Join(dir, "dst", "stream")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	feeder := startFifoFeeder(t, src)

	counter := &file.ProgressCounter{}
	errCh := make(chan error, 1)
	go func() { errCh <- copyTreeCtx(ctx, src, dst, counter) }()

	waitFor(t, "the copy to start moving bytes", 15*time.Second, func() bool { return counter.Done() > 0 })
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("copy returned %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the copy never returned after its context was cancelled: it is not cancellable")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("a half-written destination was published at %s (err=%v)", dst, err)
	}
	if _, err := os.Lstat(dst + copyTmpSuffix); !os.IsNotExist(err) {
		t.Fatalf("the interrupted temporary file was left behind at %s", dst+copyTmpSuffix)
	}
	_ = feeder
}

// TestCancelledMoveKeepsSourceAndDestination pins the data-safety half of
// cancellation on the rename(2) fast path: aborting must never be able to
// produce zero copies. The old code had no cancellation point at all, so a
// "cancelled" move ran to completion and deleted the source anyway.
func TestCancelledMoveKeepsSourceAndDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "album.flac")
	dst := filepath.Join(dir, "dst", "album.flac")
	writeFileT(t, src, "the only copy")
	writeFileT(t, dst, "the previous release")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := moveItem(ctx, src, dst, int64(len("the only copy")), &file.ProgressCounter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("moveItem returned %v, want context.Canceled", err)
	}
	if got := readFileT(t, src); got != "the only copy" {
		t.Fatalf("source after a cancelled move = %q, want it untouched", got)
	}
	if got := readFileT(t, dst); got != "the previous release" {
		t.Fatalf("destination after a cancelled move = %q, want it untouched", got)
	}
	if leftovers := stageLeftovers(t, filepath.Dir(dst)); len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
}

// TestCancelledCrossMountMoveKeepsSourceAndDestination is the same contract on
// the copy fallback, where the payload is staged rather than renamed.
func TestCancelledCrossMountMoveKeepsSourceAndDestination(t *testing.T) {
	forceCrossMount(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src", "album.flac")
	dst := filepath.Join(dir, "dst", "album.flac")
	writeFileT(t, src, "the only copy")
	writeFileT(t, dst, "the previous release")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := moveItem(ctx, src, dst, int64(len("the only copy")), &file.ProgressCounter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("moveItem returned %v, want context.Canceled", err)
	}
	if got := readFileT(t, src); got != "the only copy" {
		t.Fatalf("source after a cancelled cross-mount move = %q, want it untouched", got)
	}
	if got := readFileT(t, dst); got != "the previous release" {
		t.Fatalf("destination after a cancelled cross-mount move = %q, want it untouched", got)
	}
	if leftovers := stageLeftovers(t, filepath.Dir(dst)); len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
}

// countTrashCalls replaces the recycle-bin seam with a counter. It is how a
// test proves a cancellation landed BEFORE the destination was retired: that
// retirement is moveItem's point of no return, and counting zero calls is a
// far stronger statement than "the destination still has its old bytes"
// (which a re-created file would also satisfy).
func countTrashCalls(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	original := trashDestination
	trashDestination = func(path string) error {
		calls.Add(1)
		return original(path)
	}
	t.Cleanup(func() { trashDestination = original })
	return &calls
}

// TestCancelMidCrossMountMoveKeepsBothCopies is the invariant test for the
// dangerous case: a cancel that arrives while a cross-mount move is COPYING,
// with the payload half-staged and a populated destination already in place.
//
// The sibling test above only pre-cancels the context, which copyTreeCtx
// rejects at its very first line — it never reaches the window where the
// staging directory holds a partial tree, so it cannot pin this. The rule the
// move has to keep is that cancelling never produces fewer than one intact
// copy: here the source must be whole, the existing destination must be
// untouched, and the recycle bin must never have been asked to retire it.
//
// A named pipe inside the source tree is what holds the copy open mid-flight
// without writing gigabytes: it is a regular non-directory file to the copier
// and it delivers bytes for as long as the test keeps feeding it.
func TestCancelMidCrossMountMoveKeepsBothCopies(t *testing.T) {
	forceCrossMount(t)
	trashCalls := countTrashCalls(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src", "album")
	stream := filepath.Join(srcDir, "side-b.flac")
	mkfifoT(t, stream)
	writeFileT(t, filepath.Join(srcDir, "cover.jpg"), "artwork")

	dstParent := filepath.Join(dir, "dst")
	dstDir := filepath.Join(dstParent, "album")
	writeFileT(t, filepath.Join(dstDir, "irreplaceable.txt"), "ten years of photos")

	feeder := startFifoFeeder(t, stream)
	counter := &file.ProgressCounter{}
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	moveDone := make(chan struct{})
	go func() {
		defer close(moveDone)
		errCh <- moveItem(ctx, srcDir, dstDir, 1<<40, counter)
	}()

	// Gate on the feeder, not on the counter: bytes on the counter could all
	// come from cover.jpg, which would let the cancel land between files
	// instead of inside a copy. The feeder only makes progress once the copier
	// has the pipe open, so this puts us provably inside the streaming copy.
	streamed := waitUntil(15*time.Second, moveDone, func() bool { return feeder.written.Load() > 0 })
	cancel()

	select {
	case err := <-errCh:
		if streamed && !errors.Is(err, context.Canceled) {
			t.Fatalf("moveItem returned %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the move never returned after its context was cancelled: it is not cancellable")
	}
	if !streamed {
		// Not fatal on its own — the assertions below are the contract — but
		// it means the cancel never landed mid-copy, so say so.
		t.Error("the move returned before it ever streamed the staged payload")
	}

	if got := readFileT(t, filepath.Join(srcDir, "cover.jpg")); got != "artwork" {
		t.Fatalf("source file after a cancelled cross-mount move = %q, want it untouched", got)
	}
	if _, err := os.Lstat(stream); err != nil {
		t.Fatalf("the source pipe was consumed by a cancelled move: %v", err)
	}
	if got := readFileT(t, filepath.Join(dstDir, "irreplaceable.txt")); got != "ten years of photos" {
		t.Fatalf("pre-existing destination = %q, want it untouched", got)
	}
	if n := trashCalls.Load(); n != 0 {
		t.Fatalf("the destination was retired %d time(s) by a move that never got past staging", n)
	}
	if leftovers := stageLeftovers(t, dstParent); len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
}

// TestCancelAtPointOfNoReturnStillCompletesMove is the inverse invariant, and
// the reason moveItem has exactly one cancellation point instead of many.
//
// Once the previous destination has been retired, the staged payload is the
// only copy standing at that path. Honouring a cancel from there — the
// "obvious" improvement of checking ctx between every step — is precisely how
// a cancellation ends up destroying the data it was supposed to protect. The
// move must run to completion instead.
//
// The recycle bin is stubbed rather than exercised (pkg/trash puts its root at
// the destination's mount point, which no unprivileged test can create under
// /tmp); what is under test here is the ordering after that step, not the bin.
func TestCancelAtPointOfNoReturnStillCompletesMove(t *testing.T) {
	forceCrossMount(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src", "album.flac")
	dst := filepath.Join(dir, "dst", "album.flac")
	writeFileT(t, src, "the new master")
	writeFileT(t, dst, "the previous release")

	ctx, cancel := context.WithCancel(context.Background())
	original := trashDestination
	trashDestination = func(path string) error {
		// Cancel at the worst possible instant: the old destination is going
		// away right now and its replacement only exists in staging.
		cancel()
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { trashDestination = original })

	done := make(chan error, 1)
	go func() {
		done <- moveItem(ctx, src, dst, int64(len("the new master")), &file.ProgressCounter{})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("moveItem abandoned a move past its point of no return: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("moveItem never returned")
	}

	if got := readFileT(t, dst); got != "the new master" {
		t.Fatalf("destination = %q, want the move to have completed", got)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after a completed move (err=%v)", err)
	}
	if leftovers := stageLeftovers(t, filepath.Dir(dst)); len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
}

// TestCancelOpDuringMoveKeepsSourceAndDestination walks the whole chain the
// operator actually touches — DELETE /v1/file/operate/{id} → CancelOp → the
// operation's context → moveItem — for the operation type where getting it
// wrong loses data. Before this, cancelling a move only edited the tracking:
// the worker kept going and deleted the source anyway, minutes after the UI
// swore the move was gone.
func TestCancelOpDuringMoveKeepsSourceAndDestination(t *testing.T) {
	resetOpState(t)
	forceCrossMount(t)
	trashCalls := countTrashCalls(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src", "album")
	stream := filepath.Join(srcDir, "side-b.flac")
	mkfifoT(t, stream)
	writeFileT(t, filepath.Join(srcDir, "cover.jpg"), "artwork")

	to := filepath.Join(dir, "to")
	writeFileT(t, filepath.Join(to, "album", "irreplaceable.txt"), "ten years of photos")

	feeder := startFifoFeeder(t, stream)

	const id = "op-move"
	FileQueue.Store(id, model.FileOperate{
		Type: "move", To: to, TotalSize: 1 << 40,
		Item: []model.FileItem{{From: srcDir, Size: 1 << 40}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = []string{id}
	OpMu.Unlock()
	RegisterOp(id)

	workerDone := make(chan struct{})
	go func() { defer close(workerDone); FileOperate(id) }()

	streamed := waitUntil(15*time.Second, workerDone, func() bool { return feeder.written.Load() > 0 })

	if !CancelOp(id, 15*time.Second) {
		t.Fatal("CancelOp reported that the move worker was still running: the move is not cancellable")
	}
	waitFor(t, "the worker goroutine to exit", 5*time.Second, closed(workerDone))
	if !streamed {
		t.Error("the move returned before it ever streamed the staged payload")
	}

	// Bytes must stop flowing, or the "cancelled" move is still consuming the
	// destination filesystem exactly as the report describes. Gated on streamed
	// because a move that never opened the pipe leaves the feeder parked in
	// open(2) for ever; in that case the data-safety assertions below are the
	// ones that name the real damage, and they must still get to run.
	if streamed {
		if fed := feeder.requireSourceReleased(t, 15*time.Second); fed <= 0 {
			t.Fatal("the feeder delivered no bytes: the cancel never landed inside the streaming copy")
		}
	}

	if got := readFileT(t, filepath.Join(srcDir, "cover.jpg")); got != "artwork" {
		t.Fatalf("source file after a cancelled move = %q, want it untouched", got)
	}
	if _, err := os.Lstat(stream); err != nil {
		t.Fatalf("the source pipe was consumed by a cancelled move: %v", err)
	}
	if got := readFileT(t, filepath.Join(to, "album", "irreplaceable.txt")); got != "ten years of photos" {
		t.Fatalf("pre-existing destination = %q, want it untouched", got)
	}
	if n := trashCalls.Load(); n != 0 {
		t.Fatalf("the destination was retired %d time(s) by a move that never got past staging", n)
	}
	if leftovers := stageLeftovers(t, to); len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
	if _, ok := FileQueue.Load(id); ok {
		t.Fatal("the cancelled move is still in FileQueue: a ghost the UI can never clear")
	}
}

// TestCancelOpStopsInFlightWorker is the end-to-end version of the reported
// scenario: an operator cancels a runaway copy and the goroutine behind it has
// to actually stop, not keep writing while the dashboard shows nothing.
func TestCancelOpStopsInFlightWorker(t *testing.T) {
	resetOpState(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src", "stream")
	mkfifoT(t, src)
	to := filepath.Join(dir, "to")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	feeder := startFifoFeeder(t, src)

	const id = "op-inflight"
	FileQueue.Store(id, model.FileOperate{
		Type:       "copy",
		To:         to,
		TotalSize:  1 << 40,
		Item:       []model.FileItem{{From: src, Size: 1 << 40}},
		LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = []string{id}
	OpMu.Unlock()
	RegisterOp(id)

	workerDone := make(chan struct{})
	go func() { defer close(workerDone); FileOperate(id) }()

	waitFor(t, "the worker to start copying", 15*time.Second, func() bool {
		c := opCounter(t, id)
		return c != nil && c.Done() > 0
	})

	if !CancelOp(id, 15*time.Second) {
		t.Fatal("CancelOp reported that the worker was still running: the copy is not cancellable")
	}
	waitFor(t, "the worker goroutine to exit", 5*time.Second, closed(workerDone))

	// The bytes must stop flowing. The waitFor above only proves the worker
	// goroutine returned; this proves it took the source with it, which is what
	// the operator was promised when the dashboard said the copy was gone.
	if fed := feeder.requireSourceReleased(t, 15*time.Second); fed <= 0 {
		t.Fatal("the feeder delivered no bytes: the cancel never landed inside the streaming copy")
	}

	if _, ok := FileQueue.Load(id); ok {
		t.Fatal("the cancelled operation is still in FileQueue: a ghost the UI can never clear")
	}
	OpMu.Lock()
	remaining := len(OpStrArr)
	OpMu.Unlock()
	if remaining != 0 {
		t.Fatalf("OpStrArr still holds %d entries after the cancel", remaining)
	}
}

// TestDroppedOperationIsNeverRepublished pins the anti-resurrection rule for
// both writers of a queue entry. The worker's final write and the 500ms
// progress poller used to Store over a key a cancel had just deleted, which
// left a FileQueue entry that was no longer in OpStrArr: invisible in the
// transfer panel, impossible to cancel again, and enough to keep the notify
// loop spinning for the rest of the daemon's life.
func TestDroppedOperationIsNeverRepublished(t *testing.T) {
	resetOpState(t)

	const id = "op-ghost"
	op := model.FileOperate{
		Type:       "copy",
		To:         "/tmp",
		TotalSize:  100,
		Item:       []model.FileItem{{From: "/tmp/whatever", Size: 100}},
		LastUpdate: time.Now(),
	}
	FileQueue.Store(id, op)
	OpMu.Lock()
	OpStrArr = []string{id}
	OpMu.Unlock()
	RegisterOp(id)

	if !CancelOp(id, time.Second) {
		t.Fatal("cancelling an operation that never started should complete immediately")
	}

	if publishOpProgress(id) {
		t.Fatal("the progress poller republished a cancelled operation")
	}
	op.Finished = true
	if storeOpIfLive(id, op) {
		t.Fatal("a late worker write republished a cancelled operation")
	}

	entries := 0
	FileQueue.Range(func(_, _ any) bool { entries++; return true })
	if entries != 0 {
		t.Fatalf("FileQueue still holds %d entries after the cancel", entries)
	}
}

// TestCancelDoesNotAdvanceQueueUntilWorkerUnwinds covers the worst case: the
// worker is stuck in a read on a dead mount, so the cancel cannot take effect
// immediately. The old code deleted the tracking and launched the next
// operation regardless — two workers on the same destination, with the first
// one invisible. The contract now is: report honestly that it has not stopped,
// keep it on screen, hold the queue, and settle everything once it unwinds.
func TestCancelDoesNotAdvanceQueueUntilWorkerUnwinds(t *testing.T) {
	resetOpState(t)

	dir := t.TempDir()
	stalled := filepath.Join(dir, "src", "stream")
	mkfifoT(t, stalled)
	next := filepath.Join(dir, "src2", "report.txt")
	writeFileT(t, next, "payload")
	to := filepath.Join(dir, "to")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	feeder := startFifoFeeder(t, stalled)

	const stuckID, nextID = "op-stuck", "op-next"
	FileQueue.Store(stuckID, model.FileOperate{
		Type: "copy", To: to, TotalSize: 1 << 40,
		Item: []model.FileItem{{From: stalled, Size: 1 << 40}}, LastUpdate: time.Now(),
	})
	FileQueue.Store(nextID, model.FileOperate{
		Type: "copy", To: to, TotalSize: int64(len("payload")),
		Item: []model.FileItem{{From: next, Size: int64(len("payload"))}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = []string{stuckID, nextID}
	OpMu.Unlock()
	RegisterOp(stuckID)
	RegisterOp(nextID)

	workerDone := make(chan struct{})
	go func() { defer close(workerDone); FileOperate(stuckID) }()

	waitFor(t, "the worker to start copying", 15*time.Second, func() bool {
		c := opCounter(t, stuckID)
		return c != nil && c.Done() > 0
	})

	// Starve the source so the worker ends up blocked inside read(2), where a
	// cancelled context cannot reach it.
	feeder.pause()
	counter := opCounter(t, stuckID)
	waitFor(t, "the worker to block on the stalled source", 15*time.Second, func() bool {
		before := counter.Done()
		time.Sleep(100 * time.Millisecond)
		return counter.Done() == before
	})

	if CancelOp(stuckID, 200*time.Millisecond) {
		t.Fatal("CancelOp claimed a worker blocked on a dead source had stopped")
	}

	// Nothing may be reported as done, and nothing new may start, while the
	// worker still owns the destination. Give a mis-behaving implementation
	// time to do the wrong thing.
	time.Sleep(300 * time.Millisecond)
	if _, ok := FileQueue.Load(stuckID); !ok {
		t.Fatal("the operation vanished from the dashboard while its worker was still running")
	}
	if !OpCancelPending(stuckID) {
		t.Fatal("the operation is not reported as cancelling")
	}
	if _, err := os.Lstat(filepath.Join(to, "report.txt")); !os.IsNotExist(err) {
		t.Fatal("the next queued operation started while the cancelled worker still owned the destination")
	}

	// Let the stalled read complete: the worker must now notice the cancel,
	// unwind, retire its own entry and release the queue.
	feeder.resume()

	waitFor(t, "the cancelled worker to unwind", 15*time.Second, closed(workerDone))
	waitFor(t, "the cancelled operation to disappear", 15*time.Second, func() bool {
		_, ok := FileQueue.Load(stuckID)
		return !ok
	})
	waitFor(t, "the next queued operation to run", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(to, "report.txt"))
		return err == nil
	})
	if got := readFileT(t, filepath.Join(to, "report.txt")); got != "payload" {
		t.Fatalf("the next operation copied %q, want %q", got, "payload")
	}
}

// TestReapedOperationIsNotResurrectedByItsWorker pins defect (a) of the report:
// the transfer panel retires an operation (FINISHED once the byte count
// reaches the total, or an admin queue reset) while the worker is still
// unwinding, and the worker's final write then re-created the entry. The
// resulting FileQueue entry was no longer in OpStrArr, so it appeared in no
// broadcast, could not be cancelled, and kept the notify loop alive forever.
func TestReapedOperationIsNotResurrectedByItsWorker(t *testing.T) {
	resetOpState(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src", "stream")
	mkfifoT(t, src)
	to := filepath.Join(dir, "to")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	feeder := startFifoFeeder(t, src)

	const id = "op-reaped"
	FileQueue.Store(id, model.FileOperate{
		Type: "copy", To: to, TotalSize: 1,
		Item: []model.FileItem{{From: src, Size: 1}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = []string{id}
	OpMu.Unlock()
	RegisterOp(id)

	workerDone := make(chan struct{})
	go func() { defer close(workerDone); FileOperate(id) }()
	waitFor(t, "the worker to start copying", 15*time.Second, func() bool {
		c := opCounter(t, id)
		return c != nil && c.Done() > 0
	})

	// The panel retires it while the copy is still in flight.
	ReapOp(id)

	// Now let the copy end normally: this is the moment the worker publishes
	// its result.
	feeder.finish()
	waitFor(t, "the worker goroutine to exit", 15*time.Second, closed(workerDone))

	entries := 0
	FileQueue.Range(func(_, _ any) bool { entries++; return true })
	if entries != 0 {
		t.Fatalf("the finished worker re-created %d queue entries after the operation was retired", entries)
	}
}

// TestQueueNeverRunsTwoWorkersAtOnce pins the serialization the cancellation
// design depends on. fileOperateSnapshot retires an operation and immediately
// calls ExecOpFile the moment the byte count reaches the declared total —
// which happens while the worker is still holding the destination. Starting
// the next operation there puts two workers on the same directory.
func TestQueueNeverRunsTwoWorkersAtOnce(t *testing.T) {
	resetOpState(t)

	dir := t.TempDir()
	slow := filepath.Join(dir, "src", "stream")
	mkfifoT(t, slow)
	next := filepath.Join(dir, "src2", "report.txt")
	writeFileT(t, next, "payload")
	to := filepath.Join(dir, "to")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	feeder := startFifoFeeder(t, slow)

	const runningID, nextID = "op-running", "op-next"
	// TotalSize 1 makes the "processed >= total" completion rule fire on the
	// first chunk, long before the worker is done.
	FileQueue.Store(runningID, model.FileOperate{
		Type: "copy", To: to, TotalSize: 1,
		Item: []model.FileItem{{From: slow, Size: 1}}, LastUpdate: time.Now(),
	})
	FileQueue.Store(nextID, model.FileOperate{
		Type: "copy", To: to, TotalSize: int64(len("payload")),
		Item: []model.FileItem{{From: next, Size: int64(len("payload"))}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = []string{runningID, nextID}
	OpMu.Unlock()
	RegisterOp(runningID)
	RegisterOp(nextID)

	workerDone := make(chan struct{})
	go func() { defer close(workerDone); FileOperate(runningID) }()
	waitFor(t, "the worker to start copying", 15*time.Second, func() bool {
		c := opCounter(t, runningID)
		return c != nil && c.Done() > 0
	})
	feeder.pause()

	publishOpProgress(runningID)
	tasks := fileOperateSnapshot()
	reported := false
	for _, task := range tasks {
		if task.Id == runningID {
			reported = true
			if task.Status != "FINISHED" {
				t.Fatalf("operation reported as %q, want FINISHED", task.Status)
			}
		}
	}
	if !reported {
		t.Fatal("the operation was not reported at all")
	}

	// The retire above advanced the queue. Give a mis-behaving implementation
	// time to start the next operation on top of the running one.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Lstat(filepath.Join(to, "report.txt")); !os.IsNotExist(err) {
		t.Fatal("the next operation started while the previous worker still owned the destination")
	}

	feeder.finish()
	waitFor(t, "the first worker to finish", 15*time.Second, closed(workerDone))
	waitFor(t, "the next queued operation to run", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(to, "report.txt"))
		return err == nil
	})
	if got := readFileT(t, filepath.Join(to, "report.txt")); got != "payload" {
		t.Fatalf("the next operation copied %q, want %q", got, "payload")
	}
}
