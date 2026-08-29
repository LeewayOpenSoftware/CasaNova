//go:build linux

package storageauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// TOCTOU: the window between deciding and doing.
//
// AuthorizeStorage resolves symlinks and answers a question about a path. Then
// the caller opens that path — and between those two moments an attacker who
// can write inside the storage area can replace an ancestor directory with a
// symlink. The decision was correct when it was made and wrong by the time it
// was used. filepath.Clean and strings.HasPrefix cannot close this: they reason
// about a string, and the attack changes the filesystem underneath it.
//
// openat2(2) with RESOLVE_BENEATH closes it in the kernel. The resolution
// happens once, atomically, relative to a directory descriptor, and any attempt
// to escape that descriptor — through a symlink, through "..", through an
// absolute path — fails instead of succeeding somewhere else. There is no
// window because there is no second resolution.
//
// The descriptor is the NARROWEST boundary that authorized the request:
// the principal's grant when one applied, otherwise the storage root. Confining
// to the ceiling when a narrower grant decided would leave a swap free to move
// the target across users while staying inside authorized storage.
//
// WHAT THIS DOES NOT CLAIM
//
// It confines resolution, not the file. A symlink that stays inside the
// boundary still resolves, which is correct: the authority already decided the
// whole subtree is this principal's to use. And a rename of the final component
// between open and read is not a path-resolution problem — the descriptor
// already points at the inode the open selected.

// ErrConfinementUnavailable reports that the kernel cannot confine path
// resolution. Callers fall back to the unconfined path, which is the behaviour
// that existed before this file — never something weaker.
var ErrConfinementUnavailable = errors.New("kernel path confinement (openat2 RESOLVE_BENEATH) is unavailable")

var (
	confinementOnce sync.Once
	confinementOK   bool
	confinementWhy  string
)

// ConfinementAvailable reports whether the kernel supports confined resolution,
// and why not when it does not. Probed once, against the real syscall rather
// than a kernel version string: a container runtime can block openat2 with
// seccomp on a kernel new enough to have it, and a version check would then
// promise a guarantee that does not hold.
func ConfinementAvailable() (bool, string) {
	confinementOnce.Do(func() {
		dir, err := os.Open("/")
		if err != nil {
			confinementWhy = "cannot open a directory to probe with: " + err.Error()
			return
		}
		defer dir.Close()

		how := &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
		}
		fd, err := unix.Openat2(int(dir.Fd()), ".", how)
		switch {
		case err == nil:
			unix.Close(fd)
			confinementOK = true
		case errors.Is(err, unix.ENOSYS):
			confinementWhy = "openat2 is not implemented by this kernel (needs 5.6 or newer)"
		case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
			confinementWhy = "openat2 is blocked, most likely by a seccomp profile"
		case errors.Is(err, unix.EINVAL):
			confinementWhy = "this kernel's openat2 does not support RESOLVE_BENEATH"
		default:
			confinementWhy = "openat2 probe failed: " + err.Error()
		}
	})
	return confinementOK, confinementWhy
}

// OpenAuthorized authorizes an operation and opens the target with the kernel
// confining resolution to the boundary that authorized it.
//
// This is the primitive callers should use wherever the gap between deciding
// and acting is exploitable — anything that opens, creates, truncates or
// replaces a file at a caller-influenced path.
//
// On a kernel without confinement it returns a file opened the ordinary way
// together with ErrConfinementUnavailable, so a caller that requires the
// guarantee can refuse while one that merely prefers it can continue. That is
// deliberate: refusing every file operation on an older kernel would be an
// outage, and the fallback is exactly the behaviour that existed before this
// file — not something weaker. Health reports which mode is in force.
func OpenAuthorized(p Principal, op Op, path string, flags int, perm os.FileMode) (*os.File, Decision, error) {
	d := AuthorizeStorage(p, op, path)
	if !d.Allowed {
		return nil, d, d
	}

	f, err := openConfined(d, flags, perm)
	if err != nil {
		return nil, d, err
	}
	return f, d, nil
}

// OpenAuthorizedSystem is OpenAuthorized for paths with no requesting
// principal: startup recovery, cleanup, rehydration. It applies the ceiling
// only, exactly like AuthorizeSystem, and confines resolution the same way.
func OpenAuthorizedSystem(op Op, path string, flags int, perm os.FileMode) (*os.File, Decision, error) {
	d := AuthorizeSystem(op, path)
	if !d.Allowed {
		return nil, d, d
	}
	f, err := openConfined(d, flags, perm)
	if err != nil {
		return nil, d, err
	}
	return f, d, nil
}

func openConfined(d Decision, flags int, perm os.FileMode) (*os.File, error) {
	target := d.Resolved
	if target == "" {
		target = d.Path
	}
	root := d.ConfineRoot

	ok, why := ConfinementAvailable()
	if !ok || root == "" {
		f, err := os.OpenFile(d.Path, flags, perm)
		if err != nil {
			return nil, err
		}
		if root == "" {
			// A decision with no boundary should not exist; treat it as a
			// missing guarantee rather than silently pretending to have one.
			return f, fmt.Errorf("%w: the decision named no confinement boundary", ErrConfinementUnavailable)
		}
		return f, fmt.Errorf("%w: %s", ErrConfinementUnavailable, why)
	}

	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Cannot express the target beneath its own boundary. Refuse rather
		// than fall back: this is not "the kernel cannot help", it is "the
		// decision and the target disagree", which must never open anything.
		return nil, fmt.Errorf("authorized path %s is not beneath its boundary %s", target, root)
	}
	if rel == "." {
		rel = "."
	}

	// O_PATH: we only need the descriptor as a resolution anchor, so do not
	// require read permission on the directory itself.
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open confinement boundary %s: %w", root, err)
	}
	defer unix.Close(rootFD)

	how := &unix.OpenHow{
		Flags: uint64(flags) | unix.O_CLOEXEC,
		Mode:  uint64(syscallMode(perm)),
		// RESOLVE_BENEATH: no component may escape rootFD — not through a
		// symlink, not through "..", not through an absolute path. This is the
		// whole guarantee, and it is enforced during resolution rather than
		// checked before it.
		//
		// RESOLVE_NO_MAGICLINKS: /proc/*/fd/* and friends are not ordinary
		// symlinks and would otherwise reopen an arbitrary descriptor.
		//
		// Belt-and-braces, stated honestly: RESOLVE_BENEATH already refuses
		// these in the shape we can reach, because a magic link resolves to an
		// absolute path outside the boundary. Removing this flag does NOT fail
		// TestMagicLinksAreRefused — verified by mutation. It is kept because
		// it costs nothing and covers the case where a boundary one day
		// contains /proc itself, not because a test proves it load-bearing.
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	}

	fd, err := unix.Openat2(rootFD, rel, how)
	if err != nil {
		// EXDEV / ELOOP here means the kernel refused an escape — the very
		// race this exists to stop. Name it, so an operator reading a log sees
		// an attack and not a puzzling I/O error.
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("path resolution escaped its authorized boundary %s "+
				"(the target changed between authorization and open): %w", root, err)
		}
		return nil, &os.PathError{Op: "openat2", Path: target, Err: err}
	}
	return os.NewFile(uintptr(fd), target), nil
}

// syscallMode mirrors os.syscallMode, which is unexported.
func syscallMode(perm os.FileMode) uint32 {
	m := uint32(perm.Perm())
	if perm&os.ModeSetuid != 0 {
		m |= syscall.S_ISUID
	}
	if perm&os.ModeSetgid != 0 {
		m |= syscall.S_ISGID
	}
	if perm&os.ModeSticky != 0 {
		m |= syscall.S_ISVTX
	}
	return m
}
