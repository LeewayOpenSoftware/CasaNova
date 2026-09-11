package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagehealth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagemaint"
	"github.com/labstack/echo/v4"
)

// HTTP surface of the destructive storage-maintenance subsystem: the external
// formatter and free-space sanitization.
//
// # THE API TAKES AN IDENTITY, NEVER A PATH
//
// /dev/sdb is a slot, not a device. Unplug a drive, plug in another, and the
// kernel reuses the name. So no request body here carries a device path, and
// none is accepted: bodies are decoded with DisallowUnknownFields precisely so
// that a client sending {"device":{"path":"/dev/sda"}} is REFUSED rather than
// silently having its path ignored while some other field selects a disk. What
// the client sends is a stable identity — fingerprint, serial or WWN — and this
// layer resolves it to exactly one physical device or refuses.
//
// # THE ROUTE LAYER IS A FILTER, NOT THE LAST WORD
//
// Everything below runs BEFORE the engine and is repeated by the engine
// immediately before it touches anything. Two independent refusals are the
// point: a bug in either one still leaves the other standing. This layer never
// decides that an operation is safe — it only decides that it is not obviously
// unsafe, and hands on a target the engine re-derives from scratch.
//
// # WHOLE DEVICES ONLY
//
// A partition inherits its disk's serial and WWN (pkg/blockdev, readPartition),
// so a disk and every partition on it share one fingerprint. That is correct —
// one physical device, one identity, one lock — but it means a partition cannot
// be addressed unambiguously by identity. Rather than invent a second
// addressing scheme for destructive work, this API resolves ONLY whole devices.
// Formatting a disk rewrites its partition table; that is the operation on
// offer, and it is the one the protection classifier reasons about.
//
// # NO force=true
//
// There is no override parameter anywhere in this file, and adding one would
// defeat every guard in it. A refusal carries a stable `reason` code and a
// message an operator can act on; the fix is to resolve the stated cause.
//
// # RESPONSE SHAPE
//
// Flat objects, no model.Result envelope — the same exception GET
// /v1/sys/diagnosis and the maintenance module already make. A refusal answers
// {"reason": "<stable code>", "message": "<what to do>"} plus whatever context
// the specific refusal has, because the reason is the part the caller needs and
// an envelope buries it. The 403 from the shared RBAC gate keeps its own
// envelope: that is the gate's wire format across the whole service.

// ---------------------------------------------------------------- engine seam

// StorageOpKind distinguishes the two destructive operations. It is carried on
// the plan so a sanitize plan cannot be executed through the format endpoint.
type StorageOpKind string

const (
	StorageOpFormat   StorageOpKind = "format"
	StorageOpSanitize StorageOpKind = "sanitize"
)

// StorageSanitizeMethod is what a sanitize actually does. Each one has a
// different, honestly stated reach — see storageSanitizeGuarantee.
type StorageSanitizeMethod string

// The vocabulary is EXACTLY what pkg/storagemaint implements — every method
// here operates on the free space of a mounted filesystem.
//
// It used to also offer "discard" and "overwrite", described as acting on
// "every block the drive exposes". No whole-device mechanism exists in this
// tree, so those were guarantees with no implementation behind them: a user
// selecting "overwrite" to wipe a drive before selling it would have been told
// something happened that never could. A method the engine cannot perform is
// not offered, and the guarantee text is generated from the engine's own
// semantics table rather than restated here, so the two cannot drift.
const (
	// SanitizeTrim asks the filesystem to discard its unused blocks.
	SanitizeTrim StorageSanitizeMethod = StorageSanitizeMethod(storagemaint.SanitizeTrim)
	// SanitizeZeroFill overwrites allocatable free space once with zeros.
	SanitizeZeroFill StorageSanitizeMethod = StorageSanitizeMethod(storagemaint.SanitizeZeroFill)
	// SanitizeMultiPass overwrites allocatable free space several times.
	SanitizeMultiPass StorageSanitizeMethod = StorageSanitizeMethod(storagemaint.SanitizeMultiPass)
)

// storageSanitizeMethodImplemented reports whether the engine can perform a
// method. The API refuses anything else rather than accepting a name it cannot
// honour.
func storageSanitizeMethodImplemented(m StorageSanitizeMethod) bool {
	for _, impl := range storagemaint.ImplementedSanitizeMethods() {
		if string(impl) == string(m) {
			return true
		}
	}
	return false
}

// storageSupportedSanitizeMethods is what a refusal lists as the alternatives.
func storageSupportedSanitizeMethods() []string {
	var out []string
	for _, m := range storagemaint.ImplementedSanitizeMethods() {
		out = append(out, string(m))
	}
	return out
}

// StoragePrincipal is who asked. Carried into the engine so the audit record
// names a person rather than "the API".
type StoragePrincipal struct {
	UserID int    `json:"user_id"`
	Role   string `json:"role"`
}

// ID is the audit-friendly form.
func (p StoragePrincipal) ID() string { return strconv.Itoa(p.UserID) }

// StorageTarget is a device this layer has already resolved to exactly one
// physical device and proven eligible. The engine receives this rather than
// anything from the request, and re-derives it all anyway.
type StorageTarget struct {
	Fingerprint     string                      `json:"fingerprint"`
	Identity        blockdev.DeviceIdentity     `json:"identity"`
	Name            string                      `json:"name"` // display only
	SizeBytes       uint64                      `json:"size_bytes"`
	Transport       blockdev.Transport          `json:"transport"`
	SupportsDiscard bool                        `json:"supports_discard"`
	Mountpoints     []string                    `json:"mountpoints,omitempty"`
	Children        []string                    `json:"children,omitempty"`
	Reasons         []blockdev.ProtectionReason `json:"protection_reasons,omitempty"`
}

// StorageFormatRequest is a validated format, ready for the engine.
type StorageFormatRequest struct {
	Target     StorageTarget           `json:"target"`
	Filesystem storagemaint.Filesystem `json:"filesystem"`
	Label      string                  `json:"label,omitempty"`
	Principal  StoragePrincipal        `json:"principal"`
}

// StorageSanitizeRequest is a validated sanitize, ready for the engine.
type StorageSanitizeRequest struct {
	Target    StorageTarget         `json:"target"`
	Method    StorageSanitizeMethod `json:"method"`
	Principal StoragePrincipal      `json:"principal"`
}

// StoragePlan is a prepared, not-yet-executed operation.
type StoragePlan struct {
	ID          string                  `json:"id"`
	Kind        StorageOpKind           `json:"kind"`
	Fingerprint string                  `json:"device_fingerprint"`
	Identity    blockdev.DeviceIdentity `json:"identity"`
	DeviceName  string                  `json:"device_name"`
	SizeBytes   uint64                  `json:"size_bytes"`

	Filesystem storagemaint.Filesystem `json:"filesystem,omitempty"`
	Label      string                  `json:"label,omitempty"`
	Method     StorageSanitizeMethod   `json:"method,omitempty"`

	Steps    []string `json:"steps,omitempty"`
	Warnings []string `json:"warnings,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// StorageJob is a running, finished or interrupted operation.
type StorageJob struct {
	ID          string        `json:"id"`
	Kind        StorageOpKind `json:"kind"`
	Fingerprint string        `json:"device_fingerprint"`
	DeviceName  string        `json:"device_name,omitempty"`
	State       string        `json:"state"`
	Stage       string        `json:"stage,omitempty"`
	Percent     int           `json:"percent"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  time.Time     `json:"finished_at,omitempty"`
	Error       string        `json:"error,omitempty"`
	// Interrupted marks a job that was running when the service stopped. It is
	// recovered from persisted state, not from memory, which is the only way
	// a power cut mid-format is diagnosable afterwards.
	Interrupted bool `json:"interrupted"`
}

// StorageMaintenanceEngine is the MINIMUM surface this HTTP layer needs from
// pkg/storagemaint's format and sanitize implementations. It is declared here,
// against this layer's needs, because those implementations are being written
// in parallel; whichever type provides them satisfies this interface with a
// thin adapter and is installed with SetStorageMaintenanceEngine at startup.
//
// Contract the implementation MUST honour — this layer cannot enforce it and
// does not pretend to:
//
//   - Prepare* receives a target this layer already resolved and validated. It
//     must NOT trust it. Re-discover, re-compare identity, re-check protection.
//   - Execute must re-discover and re-validate IMMEDIATELY before the first
//     destructive syscall or helper invocation, and abort if anything moved.
//   - Execute must hold storagemaint.AcquireDevice for the device's fingerprint
//     for the whole run, and release it afterwards.
//   - Every helper runs through exec.CommandContext with structured arguments
//     and a "--" separator before any derived value. No shell, ever.
//   - Every outcome, including refusals, goes to storagemaint.Audit.
//
// Until an implementation is installed, every operation endpoint answers 503
// with reason ENGINE_UNAVAILABLE. Failing closed on a missing engine is the
// only correct default for a subsystem whose job is destroying disks.
type StorageMaintenanceEngine interface {
	PrepareFormat(ctx context.Context, req StorageFormatRequest) (StoragePlan, error)
	PrepareSanitize(ctx context.Context, req StorageSanitizeRequest) (StoragePlan, error)
	// Plan returns a previously prepared plan. Used to re-validate before
	// execution; returns false when the id is unknown or the plan lapsed.
	Plan(ctx context.Context, planID string) (StoragePlan, bool)
	Execute(ctx context.Context, planID string, kind StorageOpKind, p StoragePrincipal) (StorageJob, error)
	Jobs(ctx context.Context) ([]StorageJob, error)
	Cancel(ctx context.Context, jobID string, p StoragePrincipal) error
}

var (
	storageEngineMu sync.RWMutex
	storageEngine   StorageMaintenanceEngine
)

// SetStorageMaintenanceEngine installs the implementation. Called once at
// startup; passing nil deliberately disables every destructive endpoint.
func SetStorageMaintenanceEngine(e StorageMaintenanceEngine) {
	storageEngineMu.Lock()
	defer storageEngineMu.Unlock()
	storageEngine = e
}

func currentStorageEngine() StorageMaintenanceEngine {
	storageEngineMu.RLock()
	defer storageEngineMu.RUnlock()
	return storageEngine
}

// ---------------------------------------------------------------- refusals

// Stable refusal codes. These are contract: a UI keys its explanation on them
// and an operator quotes them in a support thread.
const (
	reasonEngineUnavailable  = "ENGINE_UNAVAILABLE"
	reasonMalformedBody      = "MALFORMED_BODY"
	reasonIdentityRequired   = "IDENTITY_REQUIRED"
	reasonIdentityNotFound   = "IDENTITY_NOT_FOUND"
	reasonIdentityAmbiguous  = "IDENTITY_AMBIGUOUS"
	reasonIdentityTooWeak    = "IDENTITY_TOO_WEAK"
	reasonDiscoveryFailed    = "DISCOVERY_FAILED"
	reasonDiscoveryIncomplet = "DISCOVERY_INCOMPLETE"
	reasonDeviceProtected    = "DEVICE_PROTECTED"
	reasonDeviceNotEligible  = "DEVICE_NOT_ELIGIBLE"
	reasonDeviceMounted      = "DEVICE_MOUNTED"
	reasonMountRequired      = "MOUNT_REQUIRED"
	reasonMountpointProtect  = "MOUNTPOINT_PROTECTED"
	reasonUnsupportedFS      = "UNSUPPORTED_FILESYSTEM"
	reasonFilesystemUnavail  = "FILESYSTEM_UNAVAILABLE"
	reasonInvalidLabel       = "INVALID_LABEL"
	reasonUnsupportedMethod  = "UNSUPPORTED_METHOD"
	reasonDiscardUnsupported = "DISCARD_UNSUPPORTED"
	reasonPlanNotFound       = "PLAN_NOT_FOUND"
	reasonPlanKindMismatch   = "PLAN_KIND_MISMATCH"
	reasonPlanExpired        = "PLAN_EXPIRED"
	reasonTargetChanged      = "TARGET_CHANGED"
	reasonSubsystemUnhealthy = "SUBSYSTEM_UNHEALTHY"
	reasonEngineRefused      = "ENGINE_REFUSED"
	reasonJobIDRequired      = "JOB_ID_REQUIRED"
	reasonNotAuthenticated   = "NOT_AUTHENTICATED"
)

// storageRefusal is a machine-readable "no". Carrying the reason separately
// from the message is what lets the UI explain a refusal instead of greying out
// a button with no cause.
type storageRefusal struct {
	status  int
	Reason  string         `json:"reason"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
	Reasons []string       `json:"protection_reasons,omitempty"`
}

func refuse(status int, reason, message string) *storageRefusal {
	return &storageRefusal{status: status, Reason: reason, Message: message}
}

func (r *storageRefusal) with(k string, v any) *storageRefusal {
	if r.Detail == nil {
		r.Detail = map[string]any{}
	}
	r.Detail[k] = v
	return r
}

func (r *storageRefusal) write(ctx echo.Context) error {
	return ctx.JSON(r.status, r)
}

// ---------------------------------------------------------------- gates

// requireStorageReadCaller is the gate for the read-only endpoints.
//
// PermStorageRead, not PermSystemRead: guest holds PermSystemRead, and an
// unauthenticated context in RoleFromContext fails closed to guest.
// The device inventory carries serial numbers, models and the exact
// protection topology of the box — a map of what to attack and what is
// disposable. Guest does not get it.
func requireStorageReadCaller(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	// Second, independent barrier. requirePerm currently returns its refusal
	// as an *echo.HTTPError, which aborts correctly — but this service has
	// already shipped a version where a gate wrote 403 with ctx.JSON, returned
	// the nil that echo hands back on a successful write, and let the handler
	// run anyway (an unauthenticated DELETE /v1/batch answered 403 and still
	// binned the file). If that regression ever returns, a committed response
	// at this point means the gate denied, and this line stops the handler
	// regardless of what the gate returned.
	if ctx.Response().Committed {
		return echo.NewHTTPError(http.StatusForbidden, "permission denied")
	}
	return nil
}

// requireStorageMutationCaller is the gate for everything destructive.
//
// It is deliberately TWO independent checks, because a single one can be wrong
// in a single place:
//
//  1. the RBAC bit — PermStorageFormat or PermStorageSanitize, which only admin
//     holds and which PermStorageWrite does not imply; and
//  2. a real authenticated principal — user id 0 means no token was ever
//     validated. An anonymous caller has no principal, so even if the
//     permission bit were mis-granted tomorrow, there is still nobody to
//     attribute the wipe to and the request is refused.
//
// The second check is not decoration: it is the one that survives a mistake in
// the permission table, and the audit record depends on it being true.
func requireStorageMutationCaller(ctx echo.Context, p rbac.Permission) (StoragePrincipal, error) {
	if err := requirePerm(ctx, p); err != nil {
		return StoragePrincipal{}, err
	}
	if ctx.Response().Committed {
		return StoragePrincipal{}, echo.NewHTTPError(http.StatusForbidden, "permission denied")
	}
	uid := commonmw.UserIDFromContext(ctx)
	if uid == 0 {
		return StoragePrincipal{}, echo.NewHTTPError(http.StatusUnauthorized, map[string]string{
			"reason": reasonNotAuthenticated,
			"message": "Destroying a disk requires a signed-in administrator. This request carried no " +
				"validated session, so there is nobody to attribute the operation to.",
		})
	}
	return StoragePrincipal{UserID: uid, Role: string(commonmw.RoleFromContext(ctx))}, nil
}

// ---------------------------------------------------------------- body shapes

// storageDeviceRef is how a client names a device. There is no path field, and
// DisallowUnknownFields means adding one to the request is a 400 rather than a
// field that gets quietly ignored while something else picks the disk.
type storageDeviceRef struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	Serial      string `json:"serial,omitempty"`
	WWN         string `json:"wwn,omitempty"`
	// SizeBytes is optional and, when present, must match. It is a cheap
	// second opinion against "same model, different disk".
	SizeBytes uint64 `json:"size_bytes,omitempty"`
}

func (r storageDeviceRef) empty() bool {
	return strings.TrimSpace(r.Fingerprint) == "" &&
		strings.TrimSpace(r.Serial) == "" &&
		strings.TrimSpace(r.WWN) == ""
}

type formatPrepareBody struct {
	Device     storageDeviceRef `json:"device"`
	Filesystem string           `json:"filesystem"`
	Label      string           `json:"label,omitempty"`
}

type sanitizePrepareBody struct {
	Device storageDeviceRef `json:"device"`
	Method string           `json:"method"`
}

// storageMaxBodyBytes bounds a request body. These bodies are a few hundred
// bytes; the cap exists so a hostile client cannot make the service allocate.
const storageMaxBodyBytes = 64 << 10

// decodeStorageBody decodes strictly.
//
// DisallowUnknownFields is load-bearing, not tidiness. It is what turns
// {"device":{"path":"/dev/sda"}} into an explicit refusal. Without it the path
// would be dropped on the floor and the request would proceed against whatever
// the remaining fields resolved to — the exact "the API took a path" confusion
// this subsystem is built to avoid.
func decodeStorageBody(ctx echo.Context, dst any) *storageRefusal {
	body := http.MaxBytesReader(ctx.Response(), ctx.Request().Body, storageMaxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		msg := "The request body could not be read. This API selects a device by stable identity " +
			"(fingerprint, serial or WWN) and accepts no other fields — in particular it does not " +
			"accept a device path, because a path names a slot rather than a disk."
		return refuse(http.StatusBadRequest, reasonMalformedBody, msg).with("error", err.Error())
	}
	// A second JSON document in the same body would mean the first one was not
	// the whole request.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return refuse(http.StatusBadRequest, reasonMalformedBody,
			"The request body contained more than one JSON document.")
	}
	return nil
}

// ---------------------------------------------------------------- resolution

// resolveStorageTarget maps a client-supplied identity to exactly one physical
// device, or refuses with a reason.
//
// Every step is a way to say no:
//
//   - discovery must succeed, and must be COMPLETE. Incompleteness is refused
//     globally here, not per device: an unreadable mount table makes every
//     device look unmounted, which is the one condition under which the root
//     disk could pass the protection classifier. blockdev flags the inventory;
//     this is the layer that acts on the flag.
//   - all supplied fields must match (AND, never OR), so a client that sends a
//     serial and a size cannot have the size ignored.
//   - exactly one WHOLE device must match. Zero is a 404; two or more is a
//     refusal, never a "pick the first".
//   - the identity must not be weak, the device must not be protected, and it
//     must be eligible.
func resolveStorageTarget(ref storageDeviceRef) (blockdev.Device, blockdev.Inventory, *storageRefusal) {
	var zero blockdev.Device

	if ref.empty() {
		return zero, blockdev.Inventory{}, refuse(http.StatusBadRequest, reasonIdentityRequired,
			"Name the device by stable identity: fingerprint, serial or WWN. A device path is not "+
				"accepted — /dev/sdb is a slot that the kernel reuses, so an operation prepared "+
				"against a path can land on a different disk.")
	}

	inv, err := blockdev.Discover()
	if err != nil {
		return zero, inv, refuse(http.StatusServiceUnavailable, reasonDiscoveryFailed,
			"The block devices of this host could not be enumerated, so no device can be identified. "+
				"Nothing was touched.").with("error", err.Error())
	}
	if !inv.Complete {
		return zero, inv, refuse(http.StatusConflict, reasonDiscoveryIncomplet,
			"Some device facts could not be read, so CasaNova cannot prove which disks are in use. "+
				"Destructive operations are refused until discovery is complete — an unreadable mount "+
				"table in particular would make a mounted system disk look free.").
			with("incomplete", inv.Incomplete)
	}

	var matches []blockdev.Device
	for _, d := range inv.Devices {
		// Whole devices only: a partition shares its disk's fingerprint, so
		// "which one did you mean" has no answer. See the file header.
		if d.IsPartition {
			continue
		}
		if storageRefMatches(ref, d) {
			matches = append(matches, d)
		}
	}

	switch len(matches) {
	case 0:
		return zero, inv, refuse(http.StatusNotFound, reasonIdentityNotFound,
			"No connected device matches that identity. If the drive was unplugged and plugged back "+
				"in, list the devices again and use the identity from the fresh list.")
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return zero, inv, refuse(http.StatusConflict, reasonIdentityAmbiguous,
			"That identity matches more than one connected device, so CasaNova cannot tell which one "+
				"you mean. It will not guess. Supply the fingerprint from the device list, which is "+
				"unique per physical device.").with("matched", names)
	}

	dev := matches[0]

	if dev.Identity.Strength() == blockdev.StrengthWeak {
		return zero, inv, refuse(http.StatusConflict, reasonIdentityTooWeak,
			"This device does not report enough information (no serial, no WWN, no usable model and "+
				"size) to be told apart from another one. A device CasaNova cannot identify is never "+
				"a destructive target.").with("device", dev.Name)
	}
	if dev.Protected {
		r := refuse(http.StatusConflict, reasonDeviceProtected,
			"This device is protected: it is part of what keeps this system running, or CasaNova "+
				"could not prove that it is not. "+storageProtectionAdvice(dev.ProtectionReasons))
		for _, pr := range dev.ProtectionReasons {
			r.Reasons = append(r.Reasons, string(pr))
		}
		return zero, inv, r.with("device", dev.Name)
	}
	if !dev.Eligible() {
		return zero, inv, refuse(http.StatusConflict, reasonDeviceNotEligible,
			storageIneligibilityReason(dev)).with("device", dev.Name)
	}

	return dev, inv, nil
}

// storageRefMatches is an AND over every field the client supplied.
func storageRefMatches(ref storageDeviceRef, d blockdev.Device) bool {
	if fp := strings.TrimSpace(ref.Fingerprint); fp != "" {
		// Exact, not case-insensitive: Fingerprint() already lower-cases the
		// serial and WWN forms, and the topological form embeds a sysfs path
		// where case is significant.
		if d.Identity.Fingerprint() != fp {
			return false
		}
	}
	if s := strings.TrimSpace(ref.Serial); s != "" {
		if strings.TrimSpace(d.Identity.Serial) == "" || !strings.EqualFold(strings.TrimSpace(d.Identity.Serial), s) {
			return false
		}
	}
	if w := strings.TrimSpace(ref.WWN); w != "" {
		if strings.TrimSpace(d.Identity.WWN) == "" || !strings.EqualFold(strings.TrimSpace(d.Identity.WWN), w) {
			return false
		}
	}
	if ref.SizeBytes != 0 && ref.SizeBytes != d.SizeBytes {
		return false
	}
	return true
}

func storageProtectionAdvice(rs []blockdev.ProtectionReason) string {
	for _, r := range rs {
		switch r {
		case blockdev.ReasonRootFilesystem, blockdev.ReasonBootFilesystem:
			return "It carries this host's operating system."
		case blockdev.ReasonCasaNovaState, blockdev.ReasonManagedStorage:
			return "It holds CasaNova's own data."
		case blockdev.ReasonActiveSwap:
			return "It is in use as swap; turn swap off on it first if that is really intended."
		case blockdev.ReasonHasHolders:
			return "It is a member of an LVM volume group, a RAID array or an encrypted volume. " +
				"Dismantle that layer first."
		case blockdev.ReasonChildProtected:
			return "One of its partitions is protected, and rewriting the partition table would " +
				"destroy that partition just as thoroughly."
		case blockdev.ReasonParentProtected:
			return "The disk it belongs to is protected."
		case blockdev.ReasonDiscoveryFailed:
			return "Some of its attributes could not be read, and an unknown device is treated as " +
				"protected rather than as free."
		case blockdev.ReasonUnidentifiable:
			return "It cannot be identified reliably enough to be a destructive target."
		}
	}
	return "Its protection reasons are listed above."
}

func storageIneligibilityReason(d blockdev.Device) string {
	switch {
	case d.ReadOnly:
		return "This device is read-only. A write-protect switch or a read-only controller has to be " +
			"cleared before anything can be written to it."
	case d.SizeBytes == 0:
		return "This device reports a size of zero — typically an empty card reader. There is nothing " +
			"to operate on."
	case !d.External():
		return "This device is attached as internal storage. CasaNova's storage-maintenance tools " +
			"operate on external and removable drives only; wiping an internal disk is not offered here."
	default:
		return "This device is not an eligible target for a destructive operation."
	}
}

// storageTargetOf packages a resolved device for the engine.
func storageTargetOf(inv blockdev.Inventory, d blockdev.Device) StorageTarget {
	return StorageTarget{
		Fingerprint:     d.Identity.Fingerprint(),
		Identity:        d.Identity,
		Name:            d.Name,
		SizeBytes:       d.SizeBytes,
		Transport:       d.Transport,
		SupportsDiscard: d.SupportsDiscard,
		Mountpoints:     storageTreeMountpoints(inv, d),
		Children:        d.Children,
		Reasons:         d.ProtectionReasons,
	}
}

// storageTreeMountpoints collects the mountpoints of a disk AND of every
// partition on it. Checking only the disk's own mountpoints would miss the
// normal case entirely: a USB drive is mounted through its partition, not
// through /dev/sdb.
func storageTreeMountpoints(inv blockdev.Inventory, d blockdev.Device) []string {
	out := append([]string{}, d.Mountpoints...)
	for _, childName := range d.Children {
		if c, ok := inv.ByName(childName); ok {
			out = append(out, c.Mountpoints...)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- op checks

// validateFormatTarget re-runs every format-specific precondition. It is called
// at prepare AND again at execute against a freshly discovered device, so a
// drive that got mounted in between is caught rather than formatted.
func validateFormatTarget(inv blockdev.Inventory, d blockdev.Device, caps storagemaint.Capabilities,
	fs storagemaint.Filesystem, label string) *storageRefusal {

	if mps := storageTreeMountpoints(inv, d); len(mps) > 0 {
		return refuse(http.StatusConflict, reasonDeviceMounted,
			"This drive is mounted. CasaNova will not format a filesystem that is in use — a program "+
				"writing to it during a format produces corruption that looks like a hardware fault. "+
				"Eject the drive in the Files view first.").with("mountpoints", mps)
	}
	if len(d.Holders) > 0 {
		return refuse(http.StatusConflict, reasonDeviceProtected,
			"This device is claimed by LVM, RAID or an encrypted volume. Dismantle that layer first.").
			with("holders", d.Holders)
	}

	valid := false
	for _, s := range storagemaint.SupportedFilesystems {
		if s == fs {
			valid = true
			break
		}
	}
	if !valid {
		names := make([]string, 0, len(storagemaint.SupportedFilesystems))
		for _, s := range storagemaint.SupportedFilesystems {
			names = append(names, string(s))
		}
		return refuse(http.StatusBadRequest, reasonUnsupportedFS,
			"That filesystem is not one CasaNova can create.").with("supported", names)
	}
	if err := storagemaint.ValidateFilesystemForDevice(caps, fs, d.SizeBytes); err != nil {
		return refuse(http.StatusConflict, reasonFilesystemUnavail, err.Error())
	}
	if err := storagemaint.ValidateLabel(fs, label); err != nil {
		return refuse(http.StatusBadRequest, reasonInvalidLabel, err.Error())
	}
	return nil
}

// validateSanitizeTarget re-runs every sanitize-specific precondition. The
// mount requirement is inverted per method, which is exactly why this cannot be
// one shared check: a whole-device discard must NOT be mounted, and a free-space
// pass can only work on something that IS.
func validateSanitizeTarget(inv blockdev.Inventory, d blockdev.Device,
	caps storagemaint.Capabilities, method StorageSanitizeMethod) *storageRefusal {

	if len(d.Holders) > 0 {
		return refuse(http.StatusConflict, reasonDeviceProtected,
			"This device is claimed by LVM, RAID or an encrypted volume. Dismantle that layer first.").
			with("holders", d.Holders)
	}
	mps := storageTreeMountpoints(inv, d)

	if !storageSanitizeMethodImplemented(method) {
		return refuse(http.StatusBadRequest, reasonUnsupportedMethod,
			"That sanitize method is not available.").with("supported", storageSupportedSanitizeMethods())
	}

	switch method {
	case SanitizeTrim:
		if !d.SupportsDiscard {
			return refuse(http.StatusConflict, reasonDiscardUnsupported,
				"This drive does not advertise discard support, so a TRIM pass would do nothing. "+
					"Use zero-fill instead.")
		}
		if !caps.CanDiscard {
			return refuse(http.StatusConflict, reasonDiscardUnsupported,
				"Neither blkdiscard nor fstrim is installed on this host, so discard cannot be "+
					"issued. Install util-linux, or use zero-fill.")
		}
	case SanitizeMultiPass:
		// Multi-pass overwriting has no established meaning on flash: the
		// controller decides where each write lands, so repeating it does not
		// repeatedly overwrite the same physical cells. Offering it for an SSD
		// would sell a stronger guarantee for nothing but wear.
		if !d.Rotational {
			return refuse(http.StatusBadRequest, reasonUnsupportedMethod,
				"Multi-pass overwriting is only meaningful on a spinning disk. On flash the "+
					"controller decides where each pass lands, so extra passes add wear without "+
					"adding certainty. Use TRIM, or a single zero-fill pass.")
		}
	}

	// Every implemented method works THROUGH a mounted filesystem: they act on
	// its free space, not on the raw device.
	{
		if len(mps) == 0 {
			return refuse(http.StatusConflict, reasonMountRequired,
				"Overwriting free space works through the mounted filesystem, so the drive has to be "+
					"mounted. Mount it in the Files view first.")
		}
		if len(mps) > 1 {
			return refuse(http.StatusConflict, reasonMountRequired,
				"This drive is mounted at more than one location, so CasaNova cannot tell which "+
					"filesystem's free space you mean.").with("mountpoints", mps)
		}
		// A mounted external drive should never be serving a system path, but
		// the check is cheap and the consequence of being wrong is filling the
		// root filesystem to capacity.
		if storageIsSystemMount(mps[0]) {
			return refuse(http.StatusConflict, reasonMountpointProtect,
				"That drive is mounted at a system location. Filling its free space would take the "+
					"system offline.").with("mountpoint", mps[0])
		}
	}
	return nil
}

// storageIsSystemMount mirrors the protected-mount list in pkg/blockdev. It is
// a second opinion rather than a substitute: blockdev's classifier is what
// makes the device protected in the first place, and this only guards the
// free-space method's own extra requirement that a mountpoint be user storage.
func storageIsSystemMount(mp string) bool {
	for _, p := range []string{"/", "/boot", "/efi", "/usr", "/var", "/etc", "/home"} {
		if mp == p {
			return true
		}
		if p != "/" && strings.HasPrefix(mp, p+"/") {
			return true
		}
	}
	return false
}

// storageSanitizeGuarantee states exactly what a method does and, more
// importantly, what it does NOT. A UI string promising more than the code
// delivers is a defect, so the honest text is generated by the backend and
// carried in the response rather than written into the frontend.
func storageSanitizeGuarantee(m StorageSanitizeMethod) string {
	// Generated from the engine's own semantics table. Restating it here is how
	// a UI string ends up claiming more than the code delivers, which is
	// exactly what the removed whole-device methods did.
	s := storagemaint.SanitizeSemanticsFor(storagemaint.SanitizeMethod(m))
	if !s.Implemented {
		return ""
	}
	out := s.WhatItDoes
	if len(s.DoesNotReach) > 0 {
		out += " Not reached: " + strings.Join(s.DoesNotReach, "; ") + "."
	}
	if s.WriteCostNote != "" {
		out += " " + s.WriteCostNote
	}
	return out
}

// ---------------------------------------------------------------- read routes

// GetStorageMaintenanceDevices lists every block device with its eligibility
// and, when it is not eligible, WHY.
//
// The refusal reasons are the product here. A UI that just hides ineligible
// disks teaches the user nothing and produces a support ticket; one that says
// "this disk holds your operating system" does not.
func GetStorageMaintenanceDevices(ctx echo.Context) error {
	if err := requireStorageReadCaller(ctx); err != nil {
		return err
	}

	inv, err := blockdev.Discover()
	if err != nil {
		return refuse(http.StatusServiceUnavailable, reasonDiscoveryFailed,
			"The block devices of this host could not be enumerated.").with("error", err.Error()).write(ctx)
	}

	type deviceView struct {
		Fingerprint       string                      `json:"fingerprint"`
		Name              string                      `json:"name"`
		Model             string                      `json:"model,omitempty"`
		Vendor            string                      `json:"vendor,omitempty"`
		Serial            string                      `json:"serial,omitempty"`
		WWN               string                      `json:"wwn,omitempty"`
		IdentityStrength  blockdev.Strength           `json:"identity_strength"`
		SizeBytes         uint64                      `json:"size_bytes"`
		SizeHuman         string                      `json:"size_human"`
		Transport         blockdev.Transport          `json:"transport"`
		Removable         bool                        `json:"removable"`
		Rotational        bool                        `json:"rotational"`
		ReadOnly          bool                        `json:"read_only"`
		SupportsDiscard   bool                        `json:"supports_discard"`
		Children          []string                    `json:"children,omitempty"`
		Mountpoints       []string                    `json:"mountpoints,omitempty"`
		Holders           []string                    `json:"holders,omitempty"`
		Eligible          bool                        `json:"eligible"`
		Protected         bool                        `json:"protected"`
		ProtectionReasons []blockdev.ProtectionReason `json:"protection_reasons,omitempty"`
		Explanation       string                      `json:"explanation,omitempty"`
	}

	out := make([]deviceView, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		// Whole devices only — the same rule the operation endpoints apply.
		// Listing partitions as targets would offer something that cannot then
		// be addressed unambiguously.
		if d.IsPartition {
			continue
		}
		v := deviceView{
			Fingerprint:       d.Identity.Fingerprint(),
			Name:              d.Name,
			Model:             d.Identity.Model,
			Vendor:            d.Identity.Vendor,
			Serial:            d.Identity.Serial,
			WWN:               d.Identity.WWN,
			IdentityStrength:  d.Identity.Strength(),
			SizeBytes:         d.SizeBytes,
			SizeHuman:         storageDescribeSize(d.SizeBytes),
			Transport:         d.Transport,
			Removable:         d.Removable,
			Rotational:        d.Rotational,
			ReadOnly:          d.ReadOnly,
			SupportsDiscard:   d.SupportsDiscard,
			Children:          d.Children,
			Mountpoints:       storageTreeMountpoints(inv, d),
			Holders:           d.Holders,
			Eligible:          d.Eligible(),
			Protected:         d.Protected,
			ProtectionReasons: d.ProtectionReasons,
		}
		switch {
		case d.Protected:
			v.Explanation = storageProtectionAdvice(d.ProtectionReasons)
		case !d.Eligible():
			v.Explanation = storageIneligibilityReason(d)
		}
		out = append(out, v)
	}

	return ctx.JSON(http.StatusOK, map[string]any{
		"devices":  out,
		"taken_at": inv.TakenAt,
		"complete": inv.Complete,
		// Surfaced, not hidden: while this is non-empty every destructive
		// operation is refused, and the user is entitled to know why the
		// buttons are inert.
		"incomplete":                     inv.Incomplete,
		"destructive_operations_allowed": inv.Complete,
		"note": "Devices are addressed by fingerprint. A device path such as /dev/sdb is a slot the " +
			"kernel reuses and is never accepted by this API.",
	})
}

// GetStorageMaintenanceCapabilities reports which filesystems can actually be
// created on this host and which helpers are missing.
//
// This exists so the UI is generated from the backend's real state instead of
// offering exFAT and discovering at execution time that exfatprogs is not
// installed — after the user has already confirmed a destructive action.
func GetStorageMaintenanceCapabilities(ctx echo.Context) error {
	if err := requireStorageReadCaller(ctx); err != nil {
		return err
	}
	caps := storagemaint.CurrentCapabilities(ctx.Request().Context())

	// Built from what the engine implements, so the UI cannot offer a method
	// that would be refused — or worse, accepted and silently not performed.
	// Each entry carries the engine's own honest semantics rather than a
	// restatement written here.
	methods := []map[string]any{}
	for _, em := range storagemaint.ImplementedSanitizeMethods() {
		m := StorageSanitizeMethod(em)
		s := storagemaint.SanitizeSemanticsFor(em)
		entry := map[string]any{
			"method":    string(m),
			"ui_label":  s.UILabel,
			"guarantee": storageSanitizeGuarantee(m),
			// Every implemented method acts on a mounted filesystem's free
			// space, so none of them wants the drive unmounted.
			"requires_mounted":       true,
			"host_overwrites":        s.HostOverwrites,
			"cryptographic_erase":    s.CryptographicErase,
			"device_level_primitive": s.DeviceLevelPrimitive,
			"determinate_progress":   s.DeterminateProgress,
			"rotational_only":        s.RotationalOnly,
			"available":              true,
		}
		if em == storagemaint.SanitizeTrim {
			entry["available"] = caps.CanDiscard
			if !caps.CanDiscard {
				entry["detail"] = "neither blkdiscard nor fstrim is installed (provided by util-linux)"
			}
		}
		methods = append(methods, entry)
	}

	return ctx.JSON(http.StatusOK, map[string]any{
		"tools":               caps.Tools,
		"filesystems":         caps.Filesystems,
		"can_partition":       caps.CanPartition,
		"can_wipe_signatures": caps.CanWipeSignatures,
		"can_discard":         caps.CanDiscard,
		"can_identify":        caps.CanIdentify,
		"discovered_at":       caps.DiscoveredAt,
		"sanitize_methods":    methods,
		"engine_available":    currentStorageEngine() != nil,
	})
}

// GetStorageMaintenanceHealth serves the subsystem's own validation report.
func GetStorageMaintenanceHealth(ctx echo.Context) error {
	if err := requireStorageReadCaller(ctx); err != nil {
		return err
	}
	rep := storagehealth.Evaluate(ctx.Request().Context())

	// The engine's absence is a fact about this HTTP layer, not about the
	// storage machinery, so it is reported alongside rather than folded into
	// the invariant states.
	return ctx.JSON(http.StatusOK, map[string]any{
		"state":            rep.State,
		"invariant":        rep.Invariant,
		"actionable_cause": rep.ActionableCause,
		"destructive_operations_allowed": rep.DestructiveOperationsAllowed &&
			currentStorageEngine() != nil,
		"engine_available":  currentStorageEngine() != nil,
		"checks":            rep.Checks,
		"held_device_locks": rep.HeldDevices,
		"checked_at":        rep.CheckedAt,
		"note":              rep.Note,
	})
}

// GetStorageMaintenanceJobs lists running and interrupted jobs.
//
// Interrupted jobs are the reason this endpoint is not just a progress bar: a
// format cut short by a power failure leaves a disk in a shape nobody can
// describe from memory, and the persisted record is what makes it explainable
// after the box comes back.
func GetStorageMaintenanceJobs(ctx echo.Context) error {
	if err := requireStorageReadCaller(ctx); err != nil {
		return err
	}
	engine := currentStorageEngine()
	if engine == nil {
		return storageEngineUnavailable().write(ctx)
	}
	jobs, err := engine.Jobs(ctx.Request().Context())
	if err != nil {
		return refuse(http.StatusInternalServerError, reasonEngineRefused,
			"The job list could not be read.").with("error", err.Error()).write(ctx)
	}
	if jobs == nil {
		jobs = []StorageJob{}
	}
	var interrupted int
	for _, j := range jobs {
		if j.Interrupted {
			interrupted++
		}
	}
	return ctx.JSON(http.StatusOK, map[string]any{
		"jobs":              jobs,
		"interrupted_count": interrupted,
		"held_device_locks": storagemaint.HeldDevices(),
	})
}

func storageEngineUnavailable() *storageRefusal {
	return refuse(http.StatusServiceUnavailable, reasonEngineUnavailable,
		"The storage-maintenance engine is not installed in this build, so no destructive operation "+
			"can run. This is a fail-closed default, not a transient error.")
}

// ---------------------------------------------------------------- prepare

// PostStorageMaintenanceFormatPrepare validates a format and returns a plan.
//
// Nothing is written here. The plan exists so the user confirms a specific,
// named physical device — and so the execute step has something concrete to
// compare against when it re-checks that the same disk is still there.
// requireHealthySubsystem refuses a destructive operation when the subsystem
// cannot demonstrate its own safety properties.
//
// pkg/storagehealth is the only place in the tree that cross-examines the
// protection classifier with an INDEPENDENT parser: it reads
// /proc/self/mountinfo itself, finds the device backing /, and asserts
// blockdev reports it protected and absent from Eligible(). That is a genuine
// second opinion on the one question that matters — "would we refuse to format
// the system disk?" — and its verdict was computed and then discarded,
// consulted only by the diagnostics page.
//
// A second opinion nobody asks before acting is not a safety check. It runs
// here, before every prepare and again before every execute, because the
// window between them is exactly where a device fact can stop being derivable.
func requireHealthySubsystem(ctx echo.Context) *storageRefusal {
	rep := storagehealth.Evaluate(ctx.Request().Context())
	switch rep.State {
	case storagehealth.StateHealthy:
		return nil
	case storagehealth.StateDegraded:
		// Degraded means the subsystem still refuses correctly but something
		// it depends on is weaker than it should be. Reads stay available;
		// destroying a disk on a weakened footing does not.
		return refuse(http.StatusServiceUnavailable, reasonSubsystemUnhealthy,
			"Storage maintenance is running in a degraded state, so destructive operations are "+
				"paused until it can prove its own safety checks.").
			with("invariant", rep.Invariant).
			with("actionable_cause", rep.ActionableCause).
			with("state", string(rep.State))
	default:
		return refuse(http.StatusServiceUnavailable, reasonSubsystemUnhealthy,
			"Storage maintenance cannot demonstrate that it would refuse to damage this system, "+
				"so destructive operations are refused.").
			with("invariant", rep.Invariant).
			with("actionable_cause", rep.ActionableCause).
			with("state", string(rep.State))
	}
}

func PostStorageMaintenanceFormatPrepare(ctx echo.Context) error {
	if r := requireHealthySubsystem(ctx); r != nil {
		return r.write(ctx)
	}
	principal, err := requireStorageMutationCaller(ctx, rbac.PermStorageFormat)
	if err != nil {
		return err
	}
	engine := currentStorageEngine()
	if engine == nil {
		return storageEngineUnavailable().write(ctx)
	}

	var body formatPrepareBody
	if r := decodeStorageBody(ctx, &body); r != nil {
		return r.write(ctx)
	}

	dev, inv, r := resolveStorageTarget(body.Device)
	if r != nil {
		return r.write(ctx)
	}
	caps := storagemaint.CurrentCapabilities(ctx.Request().Context())
	fs := storagemaint.Filesystem(strings.ToLower(strings.TrimSpace(body.Filesystem)))
	label := strings.TrimSpace(body.Label)
	if r := validateFormatTarget(inv, dev, caps, fs, label); r != nil {
		return r.write(ctx)
	}

	plan, perr := engine.PrepareFormat(ctx.Request().Context(), StorageFormatRequest{
		Target:     storageTargetOf(inv, dev),
		Filesystem: fs,
		Label:      label,
		Principal:  principal,
	})
	if perr != nil {
		return refuse(http.StatusConflict, reasonEngineRefused, perr.Error()).write(ctx)
	}
	return ctx.JSON(http.StatusOK, storagePlanResponse(plan))
}

// PostStorageMaintenanceSanitizePrepare validates a sanitize and returns a plan.
func PostStorageMaintenanceSanitizePrepare(ctx echo.Context) error {
	if r := requireHealthySubsystem(ctx); r != nil {
		return r.write(ctx)
	}
	principal, err := requireStorageMutationCaller(ctx, rbac.PermStorageSanitize)
	if err != nil {
		return err
	}
	engine := currentStorageEngine()
	if engine == nil {
		return storageEngineUnavailable().write(ctx)
	}

	var body sanitizePrepareBody
	if r := decodeStorageBody(ctx, &body); r != nil {
		return r.write(ctx)
	}

	dev, inv, r := resolveStorageTarget(body.Device)
	if r != nil {
		return r.write(ctx)
	}
	caps := storagemaint.CurrentCapabilities(ctx.Request().Context())
	method := StorageSanitizeMethod(strings.ToLower(strings.TrimSpace(body.Method)))
	if r := validateSanitizeTarget(inv, dev, caps, method); r != nil {
		return r.write(ctx)
	}

	plan, perr := engine.PrepareSanitize(ctx.Request().Context(), StorageSanitizeRequest{
		Target:    storageTargetOf(inv, dev),
		Method:    method,
		Principal: principal,
	})
	if perr != nil {
		return refuse(http.StatusConflict, reasonEngineRefused, perr.Error()).write(ctx)
	}
	resp := storagePlanResponse(plan)
	resp["guarantee"] = storageSanitizeGuarantee(method)
	return ctx.JSON(http.StatusOK, resp)
}

func storagePlanResponse(plan StoragePlan) map[string]any {
	return map[string]any{
		"plan":                  plan,
		"confirmation_required": true,
		"note": "Nothing has been written. Executing this plan re-identifies the device and aborts if " +
			"anything about it has changed.",
	}
}

// ---------------------------------------------------------------- execute

// PostStorageMaintenanceFormatExecute runs a prepared format.
func PostStorageMaintenanceFormatExecute(ctx echo.Context) error {
	return storageExecute(ctx, StorageOpFormat, rbac.PermStorageFormat)
}

// PostStorageMaintenanceSanitizeExecute runs a prepared sanitize.
func PostStorageMaintenanceSanitizeExecute(ctx echo.Context) error {
	return storageExecute(ctx, StorageOpSanitize, rbac.PermStorageSanitize)
}

// storageExecute is the revalidation chokepoint.
//
// The plan is not a permission slip. Between prepare and execute the drive can
// have been unplugged, replaced by another one in the same port, mounted by the
// automounter, or absorbed into an LVM group. So every check the prepare step
// ran is run AGAIN here, against a FRESH discovery, and the identity recorded
// in the plan is compared with the identity of the device found now. Anything
// different aborts, and the engine is never called.
//
// The engine repeats all of it immediately before the first destructive call.
// This is the outer of two independent gates, not the only one.
func storageExecute(ctx echo.Context, kind StorageOpKind, perm rbac.Permission) error {
	// Re-evaluated here, not inherited from prepare: the window between the
	// two is exactly where a device fact can stop being derivable, and that is
	// the state in which this must not proceed.
	if r := requireHealthySubsystem(ctx); r != nil {
		return r.write(ctx)
	}
	principal, err := requireStorageMutationCaller(ctx, perm)
	if err != nil {
		return err
	}
	engine := currentStorageEngine()
	if engine == nil {
		return storageEngineUnavailable().write(ctx)
	}

	planID := strings.TrimSpace(ctx.Param("id"))
	if planID == "" {
		return refuse(http.StatusBadRequest, reasonPlanNotFound,
			"No plan was named in the request path.").write(ctx)
	}

	plan, ok := engine.Plan(ctx.Request().Context(), planID)
	if !ok {
		return refuse(http.StatusNotFound, reasonPlanNotFound,
			"That plan is unknown or has lapsed. Prepare the operation again — a fresh plan re-reads "+
				"the device as it is now.").write(ctx)
	}
	// A sanitize plan must not be executable through the format endpoint. The
	// plan carries the operation it was prepared for, and the two endpoints
	// carry different permission bits, so accepting a mismatch would let the
	// weaker of the two bits authorise the other operation.
	if plan.Kind != kind {
		return refuse(http.StatusConflict, reasonPlanKindMismatch,
			"That plan was prepared for a different operation.").
			with("plan_kind", string(plan.Kind)).with("endpoint_kind", string(kind)).write(ctx)
	}
	if !plan.ExpiresAt.IsZero() && time.Now().After(plan.ExpiresAt) {
		return refuse(http.StatusConflict, reasonPlanExpired,
			"That plan has expired. Prepare the operation again so the device is re-identified.").
			write(ctx)
	}

	// Re-resolve from the plan's own fingerprint — never from anything in this
	// request. The request names a plan; the plan names a device.
	dev, inv, r := resolveStorageTarget(storageDeviceRef{Fingerprint: plan.Fingerprint})
	if r != nil {
		return r.write(ctx)
	}
	// Identity comparison, not fingerprint comparison alone. Equal() refuses
	// outright when either side is weak, so a device that lost its serial
	// between prepare and execute cannot satisfy this.
	if !dev.Identity.Equal(plan.Identity) {
		return refuse(http.StatusConflict, reasonTargetChanged,
			"The device at that identity is not the one this plan was prepared against. Nothing was "+
				"touched. Prepare the operation again from the current device list.").
			with("planned", plan.Identity.Fingerprint()).
			with("found", dev.Identity.Fingerprint()).write(ctx)
	}
	if dev.Identity.Fingerprint() != plan.Fingerprint {
		return refuse(http.StatusConflict, reasonTargetChanged,
			"The device's fingerprint changed after the plan was prepared. Nothing was touched.").
			write(ctx)
	}

	// Re-run the operation's own preconditions against the fresh inventory.
	caps := storagemaint.CurrentCapabilities(ctx.Request().Context())
	switch kind {
	case StorageOpFormat:
		if r := validateFormatTarget(inv, dev, caps, plan.Filesystem, plan.Label); r != nil {
			return r.write(ctx)
		}
	case StorageOpSanitize:
		if r := validateSanitizeTarget(inv, dev, caps, plan.Method); r != nil {
			return r.write(ctx)
		}
	}

	job, jerr := engine.Execute(ctx.Request().Context(), planID, kind, principal)
	if jerr != nil {
		return refuse(http.StatusConflict, reasonEngineRefused, jerr.Error()).write(ctx)
	}
	return ctx.JSON(http.StatusAccepted, map[string]any{
		"job": job,
		"note": "The operation is running in the background. Poll " +
			"/v1/maintenance/storage/jobs for progress.",
	})
}

// PostStorageMaintenanceJobCancel asks a running job to stop.
//
// Gated on PermStorageFormat: cancelling a destructive run mid-flight leaves
// the device in a partial state, which is itself a consequential act, and the
// same people who may start one may stop it.
func PostStorageMaintenanceJobCancel(ctx echo.Context) error {
	principal, err := requireStorageMutationCaller(ctx, rbac.PermStorageFormat)
	if err != nil {
		return err
	}
	engine := currentStorageEngine()
	if engine == nil {
		return storageEngineUnavailable().write(ctx)
	}
	jobID := strings.TrimSpace(ctx.Param("id"))
	if jobID == "" {
		return refuse(http.StatusBadRequest, reasonJobIDRequired,
			"No job was named in the request path.").write(ctx)
	}
	if cerr := engine.Cancel(ctx.Request().Context(), jobID, principal); cerr != nil {
		return refuse(http.StatusConflict, reasonEngineRefused, cerr.Error()).write(ctx)
	}
	return ctx.JSON(http.StatusOK, map[string]any{
		"job_id":    jobID,
		"cancelled": true,
		"note": "Cancellation is requested, not instantaneous. A step already in progress runs to " +
			"its own boundary, so the drive can be left partially written; check the job list for " +
			"the final state.",
	})
}

// storageDescribeSize renders a size the way a person confirming a wipe needs
// to read it. The raw byte count is also sent, but "1.9 TiB" is what tells
// someone they are about to erase the wrong drive.
func storageDescribeSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
