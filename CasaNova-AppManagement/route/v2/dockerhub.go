package v2

import (
	"encoding/json"
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
	Pulls       int64  `json:"pulls"`
	Official    bool   `json:"official"`
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

	api := "https://hub.docker.com/v2/search/repositories/?page_size=25&query=" + url.QueryEscape(q)
	httpClient := &http.Client{Timeout: 8 * time.Second}
	resp, err := httpClient.Get(api)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "Docker Hub unreachable: " + err.Error()})
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "Docker Hub returned " + resp.Status})
	}

	var raw struct {
		Results []struct {
			RepoName         string `json:"repo_name"`
			ShortDescription string `json:"short_description"`
			StarCount        int    `json:"star_count"`
			PullCount        int64  `json:"pull_count"`
			IsOfficial       bool   `json:"is_official"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]string{"message": "invalid Docker Hub response"})
	}

	out := make([]DockerHubResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		name := r.RepoName
		// Official images (nginx, postgres, …) live under the implicit
		// "library" namespace; make it explicit so the pull name is valid.
		if r.IsOfficial && !strings.Contains(name, "/") {
			name = "library/" + name
		}
		out = append(out, DockerHubResult{
			Name:        name,
			Description: r.ShortDescription,
			Stars:       r.StarCount,
			Pulls:       r.PullCount,
			Official:    r.IsOfficial,
		})
	}
	return ctx.JSON(http.StatusOK, out)
}
