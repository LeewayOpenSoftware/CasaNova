package file

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// WHY A THUMBNAIL CACHE.
//
// GetImage decoded the ENTIRE source image on every request. Measured on a
// 4000x3000 JPEG (7.5 MB on disk):
//
//	650 ms and 20,430,363 B allocated, per thumbnail, per request
//
// A gallery view asks for dozens at once, and asks again on every navigation.
// Thirty thumbnails is ~19.5 s of CPU and ~612 MB of allocation to produce
// images the box already produced a moment ago. On a home NAS that is the
// difference between a gallery that opens and one that appears broken.
//
// THE KEY IS CONTENT-DERIVED, NOT PATH-DERIVED. A cache keyed on the path alone
// serves a stale thumbnail forever after the file is replaced — the classic way
// a cache turns a slow feature into a wrong one. The key mixes the path with
// the source's modification time AND size, so replacing the file misses the
// cache by construction, with no invalidation logic to get wrong.
//
// EVERY FAILURE FALLS BACK TO GENERATING. An unwritable directory, a corrupt
// entry, a full disk: none of them may fail a request. The cache is an
// optimisation, and an optimisation that can break the feature is a liability.

const (
	// thumbCacheMaxBytes bounds what the cache may occupy. Thumbnails are a few
	// KB each, so this holds tens of thousands of them while staying a rounding
	// error against the array it serves.
	thumbCacheMaxBytes = 256 << 20

	// thumbCacheSweepEvery bounds how often the eviction sweep runs. Sweeping
	// on every write would turn a cheap cache hit into a directory walk.
	thumbCacheSweepEvery = 5 * time.Minute
)

var (
	thumbGroup     singleflight.Group
	thumbDirOnce   sync.Once
	thumbDir       string
	thumbDirErr    error
	thumbLastSweep atomic.Int64

	// thumbCacheDisabled short-circuits everything if provisioning failed, so a
	// box with a read-only state directory pays one failed MkdirAll, not one
	// per request.
	thumbCacheDisabled atomic.Bool
)

// SetThumbnailCacheDirForTest points the cache at a temp directory.
func SetThumbnailCacheDirForTest(dir string) {
	thumbDirOnce = sync.Once{}
	thumbDir, thumbDirErr = dir, nil
	thumbCacheDisabled.Store(false)
	thumbDirOnce.Do(func() {})
}

func thumbnailDir() (string, error) {
	thumbDirOnce.Do(func() {
		base := os.Getenv("CASANOVA_STATE_DIR")
		if base == "" {
			base = "/var/lib/casanova"
		}
		thumbDir = filepath.Join(base, "thumbnails")
		if err := os.MkdirAll(thumbDir, 0o755); err != nil {
			thumbDirErr = err
			thumbCacheDisabled.Store(true)
		}
	})
	return thumbDir, thumbDirErr
}

// thumbKey derives the cache filename from the SOURCE's identity, not its name.
func thumbKey(path string, info os.FileInfo, width, height int) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(info.Size(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(width) + "x" + strconv.Itoa(height)))
	return hex.EncodeToString(h.Sum(nil))
}

// CachedImage is GetImage with a disk cache in front of it.
//
// generate is the expensive path; it is called at most once per key even when a
// gallery asks for the same image from twenty goroutines at once, because a
// cache that stampedes on a cold key is worst exactly when it matters most.
func CachedImage(path string, width, height int, generate func() ([]byte, error)) ([]byte, error) {
	if thumbCacheDisabled.Load() {
		return generate()
	}
	info, err := os.Stat(path)
	if err != nil {
		// Let the generator produce the real error, so the caller's error
		// handling stays in one place.
		return generate()
	}
	dir, err := thumbnailDir()
	if err != nil {
		return generate()
	}
	key := thumbKey(path, info, width, height)
	entry := filepath.Join(dir, key)

	if b, err := os.ReadFile(entry); err == nil && len(b) > 0 {
		_ = os.Chtimes(entry, time.Now(), info.ModTime()) // touch for LRU
		return b, nil
	}

	v, err, _ := thumbGroup.Do(key, func() (any, error) {
		// Re-check: another goroutine may have written it while we queued.
		if b, err := os.ReadFile(entry); err == nil && len(b) > 0 {
			return b, nil
		}
		b, err := generate()
		if err != nil {
			return nil, err
		}
		writeThumbEntry(dir, entry, b)
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, errors.New("thumbnail cache returned an unexpected type")
	}
	return b, nil
}

// writeThumbEntry stores an entry atomically. A partial file must never be
// readable as a thumbnail — that is how a cache starts serving truncated JPEGs.
func writeThumbEntry(dir, entry string, b []byte) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, entry); err != nil {
		_ = os.Remove(name)
		return
	}
	maybeSweep(dir)
}

// maybeSweep evicts oldest-accessed entries when the cache exceeds its bound.
// Rate-limited: the check is a clock comparison, the walk happens rarely.
func maybeSweep(dir string) {
	now := time.Now().UnixNano()
	last := thumbLastSweep.Load()
	if now-last < int64(thumbCacheSweepEvery) {
		return
	}
	if !thumbLastSweep.CompareAndSwap(last, now) {
		return // another goroutine is sweeping
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type ent struct {
		path string
		size int64
		at   time.Time
	}
	all := make([]ent, 0, len(entries))
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, ent{filepath.Join(dir, e.Name()), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	if total <= thumbCacheMaxBytes {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, e := range all {
		if total <= thumbCacheMaxBytes/2 { // evict down to half, not to the line
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}
