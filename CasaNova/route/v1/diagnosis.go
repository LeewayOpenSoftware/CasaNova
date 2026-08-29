package v1

import (
	"net/http"
	"sync"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/diagnosis"
	"github.com/labstack/echo/v4"
)

// diagEngine is the package-level diagnosis engine, built once (sync.Once —
// the previous lazy nil-check raced on the first concurrent requests and
// could construct two engines with separate caches).
var (
	diagEngine     *diagnosis.Engine
	diagEngineOnce sync.Once
)

func getDiagEngine() *diagnosis.Engine {
	diagEngineOnce.Do(func() {
		diagEngine = diagnosis.NewEngine()
		for _, check := range diagnosis.BuildDefaultChecks() {
			diagEngine.Register(check)
		}
	})
	return diagEngine
}

// diagnosisResponse is the JSON shape returned by the diagnosis endpoint.
type diagnosisResponse struct {
	Results  []diagnosis.Result                        `json:"results"`
	Summary  map[diagnosis.Severity]int                `json:"summary"`
	ByCat    map[diagnosis.Category][]diagnosis.Result `json:"by_category"`
	HasError bool                                      `json:"has_error"`
}

// GetDiagnosis runs all registered diagnostic checks and returns structured results.
// Pass ?force=true to bypass the 5-minute cache.
// Endpoint: GET /v1/sys/diagnosis
func GetDiagnosis(ctx echo.Context) error {
	// System internals (versions, ports, service states, SSH posture) are
	// reconnaissance gold. A perm check alone is NOT enough here: guest
	// deliberately holds PermSystemRead so tokenless internal reads pass,
	// and the gateway makes every LAN caller look loopback — so require an
	// actual LOGIN first (UserIDFromContext is 0 unless the JWT middleware
	// validated a real token; canonical pattern from casanova_search.go).
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	force := ctx.QueryParam("force") == "true"

	engine := getDiagEngine()
	results := engine.Run(ctx.Request().Context(), force)

	return ctx.JSON(http.StatusOK, diagnosisResponse{
		Results:  results,
		Summary:  diagnosis.Summary(results),
		ByCat:    diagnosis.ByCategory(results),
		HasError: diagnosis.HasErrors(results),
	})
}
