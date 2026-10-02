package utils

import (
	"errors"
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"
)

// BlockedSystemPrefixes is the single canonical list of host system
// directories the file API must never read, write, download, or delete.
// /var is the load-bearing entry: it holds the JWT ECDSA signing key
// (/var/lib/casaos/db/jwt_ec_private.pem) and the user DB — leaking it is
// full root. SanitizeFilePath, the v3 InitFile/InitDir download handlers, and
// the v2 upload blocklist all reference THIS list so the three can't drift
// (that drift is exactly how /var went unblocked on the read side).
// Boundary-safe matching at call sites (exact dir OR prefix+"/") keeps legit
// paths like "/etcd-data" from being caught by "/etc".
var BlockedSystemPrefixes = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot",
	"/dev", "/proc", "/sys", "/root", "/run", "/var", "/opt", "/srv",
}

// DataRoots lists the storage areas the file API exists to serve. The same
// three roots confine the search endpoints (route/casanova_search.go and
// route/v1/file.go::GetFileSearch); this copy is the one the resolved-path
// check below consults.
//
// It is needed there because a storage root is not always a plain directory on
// the host. On this box /DATA, /mnt and /media resolve to themselves, but the
// product has a documented fallback layout where /DATA's contents actually live
// in /var/lib/casaos/files (CasaNova-LocalStorage route/v2/merge.go). On such an
// install every single file-manager path resolves into /var, so the blocklist
// re-check would reject the entire file API with a message about symlinks that
// tells the user nothing. A path that lands inside a RESOLVED storage root is
// therefore accepted even when that root itself lives under a blocked prefix.
// This does not reopen the escape it closed: /DATA/x -> /etc stays blocked,
// because /etc is not inside the resolved root.
//
// A var, not a const slice, so tests can point it at a fixture tree.
var DataRoots = []string{"/DATA", "/mnt", "/media"}

// FixAndCleanPath
// The upper layer of the root directory is still the root directory.
// So ".." And "." will be cleared
// for example
// 1. ".." or "." => "/"
// 2. "../..." or "./..." => "/..."
// 3. "../.x." or "./.x." => "/.x."
// 4. "x//\\y" = > "/z/x"
func FixAndCleanPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return stdpath.Clean(path)
}

// PathAddSeparatorSuffix Add path '/' suffix
// for example /root => /root/
func PathAddSeparatorSuffix(path string) string {
	if !strings.HasSuffix(path, "/") {
		path = path + "/"
	}
	return path
}

// PathEqual judge path is equal
func PathEqual(path1, path2 string) bool {
	return FixAndCleanPath(path1) == FixAndCleanPath(path2)
}

func IsSubPath(path string, subPath string) bool {
	path, subPath = FixAndCleanPath(path), FixAndCleanPath(subPath)
	return path == subPath || strings.HasPrefix(subPath, PathAddSeparatorSuffix(path))
}

func Ext(path string) string {
	ext := stdpath.Ext(path)
	if strings.HasPrefix(ext, ".") {
		return ext[1:]
	}
	return ext
}

func EncodePath(path string, all ...bool) string {
	seg := strings.Split(path, "/")
	toReplace := []struct {
		Src string
		Dst string
	}{
		{Src: "%", Dst: "%25"},
		{"%", "%25"},
		{"?", "%3F"},
		{"#", "%23"},
	}
	for i := range seg {
		if len(all) > 0 && all[0] {
			seg[i] = url.PathEscape(seg[i])
		} else {
			for j := range toReplace {
				seg[i] = strings.ReplaceAll(seg[i], toReplace[j].Src, toReplace[j].Dst)
			}
		}
	}
	return strings.Join(seg, "/")
}

func JoinBasePath(basePath, reqPath string) (string, error) {
	if strings.HasSuffix(reqPath, "..") || strings.Contains(reqPath, "../") {
		return "", errors.New("access using relative path is not allowed")
	}
	return stdpath.Join(FixAndCleanPath(basePath), FixAndCleanPath(reqPath)), nil
}

// blockedSystemPrefix reports which BlockedSystemPrefixes entry contains path,
// or "" when none does. Boundary-safe match (exact dir OR prefix + "/") so a
// legit path like "/etcd-data" is NOT caught by "/etc".
func blockedSystemPrefix(path string) string {
	for _, prefix := range BlockedSystemPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return prefix
		}
	}
	return ""
}

// deepestExistingAncestor splits path into the deepest ancestor that exists on
// disk and the trailing components that do not exist yet. It exists because
// SanitizeFilePath is also called on paths that are *about* to be created —
// copy/move destinations, new files, upload targets — where EvalSymlinks on
// the full path would fail with ENOENT and break those operations.
// os.Lstat (not os.Stat) is deliberate: a dangling symlink must count as
// existing so the caller below can expand it instead of walking past it.
// Cost is one Lstat per missing component, so O(depth) in the worst case and
// exactly one syscall for the common "path already exists" request.
func deepestExistingAncestor(path string) (string, []string) {
	var missing []string
	for {
		if _, err := os.Lstat(path); err == nil {
			// components were collected leaf-first — restore source order
			for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
				missing[i], missing[j] = missing[j], missing[i]
			}
			return path, missing
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", nil
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}

// resolveSymlinks returns where path actually lands on disk: its deepest
// existing ancestor passed through filepath.EvalSymlinks, with the components
// that do not exist yet re-appended. On any resolution failure it returns path
// unchanged so the caller still gets the lexical check it always had.
func resolveSymlinks(path string) string {
	// A dangling link needs one extra hop per link in the chain; the bound is
	// only here so a symlink loop can never spin a request thread.
	for hop := 0; hop < 8; hop++ {
		dir, missing := deepestExistingAncestor(path)
		if dir == "" {
			return path
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		// dir survives Lstat but not EvalSymlinks, so dir itself is a dangling
		// symlink. Expand it by hand and retry, otherwise a link such as
		// /DATA/x -> /etc/newfile would still let a create or an upload land
		// inside a blocked directory.
		target, err := os.Readlink(dir)
		if err != nil {
			return path
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(dir), target)
		}
		path = filepath.Join(append([]string{target}, missing...)...)
	}
	return path
}

// withinDataRoot reports whether resolved — a path that has already been run
// through resolveSymlinks — lands inside one of the DataRoots, compared against
// the roots' own resolved locations. See DataRoots for why this exemption
// exists. Only ever consulted after the blocklist already matched, so the extra
// EvalSymlinks calls never touch the path every valid request takes.
func withinDataRoot(resolved string) bool {
	for _, root := range DataRoots {
		target := resolveSymlinks(FixAndCleanPath(root))
		// A root that resolved to "/" — or onto a blocked prefix as a whole,
		// e.g. /DATA -> /var — would exempt everything under it and silently
		// disable the blocklist. Only a root that lands STRICTLY inside a
		// blocked prefix (the /var/lib/casaos/files fallback) exempts anything.
		if target == "/" || blockedSystemPrefix(target) == target {
			continue
		}
		if resolved == target || strings.HasPrefix(resolved, target+"/") {
			return true
		}
	}
	return false
}

// sanitizeLexical runs the checks that need no filesystem access: cleaning, the
// blocked-prefix match and the null-byte guard. Shared by SanitizeFilePath and
// SanitizeFilePathNoFollow so the two can never drift apart.
func sanitizeLexical(path string) (string, error) {
	if path == "" {
		return "", errors.New("path cannot be empty")
	}

	// Clean the path
	cleaned := FixAndCleanPath(path)

	// Block read/write/delete of sensitive system directories. Uses the single
	// canonical BlockedSystemPrefixes list so SanitizeFilePath, the v3 download
	// handlers and the upload blocklist can't drift — the drift is exactly how
	// /var (which holds the JWT signing key + user DB) went unblocked on the
	// read side. Boundary-safe match (exact dir OR prefix + "/") so a legit
	// path like "/etcd-data" is NOT caught by "/etc".
	if prefix := blockedSystemPrefix(cleaned); prefix != "" {
		return "", errors.New("access to " + prefix + " is not allowed")
	}

	// Ensure path doesn't contain null bytes (can bypass checks in some systems)
	if strings.ContainsRune(cleaned, 0) {
		return "", errors.New("path contains invalid characters")
	}

	return cleaned, nil
}

// CleanFilePathForSystemDiskRead is sanitizeLexical WITHOUT the blocked-prefix
// list: cleaning, the empty check and the null-byte guard only.
//
// It exists for one caller shape: a READ by an account whose system-disk switch
// is on. For that account the blocked-prefix list is exactly the rule the switch
// lifts — "/etc, /usr, /var… are not yours to see" — and leaving it in front of
// the authority made the switch a no-op for every one of those directories. The
// boundary does not disappear: storageauth still judges the resolved path,
// refuses kernel pseudo-filesystems and withheld secrets, honours operator
// DENY, and the switch never applies to a write. Every other caller keeps
// SanitizeFilePath.
func CleanFilePathForSystemDiskRead(path string) (string, error) {
	if path == "" {
		return "", errors.New("path cannot be empty")
	}
	cleaned := FixAndCleanPath(path)
	if strings.ContainsRune(cleaned, 0) {
		return "", errors.New("path contains invalid characters")
	}
	return cleaned, nil
}

// checkResolvedTarget runs the blocklist against where a path really lands on
// disk. The lexical check alone is not enough: a symlink planted inside an
// allowed area (/DATA/x -> /etc) walks straight past it, because the requested
// path starts with /DATA while the bytes read, written or deleted come from a
// blocked directory. Only the destination decides — a link to another data area
// (/DATA/media -> /mnt/disk1/media, routine on a NAS) keeps working, and so does
// an install whose storage root itself lives under a blocked prefix (DataRoots).
func checkResolvedTarget(resolved string) error {
	if prefix := blockedSystemPrefix(resolved); prefix != "" && !withinDataRoot(resolved) {
		return errors.New("path resolves into " + prefix + ", which is not allowed")
	}
	return nil
}

// SanitizeFilePath cleans and validates a user-supplied file path.
// It prevents path traversal attacks by:
// 1. Cleaning the path (resolving . and ..)
// 2. Rejecting paths that try to escape with ../
// 3. Ensuring the path is absolute
// 4. Blocking access to sensitive system paths
// 5. Blocking paths that reach a sensitive system path through a symlink
//
// Use this for everything that touches a path's CONTENT — read, write,
// download, copy, upload. Operations that act on the entry itself (delete,
// trash, rename) want SanitizeFilePathNoFollow instead.
func SanitizeFilePath(path string) (string, error) {
	cleaned, err := sanitizeLexical(path)
	if err != nil {
		return "", err
	}

	// Runs last so the syscalls only happen for otherwise-valid paths.
	//
	// The lexical path is still what we return: callers delete, trash and
	// upload through it, and handing back the resolved target would make those
	// operations act on a symlink's destination instead of on the link.
	if err := checkResolvedTarget(resolveSymlinks(cleaned)); err != nil {
		return "", err
	}

	return cleaned, nil
}

// SanitizeFilePathNoFollow is SanitizeFilePath for operations that act on the
// entry ITSELF rather than on its content: delete, move-to-trash and rename.
// It resolves the PARENT directory and re-attaches the final component instead
// of following it.
//
// Following the last component costs a real capability: /DATA/AppData/foo/
// nginx.conf -> /etc/nginx/nginx.conf lists fine in the file manager, but every
// attempt to remove or rename it answered 400 because the check saw /etc — the
// user had no way to clean the link up from the UI. Unlinking or renaming a
// symlink never touches its target, so only the parent has to be outside the
// blocklist. The parent IS still resolved, so /DATA/escape/shadow (through
// /DATA/escape -> /etc) stays blocked, and so does a final component that lands
// in a blocked directory because the parent itself resolved there.
func SanitizeFilePathNoFollow(path string) (string, error) {
	cleaned, err := sanitizeLexical(path)
	if err != nil {
		return "", err
	}

	resolved := filepath.Join(resolveSymlinks(filepath.Dir(cleaned)), filepath.Base(cleaned))
	if err := checkResolvedTarget(resolved); err != nil {
		return "", err
	}

	return cleaned, nil
}
