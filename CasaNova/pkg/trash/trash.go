// Package trash implements a recycle bin for CasaNova following the XDG Trash
// specification. Deleted files are moved (not copied) to a .Trash-casanova/
// directory on the SAME filesystem as the original file, which makes delete
// effectively free and makes the trash travel with its disk if unmounted.
//
// Layout per filesystem:
//
//	/mountpoint/.Trash-casanova/
//	├── files/              # actual trashed content
//	├── info/               # .trashinfo metadata (XDG format)
//
// Collision handling: if a file with the same name already exists in trash,
// we append " 2", " 3", etc. to the name (matching the XDG convention).
//
// This package is intentionally standalone — it only uses stdlib so it can
// be reused by other services (LocalStorage, etc.) without circular deps.
package trash

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// TrashDirName is the folder created on each mounted filesystem to hold
// deleted items. The ".Trash-casanova" name avoids clashing with user
// trash dirs like ".Trash-1000" that Linux desktops create.
const TrashDirName = ".Trash-casanova"

// DefaultRetention is how long trashed items are kept before auto-purge.
// Matches Windows/macOS default.
const DefaultRetention = 30 * 24 * time.Hour

// Entry describes a single item in the trash.
type Entry struct {
	// ID is the relative path inside the trash "files" dir.
	// Used to target this entry for restore or permanent delete.
	ID string `json:"id"`

	// TrashedName is the on-disk name inside trash (may differ from OriginalPath
	// basename if a collision happened, e.g. "photo.jpg" → "photo 2.jpg").
	TrashedName string `json:"trashed_name"`

	// OriginalPath is where the file used to live before deletion.
	// Restore brings it back here (if the parent still exists).
	OriginalPath string `json:"original_path"`

	// DeletionDate is when the file was moved to trash.
	DeletionDate time.Time `json:"deletion_date"`

	// Size in bytes. For directories, the recursive total.
	Size int64 `json:"size"`

	// IsDir is true if this entry is a directory.
	IsDir bool `json:"is_dir"`

	// trashRoot is the .Trash-casanova directory this entry lives in.
	// Needed internally for Restore/Delete operations.
	trashRoot string
}

// Manager handles trash operations across all mounted filesystems.
type Manager struct {
	// Retention is the max age for items in trash before auto-purge.
	// Zero means never auto-purge (items stay until Empty is called).
	Retention time.Duration

	// DestValidator gates where Restore may write. It receives the untrusted
	// OriginalPath from a .trashinfo file and must return the cleaned, safe
	// destination or an error. The main service wires it to the canonical path
	// sanitiser (blocklist + symlink resolution); pkg/trash stays stdlib-only
	// so it can be reused elsewhere. Restore refuses to run when this is nil.
	DestValidator func(originalPath string) (string, error)
}

// NewManager creates a trash manager with default retention (30 days).
func NewManager() *Manager {
	return &Manager{Retention: DefaultRetention}
}

// Trash moves a file or directory to the recycle bin on its own filesystem.
// Returns the trash Entry describing the moved item, or an error if the
// operation fails.
//
// This is a near-instant operation because we use rename() within the same
// filesystem — no data copy, no disk I/O beyond a few metadata updates.
func (m *Manager) Trash(srcPath string) (*Entry, error) {
	absPath, err := filepath.Abs(srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute path: %w", err)
	}

	srcInfo, err := os.Lstat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}

	// Find or create the trash directory on the same filesystem.
	trashRoot, err := ensureTrashDir(absPath)
	if err != nil {
		return nil, err
	}

	// Refuse to trash the trash directory itself (infinite recursion).
	if strings.HasPrefix(absPath, trashRoot) {
		return nil, errors.New("cannot trash items inside the trash directory")
	}

	filesDir := filepath.Join(trashRoot, "files")
	infoDir := filepath.Join(trashRoot, "info")

	// Find a collision-free name in the trash.
	baseName := filepath.Base(absPath)
	trashedName := pickUniqueName(filesDir, infoDir, baseName)

	// Order: metadata FIRST, then the rename. The two steps cannot be one
	// atomic operation, so a crash between them must land on the recoverable
	// side. The old order (rename, then info) parked the crash on the wrong
	// one: the file was already in files/ with no .trashinfo, so List —
	// which enumerates info/ — could never show it and Restore could never
	// find it. Invisible, unrestorable, and nothing ever said so. With info
	// first, the crash leaves a .trashinfo pointing at a file that never
	// moved: List detects exactly that (file missing from files/), skips it
	// and removes the stale info. One side is cleanable garbage; the other
	// was silent data loss.
	deletionTime := time.Now()
	infoContent := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		absPath,
		deletionTime.Format("2006-01-02T15:04:05"),
	)
	infoPath := filepath.Join(infoDir, trashedName+".trashinfo")
	if err := writeInfoDurably(infoPath, infoContent); err != nil {
		return nil, fmt.Errorf("write trashinfo: %w", err)
	}

	if trashCrashWindowHook != nil {
		trashCrashWindowHook()
	}

	// Move the file (rename on same filesystem = atomic, near-instant).
	destPath := filepath.Join(filesDir, trashedName)
	if err := os.Rename(absPath, destPath); err != nil {
		// Nothing moved: removing the just-written info restores the world
		// exactly as it was. (Under the old order this compensation was a
		// second rename that could itself fail, stranding the file.)
		_ = os.Remove(infoPath)
		// If the rename fails with EXDEV (cross-device), the caller
		// passed a path that isn't actually on this filesystem. That's
		// a bug in ensureTrashDir — report it clearly.
		if linkErr, ok := err.(*os.LinkError); ok {
			if errno, ok := linkErr.Err.(syscall.Errno); ok && errno == syscall.EXDEV {
				return nil, fmt.Errorf("trash: cross-device rename, filesystem detection failed for %s", absPath)
			}
		}
		return nil, fmt.Errorf("move to trash: %w", err)
	}

	return &Entry{
		ID:           trashedName,
		TrashedName:  trashedName,
		OriginalPath: absPath,
		DeletionDate: deletionTime,
		Size:         dirSize(destPath, srcInfo),
		IsDir:        srcInfo.IsDir(),
		trashRoot:    trashRoot,
	}, nil
}

// List returns all items currently in the trash, across every filesystem
// known to have a CasaNova trash directory. Results are sorted newest first.
//
// searchRoots is the list of paths to scan for trash directories. Typically
// this is the list of currently mounted filesystems from LocalStorage.
// Pass nil or empty to scan /proc/mounts automatically.
func (m *Manager) List(searchRoots []string) ([]Entry, error) {
	if len(searchRoots) == 0 {
		var err error
		searchRoots, err = discoverMountpoints()
		if err != nil {
			return nil, err
		}
	}

	var entries []Entry
	for _, root := range searchRoots {
		trashRoot := filepath.Join(root, TrashDirName)
		infoDir := filepath.Join(trashRoot, "info")

		if _, err := os.Stat(infoDir); os.IsNotExist(err) {
			continue // this filesystem has no trash yet
		}

		infoFiles, err := os.ReadDir(infoDir)
		if err != nil {
			continue
		}

		for _, f := range infoFiles {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".trashinfo") {
				continue
			}
			entry, err := readEntry(trashRoot, f.Name())
			if err != nil {
				continue // skip malformed entries
			}
			// A .trashinfo whose file never arrived is the residue of a
			// crash between metadata and rename (metadata is written
			// first, on purpose — see MoveToTrash). The original file
			// never moved, so this record describes nothing: showing it
			// would offer a Restore that must fail. Clean it and move on.
			if _, err := os.Lstat(filepath.Join(trashRoot, "files", entry.TrashedName)); err != nil {
				_ = os.Remove(filepath.Join(infoDir, f.Name()))
				continue
			}
			entries = append(entries, *entry)
		}
	}

	// Sort newest first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].DeletionDate.After(entries[j].DeletionDate)
	})

	return entries, nil
}

// Restore moves a trashed item back to its original location. If the parent
// directory of the original path no longer exists, it's recreated. If a file
// already exists at the destination, Restore fails rather than overwriting.
//
// SECURITY: the destination comes from OriginalPath, which is read verbatim
// from the .trashinfo file — attacker-controlled input. This product runs
// arbitrary containers, and any container with a bind mount of a filesystem
// that has a trash dir (e.g. /DATA) can plant an info file whose Path= points
// at /etc/cron.d/… and then a single restore lands a root-owned file there:
// container→host RCE, straight past the blocklist that guards the JWT key and
// the user DB. So Restore refuses to write anywhere the DestValidator does not
// bless, and — enterprise default — refuses entirely if no validator is set: a
// Manager nobody configured must not be a write-anywhere-as-root primitive.
func (m *Manager) Restore(entry *Entry) error {
	if entry.trashRoot == "" {
		return errors.New("entry has no trash root — was it loaded via List?")
	}

	if m.DestValidator == nil {
		return errors.New("trash restore refused: no destination validator configured")
	}
	dst, err := m.DestValidator(entry.OriginalPath)
	if err != nil {
		return fmt.Errorf("refusing to restore to an unsafe destination: %w", err)
	}

	src := filepath.Join(entry.trashRoot, "files", entry.TrashedName)

	// Refuse to overwrite existing files at the destination.
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("cannot restore: %s already exists", dst)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	if err := safeRename(src, dst); err != nil {
		return fmt.Errorf("restore rename: %w", err)
	}

	// Remove the .trashinfo metadata (the file is no longer in trash).
	infoPath := filepath.Join(entry.trashRoot, "info", entry.TrashedName+".trashinfo")
	_ = os.Remove(infoPath)

	return nil
}

// Delete permanently removes a single trashed item. No recovery after this.
func (m *Manager) Delete(entry *Entry) error {
	if entry.trashRoot == "" {
		return errors.New("entry has no trash root")
	}
	filePath := filepath.Join(entry.trashRoot, "files", entry.TrashedName)
	infoPath := filepath.Join(entry.trashRoot, "info", entry.TrashedName+".trashinfo")

	if err := os.RemoveAll(filePath); err != nil {
		return err
	}
	return os.Remove(infoPath)
}

// Empty permanently deletes everything in every trash on every filesystem.
// Returns the total count of items removed.
func (m *Manager) Empty(searchRoots []string) (int, error) {
	entries, err := m.List(searchRoots)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range entries {
		if err := m.Delete(&entries[i]); err == nil {
			count++
		}
	}
	return count, nil
}

// Purge auto-removes trash entries older than m.Retention.
// Returns the count of items purged. Call periodically from a background
// goroutine (typically once per day).
func (m *Manager) Purge(searchRoots []string) (int, error) {
	if m.Retention <= 0 {
		return 0, nil // retention disabled
	}

	entries, err := m.List(searchRoots)
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-m.Retention)
	count := 0
	for i := range entries {
		if entries[i].DeletionDate.Before(cutoff) {
			if err := m.Delete(&entries[i]); err == nil {
				count++
			}
		}
	}
	return count, nil
}

// ---- internal helpers ----

// ensureTrashDir finds the mount point containing filePath and creates
// the trash directory structure there. Returns the path to .Trash-casanova/.
func ensureTrashDir(filePath string) (string, error) {
	mountPoint, err := findMountpointFn(filePath)
	if err != nil {
		return "", err
	}

	trashRoot := filepath.Join(mountPoint, TrashDirName)
	filesDir := filepath.Join(trashRoot, "files")
	infoDir := filepath.Join(trashRoot, "info")

	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("create trash dir %s: %w", d, err)
		}
	}

	return trashRoot, nil
}

// mountinfoPath is the kernel's view of this process's mount table. It is a
// variable so the parser can be tested against a fixture.
var mountinfoPath = "/proc/self/mountinfo"

// mountPointOf returns the longest mount point in /proc/self/mountinfo that
// contains filePath.
//
// This exists because comparing st_dev cannot see a BIND MOUNT: a bind mount
// shares its superblock with the original, so parent and child report the same
// device — while rename(2) still fails with EXDEV across the boundary, because
// EXDEV is about mounts, not filesystems. That is not a corner case here: every
// CasaNova unit runs with `ReadWritePaths=... /DATA /mnt /media ...`, and
// systemd implements those by bind-mounting each path into the service's mount
// namespace. So the st_dev walk placed the trash at "/" while the file lived
// under the /DATA bind mount, every rename returned EXDEV, and deleting a file
// from the web UI failed with "cross-device rename" on the box's MAIN storage.
func mountPointOf(filePath string) (string, bool) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	best := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// mountinfo: id parent major:minor root MOUNTPOINT options...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountinfo(fields[4])
		if !pathWithinMount(filePath, mp) {
			continue
		}
		// Longest match wins: /DATA beats / for a file under /DATA.
		if len(mp) > len(best) {
			best = mp
		}
	}
	if scanner.Err() != nil || best == "" {
		return "", false
	}
	return best, true
}

// pathWithinMount reports whether p sits at or under the mount point mp.
func pathWithinMount(p, mp string) bool {
	if mp == "/" {
		return true
	}
	return p == mp || strings.HasPrefix(p, mp+string(filepath.Separator))
}

// unescapeMountinfo decodes the octal escapes the kernel writes for space,
// tab, newline and backslash in mountinfo fields.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// findMountpoint locates the mount that holds filePath, so the trash directory
// lands on the same mount and the move stays an atomic rename.
//
// The mount table is authoritative and is consulted first; the st_dev walk
// below is kept as a fallback for systems without /proc (and is what the
// XDG-style heuristic always did).
func findMountpoint(filePath string) (string, error) {
	if mp, ok := mountPointOf(filePath); ok {
		return mp, nil
	}

	origStat, err := os.Lstat(filePath)
	if err != nil {
		return "", err
	}
	origDev := fsDev(origStat)

	current := filepath.Dir(filePath)
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root "/"
			return current, nil
		}
		parentStat, err := os.Lstat(parent)
		if err != nil {
			return current, nil
		}
		if fsDev(parentStat) != origDev {
			// parent is on a different filesystem → current is the mount point
			return current, nil
		}
		current = parent
	}
}

// fsDev extracts the filesystem device ID from a FileInfo (Linux only).
// On non-Linux, returns 0 which degrades the algorithm but doesn't crash.
func fsDev(info os.FileInfo) uint64 {
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev)
	}
	return 0
}

// pickUniqueName returns a trash filename that doesn't collide with an
// existing entry. On collision, appends " 2", " 3", etc. (XDG convention).
func pickUniqueName(filesDir, infoDir, baseName string) string {
	candidate := baseName
	for i := 2; ; i++ {
		filePath := filepath.Join(filesDir, candidate)
		infoPath := filepath.Join(infoDir, candidate+".trashinfo")
		_, fileErr := os.Lstat(filePath)
		_, infoErr := os.Lstat(infoPath)
		if os.IsNotExist(fileErr) && os.IsNotExist(infoErr) {
			return candidate
		}
		// Insert counter before the extension: "foo.txt" → "foo 2.txt"
		ext := filepath.Ext(baseName)
		stem := strings.TrimSuffix(baseName, ext)
		candidate = fmt.Sprintf("%s %d%s", stem, i, ext)
	}
}

// readEntry parses a .trashinfo file and returns the Entry.
func readEntry(trashRoot, infoFileName string) (*Entry, error) {
	infoPath := filepath.Join(trashRoot, "info", infoFileName)
	f, err := os.Open(infoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entry := &Entry{
		trashRoot:   trashRoot,
		TrashedName: strings.TrimSuffix(infoFileName, ".trashinfo"),
	}
	entry.ID = entry.TrashedName

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Path=") {
			entry.OriginalPath = strings.TrimPrefix(line, "Path=")
		} else if strings.HasPrefix(line, "DeletionDate=") {
			t, err := time.Parse("2006-01-02T15:04:05", strings.TrimPrefix(line, "DeletionDate="))
			if err == nil {
				entry.DeletionDate = t
			}
		}
	}

	// Stat the actual file in trash to get its size and whether it's a dir.
	filePath := filepath.Join(trashRoot, "files", entry.TrashedName)
	if stat, err := os.Lstat(filePath); err == nil {
		entry.IsDir = stat.IsDir()
		entry.Size = dirSize(filePath, stat)
	}

	return entry, nil
}

// dirSize returns the total size of a file or directory tree in bytes.
func dirSize(path string, info os.FileInfo) int64 {
	if !info.IsDir() {
		return info.Size()
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, f os.FileInfo, err error) error {
		if err == nil && f != nil && !f.IsDir() {
			total += f.Size()
		}
		return nil
	})
	return total
}

// discoverMountpoints reads /proc/mounts to find all real filesystems
// that might contain a CasaNova trash. Filters out pseudo-filesystems
// (proc, sysfs, devtmpfs, etc.) that would never hold user data.
func discoverMountpoints() ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ignoredTypes := map[string]bool{
		"proc": true, "sysfs": true, "devtmpfs": true, "tmpfs": true,
		"cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
		"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true,
		"bpf": true, "autofs": true, "fusectl": true, "hugetlbfs": true,
		"configfs": true, "rpc_pipefs": true, "binfmt_misc": true,
	}

	var mountpoints []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mountPoint := fields[1]
		fsType := fields[2]
		if ignoredTypes[fsType] {
			continue
		}
		mountpoints = append(mountpoints, mountPoint)
	}
	return mountpoints, scanner.Err()
}

// trashCrashWindowHook runs, when non-nil, in the gap between writing the
// .trashinfo and renaming the file — the window a crash-consistency test
// needs to stand in. Nil in production; the compiler erases the branch.
var trashCrashWindowHook func()

// findMountpointFn is the mountpoint resolver, a variable so tests can pin
// the trash root to a TempDir instead of the real filesystem's mounts.
var findMountpointFn = findMountpoint

// writeInfoDurably writes the .trashinfo with an fsync before returning:
// the metadata is the recovery record, and rename ordering means nothing if
// the record itself can evaporate in the crash it exists to survive.
func writeInfoDurably(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
