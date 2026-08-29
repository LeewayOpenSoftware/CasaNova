package maintenance

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Build-version ordering.
//
// This is the comparator route/v1/update.go already uses (compareBuildVersions
// / isVersionNewer / extractVersionFromPackage), moved here UNCHANGED so the
// maintenance catalogue can reuse it: a package under pkg/ cannot import
// route/v1 without a cycle, and the one thing this module must not do is
// invent a second way to decide which tarball is older. See update_version_test
// .go for the cases that pin the behaviour — version_test.go in this package
// runs the same table.
//
// The two failure modes it exists to prevent are both real:
//   - lexicographic compare puts "5.0.0" above "18.6.2";
//   - mtime compare lets `touch` resurrect a superseded tarball, which is the
//     bug that made the updater offer a downgrade.
//
// Build versions look like "18.6.1.dev1.1707261644": MAJOR.MINOR.PATCH .
// <channel><num> . <timestamp>. Semver first, then the build timestamp. The
// Makefile stamps that timestamp as `date +%d%m%y%H%M` (DDMMYYHHMM), which is
// NOT numerically monotonic across month boundaries, so it is reordered to
// YYMMDDHHMM before comparing.

type buildVersion struct {
	nums []int // [18,6,1]
	ts   int64 // sortable YYMMDDHHMM, 0 if absent
}

func sortableTimestamp(s string) int64 {
	if len(s) == 10 { // DDMMYYHHMM → YYMMDDHHMM
		reordered := s[4:6] + s[2:4] + s[0:2] + s[6:8] + s[8:10]
		if n, err := strconv.ParseInt(reordered, 10, 64); err == nil {
			return n
		}
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func parseBuildVersion(v string) buildVersion {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	bv := buildVersion{}
	for _, p := range strings.Split(v, ".") {
		if p == "" {
			continue
		}
		if len(p) >= 8 { // the timestamp segment
			bv.ts = sortableTimestamp(p)
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			bv.nums = append(bv.nums, n) // a semver component
		}
		// a channel token like "dev1" carries no ordering weight → ignore
	}
	return bv
}

// CompareBuildVersions returns -1/0/1 for a<b / a==b / a>b.
func CompareBuildVersions(a, b string) int {
	pa, pb := parseBuildVersion(a), parseBuildVersion(b)
	for i := 0; i < len(pa.nums) || i < len(pb.nums); i++ {
		var x, y int
		if i < len(pa.nums) {
			x = pa.nums[i]
		}
		if i < len(pb.nums) {
			y = pb.nums[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if pa.ts != pb.ts {
		if pa.ts < pb.ts {
			return -1
		}
		return 1
	}
	return 0
}

// IsVersionNewer reports whether candidate is a genuine upgrade over running.
func IsVersionNewer(candidate, running string) bool {
	return CompareBuildVersions(candidate, running) > 0
}

// versionTokenRe matches a plausible version token — a leading digit or "v"
// followed by a digit, then dots / alphanumerics. Anything failing this is
// unparseable, so a malformed `casanova-x86_64.tar.gz` yields "unknown"
// instead of surfacing an arch marker as a fake version.
var versionTokenRe = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.]*$`)

// packageNameRe is update_disk.go's canonical package-name matcher: only a
// filename that matches it is treated as one of our release tarballs. A file
// somebody else dropped into the directory is not our artefact and is left
// alone — rule 2, whitelist not blacklist.
var packageNameRe = regexp.MustCompile(`^casanova-(.+?)(?:-([a-zA-Z0-9_]+))?\.tar\.gz$`)

// ExtractVersionFromPackage pulls the version out of a package filename
// (casanova-v5.0.0-x86_64.tar.gz → v5.0.0), or "unknown".
func ExtractVersionFromPackage(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	parts := strings.SplitN(base, "-", 3)
	if len(parts) >= 2 && versionTokenRe.MatchString(parts[1]) {
		return parts[1]
	}
	return "unknown"
}

// isReleasePackageName reports whether base is one of our release tarballs
// with a version we can actually order. Both halves are required: a name we
// cannot parse a version out of can never be proven superseded, so it is never
// a deletion candidate.
func isReleasePackageName(base string) (string, bool) {
	if packageNameRe.FindStringSubmatch(base) == nil {
		return "", false
	}
	v := ExtractVersionFromPackage(base)
	if v == "" || v == "unknown" {
		return "", false
	}
	return v, true
}
