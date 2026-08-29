package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// scanTTL is how long a scan's plan stays usable. Long enough for a human to
// read the screen and decide; short enough that the world has not changed
// underneath. Expiry is not the safety net — revalidation at unlink time is —
// but it keeps the UI from acting on a picture from last week.
const scanTTL = 15 * time.Minute

var (
	ErrUnknownCategory = errors.New("unknown maintenance category")
	ErrStaleScan       = errors.New("scan token no longer valid, re-analyse first")
	ErrNoScan          = errors.New("no scan has been run yet")
	ErrBlockedCategory = errors.New("category is blocked")
	ErrUnknownItem     = errors.New("unknown item id")
	ErrScanInProgress  = errors.New("a scan is already in progress")
)

// PreflightError is a refusal to start, with the reason code the API returns
// in its 409 body.
type PreflightError struct {
	Code   string
	Reason string
}

func (e *PreflightError) Error() string { return e.Code + ": " + e.Reason }

// Engine owns the guard, the scan state and the run lock.
type Engine struct {
	env   *Env
	guard *Guard
	fdx   fdLookup

	scanMu sync.Mutex // single-flight for Scan
	runMu  sync.Mutex // single-flight for Clean (rule 11)

	// scanCtx carries the active scan's context so a detector can honour the
	// handler's timeout. atomic because a detector reads it while Scan writes.
	scanCtx atomic.Pointer[context.Context]

	mu       sync.RWMutex
	lastScan *ScanResult
}

// New builds an engine. It fails loudly if the whitelist of roots is not
// something the guard is willing to accept, so a bad catalogue never reaches a
// production run.
func New(env *Env) (*Engine, error) {
	if env == nil {
		env = DefaultEnv()
	}
	if env.UploadSessionsFile == "" {
		env.UploadSessionsFile = defaultUploadSessionsFile()
	}
	guard, err := NewGuard(env.allowedRoots(), env.remoteMounts())
	if err != nil {
		return nil, err
	}
	e := &Engine{env: env, guard: guard, fdx: env.FD}
	if e.fdx == nil {
		e.fdx = newProcFDIndex(env.ProcRoot)
	}
	return e, nil
}

// NewDefault is New(DefaultEnv()).
func NewDefault() (*Engine, error) { return New(DefaultEnv()) }

// Env exposes the wiring for the handler layer (paths shown in the UI).
func (e *Engine) Env() *Env { return e.env }

// Guard exposes the chokepoint for tests and for logging its roots.
func (e *Engine) Guard() *Guard { return e.guard }

// ─── Scan ───────────────────────────────────────────────────────────────────

// Scan runs every category. It mutates nothing: a category that cannot prove
// its case comes back blocked, with the size still filled in so the UI can say
// what is being withheld and why.
//
// progress may be nil; it is called before each category so the UI can render
// "Analysing… 3 of 6".
func (e *Engine) Scan(ctx context.Context, progress func(category string, index, total int)) (*ScanResult, error) {
	if !e.scanMu.TryLock() {
		return nil, ErrScanInProgress
	}
	defer e.scanMu.Unlock()

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	res := &ScanResult{
		Token:     token,
		StartedAt: e.env.now(),
		ByClass:   map[Class]int64{},
		Notices:   ReportOnlyNotices(),
	}
	res.Disk, _ = diskUsage("/")

	// Refresh the unwalkable-mount set for this scan. It was captured once at
	// engine construction, so a share mounted after boot was invisible; a scan
	// must see the mount table as it is now, not as it was at startup.
	e.guard.setRemoteMounts(e.env.remoteMounts())
	// Make the scan's deadline reachable from inside a category detector, so a
	// future deep walk (scratch) can be cut by the handler's timeout instead of
	// only being checked between categories.
	e.setScanCtx(ctx)
	defer e.setScanCtx(nil)

	order := e.enabledOrder()
	total := len(order)
	for i, key := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if progress != nil {
			progress(key, i, total)
		}
		f := e.scanCategory(key)
		res.Findings = append(res.Findings, *f)
		if !f.Blocked {
			res.ReclaimableBytes += f.ReclaimableBytes
		}
		res.ByClass[f.Class] += f.ReclaimableBytes
	}
	res.FinishedAt = e.env.now()

	e.mu.Lock()
	e.lastScan = res
	e.mu.Unlock()
	return res, nil
}

// categoryEnabled reports whether a category is wired into production scans and
// runs. Everything is on except scratch, which stays off until its untouchable
// set (rule 1) exists — the toggle lives on Env so a test can turn it on.
func (e *Engine) categoryEnabled(key string) bool {
	if key == CatScratch {
		return e.env.ScratchEnabled
	}
	return true
}

// enabledOrder is catalogOrder minus whatever is not wired in yet.
func (e *Engine) enabledOrder() []string {
	out := make([]string, 0, len(catalogOrder))
	for _, key := range catalogOrder {
		if e.categoryEnabled(key) {
			out = append(out, key)
		}
	}
	return out
}

// setScanCtx publishes the running scan's context so detectors can honour the
// handler's timeout. Stored atomically because a detector reads it while Scan
// owns the write; there is only ever one scan at a time (scanMu).
func (e *Engine) setScanCtx(ctx context.Context) {
	if ctx == nil {
		e.scanCtx.Store(nil)
		return
	}
	e.scanCtx.Store(&ctx)
}

// scanErr returns the scan context's error, or nil when there is no active
// scan context. A detector that walks should check this and bail with SkipAll.
func (e *Engine) scanErr() error {
	if p := e.scanCtx.Load(); p != nil {
		return (*p).Err()
	}
	return nil
}

// scanCategory isolates one category's scan so a panic in a detector cannot
// take the whole screen down with it. A detector that panics is a detector
// whose findings we must not trust, so the category comes back blocked.
func (e *Engine) scanCategory(key string) (f *Finding) {
	c := catalog[key]
	defer func() {
		if r := recover(); r != nil {
			logger.Error("maintenance scan panicked",
				zap.String("category", key), zap.Any("panic", r))
			f = newFinding(key)
			f.block(BlockScanError, "the scan for this category failed")
		}
	}()
	return c.scan(e)
}

// LastScan returns the stored scan, or nil.
func (e *Engine) LastScan() *ScanResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastScan
}

// ForAPI copies a scan result with each finding's item list capped, so a
// category with 40 000 entries does not turn one HTTP response into a
// multi-megabyte payload. The full plan stays in memory and in the manifest.
func (r *ScanResult) ForAPI(maxItems int) *ScanResult {
	if r == nil {
		return nil
	}
	out := *r
	out.Findings = make([]Finding, len(r.Findings))
	for i, f := range r.Findings {
		cp := f
		if maxItems > 0 && len(f.Items) > maxItems {
			cp.Items = append([]Item{}, f.Items[:maxItems]...)
			cp.Truncated = true
		} else {
			cp.Items = append([]Item{}, f.Items...)
		}
		// Drop the internal removal plan from the copy that leaves the process.
		for j := range cp.Items {
			cp.Items[j].targets = nil
		}
		out.Findings[i] = cp
	}
	return &out
}

// FindingFor returns one category's finding from a scan result.
func (r *ScanResult) FindingFor(category string) *Finding {
	if r == nil {
		return nil
	}
	for i := range r.Findings {
		if r.Findings[i].Category == category {
			return &r.Findings[i]
		}
	}
	return nil
}

// ─── Clean ──────────────────────────────────────────────────────────────────

// Clean executes ONE category against the stored plan.
//
// scanToken ties the request to the scan that produced the item IDs: a client
// holding a page from before the last scan gets ErrStaleScan (410) and
// re-analyses, instead of acting on a plan the server has replaced.
func (e *Engine) Clean(ctx context.Context, scanToken, category string, itemIDs []string) (*RunResult, error) {
	c, ok := catalog[category]
	if !ok || !e.categoryEnabled(category) {
		// A disabled category (scratch, until rule 1 lands) is treated as
		// unknown: it never appears in a scan, so a client asking to clean it
		// is either stale or probing. Refuse before any plan is built.
		return nil, ErrUnknownCategory
	}
	// Same mount refresh as Scan: a Clean preflight may descend, and it must not
	// trust a mount set captured at boot.
	e.guard.setRemoteMounts(e.env.remoteMounts())

	e.mu.RLock()
	scan := e.lastScan
	e.mu.RUnlock()
	if scan == nil {
		return nil, ErrNoScan
	}
	if scanToken == "" || scanToken != scan.Token {
		return nil, ErrStaleScan
	}
	if e.env.now().Sub(scan.FinishedAt) > scanTTL {
		return nil, ErrStaleScan
	}

	f := scan.FindingFor(category)
	if f == nil {
		return nil, ErrUnknownCategory
	}
	if f.Blocked {
		return nil, &PreflightError{Code: f.BlockedCode, Reason: f.BlockedReason}
	}

	items, err := c.plan(e, f, itemIDs)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUnknownItem
	}

	// Rule 11: one run at a time, and only with the system quiet. The process
	// mutex stops two handlers; the lock file stops two processes (an old
	// binary left behind by an update, a CLI). flock is released by the kernel
	// when the process dies, so a crash cannot wedge the feature forever —
	// which a PID file alone would.
	if !e.runMu.TryLock() {
		return nil, &PreflightError{Code: BlockRunInProgress, Reason: "another maintenance run is in progress"}
	}
	defer e.runMu.Unlock()

	unlock, err := e.acquireRunLock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	if perr := e.systemQuiet(); perr != nil {
		return nil, perr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	runID := e.env.now().UTC().Format("20060102-150405")
	return c.exec(e, f, items, runID)
}

// acquireRunLock takes an exclusive, non-blocking flock on the state file and
// records our pid in it for the humans.
func (e *Engine) acquireRunLock() (func(), error) {
	if err := os.MkdirAll(e.env.StateDir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(e.env.lockFile(), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o640)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, &PreflightError{Code: BlockRunInProgress, Reason: "another process holds the maintenance lock"}
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// systemQuiet is rule 11's precondition, minus the parts only the handler can
// see (which arrive through Env.SystemBusy).
func (e *Engine) systemQuiet() *PreflightError {
	if stage := e.updateStage(); isUpdateBusyStage(stage) {
		return &PreflightError{Code: BlockUpdateRunning, Reason: "an update is being applied (" + stage + ")"}
	}
	if e.env.SystemBusy != nil {
		if busy, why := e.env.SystemBusy(); busy {
			return &PreflightError{Code: BlockSystemBusy, Reason: why}
		}
	}
	return nil
}

// ─── Generic plan and exec ──────────────────────────────────────────────────

// defaultPlan resolves ticked item IDs against the stored finding. IDs that do
// not exist are an error, not a silent skip: a client sending an unknown ID is
// either stale or wrong, and both deserve to be told.
func defaultPlan(e *Engine, f *Finding, ids []string) ([]Item, error) {
	byID := map[string]Item{}
	for _, it := range f.Items {
		byID[it.ID] = it
	}
	var out []Item
	lim := newPlanLimits()
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownItem, id)
		}
		var bytes int64
		for _, t := range it.targets {
			bytes += t.Bytes
		}
		if err := lim.add(len(it.targets), bytes); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// execWithPrecheck wraps the generic executor with the category's own
// precondition, re-evaluated at RUN time. Scanning proved apt was idle
// minutes ago; that is not the same statement as "apt is idle now".
func execWithPrecheck(pre func(*Engine, []Item) *PreflightError) execFunc {
	return func(e *Engine, f *Finding, items []Item, runID string) (*RunResult, error) {
		if pre != nil {
			if perr := pre(e, items); perr != nil {
				return nil, perr
			}
		}
		return defaultExec(e, f, items, runID)
	}
}

// defaultExec is the only executor. It writes the manifest first (rule 12),
// then removes every target through the guard, leaves before containers.
func defaultExec(e *Engine, f *Finding, items []Item, runID string) (*RunResult, error) {
	res := &RunResult{
		RunID:     runID,
		Category:  f.Category,
		StartedAt: e.env.now(),
	}
	probe := probePath(items)
	res.DiskBefore, _ = diskUsage(probe)

	reasons := map[string]string{}
	for _, it := range items {
		reasons[it.ID] = it.Detail
	}
	m := newManifest(runID, runID, f.Category, e.env.RunningVersion, items, reasons, true)
	m.DiskBefore = res.DiskBefore
	if err := m.write(e.env.manifestDir()); err != nil {
		// No manifest, no run. This is the rule that makes the feature
		// supportable, so it is a hard failure and not a warning.
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	res.ManifestID = m.ID

	// Start the run with a fresh view of open descriptors instead of the one
	// the scan built minutes ago.
	if r, ok := e.fdx.(interface{ Refresh() }); ok {
		r.Refresh()
	}

	idx := 0
	for _, it := range items {
		itemOK := true
		for _, t := range it.targets {
			err := e.guard.Remove(t, e.fdx)
			if idx < len(m.Entries) && m.Entries[idx].Path == t.Path {
				m.Entries[idx].Removed = err == nil
				if err != nil {
					m.Entries[idx].Error = err.Error()
				}
			}
			idx++
			switch {
			case err == nil:
				res.RemovedEntries++
				res.RemovedBytes += t.Bytes
				m.RemovedBytes += t.Bytes
			case isStructuralRefusal(err):
				// The guard refused the path itself, not the state of the
				// entry. That means the plan contains something it should
				// never have contained — abort the run and keep the manifest
				// as evidence (rule 17).
				logger.Error("maintenance run aborted by the deletion guard",
					zap.String("category", f.Category), zap.String("path", t.Path), zap.Error(err))
				res.Failed = append(res.Failed, Failure{ID: it.ID, Path: t.Path, Reason: err.Error()})
				res.FinishedAt = e.env.now()
				finishManifest(e, m, res, probe)
				return res, err
			default:
				itemOK = false
				if isRevalidationSkip(err) {
					res.Skipped = append(res.Skipped, Failure{ID: it.ID, Path: t.Path, Reason: err.Error()})
				} else {
					res.Failed = append(res.Failed, Failure{ID: it.ID, Path: t.Path, Reason: err.Error()})
				}
			}
		}
		if itemOK {
			res.RemovedItems++
		}
	}

	res.FinishedAt = e.env.now()
	finishManifest(e, m, res, probe)
	logger.Info("maintenance run finished",
		zap.String("category", f.Category),
		zap.String("run", runID),
		zap.Int("entries", res.RemovedEntries),
		zap.Int64("bytes", res.RemovedBytes),
		zap.Int64("disk_delta", res.DiskDeltaBytes))
	return res, nil
}

// finishManifest records the outcome and the statfs delta — the only number in
// this feature that is a measurement rather than an estimate.
func finishManifest(e *Engine, m *Manifest, res *RunResult, probe string) {
	res.DiskAfter, _ = diskUsage(probe)
	res.DiskDeltaBytes = res.DiskAfter.Free - res.DiskBefore.Free
	m.FinishedAt = res.FinishedAt
	m.DiskAfter = res.DiskAfter
	m.DiskDeltaBytes = res.DiskDeltaBytes
	if err := m.write(e.env.manifestDir()); err != nil {
		logger.Error("could not update maintenance manifest", zap.Error(err))
	}
}

// isStructuralRefusal reports whether the guard rejected the PATH — a bug or
// an injection — as opposed to the entry having changed.
func isStructuralRefusal(err error) bool {
	return errors.Is(err, ErrDenied) ||
		errors.Is(err, ErrOutsideRoots) ||
		errors.Is(err, ErrTooShallow) ||
		errors.Is(err, ErrEscapesRoot) ||
		errors.Is(err, ErrCrossMount) ||
		errors.Is(err, ErrRemoteMount) ||
		errors.Is(err, ErrPlanTooLarge)
}

// isRevalidationSkip reports whether a guard did its job on one entry: the
// file changed, someone has it open, the directory is not empty. The run
// continues and the item is reported as skipped.
func isRevalidationSkip(err error) bool {
	return errors.Is(err, ErrChanged) ||
		errors.Is(err, ErrIdentity) ||
		errors.Is(err, ErrOpenFD) ||
		errors.Is(err, ErrNotEmpty)
}

// probePath picks the filesystem to measure. The first target's directory,
// because a category's entries all live under one root, and "/" when there is
// nothing to go on.
func probePath(items []Item) string {
	for _, it := range items {
		for _, t := range it.targets {
			return filepath.Dir(t.Path)
		}
	}
	return "/"
}

// ─── Manifest access ────────────────────────────────────────────────────────

// Manifests lists past runs, newest first.
func (e *Engine) Manifests() ([]ManifestSummary, error) {
	return listManifests(e.env.manifestDir())
}

// Manifest returns one past run by id.
func (e *Engine) Manifest(id string) (*Manifest, error) {
	return readManifest(e.env.manifestDir(), id)
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
