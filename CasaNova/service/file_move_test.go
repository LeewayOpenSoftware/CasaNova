package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
)

// These tests pin the data-safety contract of moveItem. Every one of them
// corresponds to a way the previous implementation could destroy user data:
// erasing the overwrite target before the copy, "rolling back" a good copy
// when the source refused to die, and never using rename(2) at all.

// forceCrossMount makes the staging probe answer EXDEV for the duration of a
// test, which is the only way to exercise the copy fallback without a second
// real filesystem mounted underneath the test directory.
func forceCrossMount(t *testing.T) {
	t.Helper()
	original := stageRename
	stageRename = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { stageRename = original })
}

// failFsyncDirOn makes fsyncDir answer EIO for exactly one directory, leaving
// every other fsync real. EIO is outside the tolerated errno list on purpose:
// it is what a dropping rclone/SMB mount actually returns, and the two move
// paths must react to it differently.
func failFsyncDirOn(t *testing.T, target string) {
	t.Helper()
	original := fsyncDir
	fsyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(target) {
			return syscall.EIO
		}
		return original(path)
	}
	t.Cleanup(func() { fsyncDir = original })
}

// rejectTrash makes the recycle bin refuse everything, so the fail-closed
// branch of the overwrite step can be exercised.
func rejectTrash(t *testing.T) {
	t.Helper()
	original := trashDestination
	trashDestination = func(string) error { return errors.New("recycle bin unavailable") }
	t.Cleanup(func() { trashDestination = original })
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("no stat_t on this platform")
	}
	return st.Ino
}

// stageLeftovers reports staging directories still sitting in dir. A failed
// move may not delete a stage it cannot read, but a successful one must.
func stageLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, moveStagePrefix+"*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// TestMoveSameMountUsesRename covers defect 3: a move inside one mount used to
// be a full copy+delete (double space, slow, and destructive on failure). The
// inode surviving proves rename(2) did the work, and the progress counter must
// still read 100% for an operation where no bytes ever flowed.
func TestMoveSameMountUsesRename(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src", "payload.bin")
	writeFileT(t, src, "the only copy")
	srcIno := inodeOf(t, src)

	dst := filepath.Join(base, "dest", "payload.bin")
	counter := &file.ProgressCounter{}
	size := int64(len("the only copy"))

	if err := moveItem(context.Background(), src, dst, size, counter); err != nil {
		t.Fatalf("moveItem: %v", err)
	}
	if got := readFileT(t, dst); got != "the only copy" {
		t.Fatalf("destination content = %q", got)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after move: %v", err)
	}
	if dstIno := inodeOf(t, dst); dstIno != srcIno {
		t.Fatalf("inode changed (%d -> %d): move fell back to copy+delete instead of rename(2)", srcIno, dstIno)
	}
	if counter.Done() != size {
		t.Fatalf("progress counter = %d, want %d: an instant move must report as completed, not 0%%", counter.Done(), size)
	}
	if left := stageLeftovers(t, filepath.Dir(dst)); len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// TestMoveOverwriteFailedCopyKeepsDestination covers defect 1: the old code
// did RemoveAll(dst) BEFORE copying, so a copy that died halfway (ENOSPC, a
// network mount dropping) left the pre-existing destination gone for good with
// no trash entry. The destination must survive a failed move untouched.
func TestMoveOverwriteFailedCopyKeepsDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test uses to fail the copy")
	}
	forceCrossMount(t)

	base := t.TempDir()
	srcDir := filepath.Join(base, "src", "album")
	writeFileT(t, filepath.Join(srcDir, "readable.txt"), "fine")
	unreadable := filepath.Join(srcDir, "locked.bin")
	writeFileT(t, unreadable, "cannot be read")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	destDir := filepath.Join(base, "dest")
	dst := filepath.Join(destDir, "album")
	writeFileT(t, filepath.Join(dst, "irreplaceable.txt"), "ten years of photos")

	if err := moveItem(context.Background(), srcDir, dst, 100, &file.ProgressCounter{}); err == nil {
		t.Fatal("moveItem succeeded although the copy could not read the source")
	}

	if got := readFileT(t, filepath.Join(dst, "irreplaceable.txt")); got != "ten years of photos" {
		t.Fatalf("pre-existing destination content = %q, want it untouched", got)
	}
	if _, err := os.Lstat(filepath.Join(srcDir, "readable.txt")); err != nil {
		t.Fatalf("source damaged by a failed move: %v", err)
	}
	if left := stageLeftovers(t, destDir); len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// TestMoveSourceRemovalFailureKeepsDestination covers defect 2: when the copy
// succeeded but removing the source failed, the old code "rolled back" by
// deleting the destination — destroying the only complete copy while the
// source had already been partly unlinked. The surviving copy must stay.
func TestMoveSourceRemovalFailureKeepsDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test uses to fail the source removal")
	}
	forceCrossMount(t)

	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	src := filepath.Join(srcDir, "photo.jpg")
	writeFileT(t, src, "irreplaceable bytes")
	// Read+execute but not write on the parent: the copy reads the file
	// fine, the unlink afterwards gets EACCES — the permission-pinned /
	// in-use class of failure whose "rollback" used to delete the good copy.
	if err := os.Chmod(srcDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o755) })

	dst := filepath.Join(base, "dest", "photo.jpg")
	err := moveItem(context.Background(), src, dst, 100, &file.ProgressCounter{})
	if err == nil {
		t.Fatal("moveItem reported success although the source could not be removed")
	}

	if got := readFileT(t, dst); got != "irreplaceable bytes" {
		t.Fatalf("destination content = %q, want the copy kept as the surviving copy", got)
	}
	if _, statErr := os.Lstat(src); statErr != nil {
		t.Fatalf("source vanished as well: %v", statErr)
	}
}

// trashCapableDir returns a directory on a filesystem where pkg/trash can
// actually create its root, or skips. On a workstation /tmp usually sits on
// "/", whose root is not writable by an unprivileged user, so the per-user
// tmpfs is tried first.
func trashCapableDir(t *testing.T) string {
	t.Helper()
	candidates := []string{filepath.Join("/run/user", strconv.Itoa(os.Getuid())), ""}
	for _, base := range candidates {
		var dir string
		if base == "" {
			dir = t.TempDir()
		} else {
			info, err := os.Stat(base)
			if err != nil || !info.IsDir() {
				continue
			}
			d, err := os.MkdirTemp(base, "casanova-move-test-*")
			if err != nil {
				continue
			}
			t.Cleanup(func() { _ = os.RemoveAll(d) })
			dir = d
		}
		probe := filepath.Join(dir, "trash-probe")
		if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
			continue
		}
		entry, err := trash.NewManager().Trash(probe)
		if err != nil {
			_ = os.Remove(probe)
			continue
		}
		_ = trash.NewManager().Delete(entry)
		return dir
	}
	t.Skip("no filesystem reachable from this test can host a recycle bin")
	return ""
}

// findTrashedTree looks for a trashed copy of path in a recycle bin on any
// filesystem above it, identified by member holding content — member is a path
// relative to the trashed entry, or "" when the entry is a plain file. Returns
// the trashed entry's path, or "" when it is nowhere.
func findTrashedTree(t *testing.T, path, member, content string) string {
	t.Helper()
	base := filepath.Base(path)
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		matches, err := filepath.Glob(filepath.Join(dir, trash.TrashDirName, "files", base+"*"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, m := range matches {
			probe := m
			if member != "" {
				probe = filepath.Join(m, member)
			}
			b, err := os.ReadFile(probe)
			if err == nil && string(b) == content {
				return m
			}
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}

// findTrashedCopy is findTrashedTree for a plain file.
func findTrashedCopy(t *testing.T, path, content string) string {
	t.Helper()
	return findTrashedTree(t, path, "", content)
}

// cleanupTrashed removes a trashed entry and its .trashinfo sidecar. The bin
// lives at the mount root, which outlives t.TempDir().
func cleanupTrashed(t *testing.T, trashed string) {
	t.Helper()
	t.Cleanup(func() {
		_ = os.RemoveAll(trashed)
		_ = os.Remove(filepath.Join(filepath.Dir(filepath.Dir(trashed)), "info", filepath.Base(trashed)+".trashinfo"))
	})
}

// TestMoveOverwriteSendsOldDestinationToTrash pins the other half of defect 1:
// an overwrite target is REPLACED, not erased. It must be recoverable from the
// recycle bin afterwards, and it must only leave its place once the
// replacement is complete.
func TestMoveOverwriteSendsOldDestinationToTrash(t *testing.T) {
	base := trashCapableDir(t)

	src := filepath.Join(base, "src", "report.txt")
	writeFileT(t, src, "new revision")
	dst := filepath.Join(base, "dest", "report.txt")
	writeFileT(t, dst, "the only copy of last year")

	if err := moveItem(context.Background(), src, dst, int64(len("new revision")), &file.ProgressCounter{}); err != nil {
		t.Fatalf("moveItem: %v", err)
	}
	if got := readFileT(t, dst); got != "new revision" {
		t.Fatalf("destination content = %q", got)
	}

	trashed := findTrashedCopy(t, dst, "the only copy of last year")
	if trashed == "" {
		t.Fatal("overwritten destination is not in the recycle bin: it was deleted outright")
	}
	cleanupTrashed(t, trashed)
}

// TestMoveCrossMountOverwriteSendsOldDestinationToTrash pins "overwrite =
// recycle bin, never delete" on the CROSS-MOUNT path — the one that matters,
// since /DATA↔/mnt is where ENOSPC and dropping network mounts happen, and the
// only one where the code has a reason to reach for RemoveAll (the source is
// still on disk, so deleting the target "looks" recoverable).
//
// The same-mount test above cannot catch a regression here: it never forces
// EXDEV, so a version that trashes on the rename path and RemoveAll's on the
// copy path passes it. A directory destination is used deliberately — the old
// tree must be REPLACED, not merged into, and it must arrive in the bin whole.
func TestMoveCrossMountOverwriteSendsOldDestinationToTrash(t *testing.T) {
	base := trashCapableDir(t)
	forceCrossMount(t)

	srcDir := filepath.Join(base, "src", "album")
	writeFileT(t, filepath.Join(srcDir, "cover.jpg"), "new cover")
	writeFileT(t, filepath.Join(srcDir, "track01.flac"), "new audio")

	destDir := filepath.Join(base, "dest")
	dst := filepath.Join(destDir, "album")
	writeFileT(t, filepath.Join(dst, "cover.jpg"), "ten years of photos")
	writeFileT(t, filepath.Join(dst, "old-only.txt"), "only in the old tree")

	if err := moveItem(context.Background(), srcDir, dst, int64(len("new cover")+len("new audio")), &file.ProgressCounter{}); err != nil {
		t.Fatalf("moveItem: %v", err)
	}

	if got := readFileT(t, filepath.Join(dst, "cover.jpg")); got != "new cover" {
		t.Fatalf("destination cover.jpg = %q, want the new revision", got)
	}
	if got := readFileT(t, filepath.Join(dst, "track01.flac")); got != "new audio" {
		t.Fatalf("destination track01.flac = %q, want the new revision", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, "old-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("old destination tree was merged into the new one instead of replaced: %v", err)
	}

	trashed := findTrashedTree(t, dst, "old-only.txt", "only in the old tree")
	if trashed == "" {
		t.Fatal("cross-mount overwrite destroyed the old destination instead of sending it to the recycle bin")
	}
	cleanupTrashed(t, trashed)
	if got := readFileT(t, filepath.Join(trashed, "cover.jpg")); got != "ten years of photos" {
		t.Fatalf("trashed cover.jpg = %q, want the old destination intact in the bin", got)
	}

	if _, err := os.Lstat(srcDir); !os.IsNotExist(err) {
		t.Fatalf("source still present after a successful move: %v", err)
	}
	if left := stageLeftovers(t, destDir); len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// TestMoveOverwriteRefusedWhenRecycleBinRejects pins the fail-closed half of
// the overwrite contract: if the old destination cannot be preserved, the move
// does not happen at all. Both copies must survive — the destination where it
// was, the source put back from the staging directory.
func TestMoveOverwriteRefusedWhenRecycleBinRejects(t *testing.T) {
	rejectTrash(t)

	base := t.TempDir()
	src := filepath.Join(base, "src", "report.txt")
	writeFileT(t, src, "new revision")
	destDir := filepath.Join(base, "dest")
	dst := filepath.Join(destDir, "report.txt")
	writeFileT(t, dst, "the only copy of last year")

	if err := moveItem(context.Background(), src, dst, int64(len("new revision")), &file.ProgressCounter{}); err == nil {
		t.Fatal("moveItem overwrote the destination although the recycle bin refused to take the old copy")
	}
	if got := readFileT(t, dst); got != "the only copy of last year" {
		t.Fatalf("destination content = %q, want it untouched", got)
	}
	if got := readFileT(t, src); got != "new revision" {
		t.Fatalf("source content = %q, want it restored from the staging directory", got)
	}
	if left := stageLeftovers(t, destDir); len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// TestMoveRenamePathSurvivesUndurableDirEntry: on the rename fast path the
// source is already unlinked and the payload is already at the destination when
// the final fsyncDir runs. Only the durability of the parent directory entry
// across a power cut is unproven, so failing the operation would report a
// COMPLETED move as failed; the user then retries, hits ENOENT on a source that
// no longer exists, and reads it as data loss. Success is the correct answer.
func TestMoveRenamePathSurvivesUndurableDirEntry(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src", "payload.bin")
	writeFileT(t, src, "the only copy")
	destDir := filepath.Join(base, "dest")
	dst := filepath.Join(destDir, "payload.bin")

	failFsyncDirOn(t, destDir)

	counter := &file.ProgressCounter{}
	size := int64(len("the only copy"))
	if err := moveItem(context.Background(), src, dst, size, counter); err != nil {
		t.Fatalf("rename-path move reported failure although the payload is at the destination and the source is gone: %v", err)
	}
	if got := readFileT(t, dst); got != "the only copy" {
		t.Fatalf("destination content = %q", got)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
	if counter.Done() != size {
		t.Fatalf("progress counter = %d, want %d", counter.Done(), size)
	}
	if left := stageLeftovers(t, destDir); len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// TestMoveCrossMountFailsClosedOnUndurableDirEntry is the other half: on the
// copy path the source is still on disk, so an unprovable directory entry costs
// the user nothing but a retry. Report it, and keep both copies.
func TestMoveCrossMountFailsClosedOnUndurableDirEntry(t *testing.T) {
	forceCrossMount(t)

	base := t.TempDir()
	src := filepath.Join(base, "src", "photo.jpg")
	writeFileT(t, src, "irreplaceable bytes")
	destDir := filepath.Join(base, "dest")
	dst := filepath.Join(destDir, "photo.jpg")

	failFsyncDirOn(t, destDir)

	if err := moveItem(context.Background(), src, dst, int64(len("irreplaceable bytes")), &file.ProgressCounter{}); err == nil {
		t.Fatal("cross-mount move reported success although the destination directory entry could not be made durable")
	}
	if got := readFileT(t, src); got != "irreplaceable bytes" {
		t.Fatalf("source content = %q, want it kept for the retry", got)
	}
	if got := readFileT(t, dst); got != "irreplaceable bytes" {
		t.Fatalf("destination content = %q, want the staged copy kept rather than rolled back", got)
	}
}

// TestMoveOntoItselfIsNoOp: the old code answered a move onto its own path by
// RemoveAll'ing the destination — which is the source — and then copying from
// a path that no longer existed. The item simply disappeared.
func TestMoveOntoItselfIsNoOp(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "keep.txt")
	writeFileT(t, src, "still here")

	if err := moveItem(context.Background(), src, src, 10, &file.ProgressCounter{}); err != nil {
		t.Fatalf("moveItem: %v", err)
	}
	if got := readFileT(t, src); got != "still here" {
		t.Fatalf("content = %q, want the file untouched", got)
	}
}

// TestMoveIntoOwnSubtreeRefused: copying a directory into itself walks its own
// output forever. Refuse before anything touches the source.
func TestMoveIntoOwnSubtreeRefused(t *testing.T) {
	base := t.TempDir()
	srcDir := filepath.Join(base, "album")
	writeFileT(t, filepath.Join(srcDir, "photo.jpg"), "bytes")

	dst := filepath.Join(srcDir, "sub", "album")
	if err := moveItem(context.Background(), srcDir, dst, 5, &file.ProgressCounter{}); !errors.Is(err, errDestInsideSource) {
		t.Fatalf("moveItem error = %v, want the destination-inside-source refusal", err)
	}
	if got := readFileT(t, filepath.Join(srcDir, "photo.jpg")); got != "bytes" {
		t.Fatalf("source content = %q, want it untouched", got)
	}
}
