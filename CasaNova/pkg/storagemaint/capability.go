// Package storagemaint is the destructive storage-maintenance subsystem: the
// external-disk formatter and free-space sanitization, and the shared
// machinery both need.
//
// It owns four things neither feature may implement for itself:
//
//	CAPABILITY  which helpers actually exist on this host, discovered at
//	            startup rather than at the moment the user clicks a button;
//	LOCKING     one destructive job per physical device, keyed on stable
//	            identity so a rename cannot let two jobs onto one disk;
//	JOBS        durable job state, so an interrupted operation is diagnosed
//	            after a restart instead of leaving a disk in an unknown shape;
//	AUDIT       an append-only record of who destroyed what.
//
// Device discovery and the protection classifier live in pkg/blockdev, which is
// the single authority both features consult. Nothing here re-derives what a
// device is or whether it may be touched.
package storagemaint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Filesystem is a filesystem CasaNova can create.
type Filesystem string

const (
	FSExt4  Filesystem = "ext4"
	FSExFAT Filesystem = "exfat"
	FSNTFS  Filesystem = "ntfs"
	FSFAT32 Filesystem = "fat32"
)

// SupportedFilesystems is the closed set. Only options that can be validated
// are offered; a filesystem whose creation cannot be verified afterwards has no
// business in a destructive workflow.
var SupportedFilesystems = []Filesystem{FSExt4, FSExFAT, FSNTFS, FSFAT32}

// Tool is an external helper this subsystem needs.
type Tool string

const (
	ToolMkfsExt4   Tool = "mkfs.ext4"
	ToolMkfsExFAT  Tool = "mkfs.exfat"
	ToolMkfsNTFS   Tool = "mkfs.ntfs"
	ToolMkfsFAT    Tool = "mkfs.fat"
	ToolWipefs     Tool = "wipefs"
	ToolSfdisk     Tool = "sfdisk"
	ToolBlkdiscard Tool = "blkdiscard"
	ToolFstrim     Tool = "fstrim"
	ToolBlkid      Tool = "blkid"
	ToolPartprobe  Tool = "partprobe"
)

// filesystemTools maps a filesystem to the helper that creates it.
var filesystemTools = map[Filesystem]Tool{
	FSExt4:  ToolMkfsExt4,
	FSExFAT: ToolMkfsExFAT,
	FSNTFS:  ToolMkfsNTFS,
	FSFAT32: ToolMkfsFAT,
}

// allTools is everything discovery probes for.
var allTools = []Tool{
	ToolMkfsExt4, ToolMkfsExFAT, ToolMkfsNTFS, ToolMkfsFAT,
	ToolWipefs, ToolSfdisk, ToolBlkdiscard, ToolFstrim, ToolBlkid, ToolPartprobe,
}

// ToolStatus is what discovery found for one helper.
type ToolStatus struct {
	Tool      Tool   `json:"tool"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	// Detail explains an unavailable tool in terms an operator can act on.
	Detail string `json:"detail,omitempty"`
}

// FilesystemCapability is what the UI renders per filesystem, so an option that
// cannot work is never offered.
type FilesystemCapability struct {
	Filesystem Filesystem `json:"filesystem"`
	Available  bool       `json:"available"`
	Tool       Tool       `json:"tool"`
	Detail     string     `json:"detail,omitempty"`
	// MaxVolumeBytes and MaxFileBytes are the constraints that must reach the
	// UI: FAT32 in particular cannot hold a file over 4 GiB, and offering it
	// for a media drive without saying so produces a failure hours later.
	MaxVolumeBytes uint64 `json:"max_volume_bytes,omitempty"`
	MaxFileBytes   uint64 `json:"max_file_bytes,omitempty"`
	// LabelMaxLen and LabelNote carry each filesystem's label rules, which
	// differ enough that a shared validator would be wrong for all of them.
	LabelMaxLen int    `json:"label_max_len"`
	LabelNote   string `json:"label_note,omitempty"`
}

// Capabilities is the whole picture, discovered once at startup and refreshed
// on demand.
type Capabilities struct {
	Tools       []ToolStatus           `json:"tools"`
	Filesystems []FilesystemCapability `json:"filesystems"`
	// CanPartition, CanWipeSignatures and CanDiscard gate whole workflows
	// rather than one filesystem.
	CanPartition      bool      `json:"can_partition"`
	CanWipeSignatures bool      `json:"can_wipe_signatures"`
	CanDiscard        bool      `json:"can_discard"`
	CanIdentify       bool      `json:"can_identify"`
	DiscoveredAt      time.Time `json:"discovered_at"`
}

// Available reports whether a filesystem can be created right now.
func (c Capabilities) Available(fs Filesystem) (bool, string) {
	for _, f := range c.Filesystems {
		if f.Filesystem == fs {
			return f.Available, f.Detail
		}
	}
	return false, "unsupported filesystem"
}

var (
	capMu     sync.RWMutex
	capCache  Capabilities
	capLoaded bool
	// lookPath is indirected so tests can present a host with a chosen set of
	// helpers instead of whatever the developer's machine happens to have.
	lookPath = exec.LookPath
)

// SetLookPath overrides helper discovery. Tests only.
//
// CONTRACT: this invalidates capLoaded as a side effect. Five call sites
// across this package, CasaNova/route and CasaNova/route/v1 rely on that —
// see TestSetLookPathInvalidatesTheCapabilityCache — because it is what
// makes a fresh SetLookPath call force a real re-probe instead of serving
// back whatever the PREVIOUS lookPath's result was. Do not remove it as
// "redundant" without checking that test first.
func SetLookPath(f func(string) (string, error)) {
	capMu.Lock()
	defer capMu.Unlock()
	lookPath = f
	capLoaded = false
}

// ResetCapabilityCache clears the discovered-capabilities cache: both the
// cached result and the loaded flag. Tests only.
//
// capCache/capLoaded are package-level, shared by every test that reaches
// CurrentCapabilities/DiscoverCapabilities in the SAME test binary — which
// includes every test in this package plus, through the exported
// SetLookPath, every test in CasaNova/route and CasaNova/route/v1 that
// exercises the storage-maintenance HTTP handlers. A test that primes the
// cache via SetLookPath + Discover/CurrentCapabilities must call this in its
// own t.Cleanup, or a later test in the same binary can inherit its fake —
// or, worse, its real — tool set instead of getting a fresh probe of its
// own host. Same set+restore shape as SetRoots/SetSanitizeProcRoot/
// SetProtectedMountPrefixes.
func ResetCapabilityCache() {
	capMu.Lock()
	defer capMu.Unlock()
	capCache = Capabilities{}
	capLoaded = false
}

// DiscoverCapabilities probes for every helper.
//
// At startup, not at click time. A user who selects exFAT, confirms a
// destructive operation and only then learns mkfs.exfat is missing has been
// walked up to the edge of losing data for nothing. The UI is built from this,
// so an unavailable filesystem is never offered.
func DiscoverCapabilities(ctx context.Context) Capabilities {
	capMu.Lock()
	lp := lookPath
	capMu.Unlock()

	c := Capabilities{DiscoveredAt: time.Now()}
	found := map[Tool]ToolStatus{}

	for _, t := range allTools {
		st := ToolStatus{Tool: t}
		path, err := lp(string(t))
		if err != nil {
			st.Detail = "helper not found in PATH"
			// mkfs.ntfs ships as ntfs-3g on most distributions and mkfs.exfat
			// as exfatprogs; naming the package is the difference between an
			// operator who can fix it and one who files a bug.
			switch t {
			case ToolMkfsNTFS:
				st.Detail += " (provided by ntfs-3g)"
			case ToolMkfsExFAT:
				st.Detail += " (provided by exfatprogs)"
			case ToolMkfsFAT:
				st.Detail += " (provided by dosfstools)"
			}
		} else {
			st.Available = true
			st.Path = path
			st.Version = toolVersion(ctx, path)
		}
		found[t] = st
		c.Tools = append(c.Tools, st)
	}
	sort.Slice(c.Tools, func(i, j int) bool { return c.Tools[i].Tool < c.Tools[j].Tool })

	for _, fs := range SupportedFilesystems {
		tool := filesystemTools[fs]
		st := found[tool]
		cap := FilesystemCapability{
			Filesystem: fs, Tool: tool, Available: st.Available, Detail: st.Detail,
		}
		switch fs {
		case FSFAT32:
			// The limits that actually bite: a single file cannot exceed 4 GiB
			// minus one byte, and mkfs.fat refuses volumes past 2 TiB with the
			// default cluster size. A media drive formatted FAT32 fails on the
			// first large video, hours after the format "succeeded".
			cap.MaxFileBytes = 4<<30 - 1
			cap.MaxVolumeBytes = 2 << 40
			cap.LabelMaxLen = 11
			cap.LabelNote = "up to 11 characters, uppercase A-Z, 0-9 and _ - only"
		case FSExFAT:
			cap.LabelMaxLen = 15
			cap.LabelNote = "up to 15 characters"
		case FSNTFS:
			cap.LabelMaxLen = 32
			cap.LabelNote = "up to 32 characters"
		case FSExt4:
			cap.LabelMaxLen = 16
			cap.LabelNote = "up to 16 characters"
		}
		c.Filesystems = append(c.Filesystems, cap)
	}

	c.CanPartition = found[ToolSfdisk].Available
	c.CanWipeSignatures = found[ToolWipefs].Available
	c.CanDiscard = found[ToolBlkdiscard].Available || found[ToolFstrim].Available
	c.CanIdentify = found[ToolBlkid].Available

	capMu.Lock()
	capCache, capLoaded = c, true
	capMu.Unlock()
	return c
}

// CurrentCapabilities returns the cached discovery, running it if needed.
func CurrentCapabilities(ctx context.Context) Capabilities {
	capMu.RLock()
	if capLoaded {
		c := capCache
		capMu.RUnlock()
		return c
	}
	capMu.RUnlock()
	return DiscoverCapabilities(ctx)
}

// toolVersion asks a helper for its version, bounded.
//
// Bounded because this runs at startup: a helper that hangs would hold the
// service's boot, and there is no version string worth that.
func toolVersion(ctx context.Context, path string) string {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Structured arguments, never a shell string. Nothing here is
	// caller-influenced, and it stays that way by construction.
	out, err := exec.CommandContext(tctx, path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

// ValidateLabel checks a volume label against the target filesystem's rules
// BEFORE anything destructive happens.
//
// Each filesystem has different rules, so a shared validator would be wrong for
// all of them. Rejecting at prepare time means the user fixes a typo instead of
// discovering after the disk is already wiped that mkfs refused the label.
func ValidateLabel(fs Filesystem, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil // no label is always valid
	}
	if strings.ContainsAny(label, "\x00/\\") {
		return fmt.Errorf("a volume label cannot contain a null byte, a slash or a backslash")
	}

	switch fs {
	case FSFAT32:
		if len(label) > 11 {
			return fmt.Errorf("FAT32 labels are at most 11 characters (got %d)", len(label))
		}
		for _, r := range label {
			ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '_' || r == '-' || r == ' '
			if !ok {
				return fmt.Errorf("FAT32 labels allow only uppercase A-Z, digits, space, _ and - (found %q)", r)
			}
		}
	case FSExFAT:
		if len([]rune(label)) > 15 {
			return fmt.Errorf("exFAT labels are at most 15 characters")
		}
	case FSNTFS:
		if len([]rune(label)) > 32 {
			return fmt.Errorf("NTFS labels are at most 32 characters")
		}
	case FSExt4:
		if len(label) > 16 {
			return fmt.Errorf("ext4 labels are at most 16 bytes")
		}
	default:
		return fmt.Errorf("unsupported filesystem %q", fs)
	}
	return nil
}

// ValidateFilesystemForDevice rejects a combination that cannot work on this
// device, before the user confirms anything.
func ValidateFilesystemForDevice(c Capabilities, fs Filesystem, sizeBytes uint64) error {
	ok, detail := c.Available(fs)
	if !ok {
		return fmt.Errorf("%s cannot be created on this host: %s", fs, detail)
	}
	for _, f := range c.Filesystems {
		if f.Filesystem != fs {
			continue
		}
		if f.MaxVolumeBytes > 0 && sizeBytes > f.MaxVolumeBytes {
			return fmt.Errorf("%s cannot address a volume of %s (limit %s); "+
				"choose exFAT or ext4 for a drive this size",
				fs, humanBytes(sizeBytes), humanBytes(f.MaxVolumeBytes))
		}
	}
	return nil
}

func humanBytes(b uint64) string {
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

// ---------------------------------------------------------------- job locking

// ErrDeviceBusy reports that another destructive job holds this device.
var ErrDeviceBusy = errors.New("another destructive operation is already running on this device")

// ErrTooManyJobs reports the global ceiling.
var ErrTooManyJobs = errors.New("too many destructive storage operations are already running")

// maxConcurrentJobs bounds destructive work across the whole appliance.
//
// Two is not arbitrary: these operations saturate I/O, and letting the user
// start six of them turns a maintenance action into an outage of everything
// else the box serves.
const maxConcurrentJobs = 2

var (
	lockMu    sync.Mutex
	heldLocks = map[string]string{} // device fingerprint -> job id
)

// AcquireDevice takes the exclusive destructive lock for a physical device.
//
// The key is the stable identity fingerprint, never the kernel name: two jobs
// prepared against "sdb" before and after a replug are about different disks,
// and a name-keyed lock would either wrongly serialise them or wrongly let both
// run. It also means a whole-disk job and a job on one of its partitions
// collide, because the caller passes the PARENT's fingerprint for both.
func AcquireDevice(fingerprint, jobID string) error {
	if strings.TrimSpace(fingerprint) == "" {
		return errors.New("refusing to lock an unidentified device")
	}
	lockMu.Lock()
	defer lockMu.Unlock()

	if holder, ok := heldLocks[fingerprint]; ok {
		if holder == jobID {
			return nil // re-entrant for the same job
		}
		return fmt.Errorf("%w (held by job %s)", ErrDeviceBusy, holder)
	}
	if len(heldLocks) >= maxConcurrentJobs {
		return fmt.Errorf("%w (%d running, limit %d)", ErrTooManyJobs, len(heldLocks), maxConcurrentJobs)
	}
	heldLocks[fingerprint] = jobID
	return nil
}

// ReleaseDevice drops the lock. Safe to call for a job that does not hold it.
func ReleaseDevice(fingerprint, jobID string) {
	lockMu.Lock()
	defer lockMu.Unlock()
	if heldLocks[fingerprint] == jobID {
		delete(heldLocks, fingerprint)
	}
}

// HeldDevices reports the current locks, for health output.
func HeldDevices() map[string]string {
	lockMu.Lock()
	defer lockMu.Unlock()
	out := make(map[string]string, len(heldLocks))
	for k, v := range heldLocks {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- audit

// AuditEntry records a destructive action.
type AuditEntry struct {
	At          time.Time `json:"at"`
	PrincipalID string    `json:"principal_id"`
	Operation   string    `json:"operation"`
	Fingerprint string    `json:"device_fingerprint"`
	Model       string    `json:"model,omitempty"`
	Serial      string    `json:"serial,omitempty"`
	SizeBytes   uint64    `json:"size_bytes"`
	Requested   string    `json:"requested,omitempty"`
	Result      string    `json:"result"`
	Stage       string    `json:"stage,omitempty"`
	Detail      string    `json:"detail,omitempty"`
}

var (
	auditMu  sync.Mutex
	stateDir = "/var/lib/casanova/storage-maintenance"
)

// SetStateDir relocates persisted job state and the audit log. Tests only.
func SetStateDir(dir string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	stateDir = dir
}

// StateDir reports where job state lives.
func StateDir() string {
	auditMu.Lock()
	defer auditMu.Unlock()
	return stateDir
}

// Audit appends an entry. Append-only and fsynced: an audit record that does
// not survive the crash it was describing is not an audit record.
func Audit(e AuditEntry) error {
	auditMu.Lock()
	defer auditMu.Unlock()

	if e.At.IsZero() {
		e.At = time.Now()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "audit.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadAudit returns the recorded entries, newest last.
func ReadAudit() ([]AuditEntry, error) {
	auditMu.Lock()
	dir := stateDir
	auditMu.Unlock()

	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AuditEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e AuditEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
		// A malformed line is skipped rather than aborting the read: a
		// truncated final record from a crash must not hide every entry
		// before it.
	}
	return out, nil
}
