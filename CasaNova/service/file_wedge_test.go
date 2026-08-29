package service

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/model"
)

// These tests pin what happens when a worker is parked in a syscall that no
// context can interrupt — a hard-mounted NFS/SMB share whose server left the
// network. That worker used to hold the single execution slot for ever: every
// other file operation on the box, including ones on unrelated local disks,
// stayed queued behind it, and the operator's only feedback was an entry stuck
// on CANCELLING.
//
// Two rules are pinned here at once, and they pull in opposite directions:
//   - the queue must recover — unrelated work runs again and the operator is
//     told which mount is not answering; and
//   - the parked worker must keep owning its own tree, because it can return
//     from its syscall at any instant and go on writing. Anything that relaxes
//     the second rule trades a stalled queue for corrupted data.

// resetWedgeState clears the parked-worker registry and installs deterministic
// stubs for the two things that would otherwise reach the real machine: the
// mount table and the liveness probe. The defaults are the benign ones — no
// mount information, every path answers — so each test only has to describe the
// failure it is actually about.
func resetWedgeState(t *testing.T) {
	t.Helper()
	clearWedges := func() {
		wedgedOps.Range(func(k, _ any) bool { wedgedOps.Delete(k); return true })
	}
	clearWedges()

	origAfter, origStall, origTimeout := WedgeAfter, OpStallProbeAfter, wedgeProbeTimeout
	origProbe, origMount := probePathAlive, mountHolding
	t.Cleanup(func() {
		clearWedges()
		WedgeAfter, OpStallProbeAfter, wedgeProbeTimeout = origAfter, origStall, origTimeout
		probePathAlive, mountHolding = origProbe, origMount
	})

	mountHolding = func(string) string { return "" }
	probePathAlive = func(string, time.Duration) bool { return true }
}

// parkedCopy is one operation whose worker is blocked inside read(2) on a
// starved named pipe — the same place a worker sits when an NFS/SMB server stops
// answering, and the one place a cancelled context cannot reach it.
type parkedCopy struct {
	id         string
	feeder     *fifoFeeder
	workerDone chan struct{}
}

// startParkedCopy queues one copy, runs it, and returns once its worker is
// provably blocked on the source.
//
// The cleanup joins the worker before the test ends. Leaving it running is not a
// tidiness question: the operation tracking is process-wide, so a straggler from
// one test reaches finishOp during the NEXT test and deletes the control that
// test had just registered under the same id — which showed up as CancelOp
// reporting a stop that never happened, in a test about something else.
func startParkedCopy(t *testing.T, id, src, to string) *parkedCopy {
	t.Helper()
	mkfifoT(t, src)
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	feeder := startFifoFeeder(t, src)

	FileQueue.Store(id, model.FileOperate{
		Type: "copy", To: to, TotalSize: 1 << 40,
		Item: []model.FileItem{{From: src, Size: 1 << 40}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = append(OpStrArr, id)
	OpMu.Unlock()
	RegisterOp(id)

	done := make(chan struct{})
	go func() { defer close(done); FileOperate(id) }()
	t.Cleanup(func() {
		feeder.finish()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the parked worker never exited; later tests would inherit it")
		}
	})

	waitFor(t, "the worker to start copying", 15*time.Second, func() bool {
		c := opCounter(t, id)
		return c != nil && c.Done() > 0
	})
	// Starve the source: from here the worker is blocked in read(2).
	feeder.pause()
	counter := opCounter(t, id)
	waitFor(t, "the worker to block on the stalled source", 15*time.Second, func() bool {
		before := counter.Done()
		time.Sleep(100 * time.Millisecond)
		return counter.Done() == before
	})

	return &parkedCopy{id: id, feeder: feeder, workerDone: done}
}

// queueCopy appends a plain single-file copy to the queue without starting it.
func queueCopy(t *testing.T, id, from, to string) {
	t.Helper()
	writeFileT(t, from, "payload")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	FileQueue.Store(id, model.FileOperate{
		Type: "copy", To: to, TotalSize: int64(len("payload")),
		Item: []model.FileItem{{From: from, Size: int64(len("payload"))}}, LastUpdate: time.Now(),
	})
	OpMu.Lock()
	OpStrArr = append(OpStrArr, id)
	OpMu.Unlock()
	RegisterOp(id)
}

// parkByCancelling asks for a cancellation the worker cannot honour and waits
// for the queue to give up predicting and start reporting.
func parkByCancelling(t *testing.T, id string) WedgedOp {
	t.Helper()
	if CancelOp(id, 20*time.Millisecond) {
		t.Fatal("CancelOp claimed a worker blocked on a dead source had stopped")
	}
	waitFor(t, "the parked worker to be reported as wedged", 15*time.Second, func() bool {
		_, ok := OpWedged(id)
		return ok
	})
	rec, _ := OpWedged(id)
	return rec
}

// TestWedgedWorkerReleasesQueueForUnrelatedTree is the headline recovery: a
// cancellation that never lands stops being a prediction ("it is about to
// stop") and becomes a report ("it is parked, on this mount"), and work on other
// trees runs again instead of waiting for a server that may never come back.
func TestWedgedWorkerReleasesQueueForUnrelatedTree(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, otherID := t.Name()+"-parked", t.Name()+"-elsewhere"
	dir := t.TempDir()
	stuck := startParkedCopy(t, stuckID, filepath.Join(dir, "src", "stream"), filepath.Join(dir, "to"))
	otherTo := filepath.Join(dir, "other")
	queueCopy(t, otherID, filepath.Join(dir, "src2", "report.txt"), otherTo)

	rec := parkByCancelling(t, stuckID)
	if rec.Reason != WedgeReasonCancelUnwind {
		t.Fatalf("wedge reason = %q, want %q", rec.Reason, WedgeReasonCancelUnwind)
	}
	if rec.Holder == "" {
		t.Fatal("the wedge names no path: the operator is told something is stuck but not what")
	}

	// The operation stays on screen — it really has not finished — and it must
	// say WEDGED rather than CANCELLING, which claims it is about to stop.
	if _, ok := FileQueue.Load(stuckID); !ok {
		t.Fatal("the parked operation vanished from the dashboard while its worker was still in the kernel")
	}
	status := ""
	for _, task := range fileOperateSnapshot() {
		if task.Id == stuckID {
			status = task.Status
		}
	}
	if status != "WEDGED" {
		t.Fatalf("parked operation reported as %q, want WEDGED", status)
	}

	// The whole point: unrelated work runs again.
	waitFor(t, "the unrelated queued operation to run", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(otherTo, "report.txt"))
		return err == nil
	})
	if got := readFileT(t, filepath.Join(otherTo, "report.txt")); got != "payload" {
		t.Fatalf("the unrelated operation copied %q, want %q", got, "payload")
	}

	// And when the mount answers again, the parked worker unwinds its cancel and
	// the reservation goes with it.
	stuck.feeder.resume()
	waitFor(t, "the parked worker to unwind", 15*time.Second, closed(stuck.workerDone))
	waitFor(t, "the wedge record to be released", 15*time.Second, func() bool {
		_, ok := OpWedged(stuckID)
		return !ok
	})
	if _, ok := FileQueue.Load(stuckID); ok {
		t.Fatal("the cancelled operation is still in FileQueue after its worker unwound")
	}
	if ops := WedgedFileOps(); len(ops) != 0 {
		t.Fatalf("health still reports %d parked operations", len(ops))
	}
}

// TestQueuePumpStartsWorkEnqueuedAfterAWedge isolates the pump. Everything else
// that starts an operation hangs off some operation finishing, or off an enqueue
// that found the queue EMPTY (route/v1 only pumps when it queued the first
// entry). A parked worker's entry stays in the queue by design, so an operation
// queued after it had nobody left to start it — and waited for ever even though
// nothing at all was running.
//
// The test deliberately never calls ExecOpFile: the progress broadcast is the
// only thing still ticking in that state, so the broadcast is what has to keep
// the queue moving.
func TestQueuePumpStartsWorkEnqueuedAfterAWedge(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, laterID := t.Name()+"-parked", t.Name()+"-queued-later"
	dir := t.TempDir()
	startParkedCopy(t, stuckID, filepath.Join(dir, "src", "stream"), filepath.Join(dir, "to"))
	parkByCancelling(t, stuckID)

	// Queued only now, into a queue that is not empty: nothing pumps it.
	laterTo := filepath.Join(dir, "later")
	queueCopy(t, laterID, filepath.Join(dir, "src3", "report.txt"), laterTo)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(filepath.Join(laterTo, "report.txt")); err == nil {
			break
		}
		fileOperateSnapshot() // the 3s broadcast tick, and nothing else
		time.Sleep(20 * time.Millisecond)
	}
	if got := readFileT(t, filepath.Join(laterTo, "report.txt")); got != "payload" {
		t.Fatalf("the operation queued after the wedge copied %q, want %q: nothing ever started it", got, "payload")
	}
}

// TestWedgedWorkerStillOwnsItsOwnTree is the half that must never be traded
// away. Releasing the queue slot does not mean the parked worker released its
// files: it is inside read(2)/write(2) and can resume at any instant. An
// operation aimed at the same destination therefore keeps waiting exactly as it
// did before — and starts by itself once the worker really lets go.
func TestWedgedWorkerStillOwnsItsOwnTree(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, sameTreeID := t.Name()+"-parked", t.Name()+"-same-tree"
	dir := t.TempDir()
	to := filepath.Join(dir, "to")
	stuck := startParkedCopy(t, stuckID, filepath.Join(dir, "src", "stream"), to)
	// Same destination directory as the parked worker.
	queueCopy(t, sameTreeID, filepath.Join(dir, "src2", "report.txt"), to)

	parkByCancelling(t, stuckID)

	// Give an implementation that only looks at the execution slot every chance
	// to start the second operation on the parked worker's destination.
	for i := 0; i < 5; i++ {
		ExecOpFile()
		fileOperateSnapshot()
		time.Sleep(60 * time.Millisecond)
	}
	if _, err := os.Lstat(filepath.Join(to, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("an operation started on the destination a parked worker still owns (lstat=%v): "+
			"that worker can resume writing there at any moment", err)
	}

	// Once the worker is really out of the kernel the tree is free, and the
	// waiting operation must start on its own — a reservation that outlives its
	// worker is just the original stall under a new name.
	stuck.feeder.resume()
	waitFor(t, "the parked worker to unwind", 15*time.Second, closed(stuck.workerDone))
	waitFor(t, "the operation that was waiting for the tree to run", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(to, "report.txt"))
		return err == nil
	})
	if got := readFileT(t, filepath.Join(to, "report.txt")); got != "payload" {
		t.Fatalf("the waiting operation copied %q, want %q", got, "payload")
	}
}

// TestBeginOpRefusesToClaimAReservedTree isolates the guard inside beginOp.
//
// The sibling test above goes through ExecOpFile, which skips reserved entries
// when it picks a candidate — so it would still pass with the claim-time guard
// removed, and would be pinning the wrong thing. The claim-time guard is the one
// that matters for a worker goroutine that was already waiting on the previous
// worker's control when the wedge was declared: nothing re-selects it, it simply
// wakes up and claims. This test therefore calls FileOperate directly.
func TestBeginOpRefusesToClaimAReservedTree(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, sameTreeID := t.Name()+"-parked", t.Name()+"-same-tree"
	dir := t.TempDir()
	to := filepath.Join(dir, "to")
	startParkedCopy(t, stuckID, filepath.Join(dir, "src", "stream"), to)
	queueCopy(t, sameTreeID, filepath.Join(dir, "src2", "report.txt"), to)

	parkByCancelling(t, stuckID)

	refused := make(chan struct{})
	go func() { defer close(refused); FileOperate(sameTreeID) }()
	waitFor(t, "the worker to refuse the reserved tree and return", 15*time.Second, closed(refused))

	if _, err := os.Lstat(filepath.Join(to, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("a worker claimed a tree a parked worker still owns (lstat=%v)", err)
	}
}

// TestWedgeReservesTheUnresponsiveMount: a worker parked on a share is evidence
// about the whole share, not about one directory. Sending the next operation to
// another directory of the same dead mount would only park a second worker, so
// the mount is reserved too — and named, so the operator knows what to fix.
func TestWedgeReservesTheUnresponsiveMount(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, sameMountID := t.Name()+"-parked", t.Name()+"-same-mount"
	dir := t.TempDir()
	// Both trees live under `dir`, which the mount table reports as one mount.
	mountHolding = func(string) string { return dir }

	stuck := startParkedCopy(t, stuckID, filepath.Join(dir, "share", "stream"), filepath.Join(dir, "share", "to"))
	otherTo := filepath.Join(dir, "share-other", "to")
	queueCopy(t, sameMountID, filepath.Join(dir, "elsewhere", "report.txt"), otherTo)

	if rec := parkByCancelling(t, stuckID); rec.Mount != dir {
		t.Fatalf("wedge mount = %q, want %q: the operator is not told which mount stopped answering", rec.Mount, dir)
	}

	for i := 0; i < 5; i++ {
		ExecOpFile()
		fileOperateSnapshot()
		time.Sleep(60 * time.Millisecond)
	}
	if _, err := os.Lstat(filepath.Join(otherTo, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("an operation was started on the same unresponsive mount as a parked worker (lstat=%v)", err)
	}

	stuck.feeder.resume()
	waitFor(t, "the parked worker to unwind", 15*time.Second, closed(stuck.workerDone))
	waitFor(t, "the waiting operation to run once the mount was released", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(otherTo, "report.txt"))
		return err == nil
	})
}

// TestWedgeDoesNotReserveTheRootMount: reserving "/" would put every operation
// on the box behind the parked worker, which is exactly the stall this change
// exists to end. A wedge whose mount is the root filesystem reserves the tree
// and nothing more.
func TestWedgeDoesNotReserveTheRootMount(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	WedgeAfter = 300 * time.Millisecond

	stuckID, otherID := t.Name()+"-parked", t.Name()+"-elsewhere"
	dir := t.TempDir()
	mountHolding = func(string) string { return string(os.PathSeparator) }

	startParkedCopy(t, stuckID, filepath.Join(dir, "src", "stream"), filepath.Join(dir, "to"))
	otherTo := filepath.Join(dir, "other")
	queueCopy(t, otherID, filepath.Join(dir, "src2", "report.txt"), otherTo)

	parkByCancelling(t, stuckID)

	waitFor(t, "the unrelated queued operation to run", 15*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(otherTo, "report.txt"))
		return err == nil
	})
}

// TestStalledButRespondingOperationIsNotWedged is the guard against the fix
// being worse than the bug. A perfectly healthy move sits at zero bytes for
// minutes — retiring a large destination to the recycle bin walks the whole tree
// before a byte is copied — and a stopwatch cannot tell that from a dead mount.
// Taking such an operation out of the queue slot on the strength of a byte
// counter alone would open its destination to the next operation while it is
// still working, so the verdict has to come from the filesystem itself.
func TestStalledButRespondingOperationIsNotWedged(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	OpStallProbeAfter = time.Millisecond

	var probes atomic.Int32
	probePathAlive = func(string, time.Duration) bool {
		probes.Add(1)
		return true // slow, not gone
	}

	id := t.Name() + "-slow"
	dir := t.TempDir()
	startParkedCopy(t, id, filepath.Join(dir, "src", "stream"), filepath.Join(dir, "to"))

	// Two ticks: the first records the byte count, the second sees it has not
	// moved and asks the filesystem.
	fileOperateSnapshot()
	time.Sleep(20 * time.Millisecond)
	fileOperateSnapshot()

	waitFor(t, "the stalled operation to be probed", 15*time.Second, func() bool {
		return probes.Load() > 0
	})
	ctl, ok := loadOpControl(id)
	if !ok {
		t.Fatal("the operation lost its control")
	}
	// Wait for the probe to settle, so this cannot pass merely because the
	// verdict had not been reached yet.
	waitFor(t, "the liveness probe to settle", 15*time.Second, func() bool {
		return !ctl.probing.Load()
	})
	time.Sleep(200 * time.Millisecond)

	if rec, wedged := OpWedged(id); wedged {
		t.Fatalf("a slow but responding operation was declared parked (%+v): its destination is now open to the next operation while it is still working", rec)
	}
	if OpQueueIdle() {
		t.Fatal("the execution slot was released for an operation whose filesystem answers")
	}
}

// TestUnresponsivePathIsWedgedWithoutCancelling covers the case nobody asked
// for: the NAS leaves the network and the operator has touched nothing. The
// queue has to recover on its own — but a report about a stuck worker must never
// stop a working one, so this path takes away the operation's hold on the queue
// and never touches its context.
func TestUnresponsivePathIsWedgedWithoutCancelling(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)
	OpStallProbeAfter = time.Millisecond
	wedgeProbeTimeout = 10 * time.Millisecond
	mountHolding = func(string) string { return "/mnt/nas" }
	probePathAlive = func(string, time.Duration) bool { return false }

	id := t.Name() + "-dead-mount"
	dir := t.TempDir()
	startParkedCopy(t, id, filepath.Join(dir, "src", "stream"), filepath.Join(dir, "to"))

	fileOperateSnapshot()
	time.Sleep(20 * time.Millisecond)
	fileOperateSnapshot()

	waitFor(t, "the operation on the dead mount to be reported as wedged", 15*time.Second, func() bool {
		_, ok := OpWedged(id)
		return ok
	})
	rec, _ := OpWedged(id)
	if rec.Reason != WedgeReasonUnresponsivePath {
		t.Fatalf("wedge reason = %q, want %q", rec.Reason, WedgeReasonUnresponsivePath)
	}
	if rec.Mount != "/mnt/nas" {
		t.Fatalf("wedge mount = %q, want /mnt/nas", rec.Mount)
	}
	if rec.Holder == "" {
		t.Fatal("the wedge names no path")
	}

	ctl, ok := loadOpControl(id)
	if !ok {
		t.Fatal("the parked operation lost its control")
	}
	if ctl.cancelRequested.Load() || ctl.ctx.Err() != nil {
		t.Fatal("declaring an operation parked cancelled it: a stuck REPORT must never stop the operation itself")
	}
	if !OpQueueIdle() {
		t.Fatal("the parked worker is still holding the execution slot, so the queue has not recovered")
	}
	if ops := WedgedFileOps(); len(ops) != 1 || ops[0].ID != id {
		t.Fatalf("health reports %+v, want exactly the parked operation", ops)
	}
}

// TestPathContainsHandlesRootAndSiblings pins the containment rule the
// reservations are built on. Root is the case that bites: naive prefix
// concatenation gives "//", which is a prefix of nothing, so a reservation
// covering the root filesystem would silently cover no path at all — and a
// reservation that covers nothing is worse than no reservation, because the
// queue releases the slot believing the tree is protected.
func TestPathContainsHandlesRootAndSiblings(t *testing.T) {
	cases := []struct {
		dir, p string
		want   bool
	}{
		{"/", "/DATA/media", true},
		{"/", "/", true},
		{"/DATA", "/DATA/media", true},
		{"/DATA", "/DATA", true},
		{"/DATA", "/DATA2/media", false}, // sibling, not child
		{"/DATA/media", "/DATA", false},  // parent is not contained in its child
		{"/mnt/nas", "/mnt/nas-backup", false},
	}
	for _, c := range cases {
		if got := pathContains(c.dir, c.p); got != c.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", c.dir, c.p, got, c.want)
		}
	}
}

// TestWedgeIsRefusedForARetiredWorker isolates the race between "declare this
// worker parked" and "the worker just came back". Only finishOp releases a
// reservation, so a record written after the worker exited would reserve its
// tree for the lifetime of the daemon — the same stall, permanent this time.
func TestWedgeIsRefusedForARetiredWorker(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)

	id := t.Name() + "-already-gone"
	FileQueue.Store(id, model.FileOperate{
		Type: "copy", To: "/DATA/target",
		Item: []model.FileItem{{From: "/mnt/nas/source"}},
	})
	RegisterOp(id)
	ctl, _ := loadOpControl(id)
	ctl.claimed.Store(true)
	wp1 := []string{"/DATA/target", "/mnt/nas/source"}
	ctl.paths.Store(&wp1)

	finishOp(id, ctl) // the worker unwinds first

	if markWedged(id, ctl, WedgeReasonCancelUnwind, "/mnt/nas/source") {
		t.Fatal("a worker that had already let go was recorded as parked: nothing would ever clear that reservation")
	}
	if _, ok := OpWedged(id); ok {
		t.Fatal("a reservation was left behind for a worker that is gone")
	}
	opTrackMu.Lock()
	_, blocked := wedgeConflictLocked([]string{"/DATA/target"})
	opTrackMu.Unlock()
	if blocked {
		t.Fatal("the destination of a finished worker is still reserved")
	}
}

// TestWedgeIsRefusedWhenTheWorkerOwnsNoKnownPaths: releasing the queue slot is
// only safe because the parked worker's tree stays reserved. If we cannot say
// what it owns we keep the slot held — the old, stalling behaviour — rather than
// let the next operation loose on a destination somebody may still be writing.
func TestWedgeIsRefusedWhenTheWorkerOwnsNoKnownPaths(t *testing.T) {
	resetOpState(t)
	resetWedgeState(t)

	id := t.Name() + "-unknown-tree"
	RegisterOp(id)
	ctl, _ := loadOpControl(id)
	ctl.claimed.Store(true)
	opTrackMu.Lock()
	activeOp = ctl
	opTrackMu.Unlock()

	if markWedged(id, ctl, WedgeReasonCancelUnwind, "") {
		t.Fatal("a worker whose tree is unknown was declared parked, releasing the queue with nothing reserved")
	}
	if OpQueueIdle() {
		t.Fatal("the execution slot was released without a reservation to replace it")
	}
}
