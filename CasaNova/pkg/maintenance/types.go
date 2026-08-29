package maintenance

import (
	"time"
)

// Category keys. The HTTP layer accepts one of THESE strings and a set of
// opaque item IDs — never a path — which is what removes the whole
// path-traversal class from the API surface.
const (
	CatUpdatePackages = "casanova-update-packages"
	CatUpgradeBackups = "casanova-upgrade-backups"
	CatRotatedLogs    = "system-rotated-logs"
	CatOrphanJournal  = "orphan-journal"
	CatAptCache       = "apt-package-cache"
	CatScratch        = "casanova-scratch"
)

// Class tells the UI how much explaining a finding needs. It is NOT a
// permission level: every category is executed on its own, with its own
// confirmation, whatever its class.
type Class string

const (
	// ClassSafe: the artefact is ours or the OS's, it is regenerated or it is
	// already superseded, and the guards proved it.
	ClassSafe Class = "safe"
	// ClassConditional: safe only while a precondition holds (apt idle, no
	// staged update). The precondition is re-checked at run time too.
	ClassConditional Class = "conditional"
	// ClassReportOnly: shown with a size and a reason, never with a button.
	ClassReportOnly Class = "report_only"
)

// Action is what the category's button does.
type Action string

const (
	ActionDelete   Action = "delete"
	ActionTruncate Action = "truncate"
	ActionNone     Action = "none"
)

// Kind is the entry type a target MUST still have when we come back to delete
// it. Recorded during the scan and re-verified against fstatat right before
// the unlink: a regular file that became a symlink between scan and run is a
// planted swap, not a race we should absorb.
type Kind uint8

const (
	KindFile Kind = iota
	KindSymlink
	KindDir
)

func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindSymlink:
		return "symlink"
	case KindDir:
		return "dir"
	}
	return "unknown"
}

// target is one entry the executor may unlink. It carries the identity the
// scan observed so the run can prove it is still looking at the same inode.
// Unexported on purpose: a target is only ever produced by a category scan,
// never by anything that crossed the network.
type target struct {
	Path  string
	Kind  Kind
	Dev   uint64
	Ino   uint64
	Bytes int64     // st_blocks*512, already de-duplicated per inode
	MTime time.Time // exact mtime seen during the scan

	// Cutoff, when non-zero, is the age gate: the entry's mtime must still be
	// strictly before it at run time. Directories skip it — removing their
	// children rewrites their mtime — and rely on rmdir's ENOTEMPTY instead.
	Cutoff time.Time
}

// Item is the client-visible half of a target group. One item can expand to
// many targets (a whole upgrade-backup directory is one checkbox), which is
// why the two types are separate: the user ticks items, the executor walks
// targets.
type Item struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Path        string    `json:"path"`
	Bytes       int64     `json:"bytes"`
	MTime       time.Time `json:"mtime"`
	Detail      string    `json:"detail"`
	Preselected bool      `json:"preselected"`

	// targets is the leaves-first removal order for this item. Not serialized:
	// the client never sees paths it could echo back at us.
	targets []target
}

// Finding is one category's result. Shape mirrors pkg/diagnosis: flat,
// serializable, no envelope.
type Finding struct {
	Category         string   `json:"category"`
	Title            string   `json:"title"`
	Summary          string   `json:"summary"`
	Class            Class    `json:"class"`
	Action           Action   `json:"action"`
	ReclaimableBytes int64    `json:"reclaimable_bytes"`
	ItemCount        int      `json:"item_count"`
	Truncated        bool     `json:"truncated"`
	Blocked          bool     `json:"blocked"`
	BlockedReason    string   `json:"blocked_reason"`
	BlockedCode      string   `json:"blocked_code"`
	Preselect        bool     `json:"preselect"`
	Notes            []string `json:"notes,omitempty"`
	Items            []Item   `json:"items"`
}

// DiskInfo is a statfs snapshot. Taken before and after every run so the UI
// can show the REAL delta instead of the scan's promise (rule 14).
type DiskInfo struct {
	Path  string `json:"path"`
	Total int64  `json:"total"`
	Used  int64  `json:"used"`
	Free  int64  `json:"free"`
}

// ScanResult is the whole screen's worth of data plus the token that ties a
// later clean request to THIS scan.
type ScanResult struct {
	Token            string          `json:"scan_token"`
	StartedAt        time.Time       `json:"started_at"`
	FinishedAt       time.Time       `json:"finished_at"`
	Disk             DiskInfo        `json:"disk"`
	ReclaimableBytes int64           `json:"reclaimable_bytes"`
	ByClass          map[Class]int64 `json:"by_class"`
	Findings         []Finding       `json:"findings"`
	Notices          []Notice        `json:"notices"`
}

// Notice is a row of the read-only "space we are not going to touch" panel
// (design §3.9). It has no items and no button by construction; Bytes is
// filled in by the caller when it has a number, since most of those numbers
// come from Docker, which lives in another microservice.
type Notice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Why   string `json:"why"`
	Link  string `json:"link,omitempty"`
	Bytes int64  `json:"bytes"`
}

// Failure explains why one item was not removed. Skipped and failed are kept
// apart: a skip is a guard doing its job, a failure is the filesystem saying no.
type Failure struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// RunResult is the outcome of cleaning ONE category.
type RunResult struct {
	RunID          string    `json:"run_id"`
	Category       string    `json:"category"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	RemovedItems   int       `json:"removed_items"`
	RemovedEntries int       `json:"removed_entries"`
	RemovedBytes   int64     `json:"removed_bytes"`
	DiskBefore     DiskInfo  `json:"disk_before"`
	DiskAfter      DiskInfo  `json:"disk_after"`
	DiskDeltaBytes int64     `json:"disk_delta_bytes"`
	Failed         []Failure `json:"failed"`
	Skipped        []Failure `json:"skipped"`
	ManifestID     string    `json:"manifest_id"`
}
