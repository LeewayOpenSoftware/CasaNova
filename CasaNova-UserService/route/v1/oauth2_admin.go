package v1

import (
	"net/http"
	"regexp"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/labstack/echo/v4"
)

// validClientID matches UUIDs and similar safe identifiers used as client IDs.
// Reject anything that could carry path-traversal or injection characters.
var validClientID = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// validClientName matches reasonable display names for OAuth2 clients.
var validClientName = regexp.MustCompile(`^[a-zA-Z0-9 _.@()-]{1,128}$`)

// OAuth2 client management is admin-only: creating a client effectively
// hands out a credential that can be used to impersonate CasaNova users
// during the consent flow. Non-admin callers can't read the list either
// because the list itself (client IDs + redirect URIs) is considered
// sensitive infrastructure topology.
// requireOAuth2Admin gates OAuth2 client management to admins. No loopback
// bypass — the gateway proxies browser traffic over loopback, so trusting it
// would let any caller register/delete OAuth2 clients (mint tokens, DoS).
func requireOAuth2Admin(ctx echo.Context) bool {
	return rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage)
}

// PostOAuth2RegisterClient creates a new OAuth2 client.
//
// POST /v1/users/oauth2/clients
// body: {"name": "Gitea", "redirect_uris": ["https://gitea.home.lan/user/oauth2/callback"], "public": false, "scopes": ["openid","profile","email"]}
//
// Returns the created client AND the plaintext secret. The secret is
// shown exactly once — the admin must copy it immediately.
func PostOAuth2RegisterClient(ctx echo.Context) error {
	if !requireOAuth2Admin(ctx) {
		return ctx.JSON(http.StatusForbidden, echo.Map{"error": "admin role required"})
	}
	var req struct {
		Name         string   `json:"name"`
		RedirectURIs []string `json:"redirect_uris"`
		Public       bool     `json:"public"`
		Scopes       []string `json:"scopes,omitempty"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})
	}

	// Validate client name.
	if req.Name == "" || !validClientName.MatchString(req.Name) {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid client name: must be 1-128 alphanumeric/space/punctuation characters"})
	}

	// Validate redirect URIs are present and non-empty.
	if len(req.RedirectURIs) == 0 {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "at least one redirect_uri is required"})
	}
	for _, uri := range req.RedirectURIs {
		if uri == "" {
			return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "redirect_uris must not contain empty strings"})
		}
	}

	store := OAuth2Clients()
	if store == nil {
		return ctx.JSON(http.StatusServiceUnavailable, echo.Map{"error": "oauth2 store not initialized"})
	}
	client, secret, err := store.Register(req.Name, req.RedirectURIs, req.Scopes, req.Public)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})
	}

	// Audit the creation — OAuth2 client registration is a privileged action.
	actor := ctx.Request().Header.Get("user_id")
	if l := service.AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventUserRoleChange,
			Severity: auditlog.SeverityWarning,
			Actor:    actor,
			ActorIP:  ctx.RealIP(),
			Target:   client.ID,
			Message:  "registered oauth2 client: " + req.Name,
		})
	}

	// Return the secret in a separate field with a warning comment. The
	// stored client struct's SecretHash is NOT returned (the JSON tag
	// is `,omitempty` and we zero it out before marshaling).
	clientSafe := *client
	clientSafe.SecretHash = ""
	return ctx.JSON(http.StatusCreated, echo.Map{
		"client":        clientSafe,
		"client_secret": secret, // SHOWN ONCE — copy immediately
	})
}

// ListOAuth2Clients returns every registered client, without secrets.
// GET /v1/users/oauth2/clients
func ListOAuth2Clients(ctx echo.Context) error {
	if !requireOAuth2Admin(ctx) {
		return ctx.JSON(http.StatusForbidden, echo.Map{"error": "admin role required"})
	}
	store := OAuth2Clients()
	if store == nil {
		return ctx.JSON(http.StatusServiceUnavailable, echo.Map{"error": "oauth2 store not initialized"})
	}
	list := store.All()
	// Strip secret hashes before serialization — the admin list doesn't
	// need them and exposing bcrypt hashes is avoidable surface.
	safe := make([]interface{}, 0, len(list))
	for _, c := range list {
		cp := *c
		cp.SecretHash = ""
		safe = append(safe, cp)
	}
	return ctx.JSON(http.StatusOK, echo.Map{"clients": safe})
}

// DeleteOAuth2Client removes a registered client. Any access tokens
// already issued for this client remain valid until their natural
// expiry — revoking in-flight JWTs would require a persistent revocation
// list, which is out of scope for the minimum viable provider.
//
// DELETE /v1/users/oauth2/clients/:id
func DeleteOAuth2Client(ctx echo.Context) error {
	if !requireOAuth2Admin(ctx) {
		return ctx.JSON(http.StatusForbidden, echo.Map{"error": "admin role required"})
	}
	id := ctx.Param("id")

	// Validate the client ID format to prevent injection or traversal.
	if !validClientID.MatchString(id) {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid client id format"})
	}

	store := OAuth2Clients()
	if store == nil {
		return ctx.JSON(http.StatusServiceUnavailable, echo.Map{"error": "oauth2 store not initialized"})
	}
	if err := store.Delete(id); err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}

	// Audit the deletion.
	actor := ctx.Request().Header.Get("user_id")
	if l := service.AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventUserRoleChange,
			Severity: auditlog.SeverityWarning,
			Actor:    actor,
			ActorIP:  ctx.RealIP(),
			Target:   id,
			Message:  "deleted oauth2 client",
		})
	}

	return ctx.NoContent(http.StatusNoContent)
}
