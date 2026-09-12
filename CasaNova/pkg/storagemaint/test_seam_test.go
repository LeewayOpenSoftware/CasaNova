package storagemaint

// THE TEST SEAM ITSELF, pinned.
//
// This package reads the host through two independent indirections, and a
// fixture has to set BOTH or it is only half isolated:
//
//	blockdev.SetRoots(sys, proc, dev)  — the device inventory
//	SetSanitizeProcRoot(proc)          — this package's own mount-table reads
//
// They are separate because they answer different questions (see the comment
// on sanitizeMount), and SetSanitizeProcRoot's own doc says to pair it with
// blockdev.SetRoots "so the inventory and the mount table describe one world".
// fxNewHost set only the first, so readSanitizeMounts() opened the REAL
// /proc/self/mountinfo and reported the mounted devices of whoever ran the
// suite — while the fixture had already written a mountinfo into its fake root
// that nobody ever read.
//
// Nothing reachable from fxNewHost calls PlanSanitize today, so that gap was
// latent rather than a live failure. It is pinned here anyway: a half-set seam
// is a trap that goes off the first time a test in this file grows a call into
// the sanitize path, and the symptom then is a test naming a disk of the
// machine it runs on, which reads like flakiness rather than like a defect.

import (
	"os"
	"strings"
	"testing"
)

// hostMountedDevices reads the REAL mount table, deliberately bypassing the
// seam, so the assertions below can be about the difference between the two.
func hostMountedDevices(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("this host has no readable /proc/self/mountinfo (%v); the leak this pins cannot be observed here", err)
	}
	var devs []string
	for _, line := range strings.Split(string(data), "\n") {
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		right := strings.Fields(line[sep+3:])
		if len(right) >= 2 && strings.HasPrefix(right[1], "/dev/") {
			devs = append(devs, strings.TrimPrefix(right[1], "/dev/"))
		}
	}
	return devs
}

func TestFixtureMountTableDoesNotLeakTheHostsDisks(t *testing.T) {
	hostDevs := hostMountedDevices(t)
	// ANTI-VACUITY: if the host has no /dev-backed mounts at all, "the fixture
	// returned none of them" is true for free and proves nothing.
	if len(hostDevs) == 0 {
		t.Skip("no /dev-backed mounts on this host; the assertion below would be vacuous")
	}

	h := fxNewHost(t)
	h.addDisk(fxDisk{name: "sdz", size: 8 << 30})
	h.addPartition("sdz", "sdz1", 8<<30)
	h.mount("sdz1", "/mnt/lab")

	got, err := readSanitizeMounts()
	if err != nil {
		t.Fatalf("readSanitizeMounts with a fixture active: %v", err)
	}

	for _, m := range got {
		for _, hd := range hostDevs {
			if m.DeviceName == hd {
				t.Fatalf("the fixture's mount table contains %q, a device mounted on the MACHINE RUNNING THIS SUITE "+
					"(host devices: %v). blockdev.SetRoots was set but SetSanitizeProcRoot was not, so "+
					"readSanitizeMounts opened the real /proc/self/mountinfo.", m.DeviceName, hostDevs)
			}
		}
	}

	// POSITIVE CONTROL: isolation that returns nothing is not isolation, it is
	// a broken path. The fixture's own row must come back, which also proves
	// the reader is pointed at the fake root rather than at nowhere.
	var found bool
	for _, m := range got {
		if m.DeviceName == "sdz1" && m.Point == "/mnt/lab" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the fixture's own mount (sdz1 on /mnt/lab) is missing; readSanitizeMounts returned %+v", got)
	}
}
