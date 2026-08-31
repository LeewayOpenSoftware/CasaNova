package command

// OnlyExec ran `/bin/bash -c` with no bound of any kind. Its callers are
// helper-script functions and mount/umount — a umount blocking on writeback, a
// helper waiting on a wedged device, or systemd taking its full stop timeout
// each held the handler, and whatever lock it was under, with nothing able to
// end it. There was no deadline to exceed, so no client timeout could be
// chosen sensibly either: any number would have been a guess.
//
// These pin that a hung command now dies, that a healthy one is untouched, and
// that the failure says which command and how long — "signal: killed" names
// neither.

import (
	"strings"
	"testing"
	"time"
)

func TestOnlyExecBounded_KillsACommandThatNeverReturns(t *testing.T) {
	start := time.Now()
	_, err := OnlyExecBounded(300*time.Millisecond, "sleep 30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command past its ceiling must fail, not be waited for")
	}
	// Generous upper bound: the ceiling plus the 5s WaitDelay plus slack. The
	// point is that it returns at all — the unfixed version waits 30 seconds.
	if elapsed > 8*time.Second {
		t.Errorf("took %v; the ceiling is not being enforced", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "sleep 30") {
		t.Errorf("the error must name the timeout and the command, got: %v", err)
	}
}

func TestOnlyExecBounded_LeavesAHealthyCommandAlone(t *testing.T) {
	// Guards the guard. A ceiling that also killed working commands would make
	// the test above pass while breaking every caller.
	out, err := OnlyExecBounded(10*time.Second, "echo hola")
	if err != nil {
		t.Fatalf("a fast command must succeed: %v", err)
	}
	if strings.TrimSpace(out) != "hola" {
		t.Errorf("output was lost: %q", out)
	}
}

func TestOnlyExecBounded_StillReportsTheCommandsOwnFailure(t *testing.T) {
	// A non-zero exit is not a timeout, and must not be reported as one.
	out, err := OnlyExecBounded(10*time.Second, "echo nope >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit must still be an error")
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("an exit code was misreported as a timeout: %v", err)
	}
	if !strings.Contains(out, "nope") {
		t.Errorf("stderr must survive for the caller to log, got %q", out)
	}
}

func TestOnlyExec_CarriesTheDefaultCeiling(t *testing.T) {
	// The deprecated entry point is what the eight existing callers use; it
	// has to inherit the bound rather than keep the old unbounded behaviour.
	if defaultExecCeiling <= 0 {
		t.Fatal("OnlyExec would still be unbounded")
	}
	start := time.Now()
	if _, err := OnlyExec("echo x"); err != nil {
		t.Fatalf("the default path must still work: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the default path should not wait on anything")
	}
}
