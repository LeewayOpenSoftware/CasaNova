package v1

// Regression tests for the archive extraction path.
//
// Every test here asserts an INVARIANT of extractArchive, not that some
// function was called:
//
//   - a link / traversal / special member is never materialised, and the file
//     it aimed at outside the destination does not appear;
//   - a decompression bomb cannot write more than the byte budget;
//   - a cancelled context stops the work rather than abandoning it;
//   - the archive is opened exactly ONCE, so validation and extraction cannot
//     see different bytes (PT-03).
//
// Each test bounds its own runtime; nothing here can hang the suite.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testStall = 30 * time.Second

func testLimits(maxBytes int64) archiveLimits {
	return archiveLimits{MaxBytes: maxBytes, MaxEntries: maxArchiveEntries, Stall: testStall}
}

// tarEntry is one member to put in a synthetic tar.
type tarEntry struct {
	hdr  tar.Header
	body string
}

func writeTar(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		if h.Size == 0 && e.body != "" {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("tar header %s: %v", h.Name, err)
		}
		if e.body != "" {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatalf("tar body %s: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestExtractArchiveRefusesUnsafeEntries is the zip-slip invariant: the escape
// target must not exist after the call, whatever shape the escape takes.
func TestExtractArchiveRefusesUnsafeEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name: "symlink to an absolute path",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "safe.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "ok"},
				{hdr: tar.Header{Name: "pwn", Typeflag: tar.TypeSymlink, Linkname: "/", Mode: 0o777}},
			},
		},
		{
			name: "hardlink entry",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "safe.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "ok"},
				{hdr: tar.Header{Name: "pwn", Typeflag: tar.TypeLink, Linkname: "safe.txt", Mode: 0o644}},
			},
		},
		{
			name: "parent traversal in the name",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "../escaped.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
			},
		},
		{
			name: "absolute name",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "/escaped.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
			},
		},
		{
			name: "setuid bit",
			entries: []tarEntry{
				// 0o4755 is c_ISUID in the POSIX mode field, which is what
				// tar.Header.FileInfo turns into os.ModeSetuid.
				{hdr: tar.Header{Name: "suid", Typeflag: tar.TypeReg, Mode: 0o4755}, body: "x"},
			},
		},
		{
			name: "character device",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "payload.tar")
			writeTar(t, src, tc.entries)
			dest := filepath.Join(root, "out")

			status, err := extractArchive(context.Background(), src, dest, testLimits(1<<30))
			if err == nil {
				t.Fatal("extractArchive accepted an unsafe archive")
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (err: %v)", status, http.StatusBadRequest, err)
			}

			// The invariant: nothing escaped the destination.
			if _, statErr := os.Lstat(filepath.Join(root, "escaped.txt")); statErr == nil {
				t.Fatal("an entry escaped the destination: root/escaped.txt exists")
			}
			// ...and the refused member itself was never created.
			for _, bad := range []string{"pwn", "suid", "dev/null"} {
				if _, statErr := os.Lstat(filepath.Join(dest, bad)); statErr == nil {
					t.Fatalf("refused member %q was materialised anyway", bad)
				}
			}
		})
	}
}

// TestExtractArchiveRefusesZipSymlink covers the format the old tar-only
// branch missed: a zip carries the symlink bit in its external attributes.
func TestExtractArchiveRefusesZipSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "payload.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "pwn", Method: zip.Deflate}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatalf("zip header: %v", err)
	}
	if _, err := io.WriteString(w, "/etc"); err != nil {
		t.Fatalf("zip body: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	dest := filepath.Join(root, "out")
	status, err := extractArchive(context.Background(), src, dest, testLimits(1<<30))
	if err == nil {
		t.Fatal("extractArchive accepted a zip carrying a symlink member")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (err: %v)", status, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "pwn")); err == nil {
		t.Fatal("the symlink member was materialised")
	}
}

// TestExtractArchiveStopsDecompressionBomb is the CN-DOS-05 invariant: no
// matter what the archive declares, the bytes on disk stay within the budget.
func TestExtractArchiveStopsDecompressionBomb(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "bomb.tar")
	// 8 members of 1 MiB each; the budget only allows 2 MiB.
	const member = 1 << 20
	entries := make([]tarEntry, 0, 8)
	for i := 0; i < 8; i++ {
		entries = append(entries, tarEntry{
			hdr:  tar.Header{Name: fmt.Sprintf("chunk-%d.bin", i), Typeflag: tar.TypeReg, Mode: 0o644},
			body: strings.Repeat("A", member),
		})
	}
	writeTar(t, src, entries)

	const budget = 2 << 20
	dest := filepath.Join(root, "out")
	// Pre-create the destination so the refusal does NOT roll it back: the
	// bytes that landed have to stay observable, otherwise the size assertion
	// below would pass for free.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	status, err := extractArchive(context.Background(), src, dest, testLimits(budget))
	if err == nil {
		t.Fatal("extractArchive expanded a bomb without complaint")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (err: %v)", status, err)
	}
	// The limit has to be legible to whoever hit it.
	if !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("refusal does not state the limit: %v", err)
	}
	// The invariant: the disk never took more than the budget.
	if n := treeSize(t, dest); n > budget {
		t.Fatalf("wrote %d bytes with a %d byte budget", n, budget)
	}
}

// TestExtractArchiveEnforcesEntryCount covers the millions-of-empty-files
// variant, which never trips a byte budget.
func TestExtractArchiveEnforcesEntryCount(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "many.tar")
	entries := make([]tarEntry, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, tarEntry{
			hdr: tar.Header{Name: fmt.Sprintf("f-%d", i), Typeflag: tar.TypeReg, Mode: 0o644},
		})
	}
	writeTar(t, src, entries)

	dest := filepath.Join(root, "out")
	lim := testLimits(1 << 30)
	lim.MaxEntries = 10
	status, err := extractArchive(context.Background(), src, dest, lim)
	if err == nil {
		t.Fatal("extractArchive ignored the entry-count limit")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (err: %v)", status, err)
	}
	if !strings.Contains(err.Error(), "10 entries") {
		t.Fatalf("refusal does not state the limit: %v", err)
	}
}

// TestExtractArchiveHonoursCancellation is the "no abandoned goroutine"
// invariant: when the deadline fires, the work STOPS. The old code ran
// archiver.Unarchive in a goroutine and merely walked away from it, so the
// extraction ran to completion after the handler had answered 504.
func TestExtractArchiveHonoursCancellation(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "payload.tar")
	writeTar(t, src, []tarEntry{
		{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("a", 1<<20)},
		{hdr: tar.Header{Name: "b.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("b", 1<<20)},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(root, "out")
	// Pre-created so the rollback cannot be what makes the tree empty.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	status, err := extractArchive(ctx, src, dest, testLimits(1<<30))
	if err == nil {
		t.Fatal("extractArchive ran to completion under a cancelled context")
	}
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (err: %v)", status, http.StatusGatewayTimeout, err)
	}
	if n := treeSize(t, dest); n != 0 {
		t.Fatalf("cancelled extraction still wrote %d bytes", n)
	}
}

// TestExtractArchiveReadsTheArchiveExactlyOnce is the PT-03 invariant.
//
// The archive is a FIFO, which can only be consumed once. The old shape —
// archiveHasUnsafeEntry(src) followed by archiver.Unarchive(src) — opened the
// path twice, which is exactly what let anything with write access to /DATA
// swap the file between the check and the extraction. A single-pass extractor
// succeeds here; a two-pass one cannot, because there are no second bytes.
func TestExtractArchiveReadsTheArchiveExactlyOnce(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "payload.tar")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "hello.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := io.WriteString(tw, "world"); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	payload := buf.Bytes()

	// One writer, one payload. Opening the FIFO for write blocks until the
	// reader shows up, so this goroutine parks until extractArchive opens it.
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer w.Close()
		_, _ = w.Write(payload)
	}()

	dest := filepath.Join(root, "out")
	type result struct {
		status int
		err    error
	}
	res := make(chan result, 1)
	go func() {
		status, err := extractArchive(context.Background(), fifo, dest, testLimits(1<<30))
		res <- result{status, err}
	}()

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("single-pass extraction failed on a once-readable archive: %v", r.err)
		}
	case <-time.After(20 * time.Second):
		// A second open of the FIFO parks forever waiting for a writer that
		// will never come — that is a two-pass extractor.
		t.Fatal("extraction blocked: the archive was opened more than once")
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("content = %q, want %q", got, "world")
	}
}

// TestExtractArchiveDoesNotWriteThroughExistingSymlink covers a link planted
// in the destination beforehand (any container with /DATA access can do that),
// which O_NOFOLLOW refuses to follow.
func TestExtractArchiveDoesNotWriteThroughExistingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "victim.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	src := filepath.Join(root, "payload.tar")
	writeTar(t, src, []tarEntry{
		{hdr: tar.Header{Name: "victim.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "pwned"},
	})

	_, err := extractArchive(context.Background(), src, dest, testLimits(1<<30))
	if err == nil {
		t.Fatal("extraction followed a symlink already present in the destination")
	}
	got, readErr := os.ReadFile(outside)
	if readErr != nil {
		t.Fatalf("reading the link target: %v", readErr)
	}
	if string(got) != "original" {
		t.Fatalf("the link target was overwritten: %q", got)
	}
}

// TestExtractArchiveHappyPath keeps the refusals honest: an ordinary archive
// still comes out whole, with its directory structure intact. A "fix" that
// broke this would be worse than the bug.
func TestExtractArchiveHappyPath(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "payload.tar")
	writeTar(t, src, []tarEntry{
		{hdr: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, Size: 0}},
		{hdr: tar.Header{Name: "docs/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "docs/readme.md", Typeflag: tar.TypeReg, Mode: 0o644}, body: "# hi"},
		{hdr: tar.Header{Name: "docs/deep/nested.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: "nested"},
		{hdr: tar.Header{Name: "empty.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
	})

	dest := filepath.Join(root, "out")
	status, err := extractArchive(context.Background(), src, dest, testLimits(1<<30))
	if err != nil {
		t.Fatalf("extractArchive(status=%d) rejected a legitimate archive: %v", status, err)
	}
	for name, want := range map[string]string{
		"docs/readme.md":       "# hi",
		"docs/deep/nested.txt": "nested",
		"empty.txt":            "",
	} {
		got, readErr := os.ReadFile(filepath.Join(dest, name))
		if readErr != nil {
			t.Fatalf("missing %s: %v", name, readErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// The pax global header is metadata, not a member.
	if _, err := os.Lstat(filepath.Join(dest, "pax_global_header")); err == nil {
		t.Fatal("pax global header was written out as a file")
	}
}

// TestExtractArchiveKeepsAPreexistingDestination: a refusal must not delete
// data the user already had there. Rollback is only for a destination we made.
func TestExtractArchiveKeepsAPreexistingDestination(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keep := filepath.Join(dest, "mine.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	src := filepath.Join(root, "payload.tar")
	writeTar(t, src, []tarEntry{
		{hdr: tar.Header{Name: "pwn", Typeflag: tar.TypeSymlink, Linkname: "/", Mode: 0o777}},
	})

	if _, err := extractArchive(context.Background(), src, dest, testLimits(1<<30)); err == nil {
		t.Fatal("expected a refusal")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "mine" {
		t.Fatalf("pre-existing data was destroyed by the refusal (err=%v, got=%q)", err, got)
	}
}

// TestExtractArchiveKeepsPartialOutputOnIOFailure is the first half of the
// "a rollback must not destroy a restore" invariant, in its deterministic form.
//
// The archive is legitimate; the second member simply cannot be written
// (its parent path is a regular file the first member just created), which is
// the same class of failure as a bad sector or an ENOSPC mid-restore: status
// 500, nothing hostile. The old code rolled back on EVERY rejection whenever it
// had created the destination, so hours of extracted data vanished on the last
// broken entry. The invariant is that what landed is still there.
func TestExtractArchiveKeepsPartialOutputOnIOFailure(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "payload.tar")
	writeTar(t, src, []tarEntry{
		{hdr: tar.Header{Name: "restored.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "hours of work"},
		// MkdirAll("<dest>/restored.txt") fails with ENOTDIR: restored.txt is a
		// file. Deterministic, no timing involved.
		{hdr: tar.Header{Name: "restored.txt/child", Typeflag: tar.TypeReg, Mode: 0o644}, body: "boom"},
	})

	// NOT pre-created: this is exactly the case the old rollback applied to.
	dest := filepath.Join(root, "out")
	status, err := extractArchive(context.Background(), src, dest, testLimits(1<<30))
	if err == nil {
		t.Fatal("expected the unwritable member to fail the extraction")
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (err: %v)", status, http.StatusInternalServerError, err)
	}

	// The invariant: the bytes that made it to disk were not destroyed.
	got, readErr := os.ReadFile(filepath.Join(dest, "restored.txt"))
	if readErr != nil {
		t.Fatalf("the partial extraction was rolled back: %v", readErr)
	}
	if string(got) != "hours of work" {
		t.Fatalf("kept content = %q, want %q", got, "hours of work")
	}
}

// TestExtractArchiveKeepsPartialOutputOnStall is the assigned regression: a
// STALL must not delete the restore. A 15 minute read stall on a failing USB
// disk used to cost the user every byte extracted so far, which is worse than
// the pre-patch behaviour (that one abandoned the worker and kept the tree).
//
// The archive is a FIFO so the test, not the clock, decides when the extraction
// starves: the first member is written in full, and the rest is withheld until
// the context has been cancelled. Everything is bounded; nothing can hang.
func TestExtractArchiveKeepsPartialOutputOnStall(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "payload.tar")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	const firstBody = 1024 // a whole number of 512-byte tar blocks: no padding
	first := strings.Repeat("R", firstBody)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "restored.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: firstBody}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := io.WriteString(tw, first); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	// Flush completes the current member's block padding, so cut is exactly the
	// end of member one: the reader can consume it and nothing more.
	if err := tw.Flush(); err != nil {
		t.Fatalf("tar flush: %v", err)
	}
	cut := buf.Len()
	if err := tw.WriteHeader(&tar.Header{Name: "second.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatalf("tar header 2: %v", err)
	}
	if _, err := io.WriteString(tw, "tail"); err != nil {
		t.Fatalf("tar body 2: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	payload := buf.Bytes()

	release := make(chan struct{})
	go func() {
		// Blocks until the extractor opens the FIFO for reading.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer w.Close()
		if _, err := w.Write(payload[:cut]); err != nil {
			return
		}
		<-release
		_, _ = w.Write(payload[cut:])
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dest := filepath.Join(root, "out") // NOT pre-created: rollback would apply
	type result struct {
		status int
		err    error
	}
	res := make(chan result, 1)
	go func() {
		status, err := extractArchive(ctx, fifo, dest, testLimits(1<<30))
		res <- result{status, err}
	}()

	// Wait for member one to be fully on disk, then starve the extraction.
	landed := filepath.Join(dest, "restored.txt")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, err := os.Stat(landed); err == nil && st.Size() == firstBody {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("first member never landed; cannot exercise the stall path")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	close(release) // let the walk reach the next member and observe the stall

	var r result
	select {
	case r = <-res:
	case <-time.After(30 * time.Second):
		t.Fatal("extractArchive did not return after cancellation")
	}
	if r.err == nil {
		t.Fatal("extraction reported success despite the stall")
	}
	if r.status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (err: %v)", r.status, http.StatusGatewayTimeout, r.err)
	}
	// The invariant: the stall cost the user nothing that was already restored.
	got, readErr := os.ReadFile(landed)
	if readErr != nil {
		t.Fatalf("the stall destroyed the partial restore: %v", readErr)
	}
	if string(got) != first {
		t.Fatalf("kept %d bytes, want %d", len(got), firstBody)
	}
	// ...and the user is told the tree survived, so they know to resume.
	if !strings.Contains(r.err.Error(), "kept") {
		t.Fatalf("the 504 does not tell the user the partial tree was kept: %v", r.err)
	}
}

// TestExtractArchiveRollsBackUnsafeAndOverBudget pins the other half: the two
// rejection classes that MUST still discard their partial output. Keeping half
// a hostile payload, or leaving a bomb's bytes on a disk it just filled, is the
// harm itself — so narrowing the rollback must not narrow it to nothing.
func TestExtractArchiveRollsBackUnsafeAndOverBudget(t *testing.T) {
	const member = 1 << 20

	cases := []struct {
		name    string
		entries []tarEntry
		budget  int64
		limit   int
	}{
		{
			name: "unsafe member",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "landed"},
				{hdr: tar.Header{Name: "pwn", Typeflag: tar.TypeSymlink, Linkname: "/", Mode: 0o777}},
			},
			budget: 1 << 30,
			limit:  maxArchiveEntries,
		},
		{
			name: "over the byte budget",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "a.bin", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("A", member)},
				{hdr: tar.Header{Name: "b.bin", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("B", member)},
				{hdr: tar.Header{Name: "c.bin", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("C", member)},
			},
			budget: 1 << 20,
			limit:  maxArchiveEntries,
		},
		{
			name: "over the entry budget",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "landed"},
				{hdr: tar.Header{Name: "b.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "landed"},
				{hdr: tar.Header{Name: "c.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "landed"},
			},
			budget: 1 << 30,
			limit:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "payload.tar")
			writeTar(t, src, tc.entries)

			dest := filepath.Join(root, "out")
			lim := testLimits(tc.budget)
			lim.MaxEntries = tc.limit
			status, err := extractArchive(context.Background(), src, dest, lim)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (err: %v)", status, err)
			}
			// The invariant: a destination we created is gone, bytes and all.
			if _, statErr := os.Lstat(dest); !os.IsNotExist(statErr) {
				t.Fatalf("partial output of a hostile archive survived at %s (%d bytes)", dest, treeSize(t, dest))
			}
		})
	}
}

// TestExtractArchiveBudgetCountsNetNewBytes is the second assigned regression:
// the budget is free space, so it has to bound NET NEW bytes. Re-extracting a
// backup into the folder that already holds it needs no new space — each member
// is opened O_TRUNC, handing the old copy's blocks back as the new copy lands —
// yet the gross-byte accounting refused it with "expands to more than N", so a
// 50 GB restore over its own folder failed on a NAS with 40 GB free.
func TestExtractArchiveBudgetCountsNetNewBytes(t *testing.T) {
	const member = 1 << 20 // 1 MiB per file, 4 MiB of archive

	// Incompressible bodies: the credit is capped by ALLOCATED blocks, so a
	// run of one repeated byte would credit almost nothing on a transparently
	// compressing filesystem and make this test depend on where /tmp lives.
	newBody := incompressible(member, 1)
	oldBody := incompressible(member, 2)

	root := t.TempDir()
	src := filepath.Join(root, "backup.tar")
	entries := make([]tarEntry, 0, 4)
	for i := 0; i < 4; i++ {
		entries = append(entries, tarEntry{
			hdr:  tar.Header{Name: fmt.Sprintf("data/part-%d.bin", i), Typeflag: tar.TypeReg, Mode: 0o644},
			body: newBody,
		})
	}
	writeTar(t, src, entries)

	// The destination already holds the previous copy of the same backup:
	// same names, same sizes, different content.
	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(dest, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 4; i++ {
		old := filepath.Join(dest, fmt.Sprintf("data/part-%d.bin", i))
		if err := os.WriteFile(old, []byte(oldBody), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Far less free space than the archive expands to, but the re-extraction
	// needs none of it: every byte written replaces a byte reclaimed.
	const budget = 512 << 10 // half a member
	status, err := extractArchive(context.Background(), src, dest, testLimits(budget))
	if err != nil {
		t.Fatalf("re-extracting over an existing copy was refused (status=%d): %v", status, err)
	}

	// The invariant is not just "no error": the data actually got replaced.
	for i := 0; i < 4; i++ {
		got, readErr := os.ReadFile(filepath.Join(dest, fmt.Sprintf("data/part-%d.bin", i)))
		if readErr != nil {
			t.Fatalf("part-%d missing: %v", i, readErr)
		}
		if string(got) != newBody {
			t.Fatalf("part-%d was not replaced (%d bytes)", i, len(got))
		}
	}
	if n := treeSize(t, dest); n != 4*member {
		t.Fatalf("tree = %d bytes, want %d — the old copy was not reclaimed", n, 4*member)
	}
}

// TestExtractArchiveNetBudgetStillStopsGrowth is the guard on the credit above:
// reclaiming must only cover bytes the filesystem really gets back. An archive
// whose members are BIGGER than the files they replace still has to be stopped
// once the growth exceeds the budget.
func TestExtractArchiveNetBudgetStillStopsGrowth(t *testing.T) {
	const small = 64 << 10   // what is already there per name
	const large = 1 << 20    // what the archive wants to put there
	const budget = 256 << 10 // less than the growth of 4 members

	root := t.TempDir()
	src := filepath.Join(root, "grow.tar")
	entries := make([]tarEntry, 0, 4)
	for i := 0; i < 4; i++ {
		entries = append(entries, tarEntry{
			hdr:  tar.Header{Name: fmt.Sprintf("p-%d.bin", i), Typeflag: tar.TypeReg, Mode: 0o644},
			body: strings.Repeat("N", large),
		})
	}
	writeTar(t, src, entries)

	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := os.WriteFile(filepath.Join(dest, fmt.Sprintf("p-%d.bin", i)), []byte(strings.Repeat("O", small)), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	status, err := extractArchive(context.Background(), src, dest, testLimits(budget))
	if err == nil {
		t.Fatal("the net budget let an archive grow the destination without limit")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (err: %v)", status, err)
	}
	// The invariant: growth over what was already there stayed within budget.
	// (dest pre-existed, so it is not rolled back and the size is observable.)
	if growth := treeSize(t, dest) - 4*small; growth > budget {
		t.Fatalf("destination grew by %d bytes with a %d byte budget", growth, budget)
	}
}

// TestExtractArchiveDoesNotCreditSparseOrLinkedFiles closes the way the net
// accounting could be turned into unlimited budget: a file planted at the
// destination that LOOKS big but costs the filesystem nothing. `truncate -s 1T`
// is free, and a symlink to a huge file is free too — crediting either would
// buy an extraction the whole array, which on this box is an outage for every
// container, not just a failed restore.
func TestExtractArchiveDoesNotCreditSparseOrLinkedFiles(t *testing.T) {
	const member = 1 << 20
	const budget = 1 << 20 // one member's worth of genuinely new space

	root := t.TempDir()
	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 1 TiB of apparent size, zero allocated blocks — free to create.
	f, err := os.Create(filepath.Join(dest, "p-0.bin"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if truncErr := f.Truncate(1 << 40); truncErr != nil {
		_ = f.Close()
		t.Skipf("sparse files unsupported here: %v", truncErr)
	}
	_ = f.Close()

	src := filepath.Join(root, "bomb.tar")
	entries := make([]tarEntry, 0, 8)
	for i := 0; i < 8; i++ {
		entries = append(entries, tarEntry{
			hdr:  tar.Header{Name: fmt.Sprintf("p-%d.bin", i), Typeflag: tar.TypeReg, Mode: 0o644},
			body: strings.Repeat("A", member),
		})
	}
	writeTar(t, src, entries)

	// dest pre-exists, so nothing is rolled back and the bytes that landed
	// stay observable for the assertion below.
	if _, err := extractArchive(context.Background(), src, dest, testLimits(budget)); err == nil {
		t.Fatal("a sparse file at the destination bought the archive an unlimited budget")
	}
	// The invariant: the credit did not let the bomb past the budget.
	if n := treeSize(t, dest); n > 2*budget {
		t.Fatalf("wrote %d bytes with a %d byte budget", n, budget)
	}
}

// TestArchiveReclaimableBytes pins the credit itself: only a regular file's
// really-allocated bytes may be handed to the budget.
func TestArchiveReclaimableBytes(t *testing.T) {
	root := t.TempDir()

	plain := filepath.Join(root, "plain.bin")
	if err := os.WriteFile(plain, []byte(incompressible(64<<10, 7)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := archiveReclaimableBytes(plain); got != 64<<10 {
		t.Fatalf("regular file credit = %d, want %d", got, 64<<10)
	}

	// A symlink pointing at a big file must credit nothing: Lstat must not
	// follow it (writing through it is refused by O_NOFOLLOW anyway).
	link := filepath.Join(root, "link.bin")
	if err := os.Symlink(plain, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := archiveReclaimableBytes(link); got != 0 {
		t.Fatalf("symlink credit = %d, want 0", got)
	}

	if got := archiveReclaimableBytes(root); got != 0 {
		t.Fatalf("directory credit = %d, want 0", got)
	}
	if got := archiveReclaimableBytes(filepath.Join(root, "absent")); got != 0 {
		t.Fatalf("missing file credit = %d, want 0", got)
	}

	sparse := filepath.Join(root, "sparse.bin")
	sf, err := os.Create(sparse)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if truncErr := sf.Truncate(1 << 40); truncErr != nil {
		_ = sf.Close()
		t.Skipf("sparse files unsupported here: %v", truncErr)
	}
	_ = sf.Close()
	if got := archiveReclaimableBytes(sparse); got > 1<<20 {
		t.Fatalf("sparse 1 TiB file credited %d bytes; it occupies almost none", got)
	}
}

// incompressible builds a body no filesystem can shrink, so tests that reason
// about allocated blocks behave the same on ext4, tmpfs and btrfs-with-zstd.
func incompressible(n int, seed uint32) string {
	b := make([]byte, n)
	x := seed*2654435761 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return string(b)
}

// TestArchiveSpaceBudgetLeavesRoom: the budget must be generous enough for a
// real restore and must never be the whole filesystem.
func TestArchiveSpaceBudgetLeavesRoom(t *testing.T) {
	dir := t.TempDir()
	budget, err := archiveSpaceBudget(filepath.Join(dir, "does", "not", "exist", "yet"))
	if err != nil {
		t.Fatalf("archiveSpaceBudget: %v", err)
	}
	if budget <= 0 {
		t.Fatalf("budget = %d; a legitimate extraction would be refused outright", budget)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	avail := int64(st.Bavail) * st.Bsize
	if budget >= avail {
		t.Fatalf("budget %d does not reserve anything below the %d bytes free", budget, avail)
	}
}

func treeSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	return total
}
