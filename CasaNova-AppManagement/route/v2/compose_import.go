package v2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// ── POST /v2/app_management/compose/import ──────────────────────────
// Accepts a raw docker-compose YAML string, validates it, writes it to
// the apps directory, and starts `docker compose up -d`.

type composeImportRequest struct {
	YAML string `json:"yaml"`
}

func (a *AppManagement) PostComposeImport(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	var req composeImportRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid request body: " + err.Error(),
		})
	}

	if strings.TrimSpace(req.YAML) == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "yaml field is required and must not be empty",
		})
	}

	// Reject oversized YAML to prevent billion-laughs / YAML bomb attacks.
	const maxComposeYAMLSize = 1 * 1024 * 1024 // 1 MB
	if len(req.YAML) > maxComposeYAMLSize {
		return ctx.JSON(http.StatusRequestEntityTooLarge, map[string]interface{}{
			"success": 413,
			"message": "YAML content exceeds maximum allowed size",
		})
	}

	// Validate that the content is parseable YAML with at least a
	// "services" key (minimal docker-compose requirement).
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(req.YAML), &parsed); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid YAML: " + err.Error(),
		})
	}
	if _, ok := parsed["services"]; !ok {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "YAML must contain a 'services' key",
		})
	}

	// Reject privileged containers. The CLI import path (dockerRunToCompose)
	// already blocks --privileged, but raw YAML import used to let a caller
	// sneak `privileged: true` past validation and get full host access.
	if svcErr := rejectDangerousComposeOptions(parsed); svcErr != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": svcErr.Error(),
		})
	}

	// Derive project name from the YAML "name" field, falling back to
	// the first service name.
	projectName := yamlProjectName(parsed)
	if projectName == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "could not determine a project name from the compose file — add a top-level 'name' key",
		})
	}

	// Prevent duplicate installs.
	if service.MyService.Compose().IsInstalling(projectName) {
		return ctx.JSON(http.StatusConflict, map[string]interface{}{
			"success": 409,
			"message": fmt.Sprintf("compose app '%s' is already being installed", projectName),
		})
	}

	// Parse through the service layer so extensions / interpolation work.
	composeApp, err := service.NewComposeAppFromYAML([]byte(req.YAML), false, true)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "compose file validation failed: " + err.Error(),
		})
	}

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := service.MyService.Compose().Install(backgroundCtx, composeApp); err != nil {
		logger.Error("failed to start compose import install", zap.Error(err))
		// A rejection is not a server fault. Install now refuses with wrapped
		// sentinels (busy / already installed) alongside the pre-existing
		// "already being installed", so match with errors.Is — a wrapped
		// sentinel slips straight past `==` and surfaces as a 500, which reads
		// to the operator as a broken box rather than "try again".
		if errors.Is(err, service.ErrComposeAppInstalling) {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"success": 409,
				"message": fmt.Sprintf("compose app '%s' is already being installed", projectName),
			})
		}
		if msg, conflict := composeAppConflict(err, projectName); conflict {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"success": 409,
				"message": msg,
			})
		}
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data": map[string]string{
			"id":          projectName,
			"progressUrl": fmt.Sprintf("/v2/app_management/compose/install/%s/progress", projectName),
		},
	})
}

// ── POST /v2/app_management/compose/import-cli ──────────────────────
// Accepts a `docker run ...` command string, converts it to a minimal
// docker-compose YAML, and installs it via the same compose pipeline.

type cliImportRequest struct {
	Command string `json:"command"`
}

func (a *AppManagement) PostComposeImportCli(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	var req cliImportRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid request body: " + err.Error(),
		})
	}

	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "command field is required",
		})
	}

	composeYAML, projectName, err := dockerRunToCompose(cmd)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "failed to parse docker run command: " + err.Error(),
		})
	}

	if service.MyService.Compose().IsInstalling(projectName) {
		return ctx.JSON(http.StatusConflict, map[string]interface{}{
			"success": 409,
			"message": fmt.Sprintf("compose app '%s' is already being installed", projectName),
		})
	}

	composeApp, err := service.NewComposeAppFromYAML(composeYAML, false, true)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "generated compose file validation failed: " + err.Error(),
		})
	}

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := service.MyService.Compose().Install(backgroundCtx, composeApp); err != nil {
		logger.Error("failed to start compose import-cli install", zap.Error(err))
		// A rejection is not a server fault. Install now refuses with wrapped
		// sentinels (busy / already installed) alongside the pre-existing
		// "already being installed", so match with errors.Is — a wrapped
		// sentinel slips straight past `==` and surfaces as a 500, which reads
		// to the operator as a broken box rather than "try again".
		if errors.Is(err, service.ErrComposeAppInstalling) {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"success": 409,
				"message": fmt.Sprintf("compose app '%s' is already being installed", projectName),
			})
		}
		if msg, conflict := composeAppConflict(err, projectName); conflict {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"success": 409,
				"message": msg,
			})
		}
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data": map[string]string{
			"id":          projectName,
			"progressUrl": fmt.Sprintf("/v2/app_management/compose/install/%s/progress", projectName),
		},
	})
}

// ── POST /v2/app_management/compose/:id/check_updates ───────────────
// Reports whether an update is available for the compose app. The source
// of truth is the app-store catalog (AppStoreManagement.IsUpdateAvailable),
// which is the unit of versioning — so the boolean is per-app and copied to
// each service row for the UI's convenience. We deliberately do NOT fabricate
// a "newTag": the store check doesn't query the registry for a concrete new
// tag, and returning the *current* tag as the new one (the old behaviour)
// misled the UI into showing "update to <the version you already run>".
// `newTag` is left empty until a real registry digest/tag comparison lands.

func (a *AppManagement) PostComposeCheckUpdates(ctx echo.Context) error {
	// Wave 34 (ported from 27.04, docs/design/per-app-grants.md §7): this
	// handler had NO authorization gate at all — not the wrong bit, none.
	// The router's JWT middleware still refused an anonymous caller, so it
	// was AUTHENTICATED but UNAUTHORIZED: every logged-in account reached
	// it, and so did any context that resolved to guest without a principal
	// behind it. PermAppView is the bit the whole family gets — guest
	// already holds it, so no role loses anything it has today.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "compose app id is required",
		})
	}

	composeApps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": err.Error(),
		})
	}

	composeApp, ok := composeApps[id]
	if !ok {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"success": 404,
			"message": fmt.Sprintf("compose app '%s' not found", id),
		})
	}

	type serviceUpdate struct {
		ServiceID string `json:"serviceId"`
		HasUpdate bool   `json:"hasUpdate"`
		NewTag    string `json:"newTag,omitempty"`
	}

	// Availability is per-app (the store catalog is the unit of versioning),
	// so compute it once instead of re-querying for every service.
	hasUpdate := service.MyService.AppStoreManagement().IsUpdateAvailable(ctx.Request().Context(), composeApp)

	results := []serviceUpdate{}
	for _, svc := range composeApp.Services {
		if svc.Image == "" {
			continue
		}
		// NewTag intentionally left empty: this store-based check has no
		// concrete registry tag to advertise. Returning the current tag
		// here previously made the UI claim "update to <current version>".
		results = append(results, serviceUpdate{
			ServiceID: svc.Name,
			HasUpdate: hasUpdate,
		})
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"data": map[string]interface{}{
			"services": results,
		},
	})
}

// ── GET /v2/app_management/compose/install/:id/progress ─────────────
// Server-Sent Events endpoint that streams install progress. For now
// this is a lightweight implementation that sends an immediate
// "completed" event — real progress tracking requires deeper integration
// with the docker compose pull/up pipeline.

func (a *AppManagement) GetComposeInstallProgress(ctx echo.Context) error {
	// Same family, same reasoning as the gate above.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "install id is required",
		})
	}

	// Check flusher FIRST. Previously we wrote SSE headers before the
	// flusher check, which meant a server without a flushable writer
	// would double-WriteHeader (once for 200 OK from the SSE prelude,
	// then again inside ctx.JSON(500)). Echo logs the second call as
	// "http: superfluous response.WriteHeader call" and the client
	// never sees the error body.
	flusher, ok := ctx.Response().Writer.(http.Flusher)
	if !ok {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": "streaming not supported",
		})
	}

	// Set SSE headers.
	ctx.Response().Header().Set("Content-Type", "text/event-stream")
	ctx.Response().Header().Set("Cache-Control", "no-cache")
	ctx.Response().Header().Set("Connection", "keep-alive")
	ctx.Response().Header().Set("X-Accel-Buffering", "no")
	ctx.Response().WriteHeader(http.StatusOK)

	// Helper to write an SSE frame. Returns an error if the client
	// disconnected. Uses json.Marshal so newlines, quotes, or control
	// chars inside logLine can't break the "data: …\n\n" framing that
	// the SSE spec depends on.
	sendEvent := func(progress int, status, logLine string) error {
		payload, err := json.Marshal(map[string]interface{}{
			"progress": progress,
			"status":   status,
			"log_line": logLine,
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Response().Writer, "data: %s\n\n", payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		progress, ok := service.MyService.Compose().InstallProgress(id)
		if !ok {
			_ = sendEvent(100, "completed", fmt.Sprintf("Compose app %s is ready", id))
			return nil
		}
		if err := sendEvent(progress.Progress, progress.Status, progress.Message); err != nil {
			return nil
		}
		if progress.Status == "completed" || progress.Status == "failed" {
			return nil
		}
		select {
		case <-ctx.Request().Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────

// yamlProjectName extracts a project name from a parsed compose map.
func yamlProjectName(parsed map[string]interface{}) string {
	if name, ok := parsed["name"].(string); ok && name != "" {
		return sanitizeName(name)
	}
	// Fall back to first service name.
	if svcs, ok := parsed["services"].(map[string]interface{}); ok {
		for name := range svcs {
			return sanitizeName(name)
		}
	}
	return ""
}

// rejectDangerousComposeOptions walks each service entry in a parsed
// compose map and refuses any setting that grants host-level access.
// The CLI import path blocks --privileged at tokenization time; the
// raw YAML import used to skip this check entirely, letting a
// malicious manifest mount /, share host PID/network, or run
// privileged. Keep this list in sync with dockerRunToCompose's flag
// handler.
func rejectDangerousComposeOptions(parsed map[string]interface{}) error {
	svcs, ok := parsed["services"].(map[string]interface{})
	if !ok {
		return nil
	}
	for name, raw := range svcs {
		svc, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := svc["privileged"].(bool); ok && v {
			return fmt.Errorf("service %q: privileged containers are not allowed", name)
		}
		if v, ok := svc["pid"].(string); ok && v == "host" {
			return fmt.Errorf("service %q: pid: host is not allowed", name)
		}
		if v, ok := svc["network_mode"].(string); ok && v == "host" {
			return fmt.Errorf("service %q: network_mode: host is not allowed", name)
		}
		if v, ok := svc["ipc"].(string); ok && v == "host" {
			return fmt.Errorf("service %q: ipc: host is not allowed", name)
		}
		if v, ok := svc["userns_mode"].(string); ok && v == "host" {
			return fmt.Errorf("service %q: userns_mode: host is not allowed", name)
		}
		// Block bind-mounts of sensitive host paths.
		if vols, ok := svc["volumes"].([]interface{}); ok {
			for _, vol := range vols {
				s, ok := vol.(string)
				if !ok {
					continue
				}
				// Short-syntax bind: "<host>:<container>[:mode]"
				parts := strings.SplitN(s, ":", 3)
				if len(parts) < 2 {
					continue
				}
				host := strings.TrimSpace(parts[0])
				if host == "/" || host == "/etc" || host == "/var/run/docker.sock" ||
					host == "/proc" || host == "/sys" || host == "/boot" ||
					host == "/root" || host == "/var/lib/casanova" {
					return fmt.Errorf("service %q: bind mount of %q is not allowed", name, host)
				}
			}
		}
		// Block cap_add with DAC_OVERRIDE / SYS_ADMIN etc.
		if caps, ok := svc["cap_add"].([]interface{}); ok {
			for _, c := range caps {
				if s, ok := c.(string); ok {
					up := strings.ToUpper(s)
					if up == "ALL" || up == "SYS_ADMIN" || up == "DAC_READ_SEARCH" {
						return fmt.Errorf("service %q: cap_add %s is not allowed", name, up)
					}
				}
			}
		}
	}
	return nil
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9-]`)

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = nonAlphaNum.ReplaceAllString(s, "")
	if s == "" {
		s = "imported-app"
	}
	return s
}

// dockerRunToCompose converts a `docker run` command to a minimal
// docker-compose YAML. It handles the most common flags:
//
//	-d, --name, -p, -v, -e, --restart, --privileged, image
func dockerRunToCompose(cmd string) ([]byte, string, error) {
	// Strip leading "docker" and "run" tokens.
	tokens := tokenize(cmd)
	if len(tokens) == 0 {
		return nil, "", fmt.Errorf("empty command")
	}
	// Skip past "docker" and "run"
	start := 0
	if tokens[0] == "docker" {
		start = 1
	}
	if start < len(tokens) && tokens[start] == "run" {
		start++
	}
	tokens = tokens[start:]

	name := ""
	image := ""
	ports := []string{}
	volumes := []string{}
	envVars := map[string]string{}
	restart := "unless-stopped"
	privileged := false

	i := 0
	for i < len(tokens) {
		t := tokens[i]
		switch {
		case t == "-d" || t == "--detach":
			i++
		case t == "--name" && i+1 < len(tokens):
			name = tokens[i+1]
			i += 2
		case strings.HasPrefix(t, "--name="):
			name = strings.TrimPrefix(t, "--name=")
			i++
		case (t == "-p" || t == "--publish") && i+1 < len(tokens):
			ports = append(ports, tokens[i+1])
			i += 2
		case strings.HasPrefix(t, "-p=") || strings.HasPrefix(t, "--publish="):
			ports = append(ports, strings.SplitN(t, "=", 2)[1])
			i++
		case (t == "-v" || t == "--volume") && i+1 < len(tokens):
			volumes = append(volumes, tokens[i+1])
			i += 2
		case strings.HasPrefix(t, "-v=") || strings.HasPrefix(t, "--volume="):
			volumes = append(volumes, strings.SplitN(t, "=", 2)[1])
			i++
		case (t == "-e" || t == "--env") && i+1 < len(tokens):
			kv := strings.SplitN(tokens[i+1], "=", 2)
			if len(kv) == 2 {
				envVars[kv[0]] = kv[1]
			}
			i += 2
		case strings.HasPrefix(t, "-e=") || strings.HasPrefix(t, "--env="):
			val := strings.SplitN(t, "=", 2)[1]
			kv := strings.SplitN(val, "=", 2)
			if len(kv) == 2 {
				envVars[kv[0]] = kv[1]
			}
			i++
		case (t == "--restart") && i+1 < len(tokens):
			restart = tokens[i+1]
			i += 2
		case strings.HasPrefix(t, "--restart="):
			restart = strings.TrimPrefix(t, "--restart=")
			i++
		case t == "--privileged":
			privileged = true
			i++
		case strings.HasPrefix(t, "-"):
			// Skip unknown flags with optional value.
			i++
			if i < len(tokens) && !strings.HasPrefix(tokens[i], "-") {
				i++ // skip the value too
			}
		default:
			// First non-flag token is the image. Everything after is the command.
			image = t
			i++
			// Remaining tokens are the container command — ignore for compose purposes.
			i = len(tokens)
		}
	}

	if image == "" {
		return nil, "", fmt.Errorf("no image found in docker run command")
	}

	// Reject privileged containers — they grant full host access.
	if privileged {
		return nil, "", fmt.Errorf("privileged containers are not allowed")
	}

	if name == "" {
		// Derive name from image.
		parts := strings.Split(image, "/")
		name = strings.Split(parts[len(parts)-1], ":")[0]
	}
	projectName := sanitizeName(name)

	// Build the compose YAML structure.
	svc := map[string]interface{}{
		"image":   image,
		"restart": restart,
	}
	if name != "" {
		svc["container_name"] = name
	}
	if len(ports) > 0 {
		svc["ports"] = ports
	}
	if len(volumes) > 0 {
		svc["volumes"] = volumes
	}
	if len(envVars) > 0 {
		envList := []string{}
		for k, v := range envVars {
			envList = append(envList, k+"="+v)
		}
		svc["environment"] = envList
	}
	compose := map[string]interface{}{
		"name": projectName,
		"services": map[string]interface{}{
			projectName: svc,
		},
	}

	// Add the canonical x-casanova extension so the service layer
	// treats it properly. v0.1 writes the new key; the reader at
	// common.ComposeExtension also accepts x-casaos for legacy
	// upstream-store compose files.
	compose["x-casanova"] = map[string]interface{}{
		"main":         projectName,
		"store_app_id": projectName,
		"title": map[string]string{
			"en_us": name,
		},
	}

	out, err := yaml.Marshal(compose)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate compose YAML: %w", err)
	}

	// Also write to disk so LoadComposeAppFromConfigFile can find it.
	workDir := filepath.Join(config.AppInfo.AppsPath, projectName)
	// DATA-PRESERVATION GUARD — never overwrite an already-installed app.
	// This writer reconstructs the compose from the docker-run STRING alone
	// (no volumes unless -v was given, no env unless -e, image often untagged),
	// so clobbering an installed app's docker-compose.yml here silently drops
	// its bind mounts; on the next recreate Docker mounts empty anonymous
	// volumes over them → total data loss. This is exactly how navidrome was
	// wiped on 2026-07-17. import-cli is an ADD-NEW-app path, so a name
	// collision with an installed app must fail loudly, not clobber.
	if _, err := os.Stat(filepath.Join(workDir, common.ComposeYAMLFileName)); err == nil {
		return nil, "", fmt.Errorf("an app named %q is already installed — refusing to overwrite it from a docker run command; uninstall it first or import under a different name", projectName)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("failed to create compose app directory: %w", err)
	}
	// Wave 35-X: atomic write. A crash mid-write would leave a
	// half-written docker-compose.yml — at next boot the project
	// fails to parse and the entire stack disappears from the UI.
	if err := file.WriteFileAtomic(filepath.Join(workDir, common.ComposeYAMLFileName), out, 0o600); err != nil {
		return nil, "", fmt.Errorf("failed to write compose file: %w", err)
	}

	return out, projectName, nil
}

// tokenize splits a shell-like command into tokens, respecting single
// and double quotes.
func tokenize(s string) []string {
	var tokens []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case ch == '\\' && inDouble && i+1 < len(s):
			i++
			current.WriteByte(s[i])
		case (ch == ' ' || ch == '\t') && !inSingle && !inDouble:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}
