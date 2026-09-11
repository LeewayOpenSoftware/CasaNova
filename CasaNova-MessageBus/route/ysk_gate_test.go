package route

// DELETE /v2/message_bus/ysk_card/{id} carried no authorization at all, while
// its mutating siblings PublishEvent and TriggerAction both take
// requireBusMutator. The deletion is by id alone with no user scoping and the
// cards are seeded server-wide, so any authenticated role — guest included —
// could remove a dashboard advisory for EVERY user.

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestDeleteYskCardIsGated is the RED probe. Before the gate this reached the
// service; the APIRoute here holds a nil service precisely so that "the gate
// aborted first" and "the handler ran" are distinguishable — reaching the
// service panics, refusing it does not.
func TestDeleteYskCardIsGated(t *testing.T) {
	r := &APIRoute{}

	for _, role := range []string{"", "guest", "user"} {
		t.Run("role="+role, func(t *testing.T) {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("the handler reached the service before authorizing: %v", rec)
				}
			}()

			err := r.DeleteYskCard(gateCtx(t, role, false), "some-card-id")
			if err == nil {
				t.Fatal("an unauthorized caller deleted a server-wide advisory card")
			}
			he, ok := err.(*echo.HTTPError)
			if !ok || he.Code != http.StatusForbidden {
				t.Fatalf("refusal must be an aborting 403 HTTPError, got %v", err)
			}
		})
	}
}

// TestDeleteYskCardPositiveControl proves the gate is not simply refusing
// everyone: an authorized caller gets PAST it and reaches the service, which
// with a nil service means it panics. That panic is the evidence — without
// it this file would pass just as well against a handler that rejects
// unconditionally.
func TestDeleteYskCardPositiveControl(t *testing.T) {
	r := &APIRoute{}

	for _, tc := range []struct {
		name      string
		role      string
		viaSocket bool
	}{
		{"admin por TCP", "admin", false},
		{"socket interno sin token", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						reached = true
					}
				}()
				_ = r.DeleteYskCard(gateCtx(t, tc.role, tc.viaSocket), "some-card-id")
			}()
			if !reached {
				t.Fatal("an authorized caller was blocked by the gate — internal publishers and admins must still be able to clear a card")
			}
		})
	}
}
