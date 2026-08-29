package command

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	exec2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/exec"
)

var (
	// smartCTLTimeout bounds every smartctl invocation. The -a probe always
	// had a deadline; `-s on` did not, and it runs in the per-disk loop of
	// CheckSerialDiskMount - the whole job of the casanova-local-storage-first
	// oneshot, which has no start timeout and which docker is ordered behind. A
	// drive parked in uninterruptible I/O (exactly the drive you need SMART
	// for) used to hang that call forever, so the oneshot never finished,
	// docker never started, and the box came up with no /DATA at all. The
	// health of one disk must never gate the remount of the others.
	// Variable rather than const so tests can compress it.
	smartCTLTimeout = 6 * time.Second

	// lsblkTimeout bounds lsblk, which sits on the same boot path
	// (CheckSerialDiskMount -> LSBLK) and walks every block device, so a dying
	// drive hangs it just as hard. The budget is deliberately generous: a
	// healthy sweep finishes in milliseconds, so anything approaching this is
	// already a sick device. Note that a timeout here is reported as an error,
	// never as "the machine has no disks" - see ExecLSBLK.
	lsblkTimeout = 30 * time.Second

	// commandContext is the single seam the bounded helpers below build their
	// commands through, so tests can exercise the timeout paths without a real
	// dying disk.
	commandContext = exec2.CommandContext
)

// ErrProbeStalled is returned instead of launching a new probe while an earlier
// invocation of the same probe is still parked past its deadline.
var ErrProbeStalled = errors.New("a previous invocation of this probe has not returned yet")

// maxStalledProbesPerKey is the circuit breaker for abandoned probes. A process
// blocked in uninterruptible disk I/O does not die on SIGKILL, so every expired
// probe leaves a child, its pipes and a goroutine behind until the kernel
// finally reaps it. The storage UI polls, so without a breaker a single wedged
// drive turns into an unbounded pile of lsblk processes and file descriptors -
// on a box that is expected to run for months, that ends in fork() failing
// machine-wide. One outstanding stalled probe per command is enough to know the
// device is not answering; further attempts add nothing but resource pressure.
const maxStalledProbesPerKey = 1

// stalledProbes counts, per probe key, how many invocations were abandoned at
// their deadline and have not returned yet.
type stalledProbes struct {
	mu    sync.Mutex
	count map[string]int
}

var probeBreaker = &stalledProbes{count: make(map[string]int)}

func (s *stalledProbes) isTripped(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.count[key] >= maxStalledProbesPerKey
}

// RunBoundedProbe runs a probe and gives up when ctx expires. CommandContext
// already kills the child at the deadline, but that is not enough on its own:
// a process blocked in uninterruptible disk I/O does not die on SIGKILL, and
// Wait() (hence Output()/CombinedOutput()) also waits for every writer of the
// output pipe to close. Returning on ctx.Done() is what actually frees the
// caller to move on to the next disk; the goroutine finishes by itself once
// the kernel finally reaps the child, and the breaker keyed on `key` keeps us
// from stacking more of those while one is still parked.
//
// It is generic and exported because the same problem exists for probes that
// are not child processes at all: statfs(2) on a dead NFS/CIFS mount parks the
// calling goroutine (and the OS thread under it) in uninterruptible sleep with
// no way to cancel it. Those callers need exactly this treatment - a deadline
// that frees the caller, plus the breaker so a polled endpoint cannot pile up
// one stranded thread per poll for months.
func RunBoundedProbe[T any](ctx context.Context, key string, run func() (T, error)) (T, error) {
	var zero T

	if probeBreaker.isTripped(key) {
		return zero, fmt.Errorf("%s: %w", key, ErrProbeStalled)
	}

	type result struct {
		output T
		err    error
	}

	// abandoned/finished are handed over under the breaker's lock, so the case
	// where the child returns at the very instant the deadline fires cannot
	// leave the breaker tripped forever (which would be a self-inflicted
	// permanent outage of the probe).
	var abandoned, finished bool

	done := make(chan result, 1)
	go func() {
		output, err := run()

		probeBreaker.mu.Lock()
		finished = true
		if abandoned {
			probeBreaker.count[key]--
			if probeBreaker.count[key] <= 0 {
				delete(probeBreaker.count, key)
			}
		}
		probeBreaker.mu.Unlock()

		done <- result{output: output, err: err}
	}()

	select {
	case r := <-done:
		return r.output, r.err
	case <-ctx.Done():
		probeBreaker.mu.Lock()
		if !finished {
			abandoned = true
			probeBreaker.count[key]++
		}
		probeBreaker.mu.Unlock()

		return zero, fmt.Errorf("%s: %w", key, ctx.Err())
	}
}

// exec smart
func ExecSmartCTLByPath(path string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), smartCTLTimeout)
	defer cancel()

	cmd := commandContext(ctx, "smartctl", "-a", "-n", "standby", path, "-j")

	output, err := RunBoundedProbe(ctx, "smartctl -a "+path, cmd.Output)
	if err != nil {
		fmt.Println("smartctl", err.Error())
		fmt.Println("smartctl", string(path))
		fmt.Println("smartctl", len(output))
	}
	return output
}

func ExecEnabledSMART(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), smartCTLTimeout)
	defer cancel()

	cmd := commandContext(ctx, "smartctl", "-s", "on", path)

	return RunBoundedProbe(ctx, "smartctl -s on "+path, cmd.CombinedOutput)
}

// ExecLSBLKByPath reports the block device tree of a single device. It returns
// the error rather than a bare nil so callers can tell "this device reports
// nothing" from "this device never answered".
func ExecLSBLKByPath(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsblkTimeout)
	defer cancel()

	return ExecLSBLKByPathContext(ctx, path)
}

// ExecLSBLKByPathContext is the variant that answers to a caller-owned
// deadline, for probe loops that must respect a budget spanning several probes.
func ExecLSBLKByPathContext(ctx context.Context, path string) ([]byte, error) {
	cmd := commandContext(ctx, "lsblk", path, "-O", "-J", "-b")

	return RunBoundedProbe(ctx, "lsblk "+path, cmd.Output)
}

// ExecLSBLK reports the block device tree of the whole system.
//
// It returns ([]byte, error) - and every caller must honour the error - because
// this command is global: it walks every block device on the machine, so one
// drive in uninterruptible I/O makes the whole inventory fail. Collapsing that
// failure into "no output" is what turned a dying disk into a boot where the
// remount sweep saw an empty device list, mounted nothing, and let docker start
// on top of an empty /DATA. Failure to enumerate is never absence of disks.
func ExecLSBLK() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsblkTimeout)
	defer cancel()

	cmd := commandContext(ctx, "lsblk", "-O", "-J", "-b")

	return RunBoundedProbe(ctx, "lsblk", cmd.Output)
}

// ExecuteCommand stays deliberately unbounded: it carries the destructive,
// legitimately long-running tools (mkfs on a multi-TB drive, parted, sfdisk).
// Cutting one of those off mid-flight is far worse than waiting. Read-only
// probes that run on the boot path use the bounded helpers above instead.
func ExecuteCommand(name string, arg ...string) ([]byte, error) {
	cmd := exec2.Command(name, arg...)

	out, err := cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			message := string(exitError.Stderr)
			return nil, errors.New(message)
		}
		return nil, err
	}

	return out, nil
}

// ExecuteCommandContext is the bounded twin of ExecuteCommand, for read-only
// probes only (blkid, lsblk, partx). It must never be used for mkfs/parted/
// sfdisk: those are destructive and legitimately slow, which is precisely why
// ExecuteCommand has no deadline. Probes are the opposite - they run in polling
// loops on the boot path, so one of them stuck on a dying disk must not be able
// to hold a caller (or an HTTP handler goroutine) forever.
func ExecuteCommandContext(ctx context.Context, name string, arg ...string) ([]byte, error) {
	key := name
	if len(arg) > 0 {
		key = name + " " + strings.Join(arg, " ")
	}

	cmd := commandContext(ctx, name, arg...)

	out, err := RunBoundedProbe(ctx, key, cmd.Output)
	if err != nil {
		// Same error shape as ExecuteCommand: callers of the probe helpers
		// treat a non-zero exit (blkid finding no such UUID, for instance) as a
		// plain error carrying the tool's own stderr.
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, errors.New(string(exitError.Stderr))
		}
		return nil, err
	}

	return out, nil
}
