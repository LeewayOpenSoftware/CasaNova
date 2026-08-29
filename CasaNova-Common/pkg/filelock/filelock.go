// Package filelock serializes a read-modify-write cycle on a shared file
// BETWEEN PROCESSES.
//
// # THE DEFECT THIS EXISTS TO FIX
//
// CasaNova runs nine separate processes over one set of JSON state files.
// Several of them guard a read-modify-write of a shared file with a package
// level sync.Mutex and a comment claiming that concurrent writers cannot
// interleave. A sync.Mutex exists once per process; it orders nothing between
// processes. The read-modify-write of the token revocation list was measured
// losing 27 of 40 revocations that the API had already reported as successful,
// simply because another service happened to be starting at the time.
//
// An atomic rename gives a READER all-or-nothing content. It gives a WRITER
// nothing: two processes that both read version A, both edit their own copy,
// and both rename, leave whichever renamed last, and the other one's edit is
// gone with no error anywhere.
//
// # SHAPE
//
// One advisory flock(2) on a sidecar "<target>.lock", held across the whole
// read-modify-write. flock is the right primitive here for three reasons:
//
//   - The kernel releases it when the holding process dies or the descriptor
//     closes, so a service killed mid-update (self-update stops services one by
//     one) cannot wedge the box behind a stale lock. A lock expressed as "a
//     file exists" cannot make that promise.
//   - It is held on a SIDECAR, never on the target. The target is replaced by
//     rename on every write, and a lock taken on a replaced inode stops
//     excluding anything.
//   - It is advisory and process-scoped, so it composes with the in-process
//     mutex a caller already holds instead of replacing it.
//
// # WHAT THIS DOES NOT DO
//
// It does not make writes durable, does not order anything against a process
// that never calls Acquire, and does not protect readers — readers rely on the
// writer's atomic rename, as before.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrTimeout is returned when the lock could not be taken within the deadline.
// Callers must treat it as "the update did not happen": reporting success
// without the lock is the failure this package exists to prevent.
var ErrTimeout = errors.New("timed out waiting for the file lock")

// Suffix is appended to the protected path to name the lock file. It is a
// sidecar and is never removed: unlinking a lock file lets a later acquirer
// create a fresh inode and hold "the lock" at the same time as the process
// holding the unlinked one.
const Suffix = ".lock"

const (
	// lockMode has to let every service take the lock, and they do not all run
	// as the same user. The file carries no content, so world-readable costs
	// nothing; flock(LOCK_EX) works on a read-only descriptor on Linux, which
	// is what lets a non-owner still take the exclusive lock.
	lockMode = 0o644

	minRetry = 500 * time.Microsecond
	maxRetry = 20 * time.Millisecond
)

// Lock is a held cross-process lock. Release it exactly once.
type Lock struct {
	f *os.File
}

// Acquire takes the exclusive lock guarding target, waiting up to timeout.
//
// The wait is bounded on purpose. The critical sections this guards are one
// small read and one small write, so contention resolves in microseconds; a
// wait that lasted for an unbounded time would convert a stuck peer into a
// stuck request. On timeout the caller gets ErrTimeout and must report failure
// rather than write unserialized.
//
// The parent directory must already exist — this deliberately does not create
// it, because the caller that owns the state directory also owns its mode.
func Acquire(target string, timeout time.Duration) (*Lock, error) {
	name := target + Suffix

	// O_RDONLY is enough for LOCK_EX and means a service that may only read the
	// state directory's files can still participate in the exclusion.
	f, err := os.OpenFile(name, os.O_RDONLY|os.O_CREATE, lockMode)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", name, err)
	}

	deadline := time.Now().Add(timeout)
	backoff := minRetry
	for {
		err := tryLockEx(f)
		switch {
		case err == nil:
			return &Lock{f: f}, nil
		case errors.Is(err, errInterrupted):
			// A signal (Go's own preemption sends them) interrupted the call.
			// Retry immediately: no time has been spent waiting.
			continue
		case !errors.Is(err, errWouldBlock):
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", name, err)
		}

		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrTimeout, name)
		}
		time.Sleep(backoff)
		if backoff < maxRetry {
			backoff *= 2
		}
	}
}

// Release drops the lock. Closing the descriptor would be enough — the kernel
// releases the flock with it — but unlocking first keeps the intent explicit.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlock(l.f)
	l.f.Close()
	l.f = nil
}
