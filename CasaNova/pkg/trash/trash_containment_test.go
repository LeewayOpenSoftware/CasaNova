package trash

// The self-recursion guard in Trash() used to compare paths with a bare
// strings.HasPrefix(absPath, trashRoot). That rejects more than the trash
// directory itself: any sibling whose NAME happens to start with the trash
// directory's name is also a lexical prefix match, even though it sits next
// to the trash dir, not inside it. Same containment-lexica bug class already
// fixed for mount points (pathWithinMount) and for volumes elsewhere in the
// repo — fixed here by reusing pathWithinMount instead of re-deriving it.

import (
	"os"
	"path/filepath"
	"testing"
)

// THE GAP: a directory named ".Trash-casanova-backup" sits right next to
// ".Trash-casanova" and shares it as a lexical prefix. A file inside that
// unrelated sibling must still be trashable.
func TestTrashDoesNotRejectASiblingWithAPrefixedName(t *testing.T) {
	m, root := trashManagerForTest(t)

	sibling := filepath.Join(root, TrashDirName+"-backup")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(sibling, "innocent.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Trash(victim); err != nil {
		t.Fatalf("Trash refused a file in an unrelated sibling directory whose name merely "+
			"starts with the trash dir's name: %v", err)
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatal("victim was not moved out of the sibling directory")
	}
}

// Positive control: the guard must still refuse an item genuinely inside the
// trash directory (files/ subtree), the case the guard exists for.
func TestTrashRefusesAnItemInsideTheTrashDirectory(t *testing.T) {
	m, root := trashManagerForTest(t)

	victim := filepath.Join(root, "documento.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := m.Trash(victim)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	trashedFile := filepath.Join(entry.trashRoot, "files", entry.TrashedName)

	if _, err := m.Trash(trashedFile); err == nil {
		t.Fatal("Trash accepted an item already inside the trash directory; infinite recursion guard is broken")
	}
}
