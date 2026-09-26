package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/model"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/utils/envHelper"
	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/service/v1"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/random"
	timeutils "github.com/LeewayOpenSoftware/CasaNova-Common/utils/time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/go-connections/nat"
)

var (
	dataStats atomic.Pointer[sync.Map]
	isFinish  atomic.Bool

	// The stats collector is demand-driven: the first GetContainerStats
	// starts it and it shuts itself down once nobody has asked for stats
	// in statsIdleTimeout. It used to be launched with context.Background()
	// and never stopped, so a single /container/usage request left a 1 Hz
	// stats poll running against dockerd for every running container (27 of
	// them on a real box) long after that request was answered. statsMu
	// serialises start against the collector's own teardown so a request
	// racing the idle check either renews the lease or starts a fresh
	// collector — it can never end up with no collector at all.
	statsMu              sync.Mutex
	statsRunning         bool
	statsLastRequest     time.Time
	statsCollectorCancel context.CancelFunc
)

// A collector nobody reads is pure load on dockerd, but tearing it down the
// moment a request is answered would make every next request pay a cold
// start. A minute comfortably outlives the UI's polling cadence while
// bounding the idle cost to that same minute.
const statsIdleTimeout = time.Minute

func init() {
	dataStats.Store(&sync.Map{})
}

// stopStatsCollector releases the collector slot and cancels its context, so
// the per-container goroutines still in flight give up instead of holding
// their stats streams open for the rest of their timeout. The last sample is
// deliberately left in dataStats and isFinish untouched: the request that
// restarts the collector then answers as fast as it does today (with numbers
// up to statsIdleTimeout old, replaced within a second) instead of blocking
// on the cold-start wait below.
func stopStatsCollector() {
	statsMu.Lock()
	defer statsMu.Unlock()

	stopStatsCollectorLocked()
}

func stopStatsCollectorLocked() {
	statsRunning = false
	if statsCollectorCancel != nil {
		statsCollectorCancel()
		statsCollectorCancel = nil
	}
}

// statsCollectorExpired reports whether the collector has outlived its
// consumers, tearing it down in the same critical section as the check so
// that a GetContainerStats arriving concurrently either renews the lease
// (and the collector keeps running) or finds statsRunning already false and
// starts a replacement.
func statsCollectorExpired() bool {
	statsMu.Lock()
	defer statsMu.Unlock()

	if time.Since(statsLastRequest) < statsIdleTimeout {
		return false
	}
	stopStatsCollectorLocked()
	return true
}

type DockerService interface {
	// image
	IsExistImage(imageName string) bool
	PullImage(ctx context.Context, imageName string) error
	PullLatestImage(ctx context.Context, imageName string) (bool, error)
	RemoveImage(name string) error

	// container
	CheckContainerHealth(id string) (bool, error)
	CreateContainer(ctx context.Context, m model.CustomizationPostData, id string) (containerID string, err error)
	CreateContainerShellSession(containerID, row, col string) (hr types.HijackedResponse, err error)
	DescribeContainer(ctx context.Context, name string) (*container.InspectResponse, error)
	GetContainer(id string) (container.Summary, error)
	GetContainerAppList(name, image, state *string) (*[]model.MyAppList, *[]model.MyAppList)
	GetContainerByName(name string) (*container.Summary, error)
	GetContainerLog(name string) ([]byte, error)
	GetContainerStats() []model.DockerStatsModel
	RecreateContainer(ctx context.Context, id string, pull bool, force bool) error
	RemoveContainer(name string, update bool) error
	RenameContainer(name, id string) (err error)
	StartContainer(name string) error
	StopContainer(id string) error

	// network
	GetNetworkList() []network.Inspect

	// docker server
	GetServerInfo() (system.Info, error)
}

type dockerService struct{}

// cpuCounters pulls the two cumulative CPU counters the dashboard derives its
// percentage from out of a raw docker stats document. They are read from the
// decoded payload rather than a typed struct because the collector
// deliberately forwards that document to the client untouched.
func cpuCounters(sample interface{}) (total, system float64, ok bool) {
	root, ok := sample.(map[string]interface{})
	if !ok {
		return 0, 0, false
	}

	cpuStats, ok := root["cpu_stats"].(map[string]interface{})
	if !ok {
		return 0, 0, false
	}

	usage, ok := cpuStats["cpu_usage"].(map[string]interface{})
	if !ok {
		return 0, 0, false
	}

	total, ok = usage["total_usage"].(float64)
	if !ok {
		return 0, 0, false
	}

	system, ok = cpuStats["system_cpu_usage"].(float64)
	if !ok {
		return 0, 0, false
	}

	return total, system, true
}

// pairableWithPrevious reports whether `data` can be paired with `previous` to
// derive a CPU percentage. Both counters are cumulative over the life of the
// container's cgroup, and `docker restart` recreates that cgroup while keeping
// the container ID, so a pair straddling a restart makes them run backwards and
// the client renders a negative or absurd percentage. When either sample can't
// be read we keep the pair: that is the long-standing behaviour, and a payload
// we don't recognise is not evidence of a reset.
func pairableWithPrevious(previous, data interface{}) bool {
	prevTotal, prevSystem, ok := cpuCounters(previous)
	if !ok {
		return true
	}

	total, system, ok := cpuCounters(data)
	if !ok {
		return true
	}

	return total >= prevTotal && system >= prevSystem
}

// FIXME - should use WebSocket or SocketIO instead of HTTP polling (tiger)
func getContainerStats(ctx context.Context) {
	// The collector holds the statsRunning slot until it returns, so a death
	// it did not plan for (client error, panic in the loop) has to release it
	// — otherwise statsRunning stays true and stats never come back. The idle
	// exit below releases the slot itself while holding statsMu and marks
	// itself here, so this cleanup can't tear down a replacement collector
	// that a racing request already started in its place.
	stopped := false
	defer func() {
		if !stopped {
			stopStatsCollector()
		}
	}()

	cli, err := docker.SharedClient()
	if err != nil {
		return
	}

	// Wave 35-X: bound the initial ContainerList. The outer ctx carries no
	// deadline (it is only cancelled when the collector is torn down), so
	// without an inner one a wedged daemon freezes the entire stats loop
	// forever — dashboard stats never refresh and isFinish stays false.
	listCtx, listCancel := context.WithTimeout(ctx, 5*time.Second)
	containers, err := cli.ContainerList(listCtx, container.ListOptions{All: true})
	listCancel()
	if err != nil {
		logger.Error("Failed to get container_list", zap.Any("err", err))
	}
	i := 0
	// The collector is demand-driven and shuts itself down after
	// statsIdleTimeout, but stopStatsCollector deliberately leaves the last
	// sample in dataStats so the request that restarts it can be answered
	// immediately. That leftover is up to a minute old and belongs to the
	// previous collector generation, so pairing the first fresh sample with it
	// describes a minute-long window as if it were the usual one second — and
	// if the container was restarted meanwhile, the reused ID hides a recreated
	// cgroup whose counters are back at zero, which reads as a negative delta.
	// Publish the first sample of a generation without a Previous; the next
	// tick, a second later, has a same-generation pair and the graph resumes.
	coldStart := true
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Nobody has asked for stats in a while: stop polling dockerd once
		// per second for a request that ended minutes ago. The next
		// GetContainerStats starts a fresh collector.
		if statsCollectorExpired() {
			stopped = true
			return
		}

		if i%10 == 0 {
			listCtx, listCancel := context.WithTimeout(ctx, 5*time.Second)
			containers, err = cli.ContainerList(listCtx, container.ListOptions{All: true})
			listCancel()
			if err != nil {
				logger.Error("Failed to get container_list", zap.Any("err", err))
				i++
				time.Sleep(time.Second * 1)
				continue
			}
		}
		if config.CasaOSGlobalVariables.AppChange.CompareAndSwap(true, false) {
			dataStats.Store(&sync.Map{})
		}

		current := dataStats.Load()
		// Read once per iteration: the fan-out below reads it concurrently and
		// coldStart is only written after wg.Wait().
		usePrevious := !coldStart
		var temp sync.Map
		var wg sync.WaitGroup
		for _, v := range containers {
			if v.State != "running" {
				continue
			}
			wg.Add(1)
			go func(v container.Summary) {
				defer wg.Done()
				// Wave 31: recover() so a panic inside json.Decode
				// (mangled stats payload from a misbehaving container
				// runtime) does not propagate and crash the entire
				// AppManagement daemon. wg.Done() is already deferred,
				// so the parent loop unblocks even on panic.
				defer func() {
					if r := recover(); r != nil {
						logger.Error("container stats goroutine panicked",
							zap.Any("panic", r), zap.String("container", v.ID))
					}
				}()
				select {
				case <-ctx.Done():
					return
				default:
				}
				// Wave 35-X: bound per-container stats call. A single
				// hung container's stats stream would block wg.Wait()
				// for the whole iteration, freezing dashboard updates.
				statsCtx, statsCancel := context.WithTimeout(ctx, 3*time.Second)
				defer statsCancel()
				stats, err := cli.ContainerStatsOneShot(statsCtx, v.ID)
				if err != nil {
					logger.Error("failed to get container stats", zap.Error(err), zap.String("container", v.ID))
					return
				}
				defer stats.Body.Close()

				decoder := json.NewDecoder(stats.Body)

				// data
				var data interface{}
				if err := decoder.Decode(&data); err != nil {
					logger.Error("failed to decode container stats", zap.Error(err), zap.String("container", v.ID))
					return // handles io.EOF and any other decode errors
				}
				m, _ := current.Load(v.ID)
				dockerStats := model.DockerStatsModel{}
				if usePrevious && m != nil {
					if prev, ok := m.(model.DockerStatsModel); ok && pairableWithPrevious(prev.Data, data) {
						dockerStats.Previous = prev.Data
					}
				}

				// icon
				if icon, ok := v.Labels[v1.V1LabelIcon]; ok {
					dockerStats.Icon = icon
				}

				dockerStats.Data = data
				dockerStats.Title = containerName(v.Names)

				temp.Store(v.ID, dockerStats)
			}(v)
		}
		wg.Wait()
		dataStats.Store(&temp)
		isFinish.Store(true)
		coldStart = false

		i++
		// 3s, matched to the UI's own polling cadence. At 1s this loop held
		// dockerd at ~27 ContainerStatsOneShot calls per second (one per
		// running container) to feed a dashboard that reads the result every
		// ~3s — two thirds of the samples were computed and thrown away. The
		// CPU% pairing just works over the wider window.
		time.Sleep(3 * time.Second)
	}
}

func (ds *dockerService) GetContainerStats() []model.DockerStatsModel {
	// Renew the collector's lease and start it if it isn't running (or has
	// already expired), holding statsMu so this can't cross with the
	// collector's own idle teardown and leave nobody collecting.
	statsMu.Lock()
	statsLastRequest = time.Now()
	if !statsRunning {
		statsRunning = true
		var collectorCtx context.Context
		collectorCtx, statsCollectorCancel = context.WithCancel(context.Background())
		go getContainerStats(collectorCtx)
	}
	statsMu.Unlock()

	// If stats aren't ready yet, wait up to 5 seconds.
	for i := 0; i < 50 && !isFinish.Load(); i++ {
		time.Sleep(100 * time.Millisecond)
	}

	list := []model.DockerStatsModel{}
	dataStats.Load().Range(func(key, value interface{}) bool {
		if ds, ok := value.(model.DockerStatsModel); ok {
			list = append(list, ds)
		}
		return true
	})
	return list
}

// healthProbeConcurrency bounds how many container health probes may be in
// flight at once, across every caller. Backported from 27.04; the number is a
// burst budget, not a capacity plan.
//
// It is deliberately above the legitimate burst. A dashboard refresh probes
// every app with a `web` label at once; a box with more apps than this queues
// the overflow for up to healthProbeWait, which a healthy loopback GET --
// milliseconds -- absorbs without a single rejection. The only way to hold a
// slot for long is for the probed app to be unresponsive, which is exactly the
// state in which the box should stop opening more sockets.
//
// THIS LINE HOLDS SLOTS 6x LONGER THAN 27.04: the per-probe budget below is
// still 30s here, not 5s, so the worst-case ceiling is 16*30 = 480
// probe-seconds against 27.04's 16*5 = 80. Constant either way, which is the
// property that matters, but the shorter budget is worth porting on its own
// merits -- deliberately NOT done here: this card is the cap, and shortening a
// timeout on the maintenance line changes observable behaviour for apps that
// legitimately answer slowly.
const healthProbeConcurrency = 16

// healthProbeWait is how long a caller queues for a slot before being told the
// box is busy. A var, not a const, so tests can shorten it; nothing in
// production writes it.
//
// It stays 2s even though slots turn over more slowly here. Lengthening it to
// compensate would pin the caller's goroutine for longer, which is the very
// thing this cap exists to bound.
var healthProbeWait = 2 * time.Second

// healthProbeSem is the cap itself, in the same buffered-channel idiom as
// eventSemaphore in service.go.
var healthProbeSem = make(chan struct{}, healthProbeConcurrency)

// errHealthProbeBusy is returned instead of a probe result when the cap is
// saturated. The handler maps every error to 503, which is the honest code:
// the health of the container is unknown, not known-bad.
var errHealthProbeBusy = errors.New("too many container health checks in flight, try again")

// acquireHealthProbe takes a slot in the health-probe cap, waiting at most
// healthProbeWait. It returns the release function on success.
//
// Waiting BOUNDED and then refusing is the whole point. Waiting forever would
// only move the amplification from sockets to the queue: the caller's goroutine
// would still be pinned, which is the occupancy lever this closes.
func acquireHealthProbe() (func(), error) {
	timer := time.NewTimer(healthProbeWait)
	defer timer.Stop()

	select {
	case healthProbeSem <- struct{}{}:
		return func() { <-healthProbeSem }, nil
	case <-timer.C:
		return nil, errHealthProbeBusy
	}
}

func (ds *dockerService) CheckContainerHealth(id string) (bool, error) {
	// Wave 34 gated this endpoint behind PermAppView, which guest already
	// holds, so the probe is still reachable by the least privileged role on
	// the box. Authorization said WHO may ask; nothing said HOW MANY may be
	// asked at once, and each one pins a goroutine, a socket and a call to the
	// shared docker client for up to the probe budget below.
	//
	// The cap is taken BEFORE the docker call, not just around the HTTP probe:
	// both halves are work the caller can ask for an unbounded number of
	// times, and capping only the second half would leave dockerd exposed to
	// the same lever.
	//
	// What this does NOT bound: the goroutine echo already allocated for the
	// request. That belongs to the HTTP server, not to this function; what is
	// bounded here is how long such a goroutine can stay expensive (<= wait +
	// the probe budget) and how many can be expensive at once.
	release, err := acquireHealthProbe()
	if err != nil {
		logger.Error("refusing container health check, probe cap saturated",
			zap.String("id", id), zap.Int("cap", healthProbeConcurrency))
		return false, err
	}
	defer release()

	container, err := ds.GetContainer(id)
	if err != nil {
		logger.Error("failed to get container by id", zap.Error(err), zap.String("id", id))
		return false, err
	}

	if webUIPort, ok := container.Labels["web"]; ok {
		index := container.Labels["index"]
		url := fmt.Sprintf("http://%s:%s/%s", common.Localhost, webUIPort, strings.TrimLeft(index, "/"))

		logger.Info("checking container health at the specified web port...", zap.Any("name", container.Names), zap.String("id", id), zap.Any("url", url))
		client := resty.New()
		// 5s, aligned with 27.04 (0f505f4). Was 30s: with the 16-slot cap
		// above that put the worst-case probe ceiling at 480s here vs 80s on
		// 27.04. This changes observable behavior: an app that legitimately
		// takes 6-30s to answer now flips to unhealthy instead of passing.
		client.SetTimeout(5 * time.Second)
		client.SetHeader("Accept", "text/html")
		response, err := client.R().Get(url)
		if err != nil {
			logger.Error("failed to check container health", zap.Error(err), zap.Any("name", container.Names), zap.String("id", id))
			return false, err
		}
		if response.StatusCode() == http.StatusOK || response.StatusCode() == http.StatusUnauthorized {
			return true, nil
		}
		// response, err := httpUtil.GetWithHeader(url, 30*time.Second, map[string]string{
		// 	echo.HeaderAccept: echo.MIMETextHTML, // emulate a browser
		// })
		// if err != nil {
		// 	logger.Error("failed to check container health", zap.Error(err), zap.Any("name", container.Names), zap.String("id", id))
		// 	return false, err
		// }

		// if (response.StatusCode == http.StatusUnauthorized) || // we treat Unauthorized as a success because it means the container is up and running
		// 	(response.StatusCode >= 200 && response.StatusCode < 300) {
		// 	logger.Info("container health check passed at the specified web port", zap.Any("name", container.Names), zap.String("id", id), zap.Any("url", url))
		// 	return true, nil
		// }

		logger.Error("container health check failed at the specified web port", zap.Any("name", container.Names), zap.String("id", id), zap.Any("url", url), zap.String("status", fmt.Sprint(response.StatusCode())))
		return false, errors.New(fmt.Sprint(response.StatusCode()))
	}

	logger.Error("container health check failed, no web port specified", zap.Any("name", container.Names), zap.String("id", id))
	return false, errors.New("no web port")
}

// Get app list
func (ds *dockerService) GetContainer(id string) (container.Summary, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		logger.Error("Failed to init client", zap.Any("err", err))
		return container.Summary{}, err
	}

	f := filters.NewArgs()
	f.Add("id", id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		logger.Error("Failed to get container_list", zap.Any("err", err))
		return container.Summary{}, err
	}

	if len(containers) > 0 {
		return containers[0], nil
	}
	return container.Summary{}, fmt.Errorf("container %q not found", id)
}

// Get app list
func (ds *dockerService) GetContainerAppList(name, image, state *string) (*[]model.MyAppList, *[]model.MyAppList) {
	cli, err := docker.SharedClient()
	if err != nil {
		logger.Error("Failed to init client", zap.Any("err", err))
		return nil, nil
	}
	// fts := filters.NewArgs()
	// fts.Add("label", "casaos=casaos")
	// fts.Add("label", "casaos")
	// fts.Add("casaos", "casaos")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		logger.Error("Failed to get container_list", zap.Any("err", err))
	}

	localApps := []model.MyAppList{}

	casaOSApps := []model.MyAppList{}

	for i, m := range containers {

		if name != nil && len(*name) > 0 {
			if !lo.ContainsBy(m.Names, func(n string) bool { return strings.Contains(n, *name) }) {
				continue
			}
		}

		if image != nil && len(*image) > 0 {
			if !strings.HasPrefix(m.Image, *image) {
				continue
			}
		}

		if state != nil && len(*state) > 0 {
			if m.State != *state {
				continue
			}
		}

		if m.Labels["casaos"] == "casaos" {

			// v0.1: dropped the unused NewVersionApp map and the
			// dead `newVersion` lookup. v1 list responses always
			// reported `Latest: false` because the map was never
			// populated; the real update-availability check lives
			// in AppStoreManagement.IsUpdateAvailable now.
			name := containerName(m.Names)
			icon := m.Labels["icon"]
			if len(m.Labels["name"]) > 0 {
				name = m.Labels["name"]
			}
			if m.Labels["origin"] == "system" {
				name = strings.Split(m.Image, ":")[0]
				if len(strings.Split(name, "/")) > 1 {
					icon = "https://icon.casaos.io/main/all/" + strings.Split(name, "/")[1] + ".png"
				}
			}

			casaOSApp := model.MyAppList{
				Name:       name,
				Icon:       icon,
				State:      m.State,
				CustomID:   m.Labels["custom_id"],
				ID:         m.ID,
				Port:       m.Labels["web"],
				Index:      m.Labels["index"],
				Image:      m.Image,
				Latest:     false,
				Host:       m.Labels["host"],
				Protocol:   m.Labels["protocol"],
				Created:    m.Created,
				AppStoreID: getV1AppStoreID(&containers[i]),
			}

			casaOSApps = append(casaOSApps, casaOSApp)
		} else {
			localApp := model.MyAppList{
				Name:     containerName(m.Names),
				Icon:     "",
				State:    m.State,
				CustomID: m.ID,
				ID:       m.ID,
				Port:     "",
				Latest:   false,
				Host:     "",
				Protocol: "",
				Image:    m.Image,
				Created:  m.Created,
			}

			localApps = append(localApps, localApp)
		}
	}

	return &casaOSApps, &localApps
}

func (ds *dockerService) CreateContainerShellSession(containerID, row, col string) (types.HijackedResponse, error) {
	// Use shared client instead of creating (and leaking) a new one per session.
	cli, err := docker.SharedClient()
	if err != nil {
		return types.HijackedResponse{}, err
	}

	ctx := context.Background()
	ir, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          []string{"COLUMNS=" + col, "LINES=" + row},
		Cmd:          []string{"/bin/sh"},
		Tty:          true,
	})
	if err != nil {
		return types.HijackedResponse{}, err
	}

	return cli.ContainerExecAttach(ctx, ir.ID, container.ExecStartOptions{Detach: false, Tty: true})
}

// Main content

// param imageName
// param containerDbId id
// param port
// param mapPort
// param tcp tcp
// param udp udp
func (ds *dockerService) CreateContainer(ctx context.Context, m model.CustomizationPostData, id string) (containerID string, err error) {
	// Validate network mode
	validNetworkModes := []string{"bridge", "host", "none"}
	if len(m.NetworkModel) == 0 {
		m.NetworkModel = "bridge"
	} else if !lo.Contains(validNetworkModes, m.NetworkModel) && !strings.HasPrefix(m.NetworkModel, "container:") {
		return "", fmt.Errorf("invalid network mode: %s (valid: bridge, host, none, container:<name>)", m.NetworkModel)
	}

	// Validate restart policy
	if len(m.Restart) > 0 {
		validRestartPolicies := []string{"no", "always", "unless-stopped", "on-failure"}
		if !lo.Contains(validRestartPolicies, m.Restart) {
			return "", fmt.Errorf("invalid restart policy: %s", m.Restart)
		}
	}

	cli, err := docker.SharedClient()
	if err != nil {
		return "", err
	}

	ports := make(nat.PortSet)
	portMaps := make(nat.PortMap)

	for _, portMap := range m.Ports {
		protocol := strings.ToLower(portMap.Protocol)

		if !lo.Contains([]string{"tcp", "udp", "both"}, protocol) {
			message := "unknown protocol"
			logger.Error(message, zap.String("protocol", protocol))
			return "", errors.New(message)
		}

		// Validate port ranges (1-65535)
		protocols := strings.Replace(protocol, "both", "tcp,udp", -1)
		for _, p := range strings.Split(protocols, ",") {
			tContainer, err := strconv.Atoi(portMap.ContainerPort)
			if err != nil || tContainer < 1 || tContainer > 65535 {
				return "", fmt.Errorf("invalid container port: %s (must be 1-65535)", portMap.ContainerPort)
			}
			if len(portMap.CommendPort) > 0 {
				tHost, err := strconv.Atoi(portMap.CommendPort)
				if err != nil || tHost < 1 || tHost > 65535 {
					return "", fmt.Errorf("invalid host port: %s (must be 1-65535)", portMap.CommendPort)
				}
			}
			ports[nat.Port(portMap.ContainerPort+"/"+p)] = struct{}{}
			if m.NetworkModel != "host" {
				portMaps[nat.Port(portMap.ContainerPort+"/"+p)] = []nat.PortBinding{{HostPort: portMap.CommendPort}}
			}
		}
	}

	var envArr []string

	showENV := []string{"casaos"}

	for _, e := range m.Envs {
		// Validate env var names: reject empty, containing '=' or newlines (injection prevention)
		if len(e.Name) == 0 || strings.ContainsAny(e.Name, "=\n\r") {
			logger.Error("skipping invalid env var name", zap.String("name", e.Name))
			continue
		}
		showENV = append(showENV, e.Name)
		if strings.HasPrefix(e.Value, "$") {
			systemTimeZoneName := timeutils.GetSystemTimeZoneName()
			envArr = append(envArr, e.Name+"="+envHelper.ReplaceDefaultENV(e.Value, systemTimeZoneName))
			continue
		}
		if len(e.Value) > 0 {
			if e.Value == "port_map" {
				envArr = append(envArr, e.Name+"="+m.PortMap)
				continue
			}
			envArr = append(envArr, e.Name+"="+e.Value)
		}
	}

	res := container.Resources{}
	// Validate CPU shares (Docker valid range: 2-262144)
	if m.CPUShares > 0 {
		if m.CPUShares < 2 {
			m.CPUShares = 2
		} else if m.CPUShares > 262144 {
			m.CPUShares = 262144
		}
		res.CPUShares = m.CPUShares
	}
	// Validate memory (minimum 4MB, maximum 1TB in MB)
	if m.Memory > 0 {
		if m.Memory < 4 {
			m.Memory = 4
		} else if m.Memory > 1048576 { // 1TB in MB
			m.Memory = 1048576
		}
		res.Memory = m.Memory << 20
	}
	for _, p := range m.Devices {
		if len(p.Path) > 0 {
			// Validate device path: must be under /dev/ and must be an actual device.
			cleanDevPath := filepath.Clean(p.Path)
			if !strings.HasPrefix(cleanDevPath, "/dev/") {
				logger.Error("rejected device mapping: not under /dev/", zap.String("path", p.Path))
				continue
			}
			if strings.Contains(cleanDevPath, "..") {
				logger.Error("rejected device mapping: path traversal", zap.String("path", p.Path))
				continue
			}
			// Block dangerous devices that enable host escape.
			dangerousDevices := []string{"/dev/mem", "/dev/kmem", "/dev/port"}
			isDangerous := false
			for _, d := range dangerousDevices {
				if cleanDevPath == d {
					isDangerous = true
					break
				}
			}
			if isDangerous {
				logger.Error("rejected dangerous device mapping", zap.String("path", cleanDevPath))
				continue
			}
			res.Devices = append(res.Devices, container.DeviceMapping{PathOnHost: cleanDevPath, PathInContainer: p.ContainerPath, CgroupPermissions: "r"})
		}
	}
	hostConfingBind := []string{}
	// volumes bind
	volumes := []mount.Mount{}
	for _, v := range m.Volumes {
		path := v.Path
		if len(path) == 0 {
			path = docker.GetDir(m.Label, v.Path)
			if len(path) == 0 {
				continue
			}
		}
		// Sanitize label to prevent path traversal via $AppID substitution
		safeLabel := strings.ReplaceAll(m.Label, "..", "")
		safeLabel = strings.ReplaceAll(safeLabel, "/", "_")
		path = strings.ReplaceAll(path, "$AppID", safeLabel)

		if err := file.IsNotExistMkDir(path); err != nil {
			logger.Error("Failed to create volume directory", zap.String("path", path), zap.Error(err))
			return "", fmt.Errorf("failed to create volume directory %s: %w", path, err)
		}

		// Validate container-side mount target — block dangerous mount points
		// that could allow container escape or override critical paths.
		cleanTarget := filepath.Clean(v.ContainerPath)
		dangerousMounts := []string{"/proc", "/sys", "/dev", "/"}
		skipMount := false
		for _, dm := range dangerousMounts {
			if cleanTarget == dm {
				logger.Error("rejected dangerous container mount target", zap.String("target", v.ContainerPath))
				skipMount = true
				break
			}
		}
		if skipMount || strings.Contains(cleanTarget, "..") {
			continue
		}

		volumes = append(volumes, mount.Mount{
			Type:   mount.TypeBind,
			Source: path,
			Target: cleanTarget,
		})

		hostConfingBind = append(hostConfingBind, path+":"+cleanTarget)
	}

	rp := container.RestartPolicy{}

	if len(m.Restart) > 0 {
		rp.Name = container.RestartPolicyMode(m.Restart)
	}
	// healthTest := []string{}
	// if len(port) > 0 {
	// 	healthTest = []string{"CMD-SHELL", "curl -f http://localhost:" + port + m.Index + " || exit 1"}
	// }

	// health := &container.HealthConfig{
	// 	Test:        healthTest,
	// 	StartPeriod: 0,
	// 	Retries:     1000,
	// }
	// fmt.Print(health)
	if len(m.HostName) == 0 {
		m.HostName = m.Label
	}

	info, err := cli.ContainerInspect(ctx, id)
	hostConfig := &container.HostConfig{}
	config := &container.Config{}
	config.Labels = map[string]string{}
	if err == nil {
		// info.HostConfig = &container.HostConfig{}
		// info.Config = &container.Config{}
		// info.NetworkSettings = &types.NetworkSettings{}
		hostConfig = info.HostConfig
		config = info.Config
		if config.Labels["casaos"] == "casaos" {
			config.Cmd = m.Cmd
			config.Image = m.Image
			config.Env = envArr
			config.Hostname = m.HostName
			config.ExposedPorts = ports
		}
	} else {
		config.Cmd = m.Cmd
		config.Image = m.Image
		config.Env = envArr
		config.Hostname = m.HostName
		config.ExposedPorts = ports
	}

	config.Labels["origin"] = m.Origin
	config.Labels["casaos"] = "casaos"
	config.Labels["web"] = m.PortMap
	config.Labels["icon"] = m.Icon
	config.Labels["desc"] = m.Description
	config.Labels["index"] = m.Index
	config.Labels["custom_id"] = m.CustomID
	config.Labels["show_env"] = strings.Join(showENV, ",")
	config.Labels["protocol"] = m.Protocol
	config.Labels["host"] = m.Host
	config.Labels["name"] = m.Label
	config.Labels[common.ContainerLabelV1AppStoreID] = strconv.Itoa((int)(m.AppStoreID))
	// container, err := cli.ContainerCreate(context.Background(), info.Config, info.HostConfig, &network.NetworkingConfig{info.NetworkSettings.Networks}, nil, info.Name)

	hostConfig.Mounts = volumes
	hostConfig.Binds = []string{}

	// SECURITY: Never allow privileged containers — they have full host access.
	if m.Privileged {
		logger.Error("rejected privileged container request", zap.String("image", m.Image))
		return "", fmt.Errorf("privileged containers are not allowed")
	}
	hostConfig.Privileged = false

	// Filter capabilities against an allowlist. SYS_ADMIN and ALL enable
	// container escapes and are never permitted.
	allowedCaps := map[string]bool{
		"NET_BIND_SERVICE": true, "CHOWN": true, "DAC_OVERRIDE": true,
		"FOWNER": true, "SETGID": true, "SETUID": true, "NET_RAW": true,
		"MKNOD": true, "AUDIT_WRITE": true, "SETFCAP": true,
		"NET_ADMIN": true, "SYS_NICE": true, "IPC_LOCK": true,
	}
	var safeCaps []string
	for _, cap := range m.CapAdd {
		capUpper := strings.ToUpper(strings.TrimSpace(cap))
		if capUpper == "ALL" || capUpper == "SYS_ADMIN" || capUpper == "SYS_PTRACE" {
			logger.Error("rejected dangerous capability", zap.String("cap", capUpper), zap.String("image", m.Image))
			continue
		}
		if allowedCaps[capUpper] {
			safeCaps = append(safeCaps, capUpper)
		} else {
			logger.Error("rejected unknown capability", zap.String("cap", capUpper))
		}
	}
	hostConfig.CapAdd = safeCaps
	hostConfig.NetworkMode = container.NetworkMode(m.NetworkModel)
	hostConfig.RestartPolicy = rp
	hostConfig.Resources = res
	// hostConfig := &container.HostConfig{Resources: res, Mounts: volumes, RestartPolicy: rp, NetworkMode: , Privileged: m.Privileged, CapAdd: m.CapAdd}
	// if net != "host" {

	hostConfig.PortBindings = portMaps
	//}
	containerDb, err := cli.ContainerCreate(ctx,
		config,
		hostConfig,
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{m.NetworkModel: {NetworkID: "", Aliases: []string{}}}},
		nil,
		m.ContainerName)
	if err != nil {
		return "", err
	}
	return containerDb.ID, err
}

func (ds *dockerService) RecreateContainer(ctx context.Context, id string, pull bool, force bool) error {
	containerInfo, err := docker.Container(ctx, id)
	if err != nil {
		return err
	}

	isImageUpdated := false
	var pullErr error
	if pull {
		imageName := docker.ImageName(containerInfo)
		if imageName != "" {
			_isImageUpdated, err := ds.PullLatestImage(ctx, imageName) // image update result will be included in ctx properties
			if err != nil {
				pullErr = err
				logger.Error("pull new image failed", zap.Error(err), zap.String("image", imageName))
			}
			isImageUpdated = _isImageUpdated
		}
	}

	// A FAILED PULL AND AN UP-TO-DATE IMAGE ARE NOT THE SAME ANSWER.
	//
	// Both used to leave isImageUpdated false and fall into `return nil`, so a
	// registry that was unreachable, rate-limiting, or refusing credentials
	// produced the same silence as "you are already on the latest image". The
	// caller in route/v2/docker.go answers 200 immediately and reports the
	// outcome over the event bus, publishing AppUpdateError only when this
	// function returns an error — so with nil returned the user watched
	// "Update" complete and stay on the old image, with nothing anywhere
	// saying the pull never happened.
	//
	// See recreateOutcome for the decision itself; it is a separate function
	// so the three cases can be tested without a Docker daemon.
	proceed, err := recreateOutcome(pull, force, isImageUpdated, pullErr)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}

	// Clone the old container
	var newID string
	if err := func() error {
		tempName := fmt.Sprintf("%s-%s", containerInfo.Name, random.RandomString(4, false))

		go PublishEventWrapper(ctx, common.EventTypeContainerCreateBegin, map[string]string{
			common.PropertyTypeContainerName.Name: tempName,
		})

		defer PublishEventWrapper(ctx, common.EventTypeContainerCreateEnd, map[string]string{
			common.PropertyTypeContainerID.Name:   newID,
			common.PropertyTypeContainerName.Name: tempName,
		})

		_newID, err := docker.CloneContainer(ctx, id, tempName)
		if err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
				common.PropertyTypeContainerName.Name: tempName,
				common.PropertyTypeMessage.Name:       err.Error(),
			})
			return err
		}
		newID = _newID

		return nil
	}(); err != nil {
		return err
	}

	// stop old container if it is running
	if containerInfo.State.Running {
		if err := func() error {
			go PublishEventWrapper(ctx, common.EventTypeContainerStopBegin, map[string]string{
				common.PropertyTypeContainerID.Name: id,
			})

			defer PublishEventWrapper(ctx, common.EventTypeContainerStopEnd, map[string]string{
				common.PropertyTypeContainerID.Name: id,
			})

			if err := docker.StopContainer(ctx, id); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeContainerStopError, map[string]string{
					common.PropertyTypeContainerID.Name: id,
					common.PropertyTypeMessage.Name:     err.Error(),
				})
				return err
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	// start new container
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, map[string]string{
			common.PropertyTypeContainerID.Name: newID,
		})

		defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, map[string]string{
			common.PropertyTypeContainerID.Name: newID,
		})

		if err := docker.StartContainer(ctx, newID); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
				common.PropertyTypeContainerID.Name: newID,
				common.PropertyTypeMessage.Name:     err.Error(),
			})
			return err
		}
		return nil
	}(); err != nil {
		// if failed to start new container and old container was running...
		if containerInfo.State.Running {
			// start the old container
			if err := func() error {
				go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, map[string]string{
					common.PropertyTypeContainerID.Name: id,
				})

				defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, map[string]string{
					common.PropertyTypeContainerID.Name: id,
				})

				if err := docker.StartContainer(ctx, id); err != nil {
					go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
						common.PropertyTypeContainerID.Name: id,
						common.PropertyTypeMessage.Name:     err.Error(),
					})
					return err
				}
				return nil
			}(); err != nil {
				return err
			}

			// remove the new container
			if err := func() error {
				go PublishEventWrapper(ctx, common.EventTypeContainerRemoveBegin, map[string]string{
					common.PropertyTypeContainerID.Name: newID,
				})

				defer PublishEventWrapper(ctx, common.EventTypeContainerRemoveEnd, map[string]string{
					common.PropertyTypeContainerID.Name: newID,
				})

				if err := docker.RemoveContainer(ctx, newID); err != nil {
					go PublishEventWrapper(ctx, common.EventTypeContainerRemoveError, map[string]string{
						common.PropertyTypeContainerID.Name: newID,
						common.PropertyTypeMessage.Name:     err.Error(),
					})
					return err
				}
				return nil
			}(); err != nil {
				return err
			}
		}
		return err // new container failed to start; old container restored — stop here
	}

	// remove the old container if new container started successfully
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerRemoveBegin, map[string]string{
			common.PropertyTypeContainerID.Name: containerInfo.ID,
		})

		defer PublishEventWrapper(ctx, common.EventTypeContainerRemoveEnd, map[string]string{
			common.PropertyTypeContainerID.Name: containerInfo.ID,
		})

		if err := docker.RemoveContainer(ctx, containerInfo.ID); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerRemoveError, map[string]string{
				common.PropertyTypeContainerID.Name: containerInfo.ID,
				common.PropertyTypeMessage.Name:     err.Error(),
			})
			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	// rename the new container
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerRenameBegin, map[string]string{
			common.PropertyTypeContainerID.Name:   newID,
			common.PropertyTypeContainerName.Name: containerInfo.Name,
		})

		defer PublishEventWrapper(ctx, common.EventTypeContainerRenameEnd, map[string]string{
			common.PropertyTypeContainerID.Name:   newID,
			common.PropertyTypeContainerName.Name: containerInfo.Name,
		})

		if err := docker.RenameContainer(ctx, newID, containerInfo.Name); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerRenameError, map[string]string{
				common.PropertyTypeContainerID.Name:   newID,
				common.PropertyTypeContainerName.Name: containerInfo.Name,
				common.PropertyTypeMessage.Name:       err.Error(),
			})

			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	return nil
}

// Remove container
//
// Wave 35-X: bounded ctx. Hung daemon must not pin the UI Remove
// click handler forever.
func (ds *dockerService) RemoveContainer(name string, update bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := docker.RemoveContainer(ctx, name); err != nil {
		return err
	}

	if update {
		return nil
	}

	// Path handling
	if path := docker.GetDir(name, "/config"); !file.CheckNotExist(path) {
		return file.RMDir(path)
	}

	return nil
}

// Stop container
//
// Wave 35-X: bounded context. Previously context.Background() — a
// wedged Docker daemon froze the request handler indefinitely and
// leaked one goroutine per UI Stop click.
func (ds *dockerService) StopContainer(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return docker.StopContainer(ctx, id)
}

// Start container — bounded ctx (see StopContainer above).
func (ds *dockerService) StartContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return docker.StartContainer(ctx, name)
}

// View logs
//
// Wave 35-X: bounded ctx. The 10MB cap doesn't help if the daemon
// stops sending mid-stream — io.ReadAll blocks forever and pins the
// HTTP handler goroutine.
func (ds *dockerService) GetContainerLog(name string) ([]byte, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		return []byte(""), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := cli.ContainerLogs(ctx, name, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return []byte(""), err
	}

	defer body.Close()
	// Limit log reads to 10MB to prevent OOM on containers with huge logs
	const maxLogBytes = 10 * 1024 * 1024
	content, err := io.ReadAll(io.LimitReader(body, maxLogBytes))
	if err != nil {
		return []byte(""), err
	}
	return content, nil
}

func (ds *dockerService) GetContainerByName(name string) (*container.Summary, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		return nil, err
	}
	filter := filters.NewArgs()
	filter.Add("name", name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filter})
	if err != nil {
		return &container.Summary{}, err
	}
	if len(containers) == 0 {
		return &container.Summary{}, errors.New("not found")
	}
	return &containers[0], nil
}

// Get container details
func (ds *dockerService) DescribeContainer(ctx context.Context, nameOrID string) (*container.InspectResponse, error) {
	return docker.Container(ctx, nameOrID)
}

// Update container name
// param name
// param id
func (ds *dockerService) RenameContainer(name, id string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return docker.RenameContainer(ctx, id, name)
}

// Get network list
func (ds *dockerService) GetNetworkList() []network.Inspect {
	cli, err := docker.SharedClient()
	if err != nil {
		logger.Error("Failed to init client", zap.Any("err", err))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	networks, _ := cli.NetworkList(ctx, network.ListOptions{})
	return networks
}

func (ds *dockerService) GetServerInfo() (system.Info, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		return system.Info{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cli.Info(ctx)
}

func NewDockerService() DockerService {
	return &dockerService{}
}

func getV1AppStoreID(m *container.Summary) uint {
	if appStoreIDString, ok := m.Labels[common.ContainerLabelV1AppStoreID]; ok {
		appStoreID, err := strconv.Atoi(appStoreIDString)
		if err != nil {
			logger.Info("failed to convert v1 app store id", zap.Error(err), zap.String("appStoreIDString", appStoreIDString), zap.String("containerID", m.ID), zap.String("containerName", containerName(m.Names)))
		}

		if appStoreID < 0 {
			appStoreID = 0
		}

		return uint(appStoreID)
	}

	logger.Info("the container does not have a v1 app store id", zap.String("containerID", m.ID), zap.String("containerName", containerName(m.Names)))
	return 0
}

// containerName safely returns the first name from a container's Names slice,
// stripped of the leading "/". Returns "unknown" if the slice is empty.
func containerName(names []string) string {
	if len(names) == 0 {
		return "unknown"
	}
	return strings.ReplaceAll(names[0], "/", "")
}
