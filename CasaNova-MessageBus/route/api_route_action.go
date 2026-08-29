package route

import (
	"net"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/common"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/route/adapter/in"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/route/adapter/out"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (r *APIRoute) GetActionTypes(c echo.Context) error {
	actionType, err := r.services.ActionTypeService.GetActionTypes()
	if err != nil {
		message := err.Error()
		return c.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	results := make([]codegen.ActionType, 0)

	for _, actionType := range actionType {
		results = append(results, out.ActionTypeAdapter(actionType))
	}

	return c.JSON(http.StatusOK, results)
}

func (r *APIRoute) RegisterActionTypes(c echo.Context) error {
	if err := requireBusMutator(c); err != nil {
		return err
	}
	var actionTypes []codegen.ActionType
	if err := c.Bind(&actionTypes); err != nil {
		message := err.Error()
		return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	for _, actionType := range actionTypes {
		_, err := r.services.ActionTypeService.RegisterActionType(in.ActionTypeAdapter(actionType))
		if err != nil {
			message := err.Error()
			return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}
	}

	return c.JSON(http.StatusOK, codegen.ResponseOK{})
}

func (r *APIRoute) GetActionTypesBySourceID(c echo.Context, sourceID codegen.SourceID) error {
	results, err := r.services.ActionTypeService.GetActionTypesBySourceID(sourceID)
	if err != nil {
		message := err.Error()
		return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return c.JSON(http.StatusOK, results)
}

func (r *APIRoute) GetActionType(c echo.Context, sourceID codegen.SourceID, name codegen.EventName) error {
	result, err := r.services.ActionTypeService.GetActionType(sourceID, name)
	if err != nil {
		message := err.Error()
		return c.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	if result == nil {
		return c.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: utils.Ptr("not found")})
	}

	return c.JSON(http.StatusOK, result)
}

func (r *APIRoute) TriggerAction(c echo.Context, sourceID codegen.SourceID, name codegen.EventName) error {
	if err := requireBusMutator(c); err != nil {
		return err
	}
	actionType, err := r.services.ActionTypeService.GetActionType(sourceID, name)
	if err != nil {
		message := err.Error()
		return c.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	if actionType == nil {
		return c.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: utils.Ptr("not found")})
	}

	var properties map[string]string
	if err := c.Bind(&properties); err != nil {
		message := err.Error()
		return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	action := in.ActionAdapter(codegen.Action{
		SourceID:   sourceID,
		Name:       name,
		Properties: properties,
		Timestamp:  utils.Ptr(time.Now()),
	})

	r.services.SocketIOService.Publish(action)

	// B11: same contract as PublishEvent — an action the dispatcher
	// never took must not be reported as accepted.
	if err := r.services.ActionServiceWS.Trigger(action); err != nil {
		logger.Error("action not accepted by the bus dispatcher",
			zap.Error(err),
			zap.String("source_id", sourceID),
			zap.String("name", name))

		message := "action not accepted by the message bus: " + err.Error()
		return c.JSON(http.StatusServiceUnavailable, codegen.ResponseInternalServerError{Message: &message})
	}

	return c.JSON(http.StatusOK, out.ActionAdapter(action))
}

func (r *APIRoute) SubscribeActionWS(c echo.Context, sourceID codegen.SourceID, params codegen.SubscribeActionWSParams) error {
	var actionNames []string
	if params.Names != nil {
		for _, actionName := range *params.Names {
			actionType, err := r.services.ActionTypeService.GetActionType(sourceID, actionName)
			if err != nil {
				message := err.Error()
				return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}

			if actionType == nil {
				return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: utils.Ptr("action type not found")})
			}

			actionNames = append(actionNames, actionName)
		}
	} else {
		actionTypes, err := r.services.ActionTypeService.GetActionTypesBySourceID(sourceID)
		if err != nil {
			message := err.Error()
			return c.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		for _, actionType := range actionTypes {
			actionNames = append(actionNames, actionType.Name)
		}
	}

	conn, _, _, err := ws.UpgradeHTTP(c.Request(), c.Response())
	if err != nil {
		// UpgradeHTTP failed before hijacking — safe to write JSON.
		message := err.Error()
		return c.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	channel, err := r.services.ActionServiceWS.Subscribe(sourceID, actionNames)
	if err != nil {
		// Wave 27: the connection has already been hijacked by
		// UpgradeHTTP at this point — echo no longer owns the
		// underlying socket. Calling c.JSON would attempt to write to
		// a closed/hijacked response writer and either no-op or
		// surface a confusing "http: connection has been hijacked"
		// error in the log. Mirror api_route_event.go: close the
		// conn, log the failure, return nil. The client sees an
		// abrupt close, which is the correct WS-level signal here.
		conn.Close()
		logger.Error("error when subscribing to action via websocket", zap.Error(err), zap.String("source_id", sourceID))
		return nil
	}

	go func(conn net.Conn, channel chan model.Action, actionNames []string) {
		defer conn.Close()
		// NOTE: do NOT close(channel) here — the dispatch loop may still
		// hold a snapshot reference and sending on a closed channel panics.
		defer func(actionNames []string) {
			for _, name := range actionNames {
				if err := r.services.ActionServiceWS.Unsubscribe(sourceID, name, channel); err != nil {
					logger.Error("error when trying to unsubscribe an action type via websocket", zap.Error(err), zap.String("source_id", sourceID), zap.String("name", name))
				}
			}
		}(actionNames)

		logger.Info("a websocket connection has started for actions", zap.String("remote_addr", conn.RemoteAddr().String()))

		for {
			action, ok := <-channel
			if !ok {
				logger.Info("websocket channel for events is closed")
				return
			}

			if action.SourceID == common.MessageBusSourceID && action.Name == common.MessageBusHeartbeatName {
				if err := wsutil.WriteServerMessage(conn, ws.OpPing, []byte{}); err != nil {
					logger.Error("error when trying to send ping message via websocket", zap.Error(err))
					return
				}
				continue
			}

			message, err := json.Marshal(out.ActionAdapter(action))
			if err != nil {
				logger.Error("error when trying to marshal action for websocket", zap.Error(err))
				continue
			}

			logger.Info("sending action via websocket", zap.String("remote_addr", conn.RemoteAddr().String()), zap.String("message", string(message)))

			if err := wsutil.WriteServerBinary(conn, message); err != nil {
				if _, ok := err.(*net.OpError); ok {
					logger.Info("websocket connection ended", zap.String("error", err.Error()))
				} else {
					logger.Error("error when sending event via websocket", zap.String("error", err.Error()))
				}
				return
			}
		}
	}(conn, channel, actionNames)

	return nil
}
