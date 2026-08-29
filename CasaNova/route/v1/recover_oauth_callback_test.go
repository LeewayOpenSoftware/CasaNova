package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/google_drive"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/oauthflow"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// L-02 / G-01 — THE CLOUD OAUTH CALLBACK.
//
// GET /v1/recover/:type persists a provider refresh token into rclone's config
// and FUSE-mounts the result under /mnt as root. It sits inside the v1 group
// whose JWT middleware skips "loopback with no Authorization header", and the
// gateway proxies the LAN and the Cloudflare tunnel over 127.0.0.1 — so before
// this change a bare `curl https://<box>/v1/recover/GoogleDrive?code=…` from the
// internet ran the whole state machine as guest.
//
// The route cannot take a bearer token: it is a browser redirect arriving from a
// third-party relay. So the tests below assert the property that replaces one —
// the caller must present a state token this box minted for an identified
// principal, for this provider, recently, and exactly once.

// ─── stubs ──────────────────────────────────────────────────────────────────

// oauthStubNotify counts notifications. The embedded interface leaves every
// other method nil on purpose: a handler that reaches one panics, and the panic
// guard below reports that as the gate having leaked.
type oauthStubNotify struct {
	service.NotifyServer
	sent *[]map[string]interface{}
}

func (n oauthStubNotify) SendNotify(name string, message map[string]interface{}) {
	*n.sent = append(*n.sent, message)
}

type oauthStubStorage struct {
	service.StorageService
	err error

	// transaction bookkeeping for completeRecovery
	remotes    []string
	createErr  error
	mountErr   error
	created    *[]string
	mounted    *[]string
	deleted    *[]string
	configByID map[string]map[string]string
}

func (s oauthStubStorage) GetConfig() (httper.RemotesResult, error) {
	return httper.RemotesResult{Remotes: s.remotes}, s.err
}

func (s oauthStubStorage) GetConfigByName(name string) (map[string]string, error) {
	return s.configByID[name], nil
}

func (s oauthStubStorage) CreateConfig(_ map[string]string, name string, _ string) error {
	if s.createErr != nil {
		return s.createErr
	}
	*s.created = append(*s.created, name)
	return nil
}

func (s oauthStubStorage) MountStorage(mountPoint, fs string) error {
	if s.mountErr != nil {
		return s.mountErr
	}
	*s.mounted = append(*s.mounted, mountPoint+"|"+fs)
	return nil
}

func (s oauthStubStorage) DeleteConfigByName(name string) error {
	*s.deleted = append(*s.deleted, name)
	return nil
}

type oauthStubRepo struct {
	service.Repository
	notify  oauthStubNotify
	storage oauthStubStorage
}

func (r oauthStubRepo) Notify() service.NotifyServer    { return r.notify }
func (r oauthStubRepo) Storage() service.StorageService { return r.storage }

// installOAuthStubs points the authority at a scratch directory and the service
// layer at stubs, restoring both afterwards.
func installOAuthStubs(t *testing.T) *[]map[string]interface{} {
	t.Helper()
	oauthflow.Configure(filepath.Join(t.TempDir(), "oauth"))
	sent := &[]map[string]interface{}{}
	prev := service.MyService
	service.MyService = oauthStubRepo{
		notify:  oauthStubNotify{sent: sent},
		storage: oauthStubStorage{err: errNoRcloneInTest},
	}
	t.Cleanup(func() {
		service.MyService = prev
		oauthflow.Configure(oauthflow.DefaultStateDir)
	})
	return sent
}

var errNoRcloneInTest = &stubError{"rclone is not running in the test binary"}

type stubError struct{ s string }

func (e *stubError) Error() string { return e.s }

// callbackContext builds the request a relay redirect produces.
func callbackContext(t *testing.T, providerParam, pathState, rawQuery string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	target := "/v1/recover/" + providerParam
	if pathState != "" {
		target += "/" + pathState
	}
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, target, nil), rec)
	if pathState != "" {
		ctx.SetParamNames("type", "state")
		ctx.SetParamValues(providerParam, pathState)
	} else {
		ctx.SetParamNames("type")
		ctx.SetParamValues(providerParam)
	}
	return ctx, rec
}

func mintStateForTest(t *testing.T, p oauthflow.Provider, principal int) string {
	t.Helper()
	token, _, err := oauthflow.Default().Begin(p, principal, string(rbac.RoleAdmin), "https://box.example")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return token
}

func runCallback(t *testing.T, ctx echo.Context) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the callback reached the service layer (panic: %v). It must refuse "+
					"before any provider call, credential write or mount.", r)
			}
		}()
		err = GetRecoverStorage(ctx)
	}()
	return err
}

// ─── the gate ───────────────────────────────────────────────────────────────

// The confirmed attack, verbatim: no state, just a code.
func TestCallbackRefusesARequestWithNoState(t *testing.T) {
	sent := installOAuthStubs(t)

	ctx, rec := callbackContext(t, "GoogleDrive", "", "code=attacker-authorization-code")
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("handler returned %v; it answers with a refusal page, not an error", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a stateless callback got %d, want 403. This is the request that made the "+
			"box persist an attacker refresh token and mount their drive as root.", rec.Code)
	}
	if len(*sent) != 0 {
		t.Fatalf("a refused callback sent %d notification(s): %v. An endpoint reachable "+
			"unauthenticated from the internet must not be usable as a notification "+
			"spam primitive.", len(*sent), *sent)
	}
	if strings.Contains(rec.Body.String(), "code") {
		t.Fatalf("the refusal page echoes request detail: %q", rec.Body.String())
	}
}

func TestCallbackRefusesAnUnknownProvider(t *testing.T) {
	installOAuthStubs(t)
	for _, bad := range []string{"", "s3", "../GoogleDrive", "GoogleDrive2"} {
		ctx, rec := callbackContext(t, bad, "", "code=x")
		if err := runCallback(t, ctx); err != nil {
			t.Fatalf("provider %q: handler returned %v", bad, err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("provider %q got %d, want 403 — :type must not reach a permissive "+
				"default branch", bad, rec.Code)
		}
	}
}

// A minted state completes once. The second attempt is the replay.
func TestCallbackAcceptsAMintedStateExactlyOnce(t *testing.T) {
	sent := installOAuthStubs(t)
	token := mintStateForTest(t, oauthflow.ProviderGoogleDrive, 42)

	// No `code`, so the handler stops right after the state check without
	// making an outbound provider call. Reaching that stop is the proof the
	// state was accepted.
	ctx, rec := callbackContext(t, "GoogleDrive", "", "s="+token)
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("a legitimately minted state was refused (%d); the fix broke the feature "+
			"it was meant to protect", rec.Code)
	}
	if len(*sent) != 1 {
		t.Fatalf("an accepted callback sent %d notifications, want 1: %v", len(*sent), *sent)
	}

	ctx, rec = callbackContext(t, "GoogleDrive", "", "s="+token)
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("replay: handler returned %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the same state completed a second callback (%d); the flow is replayable", rec.Code)
	}
	if len(*sent) != 1 {
		t.Fatalf("the replayed callback produced another notification: %v", *sent)
	}
}

func TestCallbackAcceptsTheStateFromThePathSegment(t *testing.T) {
	installOAuthStubs(t)
	token := mintStateForTest(t, oauthflow.ProviderDropbox, 42)

	ctx, rec := callbackContext(t, "Dropbox", token, "code=")
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("a state delivered as a path segment was refused (%d); that is the shape "+
			"the minted authorization URL produces", rec.Code)
	}
}

func TestCallbackRefusesAStateMintedForAnotherProvider(t *testing.T) {
	installOAuthStubs(t)
	token := mintStateForTest(t, oauthflow.ProviderGoogleDrive, 42)

	ctx, rec := callbackContext(t, "Dropbox", token, "code=x")
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a GoogleDrive state completed a Dropbox callback (%d)", rec.Code)
	}
}

// Every refusal must look the same. A caller who cannot authenticate must not
// learn whether a state was unknown, expired, or for another provider.
func TestEveryRefusalIsIndistinguishable(t *testing.T) {
	installOAuthStubs(t)
	crossProvider := mintStateForTest(t, oauthflow.ProviderGoogleDrive, 42)

	type probe struct{ name, provider, query string }
	probes := []probe{
		{"no state", "Dropbox", "code=x"},
		{"unknown state", "Dropbox", "s=" + strings.Repeat("A", 43)},
		{"malformed state", "Dropbox", "s=nonsense"},
		{"wrong provider", "Dropbox", "s=" + crossProvider},
		{"unknown provider", "S3", "s=" + strings.Repeat("A", 43)},
	}
	var body string
	for i, p := range probes {
		ctx, rec := callbackContext(t, p.provider, "", p.query)
		if err := runCallback(t, ctx); err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", p.name, rec.Code)
		}
		if i == 0 {
			body = rec.Body.String()
			continue
		}
		if rec.Body.String() != body {
			t.Fatalf("%s produced a distinguishable refusal:\n got %q\nwant %q",
				p.name, rec.Body.String(), body)
		}
	}
}

// ─── the minted URL round-trips through the relay ───────────────────────────

// The authorization URL we hand the browser has to survive the third-party
// relay at cloudoauth.files.casaos.app, which redirects to whatever URL sits in
// the OAuth `state` parameter and appends the code. This walks that path with no
// network: mint → read the state the provider would echo → build the redirect
// the relay would issue → parse it back out → consume it.
func TestMintedAuthorizationURLSurvivesTheRelayRoundTrip(t *testing.T) {
	installOAuthStubs(t)
	token := mintStateForTest(t, oauthflow.ProviderGoogleDrive, 42)

	tpl := google_drive.GetConfig().AuthUrl
	authorizeURL, err := authorizeURLWithState(tpl, "https://box.example", oauthflow.ProviderGoogleDrive, token)
	if err != nil {
		t.Fatalf("authorizeURLWithState: %v", err)
	}
	if strings.Contains(authorizeURL, "${HOST}") {
		t.Fatalf("the legacy ${HOST} placeholder survived into a minted URL: %s", authorizeURL)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("minted URL does not parse: %v", err)
	}
	state := u.Query().Get("state")
	if state != "https://box.example/v1/recover/GoogleDrive/"+token {
		t.Fatalf("state parameter is %q; the relay redirects to exactly this URL", state)
	}

	// The relay appends the provider's code to the state URL.
	redirect, err := url.Parse(state + "?code=real-authorization-code")
	if err != nil {
		t.Fatalf("relay redirect does not parse: %v", err)
	}
	segments := strings.Split(strings.Trim(redirect.Path, "/"), "/")
	ctx, rec := callbackContext(t, segments[len(segments)-2], segments[len(segments)-1], "code=")
	if err := runCallback(t, ctx); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the callback produced by our own minted URL was refused (%d)", rec.Code)
	}
}

func TestExtractFlowStateAcceptsTheShapesARelayCanProduce(t *testing.T) {
	token := strings.Repeat("A", 43)
	cases := map[string]struct{ pathState, query string }{
		"path segment":   {token, "code=x"},
		"short query":    {"", "s=" + token + "&code=x"},
		"state query":    {"", "state=" + token},
		"state as a URL": {"", "state=" + url.QueryEscape("https://box.example/v1/recover/GoogleDrive/"+token)},
		"state URL with a query": {"", "state=" +
			url.QueryEscape("https://box.example/v1/recover/GoogleDrive/"+token+"?code=x")},
	}
	for name, c := range cases {
		ctx, _ := callbackContext(t, "GoogleDrive", c.pathState, c.query)
		if got := extractFlowState(ctx); got != token {
			t.Fatalf("%s: extractFlowState = %q, want the token", name, got)
		}
	}
}

// ─── the initiation half ────────────────────────────────────────────────────

func TestConnectRefusesCallersWithoutStorageWrite(t *testing.T) {
	installOAuthStubs(t)
	for _, role := range []string{"", string(rbac.RoleGuest), string(rbac.RoleUser)} {
		e := echo.New()
		rec := httptest.NewRecorder()
		ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/cloud/gdrive/connect", nil), rec)
		ctx.SetParamNames("provider")
		ctx.SetParamValues("gdrive")
		if role != "" {
			commonmw.SetRoleInContext(ctx, role, 5)
		}

		err := PostCloudProviderConnect(ctx)
		he, ok := err.(*echo.HTTPError)
		if !ok {
			t.Fatalf("role %q: got %T (%v); the gate must return an *echo.HTTPError so echo "+
				"aborts the chain", role, err, err)
		}
		if he.Code != http.StatusForbidden {
			t.Fatalf("role %q: status %d, want 403", role, he.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("role %q: the denial committed a body: %q", role, rec.Body.String())
		}
		if p := oauthflow.Default().Pending(); p != 0 {
			t.Fatalf("role %q: a refused initiation still registered %d flow(s)", role, p)
		}
	}
}

// A build with no OAuth application credentials cannot complete a flow, so it
// must not mint a state for one either — an unusable pending flow is capacity an
// attacker gets for free.
func TestConnectRefusesAnUnconfiguredProviderWithoutRegisteringAFlow(t *testing.T) {
	installOAuthStubs(t)
	if google_drive.GetConfig().ClientID != unconfiguredClientID {
		t.Skip("this build injects real OAuth client credentials")
	}
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/cloud/gdrive/connect", nil), rec)
	ctx.SetParamNames("provider")
	ctx.SetParamValues("gdrive")
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)

	if err := PostCloudProviderConnect(ctx); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 for a provider this build cannot talk to", rec.Code)
	}
	if p := oauthflow.Default().Pending(); p != 0 {
		t.Fatalf("a dead-end initiation registered %d pending flow(s)", p)
	}
}

// ─── the sibling surface that hands out authorization URLs ──────────────────

func TestListDriverInfoRefusesAnonymousCallers(t *testing.T) {
	installOAuthStubs(t)
	ctx, rec := anonContext()
	err := ListDriverInfo(ctx)
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("got %T (%v); an anonymous caller must be refused with an *echo.HTTPError", err, err)
	}
	if he.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — the provider catalogue and its authorization URLs "+
			"were readable from the internet", he.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("the denial committed a body: %q", rec.Body.String())
	}
}

// configureProvidersForTest stands in an OAuth application this build does not
// ship, so the tests can tell "refused by the permission check" apart from
// "this build has no client id".
func configureProvidersForTest(t *testing.T) {
	t.Helper()
	prev := providerAuthTemplateFn
	providerAuthTemplateFn = func(p oauthflow.Provider) (string, string) {
		return "https://provider.example/authorize?client_id=test-client&state=${HOST}%2Fv1%2Frecover%2F" +
			string(p), "test-client"
	}
	t.Cleanup(func() { providerAuthTemplateFn = prev })
}

func listDriverAuthURLs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var payload struct {
		Data []struct {
			Name    string `json:"name"`
			AuthUrl string `json:"auth_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if len(payload.Data) == 0 {
		t.Fatalf("the provider list is empty: %q", rec.Body.String())
	}
	out := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		out = append(out, d.AuthUrl)
	}
	return out
}

func TestListDriverInfoMintsNothingForAReadOnlyCaller(t *testing.T) {
	installOAuthStubs(t)
	configureProvidersForTest(t)
	ctx, rec := identifiedContext(9, rbac.RoleUser) // storage:read, no storage:write

	if err := ListDriverInfo(ctx); err != nil {
		t.Fatalf("a storage reader may still see the provider list: %v", err)
	}
	for _, authURL := range listDriverAuthURLs(t, rec) {
		if authURL != "" {
			t.Fatalf("an authorization URL was handed to a caller without storage:write: %s", authURL)
		}
	}
	if p := oauthflow.Default().Pending(); p != 0 {
		t.Fatalf("listing minted %d flow(s) for a caller that cannot complete one", p)
	}
}

// The other half of the same contract: a permitted caller must still get a
// working URL, and it must carry a state token that the callback accepts. A fix
// that only ever refuses is not a fix.
func TestListDriverInfoStillMintsUsableURLsForAdmins(t *testing.T) {
	installOAuthStubs(t)
	configureProvidersForTest(t)
	ctx, rec := identifiedContext(1, rbac.RoleAdmin)

	if err := ListDriverInfo(ctx); err != nil {
		t.Fatalf("ListDriverInfo: %v", err)
	}
	urls := listDriverAuthURLs(t, rec)
	if len(urls) != 3 {
		t.Fatalf("want three providers, got %d", len(urls))
	}
	for _, authURL := range urls {
		if authURL == "" {
			t.Fatal("an administrator got no authorization URL; the feature is broken")
		}
		if strings.Contains(authURL, "${HOST}") {
			t.Fatalf("the legacy placeholder survived: %s", authURL)
		}
		if stateFromAuthorizeURL(authURL) == "" {
			t.Fatalf("a minted URL carries no state token: %s", authURL)
		}
	}
	if p := oauthflow.Default().Pending(); p != 3 {
		t.Fatalf("three minted URLs registered %d flow(s)", p)
	}

	// The token in the Google Drive URL completes its own callback.
	token := stateFromAuthorizeURL(urls[0])
	cbCtx, cbRec := callbackContext(t, "GoogleDrive", token, "code=")
	if err := runCallback(t, cbCtx); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cbRec.Code != http.StatusOK {
		t.Fatalf("the state from a minted URL was refused by the callback (%d)", cbRec.Code)
	}
}

func TestConnectMintsAFlowForAPermittedCaller(t *testing.T) {
	installOAuthStubs(t)
	configureProvidersForTest(t)

	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cloud/gdrive/connect", nil)
	req.Host = "box.example:8080"
	ctx := e.NewContext(req, rec)
	ctx.SetParamNames("provider")
	ctx.SetParamValues("gdrive")
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)

	if err := PostCloudProviderConnect(ctx); err != nil {
		t.Fatalf("PostCloudProviderConnect: %v", err)
	}
	var payload struct {
		Data struct {
			AuthorizeURL string `json:"authorize_url"`
			State        string `json:"state"`
			FlowID       string `json:"flow_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if payload.Data.State == "" || payload.Data.FlowID == "" {
		t.Fatalf("connect returned no state/flow: %q", rec.Body.String())
	}
	if !strings.Contains(payload.Data.AuthorizeURL, url.QueryEscape("http://box.example:8080/v1/recover/GoogleDrive/")) {
		t.Fatalf("the callback URL does not point back at the origin the caller used: %s",
			payload.Data.AuthorizeURL)
	}
	if p := oauthflow.Default().Pending(); p != 1 {
		t.Fatalf("connect registered %d flows, want 1", p)
	}
}

// The Host header is client-controlled and is interpolated into a URL a browser
// will follow. A header carrying a second URL or a control character must not
// produce an authorization URL at all.
func TestConnectRefusesAnUnusableHostHeader(t *testing.T) {
	installOAuthStubs(t)
	configureProvidersForTest(t)

	for _, host := range []string{"box.example/evil.com", "box.example:99999999", "", "box example"} {
		e := echo.New()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/cloud/gdrive/connect", nil)
		req.Host = host
		ctx := e.NewContext(req, rec)
		ctx.SetParamNames("provider")
		ctx.SetParamValues("gdrive")
		commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)

		if err := PostCloudProviderConnect(ctx); err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		if rec.Code == http.StatusOK {
			t.Fatalf("host %q produced an authorization URL: %s", host, rec.Body.String())
		}
	}
	if p := oauthflow.Default().Pending(); p != 0 {
		t.Fatalf("refused initiations registered %d flow(s)", p)
	}
}

// ─── the credential/mount transaction ───────────────────────────────────────

// installTransactionStubs wires a storage layer that records what the
// transaction did, so the assertions below are about ORDER and COMPENSATION,
// not about rclone.
func installTransactionStubs(t *testing.T, createErr, mountErr error) (*oauthStubStorage, *[]map[string]interface{}) {
	t.Helper()
	oauthflow.Configure(filepath.Join(t.TempDir(), "oauth"))
	sent := &[]map[string]interface{}{}
	created, mounted, deleted := &[]string{}, &[]string{}, &[]string{}
	storage := oauthStubStorage{
		createErr: createErr, mountErr: mountErr,
		created: created, mounted: mounted, deleted: deleted,
		configByID: map[string]map[string]string{},
	}
	prev := service.MyService
	service.MyService = oauthStubRepo{notify: oauthStubNotify{sent: sent}, storage: storage}
	t.Cleanup(func() {
		service.MyService = prev
		oauthflow.Configure(oauthflow.DefaultStateDir)
	})
	return &storage, sent
}

func runCompleteRecovery(t *testing.T, identity string) (*httptest.ResponseRecorder, oauthflow.Flow) {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/recover/GoogleDrive", nil), rec)
	flow := oauthflow.Flow{ID: "flow-1", PrincipalID: 42, Provider: oauthflow.ProviderGoogleDrive}
	params := map[string]string{"username": identity, "token": "{}"}
	if err := completeRecovery(ctx, oauthflow.ProviderGoogleDrive, flow, identity, params); err != nil {
		t.Fatalf("completeRecovery returned %v", err)
	}
	return rec, flow
}

// A failed mount must not leave a provider refresh token on disk. The pre-fix
// code discarded both return values, so a failed mount left a live credential
// that CheckAndMountAll would retry on every boot, and still announced success.
func TestAFailedMountRollsTheCredentialBack(t *testing.T) {
	storage, sent := installTransactionStubs(t, nil, &stubError{"mount refused"})

	runCompleteRecovery(t, "victim@example.com")

	if len(*storage.created) != 1 {
		t.Fatalf("the credential was not written before the mount: %v", *storage.created)
	}
	if len(*storage.deleted) != 1 || (*storage.deleted)[0] != (*storage.created)[0] {
		t.Fatalf("a failed mount left the credential behind: created %v, deleted %v",
			*storage.created, *storage.deleted)
	}
	if len(*sent) != 1 || (*sent)[0]["status"] != "fail" {
		t.Fatalf("a failed mount was not reported as a failure: %v", *sent)
	}
	// Audit the ledger against a world where the rolled-back remote name IS
	// present, so a stamp written before the mount succeeded shows up as an
	// attribution the box cannot honestly make.
	name := (*storage.created)[0]
	oauthflow.Default().EnsureBaseline([]oauthflow.RemoteInfo{}, nil)
	h := oauthflow.Default().Health([]oauthflow.RemoteInfo{
		{Name: name, Type: "drive", MountPoint: "/mnt/" + name},
	}, nil)
	if h.AttributedRemotes != 0 {
		t.Fatalf("a remote whose mount failed was stamped as created by a completed flow: %+v", h)
	}
}

// A failed credential write must not be followed by a mount of a remote that
// does not exist, and must not be announced as success.
func TestAFailedCredentialWriteStopsBeforeTheMount(t *testing.T) {
	storage, sent := installTransactionStubs(t, &stubError{"config/create failed"}, nil)

	runCompleteRecovery(t, "victim@example.com")

	if len(*storage.mounted) != 0 {
		t.Fatalf("a mount was attempted after the credential write failed: %v", *storage.mounted)
	}
	if len(*sent) != 1 || (*sent)[0]["status"] != "fail" {
		t.Fatalf("a failed credential write was reported as %v", *sent)
	}
}

// The happy path stamps provenance only after the mount actually succeeded, and
// the mount lands under /mnt with a name derived from — but not equal to — the
// provider-supplied identity.
func TestASuccessfulRecoveryIsStampedAndMountedUnderMountRoot(t *testing.T) {
	storage, sent := installTransactionStubs(t, nil, nil)

	runCompleteRecovery(t, "../../etc/cron.d/pwn@evil.com")

	if len(*storage.created) != 1 || len(*storage.mounted) != 1 {
		t.Fatalf("credential/mount not both applied: created %v mounted %v",
			*storage.created, *storage.mounted)
	}
	name := (*storage.created)[0]
	if strings.ContainsAny(name, "/\\.") {
		t.Fatalf("the provider-supplied identity reached the remote name: %q", name)
	}
	if want := "/mnt/" + name + "|" + name + ":"; (*storage.mounted)[0] != want {
		t.Fatalf("mounted %q, want %q — the mount must land under /mnt with the sanitized name",
			(*storage.mounted)[0], want)
	}
	if len(*sent) != 1 || (*sent)[0]["status"] != "success" {
		t.Fatalf("a successful recovery reported %v", *sent)
	}
	h := oauthflow.Default().Health([]oauthflow.RemoteInfo{
		{Name: name, Type: "drive", MountPoint: "/mnt/" + name},
	}, nil)
	if h.AttributedRemotes != 1 || len(h.UnattributedRemotes) != 0 {
		t.Fatalf("a remote created by a completed flow is not attributed to it: %+v", h)
	}
}

// ─── observability ──────────────────────────────────────────────────────────

func TestOAuthHealthRefusesAnonymousCallersAndReportsItsInvariant(t *testing.T) {
	installOAuthStubs(t)

	ctx, _ := anonContext()
	err := GetCloudOAuthHealth(ctx)
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusForbidden {
		t.Fatalf("anonymous health read: got %v (%T), want a 403 *echo.HTTPError", err, err)
	}

	// An admin gets a report even when rclone cannot be reached — and the
	// baseline is NOT captured from a failed listing.
	ctx, rec := identifiedContext(1, rbac.RoleAdmin)
	if err := GetCloudOAuthHealth(ctx); err != nil {
		t.Fatalf("admin health read: %v", err)
	}
	var payload struct {
		Data struct {
			BaselineCaptured bool   `json:"baseline_captured"`
			StorePath        string `json:"store_path"`
			Detail           string `json:"detail"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if payload.Data.BaselineCaptured {
		t.Fatal("the provenance baseline was captured from a failed remote listing; every " +
			"real remote would be reclassified as unexplained")
	}
	if payload.Data.StorePath == "" || !strings.Contains(payload.Data.Detail, "audit unavailable") {
		t.Fatalf("health does not say what it could not check: %+v", payload.Data)
	}
}
