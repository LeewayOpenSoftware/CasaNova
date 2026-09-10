package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// The invariant: ANY helper that reports refusal through an `error` must return
// an error VALUE, never c.JSON.
//
// c.JSON returns the SERIALIZER's error, which is nil once the bytes are
// written. A helper that "denies" that way hands its caller nil, so the
// caller's `if err != nil { return err }` does not fire and the guarded work
// runs anyway — after a refusal has already been committed to the response.
//
// This module had a test for its PERMISSION GATES only, which is why
// validateTaskID kept the bug in the very same file whose comments warn about
// it three times. The invariant is about the shape of an error-returning
// helper, not about permissions, so this table covers every one of them.
//
// Mutation that must fail this test: change any refusal back to
// `return ..., c.JSON(http.StatusBadRequest, ...)`.
func TestEveryDenyingHelperReturnsAnErrorValueAndWritesNothing(t *testing.T) {
	cases := []struct {
		name     string
		wantCode int
		// call drives the helper on a context that must be refused, and
		// reports whether the caller would have carried on (err == nil).
		call func(c echo.Context) error
	}{
		{
			name:     "validateTaskID on a malformed id",
			wantCode: http.StatusBadRequest,
			call: func(c echo.Context) error {
				c.SetParamNames("id")
				c.SetParamValues("../../etc/passwd")
				_, err := validateTaskID(c)
				return err
			},
		},
		// requireTaskRead used to be absent here, with a comment explaining
		// that under the old policy it could not refuse anyone: a context with
		// no role resolves to Guest (middleware.RoleFromContext), guestPerms
		// holds PermSystemRead, and the read gate therefore admitted every
		// caller the JWT middleware let past. There was no input that made it
		// take its deny path, so there was nothing to assert.
		//
		// It is gated on PermSystemConfig now, which guest does not hold, so
		// the deny path exists and is asserted. Keeping the case here also
		// keeps the convention honest: a gate that cannot be driven to its
		// denial is a gate this test cannot protect.
		{
			name:     "requireTaskRead without a role",
			wantCode: http.StatusForbidden,
			call:     func(c echo.Context) error { return requireTaskRead(c) },
		},
		{
			name:     "requireTaskWrite without a role",
			wantCode: http.StatusForbidden,
			call:     func(c echo.Context) error { return requireTaskWrite(c) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

			sideEffectRan := false
			err := func() error {
				if err := tc.call(c); err != nil {
					return err
				}
				sideEffectRan = true
				return nil
			}()

			if sideEffectRan {
				t.Fatal("the guarded work ran despite the caller being refused: " +
					"the helper returned nil, so every call site continues")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("want *echo.HTTPError so the caller aborts, got %T (%v)", err, err)
			}
			if he.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", he.Code, tc.wantCode)
			}
			// The helper must not commit the response; echo's error handler owns it.
			if rec.Body.Len() != 0 {
				t.Fatalf("the helper wrote the response itself: %q", rec.Body.String())
			}
		})
	}
}
