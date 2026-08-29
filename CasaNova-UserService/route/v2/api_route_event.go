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

// GetEvents is a read — gated behind PermSystemRead, which guests hold, so the
// CasaNova-CLI's tokenless `casanova user list events` call keeps working while
// a presented token still resolves the real role.
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
