package devid

import (
	"errors"
	"strings"
	"testing"
)

// zpoolFixture returns a ZpoolStatus func serving canned output per pool, and a
// pointer to the list of pools it was ASKED about.
//
// The asked list is what makes the over-correction control an assertion rather
// than a hope: a probe that resolved every imported pool would still report the
// right disks for the root pool, and only the record of what it queried shows
// that the data pool was left alone.
func zpoolFixture(out map[string]string) (func(string) ([]byte, error), *[]string) {
	asked := &[]string{}
	return func(pool string) ([]byte, error) {
		*asked = append(*asked, pool)
		text, ok := out[pool]
		if !ok {
			// What the real command does for a pool that is not imported.
			return nil, errors.New("cannot open '" + pool + "': no such pool")
		}
		return []byte(text), nil
	}, asked
}

// Verbatim shapes from `zpool status -P`, header and errors line included,
// because the header and the tree rows are exactly what the parser has to walk
// past and a trimmed fixture would not prove it does.
const mirrorStatus = `  pool: rpool
 state: ONLINE
  scan: scrub repaired 0B in 00:03:12 with 0 errors on Sun Sep  6 03:12:44 2026
config:

	NAME                        STATE     READ WRITE CKSUM
	rpool                       ONLINE       0     0     0
	  mirror-0                  ONLINE       0     0     0
	    /dev/sda3               ONLINE       0     0     0
	    /dev/sdb3               ONLINE       0     0     0

errors: No known data errors
`

const raidzStatus = `  pool: rpool
 state: ONLINE
config:

	NAME               STATE     READ WRITE CKSUM
	rpool              ONLINE       0     0     0
	  raidz1-0         ONLINE       0     0     0
	    /dev/sdc1      ONLINE       0     0     0
	    /dev/sdd1      ONLINE       0     0     0
	    /dev/sde1      ONLINE       0     0     0
	logs
	  /dev/nvme0n1p2   ONLINE       0     0     0
	cache
	  /dev/nvme0n1p3   ONLINE       0     0     0

errors: No known data errors
`

// THE LOCKOUT, in the form that stops the operator working. A ZFS root is named
// in the mount table by DATASET -- rpool/ROOT/ubuntu_a1b2c3 -- which is not a
// device path, has no node in /sys/block and carries no partition rule. The
// block-device walk rejected it, the probe answered unknown, and unknown
// refuses: with / on a pool, every disk in the machine was protected, including
// the blank one just plugged in. Safe, and unusable.
func TestZFSMirrorRootProtectsBothDisks(t *testing.T) {
	res := fixtureTree(t, nil, nil)
	zpool, asked := zpoolFixture(map[string]string{"rpool": mirrorStatus})
	res.ZpoolStatus = zpool

	root := ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/ubuntu_a1b2c3", "zfs")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved -- a ZFS root is still not being resolved (reason: %s)", root.Status, root.Reason)
	}
	if got := strings.Join(root.Disks, ","); got != "/dev/sda,/dev/sdb" {
		t.Fatalf("disks = %v, want [/dev/sda /dev/sdb]", root.Disks)
	}
	// Both halves of a mirror carry the root filesystem. Protecting one and
	// releasing the other is how a two-disk box loses its boot pool in one
	// command while the guard reports it did its job.
	for _, dev := range []string{"/dev/sda", "/dev/sda3", "/dev/sdb", "/dev/sdb3"} {
		if !root.Protects(dev) {
			t.Errorf("formatting %q was ALLOWED while / is on a mirror over /dev/sda3 and /dev/sdb3 -- that destroys the boot pool", dev)
		}
	}
	// THE ASSERTION THAT MAKES THE RED PROBE HONEST. An unresolved root
	// protects EVERYTHING, so every check above would still pass with the ZFS
	// path removed. This is the one that cannot.
	if root.Protects("/dev/sdz") {
		t.Error("/dev/sdz was refused although it is in no pool -- the probe is protecting by ignorance again, not by name")
	}
	if len(*asked) != 1 || (*asked)[0] != "rpool" {
		t.Errorf("pools queried = %v, want exactly [rpool]", *asked)
	}
}

// A raidz vdev has no upper bound on members, and a pool can carry separate log
// and cache devices alongside it. Every device the pool names has to come back.
func TestZFSRaidzRootProtectsEveryMemberIncludingLogAndCache(t *testing.T) {
	res := fixtureTree(t, nil, nil)
	zpool, _ := zpoolFixture(map[string]string{"rpool": raidzStatus})
	res.ZpoolStatus = zpool

	root := ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/default", "zfs")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if got := strings.Join(root.Disks, ","); got != "/dev/nvme0n1,/dev/sdc,/dev/sdd,/dev/sde" {
		t.Fatalf("disks = %v, want the three raidz members plus the nvme carrying log and cache", root.Disks)
	}
	for _, dev := range []string{"/dev/sdc", "/dev/sdc1", "/dev/sdd", "/dev/sde"} {
		if !root.Protects(dev) {
			t.Errorf("formatting raidz member %q was ALLOWED -- losing one member of a raidz1 while another is resilvering ends the pool", dev)
		}
	}
	// A separate log device holds writes the pool has already acknowledged.
	// Formatting it loses data that / believes is committed, so it is not a
	// "cache disk the operator may reuse".
	if !root.Protects("/dev/nvme0n1p2") {
		t.Error("formatting the pool's separate LOG device was ALLOWED -- it holds acknowledged writes not yet committed to the raidz")
	}
	if root.Protects("/dev/sdz") {
		t.Error("/dev/sdz was refused although it is in no pool")
	}
}

// OVER-CORRECTION CONTROL. The disks an operator reformats are data-pool disks.
// Resolving every imported pool would report the root pool correctly and still
// refuse those, turning this card's lockout into a narrower one.
func TestZFSRootDoesNotProtectADataPoolsDisks(t *testing.T) {
	const tankStatus = `  pool: tank
 state: ONLINE
config:

	NAME            STATE     READ WRITE CKSUM
	tank            ONLINE       0     0     0
	  mirror-0      ONLINE       0     0     0
	    /dev/sdc    ONLINE       0     0     0
	    /dev/sdd    ONLINE       0     0     0

errors: No known data errors
`
	res := fixtureTree(t, nil, nil)
	zpool, asked := zpoolFixture(map[string]string{
		"rpool": mirrorStatus,
		"tank":  tankStatus,
		// What `zpool status -P` prints with NO pool argument: every imported
		// pool, one section after another. It is in the fixture so that a
		// probe which dropped the pool argument gets the answer the real
		// command would give it -- both pools' disks, silently merged --
		// instead of an error that would hide the over-correction behind a
		// refusal.
		"": mirrorStatus + "\n" + tankStatus,
	})
	res.ZpoolStatus = zpool

	root := ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/ubuntu_a1b2c3", "zfs")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	for _, dev := range []string{"/dev/sdc", "/dev/sdd"} {
		if root.Protects(dev) {
			t.Errorf("formatting %q was REFUSED; it belongs to the data pool tank, not to the root pool, and refusing it is the lockout this card was opened to remove", dev)
		}
	}
	if len(*asked) != 1 || (*asked)[0] != "rpool" {
		t.Errorf("pools queried = %v, want exactly [rpool] -- querying every pool is what protects a data pool's disks by accident", *asked)
	}
}

// A pool built on LUKS containers goes through the same stacking walk that
// already handles an LVM or md root, rather than through a second copy of it.
func TestZFSRootOnLUKSWalksToTheDisksUnderneath(t *testing.T) {
	res := fixtureTree(t,
		map[string]string{"dev/mapper/luks-9f2a": "dev/dm-0"},
		map[string][]string{"dm-0": {"sda3"}},
	)
	zpool, _ := zpoolFixture(map[string]string{"rpool": `  pool: rpool
 state: ONLINE
config:

	NAME                        STATE     READ WRITE CKSUM
	rpool                       ONLINE       0     0     0
	  /dev/mapper/luks-9f2a     ONLINE       0     0     0

errors: No known data errors
`})
	res.ZpoolStatus = zpool

	root := ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/default", "zfs")), res)

	if root.Status != RootOnDisks {
		t.Fatalf("status = %s, want resolved (reason: %s)", root.Status, root.Reason)
	}
	if strings.Join(root.Disks, ",") != "/dev/sda" {
		t.Fatalf("disks = %v, want [/dev/sda]", root.Disks)
	}
	if !root.Protects("/dev/sda") {
		t.Error("formatting /dev/sda was ALLOWED while / is on a ZFS pool inside a LUKS container on it")
	}
}

// NEVER FAIL OPEN. Everything below is a way for the pool query to come back
// useless, and every one of them has to refuse. A machine whose / is a zfs
// mount and which cannot say what the pool is on is a machine this probe cannot
// answer for, and answering "no disks" would allow the format.
func TestZFSRootThatCannotBeResolvedStillRefuses(t *testing.T) {
	cases := []struct {
		name   string
		zpool  func(string) ([]byte, error)
		source string
		want   string // substring the reason has to carry
	}{
		{
			name: "zpool is not installed or the pool is not imported",
			zpool: func(string) ([]byte, error) {
				return nil, errors.New(`exec: "zpool": executable file not found in $PATH`)
			},
			source: "rpool/ROOT/default",
			want:   "could not be listed",
		},
		{
			name:   "zpool returns nothing at all",
			zpool:  func(string) ([]byte, error) { return nil, nil },
			source: "rpool/ROOT/default",
			want:   "reported no devices",
		},
		{
			name: "zpool prints a tree with no device paths",
			zpool: func(string) ([]byte, error) {
				return []byte("  pool: rpool\n state: UNAVAIL\nstatus: One or more devices could not be opened.\nconfig:\n\n\tNAME     STATE\n\trpool    UNAVAIL\n"), nil
			},
			source: "rpool/ROOT/default",
			want:   "reported no devices",
		},
		{
			name: "a vdev is a file, not a device node",
			zpool: func(string) ([]byte, error) {
				return []byte("config:\n\n\trpool           ONLINE\n\t  /srv/zfs/disk0  ONLINE\n"), nil
			},
			source: "rpool/ROOT/default",
			want:   "could not be determined",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := fixtureTree(t, nil, nil)
			res.ZpoolStatus = tc.zpool

			root := ProbeRootFrom(strings.NewReader(mountsRoot(tc.source, "zfs")), res)

			if root.Status != RootUnknown {
				t.Fatalf("status = %s, want unknown -- an unanswerable pool query must refuse, not resolve", root.Status)
			}
			// Unknown protects everything; that is the point.
			if !root.Protects("/dev/sda") || !root.Protects("/dev/sdz") {
				t.Error("a disk was ALLOWED although the root pool could not be resolved -- this is the fail-open this package exists to prevent")
			}
			if !strings.Contains(root.Reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q; the operator has to be told the answer was \"I cannot tell\"", root.Reason, tc.want)
			}
			if !strings.Contains(root.Reason, "rpool") {
				t.Errorf("reason = %q, want it to name the pool", root.Reason)
			}
		})
	}
}

// A nil resolver is the caller that did not supply the lookups. It refuses, and
// it must do so without reaching for a command.
func TestZFSRootWithNoResolverRefuses(t *testing.T) {
	root := ProbeRootFrom(strings.NewReader(mountsRoot("rpool/ROOT/default", "zfs")), nil)

	if root.Status != RootUnknown {
		t.Fatalf("status = %s, want unknown", root.Status)
	}
	if !root.Protects("/dev/sda") {
		t.Error("a disk was ALLOWED with no resolver to list the pool's disks")
	}
	if !strings.Contains(root.Reason, "no resolver") {
		t.Errorf("reason = %q, want it to say no resolver was given", root.Reason)
	}
}

func TestPoolNameTakesTheFirstComponent(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"rpool/ROOT/ubuntu_a1b2c3", "rpool"},
		{"rpool/ROOT", "rpool"},
		{"rpool", "rpool"},
		{"tank-01/root", "tank-01"},
		{"", ""},
		{"/dev/sda2", ""}, // a path, not a dataset
		{"/", ""},
	} {
		if got := poolName(tc.source); got != tc.want {
			t.Errorf("poolName(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// The parser must survive the parts of the output that are not the tree, and
// must not invent devices out of them.
func TestParseZpoolStatusTakesOnlyAbsolutePaths(t *testing.T) {
	got := parseZpoolStatus([]byte(raidzStatus))
	want := "/dev/nvme0n1p2,/dev/nvme0n1p3,/dev/sdc1,/dev/sdd1,/dev/sde1"
	if strings.Join(got, ",") != want {
		t.Errorf("devices = %v, want %s", got, want)
	}
	// "errors:", "config:", "logs", "cache", the NAME header row and the
	// pool and vdev rows are all rows this must not read as devices.
	for _, dev := range got {
		if !strings.HasPrefix(dev, "/dev/") {
			t.Errorf("%q was read as a device path", dev)
		}
	}
}
