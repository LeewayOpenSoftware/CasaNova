package scheduled

// RunTask stamps EndedAt from a deferred function. That only reaches the
// caller because the return value is NAMED: with a plain `Run` return, each
// `return run` copies the struct out before the defer runs, so the stamp lands
// on a dead local and every run is serialized (task.go's Run.EndedAt) with the
// zero time. These tests pin the stamp on every exit path RunTask has —
// success, non-zero exit, timeout, and the runner failing to build a command
// at all — because the bug was invisible in all four.

import (
	"context"
	"testing"
	"time"
)

func hostTask(command string, timeoutSeconds int) *Task {
	return &Task{
		ID:             "t-1",
		Name:           "test",
		Command:        command,
		Target:         Target{Host: true},
		Enabled:        true,
		TimeoutSeconds: timeoutSeconds,
	}
}

func TestRunTask_StampsEndedAtOnTheReturnedRun(t *testing.T) {
	before := time.Now()
	run := RunTask(context.Background(), hostTask("true", 30))
	after := time.Now()

	if run.EndedAt.IsZero() {
		t.Fatal("EndedAt is zero on the returned Run — the deferred stamp did not reach the caller")
	}
	if run.EndedAt.Before(before) || run.EndedAt.After(after) {
		t.Errorf("EndedAt %v is outside the call window [%v, %v]", run.EndedAt, before, after)
	}
	if run.EndedAt.Before(run.StartedAt) {
		t.Errorf("EndedAt %v precedes StartedAt %v", run.EndedAt, run.StartedAt)
	}
	if run.Status != StatusSuccess {
		t.Errorf("want %q for `true`, got %q", StatusSuccess, run.Status)
	}
}

func TestRunTask_StampsEndedAtOnEveryExitPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		task *Task
		want RunStatus
	}{
		{"success", hostTask("true", 30), StatusSuccess},
		{"non-zero exit", hostTask("exit 3", 30), StatusFailed},
		// buildCmd refuses shell metacharacters, so this returns before any
		// process is spawned — the earliest `return run` in the function.
		{"runner error", hostTask("echo hi | tee /tmp/x", 30), StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := RunTask(context.Background(), tc.task)
			if run.EndedAt.IsZero() {
				t.Errorf("EndedAt is zero on the %s path", tc.name)
			}
			if run.Status != tc.want {
				t.Errorf("want status %q, got %q (output %q)", tc.want, run.Status, run.Output)
			}
		})
	}
}

func TestRunTask_StampsEndedAtWhenTheTaskTimesOut(t *testing.T) {
	run := RunTask(context.Background(), hostTask("sleep 5", 1))

	if run.EndedAt.IsZero() {
		t.Fatal("EndedAt is zero on the timeout path")
	}
	if run.Status != StatusTimedOut {
		t.Fatalf("want %q, got %q", StatusTimedOut, run.Status)
	}
	// The stamp has to be the real end, not the start: a killed run must show
	// roughly its timeout of elapsed time, which is what the UI displays.
	if elapsed := run.EndedAt.Sub(run.StartedAt); elapsed < time.Second {
		t.Errorf("a 1s-timeout task reports only %v elapsed", elapsed)
	}
}
