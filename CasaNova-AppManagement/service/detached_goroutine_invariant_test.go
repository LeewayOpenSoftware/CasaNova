package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDetachedGoroutinesDropCallerCancellation guards the single mistake that
// has now broken three separate app operations in compose_app.go.
//
// Every HTTP handler here builds its background context with
// `context.WithTimeout(context.Background(), …)` and then `defer baseCancel()`s
// it. That cancel fires the instant the handler returns 200 — which is BEFORE
// the goroutine it just spawned has done any real work. So a goroutine that
// keeps using the caller's ctx is running on an already-canceled context:
//
//   - Update() (:292) — the image pull died "context canceled" and PullAndApply
//     rolled the update back, so every app update silently reverted.
//   - Apply() (:1077) — same, so every tag edit reverted.
//   - SetStatus/Start (:1274) — every service.Ps() failed instantly, so the
//     20s "wait for the container to stop" loop burned all 10 retries in a few
//     microseconds and then issued a Start that was already doomed.
//
// The fix is always the same shape: keep the caller's VALUES (event properties
// ride the ctx to the message bus) but drop its cancellation, then own a
// bounded deadline here — `context.WithTimeout(context.WithoutCancel(ctx), …)`.
//
// A goroutine literal that takes a `ctx context.Context` parameter in this file
// is by construction one of these detached workers, so the invariant is:
// every one of them re-derives ctx through context.WithoutCancel.
//
// Revert probe: change any of the three to `context.WithTimeout(ctx, …)` and
// this test goes red naming the line.
func TestDetachedGoroutinesDropCallerCancellation(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "compose_app.go", nil, 0)
	if err != nil {
		t.Fatalf("parse compose_app.go: %v", err)
	}

	checked := 0
	ast.Inspect(f, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		lit, ok := goStmt.Call.Fun.(*ast.FuncLit)
		if !ok || lit.Body == nil || !takesContextParam(lit) {
			return true
		}
		checked++

		found := false
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "WithoutCancel" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s: detached goroutine takes the caller's ctx but never calls "+
				"context.WithoutCancel — the handler cancels that ctx the moment it "+
				"returns, so this work runs on a dead context. Use "+
				"context.WithTimeout(context.WithoutCancel(ctx), d).",
				fset.Position(lit.Pos()))
		}
		return true
	})

	// Anti-vacuity: green must mean "we inspected the three known detached
	// workers", not "the AST walk matched nothing".
	if checked < 3 {
		t.Fatalf("guard is vacuous: only %d detached goroutine literals found in "+
			"compose_app.go (expected at least 3 — Update, Apply, SetStatus/Start)", checked)
	}
}

// takesContextParam reports whether the func literal declares a
// `context.Context` parameter — the marker of a worker that was handed the
// caller's context explicitly rather than closing over one.
func takesContextParam(lit *ast.FuncLit) bool {
	if lit.Type.Params == nil {
		return false
	}
	for _, field := range lit.Type.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Context" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" {
			return true
		}
	}
	return false
}
