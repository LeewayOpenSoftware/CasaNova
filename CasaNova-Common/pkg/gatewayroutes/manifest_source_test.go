package gatewayroutes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This test closes the gap that makes a route-ownership manifest worth having
// at all.
//
// Only the services that import this package are forced to agree with it. The
// remaining services still build their gateway path list by hand, so the
// manifest could describe a world that no longer exists — and a manifest that
// describes fiction is worse than none, because the next reviewer trusts it.
//
// So instead of trusting, we read: every sibling service's main.go is parsed
// and the string prefixes it registers at the gateway are extracted. Any
// prefix a service actually registers must be declared for that service in the
// manifest. This is what catches "a new service quietly claims a prefix that
// already belongs to someone else" without needing that service to be migrated
// first.
//
// What it cannot see: prefixes computed at run time (a service's generated v2
// base path comes from its OpenAPI document). Those are declared in the
// manifest's Derived lists and are deliberately skipped here rather than
// guessed at.

// serviceDirs maps an owner to its source directory, relative to the repo root
// (the parent of CasaNova-Common). Owners without a directory here are not
// checked against source.
var serviceDirs = map[Owner]string{
	OwnerCasaNova:      "CasaNova",
	OwnerAppManagement: "CasaNova-AppManagement",
	OwnerLocalStorage:  "CasaNova-LocalStorage",
	OwnerUserService:   "CasaNova-UserService",
	OwnerMessageBus:    "CasaNova-MessageBus",
	OwnerJobManagement: "CasaNova-JobManagement",
	OwnerDriver:        "CasaNova-Driver",
	OwnerVirt:          "CasaNova-Virt",
}

// routeListNames are the identifiers each service assigns its gateway path
// list to before handing it to CreateRoute / the registration watchdog.
var routeListNames = map[string]bool{"routers": true, "apiPaths": true}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file location")
	}
	// .../<root>/CasaNova-Common/pkg/gatewayroutes/manifest_source_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
}

func TestManifestMatchesServiceSources(t *testing.T) {
	root := repoRoot(t)

	// CasaNova-Common is published as its own module. When it is consumed
	// standalone the sibling services are simply not on disk and there is
	// nothing to cross-check; inside the CasaNova OS tree they always are.
	if _, err := os.Stat(filepath.Join(root, "CasaNova")); err != nil {
		t.Skipf("sibling services not present at %s; nothing to cross-check", root)
	}

	checked := 0
	for _, owner := range Owners() {
		dir, ok := serviceDirs[owner]
		if !ok {
			continue
		}
		mainFile := filepath.Join(root, dir, "main.go")
		if _, err := os.Stat(mainFile); err != nil {
			t.Errorf("owner %q: %s is missing (renamed service? update serviceDirs)", owner, mainFile)
			continue
		}

		registered, err := registeredPrefixes(root, dir)
		if err != nil {
			t.Errorf("owner %q: %v", owner, err)
			continue
		}

		declared := map[string]bool{}
		for _, p := range AllPrefixes(owner) {
			declared[p] = true
		}
		for _, p := range registered {
			checked++
			if !declared[p] {
				t.Errorf("%s/main.go registers gateway prefix %q, which is not declared for owner %q "+
					"in the route manifest. Either declare it (and resolve any conflict Validate reports) "+
					"or stop registering it — an undeclared prefix can silently steal another service's "+
					"subtree or flap against a duplicate claim.", dir, p, owner)
			}
		}
	}

	// A scan that resolves nothing would pass forever without proving
	// anything. Fail loudly instead of pretending to be a check.
	if checked == 0 {
		t.Fatal("no gateway prefixes could be extracted from any service main.go; the source scan is not working")
	}
}

// wiredOwners are the services that build their gateway list from this
// package. Being wired is the whole point — a manifest a service does not read
// is documentation, and documentation drifts. This test checks that each of
// them calls Prefixes with ITS OWN owner constant: passing the wrong one
// compiles cleanly and would make a service publish another service's surface.
var wiredOwners = map[Owner]string{
	OwnerCasaNova:      "OwnerCasaNova",
	OwnerAppManagement: "OwnerAppManagement",
}

func TestWiredServicesRegisterTheirOwnClaim(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "CasaNova")); err != nil {
		t.Skipf("sibling services not present at %s; nothing to cross-check", root)
	}

	for owner, constName := range wiredOwners {
		dir := serviceDirs[owner]
		mainFile := filepath.Join(root, dir, "main.go")
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, mainFile, nil, 0)
		if err != nil {
			t.Errorf("owner %q: %v", owner, err)
			continue
		}

		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Prefixes" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "gatewayroutes" || len(call.Args) != 1 {
				return true
			}
			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if ok && arg.Sel.Name == constName {
				found = true
			}
			return true
		})

		if !found {
			t.Errorf("%s/main.go does not call gatewayroutes.Prefixes(gatewayroutes.%s); "+
				"the manifest is then just a comment for this service and will drift from what it registers",
				dir, constName)
		}
	}
}

// registeredPrefixes extracts the literal gateway prefixes that <dir>/main.go
// hands to the gateway. Elements it cannot resolve to a compile-time string
// (a call, or a constant computed from an OpenAPI document at init) are
// skipped.
func registeredPrefixes(root, dir string) ([]string, error) {
	fset := token.NewFileSet()
	mainFile := filepath.Join(root, dir, "main.go")
	file, err := parser.ParseFile(fset, mainFile, nil, 0)
	if err != nil {
		return nil, err
	}

	routeConsts := routePackageStrings(root, dir)

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || !routeListNames[ident.Name] {
			return true
		}
		for _, expr := range routeListExprs(assign.Rhs[0]) {
			if s, ok := constString(expr, routeConsts); ok {
				out = append(out, s)
			}
		}
		return true
	})
	return out, nil
}

// routeListExprs returns the element expressions of a gateway path list,
// handling both `[]string{...}` and `append(gatewayroutes.Prefixes(x), ...)`.
// The manifest-sourced part of an append needs no checking — it IS the
// manifest.
func routeListExprs(rhs ast.Expr) []ast.Expr {
	switch e := rhs.(type) {
	case *ast.CompositeLit:
		return e.Elts
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok && fn.Name == "append" && len(e.Args) > 1 {
			return e.Args[1:]
		}
	}
	return nil
}

// routePackageStrings collects `X = "literal"` (and simple concatenations of
// those) from <dir>/route/*.go so that `route.V1APIPath` in main.go resolves.
// This is the case that matters: casanova-driver published "/v1/driver" this
// way, colliding with the main service's claim, and no literal ever appeared
// in its main.go.
func routePackageStrings(root, dir string) map[string]string {
	out := map[string]string{}
	routeDir := filepath.Join(root, dir, "route")
	entries, err := os.ReadDir(routeDir)
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	// Two passes so a constant defined after the one that references it still
	// resolves; the declarations are unordered across files.
	for pass := 0; pass < 2; pass++ {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(routeDir, entry.Name()), nil, 0)
			if err != nil {
				continue
			}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != len(vs.Values) {
						continue
					}
					for i, name := range vs.Names {
						if s, ok := constString(vs.Values[i], out); ok {
							out[name.Name] = s
						}
					}
				}
			}
		}
	}
	return out
}

// constString evaluates an expression to a compile-time string, resolving
// identifiers (and `route.X` selectors) through the supplied table. It returns
// false for anything it cannot pin down, which is the safe answer: an
// unresolved element is reported by nobody rather than asserted wrongly.
func constString(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.Ident:
		s, ok := consts[e.Name]
		return s, ok
	case *ast.SelectorExpr:
		s, ok := consts[e.Sel.Name]
		return s, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, okL := constString(e.X, consts)
		r, okR := constString(e.Y, consts)
		if !okL || !okR {
			return "", false
		}
		return l + r, true
	}
	return "", false
}
