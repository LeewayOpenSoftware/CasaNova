package v1

import "testing"

// A log viewer that quietly shows a different backlog than the caller asked
// for is indistinguishable from a container that has fewer lines than it does.
// parseLogTail must therefore hand back a notice for every value it changes.
func TestParseLogTail(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		want       int
		wantNotice bool
	}{
		{"default", "500", 500, false},
		{"zero is a legitimate request for no backlog", "0", 0, false},
		{"upper bound is accepted as-is", "10000", maxLogTail, false},
		{"above the bound is clamped and announced", "5000000", maxLogTail, true},
		{"garbage falls back and announces", "; rm -rf /", defaultLogTail, true},
		{"empty falls back and announces", "", defaultLogTail, true},
		{"negative falls back and announces", "-1", defaultLogTail, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, notice := parseLogTail(tc.raw)
			if got != tc.want {
				t.Errorf("parseLogTail(%q) = %d, want %d", tc.raw, got, tc.want)
			}
			if (notice != "") != tc.wantNotice {
				t.Errorf("parseLogTail(%q) notice = %q, wantNotice=%v", tc.raw, notice, tc.wantNotice)
			}
		})
	}
}
