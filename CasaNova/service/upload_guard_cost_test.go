package service

// upload_guard_cost_test.go — what the upload path guards COST, because a fix
// that is more expensive than the bug it removes is not a fix. (This project
// shipped one: a UDP limiter that turned out 17x more expensive per packet than
// the flaw it replaced.)
//
// The numbers on an i7-6700HQ, /tmp, 2000 iterations:
//
//	sanitize only (what the previous patch did)   61 us/op
//	sanitize + containment (this patch)          138 us/op   +77 us
//	rejecting an escape                          107 us/op   cheaper than accepting
//	deepest path the component bound permits    2924 us/op   the ceiling
//
// Two properties matter more than the absolute numbers:
//
//   - refusing costs LESS than accepting, so the guard is not an amplifier an
//     attacker can drive by sending traffic it will reject;
//   - the cost is bounded. It is linear in the number of path components, and
//     the component count is caller-chosen, so UploadFile caps it up front.
//     Before that cap a single small POST carrying a few hundred thousand path
//     segments bought a matching number of Lstat syscalls.
//
// For scale: every accepted chunk also fsyncs its data (file.Sync in
// UploadFile), which costs milliseconds on real storage. 77 us is well under
// that floor, so the guarantee is not paid for out of throughput.

import (
	"os"
	"path/filepath"
	"testing"
)

func benchPaths(tb testing.TB, base, rel string) (string, string, string, string) {
	tb.Helper()
	bp, err := sanitizeUploadPath(filepath.Clean(base))
	if err != nil {
		tb.Fatal(err)
	}
	dp, err := sanitizeUploadPath(filepath.Join(bp, rel))
	if err != nil {
		tb.Fatal(err)
	}
	fp, err := sanitizeUploadPath(filepath.Dir(dp))
	if err != nil {
		tb.Fatal(err)
	}
	tp, err := sanitizeUploadPath(uploadTmpPath(dp, "1234-file.bin"))
	if err != nil {
		tb.Fatal(err)
	}
	return bp, dp, fp, tp
}

// The full guard sequence a chunk pays on the accept path.
func BenchmarkUploadPathGuards(b *testing.B) {
	base := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bp, dp, fp, tp := benchPaths(b, base, "sub/deep/file.bin")
		root, err := uploadResolve(bp)
		if err != nil {
			b.Fatal(err)
		}
		for _, p := range []string{fp, dp, tp} {
			if err := requireUnderUploadRoot(root, p); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// The same sequence without the containment half — the previous patch's cost,
// so the delta is exactly the price of the guarantee this one adds.
func BenchmarkUploadPathGuardsSanitizeOnly(b *testing.B) {
	base := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchPaths(b, base, "sub/deep/file.bin")
	}
}

// What an attacker pays for, and what the daemon pays to refuse them.
func BenchmarkUploadPathGuardsRejection(b *testing.B) {
	base := b.TempDir()
	outside := b.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bp, dp, _, _ := benchPaths(b, base, "link/victim/x")
		root, err := uploadResolve(bp)
		if err != nil {
			b.Fatal(err)
		}
		if err := requireUnderUploadRoot(root, dp); err == nil {
			b.Fatal("expected the escape to be rejected")
		}
	}
}

// The worst case the component bound still permits.
func BenchmarkUploadPathGuardsDeepestPermitted(b *testing.B) {
	base := b.TempDir()
	rel := ""
	for i := 0; i < 128; i++ {
		rel = filepath.Join(rel, "aa")
	}
	rel = filepath.Join(rel, "f.bin")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bp, dp, fp, tp := benchPaths(b, base, rel)
		root, err := uploadResolve(bp)
		if err != nil {
			b.Fatal(err)
		}
		for _, p := range []string{fp, dp, tp} {
			if err := requireUnderUploadRoot(root, p); err != nil {
				b.Fatal(err)
			}
		}
	}
}
