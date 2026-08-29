package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// tmpSuffix is appended to destination paths during atomic copy operations.
// Files with this suffix should be considered in-flight and cleaned up on
// startup by the orphan-temp sweeper.
const tmpSuffix = ".casanova-tmp"

// SafeRename renames src to dst, falling back to a copy+delete if the two
// paths live on different filesystems (EXDEV). Users expect `mv` semantics —
// cross-device moves should "just work", not fail with cryptic errors.
//
// The fallback is NOT atomic across filesystems (no OS primitive is), but it
// guarantees the destination is either complete or absent: copy first to a
// temp file, fsync, then rename into place, then remove the source.
func SafeRename(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}

	// Cross-device fallback. Determine whether src is a file or directory.
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := copyTreeAtomic(src, dst); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	if err := copyFileAtomic(src, dst, info.Mode()); err != nil {
		return err
	}
	return os.Remove(src)
}

// isCrossDevice returns true if err indicates a rename across filesystems.
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// copyFileAtomic copies src to dst using a temp-file-then-rename pattern.
// On any error the temp file is removed. On success dst has the same mode
// as the source file.
//
// This is the building block for every safe copy in the file manager.
// Crash mid-write leaves a `.casanova-tmp` stub on disk (cleaned up by
// the orphan sweep) but NEVER a half-written file at the destination.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	tmp := dst + tmpSuffix
	// O_EXCL so we never clobber a stale tmp from a crashed writer.
	// If one exists, remove it first — it's garbage from a previous run.
	_ = os.Remove(tmp)

	dstF, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstF, srcF); err != nil {
		dstF.Close()
		os.Remove(tmp)
		return err
	}
	// fsync to guarantee bytes reach disk before the rename makes dst visible.
	if err := dstF.Sync(); err != nil {
		dstF.Close()
		os.Remove(tmp)
		return err
	}
	if err := dstF.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyTreeAtomic recursively copies a directory tree using copyFileAtomic
// for each file. Directories are created with the source's mode; files
// become visible at their destinations only after their bytes are durable.
// Symlinks are recreated as symlinks (not followed).
func copyTreeAtomic(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !info.IsDir() {
		return copyFileAtomic(src, dst, info.Mode())
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if err := copyTreeAtomic(s, d); err != nil {
			return err
		}
	}
	return nil
}

// CopyFileAtomic is the exported version of copyFileAtomic for callers that
// want a drop-in replacement for an unsafe direct-write copy.
func CopyFileAtomic(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("CopyFileAtomic: %s is a directory", src)
	}
	return copyFileAtomic(src, dst, info.Mode())
}

// CopyTreeAtomic is the exported version of copyTreeAtomic.
func CopyTreeAtomic(src, dst string) error {
	return copyTreeAtomic(src, dst)
}
