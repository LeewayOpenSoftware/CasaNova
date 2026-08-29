// Package release holds build-time invariants that span a Go source file and a
// packaged artifact, where neither side can assert the relationship alone.
//
// They live in their own package because main's init() calls flag.Parse(), which
// aborts any test binary in package main.
package release

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

const (
	mainGo   = "../../main.go"
	unitFile = "../../build/sysroot/usr/lib/systemd/system/casanova.service"
)

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s is unreadable, so the invariant cannot be checked against "+
			"anything — this is a FAILURE, not a skip: %v", filepath.Base(p), err)
	}
	return string(b)
}

// The in-process recovery deadline is only a bound if systemd waits longer than
// it. It did not: the unit inherited a 90s default while the budget was 120s,
// so systemd would have killed the service thirty seconds before the deadline
// could fire — turning the hang it was written to bound into a crash-loop.
//
// Either file may be edited. Editing one alone fails here.
func TestStartupBudgetFitsInsideTheSystemdStartTimeout(t *testing.T) {
	src := mustRead(t, mainGo)
	unit := mustRead(t, unitFile)

	bm := regexp.MustCompile(
		`storageMaintenanceStartBudget\s*=\s*(\d+)\s*\*\s*time\.(Second|Minute)`,
	).FindStringSubmatch(src)
	if bm == nil {
		t.Fatal("storageMaintenanceStartBudget was not found in main.go in a form " +
			"this test can read. It was RENAMED OR REWRITTEN, and this check went " +
			"blind — that is exactly how a gate starts passing vacuously. Update " +
			"the pattern deliberately.")
	}
	n, _ := strconv.Atoi(bm[1])
	budget := time.Duration(n) * time.Second
	if bm[2] == "Minute" {
		budget = time.Duration(n) * time.Minute
	}

	um := regexp.MustCompile(`(?m)^TimeoutStartSec=(\d+)\s*$`).FindStringSubmatch(unit)
	if um == nil {
		t.Fatal("casanova.service does not DECLARE TimeoutStartSec. Inheriting " +
			"DefaultTimeoutStartSec checks the deadline in main.go against a " +
			"number this repository does not control; a host that lowers it turns " +
			"a bounded startup into a restart loop.")
	}
	secs, _ := strconv.Atoi(um[1])
	unitTimeout := time.Duration(secs) * time.Second

	// Recovery is not the only thing that runs before the service reports ready,
	// so the budget must leave room rather than squeak under the limit. The rest
	// of startup — revocation authority, storage authority, DB, routers — shares
	// the same window.
	if budget*2 > unitTimeout {
		t.Fatalf("storage recovery may take %s but systemd waits only %s for the "+
			"whole startup, so the in-process deadline cannot fire before the unit "+
			"is killed and bounds nothing. Lower the budget or raise "+
			"TimeoutStartSec (keep the budget under half).", budget, unitTimeout)
	}
}

// `casanova -v` must answer and exit without touching the system. The upgrade
// script invokes /usr/bin/casanova -v four times, once while the services are
// already running, as root — so any side effect ahead of the guard runs
// concurrently against the live instance's own state. It used to run storage
// authority provisioning and the full storage-maintenance recovery.
func TestVersionFlagIsCheckedBeforeAnySideEffect(t *testing.T) {
	// Parsed, not grepped. The first version of this scanned raw source and
	// matched its own explanatory COMMENT — which names the very calls it
	// forbids — so it failed on correct code. A checker that reads prose as
	// program text is not a checker; the AST is the only honest reading.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, mainGo, nil, 0)
	if err != nil {
		t.Fatalf("main.go does not parse, so this invariant cannot be checked: %v", err)
	}

	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("func main was not found; this check went blind")
	}

	// Where is the guard? It must be a top-level statement of main, and every
	// statement before it must be effect-free.
	guardAt := -1
	for i, stmt := range body.List {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok || ifs.Else != nil || len(ifs.Body.List) != 1 {
			continue
		}
		star, ok := ifs.Cond.(*ast.StarExpr)
		if !ok {
			continue
		}
		id, ok := star.X.(*ast.Ident)
		if !ok || id.Name != "versionFlag" {
			continue
		}
		if _, ok := ifs.Body.List[0].(*ast.ReturnStmt); !ok {
			continue
		}
		guardAt = i
		break
	}
	if guardAt < 0 {
		t.Fatal("main() has no top-level `if *versionFlag { return }` guard, so " +
			"`casanova -v` runs whatever main() does before reaching one")
	}

	// Any call at all before the guard is suspect: this is a version print.
	for _, stmt := range body.List[:guardAt] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			t.Errorf("main() calls %s at %s, BEFORE the -v guard, so "+
				"`casanova -v` performs it against the live system",
				render(call.Fun), fset.Position(call.Pos()))
			return true
		})
	}
}

// render names a call target well enough for an error message.
func render(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return render(v.X) + "." + v.Sel.Name
	default:
		return "a call"
	}
}
