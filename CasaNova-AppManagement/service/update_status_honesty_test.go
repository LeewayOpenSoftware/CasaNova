package service

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A FAILURE TO CHECK MUST NOT BE CACHED AS "UP TO DATE".
//
// isUpdateAvailable returned (false, nil) when it could not read an app's
// store info. Its caller, IsUpdateAvailable, declines to cache only when the
// error is non-nil — so a transient failure was written into the
// isAppUpgradable cache as a measured `false` and stayed there for the cache's
// lifetime. The app reported "up to date" for something nobody had managed to
// check, and the only way out was a restart or the cache expiring.
//
// The seam is real rather than mocked: a compose project with no x-casaos
// extension makes StoreInfo fail exactly the way a malformed or partially
// written one does, which is the case this is about.

// The app is built as a literal rather than from YAML, and that took two
// tries to get right:
//
//   - common's sample projects all HAVE store info, so the first version
//     exercised the success path; its own premise check caught that.
//   - a hand-written minimal YAML does not help either: NewComposeAppFromYAML
//     fills in x-casaos when it is missing (compose_app.go, "re-using app name
//     as title"), so an app built that way ALWAYS has store info.
//
// A ComposeApp with no x-casaos extension is what a project written by an
// older build, or half-written by an interrupted save, looks like on disk —
// and it is reachable only by constructing the value, which is what this does.

// Mutation that must fail this test: return (false, nil) from the StoreInfo
// error branch again.
func TestAnAppWhoseStoreInfoCannotBeReadIsNotReportedAsUpToDate(t *testing.T) {
	app := &ComposeApp{Name: "no-store-info", Extensions: map[string]interface{}{}}

	// Premise, stated out loud: this app really is one whose store info cannot
	// be read. If that ever stops being true the test below would exercise a
	// different path entirely and its silence would mean nothing.
	if _, siErr := app.StoreInfo(false); siErr == nil {
		t.Fatal("this app now HAS store info; the test's premise is gone, not its property")
	}

	a := &AppStoreManagement{}
	available, err := a.isUpdateAvailable(context.Background(), app)

	if err == nil {
		t.Fatal("isUpdateAvailable returned nil for an app it could not check; the caller caches that as a measured " +
			"'up to date' and the app stops being checked at all")
	}
	if available {
		t.Error("an app that could not be checked was reported as having an update")
	}
}

// The nil guard has to run BEFORE the first dereference.
//
// `if storeInfo == nil` used to sit below `storeInfo.IsUncontrolled`, where it
// could only ever be reached after a nil storeInfo had already panicked — a
// guard placed where it cannot guard. There is no way to hand this function a
// nil storeInfo with a nil error from outside, so the ORDER is pinned in the
// source instead.
//
// It reads the AST rather than the text. The first version searched the file
// as a string and failed on its own explanatory comment, which mentions
// `storeInfo.IsUncontrolled` above the guard — a test fooled by prose about
// the thing it is checking.
func TestTheNilStoreInfoGuardRunsBeforeTheDereference(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "appstore_management.go", nil, 0)
	if err != nil {
		t.Fatalf("parse appstore_management.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == "isUpdateAvailable" && fd.Recv != nil {
			fn = fd
			break
		}
	}
	if fn == nil || fn.Body == nil {
		t.Fatal("isUpdateAvailable not found; this test is looking at the wrong file")
	}

	guard := token.NoPos
	deref := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			// `storeInfo == nil`
			if id, ok := node.X.(*ast.Ident); ok && id.Name == "storeInfo" {
				if nilIdent, ok := node.Y.(*ast.Ident); ok && nilIdent.Name == "nil" && node.Op == token.EQL {
					if guard == token.NoPos || node.Pos() < guard {
						guard = node.Pos()
					}
				}
			}
		case *ast.SelectorExpr:
			// any `storeInfo.<field>`
			if id, ok := node.X.(*ast.Ident); ok && id.Name == "storeInfo" {
				if deref == token.NoPos || node.Pos() < deref {
					deref = node.Pos()
				}
			}
		}
		return true
	})

	if guard == token.NoPos {
		t.Fatal("the `storeInfo == nil` guard is gone entirely")
	}
	if deref == token.NoPos {
		t.Fatal("nothing dereferences storeInfo any more; the test's premise is gone, not its property")
	}
	if guard > deref {
		t.Fatalf("the nil guard (%s) is BELOW the first dereference (%s); it can only run after a nil storeInfo "+
			"has already panicked", fset.Position(guard), fset.Position(deref))
	}
}
