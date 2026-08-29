// Package gateaudit finds authorization gates whose denial path does not abort
// the handler.
//
// THE DEFECT IT EXISTS FOR.
//
// In echo, returning an error VALUE is what stops a handler chain. ctx.JSON
// returns the serializer's error, which is nil once the bytes are written. So a
// helper written as
//
//	func requireX(c echo.Context) error {
//	    if !allowed { return c.JSON(403, ...) }   // returns nil!
//	    return nil
//	}
//
// hands nil to `if err := requireX(c); err != nil { return err }`. The 403 is
// written to the response and THE HANDLER CONTINUES. The caller is told
// "permission denied" while the guarded work runs.
//
// This has been wrong three separate times in this codebase. The third instance
// guarded task creation and execution in CasaNova-JobManagement, which run
// /bin/sh -c as root: GET :85/v2/job_management/tasks with no token answered
// 200 rather than 403, live, on a box exposed through a Cloudflare tunnel.
//
// Grep cannot find this reliably — the string "ctx.JSON" appears in every
// handler's success path — so this walks the AST and looks only at RETURN
// statements inside functions shaped like a gate.
package gateaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// Finding names one gate whose denial does not abort.
type Finding struct {
	File     string
	Line     int
	Function string
}

// gateName reports whether a function name looks like an authorization gate.
func gateName(n string) bool {
	l := strings.ToLower(n)
	return strings.HasPrefix(l, "require") || strings.HasPrefix(l, "ensureperm") ||
		strings.HasPrefix(l, "checkperm") || strings.HasPrefix(l, "authorize")
}

// Scan walks root and reports every gate that returns ctx.JSON(...) or
// ctx.NoContent(...) — expressions whose value is nil on success.
func Scan(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !gateName(fn.Name.Name) {
				continue
			}
			// A gate returns error. Anything else is not this pattern.
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) != 1 {
					return true
				}
				call, ok := ret.Results[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "JSON", "JSONBlob", "String", "NoContent", "HTML", "Blob":
					// ctx.JSON(...) and friends: nil once written.
					out = append(out, Finding{
						File: path, Line: fset.Position(ret.Pos()).Line,
						Function: fn.Name.Name,
					})
				}
				return true
			})
		}
		return nil
	})
	return out, err
}
