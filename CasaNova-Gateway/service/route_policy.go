package service

// Per-route AUTHORIZATION policy, stored separately from route registration.
//
// The invariant this file exists to hold:
//
//	A SERVICE RE-REGISTRATION — I.E. AVAILABILITY MAINTENANCE — MAY NEVER
//	RELAX AN AUTHORIZATION DECISION AN ADMINISTRATOR MADE. Only an explicit
//	administrative operation may change policy.
//
// Why a separate store instead of a flag inside CreateRoute:
//
//   - A route used to be (path -> target) only, so re-POSTing a route was
//     genuinely a no-op and every service's registration watchdog re-POSTs
//     one every 30s. When per-route CIDR whitelists were added to the SAME
//     payload with replace-everything semantics, those availability re-POSTs
//     (which never carry allowed_cidrs) silently deleted the whitelist. A
//     liveness mechanism was deciding authorization.
//
//   - Route registration is EPHEMERAL, rebuildable state: routes.json lives
//     in the gateway's RuntimeDirectory (/run, tmpfs). Losing it is harmless
//     because every service re-registers. Authorization policy has the
//     opposite lifetime: it is durable operator intent, and losing it fails
//     OPEN. Storing both in the same tmpfs file meant a reboot also dropped
//     every per-route whitelist. Policy therefore lives next to the global
//     whitelist in /etc/casanova, which survives reboots.
//
// Enforcement note, stated honestly: this store decides WHICH networks a
// route allows. It does not change WHICH client address is compared against
// them — that is securityPeerIP()/rewriteRequestSourceIP() in the route
// package, and behind a loopback reverse proxy (cloudflared) the compared
// address is the proxy's. This file deliberately adds NO loopback exemption
// of its own, because such an exemption would be equivalent to disabling
// per-route whitelists entirely for tunnelled traffic.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// routePolicyFile is the durable home of per-route authorization policy.
// Deliberately NOT under State.GetRuntimePath(): that is a tmpfs runtime
// directory. Same directory as whitelistFile, for the same reason.
//
// A var (not a const) purely so tests can point it at a temp directory.
var routePolicyFile = "/etc/casanova/route-policy.json"

const routePolicyDocVersion = 1

// Named invariants. These strings are what a failed health check reports and
// what an operator greps the journal for; keep them stable.
const (
	// The policy file must be a regular file, owned by us, and not writable
	// by anyone else — otherwise its contents are not an admin decision.
	invRoutePolicyFileTrusted = "route_policy_file_is_owned_and_unwritable_by_others"
	// A policy decision must still be in force after a restart or reboot.
	invRoutePolicyDurable = "route_policy_survives_restart"
)

// RoutePolicyHealth reports whether per-route authorization policy is in a
// state we can vouch for. OK=false names the invariant that failed rather
// than only saying "error", so the failure is actionable from a log line.
type RoutePolicyHealth struct {
	OK               bool   `json:"ok"`
	FailedInvariant  string `json:"failed_invariant,omitempty"`
	Detail           string `json:"detail,omitempty"`
	Path             string `json:"path"`
	DeclaredRoutes   int    `json:"declared_routes"`
	RestrictedRoutes int    `json:"restricted_routes"`
	UnusableRoutes   int    `json:"unusable_routes"`
}

// routeACL is one administrator decision about one route.
//
// The three states are distinct and all three are meaningful:
//
//	absent key          -> no decision on record. Unrestricted, and a legacy
//	                       routes.json may still supply one (migration).
//	present, len(raw)=0 -> the admin explicitly removed the restriction.
//	                       Unrestricted, and migration must NOT resurrect an
//	                       older list.
//	present, len(raw)>0 -> restricted to nets.
type routeACL struct {
	// raw is exactly the CIDR list as recorded, so a rewrite of the document
	// never loses an entry we could not parse.
	raw []string
	// nets is the parsed form used for enforcement.
	nets []*net.IPNet
	// unusable is set when raw declared restrictions but none of them parsed.
	// Such a route denies every client: silently serving it unrestricted
	// would be exactly the relaxation this file exists to prevent. It is only
	// reachable by hand-editing the file (Set validates before persisting),
	// i.e. by someone who already has local write access to /etc.
	unusable bool
}

func (a routeACL) restricted() bool { return len(a.nets) > 0 || a.unusable }

// allows reports whether ip satisfies this decision.
func (a routeACL) allows(ip net.IP) bool {
	if a.unusable {
		return false
	}
	if len(a.nets) == 0 {
		// Explicitly unrestricted.
		return true
	}
	if ip == nil {
		return false
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

type routePolicyStore struct {
	mu   sync.RWMutex
	path string
	acls map[string]routeACL

	failedInvariant string

	// unreadContent records that a policy file EXISTS on disk whose contents
	// this process refused to read. It is what stops a later write from
	// destroying a decision nobody has seen.
	unreadContent bool
	failedDetail  string
}

// newRoutePolicyStore opens (and autoprovisions) the policy store. It never
// fails hard: a store that cannot read its file starts empty and DEGRADED,
// because a gateway that refuses to start is a worse outcome than one that
// reports a named broken invariant. Callers that care must consult Health.
func newRoutePolicyStore(path string) *routePolicyStore {
	s := &routePolicyStore{
		path: path,
		acls: make(map[string]routeACL),
	}
	s.load()
	return s
}

func (s *routePolicyStore) degrade(invariant, detail string) {
	s.failedInvariant = invariant
	s.failedDetail = detail
}

func (s *routePolicyStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := verifyPolicyFileTrust(s.path); err != nil {
		// Do NOT read a file we cannot attribute to ourselves. Starting with
		// no policy is the same exposure as before this store existed; acting
		// on an attacker-writable file would be worse.
		s.degrade(invRoutePolicyFileTrusted, err.Error())
		if _, statErr := os.Stat(s.path); statErr == nil {
			// The file is there and holds an administrator's decision this
			// process is not willing to read. Remember that, so persistLocked
			// cannot later replace it with the empty in-memory set.
			s.unreadContent = true
		}
		logger.Error("route policy file failed its trust check; not loading it",
			zap.String("invariant", invRoutePolicyFileTrusted),
			zap.String("path", s.path),
			zap.Error(err))
		return
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		// Fresh box: no decision on record. Not a failure.
		return
	}
	if err != nil {
		s.degrade(invRoutePolicyDurable, err.Error())
		logger.Error("failed to read route policy file",
			zap.String("invariant", invRoutePolicyDurable),
			zap.String("path", s.path),
			zap.Error(err))
		return
	}

	var doc routePolicyDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		s.degrade(invRoutePolicyDurable, err.Error())
		logger.Error("route policy file is not parseable; per-route whitelists are NOT in force",
			zap.String("invariant", invRoutePolicyDurable),
			zap.String("path", s.path),
			zap.Error(err))
		return
	}

	for routePath, entry := range doc.Routes {
		s.acls[routePath] = buildACL(routePath, entry.AllowedCIDRs)
	}
}

// buildACL parses a persisted CIDR list. Unparseable entries are dropped from
// enforcement (narrowing, the safe direction) but kept in raw so a later
// rewrite does not lose them. A list that yields nothing at all is marked
// unusable rather than degraded into "unrestricted".
func buildACL(routePath string, raw []string) routeACL {
	acl := routeACL{raw: append([]string(nil), raw...)}
	for _, cidr := range raw {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Error("unparseable CIDR in route policy",
				zap.String("invariant", invRoutePolicyDurable),
				zap.String("route", routePath),
				zap.String("cidr", cidr),
				zap.Error(err))
			continue
		}
		acl.nets = append(acl.nets, ipnet)
	}
	if len(raw) > 0 && len(acl.nets) == 0 {
		acl.unusable = true
		logger.Error("route policy declares restrictions that could not be reconstructed; route denies all clients until fixed",
			zap.String("invariant", invRoutePolicyDurable),
			zap.String("route", routePath))
	}
	return acl
}

// Lookup returns the decision on record for an exact route path.
func (s *routePolicyStore) Lookup(routePath string) (routeACL, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acl, ok := s.acls[routePath]
	return acl, ok
}

// CIDRsFor returns the recorded CIDR list for a route, and whether any
// decision is on record at all.
func (s *routePolicyStore) CIDRsFor(routePath string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acl, ok := s.acls[routePath]
	if !ok {
		return nil, false
	}
	return append([]string(nil), acl.raw...), true
}

// Snapshot returns the restricted routes as path -> CIDR strings. Routes an
// admin explicitly left unrestricted are omitted, so the shape matches what
// routes.json has always carried.
func (s *routePolicyStore) Snapshot() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.acls))
	for routePath, acl := range s.acls {
		if len(acl.raw) == 0 {
			continue
		}
		out[routePath] = append([]string(nil), acl.raw...)
	}
	return out
}

// Set records an explicit administrative decision. A nil or empty list means
// "explicitly unrestricted" and is recorded as such — that is what makes a
// later migration refuse to resurrect an older list. Every CIDR is validated
// before anything is written, so a bad request changes nothing.
func (s *routePolicyStore) Set(routePath string, cidrs []string) error {
	if routePath == "" {
		return errors.New("route path is empty")
	}
	acl := routeACL{raw: make([]string, 0, len(cidrs))}
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		acl.nets = append(acl.nets, ipnet)
		acl.raw = append(acl.raw, ipnet.String())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had := s.acls[routePath]
	s.acls[routePath] = acl
	if err := s.persistLocked(); err != nil {
		// Roll the in-memory decision back so memory and disk agree. An admin
		// who got an error must not find the change silently applied until
		// the next restart, and a failed CLEAR must keep restricting.
		if had {
			s.acls[routePath] = previous
		} else {
			delete(s.acls, routePath)
		}
		return err
	}
	return nil
}

// AdoptLegacy migrates per-route CIDRs that were persisted inside routes.json
// (the pre-split format) into this store. It only fills in routes with NO
// decision on record, so an explicit clear is never undone. Idempotent: after
// the first successful adoption the routes are known and later calls no-op.
func (s *routePolicyStore) AdoptLegacy(legacy map[string][]*net.IPNet) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	adopted := 0
	for routePath, nets := range legacy {
		if len(nets) == 0 {
			continue
		}
		if _, known := s.acls[routePath]; known {
			continue
		}
		acl := routeACL{nets: append([]*net.IPNet(nil), nets...)}
		for _, n := range nets {
			acl.raw = append(acl.raw, n.String())
		}
		s.acls[routePath] = acl
		adopted++
	}
	if adopted == 0 {
		return 0, nil
	}
	logger.Info("migrated per-route authorization policy out of routes.json into durable storage",
		zap.Int("routes", adopted),
		zap.String("path", s.path))
	return adopted, s.persistLocked()
}

// Health reports the store's current standing. Never returns OK for a store
// whose file we could not trust or write.
func (s *routePolicyStore) Health() RoutePolicyHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthLocked()
}

// healthLocked is Health for callers that already hold s.mu. persistLocked runs
// under the write lock, so it cannot call Health — RWMutex is not reentrant and
// the RLock would deadlock against the Lock its own caller holds.
func (s *routePolicyStore) healthLocked() RoutePolicyHealth {
	h := RoutePolicyHealth{
		OK:              s.failedInvariant == "",
		FailedInvariant: s.failedInvariant,
		Detail:          s.failedDetail,
		Path:            s.path,
		DeclaredRoutes:  len(s.acls),
	}
	for _, acl := range s.acls {
		if acl.restricted() {
			h.RestrictedRoutes++
		}
		if acl.unusable {
			h.UnusableRoutes++
		}
	}
	return h
}

// logDegradedLocked journals the store's AGGREGATE standing when it is not
// healthy, naming the occasion that produced it.
//
// Every degrade site already logs the individual failure, and the constructor
// already reports the aggregate once (service/management.go). What was missing
// is the aggregate AFTER BOOT: policy can degrade at runtime, because Set() —
// reachable from the management API — goes through persistLocked, and a
// degrade there was only ever visible as the single line describing that one
// failed write. An operator reading "failed to persist route policy" could not
// tell from it how much policy is now unvouched for.
//
// The message string is deliberately IDENTICAL to the one the constructor
// emits, so the same grep finds the boot-time and the runtime occurrences;
// "occasion" is what tells them apart. The invariant names above carry the same
// promise of stability for the same reason.
func (s *routePolicyStore) logDegradedLocked(occasion string) {
	h := s.healthLocked()
	if h.OK {
		return
	}

	logger.Error("per-route authorization policy is DEGRADED",
		zap.String("invariant", h.FailedInvariant),
		zap.String("detail", h.Detail),
		zap.String("path", h.Path),
		zap.String("occasion", occasion),
		zap.Int("declared_routes", h.DeclaredRoutes),
		zap.Int("restricted_routes", h.RestrictedRoutes),
		zap.Int("unusable_routes", h.UnusableRoutes))
}

type routePolicyDoc struct {
	Version int                             `json:"version"`
	Routes  map[string]persistedRoutePolicy `json:"routes"`
}

type persistedRoutePolicy struct {
	// Always emitted, including when empty: an empty list is the durable
	// record of "the admin removed this restriction", which is different from
	// the route being absent from the document.
	AllowedCIDRs []string `json:"allowed_cidrs"`
}

// persistLocked writes the whole document and then re-verifies the file's
// properties. Callers must hold s.mu for writing.
func (s *routePolicyStore) persistLocked() error {
	// REFUSE to write over a file whose contents were never read.
	//
	// Fail-closed on load turned fail-OPEN and silent on the second restart.
	// The sequence: an administrator records a restriction; the file becomes
	// untrusted (a packaging bug, a restore from backup, the wrong uid); boot 2
	// correctly refuses to load it and reports the failed invariant — and then
	// the first write persists the EMPTY in-memory set over it, passes its own
	// trust check on the file it just created, clears failedInvariant, and
	// reports healthy with no policy at all. The refusal destroyed the very
	// data it was protecting, and then stopped mentioning it.
	//
	// A store that could not read the decision has nothing to say about it.
	if s.unreadContent {
		detail := fmt.Sprintf("%s exists but its contents were never read (%s); refusing to "+
			"overwrite an administrator's decision with an empty set. Repair the file's ownership "+
			"and permissions, or remove it deliberately, then restart.", s.path, s.failedDetail)
		logger.Error("refusing to persist route policy over unread content",
			zap.String("invariant", invRoutePolicyFileTrusted),
			zap.String("path", s.path))
		return errors.New(detail)
	}

	doc := routePolicyDoc{
		Version: routePolicyDocVersion,
		Routes:  make(map[string]persistedRoutePolicy, len(s.acls)),
	}
	for routePath, acl := range s.acls {
		cidrs := acl.raw
		if cidrs == nil {
			cidrs = []string{}
		}
		doc.Routes[routePath] = persistedRoutePolicy{AllowedCIDRs: cidrs}
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// This one degraded SILENTLY: unlike its two neighbours below it had no
		// log line of its own, so the store could enter a degraded state with
		// nothing at all in the journal.
		s.degrade(invRoutePolicyDurable, err.Error())
		logger.Error("failed to serialise route policy",
			zap.String("invariant", invRoutePolicyDurable),
			zap.String("path", s.path),
			zap.Error(err))
		s.logDegradedLocked("serialising route policy")
		return err
	}
	if err := writePolicyFileAtomic(s.path, data); err != nil {
		s.degrade(invRoutePolicyDurable, err.Error())
		logger.Error("failed to persist route policy; the decision is in force now but will NOT survive a restart",
			zap.String("invariant", invRoutePolicyDurable),
			zap.String("path", s.path),
			zap.Error(err))
		s.logDegradedLocked("persisting route policy")
		return err
	}
	// Validate the PROPERTY we depend on, not merely that the write returned
	// nil. A successful validated write also repairs a store that started
	// degraded because the file was untrusted.
	if err := verifyPolicyFileTrust(s.path); err != nil {
		s.degrade(invRoutePolicyFileTrusted, err.Error())
		logger.Error("route policy file failed its trust check right after being written",
			zap.String("invariant", invRoutePolicyFileTrusted),
			zap.String("path", s.path),
			zap.Error(err))
		s.logDegradedLocked("verifying route policy file after write")
		return err
	}
	if s.failedInvariant != "" {
		logger.Info("route policy store recovered",
			zap.String("path", s.path),
			zap.String("previous_failed_invariant", s.failedInvariant))
	}
	s.failedInvariant = ""
	s.failedDetail = ""
	return nil
}

// verifyPolicyFileTrust autoprovisions the parent directory and checks the
// properties that make the file's contents attributable to us: the directory
// and the file are not writable by other users, neither is a symlink, and
// both are owned by the current user. A missing file is fine — that is a
// fresh box, not a violation.
func verifyPolicyFileTrust(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dir)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkOwner(dir, dirInfo); err != nil {
		return err
	}
	// For the DIRECTORY only world-write is disqualifying, and only without
	// the sticky bit (which stops other users from replacing a file they do
	// not own). Rejecting group-write here would be a trap: a packaging
	// script leaving /etc/casanova at 0775 root:root would silently switch
	// every per-route whitelist off — exactly the failure this store exists
	// to prevent. Group members of a root-owned directory are already
	// privileged.
	if perm := dirInfo.Mode().Perm(); perm&0o002 != 0 && dirInfo.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s is world writable (%04o) without the sticky bit", dir, perm)
	}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	// The FILE is ours: we create it 0600, so any write bit for another user
	// means someone else has been given the ability to author policy.
	// Readability is not a property we depend on, so read bits are ignored.
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is group/world writable (%04o)", path, perm)
	}
	return checkOwner(path, info)
}

// checkOwner rejects a path owned by another user — its contents are then not
// attributable to an administrator acting through us.
func checkOwner(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown platform: we cannot prove ownership, so do not claim it.
		return fmt.Errorf("cannot determine owner of %s", path)
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return fmt.Errorf("%s is owned by uid %d, expected %d", path, st.Uid, euid)
	}
	return nil
}

// writePolicyFileAtomic writes data durably: exclusive temp file (so a
// pre-planted symlink at the temp path cannot redirect the write), fsync,
// rename over the target, then fsync the directory so the rename itself
// survives power loss.
func writePolicyFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// A leftover temp file from a crashed write must not block the retry,
	// and O_EXCL below is what makes the create refuse a symlink.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
