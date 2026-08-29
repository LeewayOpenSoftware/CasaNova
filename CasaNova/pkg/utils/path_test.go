package utils

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSanitizeFilePathBlocksVar is the release-blocking regression: /var holds
// the JWT ECDSA signing key (/var/lib/casaos/db/jwt_ec_private.pem). If the
// blocklist ever drops /var again, the file API can read/download it → full
// root. Also pins the boundary-safe match so a legit sibling like /etcd-data
// is NOT caught by the /etc entry.
func TestSanitizeFilePathBlocksVar(t *testing.T) {
	blocked := []string{
		"/var",
		"/var/lib/casaos/db/jwt_ec_private.pem",
		"/etc/shadow",
		"/root/.ssh/authorized_keys",
		"/opt/anything",
		"/srv/x",
	}
	for _, p := range blocked {
		if _, err := SanitizeFilePath(p); err == nil {
			t.Errorf("SanitizeFilePath(%q) = nil error, want blocked", p)
		}
	}

	allowed := []string{"/DATA/movies/a.mkv", "/mnt/usb/x", "/etcd-data/wal", "/variable/y"}
	for _, p := range allowed {
		if _, err := SanitizeFilePath(p); err != nil {
			t.Errorf("SanitizeFilePath(%q) = %v, want allowed", p, err)
		}
	}

	// Traversal still has to collapse into the blocklist, and a ".." that ends
	// back inside a data area must stay allowed.
	if _, err := SanitizeFilePath("/DATA/../etc/shadow"); err == nil {
		t.Error(`SanitizeFilePath("/DATA/../etc/shadow") = nil error, want blocked`)
	}
	if got, err := SanitizeFilePath("/DATA/movies/../music/a.flac"); err != nil || got != "/DATA/music/a.flac" {
		t.Errorf(`SanitizeFilePath("/DATA/movies/../music/a.flac") = %q, %v; want "/DATA/music/a.flac", nil`, got, err)
	}
}

// TestSanitizeFilePathFollowsSymlinks pins the fix for the lexical-only check:
// a symlink planted inside an allowed area used to serve (and let callers
// write to) a blocked system directory, because the requested path still
// started with an allowed prefix. Links that stay inside user data, and paths
// that do not exist yet (copy destinations, uploads, new files), must keep
// working — rejecting those would break create/copy/move/upload.
func TestSanitizeFilePathFollowsSymlinks(t *testing.T) {
	data := t.TempDir()
	other := t.TempDir()
	if blockedSystemPrefix(data) != "" || blockedSystemPrefix(other) != "" {
		t.Skipf("temp dir %q sits inside a blocked prefix; test cannot distinguish", data)
	}

	if err := os.WriteFile(filepath.Join(data, "real.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "media.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// /DATA/escape -> /etc, the reported escape.
	if err := os.Symlink("/etc", filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}
	// /DATA/media -> /mnt/disk1, the routine NAS layout that must keep working.
	if err := os.Symlink(other, filepath.Join(data, "media")); err != nil {
		t.Fatal(err)
	}
	// A link whose target does not exist yet: reading it fails anyway, but a
	// create or an upload through it would land in the blocked directory.
	if err := os.Symlink("/etc/casanova-planted.conf", filepath.Join(data, "dangling")); err != nil {
		t.Fatal(err)
	}

	blocked := []string{
		filepath.Join(data, "escape"),
		filepath.Join(data, "escape", "shadow"),
		filepath.Join(data, "escape", "not-created-yet.conf"),
		filepath.Join(data, "dangling"),
	}
	for _, p := range blocked {
		if _, err := SanitizeFilePath(p); err == nil {
			t.Errorf("SanitizeFilePath(%q) = nil error, want blocked (symlink into /etc)", p)
		}
	}

	allowed := []string{
		filepath.Join(data, "real.txt"),                       // plain existing file
		filepath.Join(data, "media"),                          // link to another data area
		filepath.Join(data, "media", "media.mkv"),             // read through that link
		filepath.Join(data, "new-upload.bin"),                 // does not exist yet
		filepath.Join(data, "sub", "dir", "copy-destination"), // several levels missing
		filepath.Join(data, "media", "incoming.bin"),          // new file under a legit link
	}
	for _, p := range allowed {
		got, err := SanitizeFilePath(p)
		if err != nil {
			t.Errorf("SanitizeFilePath(%q) = %v, want allowed", p, err)
			continue
		}
		// The lexical path is the contract: callers delete/trash/upload through
		// it, so it must NOT come back rewritten to the symlink's target.
		if got != p {
			t.Errorf("SanitizeFilePath(%q) = %q, want the path unchanged", p, got)
		}
	}
}

// TestSanitizeFilePathDataRootUnderBlockedPrefix covers the installs where the
// storage root does NOT resolve to itself: the documented fallback layout puts
// /DATA's contents under /var/lib/casaos/files, so every file-manager path
// resolves into /var. Without the DataRoots exemption the whole file API would
// answer 400 there and the user would lose the file manager entirely. An escape
// out of the resolved root must still be refused.
func TestSanitizeFilePathDataRootUnderBlockedPrefix(t *testing.T) {
	base := t.TempDir()
	if blockedSystemPrefix(base) != "" {
		t.Skipf("temp dir %q sits inside a blocked prefix; test cannot distinguish", base)
	}

	// `system` stands in for /var, `backing` for /var/lib/casaos/files and
	// `root` for the /DATA symlink the file manager is pointed at.
	system := filepath.Join(base, "system")
	backing := filepath.Join(system, "files")
	root := filepath.Join(base, "DATA")
	if err := os.MkdirAll(filepath.Join(backing, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(system, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backing, root); err != nil {
		t.Fatal(err)
	}
	// A link that leaves the storage root, which must stay blocked.
	if err := os.Symlink(filepath.Join(system, "secrets"), filepath.Join(backing, "escape")); err != nil {
		t.Fatal(err)
	}

	// Both lists are package vars precisely so a test can stand a fixture tree
	// in for the real host layout.
	oldBlocked, oldRoots := BlockedSystemPrefixes, DataRoots
	BlockedSystemPrefixes = append(append([]string{}, oldBlocked...), system)
	DataRoots = []string{root}
	t.Cleanup(func() { BlockedSystemPrefixes, DataRoots = oldBlocked, oldRoots })

	allowed := []string{
		root,
		filepath.Join(root, "movies"),
		filepath.Join(root, "movies", "a.mkv"), // does not exist yet
	}
	for _, p := range allowed {
		got, err := SanitizeFilePath(p)
		if err != nil {
			t.Errorf("SanitizeFilePath(%q) = %v, want allowed (storage root under a blocked prefix)", p, err)
			continue
		}
		if got != p {
			t.Errorf("SanitizeFilePath(%q) = %q, want the path unchanged", p, got)
		}
	}

	blocked := []string{
		filepath.Join(system, "secrets", "key.pem"),  // the blocked prefix itself
		filepath.Join(root, "escape"),                // link out of the storage root
		filepath.Join(root, "escape", "key.pem"),     // read through that link
		filepath.Join(root, "escape", "planted.pem"), // create through that link
	}
	for _, p := range blocked {
		if _, err := SanitizeFilePath(p); err == nil {
			t.Errorf("SanitizeFilePath(%q) = nil error, want blocked", p)
		}
	}
}

// TestSanitizeFilePathNoFollowKeepsLinkOperations pins the delete/trash/rename
// carve-out: a link whose target is a blocked system file lists fine in the
// file manager, so refusing to unlink it left the user with an entry they could
// never remove. Unlinking the link does not touch its target — but anything
// that reads or writes THROUGH it, and any path whose parent resolves into a
// blocked directory, must still be refused.
func TestSanitizeFilePathNoFollowKeepsLinkOperations(t *testing.T) {
	data := t.TempDir()
	if blockedSystemPrefix(data) != "" {
		t.Skipf("temp dir %q sits inside a blocked prefix; test cannot distinguish", data)
	}

	// The reported case: /DATA/AppData/foo/nginx.conf -> /etc/nginx/nginx.conf.
	link := filepath.Join(data, "nginx.conf")
	if err := os.Symlink("/etc/nginx/nginx.conf", link); err != nil {
		t.Fatal(err)
	}
	// A directory link, used to check that paths THROUGH it stay blocked.
	if err := os.Symlink("/etc", filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}

	got, err := SanitizeFilePathNoFollow(link)
	if err != nil {
		t.Errorf("SanitizeFilePathNoFollow(%q) = %v, want allowed (deleting a link is not deleting its target)", link, err)
	} else if got != link {
		t.Errorf("SanitizeFilePathNoFollow(%q) = %q, want the path unchanged", link, got)
	}
	// Reading or writing through the same link must still be refused.
	if _, err := SanitizeFilePath(link); err == nil {
		t.Errorf("SanitizeFilePath(%q) = nil error, want blocked (content comes from /etc)", link)
	}

	blocked := []string{
		"/etc/shadow",                            // plain blocked path
		filepath.Join(data, "escape", "shadow"),  // parent resolves into /etc
		filepath.Join(data, "escape", "planted"), // same, for a path being created
	}
	for _, p := range blocked {
		if _, err := SanitizeFilePathNoFollow(p); err == nil {
			t.Errorf("SanitizeFilePathNoFollow(%q) = nil error, want blocked", p)
		}
	}
}
