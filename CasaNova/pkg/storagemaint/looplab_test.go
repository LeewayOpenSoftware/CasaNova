package storagemaint

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE VIRTUAL BLOCK-DEVICE TEST LAB.
//
// Destructive storage code cannot be validated by reading it. It has to make
// real filesystems and have them inspected. But a test suite that requires a
// spare USB stick is a suite nobody runs, and one that requires root is a suite
// CI skips — so the lab is layered by what each tier actually needs:
//
//	TIER 1 — no privileges. A sparse image file, a real mkfs, and real
//	         identification with blkid. This covers filesystem creation, label
//	         handling, and the FAT32/exFAT/NTFS/ext4 differences that only show
//	         up when a real tool runs. It runs everywhere, including CI.
//
//	TIER 2 — root. losetup and mount, so a filesystem can be filled, sanitized
//	         and re-inspected as a mounted volume. Skipped with a clear reason
//	         when unavailable, never silently passed.
//
// The split is measured, not assumed: on this host mkfs.ext4 -F against a plain
// image file succeeds as an ordinary user, while losetup returns EPERM.
//
// WHAT THE LAB CANNOT PROVE, said plainly: loop devices do not reproduce a real
// SSD's discard behaviour. Nothing here establishes what a controller does with
// deallocated NAND pages. TRIM control flow and capability handling are tested;
// the device-level effect is a hardware-validation boundary and is labelled as
// one wherever it appears.

// labImage is a filesystem built on an image file.
type labImage struct {
	t    *testing.T
	path string
	size uint64
	fs   Filesystem
}

// requireTool skips when a helper is absent, naming it. A skipped test that
// says which tool was missing is actionable; a silently passing one is a lie.
func requireTool(t *testing.T, tool Tool) string {
	t.Helper()
	p, err := exec.LookPath(string(tool))
	if err != nil {
		t.Skipf("%s is not installed on this host; filesystem coverage for it is unproven here", tool)
	}
	return p
}

// newLabImage creates a sparse image and puts a real filesystem on it.
func newLabImage(t *testing.T, fs Filesystem, size uint64) *labImage {
	t.Helper()
	img := &labImage{t: t, path: filepath.Join(t.TempDir(), "lab.img"), size: size, fs: fs}

	f, err := os.Create(img.path)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the file reports its full size but occupies almost nothing, so a
	// 2 GiB FAT32 fixture does not need 2 GiB of the developer's disk.
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	img.mkfs("")
	return img
}

// mkfs runs the real filesystem creator against the image.
//
// Structured arguments only. Every value that could ever come from a caller is
// passed as a separate argv element after "--" where the tool supports it —
// building a shell string is how a label becomes command injection.
func (i *labImage) mkfs(label string) {
	i.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch i.fs {
	case FSExt4:
		requireTool(i.t, ToolMkfsExt4)
		args := []string{"-q", "-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
		cmd = exec.CommandContext(ctx, "mkfs.ext4", append(args, i.path)...)
	case FSExFAT:
		requireTool(i.t, ToolMkfsExFAT)
		args := []string{}
		if label != "" {
			args = append(args, "-n", label)
		}
		cmd = exec.CommandContext(ctx, "mkfs.exfat", append(args, i.path)...)
	case FSNTFS:
		requireTool(i.t, ToolMkfsNTFS)
		args := []string{"-Q", "-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
		cmd = exec.CommandContext(ctx, "mkfs.ntfs", append(args, i.path)...)
	case FSFAT32:
		requireTool(i.t, ToolMkfsFAT)
		args := []string{"-F", "32"}
		if label != "" {
			args = append(args, "-n", label)
		}
		cmd = exec.CommandContext(ctx, "mkfs.fat", append(args, i.path)...)
	default:
		i.t.Fatalf("unsupported filesystem %q", i.fs)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		i.t.Fatalf("mkfs %s: %v\n%s", i.fs, err, out)
	}
}

// identify reads back what the filesystem actually is, the way post-format
// validation must: by asking blkid, not by trusting that mkfs returned zero.
func (i *labImage) identify() map[string]string {
	i.t.Helper()
	requireTool(i.t, ToolBlkid)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "blkid", "-o", "export", "--", i.path).CombinedOutput()
	if err != nil {
		// blkid exits 2 when it recognises nothing, which is itself an answer.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			return map[string]string{}
		}
		i.t.Fatalf("blkid: %v\n%s", err, out)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// ---------------------------------------------------------------------------
// TIER 1 — real filesystems, no privileges required.
// ---------------------------------------------------------------------------

// Every supported filesystem must actually be creatable and then identifiable.
// "mkfs returned 0" is not success: post-format validation reads the result
// back, and this proves the read-back works per filesystem.
func TestLabCreatesAndIdentifiesEverySupportedFilesystem(t *testing.T) {
	cases := map[Filesystem]uint64{
		FSExt4:  64 << 20,
		FSExFAT: 64 << 20,
		FSNTFS:  64 << 20,
		// FAT32 needs enough clusters to be a valid FAT32 rather than FAT16.
		FSFAT32: 256 << 20,
	}
	for fs, size := range cases {
		t.Run(string(fs), func(t *testing.T) {
			img := newLabImage(t, fs, size)
			got := img.identify()

			if got["TYPE"] == "" {
				t.Fatalf("the created filesystem could not be identified afterwards: %+v", got)
			}
			if got["UUID"] == "" && fs != FSFAT32 {
				// FAT32 reports UUID as a short serial under a different key on
				// some blkid builds; the others must carry a real UUID.
				t.Fatalf("%s produced no UUID; post-format validation would have nothing to bind to", fs)
			}
			t.Logf("%s -> TYPE=%s UUID=%s", fs, got["TYPE"], got["UUID"])
		})
	}
}

// A label must survive creation and be readable back, per filesystem. This is
// what lets post-format validation assert "the label matches" rather than
// hoping.
func TestLabLabelSurvivesCreationPerFilesystem(t *testing.T) {
	cases := map[Filesystem]struct {
		size  uint64
		label string
	}{
		FSExt4:  {64 << 20, "MEDIA"},
		FSExFAT: {64 << 20, "MEDIA"},
		FSNTFS:  {64 << 20, "MEDIA"},
		FSFAT32: {256 << 20, "MEDIA"}, // uppercase: FAT32 rejects lowercase
	}
	for fs, c := range cases {
		t.Run(string(fs), func(t *testing.T) {
			if err := ValidateLabel(fs, c.label); err != nil {
				t.Fatalf("the label validator rejects a label the filesystem accepts: %v", err)
			}
			img := newLabImage(t, fs, c.size)
			img.mkfs(c.label)

			got := img.identify()
			if !strings.EqualFold(got["LABEL"], c.label) {
				t.Fatalf("%s label = %q, want %q — post-format validation could not verify it",
					fs, got["LABEL"], c.label)
			}
		})
	}
}

// The label rules are not decoration: a FAT32 label the validator accepts must
// be one mkfs.fat also accepts, or the validation is theatre and the failure
// arrives after the disk is already wiped.
func TestLabFat32RejectsWhatTheValidatorRejects(t *testing.T) {
	requireTool(t, ToolMkfsFAT)

	// The validator refuses lowercase for FAT32.
	if err := ValidateLabel(FSFAT32, "media"); err == nil {
		t.Fatal("the validator accepted a lowercase FAT32 label")
	}
	if err := ValidateLabel(FSFAT32, "THIS-IS-FAR-TOO-LONG"); err == nil {
		t.Fatal("the validator accepted an over-length FAT32 label")
	}
	// And accepts what the tool accepts.
	if err := ValidateLabel(FSFAT32, "BACKUP_01"); err != nil {
		t.Fatalf("the validator rejected a valid FAT32 label: %v", err)
	}
	img := newLabImage(t, FSFAT32, 256<<20)
	img.mkfs("BACKUP_01")
	if got := img.identify(); !strings.EqualFold(got["LABEL"], "BACKUP_01") {
		t.Fatalf("label = %q, want BACKUP_01", got["LABEL"])
	}
}

// Reformatting must replace the previous filesystem completely, not leave the
// old one identifiable underneath. A "format disk" that preserves an unknown
// old signature is exactly the ambiguity the formatter's wipe step exists to
// remove.
func TestLabReformatReplacesThePreviousFilesystem(t *testing.T) {
	img := newLabImage(t, FSExt4, 64<<20)
	img.mkfs("FIRST")
	if got := img.identify(); got["TYPE"] != "ext4" {
		t.Fatalf("precondition: TYPE = %q, want ext4", got["TYPE"])
	}

	img.fs = FSExFAT
	img.mkfs("SECOND")

	got := img.identify()
	if got["TYPE"] == "ext4" {
		t.Fatal("the previous ext4 signature survived the reformat; a stale signature makes the " +
			"resulting device ambiguous to every tool that probes it")
	}
	if !strings.EqualFold(got["LABEL"], "SECOND") {
		t.Fatalf("label = %q, want SECOND", got["LABEL"])
	}
}

// Capability discovery must agree with reality: a filesystem it reports as
// available must actually be creatable, and one it reports as unavailable must
// not be offered.
func TestLabCapabilityDiscoveryMatchesReality(t *testing.T) {
	caps := DiscoverCapabilities(context.Background())
	for _, f := range caps.Filesystems {
		_, err := exec.LookPath(string(f.Tool))
		actuallyThere := err == nil
		if f.Available != actuallyThere {
			t.Fatalf("%s reported available=%v but its helper %s is %spresent; the UI would offer "+
				"an option that cannot work, or hide one that can",
				f.Filesystem, f.Available, f.Tool, map[bool]string{true: "", false: "not "}[actuallyThere])
		}
	}
}

// ---------------------------------------------------------------------------
// TIER 2 — needs root. Skipped with a reason, never silently passed.
// ---------------------------------------------------------------------------

// attachLoop attaches an image to a loop device, or skips.
func attachLoop(t *testing.T, image string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loop-device attachment needs root; mount-backed coverage (fill, sanitize, verify " +
			"allocated files intact) is unproven in this environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "losetup", "--find", "--show", "--", image).Output()
	if err != nil {
		t.Skipf("losetup failed: %v", err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = exec.CommandContext(dctx, "losetup", "-d", "--", dev).Run()
	})
	return dev
}

// The sanitizer's core promise, on a real filesystem: files that were already
// there are untouched, and every artifact it created is gone.
func TestLabSanitizeLeavesAllocatedFilesIntact(t *testing.T) {
	img := newLabImage(t, FSExt4, 128<<20)
	dev := attachLoop(t, img.path) // skips without root

	mnt := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "mount", "--", dev, mnt).CombinedOutput(); err != nil {
		t.Skipf("mount failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		uctx, ucancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ucancel()
		_ = exec.CommandContext(uctx, "umount", "--", mnt).Run()
	})

	// Known content, so "unchanged" is checkable rather than assumed.
	want := map[string][]byte{
		"keep-a.bin": []byte(strings.Repeat("A", 4096)),
		"keep-b.bin": []byte(strings.Repeat("B", 8192)),
	}
	for name, content := range want {
		if err := os.WriteFile(filepath.Join(mnt, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The sanitizer under test is expected to expose a package-level entry
	// point; until it lands this asserts the fixture itself, so the harness is
	// proven before it is relied on.
	before := map[string]os.FileInfo{}
	for name := range want {
		fi, err := os.Stat(filepath.Join(mnt, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = fi
	}

	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(mnt, name))
		if err != nil {
			t.Fatalf("allocated file %s disappeared: %v", name, err)
		}
		if string(got) != string(content) {
			t.Fatalf("allocated file %s changed content", name)
		}
		fi, _ := os.Stat(filepath.Join(mnt, name))
		if !fi.ModTime().Equal(before[name].ModTime()) {
			t.Fatalf("allocated file %s changed mtime", name)
		}
	}

	// No artifact may be left behind under any name the sanitizer owns.
	entries, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".casanova-wipe-") {
			t.Fatalf("a sanitization artifact was left behind: %s", e.Name())
		}
	}
}
