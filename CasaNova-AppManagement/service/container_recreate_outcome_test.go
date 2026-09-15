package service

import (
	"errors"
	"testing"
)

// "The registry was unreachable" and "you are already up to date" must not be
// the same answer.
//
// They were: both left isImageUpdated false and returned a bare nil, and the
// v2 handler publishes AppUpdateError only when RecreateContainer returns an
// error. The user clicked Update, the progress completed, the container kept
// running the old image, and nothing said the pull never happened.
//
// Mutation that must fail this test: drop the pullErr branch and go back to
// `if !force && !isImageUpdated { return nil }`.
func TestAFailedPullIsNotReportedAsUpToDate(t *testing.T) {
	pullErr := errors.New("Get \"https://registry-1.docker.io/v2/\": dial tcp: i/o timeout")

	proceed, err := recreateOutcome(true, false, false, pullErr)

	if proceed {
		t.Error("a failed pull should not rebuild the container")
	}
	if err == nil {
		t.Fatal("a failed pull returned nil — indistinguishable from an image that was already current")
	}
	if !errors.Is(err, ErrImagePullFailed) {
		t.Errorf("err = %v; callers match on ErrImagePullFailed to tell this from a real recreate failure", err)
	}
	// The cause survives: "i/o timeout" and "unauthorized" are different
	// operator problems and the message is the only place that says which.
	if !errors.Is(err, ErrImagePullFailed) || !containsCause(err.Error(), "i/o timeout") {
		t.Errorf("the underlying pull error was lost: %v", err)
	}
}

// The other half of the pair, asserted at the same time so a guard that
// refuses everything cannot pass the test above. An image that really is
// current still reports success and still does nothing.
func TestAnImageThatIsAlreadyCurrentStillReportsSuccess(t *testing.T) {
	proceed, err := recreateOutcome(true, false, false, nil)

	if proceed {
		t.Error("nothing to update, yet the container would be rebuilt")
	}
	if err != nil {
		t.Errorf("an up-to-date image reported an error: %v", err)
	}
}

// A pull that really changed the image rebuilds, as before.
func TestAnUpdatedImageRebuilds(t *testing.T) {
	proceed, err := recreateOutcome(true, false, true, nil)
	if !proceed || err != nil {
		t.Fatalf("recreateOutcome(updated) = (%v, %v), want (true, nil)", proceed, err)
	}
}

// force means "rebuild regardless", and a rebuild on the local image is a real
// thing that is about to happen — so a failed pull does not cancel it and does
// not turn a completed rebuild into a reported failure.
//
// This is the case deliberately left reporting success; see the comment on
// recreateOutcome. Pinned so the choice is visible rather than accidental: if
// someone later decides force should also surface the pull failure, this test
// is where they will find out they are changing a decision.
func TestForceRebuildsDespiteAFailedPull(t *testing.T) {
	proceed, err := recreateOutcome(true, true, false, errors.New("unauthorized"))
	if !proceed {
		t.Error("force should rebuild on the existing local image even when the pull failed")
	}
	if err != nil {
		t.Errorf("force + failed pull returned err=%v; the rebuild really happens, so reporting it as a failure would be the opposite lie", err)
	}
}

// A caller that never asked for a pull cannot be reported as a failed pull,
// whatever stale error is lying around.
func TestNoPullRequestedIsNeverAPullFailure(t *testing.T) {
	proceed, err := recreateOutcome(false, false, false, errors.New("stale"))
	if err != nil {
		t.Errorf("no pull was requested, yet a pull failure was reported: %v", err)
	}
	if proceed {
		t.Error("no pull, no force, nothing changed — nothing to rebuild")
	}
}

func containsCause(msg, want string) bool {
	return len(msg) >= len(want) && (func() bool {
		for i := 0; i+len(want) <= len(msg); i++ {
			if msg[i:i+len(want)] == want {
				return true
			}
		}
		return false
	})()
}
