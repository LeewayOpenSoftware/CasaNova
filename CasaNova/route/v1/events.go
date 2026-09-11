package v1

import (
	"net/http"
	"strconv"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/notifications"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// GetAuditEvents returns a filtered slice of security/audit events from
// CasaNova's own log. Non-destructive — this is the admin review endpoint.
//
// GET /v1/sys/audit?category=auth&severity=warning&since=2026-01-01&limit=200
func GetAuditEvents(ctx echo.Context) error {
	// This is the admin review endpoint described above: its actor, address and
	// security-event history is operator-grade reconnaissance, not a general
	// system-status read for guest accounts.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	l := service.AuditLogger()
	if l == nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "audit log not initialized",
		})
	}

	filter := auditlog.QueryFilter{
		Actor:   ctx.QueryParam("actor"),
		ActorIP: ctx.QueryParam("ip"),
	}
	if s := ctx.QueryParam("severity"); s != "" {
		filter.Severity = auditlog.Severity(s)
	}
	if t := ctx.QueryParam("type"); t != "" {
		filter.Types = []auditlog.EventType{auditlog.EventType(t)}
	}
	if sinceStr := ctx.QueryParam("since"); sinceStr != "" {
		if parsed, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			filter.Since = parsed
		}
	}
	if limitStr := ctx.QueryParam("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil {
			filter.Limit = n
		}
	}
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 1000
	}

	events, err := l.Query(filter)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"events": events,
			"count":  len(events),
		},
	})
}

// GetNotifications returns the notification center feed.
//
// GET /v1/sys/notifications?unread=true&limit=50
func GetNotifications(ctx echo.Context) error {
	// requireSystemPrincipal: see antivirus.go's GetAntivirusStatus comment.
	// Reads stay at PermSystemRead (the dashboard widgets poll them) — this
	// closes the tokenless-caller gap, not the permission tier.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	svc := service.Notifications()
	if svc == nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "notification service not initialized",
		})
	}

	opts := notifications.ListOptions{
		UnreadOnly: ctx.QueryParam("unread") == "true",
		Category:   notifications.Category(ctx.QueryParam("category")),
		Level:      notifications.Level(ctx.QueryParam("level")),
	}
	if limitStr := ctx.QueryParam("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil {
			opts.Limit = n
		}
	}
	if opts.Limit <= 0 || opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if offsetStr := ctx.QueryParam("offset"); offsetStr != "" {
		if n, err := strconv.Atoi(offsetStr); err == nil {
			opts.Offset = n
		}
	}

	list, err := svc.List(opts)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	unread, _ := svc.UnreadCount()
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"notifications": list,
			"unread_count":  unread,
		},
	})
}

// PostMarkNotificationRead marks one or more notifications as read.
//
// POST /v1/sys/notifications/read   body: {"ids": [1, 2, 3]}
// POST /v1/sys/notifications/read   body: {"all": true}
func PostMarkNotificationRead(ctx echo.Context) error {
	// Reads stay at PermSystemRead (the dashboard widgets poll them), but
	// mutating another principal's view of history is operator work:
	// PermAppStart is the narrowest permission guests do not hold.
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}
	svc := service.Notifications()
	if svc == nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "notification service not initialized",
		})
	}

	var req struct {
		IDs []uint `json:"ids"`
		All bool   `json:"all"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	var err error
	if req.All {
		err = svc.MarkAllRead()
	} else if len(req.IDs) > 0 {
		err = svc.MarkRead(req.IDs)
	} else {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "provide ids or all:true",
		})
	}
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// DeleteNotification removes a notification by ID. Idempotent.
//
// DELETE /v1/sys/notifications/:id
func DeleteNotification(ctx echo.Context) error {
	// Reads stay at PermSystemRead (the dashboard widgets poll them), but
	// mutating another principal's view of history is operator work:
	// PermAppStart is the narrowest permission guests do not hold.
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}
	svc := service.Notifications()
	if svc == nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "notification service not initialized",
		})
	}
	idStr := ctx.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid id",
		})
	}
	if err := svc.Delete(uint(id)); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}
