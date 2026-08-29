package storagemaint

import (
	"context"
	"strings"
	"testing"
)

// RecoverSanitizeJobs runs on EVERY service start, from
// InstallStorageMaintenanceEngine, against roots read off disk. Its body does
// authorization lookups, statfs and directory walks, so a root on a mount that
// has gone away blocks in the kernel. It took a context and never read it,
// which meant the deadline the caller sets bounds nothing.
//
// The property is not "it returns quickly" — a walk already inside a dead
// syscall cannot be hurried. It is that an EXPIRED context stops the loops
// before they enter the next one, and that the caller is told the pass was cut
// short rather than being handed a report that reads as complete.
func TestRecoveryStopsOnAnExpiredContextAndSaysSo(t *testing.T) {
	f := newSanitizeFixture(t)
	// A record to iterate over: an empty list would make the loops trivially
	// finish and the test would pass without exercising the stop at all.
	seedInterruptedJob(t, f, "beefbeefbeefbeefbeefbeefbeefbeef")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired when recovery is entered

	rep := RecoverSanitizeJobs(ctx)

	found := false
	for _, e := range rep.Errors {
		if strings.Contains(e, "recovery stopped early") {
			found = true
			if !strings.Contains(e, "recorded for the next start") {
				t.Errorf("the report does not say the unfinished roots were recorded: %q", e)
			}
		}
	}
	if !found {
		t.Fatalf("an expired context produced a report that reads as a completed "+
			"pass; errors were %v", rep.Errors)
	}
}
