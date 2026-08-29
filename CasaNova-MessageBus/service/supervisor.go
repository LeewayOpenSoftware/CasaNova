package service

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// Supervision for the long-lived bus goroutines (event dispatcher,
// action dispatcher, socket.io server).
//
// B11: before this, Services.Start() launched all three as bare
// goroutines whose only protection was a recover() that logged
// "OFFLINE until restart" and then let the goroutine die. Nothing
// restarted them, and their state was not reflected anywhere an
// operator could look: main() stays alive as long as the HTTP
// listeners are up, so systemd still reported the unit `active`,
// every POST to /event/{source}/{name} still answered 200 OK, and the
// dashboard stayed green while an entire class of events had stopped
// being delivered. The only trace was a single log line, possibly
// months back in journald.
//
// Two rules drive the design:
//
//   - An unexpected *return* is exactly as fatal as a panic. Both
//     dispatchers clear inboundChannelPtr in their defer, which turns
//     Publish()/Trigger() into a permanent no-op, so a silent return
//     is treated as a failure and restarted just like a panic.
//
//   - The supervisor never gives up. Restarts are capped in *rate*
//     (exponential backoff up to 30s) but never in count. Giving up
//     after N attempts would land us back on "silently dead forever",
//     which is the exact defect being fixed. A dispatcher that panics
//     deterministically therefore shows up as a climbing restart
//     counter in /health plus one ERROR per attempt in journald —
//     loud and recoverable, instead of quiet and terminal.
const (
	superviseInitialBackoff = 500 * time.Millisecond
	superviseMaxBackoff     = 30 * time.Second

	// A run that stayed up this long counts as healthy, so the next
	// failure starts from the initial backoff again instead of
	// inheriting the delay accumulated during an old burst. Without
	// this, one bad afternoon would leave every later restart pinned
	// at the 30s ceiling for the rest of the daemon's uptime.
	superviseStableRun = 2 * time.Minute
)

// failureRecord is the last abnormal exit of a supervised goroutine.
type failureRecord struct {
	when   time.Time
	reason string
}

// goroutineState is the observable state of one supervised goroutine.
// Every field is atomic so Health() can read it from an HTTP handler
// without taking any lock the dispatcher itself might be holding — a
// health endpoint that can block on the subsystem it is reporting on
// is worthless precisely when it is needed.
type goroutineState struct {
	name string

	running       atomic.Bool
	starts        atomic.Uint64
	restarts      atomic.Uint64
	lastStartedAt atomic.Int64
	lastExitedAt  atomic.Int64
	lastFailure   atomic.Pointer[failureRecord]

	// Backoff parameters live on the struct (not as bare constants) so
	// tests can drive the restart path without sleeping for seconds.
	initialBackoff time.Duration
	maxBackoff     time.Duration
	stableRun      time.Duration
}

func newGoroutineState(name string) *goroutineState {
	return &goroutineState{
		name:           name,
		initialBackoff: superviseInitialBackoff,
		maxBackoff:     superviseMaxBackoff,
		stableRun:      superviseStableRun,
	}
}

// GoroutineHealth is a point-in-time snapshot of one supervised
// goroutine, serialized by the liveness endpoint.
type GoroutineHealth struct {
	Name          string `json:"name"`
	Running       bool   `json:"running"`
	Starts        uint64 `json:"starts"`
	Restarts      uint64 `json:"restarts"`
	LastStartedAt int64  `json:"last_started_at,omitempty"`
	LastExitedAt  int64  `json:"last_exited_at,omitempty"`
	LastFailure   string `json:"last_failure,omitempty"`
	LastFailureAt int64  `json:"last_failure_at,omitempty"`
}

func (g *goroutineState) snapshot() GoroutineHealth {
	h := GoroutineHealth{
		Name:          g.name,
		Running:       g.running.Load(),
		Starts:        g.starts.Load(),
		Restarts:      g.restarts.Load(),
		LastStartedAt: g.lastStartedAt.Load(),
		LastExitedAt:  g.lastExitedAt.Load(),
	}

	if f := g.lastFailure.Load(); f != nil {
		h.LastFailure = f.reason
		h.LastFailureAt = f.when.Unix()
	}

	return h
}

// runOnce runs fn to completion, converting a panic into a reason
// string. An empty reason means fn returned on its own.
func (g *goroutineState) runOnce(fn func()) (reason string) {
	g.starts.Add(1)
	g.lastStartedAt.Store(time.Now().Unix())
	g.running.Store(true)

	defer func() {
		g.running.Store(false)
		g.lastExitedAt.Store(time.Now().Unix())

		if r := recover(); r != nil {
			reason = fmt.Sprintf("panic: %v", r)
			// The stack is the only way to find out *where* a
			// months-old dispatcher panic came from; the previous
			// recover() logged the panic value alone.
			logger.Error("supervised goroutine panicked",
				zap.String("goroutine", g.name),
				zap.Any("panic", r),
				zap.ByteString("stack", debug.Stack()))
		}
	}()

	fn()

	return ""
}

// supervise runs fn in a restart loop until ctx is cancelled. onFailure
// (may be nil) is invoked for every abnormal exit so callers can push
// the degradation somewhere an operator sees it (systemd STATUS=).
func (g *goroutineState) supervise(ctx context.Context, fn func(), onFailure func(name, reason string, restarts uint64)) {
	backoff := g.initialBackoff

	for {
		startedAt := time.Now()
		reason := g.runOnce(fn)

		if reason == "" {
			// A clean return during shutdown is the expected path;
			// at any other time it means the dispatcher is gone and
			// Publish() has silently become a no-op.
			reason = "returned unexpectedly while the bus was still running"
		}

		if ctx.Err() != nil {
			logger.Info("supervised goroutine stopped for shutdown",
				zap.String("goroutine", g.name))
			return
		}

		restarts := g.restarts.Add(1)
		g.lastFailure.Store(&failureRecord{when: time.Now(), reason: reason})

		logger.Error("supervised goroutine died — restarting",
			zap.String("goroutine", g.name),
			zap.String("reason", reason),
			zap.Uint64("restarts_total", restarts),
			zap.Duration("backoff", backoff))

		if onFailure != nil {
			// A panic inside the notifier must not take down the
			// supervisor that exists to survive panics.
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error("panic in supervisor failure notifier",
							zap.String("goroutine", g.name), zap.Any("panic", r))
					}
				}()
				onFailure(g.name, reason, restarts)
			}()
		}

		if time.Since(startedAt) >= g.stableRun {
			backoff = g.initialBackoff
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff < g.maxBackoff {
			backoff *= 2
			if backoff > g.maxBackoff {
				backoff = g.maxBackoff
			}
		}
	}
}
