package v1

import (
	"fmt"
	"testing"

	psnet "github.com/shirou/gopsutil/v3/net"
)

// countingNetSources builds netInfoSources over a fixture that mirrors the
// measured box: 62 container veth pairs plus 2 physical NICs in
// /proc/net/dev, of which only the 2 physical ones are real interfaces.
// Every probe increments its own counter so the test can assert CALL COUNTS
// rather than wall-clock time — a clock budget is what made an earlier
// performance test flaky, a call count is exact and machine-independent.
func countingNetSources(vethCount int, physicalCalls, stateCalls, detailCalls *int) netInfoSources {
	counters := make([]psnet.IOCountersStat, 0, vethCount+2)
	counters = append(counters, psnet.IOCountersStat{
		Name: "enp4s0", BytesSent: 10, BytesRecv: 20, PacketsSent: 1, PacketsRecv: 2,
	})
	for i := 0; i < vethCount; i++ {
		counters = append(counters, psnet.IOCountersStat{Name: fmt.Sprintf("veth%04x", i)})
	}
	counters = append(counters, psnet.IOCountersStat{
		Name: "wlp3s0", BytesSent: 30, BytesRecv: 40, PacketsSent: 3, PacketsRecv: 4,
	})

	return netInfoSources{
		counters: func() []psnet.IOCountersStat { return counters },
		physicalNICs: func() []string {
			*physicalCalls++
			// GetNet's real return shape, trailing empty sentinel included.
			return []string{"enp4s0", "wlp3s0", ""}
		},
		state: func(name string) string {
			*stateCalls++
			return "up\n"
		},
		detail: func(name string) (string, string, int) {
			*detailCalls++
			return "192.0.2.10", "aa:bb:cc:dd:ee:ff", 1000
		},
	}
}

// The invariant: the physical-NIC listing — two os.ReadDir over /sys plus a
// sort, per call — runs exactly ONCE per request, no matter how many
// interfaces /proc/net/dev reports.
//
// The pre-fix /v1/sys/net called it inside the loop over those interfaces, so
// a box with 27 containers paid 64 listings (128 os.ReadDir, 6.0ms) where its
// already-hoisted sibling paid 1 (2 os.ReadDir, 0.13ms). The cost grew with
// the container count, and the caller controlled only the request rate.
func TestBuildNetInfoListsPhysicalNICsOncePerRequest(t *testing.T) {
	for _, vethCount := range []int{0, 8, 62, 240} {
		t.Run(fmt.Sprintf("veth=%d", vethCount), func(t *testing.T) {
			physicalCalls, stateCalls, detailCalls := 0, 0, 0
			out := buildNetInfo(countingNetSources(vethCount, &physicalCalls, &stateCalls, &detailCalls))

			if physicalCalls != 1 {
				t.Fatalf("physical NIC listing called %d times with %d veth interfaces, want exactly 1",
					physicalCalls, vethCount)
			}
			// The per-name probes take a name, so they can only run for
			// interfaces that survived the filter — never once per
			// /proc/net/dev entry.
			if stateCalls != 2 || detailCalls != 2 {
				t.Fatalf("per-NIC probes ran state=%d detail=%d, want 2 each (the physical NICs)",
					stateCalls, detailCalls)
			}
			if len(out) != 2 {
				t.Fatalf("payload has %d entries, want the 2 physical NICs", len(out))
			}
		})
	}
}

// The payload itself must not change: same order as the kernel counters, same
// fields, virtual interfaces filtered out, and the empty sentinel GetNet
// appends must stay unmatchable.
func TestBuildNetInfoPreservesPayloadShape(t *testing.T) {
	physicalCalls, stateCalls, detailCalls := 0, 0, 0
	src := countingNetSources(3, &physicalCalls, &stateCalls, &detailCalls)

	// An interface with an empty name must not be admitted by the sentinel.
	base := src.counters
	src.counters = func() []psnet.IOCountersStat {
		return append([]psnet.IOCountersStat{{Name: ""}}, base()...)
	}

	out := buildNetInfo(src)
	if len(out) != 2 {
		t.Fatalf("got %d entries, want 2 (the empty-named counter must not match the sentinel)", len(out))
	}
	if out[0].Name != "enp4s0" || out[1].Name != "wlp3s0" {
		t.Fatalf("order changed: %q, %q", out[0].Name, out[1].Name)
	}
	if out[0].BytesSent != 10 || out[0].BytesRecv != 20 || out[0].PacketsSent != 1 || out[0].PacketsRecv != 2 {
		t.Fatalf("counter fields not carried through: %+v", out[0])
	}
	if out[0].State != "up" {
		t.Fatalf("State = %q, want the trimmed operstate", out[0].State)
	}
	if out[0].IPv4 != "192.0.2.10" || out[0].MAC != "aa:bb:cc:dd:ee:ff" || out[0].Speed != 1000 {
		t.Fatalf("detail fields not carried through: %+v", out[0])
	}
	if out[0].Time == 0 {
		t.Fatal("Time was not stamped")
	}
}

// No physical NIC at all (a container or a stripped VM) must yield an empty
// list, not nil: the JSON payload has always been [] and the UI indexes it.
func TestBuildNetInfoReturnsEmptyListWhenNoPhysicalNICs(t *testing.T) {
	src := netInfoSources{
		counters: func() []psnet.IOCountersStat {
			return []psnet.IOCountersStat{{Name: "veth0"}, {Name: "docker0"}}
		},
		physicalNICs: func() []string { return []string{""} },
		state:        func(string) string { return "down" },
		detail:       func(string) (string, string, int) { return "", "", 0 },
	}
	out := buildNetInfo(src)
	if out == nil {
		t.Fatal("returned nil; the payload must serialise as [] not null")
	}
	if len(out) != 0 {
		t.Fatalf("got %d entries, want 0", len(out))
	}
}
