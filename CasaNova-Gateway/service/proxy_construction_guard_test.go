package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// EVERY REVERSE PROXY ON THIS BOX MUST BE BUILT IN ONE PLACE.
//
// newConfiguredProxy wraps the Director so that it deletes the headers a
// client must never be able to forge — X-Forwarded-*, user_id, role, and above
// all X-CasaNova-Internal, whose valid value makes requirePermInternal admit a
// request WITHOUT A JWT. Today both production call sites go through it. That
// was measured, and measuring it is not the same as enforcing it: nothing stops
// the next person from writing httputil.NewSingleHostReverseProxy directly, and
// the proxy they get looks and works exactly like the safe one until the day
// somebody sends the header through it.
//
// A gate that is merely true today is the shape this floor keeps finding: the
// success by VERIFICATION and the success by ABSENCE print the same. This test
// is what makes the property hold tomorrow.
//
// WHY THE AST AND NOT A REGEX over the source, which is how
// route/management_gate_test.go pins its own guard:
//
//  1. The question is structural, not textual. The rule is not "does this
//     string appear" but "is this CALL inside newConfiguredProxy". Which
//     function encloses a line is something a text scan can only approximate,
//     and approximating it means re-implementing a parser badly.
//  2. Prose immunity, and this one is not hypothetical. To be readable, this
//     very file has to name NewSingleHostReverseProxy in its comments, and so
//     does management.go. A regex would match its own explanation — a test
//     fooled by the text it wrote about the thing it is checking. That mistake
//     was made on this floor within the week, by a guard that searched for a
//     symbol and matched the comment above it.
//  3. It removes the need for this file to exempt itself. Because the mentions
//     here are comments and strings rather than call expressions, the walk
//     below does not see this file at all, so no exception has to live inside
//     the artifact the rule examines.
//  4. It survives an alias. `hu "net/http/httputil"` then
//     `hu.NewSingleHostReverseProxy(target)`, or a dot-import and a bare call,
//     defeat a regex anchored on "httputil." and are matched here by resolving
//     each file's own import name.
//
// The regex form was still the right thing to read first: the counted
// assertions and the anti-vacuity checks below are that test's shape, kept.

// proxyConstructionSite is one (file, function) pair allowed to call
// httputil.NewSingleHostReverseProxy.
//
// An allowlist of exactly two entries, each naming BOTH the file and the
// enclosing function. Naming the file alone would let a second function added
// to management.go build an unstripped proxy and still pass.
var proxyConstructionSites = map[string]string{
	// The constructor itself. This is the one the rule exists to protect.
	"CasaNova-Gateway/service/management.go": "newConfiguredProxy",

	// A test in package `route` that deliberately builds a bare proxy,
	// because what it tests is an idle-timeout wrapper and — in its own
	// words — "a wrapper for a proxy has to be tested THROUGH a proxy".
	//
	// This exception is structural rather than a favour: newConfiguredProxy
	// is UNEXPORTED and lives in package `service`. A test in package `route`
	// cannot call it, whatever it would prefer. The day that stops being true
	// this entry should go.
	"CasaNova-Gateway/route/idle_deadline_test.go": "TestASlowClientThroughAProxyReceivesTheWholeBody",
}

const (
	proxyConstructor = "NewSingleHostReverseProxy"
	proxyType        = "ReverseProxy"
)

// Mutation that must fail this test: add
// `_ = httputil.NewSingleHostReverseProxy(nil)` to any function anywhere in
// the repository that is not one of the two named above.
func TestEveryReverseProxyIsBuiltByNewConfiguredProxy(t *testing.T) {
	root := repoRoot(t)

	type site struct {
		file string
		fn   string
		pos  string
	}
	var found []site
	filesParsed := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			// Build caches and lab scratch directories are generated, not
			// work; they are also where a stray copy of a source tree lands.
			if strings.HasPrefix(d.Name(), ".gocache") || strings.HasPrefix(d.Name(), ".npm") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file we could not read is a file we did not check, and a
			// silent skip here would make this guard weaker exactly where the
			// source is strangest. Say so instead.
			t.Errorf("cannot parse %s: %v — this guard did not examine it", rel(root, path), perr)
			return nil
		}
		filesParsed++

		names, dot := httputilNames(f)
		if len(names) == 0 && !dot {
			return nil // this file cannot name the constructor at all
		}

		// isHTTPUtil reports whether sel names `<httputil>.<want>` under this
		// file's own import names, including an alias or a dot-import.
		isHTTPUtil := func(e ast.Expr, want string) bool {
			switch t := e.(type) {
			case *ast.SelectorExpr:
				id, ok := t.X.(*ast.Ident)
				return ok && names[id.Name] && t.Sel.Name == want
			case *ast.Ident:
				return dot && t.Name == want
			}
			return false
		}

		// Two ways to build one, and the guard has to know both. The
		// constructor is what anyone reaches for; the STRUCT LITERAL is what
		// anyone reaches for next, once a guard on the constructor exists.
		// `&httputil.ReverseProxy{Director: ...}` produces the same object,
		// strips nothing, and would have walked straight past a rule written
		// only against the function name. Neither form is used outside the two
		// sites today — the literal is closed because it is cheap to close,
		// not because somebody used it.
		record := func(pos token.Pos) {
			found = append(found, site{
				file: rel(root, path),
				fn:   enclosingFunc(f, pos),
				pos: rel(root, fset.Position(pos).Filename) + ":" +
					strconv.Itoa(fset.Position(pos).Line),
			})
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if isHTTPUtil(node.Fun, proxyConstructor) {
					record(node.Pos())
				}
			case *ast.CompositeLit:
				if isHTTPUtil(node.Type, proxyType) {
					record(node.Pos())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// ANTI-VACUITY, first and loudest. A walk that started in the wrong place
	// finds nothing and congratulates the repository — the failure mode this
	// floor has hit more than any other. Both numbers below are floors, not
	// expectations: the tree holds ~990 Go files and at least one real call.
	if filesParsed < 500 {
		t.Fatalf("only %d Go files were parsed under %s; this guard is looking at the wrong tree "+
			"and its silence means nothing", filesParsed, root)
	}
	if len(found) == 0 {
		t.Fatalf("no call to %s was found anywhere under %s. The constructor in management.go is "+
			"supposed to be one; finding zero means the search is broken, not that the repository is clean",
			proxyConstructor, root)
	}

	used := map[string]bool{}
	for _, s := range found {
		want, allowed := proxyConstructionSites[s.file]
		if allowed && want == s.fn {
			used[s.file] = true
			continue
		}
		t.Errorf("%s builds a reverse proxy directly, in %s.\n"+
			"Every proxy must come from service.newConfiguredProxy, whose Director deletes the "+
			"identity headers a client must never be able to forge — including %s, which admits a "+
			"request with no JWT at all. A proxy built here strips nothing and looks identical to "+
			"one that does.\n"+
			"If this call is genuinely not a public boundary, add the (file, function) pair to "+
			"proxyConstructionSites with the reason, rather than widening the rule.",
			s.pos, describeFunc(s.fn), "X-CasaNova-Internal")
	}

	// An unused exception is a stale excuse: it reads as a live carve-out for
	// something that has since moved or been deleted, and nobody would notice.
	for file, fn := range proxyConstructionSites {
		if !used[file] {
			t.Errorf("the exception for %s (%s) matched nothing. Either the file moved, the function "+
				"was renamed, or the call is gone — in every case the entry is now a carve-out for "+
				"nothing and should be removed or corrected.", file, fn)
		}
	}
}

// httputilNames returns the local names under which f can reach
// net/http/httputil, and whether it dot-imports the package.
func httputilNames(f *ast.File) (map[string]bool, bool) {
	names := map[string]bool{}
	dot := false
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "net/http/httputil" {
			continue
		}
		switch {
		case imp.Name == nil:
			names["httputil"] = true
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name == "_":
			// blank import: the package is linked but cannot be called.
		default:
			names[imp.Name.Name] = true
		}
	}
	return names, dot
}

// enclosingFunc names the function declaration containing pos, or "" for a
// call at package level (a var initializer, say) — which is not
// newConfiguredProxy either, and is reported as the violation it is.
func enclosingFunc(f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if fd.Body.Pos() <= pos && pos <= fd.Body.End() {
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				return recvTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
			}
			return fd.Name.Name
		}
	}
	return ""
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver
		return recvTypeName(t.X)
	}
	return "?"
}

func describeFunc(fn string) string {
	if fn == "" {
		return "a package-level initializer, outside any function"
	}
	return "func " + fn
}

// repoRoot climbs from the package directory to the checkout root, identified
// by holding BOTH top-level module directories. Two markers rather than one:
// a single directory named CasaNova-Gateway could exist anywhere, and stopping
// at the wrong one is precisely how this guard would pass without checking
// anything.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the package directory: %v", err)
	}
	for {
		_, gwErr := os.Stat(filepath.Join(dir, "CasaNova-Gateway"))
		_, cmErr := os.Stat(filepath.Join(dir, "CasaNova-Common"))
		if gwErr == nil && cmErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no checkout root above the package directory holds both CasaNova-Gateway and " +
				"CasaNova-Common; this guard cannot tell what it would be scanning")
		}
		dir = parent
	}
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
