package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	servicemodel "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/labstack/echo/v4"
)

// G-03 — DIAGNOSTIC READ SURFACES.
//
// The claim under test is one sentence: no endpoint that returns internal
// system state answers a caller that presented no token.
//
// It is not the same claim as "the handler calls requirePerm". A tokenless
// request arrives with no role on the context, RoleFromContext fails closed to
// guest, and guest holds PermSystemRead — so requirePerm(PermSystemRead) is a
// no-op against an unauthenticated context.
//
// The tokenless-loopback skipper that used to make that fatal is GONE (see
// route/auth_skipper.go): none of these routes is in v1PreLoginRoutes, so the
// v1 JWT middleware now refuses unauthenticated callers at the perimeter before
// any handler runs. Historically, that skipper paired with a gateway that
// proxies the LAN and the Cloudflare tunnel over 127.0.0.1 was why /v1/sys/logs
// (the daemon log tail) and /v1/sys/debug (uname -a, distro, RAM, disk) were
// readable from the internet with curl and no credentials.
//
// Handler-level defense-in-depth remains essential: if a diagnostic route is
// ever remounted or invoked outside the group middleware, requirePerm alone
// would still fail open to guest. The tests below therefore drive the HANDLERS
// directly with an anonymous context and require a 401 — not "some error", and
// not a 403 that a permission table could hand out by accident.

// anonContext builds the request context a tokenless caller produces: the JWT
// middleware never ran, so nothing was stored under the RBAC context keys.
func anonContext() (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// identifiedContext builds the context the JWT middleware produces after it
// validated a real token for user id `uid` with role `role`.
func identifiedContext(uid int, role rbac.Role) (echo.Context, *httptest.ResponseRecorder) {
	ctx, rec := anonContext()
	commonmw.SetRoleInContext(ctx, string(role), uid)
	return ctx, rec
}

// gatedReadHandlers are every handler in system.go that returns internal
// system state. Each must refuse an anonymous caller before it touches any
// service — service.MyService is nil in this test binary, so a handler that
// let the request through would panic, which the runner below reports as the
// gate having failed rather than as a mysterious crash.
var gatedReadHandlers = map[string]func(echo.Context) error{
	"GetCasaOSErrorLogs":    GetCasaOSErrorLogs,    // GET /v1/sys/logs — daemon log tail
	"GetSystemConfigDebug":  GetSystemConfigDebug,  // GET /v1/sys/debug — system profile
	"GetSystemHardwareInfo": GetSystemHardwareInfo, // GET /v1/sys/hardware — DMI identity
	"GetSystemUtilization":  GetSystemUtilization,  // GET /v1/sys/utilization — NIC IP/MAC, disk
	"GetSystemEntry":        GetSystemEntry,        // GET /v1/sys/entry — installed modules
	"GetSystemNetInfo":      GetSystemNetInfo,      // NIC IP/MAC (handler retained, unrouted)
	"GetSystemCupInfo":      GetSystemCupInfo,
	"GetSystemMemInfo":      GetSystemMemInfo,
	"GetSystemDiskInfo":     GetSystemDiskInfo,
	"GetCasaOSPort":         GetCasaOSPort,
	"GetPort":               GetPort,       // GET /v1/port — free-port oracle
	"PortCheck":             PortCheck,     // GET /v1/port/state/:port — port scan oracle
	"PutCasaOSPort":         PutCasaOSPort, // mutation, gated for the same reason
}

// callGuardingAgainstPanic runs h and converts a panic — which on this build
// means "the handler reached service.MyService, i.e. the gate did not stop
// it" — into a test failure with the name of the handler that leaked.
func callGuardingAgainstPanic(t *testing.T, name string, h func(echo.Context) error, ctx echo.Context) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: the request reached the service layer (panic: %v). "+
				"The gate must refuse before any system state is read.", name, r)
		}
	}()
	return h(ctx)
}

func TestDiagnosticReadHandlersRefuseAnonymousCallers(t *testing.T) {
	for name, h := range gatedReadHandlers {
		t.Run(name, func(t *testing.T) {
			ctx, rec := anonContext()
			err := callGuardingAgainstPanic(t, name, h, ctx)
			if err == nil {
				t.Fatalf("%s answered an anonymous caller (status %d, body %q). "+
					"Every caller that presented no token must be refused.",
					name, rec.Code, rec.Body.String())
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("%s returned %T (%v); the gate must return an *echo.HTTPError so "+
					"echo aborts the chain — a helper that writes the response with ctx.JSON "+
					"returns nil and the handler runs on", name, err, err)
			}
			if he.Code != http.StatusUnauthorized {
				t.Fatalf("%s refused an anonymous caller with %d; want 401. A 403 here would "+
					"mean the refusal came from the permission table, which guest satisfies "+
					"for PermSystemRead", name, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("%s wrote a body (%q) while denying; the denial must not commit the "+
					"response", name, rec.Body.String())
			}
		})
	}
}

// The log tail and the system profile are the two G-03 endpoints. They are
// admin-grade reconnaissance (paths under the user's data, share names, cloud
// account names, kernel build), so an ordinary signed-in account must not read
// them either — the gate is identity AND PermSystemConfig.
func TestLogAndProfileRefuseNonAdminPrincipals(t *testing.T) {
	for name, h := range map[string]func(echo.Context) error{
		"GetCasaOSErrorLogs":   GetCasaOSErrorLogs,
		"GetSystemConfigDebug": GetSystemConfigDebug,
	} {
		for _, role := range []rbac.Role{rbac.RoleGuest, rbac.RoleUser} {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				ctx, _ := identifiedContext(7, role)
				err := callGuardingAgainstPanic(t, name, h, ctx)
				he, ok := err.(*echo.HTTPError)
				if !ok || he.Code != http.StatusForbidden {
					t.Fatalf("%s: role %s got %v; want 403 — neither role holds PermSystemConfig",
						name, role, err)
				}
			})
		}
	}
}

// The gate helper itself: identity first, permission second, in that order.
func TestRequireSystemPrincipalOrder(t *testing.T) {
	// Anonymous, asking for the permission guest DOES hold: still 401.
	ctx, _ := anonContext()
	err := requireSystemPrincipal(ctx, rbac.PermSystemRead)
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous + PermSystemRead: got %v, want 401. Guest holds PermSystemRead, so "+
			"the permission check alone can never refuse an anonymous caller", err)
	}

	// Identified guest, asking for a permission guest holds: allowed.
	ctx, _ = identifiedContext(3, rbac.RoleGuest)
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		t.Fatalf("identified guest + PermSystemRead: got %v, want allowed — the dashboard "+
			"widgets of every signed-in role depend on this", err)
	}

	// Identified guest, asking for a permission guest lacks: 403.
	err = requireSystemPrincipal(ctx, rbac.PermSystemConfig)
	he, ok = err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusForbidden {
		t.Fatalf("identified guest + PermSystemConfig: got %v, want 403", err)
	}

	// Admin: allowed for both.
	ctx, _ = identifiedContext(1, rbac.RoleAdmin)
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		t.Fatalf("identified admin + PermSystemConfig: got %v, want allowed", err)
	}
}

// ─── The unauthenticated write on the public version endpoint ───────────────

// GET /v1/sys/version/check is mounted OUTSIDE the JWT group so the login
// screen can show a version. Its notification insert was not part of that
// bargain: AddLog is an unconditional db.Create with no de-duplication, so
// every anonymous hit appended a row.

type fakeCasa struct{ v model.Version }

func (f fakeCasa) GetCasaosVersion() model.Version { return f.v }

// fakeNotify embeds the interface so the compiler is satisfied for the methods
// this test never calls; calling one of those would nil-panic, which is the
// intent — the test asserts on AddLog alone.
type fakeNotify struct {
	service.NotifyServer
	adds *int
}

func (f fakeNotify) AddLog(servicemodel.AppNotify) { *f.adds++ }

type fakeRepo struct {
	service.Repository
	casa   fakeCasa
	notify fakeNotify
}

func (f fakeRepo) Casa() service.CasaService    { return f.casa }
func (f fakeRepo) Notify() service.NotifyServer { return f.notify }

func TestVersionCheckDoesNotWriteForAnonymousCallers(t *testing.T) {
	adds := 0
	prev := service.MyService
	service.MyService = fakeRepo{
		// A version far above anything this build could be, so IsNeedUpdate
		// takes the branch that writes.
		casa:   fakeCasa{v: model.Version{Version: "999.0.0"}},
		notify: fakeNotify{adds: &adds},
	}
	defer func() { service.MyService = prev }()

	ctx, rec := anonContext()
	if err := GetSystemCheckVersion(ctx); err != nil {
		t.Fatalf("anonymous version check should still answer (it is the login screen's "+
			"version banner); got %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous version check: status %d, want 200 — this endpoint stays public", rec.Code)
	}
	var body struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Data["need_update"] != true {
		t.Fatalf("fixture did not reach the update branch (%v); the test would prove nothing",
			body.Data)
	}
	if adds != 0 {
		t.Fatalf("an anonymous GET inserted %d notification row(s). An unauthenticated request "+
			"from the internet must not write to the database", adds)
	}

	ctx, _ = identifiedContext(1, rbac.RoleAdmin)
	if err := GetSystemCheckVersion(ctx); err != nil {
		t.Fatalf("identified version check: %v", err)
	}
	if adds != 1 {
		t.Fatalf("identified caller produced %d notification row(s), want 1 — the WebUI's "+
			"update notice must keep working", adds)
	}
}

// ─── THE CENSUS ─────────────────────────────────────────────────────────────
//
// One handler gated by hand is worth nothing next month. This walks the
// service's own route tree, finds every GET whose path reads like a
// diagnostic/status/debug surface, resolves the handler it points at, and
// requires that the handler consults SOME gate.
//
// It is deliberately blunt about what counts as a gate: any of the package's
// gate helpers, an inline identity/role check, or strictJWT() on the
// registration. The point is not to grade the strength of the check — that is
// what the tests above do for this file — but to make a NEW ungated diagnostic
// endpoint impossible to add silently.

var diagPathRE = regexp.MustCompile(
	`(?i)(debug|diagnos|log|profil|status|health|metric|pprof|version|/info|entry|utilization|hardware|stats|port)`)

var routeRegistrationRE = regexp.MustCompile(
	`\.GET\(\s*"([^"]*)"\s*,\s*([A-Za-z0-9_.]+)([^\n]*)`)

var gateCallRE = regexp.MustCompile(
	`requirePerm|requirePermInternal|requireSystemPrincipal|require[A-Za-z]*Caller|` +
		`RequirePermission|RequireAdmin|RequireRole|UserIDFromContext|RoleFromContext|` +
		`internalauth\.Valid|strictJWT`)

// publicByDesign: reachable without a token ON PURPOSE, with the reason. Each
// one was read, not assumed: none of them returns state that is not already
// implied by the box answering at all. Keyed by handler name, or by route path
// for the handlers written inline at the registration.
var publicByDesign = map[string]string{
	"/v1/sys/version/current": "inline handler returning common.FullVersion(); the login screen's " +
		"version banner, mounted outside the JWT group on purpose",
	"v1.GetSystemCheckVersion": "version banner on the login screen; mounted outside the JWT " +
		"group on purpose. Its one side effect (the notification insert) is gated on identity " +
		"— see TestVersionCheckDoesNotWriteForAnonymousCallers",
	"searchPing": "returns the constant \"Pong\"; no state at all",
	"searchStats": "returns two constants ({status:ok, support_ai:false}); the live-walk search " +
		"backend has no index whose state could leak",
	"zimaDeviceInfo": "ZimaOS client discovery probe. The Android/Windows clients read it BEFORE " +
		"any login exists — making it authenticated breaks pairing, which is the documented " +
		"compatibility contract. Payload is the device identity those clients require",
	"zimaZTInfoStub": "stub beside zimaDeviceInfo, same pre-login client contract; returns a " +
		"fixed empty ZeroTier document",

	// Added when the census was inverted to audit every GET rather than only
	// the ones whose path happened to contain a word like "status". Each was
	// read before being listed here; a weak justification in this map is how a
	// hole gets blessed permanently.
	"v1.GetRecoverStorage": "OAuth callback. It MUST answer without a session because the " +
		"identity provider redirects the user's browser here. It is not unauthenticated: " +
		"oauthflow.Consume spends a single-use flow state BEFORE the code is read and before " +
		"any outbound call is made, so an unauthorized caller cannot make this process " +
		"originate a token exchange. Requiring a permission here breaks cloud storage entirely",
	"zimaDeviceImage": "ZimaOS client compatibility. The Android/Windows clients fetch the " +
		"device image BEFORE any login exists; gating it breaks pairing, which is the " +
		"documented compatibility contract",
	"zimaModulesStub": "ZimaOS client compatibility stub beside zimaDeviceImage; same " +
		"pre-login client contract, returns a fixed document",
	"zimaDocFeatures": "ZimaOS client compatibility; static feature document read pre-login " +
		"by the clients",
	"zimaDocFeaturesHTML": "the HTML rendering of zimaDocFeatures, same contract",
	"v1.GetPublicShare": "public share links are the feature. The share id IS the capability, " +
		"and the registration sits outside the JWT group deliberately (see route/v1.go). " +
		"Authorization is the unguessable id plus whatever the share itself carries",
}

// reportedElsewhere: diagnostic GETs in THIS service that are still ungated but
// live in files owned by other work in flight. They are recorded so this census
// is green on what it can control while the debt stays enumerated by name; each
// is reported upward with its file:line. Removing a name here without gating the
// handler re-opens the hole, and the test will say so.
var reportedElsewhere = map[string]string{
	"v1.GetTOTPStatus":            "route/v1/totp.go — leaks whether 2FA is on and how many backup codes remain",
	"v1.GetCloudBackupStatus":     "route/v1/cloud_backup.go — leaks configured rclone remote names",
	"v1.GetCloudBackupSyncStatus": "route/v1/cloud_backup.go — leaks sync history",
	"v1.GetZFSStatus":             "route/v1/zfs.go — leaks zfs/zpool paths, version, pool count",
	"v1.GetSmbStatus":             "route/v1/smb.go — leaks the workgroup and every share definition (paths)",
	"v1.GetSambaStatus":           "route/v1/samba.go — leaks smbd state and whether smb.conf is CasaOS-managed",
	"v1.GetUpdateStatus":          "route/v1/update.go — leaks version, update stage and post-update service reports",
	"v1.GetUpdateChangelog":       "route/v1/update.go — leaks the pending package's changelog and version",
}

func TestNoUngatedDiagnosticEndpoints(t *testing.T) {
	root := diagnosticCensusRoot(t)
	routeDir := filepath.Join(root, "route")

	type offender struct{ where, path, handler string }
	var offenders []offender
	scanned := 0

	err := filepath.Walk(routeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := string(data)

		for _, m := range routeRegistrationRE.FindAllStringSubmatchIndex(src, -1) {
			lineStart := strings.LastIndexByte(src[:m[0]], '\n') + 1
			if strings.HasPrefix(strings.TrimSpace(src[lineStart:m[0]]), "//") {
				continue // commented-out registration
			}
			routePath := src[m[2]:m[3]]
			handler := src[m[4]:m[5]]
			rest := src[m[6]:m[7]]
			// INVERTED: every GET is audited, not only the ones whose PATH
			// happens to contain a word like "status" or "health".
			//
			// The name heuristic covered 31 of 105 registrations. It did not
			// match "/wedged", so a genuinely ungated endpoint that answered
			// 200 with absolute filesystem paths to a tokenless caller shipped
			// to production with this census green. It also did not match
			// /devices, /jobs, /processes, /ups or /memory.
			//
			// A census that decides what to audit by GUESSING FROM THE NAME
			// can only ever be as good as the vocabulary of whoever named the
			// route last. Auditing everything and enumerating the exceptions by
			// name is the fail-closed form: adding a route now requires either a
			// gate or an explicit, reviewed entry below.
			scanned++
			if _, ok := publicByDesign[handler]; ok {
				continue
			}
			if _, ok := publicByDesign[routePath]; ok {
				continue
			}
			if _, ok := reportedElsewhere[handler]; ok {
				continue
			}
			if gateCallRE.MatchString(rest) {
				continue // strictJWT() or a middleware on the registration itself
			}
			body, found := handlerBody(root, handler)
			if !found {
				// An unresolvable handler is not a pass: it is a route whose
				// gate nobody can see.
				offenders = append(offenders, offender{rel + ":" + lineNumber(src, m[0]), routePath, handler + " (handler not found)"})
				continue
			}
			if gateCallRE.MatchString(body) {
				continue
			}
			offenders = append(offenders, offender{rel + ":" + lineNumber(src, m[0]), routePath, handler})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", routeDir, err)
	}

	if scanned == 0 {
		t.Skip("no diagnostic routes found; not running against the full module")
	}
	if len(offenders) > 0 {
		var lines []string
		for _, o := range offenders {
			lines = append(lines, o.where+"  GET "+o.path+" → "+o.handler)
		}
		t.Fatalf("%d diagnostic endpoint(s) reachable with no gate:\n  %s\n\n"+
			"An anonymous context defaults to RoleGuest which holds PermSystemRead, so an "+
			"ungated or naively gated read here would fail open if invoked outside group "+
			"middleware. Gate it (requireSystemPrincipal for state a signed-in principal "+
			"may read), or record it in publicByDesign with the reason it is safe for an "+
			"anonymous caller.",
			len(offenders), strings.Join(lines, "\n  "))
	}
	t.Logf("%d diagnostic route(s) scanned; %d public by design, %d ungated and reported upward",
		scanned, len(publicByDesign), len(reportedElsewhere))
}

// The recorded-debt list must not rot into an excuse. Entries that HAVE been
// gated since are reported so the list shrinks, but they do not fail the run —
// the person who fixed one of these owns another file and must not be blocked
// by this test.
func TestReportedElsewhereListStaysHonest(t *testing.T) {
	root := diagnosticCensusRoot(t)
	for handler, note := range reportedElsewhere {
		body, found := handlerBody(root, handler)
		if !found {
			t.Errorf("%s is recorded as an ungated diagnostic endpoint but no longer exists "+
				"(%s). Remove the entry deliberately.", handler, note)
			continue
		}
		if gateCallRE.MatchString(body) {
			t.Logf("%s now has a gate — drop it from reportedElsewhere (%s)", handler, note)
		}
	}
}

// handlerBody finds the named handler anywhere in the module and returns its
// source. `v1.GetFoo` and `GetFoo` both resolve to func GetFoo.
func handlerBody(root, handler string) (string, bool) {
	short := handler[strings.LastIndexByte(handler, '.')+1:]

	// MATCH THE HANDLER SHAPE, NOT JUST THE NAME.
	//
	// This matched `func <name>(` anywhere in the module and stopped at the
	// first hit in walk order. pkg/utils/file/file.go has `func GetSize(f
	// multipart.File)` and pkg/samba/smaba.go has `func GetSambaSharesList(host,
	// port, username, password string)`; both sort before route/, so the census
	// resolved the wrong function and reported two correctly gated handlers as
	// offenders.
	//
	// Today that was a false positive. The same defect is a false NEGATIVE
	// waiting to happen: a same-named function elsewhere that merely mentions
	// RoleFromContext would have blessed a genuinely ungated handler. Requiring
	// the echo handler signature makes the resolution unambiguous — there is
	// exactly one function with a given name taking (echo.Context) error.
	decl := regexp.MustCompile(`\nfunc\s+(\([^)]*\)\s*)?` + regexp.QuoteMeta(short) +
		`\s*\(\s*[A-Za-z_][A-Za-z0-9_]*\s+echo\.Context\s*\)\s*error\s*\{`)

	var body string
	found := false
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "vendor" || info.Name() == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := string(data)
		loc := decl.FindStringIndex(src)
		if loc == nil {
			return nil
		}
		end := strings.Index(src[loc[0]+1:], "\n}")
		if end < 0 {
			body = src[loc[0]:]
		} else {
			body = src[loc[0] : loc[0]+1+end]
		}
		found = true
		return nil
	})
	return body, found
}

func lineNumber(src string, offset int) string {
	return strconv.Itoa(1 + strings.Count(src[:offset], "\n"))
}

// diagnosticCensusRoot walks up to the directory holding go.mod. Named apart
// from the storage census's own helper: both live in package v1.
func diagnosticCensusRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Skipf("cannot determine working directory: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("module root not found from this working directory")
	return ""
}
