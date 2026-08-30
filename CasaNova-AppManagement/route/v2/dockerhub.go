package v2

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// DockerHubResult is one repository from a Docker Hub search, trimmed to what
// the install wizard shows.
type DockerHubResult struct {
	Name        string `json:"name"` // pullable name (library/<x> for official)
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	// A human string ("1B+", "50K+"), which is what v4 returns — Docker Hub
	// does the rounding now, and passing its own wording through beats
	// reformatting an approximation into a fake-precise number.
	Pulls    string `json:"pulls"`
	Official bool   `json:"official"`
}

// SearchDockerHub proxies Docker Hub's public repository search so the install
// wizard can "search Docker Hub" without the browser hitting CORS / Docker Hub
// rate limits directly. Install-adjacent → PermAppInstall.
// Route: GET /v2/app_management/dockerhub/search?q=<query>
func (a *AppManagement) SearchDockerHub(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	q := strings.TrimSpace(ctx.QueryParam("q"))
	if q == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "query 'q' is required"})
	}

	// Docker Hub's /v2/search/repositories/ endpoint now answers 403 to this
	// client no matter what headers it carries — verified from the box with a
	// Go binary against every combination of User-Agent and Accept, while
	// curl and python got 200 from the same host with the same headers. The
	// old path is simply closed to non-browser clients. /api/search/v4 is
	// what Docker Hub's own web UI calls, and it answers 200 to a plain Go
	// request.
	api := "https://hub.docker.com/api/search/v4?size=25&query=" + url.QueryEscape(q)
	httpClient := &http.Client{Timeout: 8 * time.Second}
	resp, err := httpClient.Get(api)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "Docker Hub unreachable: " + err.Error()})
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "Docker Hub returned " + resp.Status})
	}

	out, err := parseDockerHubV4(resp.Body)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "invalid Docker Hub response"})
	}
	return ctx.JSON(http.StatusOK, out)
}

// parseDockerHubV4 maps one /api/search/v4 payload to what the wizard shows.
//
// Split out from the handler so the SHAPE can be pinned against a captured
// real response — the shape is what broke: the previous code decoded the old
// endpoint's schema, where the pull name was `repo_name`, "official" was a
// bool, and pull_count was a number. v4 renamed the first, turned the second
// into a `badge` string and the third into a human string like "1B+", any one
// of which silently empties the result list.
func parseDockerHubV4(body io.Reader) ([]DockerHubResult, error) {
	var raw struct {
		Results []struct {
			Slug             string `json:"slug"`
			Name             string `json:"name"`
			Type             string `json:"type"`
			Badge            string `json:"badge"`
			ShortDescription string `json:"short_description"`
			StarCount        int    `json:"star_count"`
			PullCount        string `json:"pull_count"`
		} `json:"results"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, err
	}

	out := make([]DockerHubResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		// Only actual images are installable. v4 also returns Docker
		// Extensions, plugins and hardened-image listings, which would fail
		// at pull time with a confusing error if offered here as apps.
		if r.Type != "" && r.Type != "image" {
			continue
		}
		name := r.Slug
		if name == "" {
			continue
		}
		official := r.Badge == "official"
		// Official images (nginx, postgres, …) live under the implicit
		// "library" namespace; make it explicit so the pull name is valid.
		if official && !strings.Contains(name, "/") {
			name = "library/" + name
		}
		out = append(out, DockerHubResult{
			Name:        name,
			Description: r.ShortDescription,
			Stars:       r.StarCount,
			Pulls:       r.PullCount,
			Official:    official,
		})
	}
	return out, nil
}
