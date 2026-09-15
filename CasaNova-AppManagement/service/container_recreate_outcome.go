package service

import (
	"errors"
	"fmt"
)

// ErrImagePullFailed reports that an update was asked for, the pull did not
// happen, and NOTHING WAS CHANGED.
//
// It exists so the caller can tell that apart from "the image was already
// current", which is the other way RecreateContainer does nothing. Those two
// used to be the same return value — a bare nil — and the v2 handler answers
// 200 up front and reports the real outcome over the event bus, publishing
// AppUpdateError only when RecreateContainer returns an error. So a registry
// that was down, rate-limiting, or refusing credentials looked exactly like an
// app that had nothing to update: the progress bar completed, the container
// kept running the old image, and no event, no toast and no log line the user
// could see said the pull had failed.
var ErrImagePullFailed = errors.New("image pull failed, container not updated")

// recreateOutcome decides what RecreateContainer does after the pull attempt.
//
// It is a separate, dependency-free function because the decision is the part
// that was wrong, and in place it is unreachable without a Docker daemon, a
// real registry and a running container. Here the three cases are three lines
// of test.
//
//	proceed  the container should be torn down and rebuilt
//	err      nothing was done, and the reason is not "already up to date"
//
// The cases:
//
//   - pull asked for, pull failed, force NOT set: nothing happened and nothing
//     will. Return the error. This is the case the user reads as a completed
//     update.
//   - pull asked for, pull failed, force SET: the caller asked for a rebuild
//     regardless, and a rebuild on the existing local image is a real thing
//     that is about to happen. Proceed, and report success for what actually
//     occurred — the failure is already in the log. What this does NOT do is
//     tell the user the image did not move; that needs a second channel
//     (a distinct event, or a result rather than an error), and inventing one
//     here would mean reporting a completed rebuild as a failure. Named in
//     docs/design/per-app-grants.md §8.1 rather than half-built.
//   - no failure: the old rule stands — rebuild when forced or when the image
//     actually changed, otherwise do nothing and say so with nil.
func recreateOutcome(pull, force, isImageUpdated bool, pullErr error) (proceed bool, err error) {
	if pull && pullErr != nil && !force {
		return false, fmt.Errorf("%w: %v", ErrImagePullFailed, pullErr)
	}
	if !force && !isImageUpdated {
		return false, nil
	}
	return true, nil
}
