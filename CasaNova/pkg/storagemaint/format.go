// External-disk formatter.
//
// # WHY THIS IS NOT A WRAPPER AROUND mkfs
//
// `POST /format {"device": "/dev/sdb"}` is an unsafe API no matter how careful
// the implementation behind it is. /dev/sdb is a slot, not a disk: between the
// moment a user reads "SanDisk 64 GB" on screen and the moment the request
// lands, that name can belong to a different physical device. So formatting is
// a TWO-STAGE protocol. Prepare re-discovers the hardware and returns a plan
// that names the exact physical device, everything currently on it and every
// destructive action about to happen. Execute consumes a single-use,
// short-lived token bound to that exact device AND that exact plan, and
// re-proves every safety property from scratch immediately before the first
// syscall that destroys anything.
//
// # WHY THERE IS A PERSISTED STATE MACHINE
//
// Formatting is not atomic. Between "partition table written" and "filesystem
// created" there is a window in which a power cut leaves a disk that is neither
// what it was nor what it was going to be. Pretending otherwise — resuming as
// if nothing happened, or reporting the last known good state — is how a user
// is told their disk is fine while it is not. Every stage is fsynced BEFORE the
// step that leaves it, together with the step about to run, so an interrupted
// job is diagnosed precisely ("interrupted after PARTITIONED, while creating
// the filesystem") instead of guessed at.
//
// # WHAT THIS FILE DELIBERATELY DOES NOT CLAIM
//
// Formatting is not erasure. Creating a new partition table and filesystem
// makes the old contents unreachable through the filesystem; it does not
// overwrite them and it does not destroy data held in a device's spare blocks
// or FTL. Nothing here says "wiped", "erased" or "unrecoverable", because the
// mechanism does not provide it.
package storagemaint

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// ---------------------------------------------------------------- vocabulary

// PartitionMode is the partition table to create.
type PartitionMode string

const (
	// PartitionGPT is the default. It is the only table that addresses a modern
	// drive correctly.
	PartitionGPT PartitionMode = "gpt"
	// PartitionMBR exists for compatibility with appliances and cameras that
	// cannot read GPT. It is a deliberate downgrade, never a fallback chosen
	// silently.
	PartitionMBR PartitionMode = "mbr"
)

// FormatStage is a durable point in the format lifecycle. The value persisted
// on disk is the LAST COMPLETED stage; PendingStep records what was about to
// run, which is what makes an interruption diagnosable rather than ambiguous.
type FormatStage string

const (
	StageDiscovered        FormatStage = "DISCOVERED"
	StageValidated         FormatStage = "VALIDATED"
	StageQuiesced          FormatStage = "QUIESCED"
	StageSignaturesCleared FormatStage = "SIGNATURES_CLEARED"
	StagePartitioned       FormatStage = "PARTITIONED"
	StagePartitionVisible  FormatStage = "PARTITION_VISIBLE"
	StageFilesystemCreated FormatStage = "FILESYSTEM_CREATED"
	StageVerified          FormatStage = "VERIFIED"
	StageIntegrated        FormatStage = "INTEGRATED"
	StageComplete          FormatStage = "COMPLETE"

	// StageFailed, StageCancelled and StageInterrupted are terminal outcomes,
	// not points on the ordinary path. StageUnknown is what a job record whose
	// integrity cannot be proven collapses to — never COMPLETE.
	StageFailed      FormatStage = "FAILED"
	StageCancelled   FormatStage = "CANCELLED"
	StageInterrupted FormatStage = "INTERRUPTED"
	StageUnknown     FormatStage = "UNKNOWN"
)

// stageOrder ranks the ordinary path so resume can refuse to go backwards.
var stageOrder = map[FormatStage]int{
	StageDiscovered:        0,
	StageValidated:         1,
	StageQuiesced:          2,
	StageSignaturesCleared: 3,
	StagePartitioned:       4,
	StagePartitionVisible:  5,
	StageFilesystemCreated: 6,
	StageVerified:          7,
	StageIntegrated:        8,
	StageComplete:          9,
}

// CancelState is what the UI may truthfully show about cancellation.
//
// A universally-safe Cancel button would be a lie: once the partition table is
// written the disk is already different, and once mkfs has started interrupting
// it leaves a half-built filesystem. These states exist so the UI can say which
// of those is true right now.
type CancelState string

const (
	// CancelStateCancellable: stopping now leaves the device as it was found.
	CancelStateCancellable CancelState = "cancellable"
	// CancelStateRequested: a cancellation was accepted and will be honoured at
	// the next safe boundary.
	CancelStateRequested CancelState = "cancellation_requested"
	// CancelStateNonCancellablePhase: a step is running that cannot be
	// interrupted without leaving the device in a worse state than finishing.
	CancelStateNonCancellablePhase CancelState = "non_cancellable_phase"
	// CancelStateRecovering: a cancellation was honoured after the device was
	// already modified, and the job is settling into a describable state.
	CancelStateRecovering CancelState = "recovering"
	// CancelStateFinished: the job is terminal; there is nothing to cancel.
	CancelStateFinished CancelState = "finished"
)

// StageCancelPolicy tells the UI, per stage, what pressing Cancel would mean.
type StageCancelPolicy struct {
	Stage FormatStage `json:"stage"`
	// Cancellable reports whether a cancellation is accepted while this stage
	// is the last completed one.
	Cancellable bool `json:"cancellable"`
	// LeavesDeviceModified is the honest part: true means the disk has already
	// changed and cancelling does not undo it.
	LeavesDeviceModified bool   `json:"leaves_device_modified"`
	Note                 string `json:"note"`
}

// cancelPolicy is the per-stage truth about cancellation, exposed in the plan
// so a UI cannot render a Cancel button that promises more than the code does.
var cancelPolicy = []StageCancelPolicy{
	{StageDiscovered, true, false, "nothing has been touched yet"},
	{StageValidated, true, false, "nothing has been touched yet"},
	{StageQuiesced, true, false, "the volume was unmounted and stays unmounted; no data was changed"},
	{StageSignaturesCleared, true, true, "filesystem signatures were already removed; the previous filesystem is no longer detectable"},
	{StagePartitioned, true, true, "a new partition table was already written; the previous layout is gone"},
	{StagePartitionVisible, true, true, "a new partition table was already written; the previous layout is gone"},
	{StageFilesystemCreated, true, true, "the new filesystem exists but has not been validated; it is not safe to use yet"},
	{StageVerified, true, true, "the new filesystem is valid but not yet integrated"},
	{StageIntegrated, false, true, "the operation is finishing; there is nothing left to stop"},
	{StageComplete, false, true, "finished"},
}

// nonCancellableSteps are the steps that run to completion once started.
// Interrupting sfdisk mid-write or mkfs mid-write produces something strictly
// worse than letting it finish and then reporting the truth.
var nonCancellableSteps = map[stepKind]bool{
	stepPartition: true,
	stepMkfs:      true,
}

// ---------------------------------------------------------------- errors

// FormatErrorCode is the machine-readable refusal. A UI maps this to an
// explanation; nothing parses English.
type FormatErrorCode string

const (
	ErrCodeNotAuthorized      FormatErrorCode = "NOT_AUTHORIZED"
	ErrCodeIdentityWeak       FormatErrorCode = "IDENTITY_TOO_WEAK"
	ErrCodeDeviceNotFound     FormatErrorCode = "DEVICE_NOT_FOUND"
	ErrCodeIdentityMismatch   FormatErrorCode = "DEVICE_IDENTITY_MISMATCH"
	ErrCodeDeviceProtected    FormatErrorCode = "DEVICE_PROTECTED"
	ErrCodeParentProtected    FormatErrorCode = "PARENT_DEVICE_PROTECTED"
	ErrCodeNotEligible        FormatErrorCode = "DEVICE_NOT_ELIGIBLE"
	ErrCodeDeviceBusy         FormatErrorCode = "DEVICE_BUSY"
	ErrCodeDeviceLocked       FormatErrorCode = "DEVICE_LOCKED"
	ErrCodeDiscoveryIncmplete FormatErrorCode = "DISCOVERY_INCOMPLETE"
	ErrCodeCapability         FormatErrorCode = "CAPABILITY_UNAVAILABLE"
	ErrCodeInvalidRequest     FormatErrorCode = "INVALID_REQUEST"
	ErrCodeTokenUnknown       FormatErrorCode = "OPERATION_UNKNOWN"
	ErrCodeTokenExpired       FormatErrorCode = "OPERATION_EXPIRED"
	ErrCodeTokenConsumed      FormatErrorCode = "OPERATION_ALREADY_USED"
	ErrCodePlanChanged        FormatErrorCode = "PLAN_NO_LONGER_VALID"
	ErrCodeToolFailed         FormatErrorCode = "HELPER_FAILED"
	ErrCodeVerificationFailed FormatErrorCode = "POST_FORMAT_VALIDATION_FAILED"
	ErrCodeCancelled          FormatErrorCode = "CANCELLED"
	ErrCodeJobStateCorrupt    FormatErrorCode = "JOB_STATE_CORRUPT"
	ErrCodeAuditUnavailable   FormatErrorCode = "AUDIT_UNAVAILABLE"
	ErrCodeInterrupted        FormatErrorCode = "INTERRUPTED"
)

// FormatError is every refusal and every failure this file produces.
type FormatError struct {
	Code   FormatErrorCode `json:"code"`
	Detail string          `json:"detail"`
	Stage  FormatStage     `json:"stage,omitempty"`
	// Blockers names the concrete things standing in the way — mountpoints,
	// holders, protection reasons — because "device busy" without saying what
	// holds it is an instruction to reboot and hope.
	Blockers []string `json:"blockers,omitempty"`
	// DeviceModified is the question a user actually has after a failure.
	DeviceModified bool `json:"device_modified"`
}

func (e *FormatError) Error() string {
	if len(e.Blockers) > 0 {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Detail, strings.Join(e.Blockers, "; "))
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func fail(code FormatErrorCode, format string, args ...interface{}) *FormatError {
	return &FormatError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// CodeOf extracts the machine-readable code from an error produced here.
func CodeOf(err error) (FormatErrorCode, bool) {
	var fe *FormatError
	if errors.As(err, &fe) {
		return fe.Code, true
	}
	return "", false
}

// ---------------------------------------------------------------- the plan

// PartitionView is one existing partition, as found at prepare time.
type PartitionView struct {
	Name        string   `json:"name"`
	SizeBytes   uint64   `json:"size_bytes"`
	Filesystem  string   `json:"filesystem,omitempty"`
	Label       string   `json:"label,omitempty"`
	UUID        string   `json:"uuid,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Holders     []string `json:"holders,omitempty"`
}

// DestructiveAction is one thing the plan will do, in the order it happens.
// This is the text a confirmation dialog is built from.
type DestructiveAction struct {
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Description string `json:"description"`
	// Destroys is false for steps that only change reachability (unmounting)
	// and true for steps that change the contents of the device.
	Destroys bool `json:"destroys"`
}

// stepKind is the internal step vocabulary. Steps map onto stages.
type stepKind string

const (
	stepQuiesce   stepKind = "unmount"
	stepWipe      stepKind = "wipe_signatures"
	stepPartition stepKind = "write_partition_table"
	stepSettle    stepKind = "await_partition"
	stepMkfs      stepKind = "create_filesystem"
	stepVerify    stepKind = "verify"
	stepIntegrate stepKind = "integrate"
)

// stepStage is the stage recorded once a step completes.
var stepStage = map[stepKind]FormatStage{
	stepQuiesce:   StageQuiesced,
	stepWipe:      StageSignaturesCleared,
	stepPartition: StagePartitioned,
	stepSettle:    StagePartitionVisible,
	stepMkfs:      StageFilesystemCreated,
	stepVerify:    StageVerified,
	stepIntegrate: StageIntegrated,
}

// FormatPlan is what Prepare returns and what Execute is bound to. It is
// deliberately verbose: the user is about to authorise the destruction of
// everything on a physical device and must be able to recognise that device
// from what is on the screen.
type FormatPlan struct {
	OperationID string    `json:"operation_id"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`

	// Identity is the binding. Execute re-derives it and refuses on any
	// difference.
	Identity    blockdev.DeviceIdentity `json:"identity"`
	Fingerprint string                  `json:"device_fingerprint"`
	// LockFingerprint is the PHYSICAL device's fingerprint, which for a
	// partition target is its parent disk's. Two jobs on one disk must collide
	// even when one targets the disk and the other a partition on it.
	LockFingerprint string `json:"lock_fingerprint"`

	// DeviceName is for display only. Nothing binds to it.
	DeviceName  string             `json:"device_name"`
	DevicePath  string             `json:"device_path"`
	IsPartition bool               `json:"is_partition"`
	Model       string             `json:"model,omitempty"`
	Vendor      string             `json:"vendor,omitempty"`
	Serial      string             `json:"serial,omitempty"`
	WWN         string             `json:"wwn,omitempty"`
	Transport   blockdev.Transport `json:"transport"`
	SizeBytes   uint64             `json:"size_bytes"`
	SizeHuman   string             `json:"size_human"`

	CurrentTable      string          `json:"current_table,omitempty"`
	CurrentFilesystem string          `json:"current_filesystem,omitempty"`
	CurrentLabel      string          `json:"current_label,omitempty"`
	CurrentPartitions []PartitionView `json:"current_partitions"`

	Filesystem    Filesystem    `json:"filesystem"`
	Label         string        `json:"label,omitempty"`
	PartitionMode PartitionMode `json:"partition_mode"`

	Actions []DestructiveAction `json:"actions"`
	steps   []stepKind

	// CancelPolicy is the per-stage truth about what Cancel would do.
	CancelPolicy []StageCancelPolicy `json:"cancel_policy"`

	// PlanHash covers the request and the plan. TopologyHash covers what the
	// device looked like. Execute recomputes both; either changing aborts.
	PlanHash     string `json:"plan_hash"`
	TopologyHash string `json:"topology_hash"`

	// Guarantees states plainly what this operation does NOT provide, so no
	// caller can present it as erasure.
	Guarantees string `json:"guarantees"`
}

const guaranteeText = "Formatting replaces the partition table and creates an empty filesystem. " +
	"It makes previous contents unreachable through the filesystem. It does NOT overwrite " +
	"the previous data and it is NOT a secure erase: data may remain recoverable with " +
	"specialised tools or in the drive's spare blocks."

// ---------------------------------------------------------------- job record

// JobRecord is the durable state of one format job.
type JobRecord struct {
	JobID       string    `json:"job_id"`
	OperationID string    `json:"operation_id"`
	PrincipalID string    `json:"principal_id"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	Identity        blockdev.DeviceIdentity `json:"identity"`
	Fingerprint     string                  `json:"device_fingerprint"`
	LockFingerprint string                  `json:"lock_fingerprint"`
	Plan            FormatPlan              `json:"plan"`

	// Stage is the LAST COMPLETED stage. PendingStep is what was about to run
	// when this record was written, which is the whole point: after a crash the
	// pair says exactly where the disk was left.
	Stage       FormatStage `json:"stage"`
	PendingStep string      `json:"pending_step,omitempty"`

	// DeviceModified flips the first time a step changes the device, and never
	// flips back. It is the answer to "is my disk still what it was".
	DeviceModified bool `json:"device_modified"`

	Terminal bool `json:"terminal"`
	// Interrupted marks a job that was running when the process stopped.
	//
	// It is deliberately NOT Terminal. Conflating "the process died" with "the
	// job is over" made Resume unreachable: RecoverInterruptedJobs runs at
	// every start and marked every unfinished job Terminal, while the
	// Diagnosis it returned for that same job reported Resumable — an API that
	// contradicted itself, offering a resume that could never be accepted.
	//
	// A power cut mid-format leaves work that CAN be continued from the last
	// proven stage. That is a different state from "finished", and it needs
	// its own name.
	Interrupted     bool   `json:"interrupted"`
	Result          string `json:"result,omitempty"` // success | failed | cancelled | interrupted
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorDetail     string `json:"error_detail,omitempty"`
	CancelRequested bool   `json:"cancel_requested"`

	// Outcome facts, filled in by validation, not by mkfs's exit status.
	ResultDevice     string `json:"result_device,omitempty"`
	ResultFilesystem string `json:"result_filesystem,omitempty"`
	ResultUUID       string `json:"result_uuid,omitempty"`
	ResultLabel      string `json:"result_label,omitempty"`
	ResultMountpoint string `json:"result_mountpoint,omitempty"`

	// Checksum proves the record was written by this code and not truncated or
	// edited. A record that does not verify is reported as UNKNOWN, never as
	// COMPLETE — "the file says it finished" is not evidence that it did.
	Checksum string `json:"checksum"`

	// Corrupt is set on load, never persisted.
	Corrupt bool `json:"-"`
}

// CancelState reports what cancellation means for this job right now.
func (j JobRecord) CancelState() CancelState {
	if j.Terminal || j.Interrupted {
		// Nothing is running: the job either finished or died with the
		// process. There is no in-flight work to cancel.
		return CancelStateFinished
	}
	if j.CancelRequested {
		if nonCancellableSteps[stepKind(j.PendingStep)] {
			return CancelStateNonCancellablePhase
		}
		if j.DeviceModified {
			return CancelStateRecovering
		}
		return CancelStateRequested
	}
	if nonCancellableSteps[stepKind(j.PendingStep)] {
		return CancelStateNonCancellablePhase
	}
	for _, p := range cancelPolicy {
		if p.Stage == j.Stage {
			if !p.Cancellable {
				return CancelStateNonCancellablePhase
			}
			return CancelStateCancellable
		}
	}
	return CancelStateNonCancellablePhase
}

// ---------------------------------------------------------------- runner

// CommandSpec is one external helper invocation. It is a struct rather than a
// string because a string is a shell injection waiting for a volume label with
// a semicolon in it. There is no code path here that builds a command line.
type CommandSpec struct {
	Name    string
	Args    []string
	Stdin   string
	Timeout time.Duration
}

// CommandResult is what a helper reported.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CommandRunner executes helpers. Indirected so the whole state machine can be
// exercised without destroying a disk.
type CommandRunner interface {
	Run(ctx context.Context, spec CommandSpec) (CommandResult, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = 60 * time.Second
	}
	// Every helper is bounded. A hung mkfs on a failing USB bridge would
	// otherwise hold the device lock forever and take the job with it.
	tctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	path, err := lookupHelper(spec.Name)
	if err != nil {
		return CommandResult{ExitCode: -1}, fail(ErrCodeCapability,
			"helper %s is not available on this host", spec.Name)
	}
	// Structured arguments only: exec.CommandContext never involves a shell.
	cmd := exec.CommandContext(tctx, path, spec.Args...)
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	res := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode = -1
	}
	if tctx.Err() != nil && runErr != nil {
		return res, fmt.Errorf("%s timed out after %s", spec.Name, spec.Timeout)
	}
	return res, runErr
}

// lookupHelper resolves a helper through the same indirection capability
// discovery uses, so a test that presents a host without mkfs.exfat presents it
// consistently to discovery and to execution.
func lookupHelper(name string) (string, error) {
	capMu.RLock()
	lp := lookPath
	capMu.RUnlock()
	return lp(name)
}

// ---------------------------------------------------------------- formatter

// managedMountRoots are the only places CasaNova will unmount from.
//
// This is the boundary between "the volume CasaNova mounted for the user" and
// "something else on this box is using that disk". Unmounting the second kind
// because the user clicked Format is how a database loses its data directory.
// Anything mounted outside these roots produces DEVICE_BUSY naming the
// mountpoint, and there is no force flag that overrides it.
var defaultManagedMountRoots = []string{"/media", "/mnt/casanova", "/DATA"}

const defaultMountRoot = "/media"

// prepared is a live, unexecuted plan.
type prepared struct {
	plan        FormatPlan
	principalID string
	expiresAt   time.Time
}

// Formatter owns the two-stage protocol. Its dependencies are injected so the
// entire state machine — including every abort path — is testable without
// hardware and without root.
type Formatter struct {
	discover        func() (blockdev.Inventory, error)
	runner          CommandRunner
	capabilities    func(context.Context) Capabilities
	addStorageRoot  func(path, note string) error
	isMountpoint    func(path string) (bool, error)
	now             func() time.Time
	newID           func() string
	managedRoots    []string
	mountRoot       string
	tokenTTL        time.Duration
	settleTimeout   time.Duration
	settleInterval  time.Duration
	stepTimeout     time.Duration
	mkfsTimeout     time.Duration
	auditFn         func(AuditEntry) error
	mu              sync.Mutex
	live            map[string]*prepared
	consumed        map[string]time.Time
	expired         map[string]time.Time
	cancelRequested map[string]bool
}

// NewFormatter builds the production formatter.
func NewFormatter() *Formatter {
	return &Formatter{
		discover:        blockdev.Discover,
		runner:          execRunner{},
		capabilities:    CurrentCapabilities,
		addStorageRoot:  storageauth.AddOperatorRoot,
		isMountpoint:    defaultIsMountpoint,
		now:             time.Now,
		newID:           randomID,
		managedRoots:    append([]string{}, defaultManagedMountRoots...),
		mountRoot:       defaultMountRoot,
		tokenTTL:        5 * time.Minute,
		settleTimeout:   15 * time.Second,
		settleInterval:  250 * time.Millisecond,
		stepTimeout:     60 * time.Second,
		mkfsTimeout:     30 * time.Minute,
		auditFn:         Audit,
		live:            map[string]*prepared{},
		consumed:        map[string]time.Time{},
		expired:         map[string]time.Time{},
		cancelRequested: map[string]bool{},
	}
}

var (
	defaultFormatterOnce sync.Once
	defaultFormatter     *Formatter
)

// DefaultFormatter is the process-wide formatter.
func DefaultFormatter() *Formatter {
	defaultFormatterOnce.Do(func() { defaultFormatter = NewFormatter() })
	return defaultFormatter
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable operation id would let a caller guess a token it was
		// never given. Refusing to produce one is the only safe failure.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------- targets

// FormatTarget is an offerable device, with the reason attached when it is not.
type FormatTarget struct {
	Identity          blockdev.DeviceIdentity     `json:"identity"`
	Fingerprint       string                      `json:"device_fingerprint"`
	Name              string                      `json:"name"`
	Model             string                      `json:"model,omitempty"`
	Serial            string                      `json:"serial,omitempty"`
	SizeBytes         uint64                      `json:"size_bytes"`
	SizeHuman         string                      `json:"size_human"`
	Transport         blockdev.Transport          `json:"transport"`
	IsPartition       bool                        `json:"is_partition"`
	Mountpoints       []string                    `json:"mountpoints,omitempty"`
	Eligible          bool                        `json:"eligible"`
	ProtectionReasons []blockdev.ProtectionReason `json:"protection_reasons,omitempty"`
}

// Targets lists every block device with its eligibility verdict.
//
// Protected devices are RETURNED, not hidden: a user who cannot find their disk
// in a list needs to know it was refused and why, otherwise the next step is to
// go looking for a command line.
func (f *Formatter) Targets() ([]FormatTarget, error) {
	inv, err := f.discover()
	if err != nil {
		return nil, fail(ErrCodeDiscoveryIncmplete, "block device discovery failed: %v", err)
	}
	out := make([]FormatTarget, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		out = append(out, FormatTarget{
			Identity: d.Identity, Fingerprint: d.Identity.Fingerprint(),
			Name: d.Name, Model: d.Identity.Model, Serial: d.Identity.Serial,
			SizeBytes: d.SizeBytes, SizeHuman: humanBytes(d.SizeBytes),
			Transport: d.Transport, IsPartition: d.IsPartition,
			Mountpoints: d.Mountpoints, Eligible: d.Eligible(),
			ProtectionReasons: d.ProtectionReasons,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- prepare

// PrepareFormatRequest is what a caller asks for. Note what it does NOT
// contain: a device path. Binding is by identity, always.
type PrepareFormatRequest struct {
	Identity      blockdev.DeviceIdentity `json:"identity"`
	Fingerprint   string                  `json:"device_fingerprint,omitempty"`
	Filesystem    Filesystem              `json:"filesystem"`
	Label         string                  `json:"label,omitempty"`
	PartitionMode PartitionMode           `json:"partition_mode,omitempty"`
}

// Prepare re-discovers the hardware, proves the target may be destroyed, and
// returns a short-lived single-use plan.
//
// Prepare never touches the device.
func (f *Formatter) Prepare(ctx context.Context, p storageauth.Principal, req PrepareFormatRequest) (FormatPlan, error) {
	if err := requireFormatAuthority(p); err != nil {
		return FormatPlan{}, err
	}
	if req.PartitionMode == "" {
		req.PartitionMode = PartitionGPT
	}
	switch req.PartitionMode {
	case PartitionGPT, PartitionMBR:
	default:
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "unknown partition mode %q", req.PartitionMode)
	}
	req.Label = strings.TrimSpace(req.Label)
	if err := ValidateLabel(req.Filesystem, req.Label); err != nil {
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "%v", err)
	}
	// A label is passed as the ARGUMENT of an option, so getopt cannot mistake
	// it for a flag. Rejecting a leading dash anyway costs a user nothing and
	// removes the whole class of argument-injection reasoning from review.
	if strings.HasPrefix(req.Label, "-") {
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "a volume label cannot start with '-'")
	}

	inv, dev, err := f.locateTarget(req.Identity, req.Fingerprint)
	if err != nil {
		return FormatPlan{}, err
	}
	if err := f.validateTarget(inv, dev); err != nil {
		return FormatPlan{}, err
	}

	caps := f.capabilities(ctx)
	// Capability is proven BEFORE the job, not discovered halfway through. A
	// user who confirms a destructive operation and then learns mkfs.exfat is
	// missing has been walked to the edge of losing data for nothing.
	if err := f.requireCapabilities(caps, req.Filesystem, dev.IsPartition); err != nil {
		return FormatPlan{}, err
	}
	// The data partition is the disk minus the 1 MiB alignment gap; validating
	// against the disk size would let a FAT32 request through on a volume it
	// cannot address.
	dataSize := dev.SizeBytes
	if !dev.IsPartition && dataSize > alignmentBytes {
		dataSize -= alignmentBytes
	}
	if err := ValidateFilesystemForDevice(caps, req.Filesystem, dataSize); err != nil {
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "%v", err)
	}
	// The same argument as the FAT32 check above, for the TABLE instead of the
	// filesystem: an MBR/DOS entry addresses a 32-bit sector count, so it
	// cannot describe a disk larger than 2 TiB. Discovering that after the
	// wipe leaves the user with a destroyed disk and, worse, a table that
	// addresses only part of it. Only whole-disk jobs write a table; a
	// partition target keeps the one already there, so its mode is not used
	// and must not be a reason to refuse.
	if req.PartitionMode == PartitionMBR && !dev.IsPartition && dev.SizeBytes > mbrMaxAddressableBytes {
		return FormatPlan{}, fail(ErrCodeInvalidRequest,
			"an MBR partition table cannot address a disk of %s (limit %s); "+
				"choose GPT for a drive this size",
			humanBytes(dev.SizeBytes), humanBytes(mbrMaxAddressableBytes))
	}

	view, err := f.inspect(ctx, inv, dev)
	if err != nil {
		return FormatPlan{}, err
	}

	id := f.newID()
	if id == "" {
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "could not generate an operation identifier")
	}
	now := f.now()
	plan := FormatPlan{
		OperationID: id,
		CreatedAt:   now,
		ExpiresAt:   now.Add(f.tokenTTL),

		Identity: dev.Identity, Fingerprint: dev.Identity.Fingerprint(),
		LockFingerprint: lockFingerprint(inv, dev),

		DeviceName: dev.Name, DevicePath: dev.Path, IsPartition: dev.IsPartition,
		Model: dev.Identity.Model, Vendor: dev.Identity.Vendor,
		Serial: dev.Identity.Serial, WWN: dev.Identity.WWN,
		Transport: dev.Transport, SizeBytes: dev.SizeBytes,
		SizeHuman: humanBytes(dev.SizeBytes),

		CurrentTable:      view.table,
		CurrentFilesystem: view.filesystem,
		CurrentLabel:      view.label,
		CurrentPartitions: view.partitions,

		Filesystem: req.Filesystem, Label: req.Label, PartitionMode: req.PartitionMode,
		CancelPolicy: append([]StageCancelPolicy{}, cancelPolicy...),
		Guarantees:   guaranteeText,
	}
	plan.steps = planSteps(dev.IsPartition)
	plan.Actions = describeActions(plan, view)

	th, err := topologyHash(inv, dev)
	if err != nil {
		return FormatPlan{}, fail(ErrCodeDiscoveryIncmplete, "could not snapshot the device topology: %v", err)
	}
	plan.TopologyHash = th
	ph, err := planHash(plan)
	if err != nil {
		return FormatPlan{}, fail(ErrCodeInvalidRequest, "could not seal the plan: %v", err)
	}
	plan.PlanHash = ph

	if err := f.auditFn(AuditEntry{
		PrincipalID: p.ID, Operation: "format.prepare",
		Fingerprint: plan.Fingerprint, Model: plan.Model, Serial: plan.Serial,
		SizeBytes: plan.SizeBytes,
		Requested: fmt.Sprintf("%s label=%q mode=%s op=%s",
			req.Filesystem, req.Label, req.PartitionMode, plan.OperationID),
		Result: "prepared",
	}); err != nil {
		// If the destruction cannot be recorded, it does not happen.
		return FormatPlan{}, fail(ErrCodeAuditUnavailable, "the audit log could not be written: %v", err)
	}

	f.mu.Lock()
	f.pruneLocked(now)
	f.live[plan.OperationID] = &prepared{plan: plan, principalID: p.ID, expiresAt: plan.ExpiresAt}
	f.mu.Unlock()
	return plan, nil
}

// requireFormatAuthority is the role gate.
//
// Formatting is an administrative act on the appliance's hardware. It is not
// part of any user's file access, so it is not expressible as a storage grant:
// it is admin-only, and there is no per-path grant that substitutes.
func requireFormatAuthority(p storageauth.Principal) error {
	if strings.TrimSpace(p.ID) == "" {
		return fail(ErrCodeNotAuthorized, "an anonymous caller cannot format a device")
	}
	if !p.Admin || !strings.EqualFold(strings.TrimSpace(p.Role), "admin") {
		return fail(ErrCodeNotAuthorized,
			"formatting a device requires an administrator; %q holds the role %q", p.ID, p.Role)
	}
	return nil
}

func (f *Formatter) requireCapabilities(caps Capabilities, fs Filesystem, isPartition bool) error {
	if ok, detail := caps.Available(fs); !ok {
		return fail(ErrCodeCapability, "%s cannot be created on this host: %s", fs, detail)
	}
	if !caps.CanWipeSignatures {
		return fail(ErrCodeCapability, "wipefs is not available; stale filesystem signatures cannot be cleared")
	}
	if !caps.CanIdentify {
		return fail(ErrCodeCapability,
			"blkid is not available; the result of a format could not be verified, so the format is refused")
	}
	if !isPartition && !caps.CanPartition {
		return fail(ErrCodeCapability, "sfdisk is not available; a partition table cannot be written")
	}
	for _, helper := range []string{"mount", "umount"} {
		if _, err := lookupHelper(helper); err != nil {
			return fail(ErrCodeCapability,
				"%s is not available; the new filesystem could not be validated by mounting it", helper)
		}
	}
	return nil
}

// locateTarget finds the device the caller means, by identity.
func (f *Formatter) locateTarget(want blockdev.DeviceIdentity, fp string) (blockdev.Inventory, blockdev.Device, error) {
	inv, err := f.discover()
	if err != nil {
		// FAIL-CLOSED: a discovery error is never "nothing in the way".
		return inv, blockdev.Device{}, fail(ErrCodeDiscoveryIncmplete,
			"block device discovery failed, so no device can be proven safe to format: %v", err)
	}
	if want.Strength() == blockdev.StrengthWeak {
		return inv, blockdev.Device{}, fail(ErrCodeIdentityWeak,
			"the supplied device identity is too weak to bind a destructive operation to")
	}
	if strings.TrimSpace(fp) == "" {
		fp = want.Fingerprint()
	}
	// The fingerprint is NOT the lookup key. A partition and its disk report
	// the same fingerprint whenever the hardware supplies a serial or WWN — the
	// fingerprint is derived from the device, not from the slice of it — so
	// looking a target up by fingerprint would silently return the whole disk
	// for a partition request. Full identity equality is the binding, and an
	// ambiguous match is refused rather than resolved by picking one.
	dev, err := findByIdentity(inv, want)
	if err != nil {
		return inv, blockdev.Device{}, err
	}
	if dev.Identity.Fingerprint() != fp {
		return inv, blockdev.Device{}, fail(ErrCodeIdentityMismatch,
			"the device matching this identity fingerprints as %s, not %s",
			dev.Identity.Fingerprint(), fp)
	}
	return inv, dev, nil
}

// findByIdentity is the only way a device is ever resolved from a request.
func findByIdentity(inv blockdev.Inventory, want blockdev.DeviceIdentity) (blockdev.Device, error) {
	var matches []blockdev.Device
	for _, d := range inv.Devices {
		if d.Identity.Equal(want) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return blockdev.Device{}, fail(ErrCodeDeviceNotFound,
			"no attached device matches the identity this operation was prepared against")
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return blockdev.Device{}, &FormatError{Code: ErrCodeIdentityMismatch,
			Detail:   "more than one attached device matches this identity, so the target is ambiguous",
			Blockers: names}
	}
}

// validateTarget re-proves every safety property. It is called at prepare time
// AND again immediately before the first destructive step; it deliberately does
// not trust the inventory to have propagated anything.
func (f *Formatter) validateTarget(inv blockdev.Inventory, dev blockdev.Device) error {
	if !inv.Complete {
		// Some other device could not be read. That device might be the one
		// holding root, and the propagation that would have protected this one
		// depends on facts we do not have.
		return &FormatError{Code: ErrCodeDiscoveryIncmplete,
			Detail:   "the block device inventory is incomplete, so protection cannot be proven",
			Blockers: inv.Incomplete}
	}
	if dev.Identity.Strength() == blockdev.StrengthWeak {
		return fail(ErrCodeIdentityWeak, "%s cannot be identified well enough to destroy", dev.Name)
	}
	if dev.Protected {
		return &FormatError{Code: ErrCodeDeviceProtected,
			Detail:   fmt.Sprintf("%s is protected and can never be formatted", dev.Name),
			Blockers: reasonStrings(dev.ProtectionReasons)}
	}
	// The parent is re-checked HERE rather than relying on the inventory having
	// propagated protection downward. Wiping a partition on the disk that holds
	// root destroys root; that must be refused by this code on its own facts,
	// not because another component happened to mark the child.
	if dev.IsPartition {
		parent, ok := inv.ByName(dev.Parent)
		if !ok {
			return fail(ErrCodeDiscoveryIncmplete,
				"%s claims a parent disk %q that is not in the inventory", dev.Name, dev.Parent)
		}
		if parent.Protected {
			return &FormatError{Code: ErrCodeParentProtected,
				Detail: fmt.Sprintf("%s lives on %s, which is protected; formatting a partition of a "+
					"protected disk destroys what protects it", dev.Name, parent.Name),
				Blockers: reasonStrings(parent.ProtectionReasons)}
		}
		// A protected SIBLING means the disk carries something critical, even
		// if propagation has not yet marked the parent.
		for _, sib := range parent.Children {
			s, ok := inv.ByName(sib)
			if !ok {
				return fail(ErrCodeDiscoveryIncmplete,
					"partition %q of %s is not in the inventory", sib, parent.Name)
			}
			if s.Name != dev.Name && s.Protected {
				return &FormatError{Code: ErrCodeParentProtected,
					Detail: fmt.Sprintf("%s shares the disk %s with the protected partition %s",
						dev.Name, parent.Name, s.Name),
					Blockers: reasonStrings(s.ProtectionReasons)}
			}
		}
	}
	if !dev.Eligible() {
		return fail(ErrCodeNotEligible,
			"%s is not an eligible target: it is not external, removable, writable and identifiable storage", dev.Name)
	}
	// Mount and holder safety, on the target and on everything below it.
	if err := f.checkQuiescable(inv, dev); err != nil {
		return err
	}
	return nil
}

// checkQuiescable proves every mount and holder below the target is something
// CasaNova is entitled to take down.
func (f *Formatter) checkQuiescable(inv blockdev.Inventory, dev blockdev.Device) error {
	var blockers []string
	scope := []blockdev.Device{dev}
	for _, c := range dev.Children {
		child, ok := inv.ByName(c)
		if !ok {
			return fail(ErrCodeDiscoveryIncmplete,
				"partition %q of %s is not in the inventory", c, dev.Name)
		}
		scope = append(scope, child)
	}
	for _, d := range scope {
		for _, h := range d.Holders {
			// LVM, mdraid, dm-crypt. Discovery cannot see what is layered on
			// top, so there is nothing to reason about: refuse.
			blockers = append(blockers, fmt.Sprintf("%s is held by %s", d.Name, h))
		}
		for _, mp := range d.Mountpoints {
			if !f.isManagedMount(mp) {
				blockers = append(blockers, fmt.Sprintf(
					"%s is mounted at %s, which CasaNova does not manage", d.Name, mp))
			}
		}
	}
	if len(blockers) > 0 {
		sort.Strings(blockers)
		return &FormatError{Code: ErrCodeDeviceBusy,
			Detail: fmt.Sprintf("%s is in use and will not be taken down automatically; "+
				"stop whatever is using it and try again", dev.Name),
			Blockers: blockers}
	}
	return nil
}

func (f *Formatter) isManagedMount(mp string) bool {
	clean := filepath.Clean(mp)
	if clean == "" || clean == "/" {
		return false
	}
	for _, root := range f.managedRoots {
		r := filepath.Clean(root)
		if clean == r {
			// The root itself is a CasaNova directory, not a mounted volume.
			// Unmounting it would take every volume with it.
			return false
		}
		if strings.HasPrefix(clean, r+"/") {
			return true
		}
	}
	return false
}

func reasonStrings(rs []blockdev.ProtectionReason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}

// lockFingerprint is the PHYSICAL device key. For a partition it is the parent
// disk's, so a whole-disk job and a partition job on the same hardware collide.
func lockFingerprint(inv blockdev.Inventory, dev blockdev.Device) string {
	if dev.IsPartition {
		if parent, ok := inv.ByName(dev.Parent); ok {
			return parent.Identity.Fingerprint()
		}
	}
	return dev.Identity.Fingerprint()
}

func planSteps(isPartition bool) []stepKind {
	if isPartition {
		// A partition target keeps the existing table: rewriting it would
		// destroy the sibling partitions the user did not ask about.
		return []stepKind{stepQuiesce, stepWipe, stepMkfs, stepVerify, stepIntegrate}
	}
	return []stepKind{stepQuiesce, stepWipe, stepPartition, stepSettle, stepMkfs, stepVerify, stepIntegrate}
}

// deviceView is what the device looks like right now, for the plan.
type deviceView struct {
	table      string
	filesystem string
	label      string
	partitions []PartitionView
	mounted    []string
}

func (f *Formatter) inspect(ctx context.Context, inv blockdev.Inventory, dev blockdev.Device) (deviceView, error) {
	var v deviceView
	probe, err := f.probe(ctx, dev.Path)
	if err != nil {
		return v, err
	}
	v.table = probe["PTTYPE"]
	v.filesystem = probe["TYPE"]
	v.label = probe["LABEL"]
	v.mounted = append(v.mounted, dev.Mountpoints...)

	for _, c := range dev.Children {
		child, ok := inv.ByName(c)
		if !ok {
			return v, fail(ErrCodeDiscoveryIncmplete, "partition %q vanished during inspection", c)
		}
		cp, err := f.probe(ctx, child.Path)
		if err != nil {
			return v, err
		}
		v.partitions = append(v.partitions, PartitionView{
			Name: child.Name, SizeBytes: child.SizeBytes,
			Filesystem: cp["TYPE"], Label: cp["LABEL"], UUID: cp["UUID"],
			Mountpoints: child.Mountpoints, Holders: child.Holders,
		})
		v.mounted = append(v.mounted, child.Mountpoints...)
	}
	sort.Slice(v.partitions, func(i, j int) bool { return v.partitions[i].Name < v.partitions[j].Name })
	return v, nil
}

// probe reads what is on a device with blkid.
//
// Exit status 2 means "nothing recognisable here", which is a fact, not a
// failure. Any other failure is fail-closed: if we cannot read what is on the
// disk, we do not get to destroy it.
func (f *Formatter) probe(ctx context.Context, path string) (map[string]string, error) {
	res, err := f.runner.Run(ctx, CommandSpec{
		Name: string(ToolBlkid), Timeout: f.stepTimeout,
		Args: []string{"-p", "-o", "export", "--", path},
	})
	out := map[string]string{}
	if err != nil {
		if res.ExitCode == 2 {
			return out, nil // no signature: an empty disk
		}
		return nil, fail(ErrCodeDiscoveryIncmplete,
			"could not read what is currently on %s: %v", path, err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		out[k] = val
	}
	return out, nil
}

func describeActions(plan FormatPlan, view deviceView) []DestructiveAction {
	var acts []DestructiveAction
	for _, mp := range view.mounted {
		acts = append(acts, DestructiveAction{
			Kind: string(stepQuiesce), Target: mp,
			Description: "unmount " + mp, Destroys: false,
		})
	}
	acts = append(acts, DestructiveAction{
		Kind: string(stepWipe), Target: plan.DeviceName,
		Description: fmt.Sprintf("remove every filesystem and partition-table signature from %s (%s, %s)",
			plan.DeviceName, plan.Model, plan.SizeHuman),
		Destroys: true,
	})
	if !plan.IsPartition {
		acts = append(acts, DestructiveAction{
			Kind: string(stepPartition), Target: plan.DeviceName,
			Description: fmt.Sprintf("write a new %s partition table with one data partition covering the whole disk",
				strings.ToUpper(string(plan.PartitionMode))),
			Destroys: true,
		})
	}
	label := plan.Label
	if label == "" {
		label = "(no label)"
	}
	acts = append(acts, DestructiveAction{
		Kind: string(stepMkfs), Target: plan.DeviceName,
		Description: fmt.Sprintf("create an empty %s filesystem labelled %s", plan.Filesystem, label),
		Destroys:    true,
	})
	for _, p := range view.partitions {
		if p.Filesystem == "" {
			continue
		}
		acts = append(acts, DestructiveAction{
			Kind: "data_loss", Target: p.Name,
			Description: fmt.Sprintf("the existing %s filesystem on %s (%s) becomes unreachable",
				p.Filesystem, p.Name, humanBytes(p.SizeBytes)),
			Destroys: true,
		})
	}
	return acts
}

// ---------------------------------------------------------------- hashes

// topologySnapshot is the subset of discovery that, if it changes between
// prepare and execute, means the plan is talking about a different situation.
type topologySnapshot struct {
	Identity blockdev.DeviceIdentity `json:"identity"`
	Devices  []topologyDevice        `json:"devices"`
}

type topologyDevice struct {
	Name        string   `json:"name"`
	Size        uint64   `json:"size"`
	IsPartition bool     `json:"is_partition"`
	Parent      string   `json:"parent,omitempty"`
	Children    []string `json:"children,omitempty"`
	ReadOnly    bool     `json:"read_only"`
	Protected   bool     `json:"protected"`
	Reasons     []string `json:"reasons,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Holders     []string `json:"holders,omitempty"`
}

func topologyHash(inv blockdev.Inventory, dev blockdev.Device) (string, error) {
	snap := topologySnapshot{Identity: dev.Identity}
	scope := []blockdev.Device{dev}
	for _, c := range dev.Children {
		child, ok := inv.ByName(c)
		if !ok {
			return "", fmt.Errorf("partition %q is missing from the inventory", c)
		}
		scope = append(scope, child)
	}
	if dev.IsPartition {
		parent, ok := inv.ByName(dev.Parent)
		if !ok {
			return "", fmt.Errorf("parent disk %q is missing from the inventory", dev.Parent)
		}
		scope = append(scope, parent)
		// Siblings matter: the disk gaining a system partition between prepare
		// and execute must invalidate the plan.
		for _, sib := range parent.Children {
			if sib == dev.Name {
				continue
			}
			s, ok := inv.ByName(sib)
			if !ok {
				return "", fmt.Errorf("partition %q is missing from the inventory", sib)
			}
			scope = append(scope, s)
		}
	}
	for _, d := range scope {
		snap.Devices = append(snap.Devices, topologyDevice{
			Name: d.Name, Size: d.SizeBytes, IsPartition: d.IsPartition,
			Parent: d.Parent, Children: append([]string{}, d.Children...),
			ReadOnly: d.ReadOnly, Protected: d.Protected,
			Reasons:     reasonStrings(d.ProtectionReasons),
			Mountpoints: append([]string{}, d.Mountpoints...),
			Holders:     append([]string{}, d.Holders...),
		})
	}
	sort.Slice(snap.Devices, func(i, j int) bool { return snap.Devices[i].Name < snap.Devices[j].Name })
	return hashJSON(snap)
}

// planHash seals the request. It excludes the operation id and the timestamps,
// which vary, and includes everything a user was shown and agreed to.
func planHash(p FormatPlan) (string, error) {
	type sealed struct {
		Identity    blockdev.DeviceIdentity `json:"identity"`
		Fingerprint string                  `json:"fp"`
		Lock        string                  `json:"lock"`
		Size        uint64                  `json:"size"`
		IsPartition bool                    `json:"is_partition"`
		FS          Filesystem              `json:"fs"`
		Label       string                  `json:"label"`
		Mode        PartitionMode           `json:"mode"`
		Actions     []DestructiveAction     `json:"actions"`
		Partitions  []PartitionView         `json:"partitions"`
	}
	return hashJSON(sealed{
		Identity: p.Identity, Fingerprint: p.Fingerprint, Lock: p.LockFingerprint,
		Size: p.SizeBytes, IsPartition: p.IsPartition,
		FS: p.Filesystem, Label: p.Label, Mode: p.PartitionMode,
		Actions: p.Actions, Partitions: p.CurrentPartitions,
	})
}

func hashJSON(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		// Returning an empty hash would compare equal to another empty hash and
		// silently disable the check. Errors propagate.
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------- execute

const alignmentBytes = 1 << 20 // 1 MiB, the alignment every current drive wants

// mbrMaxAddressableBytes is what a DOS/MBR partition entry can describe: a
// 32-bit LBA start plus a 32-bit sector count, at 512 bytes per sector — 512
// bytes short of 2 TiB, which is why a 4 TB disk needs GPT.
const mbrMaxAddressableBytes uint64 = uint64(^uint32(0)) * 512

// Execute consumes an operation token and performs the format.
//
// Everything proven at prepare time is proven again here, from a fresh
// discovery, before the first destructive syscall.
func (f *Formatter) Execute(ctx context.Context, p storageauth.Principal, operationID string) (JobRecord, error) {
	if err := requireFormatAuthority(p); err != nil {
		return JobRecord{}, err
	}
	pl, err := f.consume(operationID, p)
	if err != nil {
		return JobRecord{}, err
	}
	plan := pl.plan

	// One destructive job per physical device, keyed on stable identity. A
	// second execution cannot slip in beside the first.
	jobID := f.newID()
	if jobID == "" {
		return JobRecord{}, fail(ErrCodeInvalidRequest, "could not generate a job identifier")
	}
	if err := AcquireDevice(plan.LockFingerprint, jobID); err != nil {
		return JobRecord{}, fail(ErrCodeDeviceLocked, "%v", err)
	}
	defer ReleaseDevice(plan.LockFingerprint, jobID)

	job := &JobRecord{
		JobID: jobID, OperationID: plan.OperationID, PrincipalID: p.ID,
		StartedAt: f.now(), Identity: plan.Identity, Fingerprint: plan.Fingerprint,
		LockFingerprint: plan.LockFingerprint, Plan: plan, Stage: StageDiscovered,
		PendingStep: "revalidate",
	}
	if err := f.auditFn(AuditEntry{
		PrincipalID: p.ID, Operation: "format.execute", Fingerprint: plan.Fingerprint,
		Model: plan.Model, Serial: plan.Serial, SizeBytes: plan.SizeBytes,
		Requested: fmt.Sprintf("%s label=%q mode=%s job=%s",
			plan.Filesystem, plan.Label, plan.PartitionMode, jobID),
		Result: "started",
	}); err != nil {
		return *job, fail(ErrCodeAuditUnavailable, "the audit log could not be written: %v", err)
	}
	if err := f.persist(job); err != nil {
		return *job, err
	}

	res, runErr := f.run(ctx, job, plan.steps)
	f.finish(job, runErr)
	if auditErr := f.auditFn(AuditEntry{
		PrincipalID: p.ID, Operation: "format.execute", Fingerprint: plan.Fingerprint,
		Model: plan.Model, Serial: plan.Serial, SizeBytes: plan.SizeBytes,
		Requested: fmt.Sprintf("%s label=%q job=%s", plan.Filesystem, plan.Label, jobID),
		Result:    job.Result, Stage: string(job.Stage), Detail: job.ErrorDetail,
	}); auditErr != nil && runErr == nil {
		runErr = fail(ErrCodeAuditUnavailable, "the outcome could not be recorded: %v", auditErr)
	}
	_ = res
	if perr := f.persist(job); perr != nil && runErr == nil {
		runErr = perr
	}
	f.mu.Lock()
	delete(f.cancelRequested, jobID)
	f.mu.Unlock()
	return *job, runErr
}

// consume takes the token. Single use is enforced by removing it before any
// work starts: a caller that retries a request cannot get two formats.
func (f *Formatter) consume(operationID string, p storageauth.Principal) (*prepared, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.pruneLocked(now)

	if _, used := f.consumed[operationID]; used {
		return nil, fail(ErrCodeTokenConsumed,
			"operation %s was already executed; prepare a new one", operationID)
	}
	// Expiry is reported as expiry, not as "unknown operation". The two mean
	// different things to whoever is holding the token: one says try again from
	// the start, the other says something is broken.
	if at, gone := f.expired[operationID]; gone {
		return nil, fail(ErrCodeTokenExpired,
			"operation %s expired at %s; the device may have changed since, so prepare a new one",
			operationID, at.Format(time.RFC3339))
	}
	pl, ok := f.live[operationID]
	if !ok {
		return nil, fail(ErrCodeTokenUnknown,
			"operation %s is not a prepared format operation", operationID)
	}
	if !now.Before(pl.expiresAt) {
		delete(f.live, operationID)
		f.expired[operationID] = pl.expiresAt
		return nil, fail(ErrCodeTokenExpired,
			"operation %s expired at %s; the device may have changed since, so prepare a new one",
			operationID, pl.expiresAt.Format(time.RFC3339))
	}
	if pl.principalID != p.ID {
		return nil, fail(ErrCodeNotAuthorized,
			"operation %s was prepared by another administrator", operationID)
	}
	delete(f.live, operationID)
	f.consumed[operationID] = now
	return pl, nil
}

// pruneLocked drops expired plans and old consumed markers. Consumed markers
// outlive the plan on purpose, so a replayed token reports "already used"
// rather than "unknown".
func (f *Formatter) pruneLocked(now time.Time) {
	for id, pl := range f.live {
		if !now.Before(pl.expiresAt) {
			delete(f.live, id)
			f.expired[id] = pl.expiresAt
		}
	}
	for id, at := range f.consumed {
		if now.Sub(at) > 24*time.Hour {
			delete(f.consumed, id)
		}
	}
	for id, at := range f.expired {
		if now.Sub(at) > 24*time.Hour {
			delete(f.expired, id)
		}
	}
}

type runContext struct {
	dev    blockdev.Device
	inv    blockdev.Inventory
	target string // the path mkfs will be pointed at
}

func (f *Formatter) run(ctx context.Context, job *JobRecord, steps []stepKind) (runContext, error) {
	var rc runContext

	// REVALIDATION. Everything proven at prepare time is proven again here,
	// from a fresh discovery, before anything is touched.
	inv, dev, err := f.locateTarget(job.Plan.Identity, job.Plan.Fingerprint)
	if err != nil {
		return rc, err
	}
	if err := f.validateTarget(inv, dev); err != nil {
		return rc, err
	}
	th, err := topologyHash(inv, dev)
	if err != nil {
		return rc, fail(ErrCodeDiscoveryIncmplete, "could not re-snapshot the device topology: %v", err)
	}
	if th != job.Plan.TopologyHash {
		return rc, fail(ErrCodePlanChanged,
			"%s is not in the state the plan was built against; nothing was changed. "+
				"Review the device and prepare the operation again", dev.Name)
	}
	if ph, err := planHash(job.Plan); err != nil || ph != job.Plan.PlanHash {
		return rc, fail(ErrCodePlanChanged, "the prepared plan no longer matches its own seal")
	}
	if lockFingerprint(inv, dev) != job.Plan.LockFingerprint {
		return rc, fail(ErrCodePlanChanged, "the device's physical parent changed since the plan was prepared")
	}
	rc.inv, rc.dev, rc.target = inv, dev, dev.Path

	job.Stage = StageValidated
	job.PendingStep = string(steps[0])
	if err := f.persist(job); err != nil {
		return rc, err
	}

	for i, step := range steps {
		if f.cancelled(job.JobID) {
			job.CancelRequested = true
			return rc, &FormatError{Code: ErrCodeCancelled,
				Detail:         fmt.Sprintf("cancelled before %s", step),
				Stage:          job.Stage,
				DeviceModified: job.DeviceModified}
		}
		if err := ctx.Err(); err != nil {
			return rc, &FormatError{Code: ErrCodeCancelled,
				Detail: fmt.Sprintf("the request was abandoned before %s", step),
				Stage:  job.Stage, DeviceModified: job.DeviceModified}
		}
		if err := f.step(ctx, job, &rc, step); err != nil {
			return rc, err
		}
		job.Stage = stepStage[step]
		if i+1 < len(steps) {
			job.PendingStep = string(steps[i+1])
		} else {
			job.PendingStep = "finalize"
		}
		if err := f.persist(job); err != nil {
			return rc, err
		}
	}
	return rc, nil
}

// step runs one step. The stage and the step about to run are already on disk
// and fsynced before this is called, so an interruption anywhere inside is
// diagnosable.
func (f *Formatter) step(ctx context.Context, job *JobRecord, rc *runContext, k stepKind) error {
	switch k {
	case stepQuiesce:
		return f.doQuiesce(ctx, job, rc)
	case stepWipe:
		job.DeviceModified = true
		if err := f.persist(job); err != nil {
			return err
		}
		return f.doWipe(ctx, rc)
	case stepPartition:
		job.DeviceModified = true
		if err := f.persist(job); err != nil {
			return err
		}
		return f.doPartition(ctx, job, rc)
	case stepSettle:
		return f.doSettle(ctx, job, rc)
	case stepMkfs:
		return f.doMkfs(ctx, job, rc)
	case stepVerify:
		return f.doVerify(ctx, job, rc)
	case stepIntegrate:
		return f.doIntegrate(ctx, job, rc)
	default:
		return fail(ErrCodeInvalidRequest, "unknown step %q", k)
	}
}

// doQuiesce unmounts what CasaNova manages, and nothing else.
//
// There is no lazy unmount here. `umount -l` detaches the tree and lets the
// writers carry on against a filesystem that is about to be destroyed; it turns
// "device busy" into silent data loss.
func (f *Formatter) doQuiesce(ctx context.Context, job *JobRecord, rc *runContext) error {
	var mounts []string
	scope := []blockdev.Device{rc.dev}
	for _, c := range rc.dev.Children {
		if child, ok := rc.inv.ByName(c); ok {
			scope = append(scope, child)
		}
	}
	for _, d := range scope {
		for _, mp := range d.Mountpoints {
			if !f.isManagedMount(mp) {
				// checkQuiescable already refused this; reaching here means the
				// world changed underneath us.
				return &FormatError{Code: ErrCodeDeviceBusy,
					Detail:   fmt.Sprintf("%s became mounted at %s, which CasaNova does not manage", d.Name, mp),
					Blockers: []string{mp}}
			}
			mounts = append(mounts, mp)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mounts))) // deepest first
	for _, mp := range mounts {
		if _, err := f.runner.Run(ctx, CommandSpec{
			Name: "umount", Timeout: f.stepTimeout, Args: []string{"--", mp},
		}); err != nil {
			return &FormatError{Code: ErrCodeDeviceBusy,
				Detail: fmt.Sprintf("%s could not be unmounted; something is still using it. "+
					"CasaNova will not force it detached, because a forced detach lets writers "+
					"continue against a filesystem that is about to be destroyed", mp),
				Blockers: []string{mp, errText(err)}}
		}
	}
	_ = job
	return nil
}

// doWipe removes stale signatures.
//
// A disk that carried both a filesystem and an old RAID superblock will be
// mis-detected forever if only one is removed; wipefs --all is what makes the
// device honestly empty before the new table is written.
func (f *Formatter) doWipe(ctx context.Context, rc *runContext) error {
	// --force is required because wipefs refuses a device that still has a
	// partition table. Our own gates are strictly stronger than the one being
	// bypassed: the device is proven unprotected, unmounted, holder-free and
	// identity-matched before we get here.
	if _, err := f.runner.Run(ctx, CommandSpec{
		Name: string(ToolWipefs), Timeout: f.stepTimeout,
		Args: []string{"--all", "--force", "--", rc.dev.Path},
	}); err != nil {
		return fail(ErrCodeToolFailed, "wipefs failed on %s: %v", rc.dev.Name, err)
	}
	return nil
}

// gpt/mbr partition type codes for one data partition.
const (
	gptLinuxData     = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	gptMicrosoftData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
	mbrLinux         = "83"
	mbrNTFSExFAT     = "07"
	mbrFAT32LBA      = "0c"
)

func partitionType(mode PartitionMode, fs Filesystem) string {
	if mode == PartitionMBR {
		switch fs {
		case FSExt4:
			return mbrLinux
		case FSFAT32:
			return mbrFAT32LBA
		default:
			return mbrNTFSExFAT
		}
	}
	if fs == FSExt4 {
		return gptLinuxData
	}
	// exFAT, NTFS and FAT32 volumes are read by Windows and cameras, which
	// expect the Microsoft basic data type.
	return gptMicrosoftData
}

// doPartition writes a brand-new table with one aligned data partition.
//
// It does NOT preserve whatever was underneath. "Format this disk" that leaves
// an unknown old table in place produces a device that boots differently, shows
// phantom partitions, and cannot be explained to the user later.
func (f *Formatter) doPartition(ctx context.Context, job *JobRecord, rc *runContext) error {
	label := "gpt"
	if job.Plan.PartitionMode == PartitionMBR {
		label = "dos"
	}
	// The script is fixed text plus a constant type code; no caller-supplied
	// value reaches it, and it is fed on stdin rather than through a shell.
	script := fmt.Sprintf("label: %s\nstart=%d, type=%s\n",
		label, alignmentBytes/512, partitionType(job.Plan.PartitionMode, job.Plan.Filesystem))

	if _, err := f.runner.Run(ctx, CommandSpec{
		Name: string(ToolSfdisk), Timeout: f.stepTimeout, Stdin: script,
		Args: []string{"--wipe", "always", "--wipe-partitions", "always",
			"--label", label, "--", rc.dev.Path},
	}); err != nil {
		return fail(ErrCodeToolFailed, "writing the %s partition table to %s failed: %v",
			strings.ToUpper(string(job.Plan.PartitionMode)), rc.dev.Name, err)
	}
	return nil
}

// doSettle waits for the kernel to publish the new partition.
//
// It polls DISCOVERY rather than sleeping and hoping: the partition is
// considered visible when the inventory shows it as a child of the disk whose
// identity we are still bound to, which also re-proves we are looking at the
// right hardware.
func (f *Formatter) doSettle(ctx context.Context, job *JobRecord, rc *runContext) error {
	if caps := f.capabilities(ctx); caps.CanPartition {
		// partprobe is best-effort: sfdisk already asks the kernel to re-read.
		if _, err := lookupHelper(string(ToolPartprobe)); err == nil {
			_, _ = f.runner.Run(ctx, CommandSpec{
				Name: string(ToolPartprobe), Timeout: f.stepTimeout,
				Args: []string{"--", rc.dev.Path},
			})
		}
	}
	deadline := f.now().Add(f.settleTimeout)
	for {
		inv, err := f.discover()
		if err == nil {
			if dev, derr := findByIdentity(inv, job.Plan.Identity); derr == nil {
				if len(dev.Children) == 1 {
					child, ok := inv.ByName(dev.Children[0])
					if !ok {
						return fail(ErrCodeDiscoveryIncmplete,
							"the new partition %q is not in the inventory", dev.Children[0])
					}
					if child.SizeBytes == 0 {
						return fail(ErrCodeVerificationFailed,
							"the new partition on %s has zero size", dev.Name)
					}
					rc.inv, rc.dev, rc.target = inv, dev, child.Path
					return nil
				}
				if len(dev.Children) > 1 {
					return fail(ErrCodeVerificationFailed,
						"%s shows %d partitions after writing a single-partition table",
						dev.Name, len(dev.Children))
				}
			}
		}
		if !f.now().Before(deadline) {
			return fail(ErrCodeVerificationFailed,
				"the new partition on %s did not appear within %s; the partition table was written "+
					"but no filesystem was created", rc.dev.Name, f.settleTimeout)
		}
		select {
		case <-ctx.Done():
			return fail(ErrCodeInterrupted, "waiting for the new partition was interrupted")
		case <-time.After(f.settleInterval):
		}
	}
}

func (f *Formatter) mkfsSpec(job *JobRecord, target string) (CommandSpec, error) {
	label := job.Plan.Label
	var name string
	var args []string
	switch job.Plan.Filesystem {
	case FSExt4:
		name = string(ToolMkfsExt4)
		args = []string{"-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case FSExFAT:
		name = string(ToolMkfsExFAT)
		if label != "" {
			args = append(args, "-L", label)
		}
	case FSNTFS:
		name = string(ToolMkfsNTFS)
		args = []string{"--quick", "--force"}
		if label != "" {
			args = append(args, "--label", label)
		}
	case FSFAT32:
		name = string(ToolMkfsFAT)
		args = []string{"-F", "32"}
		if label != "" {
			args = append(args, "-n", label)
		}
	default:
		return CommandSpec{}, fail(ErrCodeInvalidRequest, "unsupported filesystem %q", job.Plan.Filesystem)
	}
	// "--" terminates options so the device operand can never be read as a
	// flag, and the label is always an option ARGUMENT, never positional.
	args = append(args, "--", target)
	return CommandSpec{Name: name, Args: args, Timeout: f.mkfsTimeout}, nil
}

func (f *Formatter) doMkfs(ctx context.Context, job *JobRecord, rc *runContext) error {
	spec, err := f.mkfsSpec(job, rc.target)
	if err != nil {
		return err
	}
	if _, err := f.runner.Run(ctx, spec); err != nil {
		// mkfs failing is a failure. There is no path from here to "Ready".
		return fail(ErrCodeToolFailed,
			"creating the %s filesystem on %s failed: %v", job.Plan.Filesystem, rc.target, err)
	}
	job.ResultDevice = rc.target
	return nil
}

// expectedFSType is what blkid must report for each filesystem we create.
var expectedFSType = map[Filesystem][]string{
	FSExt4:  {"ext4"},
	FSExFAT: {"exfat"},
	FSNTFS:  {"ntfs"},
	FSFAT32: {"vfat"},
}

// mountTypes are the kernel drivers to try, in order.
var mountTypes = map[Filesystem][]string{
	FSExt4:  {"ext4"},
	FSExFAT: {"exfat"},
	FSNTFS:  {"ntfs3", "ntfs"},
	FSFAT32: {"vfat"},
}

// doVerify is the reason mkfs returning 0 is not the end of the operation.
//
// mkfs exits 0 having written to a device that silently discards writes; a USB
// bridge can report success and produce a volume that cannot be mounted; a
// label can be silently truncated or dropped. Nothing here is "Ready" until the
// filesystem has been detected, mounted, written to, read back, unmounted and
// mounted again.
func (f *Formatter) doVerify(ctx context.Context, job *JobRecord, rc *runContext) error {
	inv, err := f.discover()
	if err != nil {
		return fail(ErrCodeVerificationFailed, "could not re-read the device after formatting: %v", err)
	}
	dev, derr := findByIdentity(inv, job.Plan.Identity)
	if derr != nil {
		return fail(ErrCodeIdentityMismatch,
			"the device changed during the operation; the result cannot be validated (%v)", derr)
	}
	if !job.Plan.IsPartition {
		if len(dev.Children) != 1 {
			return fail(ErrCodeVerificationFailed,
				"%s shows %d partitions after formatting, expected exactly 1", dev.Name, len(dev.Children))
		}
		child, ok := inv.ByName(dev.Children[0])
		if !ok {
			return fail(ErrCodeVerificationFailed, "the expected partition is not in the inventory")
		}
		rc.target = child.Path
	} else {
		rc.target = dev.Path
	}
	job.ResultDevice = rc.target

	probe, err := f.probe(ctx, rc.target)
	if err != nil {
		return fail(ErrCodeVerificationFailed, "%v", err)
	}
	got := strings.ToLower(probe["TYPE"])
	if !containsFold(expectedFSType[job.Plan.Filesystem], got) {
		return fail(ErrCodeVerificationFailed,
			"%s reports the filesystem %q, expected %s", rc.target, probe["TYPE"], job.Plan.Filesystem)
	}
	if strings.TrimSpace(probe["UUID"]) == "" {
		return fail(ErrCodeVerificationFailed,
			"%s has no filesystem UUID; the filesystem is not usable", rc.target)
	}
	job.ResultFilesystem = got
	job.ResultUUID = probe["UUID"]
	job.ResultLabel = probe["LABEL"]

	if want := job.Plan.Label; want != "" {
		gotLabel := strings.TrimSpace(probe["LABEL"])
		// FAT32 stores labels uppercased; the others preserve them.
		if !strings.EqualFold(gotLabel, want) {
			return fail(ErrCodeVerificationFailed,
				"the volume label is %q, but %q was requested", gotLabel, want)
		}
	}

	// Mount, prove it is really mounted, write, read back, delete, unmount,
	// and mount again.
	mp, err := f.mountpointFor(job)
	if err != nil {
		return err
	}
	if err := f.mountRoundTrip(ctx, job, rc.target, mp); err != nil {
		return err
	}
	if err := f.mountRoundTrip(ctx, job, rc.target, mp); err != nil {
		return fail(ErrCodeVerificationFailed,
			"the new filesystem mounted once but not a second time: %v", err)
	}
	job.ResultMountpoint = mp
	return nil
}

func (f *Formatter) mountRoundTrip(ctx context.Context, job *JobRecord, target, mp string) error {
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return fail(ErrCodeVerificationFailed, "could not create the mountpoint %s: %v", mp, err)
	}
	var mountErr error
	mounted := false
	for _, t := range mountTypes[job.Plan.Filesystem] {
		_, err := f.runner.Run(ctx, CommandSpec{
			Name: "mount", Timeout: f.stepTimeout,
			// nosuid,nodev because this is user-supplied removable media.
			Args: []string{"-t", t, "-o", "rw,nosuid,nodev", "--", target, mp},
		})
		if err == nil {
			mounted = true
			break
		}
		mountErr = err
	}
	if !mounted {
		return fail(ErrCodeVerificationFailed,
			"the new %s filesystem on %s could not be mounted: %v", job.Plan.Filesystem, target, mountErr)
	}
	defer func() {
		_, _ = f.runner.Run(ctx, CommandSpec{Name: "umount", Timeout: f.stepTimeout, Args: []string{"--", mp}})
	}()

	// Prove the writes below land on the NEW filesystem and not on the root
	// filesystem underneath an empty directory.
	real, err := f.isMountpoint(mp)
	if err != nil {
		return fail(ErrCodeVerificationFailed, "could not confirm %s is a mountpoint: %v", mp, err)
	}
	if !real {
		return fail(ErrCodeVerificationFailed,
			"mount reported success but %s is not a mountpoint; the validation writes would have "+
				"gone to the system disk", mp)
	}

	probe := filepath.Join(mp, ".casanova-format-check")
	payload := []byte("casanova format validation " + job.JobID)
	if err := os.WriteFile(probe, payload, 0o600); err != nil {
		return fail(ErrCodeVerificationFailed, "the new filesystem is not writable: %v", err)
	}
	back, err := os.ReadFile(probe)
	if err != nil {
		return fail(ErrCodeVerificationFailed, "the new filesystem is not readable: %v", err)
	}
	if string(back) != string(payload) {
		return fail(ErrCodeVerificationFailed,
			"data read back from the new filesystem did not match what was written")
	}
	if err := os.Remove(probe); err != nil {
		return fail(ErrCodeVerificationFailed, "the validation file could not be deleted: %v", err)
	}
	return nil
}

// mountpointFor derives the volume directory from the label, sanitised.
func (f *Formatter) mountpointFor(job *JobRecord) (string, error) {
	name := sanitizeVolumeName(job.Plan.Label)
	if name == "" {
		name = sanitizeVolumeName(job.Plan.Model)
	}
	if name == "" {
		name = "volume"
	}
	// Suffixing with the job id keeps two identically-labelled drives apart and
	// guarantees the path is new rather than something already in use.
	mp := filepath.Join(f.mountRoot, name+"-"+shortID(job.Fingerprint))
	clean := filepath.Clean(mp)
	if !strings.HasPrefix(clean, filepath.Clean(f.mountRoot)+"/") {
		return "", fail(ErrCodeVerificationFailed, "refusing to mount outside %s", f.mountRoot)
	}
	return clean, nil
}

// sanitizeVolumeName reduces a label to a safe single path component. It is not
// escaping: anything outside the allowed set is dropped, so no input can
// produce a traversal or a hidden file.
func sanitizeVolumeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}

func shortID(fp string) string {
	sum := sha256.Sum256([]byte(fp))
	return hex.EncodeToString(sum[:4])
}

// doIntegrate makes the new volume a managed storage root.
//
// AddOperatorRoot widens the storage CEILING; it does not entitle anybody. Each
// principal still needs a grant. CasaOS treated "mounted" as "readable by
// everyone", and that assumption is not being reintroduced here.
func (f *Formatter) doIntegrate(ctx context.Context, job *JobRecord, rc *runContext) error {
	mp := job.ResultMountpoint
	if mp == "" {
		return fail(ErrCodeVerificationFailed, "the volume has no validated mountpoint to integrate")
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return fail(ErrCodeToolFailed, "could not create %s: %v", mp, err)
	}
	mounted := false
	var mountErr error
	for _, t := range mountTypes[job.Plan.Filesystem] {
		if _, err := f.runner.Run(ctx, CommandSpec{
			Name: "mount", Timeout: f.stepTimeout,
			Args: []string{"-t", t, "-o", "rw,nosuid,nodev", "--", rc.target, mp},
		}); err == nil {
			mounted = true
			break
		} else {
			mountErr = err
		}
	}
	if !mounted {
		return fail(ErrCodeToolFailed, "the validated filesystem could not be mounted at %s: %v", mp, mountErr)
	}
	if err := f.addStorageRoot(mp, fmt.Sprintf(
		"volume %s formatted %s by %s", job.Plan.DeviceName, job.Plan.Filesystem, job.PrincipalID)); err != nil {
		return fail(ErrCodeToolFailed,
			"the volume was formatted and mounted at %s but could not be registered as a storage root: %v", mp, err)
	}
	return nil
}

func (f *Formatter) finish(job *JobRecord, err error) {
	job.UpdatedAt = f.now()
	job.Terminal = true
	job.PendingStep = ""
	if err == nil {
		job.Stage = StageComplete
		job.Result = "success"
		return
	}
	var fe *FormatError
	if errors.As(err, &fe) {
		job.ErrorCode = string(fe.Code)
		job.ErrorDetail = fe.Detail
		if fe.Code == ErrCodeCancelled {
			job.Stage = StageCancelled
			job.Result = "cancelled"
			return
		}
	} else {
		job.ErrorCode = string(ErrCodeToolFailed)
		job.ErrorDetail = err.Error()
	}
	job.Stage = StageFailed
	job.Result = "failed"
}

// ---------------------------------------------------------------- cancel

// Cancel requests cancellation and reports honestly what that will mean.
//
// It never claims the device will be left untouched when it will not.
func (f *Formatter) Cancel(jobID string) (CancelState, string, error) {
	job, err := f.LoadJob(jobID)
	if err != nil {
		return "", "", err
	}
	if job.Terminal {
		return CancelStateFinished, "the operation has already finished", nil
	}
	f.mu.Lock()
	f.cancelRequested[jobID] = true
	f.mu.Unlock()

	job.CancelRequested = true
	if perr := f.persist(&job); perr != nil {
		return "", "", perr
	}
	state := job.CancelState()
	switch state {
	case CancelStateNonCancellablePhase:
		return state, fmt.Sprintf(
			"%s is running and cannot be interrupted without leaving the device in a worse state; "+
				"the cancellation will be honoured as soon as it finishes", job.PendingStep), nil
	case CancelStateRecovering:
		return state, "the device has already been modified; stopping now will not restore it", nil
	default:
		return state, "the operation will stop before anything else is changed", nil
	}
}

func (f *Formatter) cancelled(jobID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelRequested[jobID]
}

// ---------------------------------------------------------------- persistence

func (f *Formatter) jobsDir() string {
	return filepath.Join(StateDir(), "format-jobs")
}

// persist writes the record and fsyncs it, including the directory entry.
//
// Called BEFORE each step, never after: a record that lands after the step it
// describes is exactly as useless as no record at all.
func (f *Formatter) persist(job *JobRecord) error {
	job.UpdatedAt = f.now()
	sum, err := jobChecksum(*job)
	if err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not seal the job record: %v", err)
	}
	job.Checksum = sum

	dir := f.jobsDir()
	// Autoprovision: no operator ever has to create this by hand.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not create %s: %v", dir, err)
	}
	data, err := json.Marshal(job)
	if err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not encode the job record: %v", err)
	}
	final := filepath.Join(dir, job.JobID+".json")
	tmp := final + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not write job state: %v", err)
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return fail(ErrCodeJobStateCorrupt, "could not write job state: %v", err)
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return fail(ErrCodeJobStateCorrupt, "could not flush job state: %v", err)
	}
	if err := fh.Close(); err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not close job state: %v", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not commit job state: %v", err)
	}
	// The rename itself has to reach the platter, or a power cut can lose the
	// stage transition we just recorded. Losing it is not cosmetic: the whole
	// recovery state machine reads this record back and decides what the disk
	// looks like from it, so an undurable record makes the next diagnosis a
	// guess. If it cannot be made durable, the caller must not take the step
	// this record describes.
	if err := syncDir(dir); err != nil {
		return fail(ErrCodeJobStateCorrupt, "could not make the job record durable: %v", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename inside it survives a power cut.
//
// It is a package variable so tests can prove that a durability failure stops
// the job instead of being swallowed.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	// A close error on a read-only descriptor says nothing about durability;
	// fsync is what decides that, and it has already been observed.
	_ = d.Close()
	return dirSyncResult(err)
}

// dirSyncResult classifies the outcome of fsync on a directory.
//
// Filesystems that do not implement it report EINVAL or EOPNOTSUPP. On those
// there is no stronger ordering this code can ask for, and refusing every
// format because the state directory happens to live on such a filesystem
// would trade a durability weakness for an outage. Tolerating that case is NOT
// a claim that the rename is durable there — only that nothing further is
// available to us. Every other error is a real failure to make the record
// durable and is reported as one.
func dirSyncResult(err error) error {
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return err
}

// jobChecksum covers the record with the checksum field cleared. It is an
// integrity check against truncation and hand-editing, not an authentication
// mechanism, and the code does not claim otherwise.
func jobChecksum(job JobRecord) (string, error) {
	job.Checksum = ""
	job.Corrupt = false
	return hashJSON(job)
}

// LoadJob reads a job record and PROVES its integrity.
//
// A record that does not verify collapses to StageUnknown. It never reports
// COMPLETE on the strength of the file saying so: the file is exactly what a
// crash truncates and what a confused operator edits.
func (f *Formatter) LoadJob(jobID string) (JobRecord, error) {
	if strings.ContainsAny(jobID, "/\\.") || strings.TrimSpace(jobID) == "" {
		return JobRecord{}, fail(ErrCodeInvalidRequest, "invalid job identifier")
	}
	data, err := os.ReadFile(filepath.Join(f.jobsDir(), jobID+".json"))
	if err != nil {
		return JobRecord{}, fail(ErrCodeTokenUnknown, "no such job %s", jobID)
	}
	var job JobRecord
	if err := json.Unmarshal(data, &job); err != nil {
		return JobRecord{JobID: jobID, Stage: StageUnknown, Corrupt: true, Terminal: false,
			Result: "unknown", ErrorCode: string(ErrCodeJobStateCorrupt),
			ErrorDetail: "the job record could not be decoded"}, nil
	}
	want, err := jobChecksum(job)
	if err != nil || want != job.Checksum {
		job.Corrupt = true
		job.Stage = StageUnknown
		job.Terminal = false
		job.Result = "unknown"
		job.ErrorCode = string(ErrCodeJobStateCorrupt)
		job.ErrorDetail = "the job record failed its integrity check; the recorded stage cannot be trusted"
	}
	return job, nil
}

// ListJobs returns every persisted job, newest first.
func (f *Formatter) ListJobs() ([]JobRecord, error) {
	entries, err := os.ReadDir(f.jobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fail(ErrCodeJobStateCorrupt, "could not list jobs: %v", err)
	}
	var out []JobRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		job, err := f.LoadJob(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// ---------------------------------------------------------------- diagnosis

// Diagnosis explains a job that did not finish, in terms of what is actually on
// the disk now.
type Diagnosis struct {
	JobID string `json:"job_id"`
	// Stage is the last stage proven to have completed.
	Stage FormatStage `json:"stage"`
	// InterruptedDuring is the step that was running. Empty if none was.
	InterruptedDuring string `json:"interrupted_during,omitempty"`
	DeviceModified    bool   `json:"device_modified"`
	DevicePresent     bool   `json:"device_present"`
	IdentityMatches   bool   `json:"identity_matches"`
	// Observed is what the hardware says now, independently of the record.
	ObservedPartitions int    `json:"observed_partitions"`
	ObservedFilesystem string `json:"observed_filesystem,omitempty"`
	// Summary is one sentence an operator can act on.
	Summary string `json:"summary"`
	// Resumable reports whether the job can be continued without re-running a
	// destructive phase that already ran.
	Resumable bool        `json:"resumable"`
	Corrupt   bool        `json:"corrupt"`
	Cancel    CancelState `json:"cancel_state"`
}

// Diagnose re-discovers the device and reports what actually happened.
//
// It never says "the disk is as it was" on the strength of the record alone.
func (f *Formatter) Diagnose(ctx context.Context, jobID string) (Diagnosis, error) {
	job, err := f.LoadJob(jobID)
	if err != nil {
		return Diagnosis{}, err
	}
	d := Diagnosis{
		JobID: job.JobID, Stage: job.Stage, InterruptedDuring: job.PendingStep,
		DeviceModified: job.DeviceModified, Corrupt: job.Corrupt, Cancel: job.CancelState(),
	}
	if job.Corrupt {
		d.Summary = "the job record failed its integrity check; the device must be inspected before " +
			"anything else is done to it"
		return d, nil
	}

	inv, err := f.discover()
	if err != nil {
		d.Summary = "the device could not be re-examined because block device discovery failed: " + err.Error()
		return d, nil
	}
	dev, derr := findByIdentity(inv, job.Identity)
	if derr == nil {
		d.DevicePresent, d.IdentityMatches = true, true
		d.ObservedPartitions = len(dev.Children)
		target := dev.Path
		if !job.Plan.IsPartition && len(dev.Children) == 1 {
			if child, ok := inv.ByName(dev.Children[0]); ok {
				target = child.Path
			}
		}
		if probe, perr := f.probe(ctx, target); perr == nil {
			d.ObservedFilesystem = probe["TYPE"]
		}
	} else if occupant, ok := inv.ByName(job.Plan.DeviceName); ok {
		// The kernel name is display-only, and it is used here for exactly that:
		// telling an operator "something else is in that slot now" rather than
		// "your disk vanished". Nothing binds to it.
		d.DevicePresent = true
		d.IdentityMatches = false
		d.ObservedPartitions = len(occupant.Children)
	}

	switch {
	case job.Terminal && job.Result == "success":
		d.Summary = fmt.Sprintf("completed: %s filesystem %s mounted at %s",
			job.ResultFilesystem, job.ResultUUID, job.ResultMountpoint)
	case job.Terminal:
		d.Summary = fmt.Sprintf("%s at stage %s: %s", job.Result, job.Stage, job.ErrorDetail)
	case !d.DevicePresent:
		d.Summary = fmt.Sprintf("interrupted at %s while %s; the device is no longer attached, so its "+
			"state cannot be confirmed", job.Stage, orNone(job.PendingStep))
	case !d.IdentityMatches:
		d.Summary = fmt.Sprintf("interrupted at %s; a DIFFERENT device now occupies that slot, so this "+
			"job must not be resumed", job.Stage)
	default:
		d.Summary = interruptedSummary(job, d)
		d.Resumable = resumableFrom(job.Stage) && d.IdentityMatches
	}
	return d, nil
}

func interruptedSummary(job JobRecord, d Diagnosis) string {
	base := fmt.Sprintf("interrupted after %s while %s", job.Stage, orNone(job.PendingStep))
	switch job.Stage {
	case StageDiscovered, StageValidated:
		return base + "; nothing on the device was changed"
	case StageQuiesced:
		return base + "; the volume was unmounted but no data was changed"
	case StageSignaturesCleared:
		return base + "; filesystem signatures were removed, so the previous filesystem is no longer " +
			"detectable, and no new partition table was written"
	case StagePartitioned, StagePartitionVisible:
		return base + fmt.Sprintf("; a new partition table was written (%d partition(s) visible) but NO "+
			"filesystem was created", d.ObservedPartitions)
	case StageFilesystemCreated:
		return base + fmt.Sprintf("; a %s filesystem was created but never validated, so it is not safe "+
			"to use yet", d.ObservedFilesystem)
	case StageVerified:
		return base + "; the filesystem is valid but was not registered as managed storage"
	case StageIntegrated:
		return base + "; the volume is usable but the job never recorded completion"
	default:
		return base
	}
}

func orNone(s string) string {
	if s == "" {
		return "no step"
	}
	return s
}

// resumableFrom says which stages may be continued.
//
// Anything before the first destructive step is NOT resumable: there is nothing
// to salvage and a fresh prepare re-proves every safety property against the
// device as it is now, which is strictly better than continuing from a stale
// plan.
func resumableFrom(s FormatStage) bool {
	switch s {
	case StageSignaturesCleared, StagePartitioned, StagePartitionVisible, StageFilesystemCreated, StageVerified, StageIntegrated:
		return true
	default:
		return false
	}
}

// Resume continues an interrupted job from where it stopped.
//
// It re-proves identity, protection, mounts and holders first, and it never
// re-runs a destructive phase that already completed: the step list is sliced
// at the recorded stage. A record that fails its integrity check is refused
// outright.
func (f *Formatter) Resume(ctx context.Context, p storageauth.Principal, jobID string) (JobRecord, error) {
	if err := requireFormatAuthority(p); err != nil {
		return JobRecord{}, err
	}
	job, err := f.LoadJob(jobID)
	if err != nil {
		return JobRecord{}, err
	}
	if job.Corrupt {
		return job, fail(ErrCodeJobStateCorrupt,
			"job %s failed its integrity check and will not be resumed", jobID)
	}
	if job.Terminal {
		return job, fail(ErrCodeInvalidRequest, "job %s already finished (%s)", jobID, job.Result)
	}
	if !resumableFrom(job.Stage) {
		return job, fail(ErrCodeInvalidRequest,
			"job %s stopped at %s; prepare a new operation instead of resuming", jobID, job.Stage)
	}
	// The job is being picked back up, so it is no longer merely interrupted.
	// Clearing it here — before any step runs — means a second crash during
	// the resume is diagnosed as a fresh interruption of the resumed work
	// rather than as an untouched leftover.
	if job.Interrupted {
		job.Interrupted = false
		job.Result = ""
		job.ErrorCode = ""
		job.ErrorDetail = ""
		if perr := f.persist(&job); perr != nil {
			return job, perr
		}
	}
	steps := planSteps(job.Plan.IsPartition)
	remaining := steps[:0:0]
	for _, s := range steps {
		if stageOrder[stepStage[s]] > stageOrder[job.Stage] {
			remaining = append(remaining, s)
		}
	}
	if len(remaining) == 0 {
		return job, fail(ErrCodeInvalidRequest, "job %s has no remaining steps", jobID)
	}
	// The plan's step list is not persisted (unexported); rebuild it so the
	// resumed run walks the same sequence.
	job.Plan.steps = steps

	if err := AcquireDevice(job.LockFingerprint, job.JobID); err != nil {
		return job, fail(ErrCodeDeviceLocked, "%v", err)
	}
	defer ReleaseDevice(job.LockFingerprint, job.JobID)

	// The plan's topology hash describes the device BEFORE the format started;
	// it cannot match now. Identity, protection and mount safety are re-proven
	// instead, which is what actually protects the wrong disk from being hit.
	inv, dev, err := f.locateTarget(job.Plan.Identity, job.Plan.Fingerprint)
	if err != nil {
		return job, err
	}
	if err := f.validateTarget(inv, dev); err != nil {
		return job, err
	}
	rc := runContext{inv: inv, dev: dev, target: dev.Path}
	if !job.Plan.IsPartition && len(dev.Children) == 1 {
		if child, ok := inv.ByName(dev.Children[0]); ok {
			rc.target = child.Path
		}
	}

	var runErr error
	for i, step := range remaining {
		if err := f.step(ctx, &job, &rc, step); err != nil {
			runErr = err
			break
		}
		job.Stage = stepStage[step]
		if i+1 < len(remaining) {
			job.PendingStep = string(remaining[i+1])
		} else {
			job.PendingStep = "finalize"
		}
		if err := f.persist(&job); err != nil {
			runErr = err
			break
		}
	}
	f.finish(&job, runErr)
	_ = f.auditFn(AuditEntry{
		PrincipalID: p.ID, Operation: "format.resume", Fingerprint: job.Fingerprint,
		Model: job.Plan.Model, Serial: job.Plan.Serial, SizeBytes: job.Plan.SizeBytes,
		Result: job.Result, Stage: string(job.Stage), Detail: job.ErrorDetail,
	})
	if perr := f.persist(&job); perr != nil && runErr == nil {
		runErr = perr
	}
	return job, runErr
}

// RecoverInterruptedJobs is the automatic recovery pass, run at startup.
//
// Every job that was mid-flight when the process died is marked INTERRUPTED
// with a diagnosis attached, so nothing is left claiming to be running and no
// operator has to go and tidy up state by hand.
func (f *Formatter) RecoverInterruptedJobs(ctx context.Context) ([]Diagnosis, error) {
	jobs, err := f.ListJobs()
	if err != nil {
		return nil, err
	}
	var out []Diagnosis
	for _, job := range jobs {
		if job.Terminal || job.Interrupted || job.Corrupt {
			if job.Corrupt {
				d, derr := f.Diagnose(ctx, job.JobID)
				if derr == nil {
					out = append(out, d)
				}
			}
			continue
		}
		d, derr := f.Diagnose(ctx, job.JobID)
		if derr != nil {
			continue
		}
		job.Interrupted = true
		job.Result = "interrupted"
		job.ErrorCode = string(ErrCodeInterrupted)
		job.ErrorDetail = d.Summary
		// The ordinary Stage field keeps the last PROVEN stage; the terminal
		// outcome is carried separately so the diagnosis stays precise.
		if perr := f.persist(&job); perr != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// ---------------------------------------------------------------- health

// FormatHealth is machine-readable health for the diagnostics endpoint. It
// names the failed invariant rather than reporting a boolean.
type FormatHealth struct {
	Status string `json:"status"` // healthy | degraded | unavailable
	// Invariant names what is broken, in words an operator can act on.
	Invariant string `json:"invariant,omitempty"`
	Detail    string `json:"detail,omitempty"`

	StateDirWritable  bool     `json:"state_dir_writable"`
	IntegrityEnforced bool     `json:"integrity_enforced"`
	CanPartition      bool     `json:"can_partition"`
	CanWipeSignatures bool     `json:"can_wipe_signatures"`
	CanIdentify       bool     `json:"can_identify"`
	Filesystems       []string `json:"available_filesystems"`
	MissingHelpers    []string `json:"missing_helpers,omitempty"`

	ActiveJobs      int      `json:"active_jobs"`
	InterruptedJobs []string `json:"interrupted_jobs,omitempty"`
	CorruptJobs     []string `json:"corrupt_jobs,omitempty"`
	HeldDevices     int      `json:"held_devices"`
}

// Health autoprovisions the state directory and validates the PROPERTY that
// makes job state trustworthy — that a tampered record is rejected — rather
// than merely checking the directory exists.
func (f *Formatter) Health(ctx context.Context) FormatHealth {
	h := FormatHealth{Status: "healthy"}
	caps := f.capabilities(ctx)
	h.CanPartition = caps.CanPartition
	h.CanWipeSignatures = caps.CanWipeSignatures
	h.CanIdentify = caps.CanIdentify
	for _, fs := range caps.Filesystems {
		if fs.Available {
			h.Filesystems = append(h.Filesystems, string(fs.Filesystem))
		}
	}
	for _, t := range caps.Tools {
		if !t.Available {
			h.MissingHelpers = append(h.MissingHelpers, string(t.Tool))
		}
	}
	for _, helper := range []string{"mount", "umount"} {
		if _, err := lookupHelper(helper); err != nil {
			h.MissingHelpers = append(h.MissingHelpers, helper)
		}
	}

	dir := f.jobsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.Status = "unavailable"
		h.Invariant = "job state is durable"
		h.Detail = "cannot create " + dir + ": " + err.Error()
		return h
	}
	probe := filepath.Join(dir, ".writable-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		h.Status = "unavailable"
		h.Invariant = "job state is durable"
		h.Detail = "cannot write to " + dir + ": " + err.Error()
		return h
	}
	_ = os.Remove(probe)
	h.StateDirWritable = true

	// Property, not existence: a record whose checksum does not match must be
	// rejected. If that stops working, a corrupt record could claim COMPLETE.
	tampered := JobRecord{JobID: "probe", Stage: StageComplete, Terminal: true, Result: "success"}
	sum, err := jobChecksum(tampered)
	if err == nil {
		tampered.Checksum = sum
		tampered.Stage = StagePartitioned // edited after sealing
		got, gerr := jobChecksum(tampered)
		h.IntegrityEnforced = gerr == nil && got != tampered.Checksum
	}
	if !h.IntegrityEnforced {
		h.Status = "degraded"
		h.Invariant = "a tampered job record is rejected"
		h.Detail = "the job record integrity check does not detect modification"
	}

	jobs, err := f.ListJobs()
	if err != nil {
		h.Status = "degraded"
		h.Invariant = "job state is readable"
		h.Detail = err.Error()
		return h
	}
	for _, j := range jobs {
		switch {
		case j.Corrupt:
			h.CorruptJobs = append(h.CorruptJobs, j.JobID)
		case j.Interrupted || (j.Terminal && j.Result == "interrupted"):
			// Checked BEFORE the active branch: an interrupted job is not
			// running, and counting it as active would make health report
			// phantom work forever.
			h.InterruptedJobs = append(h.InterruptedJobs, j.JobID)
		case !j.Terminal:
			h.ActiveJobs++
		}
	}
	if len(h.CorruptJobs) > 0 && h.Status == "healthy" {
		h.Status = "degraded"
		h.Invariant = "every job record verifies"
		h.Detail = fmt.Sprintf("%d job record(s) failed their integrity check", len(h.CorruptJobs))
	}
	if !caps.CanWipeSignatures || !caps.CanIdentify {
		h.Status = "degraded"
		h.Invariant = "a format can be prepared and validated"
		h.Detail = "wipefs and blkid are both required; formatting is refused without them"
	}
	h.HeldDevices = len(HeldDevices())
	return h
}

// ---------------------------------------------------------------- helpers

func defaultIsMountpoint(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	pi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	st, ok1 := fi.Sys().(*syscall.Stat_t)
	pst, ok2 := pi.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		// FAIL-CLOSED: if we cannot tell whether it is a mountpoint, we do not
		// get to claim the validation writes landed on the new filesystem.
		return false, errors.New("mountpoint detection is unavailable on this platform")
	}
	return st.Dev != pst.Dev, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
