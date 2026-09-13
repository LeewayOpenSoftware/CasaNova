package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagehealth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagemaint"
	"github.com/labstack/echo/v4"
)

// These tests exist because the failure mode of this subsystem is a formatted
// disk. Every one of them asserts that a specific EFFECT did not happen — the
// engine was not called — rather than that a status code was 403. This service
// has already shipped a gate that answered 403 and let the handler run anyway,
// so a status assertion proves nothing here.
//
// No hardware and no privileges: pkg/blockdev discovers from sysfs, so a
// fixture tree exercises the real discovery and the real protection classifier.

// ---------------------------------------------------------------- fixtures

// storageFixture is a synthetic host: a sysfs/proc tree, a private state
// directory, a chosen set of installed helpers, and a spy engine.
type storageFixture struct {
	t             *testing.T
	root          string
	sys           string
	proc          string
	dev           string
	engine        *spyStorageEngine
	hasSystemDisk bool
	noSystemDisk  bool

	mounts [][2]string
	swaps  []string
}

func newStorageFixture(t *testing.T, opts ...func(*storageFixture)) *storageFixture {
	t.Helper()
	root := t.TempDir()
	f := &storageFixture{
		t: t, root: root,
		sys:    filepath.Join(root, "sys"),
		proc:   filepath.Join(root, "proc"),
		dev:    filepath.Join(root, "dev"),
		engine: &spyStorageEngine{},
	}
	for _, d := range []string{filepath.Join(f.sys, "block"), filepath.Join(f.proc, "self"), f.dev} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range opts {
		o(f)
	}
	f.writeMounts()
	f.writeSwaps()

	blockdev.SetRoots(f.sys, f.proc, f.dev)
	storagehealth.SetRoots(f.sys, f.proc)
	storagemaint.SetStateDir(filepath.Join(root, "state"))
	// A host where every helper is installed, so a refusal in a test is never
	// an accident of the developer's machine. /bin/true answers --version
	// instantly and harmlessly.
	storagemaint.SetLookPath(func(string) (string, error) { return "/bin/true", nil })
	SetStorageMaintenanceEngine(f.engine)

	// Every fixture host has a system disk.
	//
	// Not decoration: pkg/storagehealth cross-examines the protection
	// classifier by finding the device backing / with its own independent
	// parser and asserting blockdev reports it protected. A fixture with no
	// root filesystem cannot answer that question, so the subsystem correctly
	// reports it cannot demonstrate its own safety property — and destructive
	// operations are refused. A test host without a root filesystem is not a
	// scenario worth modelling; a host where the check is disabled is worse.
	//
	// Tests that need a DIFFERENT system layout simply mount over it.
	if !f.noSystemDisk && !f.hasSystemDisk {
		// A name no test uses, so a test that adds its own sda/sdb never
		// collides with the fixture's system disk.
		f.addDisk(diskSpec{name: "sdsys", sizeBytes: 256 << 30, topology: "pci0000:00/ata9",
			model: "SYSTEM", serial: "SYS-FIXTURE-1"})
		f.addPartition("sdsys", "sdsys1", 256<<30)
		f.mount("sdsys1", "/")
		f.hasSystemDisk = true
	}

	t.Cleanup(func() {
		blockdev.SetRoots("/sys", "/proc", "/dev")
		storagehealth.SetRoots("/sys", "/proc")
		storagemaint.SetStateDir("/var/lib/casanova/storage-maintenance")
		storagemaint.SetLookPath(exec.LookPath)
		storagemaint.ResetCapabilityCache()
		SetStorageMaintenanceEngine(nil)
	})
	return f
}

type diskSpec struct {
	name       string
	sizeBytes  uint64
	topology   string
	model      string
	serial     string
	wwn        string
	removable  bool
	readOnly   bool
	discard    bool
	holders    []string
	noQueueDir bool
}

func (f *storageFixture) addDisk(s diskSpec) {
	f.t.Helper()
	realDir := filepath.Join(f.sys, "devices", s.topology, s.name)
	if err := os.MkdirAll(filepath.Join(realDir, "device"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if !s.noQueueDir {
		if err := os.MkdirAll(filepath.Join(realDir, "queue"), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	write := func(rel, val string) {
		if err := os.WriteFile(filepath.Join(realDir, rel), []byte(val+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	write("size", itoaU(s.sizeBytes/512))
	write("dev", "8:0")
	write("removable", boolStr(s.removable))
	write("ro", boolStr(s.readOnly))
	if !s.noQueueDir {
		write("queue/rotational", "0")
		if s.discard {
			write("queue/discard_max_bytes", "2147483648")
		} else {
			write("queue/discard_max_bytes", "0")
		}
	}
	if s.model != "" {
		write("device/model", s.model)
	}
	if s.serial != "" {
		write("device/serial", s.serial)
	}
	if s.wwn != "" {
		write("wwid", s.wwn)
	}
	if err := os.MkdirAll(filepath.Join(realDir, "holders"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	for _, h := range s.holders {
		if err := os.MkdirAll(filepath.Join(realDir, "holders", h), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.Symlink(realDir, filepath.Join(f.sys, "block", s.name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *storageFixture) addPartition(disk, part string, sizeBytes uint64) {
	f.t.Helper()
	real, err := filepath.EvalSymlinks(filepath.Join(f.sys, "block", disk))
	if err != nil {
		f.t.Fatal(err)
	}
	pdir := filepath.Join(real, part)
	if err := os.MkdirAll(filepath.Join(pdir, "holders"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	for rel, val := range map[string]string{
		"partition": "1", "size": itoaU(sizeBytes / 512), "dev": "8:1", "ro": "0",
	} {
		if err := os.WriteFile(filepath.Join(pdir, rel), []byte(val+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *storageFixture) mount(devName, mountpoint string) {
	f.t.Helper()
	f.mounts = append(f.mounts, [2]string{devName, mountpoint})
	f.writeMounts()
}

func (f *storageFixture) writeMounts() {
	var b strings.Builder
	for i, m := range f.mounts {
		b.WriteString(itoaU(uint64(i+20)) + " 1 8:1 / " + m[1] +
			" rw,relatime shared:1 - ext4 /dev/" + m[0] + " rw\n")
	}
	if err := os.WriteFile(filepath.Join(f.proc, "self", "mountinfo"), []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *storageFixture) writeSwaps() {
	b := "Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n"
	for _, s := range f.swaps {
		b += "/dev/" + s + "\tpartition\t1000\t0\t-2\n"
	}
	if err := os.WriteFile(filepath.Join(f.proc, "swaps"), []byte(b), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// addDiskWithUnreadableAttributes adds a device whose kernel attributes cannot
// be read, which is the OTHER way discovery goes incomplete.
//
// It exists so the "incomplete discovery is refused" tests can be isolated. The
// obvious fixture — delete the mount table — also breaks the mount reader and
// the protection cross-check, so a test built on it passes even when the
// completeness guard is removed, rescued by a different guard. Here the mount
// table is intact and only completeness is violated.
func (f *storageFixture) addDiskWithUnreadableAttributes(name string) {
	f.t.Helper()
	realDir := filepath.Join(f.sys, "devices", "pci0000:00/usb9", name)
	if err := os.MkdirAll(filepath.Join(realDir, "queue"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	// size and queue/discard_max_bytes present; "dev" (major:minor) missing, so
	// readDisk reports an unreadable attribute.
	for rel, val := range map[string]string{
		"size": "1024", "removable": "1", "ro": "0",
		"queue/rotational": "0", "queue/discard_max_bytes": "0",
	} {
		if err := os.WriteFile(filepath.Join(realDir, rel), []byte(val+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(realDir, "holders"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(f.sys, "block", name)); err != nil {
		f.t.Fatal(err)
	}
}

// removeMountTable makes discovery incomplete: with no mount table every device
// looks unmounted, which is the one condition under which a system disk could
// pass the protection classifier.
func (f *storageFixture) removeMountTable() {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.proc, "self", "mountinfo")); err != nil {
		f.t.Fatal(err)
	}
}

func itoaU(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// A typical appliance: an internal SATA system disk with root on its second
// partition, plus an external USB stick that is a legitimate target.
func (f *storageFixture) standardHost() {
	f.addDisk(diskSpec{
		name: "sda", sizeBytes: 512 << 30, topology: "pci0000:00/ata1",
		model: "SYSTEM-SSD", serial: "SYS-0001",
	})
	f.addPartition("sda", "sda1", 1<<30)
	f.addPartition("sda", "sda2", 500<<30)
	f.mount("sda1", "/boot")
	f.mount("sda2", "/")

	f.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "USB-STICK", serial: "CNPROBESERIAL777", removable: true, discard: true,
	})
}

// eligibleFingerprint returns the fingerprint of the USB stick.
func (f *storageFixture) eligibleFingerprint() string {
	f.t.Helper()
	inv, err := blockdev.Discover()
	if err != nil {
		f.t.Fatalf("Discover: %v", err)
	}
	d, ok := inv.ByName("sdb")
	if !ok {
		f.t.Fatal("fixture disk sdb not found")
	}
	if !d.Eligible() {
		f.t.Fatalf("fixture disk sdb is not eligible: protected=%t reasons=%v", d.Protected, d.ProtectionReasons)
	}
	return d.Identity.Fingerprint()
}

func (f *storageFixture) deviceIdentity(name string) blockdev.DeviceIdentity {
	f.t.Helper()
	inv, err := blockdev.Discover()
	if err != nil {
		f.t.Fatalf("Discover: %v", err)
	}
	d, ok := inv.ByName(name)
	if !ok {
		f.t.Fatalf("fixture disk %s not found", name)
	}
	return d.Identity
}

// ---------------------------------------------------------------- spy engine

type spyStorageEngine struct {
	mu sync.Mutex

	prepareFormatCalls   int
	prepareSanitizeCalls int
	executeCalls         int
	planCalls            int
	jobsCalls            int
	cancelCalls          int

	stored      StoragePlan
	storedFound bool
}

func (s *spyStorageEngine) PrepareFormat(_ context.Context, req StorageFormatRequest) (StoragePlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareFormatCalls++
	return StoragePlan{
		ID: "plan-format", Kind: StorageOpFormat,
		Fingerprint: req.Target.Fingerprint, Identity: req.Target.Identity,
		DeviceName: req.Target.Name, SizeBytes: req.Target.SizeBytes,
		Filesystem: req.Filesystem, Label: req.Label, CreatedAt: time.Now(),
	}, nil
}

func (s *spyStorageEngine) PrepareSanitize(_ context.Context, req StorageSanitizeRequest) (StoragePlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareSanitizeCalls++
	return StoragePlan{
		ID: "plan-sanitize", Kind: StorageOpSanitize,
		Fingerprint: req.Target.Fingerprint, Identity: req.Target.Identity,
		DeviceName: req.Target.Name, SizeBytes: req.Target.SizeBytes,
		Method: req.Method, CreatedAt: time.Now(),
	}, nil
}

func (s *spyStorageEngine) Plan(_ context.Context, _ string) (StoragePlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planCalls++
	return s.stored, s.storedFound
}

func (s *spyStorageEngine) Execute(_ context.Context, planID string, kind StorageOpKind, _ StoragePrincipal) (StorageJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executeCalls++
	return StorageJob{ID: "job-1", Kind: kind, State: "running"}, nil
}

func (s *spyStorageEngine) Jobs(context.Context) ([]StorageJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobsCalls++
	return nil, nil
}

func (s *spyStorageEngine) Cancel(context.Context, string, StoragePrincipal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelCalls++
	return nil
}

func (s *spyStorageEngine) setPlan(p StoragePlan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored, s.storedFound = p, true
}

// destructiveCalls counts everything that could touch a disk.
func (s *spyStorageEngine) destructiveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepareFormatCalls + s.prepareSanitizeCalls + s.executeCalls + s.cancelCalls
}

// ---------------------------------------------------------------- helpers

// callHandler invokes a handler directly with a chosen principal, bypassing the
// router middleware. That is deliberate: it isolates the handler's OWN gate, so
// a test cannot pass merely because strictJWT rejected the request first.
type principalSpec struct {
	role   string
	userID int
	anon   bool // set no role and no user id at all
}

func callHandler(t *testing.T, h echo.HandlerFunc, method, target, body string,
	p principalSpec, params map[string]string) (int, string) {
	t.Helper()

	e := echo.New()
	var reqBody *strings.Reader
	if body == "" {
		reqBody = strings.NewReader("")
	} else {
		reqBody = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reqBody)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	if !p.anon {
		commonmw.SetRoleInContext(ctx, p.role, p.userID)
	}
	if len(params) > 0 {
		names := make([]string, 0, len(params))
		vals := make([]string, 0, len(params))
		for k, v := range params {
			names = append(names, k)
			vals = append(vals, v)
		}
		ctx.SetParamNames(names...)
		ctx.SetParamValues(vals...)
	}

	err := h(ctx)
	if err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			return he.Code, rec.Body.String()
		}
		t.Fatalf("handler returned a non-HTTP error: %v", err)
	}
	return rec.Code, rec.Body.String()
}

func decodeRefusalReason(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, body)
	}
	return out.Reason
}

const admin = "admin"

// ---------------------------------------------------------------------------
// GATE ISOLATION.
//
// The end-to-end loopback probe against the REAL router lives in
// route/storage_maintenance_loopback_test.go — it has to, because package v1
// cannot import package route without a cycle. That probe would still pass with
// the handlers' own gates deleted, because strictJWT refuses a tokenless caller
// at the middleware. So the tests below call the handlers DIRECTLY, with a
// principal already in the context, and examine each barrier on its own.
// ---------------------------------------------------------------------------

// Barrier 1: the permission bit. A user who holds PermStorageWrite (and every
// other non-admin permission) must NOT be able to format.
// refusedForSafety accepts either the narrow per-device refusal a test set up
// or the broader subsystem-health refusal that now runs first.
//
// The health gate is a strictly earlier and wider check: an ambiguous identity
// or an unreadable mount table is a reason to refuse EVERY device, not only the
// one named. Asserting solely the narrow reason would fail the day the stronger
// check began catching the same condition, which is the wrong direction for a
// safety test — what matters is that nothing was prepared.
func refusedForSafety(t *testing.T, code int, resp string, want string) {
	t.Helper()
	reason := decodeRefusalReason(t, resp)
	okCode := code == http.StatusConflict || code == http.StatusServiceUnavailable
	if !okCode || (reason != want && reason != reasonSubsystemUnhealthy) {
		t.Fatalf("status=%d body=%s, want a refusal (%s or %s)", code, resp, want, reasonSubsystemUnhealthy)
	}
}

func TestFormatPrepareRefusesARoleWithoutTheFormatPermission(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	body := `{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4"}`
	// A fully authenticated non-admin: user id present, role "user".
	code, _ := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: "user", userID: 7}, nil)

	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for a role without PermStorageFormat", n)
	}
}

// Barrier 2: a real principal. Even with the permission bit, no authenticated
// user id means no token was ever validated — and nobody to attribute the wipe
// to. This is the barrier that survives a mistake in the permission table.
func TestFormatPrepareRefusesWhenNoPrincipalWasAuthenticated(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	body := `{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4"}`
	// role admin, but user id 0: the shape of a request whose role was set
	// without a validated token.
	code, _ := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 0}, nil)

	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) with no authenticated principal", n)
	}
}

// The positive control. Without this, every refusal test above could be passing
// because the handler refuses everything.
func TestFormatPrepareReachesTheEngineForAnAuthenticatedAdmin(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	body := `{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4","label":"BACKUP"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, resp)
	}
	if f.engine.prepareFormatCalls != 1 {
		t.Fatalf("PrepareFormat called %d time(s), want 1", f.engine.prepareFormatCalls)
	}
	if f.engine.executeCalls != 0 {
		t.Fatal("preparing a plan must not execute anything")
	}
}

// The read gate, isolated the same way: guest role must not receive the device inventory.
func TestDeviceListRefusesGuest(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	code, body := callHandler(t, GetStorageMaintenanceDevices, http.MethodGet,
		"/v1/maintenance/storage/devices", "", principalSpec{role: "guest", userID: 3}, nil)

	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
	if strings.Contains(body, "CNPROBESERIAL777") || strings.Contains(body, "\"devices\"") {
		t.Fatalf("guest received the device inventory: %s", body)
	}
}

// The permission table itself. Holding PermStorageWrite — mount, unmount, trim
// — must NOT imply the ability to wipe a disk. This is asserted here rather
// than in the rbac package so the shared file carries only its two-line annex.
func TestDestructiveStoragePermissionsAreAdminOnlyAndNotImpliedByStorageWrite(t *testing.T) {
	for _, p := range []rbac.Permission{rbac.PermStorageFormat, rbac.PermStorageSanitize} {
		if !rbac.Allows(rbac.RoleAdmin, p) {
			t.Errorf("admin does not hold %v; the endpoints are unreachable by anyone", p)
		}
		for _, r := range []rbac.Role{rbac.RoleUser, rbac.RoleGuest} {
			if rbac.Allows(r, p) {
				t.Errorf("role %q holds %v — formatting and sanitizing are admin-only", r, p)
			}
		}
	}
	// The specific confusion the separate bits exist to prevent.
	if !rbac.Allows(rbac.RoleUser, rbac.PermStorageRead) {
		t.Error("the user role lost PermStorageRead; that is a different, unrelated regression")
	}
	if rbac.Allows(rbac.RoleUser, rbac.PermStorageFormat) {
		t.Error("a role with storage read/write inherited the ability to format a disk")
	}
	// Bits must be distinct: two permissions sharing a bit would make one grant
	// the other silently.
	if rbac.PermStorageFormat == rbac.PermStorageSanitize ||
		rbac.PermStorageFormat == rbac.PermStorageWrite ||
		rbac.PermStorageSanitize == rbac.PermStorageWrite {
		t.Fatal("the destructive storage permissions collide with an existing bit")
	}
}

// ---------------------------------------------------------------------------
// IDENTITY, NOT PATH.
// ---------------------------------------------------------------------------

// A body carrying a device path must be REFUSED, not silently stripped. Strict
// decoding is what makes "the API does not take paths" true rather than
// aspirational: without it the path is dropped and the request proceeds against
// whatever the remaining fields resolve to.
func TestPrepareRefusesABodyCarryingADevicePath(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	for _, body := range []string{
		`{"device":{"path":"/dev/sda"},"filesystem":"ext4"}`,
		`{"device":{"fingerprint":"serial:sys-0001"},"filesystem":"ext4","path":"/dev/sda"}`,
	} {
		code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
			"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

		if code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%s)", body, code, resp)
		}
		if r := decodeRefusalReason(t, resp); r != reasonMalformedBody {
			t.Errorf("body %s: reason = %q, want %q", body, r, reasonMalformedBody)
		}
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for a path-shaped request", n)
	}
}

func TestPrepareRefusesAnEmptyIdentity(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", `{"device":{},"filesystem":"ext4"}`,
		principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusBadRequest || decodeRefusalReason(t, resp) != reasonIdentityRequired {
		t.Fatalf("status=%d body=%s, want 400 %s", code, resp, reasonIdentityRequired)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) with no identity supplied", n)
	}
}

// ---------------------------------------------------------------------------
// THE CENTRAL REFUSAL: this host's own system disk.
// ---------------------------------------------------------------------------

func TestPrepareRefusesTheSystemDisk(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	// The identity of the disk carrying / and /boot, named exactly the way a
	// client would name it.
	body := `{"device":{"serial":"SYS-0001"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, resp)
	}
	if r := decodeRefusalReason(t, resp); r != reasonDeviceProtected {
		t.Fatalf("reason = %q, want %q (%s)", r, reasonDeviceProtected, resp)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for this host's system disk", n)
	}
}

// The upward-propagation case, stated as an API refusal rather than a classifier
// unit test: sda is offered as a whole disk, and it must be refused because a
// partition on it is root — not because sda itself is mounted anywhere.
func TestPrepareRefusesAWholeDiskWhoseChildIsRoot(t *testing.T) {
	f := newStorageFixture(t)
	f.addDisk(diskSpec{
		name: "sda", sizeBytes: 256 << 30, topology: "pci0000:00/usb1",
		model: "PORTABLE", serial: "PORT-1", removable: true,
	})
	f.addPartition("sda", "sda1", 256<<30)
	f.mount("sda1", "/")

	body := `{"device":{"serial":"PORT-1"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonDeviceProtected {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonDeviceProtected)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for the disk holding root", n)
	}
}

// ---------------------------------------------------------------------------
// AMBIGUITY AND WEAKNESS: refuse rather than guess.
// ---------------------------------------------------------------------------

func TestPrepareRefusesAnAmbiguousIdentity(t *testing.T) {
	f := newStorageFixture(t)
	// Two external drives of the same model and size, neither reporting a
	// serial. A client naming them by model gets a refusal, never the first one.
	f.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "TWIN", removable: true,
	})
	f.addDisk(diskSpec{
		name: "sdc", sizeBytes: 8 << 30, topology: "pci0000:00/usb2",
		model: "TWIN", removable: true,
	})

	// size_bytes alone matches both: the AND-over-supplied-fields rule means
	// this is the whole of the client's claim, and it is not unique.
	body := `{"device":{"fingerprint":"","serial":"","wwn":"","size_bytes":8589934592}}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	// An all-empty identity is refused before ambiguity is even considered —
	// which is itself the right answer, so assert on that boundary explicitly.
	if code != http.StatusBadRequest || decodeRefusalReason(t, resp) != reasonIdentityRequired {
		t.Fatalf("status=%d body=%s, want 400 %s", code, resp, reasonIdentityRequired)
	}

	// Now the real ambiguity: both disks share a topological fingerprint shape,
	// so name them by a WWN they both report.
	f2 := newStorageFixture(t)
	f2.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "TWIN", wwn: "0xCLONEDWWN", removable: true,
	})
	f2.addDisk(diskSpec{
		name: "sdc", sizeBytes: 8 << 30, topology: "pci0000:00/usb2",
		model: "TWIN", wwn: "0xCLONEDWWN", removable: true,
	})

	body = `{"device":{"wwn":"0xCLONEDWWN"},"filesystem":"ext4"}`
	code, resp = callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	refusedForSafety(t, code, resp, reasonIdentityAmbiguous)
	if n := f2.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for an ambiguous identity", n)
	}
}

// A device that reports nothing usable cannot be told apart from another one,
// so it is refused even though nothing marks it as a system disk.
func TestPrepareRefusesAnUnidentifiableDevice(t *testing.T) {
	f := newStorageFixture(t)
	// No model, no serial, no WWN, zero size: a card reader with no card in it.
	f.addDisk(diskSpec{name: "sdb", sizeBytes: 0, topology: "pci0000:00/usb1", removable: true})

	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	d, ok := inv.ByName("sdb")
	if !ok {
		t.Fatal("fixture device missing")
	}
	if d.Identity.Strength() != blockdev.StrengthWeak {
		t.Fatalf("fixture is not weak-identity (%s); the test would prove nothing", d.Identity.Strength())
	}

	body := `{"device":{"fingerprint":"` + d.Identity.Fingerprint() + `"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonIdentityTooWeak {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonIdentityTooWeak)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for an unidentifiable device", n)
	}
}

// ---------------------------------------------------------------------------
// FAIL-CLOSED ON INCOMPLETE DISCOVERY. This is the case blockdev cannot handle
// alone: with no mount table, every device — including the root disk — looks
// unmounted and sails through the protection classifier.
// ---------------------------------------------------------------------------

func TestPrepareRefusesWhenDiscoveryIsIncomplete(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint() // captured while discovery still works
	f.removeMountTable()

	body := `{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	// Either refusal satisfies the property under test: nothing may be
	// prepared while the inventory is incomplete.
	//
	// The subsystem-health gate now runs first and is broader — a mount table
	// this process cannot read makes a mounted system disk indistinguishable
	// from a free one, which is a reason to refuse EVERY device, not just this
	// one. Asserting only the narrower reason would fail the day the stronger
	// check started working, which is the wrong direction for a safety test.
	reason := decodeRefusalReason(t, resp)
	okCode := code == http.StatusConflict || code == http.StatusServiceUnavailable
	okReason := reason == reasonDiscoveryIncomplet || reason == reasonSubsystemUnhealthy
	if !okCode || !okReason {
		t.Fatalf("status=%d body=%s, want a refusal (%s or %s)", code, resp,
			reasonDiscoveryIncomplet, reasonSubsystemUnhealthy)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) while discovery was incomplete", n)
	}
}

// The SAME refusal, isolated. The test above deletes the mount table, which
// breaks several things at once — so it would still pass with the completeness
// guard removed, rescued by some other check. Here the mount table is intact
// and the ONLY thing wrong is that one unrelated device's attributes cannot be
// read. A perfectly healthy USB stick is refused anyway, which is the point:
// CasaNova does not operate on a partial picture of the host.
func TestPrepareRefusesOnIncompletenessCausedByAnUnrelatedDevice(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()
	f.addDiskWithUnreadableAttributes("sdx")

	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete {
		t.Fatal("fixture did not make discovery incomplete; the test proves nothing")
	}
	// The mount table must still be working, or this is the other test again.
	if sda, _ := inv.ByName("sda"); !sda.Protected {
		t.Fatal("the mount table stopped working in this fixture; the isolation is lost")
	}

	body := `{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	refusedForSafety(t, code, resp, reasonDiscoveryIncomplet)
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) on a partial picture of the host", n)
	}
}

// The same fixture, seen from the classifier's side: with the mount table gone
// the system disk is NOT protected, which is precisely why the route layer
// refuses wholesale rather than per device.
func TestIncompleteDiscoveryWouldOtherwiseUnprotectTheSystemDisk(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	f.removeMountTable()

	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete {
		t.Fatal("fixture did not produce an incomplete inventory; the test proves nothing")
	}
	sda, _ := inv.ByName("sda")
	if sda.Protected {
		// If blockdev ever starts protecting on incompleteness by itself, this
		// route-layer refusal becomes belt-and-braces rather than the only
		// guard — good news, and worth noticing rather than silently relying on.
		t.Log("blockdev now protects devices when discovery is incomplete; the route-layer " +
			"refusal is now redundant defence rather than the sole guard")
	}
}

// ---------------------------------------------------------------------------
// MOUNT STATE.
// ---------------------------------------------------------------------------

func TestFormatRefusesAMountedDrive(t *testing.T) {
	f := newStorageFixture(t)
	f.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "USB-STICK", serial: "USB-1", removable: true,
	})
	f.addPartition("sdb", "sdb1", 8<<30)
	// Mounted at a user location, so blockdev does NOT protect it — the refusal
	// has to come from this layer's own mount check.
	f.mount("sdb1", "/DATA/usb")

	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := inv.ByName("sdb")
	if d.Protected {
		t.Fatalf("fixture disk is protected (%v); this test would pass for the wrong reason",
			d.ProtectionReasons)
	}

	body := `{"device":{"serial":"USB-1"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonDeviceMounted {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonDeviceMounted)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for a mounted drive", n)
	}
}

// ---------------------------------------------------------------------------
// EXECUTE-TIME REVALIDATION. A plan is not a permission slip.
// ---------------------------------------------------------------------------

func TestExecuteAbortsWhenTheDeviceIsNoLongerTheOneThatWasPlanned(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	// A plan whose recorded identity belongs to a DIFFERENT drive that once sat
	// in the same slot and reported the same fingerprint shape.
	f.engine.setPlan(StoragePlan{
		ID: "plan-format", Kind: StorageOpFormat, Fingerprint: fp,
		Identity: blockdev.DeviceIdentity{
			Serial: "CNPROBESERIAL777", Model: "USB-STICK",
			SizeBytes: 16 << 30, // the drive that was planned was twice the size
			SysfsPath: "/somewhere/else",
		},
		DeviceName: "sdb", Filesystem: "ext4", CreatedAt: time.Now(),
	})

	code, resp := callHandler(t, PostStorageMaintenanceFormatExecute, http.MethodPost,
		"/v1/maintenance/storage/format/plan-format/execute", `{}`,
		principalSpec{role: admin, userID: 1}, map[string]string{"id": "plan-format"})

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonTargetChanged {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonTargetChanged)
	}
	if f.engine.executeCalls != 0 {
		t.Fatalf("Execute was called %d time(s) after the target changed", f.engine.executeCalls)
	}
}

// A sanitize plan must not be executable through the format endpoint: the two
// endpoints carry different permission bits, so accepting a mismatch would let
// one bit authorise the other operation.
func TestExecuteRefusesAPlanPreparedForTheOtherOperation(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	f.engine.setPlan(StoragePlan{
		ID: "plan-sanitize", Kind: StorageOpSanitize, Fingerprint: fp,
		Identity: f.deviceIdentity("sdb"), DeviceName: "sdb",
		Method: SanitizeTrim, CreatedAt: time.Now(),
	})

	code, resp := callHandler(t, PostStorageMaintenanceFormatExecute, http.MethodPost,
		"/v1/maintenance/storage/format/plan-sanitize/execute", `{}`,
		principalSpec{role: admin, userID: 1}, map[string]string{"id": "plan-sanitize"})

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonPlanKindMismatch {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonPlanKindMismatch)
	}
	if f.engine.executeCalls != 0 {
		t.Fatal("Execute ran a sanitize plan through the format endpoint")
	}
}

// A drive mounted between prepare and execute must abort the run: the prepare
// checks are re-run against a FRESH discovery, not replayed from the plan.
func TestExecuteRefusesADriveMountedAfterThePlanWasPrepared(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()
	identity := f.deviceIdentity("sdb")

	f.engine.setPlan(StoragePlan{
		ID: "plan-format", Kind: StorageOpFormat, Fingerprint: fp,
		Identity: identity, DeviceName: "sdb", Filesystem: "ext4", CreatedAt: time.Now(),
	})

	// The automounter got there first.
	f.mount("sdb", "/DATA/usb")

	code, resp := callHandler(t, PostStorageMaintenanceFormatExecute, http.MethodPost,
		"/v1/maintenance/storage/format/plan-format/execute", `{}`,
		principalSpec{role: admin, userID: 1}, map[string]string{"id": "plan-format"})

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonDeviceMounted {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonDeviceMounted)
	}
	if f.engine.executeCalls != 0 {
		t.Fatalf("Execute was called %d time(s) on a drive mounted after prepare", f.engine.executeCalls)
	}
}

func TestExecuteRefusesAnExpiredPlan(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	f.engine.setPlan(StoragePlan{
		ID: "plan-format", Kind: StorageOpFormat, Fingerprint: fp,
		Identity: f.deviceIdentity("sdb"), DeviceName: "sdb", Filesystem: "ext4",
		CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
	})

	code, resp := callHandler(t, PostStorageMaintenanceFormatExecute, http.MethodPost,
		"/v1/maintenance/storage/format/plan-format/execute", `{}`,
		principalSpec{role: admin, userID: 1}, map[string]string{"id": "plan-format"})

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonPlanExpired {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonPlanExpired)
	}
	if f.engine.executeCalls != 0 {
		t.Fatal("Execute ran an expired plan")
	}
}

// The positive control for execute: a plan that still matches reality runs.
func TestExecuteRunsAPlanThatStillMatchesTheDevice(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()

	f.engine.setPlan(StoragePlan{
		ID: "plan-format", Kind: StorageOpFormat, Fingerprint: fp,
		Identity: f.deviceIdentity("sdb"), DeviceName: "sdb", Filesystem: "ext4",
		CreatedAt: time.Now(),
	})

	code, resp := callHandler(t, PostStorageMaintenanceFormatExecute, http.MethodPost,
		"/v1/maintenance/storage/format/plan-format/execute", `{}`,
		principalSpec{role: admin, userID: 1}, map[string]string{"id": "plan-format"})

	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", code, resp)
	}
	if f.engine.executeCalls != 1 {
		t.Fatalf("Execute called %d time(s), want 1", f.engine.executeCalls)
	}
}

// ---------------------------------------------------------------------------
// SANITIZE METHOD RULES AND HONEST GUARANTEES.
// ---------------------------------------------------------------------------

// The method is now called "trim" — "discard" was a whole-device name for a
// mechanism this tree never implemented and it was retired. What the test
// asserts is unchanged: a drive that does not advertise discard must not be
// offered a pass that would do nothing.
func TestSanitizeRefusesTrimOnADriveThatDoesNotSupportIt(t *testing.T) {
	f := newStorageFixture(t)
	f.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "SPINNER", serial: "SPIN-1", removable: true, discard: false,
	})

	body := `{"device":{"serial":"SPIN-1"},"method":"trim"}`
	code, resp := callHandler(t, PostStorageMaintenanceSanitizePrepare, http.MethodPost,
		"/v1/maintenance/storage/sanitize/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusConflict || decodeRefusalReason(t, resp) != reasonDiscardUnsupported {
		t.Fatalf("status=%d body=%s, want 409 %s", code, resp, reasonDiscardUnsupported)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatal("the engine was called for a TRIM on a drive without discard support")
	}
}

func TestSanitizeRefusesAnUnknownMethod(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	body := `{"device":{"serial":"CNPROBESERIAL777"},"method":"dod-5220-22-m"}`
	code, resp := callHandler(t, PostStorageMaintenanceSanitizePrepare, http.MethodPost,
		"/v1/maintenance/storage/sanitize/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusBadRequest || decodeRefusalReason(t, resp) != reasonUnsupportedMethod {
		t.Fatalf("status=%d body=%s, want 400 %s", code, resp, reasonUnsupportedMethod)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatal("the engine was called for an unknown sanitize method")
	}
}

// Rule 7: no false guarantees. A UI string promising more than the code
// delivers is a defect, so the honest text is generated here and asserted here.
func TestSanitizeGuaranteesDoNotClaimCertifiedErasure(t *testing.T) {
	// Iterated from what the engine implements, so a method added later is
	// covered automatically instead of being forgotten here.
	for _, em := range storagemaint.ImplementedSanitizeMethods() {
		m := StorageSanitizeMethod(em)
		g := storageSanitizeGuarantee(m)
		if g == "" {
			t.Fatalf("%s has no guarantee statement", m)
		}
		lower := strings.ToLower(g)
		for _, forbidden := range []string{"dod", "certified erasure", "guaranteed unrecoverable",
			"securely erased", "military"} {
			if strings.Contains(lower, forbidden) && !strings.Contains(lower, "not a certified") &&
				!strings.Contains(lower, "is not a cryptographic") {
				t.Errorf("%s claims %q: %s", m, forbidden, g)
			}
		}
	}
	// TRIM must say plainly that it proves nothing about destruction: it is
	// the method a user is most likely to mistake for a secure erase.
	trim := strings.ToLower(storageSanitizeGuarantee(SanitizeTrim))
	if !strings.Contains(trim, "not a cryptographic erase") {
		t.Error("the TRIM guarantee does not disclaim cryptographic erasure")
	}
	if !strings.Contains(trim, "host writes nothing") {
		t.Error("the TRIM guarantee does not say the host writes nothing")
	}
}

// The API must not offer a method the engine cannot perform.
//
// It previously advertised "discard" and "overwrite" as acting on "every block
// the drive exposes", with no whole-device mechanism anywhere in the tree — a
// user wiping a drive before selling it would have been told something happened
// that never could.
//
// Mutation that must fail this test: add an unimplemented method back to the
// route's vocabulary.
func TestOnlyImplementedSanitizeMethodsAreOffered(t *testing.T) {
	implemented := map[string]bool{}
	for _, m := range storagemaint.ImplementedSanitizeMethods() {
		implemented[string(m)] = true
	}
	if len(implemented) == 0 {
		t.Fatal("the engine implements no sanitize method at all")
	}

	for _, m := range []StorageSanitizeMethod{SanitizeTrim, SanitizeZeroFill, SanitizeMultiPass} {
		if !implemented[string(m)] {
			t.Fatalf("the API offers %q, which pkg/storagemaint does not implement; a method that "+
				"cannot be performed must not be offered", m)
		}
		if !storageSanitizeMethodImplemented(m) {
			t.Fatalf("%q is offered but storageSanitizeMethodImplemented says otherwise", m)
		}
	}

	// And a whole-device name must be refused outright.
	for _, gone := range []StorageSanitizeMethod{"discard", "overwrite", "device_sanitize"} {
		if storageSanitizeMethodImplemented(gone) {
			t.Fatalf("%q is accepted; no whole-device sanitize mechanism exists in this tree", gone)
		}
		if storageSanitizeGuarantee(gone) != "" {
			t.Fatalf("%q still carries a guarantee string for a mechanism that does not exist", gone)
		}
	}
}

// ---------------------------------------------------------------------------
// FAIL-CLOSED WITHOUT AN ENGINE.
// ---------------------------------------------------------------------------

func TestEveryDestructiveEndpointFailsClosedWithoutAnEngine(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	fp := f.eligibleFingerprint()
	SetStorageMaintenanceEngine(nil)

	cases := []struct {
		name    string
		handler echo.HandlerFunc
		body    string
		params  map[string]string
	}{
		{"format/prepare", PostStorageMaintenanceFormatPrepare,
			`{"device":{"fingerprint":"` + fp + `"},"filesystem":"ext4"}`, nil},
		{"format/execute", PostStorageMaintenanceFormatExecute, `{}`, map[string]string{"id": "x"}},
		{"sanitize/prepare", PostStorageMaintenanceSanitizePrepare,
			`{"device":{"fingerprint":"` + fp + `"},"method":"overwrite"}`, nil},
		{"sanitize/execute", PostStorageMaintenanceSanitizeExecute, `{}`, map[string]string{"id": "x"}},
		{"jobs/cancel", PostStorageMaintenanceJobCancel, `{}`, map[string]string{"id": "x"}},
	}
	for _, tc := range cases {
		code, resp := callHandler(t, tc.handler, http.MethodPost, "/v1/maintenance/storage/"+tc.name,
			tc.body, principalSpec{role: admin, userID: 1}, tc.params)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (%s)", tc.name, code, resp)
		}
		if r := decodeRefusalReason(t, resp); r != reasonEngineUnavailable {
			t.Errorf("%s: reason = %q, want %q", tc.name, r, reasonEngineUnavailable)
		}
	}
}

// ---------------------------------------------------------------------------
// HEALTH.
// ---------------------------------------------------------------------------

func TestHealthReportsHealthyWhenTheRootDiskIsProtected(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	rep := storagehealth.Evaluate(context.Background())
	if rep.State != storagehealth.StateHealthy {
		t.Fatalf("state = %s (invariant %s: %s)", rep.State, rep.Invariant, rep.ActionableCause)
	}
	if !rep.DestructiveOperationsAllowed {
		t.Fatal("a healthy subsystem must allow destructive operations")
	}
	// The load-bearing invariant must actually have been exercised, not skipped.
	var protectedCheck *storagehealth.Check
	for i := range rep.Checks {
		if rep.Checks[i].Invariant == storagehealth.InvProtectedDetection {
			protectedCheck = &rep.Checks[i]
		}
	}
	if protectedCheck == nil {
		t.Fatal("the protected-device invariant was not evaluated at all")
	}
	if !strings.Contains(protectedCheck.Detail, "/dev/sda2") {
		t.Fatalf("the protection check did not name this host's root device: %s", protectedCheck.Detail)
	}
}

// The negative boundary, from the other side: with the mount table gone the
// classifier can no longer prove the root disk is protected, and health must
// refuse rather than report success.
func TestHealthRefusesToReportHealthyWithoutAMountTable(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	f.removeMountTable()

	rep := storagehealth.Evaluate(context.Background())
	if rep.State == storagehealth.StateHealthy {
		t.Fatal("health reported healthy with no mount table; the root disk cannot be proven protected")
	}
	if rep.DestructiveOperationsAllowed {
		t.Fatal("destructive operations are allowed while protection cannot be validated")
	}
	if rep.Invariant == "" || rep.ActionableCause == "" {
		t.Fatalf("a failing report must name the invariant and an actionable cause: %+v", rep)
	}
}

// The completeness invariant, ISOLATED. Deleting the mount table degrades
// three checks at once, so a test built on it stays green even with the
// completeness check deleted — rescued by the protection cross-check. This
// fixture keeps the mount table intact and breaks only one unrelated device's
// attributes, so nothing but discovery_complete can produce the verdict.
func TestHealthReportsTheCompletenessInvariantOnItsOwn(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()
	f.addDiskWithUnreadableAttributes("sdx")

	rep := storagehealth.Evaluate(context.Background())
	if rep.State == storagehealth.StateHealthy {
		t.Fatal("health reported healthy while a device's attributes were unreadable")
	}
	if rep.Invariant != storagehealth.InvDiscoveryComplete {
		t.Fatalf("invariant = %q, want %q — some other check produced this verdict, so the "+
			"completeness invariant is not actually being exercised",
			rep.Invariant, storagehealth.InvDiscoveryComplete)
	}
	if rep.DestructiveOperationsAllowed {
		t.Fatal("destructive operations allowed on an incomplete picture of the host")
	}
	// The protection cross-check must still be healthy here, or the isolation
	// this test depends on has silently gone away.
	for _, c := range rep.Checks {
		if c.Invariant == storagehealth.InvProtectedDetection && c.State != storagehealth.StateHealthy {
			t.Fatalf("the protection check is %s in this fixture; the isolation is lost", c.State)
		}
	}
}

// Two physical devices that fingerprint identically is the condition under
// which a prepared operation can execute against the wrong disk. Health must
// call it broken, not merely odd.
func TestHealthFailsValidationWhenTwoDevicesShareAnIdentity(t *testing.T) {
	f := newStorageFixture(t)
	f.addDisk(diskSpec{
		name: "sda", sizeBytes: 512 << 30, topology: "pci0000:00/ata1",
		model: "SYSTEM-SSD", serial: "SYS-0001",
	})
	f.addPartition("sda", "sda1", 512<<30)
	f.mount("sda1", "/")
	// A cloned-firmware enclosure pair: same serial reported by both.
	f.addDisk(diskSpec{
		name: "sdb", sizeBytes: 8 << 30, topology: "pci0000:00/usb1",
		model: "TWIN", serial: "CLONE-1", removable: true,
	})
	f.addDisk(diskSpec{
		name: "sdc", sizeBytes: 8 << 30, topology: "pci0000:00/usb2",
		model: "TWIN", serial: "CLONE-1", removable: true,
	})

	rep := storagehealth.Evaluate(context.Background())
	if rep.State != storagehealth.StateValidationFailed {
		t.Fatalf("state = %s, want validation_failed (invariant %s)", rep.State, rep.Invariant)
	}
	if rep.Invariant != storagehealth.InvStableIdentity {
		t.Fatalf("invariant = %s, want %s", rep.Invariant, storagehealth.InvStableIdentity)
	}
	if rep.DestructiveOperationsAllowed {
		t.Fatal("destructive operations allowed while two devices share an identity")
	}
}

// Job state must survive a durable round-trip, and the directory must be
// provisioned automatically — a feature that needs an operator to mkdir
// something over SSH is not finished.
func TestHealthAutoProvisionsAndValidatesTheStateDirectory(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	dir := storagemaint.StateDir()
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("fixture state dir %s already exists; the auto-provision claim is untested", dir)
	}

	rep := storagehealth.Evaluate(context.Background())

	var persist *storagehealth.Check
	for i := range rep.Checks {
		if rep.Checks[i].Invariant == storagehealth.InvJobStatePersist {
			persist = &rep.Checks[i]
		}
	}
	if persist == nil || persist.State != storagehealth.StateHealthy {
		t.Fatalf("job-state persistence not validated: %+v", persist)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the state directory was not auto-provisioned: %v", err)
	}
	// The probe cleans up after itself: a health check that litters its own
	// state directory would eventually be indistinguishable from job state.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".health-probe-") {
			t.Fatalf("the health probe left %s behind", e.Name())
		}
	}
}

func TestHealthEndpointRefusesGuest(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	code, body := callHandler(t, GetStorageMaintenanceHealth, http.MethodGet,
		"/v1/maintenance/storage/health", "", principalSpec{role: "guest", userID: 3}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
	if strings.Contains(body, "\"checks\"") {
		t.Fatalf("guest received the health report: %s", body)
	}
}

// ---------------------------------------------------------------------------
// The device list is where the UI is generated from, so it has to carry the
// reason a disk is unavailable — not just hide it.
// ---------------------------------------------------------------------------

func TestDeviceListExplainsWhyTheSystemDiskIsUnavailable(t *testing.T) {
	f := newStorageFixture(t)
	f.standardHost()

	code, body := callHandler(t, GetStorageMaintenanceDevices, http.MethodGet,
		"/v1/maintenance/storage/devices", "", principalSpec{role: admin, userID: 1}, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}

	var out struct {
		Devices []struct {
			Name              string   `json:"name"`
			Fingerprint       string   `json:"fingerprint"`
			Eligible          bool     `json:"eligible"`
			Protected         bool     `json:"protected"`
			ProtectionReasons []string `json:"protection_reasons"`
			Explanation       string   `json:"explanation"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	// Three: this test's two, plus the fixture's own system disk. Every
	// fixture host has one, because the health check cross-examines the
	// protection classifier against the device backing / and a host without
	// one cannot answer that question.
	if len(out.Devices) != 3 {
		t.Fatalf("got %d whole devices, want 3 (partitions must not be listed)", len(out.Devices))
	}
	for _, d := range out.Devices {
		switch d.Name {
		case "sda":
			if d.Eligible || !d.Protected {
				t.Error("the system disk is offered as a target")
			}
			if d.Explanation == "" || len(d.ProtectionReasons) == 0 {
				t.Error("the system disk is refused with no stated reason")
			}
		case "sdb":
			if !d.Eligible {
				t.Errorf("the external stick is not eligible: %v %s", d.ProtectionReasons, d.Explanation)
			}
			if d.Fingerprint == "" {
				t.Error("an eligible device was listed with no fingerprint to address it by")
			}
		case "sdsys":
			// The fixture's own system disk. It must be listed AND refused,
			// which is the same property this test asserts for sda.
			if len(d.ProtectionReasons) == 0 {
				t.Error("the fixture system disk was listed with no protection reason")
			}
		default:
			t.Errorf("unexpected device %s", d.Name)
		}
	}
}

// THE HEALTH GATE MUST BE LOAD-BEARING ON ITS OWN.
//
// Removing it did not fail a single test, because every existing case was also
// caught by a narrower per-device refusal — the gate was present, correct, and
// proven by nothing. That is the same shape as a check that exists and is never
// consulted, which is what this gate was added to fix.
//
// This isolates it: the target device is external, unprotected, identifiable
// and perfectly eligible, so every per-device check PASSES. What fails is the
// subsystem's ability to demonstrate that it would refuse to damage the system
// — pkg/storagehealth cross-examines the protection classifier against the
// device backing /, and here there is no root mount to cross-examine against.
//
// A subsystem that cannot prove it would refuse the system disk must not be
// trusted to format any disk.
//
// Mutation that must fail this test: delete the requireHealthySubsystem call
// from prepare.
func TestPrepareIsRefusedWhenTheSubsystemCannotProveItsOwnSafety(t *testing.T) {
	f := newStorageFixture(t, func(f *storageFixture) { f.noSystemDisk = true })
	f.addDisk(diskSpec{name: "sdb", sizeBytes: 64 << 30, topology: "pci0000:00/usb1/1-1",
		model: "Cruzer", serial: "STICK-1", removable: true})

	// Precondition: the device itself is beyond reproach.
	inv, err := blockdev.Discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	d, ok := inv.ByName("sdb")
	if !ok {
		t.Fatal("fixture device missing")
	}
	if !d.Eligible() {
		t.Fatalf("precondition: the stick should be eligible, got %v", d.ProtectionReasons)
	}

	body := `{"device":{"serial":"STICK-1"},"filesystem":"ext4"}`
	code, resp := callHandler(t, PostStorageMaintenanceFormatPrepare, http.MethodPost,
		"/v1/maintenance/storage/format/prepare", body, principalSpec{role: admin, userID: 1}, nil)

	if code != http.StatusServiceUnavailable ||
		decodeRefusalReason(t, resp) != reasonSubsystemUnhealthy {
		t.Fatalf("status=%d body=%s, want 503 %s — the device is eligible, so ONLY the "+
			"subsystem-health gate can refuse this", code, resp, reasonSubsystemUnhealthy)
	}
	if n := f.engine.destructiveCalls(); n != 0 {
		t.Fatalf("the engine was called %d time(s) while the subsystem could not prove it would "+
			"refuse to damage the system", n)
	}
}
