package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	exec2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/exec"
	"gotest.tools/v3/assert"
)

// stubCommand replaces the command every bounded helper is built from, so the
// timeout guards can be driven without a real dying disk. The script is written
// to a file and executed by path because exec2 rejects shell metacharacters in
// arguments.
func stubCommand(t *testing.T, script string) {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("failed to write stub script: %v", err)
	}

	original := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec2.Cmd {
		return exec2.CommandContext(ctx, scriptPath)
	}
	t.Cleanup(func() {
		commandContext = original
		clearProbeBreaker()
	})
}

// clearProbeBreaker drops the abandoned-probe bookkeeping so a test that
// deliberately strands a probe does not deny the next test its process.
func clearProbeBreaker() {
	probeBreaker.mu.Lock()
	defer probeBreaker.mu.Unlock()

	probeBreaker.count = make(map[string]int)
}

// shrinkTimeouts compresses the production budgets so a test does not have to
// wait six seconds to prove the deadline fires.
func shrinkTimeouts(t *testing.T, d time.Duration) {
	t.Helper()

	originalSmart, originalLSBLK := smartCTLTimeout, lsblkTimeout
	smartCTLTimeout, lsblkTimeout = d, d
	t.Cleanup(func() { smartCTLTimeout, lsblkTimeout = originalSmart, originalLSBLK })
}

// hangingScript outgrows its own kill: the shell exits immediately but leaves a
// child holding the output pipe, so Wait() (and with it CombinedOutput) keeps
// blocking after CommandContext has already killed the process it started.
// That is the userspace stand-in for a drive stuck in uninterruptible I/O,
// which is the condition that used to hang the boot forever.
const hangingScript = "sleep 5 & exit 0"

func TestExecEnabledSMARTGivesUpOnHangingDisk(t *testing.T) {
	stubCommand(t, hangingScript)
	shrinkTimeouts(t, 300*time.Millisecond)

	start := time.Now()
	output, err := ExecEnabledSMART("/dev/sdz")
	elapsed := time.Since(start)

	assert.Assert(t, err != nil, "a disk that never answers must surface as an error, not as success")
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected a deadline error, got %v", err)
	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 2*time.Second, "ExecEnabledSMART blocked for %s past its deadline", elapsed)
}

func TestExecEnabledSMARTReturnsOutputOfHealthyDisk(t *testing.T) {
	stubCommand(t, "echo enabled")
	shrinkTimeouts(t, 5*time.Second)

	output, err := ExecEnabledSMART("/dev/sda")

	assert.NilError(t, err)
	assert.Equal(t, strings.TrimSpace(string(output)), "enabled")
}

// The whole point of the ExecLSBLK signature: a global enumeration that timed
// out has to reach the caller as a failure. Reporting it as an empty device list
// is what let the boot sweep decide the machine had no disks and mount nothing.
func TestExecLSBLKReportsHangAsError(t *testing.T) {
	stubCommand(t, hangingScript)
	shrinkTimeouts(t, 300*time.Millisecond)

	start := time.Now()
	output, err := ExecLSBLK()
	elapsed := time.Since(start)

	assert.Assert(t, err != nil, "a timed out lsblk must be an error, never an empty inventory")
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected a deadline error, got %v", err)
	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 2*time.Second, "ExecLSBLK blocked for %s past its deadline", elapsed)
}

func TestExecLSBLKReturnsOutputOfHealthySystem(t *testing.T) {
	stubCommand(t, `echo '{"blockdevices":[]}'`)
	shrinkTimeouts(t, 5*time.Second)

	output, err := ExecLSBLK()

	assert.NilError(t, err)
	assert.Equal(t, strings.TrimSpace(string(output)), `{"blockdevices":[]}`)
}

func TestExecLSBLKByPathGivesUpOnHangingDisk(t *testing.T) {
	stubCommand(t, hangingScript)
	shrinkTimeouts(t, 300*time.Millisecond)

	start := time.Now()
	output, err := ExecLSBLKByPath("/dev/sdz")
	elapsed := time.Since(start)

	assert.Assert(t, err != nil)
	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 2*time.Second, "ExecLSBLKByPath blocked for %s past its deadline", elapsed)
}

func TestExecSmartCTLByPathGivesUpOnHangingDisk(t *testing.T) {
	stubCommand(t, hangingScript)
	shrinkTimeouts(t, 300*time.Millisecond)

	start := time.Now()
	output := ExecSmartCTLByPath("/dev/sdz")
	elapsed := time.Since(start)

	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 2*time.Second, "ExecSmartCTLByPath blocked for %s past its deadline", elapsed)
}

// A read-only probe must die with the caller's budget even when the caller's
// budget is shorter than the probe's own patience: the pool assembly loop hands
// one absolute deadline to every probe of every pass.
func TestExecuteCommandContextObeysCallerDeadline(t *testing.T) {
	stubCommand(t, hangingScript)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	output, err := ExecuteCommandContext(ctx, "blkid", "--uuid", "whatever")
	elapsed := time.Since(start)

	assert.Assert(t, err != nil, "a probe that never answers must surface as an error")
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected a deadline error, got %v", err)
	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 2*time.Second, "ExecuteCommandContext blocked for %s past the caller deadline", elapsed)
}

func TestExecuteCommandContextReturnsOutput(t *testing.T) {
	stubCommand(t, "echo /dev/sdb1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	output, err := ExecuteCommandContext(ctx, "blkid", "--uuid", "whatever")

	assert.NilError(t, err)
	assert.Equal(t, strings.TrimSpace(string(output)), "/dev/sdb1")
}

// Every expired probe strands a process the kernel will not reap until the disk
// answers. The storage UI polls, so without the breaker one wedged drive turns
// into an unbounded pile of processes and file descriptors on a box that runs
// for months. While one invocation is still parked, the next must be refused
// instead of spawning another.
func TestProbeBreakerRefusesToStackStalledProbes(t *testing.T) {
	stubCommand(t, hangingScript)
	shrinkTimeouts(t, 200*time.Millisecond)

	_, err := ExecLSBLK()
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected the first call to time out, got %v", err)

	start := time.Now()
	output, err := ExecLSBLK()
	elapsed := time.Since(start)

	assert.Assert(t, errors.Is(err, ErrProbeStalled), "a second probe was launched while the first is still parked: %v", err)
	assert.Assert(t, output == nil)
	assert.Assert(t, elapsed < 100*time.Millisecond, "the refusal took %s - it must be immediate, not another timeout", elapsed)
}

// ...and the breaker must open again once the stranded probe finally returns,
// otherwise a single transient hang would disable disk enumeration for the life
// of the process - trading a leak for a permanent outage.
func TestProbeBreakerReopensAfterStalledProbeReturns(t *testing.T) {
	stubCommand(t, "sleep 1 & exit 0")
	shrinkTimeouts(t, 200*time.Millisecond)

	_, err := ExecLSBLK()
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected the first call to time out, got %v", err)

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = ExecLSBLK()
		if !errors.Is(err, ErrProbeStalled) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the breaker never reopened after the stalled probe returned")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
