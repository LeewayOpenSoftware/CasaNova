package service

import (
	"regexp"
	"strconv"
	"strings"
)

// Best-effort version comparison for Docker image tags, used to decide whether
// a store catalog tag is a genuine UPGRADE over the installed one. The previous
// logic flagged "update available" on ANY difference (currentTag != storeTag),
// which false-positives — and would DOWNGRADE — when the installed tag is newer
// than a stale catalog (e.g. installed portainer 2.43.0 vs catalog 2.31.3).

var versionTagRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)*)`)

// parseVersionTag extracts the leading numeric dotted version from a tag,
// tolerating a leading "v" and any suffix (e.g. "v4.81.0", "2.43.0-alpine").
// Returns (nums, true) only when a numeric version is present.
func parseVersionTag(tag string) ([]int, bool) {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "v")
	m := versionTagRe.FindStringSubmatch(tag)
	if m == nil {
		return nil, false
	}
	parts := strings.Split(m[1], ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil, false
	}
	return nums, true
}

func compareIntSlices(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// tagVersionNewer reports whether candidate is a strictly newer version than
// current. Returns false when EITHER tag is not a recognizable numeric version
// — so an ambiguous/non-semver change never produces a false "update available"
// nor a surprise downgrade. Callers fall back to the digest check for the
// rolling tags ("latest", …) that this intentionally doesn't handle.
func tagVersionNewer(current, candidate string) bool {
	cur, okc := parseVersionTag(current)
	cand, okd := parseVersionTag(candidate)
	if !okc || !okd {
		return false
	}
	return compareIntSlices(cand, cur) > 0
}

// tagOf returns the tag portion of an image reference (after the last colon
// that follows the last slash, so a registry "host:port/" prefix is ignored),
// or "" when the image is untagged.
func tagOf(image string) string {
	rest := image
	if s := strings.LastIndex(image, "/"); s >= 0 {
		rest = image[s+1:]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[i+1:]
	}
	return ""
}
