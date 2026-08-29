package file

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// tmpSuffix is appended to destination paths during atomic copy operations.
// Files with this suffix are in-flight and should be cleaned up on startup.
const tmpSuffix = ".casanova-tmp"

// WriteFileAtomic writes data to dst using a write-temp + fsync(file) +
// close + rename + fsync(parent dir) sequence. This is the only durable
// way to overwrite a file on Linux: a plain os.WriteFile leaves a window
// where the destination directory entry has been updated but the data
// blocks are still in the kernel page cache, so a power loss between
// the write() and the next periodic flush yields a zero-length file
// after the next boot.
//
// Wave 35-Q: every persistence call site (TLS state, OAuth client list,
// backup schedules, TOTP state, MessageBus stores.json, migration status)
// independently reinvented this pattern and got it subtly wrong. The
// only place that had it right was Gateway's savePathTargetMapTo. This
// helper centralizes the correct sequence so callers can stop reinventing
// it. Use it for any file under /etc/casanova or /var/lib/casanova that
// must survive an unclean shutdown.
//
// Crash semantics:
//   - Crash before rename: dst is unchanged, the .casanova-tmp stub is
//     orphaned (use SafeRename or call CleanupStaleTmp on startup).
//   - Crash after rename but before parent fsync: dst is the new content,
//     and most filesystems will commit the rename in the next journal
//     flush. The follow-up dir fsync makes that explicit.
//   - Crash anywhere: dst is either the old content or the new content,
//     never a half-written torn file.
func WriteFileAtomic(dst string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := dst + tmpSuffix
	_ = os.Remove(tmp) // clear stale stub from a previous crash
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the parent directory so the rename is durable. Best-effort —
	// some filesystems (procfs, tmpfs in tests) refuse to fsync a dir,
	// in which case we trust the rename has at least landed in the
	// journal queue and skip the explicit flush.
	if dirF, err := os.Open(dir); err == nil {
		_ = dirF.Sync()
		dirF.Close()
	}
	return nil
}

// SafeRename renames src to dst, falling back to a copy+delete across
// filesystems (EXDEV). The fallback is NOT atomic across devices but
// guarantees the destination is either complete or absent.
func SafeRename(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
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

// copyFileAtomic copies src to dst via temp file + rename + fsync.
// Crash mid-write leaves only a .casanova-tmp stub — never a torn file.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	tmp := dst + tmpSuffix
	_ = os.Remove(tmp) // clear any stale stub from a previous crash

	dstF, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstF, srcF); err != nil {
		dstF.Close()
		os.Remove(tmp)
		return err
	}
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

// copyTreeAtomic recursively copies a tree, preserving symlinks as symlinks.
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
