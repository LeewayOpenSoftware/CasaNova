package v2

import (
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/labstack/echo/v4"
)

type AppManagement struct{}

func NewAppManagement() codegen.ServerInterface {
	return &AppManagement{}
}

// PropertiesFromQueryParams collects the query parameters that are DECLARED as
// event properties. Everything else stays out of the bus payload.
//
// It used to copy EVERY incoming query parameter, which made any HTTP client
// the author of arbitrary fields on an internal event. Two details decide why
// the declared set is narrow rather than "anything the bus knows about":
//
//   - PublishEventWrapper merges the context properties OVER the publisher's
//     own map (service/service.go), so a client-supplied key did not merely get
//     added to the event — it OVERWROTE what the service had just set. A
//     request carrying ?message=... rewrote the error text of an error event,
//     and ?app:icon=... would have pointed every subscriber at a URL of the
//     caller's choosing.
//   - the message bus does not filter either: its PublishEvent stores the
//     properties verbatim and relays them to every subscriber, so nothing
//     downstream would have caught an invented field.
//
// Hence app:name, app:icon, app:title and message are NOT admitted here even
// though they are declared property types: they are service-owned. See
// common.QueryParamEventProperties for the declared set and for why widening it
// is a security decision.
//
// Values are passed through byte-for-byte, including an explicitly empty one —
// the filter chooses which KEYS may travel, never how they look, so a
// subscriber can still tell "sent as empty" from "not sent".
func PropertiesFromQueryParams(httpCtx echo.Context) map[string]string {
	properties := make(map[string]string)

	queryParams := httpCtx.QueryParams()
	for _, declared := range common.QueryParamEventProperties {
		if values, ok := queryParams[declared.Name]; ok && len(values) > 0 {
			properties[declared.Name] = values[0]
		}
	}

	return properties
}

func DefaultQuery(ctx echo.Context, key string, defaultValue string) string {
	if value := ctx.QueryParam(key); value != "" {
		return value
	}

	return defaultValue
}
