package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/labstack/echo/v4"
)

// ---- TLS / Let's Encrypt certificate management via certbot CLI ----

const certsConfigPath = "/var/lib/casanova/certs.json"

// certConfig is persisted to disk as JSON.
type certConfig struct {
	Domain     string `json:"domain"`
	Email      string `json:"email"`
	Method     string `json:"method"`
	AutoRenew  bool   `json:"auto_renew"`
	LastIssued string `json:"last_issued,omitempty"`
	Expires    string `json:"expires,omitempty"`
	CertPath   string `json:"cert_path,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
}

var (
	certsMu       sync.Mutex
	certsIssuing  bool   // true while a certbot process is running
	certsIssueErr string // last error from background issuance
)

// ---- helpers ----

func loadCertConfig() (*certConfig, error) {
	data, err := os.ReadFile(certsConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg certConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveCertConfig(cfg *certConfig) error {
	// Ensure parent directory exists.
	dir := "/var/lib/casanova"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Wave 35-X: atomic write — power loss mid-write would leave a
	// corrupt certs.json, blocking cert renewal until manual repair.
	return file.WriteFileAtomic(certsConfigPath, data, 0600)
}

func deleteCertConfig() error {
	err := os.Remove(certsConfigPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// parseCertExpiry uses openssl to read the expiry date from a PEM file.
func parseCertExpiry(certPath string) (time.Time, error) {
	out, err := exec.Command("openssl", "x509", "-enddate", "-noout", "-in", certPath).Output()
	if err != nil {
		return time.Time{}, err
	}
	// Output looks like: notAfter=Jul  7 00:00:00 2026 GMT
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("unexpected openssl output: %s", line)
	}
	t, err := time.Parse("Jan  2 15:04:05 2006 MST", parts[1])
	if err != nil {
		// Try alternate format without double space.
		t, err = time.Parse("Jan 2 15:04:05 2006 MST", parts[1])
	}
	return t, err
}

var validHostnameRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)
var validEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// ---- Endpoints ----

// GET /v1/sys/certs/status
func GetCertsStatus(ctx echo.Context) error {
	// requireSystemPrincipal: see antivirus.go's GetAntivirusStatus comment.
	// This response also carries cfg.Email — the operator's email address —
	// so a tokenless caller is not just recon here, same content class as the
	// already-gated /v1/sys/certs/config.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	certsMu.Lock()
	defer certsMu.Unlock()

	if _, err := exec.LookPath("certbot"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data: map[string]interface{}{
				"certbot_installed": false,
				"configured":        false,
				"issuing":           certsIssuing,
				"issue_error":       certsIssueErr,
			},
		})
	}

	cfg, err := loadCertConfig()
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to read cert config: " + err.Error(),
		})
	}

	if cfg == nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data: map[string]interface{}{
				"certbot_installed": true,
				"configured":        false,
				"issuing":           certsIssuing,
				"issue_error":       certsIssueErr,
			},
		})
	}

	result := map[string]interface{}{
		"certbot_installed": true,
		"configured":        true,
		"domain":            cfg.Domain,
		"email":             cfg.Email,
		"method":            cfg.Method,
		"auto_renew":        cfg.AutoRenew,
		"cert_path":         cfg.CertPath,
		"issuing":           certsIssuing,
		"issue_error":       certsIssueErr,
	}

	// Try to read actual expiry from cert file.
	if cfg.CertPath != "" {
		if _, statErr := os.Stat(cfg.CertPath); statErr == nil {
			if expiry, parseErr := parseCertExpiry(cfg.CertPath); parseErr == nil {
				result["expires"] = expiry.Format(time.RFC3339)
				remaining := time.Until(expiry).Hours() / 24
				result["days_remaining"] = int(math.Floor(remaining))
			}
		}
	}

	// Fallback to stored expiry if file not readable.
	if _, ok := result["expires"]; !ok && cfg.Expires != "" {
		result["expires"] = cfg.Expires
		if t, err := time.Parse(time.RFC3339, cfg.Expires); err == nil {
			remaining := time.Until(t).Hours() / 24
			result["days_remaining"] = int(math.Floor(remaining))
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    result,
	})
}

// GET /v1/sys/certs/check
func GetCertsCheck(ctx echo.Context) error {
	// requireSystemPrincipal: see antivirus.go's GetAntivirusStatus comment.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	certbotInstalled := true
	if _, err := exec.LookPath("certbot"); err != nil {
		certbotInstalled = false
	}

	port80Available := false
	ln, err := net.Listen("tcp", ":80")
	if err == nil {
		ln.Close()
		port80Available = true
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"certbot_installed": certbotInstalled,
			"port_80_available": port80Available,
		},
	})
}

// POST /v1/sys/certs/issue
func PostCertsIssue(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Domain string `json:"domain"`
		Email  string `json:"email"`
		Method string `json:"method"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	body.Domain = strings.TrimSpace(body.Domain)
	body.Email = strings.TrimSpace(body.Email)
	if body.Method == "" {
		body.Method = "http"
	}

	// Reject path traversal attempts in domain.
	if strings.Contains(body.Domain, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid domain name",
		})
	}

	// Validate domain.
	if !validHostnameRe.MatchString(body.Domain) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid domain name",
		})
	}

	// Validate email.
	if !validEmailRe.MatchString(body.Email) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid email address",
		})
	}

	// Validate method.
	if body.Method != "http" && body.Method != "dns" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "method must be 'http' or 'dns'",
		})
	}

	if _, err := exec.LookPath("certbot"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "certbot is not installed",
		})
	}

	certsMu.Lock()
	if certsIssuing {
		certsMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "certificate issuance already in progress",
		})
	}
	certsIssuing = true
	certsIssueErr = ""
	certsMu.Unlock()

	// Run certbot in background.
	go func() {
		// Wave 27: panic recovery on the issuance worker. Without it,
		// any nil-deref inside the certbot wrapper takes down the
		// daemon — and `certsIssuing` would stay stuck at true forever
		// because the cleanup defer below never runs.
		defer func() {
			if r := recover(); r != nil {
				certsMu.Lock()
				certsIssueErr = fmt.Sprintf("certbot worker panicked: %v", r)
				certsMu.Unlock()
			}
		}()
		defer func() {
			certsMu.Lock()
			certsIssuing = false
			certsMu.Unlock()
		}()

		var args []string
		if body.Method == "http" {
			args = []string{
				"certonly", "--standalone",
				"-d", body.Domain,
				"--email", body.Email,
				"--agree-tos", "--non-interactive",
			}
		} else {
			args = []string{
				"certonly", "--manual",
				"--preferred-challenges", "dns",
				"-d", body.Domain,
				"--email", body.Email,
				"--agree-tos", "--non-interactive",
			}
		}

		// Bound issuance at 15 minutes. HTTP-01 normally finishes in
		// seconds; DNS-01 manual mode can stall on TXT propagation;
		// 15m is the longest we let it pin a worker before declaring
		// it stuck and freeing certsIssuing for the next attempt.
		issueCtx, issueCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer issueCancel()
		out, err := exec.CommandContext(issueCtx, "certbot", args...).CombinedOutput()
		if errors.Is(issueCtx.Err(), context.DeadlineExceeded) {
			certsMu.Lock()
			certsIssueErr = "certbot issuance timed out after 15m and was killed"
			certsMu.Unlock()
			return
		}
		if err != nil {
			certsMu.Lock()
			certsIssueErr = fmt.Sprintf("certbot failed: %s — %s", err.Error(), strings.TrimSpace(string(out)))
			certsMu.Unlock()
			return
		}

		// Build and save config.
		certPath := fmt.Sprintf("/etc/letsencrypt/live/%s/fullchain.pem", body.Domain)
		keyPath := fmt.Sprintf("/etc/letsencrypt/live/%s/privkey.pem", body.Domain)

		cfg := &certConfig{
			Domain:     body.Domain,
			Email:      body.Email,
			Method:     body.Method,
			AutoRenew:  true,
			LastIssued: time.Now().Format(time.RFC3339),
			CertPath:   certPath,
			KeyPath:    keyPath,
		}

		// Try to read real expiry.
		if expiry, parseErr := parseCertExpiry(certPath); parseErr == nil {
			cfg.Expires = expiry.Format(time.RFC3339)
		} else {
			// Default: 90 days from now.
			cfg.Expires = time.Now().Add(90 * 24 * time.Hour).Format(time.RFC3339)
		}

		certsMu.Lock()
		if err := saveCertConfig(cfg); err != nil {
			certsIssueErr = "certificate issued but failed to save config: " + err.Error()
		}
		certsMu.Unlock()
	}()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]string{"status": "issuing"},
	})
}

// POST /v1/sys/certs/renew
func PostCertsRenew(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("certbot"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "certbot is not installed",
		})
	}

	certsMu.Lock()
	cfg, err := loadCertConfig()
	certsMu.Unlock()
	if err != nil || cfg == nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "no certificate configured",
		})
	}

	// Optional `?dry-run=1` (or POST body `dry_run: true`) skips
	// `--force-renewal` and adds `--dry-run`, so certbot talks to the
	// ACME staging endpoint, performs every step but writes nothing on
	// disk and does not touch the rate limit. Perfect for the "Test
	// Renewal" button in the UI.
	dryRun := ctx.QueryParam("dry-run") == "1"
	if !dryRun {
		body := map[string]any{}
		_ = ctx.Bind(&body)
		if v, ok := body["dry_run"].(bool); ok && v {
			dryRun = true
		}
	}

	// Wave 27: bound certbot at 10 minutes. The previous unbounded
	// exec.Command meant a stuck DNS-01 challenge or a flaky ACME
	// staging endpoint could pin this handler indefinitely. Worse,
	// the post-renewal certsMu.Lock below was unreachable, so EVERY
	// other cert API call piled up behind the wedged renew. Ten
	// minutes is generous (real renewals finish in seconds for HTTP-01
	// and under a minute for DNS-01) but short enough that an admin
	// can recover without restarting the daemon.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	argv := []string{"renew"}
	if dryRun {
		argv = append(argv, "--dry-run")
	} else {
		argv = append(argv, "--force-renewal")
	}
	out, err := exec.CommandContext(cmdCtx, "certbot", argv...).CombinedOutput()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return ctx.JSON(http.StatusGatewayTimeout, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "certbot renew timed out after 10m and was killed",
		})
	}
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("certbot renew failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}

	// Dry-run finished — report the certbot transcript so the UI can
	// show "looks good, your real renewal will work". Don't touch
	// LastIssued/Expires; nothing actually changed.
	if dryRun {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data: map[string]string{
				"dry_run": "true",
				"output":  strings.TrimSpace(string(out)),
			},
		})
	}

	// Update config with new expiry.
	certsMu.Lock()
	defer certsMu.Unlock()
	cfg.LastIssued = time.Now().Format(time.RFC3339)
	if expiry, parseErr := parseCertExpiry(cfg.CertPath); parseErr == nil {
		cfg.Expires = expiry.Format(time.RFC3339)
	} else {
		cfg.Expires = time.Now().Add(90 * 24 * time.Hour).Format(time.RFC3339)
	}
	if err := saveCertConfig(cfg); err != nil {
		log.Println("warn: failed to save cert config after renewal:", err)
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Certificate renewed",
		Data:    map[string]string{"status": "renewed", "output": string(out)},
	})
}

// DELETE /v1/sys/certs
func DeleteCerts(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	certsMu.Lock()
	defer certsMu.Unlock()

	cfg, err := loadCertConfig()
	if err != nil || cfg == nil {
		// Nothing to delete.
		_ = deleteCertConfig()
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "No certificate to remove",
		})
	}

	// Try to revoke and delete via certbot (best-effort).
	// Bound at 2 minutes each — these are local operations, but
	// certbot can stall on a missing ACME directory file.
	if _, certbotErr := exec.LookPath("certbot"); certbotErr == nil {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer revokeCancel()
		if cfg.CertPath != "" {
			if err := exec.CommandContext(revokeCtx, "certbot", "revoke", "--cert-path", cfg.CertPath, "--non-interactive").Run(); err != nil {
				log.Println("warn: certbot revoke failed:", err)
			}
		}
		delCtx, delCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer delCancel()
		if cfg.Domain != "" {
			if err := exec.CommandContext(delCtx, "certbot", "delete", "--cert-name", cfg.Domain, "--non-interactive").Run(); err != nil {
				log.Println("warn: certbot delete failed:", err)
			}
		}
	}

	if err := deleteCertConfig(); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to remove config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Certificate removed",
	})
}
