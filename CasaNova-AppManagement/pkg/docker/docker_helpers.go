// Package docker provides helpers for interacting with the Docker daemon
// and container registries. This file consolidates small helpers that
// previously each had their own file: daemon status, registry pull options,
// volume path substitution, and constants.
package docker

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// sharedClient is a package-level Docker client reused across calls to avoid
// exhausting the daemon's connection pool during bulk operations.
var (
	sharedClient     *client.Client
	sharedClientOnce sync.Once
	sharedClientMu   sync.Mutex
)

// SharedClient returns a long-lived Docker client. The client is created once
// via sync.Once; if a subsequent call detects the daemon is unreachable it
// recreates the client (reconnect-on-error).
//
// Callers MUST NOT call Close() on the returned client.
func SharedClient() (*client.Client, error) {
	sharedClientOnce.Do(func() {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err == nil {
			sharedClient = cli
		}
	})

	sharedClientMu.Lock()
	defer sharedClientMu.Unlock()

	if sharedClient == nil {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		sharedClient = cli
		return sharedClient, nil
	}

	// Quick health check — if the daemon is unreachable, reconnect.
	// Bound the ping so a wedged Docker socket cannot freeze every caller
	// that needs the shared client.
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := sharedClient.Ping(pingCtx); err != nil {
		_ = sharedClient.Close()
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		sharedClient = cli
	}

	return sharedClient, nil
}

// NETWORKNAME is the default Docker network name CasaNova creates for its apps.
const NETWORKNAME = "oasis"

// ---------------------------------------------------------------------------
// Daemon status (formerly daemon.go)
// ---------------------------------------------------------------------------

// IsDaemonRunning returns true if the Docker daemon is reachable.
func IsDaemonRunning() bool {
	cli, err := SharedClient()
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	return err == nil
}

// CurrentArchitecture returns the Docker daemon's reported architecture
// (amd64, arm64, etc.) — used for selecting compatible app images.
func CurrentArchitecture() (string, error) {
	cli, err := SharedClient()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ver, err := cli.ServerVersion(ctx)
	if err != nil {
		return "", err
	}
	return ver.Arch, nil
}

// ---------------------------------------------------------------------------
// Volume path substitution (formerly volumes.go)
// ---------------------------------------------------------------------------

// GetDir expands the $AppID placeholder in a volume env path.
// If id is empty or the path doesn't contain $AppID, returns envName unchanged.
func GetDir(id, envName string) string {
	if strings.Contains(envName, "$AppID") && len(id) > 0 {
		return strings.ReplaceAll(envName, "$AppID", id)
	}
	return envName
}

// ---------------------------------------------------------------------------
// Registry pull options (formerly registry.go)
// credit: https://github.com/containrrr/watchtower
// ---------------------------------------------------------------------------

// GetPullOptions builds the options needed to pull an image from a registry,
// including authentication if the image requires it.
func GetPullOptions(imageName string) (image.PullOptions, error) {
	auth, err := EncodedAuth(imageName)
	if err != nil {
		return image.PullOptions{}, err
	}

	if auth == "" {
		return image.PullOptions{}, nil
	}

	return image.PullOptions{
		RegistryAuth:  auth,
		PrivilegeFunc: func(_ context.Context) (string, error) { return "", nil },
	}, nil
}
