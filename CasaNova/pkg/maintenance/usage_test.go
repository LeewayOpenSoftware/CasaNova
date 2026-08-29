package maintenance

import (
	"os"
	"path/filepath"
	"testing"
)

// Rule 14: st_blocks*512, not st_size. The orphan journal on the box reports
// 375 MB apparent and 180 MB real, and promising the first number would look
// like a bug in the delete path when the second one is what df moves by.
func TestAllocatedBytesUsesBlocksNotSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 8 MiB of hole, no data written.
	if err := f.Truncate(8 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	st, err := lstatNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 8<<20 {
		t.Fatalf("apparent size = %d, want 8 MiB", st.Size)
	}
	if got := allocatedBytes(&st); got >= st.Size {
		t.Fatalf("allocatedBytes = %d for a sparse file of apparent size %d: "+
			"the estimate is following st_size", got, st.Size)
	}
}

// A hardlinked inode inside one plan must be counted once, or the promise is
// inflated by however many names it has.
func TestSizerCountsEachInodeOnce(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a")
	second := filepath.Join(dir, "b")
	writeFile(t, first, "some content")
	if err := os.Link(first, second); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}

	sa, err := lstatNoFollow(first)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := lstatNoFollow(second)
	if err != nil {
		t.Fatal(err)
	}

	sz := newSizer()
	firstBytes := sz.add(&sa)
	secondBytes := sz.add(&sb)
	if firstBytes == 0 {
		t.Fatal("the first name of an inode was not counted")
	}
	if secondBytes != 0 {
		t.Fatalf("the second name of the same inode added %d bytes", secondBytes)
	}
}

func TestDiskUsageReadsTheFilesystem(t *testing.T) {
	di, err := diskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if di.Total <= 0 || di.Free < 0 || di.Used < 0 {
		t.Fatalf("implausible statfs snapshot: %+v", di)
	}
}
