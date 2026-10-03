package storageauth

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The per-account whole-filesystem SEARCH switch: may this principal run the
// file manager's name search from anywhere on the disk, outside authorized
// storage?
//
// It is a second, separate switch rather than a consequence of the system-disk
// switch because the two cost different things. Browsing "/" reads one
// directory per click; a search from "/" walks every directory it can reach in
// one request. An administrator may want the first for an account without
// handing it the second.
//
// IT REQUIRES THE SYSTEM-DISK SWITCH. Searching outside authorized storage is
// useless without it — every hit would be a path the account cannot open — and
// the search walk is authorized directory by directory with AuthorizeListing,
// which is the system-disk switch's own decision. So:
//
//   - SetSystemSearchAccess refuses to turn it ON for an account whose
//     system-disk switch is off, with an error the administrator sees.
//   - WholeFilesystemSearch requires BOTH at decision time, so turning the
//     system-disk switch off later disables whole-filesystem search on the next
//     request without anyone having to remember the second switch.
//
// WHAT IT DOES NOT DO: everything systemdisk.go lists still holds — no writes,
// no reading inside kernel pseudo-filesystems, no operator DENY overridden,
// never a guest. On top of that the walk itself (SearchScope) does not descend
// into pseudo-filesystems at all (listing /proc recursively is thousands of
// per-process entries and nothing a user is looking for), does not cross into
// network mounts (a dead NFS server would pin the handler until its timeout,
// and a remote share is somebody else's disk), does not follow symlinks, and
// does not return the withheld credentials as hits. A search returns NAMES and
// metadata only; it never opens a file.

const systemSearchFileName = "storage-system-search.json"

// SetSystemSearchAccess turns the whole-filesystem search switch on or off for
// one account, durably. Turning it OFF is always accepted; turning it ON needs
// the account's system-disk switch to be on already.
func SetSystemSearchAccess(principalID, role string, on bool) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	if !global.provisioned {
		return global.failLocked("the authority has not been provisioned",
			errors.New("cannot record system-search access before provisioning"))
	}
	id := strings.TrimSpace(principalID)
	if id == "" {
		return errors.New("system-search access must name a principal")
	}
	if strings.EqualFold(strings.TrimSpace(role), roleGuest) {
		return fmt.Errorf("the guest account may never be given whole-filesystem search")
	}
	if _, disk := global.systemDisk[id]; on && !disk {
		return fmt.Errorf("whole-filesystem search needs system-disk access: turn that switch on for this account first")
	}

	next := make([]string, 0, len(global.systemSearch)+1)
	for existing := range global.systemSearch {
		if existing != id {
			next = append(next, existing)
		}
	}
	if on {
		next = append(next, id)
	}
	sort.Strings(next)

	if err := writeAtomic(filepath.Join(global.dir, systemSearchFileName),
		systemDiskFile{Version: schemaVersion, PrincipalIDs: next}); err != nil {
		return err
	}

	global.systemSearch = make(map[string]struct{}, len(next))
	for _, p := range next {
		global.systemSearch[p] = struct{}{}
	}
	return nil
}

// SystemSearchAccessFor reports whether the search switch is RECORDED on for
// one account. Whether it is in effect is WholeFilesystemSearch.
func SystemSearchAccessFor(principalID string) bool {
	global.mu.RLock()
	defer global.mu.RUnlock()
	_, on := global.systemSearch[strings.TrimSpace(principalID)]
	return on
}

// SystemSearchPrincipals lists the accounts the search switch is recorded on
// for, so the administrative UI can render the switches.
func SystemSearchPrincipals() []string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := make([]string, 0, len(global.systemSearch))
	for id := range global.systemSearch {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// WholeFilesystemSearch reports whether p may search outside authorized
// storage: not a guest, the search switch on AND the system-disk switch on.
func WholeFilesystemSearch(p Principal) bool {
	if strings.EqualFold(p.Role, roleGuest) {
		return false
	}
	global.mu.RLock()
	defer global.mu.RUnlock()
	id := strings.TrimSpace(p.ID)
	_, search := global.systemSearch[id]
	_, disk := global.systemDisk[id]
	return search && disk
}

// networkFilesystems are filesystem types whose contents live on another
// machine. The search walk does not cross into them.
var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"9p": true, "ceph": true, "glusterfs": true, "lustre": true, "afs": true,
	"ncpfs": true, "davfs": true, "beegfs": true,
	"fuse.sshfs": true, "fuse.rclone": true, "fuse.s3fs": true,
	"fuse.glusterfs": true, "fuse.davfs2": true,
}

// SearchScope is the per-request guard a whole-filesystem search walk consults.
// It is built once per search: the mount table is read here, never per entry.
type SearchScope struct {
	p       Principal
	network map[string]bool
}

// NewSearchScope prepares the guard for one search by p.
func NewSearchScope(p Principal) *SearchScope {
	return &SearchScope{p: p, network: readNetworkMounts()}
}

// Descend reports whether the walk may enter dir. root is the directory the
// search started from: the caller chose it, so a network mount is entered when
// it IS the root and only skipped when the walk would cross into it.
func (s *SearchScope) Descend(root, dir string) bool {
	if dir != root && s.network[dir] {
		return false
	}
	d := AuthorizeListing(s.p, dir)
	if !d.Allowed {
		return false
	}
	// Listable through the switch but a kernel pseudo-filesystem: not walked.
	if strings.HasPrefix(d.Rule, "system-disk-access+list") {
		return false
	}
	return true
}

// Hide reports whether path must not be returned as a hit: the credentials the
// system-disk switch withholds. The walk never follows symlinks, so the lexical
// path is the object itself.
func (s *SearchScope) Hide(path string) bool {
	_, withheld := isWithheld(path)
	return withheld
}

// readNetworkMounts returns the mount points carrying network filesystems. An
// unreadable table yields none: the walk is still bounded by its own file
// count and timeout, and every directory is still authorized.
func readNetworkMounts() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		if networkFilesystems[right[0]] {
			out[unescapeMountinfo(left[4])] = true
		}
	}
	return out
}
