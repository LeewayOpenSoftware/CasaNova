package tokenrevocation

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This codebase has a specific, repeated failure: a security helper is fixed
// in one place while identical copies elsewhere keep the defect. The auth gate
// was corrected in the main service and stayed broken in seven copies across
// five other services; the trash-restore and loopback classes went the same
// way.
//
// Revocation is exactly that shape. Every service declares its JWT middleware
// inline — there are thirteen of them — and a service that omits the check
// accepts tokens for deleted principals while its neighbours refuse them. The
// defect would be invisible: that service simply keeps working.
//
// So this test does not check that revocation is wired anywhere in particular.
// It sweeps the tree and requires that EVERY token-parsing middleware consults
// the authority. A new service copied from an old one fails here on the day it
// is written, not after someone's account is deleted.
func TestEveryTokenParsingMiddlewareConsultsTheAuthority(t *testing.T) {
	root := treeRoot(t)

	// The call each middleware must make. RevokedForClaims is the nil-safe
	// wrapper; a direct IsRevoked/IsRevokedID call is equally acceptable.
	consults := regexp.MustCompile(`RevokedForClaims\(|tokenrevocation\.IsRevoked`)
	parseFunc := regexp.MustCompile(`ParseTokenFunc:\s*func\(`)

	var missing []string
	checked := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corners of the tree are not this test's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "build", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := string(data)

		for _, loc := range parseFunc.FindAllStringIndex(src, -1) {
			body := functionBody(src, loc[1])
			checked++
			if !consults.MatchString(body) {
				rel, _ := filepath.Rel(root, path)
				line := 1 + strings.Count(src[:loc[0]], "\n")
				missing = append(missing, rel+":"+itoa(int64(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if checked == 0 {
		t.Skip("no token-parsing middleware found; not running against the full tree")
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d token-parsing middlewares never consult the revocation authority.\n"+
			"Each one accepts tokens for deleted principals:\n  %s",
			len(missing), checked, strings.Join(missing, "\n  "))
	}
	t.Logf("all %d token-parsing middlewares consult the revocation authority", checked)
}

// functionBody returns the body of the function literal whose parameter list
// begins at start, matched by brace depth.
//
// The opening brace has to be found deliberately rather than by taking the
// first '{': these signatures return interface{}, and counting that as the
// body opener makes the extracted "body" the signature alone — which silently
// reports every wired middleware as missing. The body brace is the one at the
// end of a line.
func functionBody(src string, start int) string {
	open := -1
	for i := start; i < len(src); i++ {
		if src[i] != '{' {
			continue
		}
		j := i + 1
		for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		if j < len(src) && src[j] == '\n' {
			open = i
			break
		}
	}
	if open < 0 {
		return ""
	}

	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open:i]
			}
		}
	}
	return src[open:]
}

// treeRoot locates the checkout root by walking up until the sibling service
// directories are visible.
func treeRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Skipf("cannot determine working directory: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "CasaNova-UserService")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("service tree not found from this working directory")
	return ""
}
