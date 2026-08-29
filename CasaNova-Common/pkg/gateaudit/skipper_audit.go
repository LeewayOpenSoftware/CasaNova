package gateaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// IPDecision names a place where an authentication decision is made from the
// peer's address instead of from a credential.
type IPDecision struct {
	File string
	Line int
	Expr string
}

// ScanIPAuth finds authentication decisions taken from the network address.
//
// THE DEFECT. Ten routers in this tree skipped JWT validation for
//
//	loopback.IsLoopback(c) && Authorization == ""
//
// The gateway reverse-proxies the LAN and the Cloudflare tunnel to every
// backend over 127.0.0.1, so the socket peer is 127.0.0.1 for the entire
// internet, and "sends no Authorization header" is something any caller
// achieves by not sending one. Measured: an anonymous request read every
// installed app's compose file, and reached a service whose write routes run
// /bin/sh -c as root.
//
// The tenth instance survived a grep-based sweep because it was a NAMED
// function rather than the one-line expression being searched for. So this
// walks the AST instead, and reports any call to loopback.IsLoopback whose
// value feeds a boolean decision — leaving alone the legitimate uses, which
// operate on a net.IP obtained from somewhere else (enumerating this host's own
// interfaces, or refusing to FETCH a loopback destination as SSRF defence).
//
// The discriminator is the argument: loopback.IsLoopback(c) takes the REQUEST
// and asks "where did this come from" — that is the auth decision. ip.IsLoopback()
// is a method on a net.IP and is not.
func ScanIPAuth(root string) ([]IPDecision, error) {
	var out []IPDecision
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
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "IsLoopback" {
				return true
			}
			// Package-qualified call taking the request context: the auth shape.
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "loopback" || len(call.Args) != 1 {
				return true
			}
			out = append(out, IPDecision{
				File: path, Line: fset.Position(call.Pos()).Line,
				Expr: "loopback.IsLoopback(<request>)",
			})
			return true
		})
		return nil
	})
	return out, err
}
