package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// GetDockerImageTags fetches available tags for a Docker image from the
// registry. Supports Docker Hub (library/ and user/ images) and other
// registries that implement the Docker Registry HTTP API V2.
//
// Route: GET /v1/container/image/tags?image=nginx
//
//	GET /v1/container/image/tags?image=linuxserver/jellyfin
//	GET /v1/container/image/tags?image=ghcr.io/immich-app/immich-server
func GetDockerImageTags(ctx echo.Context) error {
	// Same family as /v1/sys/proxy: a handler that makes THIS box open a
	// socket to the internet on client demand. The destination is confined to
	// allowedRegistries below, but the CALLER was never checked, so it was an
	// anonymous egress and amplification primitive. PermAppInstall is
	// admin-only and matches the only real caller (the app install/update UI).
	// See outbound_fetch.go for why the login check is part of the gate.
	if err := requireOutboundFetchCaller(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	image := ctx.QueryParam("image")
	if image == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "image query parameter is required",
		})
	}

	release, err := acquireOutboundSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	tags, err := fetchRegistryTags(ctx.Request().Context(), image)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to fetch tags: " + err.Error(),
			Data:    []string{"latest"},
		})
	}

	// Cap at 50 most recent tags (registries can return thousands).
	if len(tags) > 50 {
		tags = tags[:50]
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    tags,
	})
}

// allowedRegistries is the set of registry hostnames that the server is
// willing to contact on behalf of a client request. This prevents SSRF
// attacks where an attacker supplies an internal hostname as the registry.
var allowedRegistries = map[string]bool{
	"registry-1.docker.io":    true,
	"docker.io":               true,
	"ghcr.io":                 true,
	"quay.io":                 true,
	"gcr.io":                  true,
	"registry.hub.docker.com": true,
}

// fetchRegistryTags queries the Docker registry for available tags.
//
// Uses the shared outboundSafeClient rather than a bare http.Client: the
// registry hostname is confined by allowedRegistries, but the shared client is
// also what enforces the dial-time private-range refusal and the request
// timeouts, and both bodies are read through a cap so a hostile or broken
// registry cannot make this process buffer an unbounded response.
func fetchRegistryTags(reqCtx context.Context, image string) ([]string, error) {
	registry, repo := parseImageRef(image)

	if !allowedRegistries[registry] {
		return nil, fmt.Errorf("registry %q is not in the allowed list", registry)
	}

	var apiURL, tokenURL string
	if registry == "registry-1.docker.io" || registry == "docker.io" {
		// Docker Hub uses a token-based auth flow.
		tokenURL = fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)
		apiURL = fmt.Sprintf("https://registry-1.docker.io/v2/%s/tags/list", repo)
	} else {
		// Generic V2 registry (ghcr.io, quay.io, etc.)
		apiURL = fmt.Sprintf("https://%s/v2/%s/tags/list", registry, repo)
		tokenURL = ""
	}

	fetchCtx, cancel := context.WithTimeout(reqCtx, outboundFetchTimeout)
	defer cancel()

	// Get auth token if needed (Docker Hub).
	var bearerToken string
	if tokenURL != "" {
		tokenReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, tokenURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := outboundSafeClient.Do(tokenReq)
		if err != nil {
			return nil, fmt.Errorf("auth request failed: %w", err)
		}
		body, readErr := readUpstreamBody(resp, outboundMaxReadBytes)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		var tokenResp struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &tokenResp); err == nil {
			bearerToken = tokenResp.Token
		}
	}

	// Fetch tags list.
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := outboundSafeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	body, err := readUpstreamBody(resp, outboundMaxReadBytes)
	if err != nil {
		return nil, err
	}

	var tagsResp struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &tagsResp); err != nil {
		return nil, fmt.Errorf("failed to parse tags response: %w", err)
	}

	// Sort: put common tags first (latest, stable, etc.), then version-like tags.
	tags := tagsResp.Tags
	prioritized := prioritizeTags(tags)
	return prioritized, nil
}

// parseImageRef splits a Docker image reference into registry and repository.
// Examples:
//
//	"nginx"                           -> ("registry-1.docker.io", "library/nginx")
//	"linuxserver/jellyfin"            -> ("registry-1.docker.io", "linuxserver/jellyfin")
//	"ghcr.io/immich-app/immich-server" -> ("ghcr.io", "immich-app/immich-server")
func parseImageRef(image string) (registry, repo string) {
	// Strip tag if present.
	if idx := strings.LastIndex(image, ":"); idx > 0 {
		// Make sure it's a tag, not a port in the registry hostname.
		afterColon := image[idx+1:]
		if !strings.Contains(afterColon, "/") {
			image = image[:idx]
		}
	}

	parts := strings.SplitN(image, "/", 3)
	if len(parts) == 1 {
		// "nginx" -> Docker Hub official image
		return "registry-1.docker.io", "library/" + parts[0]
	}
	if len(parts) == 2 {
		// Could be "user/repo" (Docker Hub) or "ghcr.io/repo"
		if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
			// Has a dot or port -> it's a registry hostname: "ghcr.io/repo"
			return parts[0], parts[1]
		}
		// "linuxserver/jellyfin" -> Docker Hub user repo
		return "registry-1.docker.io", parts[0] + "/" + parts[1]
	}
	// "ghcr.io/immich-app/immich-server"
	return parts[0], parts[1] + "/" + parts[2]
}

// prioritizeTags sorts tags so common ones appear first: latest, stable,
// then version-like tags in reverse (newest first), then everything else.
func prioritizeTags(tags []string) []string {
	var priority []string
	var versions []string
	var rest []string

	prioritySet := map[string]bool{
		"latest": true, "stable": true, "lts": true,
		"edge": true, "nightly": true, "beta": true, "alpine": true,
	}

	for _, t := range tags {
		tl := strings.ToLower(t)
		if prioritySet[tl] {
			priority = append(priority, t)
		} else if len(t) > 0 && (t[0] >= '0' && t[0] <= '9' || t[0] == 'v') {
			versions = append(versions, t)
		} else {
			rest = append(rest, t)
		}
	}

	// Reverse versions so newest (highest) appear first.
	for i, j := 0, len(versions)-1; i < j; i, j = i+1, j-1 {
		versions[i], versions[j] = versions[j], versions[i]
	}

	result := make([]string, 0, len(tags))
	result = append(result, priority...)
	result = append(result, versions...)
	result = append(result, rest...)
	return result
}
