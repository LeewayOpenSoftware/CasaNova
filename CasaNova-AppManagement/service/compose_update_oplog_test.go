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

// The test above passes a literal nil, which the compiler turns into a truly
// nil operationFinisher (nil type AND nil value). That is NOT what StartOp
// actually produces when the oplog DB was never set (SetOpLoggerDB has zero
// callers in this codebase today, so this is the permanent, current state):
// StartOp returns a nil *oplog.Operation, a CONCRETE type. The moment that
// concrete nil crosses into an operationFinisher-typed parameter — exactly
// what happens at the `go func(ctx context.Context, op operationFinisher)`
// boundary in Update(), and again at its `updateOpOutcome(op, err)` call —
// the resulting interface value carries a non-nil type descriptor
// (*oplog.Operation) with a nil pointer underneath. `op == nil` on THAT
// value is false: this is Go's well-documented "typed nil in a non-nil
// interface" pitfall. The literal-nil test above cannot see this because a
// literal nil never goes through that conversion.
func TestUpdateOpOutcomeSurvivesATypedNilFromStartOp(t *testing.T) {
	// No test in this package calls SetOpLoggerDB, so opLogger is nil here —
	// the same permanent state production is in.
	op := StartOp("app.update", "probe")
	if op != nil {
		t.Fatal("test assumption violated: StartOp returned non-nil; opLogger must be nil for this test to mean anything")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("updateOpOutcome panicked on a StartOp-sourced nil operation: %v\n"+
				"This is the exact crash Update()'s async goroutine hits on EVERY compose app "+
				"update today, success or failure, because SetOpLoggerDB is never called: "+
				"updateOpOutcome's op==nil guard cannot see a concrete *oplog.Operation nil "+
				"once it has been implicitly converted to the operationFinisher interface.", r)
		}
	}()
	updateOpOutcome(op, errors.New("some error"))
	updateOpOutcome(op, nil)
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

	// 5. Invariant: the goroutine's panic recovery must fail op — routed
	// through updateOpOutcome (or isNilFinisher-guarded), not a raw
	// `op.Fail(...)`. A direct call on a StartOp-sourced nil panics a
	// second time (a typed-nil-in-a-non-nil-interface, invisible to a plain
	// `op != nil` check) inside a recover() that already fired once and
	// crashes the daemon instead of just this update. Checking for the
	// literal "op.Fail" here would be vacuous: a comment mentioning it
	// would satisfy the check without the real call site being fixed.
	goroutineBody := text[updateFuncStart+goroutineStart:]
	updateFuncEnd := strings.Index(goroutineBody, "func (a *ComposeApp) App")
	if updateFuncEnd != -1 {
		goroutineBody = goroutineBody[:updateFuncEnd]
	}

	recoverStart := strings.Index(goroutineBody, "recover()")
	if recoverStart == -1 {
		t.Fatal("compose_app.go detached goroutine has no recover()")
	}
	recoverBody := goroutineBody[recoverStart:]
	recoverBraceEnd := strings.Index(recoverBody, "}()")
	if recoverBraceEnd != -1 {
		recoverBody = recoverBody[:recoverBraceEnd]
	}
	if !strings.Contains(recoverBody, "updateOpOutcome(op") {
		t.Errorf("compose_app.go detached goroutine's recover handler does not finish op " +
			"via updateOpOutcome on panic — a direct op.Fail/op.Complete call here would " +
			"panic a second time on a StartOp-sourced nil operationFinisher and crash the daemon")
	}
}
