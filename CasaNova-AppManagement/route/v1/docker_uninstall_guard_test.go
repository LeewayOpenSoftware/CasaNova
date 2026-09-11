package v1

import "testing"

const testAppsPath = "/var/lib/casaos/apps"

// THE DEFECT THIS PINS. The v1 uninstall sweep confined its delete target with
//
//	candidate != appsPath && !strings.HasPrefix(candidate, appsPathPrefix)
//
// and that `!=` ADMITTED the apps root itself. The v2 sweep in
// service/compose_app.go has always required a strict prefix. Two copies of one
// guard differing by one character is how one of them ends up wrong.
//
// Deleting /var/lib/casaos/apps takes every installed app's working dir and
// compose file — every app becomes unmanageable at once.
//
// REACHABILITY, unrounded: not reachable today. Landing exactly on the root
// needs a container mount whose Source contains the apps path, and compose
// security refuses to bind /var/lib/casaos or any ancestor (pinned by
// service.TestComposeSecurity_BlocksBindingTheAppsTree). MEDIUM confidence
// rather than high, because the v1 path adopts containers whose mounts did not
// necessarily pass the v2 validator, and I did not trace every v1 import path.
// The guard should be right regardless of whether anything can reach it.
func TestTheAppsRootItselfIsNeverADeleteTarget(t *testing.T) {
	if withinAppsDir(testAppsPath, testAppsPath) {
		t.Fatal("the apps root itself was accepted as a delete target; RMDir would " +
			"take every installed app's working dir and compose file with it")
	}
	// The same thing spelled with a trailing separator, and un-cleaned.
	for _, candidate := range []string{
		testAppsPath + "/",
		"/var/lib/casaos/apps/.",
		"/var/lib/casaos/../casaos/apps",
	} {
		if withinAppsDir(candidate, testAppsPath) {
			t.Errorf("%q resolves to the apps root and was accepted", candidate)
		}
	}
}

// POSITIVE CONTROL. A guard that refuses everything would pass the test above
// while breaking the feature, so pin that real targets are still accepted —
// otherwise the assertion above is satisfied by a function that returns false.
func TestRealAppDirectoriesAreStillDeletable(t *testing.T) {
	for _, candidate := range []string{
		"/var/lib/casaos/apps/navidrome",
		"/var/lib/casaos/apps/some-app/config",
	} {
		if !withinAppsDir(candidate, testAppsPath) {
			t.Errorf("%q is a legitimate cleanup target and was refused; the sweep "+
				"would stop removing app working directories entirely", candidate)
		}
	}
}

// Everything outside the tree stays out, including the sibling-prefix shape
// that a separator-less HasPrefix would have let through.
func TestPathsOutsideTheAppsTreeAreRefused(t *testing.T) {
	for _, candidate := range []string{
		"/var/lib/casaos/appsX",       // sibling: passes a prefix check with no separator
		"/var/lib/casaos/appsX/thing", // and its children
		"/var/lib/casaos",             // the parent
		"/var/lib",                    // an ancestor
		"/etc",
		"relative/path",
	} {
		if withinAppsDir(candidate, testAppsPath) {
			t.Errorf("%q is outside the apps tree and was accepted as a delete target", candidate)
		}
	}
}

// The derivation is what makes the root reachable at all: ctr.Name carries a
// LEADING SLASH, so a container named "apps" splits /var/lib/casaos/apps/data
// at ".../casaos" and joins "/apps" back on — landing on the root exactly. This
// pins the arithmetic, so the containment test above is exercising a shape the
// code can really produce rather than a hypothetical string.
func TestAContainerNamedAppsDerivesTheAppsRoot(t *testing.T) {
	got := mountCleanupCandidate("/var/lib/casaos/apps/data", "/apps")
	if got != testAppsPath {
		t.Fatalf("derived %q, want %q — if the derivation changed, the guard test "+
			"above may no longer be pinning a reachable case", got, testAppsPath)
	}
	if withinAppsDir(got, testAppsPath) {
		t.Fatal("a container named \"apps\" derives the apps root AND the guard " +
			"accepted it — this is the exact deletion the strict prefix prevents")
	}
}

// An ordinary app derives its own directory, which must still be accepted.
func TestAnOrdinaryContainerDerivesItsOwnDirectory(t *testing.T) {
	got := mountCleanupCandidate("/var/lib/casaos/apps/navidrome/config", "/navidrome")
	want := "/var/lib/casaos/apps/navidrome"
	if got != want {
		t.Fatalf("derived %q, want %q", got, want)
	}
	if !withinAppsDir(got, testAppsPath) {
		t.Fatal("an ordinary app's own directory was refused")
	}
}
