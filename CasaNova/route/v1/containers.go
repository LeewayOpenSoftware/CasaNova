package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// ---------- Types ----------

// ExternalContainer represents any Docker container on the host.
type ExternalContainer struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"` // leading / stripped
	Image   string            `json:"image"`
	State   string            `json:"state"`   // running, exited, paused, …
	Status  string            `json:"status"`  // "Up 3 days", "Exited (0) 2h ago"
	Created int64             `json:"created"` // unix timestamp (seconds)
	Ports   []ContainerPort   `json:"ports"`
	Labels  map[string]string `json:"labels"`
	Managed bool              `json:"managed"` // true if casaos=casaos label present
}

// ContainerPort describes a single port mapping.
type ContainerPort struct {
	PrivatePort uint16 `json:"privatePort"`
	PublicPort  uint16 `json:"publicPort"`
	Type        string `json:"type"` // tcp, udp
	IP          string `json:"ip,omitempty"`
}

// ComposeStack groups containers that share a com.docker.compose.project label.
type ComposeStack struct {
	Name         string              `json:"name"`
	Containers   []ExternalContainer `json:"containers"`
	Status       string              `json:"status"` // running, partial, stopped
	ServiceCount int                 `json:"serviceCount"`
	Managed      bool                `json:"managed"`
}

// AllContainersResponse is returned by GET /v1/container/all.
type AllContainersResponse struct {
	Stacks     []ComposeStack      `json:"stacks"`
	Standalone []ExternalContainer `json:"standalone"`
	Total      int                 `json:"total"`
}

// ContainerResourceStats holds live resource usage for a single container.
type ContainerResourceStats struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	CpuPercent  float64 `json:"cpuPercent"`
	MemoryMB    float64 `json:"memoryMB"`
	MemoryLimit int64   `json:"memoryLimit"`
	NetRxBytes  int64   `json:"netRxBytes"`
	NetTxBytes  int64   `json:"netTxBytes"`
}

// ---------- Docker CLI JSON shapes ----------

// dockerPsJSON mirrors the JSON produced by `docker ps --format '{{json .}}'`.
type dockerPsJSON struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Created string `json:"CreatedAt"` // not used directly; we get unix ts separately
	Ports   string `json:"Ports"`
	Labels  string `json:"Labels"`
}

// dockerInspectJSON is a minimal subset of `docker inspect` output.
type dockerInspectJSON struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// dockerStatsJSON mirrors `docker stats --no-stream --format '{{json .}}'`.
type dockerStatsJSON struct {
	Container string `json:"Container"`
	Name      string `json:"Name"`
	CPUPerc   string `json:"CPUPerc"`
	MemUsage  string `json:"MemUsage"`
	NetIO     string `json:"NetIO"`
	MemPerc   string `json:"MemPerc"`
}

// ---------- GET /v1/container/all ----------

// GetAllContainers discovers every Docker container on the host and groups
// them by compose project. Containers not part of a compose project are
// returned under "standalone".
func GetAllContainers(ctx echo.Context) error {
	// RBAC: this read was ungated while the mutations below were not, so a
	// tokenless caller (every LAN request arrives via the gateway over
	// loopback) could enumerate every container on the host with its name,
	// image, labels and published ports.
	//
	// The permission check alone does NOT close that: PermAppView is in
	// guestPerms, and a request with no token resolves to guest, so the
	// enumeration stayed open. Require a real session as well — the only
	// consumer is the post-login web UI (no Go service calls this route;
	// main.go lists it merely as a gateway prefix), so nothing internal
	// depends on tokenless access. UserIDFromContext is 0 unless the JWT
	// middleware validated a token.
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	timeout, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Use `docker inspect` on all containers for rich data.
	out, err := exec.CommandContext(timeout, "docker", "ps", "-a", "--no-trunc", "-q").Output()
	if err != nil {
		logger.Error("container/all: docker ps failed", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to list containers: " + err.Error(),
		})
	}

	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) == 0 {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data: AllContainersResponse{
				Stacks:     []ComposeStack{},
				Standalone: []ExternalContainer{},
				Total:      0,
			},
		})
	}

	// docker inspect all at once (much faster than one-by-one).
	args := append([]string{"inspect"}, ids...)
	inspectOut, err := exec.CommandContext(timeout, "docker", args...).Output()
	if err != nil {
		logger.Error("container/all: docker inspect failed", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to inspect containers: " + err.Error(),
		})
	}

	var inspected []dockerInspectJSON
	if err := json.Unmarshal(inspectOut, &inspected); err != nil {
		logger.Error("container/all: json parse failed", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to parse inspect output",
		})
	}

	// Also get human-readable status strings via docker ps.
	statusMap := buildStatusMap(timeout)

	// Build ExternalContainer list.
	containers := make([]ExternalContainer, 0, len(inspected))
	for _, c := range inspected {
		name := strings.TrimPrefix(c.Name, "/")
		managed := c.Config.Labels["casaos"] == "casaos"

		ports := parsePorts(c)

		created := parseCreatedTimestamp(c.Created)

		status := c.State.Status
		if s, ok := statusMap[c.ID]; ok {
			status = s
		}

		containers = append(containers, ExternalContainer{
			ID:      c.ID,
			Name:    name,
			Image:   c.Config.Image,
			State:   c.State.Status,
			Status:  status,
			Created: created,
			Ports:   ports,
			Labels:  c.Config.Labels,
			Managed: managed,
		})
	}

	// Group by compose project.
	stackMap := map[string][]ExternalContainer{}
	standalone := []ExternalContainer{}
	for _, c := range containers {
		project := c.Labels["com.docker.compose.project"]
		if project != "" {
			stackMap[project] = append(stackMap[project], c)
		} else {
			standalone = append(standalone, c)
		}
	}

	// Build stacks.
	stacks := make([]ComposeStack, 0, len(stackMap))
	for name, cs := range stackMap {
		services := map[string]struct{}{}
		anyManaged := false
		allRunning := true
		allStopped := true
		for _, c := range cs {
			svc := c.Labels["com.docker.compose.service"]
			if svc != "" {
				services[svc] = struct{}{}
			}
			if c.Managed {
				anyManaged = true
			}
			if c.State == "running" {
				allStopped = false
			} else {
				allRunning = false
			}
		}
		status := "partial"
		if allRunning {
			status = "running"
		} else if allStopped {
			status = "stopped"
		}

		stacks = append(stacks, ComposeStack{
			Name:         name,
			Containers:   cs,
			Status:       status,
			ServiceCount: len(services),
			Managed:      anyManaged,
		})
	}

	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Name < stacks[j].Name })
	sort.Slice(standalone, func(i, j int) bool { return standalone[i].Name < standalone[j].Name })

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: AllContainersResponse{
			Stacks:     stacks,
			Standalone: standalone,
			Total:      len(containers),
		},
	})
}

// ---------- GET /v1/container/stats ----------

// `docker stats --no-stream` is not a cheap read: it attaches to every running
// container and waits for a second CPU sample before printing, so it costs
// 1-2s every time. The dashboard grid, the app detail view and every forced
// refresh after an app action all hit this endpoint, so a single page could
// fire three samplers back to back and pin three handler goroutines for
// seconds. Serve the last sample instead while it is fresh; the window is well
// inside the 10s the frontend store already caches app data for (CACHE_MS in
// stores/containers.ts), so nothing on screen gets staler than it already was.
//
// The dedup has to be a real singleflight, not "hold the mutex across the
// exec". While dockerd is wedged (a `docker restart`, a heavy pull, a hung
// daemon) each sample burns its full deadline, and a mutex-guarded exec makes
// the callers serialize: the Nth waiter pays N deadlines, so the queue grows
// without bound while the UI keeps repolling every 10s from every tab — the
// browser gives up at 10s but the server goroutine and its pending fork do
// not. So: exactly one sample runs at a time, everyone else waits for THAT
// result instead of enqueuing another exec, and failures are cached briefly
// too so a wedged daemon drains the queue rather than re-forking per request.
const (
	containerStatsTTL = 3 * time.Second
	// Failures get their own short TTL: long enough that a burst of polls
	// during an outage collapses into one exec, short enough that recovery
	// shows up on the next poll.
	containerStatsFailTTL = 2 * time.Second
	// The browser aborts at 10s, so a longer deadline only buys blocked
	// goroutines nobody is waiting on any more.
	containerStatsTimeout = 10 * time.Second
)

var (
	containerStatsMu    sync.Mutex
	containerStatsAt    time.Time
	containerStatsCache []ContainerResourceStats
	containerStatsErr   error
	// containerStatsInFlight is non-nil while a sample is running and is
	// closed when it lands; late arrivals block on it instead of starting
	// their own.
	containerStatsInFlight chan struct{}
)

func cachedContainerStats() ([]ContainerResourceStats, error) {
	containerStatsMu.Lock()

	ttl := containerStatsTTL
	if containerStatsErr != nil {
		ttl = containerStatsFailTTL
	}
	if !containerStatsAt.IsZero() && time.Since(containerStatsAt) < ttl {
		stats, err := containerStatsCache, containerStatsErr
		containerStatsMu.Unlock()
		return stats, err
	}

	if done := containerStatsInFlight; done != nil {
		// Someone else is already sampling. Wait for their result — the
		// mutex is released first, so the exec never runs under it.
		containerStatsMu.Unlock()
		<-done
		containerStatsMu.Lock()
		stats, err := containerStatsCache, containerStatsErr
		containerStatsMu.Unlock()
		return stats, err
	}

	done := make(chan struct{})
	containerStatsInFlight = done
	// Release the slot from a defer registered before the sample runs, not
	// after it returns: if the sampler ever panics, echo's Recover keeps the
	// process alive but the leader never gets back here, so the channel would
	// stay published and unclosed and every later caller would block on <-done
	// forever — both stats endpoints dead until the daemon restarts, leaking a
	// goroutine per poll per tab. Unwinding through this defer instead hands
	// the waiters the previous cached value (containerStatsAt is left stale, so
	// the next caller re-samples) and the panic still reaches Recover.
	defer func() {
		containerStatsMu.Lock()
		containerStatsInFlight = nil
		containerStatsMu.Unlock()
		// Wake the waiters only after dropping the mutex — they grab it the
		// moment they wake to read the cache.
		close(done)
	}()
	containerStatsMu.Unlock()

	stats, err := sampleContainerStats()

	containerStatsMu.Lock()
	containerStatsCache, containerStatsErr, containerStatsAt = stats, err, time.Now()
	containerStatsMu.Unlock()

	return stats, err
}

// GetContainerResourceStats returns CPU/memory/network stats for all running
// containers. It shells out to `docker stats --no-stream` which is the
// equivalent of the one-shot stats API call.
func GetContainerResourceStats(ctx echo.Context) error {
	// RBAC: this read was ungated while the mutations below were not, so a
	// tokenless caller (every LAN request arrives via the gateway over
	// loopback) could poll live CPU/memory/network for every container.
	// Same reasoning as GetAllContainers above: guest holds PermAppView, so
	// the permission check by itself leaves tokenless polling open. Require
	// a real session too; the UI reads this after login.
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	results, err := cachedContainerStats()
	if err != nil {
		logger.Error("container/stats: docker stats failed", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to get container stats: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    results,
	})
}

// sampleContainerStats runs the actual sampler. Only cachedContainerStats
// calls it, and only as the singleflight leader — never under
// containerStatsMu, so a slow docker cannot block the cache readers.
func sampleContainerStats() ([]ContainerResourceStats, error) {
	timeout, cancel := context.WithTimeout(context.Background(), containerStatsTimeout)
	defer cancel()

	out, err := exec.CommandContext(timeout,
		"docker", "stats", "--no-stream", "--format", "{{json .}}",
	).Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	results := make([]ContainerResourceStats, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s dockerStatsJSON
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		results = append(results, ContainerResourceStats{
			ID:          s.Container,
			Name:        strings.TrimPrefix(s.Name, "/"),
			CpuPercent:  parsePercent(s.CPUPerc),
			MemoryMB:    parseMemoryMB(s.MemUsage),
			MemoryLimit: parseMemoryLimitBytes(s.MemUsage),
			NetRxBytes:  parseNetBytes(s.NetIO, 0),
			NetTxBytes:  parseNetBytes(s.NetIO, 1),
		})
	}

	return results, nil
}

// Per-container start / stop / restart used to live here. They were
// unreachable: the gateway hands every "/v1/container/:id/..." request to
// app-management, which owns the generic "/v1/container" prefix and has its
// own equivalents. Keeping a second copy here only invited someone to fix or
// harden a handler that receives no traffic. See CasaNova-Common/pkg/gatewayroutes.

// ---------- Helpers ----------

// buildStatusMap runs `docker ps -a --format` and returns a map of
// container-ID → human-readable status string ("Up 3 days", etc.).
func buildStatusMap(ctx context.Context) map[string]string {
	out, err := exec.CommandContext(ctx,
		"docker", "ps", "-a", "--no-trunc", "--format", "{{json .}}",
	).Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ps dockerPsJSON
		if err := json.Unmarshal([]byte(line), &ps); err != nil {
			continue
		}
		m[ps.ID] = ps.Status
	}
	return m
}

// parsePorts extracts port mappings from docker inspect output.
func parsePorts(c dockerInspectJSON) []ContainerPort {
	var ports []ContainerPort
	for raw, bindings := range c.NetworkSettings.Ports {
		parts := strings.SplitN(raw, "/", 2)
		proto := "tcp"
		if len(parts) == 2 {
			proto = parts[1]
		}
		var privatePort uint16
		fmt.Sscanf(parts[0], "%d", &privatePort)

		if len(bindings) == 0 {
			ports = append(ports, ContainerPort{
				PrivatePort: privatePort,
				Type:        proto,
			})
			continue
		}
		for _, b := range bindings {
			var pubPort uint16
			fmt.Sscanf(b.HostPort, "%d", &pubPort)
			ports = append(ports, ContainerPort{
				PrivatePort: privatePort,
				PublicPort:  pubPort,
				Type:        proto,
				IP:          b.HostIP,
			})
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].PrivatePort < ports[j].PrivatePort })
	return ports
}

// parseCreatedTimestamp parses Docker's ISO 8601 timestamp into unix seconds.
func parseCreatedTimestamp(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// parsePercent turns "12.34%" into 12.34.
func parsePercent(s string) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

// parseMemoryMB extracts the used memory in MB from "123.4MiB / 7.5GiB".
func parseMemoryMB(s string) float64 {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 0 {
		return 0
	}
	return parseDataSize(strings.TrimSpace(parts[0]))
}

// parseMemoryLimitBytes extracts the memory limit in bytes from "123.4MiB / 7.5GiB".
func parseMemoryLimitBytes(s string) int64 {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) < 2 {
		return 0
	}
	mb := parseDataSize(strings.TrimSpace(parts[1]))
	return int64(mb * 1024 * 1024)
}

// parseDataSize converts "123.4MiB", "7.5GiB", "500KiB" to MB (float64).
func parseDataSize(s string) float64 {
	s = strings.TrimSpace(s)
	var val float64
	var unit string
	fmt.Sscanf(s, "%f%s", &val, &unit)
	unit = strings.ToLower(unit)
	switch {
	case strings.HasPrefix(unit, "gib"), strings.HasPrefix(unit, "gb"):
		return val * 1024
	case strings.HasPrefix(unit, "mib"), strings.HasPrefix(unit, "mb"):
		return val
	case strings.HasPrefix(unit, "kib"), strings.HasPrefix(unit, "kb"):
		return val / 1024
	case strings.HasPrefix(unit, "b"):
		return val / (1024 * 1024)
	}
	return val
}

// parseNetBytes extracts rx (idx=0) or tx (idx=1) byte count from "1.23kB / 4.56MB".
func parseNetBytes(s string, idx int) int64 {
	parts := strings.SplitN(s, "/", 2)
	if idx >= len(parts) {
		return 0
	}
	mb := parseDataSize(strings.TrimSpace(parts[idx]))
	return int64(mb * 1024 * 1024)
}

// ---------- Concurrent stats (semaphore pattern) ----------
// Kept here for future use if we switch to the Docker SDK for per-container
// stats. Currently docker stats --no-stream handles all containers at once.

var _ = runConcurrent // suppress unused warning during build

// runConcurrent runs fn for each item with at most maxWorkers goroutines.
func runConcurrent(items []string, maxWorkers int, fn func(string)) {
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(id)
		}(item)
	}
	wg.Wait()
}
