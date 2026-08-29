package route

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// HealthPathSuffix is appended to the API base path (`/v2/message_bus`)
// to form the liveness endpoint.
//
// B11: the bus had no way for an operator to ask "is the dispatcher
// alive?". systemd only knows whether main() is still running, the
// HTTP publish endpoint answered 200 OK regardless, and the drop
// counters existed but were exported nowhere. This endpoint is the
// answer to that question, reachable two ways:
//
//   - root on the box, no token:
//     curl --unix-socket /run/casanova/internal/message-bus.sock \
//     http://unix/v2/message_bus/health
//   - through the gateway (prefix-matched on /v2/message_bus), with the
//     same JWT every other bus call needs.
//
// It deliberately reports HTTP 503 when anything is wrong, so a plain
// HTTP probe is enough to page someone; the body always carries the
// full snapshot, healthy or not, so the reason is never a second
// round-trip away.
const HealthPathSuffix = "/health"

func (r *APIRoute) Health(ctx echo.Context) error {
	health := r.services.Health()

	status := http.StatusOK
	if !health.Healthy {
		status = http.StatusServiceUnavailable
	}

	return ctx.JSON(status, health)
}
