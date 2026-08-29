package gateaudit

import (
	"os"
	"path/filepath"
	"testing"
)

// The whole tree must be free of gates that deny without aborting.
//
// This scans every sibling module, not just this one: the defect has appeared
// in three different modules, and a checker that only looks at its own package
// would have missed all three.
func TestNoGateDeniesWithoutAborting(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("the source tree is unreadable, so this check went blind: %v", err)
	}

	scanned := 0
	var all []Finding
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < 8 || e.Name()[:8] != "CasaNova" {
			continue
		}
		found, err := Scan(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("scanning %s failed: %v", e.Name(), err)
		}
		scanned++
		all = append(all, found...)
	}

	// A scan that examined nothing is a failure, not a pass. This check has to
	// be able to say it looked.
	if scanned < 5 {
		t.Fatalf("only %d CasaNova modules were scanned from %s — this check is "+
			"not seeing the tree it claims to audit", scanned, root)
	}

	for _, f := range all {
		t.Errorf("%s:%d: %s denies with a ctx write and returns its nil error, so "+
			"`if err := %s(c); err != nil { return err }` does NOT abort and the "+
			"handler runs anyway. Use echo.NewHTTPError.",
			f.File, f.Line, f.Function, f.Function)
	}
}
