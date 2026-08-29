package service

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParseUnwalkableMounts(t *testing.T) {
	// Real mountinfo shapes: a plain root, an external NTFS disk (fuseblk —
	// local, must stay sweepable), a NAS share, an rclone mount, an autofs
	// trigger and a share whose mount point contains a space.
	raw := `24 30 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
41 30 8:17 / /mnt/usb rw,relatime shared:2 - fuseblk /dev/sdb1 rw
55 30 0:52 / /mnt/nas rw,relatime shared:3 - nfs4 10.0.0.5:/vol rw
57 30 0:54 / /mnt/backup rw,relatime - cifs //10.0.0.7/backup rw
59 30 0:56 / /mnt/cloud rw,relatime - fuse.rclone rclone: rw
61 30 0:58 / /media/net rw,relatime - autofs systemd-1 rw
63 30 0:60 / /media/My\040Disk rw,relatime - smb3 //10.0.0.9/pub rw
`
	blocked := parseUnwalkableMounts(raw)

	for _, want := range []string{"/mnt/nas", "/mnt/backup", "/mnt/cloud", "/media/net", "/media/My Disk"} {
		if !blocked[want] {
			t.Errorf("expected %q to be blocked", want)
		}
	}
	for _, keep := range []string{"/", "/mnt/usb"} {
		if blocked[keep] {
			t.Errorf("%q is a local filesystem, it must stay sweepable", keep)
		}
	}
	if len(blocked) != 5 {
		t.Errorf("blocked = %v, want exactly the 5 remote mounts", blocked)
	}
}

func TestSweepOrphanTempDirsStopsAtMaxDepth(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)

	// An empty, stale `.temp` is a leftover shell (sweepTempScratch drops it
	// once nothing is left inside), so all three of these are sweep candidates
	// and only the depth guard decides their fate.
	shallow := mkOldTemp(t, root, old, "Media", ".temp")
	atLimit := mkOldTemp(t, root, old, "l1", "l2", "l3", "l4", "l5", ".temp")
	tooDeep := mkOldTemp(t, root, old, "l1", "l2", "l3", "l4", "l5", "l6", ".temp")

	stub := filepath.Join(root, "Media", "movie.mkv.casanova-tmp")
	if err := os.WriteFile(stub, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stub, old, old); err != nil {
		t.Fatal(err)
	}

	SweepOrphanTempDirs([]string{root})

	for _, gone := range []string{shallow, atLimit, stub} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%q should have been swept", gone)
		}
	}
	if _, err := os.Stat(tooDeep); err != nil {
		t.Errorf("%q is past maxSweepDepth and must be left alone: %v", tooDeep, err)
	}
}

// mkOldTemp creates root/parts… and back-dates the leaf so it falls outside
// the sweeper's one-hour safety margin.
func mkOldTemp(t *testing.T, root string, mtime time.Time, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Chunk-group guards
//
// sweepTempScratch runs os.RemoveAll, recursively and unattended, over the
// user's storage on every service start, and there is no trash behind it. The
// table below is the contract for what it is allowed to touch: everything that
// is not provably one of OUR abandoned upload chunk groups has to survive.
//
// Each case isolates ONE guard — the fixtures are otherwise perfectly
// sweepable, so if the guard under test is removed the case flips to "swept"
// and fails. Breaking each guard in turn and watching the matching case fail is
// how these were validated; a case that still passes with its guard disabled is
// pinning nothing.
// ---------------------------------------------------------------------------

// groupName is a real chunk-group directory name. PostFileUpload builds
// `<target>/.temp/<md5 hex><totalChunks>/`, i.e. GetHashByContent's 32
// lowercase hex digits with the chunk count appended.
const groupName = "d41d8cd98f00b204e9800998ecf8427e12"

type entryKind int

const (
	entryFile entryKind = iota
	entryDir
	entrySymlink
	entryFifo
)

// entry is one node of a fixture tree. Zero value is a stale regular file,
// which is what most of these trees are made of.
type entry struct {
	name     string
	kind     entryKind
	target   string // entrySymlink only
	children []entry
	// fresh dates the node inside the one-hour safety margin, i.e. it stands
	// for an upload that is still streaming.
	fresh bool
}

func TestSweepTempScratchGuards(t *testing.T) {
	cases := []struct {
		name string
		// tree is the content of `<root>/Media/.temp`.
		tree []entry
		// tempFresh dates the `.temp` shell inside the safety margin.
		tempFresh bool
		// gone / kept are paths relative to `.temp`.
		gone     []string
		kept     []string
		tempGone bool
	}{
		{
			name: "stale chunk group is swept",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "1"}, {name: "2"},
			}}},
			gone:     []string{groupName},
			tempGone: true,
		},
		{
			// A folder upload mirrors the source tree, so the chunks land in
			// `<group>/<subdir>/<n>` with an arbitrary subdir name.
			name: "stale nested folder upload is swept",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "Vacation", kind: entryDir, children: []entry{{name: "0"}, {name: "1"}}},
				{name: "0"},
			}}},
			gone:     []string{groupName},
			tempGone: true,
		},
		{
			name: "non-numeric file keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "manifest.json"},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/manifest.json"},
		},
		{
			name: "non-numeric file nested inside the group keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "Vacation", kind: entryDir, children: []entry{{name: "0"}, {name: "notes.txt"}}},
			}}},
			kept: []string{groupName, groupName + "/Vacation/0", groupName + "/Vacation/notes.txt"},
		},
		{
			name: "a chunk-shaped name that is really a symlink keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "1", kind: entrySymlink, target: "../../keepme.txt"},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/1"},
		},
		{
			name: "a symlinked subdirectory keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "Vacation", kind: entrySymlink, target: "../.."},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/Vacation"},
		},
		{
			name: "an irregular entry keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "1", kind: entryFifo},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/1"},
		},
		{
			// The upload is mid-flight: one chunk landed seconds ago. Sweeping
			// now would pull the staging area out from under a live client.
			name: "a chunk newer than the cutoff keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "0"}, {name: "1", fresh: true},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/1"},
		},
		{
			// Writing `<group>/<subdir>/1` touches <subdir>, never <group>, so
			// the group directory alone looks long abandoned here.
			name: "a fresh chunk nested inside the group keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, children: []entry{
				{name: "Vacation", kind: entryDir, children: []entry{{name: "0"}, {name: "1", fresh: true}}},
			}}},
			kept: []string{groupName, groupName + "/Vacation/0", groupName + "/Vacation/1"},
		},
		{
			name: "a fresh group directory keeps the group",
			tree: []entry{{name: groupName, kind: entryDir, fresh: true, children: []entry{
				{name: "0"}, {name: "1"},
			}}},
			kept: []string{groupName, groupName + "/0", groupName + "/1"},
		},
		{
			name: "a non-hex sibling is left alone",
			tree: []entry{{name: "My Photos", kind: entryDir, children: []entry{{name: "0"}}}},
			kept: []string{"My Photos", "My Photos/0"},
		},
		{
			// A bare digest with no chunk count appended: 32 characters, one
			// short of anything we ever created.
			name: "a hex sibling shorter than a digest plus chunk count is left alone",
			tree: []entry{{name: "d41d8cd98f00b204e9800998ecf8427e", kind: entryDir, children: []entry{{name: "0"}}}},
			kept: []string{"d41d8cd98f00b204e9800998ecf8427e", "d41d8cd98f00b204e9800998ecf8427e/0"},
		},
		{
			// GetHashByContent emits lowercase, so uppercase is somebody else's
			// directory — a SHA of a torrent, a checksum folder, anything.
			name: "an uppercase-hex sibling is left alone",
			tree: []entry{{name: "D41D8CD98F00B204E9800998ECF8427E12", kind: entryDir, children: []entry{{name: "0"}}}},
			kept: []string{"D41D8CD98F00B204E9800998ECF8427E12", "D41D8CD98F00B204E9800998ECF8427E12/0"},
		},
		{
			// (f) one bad neighbour is enough to keep the shell, and the good
			// group next to it still goes.
			name: "a surviving group keeps .temp",
			tree: []entry{
				{name: groupName, kind: entryDir, children: []entry{{name: "0"}}},
				{name: "d41d8cd98f00b204e9800998ecf8427e99", kind: entryDir, children: []entry{{name: "readme.md"}}},
			},
			gone: []string{groupName},
			kept: []string{"d41d8cd98f00b204e9800998ecf8427e99", "d41d8cd98f00b204e9800998ecf8427e99/readme.md"},
		},
		{
			// Chunk groups are directories. A plain file wearing the name is
			// not one, and neither is a link pointing at the user's pictures.
			name: "a file named like a chunk group is left alone",
			tree: []entry{{name: groupName}},
			kept: []string{groupName},
		},
		{
			// Worst case for the link: it points at a real directory whose
			// contents would pass the shape test if anything ever followed it.
			// Nothing may — os.ReadDir does follow symlinks, os.Lstat does not.
			name: "a symlink named like a chunk group is left alone",
			tree: []entry{{name: groupName, kind: entrySymlink, target: "../Numbered"}},
			kept: []string{groupName},
		},
		{
			name: "a loose file under .temp keeps .temp",
			tree: []entry{
				{name: groupName, kind: entryDir, children: []entry{{name: "0"}}},
				{name: "notes.txt"},
			},
			gone: []string{groupName},
			kept: []string{"notes.txt"},
		},
		{
			name: "a group nested to the recursion limit is swept",
			tree: []entry{{name: groupName, kind: entryDir, children: nest(maxChunkGroupDepth)}},
			gone: []string{groupName},
			// The shell goes too: sweepTempScratch reads its mtime before it
			// removes anything, so the RemoveAll above cannot refresh it out of
			// the safety margin.
			tempGone: true,
		},
		{
			name: "a group nested past the recursion limit is left alone",
			tree: []entry{{name: groupName, kind: entryDir, children: nest(maxChunkGroupDepth + 1)}},
			kept: []string{groupName, filepath.Join(append([]string{groupName}, nestPath(maxChunkGroupDepth+1)...)...)},
		},
		{
			// (h) The user made their own `.temp` and put things in it. None of
			// it is chunk-shaped, so none of it may be touched.
			name: "a user's own .temp is untouched",
			tree: []entry{
				{name: "invoice.pdf"},
				{name: "Drafts", kind: entryDir, children: []entry{{name: "letter.odt"}, {name: "0"}}},
			},
			kept: []string{"invoice.pdf", "Drafts", "Drafts/letter.odt", "Drafts/0"},
		},
		{
			// An empty `.temp` that appeared minutes ago belongs to an upload
			// that is just getting started — the first chunk has not landed yet.
			name:      "a freshly created empty .temp survives",
			tree:      nil,
			tempFresh: true,
		},
		{
			name:     "a stale empty .temp is swept",
			tree:     nil,
			tempGone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			media := filepath.Join(root, "Media")
			temp := filepath.Join(media, ".temp")
			if err := os.MkdirAll(temp, 0o755); err != nil {
				t.Fatal(err)
			}
			// A plain user file next to `.temp`, both as a symlink target and
			// as a tripwire: the sweeper has no business deleting it.
			keep := filepath.Join(media, "keepme.txt")
			if err := os.WriteFile(keep, []byte("user data"), 0o644); err != nil {
				t.Fatal(err)
			}
			// `Numbered` is the adversarial symlink target: a directory OUTSIDE
			// `.temp` that would look exactly like a chunk group to anything
			// that resolved a link instead of lstat-ing it.
			numbered := filepath.Join(media, "Numbered")
			if err := os.Mkdir(numbered, 0o755); err != nil {
				t.Fatal(err)
			}

			stale := time.Now().Add(-24 * time.Hour)
			fresh := time.Now().Add(-5 * time.Minute)

			var stamps []stamp
			for _, n := range []string{"0", "1"} {
				p := filepath.Join(numbered, n)
				if err := os.WriteFile(p, []byte("not a chunk"), 0o644); err != nil {
					t.Fatal(err)
				}
				stamps = append(stamps, stamp{path: p})
			}
			stamps = append(stamps, stamp{path: numbered})
			buildEntries(t, temp, tc.tree, &stamps)
			stamps = append(stamps, stamp{path: temp, fresh: tc.tempFresh})
			// Stamp only after the whole tree exists: creating a child rewrites
			// its parent's mtime and would undo an earlier back-dating.
			for _, s := range stamps {
				ts := stale
				if s.fresh {
					ts = fresh
				}
				lchtimes(t, s.path, ts)
			}

			SweepOrphanTempDirs([]string{root})

			if _, err := os.Lstat(keep); err != nil {
				t.Errorf("the user's file next to .temp was destroyed: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(numbered, "0")); err != nil {
				t.Errorf("the sweeper reached outside .temp and ate %s: %v", numbered, err)
			}
			for _, rel := range tc.gone {
				if _, err := os.Lstat(filepath.Join(temp, rel)); !os.IsNotExist(err) {
					t.Errorf("%q is an abandoned chunk group and should have been swept (err=%v)", rel, err)
				}
			}
			for _, rel := range tc.kept {
				if _, err := os.Lstat(filepath.Join(temp, rel)); err != nil {
					t.Errorf("%q is not ours to delete: %v", rel, err)
				}
			}
			_, tempErr := os.Lstat(temp)
			if tc.tempGone && !os.IsNotExist(tempErr) {
				t.Errorf(".temp shell should have been swept (err=%v)", tempErr)
			}
			if !tc.tempGone && tempErr != nil {
				t.Errorf(".temp shell should have survived: %v", tempErr)
			}
		})
	}
}

// TestSweepOrphanTempDirsRespectsDepthWithRealChunks is the depth guard again,
// but with a populated chunk group rather than an empty shell: a `.temp` past
// maxSweepDepth is never even read, so nothing under it can be reclaimed.
func TestSweepOrphanTempDirsRespectsDepthWithRealChunks(t *testing.T) {
	root := t.TempDir()
	stale := time.Now().Add(-24 * time.Hour)

	build := func(parts ...string) string {
		temp := filepath.Join(append([]string{root}, parts...)...)
		group := filepath.Join(temp, groupName)
		if err := os.MkdirAll(group, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(group, "0"), []byte("chunk"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{filepath.Join(group, "0"), group, temp} {
			lchtimes(t, p, stale)
		}
		return temp
	}

	atLimit := build("l1", "l2", "l3", "l4", "l5", ".temp")
	tooDeep := build("l1", "l2", "l3", "l4", "l5", "l6", ".temp")

	SweepOrphanTempDirs([]string{root})

	if _, err := os.Lstat(atLimit); !os.IsNotExist(err) {
		t.Errorf("%q sits at maxSweepDepth and should have been swept (err=%v)", atLimit, err)
	}
	if _, err := os.Lstat(filepath.Join(tooDeep, groupName, "0")); err != nil {
		t.Errorf("%q is past maxSweepDepth and must be left alone: %v", tooDeep, err)
	}
}

// ---------------------------------------------------------------------------
// Single-shot upload leftovers (`<destination>.upload-<UnixNano>.tmp`)
//
// PostFileUpload's single-shot branch removes its sidecar on every error path
// it can see. What it cannot see is the process dying mid-copy, and until this
// shape was added nothing on the box collected the result: the sweeper knew
// only `.temp` and `.casanova-tmp`, and the v2 registry's isUploadTmpPath wants
// exactly 8 hex characters where this name carries a 19-digit timestamp. The
// tests below pin both halves — the shape we match, and the boundary against
// the v2 tmp that belongs to the registry and not to a clock.
// ---------------------------------------------------------------------------

func TestSingleShotUploadTmpNameMatchesItsProducer(t *testing.T) {
	// The name the handler actually writes must be the name the janitor
	// recognizes. This is the drift that created the leak in the first place.
	produced := filepath.Base(SingleShotUploadTmpPath("/DATA/Media/holiday.mkv"))
	if !isSingleShotUploadTmpName(produced) {
		t.Fatalf("the sweeper does not recognize the name we generate: %q", produced)
	}
	// And it must NOT be mistaken for a v2 session tmp, which is owned by the
	// upload registry and is never decided by age.
	if isUploadTmpPath(SingleShotUploadTmpPath("/DATA/Media/holiday.mkv")) {
		t.Fatal("a single-shot leftover must not match the v2 session registry shape")
	}
}

func TestIsSingleShotUploadTmpName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"holiday.mkv.upload-1754870400123456789.tmp", true}, // 19 digits: a real UnixNano
		{"a.upload-1234567890123456.tmp", true},              // 16 digits: the lower bound
		{"a.upload-12345678901234567890.tmp", true},          // 20 digits: the upper bound
		{"a.upload-123456789012345.tmp", false},              // 15 digits: too short
		{"a.upload-123456789012345678901.tmp", false},        // 21 digits: too long
		// The v2 session tmp. Eight characters can never be sixteen, which is
		// exactly why the registry keeps ownership of these.
		{"holiday.mkv.upload-1a2b3c4d.tmp", false},
		{"holiday.mkv.upload-12345678.tmp", false},
		{"a.upload-17548704001234567ab.tmp", false}, // not all digits
		{".upload-1754870400123456789.tmp", false},  // no destination in front
		{"upload-1754870400123456789.tmp", false},   // no marker separator
		{"holiday.mkv.upload-1754870400123456789", false},
		{"holiday.mkv.tmp", false},
		{"", false},
		// A folder upload's chunk directory is numeric, never this.
		{"0", false},
	}
	for _, tc := range cases {
		if got := isSingleShotUploadTmpName(tc.name); got != tc.want {
			t.Errorf("isSingleShotUploadTmpName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSweepCollectsSingleShotUploadLeftovers(t *testing.T) {
	root := t.TempDir()
	twoDays := time.Now().Add(-48 * time.Hour)
	twoHours := time.Now().Add(-2 * time.Hour)

	write := func(name string, ts time.Time) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("partial upload"), 0o644); err != nil {
			t.Fatal(err)
		}
		lchtimes(t, p, ts)
		return p
	}

	abandoned := write("holiday.mkv.upload-1754870400123456789.tmp", twoDays)
	// Two hours old: a 2 GiB single-shot upload over a bad link is still a
	// plausible live transfer, and there is no registry to ask. Only the age
	// margin stands between us and a file the user is still sending.
	inFlight := write("wedding.mkv.upload-1754870400987654321.tmp", twoHours)
	// Owned by the v2 upload registry (upload_sessions.go): its fate is
	// decided by the bitmap under the service's lock, never by this walker.
	v2Session := write("photos.zip.upload-1a2b3c4d.tmp", twoDays)
	// Shapes a user could plausibly own.
	userFile := write("notes.upload-12345.tmp", twoDays)
	plainTmp := write("scratch.tmp", twoDays)

	// A symlink wearing the exact shape: removing it would destroy a name the
	// user can see in the file manager, and nothing our upload code writes is
	// ever a link.
	link := filepath.Join(root, "decoy.mkv.upload-1754870400111111111.tmp")
	if err := os.Symlink(filepath.Join(root, "notes.upload-12345.tmp"), link); err != nil {
		t.Fatal(err)
	}
	lchtimes(t, link, twoDays)

	SweepOrphanTempDirs([]string{root})

	if _, err := os.Lstat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned single-shot leftover should have been swept (err=%v)", err)
	}
	for _, keep := range []string{inFlight, v2Session, userFile, plainTmp, link} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%q is not ours to delete: %v", keep, err)
		}
	}
}

// A copy stub that is not a regular file is left alone for the same reason: an
// `os.Remove` on a link named `movie.mkv.casanova-tmp` destroys a user-visible
// name and frees nothing, and there is no trash behind this sweeper.
func TestSweepLeavesIrregularCopyStubs(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)

	target := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(target, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "movie.mkv.casanova-tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lchtimes(t, link, old)

	real := filepath.Join(root, "other.mkv.casanova-tmp")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lchtimes(t, real, old)

	SweepOrphanTempDirs([]string{root})

	if _, err := os.Lstat(link); err != nil {
		t.Errorf("a symlinked copy stub must survive: %v", err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("the link's target must survive: %v", err)
	}
	if _, err := os.Lstat(real); !os.IsNotExist(err) {
		t.Errorf("a real stale copy stub should have been swept (err=%v)", err)
	}
}

// ---------------------------------------------------------------------------
// On-demand sweep (SweepScratch)
// ---------------------------------------------------------------------------

// scratchFixture lays down one of each shape the sweeper owns and returns the
// paths, all dated well outside every margin.
func scratchFixture(t *testing.T) (root, group, shell, stub, uploadTmp string) {
	t.Helper()
	root = t.TempDir()
	old := time.Now().Add(-72 * time.Hour)

	media := filepath.Join(root, "Media")
	shell = filepath.Join(media, ".temp")
	group = filepath.Join(shell, groupName)
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, "0"), []byte("chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub = filepath.Join(media, "movie.mkv.casanova-tmp")
	if err := os.WriteFile(stub, []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploadTmp = filepath.Join(media, "photo.jpg.upload-1754870400123456789.tmp")
	if err := os.WriteFile(uploadTmp, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(group, "0"), group, stub, uploadTmp, shell, media} {
		lchtimes(t, p, old)
	}
	return root, group, shell, stub, uploadTmp
}

func TestSweepScratchDryRunPlansWithoutTouchingDisk(t *testing.T) {
	root, group, shell, stub, uploadTmp := scratchFixture(t)

	plan := SweepScratch(SweepOptions{Roots: []string{root}, DryRun: true})

	for _, p := range []string{group, shell, stub, uploadTmp} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("a dry run must not delete anything, %q is gone: %v", p, err)
		}
	}
	if plan.Dirs != 2 || plan.Stubs != 1 || plan.UploadTmps != 1 {
		t.Errorf("plan = %d dirs / %d stubs / %d upload tmps, want 2/1/1", plan.Dirs, plan.Stubs, plan.UploadTmps)
	}
	// 5 bytes of chunk + 2 of stub + 3 of upload tmp. The `.temp` shell is a
	// directory and contributes nothing, which is the honest answer.
	if plan.Bytes != 10 {
		t.Errorf("plan.Bytes = %d, want 10", plan.Bytes)
	}
	if plan.Failed != 0 {
		t.Errorf("plan.Failed = %d, want 0", plan.Failed)
	}
	for _, it := range plan.Items {
		if it.Removed {
			t.Errorf("%q is marked Removed on a dry run", it.Path)
		}
	}
	if got := plan.Paths(); len(got) != 4 {
		t.Errorf("plan.Paths() = %v, want the 4 planned entries", got)
	}

	// The plan a dry run produces is the plan a real run executes.
	run := SweepScratch(SweepOptions{Roots: []string{root}})
	if run.Dirs != plan.Dirs || run.Stubs != plan.Stubs || run.UploadTmps != plan.UploadTmps || run.Bytes != plan.Bytes {
		t.Errorf("real run %+v does not match the plan %+v", run, plan)
	}
	for _, p := range []string{group, shell, stub, uploadTmp} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%q should have been swept (err=%v)", p, err)
		}
	}
	for _, it := range run.Items {
		if !it.Removed {
			t.Errorf("%q is not marked Removed after a real run", it.Path)
		}
	}
}

func TestSweepScratchOnlyHonoursThePlanTheUserApproved(t *testing.T) {
	root, group, shell, stub, uploadTmp := scratchFixture(t)

	// The user ticked the copy stub and nothing else.
	rep := SweepScratch(SweepOptions{Roots: []string{root}, Only: map[string]bool{stub: true}})

	if _, err := os.Lstat(stub); !os.IsNotExist(err) {
		t.Errorf("the approved item should have been swept (err=%v)", err)
	}
	for _, keep := range []string{group, uploadTmp} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%q was not approved and must survive: %v", keep, err)
		}
	}
	// An unapproved group keeps `.temp` alive: as far as this run is
	// concerned the shell is still in use.
	if _, err := os.Lstat(shell); err != nil {
		t.Errorf(".temp still holds an unapproved group and must survive: %v", err)
	}
	if rep.Dirs != 0 || rep.Stubs != 1 || rep.UploadTmps != 0 {
		t.Errorf("report = %d dirs / %d stubs / %d upload tmps, want 0/1/0", rep.Dirs, rep.Stubs, rep.UploadTmps)
	}
}

// The age margins are one-way. A caller asking for a shorter one is describing
// a bug — and the file it would reach is an upload that is still streaming.
func TestSweepScratchIgnoresShorterMargins(t *testing.T) {
	root := t.TempDir()
	recent := time.Now().Add(-10 * time.Minute)

	media := filepath.Join(root, "Media")
	group := filepath.Join(media, ".temp", groupName)
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, "0"), []byte("chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(media, "movie.mkv.upload-1754870400123456789.tmp")
	if err := os.WriteFile(stub, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(group, "0"), group, filepath.Join(media, ".temp"), stub} {
		lchtimes(t, p, recent)
	}

	rep := SweepScratch(SweepOptions{
		Roots:            []string{root},
		MinAge:           time.Minute,
		SingleShotMinAge: time.Minute,
	})
	if len(rep.Items) != 0 {
		t.Errorf("a shorter margin was honoured and reached %v", rep.Paths())
	}
	if _, err := os.Lstat(group); err != nil {
		t.Errorf("a 10-minute-old chunk group must survive: %v", err)
	}
	if _, err := os.Lstat(stub); err != nil {
		t.Errorf("a 10-minute-old single-shot tmp must survive: %v", err)
	}
}

func TestSweepScratchRefusesUnsafeRoots(t *testing.T) {
	// "/" would put a name-shape-driven RemoveAll in charge of the whole box,
	// and a relative root resolves against whatever the process CWD happens to
	// be. Neither is ever a storage root.
	for _, root := range []string{"/", "relative/dir", ""} {
		rep := SweepScratch(SweepOptions{Roots: []string{root}, DryRun: true})
		if len(rep.Roots) != 0 || len(rep.Items) != 0 {
			t.Errorf("root %q was accepted: roots=%v items=%v", root, rep.Roots, rep.Items)
		}
	}
}

func TestSweepScratchDeduplicatesOverlappingRoots(t *testing.T) {
	root, _, _, _, _ := scratchFixture(t)
	media := filepath.Join(root, "Media")

	// Two roots covering the same tree must not count the same leftover twice:
	// a report that inflates the byte total is a report that promises space it
	// cannot deliver.
	plan := SweepScratch(SweepOptions{Roots: []string{root, media}, DryRun: true})
	if plan.Bytes != 10 || len(plan.Items) != 4 {
		t.Errorf("overlapping roots double-counted: bytes=%d items=%d", plan.Bytes, len(plan.Items))
	}
}

// A mount point nested inside a storage root gets its own depth budget, which
// is the whole reason an on-demand run asks the kernel what is mounted instead
// of walking three fixed strings: on this box /mnt holds two ext4 disks, and a
// leftover a few levels inside one of them is past /mnt's budget.
func TestSweepScratchGivesEachRootItsOwnDepthBudget(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-72 * time.Hour)

	disk := filepath.Join(root, "l1", "l2", "l3")
	deep := filepath.Join(disk, "d1", "d2", "d3")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(deep, "movie.mkv.upload-1754870400123456789.tmp")
	if err := os.WriteFile(leftover, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	lchtimes(t, leftover, old)

	if rep := SweepScratch(SweepOptions{Roots: []string{root}, DryRun: true}); len(rep.Items) != 0 {
		t.Errorf("nothing past the depth budget is reachable from the storage root, got %v", rep.Paths())
	}

	rep := SweepScratch(SweepOptions{Roots: []string{root, disk}, DryRun: true})
	if len(rep.Items) != 1 || rep.Items[0].Path != leftover {
		t.Errorf("with the mount point as its own root, want [%s], got %v", leftover, rep.Paths())
	}
}

func TestLocalSweepRoots(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "/" and relative paths are refused, what does not exist (or is not a
	// directory) is dropped, and an exact duplicate appears once. Nesting is
	// kept: SweepScratch walks each subtree from its most specific root.
	got := LocalSweepRoots([]string{dir, sub, dir, "/", "", "relative", filepath.Join(dir, "missing"), file})
	if len(got) != 2 || got[0] != dir || got[1] != sub {
		t.Errorf("LocalSweepRoots = %v, want [%s %s]", got, dir, sub)
	}
}

func TestIsChunkGroupName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"d41d8cd98f00b204e9800998ecf8427e1", true},   // digest + 1 chunk
		{"d41d8cd98f00b204e9800998ecf8427e128", true}, // digest + 128 chunks
		{"d41d8cd98f00b204e9800998ecf8427e", false},   // digest with no count
		{"", false},
		{"D41D8CD98F00B204E9800998ECF8427E12", false}, // uppercase is not ours
		{"d41d8cd98f00b204e9800998ecf8427g12", false}, // 'g' is not hex
		{"d41d8cd98f00b204e9800998ecf8427e-12", false},
		{"Vacation 2024 Photos From The Trip", false}, // right length, wrong alphabet
		{"..", false},
	}
	for _, tc := range cases {
		if got := isChunkGroupName(tc.name); got != tc.want {
			t.Errorf("isChunkGroupName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsNumericName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"0", true},
		{"1024", true},
		{"", false},
		{"1.part", false},
		{"-1", false},
		{"1 ", false},
		{"０", false}, // full-width digit, not an ASCII chunk index
	}
	for _, tc := range cases {
		if got := isNumericName(tc.name); got != tc.want {
			t.Errorf("isNumericName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- fixture helpers -------------------------------------------------------

type stamp struct {
	path  string
	fresh bool
}

func buildEntries(t *testing.T, parent string, entries []entry, stamps *[]stamp) {
	t.Helper()
	for _, e := range entries {
		p := filepath.Join(parent, e.name)
		switch e.kind {
		case entryDir:
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			buildEntries(t, p, e.children, stamps)
		case entrySymlink:
			if err := os.Symlink(e.target, p); err != nil {
				t.Fatal(err)
			}
		case entryFifo:
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Skipf("cannot create a FIFO under %s: %v", parent, err)
			}
		default:
			if err := os.WriteFile(p, []byte("chunk"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		*stamps = append(*stamps, stamp{path: p, fresh: e.fresh})
	}
}

// lchtimes back-dates a node WITHOUT following symlinks. os.Chtimes follows,
// so it would stamp a link's target and leave the link itself dated now — and
// since the sweeper reads a symlink's own mtime (DirEntry.Info is an lstat),
// such a fixture would be rejected by the age guard and never reach the
// symlink guard it is meant to pin.
func lchtimes(t *testing.T, path string, ts time.Time) {
	t.Helper()
	spec := unix.NsecToTimespec(ts.UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{spec, spec}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatalf("lchtimes %q: %v", path, err)
	}
}

// nest returns a chain of `depth` nested directories with one numeric chunk
// file at the bottom, i.e. the deepest level chunksAllStale has to recurse to.
func nest(depth int) []entry {
	level := []entry{{name: "0"}}
	for i := depth; i > 0; i-- {
		level = []entry{{name: "d" + strconv.Itoa(i), kind: entryDir, children: level}}
	}
	return level
}

// nestPath is the path nest(depth) creates, relative to the group directory.
func nestPath(depth int) []string {
	parts := make([]string, 0, depth+1)
	for i := 1; i <= depth; i++ {
		parts = append(parts, "d"+strconv.Itoa(i))
	}
	return append(parts, "0")
}
