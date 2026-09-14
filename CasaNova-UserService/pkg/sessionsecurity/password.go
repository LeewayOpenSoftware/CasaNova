package sessionsecurity

import "github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"

// ResetPassword applies the already-prepared password update. Session
// invalidation is deliberately kept here so every non-HTTP reset path has one
// place to enforce the security boundary.
func ResetPassword(userID int, update func()) error {
	update()
	return tokenrevocation.RevokeID(userID)
}
