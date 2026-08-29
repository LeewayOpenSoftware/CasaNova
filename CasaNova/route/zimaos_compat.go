package route

// ZimaOS-client compatibility shim.
//
// Existing ZimaOS mobile/desktop clients probe a server at /v2/zimaos/* to
// decide whether to talk to it. These endpoints let such a client recognise
// and connect to a CasaNova box (LAN, proof-of-concept). Auth, app grid and
// storage already answer via the shared CasaOS lineage.
//
// Naming: the ROUTE PATHS and JSON FIELD NAMES are the exact on-the-wire
// contract the client checks and must stay verbatim. Everything else here is
// CasaNova's own generic code — we do not ship ZimaOS branding.
//
// ponytail: best-guess response shapes for a spoof PoC. The authoritative
// shape is whatever a real client accepts — point one at this box and tighten
// any field it rejects. Start with the identity gate; grow only as needed.

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// compatOSVersion is the ZimaOS version we present to clients. The client
// gates on this looking like a supported ZimaOS release; 1.6.2 matches the
// client generation we analysed. ponytail: bump only if a newer client rejects it.
const compatOSVersion = "1.6.2"

// registerZimaCompat mounts the exact paths ZimaOS clients call. Called from
// InitV2Router; the OpenAPI validator is skipped for /v2/zimaos/* (these
// aren't in our spec).
func registerZimaCompat(e *echo.Echo) {
	e.GET("/v2/zimaos/device/info", zimaDeviceInfo)
	e.GET("/v2/zimaos/device/image", zimaDeviceImage)
	e.GET("/v2/zimaos/zt/info", zimaZTInfoStub)
	// Modules system: ZimaOS-only, none on a generic box.
	e.GET("/v2/mod_management/modules", zimaModulesStub)
}

// registerZimaCompatV1 mounts the v1-path compat routes on the v1 echo. These
// are unauthenticated probes a client hits before login (like /ping), so they
// live outside the JWT group.
func registerZimaCompatV1(e *echo.Echo) {
	e.GET("/v1/doc/features", zimaDocFeatures)
	e.GET("/v1/doc/features.html", zimaDocFeaturesHTML)
}

// zimaDeviceInfo answers the HTTP identity probe on the gateway. Returns the
// FLAT device document (no {"data":...} envelope) — the same builder the
// :9527 connect gate uses (route/zimaos_gate.go) — so a ZimaOS client that
// re-fetches it post-connect parses it cleanly into its flat DeviceInfo model.
func zimaDeviceInfo(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, zimaDeviceInfoFlat())
}

// zimaDeviceImage: no device photo on a generic box.
func zimaDeviceImage(ctx echo.Context) error {
	return ctx.NoContent(http.StatusNoContent)
}

// zimaZTInfoStub: remote access (ZeroTier) isn't wired for the LAN PoC.
func zimaZTInfoStub(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, map[string]any{"data": map[string]any{}})
}

// zimaModulesStub: no ZimaOS "modules" on a generic box — empty list, not 404,
// so a client that probes this on connect doesn't bail.
func zimaModulesStub(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, map[string]any{"data": map[string]any{"modules": []any{}}})
}

// zimaDocFeatures / zimaDocFeaturesHTML: capability-doc probes. Return 200 with
// an empty feature set rather than 404 so the connect flow proceeds.
func zimaDocFeatures(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, map[string]any{"data": map[string]any{"features": []any{}}})
}

func zimaDocFeaturesHTML(ctx echo.Context) error {
	return ctx.HTML(http.StatusOK, "<!doctype html><title>features</title>")
}
