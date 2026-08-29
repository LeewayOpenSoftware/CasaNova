package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagehealth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagemaint"
	v1 "github.com/LeewayOpenSoftware/CasaNova/route/v1"
	"github.com/labstack/echo/v4"
)

// THE LOOPBACK BYPASS, probed against the router this service actually serves.
//
// The gateway reverse-proxies every LAN request AND every internet request that
// arrives over the tunnel to this backend over 127.0.0.1, with no auth of its
// own. The v1 group's JWT skipper is `loopback.IsLoopback(c) &&
// Authorization == ""`. So an unauthenticated request from anywhere on the
// internet is indistinguishable here from a genuine internal call, and lands in
// the handler with the guest role. That is not a hypothetical: it is the
// confirmed bypass that reached DELETE /v1/samba/shares/:id and POST /v1/notify.
//
// This test asks the only question that matters for a subsystem that formats
// disks: can that caller cause an effect? It asserts on the EFFECT — the engine
// was never invoked, and the device inventory never appeared in a response body
// — and not on the status code. A gate in this service has already answered 403
// and let the handler run to completion.
//
// It lives in package route rather than package v1 because it drives the real
// InitV1Router(); package v1 cannot import package route without a cycle.
func TestTokenlessLoopbackCallerCannotReachStorageMaintenance(t *testing.T) {
	fx := newLoopbackStorageFixture(t)
	router := InitV1Router()

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/maintenance/storage/devices", ""},
		{http.MethodGet, "/v1/maintenance/storage/capabilities", ""},
		{http.MethodGet, "/v1/maintenance/storage/health", ""},
		{http.MethodGet, "/v1/maintenance/storage/jobs", ""},
		{http.MethodPost, "/v1/maintenance/storage/format/prepare",
			`{"device":{"serial":"CNLOOPBACKPROBE42"},"filesystem":"ext4"}`},
		{http.MethodPost, "/v1/maintenance/storage/format/plan-1/execute", `{}`},
		{http.MethodPost, "/v1/maintenance/storage/sanitize/prepare",
			`{"device":{"serial":"CNLOOPBACKPROBE42"},"method":"overwrite"}`},
		{http.MethodPost, "/v1/maintenance/storage/sanitize/plan-1/execute", `{}`},
		{http.MethodPost, "/v1/maintenance/storage/jobs/job-1/cancel", `{}`},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		// The exact shape of the attack: RemoteAddr is loopback (the gateway's
		// hop) and there is no Authorization header at all.
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
			t.Errorf("%s %s: answered %d to a tokenless loopback caller",
				tc.method, tc.path, rec.Code)
		}
		// The effect probe. This serial exists only in this test's fixture; if
		// it reaches a response body, a read handler ran and published the map
		// of which disks are disposable and which hold the system.
		if strings.Contains(rec.Body.String(), "CNLOOPBACKPROBE42") {
			t.Errorf("%s %s: leaked the device inventory to a tokenless loopback caller: %s",
				tc.method, tc.path, rec.Body.String())
		}
	}

	if n := fx.engine.calls(); n != 0 {
		t.Fatalf("a tokenless loopback caller reached the storage-maintenance engine %d time(s)", n)
	}
}

// The routes must also be REACHABLE, or the test above passes vacuously against
// nine 404s. Registering the handlers and forgetting the gateway prefix is a
// documented failure mode in this codebase (container log streaming 404'd for
// months with a hardened handler sitting unused).
func TestStorageMaintenanceRoutesAreRegistered(t *testing.T) {
	e, ok := InitV1Router().(*echo.Echo)
	if !ok {
		t.Fatalf("router is %T", InitV1Router())
	}
	want := map[string]bool{
		"GET /v1/maintenance/storage/devices":               false,
		"GET /v1/maintenance/storage/capabilities":          false,
		"GET /v1/maintenance/storage/health":                false,
		"GET /v1/maintenance/storage/jobs":                  false,
		"POST /v1/maintenance/storage/format/prepare":       false,
		"POST /v1/maintenance/storage/format/:id/execute":   false,
		"POST /v1/maintenance/storage/sanitize/prepare":     false,
		"POST /v1/maintenance/storage/sanitize/:id/execute": false,
		"POST /v1/maintenance/storage/jobs/:id/cancel":      false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("%s is not registered; the loopback probe would pass against a 404", key)
		}
	}
}

// ---------------------------------------------------------------- fixture

type loopbackStorageFixture struct {
	engine *countingEngine
}

// countingEngine records every destructive entry point. It returns errors from
// nothing: the point is that it is never reached at all.
type countingEngine struct {
	mu sync.Mutex
	n  int
}

func (c *countingEngine) bump() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingEngine) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingEngine) PrepareFormat(context.Context, v1.StorageFormatRequest) (v1.StoragePlan, error) {
	c.bump()
	return v1.StoragePlan{ID: "plan-1", Kind: v1.StorageOpFormat}, nil
}

func (c *countingEngine) PrepareSanitize(context.Context, v1.StorageSanitizeRequest) (v1.StoragePlan, error) {
	c.bump()
	return v1.StoragePlan{ID: "plan-1", Kind: v1.StorageOpSanitize}, nil
}

func (c *countingEngine) Plan(context.Context, string) (v1.StoragePlan, bool) {
	return v1.StoragePlan{ID: "plan-1", Kind: v1.StorageOpFormat}, true
}

func (c *countingEngine) Execute(context.Context, string, v1.StorageOpKind, v1.StoragePrincipal) (v1.StorageJob, error) {
	c.bump()
	return v1.StorageJob{ID: "job-1"}, nil
}

func (c *countingEngine) Jobs(context.Context) ([]v1.StorageJob, error) { return nil, nil }

func (c *countingEngine) Cancel(context.Context, string, v1.StoragePrincipal) error {
	c.bump()
	return nil
}

// newLoopbackStorageFixture builds a synthetic host with one distinctively
// serialled external drive, so a leak of the inventory is unmistakable.
func newLoopbackStorageFixture(t *testing.T) *loopbackStorageFixture {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys")
	proc := filepath.Join(root, "proc")
	dev := filepath.Join(root, "dev")
	for _, d := range []string{filepath.Join(sys, "block"), filepath.Join(proc, "self"), dev} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	realDir := filepath.Join(sys, "devices", "pci0000:00", "usb1", "sdb")
	if err := os.MkdirAll(filepath.Join(realDir, "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realDir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, val := range map[string]string{
		"size": "16777216", "dev": "8:16", "removable": "1", "ro": "0",
		"queue/rotational": "0", "queue/discard_max_bytes": "0",
		"device/model": "PROBE-STICK", "device/serial": "CNLOOPBACKPROBE42",
	} {
		if err := os.WriteFile(filepath.Join(realDir, rel), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realDir, filepath.Join(sys, "block", "sdb")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "swaps"), []byte("Filename\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockdev.SetRoots(sys, proc, dev)
	storagehealth.SetRoots(sys, proc)
	storagemaint.SetStateDir(filepath.Join(root, "state"))
	storagemaint.SetLookPath(func(string) (string, error) { return "/bin/true", nil })

	fx := &loopbackStorageFixture{engine: &countingEngine{}}
	v1.SetStorageMaintenanceEngine(fx.engine)

	t.Cleanup(func() {
		blockdev.SetRoots("/sys", "/proc", "/dev")
		storagehealth.SetRoots("/sys", "/proc")
		storagemaint.SetStateDir("/var/lib/casanova/storage-maintenance")
		storagemaint.SetLookPath(exec.LookPath)
		v1.SetStorageMaintenanceEngine(nil)
	})
	return fx
}
