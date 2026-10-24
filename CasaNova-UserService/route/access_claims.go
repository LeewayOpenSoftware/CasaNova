package route

import (
	"strconv"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
)

// finalizeAccessClaims applies the two decisions every ParseTokenFunc in this
// package must make identically once a token's signature has already
// validated: whether it should be honored as an ACCESS token at all, and
// what role to actually authorize the request against.
//
// OLA 3 — this used to be typed out separately in v1.go and v2.go, and the
// two copies had already drifted: v1 rejected claims minted with
// Issuer=="refresh" (a refresh token is only ever meant to be exchanged at
// POST /v1/users/refresh, never used directly against an API endpoint); v2's
// copy never had that check at all, so a caller holding only a refresh
// token — typically stored and handled with more care than an access token
// specifically because it should never be usable this way — could call any
// v2 endpoint directly with it. v2's own comment claimed to "match v1's
// role-recovery path" and did, for the role-recovery half; the refresh
// rejection wasn't part of what it was copying.
func finalizeAccessClaims(claims *jwt.Claims) (role string, ok bool) {
	if claims.Issuer == "refresh" {
		return "", false
	}
	// No user id = not a first-party access token (e.g. an OIDC id_token,
	// which is signed with the same key). jwt.Validate refuses it too.
	if claims.ID <= 0 {
		return "", false
	}

	// Wave 35-S / 35-U: tokens issued before Wave 35-U's login fix may
	// carry either an empty role claim (pre-role-claim builds) or "guest"
	// (Wave 35-R..S where PostUserLogin was calling rbac.Normalize("") and
	// getting RoleGuest back). In both cases the DB row is the
	// authoritative answer — the AutoMigrate backfill in pkg/sqlite/db.go
	// has already promoted legacy empty rows to "admin" by the time any
	// middleware runs. Re-read the DB whenever the token's role is empty
	// OR "guest" so existing sessions don't need to force-logout to
	// recover from the old bug. Real (non-empty, non-guest) roles continue
	// to come straight from the signed claim, so a forged "admin" claim
	// from a stolen signing key still cannot be elevated through this path
	// — the DB lookup only ever promotes guest→(whatever the DB says), it
	// does not let the token override the DB when the claim already says
	// admin.
	//
	// Skip the DB re-read for OAuth2-delegated tokens: they are minted
	// "guest" ON PURPOSE to de-privilege a relying party, so promoting them
	// back to the user's DB role would re-open the escalation. Only legacy
	// first-party ("casaos") guest/empty tokens get recovered.
	role = claims.Role
	if (role == "" || role == "guest") && claims.ID != 0 && claims.Issuer != "oauth2-delegated" {
		if dbUser := service.MyService.User().GetUserInfoById(strconv.Itoa(claims.ID)); dbUser.Id != 0 && dbUser.Role != "" {
			role = dbUser.Role
		}
	}
	return role, true
}
