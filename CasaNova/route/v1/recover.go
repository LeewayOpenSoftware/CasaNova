package v1

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/dropbox"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/google_drive"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/onedrive"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/oauthflow"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// recoverNotifyEvent is the message-bus event the dashboard listens on for the
// outcome of a cloud-storage authorization.
const recoverNotifyEvent = "casaos:file:recover"

// unconfiguredClientID is the placeholder this fork ships instead of real OAuth
// application credentials (the Makefile does not inject them via ldflags). A
// provider whose client id still reads this cannot complete a token exchange, so
// the initiation endpoints say so plainly instead of handing the user an
// authorization URL that dead-ends at the provider.
const unconfiguredClientID = "private build"

// recoverErrorPage returns a small static HTML page for an OAuth
// callback failure. Wave 33: previously every error path here did
// `<p>...</p>` + err.Error() + `<script>window.close()</script>` and
// concatenated err.Error() raw into the response. The error string
// frequently includes the upstream HTTP body verbatim (rclone wraps
// the provider response without escaping it), so an attacker who can
// lure a logged-in dashboard user to a crafted OAuth-callback URL
// (`/v1/recover/GoogleDrive?code=...`) can land script content in
// the casanova origin and execute it under the victim's session.
// Even if the upstream provider sanitises its own bodies, an
// intermediate proxy or DNS hijack can inject content. Concatenating
// untrusted strings into HTML is a footgun; html.EscapeString fixes
// it for both legitimate and crafted inputs.
func recoverErrorPage(headline, detail string) string {
	return `<p>` + html.EscapeString(headline) + `: ` + html.EscapeString(detail) + `</p><script>window.close()</script>`
}

// oauthBackgroundCtx returns a 30-second bounded context for OAuth
// callback operations along with its cancel func. Callers MUST defer
// the cancel to satisfy go vet's lostcancel check — the deadline alone
// would still eventually fire, but vet (correctly) flags the discarded
// cancel because timers backing WithTimeout cling to a goroutine.
//
// Wave 35-X: previously every call site used context.Background() with
// no deadline — a slow Google/Dropbox/OneDrive API hung the user's
// browser-redirect page indefinitely. 30s is generous for a single
// OAuth API call; anything longer is a real failure the user needs to
// see.
func oauthBackgroundCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ---------------------------------------------------------------------------
// Callback
// ---------------------------------------------------------------------------

// GetRecoverStorage completes a cloud-storage authorization.
//
// L-02/G-01: this endpoint used to accept ANY request. It persists a provider
// refresh token into rclone's configuration and FUSE-mounts the resulting remote
// under /mnt as root, and the v1 JWT skipper (loopback && no Authorization
// header) plus a gateway that proxies the whole internet over 127.0.0.1 meant a
// tokenless request from anywhere resolved to guest and ran it. There was no
// state, no nonce, no principal, no provider binding and no replay rejection,
// because the OAuth `state` parameter is spent as a redirect target by the
// third-party relay at cloudoauth.files.casaos.app and never reaches us as a
// secret.
//
// The route is still unauthenticated by necessity — it is a browser redirect
// arriving from a provider, so it cannot carry a bearer token. Authorization now
// comes from the state token minted by pkg/oauthflow at initiation time, which is
// unguessable, single-use, expiring, bound to the principal who started the flow
// and bound to this provider. Every rejection produces one identical refusal
// page: this handler must not tell an unauthenticated caller whether a state was
// unknown, expired or issued for a different provider.
func GetRecoverStorage(ctx echo.Context) error {
	provider, ok := oauthflow.CanonicalProvider(strings.TrimSuffix(ctx.Param("type"), "/"))
	if !ok {
		return recoverRefusal(ctx)
	}

	// The state is consumed BEFORE the code is read and before any provider call
	// is made. An unauthorized caller must not be able to make this process
	// originate an outbound token exchange, nor to spend a notification.
	flow, err := oauthflow.Default().Consume(extractFlowState(ctx), provider)
	if err != nil {
		return recoverRefusal(ctx)
	}

	code := ctx.QueryParam("code")
	if code == "" {
		return recoverFailure(ctx, provider, "Code cannot be empty", "")
	}

	identity, params, err := exchangeProviderCode(provider, code)
	if err != nil {
		var pe *providerError
		if errors.As(err, &pe) {
			logger.Error("oauth callback provider step failed",
				zap.String("stage", pe.headline), zap.Error(pe.cause),
				zap.String("provider", string(provider)), zap.String("flow", flow.ID))
			return recoverFailure(ctx, provider, pe.headline, pe.cause.Error())
		}
		return recoverFailure(ctx, provider, "Initialization failure", err.Error())
	}

	return completeRecovery(ctx, provider, flow, identity, params)
}

// providerError carries the headline the pre-authority handler used for each
// stage, so the user-visible page and the notification keep their wording.
type providerError struct {
	headline string
	cause    error
}

func (e *providerError) Error() string { return e.headline + ": " + e.cause.Error() }
func (e *providerError) Unwrap() error { return e.cause }

func stageErr(headline string, cause error) error {
	return &providerError{headline: headline, cause: cause}
}

// exchangeProviderCode runs the provider-specific half: swap the authorization
// code for tokens and read back the account identity. The returned identity is
// PROVIDER-SUPPLIED and untrusted — completeRecovery never uses it as a path.
func exchangeProviderCode(p oauthflow.Provider, code string) (string, map[string]string, error) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")

	switch p {
	case oauthflow.ProviderGoogleDrive:
		gd := google_drive.GetConfig()
		gd.Code = code
		initCtx, initCancel := oauthBackgroundCtx()
		err := gd.Init(initCtx)
		initCancel()
		if err != nil {
			return "", nil, stageErr("Initialization failure", err)
		}
		infoCtx, infoCancel := oauthBackgroundCtx()
		identity, err := gd.GetUserInfo(infoCtx)
		infoCancel()
		if err != nil {
			return "", nil, stageErr("Failed to get user information", err)
		}
		return identity, map[string]string{
			"username":      identity,
			"client_id":     gd.ClientID,
			"client_secret": gd.ClientSecret,
			"scope":         "drive",
			"token": `{"access_token":"` + gd.AccessToken + `","token_type":"Bearer","refresh_token":"` + gd.RefreshToken +
				`","expiry":"` + today + `T` + now.Add(time.Hour*1).Add(time.Minute*50).Format("15:04:05") + `Z"}`,
		}, nil

	case oauthflow.ProviderDropbox:
		db := dropbox.GetConfig()
		db.Code = code
		initCtx, initCancel := oauthBackgroundCtx()
		err := db.Init(initCtx)
		initCancel()
		if err != nil {
			return "", nil, stageErr("Initialization failure", err)
		}
		infoCtx, infoCancel := oauthBackgroundCtx()
		identity, err := db.GetUserInfo(infoCtx)
		infoCancel()
		if err != nil {
			return "", nil, stageErr("Failed to get user information", err)
		}
		return identity, map[string]string{
			"username":      identity,
			"client_id":     db.AppKey,
			"client_secret": db.AppSecret,
			"token": `{"access_token":"` + db.AccessToken + `","token_type":"bearer","refresh_token":"` + db.Addition.RefreshToken +
				`","expiry":"` + today + `T` + now.Add(time.Hour*3).Add(time.Minute*50).Format("15:04:05") + `.780385354Z"}`,
		}, nil

	case oauthflow.ProviderOnedrive:
		od := onedrive.GetConfig()
		od.Code = code
		initCtx, initCancel := oauthBackgroundCtx()
		err := od.Init(initCtx)
		initCancel()
		if err != nil {
			return "", nil, stageErr("Initialization failure", err)
		}
		infoCtx, infoCancel := oauthBackgroundCtx()
		identity, driveID, driveType, err := od.GetInfo(infoCtx)
		infoCancel()
		if err != nil {
			return "", nil, stageErr("Failed to get user information", err)
		}
		return identity, map[string]string{
			"username":      identity,
			"client_id":     od.ClientID,
			"client_secret": od.ClientSecret,
			"drive_id":      driveID,
			"drive_type":    driveType,
			"token": `{"access_token":"` + od.AccessToken + `","token_type":"bearer","refresh_token":"` + od.RefreshToken +
				`","expiry":"` + today + `T` + now.Add(time.Hour*3).Add(time.Minute*50).Format("15:04:05") + `.780385354Z"}`,
		}, nil
	}
	return "", nil, oauthflow.ErrUnknownProvider
}

// completeRecovery persists the credential and mounts it as ONE transaction.
//
// The pre-authority code discarded the return values of CreateConfig and
// MountStorage and then announced "success" unconditionally, so a failed write
// was followed by a mount of a remote that did not exist, and a failed mount
// left a long-lived refresh token on disk with nothing using it and nobody told.
// Here the credential is written first, the mount second, and a failed mount
// rolls the credential back — the box never ends up holding a persisted provider
// token for a mount that does not exist.
func completeRecovery(ctx echo.Context, p oauthflow.Provider, flow oauthflow.Flow, identity string, params map[string]string) error {
	configs, err := service.MyService.Storage().GetConfig()
	if err != nil {
		logger.Error("oauth callback could not read rclone config", zap.Error(err), zap.String("provider", string(p)))
		return recoverFailure(ctx, p, "Failed to get rclone config", err.Error())
	}
	for _, name := range configs.Remotes {
		cf, err := service.MyService.Storage().GetConfigByName(name)
		if err != nil {
			logger.Error("then get config by name error: ", zap.Error(err), zap.Any("name", name))
			continue
		}
		if cf["type"] == p.RcloneType() && cf["username"] == identity {
			if err := service.MyService.Storage().CheckAndMountByName(name); err != nil {
				logger.Error("check and mount by name error: ", zap.Error(err), zap.Any("name", name))
			}
			sendRecoverNotify(p, flow, "warn", "The same configuration has been added")
			return ctx.HTML(http.StatusOK, `<p>The same configuration has been added</p><script>window.close()</script>`)
		}
	}

	remoteName := oauthflow.SanitizeRemoteName(identity, p, time.Now().Unix())
	mountPoint, err := oauthflow.MountPointFor(remoteName)
	if err != nil {
		logger.Error("oauth callback refused an unsafe remote name", zap.Error(err), zap.String("provider", string(p)))
		return recoverFailure(ctx, p, "Failed to create rclone config", "unsafe remote name")
	}
	params["mount_point"] = mountPoint

	if err := guardPanic(func() error {
		return service.MyService.Storage().CreateConfig(params, remoteName, p.RcloneType())
	}); err != nil {
		logger.Error("oauth callback could not create rclone config", zap.Error(err), zap.String("remote", remoteName))
		return recoverFailure(ctx, p, "Failed to create rclone config", err.Error())
	}

	if err := guardPanic(func() error { return service.MyService.Storage().MountStorage(mountPoint, remoteName+":") }); err != nil {
		// Roll the credential back. Leaving it behind would persist a provider
		// refresh token that nothing mounts and that CheckAndMountAll would
		// retry on every boot.
		if delErr := service.MyService.Storage().DeleteConfigByName(remoteName); delErr != nil {
			logger.Error("oauth callback could not roll back rclone config after a failed mount",
				zap.Error(delErr), zap.String("remote", remoteName))
		}
		logger.Error("oauth callback mount failed", zap.Error(err), zap.String("remote", remoteName))
		return recoverFailure(ctx, p, "Failed to mount storage", err.Error())
	}

	oauthflow.Default().RecordRemote(remoteName, oauthflow.RemoteRecord{
		Provider:    p,
		FlowID:      flow.ID,
		PrincipalID: flow.PrincipalID,
		MountPoint:  mountPoint,
		CreatedAt:   time.Now(),
	})

	sendRecoverNotify(p, flow, "success", "Success")
	return ctx.HTML(http.StatusOK, `<p>Just close the page</p><script>window.close()</script>`)
}

// guardPanic turns a panic inside a storage mutation into an error.
//
// The rclone rc client logs res.Body() before checking the request error, and
// resty can hand back a nil *Response when the unix socket is unreachable — so
// an rclone outage panics inside CreateConfig. Echo's Recover middleware would
// turn that into a 500, but it would also unwind past the rollback below and
// leave a persisted provider credential with no mount behind it. The
// transaction has to own its own failure, so it catches the panic here rather
// than relying on a middleware that runs after the damage.
func guardPanic(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("storage backend unavailable: %v", r)
		}
	}()
	return fn()
}

func sendRecoverNotify(p oauthflow.Provider, flow oauthflow.Flow, status, message string) {
	notify := map[string]interface{}{
		"status":       status,
		"message":      message,
		"driver":       string(p),
		"flow_id":      flow.ID,
		"principal_id": flow.PrincipalID,
	}
	service.MyService.Notify().SendNotify(recoverNotifyEvent, notify)
}

// recoverFailure reports a failure that happened AFTER a valid state was spent,
// so the dashboard that started the flow learns why. It is only reachable by a
// caller who presented a live state token, which is what keeps the notification
// bus from becoming an unauthenticated spam primitive.
func recoverFailure(ctx echo.Context, p oauthflow.Provider, headline, detail string) error {
	sendRecoverNotify(p, oauthflow.Flow{}, "fail", headline)
	if detail == "" {
		return ctx.HTML(http.StatusOK, recoverErrorPage(headline, ""))
	}
	return ctx.HTML(http.StatusOK, recoverErrorPage(headline, detail))
}

// recoverRefusal is the ONLY response an unauthorized callback ever gets. It
// carries no detail, sends no notification and writes no per-request log line —
// an endpoint reachable unauthenticated from the internet must not be usable as
// a notification or log-flooding primitive. The rejection is counted instead and
// surfaces through GET /v1/cloud/oauth-health.
func recoverRefusal(ctx echo.Context) error {
	return ctx.HTML(http.StatusForbidden,
		`<p>This authorization link is not valid for this device. Start the connection again from the dashboard.</p><script>window.close()</script>`)
}

// extractFlowState pulls the state token out of the callback.
//
// The redirect that lands here is built by a third party: the provider sends the
// browser to cloudoauth.files.casaos.app, which re-sends it to the URL we put in
// the OAuth `state` parameter. We mint that URL with the token as the last path
// segment, which survives a relay that appends "?code=…". Because we do not
// control the relay, the query forms are accepted as well, and a `state` that
// comes back as the whole URL is reduced to its last path segment. Every form
// ends at the same WellFormedState check, so accepting several shapes widens
// nothing: an attacker still has to present a token the authority issued.
func extractFlowState(ctx echo.Context) string {
	if s := ctx.Param("state"); s != "" {
		return s
	}
	if s := ctx.QueryParam("s"); s != "" {
		return s
	}
	s := ctx.QueryParam("state")
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// ---------------------------------------------------------------------------
// Initiation
// ---------------------------------------------------------------------------

// originHostRe accepts a hostname or IP literal with an optional port and
// nothing else. The Host header is client-controlled, and it is interpolated
// into the URL a browser will be redirected to, so a header carrying a control
// character or a second URL must not reach the provider.
var originHostRe = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$`)

// requestOrigin reconstructs the origin the authenticated caller is reaching
// this box on, which is where the provider round-trip has to come back to. It is
// NOT a trust input: it only decides where the initiating user's own browser is
// sent, and the flow it is stored on is bound to that same user.
func requestOrigin(ctx echo.Context) (string, error) {
	r := ctx.Request()
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if !originHostRe.MatchString(host) {
		return "", errors.New("unusable Host header")
	}
	scheme := "http"
	switch {
	case strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"):
		scheme = "https"
	case strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "http"):
		scheme = "http"
	case r.TLS != nil:
		scheme = "https"
	}
	return scheme + "://" + host, nil
}

// providerAuthTemplateFn is the seam the tests use to stand in a provider whose
// OAuth application credentials are configured. This build ships placeholders
// (see unconfiguredClientID), so without it the "a permitted caller does get a
// usable authorization URL" half of the contract could not be exercised at all,
// and a broken permission check would look identical to a build with no
// credentials. Production never reassigns it.
var providerAuthTemplateFn = providerAuthTemplate

// providerAuthTemplate is the provider's authorization URL as the driver
// package builds it, still carrying the legacy "${HOST}" placeholder in its
// state parameter. mintAuthorizeURL replaces that parameter outright.
func providerAuthTemplate(p oauthflow.Provider) (tpl string, clientID string) {
	switch p {
	case oauthflow.ProviderGoogleDrive:
		c := google_drive.GetConfig()
		return c.AuthUrl, c.ClientID
	case oauthflow.ProviderDropbox:
		c := dropbox.GetConfig()
		return c.AuthUrl, c.AppKey
	case oauthflow.ProviderOnedrive:
		c := onedrive.GetConfig()
		return c.AuthUrl, c.ClientID
	}
	return "", ""
}

var errProviderUnconfigured = errors.New("this build has no OAuth application credentials for that provider")

// mintAuthorizeURL registers a flow for the calling principal and returns the
// provider authorization URL that carries its state token.
//
// The state parameter is rewritten, not string-substituted: the templates encode
// it differently from each other and the legacy "${HOST}" substitution had to be
// percent-escaped by hand. Re-encoding through net/url gets that right for all
// three and removes the placeholder entirely, so no path can emit an
// authorization URL without a state token any more.
func mintAuthorizeURL(ctx echo.Context, p oauthflow.Provider) (string, oauthflow.Flow, error) {
	tpl, clientID := providerAuthTemplateFn(p)
	if tpl == "" {
		return "", oauthflow.Flow{}, oauthflow.ErrUnknownProvider
	}
	if clientID == "" || clientID == unconfiguredClientID {
		return "", oauthflow.Flow{}, errProviderUnconfigured
	}
	origin, err := requestOrigin(ctx)
	if err != nil {
		return "", oauthflow.Flow{}, err
	}
	token, flow, err := oauthflow.Default().Begin(p, commonmw.UserIDFromContext(ctx),
		string(commonmw.RoleFromContext(ctx)), origin)
	if err != nil {
		return "", oauthflow.Flow{}, err
	}
	authorizeURL, err := authorizeURLWithState(tpl, origin, p, token)
	if err != nil {
		return "", oauthflow.Flow{}, err
	}
	return authorizeURL, flow, nil
}

// authorizeURLWithState rewrites the template's `state` parameter to the
// callback URL carrying the token as its last path segment.
func authorizeURLWithState(tpl, origin string, p oauthflow.Provider, token string) (string, error) {
	u, err := url.Parse(tpl)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("state", origin+"/v1/recover/"+string(p)+"/"+token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// PostCloudProviderConnect starts a cloud-storage authorization.
//
// POST /v1/cloud/:provider/connect → { authorize_url, state, flow_id, expires_at }
//
// This is the endpoint the UI already expected (api/storage.ts
// connectCloudProvider) and that the backend never had, which is why the whole
// flow depended on an authorization URL handed out with no state at all. It
// requires storage:write — the same permission as any other mount — because
// completing the flow it starts mounts a filesystem as root.
func PostCloudProviderConnect(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	provider, ok := oauthflow.CanonicalProvider(ctx.Param("provider"))
	if !ok {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "unknown cloud provider",
		})
	}
	authorizeURL, flow, err := mintAuthorizeURL(ctx, provider)
	if err != nil {
		status, message := connectFailureResponse(err)
		return ctx.JSON(status, model.Result{Success: common_err.SERVICE_ERROR, Message: message})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"authorize_url": authorizeURL,
			"state":         stateFromAuthorizeURL(authorizeURL),
			"flow_id":       flow.ID,
			"expires_at":    flow.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
}

func connectFailureResponse(err error) (int, string) {
	switch {
	case errors.Is(err, errProviderUnconfigured):
		return http.StatusNotImplemented, errProviderUnconfigured.Error()
	case errors.Is(err, oauthflow.ErrTooManyPending):
		return http.StatusTooManyRequests, "too many authorizations in flight; try again in a few minutes"
	case errors.Is(err, oauthflow.ErrNoPrincipal):
		return http.StatusForbidden, "an identified user must start this flow"
	default:
		return http.StatusBadRequest, "cannot build an authorization URL for this request"
	}
}

// stateFromAuthorizeURL echoes back the token embedded in the minted URL so the
// caller can correlate its own flow. It is the caller's own secret; the caller
// is the authenticated principal the flow is bound to.
func stateFromAuthorizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	s := u.Query().Get("state")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

// listCloudRemotes enumerates rclone remotes in the shape the authority audits.
// The error is passed through rather than swallowed: an empty list because
// rclone is down must never be mistaken for "there are no remotes".
func listCloudRemotes() ([]oauthflow.RemoteInfo, error) {
	configs, err := service.MyService.Storage().GetConfig()
	if err != nil {
		return nil, err
	}
	out := make([]oauthflow.RemoteInfo, 0, len(configs.Remotes))
	for _, name := range configs.Remotes {
		cf, err := service.MyService.Storage().GetConfigByName(name)
		if err != nil {
			// One unreadable remote must not turn the whole audit into a
			// silent "everything is fine".
			return nil, err
		}
		out = append(out, oauthflow.RemoteInfo{
			Name:       name,
			Type:       cf["type"],
			MountPoint: cf["mount_point"],
		})
	}
	return out, nil
}

// GetCloudOAuthHealth reports the callback authority's own invariants.
//
// GET /v1/cloud/oauth-health
//
// It also captures the provenance baseline on its first successful run, which is
// what makes the subsystem self-provisioning: nothing has to be seeded by hand,
// and the baseline is only committed when the remote list was actually readable.
func GetCloudOAuthHealth(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	remotes, listErr := listCloudRemotes()
	oauthflow.Default().EnsureBaseline(remotes, listErr)
	health := oauthflow.Default().Health(remotes, listErr)
	status := common_err.SUCCESS
	if !health.Healthy {
		status = common_err.SERVICE_ERROR
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: status,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    health,
	})
}
