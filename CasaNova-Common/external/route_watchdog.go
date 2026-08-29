package external

import (
	"context"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
)

// availabilityRoute builds the payload a re-registration is allowed to send.
//
// It is a fresh struct literal with ONLY the availability fields set, and
// every caller in this file goes through it. That is the point: a route now
// also carries authorization policy (AllowedCIDRs), and a payload that
// carried policy fields would let this liveness loop overwrite an
// administrator's decision. Leaving them at their zero value is how this
// mechanism says "I am not talking about policy" — the gateway preserves the
// stored policy for such a payload (see Management.CreateRoute /
// policyIntentOf in CasaNova-Gateway). Any future policy field added to
// model.Route must likewise be left unset here.
func availabilityRoute(apiPath, target string) *model.Route {
	return &model.Route{
		Path:   apiPath,
		Target: target,
	}
}

// StartRouteRegistrationWatchdog periodically re-registers the
// given service routes with the gateway so a gateway-side route loss
// (gateway crash + tmpfs wipe of /run/.../routes.json, gateway restart
// that the child service didn't notice) auto-heals.
//
// Scope, stated precisely: this maintains AVAILABILITY only. It re-asserts
// path → target and nothing else. It does not create, change, or remove any
// authorization policy attached to those routes, and it must never be
// extended to do so — an availability mechanism that can relax authorization
// turns a gateway restart into a silent security downgrade.
//
// Re-posting a route whose target is unchanged is therefore cheap and
// non-destructive: it refreshes the target/proxy entry and leaves policy
// alone. So this watchdog is near-free when routes are already registered,
// and curative when they aren't. Call from main() after the initial
// registration succeeds.
//
// Cadence is 30s by default — short enough that a transient gateway
// restart only shows a "service unavailable" for under a minute,
// long enough that the steady-state HTTP traffic is negligible.
// Stops cleanly when ctx is cancelled.
func StartRouteRegistrationWatchdog(
	ctx context.Context,
	paths []string,
	target string,
	createRoute func(*model.Route) error,
) {
	go func() {
		const interval = 30 * time.Second
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, apiPath := range paths {
					// Best-effort. Errors are logged-and-swallowed —
					// the gateway might be momentarily restarting, in
					// which case the next tick covers it.
					_ = createRoute(availabilityRoute(apiPath, target))
				}
			}
		}
	}()
}
