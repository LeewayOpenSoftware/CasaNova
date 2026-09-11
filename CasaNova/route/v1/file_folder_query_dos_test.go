package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/labstack/echo/v4"
)

// CN-DOS-03, second half: the folder-query siblings of POST /v1/file/operate.
//
// The first patch bounded the size pre-scan inside /file/operate and left
// GET /v1/folder/size and GET /v1/folder/count exactly as they were: a full
// recursive walk (file.GetFileOrDirSize) and a full directory slurp+sort
// (os.ReadDir), neither cancellable, neither budgeted, and — unlike operate —
// neither gated by any permission at all. The v1 JWT middleware skips auth for
// tokenless loopback requests and the gateway proxies every LAN and tunnel
// request over 127.0.0.1, so both were unauthenticated, uncancellable,
// repeatable traversals of a 5.4 TB array reachable from the internet.
//
// The invariants held below:
//  1. neither endpoint answers anything to a caller without PermFileRead, and
//     the deny STOPS the handler (it does not merely write a 403 and continue);
//  2. the filesystem work runs under the request's context and dies with the
//     client;
//  3. when the bound fires the caller still gets an answer, never a refusal —
//     a limit that breaks a legitimate query is worse than the DoS;
//  4. the wire contract (Success + the type and meaning of Data) is unchanged;
//  5. the mount-point guard hoisted out of the operate loop is not weaker than
//     the per-item service.IsMounted it replaced.

// folderQueryRequest drives one of the two handlers exactly as the router does:
// a GET whose only input is ?path=, with the role the JWT middleware would have
// left in the context. A nil role means "nothing was set", which is precisely
// what a tokenless loopback request produces.
func folderQueryRequest(t *testing.T, h func(echo.Context) error, target, path string, role *rbac.Role, reqCtx context.Context) (*httptest.ResponseRecorder, error) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target+"?path="+path, nil)
	if reqCtx != nil {
		req = req.WithContext(reqCtx)
	}
	rec := httptest.NewRecorder()
	ctx := echo.New().NewContext(req, rec)
	if role != nil {
		// Both keys, because the JWT middleware sets both: SetRoleInContext(c,
		// claims.Role, claims.ID). Setting only the role modelled a principal
		// that cannot exist in production, and requirePerm now refuses it —
		// having a role but no user id is exactly the anonymous context the
		// strict default was added to reject.
		ctx.Set(commonmw.ContextKeyRole, *role)
		ctx.Set(commonmw.ContextKeyUserID, 7)
	}
	return rec, h(ctx)
}

// decodeResult parses the handler's envelope and fails loudly if the body is
// not the shape every client already parses.
func decodeResult(t *testing.T, rec *httptest.ResponseRecorder) model.Result {
	t.Helper()
	var body model.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the usual JSON envelope: %v (%q)", err, rec.Body.String())
	}
	return body
}

// dataNumber asserts Data is still a plain JSON number — the contract both the
// v2 UI (getFolderSize) and the LEGACY UI read — and returns it.
func dataNumber(t *testing.T, body model.Result) float64 {
	t.Helper()
	n, ok := body.Data.(float64)
	if !ok {
		t.Fatalf("data = %#v (%T), want a plain number: the wire contract changed", body.Data, body.Data)
	}
	return n
}

// mkFlatDir writes n files of `size` bytes directly in one fresh directory.
// Flat on purpose: it is the shape GetFileCount actually counts.
func mkFlatDir(t *testing.T, n, size int) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	payload := make([]byte, size)
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), payload, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir, int64(n * size)
}

// ---------------------------------------------------------------------------
// 1. The gate. This is the whole unauthenticated half of the finding.
// ---------------------------------------------------------------------------

// A caller the JWT middleware left unauthenticated resolves to the guest role,
// which does NOT hold PermFileRead. Both handlers must refuse it — and refuse
// by RETURNING the error, not by writing a 403 and walking the tree anyway
// (the "gate that does not gate" bug this project has already been bitten by).
//
// Mutation that must fail this test: delete the requirePerm block from either
// handler.
func TestFolderQueriesRefuseAnUnauthenticatedCaller(t *testing.T) {
	tree, realSize := mkFlatDir(t, 40, 128)
	guest := rbac.RoleGuest

	cases := []struct {
		name   string
		target string
		h      func(echo.Context) error
	}{
		{"size", "/v1/folder/size", GetSize},
		{"count", "/v1/folder/count", GetFileCount},
	}
	roles := []struct {
		name string
		role *rbac.Role
	}{
		// No role in the context at all: exactly what the loopback skipper
		// leaves behind for a tokenless request proxied by the gateway.
		{"tokenless", nil},
		{"guest", &guest},
	}

	for _, tc := range cases {
		for _, r := range roles {
			t.Run(tc.name+"/"+r.name, func(t *testing.T) {
				rec, err := folderQueryRequest(t, tc.h, tc.target, tree, r.role, context.Background())

				if err == nil {
					t.Fatalf("handler returned nil: the request was ALLOWED (body %q)", rec.Body.String())
				}
				he, ok := err.(*echo.HTTPError)
				if !ok {
					t.Fatalf("err = %#v, want *echo.HTTPError so echo actually aborts the chain", err)
				}
				if he.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", he.Code)
				}
				// Nothing may have been written by the handler itself: a 403
				// followed by a real answer is the bypass, not the fix.
				if rec.Body.Len() != 0 {
					t.Fatalf("the denied handler still wrote a body: %q", rec.Body.String())
				}
				// And it must not have leaked the measurement either.
				if got := rec.Body.String(); got != "" {
					t.Fatalf("unexpected payload %q (tree is %d bytes)", got, realSize)
				}
			})
		}
	}
}

// The gate must not cost a legitimate user their answer, and the answer must be
// byte-for-byte what the old unbounded implementation produced.
func TestGetSizeIsExactForAnAuthorizedCaller(t *testing.T) {
	tree, realSize := mkFlatDir(t, 200, 512)
	admin := rbac.RoleAdmin

	// Sanity: the unbounded walk agrees, so a different answer below can only
	// come from the new code path.
	if got, err := file.GetFileOrDirSize(tree); err != nil || got != realSize {
		t.Fatalf("fixture: unbounded walk = %d, %v; want %d, nil", got, err, realSize)
	}

	rec, err := folderQueryRequest(t, GetSize, "/v1/folder/size", tree, &admin, context.Background())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: %s", rec.Code, common_err.SUCCESS, rec.Body.String())
	}
	body := decodeResult(t, rec)
	if body.Success != common_err.SUCCESS {
		t.Fatalf("success = %d, want %d", body.Success, common_err.SUCCESS)
	}
	if got := dataNumber(t, body); int64(got) != realSize {
		t.Fatalf("size = %d, want %d", int64(got), realSize)
	}
	if body.Message != common_err.GetMsg(common_err.SUCCESS) {
		t.Fatalf("message = %q on a complete scan, want the unchanged %q", body.Message, common_err.GetMsg(common_err.SUCCESS))
	}
}

// ---------------------------------------------------------------------------
// 2. The scan dies with the client. This is the uncancellable half.
// ---------------------------------------------------------------------------

// Wiring: the walk must receive the REQUEST's context. Driven through the
// handler on purpose — a helper that honours a context nobody hands it is
// exactly the failure this checks for.
//
// Mutation that must fail this test: put `file.GetFileOrDirSize(path)` back.
// Nothing in it can be told to stop, so it reports the full tree.
func TestGetSizeDiesWithTheClient(t *testing.T) {
	tree, realSize := mkFlatDir(t, 400, 256)
	admin := rbac.RoleAdmin

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel() // the client hung up before the handler ran

	start := time.Now()
	rec, err := folderQueryRequest(t, GetSize, "/v1/folder/size", tree, &admin, reqCtx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// Still an answer, not a refusal: see the budget test below for why.
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: a dead client must not become a 5xx: %s", rec.Code, common_err.SUCCESS, rec.Body.String())
	}
	got := int64(dataNumber(t, decodeResult(t, rec)))
	if got >= realSize {
		t.Fatalf("size = %d (whole tree is %d): the walk ignored the request context", got, realSize)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("handler took %v on a dead context; it is not checking cancellation", elapsed)
	}
}

// Same wiring proof for the count sibling: os.ReadDir cannot be interrupted, so
// restoring it makes this report the full entry count.
func TestGetFileCountDiesWithTheClient(t *testing.T) {
	tree, _ := mkFlatDir(t, 2000, 1)
	admin := rbac.RoleAdmin

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	rec, err := folderQueryRequest(t, GetFileCount, "/v1/folder/count", tree, &admin, reqCtx)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: %s", rec.Code, common_err.SUCCESS, rec.Body.String())
	}
	if got := int64(dataNumber(t, decodeResult(t, rec))); got >= 2000 {
		t.Fatalf("count = %d of 2000: the read ignored the request context", got)
	}
}

// ---------------------------------------------------------------------------
// 3. Lesson 1: what happens when the bound fires on a LEGITIMATE query.
// ---------------------------------------------------------------------------

// An exhausted budget must yield the partial count with a 200, never a 503.
// A 503 would mean a genuine "how big is /DATA" can never succeed on a large
// array — a fix worse than the DoS it closes. Data must stay a plain number so
// no client has to learn a new shape; only Message says the value is a lower
// bound.
func TestGetSizeReturnsThePartialWhenTheBudgetFires(t *testing.T) {
	tree, realSize := mkFlatDir(t, 3000, 64)
	admin := rbac.RoleAdmin

	restore := folderQueryScanBudget
	folderQueryScanBudget = time.Nanosecond
	t.Cleanup(func() { folderQueryScanBudget = restore })

	rec, err := folderQueryRequest(t, GetSize, "/v1/folder/size", tree, &admin, context.Background())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: an exhausted budget must not refuse the query (%s)",
			rec.Code, common_err.SUCCESS, rec.Body.String())
	}
	body := decodeResult(t, rec)
	if body.Success != common_err.SUCCESS {
		t.Fatalf("success = %d, want %d", body.Success, common_err.SUCCESS)
	}
	got := int64(dataNumber(t, body))
	if got < 0 || got > realSize {
		t.Fatalf("partial size = %d, outside [0, %d]", got, realSize)
	}
	if body.Message == common_err.GetMsg(common_err.SUCCESS) {
		t.Fatal("a truncated scan reported itself as a complete one")
	}
}

func TestGetFileCountReturnsThePartialWhenTheBudgetFires(t *testing.T) {
	tree, _ := mkFlatDir(t, 2000, 1)
	admin := rbac.RoleAdmin

	restore := folderQueryScanBudget
	folderQueryScanBudget = time.Nanosecond
	t.Cleanup(func() { folderQueryScanBudget = restore })

	rec, err := folderQueryRequest(t, GetFileCount, "/v1/folder/count", tree, &admin, context.Background())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: %s", rec.Code, common_err.SUCCESS, rec.Body.String())
	}
	body := decodeResult(t, rec)
	if got := int64(dataNumber(t, body)); got < 0 || got > 2000 {
		t.Fatalf("partial count = %d, outside [0, 2000]", got)
	}
	if body.Message == common_err.GetMsg(common_err.SUCCESS) {
		t.Fatal("a truncated read reported itself as a complete one")
	}
}

// A real failure must still be a real failure. Turning "the path does not
// exist" into a silent 0 would be the other way to break the contract.
func TestFolderQueriesStillReportRealErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	admin := rbac.RoleAdmin

	for _, tc := range []struct {
		name   string
		target string
		h      func(echo.Context) error
	}{
		{"size", "/v1/folder/size", GetSize},
		{"count", "/v1/folder/count", GetFileCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := folderQueryRequest(t, tc.h, tc.target, missing, &admin, context.Background())
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if rec.Code != common_err.SERVICE_ERROR {
				t.Fatalf("status = %d, want %d for a missing path: %s", rec.Code, common_err.SERVICE_ERROR, rec.Body.String())
			}
			if body := decodeResult(t, rec); body.Success != common_err.SERVICE_ERROR {
				t.Fatalf("success = %d, want %d", body.Success, common_err.SERVICE_ERROR)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. countDirEntries: exactness across batch boundaries.
// ---------------------------------------------------------------------------

// Streaming the directory in batches is only an acceptable replacement for
// os.ReadDir if the count it produces is identical, including on directory
// sizes that straddle the batch size. Anything else silently changes what the
// endpoint means.
func TestCountDirEntriesMatchesReadDir(t *testing.T) {
	for _, n := range []int{0, 1, countDirEntriesBatch - 1, countDirEntriesBatch, countDirEntriesBatch + 1, 3*countDirEntriesBatch + 7} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			dir, _ := mkFlatDir(t, n, 1)

			want, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			got, truncated, err := countDirEntries(context.Background(), dir)
			if err != nil {
				t.Fatalf("countDirEntries: %v", err)
			}
			if truncated {
				t.Fatal("a complete read reported itself truncated")
			}
			if got != len(want) {
				t.Fatalf("count = %d, want %d", got, len(want))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. The MEDIUM: the mount guard hoisted out of the per-item loop.
// ---------------------------------------------------------------------------

// service.IsMounted normalises the path (Abs+EvalSymlinks) before comparing,
// so replacing it with a raw lookup in the snapshot map would silently stop
// catching a mount reached through a symlinked ancestor — a per-item cost fix
// that quietly weakens a safety gate.
//
// Mutation that must fail this test: reduce isMountedIn to
// `return mountedSet[path]`.
func TestIsMountedInStillCatchesAMountBehindASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "usb"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// The snapshot holds mount points the way the kernel reports them: fully
	// resolved. /<root>/real/usb is "the mount"; /<root>/link/usb is the same
	// directory reached through a symlinked ancestor.
	mountedSet := map[string]bool{filepath.Join(real, "usb"): true}

	if !isMountedIn(mountedSet, filepath.Join(real, "usb")) {
		t.Fatal("the exact mount point was not recognised")
	}
	if !isMountedIn(mountedSet, filepath.Join(link, "usb")) {
		t.Fatal("a mount reached through a symlinked ancestor was NOT recognised: " +
			"the hoisted guard is weaker than the service.IsMounted it replaced")
	}
	if isMountedIn(mountedSet, filepath.Join(real, "not-a-mount")) {
		t.Fatal("a plain directory was reported as a mount point")
	}
	// A path that does not resolve cannot be a mount point, and must not panic
	// or be reported as one.
	if isMountedIn(mountedSet, filepath.Join(root, "nope", "usb")) {
		t.Fatal("a non-existent path was reported as a mount point")
	}
}

// The rclone half of IsMounted compares connection mount points against the
// RAW path, so the snapshot lookup has to keep checking the raw path too —
// otherwise resolving first would drop connections recorded lexically.
func TestIsMountedInKeepsTheRawPathMatch(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "remote")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !isMountedIn(map[string]bool{target: true}, target) {
		t.Fatal("a connection mount point recorded verbatim was not matched")
	}
	// Also on a path that cannot be resolved at all: a dead rclone mount still
	// has to be refused rather than moved.
	dead := filepath.Join(root, "dead-remote")
	if !isMountedIn(map[string]bool{dead: true}, dead) {
		t.Fatal("an unresolvable connection mount point was not matched")
	}
}
