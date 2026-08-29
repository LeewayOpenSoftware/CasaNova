package v1

import "testing"

func TestCompareBuildVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign
	}{
		// same base, same-day build: 16:44 newer than 09:07
		{"18.6.1.dev1.1707261644", "18.6.1.dev1.1707260907", 1},
		{"18.6.1.dev1.1707260907", "18.6.1.dev1.1707261644", -1},
		// identical
		{"18.6.1.dev1.1707261644", "18.6.1.dev1.1707261644", 0},
		// semver dominates the timestamp
		{"18.6.0.dev1.9999261644", "18.6.1.dev1.0101260000", -1},
		{"18.7.0.dev1.0101260000", "18.6.9.dev1.9999261644", 1},
		// cross-month: Aug 1 newer than Jul 31 (DDMMYY not numerically monotonic)
		{"18.6.1.dev1.0108261200", "18.6.1.dev1.3107261200", 1},
		// v-prefix tolerated
		{"v18.6.1.dev1.1707261644", "18.6.1.dev1.1707261644", 0},
	}
	for _, c := range cases {
		got := compareBuildVersions(c.a, c.b)
		if sign(got) != c.want {
			t.Errorf("compareBuildVersions(%q,%q)=%d want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsVersionNewer(t *testing.T) {
	// A same-or-older cached package is NOT an update (the reported bug).
	if isVersionNewer("18.6.1.dev1.1707260907", "18.6.1.dev1.1707261644") {
		t.Error("older cached package wrongly reported as newer")
	}
	if !isVersionNewer("18.7.0.dev1.0101260000", "18.6.1.dev1.9999261644") {
		t.Error("higher base version not reported as newer")
	}
}

func sign(n int) int {
	if n > 0 {
		return 1
	}
	if n < 0 {
		return -1
	}
	return 0
}
