package service

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// mockOperation records calls to Complete and Fail.
type mockOperation struct {
	completed bool
	failed    bool
	message   string
	err       error
}

func (m *mockOperation) Complete(message string) {
	m.completed = true
	m.message = message
}

func (m *mockOperation) Fail(err error) {
	m.failed = true
	m.err = err
}

// A failing PullAndApply must NOT leave a success in the oplog.
// It must record failure with the error from PullAndApply.
func TestUpdateOpOutcomeFailingDoesNotLeaveSuccess(t *testing.T) {
	mock := &mockOperation{}
	pullErr := errors.New("pull image failed: connection refused")

	updateOpOutcome(mock, pullErr)

	if mock.completed {
		t.Fatalf("PullAndApply failed, but the operation was marked completed with success! Message: %q", mock.message)
	}
	if !mock.failed {
		t.Fatal("PullAndApply failed, but the operation was not marked failed")
	}
	if mock.err == nil || !strings.Contains(mock.err.Error(), "pull image failed") {
		t.Fatalf("expected failure error to contain 'pull image failed', got: %v", mock.err)
	}
}

// A succeeding PullAndApply must still record success in the oplog.
func TestUpdateOpOutcomeSucceedingRecordsSuccess(t *testing.T) {
	mock := &mockOperation{}

	updateOpOutcome(mock, nil)

	if !mock.completed {
		t.Fatal("PullAndApply succeeded, but the operation was not marked completed")
	}
	if mock.failed {
		t.Fatalf("PullAndApply succeeded, but the operation was marked failed: %v", mock.err)
	}
	if mock.message != "app updated successfully" {
		t.Fatalf("expected message 'app updated successfully', got: %q", mock.message)
	}
}

// updateOpOutcome must be safe against a nil operation (e.g. uninitialized logger).
func TestUpdateOpOutcomeNilSafe(t *testing.T) {
	// Neither of these should panic
	updateOpOutcome(nil, errors.New("some error"))
	updateOpOutcome(nil, nil)
}

// Wiring test: verifies at source level that Update does not complete the
// operation in its synchronous prologue/defer, and that the detached goroutine
// correctly owns and finishes the operation.
func TestUpdateOplogWiringIsActuallyWiredIn(t *testing.T) {
	src, err := os.ReadFile("compose_app.go")
	if err != nil {
		t.Fatalf("cannot read compose_app.go: %v", err)
	}
	text := string(src)

	// Anti-vacuity: verify positive controls exist
	if !strings.Contains(text, "func (a *ComposeApp) Update") {
		t.Fatal("cannot find func (a *ComposeApp) Update in compose_app.go")
	}
	if !strings.Contains(text, `StartOp("app.update", a.Name)`) {
		t.Fatal("cannot find StartOp(\"app.update\", a.Name) in compose_app.go")
	}

	// 1. Invariant: Update's synchronous prologue/defer must NOT call op.Complete("app updated successfully")
	oldBugPattern := "op.Complete(\"app updated successfully\")"
	updateFuncStart := strings.Index(text, "func (a *ComposeApp) Update")
	goroutineStart := strings.Index(text[updateFuncStart:], "go func(ctx context.Context")
	if goroutineStart == -1 {
		t.Fatal("cannot find goroutine spawn in Update")
	}
	syncPrologue := text[updateFuncStart : updateFuncStart+goroutineStart]

	if strings.Contains(syncPrologue, oldBugPattern) {
		t.Errorf("compose_app.go Update synchronous prologue still contains %q; "+
			"it claims the update succeeded before the background work even starts", oldBugPattern)
	}

	// 2. Invariant: goroutineStarted must guard the synchronous defer
	if !strings.Contains(syncPrologue, "goroutineStarted") {
		t.Errorf("compose_app.go Update does not guard its synchronous defer with goroutineStarted; " +
			"ownership of op must pass to the background goroutine")
	}

	// 3. Invariant: op must be passed to the goroutine
	if !strings.Contains(text, "go func(ctx context.Context, op operationFinisher)") &&
		!strings.Contains(text, "go func(ctx context.Context, op *oplog.Operation)") {
		t.Errorf("compose_app.go does not pass op to the detached background goroutine")
	}

	// 4. Invariant: the goroutine must wire updateOpOutcome to PullAndApply's error
	if !strings.Contains(text, "updateOpOutcome(op, err)") {
		t.Errorf("compose_app.go detached goroutine does not call updateOpOutcome(op, err)")
	}

	// 5. Invariant: the goroutine's panic recovery must fail op
	goroutineBody := text[updateFuncStart+goroutineStart:]
	updateFuncEnd := strings.Index(goroutineBody, "func (a *ComposeApp) App")
	if updateFuncEnd != -1 {
		goroutineBody = goroutineBody[:updateFuncEnd]
	}

	if !strings.Contains(goroutineBody, "op.Fail") {
		t.Errorf("compose_app.go detached goroutine recover does not fail op on panic")
	}
}
