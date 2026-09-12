package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/labstack/echo/v4"
)

// TestMain keeps the whole package off the real /etc/casanova path. Tests in
// this package construct Management freely and must never touch (or depend
// on) the production policy file or the global whitelist file.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "casanova-gateway-policy-testmain")
	if err != nil {
		panic(err)
	}
	SetConfigPathsForTest(dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// isolatePolicyFile points the durable policy store at a per-test file and
// returns its path. Tests using it must not run in parallel.
func isolatePolicyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "route-policy.json")
	previous := routePolicyFile
	routePolicyFile = p
	t.Cleanup(func() { routePolicyFile = previous })
	return p
}

func newTestManagement(t *testing.T, runtimeDir string) *Management {
	t.Helper()
	state := NewState()
	if err := state.SetRuntimePath(runtimeDir); err != nil {
		t.Fatal(err)
	}
	return NewManagementService(state)
}

// availabilityReRegistration is byte-for-byte the payload the registration
// watchdog and every service's startup registration send: path and target,
// no policy field. Building it here from model.Route (rather than hand-rolled
// JSON) keeps the test honest if the struct changes.
func availabilityReRegistration(path, target string) *model.Route {
	return &model.Route{Path: path, Target: target}
}

// L-01. THE invariant: a re-registration that maintains availability must not
// relax an authorization decision.
func TestReRegistrationDoesNotClearRoutePolicy(t *testing.T) {
	isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("precondition failed: the whitelist is not being enforced at all")
	}

	// 30s later, and on every tick after that.
	for i := 0; i < 3; i++ {
		if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
			t.Fatal(err)
		}
	}

	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("route re-registration deleted the admin's CIDR whitelist")
	}
	if !m.IsIPAllowedForPath("/v1/files", "192.168.1.50") {
		t.Fatal("re-registration broke the whitelist for an allowed client")
	}
	cidrs, ok := m.GetRoutePolicy("/v1/files")
	if !ok || len(cidrs) != 1 || cidrs[0] != "192.168.1.0/24" {
		t.Fatalf("policy not intact after re-registration: %v (recorded=%v)", cidrs, ok)
	}
}

// A re-registration must still do its actual job: refresh the target.
func TestReRegistrationStillUpdatesAvailability(t *testing.T) {
	isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	// A service that restarted on a new ephemeral port re-registers.
	if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:9999")); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, r := range m.GetRoutes() {
		if r.Path == "/v1/files" {
			found = true
			if r.Target != "http://127.0.0.1:9999" {
				t.Fatalf("target not refreshed by re-registration: %q", r.Target)
			}
		}
	}
	if !found {
		t.Fatal("route disappeared")
	}
	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("target refresh relaxed the whitelist")
	}
}

// Clearing a whitelist must remain possible without SSH — an admin who
// mistypes a CIDR has to be able to undo it over the same API. The explicit
// empty list is the only payload that means "remove the restriction".
func TestExplicitEmptyListClearsPolicy(t *testing.T) {
	isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	if !m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("explicit clear did not remove the restriction")
	}

	// And the removal must be durable: a restart must not bring it back.
	m2 := newTestManagement(t, t.TempDir())
	if err := m2.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if !m2.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("a restriction the admin removed came back after restart")
	}
}

// Removing a restriction has to be RECORDED, not merely forgotten. "No entry"
// and "an admin removed it" look identical to a forgetful store, and the
// migration path treats "no entry" as free to fill in from routes.json — so a
// forgotten removal comes back.
func TestExplicitClearIsRecordedAsADecision(t *testing.T) {
	isolatePolicyFile(t)
	runtimeDir := t.TempDir()
	m := newTestManagement(t, runtimeDir)

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{},
	}); err != nil {
		t.Fatal(err)
	}

	cidrs, recorded := m.GetRoutePolicy("/v1/files")
	if !recorded {
		t.Fatal("clearing a whitelist left no decision on record")
	}
	if len(cidrs) != 0 {
		t.Fatalf("cleared route still lists CIDRs: %v", cidrs)
	}

	// Why it matters: a routes.json that still carries the old list — written
	// by an older gateway, or left behind by a rollback — must not resurrect
	// the restriction on the next start.
	legacy := map[string]persistedRoute{
		"/v1/files": {Target: "http://127.0.0.1:8080", AllowedCIDRs: []string{"192.168.1.0/24"}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, RoutesFile), data, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := newTestManagement(t, runtimeDir)
	if !restarted.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("a restriction the admin removed was resurrected from a stale routes.json")
	}
}

// SetRoutePolicy is the explicit administrative operation; it must be able to
// both set and remove, and must not require the route to be registered first.
func TestSetRoutePolicyIsAuthoritative(t *testing.T) {
	isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.SetRoutePolicy("/v1/files", []string{"10.0.0.0/8"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("policy set before registration was not applied")
	}
	if err := m.SetRoutePolicy("/v1/files", nil); err != nil {
		t.Fatal(err)
	}
	if !m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("explicit policy removal did not take effect")
	}
}

// An invalid CIDR must change nothing — not the policy, and not half of it.
func TestInvalidCIDRIsRejectedWithoutSideEffects(t *testing.T) {
	isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"10.0.0.0/8", "not-a-cidr"},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid CIDR")
	}
	cidrs, _ := m.GetRoutePolicy("/v1/files")
	if len(cidrs) != 1 || cidrs[0] != "192.168.1.0/24" {
		t.Fatalf("rejected request mutated policy: %v", cidrs)
	}
}

// Policy must outlive the tmpfs runtime directory. A reboot empties /run, so
// storing whitelists in routes.json meant every reboot silently unrestricted
// every route.
func TestPolicySurvivesRuntimeDirectoryWipe(t *testing.T) {
	isolatePolicyFile(t)

	m := newTestManagement(t, t.TempDir())
	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}

	// Reboot: brand new (empty) runtime dir, same machine.
	rebooted := newTestManagement(t, t.TempDir())
	if err := rebooted.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if rebooted.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("per-route whitelist did not survive a runtime-directory wipe")
	}
}

// Upgrade path: a box already running a release that stored CIDRs inside
// routes.json must keep those restrictions when the fixed gateway starts.
func TestMigratesPolicyOutOfLegacyRoutesJSON(t *testing.T) {
	policyPath := isolatePolicyFile(t)
	runtimeDir := t.TempDir()

	legacy := map[string]persistedRoute{
		"/v1/files": {Target: "http://127.0.0.1:8080", AllowedCIDRs: []string{"192.168.1.0/24"}},
		"/":         {Target: "http://127.0.0.1:8081"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, RoutesFile), data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestManagement(t, runtimeDir)
	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("legacy whitelist was not honoured after upgrade")
	}
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("migration did not create the durable policy file: %v", err)
	}
	// And it must now survive the watchdog + a runtime wipe.
	if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	rebooted := newTestManagement(t, t.TempDir())
	if err := rebooted.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if rebooted.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("migrated whitelist lost after restart")
	}
}

// Migration must never undo an explicit removal: adopting from routes.json is
// only for routes with no decision on record.
func TestMigrationDoesNotResurrectRemovedPolicy(t *testing.T) {
	policyPath := isolatePolicyFile(t)
	runtimeDir := t.TempDir()

	// The admin removed the restriction: recorded as present-but-empty.
	doc := routePolicyDoc{
		Version: routePolicyDocVersion,
		Routes: map[string]persistedRoutePolicy{
			"/v1/files": {AllowedCIDRs: []string{}},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// A stale routes.json still carries the old list.
	legacy := map[string]persistedRoute{
		"/v1/files": {Target: "http://127.0.0.1:8080", AllowedCIDRs: []string{"192.168.1.0/24"}},
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, RoutesFile), legacyData, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestManagement(t, runtimeDir)
	if !m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("migration resurrected a restriction the admin had removed")
	}
}

// A policy file we cannot attribute to ourselves must not be obeyed, and the
// store must say so by name.
func TestUntrustedPolicyFileIsNotLoaded(t *testing.T) {
	policyPath := isolatePolicyFile(t)

	doc := routePolicyDoc{
		Version: routePolicyDocVersion,
		Routes:  map[string]persistedRoutePolicy{"/v1/files": {AllowedCIDRs: []string{"192.168.1.0/24"}}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, data, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(policyPath, 0o666); err != nil {
		t.Fatal(err)
	}

	store := newRoutePolicyStore(policyPath)
	h := store.Health()
	if h.OK {
		t.Fatal("a world-writable policy file was accepted")
	}
	if h.FailedInvariant != invRoutePolicyFileTrusted {
		t.Fatalf("health does not name the failed invariant: %+v", h)
	}
	if _, declared := store.Lookup("/v1/files"); declared {
		t.Fatal("contents of an untrusted policy file were loaded")
	}

	// And a write must NOT "repair" it by overwriting.
	//
	// That was the original behaviour, and it turned this fail-closed refusal
	// into a fail-OPEN one at the next restart: the write replaced an
	// administrator's decision with the empty in-memory set, passed its own
	// trust check on the file it had just created, cleared the degraded state,
	// and reported healthy with no policy. The refusal destroyed the data it
	// existed to protect and then stopped mentioning it.
	err = store.Set("/v1/files", []string{"10.0.0.0/8"})
	if err == nil {
		t.Fatal("a write overwrote a policy file whose contents were never read")
	}
	if !strings.Contains(err.Error(), "never read") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
	if h := store.Health(); h.OK {
		t.Fatal("the store reported healthy after refusing to write; the operator would never " +
			"learn the policy is unreadable")
	}

	// The original bytes must still be on disk for the operator to recover.
	after, rerr := os.ReadFile(policyPath)
	if rerr != nil {
		t.Fatalf("the policy file was removed: %v", rerr)
	}
	if !strings.Contains(string(after), "192.168.1.0/24") {
		t.Fatalf("the administrator's decision was destroyed; file now reads: %s", after)
	}
}

// Repairing the file's trust and restarting is the documented recovery path,
// and it has to actually work — a refusal with no way out is an outage.
func TestRepairingTheFileAndRestartingRecoversThePolicy(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "route-policy.json")
	if err := os.WriteFile(policyPath,
		[]byte(`{"version":1,"routes":{"/v1/files":{"allowed_cidrs":["192.168.1.0/24"]}}}`),
		0o666); err != nil {
		t.Fatal(err)
	}

	degraded := newRoutePolicyStore(policyPath)
	if degraded.Health().OK {
		t.Fatal("precondition: an untrusted file should degrade the store")
	}

	// The operator repairs ownership/permissions and the service restarts.
	if err := os.Chmod(policyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := newRoutePolicyStore(policyPath)

	if h := restarted.Health(); !h.OK {
		t.Fatalf("a repaired file did not recover the store: %+v", h)
	}
	acl, declared := restarted.Lookup("/v1/files")
	if !declared {
		t.Fatal("the recovered store lost the administrator's decision")
	}
	if len(acl.raw) == 0 || acl.raw[0] != "192.168.1.0/24" {
		t.Fatalf("recovered policy = %v, want the original restriction", acl.raw)
	}
}

// A restriction we cannot reconstruct must deny, never silently allow.
func TestUnreconstructablePolicyDeniesInsteadOfAllowing(t *testing.T) {
	policyPath := isolatePolicyFile(t)

	if err := os.WriteFile(policyPath,
		[]byte(`{"version":1,"routes":{"/v1/files":{"allowed_cidrs":["192.168.1.0/999"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestManagement(t, t.TempDir())
	if err := m.CreateRoute(availabilityReRegistration("/v1/files", "http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
		t.Fatal("a route with an unreconstructable restriction was served unrestricted")
	}
	if m.IsIPAllowedForPath("/v1/files", "127.0.0.1") {
		t.Fatal("unreconstructable restriction must not carve out a loopback exemption")
	}
	if h := m.RoutePolicyHealth(); h.UnusableRoutes != 1 {
		t.Fatalf("health did not report the unusable route: %+v", h)
	}
}

// The whole fix rests on absent-vs-empty surviving the production decoder.
// echo's Bind is what the gateway's POST /v1/gateway/routes handler uses.
func TestBindDistinguishesAbsentFromEmptyCIDRList(t *testing.T) {
	cases := []struct {
		name string
		body string
		want policyIntent
	}{
		{"watchdog payload", `{"path":"/v1/files","target":"http://127.0.0.1:8080"}`, policyIntentNone},
		{"explicit null", `{"path":"/v1/files","target":"http://127.0.0.1:8080","allowed_cidrs":null}`, policyIntentNone},
		{"explicit empty", `{"path":"/v1/files","target":"http://127.0.0.1:8080","allowed_cidrs":[]}`, policyIntentClear},
		{"explicit list", `{"path":"/v1/files","target":"http://127.0.0.1:8080","allowed_cidrs":["10.0.0.0/8"]}`, policyIntentSet},
	}
	e := echo.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/gateway/routes", strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			ctx := e.NewContext(req, httptest.NewRecorder())

			var route *model.Route
			if err := ctx.Bind(&route); err != nil {
				t.Fatal(err)
			}
			if got := policyIntentOf(route); got != tc.want {
				t.Fatalf("intent = %v, want %v", got, tc.want)
			}
		})
	}
}

// The Go client used by every service registration marshals model.Route. It
// must be structurally incapable of expressing "clear the policy": that is
// what makes an availability mechanism safe by construction rather than by
// the gateway's good manners alone.
func TestGoClientCannotExpressPolicyClear(t *testing.T) {
	body, err := json.Marshal(&model.Route{Path: "/v1/files", Target: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "allowed_cidrs") {
		t.Fatalf("a registration payload carries a policy field: %s", body)
	}

	var decoded *model.Route
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if policyIntentOf(decoded) != policyIntentNone {
		t.Fatal("a registration payload is read as a policy statement")
	}
}

// Census. model.Route is shared by ten services; when someone adds another
// security field to it, this test fails and forces the same absent-vs-set
// decision instead of letting the field default to "replace with nothing".
func TestRouteModelFieldsAreClassified(t *testing.T) {
	availability := map[string]bool{"Path": true, "Target": true}
	policy := map[string]bool{"AllowedCIDRs": true}

	rt := reflect.TypeOf(model.Route{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !availability[name] && !policy[name] {
			t.Fatalf("model.Route gained field %q: classify it as availability "+
				"(safe to overwrite on every re-registration) or policy (must be "+
				"preserved unless explicitly set) and update policyIntentOf/"+
				"routePolicyStore accordingly", name)
		}
	}
}

// Structural, not clock-based: enforcing a route policy must not re-read or
// re-parse the persisted file. A hot-path file read would be a denial-of-
// service lever on every proxied request.
func TestEnforcementDoesNotTouchTheFilesystem(t *testing.T) {
	policyPath := isolatePolicyFile(t)
	m := newTestManagement(t, t.TempDir())

	if err := m.CreateRoute(&model.Route{
		Path:         "/v1/files",
		Target:       "http://127.0.0.1:8080",
		AllowedCIDRs: []string{"192.168.1.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	// Remove the file entirely: enforcement must be unaffected because it
	// reads the in-memory decision.
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if m.IsIPAllowedForPath("/v1/files", "8.8.8.8") {
			t.Fatal("enforcement depends on reading the policy file per request")
		}
	}
}

// A DEGRADE THAT HAPPENS AFTER BOOT MUST LEAVE SOMETHING TO FIND.
//
// The constructor reports the aggregate standing once (service/management.go),
// but policy can degrade at RUNTIME: Set() is reachable from the management
// API and goes through persistLocked, which degrades on a failed write. Before
// this, that produced a single line about one failed write and nothing about
// the resulting state — and the exported accessor built for exactly this was
// wired to nothing but a test.
//
// WHAT THIS TEST CAN AND CANNOT ASSERT, stated plainly: there is no log-capture
// helper in this package, so it does NOT assert the text of the journal line.
// It asserts the two things underneath it that can actually break:
//
//  1. the aggregate state the logger reads is really there after a runtime
//     degrade, naming the invariant — an empty state would make the new log
//     line empty too;
//  2. reading that aggregate from inside persistLocked does not DEADLOCK.
//     persistLocked runs under the write lock and sync.RWMutex is not
//     reentrant, so calling Health() there would block forever. This test
//     drives exactly that path; if the lock-free variant regressed to Health()
//     it would hang here rather than fail politely.
func TestRuntimeDegradeLeavesTheAggregateStateBehind(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "route-policy.json")

	store := newRoutePolicyStore(policyPath)
	if h := store.Health(); !h.OK {
		t.Fatalf("precondition: a fresh box should be healthy, got %+v", h)
	}
	if err := store.Set("/v1/files", []string{"192.168.1.0/24"}); err != nil {
		t.Fatalf("precondition: first write should succeed: %v", err)
	}

	// Take write permission away from the directory so the atomic rewrite
	// cannot create its temporary file. This is the runtime failure the
	// aggregate was never reported for.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := store.Set("/v1/files", []string{"10.0.0.0/8"})
	if err == nil {
		t.Skip("the filesystem let us write to a read-only directory (running as root?); " +
			"this test cannot produce a runtime persist failure here")
	}

	h := store.Health()
	if h.OK {
		t.Fatal("the store reported healthy after a failed runtime write; nothing would be journaled " +
			"and an operator would never learn the policy is no longer durable")
	}
	if h.FailedInvariant != invRoutePolicyDurable {
		t.Fatalf("failed invariant = %q, want %q — the journal line names this field",
			h.FailedInvariant, invRoutePolicyDurable)
	}
	if h.Detail == "" {
		t.Fatal("no detail recorded; the journal line would name an invariant with no cause")
	}
	if h.Path != policyPath {
		t.Fatalf("health path = %q, want %q", h.Path, policyPath)
	}

	// The rollback in Set must have kept memory and disk in agreement, so the
	// counts the journal line reports describe the policy actually in force.
	if h.RestrictedRoutes != 1 {
		t.Fatalf("restricted routes = %d, want 1 (the decision that is still in force)", h.RestrictedRoutes)
	}
}
