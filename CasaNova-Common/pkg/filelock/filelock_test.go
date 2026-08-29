package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The whole point: while one holder has it, nobody else gets it. flock is per
// open-file-description, so a second Acquire in this same process contends
// exactly as another process would.
func TestASecondAcquireIsExcludedUntilRelease(t *testing.T) {
	target := filepath.Join(t.TempDir(), "state.json")

	first, err := Acquire(target, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	if second, err := Acquire(target, 50*time.Millisecond); err == nil {
		second.Release()
		first.Release()
		t.Fatal("two holders of one lock; the read-modify-write it guards can interleave")
	} else if !errors.Is(err, ErrTimeout) {
		first.Release()
		t.Fatalf("want ErrTimeout, got %v", err)
	}

	first.Release()

	second, err := Acquire(target, time.Second)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	second.Release()
}

// Release must be safe to call more than once: callers defer it and also run it
// on error paths, and a second Release must not free the lock the NEXT holder
// has taken.
func TestReleaseIsIdempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "state.json")
	l, err := Acquire(target, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	l.Release()

	held, err := Acquire(target, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	l.Release() // the stale handle must not touch the live lock
	if other, err := Acquire(target, 50*time.Millisecond); err == nil {
		other.Release()
		t.Fatal("a stale Release freed the lock a live holder owns")
	}
}

// The lock lives on a sidecar, never on the protected file: the protected file
// is replaced by rename on every write, and a lock held on a replaced inode
// stops excluding anything.
func TestTheLockIsNotTakenOnTheProtectedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")

	l, err := Acquire(target, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("Acquire created or touched the protected file: %v", err)
	}
	if _, err := os.Stat(target + Suffix); err != nil {
		t.Fatalf("the lock sidecar was not created: %v", err)
	}
}

// A missing directory must be an error, not a silently unheld lock.
func TestAcquireFailsRatherThanReturningAnUnheldLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "no-such-dir", "state.json")
	if l, err := Acquire(target, 50*time.Millisecond); err == nil {
		l.Release()
		t.Fatal("Acquire succeeded with nowhere to put the lock file")
	}
}
