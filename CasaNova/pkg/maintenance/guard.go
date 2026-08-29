package maintenance

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
)

// ─── The single deletion chokepoint (design rule 3) ─────────────────────────
//
// Every unlink this package performs goes through Guard: Check() decides
// whether a path may be touched at all, Remove() (fsops.go) is the only code
// that actually calls unlinkat. Nothing else here calls os.Remove.
//
// Why one function instead of a check per category: the root cause of 2 of the
// 3 criticals in bugs-4.md was blocklist DRIFT — three copies of "system
// directories we must not touch" that stopped agreeing with each other. So
// this file introduces no new list of system prefixes. It reuses
// utils.BlockedSystemPrefixes (the canonical one), utils.DataRoots (the
// canonical storage roots) and trash.TrashDirName, and adds only:
//
//   - allowedRoots: a WHITELIST of the handful of directories the catalogue is
//     allowed to reach into. Membership is required, not merely helpful.
//   - denyExact / denyComponent: the hard denials from rule 3 that are not
//     expressible as "system prefix" — mount roots, lost+found, the trash.
//
// The direction of the two lists matters. The whitelist is what makes a path
// eligible; the deny list can only ever take eligibility away.

var (
	ErrOutsideRoots = errors.New("path is outside every allowed maintenance root")
	ErrTooShallow   = errors.New("path is too close to the filesystem root")
	ErrDenied       = errors.New("path is on the maintenance deny-list")
	ErrEscapesRoot  = errors.New("path resolves outside its allowed maintenance root")
	ErrRemoteMount  = errors.New("path crosses into a remote or unwalkable mount")
	ErrCrossMount   = errors.New("path crosses a mount point")
	ErrIdentity     = errors.New("entry changed identity since the scan")
	ErrChanged      = errors.New("entry changed on disk since the scan")
	ErrOpenFD       = errors.New("a running process still holds this file open")
	ErrNotEmpty     = errors.New("directory is not empty")
	ErrPlanTooLarge = errors.New("plan exceeds the hard limits for a single run")
)

// minPathDepth is rule 3's "never a first- or second-level component": a
// deletable path needs at least three components, so /DATA, /var/log and
// /mnt/disk1 can never be the thing being removed — only something under them.
//
// It is a real constraint, not a formality: it is what makes an allowed-root
// list that someone later shortens by one component (say "/var" instead of
// "/var/log") still unable to delete /var/lib.
const minPathDepth = 3

// denyComponentNames are directory names that must not appear ANYWHERE in a
// path we delete, at any depth.
//
//   - lost+found is the filesystem's own recovery area; removing it makes fsck
//     unable to reattach orphaned inodes.
//   - the trash directory is the UNDO layer of a different feature with its own
//     30-day retention and its own janitor. A user who deleted a file by
//     mistake yesterday must not lose it to a space-reclaim button.
var denyComponentNames = map[string]bool{
	"lost+found":       true,
	trash.TrashDirName: true,
}

// Guard is the chokepoint. Build it with NewGuard; it is read-only afterwards
// and safe for concurrent use.
type Guard struct {
	roots []string // cleaned, absolute, longest-first
	// resolvedRoots maps a root to where it really lands when the root itself
	// is a symlink. Empty for the normal case. See Check for why it exists.
	resolvedRoots map[string]string
	// denyExact holds absolute paths that can never be the target of a removal.
	denyExact map[string]bool
	// remoteMounts is the set from service.UnwalkableMounts (rule 18): a dead
	// NFS/CIFS mount answers no syscall, so we refuse to enter one rather than
	// hang a request thread forever. Held as an atomic so a scan can refresh it
	// (a share can be mounted long after the engine was built) without a data
	// race against a descent already reading it. Never nil after NewGuard.
	remoteMounts atomic.Pointer[map[string]bool]
}

// setRemoteMounts swaps in a freshly read unwalkable-mount set. Callers do this
// at the start of each scan and clean so the guard never trusts a mount table
// captured at engine-construction time.
func (g *Guard) setRemoteMounts(m map[string]bool) {
	if m == nil {
		m = map[string]bool{}
	}
	g.remoteMounts.Store(&m)
}

// mounts returns the current unwalkable-mount set, never nil.
func (g *Guard) mounts() map[string]bool {
	if p := g.remoteMounts.Load(); p != nil {
		return *p
	}
	return map[string]bool{}
}

// NewGuard compiles a guard for the given whitelist of roots.
//
// The roots are the ONLY thing a caller can influence, and in production they
// come from Env's compiled-in defaults. A root that is itself denied (a bare
// "/", a data root, a system prefix as a whole) is rejected here rather than at
// delete time, so a bad catalogue entry fails at startup instead of during a
// run.
func NewGuard(roots []string, remoteMounts map[string]bool) (*Guard, error) {
	g := &Guard{
		denyExact:     map[string]bool{},
		resolvedRoots: map[string]string{},
	}
	g.setRemoteMounts(remoteMounts)

	// Rule 3's hard denials plus the canonical storage roots. utils.DataRoots
	// is reused rather than re-typed so a product that relocates /DATA (the
	// documented /var/lib/casaos/files fallback) keeps the same protection.
	for _, p := range []string{
		"/",
		"/var/lib/casanova",
		"/var/lib/casaos",
		"/var/lib/casaos/apps",
		"/var/lib/docker",
	} {
		g.denyExact[p] = true
	}
	for _, p := range utils.DataRoots {
		g.denyExact[utils.FixAndCleanPath(p)] = true
	}

	seen := map[string]bool{}
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		clean := utils.FixAndCleanPath(r)
		if seen[clean] {
			continue
		}
		if err := g.validateRoot(clean); err != nil {
			return nil, fmt.Errorf("maintenance root %q: %w", r, err)
		}
		seen[clean] = true
		g.roots = append(g.roots, clean)
		// Resolved once, here, so Check never pays for it and a root that
		// disappears later cannot change the answer mid-run.
		if target, err := filepath.EvalSymlinks(clean); err == nil && target != clean {
			g.resolvedRoots[clean] = target
		}
	}
	if len(g.roots) == 0 {
		return nil, errors.New("maintenance guard needs at least one allowed root")
	}
	// Longest first so /var/log/journal wins over /var/log when both are
	// listed; the root a path is attributed to is the one we descend from.
	sortLongestFirst(g.roots)
	return g, nil
}

// validateRoot refuses a root that would defeat the whitelist.
//
// Note what it does NOT refuse: a root that is itself on the deny-list. /DATA
// is both — it is a legitimate place to descend FROM (the scratch category
// lives there) and a directory nothing may ever delete. The two questions are
// different, and conflating them would either lock the scratch category out or
// make the storage root removable.
func (g *Guard) validateRoot(clean string) error {
	if clean == "/" || pathDepth(clean) < 1 {
		return ErrDenied
	}
	// A root may live under a blocked system prefix — /var/log and
	// /var/cache/apt/archives both do, on purpose — but it may never BE one.
	// Same shape as utils.withinDataRoot's guard: a root equal to the prefix
	// would exempt the entire prefix and silently disable the blocklist.
	if bp := blockedSystemPrefixOf(clean); bp == clean {
		return ErrDenied
	}
	for _, c := range pathComponents(clean) {
		if denyComponentNames[c] {
			return ErrDenied
		}
	}
	return nil
}

// Roots returns the whitelist, for logging and tests.
func (g *Guard) Roots() []string {
	out := make([]string, len(g.roots))
	copy(out, g.roots)
	return out
}

// Check is the gate. It answers "may this path be removed at all", and returns
// the allowed root the removal must descend from.
//
// Order is deliberate: the cheap lexical rules run first so a hostile or buggy
// path is rejected before any syscall, and the symlink resolution runs last so
// it only ever happens for paths that already passed everything else.
func (g *Guard) Check(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(path, 0) {
		return "", errors.New("path contains a null byte")
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: not absolute", ErrOutsideRoots)
	}
	cleaned := utils.FixAndCleanPath(path)
	// FixAndCleanPath collapses "." and "..", so a caller that passed a
	// traversal gets a different string back than it sent. Refuse instead of
	// silently acting on the collapsed path: inside this package every path
	// comes from our own scan and is already clean, so a mismatch means a bug
	// or an injection, never a user typing "..".
	if cleaned != path {
		return "", fmt.Errorf("%w: path is not in canonical form", ErrDenied)
	}

	comps := pathComponents(cleaned)
	if len(comps) < minPathDepth {
		return "", fmt.Errorf("%w: %s has %d components, need %d",
			ErrTooShallow, cleaned, len(comps), minPathDepth)
	}
	for _, c := range comps {
		if denyComponentNames[c] {
			return "", fmt.Errorf("%w: %s contains %q", ErrDenied, cleaned, c)
		}
	}
	if g.denyExact[cleaned] {
		return "", fmt.Errorf("%w: %s", ErrDenied, cleaned)
	}

	root := g.rootFor(cleaned)
	if root == "" {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoots, cleaned)
	}

	// Reconcile with the canonical blocklist instead of duplicating it.
	//
	// Every phase-1 root is under /var, which IS a blocked system prefix — for
	// the file API, whose job is to never touch it. This module's job is the
	// opposite, so the rule we enforce is narrower: a path may sit inside a
	// blocked prefix ONLY when the allowed root it belongs to sits strictly
	// inside that same prefix. /var/log/syslog.1 is fine because /var/log is a
	// declared root under /var; /var/lib/casaos/db/... is not, because no root
	// covers it. Paths outside every blocked prefix (the /DATA scratch roots)
	// go through the canonical sanitizer verbatim.
	if bp := blockedSystemPrefixOf(cleaned); bp != "" {
		rp := blockedSystemPrefixOf(root)
		if rp != bp || root == bp {
			return "", fmt.Errorf("%w: %s is under %s and no allowed root covers it",
				ErrDenied, cleaned, bp)
		}
	} else if _, err := utils.SanitizeFilePathNoFollow(cleaned); err != nil {
		// SanitizeFilePathNoFollow, not SanitizeFilePath: we act on the entry
		// itself, and unlinking a symlink never touches its target.
		return "", err
	}

	// Last, and only for paths that already qualify: where does the PARENT
	// really land? A symlink planted inside a root (there is one on this box
	// today: big-bear-wordpress/mysql/mysql.sock → /var/run) must not let a
	// removal walk out of the root. This is a string-level backstop; the
	// actual removal re-proves it structurally with O_NOFOLLOW at every level.
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return "", fmt.Errorf("cannot resolve parent of %s: %w", cleaned, err)
	}
	landsAt := filepath.Join(resolvedParent, filepath.Base(cleaned))
	// Compared against the root AND against where the root itself resolves: a
	// storage root can legitimately be a symlink (the /var/lib/casaos/files
	// fallback layout), and rejecting every path under it would disable a
	// category rather than protect anything. A link that leaves the root is
	// still inside neither, so the escape stays closed.
	if !withinRoot(landsAt, root) {
		if rr := g.resolvedRoots[root]; rr == "" || !withinRoot(landsAt, rr) {
			return "", fmt.Errorf("%w: %s", ErrEscapesRoot, cleaned)
		}
	}

	return root, nil
}

// rootFor returns the allowed root that strictly contains path, or "".
// Boundary-safe: "/var/logger/x" must not match the root "/var/log".
func (g *Guard) rootFor(path string) string {
	for _, r := range g.roots {
		if strings.HasPrefix(path, r+"/") {
			return r
		}
	}
	return ""
}

// withinRoot is the boundary-safe containment test rule 3 spells out:
// path == prefix || HasPrefix(path, prefix+"/").
func withinRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// blockedSystemPrefixOf reports which utils.BlockedSystemPrefixes entry
// contains path, or "" when none does.
//
// This is a boundary-safe match over THE canonical list — exactly what
// pkg/utils/path.go asks call sites to do — not a fourth copy of it. If a
// prefix is ever added or removed there, this module follows automatically.
func blockedSystemPrefixOf(path string) string {
	for _, prefix := range utils.BlockedSystemPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return prefix
		}
	}
	return ""
}

func pathComponents(clean string) []string {
	trimmed := strings.Trim(clean, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func pathDepth(clean string) int { return len(pathComponents(clean)) }

func sortLongestFirst(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
