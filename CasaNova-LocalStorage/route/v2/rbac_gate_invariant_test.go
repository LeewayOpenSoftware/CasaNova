package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// The invariant: a denied caller's side effect must NOT run.
//
// This is not a test that "requirePerm was called" — it drives a handler shaped
// exactly like every real one in this package and asserts the guarded work did
// not happen. It caught a live defect: requirePerm denied by WRITING the 403 via
// ctx.JSON, which returns the serializer's error (nil once the bytes are out),
// so `if err := requirePerm(…); err != nil { return err }` never fired and the
// handler ran anyway. Seven copies of that gate existed across five services.
//
// Mutation that must fail this test: change the refusal back to
// ctx.JSON(http.StatusForbidden, …).
func TestRequirePermAbortsTheHandler(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	// No role on the context resolves to guest, which lacks this permission.
	ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)

	sideEffectRan := false
	handler := func(c echo.Context) error {
		if err := requirePerm(c, rbac.PermStorageWrite); err != nil {
			return err
		}
		sideEffectRan = true
		return c.NoContent(http.StatusOK)
	}

	err := handler(ctx)

	if sideEffectRan {
		t.Fatal("the guarded work ran despite the caller being denied")
	}
	if err == nil {
		t.Fatal("requirePerm returned nil on the deny path; every call site would continue")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("want *echo.HTTPError so the caller aborts, got %T", err)
	}
	if he.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", he.Code)
	}
	// The helper must not commit the response; the error handler owns it.
	if rec.Body.Len() != 0 {
		t.Fatalf("the gate wrote the response itself: %q", rec.Body.String())
	}
}
