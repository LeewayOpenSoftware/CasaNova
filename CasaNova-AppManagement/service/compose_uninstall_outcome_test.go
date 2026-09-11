package service

import (
	"os"
	"strings"
	"testing"
)

// The operation log recorded "app uninstalled successfully" whether or not the
// requested config-folder deletion actually removed anything — and for an
// ordinary app it removes nothing, because the sweep is confined to AppsPath
// while real volume sources live under /DATA/AppData/<name>. An operator
// reading that record was told their data was gone while it sat on disk.
//
// These pin the message, not the deletion. The deletion is deliberately
// unchanged.
func TestUninstallOutcomeDoesNotClaimADeletionThatDidNotHappen(t *testing.T) {
	msg := uninstallOutcome(true, 0)

	if !strings.Contains(msg, "NO config folder was deleted") {
		t.Fatalf("the caller asked for the data to be deleted and nothing was; the "+
			"operation log must say so. Got: %q", msg)
	}
	if !strings.Contains(msg, "still on disk") {
		t.Fatalf("the message must say where the data actually is, not merely that a "+
			"step was skipped. Got: %q", msg)
	}
}

// The other two outcomes must stay unqualified, or the warning above stops
// meaning anything: a message that always hedges is the same as one that never
// does.
func TestUninstallOutcomeStaysPlainWhenThereIsNothingToWarnAbout(t *testing.T) {
	cases := []struct {
		name      string
		requested bool
		removed   int
	}{
		{"data deletion never requested", false, 0},
		{"data deletion requested and performed", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := uninstallOutcome(tc.requested, tc.removed)
			if strings.Contains(msg, "NO config folder was deleted") ||
				strings.Contains(msg, "still on disk") {
				t.Fatalf("%s must not carry the did-not-delete warning. Got: %q", tc.name, msg)
			}
			if !strings.Contains(msg, "app uninstalled successfully") {
				t.Fatalf("%s must still read as a success. Got: %q", tc.name, msg)
			}
		})
	}
}

// A count that is never incremented would make the warning fire on every
// uninstall, including the ones that did delete something — the failure mode
// this change could introduce. Pin that the count reaches the message.
func TestUninstallOutcomeReportsTheCount(t *testing.T) {
	msg := uninstallOutcome(true, 3)
	if !strings.Contains(msg, "3 config folder(s) deleted") {
		t.Fatalf("the number of folders actually removed must reach the operation log. Got: %q", msg)
	}
}

// The tests above exercise a pure function. They would all still pass if the
// helper were never wired into Uninstall, or if the counter were never
// incremented — the function would simply be dead code reporting on nothing.
// Uninstall itself needs a live docker daemon, so the wiring is pinned at the
// source level instead. Anti-vacuity first: if the file cannot be read, or the
// anchors are gone, this fails rather than passing silently.
func TestUninstallOutcomeIsActuallyWiredIn(t *testing.T) {
	src, err := os.ReadFile("compose_app.go")
	if err != nil {
		t.Fatalf("cannot read compose_app.go: %v", err)
	}
	text := string(src)

	for _, want := range []string{
		// the completion message comes from the helper, not a literal
		"op.Complete(uninstallOutcome(deleteConfigFolder, configFoldersRemoved))",
		// and the counter is raised where a folder is genuinely removed
		"configFoldersRemoved++",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("compose_app.go no longer contains %q — the uninstall operation log "+
				"is reporting something other than what the sweep actually did", want)
		}
	}

	if strings.Contains(text, `op.Complete("app uninstalled successfully")`) {
		t.Error(`compose_app.go records the unconditional "app uninstalled successfully" again; ` +
			`that claims a data deletion that does not happen for an ordinary app`)
	}
}
