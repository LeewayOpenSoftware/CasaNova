package v2

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every HTTP handler in this package must carry an authorization gate.
//
// Ported from 27.04 (docs/design/per-app-grants.md §7, commit 50da9c3): Jim's
// wave-5 census found nineteen of this package's forty-four methods with no
// gate at all — not the wrong bit, none — identical in both release lines.
// The router's JWT middleware still refused anonymous callers, so they were
// AUTHENTICATED but UNAUTHORIZED, and nothing failed: no build error, no
// test, no log line. A handler that forgets its gate looks exactly like one
// that does not need it.
//
// So the property is checked on the SOURCE rather than trusted to review. This
// reads the package's own AST — not a copy of the handler list, which would rot
// the moment someone adds a route — and fails naming any handler that takes an
// echo.Context and reaches neither requirePerm nor requirePermInternal.
//
// The exceptions are listed here with a reason each, and an unused exception is
// itself a failure: a name that no longer matches a handler is a stale excuse
// nobody will notice.
var gateExceptions = map[string]string{
	// Not an HTTP handler at all. It hangs off *AppManagement but takes a
	// *service.ComposeApp, is absent from the codegen ServerInterface, is
	// registered on no route, and is called only from two handlers that are
	// themselves gated. It appears in handler censuses that match on the
	// receiver instead of the signature; it is not reachable from the network.
	"IsNewComposeUncontrolled": "not a route: takes *service.ComposeApp, not echo.Context",
}

func TestEveryHandlerCarriesAGate(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	type handler struct {
		name       string
		pos        string
		takesCtx   bool
		gated      bool
		exceptions bool
	}
	var handlers []handler

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil {
				continue
			}
			// Receiver must be *AppManagement.
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != "AppManagement" {
				continue
			}

			h := handler{name: fd.Name.Name, pos: fset.Position(fd.Pos()).String()}
			// Does it take an echo.Context? That is what makes it reachable.
			for _, p := range fd.Type.Params.List {
				sel, ok := p.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if ok && pkg.Name == "echo" && sel.Sel.Name == "Context" {
					h.takesCtx = true
				}
			}
			// Does its body reach a gate?
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					if id.Name == "requirePerm" || id.Name == "requirePermInternal" {
						h.gated = true
					}
				}
				return true
			})
			handlers = append(handlers, h)
		}
	}

	// Guard the guard. A parser that matched nothing, or a package that moved,
	// would make every assertion below pass by finding no handlers at all.
	if len(handlers) < 40 {
		t.Fatalf("found only %d methods on *AppManagement; the census is not looking at the right package", len(handlers))
	}
	gated := 0
	for _, h := range handlers {
		if h.gated {
			gated++
		}
	}
	if gated == 0 {
		t.Fatal("no handler appears gated at all; the gate detection is broken, not the code")
	}

	var ungated []string
	usedException := map[string]bool{}
	for _, h := range handlers {
		if reason, excepted := gateExceptions[h.name]; excepted {
			usedException[h.name] = true
			// An exception that is WRONG is worse than none: if the excepted
			// name now takes an echo.Context, it is reachable and the reason
			// on file no longer describes it.
			if h.takesCtx {
				t.Errorf("%s is excepted as %q, but it takes an echo.Context — the exception is stale and it is now reachable (%s)", h.name, reason, h.pos)
			}
			continue
		}
		if !h.takesCtx {
			t.Errorf("%s takes no echo.Context and is not listed in gateExceptions; add it with a reason or gate it (%s)", h.name, h.pos)
			continue
		}
		if !h.gated {
			ungated = append(ungated, h.name+" ("+h.pos+")")
		}
	}

	for name := range gateExceptions {
		if !usedException[name] {
			t.Errorf("gateExceptions lists %q, which no longer matches any method on *AppManagement — a stale excuse nobody would notice", name)
		}
	}

	if len(ungated) > 0 {
		sort.Strings(ungated)
		t.Fatalf("%d handler(s) reachable over HTTP with no authorization gate:\n  %s\n"+
			"Every one of these answers any account the JWT middleware lets in, including a "+
			"context that resolved to guest with no principal behind it. Add "+
			"requirePerm (or requirePermInternal, for a documented root-local peer read).",
			len(ungated), strings.Join(ungated, "\n  "))
	}
}
