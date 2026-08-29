package file

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// ProgressCounter is an atomic byte counter shared between a copy routine
// and the code that reports progress to the user. All methods are safe
// to call from multiple goroutines.
//
// Usage:
//
//	var p ProgressCounter
//	go CopyDirWithProgress(src, dst, "", &p)
//	for {
//	    current := p.Done()
//	    // render progress bar
//	}
type ProgressCounter struct {
	done int64
}

// Add atomically adds n bytes to the counter.
func (p *ProgressCounter) Add(n int64) { atomic.AddInt64(&p.done, n) }

// Done returns the total bytes counted so far.
func (p *ProgressCounter) Done() int64 { return atomic.LoadInt64(&p.done) }

// Reset zeroes the counter. Not safe to call while a copy is in flight.
func (p *ProgressCounter) Reset() { atomic.StoreInt64(&p.done, 0) }

// progressWriter wraps an io.Writer and adds every successful byte written
// to the attached counter. This lets progress come from the actual copy
// loop instead of polling the destination file's size every few seconds
// (which jumps from 0 to 100% on fast files).
type progressWriter struct {
	w io.Writer
	p *ProgressCounter
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	if n > 0 && pw.p != nil {
		pw.p.Add(int64(n))
	}
	return n, err
}

// CopyFileWithProgress copies src to dst atomically via tmp+rename,
// tracking bytes written through the supplied counter. The counter may
// be nil, in which case this degrades to copyFileAtomic.
func CopyFileWithProgress(src, dst string, mode os.FileMode, p *ProgressCounter) error {
	if p == nil {
		return copyFileAtomic(src, dst, mode)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + tmpSuffix
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	pw := &progressWriter{w: out, p: p}
	if _, err := io.Copy(pw, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// CopyDirWithProgress recursively copies src into dst, tracking bytes
// through the supplied counter. Symlinks are preserved as symlinks.
// If p is nil this is equivalent to copyTreeAtomic.
func CopyDirWithProgress(src, dst string, p *ProgressCounter) error {
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
		return CopyFileWithProgress(src, dst, info.Mode(), p)
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
		if err := CopyDirWithProgress(s, d, p); err != nil {
			return err
		}
	}
	return nil
}
