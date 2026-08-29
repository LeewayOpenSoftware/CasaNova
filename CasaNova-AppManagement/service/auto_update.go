package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// shortDigest returns the first 12 characters of an image digest, or
// the whole string if it's shorter. Used for human-readable log lines.
// The previous code did `digest[:12]` directly which panics on any
// digest shorter than 12 chars (malformed registry response, etc.).
func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// AutoUpdateLabel is the Docker label that opts a container in for auto-update.
// Set to "true" to enable. Users can toggle this from the UI or via docker CLI:
//
//	docker label casanova-auto-update=true <container>
const AutoUpdateLabel = "casanova-auto-update"

// composeProjectLabel is the label compose v2 stamps on every container
// that belongs to a project. We use it to split the auto-update sweep
// into compose-managed vs standalone so each group takes the right code
// path (see Wave 35-U in checkAutoUpdates).
const composeProjectLabel = "com.docker.compose.project"

// AutoUpdateInterval is how often we check for image updates.
// Daily is a reasonable default — more frequent than this hammers registries.
const AutoUpdateInterval = 24 * time.Hour

// StartAutoUpdate launches a background goroutine that periodically checks
// labeled containers for image updates and recreates them if found.
//
// Opt-in: only containers with label casanova-auto-update=true are checked.
// This prevents surprise updates on critical containers.
func StartAutoUpdate(ctx context.Context) {
	go func() {
		// Wave 31: recover() so a panic anywhere inside the daily
		// auto-update sweep (Docker SDK quirk on a corrupted image,
		// nil deref in RecreateContainer's clone path, etc.) does
		// not silently take auto-update offline for the entire
		// daemon lifetime. Without this, the user thinks their
		// containers are being kept up to date when in fact the
		// loop died on the very first malformed image weeks ago.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("auto-update goroutine panicked — auto-updates are OFFLINE until restart",
					zap.Any("panic", r))
			}
		}()
		// Initial check after 5 minutes (give services time to start)
		initialDelay := time.NewTimer(5 * time.Minute)
		select {
		case <-ctx.Done():
			initialDelay.Stop()
			logger.Info("auto-update stopped during initial delay")
			return
		case <-initialDelay.C:
		}
		checkAutoUpdates(ctx)

		ticker := time.NewTicker(AutoUpdateInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("auto-update stopped")
				return
			case <-ticker.C:
				checkAutoUpdates(ctx)
			}
		}
	}()
	logger.Info("container auto-update started",
		zap.Duration("interval", AutoUpdateInterval),
		zap.String("opt_in_label", AutoUpdateLabel),
	)
}

// checkAutoUpdates scans for labeled containers and updates ones with new images.
//
// Wave 35-U: previously this iterated every container and called
// RecreateContainer unconditionally, which used docker.CloneContainer —
// a raw Docker SDK create + rename dance that bypasses compose. On
// compose-managed containers the cloned container ended up with a
// stale `com.docker.compose.config-hash` / `com.docker.compose.image`
// pair, and any failure partway through the clone/stop/start/remove/
// rename sequence left a container that compose-go no longer
// recognized as part of the project. The stack silently vanished from
// the main UI's compose list and the orphan container reappeared in
// the bottom "Individual" list, stopped, requiring the user to go
// into Portainer to manually start them. Icons, store metadata, and
// network attachments were all lost in the same move.
//
// The fix: partition the opted-in containers by `com.docker.compose.project`
// label. Standalone containers (empty label) keep using RecreateContainer.
// Compose-managed containers go through the same PullAndApply path the
// UI's manual "Update" button uses, which is compose.Up under the hood —
// it pulls the new image, lets compose recreate the containers with
// their full and correct label set, reattaches every network, and
// starts them. Multiple containers from the same project are batched
// into a single PullAndApply call so a 3-service stack isn't recreated
// three times in a row.
func checkAutoUpdates(ctx context.Context) {
	cli, err := docker.SharedClient()
	if err != nil {
		logger.Error("auto-update: failed to create Docker client", zap.Error(err))
		return
	}

	// Only list containers with the opt-in label
	f := filters.NewArgs()
	f.Add("label", AutoUpdateLabel+"=true")

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		logger.Error("auto-update: failed to list containers", zap.Error(err))
		return
	}

	if len(containers) == 0 {
		logger.Info("auto-update: no containers opted in")
		return
	}

	logger.Info("auto-update: checking containers", zap.Int("count", len(containers)))

	// Partition by compose project. Empty project label means the
	// container was created outside compose (raw `docker run`, v1
	// CasaOS custom install, etc.) — keep those on the legacy
	// RecreateContainer path. Non-empty project label means one of
	// the container's peers also carries the same label, so we batch
	// them together and update the whole stack once.
	byProject := map[string][]container.Summary{}
	for _, c := range containers {
		project := c.Labels[composeProjectLabel]
		byProject[project] = append(byProject[project], c)
	}

	// Standalone (project == "") — use the legacy clone path. These
	// containers have no compose file on disk and no project state,
	// so a raw clone is actually correct for them.
	for _, c := range byProject[""] {
		checkContainerUpdate(ctx, cli, c)
	}
	delete(byProject, "")

	// Compose-managed — one PullAndApply per project, even when the
	// project has several labeled services.
	for project, projectContainers := range byProject {
		checkComposeProjectUpdate(ctx, cli, project, projectContainers)
	}
}

// checkContainerUpdate checks one standalone (non-compose) container
// for updates and recreates it via the raw clone path if a new image is
// available.
func checkContainerUpdate(ctx context.Context, cli *dockerclient.Client, c container.Summary) {
	name := "unknown"
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}

	// Operation logging
	op := StartOp("app.auto_update_check", name)

	// Get current image digest
	oldImage := c.Image
	oldInspect, _, err := cli.ImageInspectWithRaw(ctx, oldImage)
	if err != nil {
		logger.Error("auto-update: failed to inspect current image",
			zap.String("container", name),
			zap.String("image", oldImage),
			zap.Error(err),
		)
		if op != nil {
			op.Fail(err)
		}
		return
	}
	oldDigest := oldInspect.ID

	// Pull latest
	hasNew, newDigest, err := docker.HasNewImage(ctx, oldImage, oldDigest)
	if err != nil {
		logger.Error("auto-update: failed to check for new image",
			zap.String("container", name),
			zap.Error(err),
		)
		if op != nil {
			op.Fail(err)
		}
		return
	}

	if !hasNew {
		logger.Info("auto-update: no update available",
			zap.String("container", name),
			zap.String("image", oldImage),
		)
		if op != nil {
			op.Complete("no update needed")
		}
		return
	}

	// Wave 32: clamp digest slicing. oldDigest comes from
	// dockerd's container inspect (sha256:hex...), newDigest comes
	// from the registry. In normal operation both are >64 chars,
	// but a malformed/empty digest from a flaky registry response
	// or a partially-pulled image can be shorter — and the
	// previous code did `digest[:12]` unconditionally, which
	// crashes the auto-update goroutine on a runtime slice-bounds
	// panic the moment that happens. Recover() in the caller
	// catches it, but the user's update silently dies. Truncate
	// safely so the log line just shows the whole digest when
	// it's short.
	logger.Info("auto-update: new image available, recreating standalone container",
		zap.String("container", name),
		zap.String("old_digest", shortDigest(oldDigest)),
		zap.String("new_digest", shortDigest(newDigest)),
	)

	// Recreate container with new image.
	// Uses the existing RecreateContainer which handles:
	//   - Pull new image (already done, but idempotent)
	//   - Stop old container
	//   - Clone with new image
	//   - Start new container
	//   - Remove old container
	if err := MyService.Docker().RecreateContainer(ctx, c.ID, true, true); err != nil {
		logger.Error("auto-update: recreation failed",
			zap.String("container", name),
			zap.Error(err),
		)
		if op != nil {
			op.Fail(err)
		}
		return
	}

	logger.Info("auto-update: container updated successfully",
		zap.String("container", name),
	)
	if op != nil {
		op.Complete("updated to new image")
	}
}

// checkComposeProjectUpdate handles the compose-managed half of the
// auto-update sweep: it checks whether any service in the project has
// a new image available, and if so, runs the compose-aware PullAndApply
// path that the UI's manual "Update" button already uses.
//
// Unlike the raw clone path, this reuses the project's existing compose
// YAML and goes through compose-go's Up() — which means the new
// containers are created with the full and correct set of compose
// labels (project, service, container-number, config-hash, image
// digest, config-files, working-dir, …), attached to every network the
// project defines, and started automatically. Store metadata and icons
// remain intact because they live in the compose file and not in any
// transient Docker SDK state.
func checkComposeProjectUpdate(ctx context.Context, cli *dockerclient.Client, project string, projectContainers []container.Summary) {
	// Use the project name as the op label so multi-service stacks
	// get one telemetry record instead of N (one per labeled container).
	op := StartOp("app.auto_update_check", project)

	// Check each labeled container in the project for new images.
	// We don't need EVERY service in the compose file to be opted in —
	// if ONE of them has a new image, we rerun compose.Up and let
	// compose decide what to recreate (it diffs each service's image
	// digest + config hash and only touches the ones that changed).
	hasAnyNew := false
	var checkErr error
	for _, c := range projectContainers {
		name := "unknown"
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		oldInspect, _, err := cli.ImageInspectWithRaw(ctx, c.Image)
		if err != nil {
			logger.Error("auto-update: failed to inspect current image",
				zap.String("project", project),
				zap.String("container", name),
				zap.String("image", c.Image),
				zap.Error(err),
			)
			checkErr = err
			continue
		}

		hasNew, newDigest, err := docker.HasNewImage(ctx, c.Image, oldInspect.ID)
		if err != nil {
			logger.Error("auto-update: failed to check for new image",
				zap.String("project", project),
				zap.String("container", name),
				zap.Error(err),
			)
			checkErr = err
			continue
		}

		if hasNew {
			logger.Info("auto-update: new image available for compose service",
				zap.String("project", project),
				zap.String("container", name),
				zap.String("old_digest", shortDigest(oldInspect.ID)),
				zap.String("new_digest", shortDigest(newDigest)),
			)
			hasAnyNew = true
			// Don't break — keep logging every service that needs an
			// update so the audit trail is complete even though we
			// only fire one PullAndApply below.
		}
	}

	if !hasAnyNew {
		if checkErr != nil {
			// No new images AND at least one inspect/check failed —
			// record the failure so the dashboard operation log
			// doesn't show a suspicious "completed" for a sweep that
			// actually couldn't talk to the registry.
			if op != nil {
				op.Fail(checkErr)
			}
			return
		}
		logger.Info("auto-update: no updates available for compose project",
			zap.String("project", project))
		if op != nil {
			op.Complete("no update needed")
		}
		return
	}

	// Load the full compose app from the on-disk project state —
	// Compose().List reads every stack's config file and returns the
	// same *ComposeApp the UI's update flow operates on. This is the
	// same object graph PullAndApply expects, so marshalling it and
	// feeding it right back is a well-trodden path (Install, Update,
	// Apply all do the equivalent).
	composeApps, err := MyService.Compose().List(ctx)
	if err != nil {
		logger.Error("auto-update: failed to list compose apps",
			zap.String("project", project), zap.Error(err))
		if op != nil {
			op.Fail(err)
		}
		return
	}

	composeApp, ok := composeApps[project]
	if !ok || composeApp == nil {
		// The compose list didn't return our project even though a
		// container claims membership via the label. This usually
		// means the compose file on disk has been deleted or the
		// config-hash label was stripped out of band — either way,
		// we can't safely run PullAndApply. Fall back to per-container
		// RecreateContainer so the user at least gets the new image,
		// even if the stack's compose state is already broken.
		logger.Error("auto-update: compose project not found in compose list, falling back to per-container recreate",
			zap.String("project", project))
		for _, c := range projectContainers {
			checkContainerUpdate(ctx, cli, c)
		}
		if op != nil {
			op.Fail(fmt.Errorf("compose project %q not found", project))
		}
		return
	}

	yamlBytes, err := yaml.Marshal(composeApp)
	if err != nil {
		logger.Error("auto-update: failed to marshal compose app",
			zap.String("project", project), zap.Error(err))
		if op != nil {
			op.Fail(err)
		}
		return
	}

	logger.Info("auto-update: running compose PullAndApply for project",
		zap.String("project", project),
		zap.Int("services", len(composeApp.Services)),
	)

	if err := composeApp.PullAndApply(ctx, yamlBytes); err != nil {
		// PullAndApply now holds the per-app lock for its whole run, so the
		// sweep can legitimately collide with an admin who is installing or
		// updating the same app right now. That is a deferral, not a fault:
		// recording it as a failed operation would put a red mark on the
		// dashboard for a stack nothing ever touched, and the next daily sweep
		// picks the update up anyway.
		if errors.Is(err, ErrComposeAppBusy) {
			logger.Info("auto-update: skipping project, another operation holds the app lock",
				zap.String("project", project),
			)
			if op != nil {
				op.Complete("skipped: app busy with another operation")
			}
			return
		}
		logger.Error("auto-update: compose PullAndApply failed",
			zap.String("project", project),
			zap.Error(err),
		)
		if op != nil {
			op.Fail(err)
		}
		return
	}

	logger.Info("auto-update: compose project updated successfully",
		zap.String("project", project))
	if op != nil {
		op.Complete("compose project updated to new images")
	}
}
