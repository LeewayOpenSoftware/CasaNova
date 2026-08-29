//go:build linux

package storageauth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The descriptor-relative toolkit.
//
// OpenAuthorized closes the race for one operation. It is not enough on its
// own: a handler that authorizes a path and then calls os.Remove, os.Rename or
// os.MkdirAll has the same window, because each of those resolves the path
// again from a string. The contract has to be the same for every privileged
// filesystem effect:
//
//	policy decision -> narrowest authorized boundary -> descriptor-relative syscall
//
// Only the operations real callers need are here. Symmetry is not a reason to
// add a wrapper; each function below exists because a production handler in
// this tree performs that effect on a caller-influenced path.
//
// STRICT MODE
//
// Every function in this file REQUIRES kernel confinement. LEG-001's whole
// claim is that a stale authorization cannot be turned into an escape, and a
// handler that silently continues without the guarantee would be claiming
// protection it does not have. When the kernel cannot confine, these refuse
// with ErrConfinementUnavailable and the caller returns a structured error.
// The permissive variant (OpenAuthorized, which returns the file alongside the
// error) stays available for paths explicitly classified as non-security-
// sensitive.

// boundary resolves a decision into (root descriptor, path relative to it).
// The caller must close the returned descriptor.
func boundary(d Decision) (int, string, error) {
	if ok, why := ConfinementAvailable(); !ok {
		return -1, "", fmt.Errorf("%w: %s", ErrConfinementUnavailable, why)
	}
	root := d.ConfineRoot
	if root == "" {
		return -1, "", fmt.Errorf("%w: the decision named no confinement boundary", ErrConfinementUnavailable)
	}
	target := d.Resolved
	if target == "" {
		target = d.Path
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return -1, "", fmt.Errorf("authorized path %s is not beneath its boundary %s", target, root)
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open confinement boundary %s: %w", root, err)
	}
	return fd, rel, nil
}

// escaped names the kernel's refusal for what it is, so a log reads as an
// attack rather than a puzzling I/O error.
func escaped(root string, err error) error {
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("path resolution escaped its authorized boundary %s "+
			"(the target changed between authorization and the operation): %w", root, err)
	}
	return err
}

// OpenAuthorizedStrict is OpenAuthorized without the permissive fallback: if
// the kernel cannot confine resolution, the operation is refused rather than
// performed with a weaker guarantee. Security-sensitive callers use this.
func OpenAuthorizedStrict(p Principal, op Op, path string, flags int, perm os.FileMode) (*os.File, Decision, error) {
	d := AuthorizeStorage(p, op, path)
	if !d.Allowed {
		return nil, d, d
	}
	f, err := openStrict(d, flags, perm)
	return f, d, err
}

// OpenSystemStrict is the same for paths with no requesting principal:
// startup, recovery, cleanup, rehydration.
func OpenSystemStrict(op Op, path string, flags int, perm os.FileMode) (*os.File, Decision, error) {
	d := AuthorizeSystem(op, path)
	if !d.Allowed {
		return nil, d, d
	}
	f, err := openStrict(d, flags, perm)
	return f, d, err
}

func openStrict(d Decision, flags int, perm os.FileMode) (*os.File, error) {
	rootFD, rel, err := boundary(d)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)

	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC,
		Mode:    uint64(syscallMode(perm)),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFD, rel, how)
	if err != nil {
		return nil, escaped(d.ConfineRoot, &os.PathError{Op: "openat2", Path: d.Path, Err: err})
	}
	return os.NewFile(uintptr(fd), d.Path), nil
}

// ReadFileAuthorized reads a whole file through the confined open.
func ReadFileAuthorized(p Principal, path string) ([]byte, Decision, error) {
	f, d, err := OpenAuthorizedStrict(p, OpRead, path, os.O_RDONLY, 0)
	if err != nil {
		return nil, d, err
	}
	defer f.Close()
	b, rerr := io.ReadAll(f)
	return b, d, rerr
}

// StatAuthorized stats a path without a second string resolution.
func StatAuthorized(p Principal, op Op, path string) (os.FileInfo, Decision, error) {
	f, d, err := OpenAuthorizedStrict(p, op, path, os.O_RDONLY|unix.O_PATH, 0)
	if err != nil {
		return nil, d, err
	}
	defer f.Close()
	fi, serr := f.Stat()
	return fi, d, serr
}

// RemoveAuthorized removes a single entry — file, empty directory or symlink —
// relative to the boundary that authorized it.
//
// unlinkat, not os.Remove: os.Remove resolves the string again, which is the
// window. The final component is never followed, so removing a symlink removes
// the link and not its target.
func RemoveAuthorized(p Principal, path string) (Decision, error) {
	d := AuthorizeStorage(p, OpDelete, path)
	if !d.Allowed {
		return d, d
	}
	return d, removeAt(d)
}

// RemoveSystemAuthorized is RemoveAuthorized for cleanup and recovery paths,
// which have no principal.
func RemoveSystemAuthorized(path string) (Decision, error) {
	d := AuthorizeSystem(OpDelete, path)
	if !d.Allowed {
		return d, d
	}
	return d, removeAt(d)
}

func removeAt(d Decision) error {
	rootFD, rel, err := boundary(d)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)

	if rel == "." {
		return fmt.Errorf("refusing to remove the confinement boundary itself (%s)", d.ConfineRoot)
	}
	if err := unix.Unlinkat(rootFD, rel, 0); err != nil {
		if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
			if derr := unix.Unlinkat(rootFD, rel, unix.AT_REMOVEDIR); derr == nil {
				return nil
			}
		}
		return escaped(d.ConfineRoot, &os.PathError{Op: "unlinkat", Path: d.Path, Err: err})
	}
	return nil
}

// RemoveAllAuthorized removes a tree, keeping every step beneath the boundary.
//
// The traversal opens each directory relative to the previous descriptor, so a
// swap partway through cannot redirect the recursion outside — the failure
// mode os.RemoveAll has when an attacker can write inside the tree.
func RemoveAllAuthorized(p Principal, path string) (Decision, error) {
	d := AuthorizeStorage(p, OpDelete, path)
	if !d.Allowed {
		return d, d
	}
	rootFD, rel, err := boundary(d)
	if err != nil {
		return d, err
	}
	defer unix.Close(rootFD)
	if rel == "." {
		return d, fmt.Errorf("refusing to remove the confinement boundary itself (%s)", d.ConfineRoot)
	}
	return d, removeAllAt(rootFD, rel, d.ConfineRoot, 0)
}

// maxRemoveDepth bounds the recursion. A directory tree deep enough to exhaust
// descriptors is either a mistake or an attack; either way the operation stops
// rather than taking the process with it.
const maxRemoveDepth = 128

func removeAllAt(dirFD int, name, root string, depth int) error {
	if depth > maxRemoveDepth {
		return fmt.Errorf("refusing to recurse past %d levels below %s", maxRemoveDepth, root)
	}
	if err := unix.Unlinkat(dirFD, name, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return escaped(root, err)
	}

	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	}
	childFD, err := unix.Openat2(dirFD, name, how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return escaped(root, err)
	}

	dir := os.NewFile(uintptr(childFD), name)
	entries, rerr := dir.Readdirnames(-1)
	if rerr != nil && rerr != io.EOF {
		dir.Close()
		return rerr
	}
	for _, e := range entries {
		if err := removeAllAt(childFD, e, root, depth+1); err != nil {
			dir.Close()
			return err
		}
	}
	dir.Close()

	if err := unix.Unlinkat(dirFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return escaped(root, err)
	}
	return nil
}

// RenameAuthorized moves an entry, validating and confining BOTH endpoints.
//
// Validating only the source is how a move becomes an escape: the destination
// is a separate object with its own policy, and it is the side that decides
// where the bytes end up. Both are resolved relative to their own boundary, so
// neither can be swapped between decision and syscall.
func RenameAuthorized(p Principal, from, to string) (Decision, Decision, error) {
	src := AuthorizeStorage(p, OpRename, from)
	if !src.Allowed {
		return src, Decision{}, src
	}
	dst := AuthorizeStorage(p, OpCreate|OpWrite, to)
	if !dst.Allowed {
		return src, dst, dst
	}

	srcFD, srcRel, err := boundary(src)
	if err != nil {
		return src, dst, err
	}
	defer unix.Close(srcFD)

	dstFD, dstRel, err := boundary(dst)
	if err != nil {
		return src, dst, err
	}
	defer unix.Close(dstFD)

	if err := unix.Renameat(srcFD, srcRel, dstFD, dstRel); err != nil {
		if errors.Is(err, unix.EXDEV) {
			// Genuine cross-device move, not an escape: the caller must copy.
			// Distinguished by name so it is not mistaken for a refusal.
			return src, dst, fmt.Errorf("rename crosses a filesystem boundary; copy and delete instead: %w", err)
		}
		return src, dst, escaped(src.ConfineRoot, &os.PathError{Op: "renameat", Path: from, Err: err})
	}
	return src, dst, nil
}

// MkdirAllAuthorized creates a directory and any missing parents, each step
// relative to the previous descriptor so the chain cannot be redirected.
func MkdirAllAuthorized(p Principal, path string, perm os.FileMode) (Decision, error) {
	d := AuthorizeStorage(p, OpCreate, path)
	if !d.Allowed {
		return d, d
	}
	rootFD, rel, err := boundary(d)
	if err != nil {
		return d, err
	}
	defer unix.Close(rootFD)
	if rel == "." {
		return d, nil // the boundary itself already exists
	}

	cur := rootFD
	var toClose []int
	defer func() {
		for _, fd := range toClose {
			unix.Close(fd)
		}
	}()

	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if err := unix.Mkdirat(cur, part, uint32(perm.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
			return d, escaped(d.ConfineRoot, &os.PathError{Op: "mkdirat", Path: path, Err: err})
		}
		if i == len(parts)-1 {
			break
		}
		how := &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
		}
		next, err := unix.Openat2(cur, part, how)
		if err != nil {
			return d, escaped(d.ConfineRoot, &os.PathError{Op: "openat2", Path: path, Err: err})
		}
		toClose = append(toClose, next)
		cur = next
	}
	return d, nil
}

// WalkRootAuthorized returns a directory handle to traverse beneath, so a
// recursive read (folder size, search, listing) establishes its root ONCE and
// keeps every step below it.
//
// Callers must traverse with the returned *os.File as the anchor — using its
// Name() to rebuild strings and calling filepath.Walk would put the race back.
func WalkRootAuthorized(p Principal, path string) (*os.File, Decision, error) {
	return OpenAuthorizedStrict(p, OpRead, path, os.O_RDONLY|unix.O_DIRECTORY, 0)
}
