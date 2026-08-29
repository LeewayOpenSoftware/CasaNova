package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// The compiled catalogue (design §5).
//
// The client sends a KEY from this map plus opaque item IDs. It never sends a
// path, so path traversal is not a bug class this API can have. An item's ID
// is a hash of (category, path, inode) computed during the scan; the executor
// resolves it against the plan we stored, never against anything that arrived
// over the network.
//
// A category is three functions and some metadata, in the shape of
// curatedServices (host_services.go):
//
//	scan  — look, decide, explain. Never mutates anything.
//	plan  — turn ticked item IDs into an ordered list of removals.
//	exec  — re-check the preconditions, write the manifest, remove.
//
// Most categories share the generic plan and exec; what differs between them
// is the scan and the precondition their exec re-checks. That is deliberate:
// the dangerous code exists once.

type scanFunc func(*Engine) *Finding
type planFunc func(*Engine, *Finding, []string) ([]Item, error)
type execFunc func(*Engine, *Finding, []Item, string) (*RunResult, error)

// Category is one row of the catalogue.
type Category struct {
	Key    string
	Label  string
	Title  string
	Class  Class
	Action Action
	// Warn is the consequence sentence the confirmation dialog must show.
	Warn string
	// RequiresStrictJWT marks the mutating endpoints; §5's gate table.
	RequiresStrictJWT bool
	// Phase records which delivery phase the category belongs to, so the UI
	// can explain why a listed thing is not actionable yet.
	Phase int

	scan scanFunc
	plan planFunc
	exec execFunc
}

// CategoryInfo is the catalogue as the UI sees it.
type CategoryInfo struct {
	Key               string `json:"key"`
	Label             string `json:"label"`
	Title             string `json:"title"`
	Class             Class  `json:"class"`
	Action            Action `json:"action"`
	Warn              string `json:"warn,omitempty"`
	RequiresStrictJWT bool   `json:"requires_strict_jwt"`
	Phase             int    `json:"phase"`
}

// catalog is compiled in. Adding a key here is the ONLY way to make the module
// able to delete a new kind of thing, and it is a code review with a design
// card behind it, not configuration.
//
// Populated from init() rather than as a var literal purely because the scan
// functions call newFinding, which reads this map — a package-level literal
// would be an initialization cycle. The map is never written again.
var catalog map[string]*Category

func init() {
	catalog = map[string]*Category{
		CatUpdatePackages: {
			Key:               CatUpdatePackages,
			Label:             "casanova-update-packages",
			Title:             "Superseded installation packages",
			Class:             ClassConditional,
			Action:            ActionDelete,
			Warn:              "You will not be able to reinstall these versions without a network connection.",
			RequiresStrictJWT: true,
			Phase:             1,
			scan:              scanUpdatePackages,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckUpdatePackages),
		},
		CatUpgradeBackups: {
			Key:               CatUpgradeBackups,
			Label:             "casanova-upgrade-backups",
			Title:             "Superseded upgrade backups",
			Class:             ClassSafe,
			Action:            ActionDelete,
			Warn:              "These are older rollback points. The current one is never offered.",
			RequiresStrictJWT: true,
			Phase:             1,
			scan:              scanUpgradeBackups,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckUpgradeBackups),
		},
		CatRotatedLogs: {
			Key:               CatRotatedLogs,
			Label:             "system-rotated-logs",
			Title:             "Rotated logs with no writer",
			Class:             ClassSafe,
			Action:            ActionDelete,
			Warn:              "You lose the older history of these logs. The live files are untouched.",
			RequiresStrictJWT: true,
			Phase:             1,
			scan:              scanRotatedLogs,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckRotatedLogs),
		},
		CatOrphanJournal: {
			Key:               CatOrphanJournal,
			Label:             "orphan-journal",
			Title:             "Journal from a previous installation",
			Class:             ClassSafe,
			Action:            ActionDelete,
			Warn:              "Logs from before this machine was reinstalled are lost. The current journal is untouched.",
			RequiresStrictJWT: true,
			Phase:             1,
			scan:              scanOrphanJournal,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckOrphanJournal),
		},
		CatAptCache: {
			Key:               CatAptCache,
			Label:             "apt-package-cache",
			Title:             "Downloaded .deb packages already installed",
			Class:             ClassConditional,
			Action:            ActionDelete,
			Warn:              "apt will download these again if it ever needs them.",
			RequiresStrictJWT: true,
			Phase:             1,
			scan:              scanAptCache,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckAptCache),
		},
		CatScratch: {
			Key:               CatScratch,
			Label:             "casanova-scratch",
			Title:             "Leftovers from our own upload and copy flows",
			Class:             ClassSafe,
			Action:            ActionDelete,
			Warn:              "Interrupted uploads that can no longer be resumed are discarded.",
			RequiresStrictJWT: true,
			Phase:             2,
			scan:              scanScratch,
			plan:              defaultPlan,
			exec:              execWithPrecheck(precheckScratch),
		},
	}
}

// catalogOrder is the display order: our own artefacts first (the user
// recognises them), then the OS's, then the data area.
var catalogOrder = []string{
	CatUpdatePackages,
	CatUpgradeBackups,
	CatRotatedLogs,
	CatOrphanJournal,
	CatAptCache,
	CatScratch,
}

// Categories returns the catalogue metadata, in display order.
func Categories() []CategoryInfo {
	out := make([]CategoryInfo, 0, len(catalogOrder))
	for _, key := range catalogOrder {
		c := catalog[key]
		out = append(out, CategoryInfo{
			Key:               c.Key,
			Label:             c.Label,
			Title:             c.Title,
			Class:             c.Class,
			Action:            c.Action,
			Warn:              c.Warn,
			RequiresStrictJWT: c.RequiresStrictJWT,
			Phase:             c.Phase,
		})
	}
	return out
}

// Lookup returns the compiled category for a key, or nil. A key that is not
// here is a 400, never a path we try to interpret.
func Lookup(key string) *CategoryInfo {
	c, ok := catalog[key]
	if !ok {
		return nil
	}
	return &CategoryInfo{
		Key:               c.Key,
		Label:             c.Label,
		Title:             c.Title,
		Class:             c.Class,
		Action:            c.Action,
		Warn:              c.Warn,
		RequiresStrictJWT: c.RequiresStrictJWT,
		Phase:             c.Phase,
	}
}

// itemID is the opaque handle the client gets. Hashing (category, path, inode)
// means the ID stops matching the moment the inode is replaced, so a stale UI
// cannot resolve an ID onto a file that took the old one's place.
func itemID(category, path string, ino uint64) string {
	sum := sha256.Sum256([]byte(category + "\x00" + path + "\x00" + strconv.FormatUint(ino, 10)))
	return hex.EncodeToString(sum[:])[:16]
}

// newFinding starts an empty result for a category, pre-filled from the
// compiled metadata so a category can never disagree with its own catalogue row.
func newFinding(key string) *Finding {
	c := catalog[key]
	return &Finding{
		Category: c.Key,
		Title:    c.Title,
		Class:    c.Class,
		Action:   c.Action,
		Items:    []Item{},
	}
}

// block marks a finding unusable and says why, in a code the UI can translate
// and a sentence a human can read. The size stays visible on purpose: the user
// should see what is being withheld and why, not just an absence.
func (f *Finding) block(code, reason string) {
	f.Blocked = true
	f.BlockedCode = code
	f.BlockedReason = reason
	f.Preselect = false
	for i := range f.Items {
		f.Items[i].Preselected = false
	}
}

// addItem appends an item and keeps the aggregates honest.
func (f *Finding) addItem(it Item) {
	f.Items = append(f.Items, it)
	f.ReclaimableBytes += it.Bytes
	f.ItemCount = len(f.Items)
}

// Blocked reason codes. Stable strings: the UI translates them, and the HTTP
// layer maps some of them onto its 409 payload.
const (
	BlockUpdateStaged   = "update_staged"
	BlockUpdateRunning  = "update_running"
	BlockDpkgLocked     = "dpkg_locked"
	BlockInconsistent   = "inconsistent_state"
	BlockNoProcAccess   = "proc_unreadable"
	BlockUploadRegistry = "upload_registry_unreadable"
	BlockScanError      = "scan_error"
	BlockRunInProgress  = "run_in_progress"
	BlockStaleScan      = "stale_scan"
	BlockSystemBusy     = "system_busy"
)
