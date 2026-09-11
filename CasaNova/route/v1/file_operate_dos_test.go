package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// CN-DOS-03 regression suite.
//
// POST /v1/file/operate walked the filesystem once per item, recursively, with
// no ceiling on the item count, no context and no deadline — inside the request
// goroutine, on an endpoint reachable from the internet through the tunnel. A
// single POST could keep a goroutine walking /DATA indefinitely, and it kept
// walking after the client hung up.
//
// The invariant these tests hold: a request's filesystem work is bounded and
// does not outlive the client, AND enforcing that never costs the user a
// legitimate operation.

// buildTree writes n files of size bytes each under a fresh directory and
// returns the directory and the exact total byte count.
func buildTree(t *testing.T, n, size int) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	payload := make([]byte, size)
	for i := 0; i < n; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("d%02d", i%16))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%05d", i)), payload, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir, int64(n * size)
}

// operateRequest drives the real handler with an admin role (PermFileWrite) and
// the given request context, and returns the recorder.
//
// It parks a sentinel id in OpStrArr first so the handler's `isFirst` branch
// stays false: no worker, no progress poller, no notify goroutine. This suite
// is about what the REQUEST does, and the worker is another finding's subject.
func operateRequest(t *testing.T, reqCtx context.Context, body model.FileOperate) *httptest.ResponseRecorder {
	t.Helper()

	const sentinel = "cn-dos-03-sentinel"
	service.OpMu.Lock()
	service.OpStrArr = append(service.OpStrArr, sentinel)
	service.OpMu.Unlock()
	t.Cleanup(func() {
		service.OpMu.Lock()
		ids := append([]string(nil), service.OpStrArr...)
		service.OpMu.Unlock()
		for _, id := range ids {
			service.FileQueue.Delete(id)
			service.RemoveOp(id)
		}
	})

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/file/operate", strings.NewReader(string(raw)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if reqCtx != nil {
		req = req.WithContext(reqCtx)
	}
	rec := httptest.NewRecorder()
	ctx := echo.New().NewContext(req, rec)
	// Both keys, because the JWT middleware sets both: SetRoleInContext(c,
	// claims.Role, claims.ID). A role with no user id is the anonymous context
	// requirePerm now refuses, and is not a shape production can produce.
	ctx.Set(commonmw.ContextKeyRole, rbac.RoleAdmin)
	ctx.Set(commonmw.ContextKeyUserID, 1)

	if err := PostOperateFileOrDir(ctx); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	return rec
}

// queuedOp returns the operation the handler just enqueued for destination `to`.
func queuedOp(t *testing.T, to string) (string, model.FileOperate) {
	t.Helper()
	var (
		foundID string
		found   model.FileOperate
		hits    int
	)
	service.FileQueue.Range(func(k, v any) bool {
		entry, ok := v.(model.FileOperate)
		if !ok || entry.To != to {
			return true
		}
		hits++
		foundID, found = k.(string), entry
		return true
	})
	if hits != 1 {
		t.Fatalf("expected exactly 1 queued operation for %q, found %d", to, hits)
	}
	return foundID, found
}

// The walk must stop when the request is over. With the old
// file.GetFileOrDirSize this test fails twice over: it returns the FULL size and
// a nil error, because nothing in it can be told to stop.
func TestScanItemSizeStopsWhenTheRequestIsOver(t *testing.T) {
	tree, realSize := buildTree(t, 400, 128)

	// Sanity: the unbounded walk sees the whole tree, so a smaller answer below
	// can only come from the abort.
	if got, err := file.GetFileOrDirSize(tree); err != nil || got != realSize {
		t.Fatalf("fixture: unbounded walk = %d, %v; want %d, nil", got, err, realSize)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client hung up before the scan even started

	start := time.Now()
	size, err := scanItemSize(ctx, tree)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("scanItemSize walked to completion on a dead request context")
	}
	if !isCanceled(err) {
		t.Fatalf("err = %v, want context cancellation", err)
	}
	if size >= realSize {
		t.Fatalf("scanned %d of %d bytes: the walk did not stop", size, realSize)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("abort took %v; the walk is not checking the context per entry", elapsed)
	}
}

// The bound must not cost accuracy on the normal path: with a live context the
// budgeted walk has to report exactly what the unbounded one reported, or every
// progress bar in the product silently changes meaning.
func TestScanItemSizeMatchesTheUnboundedWalk(t *testing.T) {
	tree, realSize := buildTree(t, 200, 512)

	got, err := scanItemSize(context.Background(), tree)
	if err != nil {
		t.Fatalf("scanItemSize: %v", err)
	}
	if got != realSize {
		t.Fatalf("size = %d, want %d", got, realSize)
	}

	// A plain file, not a directory, still answers its own size.
	single := filepath.Join(tree, "d00")
	entries, err := os.ReadDir(single)
	if err != nil || len(entries) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	f := filepath.Join(single, entries[0].Name())
	if got, err := scanItemSize(context.Background(), f); err != nil || got != 512 {
		t.Fatalf("single file size = %d, %v; want 512, nil", got, err)
	}
}

// A truncated walk must still report the bytes it managed to count. Returning 0
// would leave TotalSize at zero and divide the progress bar by nothing.
func TestScanItemSizeKeepsThePartialCount(t *testing.T) {
	tree, realSize := buildTree(t, 3000, 64)

	// Cancel while the walk is in flight rather than before it starts.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	size, err := scanItemSize(ctx, tree)
	if err == nil {
		// The tree was walked before the cancel landed; nothing to assert about
		// partial counts, and the accuracy case is covered above.
		t.Skip("walk completed before cancellation; timing-dependent assertion skipped")
	}
	if size < 0 || size > realSize {
		t.Fatalf("partial size = %d, outside [0, %d]", size, realSize)
	}
}

// An item list with no ceiling is unbounded per-item work (path resolution,
// mount check, recursive walk) ordered by one request. It must be refused
// before any of that work starts, and nothing may be queued.
func TestOperateRejectsAnUnboundedItemList(t *testing.T) {
	dst := t.TempDir()
	// Every item points at the same real directory: with the cap removed this
	// request is exactly the DoS — maxOperateItems+1 recursive walks.
	tree, _ := buildTree(t, 4, 16)
	items := make([]model.FileItem, maxOperateItems+1)
	for i := range items {
		items[i] = model.FileItem{From: tree}
	}

	rec := operateRequest(t, context.Background(), model.FileOperate{
		Type: "copy", To: dst, Item: items,
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body model.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !strings.Contains(body.Message, "too many items") {
		t.Fatalf("message = %q, want the item-limit refusal", body.Message)
	}
	// Refused up front: no operation may exist for this destination.
	service.FileQueue.Range(func(_, v any) bool {
		if entry, ok := v.(model.FileOperate); ok && entry.To == dst {
			t.Fatalf("a refused request still queued an operation")
		}
		return true
	})
}

// Lesson 1: the ceiling must not fire on real usage. Moving a few thousand
// files out of one folder is ordinary file-manager behaviour and has to go
// through untouched — accepted, queued, and fully measured.
func TestOperateAllowsALegitimateBulkSelection(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()

	const n, size = 2000, 32
	items := make([]model.FileItem, 0, n)
	payload := make([]byte, size)
	for i := 0; i < n; i++ {
		p := filepath.Join(src, fmt.Sprintf("f%05d", i))
		if err := os.WriteFile(p, payload, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		items = append(items, model.FileItem{From: p})
	}

	rec := operateRequest(t, context.Background(), model.FileOperate{
		Type: "copy", To: dst, Item: items,
	})
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d (a legitimate %d-item selection was refused: %s)",
			rec.Code, common_err.SUCCESS, n, rec.Body.String())
	}

	_, entry := queuedOp(t, dst)
	if entry.TotalSize != int64(n*size) {
		t.Fatalf("total_size = %d, want %d", entry.TotalSize, n*size)
	}
	for i := range entry.Item {
		if entry.Item[i].Size != size {
			t.Fatalf("item %d size = %d, want %d", i, entry.Item[i].Size, size)
		}
	}
}

// Wiring: the pre-scan must actually receive the REQUEST's context, not a
// detached one. Driven through the handler on purpose — a helper that honours
// a context nobody passes it is the failure mode this checks for.
//
// The client is already gone when the handler runs, so the walk must contribute
// nothing. With the original file.GetFileOrDirSize call the entry lands with
// the full tree size, which is the mutation that fails this test.
func TestOperatePreScanDiesWithTheClient(t *testing.T) {
	dst := t.TempDir()
	tree, realSize := buildTree(t, 300, 256)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := operateRequest(t, ctx, model.FileOperate{
		Type: "copy", To: dst, Item: []model.FileItem{{From: tree}},
	})
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: %s", rec.Code, common_err.SUCCESS, rec.Body.String())
	}

	id, entry := queuedOp(t, dst)
	if entry.TotalSize >= realSize {
		t.Fatalf("total_size = %d (full tree is %d): the pre-scan ignored the request context", entry.TotalSize, realSize)
	}
	// ...and the operation is still enqueued and cancellable: aborting the
	// measurement must not cost the user the operation itself.
	if !inOpQueue(id) {
		t.Fatal("the operation was not enqueued")
	}
}

// Lesson 1 again, for the deadline half of the bound: when the budget fires,
// the user still gets their operation. Only the progress total is approximate.
func TestOperateStillQueuesWhenTheScanBudgetFires(t *testing.T) {
	dst := t.TempDir()
	tree, realSize := buildTree(t, 3000, 64)

	restore := operateScanBudget
	operateScanBudget = time.Nanosecond
	t.Cleanup(func() { operateScanBudget = restore })

	rec := operateRequest(t, context.Background(), model.FileOperate{
		Type: "copy", To: dst, Item: []model.FileItem{{From: tree}},
	})
	if rec.Code != common_err.SUCCESS {
		t.Fatalf("status = %d, want %d: an exhausted scan budget must not refuse the operation (%s)",
			rec.Code, common_err.SUCCESS, rec.Body.String())
	}

	id, entry := queuedOp(t, dst)
	if entry.TotalSize > realSize {
		t.Fatalf("total_size = %d exceeds the real %d", entry.TotalSize, realSize)
	}
	if !inOpQueue(id) {
		t.Fatal("the operation was dropped when the scan budget fired")
	}
	if len(entry.Item) != 1 || entry.Item[0].From != tree {
		t.Fatalf("the queued operation lost its items: %+v", entry.Item)
	}
}

func inOpQueue(id string) bool {
	service.OpMu.Lock()
	defer service.OpMu.Unlock()
	for _, v := range service.OpStrArr {
		if v == id {
			return true
		}
	}
	return false
}

func isCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded ||
		strings.Contains(err.Error(), "context canceled") ||
		strings.Contains(err.Error(), "context deadline exceeded")
}
