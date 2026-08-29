package external

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NvidiaGPUInfo struct {
	Index         int
	UUID          string
	DriverVersion string
	Name          string
	GPUSerial     string
}

func NvidiaGPUInfoListWithSMI() ([]NvidiaGPUInfo, error) {
	GPUInfos := []NvidiaGPUInfo{}

	output, err := exec.Command("nvidia-smi", "--query-gpu=index,uuid,driver_version,name,gpu_serial", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		value := strings.Split(line, ", ")
		if len(value) == 5 {
			index, _ := strconv.Atoi(value[0])

			GPUInfos = append(GPUInfos, NvidiaGPUInfo{
				Index:         index,
				UUID:          value[1],
				DriverVersion: value[2],
				Name:          value[3],
				GPUSerial:     value[4],
			})
		} else {
			continue
		}
	}
	return GPUInfos, nil
}

// gpuCacheTTL is how long a GPU inventory answer stays good. The set of GPUs in
// a machine changes on a driver reload or a reboot, not between requests, so a
// minute is generous and still notices a driver that came back.
const gpuCacheTTL = time.Minute

var gpuCache struct {
	mu   sync.Mutex
	at   time.Time
	list []NvidiaGPUInfo
	err  error
}

// NvidiaGPUInfoList returns the machine's NVIDIA GPUs, cached.
//
// WHY THIS IS CACHED, AND WHY THE FAILURE IS CACHED TOO.
//
// It shells out to nvidia-smi, and three separate modules call it — including
// CasaNova-UserService's GET /v1/users/status, which the login screen polls and
// which answers without a token. Every one of those requests forked a process.
//
// Measured, and the number depends entirely on the machine:
//
//	nvidia-smi absent (this box: AMD with integrated graphics)   0.194 ms
//	nvidia-smi present but no working driver                    28 ms
//	a plain successful exec, for scale                            1.2 ms
//
// The expensive case is the awkward one: a machine that HAS the tool and no
// usable GPU pays 28 ms per call to be told "no" again, on an unauthenticated
// endpoint. That is why the ERROR is cached as well as the success — caching
// only the happy path would leave exactly the slow case uncached, which is the
// wrong way round.
//
// On this particular box the saving is microseconds. It is worth doing anyway
// because the cost is not a property of the code, it is a property of whatever
// machine the code lands on, and 28 ms is a number someone else will hit.
func NvidiaGPUInfoList() ([]NvidiaGPUInfo, error) {
	gpuCache.mu.Lock()
	defer gpuCache.mu.Unlock()

	if !gpuCache.at.IsZero() && time.Since(gpuCache.at) < gpuCacheTTL {
		return gpuCache.list, gpuCache.err
	}

	list, err := probeGPUs()
	gpuCache.at, gpuCache.list, gpuCache.err = time.Now(), list, err
	return list, err
}

// probeGPUs is the real probe, behind a variable so a test can force the
// failure path. Without this the "nvidia-smi exists but no GPU" case — the one
// that costs 28 ms and is the whole reason the error is cached — could only be
// exercised on a machine that happens to be configured that way, and a test
// that skips on the developer's laptop is not a test.
var probeGPUs = NvidiaGPUInfoListWithSMI

// SetGPUProbeForTest swaps the probe and returns a function restoring it.
func SetGPUProbeForTest(f func() ([]NvidiaGPUInfo, error)) func() {
	prev := probeGPUs
	probeGPUs = f
	ResetGPUCacheForTest()
	return func() { probeGPUs = prev; ResetGPUCacheForTest() }
}

// ResetGPUCacheForTest drops the cached inventory.
func ResetGPUCacheForTest() {
	gpuCache.mu.Lock()
	defer gpuCache.mu.Unlock()
	gpuCache.at, gpuCache.list, gpuCache.err = time.Time{}, nil, nil
}
