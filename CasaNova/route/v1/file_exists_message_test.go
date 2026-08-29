package v1

// Pins a defect FAMILY, not three individual lines.
//
// Three handlers guarded against a MISSING file and then answered
// common_err.FILE_ALREADY_EXISTS — "File already exists" — for a file that did
// not exist. PutFileContent's copy is the one that mattered: creating a note
// failed with HTTP 500 and that message, which sends whoever reads it looking
// for a collision that is not there. The text was copy-pasted, so fixing the
// three sites without a guard just leaves the fourth to be written later.
//
// The invariant: inside a branch taken because a path does NOT exist, a
// handler may not report that it already does. FILE_DOES_NOT_EXIST has been in
// the error table all along with exactly the right text.
//
// This deliberately reads the source rather than driving the handlers: the
// three call sites sit behind RBAC, a storage-authority principal and real
// filesystem state, and a test that stubbed all of that would be asserting on
// its own scaffolding. What is being pinned here is a property of the code.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// negatedExistenceCheck reports whether cond is a check that a path is ABSENT.
func negatedExistenceCheck(cond ast.Expr) bool {
	unary, ok := cond.(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		return false
	}
	call, ok := unary.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	// !file.Exists(p) — "p is absent".
	return sel.Sel.Name == "Exists"
}

func mentionsAlreadyExists(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok {
			if strings.Contains(sel.Sel.Name, "ALREADY_EXISTS") || strings.Contains(sel.Sel.Name, "FILE_OR_DIR_EXISTS") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func TestAbsentFileIsNeverReportedAsAlreadyExisting(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var offenders []string
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(f, func(node ast.Node) bool {
				ifStmt, ok := node.(*ast.IfStmt)
				if !ok || !negatedExistenceCheck(ifStmt.Cond) {
					return true
				}
				if mentionsAlreadyExists(ifStmt.Body) {
					pos := fset.Position(ifStmt.Pos())
					offenders = append(offenders,
						filepath.Base(pos.Filename)+":"+strconv.Itoa(pos.Line))
				}
				return true
			})
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("handler(s) report \"already exists\" inside a branch taken because the path does NOT exist: %s\n"+
			"use common_err.FILE_DOES_NOT_EXIST — the message is the only thing that tells the caller what went wrong",
			strings.Join(offenders, ", "))
	}
}
