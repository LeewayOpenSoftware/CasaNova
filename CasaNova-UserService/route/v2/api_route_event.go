package v2

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	codegen "github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/user_service"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/labstack/echo/v4"
)

// DeleteEvent removes a single event by UUID — a privileged mutation gated
// behind admin-grade PermSystemConfig. It has no internal tokenless HTTP
// caller (the in-process message-bus listener calls the service layer
// directly, not this endpoint), so gating it cannot break inter-service comms.
func (s *UserService) DeleteEvent(ctx echo.Context, params codegen.EventUuid) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	m := service.MyService.Event().GetEventByUUID(params.String())
	service.MyService.Event().DeleteEvent(params.String())
	return ctx.JSON(http.StatusOK, m)
}

// GetEvents is a read, gated behind PermSystemRead.
//
// This comment used to claim the guest bit was there so "the CasaNova-CLI's
// tokenless `casanova user list events` call keeps working". That was not true
// when it was written and is worth recording, because it is the kind of comment
// that stops the next reader from checking: this router mounts its JWT
// middleware with authskip.For(nil), so a request carrying neither a token nor
// the peer secret is refused 401 by the middleware and NEVER REACHES THIS
// HANDLER. The CLI builds a plain client with no Authorization header and no
// internalauth header (CasaNova-CLI/cmd/userListEvents.go), so that call has
// been failing on its own for as long as the skipper has been nil — the
// permission bit was not keeping it alive.
//
// The bit is still correct for what actually reaches here: an authenticated
// reader of any role may list events. requirePerm additionally refuses a
// context with no principal, so the guest bit cannot be borrowed by an
// unauthenticated caller if this handler is ever remounted.
func (s *UserService) GetEvents(ctx echo.Context, params codegen.GetEventsParams) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	list := service.MyService.Event().GetEvents()
	return ctx.JSON(http.StatusOK, list)
}

// DeleteEventBySerial removes every local-storage event for a serial — a
// privileged mutation gated behind admin-grade PermSystemConfig. No internal
// tokenless HTTP caller exists (in-process listener uses the service layer).
func (s *UserService) DeleteEventBySerial(ctx echo.Context, serial codegen.Serial) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	service.MyService.Event().DeleteEventBySerial(serial)
	return ctx.JSON(http.StatusOK, serial)
}
