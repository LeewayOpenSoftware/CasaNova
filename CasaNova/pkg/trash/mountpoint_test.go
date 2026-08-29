package trash

import (
	"os"
	"path/filepath"
	"testing"
)

// A bind mount shares its superblock with the original, so st_dev is identical
// on both sides — but rename(2) still returns EXDEV across the boundary. Every
// CasaNova unit gets /DATA, /mnt and /media bind-mounted into its namespace by
// systemd's ReadWritePaths=, so the old st_dev walk put the trash on "/" and
// every delete failed with "cross-device rename". These cases pin the mount
// table as the source of truth.
const bindMountFixture = `21 27 0:20 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
22 27 0:5 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
27 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw,errors=remount-ro
90 27 259:2 /DATA /DATA rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw,errors=remount-ro
91 27 259:2 /mnt /mnt rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw,errors=remount-ro
95 27 0:52 / /media/my\040disk rw,relatime shared:60 - ext4 /dev/sdb1 rw
99 27 0:60 / /var/lib/casanova rw,relatime shared:70 - ext4 /dev/nvme0n1p2 rw
`

func writeFixture(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	orig := mountinfoPath
	mountinfoPath = p
	t.Cleanup(func() { mountinfoPath = orig })
}

func TestMountPointOfPrefersLongestMatch(t *testing.T) {
	writeFixture(t, bindMountFixture)

	cases := []struct {
		path string
		want string
	}{
		// The whole point: /DATA is its own mount even though it is the
		// same device as /, so the trash must land inside /DATA.
		{"/DATA/photos/holiday.jpg", "/DATA"},
		{"/DATA", "/DATA"},
		{"/mnt/pool/file.bin", "/mnt"},
		{"/var/lib/casanova/db/user.db", "/var/lib/casanova"},
		// No more specific mount contains these, so they belong to "/".
		{"/etc/hostname", "/"},
		{"/srv/whatever", "/"},
		// A name that merely shares a prefix is NOT inside the mount.
		{"/DATABASE/file", "/"},
		{"/mnt-backup/file", "/"},
		// Octal-escaped mount points are decoded before matching.
		{"/media/my disk/movie.mkv", "/media/my disk"},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got, ok := mountPointOf(c.path)
			if !ok {
				t.Fatalf("mountPointOf(%q) reported no match", c.path)
			}
			if got != c.want {
				t.Fatalf("mountPointOf(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// Without /proc the caller must still get an answer from the st_dev walk
// rather than an error, so trashing keeps working on systems without procfs.
func TestFindMountpointFallsBackWithoutMountinfo(t *testing.T) {
	orig := mountinfoPath
	mountinfoPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { mountinfoPath = orig })

	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mp, err := findMountpoint(file)
	if err != nil {
		t.Fatalf("findMountpoint: %v", err)
	}
	if mp == "" {
		t.Fatal("findMountpoint returned an empty mount point")
	}
	// The fallback must name a real directory that contains the file.
	if !pathWithinMount(file, mp) {
		t.Fatalf("fallback mount point %q does not contain %q", mp, file)
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	cases := map[string]string{
		`/media/disk`:          "/media/disk",
		`/media/my\040disk`:    "/media/my disk",
		`/a\011b`:              "/a\tb",
		`/back\134slash`:       "/back\\slash",
		`/not\9octal`:          `/not\9octal`,
		`/trailing\`:           `/trailing\`,
		`/media/two\040 \040x`: "/media/two   x",
	}
	for in, want := range cases {
		if got := unescapeMountinfo(in); got != want {
			t.Fatalf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
