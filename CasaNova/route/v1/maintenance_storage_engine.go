package v1

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagemaint"
)

// THE ADAPTER THAT MAKES THE SUBSYSTEM LIVE.
//
// Everything under /v1/maintenance/storage was inert: the HTTP layer defined
// StorageMaintenanceEngine and a setter, no implementation was ever installed,
// and every mutating endpoint answered 503 ENGINE_UNAVAILABLE. The formatter
// and the sanitizer were complete, tested and mutation-verified — and
// unreachable. Recovery guarantees documented in the API were therefore false:
// RecoverInterruptedJobs never ran, so an interrupted format stayed
// undiagnosed, and EnsureSanitizeSubsystem never ran, so sanitize health
// reported "unprovisioned" forever.
//
// This is the project's signature defect — a correct mechanism no production
// caller reaches — landing inside the subsystem built to be destructive. The
// wiring census exists because of exactly this, and it is why
// InstallStorageMaintenanceEngine is called from main and asserted by a test.
//
// The adapter translates and nothing more. It does not re-decide safety: the
// HTTP layer has already resolved the target and proven eligibility, the
// engines re-derive everything anyway, and a third opinion here would be a
// place for the three to drift.

type storageEngineAdapter struct {
	formatter *storagemaint.Formatter

	// sanitizePlans maps the API's plan id to the engine's job id. The engine
	// seals its own plans; this only remembers which one a given API id refers
	// to, and forgets it on execution so the API id is single-use too.
	mu            sync.Mutex
	sanitizePlans map[string]storageSanitizeHandle
}

type storageSanitizeHandle struct {
	jobID     string
	plan      StoragePlan
	expiresAt time.Time
}

// sanitizePlanTTL bounds how long an API-side plan id stays meaningful. The
// engine enforces its own TTL on the sealed plan; this one only keeps the
// handle table from growing.
const storageSanitizePlanTTL = 10 * time.Minute

// InstallStorageMaintenanceEngine builds the adapter and installs it, then
// brings up the pieces that must exist before any request arrives.
//
// Returns an error rather than panicking: a service that cannot offer storage
// maintenance must still start, because its diagnostics are how an operator
// finds out why. Every mutating endpoint keeps answering 503 until this
// succeeds, which is the correct fail-closed posture.
func InstallStorageMaintenanceEngine(ctx context.Context) error {
	// Capability discovery at startup, not at click time: a user who confirms
	// a destructive operation and only then learns mkfs.exfat is missing has
	// been walked to the edge of losing data for nothing.
	storagemaint.DiscoverCapabilities(ctx)

	f := storagemaint.NewFormatter()

	// INSTALL FIRST, then diagnose.
	//
	// The order used to be the other way round, and it was the shape of the
	// outage this file was written after: a recovery error returned before
	// SetStorageMaintenanceEngine, so a failure to DIAGNOSE old jobs left every
	// format and sanitize endpoint answering 503 permanently. The same
	// inversion — a reporting step disabling the function it reports on — took
	// the whole file API down earlier today.
	//
	// The adapter does not depend on recovery having run. Recovery makes an
	// interrupted job diagnosable; the engine works either way, and a caller is
	// better served by a working endpoint that says "one old job could not be
	// diagnosed" than by no endpoint at all.
	SetStorageMaintenanceEngine(&storageEngineAdapter{
		formatter: f, sanitizePlans: map[string]storageSanitizeHandle{},
	})

	var problems []string

	// Diagnose anything that was running when the service last stopped. Until
	// this runs, an interrupted format is a disk in an unknown state that
	// nothing has looked at.
	if _, err := f.RecoverInterruptedJobs(ctx); err != nil {
		problems = append(problems, "recover interrupted format jobs: "+err.Error())
	}

	// Provision and self-validate the sanitizer, including recovering any
	// artifacts a crash left behind — the difference between "the disk is
	// mysteriously full" and "CasaNova cleaned up after itself".
	if h := storagemaint.EnsureSanitizeSubsystem(ctx); h.Status != "healthy" {
		problems = append(problems, fmt.Sprintf("sanitize subsystem is %s: %s", h.Status, h.Detail))
	}

	if len(problems) > 0 {
		// Reported, not fatal. Health names each one; the endpoints stay up.
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (a *storageEngineAdapter) principal(p StoragePrincipal) storageauth.Principal {
	return storageauth.Principal{
		ID:    strconv.Itoa(p.UserID),
		Role:  p.Role,
		Admin: p.Role == "admin",
	}
}

func (a *storageEngineAdapter) PrepareFormat(ctx context.Context, req StorageFormatRequest) (StoragePlan, error) {
	plan, err := a.formatter.Prepare(ctx, a.principal(req.Principal), storagemaint.PrepareFormatRequest{
		Identity:    req.Target.Identity,
		Fingerprint: req.Target.Fingerprint,
		Filesystem:  req.Filesystem,
		Label:       req.Label,
	})
	if err != nil {
		return StoragePlan{}, err
	}

	steps := make([]string, 0, len(plan.Actions))
	for _, act := range plan.Actions {
		steps = append(steps, act.Description)
	}
	return StoragePlan{
		ID:          plan.OperationID,
		Kind:        StorageOpFormat,
		Fingerprint: req.Target.Fingerprint,
		Identity:    plan.Identity,
		DeviceName:  plan.DeviceName,
		SizeBytes:   plan.SizeBytes,
		Filesystem:  plan.Filesystem,
		Label:       plan.Label,
		Steps:       steps,
		CreatedAt:   time.Now(),
		ExpiresAt:   plan.ExpiresAt,
	}, nil
}

func (a *storageEngineAdapter) PrepareSanitize(ctx context.Context, req StorageSanitizeRequest) (StoragePlan, error) {
	if len(req.Target.Mountpoints) == 0 {
		// The HTTP layer already refuses this; repeating the check here keeps
		// the adapter from silently passing an empty root to the engine if that
		// ever changes.
		return StoragePlan{}, fmt.Errorf("sanitization works through a mounted filesystem and this device has no mountpoint")
	}

	plan, err := storagemaint.PlanSanitize(ctx, storagemaint.SanitizeRequest{
		Root:        req.Target.Mountpoints[0],
		Method:      storagemaint.SanitizeMethod(req.Method),
		PrincipalID: req.Principal.ID(),
	})
	if err != nil {
		return StoragePlan{}, err
	}

	steps := make([]string, 0, len(plan.Passes))
	for i, pass := range plan.Passes {
		steps = append(steps, fmt.Sprintf("pass %d/%d: %s", i+1, len(plan.Passes), pass.Label))
	}
	out := StoragePlan{
		ID:          plan.JobID,
		Kind:        StorageOpSanitize,
		Fingerprint: req.Target.Fingerprint,
		Identity:    req.Target.Identity,
		DeviceName:  plan.DeviceName,
		SizeBytes:   req.Target.SizeBytes,
		Method:      req.Method,
		Steps:       steps,
		Warnings:    plan.Warnings,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(storageSanitizePlanTTL),
	}

	a.mu.Lock()
	for id, h := range a.sanitizePlans {
		if time.Now().After(h.expiresAt) {
			delete(a.sanitizePlans, id)
		}
	}
	a.sanitizePlans[out.ID] = storageSanitizeHandle{
		jobID: plan.JobID, plan: out, expiresAt: out.ExpiresAt,
	}
	a.mu.Unlock()
	return out, nil
}

func (a *storageEngineAdapter) Plan(ctx context.Context, planID string) (StoragePlan, bool) {
	// Only sanitize handles are looked up here. The formatter deliberately does
	// not expose a plan lookup: its plans are single-use tokens consumed by
	// Execute, and handing one back for inspection would create a second
	// reference to an authorization that is supposed to exist exactly once.
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.sanitizePlans[planID]
	if !ok || time.Now().After(h.expiresAt) {
		return StoragePlan{}, false
	}
	return h.plan, true
}

func (a *storageEngineAdapter) Execute(ctx context.Context, planID string, kind StorageOpKind, p StoragePrincipal) (StorageJob, error) {
	switch kind {
	case StorageOpFormat:
		rec, err := a.formatter.Execute(ctx, a.principal(p), planID)
		if err != nil {
			return StorageJob{}, err
		}
		return storageJobFromRecord(rec), nil

	case StorageOpSanitize:
		// Burn the API handle under the lock, so two concurrent executions
		// cannot both reach the engine. The engine's own plan is single-use
		// too; this is the outer half of the same property.
		a.mu.Lock()
		h, ok := a.sanitizePlans[planID]
		if ok {
			delete(a.sanitizePlans, planID)
		}
		a.mu.Unlock()
		if !ok {
			return StorageJob{}, fmt.Errorf("no prepared sanitization with id %q; a plan is single-use", planID)
		}
		if time.Now().After(h.expiresAt) {
			return StorageJob{}, fmt.Errorf("the prepared sanitization expired; prepare it again")
		}

		res, err := storagemaint.RunSanitize(ctx, h.jobID)
		if err != nil {
			return StorageJob{}, err
		}
		return StorageJob{
			ID: h.jobID, Kind: StorageOpSanitize, Fingerprint: h.plan.Fingerprint,
			DeviceName: h.plan.DeviceName, State: "finished", Percent: 100,
			StartedAt: res.StartedAt, FinishedAt: res.FinishedAt,
		}, nil

	default:
		return StorageJob{}, fmt.Errorf("unknown operation kind %q", kind)
	}
}

func (a *storageEngineAdapter) Jobs(ctx context.Context) ([]StorageJob, error) {
	recs, err := a.formatter.ListJobs()
	if err != nil {
		return nil, err
	}
	out := make([]StorageJob, 0, len(recs))
	for _, r := range recs {
		out = append(out, storageJobFromRecord(r))
	}
	return out, nil
}

func (a *storageEngineAdapter) Cancel(ctx context.Context, jobID string, p StoragePrincipal) error {
	_, _, err := a.formatter.Cancel(jobID)
	return err
}

func storageJobFromRecord(r storagemaint.JobRecord) StorageJob {
	state := "running"
	switch {
	case r.Terminal:
		state = r.Result
	case r.Interrupted:
		// Not running and not finished: work that CAN be continued.
		state = "interrupted"
	}
	return StorageJob{
		ID: r.JobID, Kind: StorageOpFormat, Fingerprint: r.Fingerprint,
		DeviceName: r.Identity.Model, State: state, Stage: string(r.Stage),
		StartedAt: r.StartedAt, FinishedAt: r.UpdatedAt,
		Error:       r.ErrorDetail,
		Interrupted: r.Interrupted || (r.Terminal && r.Result == "interrupted"),
	}
}
