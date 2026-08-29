package v1

// TLS configuration endpoint — stores the (enabled, domain, email) trio
// that the existing /v1/sys/certs/issue|renew|remove handlers consume.
// Previously the UI had these inputs but no way to persist them, so the
// cert flow was forever called with empty strings.
//
//   GET /v1/sys/certs/config → { enabled: bool, domain: string, email: string }
//   PUT /v1/sys/certs/config { enabled, domain, email }
//
// Persisted as /etc/casanova/tls-config.json with mode 0600 (it contains
// an email address that the operator may not want world-readable).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

const tlsConfigPath = "/etc/casanova/tls-config.json"

// domainRe accepts plain DNS labels separated by dots. Strict enough to
// keep shell-meta out of the cert-issue command line; permissive enough
// to allow wildcards (\\*.example.com).
var domainRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

// emailRe is the cheap-but-good-enough check. Real validation is the
// ACME server's problem.
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

type tlsConfig struct {
	Enabled bool   `json:"enabled"`
	Domain  string `json:"domain"`
	Email   string `json:"email"`
}

func readTLSConfig() tlsConfig {
	c := tlsConfig{}
	data, err := os.ReadFile(tlsConfigPath)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func writeTLSConfig(c tlsConfig) error {
	if err := os.MkdirAll(filepath.Dir(tlsConfigPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := tlsConfigPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tlsConfigPath)
}

func GetTLSConfig(ctx echo.Context) error {
	// Gated: certificate configuration; PermSystemRead is held by guest, so identity must be required first.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    readTLSConfig(),
	})
}

func PutTLSConfig(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var c tlsConfig
	if err := ctx.Bind(&c); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	c.Domain = strings.TrimSpace(c.Domain)
	c.Email = strings.TrimSpace(c.Email)

	// Domain + email are only validated when TLS is being turned ON or
	// either field is non-empty. This lets the user clear the form
	// without bumping into the validators.
	if c.Enabled || c.Domain != "" || c.Email != "" {
		if !domainRe.MatchString(c.Domain) {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.INVALID_PARAMS,
				Message: "domain must be a valid DNS name",
			})
		}
		if !emailRe.MatchString(c.Email) {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.INVALID_PARAMS,
				Message: "email must be a valid address",
			})
		}
	}

	if err := writeTLSConfig(c); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to persist tls config: " + err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    c,
	})
}
