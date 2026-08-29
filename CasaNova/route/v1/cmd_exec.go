package v1

// Bounded external-command runners for HTTP handlers.
//
// The rule these enforce (systemic theme T4 of the enterprise review): a
// handler must never inherit the lifetime of an external binary. Every
// binary this package shells out to talks to something that can wedge —
// dockerd over its socket, upsd/apcupsd over TCP to a box that may be
// mid-power-failure, systemctl over D-Bus — and a wedged callee without a
// deadline turns each poll into one permanently-stuck echo worker until the
// pool is gone. The UPS case is the sharpest: the panel polls, and the
// moment upsd stops answering is precisely the moment the operator opens
// that panel.
//
// WaitDelay matters as much as the context. CommandContext kills on expiry,
// but a process in uninterruptible sleep (D-state: dead NFS, hung device)
// ignores SIGKILL and Wait would block forever anyway; WaitDelay abandons
// the wait and releases the handler. The orphan stays until the kernel lets
// go — nothing userspace can do about that — but the HTTP worker survives.

import (
	"context"
	"os/exec"
	"time"
)

func execOutBounded(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.Output()
}

func execRunBounded(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}
