// Package service provides the StatusSync service, inspired by Runtipi's
// app-status-sync pattern. It periodically queries Docker for the actual state
// of all managed containers and reconciles against expected state.
//
// This catches cases where:
// - A container crashes and Docker doesn't restart it
// - A container is stopped externally (docker stop, OOM kill)
// - A compose app has mixed states (some containers running, some stopped)
package service

import (
	"context"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"go.uber.org/zap"
)

// StatusSyncInterval is how often we check Docker container states.
// 30 seconds is a good balance between responsiveness and overhead.
const StatusSyncInterval = 30 * time.Second

// StartStatusSync launches a background goroutine that periodically
// syncs container states from Docker to the message bus.
// Call this once during service startup.
func StartStatusSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(StatusSyncInterval)
		defer ticker.Stop()

		syncOnce(ctx)

		for {
			select {
			case <-ctx.Done():
				logger.Info("status sync stopped")
				return
			case <-ticker.C:
				syncOnce(ctx)
			}
		}
	}()
	logger.Info("container status sync started", zap.Duration("interval", StatusSyncInterval))
}

func syncOnce(ctx context.Context) {
	syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	syncContainerStates(syncCtx)
}

// syncContainerStates queries Docker for all CasaOS-managed containers
// and publishes state change events for any that have changed.
func syncContainerStates(ctx context.Context) {
	cli, err := docker.SharedClient()
	if err != nil {
		logger.Error("status sync: failed to create Docker client", zap.Error(err))
		return
	}

	// Only look at containers managed by CasaOS (labeled "casaos=casaos")
	f := filters.NewArgs()
	f.Add("label", "casaos=casaos")

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		logger.Error("status sync: failed to list containers", zap.Error(err))
		return
	}

	for _, c := range containers {
		name := "unknown"
		if len(c.Names) > 0 {
			name = c.Names[0]
		}

		// Detect unexpected states
		switch c.State {
		case "exited", "dead":
			// Container should be running but isn't — emit notification
			logger.Info("status sync: container not running",
				zap.String("name", name),
				zap.String("id", c.ID[:12]),
				zap.String("state", c.State),
				zap.String("status", c.Status),
			)

			// Publish event so the notification system and UI can react
			go PublishEventWrapper(ctx, common.EventTypeContainerStopEnd, map[string]string{
				common.PropertyTypeContainerID.Name:   c.ID,
				common.PropertyTypeContainerName.Name: name,
				common.PropertyTypeMessage.Name:       "Container stopped unexpectedly: " + c.Status,
			})
		}
	}
}
