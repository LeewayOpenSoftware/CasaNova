package middleware_test

// Tree sweep: nothing in CasaNova-Common may derive trust from the peer being
// on the loopback interface.
//
// Why this test exists. The gateway reverse-proxies every LAN request — and,
// over the Cloudflare tunnel, every request from the public internet — to the
// backend services over 127.0.0.1. A handler that asks "is my peer loopback?"
// is therefore asking "does this request exist?", and the answer is yes for the
// entire internet. This module shipped two helpers built on that mistake:
//
//	middleware.RequirePermission / RequireRole / RequireAdmin — each opened with
//	  `if loopback.IsLoopback(c) { return next(c) }`, so every RBAC check they
//	  were meant to enforce passed for any remote caller.
//	jwt.JWT() — its echojwt Skipper was a bare `return loopback.IsLoopback(c)`,
//	  skipping authentication outright, even for a caller presenting a token.
//
// Both were removed: no module referenced them, and the linker had already
// dropped all four symbols from every shipped binary. The danger was that they
// were exported, plausible-looking, and one import away from being adopted.
// This sweep is what stops an equivalent helper from growing back.
//
// The rule enforced. Outside package loopback itself, no non-test file in this
// module may reference loopback.IsLoopback unless the reference sits in a
// conjunction that also requires the absence of a presented credential — the
// hardened form the thirteen production JWT middlewares use:
//
//	loopback.IsLoopback(c) && c.Request().Header.Get(echo.HeaderAuthorization) == ""
//
// which skips auth only when there is no credential to check in the first
// place. Peer services prove they are peers with utils/internalauth (a
// per-boot, root-only shared secret), never with their address.
//
// The rule is deliberately fail-closed: it flags every unhardened reference
// regardless of polarity, including a restrictive `if !IsLoopback(c) { deny }`.
// A shared library has no "internal-only" callers, so a legitimate new use must
// either be written in the hardened form or arrive together with an argued
// amendment to this test. There are currently zero references to exempt.
//
// A second rule catches the obvious dodge: a function that parses
// Request().RemoteAddr itself and calls IsLoopback() on the result is
// reimplementing the same trust decision without importing the package. This
// does not flag net.IP.IsLoopback() used for origin classification
// (cors_origin.go) — that inspects an Origin header, not the peer.
//
// Scope is this module, and the scope is a real limit, not a formality: this
// test cannot see the other nine modules, and at least one unhardened skipper
// survives out there (CasaNova-Gateway/route/management_route.go). Extending
// the sweep across modules needs a home that can import all of them; until
// then, each module's own gate tests own their side.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// loopbackImportSuffix identifies the primitive package by import path, so a
// renamed import (`lb "…/utils/loopback"`) is still resolved.
const loopbackImportSuffix = "CasaNova-Common/utils/loopback"

// loopbackPkgDir is the primitive's own directory, relative to the module
// root. It defines IsLoopback and is exempt from the rule about calling it.
const loopbackPkgDir = "utils/loopback"

// credentialWords are the tokens that mark the other side of a conjunction as
// "…and no credential was presented", which is what makes a loopback check
// safe to act on.
var credentialWords = []string{
	"authorization", "token", "bearer", "credential",
	"internalauth", "apikey", "api_key", "secret",
}

type finding struct {
	where string // path:line, relative to the module root
	fn    string // enclosing function, or "<file scope>"
	why   string
}

func (f finding) String() string {
	return fmt.Sprintf("%s: in %s: %s", f.where, f.fn, f.why)
}

func TestNoLoopbackDerivedTrustInCommon(t *testing.T) {
	root := moduleRoot(t)

	// Positive controls. The detector must fire on the shape it hunts for and
	// stay quiet on the hardened form. Without these, a matcher that silently
	// stopped matching would report the tree clean forever — which is exactly
	// how the two removed helpers survived so long.
	bare := scanSource(t, "control_bare.go", `package p

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/loopback"
	"github.com/labstack/echo/v4"
)

func RequireAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if loopback.IsLoopback(c) {
				return next(c)
			}
			return echo.ErrForbidden
		}
	}
}
`)
	require.Len(t, bare, 1,
		"detector failed its positive control: it did not flag a bare loopback bypass")
	require.Contains(t, bare[0].fn, "RequireAdmin",
		"detector flagged the control but misattributed the enclosing function")

	hardened := scanSource(t, "control_hardened.go", `package p

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/loopback"
	"github.com/labstack/echo/v4"
)

func Skip(c echo.Context) bool {
	return loopback.IsLoopback(c) && c.Request().Header.Get(echo.HeaderAuthorization) == ""
}
`)
	require.Empty(t, hardened,
		"detector failed its negative control: it flagged the hardened conjunction, "+
			"which would push authors to disable this test rather than harden their code")

	// A conjunction is not automatically a hardening. Only an operand that
	// actually tests for a presented credential earns the exemption; any other
	// `&&` must still be flagged. Without this control, a detector that
	// treated every conjunction as hardened would pass the two controls above
	// and wave through `if enabled && loopback.IsLoopback(c) { grant }`.
	weakConjunction := scanSource(t, "control_weak_conjunction.go", `package p

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/loopback"
	"github.com/labstack/echo/v4"
)

func Skip(c echo.Context, enabled bool) bool {
	return enabled && loopback.IsLoopback(c)
}
`)
	require.Len(t, weakConjunction, 1,
		"detector failed its positive control: it accepted a conjunction whose other "+
			"operand does not test for a presented credential")

	aliased := scanSource(t, "control_aliased.go", `package p

import (
	lb "github.com/LeewayOpenSoftware/CasaNova-Common/utils/loopback"
	"github.com/labstack/echo/v4"
)

func Skip(c echo.Context) bool { return lb.IsLoopback(c) }
`)
	require.Len(t, aliased, 1,
		"detector failed its positive control: a renamed import of the loopback package evaded it")

	reimpl := scanSource(t, "control_reimpl.go", `package p

import (
	"net"

	"github.com/labstack/echo/v4"
)

func Skip(c echo.Context) bool {
	host, _, _ := net.SplitHostPort(c.Request().RemoteAddr)
	return net.ParseIP(host).IsLoopback()
}
`)
	require.Len(t, reimpl, 1,
		"detector failed its positive control: an inline RemoteAddr reimplementation evaded it")

	// The sweep itself.
	var findings []finding
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == "testdata" || base == ".git" || strings.HasPrefix(base, "_") {
				return fs.SkipDir
			}
			if rel == loopbackPkgDir {
				return fs.SkipDir // the primitive's own definition
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		scanned++
		findings = append(findings, scanAST(fset, file, rel)...)
		return nil
	})
	require.NoError(t, err)

	// Non-vacuity: a sweep that silently walked an empty tree must not pass.
	require.Greater(t, scanned, 20,
		"sweep parsed only %d non-test files under %s; it is not covering the module", scanned, root)

	if len(findings) > 0 {
		lines := make([]string, 0, len(findings))
		for _, f := range findings {
			lines = append(lines, f.String())
		}
		t.Fatalf("CasaNova-Common must not derive trust from the peer address.\n"+
			"The gateway proxies LAN and internet traffic over 127.0.0.1, so a loopback\n"+
			"peer is not a trusted peer. Skip auth only when no credential was presented\n"+
			"(`loopback.IsLoopback(c) && Authorization == \"\"`), and authenticate real peer\n"+
			"services with utils/internalauth.\n\n%d violation(s):\n  %s",
			len(findings), strings.Join(lines, "\n  "))
	}
}

// scanSource parses an in-memory fixture and runs the same detector the sweep
// runs, so the controls exercise production code rather than a copy of it.
func scanSource(t *testing.T, name, src string) []finding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	require.NoError(t, err, "control fixture %s does not parse", name)
	return scanAST(fset, file, name)
}

func scanAST(fset *token.FileSet, file *ast.File, display string) []finding {
	var findings []finding

	if alias := loopbackAlias(file); alias != "" {
		// Every reference to <alias>.IsLoopback, called or taken as a value
		// (`Skipper: loopback.IsLoopback` is a bare bypass too).
		var refs []*ast.SelectorExpr
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && isLoopbackSelector(sel, alias) {
				refs = append(refs, sel)
			}
			return true
		})

		hardened := hardenedRefs(file, alias)
		for _, sel := range refs {
			if hardened[sel.Pos()] {
				continue
			}
			findings = append(findings, finding{
				where: fmt.Sprintf("%s:%d", display, fset.Position(sel.Pos()).Line),
				fn:    enclosingFunc(file, sel.Pos()),
				why: "loopback.IsLoopback decides trust without also requiring that no " +
					"credential was presented",
			})
		}
	}

	// Inline reimplementation: reads Request().RemoteAddr and calls
	// IsLoopback() on what it parsed out.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var remoteAddr bool
		var isLoopbackCall *ast.SelectorExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "RemoteAddr":
				remoteAddr = true
			case "IsLoopback":
				if isLoopbackCall == nil {
					isLoopbackCall = sel
				}
			}
			return true
		})
		if remoteAddr && isLoopbackCall != nil {
			findings = append(findings, finding{
				where: fmt.Sprintf("%s:%d", display, fset.Position(isLoopbackCall.Pos()).Line),
				fn:    funcLabel(fn),
				why: "reimplements the peer-address trust check inline from " +
					"Request().RemoteAddr instead of using utils/internalauth",
			})
		}
	}

	return findings
}

// hardenedRefs returns the positions of loopback references that sit inside an
// `&&` whose other operand mentions a credential.
func hardenedRefs(file *ast.File, alias string) map[token.Pos]bool {
	hardened := map[token.Pos]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || be.Op != token.LAND {
			return true
		}
		for _, pair := range [2][2]ast.Expr{{be.X, be.Y}, {be.Y, be.X}} {
			side, other := pair[0], pair[1]
			if !mentionsCredential(other) {
				continue
			}
			ast.Inspect(side, func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok && isLoopbackSelector(sel, alias) {
					hardened[sel.Pos()] = true
				}
				return true
			})
		}
		return true
	})
	return hardened
}

func isLoopbackSelector(sel *ast.SelectorExpr, alias string) bool {
	if sel.Sel.Name != "IsLoopback" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == alias
}

// mentionsCredential reports whether an expression refers to a presented
// credential — the thing whose absence makes a loopback skip defensible.
func mentionsCredential(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.Ident:
			found = containsCredentialWord(v.Name)
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				found = containsCredentialWord(v.Value)
			}
		}
		return !found
	})
	return found
}

func containsCredentialWord(s string) bool {
	s = strings.ToLower(s)
	for _, w := range credentialWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// loopbackAlias returns the local name the file uses for the loopback package,
// or "" if it does not import it.
func loopbackAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasSuffix(path, loopbackImportSuffix) {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				// A blank import cannot call anything; a dot import is not
				// used anywhere in this module and would need its own rule.
				return ""
			}
			return imp.Name.Name
		}
		return "loopback"
	}
	return ""
}

func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if pos >= fn.Pos() && pos <= fn.End() {
			return funcLabel(fn)
		}
	}
	return "<file scope>"
}

func funcLabel(fn *ast.FuncDecl) string {
	if fn.Name.IsExported() {
		return "exported func " + fn.Name.Name
	}
	return "func " + fn.Name.Name
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod, so the sweep covers the whole module rather than one package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "no go.mod found above the test's working directory")
		dir = parent
	}
}
