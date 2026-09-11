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
// Pinned as a CLASS, not as one handler. InitFile was fixed first and InitDir
// still carried the identical pair of holes afterwards, which is the whole
// argument for iterating here: the defect belongs to the shape "standalone
// http.Handler in main.go that takes ?token= and validates it itself", and
// there are two of them in this file.
//
// InitDir is presently DEAD CODE — nothing in the repo calls it, verified
// against a positive control that InitFile is mounted at main.go:246. That is
// exactly why it is worth hardening rather than shrugging at: the day someone
// wires it up, they will wire up whatever it says today, and a reviewer reading
// a handler that already checks revocation and permission has nothing to
// notice. Leaving the holes in and trusting the next person to spot them is
// the bet that produced this card.
func TestStandaloneDownloadHandlersCheckRevocationAndPermission(t *testing.T) {
	src, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatalf("cannot read v2.go: %v", err)
	}
	text := string(src)

	for _, handler := range []string{"func InitFile()", "func InitDir()"} {
		start := strings.Index(text, handler)
		if start < 0 {
			t.Fatalf("%s is gone from v2.go — re-point this test", handler)
		}
		// The last function in the file has no following "func " to stop at,
		// which is InitDir's situation today — take the remainder then.
		end := strings.Index(text[start:], "\nfunc ")
		if end < 0 {
			end = len(text) - start
		}
		body := text[start : start+end]

		// Anti-vacuity: a delimiting slip that produced an empty or tiny body
		// would satisfy nothing below and still pass if the loop never ran.
		if len(body) < 400 {
			t.Fatalf("%s body came out %d bytes — the delimiter is wrong, not the code",
				handler, len(body))
		}

		for _, want := range []string{
			"jwt.RevokedForClaims(claims)",
			"rbac.Allows(rbac.Normalize(claims.Role), rbac.PermFileRead)",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s no longer contains %q — it is back to accepting any "+
					"validly-signed token, revoked or unprivileged", handler, want)
			}
		}
	}
}

// The token checks that do not need a signing key, run against InitDir the
// same way they already run against InitFile.
func TestV2DirRequiresAToken(t *testing.T) {
	rec := httptest.NewRecorder()
	InitDir().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/dir?files=/etc/passwd", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d for a request with no token; want 401", rec.Code)
	}
}

func TestV2DirRejectsAGarbageToken(t *testing.T) {
	rec := httptest.NewRecorder()
	InitDir().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v2/dir?token=not-a-jwt&files=/tmp/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d for a malformed token; want 401", rec.Code)
	}
}
