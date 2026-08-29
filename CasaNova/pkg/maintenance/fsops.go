package maintenance

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Descent and removal (design rules 4 and 5).
//
// The rule this file exists to enforce: after the scan has looked at a path,
// the string of that path is never re-resolved by the kernel again. Removal
// starts at the allowed root, opens ONE component at a time with
// O_NOFOLLOW|O_DIRECTORY, and finishes with unlinkat(dirfd, base) — so a
// symlink swapped in between scan and run cannot redirect anything. There IS a
// symlink out of /DATA into /var/run on this box today
// (big-bear-wordpress/mysql/mysql.sock), and anyone with an SMB share can
// plant another.
//
// openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) would express the same thing in
// one syscall; the per-component openat loop is the fallback the design allows
// and works on every kernel we might be running under, including a rollback to
// an older one.

const (
	// openFlags is what every component BELOW a root is opened with. O_NOFOLLOW
	// is the whole point: a symlink in the middle of the path fails with ELOOP
	// instead of redirecting us.
	openFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

	// rootOpenFlags deliberately omits O_NOFOLLOW. The root is a compiled-in
	// constant, never attacker-supplied, and on a real install a storage root
	// CAN legitimately be a symlink — the documented /var/lib/casaos/files
	// fallback layout is exactly that. Refusing to open it would silently
	// disable a category on those boxes. Everything below the root, which is
	// where anyone with an SMB share can plant something, keeps O_NOFOLLOW.
	rootOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC

	// maxTreeDepth bounds tree expansion. An upgrade backup is 3 levels deep
	// and the journal is flat; anything past this is not a shape we recognise,
	// so we decline the whole tree instead of walking it.
	maxTreeDepth = 12
)

// planLimits are rule 17's hard caps. A plan that grows past them is a bug in
// a detector, not an opportunity, so the run aborts instead of trimming.
type planLimits struct {
	MaxEntries int
	MaxBytes   int64
	entries    int
	bytes      int64
}

func (l *planLimits) add(n int, b int64) error {
	l.entries += n
	l.bytes += b
	if l.MaxEntries > 0 && l.entries > l.MaxEntries {
		return fmt.Errorf("%w: %d entries", ErrPlanTooLarge, l.entries)
	}
	if l.MaxBytes > 0 && l.bytes > l.MaxBytes {
		return fmt.Errorf("%w: %d bytes", ErrPlanTooLarge, l.bytes)
	}
	return nil
}

func newPlanLimits() *planLimits {
	// Phase 1 reclaims ~2.2 GiB across a few thousand files. These caps are an
	// order of magnitude above that: loose enough never to bite a real run,
	// tight enough that a detector gone wrong stops before it does damage.
	return &planLimits{MaxEntries: 50000, MaxBytes: 16 << 30}
}

// openRoot opens an allowed root as a directory fd — the anchor every descent
// starts from. See rootOpenFlags for why this one open is allowed to follow.
func openRoot(root string) (int, error) {
	fd, err := unix.Open(root, rootOpenFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("open root %s: %w", root, err)
	}
	return fd, nil
}

// descend walks from an already-open root dirfd to the parent directory of the
// final component, opening one level at a time and never following a link.
// The returned fd belongs to the caller.
func (g *Guard) descend(rootFD int, root string, comps []string) (int, error) {
	var rootSt unix.Stat_t
	if err := unix.Fstat(rootFD, &rootSt); err != nil {
		return -1, err
	}

	cur := rootFD
	curDev := rootSt.Dev
	curPath := root
	owned := false // whether cur must be closed by us (the root fd is the caller's)

	for i := 0; i < len(comps)-1; i++ {
		name := comps[i]
		curPath = curPath + "/" + name

		// Rule 18: never step into a share that may never answer. Checked on
		// the reconstructed path, which is only ever used to REFUSE.
		if g.mounts()[curPath] {
			if owned {
				unix.Close(cur)
			}
			return -1, fmt.Errorf("%w: %s", ErrRemoteMount, curPath)
		}

		next, err := unix.Openat(cur, name, openFlags, 0)
		if owned {
			unix.Close(cur)
		}
		if err != nil {
			// ELOOP/ENOTDIR here means the component is a symlink or not a
			// directory — i.e. exactly the escape we are refusing.
			return -1, fmt.Errorf("descend %s: %w", curPath, err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil {
			unix.Close(next)
			return -1, err
		}
		// A different device below a root is a mount point. We only ever use
		// this to refuse (never to decide "same filesystem", which rule 15
		// forbids): bind mounts share st_dev, so this can miss one, but it can
		// never wrongly ALLOW anything.
		if st.Dev != curDev {
			unix.Close(next)
			return -1, fmt.Errorf("%w: %s", ErrCrossMount, curPath)
		}
		cur, curDev, owned = next, st.Dev, true
	}

	if !owned {
		// The target sits directly in the root; hand back a private dup so the
		// caller's Close never touches the root fd it does not own.
		dup, err := unix.Dup(cur)
		if err != nil {
			return -1, err
		}
		return dup, nil
	}
	return cur, nil
}

// parentFDFor resolves path down to (parent dirfd, base name) through the
// no-follow descent. The caller closes the returned fd.
func (g *Guard) parentFDFor(path string) (int, string, error) {
	root, err := g.Check(path)
	if err != nil {
		return -1, "", err
	}
	rel := strings.TrimPrefix(path, root+"/")
	comps := strings.Split(rel, "/")

	rootFD, err := openRoot(root)
	if err != nil {
		return -1, "", err
	}
	defer unix.Close(rootFD)

	parent, err := g.descend(rootFD, root, comps)
	if err != nil {
		return -1, "", err
	}
	return parent, comps[len(comps)-1], nil
}

// Remove is the ONLY place in this package that unlinks anything.
//
// Exported so it is findable, while `target` deliberately is not: no code
// outside this package can synthesize something for it to delete, which is the
// other half of "the client sends a category key and an opaque id, never a
// path".
//
// It re-proves everything the scan believed, immediately before acting
// (rule 5 — the TOCTOU window between scan and run is minutes, not
// microseconds):
//
//	identity  the (dev, ino) must be the one the scan recorded
//	kind      a file that became a symlink or a directory is refused
//	mtime     for files, the timestamp must be untouched since the scan
//	age       the entry must still be older than the category's cutoff
//	open fd   no process may hold the inode open (rule 6)
//
// Directories skip the mtime and age checks — removing their children rewrote
// their mtime — and are removed with rmdir, which refuses a non-empty
// directory on its own. That is why this function never needs a recursive
// delete: the plan already listed the leaves.
func (g *Guard) Remove(t target, fdx fdLookup) error {
	parent, base, err := g.parentFDFor(t.Path)
	if err != nil {
		return err
	}
	defer unix.Close(parent)

	var st unix.Stat_t
	if err := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("revalidate %s: %w", t.Path, err)
	}

	kind, ok := kindOf(&st)
	if !ok {
		return fmt.Errorf("%w: %s is not a file, symlink or directory", ErrChanged, t.Path)
	}
	if kind != t.Kind {
		return fmt.Errorf("%w: %s is now a %s, was a %s", ErrChanged, t.Path, kind, t.Kind)
	}
	if st.Dev != t.Dev || st.Ino != t.Ino {
		return fmt.Errorf("%w: %s", ErrIdentity, t.Path)
	}

	if t.Kind != KindDir {
		mtime := mtimeOf(&st)
		if !mtime.Equal(t.MTime) {
			return fmt.Errorf("%w: %s was modified after the scan", ErrChanged, t.Path)
		}
		if !t.Cutoff.IsZero() && !mtime.Before(t.Cutoff) {
			return fmt.Errorf("%w: %s is younger than the safety margin", ErrChanged, t.Path)
		}
		if fdx != nil {
			if holder, held := fdx.Holder(st.Dev, st.Ino); held {
				return fmt.Errorf("%w: %s (%s)", ErrOpenFD, t.Path, holder)
			}
		}
	}

	flags := 0
	if t.Kind == KindDir {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(parent, base, flags); err != nil {
		if err == unix.ENOTEMPTY || err == unix.EEXIST {
			return fmt.Errorf("%w: %s", ErrNotEmpty, t.Path)
		}
		return fmt.Errorf("remove %s: %w", t.Path, err)
	}
	return nil
}

// expandTree turns one path into the ordered list of entries that removing it
// implies: children before their parent, so an interruption leaves a partial
// but coherent state (rule 12).
//
// It walks with the same no-follow descent Remove uses, and refuses the WHOLE
// tree — never just the offending entry — when it meets something it does not
// understand: a device node, a socket, a mount point, a remote share, a
// lost+found or a trash directory, or more depth than we expect. A tree we
// cannot fully describe is a tree we do not delete.
func (g *Guard) expandTree(path string, cutoff time.Time, sz *sizer, lim *planLimits) ([]target, error) {
	parent, base, err := g.parentFDFor(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)

	var st unix.Stat_t
	if err := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	kind, ok := kindOf(&st)
	if !ok {
		return nil, fmt.Errorf("%s is not a file, symlink or directory", path)
	}
	if kind != KindDir {
		t := targetFrom(path, kind, &st, cutoff, sz)
		if err := lim.add(1, t.Bytes); err != nil {
			return nil, err
		}
		return []target{t}, nil
	}

	fd, err := unix.Openat(parent, base, openFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var out []target
	if err := g.walkDir(fd, path, st.Dev, 0, cutoff, sz, lim, &out); err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.Close(fd)

	// The container directory goes last. Its mtime is meaningless by then, so
	// targetFrom's cutoff is zeroed for directories.
	out = append(out, targetFrom(path, KindDir, &st, time.Time{}, sz))
	if err := lim.add(1, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// walkDir appends dir's contents leaves-first. fd stays owned by the caller.
func (g *Guard) walkDir(fd int, dirPath string, dirDev uint64, depth int, cutoff time.Time, sz *sizer, lim *planLimits, out *[]target) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("tree deeper than %d levels at %s", maxTreeDepth, dirPath)
	}

	names, err := readDirNames(fd)
	if err != nil {
		return fmt.Errorf("read %s: %w", dirPath, err)
	}

	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		if denyComponentNames[name] {
			return fmt.Errorf("%w: %s/%s", ErrDenied, dirPath, name)
		}
		childPath := dirPath + "/" + name
		if g.mounts()[childPath] {
			return fmt.Errorf("%w: %s", ErrRemoteMount, childPath)
		}

		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat %s: %w", childPath, err)
		}
		if st.Dev != dirDev {
			return fmt.Errorf("%w: %s", ErrCrossMount, childPath)
		}
		kind, ok := kindOf(&st)
		if !ok {
			return fmt.Errorf("%s is not a file, symlink or directory", childPath)
		}

		if kind == KindDir {
			child, err := unix.Openat(fd, name, openFlags, 0)
			if err != nil {
				return fmt.Errorf("open %s: %w", childPath, err)
			}
			err = g.walkDir(child, childPath, st.Dev, depth+1, cutoff, sz, lim, out)
			unix.Close(child)
			if err != nil {
				return err
			}
			*out = append(*out, targetFrom(childPath, KindDir, &st, time.Time{}, sz))
			if err := lim.add(1, 0); err != nil {
				return err
			}
			continue
		}

		t := targetFrom(childPath, kind, &st, cutoff, sz)
		*out = append(*out, t)
		if err := lim.add(1, t.Bytes); err != nil {
			return err
		}
	}
	return nil
}

// readDirNames lists a directory through an fd we already hold.
//
// Deliberately NOT os.NewFile(fd).Readdirnames: os.File takes ownership of the
// descriptor and installs a finalizer that closes it, so the GC could close a
// descriptor this walk is still descending through — and a closed fd number is
// immediately reusable by any other goroutine's open. getdents through the raw
// fd keeps ownership where it belongs. ParseDirent already drops "." and "..".
func readDirNames(fd int) ([]string, error) {
	buf := make([]byte, 8192)
	var names []string
	for {
		n, err := unix.ReadDirent(fd, buf)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}

// targetFrom snapshots what the scan saw, so Remove can prove nothing changed.
func targetFrom(path string, kind Kind, st *unix.Stat_t, cutoff time.Time, sz *sizer) target {
	t := target{
		Path:  path,
		Kind:  kind,
		Dev:   st.Dev,
		Ino:   st.Ino,
		MTime: mtimeOf(st),
	}
	if kind != KindDir {
		t.Cutoff = cutoff
	}
	if sz != nil {
		t.Bytes = sz.add(st)
	} else {
		t.Bytes = allocatedBytes(st)
	}
	return t
}

func kindOf(st *unix.Stat_t) (Kind, bool) {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return KindFile, true
	case unix.S_IFLNK:
		return KindSymlink, true
	case unix.S_IFDIR:
		return KindDir, true
	}
	return 0, false
}

func mtimeOf(st *unix.Stat_t) time.Time {
	return time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
}

func isRegular(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFREG }

func isDir(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFDIR }

// lstatNoFollow is the scan-side stat: it never follows the last component, so
// a symlink is reported as a symlink and never as whatever it points at.
func lstatNoFollow(path string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Lstat(path, &st)
	return st, err
}
