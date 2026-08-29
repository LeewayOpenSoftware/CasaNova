package service

import "testing"

// tagVersionNewer decides whether the catalog is offering a real upgrade. It
// replaced a plain `!=` comparison that reported "update available" for any
// differing tag — which meant a stale catalog silently DOWNGRADED an app that
// was already newer. That is the behaviour these cases pin down; until now the
// function had no direct coverage at all.
func TestTagVersionNewer(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		candidate string
		want      bool
	}{
		{"same version is not an upgrade", "1.23.1", "1.23.1", false},
		{"patch bump is an upgrade", "1.23.1", "1.23.2", true},
		{"minor bump is an upgrade", "1.23.1", "1.24.0", true},
		{"major bump is an upgrade", "1.23.1", "2.0.0", true},
		{"older catalog is not an upgrade", "1.23.1", "1.22.0", false},
		{"older major is not an upgrade", "2.0.0", "1.99.99", false},
		{"v prefix is tolerated on both sides", "v4.81.0", "v4.82.0", true},
		{"v prefix mixed with bare", "4.81.0", "v4.82.0", true},
		{"suffixed tags still compare by version", "2.43.0-alpine", "2.44.0-alpine", true},
		{"suffix does not make an older tag newer", "2.44.0-alpine", "2.43.0-alpine", false},
		{"shorter version is compared component-wise", "1.23", "1.23.1", true},
		{"trailing zeros do not fake an upgrade", "1.23.0", "1.23", false},
		// 10 > 9 numerically but "10" < "9" as a string: the whole reason
		// this compares parsed integers instead of tags.
		{"double-digit component beats single digit", "1.9.0", "1.10.0", true},
		{"single digit does not beat double digit", "1.10.0", "1.9.0", false},
		{"unparseable candidate is never an upgrade", "1.23.1", "test", false},
		{"unparseable current is never an upgrade", "test", "1.23.1", false},
		{"rolling tag is never an upgrade here", "latest", "latest", false},
		{"empty candidate is never an upgrade", "1.23.1", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tagVersionNewer(c.current, c.candidate); got != c.want {
				t.Fatalf("tagVersionNewer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
			}
		})
	}
}

// tagOf has to ignore a registry "host:port/" prefix, otherwise the port digits
// are read as the tag and every image from a private registry looks unversioned.
func TestTagOf(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"linuxserver/syncthing:1.23.1", "1.23.1"},
		{"linuxserver/syncthing", ""},
		{"nginx:latest", "latest"},
		{"nginx", ""},
		{"registry.example.com:5000/team/app:2.1.0", "2.1.0"},
		{"registry.example.com:5000/team/app", ""},
		{"", ""},
	}

	for _, c := range cases {
		t.Run(c.image, func(t *testing.T) {
			if got := tagOf(c.image); got != c.want {
				t.Fatalf("tagOf(%q) = %q, want %q", c.image, got, c.want)
			}
		})
	}
}
