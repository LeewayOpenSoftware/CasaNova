package maintenance

// The two-release-line scheme depends on one ordering property, and this
// pins it: a build from the development line must outrank every build from
// the release line, regardless of dates.
//
// Both lines are published through the same updater. release/26.10 stamps
// 18.6.2.beta<n>.<date>; dev stamps 19.0.0.dev2.<date>. If ordering were by
// date the two would interleave, and a 26.10 patch cut next March would be
// offered to a dev box as an upgrade — silently moving that machine off the
// line it is meant to be testing. Semver-before-timestamp is what prevents
// that, so it is worth a test rather than an assumption.

import "testing"

func TestReleaseLinesNeverInterleave(t *testing.T) {
	// A dev build from today vs release patches cut months later.
	dev := "19.0.0.dev2.3008261600"
	laterReleasePatches := []string{
		"18.6.2.beta1.3008261700", // an hour later
		"18.6.2.beta9.0103270000", // next March
		"18.6.2.st1.3112271200",   // the eventual stable, next December
	}
	for _, rel := range laterReleasePatches {
		if CompareBuildVersions(dev, rel) <= 0 {
			t.Errorf("release build %s ranks at or above dev build %s — the updater would offer "+
				"a 26.10 patch to a 27.04 box as an upgrade", rel, dev)
		}
	}
}

func TestChannelRenameIsNotADowngrade(t *testing.T) {
	// release/26.10 switches the channel token from dev to beta on the same
	// BASE_VERSION. The box currently runs a dev build; the first beta must
	// still be seen as newer, or the branch could not ship at all.
	installed := "18.6.2.dev1.3008261614"
	firstBeta := "18.6.2.beta1.3008261700"
	if CompareBuildVersions(firstBeta, installed) <= 0 {
		t.Fatalf("the first beta (%s) does not supersede the installed dev build (%s): "+
			"the channel token must carry no ordering weight", firstBeta, installed)
	}
}

func TestWithinALineTheTimestampDecides(t *testing.T) {
	// And the ordinary case still works: same line, later build wins.
	if CompareBuildVersions("18.6.2.beta1.3008261700", "18.6.2.beta1.3008261600") != 1 {
		t.Fatal("a later build on the same line did not rank above the earlier one")
	}
	// Across a month boundary, where the DDMMYY stamp is not numerically
	// monotonic — the reordering exists for exactly this.
	if CompareBuildVersions("18.6.2.beta1.0109260900", "18.6.2.beta1.3008262300") != 1 {
		t.Fatal("September 1st ranked below August 30th: the DDMMYY→YYMMDD reordering is broken")
	}
}
