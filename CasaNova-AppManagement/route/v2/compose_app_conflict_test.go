package v2

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
)

// TestComposeAppConflictRecognisesWrappedRejections: the sentinels never reach
// the handlers bare. PullAndApply returns `fmt.Errorf("%w: %s", ...)`, Install
// adds its own context, and Update/Apply wrap again — so identity comparison
// (err == service.ErrComposeAppBusy) silently degrades every rejection to a 500
// and the user is told the daemon broke when the app was merely busy.
func TestComposeAppConflictRecognisesWrappedRejections(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"busy, bare", service.ErrComposeAppBusy},
		{"busy, wrapped once", fmt.Errorf("%w: app navidrome is busy with another operation", service.ErrComposeAppBusy)},
		{"busy, wrapped twice", fmt.Errorf("app.update rejected: %w", fmt.Errorf("%w: locked", service.ErrComposeAppBusy))},
		{"already installed, bare", service.ErrComposeAppAlreadyInstalled},
		{"already installed, wrapped", fmt.Errorf("%w: navidrome", service.ErrComposeAppAlreadyInstalled)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, ok := composeAppConflict(test.err, "navidrome")
			if !ok {
				t.Fatalf("rejection %v was not recognised as a conflict - the caller answers 500 for an app that is simply busy", test.err)
			}
			if !strings.Contains(message, "navidrome") {
				t.Fatalf("conflict message does not name the app: %q", message)
			}
		})
	}
}

// TestComposeAppConflictLeavesRealFailuresAlone: an actual fault must keep its
// 500. Answering 409 for a broken daemon or a full disk would tell the user to
// "just retry" forever.
func TestComposeAppConflictLeavesRealFailuresAlone(t *testing.T) {
	for _, err := range []error{
		errors.New("no space left on device"),
		fmt.Errorf("pull failed: %w", errors.New("connection refused")),
		service.ErrComposeAppNotMatch,
		service.ErrComposeFileNotFound,
	} {
		if message, ok := composeAppConflict(err, "navidrome"); ok {
			t.Fatalf("real failure %v was reported to the user as a retryable conflict: %q", err, message)
		}
	}
}
