package v1

// Sessions API — surfaces the in-memory refresh-family store as a
// per-user list of "active sessions" the UI can display and revoke. A
// "session" here is one login (= one refresh family); subsequent access-
// token refreshes within the same login don't add rows, they bump the
// session's lastSeenAt.
//
//   GET    /v1/users/sessions         → SessionInfo[]
//   DELETE /v1/users/sessions/:id     → revoke one session
//
// Both endpoints require an authenticated user; both scope strictly to
// the caller's own user_id — there's no admin view yet. The session
// identifier the API exposes is a sha256 fingerprint of the family ID,
// so the raw jti never travels over the wire.

import (
	"net/http"
	"strconv"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/golang-jwt/jwt"
	"github.com/labstack/echo/v4"
)

// callerInfo extracts the user_id (set by the gateway after JWT
// validation) and the refresh-family ID this caller's access token
// belongs to. The latter is needed so the listing can flag the
// caller's own session as "current".
func callerInfo(ctx echo.Context) (userID int, familyID string, ok bool) {
	idStr := ctx.Request().Header.Get("user_id")
	if idStr == "" {
		return 0, "", false
	}
	uid, err := strconv.Atoi(idStr)
	if err != nil || uid <= 0 {
		return 0, "", false
	}

	// The gateway-validated JWT is forwarded in the Authorization header.
	// We don't re-verify signatures here (the gateway already did) — we
	// only parse the unverified claims to recover the jti so we can
	// match against jtiToFamily.
	var jtiVal string
	if tok := ctx.Get("user"); tok != nil {
		if jwtTok, ok := tok.(*jwt.Token); ok {
			if claims, ok := jwtTok.Claims.(jwt.MapClaims); ok {
				if v, _ := claims["jti"].(string); v != "" {
					jtiVal = v
				}
			}
		}
	}

	famID := ""
	if jtiVal != "" {
		refreshFamiliesMu.Lock()
		famID = jtiToFamily[jtiVal]
		refreshFamiliesMu.Unlock()
	}
	return uid, famID, true
}

func GetUserSessions(ctx echo.Context) error {
	uid, famID, ok := callerInfo(ctx)
	if !ok {
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "authentication required",
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    listSessionsForUser(uid, famID),
	})
}

func DeleteUserSession(ctx echo.Context) error {
	uid, _, ok := callerInfo(ctx)
	if !ok {
		return ctx.JSON(http.StatusUnauthorized, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "authentication required",
		})
	}
	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "session id required",
		})
	}
	if !revokeSessionByID(uid, id) {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "session not found",
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}
