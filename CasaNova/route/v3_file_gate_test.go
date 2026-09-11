package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
)

// GET /v3/file is mounted standalone in main.go, outside InitV1Router and
// InitV2Router, so it inherits none of their middleware. I verified the
// mounting before touching it: it IS deliberate — the handler is a raw
// http.Handler taking its token from ?token= because a browser download cannot
// set an Authorization header, and V3FilePath is registered with the gateway
// from the OpenAPI document. The query-token contract is intentional. The two
// things missing from it were not.
//
// 1. It validated the SIGNATURE and never asked whether the token had been
//    revoked, so a logged-out or deleted principal kept downloading for the
//    token's full lifetime, across restarts. Every other entry point in the
//    process checks this.
// 2. It performed no authorisation at all. A valid signature says "some
//    principal", not "a principal allowed to read files".

// A request with no token at all is refused before anything touches the disk.
func TestV3FileRequiresAToken(t *testing.T) {
	rec := httptest.NewRecorder()
	InitFile().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v3/file?path=/etc/passwd", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d for a request with no token; want 401", rec.Code)
	}
}

// A token that is not a token is refused too — the signature check itself.
func TestV3FileRejectsAGarbageToken(t *testing.T) {
	rec := httptest.NewRecorder()
	InitFile().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v3/file?token=not-a-jwt&path=/tmp/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d for a malformed token; want 401", rec.Code)
	}
}

// THE PRECONDITION THE NEW AUTHORISATION CHECK RESTS ON. Gating on
// PermFileRead only refuses a guest because PermFileRead is not in guestPerms —
// it was removed precisely because the ungated file reads let a guest read
// anything outside the blocked-prefix list. If it were ever added back, this
// route would silently stop refusing guests again, and nothing else in this
// package would notice.
func TestGuestHasNoFileReadWhileRealPrincipalsDo(t *testing.T) {
	if rbac.Allows(rbac.RoleGuest, rbac.PermFileRead) {
		t.Error("guest holds PermFileRead again; /v3/file's authorisation check is " +
			"now a no-op against exactly the principal it exists to refuse")
	}
	if rbac.Allows(rbac.Normalize(""), rbac.PermFileRead) {
		t.Error("an empty role resolves to something holding PermFileRead; " +
			"rbac.Normalize must fail closed to guest")
	}
	for _, r := range []rbac.Role{rbac.RoleUser, rbac.RoleAdmin} {
		if !rbac.Allows(r, rbac.PermFileRead) {
			t.Errorf("%s cannot read files; the gate would break legitimate downloads", r)
		}
	}
}

// Reaching the revocation and authorisation branches needs a real signed token,
// which means standing up a JWKS server and writing the user-service address
// file — and the public key is cached process-wide, so doing it here would
// leak into the other tests in this package. Both branches are therefore pinned
// at the source. Anti-vacuity first: an unreadable file fails rather than
// passing silently.
func TestV3FileStillChecksRevocationAndPermission(t *testing.T) {
	src, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatalf("cannot read v2.go: %v", err)
	}
	text := string(src)

	start := strings.Index(text, "func InitFile()")
	if start < 0 {
		t.Fatal("InitFile is gone from v2.go — re-point this test")
	}
	end := strings.Index(text[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("could not delimit InitFile — re-point this test")
	}
	body := text[start : start+end]

	for _, want := range []string{
		"jwt.RevokedForClaims(claims)",
		"rbac.Allows(rbac.Normalize(claims.Role), rbac.PermFileRead)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("InitFile no longer contains %q — /v3/file is back to accepting "+
				"any validly-signed token, revoked or unprivileged", want)
		}
	}
}
