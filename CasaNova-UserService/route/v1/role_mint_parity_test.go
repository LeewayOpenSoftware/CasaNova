package v1

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model/system_model"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

// Test cases covering:
// 1. Legacy unmigrated / empty / whitespace rows (must resolve to "admin")
// 2. Legacy unrecognized admin variants / typos (must promote to "admin" rather than locking owner out)
// 3. Standard valid roles ("admin", "user", "guest") preserving their canonical representation
var parityTestCases = []struct {
	name       string
	storedRole string
	wantRole   string
}{
	// Legacy empty / whitespace variations
	{name: "empty string", storedRole: "", wantRole: "admin"},
	{name: "spaces only", storedRole: "   ", wantRole: "admin"},
	{name: "tabs and whitespace", storedRole: "\t\r\n ", wantRole: "admin"},

	// Legacy unrecognized admin roles / typos / mis-spellings
	{name: "legacy administrator lowercase", storedRole: "administrator", wantRole: "admin"},
	{name: "legacy administrator uppercase", storedRole: "ADMINISTRATOR", wantRole: "admin"},
	{name: "legacy administrator mixedcase", storedRole: "Administrator", wantRole: "admin"},
	{name: "legacy administrator with whitespace", storedRole: "  administrator  ", wantRole: "admin"},
	{name: "legacy owner lowercase", storedRole: "owner", wantRole: "admin"},
	{name: "legacy owner uppercase", storedRole: "OWNER", wantRole: "admin"},
	{name: "legacy owner mixedcase", storedRole: "Owner", wantRole: "admin"},
	{name: "legacy root lowercase", storedRole: "root", wantRole: "admin"},
	{name: "legacy root uppercase", storedRole: "ROOT", wantRole: "admin"},
	{name: "legacy root mixedcase", storedRole: "Root", wantRole: "admin"},
	{name: "legacy superuser lowercase", storedRole: "superuser", wantRole: "admin"},
	{name: "legacy superuser uppercase", storedRole: "SUPERUSER", wantRole: "admin"},
	{name: "typo adminn", storedRole: "adminn", wantRole: "admin"},
	{name: "legacy operator", storedRole: "legacy_operator", wantRole: "admin"},
	{name: "arbitrary legacy unrecognized", storedRole: "sysadmin", wantRole: "admin"},

	// Standard roles
	{name: "standard admin lowercase", storedRole: "admin", wantRole: "admin"},
	{name: "standard admin uppercase", storedRole: "ADMIN", wantRole: "admin"},
	{name: "standard admin mixedcase", storedRole: "Admin", wantRole: "admin"},
	{name: "standard admin whitespace", storedRole: "  admin  ", wantRole: "admin"},
	{name: "standard user lowercase", storedRole: "user", wantRole: "user"},
	{name: "standard user uppercase", storedRole: "USER", wantRole: "user"},
	{name: "standard user mixedcase", storedRole: "User", wantRole: "user"},
	{name: "standard user whitespace", storedRole: "  user  ", wantRole: "user"},
	{name: "standard guest lowercase", storedRole: "guest", wantRole: "guest"},
	{name: "standard guest uppercase", storedRole: "GUEST", wantRole: "guest"},
	{name: "standard guest mixedcase", storedRole: "Guest", wantRole: "guest"},
	{name: "standard guest whitespace", storedRole: "  guest  ", wantRole: "guest"},
}

// 1. Direct Unit Test of resolveEffectiveRole
func TestRoleMintParity_ResolveEffectiveRole(t *testing.T) {
	for _, tc := range parityTestCases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEffectiveRole(tc.storedRole)
			if got != tc.wantRole {
				t.Errorf("resolveEffectiveRole(%q) = %q, want %q", tc.storedRole, got, tc.wantRole)
			}
		})
	}
}

// Mock service infrastructure for HTTP handler tests
type mockParityRepo struct {
	userSvc service.UserService
}

func (m *mockParityRepo) Gateway() external.ManagementService          { return nil }
func (m *mockParityRepo) User() service.UserService                    { return m.userSvc }
func (m *mockParityRepo) MessageBus() *message_bus.ClientWithResponses { return nil }
func (m *mockParityRepo) Event() service.EventService                  { return nil }

type mockParityUserService struct {
	privKey   *ecdsa.PrivateKey
	pubKey    *ecdsa.PublicKey
	users     map[string]model2.UserDBModel
	usersByID map[string]model2.UserDBModel
}

func (m *mockParityUserService) GetKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	return m.privKey, m.pubKey
}

func (m *mockParityUserService) GetUserAllInfoByName(userName string) model2.UserDBModel {
	return m.users[userName]
}

func (m *mockParityUserService) GetUserInfoById(id string) model2.UserDBModel {
	return m.usersByID[id]
}

func (m *mockParityUserService) GetUserInfoByUserName(userName string) model2.UserDBModel {
	return m.users[userName]
}

func (m *mockParityUserService) UpLoadFile(file multipart.File, name string) error       { return nil }
func (m *mockParityUserService) CreateUser(u model2.UserDBModel) model2.UserDBModel       { return u }
func (m *mockParityUserService) GetUserCount() int64                                      { return int64(len(m.users)) }
func (m *mockParityUserService) UpdateUser(u model2.UserDBModel)                          {}
func (m *mockParityUserService) UpdateUserPassword(u model2.UserDBModel)                  {}
func (m *mockParityUserService) GetUserAllInfoById(id string) model2.UserDBModel          { return m.usersByID[id] }
func (m *mockParityUserService) DeleteUserById(id string)                                 {}
func (m *mockParityUserService) GetAllUserName() []model2.UserDBModel                     { return nil }

// 2. End-to-End Handler Comparator Test
// This test exercises PostUserLogin and PostUserRefreshToken over the full set of
// legacy and standard roles, decoding the minted JWTs and asserting that both
// endpoints produce the exact same role claim.
func TestRoleMintParity_LoginAndRefreshComparator(t *testing.T) {
	// Redirect 2FA config so no 2FA prompt interferes with login
	origTOTPCfg := userTOTPConfigFile
	userTOTPConfigFile = filepath.Join(t.TempDir(), "nonexistent-2fa.json")
	defer func() { userTOTPConfigFile = origTOTPCfg }()

	// Redirect tokenrevocation shared store to an isolated temp location
	revDir := t.TempDir()
	revPath := filepath.Join(revDir, "token-revocations.json")
	tokenrevocation.SetPath(revPath)
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })
	if err := tokenrevocation.Ensure(); err != nil {
		t.Fatalf("failed to initialize tokenrevocation: %v", err)
	}

	// Generate persistent test keypair for JWT signing
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ecdsa key: %v", err)
	}
	pubKey := &privKey.PublicKey

	origService := service.MyService
	defer func() { service.MyService = origService }()

	testPassword := "ParityTestPass123!"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	e := echo.New()

	for i, tc := range parityTestCases {
		t.Run(tc.name, func(t *testing.T) {
			userID := 2000 + i
			username := fmt.Sprintf("user_parity_%d", i)
			// Assign a distinct IP address per test case to avoid rate limiting
			remoteIP := fmt.Sprintf("10.200.%d.%d:4321", (i/250)+1, (i%250)+1)

			userModel := model2.UserDBModel{
				Id:       userID,
				Username: username,
				Password: string(hashedPassword),
				Role:     tc.storedRole,
			}

			mockUser := &mockParityUserService{
				privKey:   privKey,
				pubKey:    pubKey,
				users:     map[string]model2.UserDBModel{username: userModel},
				usersByID: map[string]model2.UserDBModel{fmt.Sprintf("%d", userID): userModel},
			}
			service.MyService = &mockParityRepo{userSvc: mockUser}

			// Step 1: POST /v1/users/login
			loginPayload, _ := json.Marshal(map[string]string{
				"username": username,
				"password": testPassword,
			})
			loginReq := httptest.NewRequest(http.MethodPost, "/v1/users/login", bytes.NewReader(loginPayload))
			loginReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			loginReq.RemoteAddr = remoteIP
			loginRec := httptest.NewRecorder()
			loginCtx := e.NewContext(loginReq, loginRec)

			if err := PostUserLogin(loginCtx); err != nil {
				t.Fatalf("PostUserLogin error: %v", err)
			}
			if loginRec.Code != http.StatusOK {
				t.Fatalf("PostUserLogin status %d: %s", loginRec.Code, loginRec.Body.String())
			}

			var loginResp struct {
				Success int    `json:"success"`
				Message string `json:"message"`
				Data    struct {
					Token system_model.VerifyInformation `json:"token"`
				} `json:"data"`
			}
			if err := json.Unmarshal(loginRec.Body.Bytes(), &loginResp); err != nil {
				t.Fatalf("failed to decode login response: %v", err)
			}

			tokenInfo := loginResp.Data.Token
			if tokenInfo.AccessToken == "" || tokenInfo.RefreshToken == "" {
				t.Fatalf("login did not issue both tokens: body=%s", loginRec.Body.String())
			}

			// Extract claims from login tokens
			loginAccessClaims, err := jwt.ParseToken(tokenInfo.AccessToken, func() (*ecdsa.PublicKey, error) {
				return pubKey, nil
			})
			if err != nil {
				t.Fatalf("failed to parse login access token: %v", err)
			}
			loginRefreshClaims, err := jwt.ParseToken(tokenInfo.RefreshToken, func() (*ecdsa.PublicKey, error) {
				return pubKey, nil
			})
			if err != nil {
				t.Fatalf("failed to parse login refresh token: %v", err)
			}

			// Step 2: POST /v1/users/refresh using the refresh token from login
			refreshPayload, _ := json.Marshal(map[string]string{
				"refresh_token": tokenInfo.RefreshToken,
			})
			refreshReq := httptest.NewRequest(http.MethodPost, "/v1/users/refresh", bytes.NewReader(refreshPayload))
			refreshReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			refreshReq.RemoteAddr = remoteIP
			refreshRec := httptest.NewRecorder()
			refreshCtx := e.NewContext(refreshReq, refreshRec)

			if err := PostUserRefreshToken(refreshCtx); err != nil {
				t.Fatalf("PostUserRefreshToken error: %v", err)
			}
			if refreshRec.Code != http.StatusOK {
				t.Fatalf("PostUserRefreshToken status %d: %s", refreshRec.Code, refreshRec.Body.String())
			}

			var refreshResp struct {
				Success int                         `json:"success"`
				Message string                      `json:"message"`
				Data    system_model.VerifyInformation `json:"data"`
			}
			if err := json.Unmarshal(refreshRec.Body.Bytes(), &refreshResp); err != nil {
				t.Fatalf("failed to decode refresh response: %v", err)
			}

			// Extract claims from refreshed tokens
			refreshedAccessClaims, err := jwt.ParseToken(refreshResp.Data.AccessToken, func() (*ecdsa.PublicKey, error) {
				return pubKey, nil
			})
			if err != nil {
				t.Fatalf("failed to parse refreshed access token: %v", err)
			}
			refreshedRefreshClaims, err := jwt.ParseToken(refreshResp.Data.RefreshToken, func() (*ecdsa.PublicKey, error) {
				return pubKey, nil
			})
			if err != nil {
				t.Fatalf("failed to parse refreshed refresh token: %v", err)
			}

			// Step 3: Assert role minting parity and correctness
			// Invariant 1: Access token minted at login must match access token minted at refresh
			if loginAccessClaims.Role != refreshedAccessClaims.Role {
				t.Fatalf("PARITY DIVERGENCE (Access Token): login role=%q, refresh role=%q for storedRole=%q",
					loginAccessClaims.Role, refreshedAccessClaims.Role, tc.storedRole)
			}

			// Invariant 2: Refresh token minted at login must match refresh token minted at refresh
			if loginRefreshClaims.Role != refreshedRefreshClaims.Role {
				t.Fatalf("PARITY DIVERGENCE (Refresh Token): login role=%q, refresh role=%q for storedRole=%q",
					loginRefreshClaims.Role, refreshedRefreshClaims.Role, tc.storedRole)
			}

			// Invariant 3: Minted roles must match the expected role
			if loginAccessClaims.Role != tc.wantRole {
				t.Errorf("Login access token role = %q, want %q for storedRole %q",
					loginAccessClaims.Role, tc.wantRole, tc.storedRole)
			}
			if refreshedAccessClaims.Role != tc.wantRole {
				t.Errorf("Refresh access token role = %q, want %q for storedRole %q",
					refreshedAccessClaims.Role, tc.wantRole, tc.storedRole)
			}
		})
	}
}

// 3. Static AST Invariant Test
// Guarantees that both PostUserLogin and PostUserRefreshToken route role resolution
// through resolveEffectiveRole, and neither function calls rbac.Normalize directly.
func TestRoleMintParity_CallSitesInvariant(t *testing.T) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "user.go", nil, 0)
	if err != nil {
		t.Fatalf("failed to parse user.go: %v", err)
	}

	handlersChecked := map[string]bool{
		"PostUserLogin":        false,
		"PostUserRefreshToken": false,
	}

	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		fnName := fn.Name.Name
		if _, monitored := handlersChecked[fnName]; !monitored {
			continue
		}

		hasHelperCall := false
		hasDirectNormalize := false

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Check for call to resolveEffectiveRole
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "resolveEffectiveRole" {
				hasHelperCall = true
			}

			// Check for raw call to rbac.Normalize
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "rbac" && sel.Sel.Name == "Normalize" {
					hasDirectNormalize = true
				}
			}

			return true
		})

		if !hasHelperCall {
			t.Errorf("handler %s does NOT invoke shared helper resolveEffectiveRole", fnName)
		}
		if hasDirectNormalize {
			t.Errorf("handler %s invokes rbac.Normalize directly instead of resolveEffectiveRole", fnName)
		}

		handlersChecked[fnName] = true
	}

	for name, checked := range handlersChecked {
		if !checked {
			t.Errorf("monitored handler %s was not found in user.go AST", name)
		}
	}
}
