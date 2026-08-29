package storageauth

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// This file holds the policy model: what the rules are, how they are ordered,
// and how a decision explains itself.
//
// THE QUESTION THIS ANSWERS
//
// The inherited question was "is this path in BlockedSystemPrefixes?" — a
// deny-list, where everything unlisted was permitted. The question now is
// "what authority establishes that CasaNova, and then this principal, may
// perform this operation on this object?"
//
// PRECEDENCE — total, deterministic, and tested
//
//	1. INVALID_PATH             lexically unusable
//	2. POLICY_UNAVAILABLE       the authority is not in force -> refuse
//	3. SYSTEM_LOCATION          kernel/pseudo filesystem -> refuse, unconditionally
//	4. EXPLICITLY_DENIED        an operator DENY rule covers it
//	5. allowed by an EXPLICIT root
//	6. allowed by a DERIVED root
//	7. OUTSIDE_AUTHORIZED_STORAGE  default deny
//
// then, having established that CasaNova may manage the object at all:
//
//	8. administrators pass
//	9. a grant covering (principal, subtree, operation) passes
//	10. PRINCIPAL_NOT_AUTHORIZED   default deny
//
// Rule 3 sits ABOVE the explicit rules on purpose. An operator may extend what
// CasaNova manages; they may not declare procfs to be ordinary user storage,
// because that subtree is not storage in any sense the file manager can
// honour. Making an "allow" able to override it would turn the Permissions
// panel into a disable-security switch, which section 4 of the specification
// forbids. An administrative escape hatch, if ever wanted, is a separate
// privileged feature and deliberately not part of this one.
//
// Every branch above yields exactly one ReasonCode, so the decision is never
// ambiguous and a UI never has to parse English.

// ReasonCode is the machine-readable outcome of an authorization decision.
// A future Permissions UI switches on these; it must never scan message text.
type ReasonCode string

const (
	// Allowed outcomes carry which authority permitted the operation, so the
	// UI can show whether a location is derived or operator-configured.
	ReasonAllowedDerived  ReasonCode = "ALLOWED_DERIVED"
	ReasonAllowedExplicit ReasonCode = "ALLOWED_EXPLICIT"

	// Denied outcomes.
	ReasonOutsideAuthorizedStorage ReasonCode = "OUTSIDE_AUTHORIZED_STORAGE"
	ReasonExplicitlyDenied         ReasonCode = "EXPLICITLY_DENIED"
	ReasonSystemLocation           ReasonCode = "SYSTEM_LOCATION"
	ReasonPrincipalNotAuthorized   ReasonCode = "PRINCIPAL_NOT_AUTHORIZED"
	ReasonStorageUnavailable       ReasonCode = "STORAGE_UNAVAILABLE"
	ReasonPolicyUnavailable        ReasonCode = "POLICY_UNAVAILABLE"
	ReasonInvalidPath              ReasonCode = "INVALID_PATH"
)

// Source records what established a root, for the Permissions UI's SOURCE
// column and for auditing.
type Source string

const (
	SourceDerived  Source = "derived"  // CasaNova found it by asking its own subsystems
	SourceExplicit Source = "explicit" // an operator declared it
	SourceSystem   Source = "system"   // a hard boundary, not configurable
)

// Access is the ALLOW/DENY sense of a rule.
type Access string

const (
	AccessAllow Access = "allow"
	AccessDeny  Access = "deny"
)

// Decision is the result of an authorization query. It is deliberately a value
// with a reason code rather than a bare error: callers map Reason to an HTTP
// status, and the UI maps it to an explanation and an action.
type Decision struct {
	Allowed bool       `json:"allowed"`
	Reason  ReasonCode `json:"reason"`
	// Path is the canonical form the caller should act on when Allowed.
	Path string `json:"path,omitempty"`
	// Rule names the policy entry that decided, for audit and for the UI's
	// "authorized by" line. Empty when the decision came from a default.
	Rule string `json:"rule,omitempty"`
	// Detail is human-facing and safe to log. It must never disclose paths a
	// principal is not entitled to know about.
	Detail string `json:"detail,omitempty"`
	// Remediable reports whether an administrator could make this succeed by
	// adding an explicit root. False for system locations and for principal
	// denials, where the fix is a grant rather than a root.
	Remediable bool `json:"remediable"`

	// ConfineRoot is the NARROWEST boundary that authorized this decision: the
	// principal's grant when one applied, otherwise the storage root. It is
	// what OpenAuthorized confines the kernel's own path resolution to, so the
	// window between deciding and opening cannot be used to land somewhere
	// else. Confining to the ceiling when a narrower grant applied would let a
	// swap move the target outside the principal's entitlement while staying
	// inside authorized storage.
	ConfineRoot string `json:"-"`

	// Resolved is where the requested path landed at decision time, with
	// symlinks followed. Kept so the open can be expressed relative to
	// ConfineRoot.
	Resolved string `json:"-"`
}

func (d Decision) Error() string {
	return string(d.Reason) + ": " + d.Detail
}

// Rule is one policy entry.
type Rule struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Access Access `json:"access"`
	Source Source `json:"source"`
	// Kind explains a derived root's provenance (mount, share, appdata…).
	Kind RootKind `json:"kind,omitempty"`
	Note string   `json:"note,omitempty"`
	// AddedBy records the principal that created an explicit rule. Mutating
	// policy is privileged, and an audit trail is part of that.
	AddedBy   string    `json:"added_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// policy is an immutable snapshot. Authorize loads it through an atomic
// pointer and evaluates it with no lock and no I/O, so independent operations
// never serialize on the authority — replacing an insecure O(1) check with a
// global mutex on the filesystem hot path would be its own defect.
type policy struct {
	version int64

	// rules are pre-sorted longest-path-first so the most specific entry that
	// covers a path is found first.
	rules []Rule

	// systemMounts maps a mount point to the pseudo-filesystem type mounted
	// there. Derived from /proc/self/mountinfo once per refresh, never per
	// authorization.
	systemMounts []systemMount

	// mountinfoOK reports whether the kernel mount table could be read. When
	// false the authority cannot prove a path is not a pseudo-filesystem, and
	// says so rather than guessing.
	mountinfoOK bool

	derivedCount  int
	explicitCount int
}

type systemMount struct {
	point  string
	fstype string
}

// pseudoFilesystems are filesystem types that are never ordinary user storage.
//
// This reasons from filesystem IDENTITY, not from pathnames. /proc is only
// dangerous because procfs is mounted there; a directory literally named
// /proc on a data disk is just a directory. Conversely a debugfs mounted at
// /DATA/debug is dangerous no matter how innocent its path looks, and a
// pathname blacklist would miss it entirely. Reading the mount table answers
// the question a name never can.
var pseudoFilesystems = map[string]bool{
	"proc": true, "procfs": true,
	"sysfs": true, "securityfs": true, "selinuxfs": true,
	"debugfs": true, "tracefs": true, "configfs": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "bpf": true,
	"devtmpfs": true, "devpts": true, "mqueue": true, "hugetlbfs": true,
	"binfmt_misc": true, "efivarfs": true, "fusectl": true, "nsfs": true,
	"autofs": true, "rpc_pipefs": true, "ramfs": true,
}

var effective atomic.Pointer[policy]

// loadPolicy returns the snapshot in force, or nil when the authority has not
// been provisioned.
func loadPolicy() *policy { return effective.Load() }

// publish swaps in a new snapshot. This is the PUBLISH step of
// validate -> prepare -> persist -> durable -> publish: nothing observes a new
// policy until it is durable on disk, so a crash mid-update leaves the old
// policy in force rather than a half-built one.
func publish(p *policy) { effective.Store(p) }

// buildPolicy assembles an immutable snapshot from derived roots, operator
// rules and the kernel mount table.
func buildPolicy(version int64, derived []Root, explicit []Rule, mounts []systemMount, mountinfoOK bool) *policy {
	rules := make([]Rule, 0, len(derived)+len(explicit))
	for _, r := range derived {
		rules = append(rules, Rule{
			ID:     "derived:" + r.Path,
			Path:   r.Path,
			Access: AccessAllow,
			Source: SourceDerived,
			Kind:   r.Kind,
			Note:   r.Note,
		})
	}
	rules = append(rules, explicit...)

	// Longest path first: the most specific rule covering a path decides.
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].Path) != len(rules[j].Path) {
			return len(rules[i].Path) > len(rules[j].Path)
		}
		// Deterministic tie-break, and DENY before ALLOW at equal specificity
		// so an operator's explicit denial of exactly one path wins over an
		// allow of the same path rather than depending on insertion order.
		if rules[i].Path == rules[j].Path && rules[i].Access != rules[j].Access {
			return rules[i].Access == AccessDeny
		}
		return rules[i].ID < rules[j].ID
	})

	p := &policy{version: version, rules: rules, systemMounts: mounts, mountinfoOK: mountinfoOK}
	for _, r := range rules {
		switch r.Source {
		case SourceDerived:
			p.derivedCount++
		case SourceExplicit:
			p.explicitCount++
		}
	}
	return p
}

// evaluateStorage applies rules 1 through 7: may CasaNova manage this object at
// all? The principal layer is applied by the caller afterwards.
//
// Cost is O(rules) with no I/O: the mount table and the rule list are both
// pre-computed in the snapshot. Authorizing N paths costs N rule scans, never
// N mount-table reads or N policy-file parses.
func (p *policy) evaluateStorage(clean, resolved string) Decision {
	// Rule 3 — system locations. Checked before every configurable rule, and
	// on the RESOLVED path, so a symlink into procfs is caught too.
	if hit, fstype, ok := p.systemLocation(resolved); ok {
		return Decision{
			Reason: ReasonSystemLocation,
			Rule:   "system:" + hit,
			Detail: fmt.Sprintf("%s is a %s filesystem, which cannot be managed as ordinary storage", hit, fstype),
			// Not remediable by configuration, and saying so keeps the UI from
			// offering an "add this location" button that could never work.
			Remediable: false,
		}
	}
	if !p.mountinfoOK && looksLikeKernelMountPoint(resolved) {
		// The mount table could not be read, so pseudo-filesystem membership
		// cannot be proven. For the handful of paths where the kernel
		// conventionally mounts them, refuse rather than assume.
		return Decision{
			Reason:     ReasonSystemLocation,
			Rule:       "system:unverifiable",
			Detail:     "the kernel mount table could not be read, so this location cannot be proven to be ordinary storage",
			Remediable: false,
		}
	}

	// Rule 4 — an explicit DENY covering EITHER form wins. Checking the
	// resolved form too means a link cannot be used to slip out from under a
	// denial that names the real location.
	for _, r := range p.rules {
		if r.Access == AccessDeny && (within(r.Path, clean) || within(r.Path, resolved)) {
			return Decision{
				Reason:     ReasonExplicitlyDenied,
				Rule:       r.ID,
				Detail:     "an administrator has denied access to this location",
				Remediable: true,
			}
		}
	}

	// Rules 5-6 — BOTH the requested path and where it really lands must be
	// covered by an ALLOW rule, but NOT necessarily the same one.
	//
	// Requiring one rule to cover both was wrong and the tests caught it: a
	// symlink between two storage areas (/DATA/media -> /mnt/disk1, routine on
	// a NAS) is covered lexically by the /DATA rule and really by the
	// /mnt/disk1 rule, and demanding a single rule refused it. The property
	// that matters is that neither end escapes authorized storage — not that
	// they escape it together.
	lex := p.firstAllow(clean)
	res := p.firstAllow(resolved)
	if lex != nil && res != nil {
		reason := ReasonAllowedDerived
		if lex.Source == SourceExplicit || res.Source == SourceExplicit {
			reason = ReasonAllowedExplicit
		}
		rule := lex.ID
		if lex.ID != res.ID {
			rule = lex.ID + "->" + res.ID
		}
		root := res.Path
		if len(lex.Path) > len(root) && within(res.Path, lex.Path) {
			root = lex.Path
		}
		return Decision{Allowed: true, Reason: reason, Rule: rule, Path: clean,
			ConfineRoot: root, Resolved: resolved}
	}
	if lex != nil && res == nil {
		return Decision{
			Reason:     ReasonOutsideAuthorizedStorage,
			Detail:     "this location resolves outside the storage CasaNova is authorized to manage",
			Remediable: true,
		}
	}

	// Rule 7 — default deny.
	return Decision{
		Reason:     ReasonOutsideAuthorizedStorage,
		Detail:     "this location is outside the storage CasaNova is authorized to manage",
		Remediable: true,
	}
}

// firstAllow returns the most specific ALLOW rule covering path, or nil.
func (p *policy) firstAllow(path string) *Rule {
	for i := range p.rules {
		if p.rules[i].Access == AccessAllow && within(p.rules[i].Path, path) {
			return &p.rules[i]
		}
	}
	return nil
}

// systemLocation reports whether path sits on a pseudo-filesystem, using the
// deepest mount point that contains it.
func (p *policy) systemLocation(path string) (string, string, bool) {
	best, bestType := "", ""
	for _, m := range p.systemMounts {
		if within(m.point, path) && len(m.point) > len(best) {
			best, bestType = m.point, m.fstype
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, bestType, true
}

// looksLikeKernelMountPoint is the fallback used ONLY when the mount table is
// unreadable. It is deliberately small and is not the security model: it exists
// so that losing /proc does not silently downgrade the boundary.
func looksLikeKernelMountPoint(path string) bool {
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if within(p, path) {
			return true
		}
	}
	return false
}

// readSystemMounts parses /proc/self/mountinfo and returns the mount points
// carrying pseudo-filesystems.
//
// Read once per policy build, not per authorization: parsing the mount table
// per file would be exactly the O(mounts)-per-request amplification a previous
// hardening round had to remove elsewhere in this tree.
func readSystemMounts() ([]systemMount, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, false
	}
	defer f.Close()

	var out []systemMount
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo: id parent maj:min root MOUNTPOINT opts... - FSTYPE source super
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		point := unescapeMountinfo(left[4])
		fstype := right[0]
		if pseudoFilesystems[fstype] {
			out = append(out, systemMount{point: point, fstype: fstype})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false
	}
	return out, true
}

// unescapeMountinfo decodes the octal escapes the kernel writes for spaces,
// tabs, newlines and backslashes in mount points.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// PolicyView is the read model a Permissions UI renders: one row per rule with
// the columns the specification calls for — LOCATION, SOURCE, SCOPE, ACCESS,
// STATUS.
type PolicyView struct {
	Location string `json:"location"`
	Source   Source `json:"source"`
	// Scope describes who the rule applies to, in terms the UI can show
	// directly.
	Scope  string `json:"scope"`
	Access Access `json:"access"`
	// Status reports whether the location is currently usable — a derived root
	// whose disk was unplugged is still policy, but it is not healthy.
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// Policy returns the effective policy for display. It is the SAME snapshot
// enforcement uses, so a Permissions panel can never drift from what is
// actually applied — the specification's "no duplicated frontend policy".
func Policy() []PolicyView {
	p := loadPolicy()
	if p == nil {
		return nil
	}
	out := make([]PolicyView, 0, len(p.rules)+len(p.systemMounts))
	for _, r := range p.rules {
		status := "healthy"
		if _, err := os.Stat(r.Path); err != nil {
			status = "unavailable"
		}
		scope := "all authorized storage users"
		if r.Access == AccessDeny {
			scope = "everyone"
		}
		out = append(out, PolicyView{
			Location: r.Path, Source: r.Source, Scope: scope,
			Access: r.Access, Status: status, Note: r.Note,
		})
	}
	// System boundaries are part of the effective policy and the UI should
	// show them, greyed out and not editable.
	for _, m := range p.systemMounts {
		out = append(out, PolicyView{
			Location: m.point, Source: SourceSystem, Scope: "—",
			Access: AccessDeny, Status: "protected",
			Note: m.fstype + " filesystem; not configurable",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Location < out[j].Location })
	return out
}

// PolicyVersion is the monotonically increasing version of the effective
// policy. It changes on every accepted mutation and on every re-derivation
// that alters the rule set, so a UI can tell whether its view is current.
func PolicyVersion() int64 {
	if p := loadPolicy(); p != nil {
		return p.version
	}
	return 0
}

// pathIsCanonicalRootCandidate rejects roots that cannot sensibly anchor
// policy, before anything is persisted.
func pathIsCanonicalRootCandidate(clean string) error {
	if clean == "" {
		return fmt.Errorf("path must be absolute")
	}
	if clean == "/" {
		return fmt.Errorf("the filesystem root cannot be a storage root; that would restore the unbounded behaviour this authority replaces")
	}
	if strings.ContainsRune(clean, 0) {
		return fmt.Errorf("path contains a null byte")
	}
	if clean != filepath.Clean(clean) {
		return fmt.Errorf("path is not canonical")
	}
	return nil
}
