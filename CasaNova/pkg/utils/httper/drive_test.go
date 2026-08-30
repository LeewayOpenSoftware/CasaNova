package httper

// The rclone client retried a unix socket.
//
// NewRestyClient dialled /var/run/rclone/rclone.sock with SetRetryCount(3)
// and SetRetryWaitTime(5s). resty floors each wait at that value, so a socket
// that is not there — rclone stopped, mid-upgrade, or never installed — cost
// 15.01 seconds of sleep to learn what the first connect() already returned:
// "no such file or directory". Measured, not estimated.
//
// That mattered because of WHERE it ran. GetMountList is reached from
// storageauth.deriveLocked, which runs on the request path while holding the
// authority's global write lock, so every concurrent file operation queued
// behind those fifteen seconds. And the caller was ready for the failure all
// along: it records "mounted filesystems could not be enumerated" and
// continues with a narrower ceiling. The retry delayed a degraded answer; it
// never avoided one.
//
// A retry is a bet that the answer will differ next time. Across a network
// that is often true. Across a unix socket on this machine it is not.

import (
	"testing"
	"time"
)

func TestGetMountList_FailsImmediatelyWhenRcloneIsNotListening(t *testing.T) {
	original := rcloneSocket
	t.Cleanup(func() { rcloneSocket = original })
	rcloneSocket = t.TempDir() + "/absent.sock"

	start := time.Now()
	_, err := GetMountList()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a missing socket must report an error, not an empty mount list")
	}
	// The shipped policy took 15.01s here. Anything near that means the retry
	// is back. One second is far above the real cost (a failed connect() to a
	// missing path returns in microseconds) and far below the bug.
	if elapsed > time.Second {
		t.Errorf("took %v to learn the socket is absent; the retry policy is back", elapsed)
	}
}

func TestNewRestyClient_DoesNotRetry(t *testing.T) {
	// States the intent directly, so a future edit that reintroduces retries
	// fails here with the reason attached rather than only as a slow test.
	if n := NewRestyClient().RetryCount; n != 0 {
		t.Errorf("RetryCount is %d; retrying a local unix socket cannot change its answer", n)
	}
}

func TestGetMountList_IsBoundedTighterThanTheClientDefault(t *testing.T) {
	// A socket that accepts the connection and then says nothing would hold
	// the storage authority's write lock for the full client budget. The
	// listing gets its own, much shorter one.
	if mountListTimeout >= DefaultTimeout {
		t.Errorf("mountListTimeout (%v) must be tighter than DefaultTimeout (%v): it runs under a global lock",
			mountListTimeout, DefaultTimeout)
	}
}
