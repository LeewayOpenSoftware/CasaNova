package v2

import (
	"context"
	"net"

	"github.com/labstack/echo/v4"
)

// InternalSocketDir/Path: the unix socket where the driver serves sibling
// ROOT services (the main service's power orchestrator). It lives inside a
// 0700 root-only subdirectory of /run/casanova (this unit's RuntimeDirectory,
// preserved across restarts): the directory closes the bind→chmod TOCTOU
// window — even while the fresh socket briefly carries umask permissions, a
// non-root peer cannot traverse the parent to reach it. The socket itself is
// still chmod'd 0600 right after bind. Keep in sync with the client constant
// in CasaNova/service/power_ladder.go.
const (
	InternalSocketDir  = "/run/casanova/internal"
	InternalSocketPath = InternalSocketDir + "/driver.sock"
)

// internalSocketCtxKeyT is the request-context key marking requests accepted
// on the internal unix-socket listener. Unexported struct type: no other
// package can forge or read the stamp. Mirrors the MessageBus Wave-32 design —
// the trust boundary is the listener that accepted the connection, NOT any
// attacker-influencable HTTP artifact (Host header, RemoteAddr, loopback).
type internalSocketCtxKeyT struct{}

var internalSocketCtxKey = internalSocketCtxKeyT{}

// InternalSocketConnContext is wired as http.Server.ConnContext on the unix
// socket server ONLY (main.go). Every connection accepted there is stamped as
// internal; TCP connections can never carry the stamp.
func InternalSocketConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, internalSocketCtxKey, true)
}

// isFromInternalSocket reports whether the request arrived over the 0600
// internal unix socket (and therefore comes from a root-owned sibling service).
func isFromInternalSocket(c echo.Context) bool {
	v, ok := c.Request().Context().Value(internalSocketCtxKey).(bool)
	return ok && v
}
