package v1

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// validContainerID matches Docker container IDs (hex) and container names.
// Docker names: [a-zA-Z0-9][a-zA-Z0-9_.-]+  Docker IDs: hex, 12-64 chars.
var validContainerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// PostContainerStart starts a Docker container by ID or name.
// POST /v1/container/:id/start
func PostContainerStart(ctx echo.Context) error {
	return dockerAction(ctx, "start")
}

// PostContainerStop stops a Docker container (10s grace).
// POST /v1/container/:id/stop
func PostContainerStop(ctx echo.Context) error {
	return dockerAction(ctx, "stop")
}

// PostContainerRestart restarts a Docker container.
// POST /v1/container/:id/restart
func PostContainerRestart(ctx echo.Context) error {
	return dockerAction(ctx, "restart")
}

func dockerAction(ctx echo.Context, action string) error {
	// start/stop/restart a container is a privileged mutation; require the
	// app-start permission (held by user and admin roles, not guests).
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}
	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": common_err.CLIENT_ERROR,
			"message": "container id required",
		})
	}

	// Validate container ID format to prevent Docker CLI flag injection.
	// IDs starting with "-" would be parsed as flags by the docker binary.
	if !validContainerID.MatchString(id) || len(id) > 64 {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": common_err.CLIENT_ERROR,
			"message": "invalid container id format",
		})
	}

	timeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	args := []string{action}
	if action == "stop" || action == "restart" {
		args = append(args, "-t", "10")
	}
	// "--" terminates flag parsing so the ID is never treated as a flag.
	args = append(args, "--", id)

	out, err := exec.CommandContext(timeout, "docker", args...).CombinedOutput()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": common_err.SERVICE_ERROR,
			"message": fmt.Sprintf("failed to %s container: %s", action, strings.TrimSpace(string(out))),
		})
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": common_err.SUCCESS,
		"message": common_err.GetMsg(common_err.SUCCESS),
	})
}
