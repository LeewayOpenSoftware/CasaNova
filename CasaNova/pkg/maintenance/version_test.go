package maintenance

import "testing"

// The same table as route/v1/update_version_test.go, run against the copy this
// package exposes. If the two ever disagree, the maintenance screen and the
// updater disagree about which tarball is older — which is exactly the bug
// that let a `touch` resurrect a superseded package.

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
		// cross-month: Aug 1 newer than Jul 31 (DDMMYY is not monotonic)
		{"18.6.1.dev1.0108261200", "18.6.1.dev1.3107261200", 1},
		// v-prefix tolerated
		{"v18.6.1.dev1.1707261644", "18.6.1.dev1.1707261644", 0},
		// the ordering a lexicographic compare gets backwards
		{"5.0.0.dev18.1306261034", "18.6.2.dev1.0808261615", -1},
	}
	for _, c := range cases {
		if got := sign(CompareBuildVersions(c.a, c.b)); got != c.want {
			t.Errorf("CompareBuildVersions(%q,%q) sign = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsVersionNewer(t *testing.T) {
	if IsVersionNewer("18.6.1.dev1.1707260907", "18.6.1.dev1.1707261644") {
		t.Error("an older package was reported as newer")
	}
	if !IsVersionNewer("18.7.0.dev1.0101260000", "18.6.1.dev1.9999261644") {
		t.Error("a higher base version was not reported as newer")
	}
}

// Only names that match the canonical package regexp AND carry an orderable
// version are ever deletion candidates: a file we cannot place in the version
// order can never be proven superseded.
func TestIsReleasePackageName(t *testing.T) {
	cases := []struct {
		name    string
		version string
		ok      bool
	}{
		{"casanova-18.6.2.dev1.0808261615-x86_64.tar.gz", "18.6.2.dev1.0808261615", true},
		{"casanova-5.0.0.dev10.1204260108-x86_64.tar.gz", "5.0.0.dev10.1204260108", true},
		{"changelog.json", "", false},
		{"casanova-x86_64.tar.gz", "", false},        // no parseable version
		{"somebody-elses-archive.tar.gz", "", false}, // not our artefact
		{"casanova-18.6.2.tar.gz.part", "", false},   // a stub, handled elsewhere
	}
	for _, c := range cases {
		v, ok := isReleasePackageName(c.name)
		if ok != c.ok || v != c.version {
			t.Errorf("isReleasePackageName(%q) = (%q,%v), want (%q,%v)", c.name, v, ok, c.version, c.ok)
		}
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
