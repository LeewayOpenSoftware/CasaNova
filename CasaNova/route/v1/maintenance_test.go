package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/maintenance"
	"github.com/labstack/echo/v4"
)

// Tests for the maintenance HTTP layer.
//
// What is worth pinning here is not "does it return JSON" but the handful of
// properties that keep a root-level deleter from firing for the wrong caller or
// on the wrong thing: the gate, the fact that no path can be expressed in the
// request, and the id validation that stands between the network and the plan.

type maintenanceHandler struct {
	name   string
	method string
	fn     func(echo.Context) error
}

func maintenanceHandlers() []maintenanceHandler {
	return []maintenanceHandler{
		{"POST /scan", http.MethodPost, PostMaintenanceScan},
		{"GET /scan", http.MethodGet, GetMaintenanceScan},
		{"POST /clean", http.MethodPost, PostMaintenanceClean},
		{"GET /clean", http.MethodGet, GetMaintenanceClean},
		{"GET /manifest", http.MethodGet, GetMaintenanceManifests},
		{"GET /manifest/:id", http.MethodGet, GetMaintenanceManifest},
	}
}

func newMaintenanceCtx(method, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// A tokenless caller must get 401 from every endpoint, the read-only ones
// included. The gateway proxies the whole LAN over 127.0.0.1, so "it came from
// loopback" proves nothing, and guest holds PermSystemRead on purpose — the
// login check is what actually separates a user from the internet.
func TestMaintenanceHandlersRefuseAnonymousCallers(t *testing.T) {
	for _, h := range maintenanceHandlers() {
		t.Run(h.name, func(t *testing.T) {
			ctx, rec := newMaintenanceCtx(h.method, "{}")
			err := h.fn(ctx)

			var he *echo.HTTPError
			if !errors.As(err, &he) {
				t.Fatalf("%s: expected an echo.HTTPError, got %v (body %q)", h.name, err, rec.Body.String())
			}
			if he.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got status %d, want 401", h.name, he.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("%s: handler wrote a body for an anonymous caller: %q", h.name, rec.Body.String())
			}
		})
	}
}

// A logged-in non-admin must be refused, and the handler must not run.
//
// These tests were written when requirePerm answered a denial by WRITING the
// 403 and returning nil, which is why the handlers carry a belt-and-braces
// maintenanceGateOpen line. That defect is fixed — requirePerm now returns an
// *echo.HTTPError and the caller's `if err != nil { return err }` aborts on its
// own (see rbac_gate_test.go) — so what this pins now is the outcome the module
// actually needs: refusal, and no side effect.
func TestMaintenanceHandlersRefuseNonAdmins(t *testing.T) {
	for _, h := range maintenanceHandlers() {
		t.Run(h.name, func(t *testing.T) {
			ctx, rec := newMaintenanceCtx(h.method, "{}")
			commonmw.SetRoleInContext(ctx, "user", 7)

			err := h.fn(ctx)
			if err == nil {
				t.Fatalf("%s: handler returned nil for a non-admin", h.name)
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("%s: want *echo.HTTPError, got %T", h.name, err)
			}
			if he.Code != http.StatusForbidden {
				t.Fatalf("%s: got status %d, want 403", h.name, he.Code)
			}
			// The handler must not have produced any output of its own:
			// anything here means execution continued past the gate.
			if rec.Body.Len() != 0 {
				t.Fatalf("%s: handler wrote a body despite being denied — %q", h.name, rec.Body.String())
			}
		})
	}
}

// requirePerm must hand its refusal back rather than commit it. This is the
// property every handler in the module leans on, pinned here too because the
// module deletes files as root and a regression would be silent.
func TestMaintenanceGateRefusalIsReturnedNotWritten(t *testing.T) {
	ctx, rec := newMaintenanceCtx(http.MethodGet, "")
	commonmw.SetRoleInContext(ctx, "user", 7)

	err := requirePerm(ctx, rbac.PermSystemConfig)
	if err == nil {
		t.Fatal("requirePerm allowed a non-admin through PermSystemConfig")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("requirePerm wrote the denial itself: %q", rec.Body.String())
	}
}

// An admin with no run in flight gets a well-formed idle answer: no invented
// run id, no null arrays for the client to guard against.
func TestGetMaintenanceCleanIdleShape(t *testing.T) {
	maintRunMu.Lock()
	maintRunning, maintRunState, maintRunErr = false, "idle", ""
	maintRunReason, maintRunID, maintRunCategory = "", "", ""
	maintRunTotal, maintRunResult = 0, nil
	maintRunMu.Unlock()

	ctx, rec := newMaintenanceCtx(http.MethodGet, "")
	commonmw.SetRoleInContext(ctx, "admin", 1)
	if err := GetMaintenanceClean(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a flat JSON object: %v", err)
	}
	// Flat, with no model.Result envelope: the same exception /v1/sys/diagnosis
	// already makes. A "data" key here would mean the envelope crept back in.
	if _, wrapped := body["data"]; wrapped {
		t.Fatal("response is wrapped in an envelope")
	}
	if body["state"] != "idle" {
		t.Fatalf("state = %v, want idle", body["state"])
	}
	if body["run_id"] != "" {
		t.Fatalf("run_id = %v, want empty for an idle module", body["run_id"])
	}
	for _, key := range []string{"failed", "skipped"} {
		if _, ok := body[key].([]any); !ok {
			t.Fatalf("%s = %#v, want an array (never null)", key, body[key])
		}
	}
}

// The request type is the reason path traversal is not a bug class this API can
// have. This fails the moment somebody adds a field that carries a filesystem
// string, which is exactly when a human should be looking.
func TestMaintenanceCleanRequestCannotCarryAPath(t *testing.T) {
	want := map[string]string{
		"scan_token": "string",
		"category":   "string",
		"item_ids":   "[]string",
	}
	rt := reflect.TypeOf(maintenanceCleanRequest{})
	if rt.NumField() != len(want) {
		t.Fatalf("maintenanceCleanRequest has %d fields, want exactly %d", rt.NumField(), len(want))
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		kind, known := want[tag]
		if !known {
			t.Fatalf("unexpected field %q on the clean request — a client must never be able to name a path", tag)
		}
		if f.Type.String() != kind {
			t.Fatalf("field %q is %s, want %s", tag, f.Type, kind)
		}
	}
}

func TestMaintenanceCleanIDs(t *testing.T) {
	valid := "0123456789abcdef"
	other := "fedcba9876543210"

	if _, err := maintenanceCleanIDs(nil); err == nil {
		t.Error("an empty selection was accepted")
	}
	if _, err := maintenanceCleanIDs([]string{}); err == nil {
		t.Error("an empty selection was accepted")
	}

	// Anything that is not the exact shape the scan mints is refused before it
	// can reach the plan lookup.
	for _, bad := range []string{
		"",
		"../../../etc/passwd",
		"/var/lib/casanova",
		"0123456789ABCDEF", // uppercase: not what itemID emits
		"0123456789abcde",  // one short
		"0123456789abcdef0",
		"0123456789abcde/",
		"0123456789abcde\x00",
	} {
		if _, err := maintenanceCleanIDs([]string{valid, bad}); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
	}

	// Duplicates collapse, order survives. A repeated id would be planned twice
	// and the second unlink would come back ENOENT — reported to the user as a
	// failure on a file we had just deleted successfully.
	got, err := maintenanceCleanIDs([]string{other, valid, other, valid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []string{other, valid}) {
		t.Fatalf("got %v, want [%s %s]", got, other, valid)
	}

	tooMany := make([]string, maintenanceMaxItemIDs+1)
	for i := range tooMany {
		tooMany[i] = valid
	}
	if _, err := maintenanceCleanIDs(tooMany); err == nil {
		t.Error("an oversized selection was accepted")
	}
}

// An unknown category never reaches the engine: it is not in the compiled
// catalogue, so there is nothing to interpret and nothing to walk.
func TestPostMaintenanceCleanRejectsAnUnknownCategory(t *testing.T) {
	for _, category := range []string{
		"",
		"nope",
		"/var/log",
		"../casanova-update-packages",
		"CASANOVA-UPDATE-PACKAGES",
	} {
		body := `{"scan_token":"x","category":"` + category + `","item_ids":["0123456789abcdef"]}`
		ctx, rec := newMaintenanceCtx(http.MethodPost, body)
		commonmw.SetRoleInContext(ctx, "admin", 1)

		err := PostMaintenanceClean(ctx)
		var he *echo.HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("category %q: expected an echo.HTTPError, got %v (body %q)", category, err, rec.Body.String())
		}
		// 503 is acceptable only if the module could not be built at all on
		// this host; anything else must be the 400.
		if he.Code == http.StatusServiceUnavailable {
			t.Skipf("maintenance engine unavailable on this host: %v", he.Message)
		}
		if he.Code != http.StatusBadRequest {
			t.Fatalf("category %q: got status %d, want 400", category, he.Code)
		}
	}
}

// Every category the catalogue publishes must be one the UI already knows how
// to name and confirm. A key added on one side and not the other ships a screen
// that asks the user to approve something described only in English.
func TestCatalogueKeysAreTheOnesTheClientSends(t *testing.T) {
	want := map[string]bool{
		maintenance.CatUpdatePackages: true,
		maintenance.CatUpgradeBackups: true,
		maintenance.CatRotatedLogs:    true,
		maintenance.CatOrphanJournal:  true,
		maintenance.CatAptCache:       true,
		maintenance.CatScratch:        true,
	}
	got := maintenance.Categories()
	if len(got) != len(want) {
		t.Fatalf("catalogue has %d categories, want %d", len(got), len(want))
	}
	for _, c := range got {
		if !want[c.Key] {
			t.Errorf("category %q is not one the client was built for", c.Key)
		}
		if maintenance.Lookup(c.Key) == nil {
			t.Errorf("category %q is listed but does not resolve", c.Key)
		}
	}
}

func TestMaintenanceRunErrText(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
	}{
		{
			"a preflight refusal keeps its own code",
			&maintenance.PreflightError{Code: maintenance.BlockDpkgLocked, Reason: "apt or dpkg is running"},
			maintenance.BlockDpkgLocked,
		},
		{"a stale token asks for a re-analysis", maintenance.ErrStaleScan, maintenance.BlockStaleScan},
		{"no scan at all asks for the same", maintenance.ErrNoScan, maintenance.BlockStaleScan},
		{"an unresolvable item id is also a stale plan", maintenance.ErrUnknownItem, maintenance.BlockStaleScan},
		{"a hard-limit abort carries no code", maintenance.ErrPlanTooLarge, ""},
		{"anything else carries no code", errors.New("boom"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, text := maintenanceRunErrText(c.err)
			if reason != c.wantReason {
				t.Errorf("reason = %q, want %q", reason, c.wantReason)
			}
			if text == "" {
				t.Error("a failure with no sentence leaves the user with nothing to read")
			}
		})
	}
}

func TestMaintenanceHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                 "0 B",
		-4096:             "0 B", // something else wrote while we deleted
		512:               "512 B",
		1024:              "1.0 KB",
		217 * 1000 * 1000: "206.9 MB",
		1 << 30:           "1.0 GB",
	}
	for in, want := range cases {
		if got := maintenanceHumanBytes(in); got != want {
			t.Errorf("maintenanceHumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// The client tells "my run finished" from "the previous run is still on screen"
// by comparing this id, so two runs must never share one.
func TestMaintenanceNewRunIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := maintenanceNewRunID()
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
}
