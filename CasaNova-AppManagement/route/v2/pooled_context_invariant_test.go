package v2

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEchoContextInsideGoroutine is a source-level invariant, not a
// behavioural test, because the bug it guards is invisible at runtime until
// it corrupts someone else's request.
//
// echo POOLS echo.Context values: when a handler returns, its context goes
// back to a sync.Pool and is Reset() onto the NEXT request that arrives. A
// goroutine spawned by a handler starts running AFTER (or racing with) that
// handler's return, so any read of the echo.Context from inside the goroutine
// — PropertiesFromQueryParams(ctx), ctx.QueryParam, ctx.Request(), … — is a
// data race against echo recycling the object, and the value it reads belongs
// to a DIFFERENT user's request. That is how updateGlobalEnv/deleteGlobalEnv
// (route/v2/global.go) and PullImages (route/v2/image.go) published message-bus
// events carrying a stranger's query parameters.
//
// The fix in every case is the same: snapshot what the goroutine needs while
// the handler still owns the context, and close over the snapshot. This test
// enforces that shape for the whole package.
//
// Revert probe: move `props := PropertiesFromQueryParams(ctx)` back inside
// either goroutine in global.go, or restore `PropertiesFromQueryParams(ctx)`
// inside the PullImages goroutine, and this test goes red naming the file and
// line.
func TestNoEchoContextInsideGoroutine(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	goroutinesChecked := 0

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			echoParams := echoContextParamNames(fn)

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				goStmt, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				lit, ok := goStmt.Call.Fun.(*ast.FuncLit)
				if !ok || lit.Body == nil {
					return true
				}
				goroutinesChecked++

				ast.Inspect(lit.Body, func(inner ast.Node) bool {
					id, ok := inner.(*ast.Ident)
					if !ok {
						return true
					}
					if id.Name == "PropertiesFromQueryParams" {
						t.Errorf("%s: goroutine spawned in %s() calls PropertiesFromQueryParams — "+
							"the echo.Context is pooled and may already belong to another request. "+
							"Snapshot it before `go func` and close over the map.",
							fset.Position(id.Pos()), fn.Name.Name)
						return true
					}
					if echoParams[id.Name] {
						t.Errorf("%s: goroutine spawned in %s() reads echo.Context %q — "+
							"echo recycles that context onto the next request once the handler "+
							"returns. Snapshot what you need before `go func`.",
							fset.Position(id.Pos()), fn.Name.Name, id.Name)
					}
					return true
				})
				return true
			})
		}
	}

	// Anti-vacuity: a green result must mean "we looked at real goroutines",
	// not "the glob matched nothing" or "the AST walk found no GoStmt".
	if goroutinesChecked < 4 {
		t.Fatalf("guard is vacuous: only %d goroutine literals inspected in package v2 "+
			"(expected at least 4) — the AST walk or the file glob is broken", goroutinesChecked)
	}
}

// echoContextParamNames returns the parameter names of fn whose declared type
// is echo.Context.
func echoContextParamNames(fn *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	if fn.Type.Params == nil {
		return names
	}
	for _, field := range fn.Type.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Context" {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "echo" {
			continue
		}
		for _, n := range field.Names {
			names[n.Name] = true
		}
	}
	return names
}
