package v1

// A directory symlink planted INSIDE the destination (any container or SMB
// client with /DATA access can do that) used to be followed: only the final
// component of each member was Lstat'd, while MkdirAll, CreateTemp and Rename
// resolved the intermediate link, so members landed outside storage.

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractArchiveDoesNotFollowPlantedDirectorySymlink(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"file member": {
			{hdr: tar.Header{Name: "sub/cron-job", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
		},
		"dir then file": {
			{hdr: tar.Header{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755}},
			{hdr: tar.Header{Name: "sub/cron-job", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
		},
		"nested dir": {
			{hdr: tar.Header{Name: "sub/deep/", Typeflag: tar.TypeDir, Mode: 0o755}},
			{hdr: tar.Header{Name: "sub/deep/cron-job", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "restore")
			outside := filepath.Join(root, "outside")
			for _, d := range []string{dest, outside} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(dest, "sub")); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(root, "payload.tar")
			writeTar(t, src, entries)

			if _, err := extractArchive(context.Background(), src, dest, testLimits(1<<30)); err == nil {
				t.Error("extraction succeeded through a planted directory symlink")
			}
			left, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Errorf("members landed outside the destination: %v", left)
			}
		})
	}
}
