package v1

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/notifications"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/maintenance"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// HTTP surface of the maintenance module (design §5).
//
// This file is plumbing on purpose. It owns no deletion logic whatsoever: every
// removal goes through pkg/maintenance's single chokepoint, which is the one
// place that knows about allowed roots, minimum depth, the deny-list, the
// no-follow descent and the re-validation before each unlink. If something here
// ever looks like it is deciding what may be deleted, that is a bug.
//
// Three properties are load-bearing and none of them are conventions:
//
//   - The client sends a CATALOGUE KEY and OPAQUE ITEM IDs, never a path. A
//     path that never crosses the network is a path that cannot be traversed,
//     which removes the entire bug class rather than defending against it. The
//     ids are resolved against the plan the scan stored in memory — never
//     against anything that arrived in the request.
//   - Every handler re-checks authentication and PermSystemConfig in its first
//     two lines, including the read-only ones. The gateway reverse-proxies the
//     whole LAN over 127.0.0.1, so IsLoopback() is worthless here, and guest
//     deliberately holds PermSystemRead — which is why the scan needs
//     PermSystemConfig too: enumerating paths and sizes of the user's data area
//     is reconnaissance even though it mutates nothing. Each gate is followed by
//     a Committed check — see maintenanceGateOpen for why that third line is
//     not optional here.
//   - Responses are FLAT objects with no model.Result envelope, the same
//     exception GET /v1/sys/diagnosis already makes for findings endpoints.
//     Neither `model` package and neither `common_err` package is imported
//     here; refusals answer with {"reason": "<stable code>", "message": "<text
//     a human can act on>"} because the reason is the part the user needs and
//     an envelope would bury it.
//
// Both POST endpoints are asynchronous with polling — the antivirus.go /
// host_apt.go mould. Synchronous would be worse than slow: this product has
// already been bitten by a 10 s axios timeout, and a client that gives up while
// the server is still unlinking files would show an error over a run that is
// succeeding.

const (
	// maintenanceScanMaxRuntime caps a background scan. Phase 1 walks /var and
	// finishes in seconds; the scratch category walks the storage roots, which
	// on a 5.4 TB box is minutes of metadata reads. The cap exists so a wedged
	// walk cannot pin the "a scan is in progress" flag for the process lifetime,
	// not because a healthy scan ever approaches it.
	maintenanceScanMaxRuntime = 30 * time.Minute

	// maintenanceRunMaxRuntime caps a background run. Note what it does and does
	// not do: the engine checks the context before the executor starts, so the
	// cap bounds the preflight and the wait, not a removal already in flight.
	// That is deliberate — aborting halfway through a tree leaves a partial
	// state for no gain, and the manifest is written before the first unlink
	// precisely so that state stays explainable.
	maintenanceRunMaxRuntime = 30 * time.Minute

	// maintenanceMaxItemsPerFinding is design §5's cap: items are truncated at
	// 200 per finding with truncated:true, and the full plan stays in memory and
	// in the manifest on disk.
	maintenanceMaxItemsPerFinding = 200

	// maintenanceMaxItemIDs is a request-shape sanity bound. The engine's own
	// plan limits (50 000 entries, 16 GiB) are the real ceiling and they abort
	// the run; this one just refuses an absurd body before we allocate for it.
	maintenanceMaxItemIDs = 50000

	// maintenanceItemIDLen is the width of the opaque item id the scan mints
	// (sha256(category, path, inode) truncated). Validating the shape buys no
	// safety on its own — an unknown id is rejected by the map lookup anyway —
	// but it turns a malformed client into a 400 instead of a confusing 409.
	maintenanceItemIDLen = 16

	// maintenanceMaxTokenLen bounds the scan token we are willing to compare.
	maintenanceMaxTokenLen = 128
)

// maintenanceGateOpen reports whether the RBAC gate above it really let the
// request through.
//
// It exists because requirePerm denies by WRITING the 403 and then returning
// the nil that echo's ctx.JSON hands back on a successful write. The canonical
// call site — `if err := requirePerm(…); err != nil { return err }` — is
// therefore a no-op on the deny path: the response is committed, the client
// does see 403, and the handler carries straight on and does the work anyway.
// Verified against echo v4.12.0, where context.JSON returns only the
// serializer's error.
//
// In a read handler that costs a leaked body. Here it would delete files as
// root for a caller the gate just rejected, so this module fails closed on its
// own rather than waiting for the shared helper to change. Repairing
// requirePerm is the real fix and is reported separately; this line stays
// correct either way, because a gate that returns properly never commits the
// response in the first place.
func maintenanceGateOpen(ctx echo.Context) bool {
	return !ctx.Response().Committed
}

// ─── Engine ─────────────────────────────────────────────────────────────────

// The engine is built once per process (sync.Once, the diagnosis.go pattern —
// a lazy nil-check races on the first concurrent requests and can build two
// engines with two separate plans). It has to be a singleton for a second
// reason: the scan's plan lives inside it, and a clean must resolve its item
// ids against the very engine that minted them.
var (
	maintEngine     *maintenance.Engine
	maintEngineErr  error
	maintEngineOnce sync.Once
)

func getMaintenanceEngine() (*maintenance.Engine, error) {
	maintEngineOnce.Do(func() {
		env := maintenance.DefaultEnv()
		// Three hooks that only route/v1 can answer. They live here rather than
		// in pkg/maintenance because the state they read is held in this package
		// and in service/, and importing route/v1 from pkg/maintenance would be
		// an import cycle.
		env.UpdateStage = maintenanceUpdateStage
		env.AptSelfManaged = maintenanceAptSelfManaged
		env.SystemBusy = maintenanceSystemBusy
		maintEngine, maintEngineErr = maintenance.New(env)
		if maintEngineErr != nil {
			logger.Error("maintenance module unavailable", zap.Error(maintEngineErr))
			return
		}
		logger.Info("maintenance module ready",
			zap.Strings("roots", maintEngine.Guard().Roots()))
	})
	return maintEngine, maintEngineErr
}

// maintenanceUpdateStage hands the engine this process's in-memory update state.
// The engine's fallback reads the status file the update script writes, which is
// correct but coarser: the file only exists once an update has actually started
// writing it, while updateState changes the moment a stage does.
func maintenanceUpdateStage() string {
	updateMu.RLock()
	defer updateMu.RUnlock()
	return string(updateState.Stage)
}

// maintenanceAptSelfManaged lets the apt category say "CasaNova is applying
// system updates" instead of the generic "some process holds the dpkg lock".
// Same question, but an answer the user can act on: our own apt run has a
// visible progress screen, someone else's does not.
func maintenanceAptSelfManaged() (bool, string) {
	aptCheckMu.Lock()
	checking := aptCheckRunning
	aptCheckMu.Unlock()
	if checking {
		return true, "CasaNova is refreshing the system package lists"
	}
	switch showUnitProp(aptUpgradeUnit+".service", "ActiveState") {
	case "active", "activating", "deactivating", "reloading":
		return true, "CasaNova is applying system updates"
	}
	return false, ""
}

// maintenanceSystemBusy is the rest of rule 11's "the system is quiet" test:
// the parts held in service/ that pkg/maintenance cannot see for itself.
//
// It errs towards busy on purpose. A copy job that the notify loop has not
// reaped yet, or an upload session whose client is a closed laptop, both make
// this answer "busy" and the run refuses to start. Refusing to delete when the
// system might be working is an inconvenience; deleting the scratch file of a
// copy that is still running is data loss.
func maintenanceSystemBusy() (bool, string) {
	service.OpMu.Lock()
	pending := len(service.OpStrArr)
	service.OpMu.Unlock()
	if pending > 0 {
		return true, "a file copy or move is in progress"
	}
	// The registry, never the age (rule 7): a v2 upload session is live because
	// the service says so, not because its file looks recent.
	for _, s := range service.ScanUploadSessions().Items {
		if s.Verdict == service.UploadSessionActive || s.Verdict == service.UploadSessionPendingFinalize {
			return true, "an upload is still in progress"
		}
	}
	return false, ""
}

// ─── Async state ────────────────────────────────────────────────────────────

// maintenanceScanProgress is design §5's progress object: "Analysing… 3 of 6".
type maintenanceScanProgress struct {
	Category string `json:"category"`
	Index    int    `json:"index"`
	Total    int    `json:"total"`
}

// maintenanceRunProgress is the run's counterpart.
type maintenanceRunProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

var (
	maintScanMu       sync.Mutex
	maintScanRunning  bool
	maintScanState    = "idle" // idle | scanning | done | error
	maintScanErr      string
	maintScanProgress maintenanceScanProgress
)

var (
	maintRunMu       sync.Mutex
	maintRunning     bool
	maintRunState    = "idle" // idle | running | done | error
	maintRunErr      string
	maintRunReason   string // stable code when the failure was a refusal
	maintRunID       string
	maintRunCategory string
	maintRunTotal    int
	maintRunStarted  time.Time
	maintRunResult   *maintenance.RunResult
)

// ─── Wire shapes ────────────────────────────────────────────────────────────

// maintenanceRefusal is the body of a 409/410. Flat, with the machine code
// first: axios collapses a 409 into "Request failed with status code 409", so a
// refusal that does not carry its reason in the body has thrown away the only
// part the user needed.
type maintenanceRefusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type maintenanceScanTotals struct {
	ReclaimableBytes int64                       `json:"reclaimable_bytes"`
	ByClass          map[maintenance.Class]int64 `json:"by_class"`
}

// maintenanceScanResponse is the scan result plus the three fields the wrapper
// owns: the state machine, the progress and the error text. The embedded
// ScanResult is promoted by encoding/json, so `scan_token`, `disk`, `findings`
// and `notices` keep exactly the shape pkg/maintenance defines and cannot drift
// away from it by being re-typed here.
type maintenanceScanResponse struct {
	State    string                   `json:"state"`
	Progress *maintenanceScanProgress `json:"progress"`
	Totals   maintenanceScanTotals    `json:"totals"`
	Error    string                   `json:"error"`
	*maintenance.ScanResult
}

// maintenanceRunResponse is spelled out field by field rather than embedded.
// The run id it reports is the HANDLER's — the one POST /clean answered with —
// because the client uses it to tell "my run finished" from "the previous run
// is still on screen". The engine's own id identifies the manifest on disk and
// is reported separately as manifest_id.
type maintenanceRunResponse struct {
	RunID          string                  `json:"run_id"`
	State          string                  `json:"state"`
	Category       string                  `json:"category"`
	Progress       *maintenanceRunProgress `json:"progress"`
	RemovedItems   int                     `json:"removed_items"`
	RemovedEntries int                     `json:"removed_entries"`
	RemovedBytes   int64                   `json:"removed_bytes"`
	DiskBefore     *maintenance.DiskInfo   `json:"disk_before,omitempty"`
	DiskAfter      *maintenance.DiskInfo   `json:"disk_after,omitempty"`
	DiskDeltaBytes int64                   `json:"disk_delta_bytes"`
	Failed         []maintenance.Failure   `json:"failed"`
	Skipped        []maintenance.Failure   `json:"skipped"`
	ManifestID     string                  `json:"manifest_id"`
	StartedAt      time.Time               `json:"started_at"`
	FinishedAt     time.Time               `json:"finished_at"`
	Reason         string                  `json:"reason"`
	Error          string                  `json:"error"`
}

// maintenanceCleanRequest is the only body this module accepts. Note what it
// cannot express: there is no path field, no glob, no "all categories" flag and
// no recursion switch.
type maintenanceCleanRequest struct {
	ScanToken string   `json:"scan_token"`
	Category  string   `json:"category"`
	ItemIDs   []string `json:"item_ids"`
}

// ─── Scan ───────────────────────────────────────────────────────────────────

// PostMaintenanceScan starts a scan and answers immediately.
// Endpoint: POST /v1/sys/maintenance/scan
func PostMaintenanceScan(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	engine, err := getMaintenanceEngine()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "maintenance module unavailable: "+err.Error())
	}

	maintScanMu.Lock()
	if maintScanRunning {
		maintScanMu.Unlock()
		// No stable code for this one: the client's only correct response is to
		// keep polling GET /scan, and an untranslatable code on screen would be
		// worse than the sentence.
		return ctx.JSON(http.StatusConflict, maintenanceRefusal{Message: "A scan is already in progress."})
	}
	maintScanRunning = true
	maintScanState = "scanning"
	maintScanErr = ""
	// Seed the total from the compiled catalogue — the same list the engine
	// iterates — so a poll that lands before the goroutine is scheduled still
	// gets "0 of 6" instead of a null progress the client has to guess around.
	maintScanProgress = maintenanceScanProgress{Total: len(maintenance.Categories())}
	maintScanMu.Unlock()

	logger.Info("maintenance scan started",
		zap.Int("user", commonmw.UserIDFromContext(ctx)))

	go func() {
		// Declared FIRST so it runs LAST. A panic in a detector must not take
		// the daemon down with it, and by the time this runs the flag-clearing
		// defer below has already fired — so the feature is never left wedged,
		// even on the panic path.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in maintenance scan goroutine", zap.Any("panic", r))
				maintScanMu.Lock()
				maintScanState = "error"
				maintScanErr = "the scan crashed; nothing was deleted"
				maintScanMu.Unlock()
			}
		}()
		runCtx, cancel := context.WithTimeout(context.Background(), maintenanceScanMaxRuntime)
		defer cancel()
		defer func() {
			maintScanMu.Lock()
			maintScanRunning = false
			maintScanMu.Unlock()
		}()

		_, serr := engine.Scan(runCtx, func(category string, index, total int) {
			maintScanMu.Lock()
			// index is 0-based and reported before the category runs; the screen
			// says "3 of 6" about the one being worked on.
			maintScanProgress = maintenanceScanProgress{Category: category, Index: index + 1, Total: total}
			maintScanMu.Unlock()
		})

		maintScanMu.Lock()
		if serr != nil {
			maintScanState = "error"
			maintScanErr = maintenanceScanErrText(runCtx, serr)
			logger.Error("maintenance scan failed", zap.Error(serr))
		} else {
			maintScanState = "done"
			maintScanErr = ""
			maintScanProgress.Category = ""
			maintScanProgress.Index = maintScanProgress.Total
		}
		maintScanMu.Unlock()
	}()

	return ctx.JSON(http.StatusOK, map[string]string{"state": "scanning"})
}

// GetMaintenanceScan returns the state of the scan and, once it has finished,
// its findings.
// Endpoint: GET /v1/sys/maintenance/scan
func GetMaintenanceScan(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	engine, err := getMaintenanceEngine()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "maintenance module unavailable: "+err.Error())
	}

	maintScanMu.Lock()
	state, scanErr, prog := maintScanState, maintScanErr, maintScanProgress
	maintScanMu.Unlock()

	// Findings are served ONLY in the done state. While a scan is running the
	// previous one's numbers are being recomputed, and a screen that shows them
	// under a "scanning" label is inviting the user to tick boxes against a
	// picture the server is in the middle of replacing. The empty result still
	// carries the read-only panel, which is static text and always true.
	res := &maintenance.ScanResult{
		ByClass:  map[maintenance.Class]int64{},
		Findings: []maintenance.Finding{},
		Notices:  maintenance.ReportOnlyNotices(),
	}
	if state == "done" {
		if last := engine.LastScan(); last != nil {
			res = last.ForAPI(maintenanceMaxItemsPerFinding)
		}
	}

	out := maintenanceScanResponse{
		State: state,
		Totals: maintenanceScanTotals{
			ReclaimableBytes: res.ReclaimableBytes,
			ByClass:          res.ByClass,
		},
		Error:      scanErr,
		ScanResult: res,
	}
	if prog.Total > 0 {
		p := prog
		out.Progress = &p
	}
	return ctx.JSON(http.StatusOK, out)
}

// ─── Clean ──────────────────────────────────────────────────────────────────

// PostMaintenanceClean executes ONE category. There is no "clean everything"
// endpoint and there deliberately never will be: what is irreversible is opt-in
// per category, with its own confirmation (design rule 13).
// Endpoint: POST /v1/sys/maintenance/clean  (also behind strictJWT)
func PostMaintenanceClean(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	engine, err := getMaintenanceEngine()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "maintenance module unavailable: "+err.Error())
	}

	var req maintenanceCleanRequest
	if err := ctx.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	// A key from the compiled catalogue or nothing. Anything else is a 400 and
	// is never interpreted as a path.
	if maintenance.Lookup(req.Category) == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "unknown maintenance category")
	}
	if len(req.ScanToken) > maintenanceMaxTokenLen {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid scan token")
	}
	ids, err := maintenanceCleanIDs(req.ItemIDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	// Preflight, read-only, against the plan this process is holding. The
	// engine re-decides all of it authoritatively inside Clean — this exists
	// only so the two refusals the user can actually act on (re-analyse, or
	// "this category is blocked and here is why") arrive in the POST response
	// instead of surfacing three seconds later in a poll.
	scan := engine.LastScan()
	if scan == nil {
		return ctx.JSON(http.StatusConflict, maintenanceRefusal{
			Reason:  maintenance.BlockStaleScan,
			Message: "Nothing has been analysed yet. Run an analysis first.",
		})
	}
	if req.ScanToken == "" || req.ScanToken != scan.Token {
		return ctx.JSON(http.StatusGone, maintenanceRefusal{
			Reason:  maintenance.BlockStaleScan,
			Message: "This analysis has been replaced. Analyse again before cleaning.",
		})
	}
	finding := scan.FindingFor(req.Category)
	if finding == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "category is not part of the current analysis")
	}
	if finding.Blocked {
		return ctx.JSON(http.StatusConflict, maintenanceRefusal{
			Reason:  finding.BlockedCode,
			Message: finding.BlockedReason,
		})
	}

	maintRunMu.Lock()
	if maintRunning {
		maintRunMu.Unlock()
		return ctx.JSON(http.StatusConflict, maintenanceRefusal{
			Reason:  maintenance.BlockRunInProgress,
			Message: "A maintenance run is already in progress.",
		})
	}
	runID := maintenanceNewRunID()
	maintRunning = true
	maintRunState = "running"
	maintRunErr = ""
	maintRunReason = ""
	maintRunID = runID
	maintRunCategory = req.Category
	maintRunTotal = len(ids)
	maintRunStarted = time.Now()
	maintRunResult = nil
	maintRunMu.Unlock()

	// Audited BEFORE the work, not after: this is a root-level deleter, and the
	// record of who asked for what has to survive the process dying mid-run.
	actor := strconv.Itoa(commonmw.UserIDFromContext(ctx))
	service.AuditFileOp("maintenance.clean", actor, req.Category,
		fmt.Sprintf("maintenance run %s requested for %d item(s)", runID, len(ids)))
	logger.Info("maintenance run started",
		zap.String("run", runID),
		zap.String("category", req.Category),
		zap.Int("items", len(ids)),
		zap.String("actor", actor))

	token := req.ScanToken
	category := req.Category
	go func() {
		// Declared FIRST so it runs LAST — same reasoning as the scan, with
		// higher stakes: a panic that left maintRunning set would make the
		// feature answer 409 forever, and the only way out would be a restart.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in maintenance run goroutine",
					zap.String("run", runID), zap.Any("panic", r))
				maintRunMu.Lock()
				maintRunState = "error"
				maintRunErr = "the run crashed"
				maintRunMu.Unlock()
			}
		}()
		runCtx, cancel := context.WithTimeout(context.Background(), maintenanceRunMaxRuntime)
		defer cancel()
		defer func() {
			maintRunMu.Lock()
			maintRunning = false
			maintRunMu.Unlock()
		}()

		res, cerr := engine.Clean(runCtx, token, category, ids)

		maintRunMu.Lock()
		// Clean can return BOTH a result and an error when it aborted partway:
		// the result describes what had already happened, and dropping it would
		// hide real deletions from the screen and from support.
		if res != nil {
			maintRunResult = res
		}
		if cerr != nil {
			maintRunState = "error"
			maintRunReason, maintRunErr = maintenanceRunErrText(cerr)
		} else {
			maintRunState = "done"
			maintRunReason = ""
			maintRunErr = ""
		}
		maintRunMu.Unlock()

		maintenanceAnnounce(category, res, cerr)
	}()

	return ctx.JSON(http.StatusOK, map[string]string{
		"run_id": runID,
		"state":  "running",
	})
}

// GetMaintenanceClean returns the run in progress, or the last one that
// finished.
// Endpoint: GET /v1/sys/maintenance/clean
func GetMaintenanceClean(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	maintRunMu.Lock()
	out := maintenanceRunResponse{
		RunID:     maintRunID,
		State:     maintRunState,
		Category:  maintRunCategory,
		StartedAt: maintRunStarted,
		Reason:    maintRunReason,
		Error:     maintRunErr,
		Failed:    []maintenance.Failure{},
		Skipped:   []maintenance.Failure{},
	}
	total := maintRunTotal
	res := maintRunResult
	if res != nil {
		out.RemovedItems = res.RemovedItems
		out.RemovedEntries = res.RemovedEntries
		out.RemovedBytes = res.RemovedBytes
		out.DiskDeltaBytes = res.DiskDeltaBytes
		out.ManifestID = res.ManifestID
		out.FinishedAt = res.FinishedAt
		before, after := res.DiskBefore, res.DiskAfter
		out.DiskBefore, out.DiskAfter = &before, &after
		if len(res.Failed) > 0 {
			out.Failed = res.Failed
		}
		if len(res.Skipped) > 0 {
			out.Skipped = res.Skipped
		}
	}
	maintRunMu.Unlock()

	if total > 0 {
		// `done` only moves when the engine hands back a result: it emits no
		// per-item callback, and a progress number invented on this side would
		// be a number the server cannot back. Total is what the user approved.
		out.Progress = &maintenanceRunProgress{Done: out.RemovedItems, Total: total}
	}
	return ctx.JSON(http.StatusOK, out)
}

// ─── Manifests ──────────────────────────────────────────────────────────────

// GetMaintenanceManifests lists past runs, newest first.
// Endpoint: GET /v1/sys/maintenance/manifest
func GetMaintenanceManifests(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	engine, err := getMaintenanceEngine()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "maintenance module unavailable: "+err.Error())
	}
	list, err := engine.Manifests()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "could not read maintenance manifests")
	}
	if list == nil {
		list = []maintenance.ManifestSummary{}
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{"manifests": list})
}

// GetMaintenanceManifest returns one past run in full: every planned entry,
// whether it was removed, and why it was on the list. This is the artefact
// support reads months later when someone asks what that button deleted.
// Endpoint: GET /v1/sys/maintenance/manifest/:id
func GetMaintenanceManifest(ctx echo.Context) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !maintenanceGateOpen(ctx) {
		return nil // the gate answered 403 by writing it; see maintenanceGateOpen
	}

	engine, err := getMaintenanceEngine()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "maintenance module unavailable: "+err.Error())
	}
	// The id is validated as a bare filename by pkg/maintenance before it is
	// joined to anything — the same reasoning as the opaque item ids. Both
	// "malformed" and "absent" answer 404 so the endpoint cannot be used to
	// probe which manifest ids exist.
	m, err := engine.Manifest(ctx.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "manifest not found")
	}
	return ctx.JSON(http.StatusOK, m)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// maintenanceCleanIDs validates and de-duplicates the ticked item ids.
//
// De-duplication is not cosmetic: a repeated id would be planned twice, count
// twice against the run's hard limits, and the second unlink would come back
// ENOENT and be reported to the user as a failure on a file we deleted
// successfully.
func maintenanceCleanIDs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("no items selected")
	}
	if len(in) > maintenanceMaxItemIDs {
		return nil, errors.New("too many items in one request")
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if !isMaintenanceItemID(id) {
			return nil, errors.New("invalid item id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// isMaintenanceItemID checks the shape the scan mints: lowercase hex, fixed
// width. Anything else never reaches the plan lookup.
func isMaintenanceItemID(s string) bool {
	if len(s) != maintenanceItemIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maintenanceNewRunID mints the id the client polls on. Timestamped so it reads
// like the manifest ids next to it, with a random tail so two runs can never
// collide even if a future change allows them within the same second.
func maintenanceNewRunID() string {
	stamp := time.Now().UTC().Format("20060102-150405")
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return stamp
	}
	return stamp + "-" + hex.EncodeToString(b[:])
}

// maintenanceScanErrText turns a scan failure into a sentence.
func maintenanceScanErrText(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "the scan took too long and was stopped; nothing was deleted"
	}
	if errors.Is(err, maintenance.ErrScanInProgress) {
		return "a scan is already in progress"
	}
	return err.Error()
}

// maintenanceRunErrText maps a run failure onto (stable code, sentence).
//
// The code matters even though this arrives through a poll rather than an HTTP
// status: a refusal the client cannot name is the one it renders as a shrug.
func maintenanceRunErrText(err error) (string, string) {
	var pe *maintenance.PreflightError
	if errors.As(err, &pe) {
		return pe.Code, pe.Reason
	}
	switch {
	case errors.Is(err, maintenance.ErrStaleScan), errors.Is(err, maintenance.ErrNoScan):
		return maintenance.BlockStaleScan, "the analysis expired before the run started; analyse again"
	case errors.Is(err, maintenance.ErrUnknownCategory):
		return "", "unknown maintenance category"
	case errors.Is(err, maintenance.ErrUnknownItem):
		return maintenance.BlockStaleScan, "the selection no longer matches the analysis; analyse again"
	case errors.Is(err, maintenance.ErrPlanTooLarge):
		return "", "the plan grew past the hard limit for a single run and was refused"
	case errors.Is(err, context.DeadlineExceeded):
		return "", "the run took too long and was stopped"
	}
	return "", err.Error()
}

// maintenanceAnnounce records the outcome where the user will find it later:
// the notification centre and the audit log. Only a run that actually removed
// something is worth a notification — a run that legitimately found nothing to
// do is noise.
func maintenanceAnnounce(category string, res *maintenance.RunResult, runErr error) {
	ns := service.Notifications()
	if runErr != nil {
		logger.Error("maintenance run failed", zap.String("category", category), zap.Error(runErr))
		if ns != nil {
			_ = ns.Err(notifications.CategoryStorage, "Maintenance failed", runErr.Error())
		}
		return
	}
	if res == nil || res.RemovedEntries == 0 {
		return
	}
	service.AuditFileOp("maintenance.cleaned", "system", category,
		fmt.Sprintf("removed %d entr%s, %s freed on disk",
			res.RemovedEntries, maintenancePluralY(res.RemovedEntries),
			maintenanceHumanBytes(res.DiskDeltaBytes)))
	if ns == nil {
		return
	}
	// The headline is the statfs delta, not the sum of the file sizes: that is
	// the number the user can verify with df, and the two differ whenever an
	// inode was still referenced by something else.
	_ = ns.Info(notifications.CategoryStorage, "Maintenance completed",
		fmt.Sprintf("%d entr%s removed, %s freed on disk.",
			res.RemovedEntries, maintenancePluralY(res.RemovedEntries),
			maintenanceHumanBytes(res.DiskDeltaBytes)))
}

func maintenancePluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// maintenanceHumanBytes formats a size for notification text only. A negative
// delta is possible and is shown as 0 B rather than as a negative number:
// something else on the box wrote while we deleted, which is not a failure but
// is also not a gain we may claim.
func maintenanceHumanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
