package v1

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// zfsExecTimeout is the deadline applied to every zpool/zfs subprocess.
// Five minutes is generous enough for a slow `zpool import` or large
// scrub-status walk, but short enough that a stuck device can't pin the
// handler indefinitely.
const zfsExecTimeout = 5 * time.Minute

// zfsExec runs a zpool/zfs command bounded by zfsExecTimeout. Returns
// combined stdout+stderr exactly like CombinedOutput, plus a separate
// "timed out" sentinel so callers can surface a useful error.
func zfsExec(bin string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(context.Background(), zfsExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, bin, args...).CombinedOutput()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", bin, zfsExecTimeout)
	}
	return out, err
}

// zfsExecOutput is the stdout-only variant for commands whose stderr is
// noise. Same timeout contract.
func zfsExecOutput(bin string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(context.Background(), zfsExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, bin, args...).Output()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", bin, zfsExecTimeout)
	}
	return out, err
}

// ---- ZFS Snapshot Management ----

// validSnapshotName matches alphanumeric, dashes, dots, and underscores.
var validSnapshotName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validDatasetName matches a ZFS dataset path like "tank", "tank/data", "rpool/ROOT".
var validDatasetName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

type ZFSSnapshot struct {
	Name       string `json:"name"`
	Dataset    string `json:"dataset"`
	Snapshot   string `json:"snapshot"`
	Creation   string `json:"creation"`
	Used       string `json:"used"`
	Referenced string `json:"referenced"`
}

type ZFSDataset struct {
	Name       string `json:"name"`
	Used       string `json:"used"`
	Available  string `json:"available"`
	Referenced string `json:"referenced"`
	MountPoint string `json:"mountpoint"`
}

// GET /v1/sys/zfs/snapshots — List all ZFS snapshots.
func GetZFSSnapshots(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	if _, err := exec.LookPath("zfs"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "ZFS not available",
			Data:    []ZFSSnapshot{},
		})
	}

	out, err := zfsExecOutput("zfs", "list", "-H", "-t", "snapshot", "-o", "name,creation,used,refer", "-s", "creation")
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to list snapshots: " + err.Error(),
			Data:    []ZFSSnapshot{},
		})
	}

	var snapshots []ZFSSnapshot
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}

		fullName := fields[0]
		parts := strings.SplitN(fullName, "@", 2)
		dataset := ""
		snapName := fullName
		if len(parts) == 2 {
			dataset = parts[0]
			snapName = parts[1]
		}

		snapshots = append(snapshots, ZFSSnapshot{
			Name:       fullName,
			Dataset:    dataset,
			Snapshot:   snapName,
			Creation:   fields[1],
			Used:       fields[2],
			Referenced: fields[3],
		})
	}

	if snapshots == nil {
		snapshots = []ZFSSnapshot{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    snapshots,
	})
}

// POST /v1/sys/zfs/snapshots — Create a ZFS snapshot.
func PostZFSSnapshot(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("zfs"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "ZFS is not available",
		})
	}

	var body struct {
		Dataset string `json:"dataset"`
		Name    string `json:"name"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	// Validate dataset.
	if body.Dataset == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "dataset is required",
		})
	}
	if strings.Contains(body.Dataset, "..") || strings.Contains(body.Dataset, " ") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "dataset contains invalid characters",
		})
	}
	if !validDatasetName.MatchString(body.Dataset) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "dataset name is invalid",
		})
	}

	// Validate snapshot name.
	if body.Name == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "name is required",
		})
	}
	if !validSnapshotName.MatchString(body.Name) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "snapshot name must be alphanumeric with dashes, dots, and underscores",
		})
	}

	fullName := body.Dataset + "@" + body.Name
	out, err := zfsExec("zfs", "snapshot", fullName)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("failed to create snapshot: %s", strings.TrimSpace(string(out))),
		})
	}

	log.Printf("[ZFS] Created snapshot: %s", fullName)

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Snapshot created",
		Data:    map[string]string{"name": fullName},
	})
}

// DELETE /v1/sys/zfs/snapshots — Destroy a ZFS snapshot.
func DeleteZFSSnapshot(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("zfs"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "ZFS is not available",
		})
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if err := validateFullSnapshotName(body.Name); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: err.Error(),
		})
	}

	out, err := zfsExec("zfs", "destroy", body.Name)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("failed to destroy snapshot: %s", strings.TrimSpace(string(out))),
		})
	}

	log.Printf("[ZFS] Destroyed snapshot: %s", body.Name)

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Snapshot destroyed",
	})
}

// POST /v1/sys/zfs/snapshots/rollback — Rollback to a ZFS snapshot.
// WARNING: This is destructive — it destroys all snapshots newer than the target.
func PostZFSRollback(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("zfs"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "ZFS is not available",
		})
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if err := validateFullSnapshotName(body.Name); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: err.Error(),
		})
	}

	log.Printf("[ZFS] ROLLBACK initiated to snapshot: %s — this will destroy newer snapshots", body.Name)

	out, err := zfsExec("zfs", "rollback", "-r", body.Name)
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("rollback failed: %s", strings.TrimSpace(string(out))),
		})
	}

	log.Printf("[ZFS] Rollback to %s completed successfully", body.Name)

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Rollback completed",
	})
}

// GET /v1/sys/zfs/datasets — List ZFS datasets/pools.
func GetZFSDatasets(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	if _, err := exec.LookPath("zfs"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "ZFS not available",
			Data:    []ZFSDataset{},
		})
	}

	out, err := zfsExecOutput("zfs", "list", "-H", "-o", "name,used,avail,refer,mountpoint")
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to list datasets: " + err.Error(),
			Data:    []ZFSDataset{},
		})
	}

	var datasets []ZFSDataset
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		datasets = append(datasets, ZFSDataset{
			Name:       fields[0],
			Used:       fields[1],
			Available:  fields[2],
			Referenced: fields[3],
			MountPoint: fields[4],
		})
	}

	if datasets == nil {
		datasets = []ZFSDataset{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    datasets,
	})
}

// validateFullSnapshotName checks that a full snapshot name (dataset@snap)
// is safe for shell execution.
func validateFullSnapshotName(name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is required")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("path traversal not allowed")
	}
	parts := strings.SplitN(name, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("snapshot name must be in dataset@snapshot format")
	}
	if !validDatasetName.MatchString(parts[0]) {
		return fmt.Errorf("dataset name is invalid")
	}
	if !validSnapshotName.MatchString(parts[1]) {
		return fmt.Errorf("snapshot name is invalid")
	}
	return nil
}

// ─── Tier 1: detection + pool inventory ─────────────────────────────────────
//
// These endpoints power the "ZFS Pools" panel in Storage settings. They
// are read-only and never mutate pool state — safe to expose to any user
// with PermSystemRead. The mutation endpoints (scrub start/stop, import,
// export) live below and gate on PermStorageWrite.

// validPoolName matches the same character class zpool create accepts:
// letters, digits, underscore, dot, dash, and colon for raidz suffixes.
// Excludes "/" which is dataset territory.
var validPoolName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]*$`)

// ZFSStatus is the bedrock detection answer: are the userland tools on
// this host, and if so what version. Frontend uses available=false to
// hide the entire ZFS card with a "install zfsutils-linux to enable"
// hint, so no ZFS UI ever lies about being functional.
type ZFSStatus struct {
	Available bool   `json:"available"`
	ZpoolPath string `json:"zpoolPath,omitempty"`
	ZfsPath   string `json:"zfsPath,omitempty"`
	Version   string `json:"version,omitempty"`
	PoolCount int    `json:"poolCount"`
}

// ZFSVdev is one device underneath a vdev tree node. The shape mirrors
// `zpool status -P` output — type is "disk" or one of the redundancy
// vdev kinds ("mirror", "raidz1", etc.).
type ZFSVdev struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	State    string    `json:"state"`
	Read     int       `json:"read"`
	Write    int       `json:"write"`
	Cksum    int       `json:"cksum"`
	Children []ZFSVdev `json:"children,omitempty"`
}

// ZFSScrubState is the scrub progress block returned by `zpool status`.
// All fields are zero when no scrub has ever run on the pool.
type ZFSScrubState struct {
	State           string `json:"state"`           // "none" | "scrubbing" | "finished" | "canceled"
	PercentComplete string `json:"percentComplete"` // "12.34%"
	Errors          int    `json:"errors"`
	StartedAt       string `json:"startedAt,omitempty"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	Speed           string `json:"speed,omitempty"`
	Repaired        string `json:"repaired,omitempty"`
}

// ZFSPool is the row in the pools table. Capacity numbers are kept as
// strings (the format `zpool list -p` emits) so the UI can render them
// without rounding errors.
type ZFSPool struct {
	Name       string        `json:"name"`
	Health     string        `json:"health"` // ONLINE / DEGRADED / FAULTED / OFFLINE / REMOVED / UNAVAIL
	SizeBytes  uint64        `json:"sizeBytes"`
	AllocBytes uint64        `json:"allocBytes"`
	FreeBytes  uint64        `json:"freeBytes"`
	Frag       string        `json:"frag"`  // "13%"
	Dedup      string        `json:"dedup"` // "1.02x"
	Vdevs      []ZFSVdev     `json:"vdevs"`
	Scrub      ZFSScrubState `json:"scrub"`
}

func GetZFSStatus(ctx echo.Context) error {
	st := ZFSStatus{}
	if p, err := exec.LookPath("zpool"); err == nil {
		st.Available = true
		st.ZpoolPath = p
	}
	if p, err := exec.LookPath("zfs"); err == nil {
		st.ZfsPath = p
	} else {
		// Both binaries must be present — the snapshot endpoints depend
		// on zfs(8) regardless of whether zpool exists.
		st.Available = false
	}
	if st.Available {
		if out, err := zfsExec("zfs", "version"); err == nil {
			lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
			if len(lines) > 0 {
				st.Version = strings.TrimSpace(lines[0])
			}
		}
		if out, err := zfsExecOutput("zpool", "list", "-Hp", "-o", "name"); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(line) != "" {
					st.PoolCount++
				}
			}
		}
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    st,
	})
}

// parseZpoolStatus walks a single pool's `zpool status -P <pool>` output
// and recovers the vdev tree + scrub progress. Strict line parsing —
// any format change in newer ZFS releases would degrade to "best
// effort" rather than crashing.
func parseZpoolStatus(name string) ([]ZFSVdev, ZFSScrubState) {
	scrub := ZFSScrubState{State: "none"}
	var vdevs []ZFSVdev

	out, err := zfsExecOutput("zpool", "status", "-P", name)
	if err != nil {
		return vdevs, scrub
	}
	lines := strings.Split(string(out), "\n")
	inConfig := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Scrub line: "scan: scrub in progress since …" or "scan: scrub
		// repaired … with 0 errors on …"
		if strings.HasPrefix(trimmed, "scan:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "scan:"))
			switch {
			case strings.Contains(rest, "in progress"):
				scrub.State = "scrubbing"
			case strings.Contains(rest, "repaired") || strings.Contains(rest, "completed"):
				scrub.State = "finished"
			case strings.Contains(rest, "canceled"):
				scrub.State = "canceled"
			}
			continue
		}
		if strings.Contains(trimmed, "scanned at") || strings.Contains(trimmed, "issued at") {
			// e.g. "12.3G scanned at 200M/s, 5.6G issued at 90M/s, 100G total"
			// pull a percent if present
			// A rough percent could be computed from scanned/total, but
			// zpool ≥ 2.1 emits an explicit "scanned: X%" line we catch
			// below — only pull an explicit percent here.
			for _, f := range strings.Fields(trimmed) {
				if strings.HasSuffix(f, "%") {
					scrub.PercentComplete = f
				}
			}
			continue
		}

		if strings.HasPrefix(trimmed, "config:") {
			inConfig = true
			continue
		}
		if inConfig && trimmed == "" {
			inConfig = false
			continue
		}
		if !inConfig {
			continue
		}
		// Header row: "NAME  STATE  READ WRITE CKSUM"
		if strings.HasPrefix(trimmed, "NAME") {
			continue
		}
		// Each remaining row: NAME STATE READ WRITE CKSUM (indented).
		fields := strings.Fields(trimmed)
		if len(fields) < 5 {
			continue
		}
		// Skip the pool root row (it duplicates the pool name).
		if fields[0] == name {
			continue
		}
		v := ZFSVdev{Name: fields[0], State: fields[1], Type: vdevTypeOf(fields[0])}
		fmt.Sscanf(fields[2], "%d", &v.Read)
		fmt.Sscanf(fields[3], "%d", &v.Write)
		fmt.Sscanf(fields[4], "%d", &v.Cksum)
		vdevs = append(vdevs, v)
	}
	return vdevs, scrub
}

// vdevTypeOf classifies a vdev line entry as either a leaf "disk" or
// one of the redundancy types. Anything containing "mirror" or "raidz"
// is the latter; everything else is a disk path.
func vdevTypeOf(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.HasPrefix(low, "mirror"):
		return "mirror"
	case strings.HasPrefix(low, "raidz3"):
		return "raidz3"
	case strings.HasPrefix(low, "raidz2"):
		return "raidz2"
	case strings.HasPrefix(low, "raidz"):
		return "raidz1"
	case strings.HasPrefix(low, "log"), strings.HasPrefix(low, "cache"), strings.HasPrefix(low, "spare"):
		return low
	default:
		return "disk"
	}
}

// GetZFSPools — Tier 1 inventory. One row per imported pool with
// vdev tree and scrub state attached. Returns an empty slice when
// the host has no ZFS tooling so the UI can show a neutral empty
// state instead of an error.
func GetZFSPools(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	if _, err := exec.LookPath("zpool"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "ZFS not available",
			Data:    []ZFSPool{},
		})
	}
	out, err := zfsExecOutput("zpool", "list", "-Hp",
		"-o", "name,size,alloc,free,frag,dedup,health")
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "no pools",
			Data:    []ZFSPool{},
		})
	}
	pools := []ZFSPool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		p := ZFSPool{
			Name:   fields[0],
			Health: fields[6],
			Frag:   fields[4],
			Dedup:  fields[5],
		}
		fmt.Sscanf(fields[1], "%d", &p.SizeBytes)
		fmt.Sscanf(fields[2], "%d", &p.AllocBytes)
		fmt.Sscanf(fields[3], "%d", &p.FreeBytes)
		pools = append(pools, p)
	}

	// Per-pool `zpool status -P` is independent across pools but the
	// prior implementation called it serially in the parse loop. On a
	// box with 8 pools that's 8×~50ms serialized. Fan them out — each
	// zfsExecOutput is bounded by zfsExecTimeout so the total latency
	// is capped at one timeout, not N.
	if len(pools) > 1 {
		var wg sync.WaitGroup
		wg.Add(len(pools))
		for i := range pools {
			go func(idx int) {
				defer wg.Done()
				pools[idx].Vdevs, pools[idx].Scrub = parseZpoolStatus(pools[idx].Name)
			}(i)
		}
		wg.Wait()
	} else if len(pools) == 1 {
		pools[0].Vdevs, pools[0].Scrub = parseZpoolStatus(pools[0].Name)
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    pools,
	})
}

// ─── Tier 2: safe ops ────────────────────────────────────────────────────────

// PostZFSScrub starts or stops a scrub on the named pool. Action must be
// "start" or "stop"; pool must match validPoolName. Scrub is the most
// common operator action — it's read-only against the data, repairs
// silent corruption, and has no destructive failure mode.
func PostZFSScrub(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Pool   string `json:"pool"`
		Action string `json:"action"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if !validPoolName.MatchString(body.Pool) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	argv := []string{"scrub"}
	switch body.Action {
	case "start":
		// no flag — default starts
	case "stop":
		argv = append(argv, "-s")
	default:
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "action must be 'start' or 'stop'",
		})
	}
	argv = append(argv, body.Pool)
	if out, err := zfsExec("zpool", argv...); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool scrub failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// PostZFSImport imports a pool by name. Use case: pool was exported
// (or the box rebooted with import disabled). Doesn't accept a guid
// path argument because that's footgun territory — pool naming is
// the operator-friendly route.
func PostZFSImport(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Pool  string `json:"pool"`
		Force bool   `json:"force"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if !validPoolName.MatchString(body.Pool) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	argv := []string{"import"}
	if body.Force {
		argv = append(argv, "-f")
	}
	argv = append(argv, body.Pool)
	if out, err := zfsExec("zpool", argv...); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool import failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// PostZFSExport cleanly exports a pool — flushes write cache, marks the
// pool as not-imported, frees the vdevs so the disks can be physically
// moved. Refuses with "force=false" if any dataset is busy; force=true
// passes -f and lets ZFS sort it out.
func PostZFSExport(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Pool  string `json:"pool"`
		Force bool   `json:"force"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if !validPoolName.MatchString(body.Pool) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	argv := []string{"export"}
	if body.Force {
		argv = append(argv, "-f")
	}
	argv = append(argv, body.Pool)
	if out, err := zfsExec("zpool", argv...); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool export failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// suppressUnused — log is imported by the snapshot handlers above; this
// keeps go vet quiet if a future refactor drops the snapshot path.
var _ = log.Println

// ─── Tier 3: destructive lifecycle (gated by advanced mode in the UI) ──
//
// These endpoints can wipe data — they require PermStorageWrite at the
// API layer AND a separate "advanced mode" toggle on the frontend. The
// backend doesn't know about that toggle (it's a UI affordance), so any
// caller that hits these endpoints directly is assumed to have read the
// docs and accepts the risk.

// devicePathRe is the strict allow-list for vdev device paths. Accepts
// /dev/sd*, /dev/nvme*, /dev/disk/by-id/*, /dev/disk/by-uuid/*, and the
// virtual /dev/mapper/* path for LUKS-backed pools. Rejects anything
// else (no shell meta, no relative paths, no /etc, no ..).
var devicePathRe = regexp.MustCompile(`^/dev/(sd[a-z]+[0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|disk/by-(id|uuid|partuuid|label)/[A-Za-z0-9._:-]+|mapper/[A-Za-z0-9_-]+)$`)

func validateDevicePaths(devices []string) error {
	if len(devices) == 0 {
		return fmt.Errorf("at least one device is required")
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if !devicePathRe.MatchString(d) {
			return fmt.Errorf("invalid device path %q", d)
		}
		if seen[d] {
			return fmt.Errorf("device %q listed twice", d)
		}
		seen[d] = true
	}
	return nil
}

// PostZFSCreatePool — Tier 3 / `zpool create`. The topology argument is
// one of the canonical ZFS vdev keywords; "stripe" is the no-keyword
// case (each device becomes its own top-level vdev). Anything else is
// emitted before the device list.
//
//	POST /v1/sys/zfs/pools
//	  {
//	    "name":       "tank",
//	    "topology":   "stripe" | "mirror" | "raidz1" | "raidz2" | "raidz3",
//	    "devices":    ["/dev/sdb", "/dev/sdc"],
//	    "mountpoint": "/tank",     // optional; defaults to /<name>
//	    "force":      false,       // -f (overwrite existing labels)
//	    "dryRun":     false        // -n (preview, no writes)
//	  }
//
// The handler refuses topologies that don't have enough devices for
// their minimum, and refuses to clobber the system disk (sentinel:
// root partition). Forward progress is hard to roll back, so we lean
// strict.
func PostZFSCreatePool(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Name       string   `json:"name"`
		Topology   string   `json:"topology"`
		Devices    []string `json:"devices"`
		MountPoint string   `json:"mountpoint"`
		Force      bool     `json:"force"`
		DryRun     bool     `json:"dryRun"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Topology = strings.ToLower(strings.TrimSpace(body.Topology))

	if !validPoolName.MatchString(body.Name) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	if err := validateDevicePaths(body.Devices); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if body.MountPoint != "" && !strings.HasPrefix(body.MountPoint, "/") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "mountpoint must be an absolute path",
		})
	}

	minDevices := map[string]int{
		"stripe": 1,
		"mirror": 2,
		"raidz1": 3,
		"raidz2": 4,
		"raidz3": 5,
	}
	min, ok := minDevices[body.Topology]
	if !ok {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "topology must be one of: stripe, mirror, raidz1, raidz2, raidz3",
		})
	}
	if len(body.Devices) < min {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("%s requires at least %d devices", body.Topology, min),
		})
	}

	// Refuse to touch the system disk. getRootDevice returns "/dev/sdX"
	// — every prefix-match rejection guards against also picking the
	// partitions (/dev/sdX1, /dev/sdXp1).
	if rootDev := zfsGetRootDevice(); rootDev != "" {
		for _, d := range body.Devices {
			if d == rootDev || strings.HasPrefix(d, rootDev) {
				return ctx.JSON(http.StatusForbidden, model.Result{
					Success: common_err.SERVICE_ERROR,
					Message: "refusing to create a pool on the system disk: " + d,
				})
			}
		}
	}

	argv := []string{"create"}
	if body.Force {
		argv = append(argv, "-f")
	}
	if body.DryRun {
		argv = append(argv, "-n")
	}
	if body.MountPoint != "" {
		argv = append(argv, "-m", body.MountPoint)
	}
	argv = append(argv, body.Name)
	// Topology keyword goes between the pool name and the device list.
	// "stripe" is the implicit no-keyword case.
	if body.Topology != "stripe" {
		argv = append(argv, body.Topology)
	}
	argv = append(argv, body.Devices...)

	out, err := zfsExec("zpool", argv...)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool create failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]string{
			"pool":   body.Name,
			"dryRun": fmt.Sprintf("%t", body.DryRun),
			"output": strings.TrimSpace(string(out)),
		},
	})
}

// DeleteZFSPool — Tier 3 / `zpool destroy`. Permanent data loss —
// every dataset, every snapshot, gone. The handler requires the
// caller to echo the pool name in two fields to defend against an
// accidental swipe of a typo'd row.
//
//	DELETE /v1/sys/zfs/pools
//	  { "pool": "tank", "confirmName": "tank", "force": false }
func DeleteZFSPool(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Pool        string `json:"pool"`
		ConfirmName string `json:"confirmName"`
		Force       bool   `json:"force"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	body.Pool = strings.TrimSpace(body.Pool)
	body.ConfirmName = strings.TrimSpace(body.ConfirmName)
	if !validPoolName.MatchString(body.Pool) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	if body.ConfirmName != body.Pool {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "confirmName must match pool",
		})
	}
	argv := []string{"destroy"}
	if body.Force {
		argv = append(argv, "-f")
	}
	argv = append(argv, body.Pool)
	if out, err := zfsExec("zpool", argv...); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool destroy failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// PostZFSReplace — Tier 3 / `zpool replace`. Replaces the failing
// device in a pool with a new spare. Both arguments validated against
// the device-path allow-list. Triggers a resilver — the UI should
// surface that as scrub-like progress (zpool status reports it under
// the same "scan:" line).
//
//	POST /v1/sys/zfs/replace
//	  { "pool": "tank", "oldDevice": "/dev/sdc", "newDevice": "/dev/sde" }
func PostZFSReplace(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var body struct {
		Pool      string `json:"pool"`
		OldDevice string `json:"oldDevice"`
		NewDevice string `json:"newDevice"`
		Force     bool   `json:"force"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if !validPoolName.MatchString(strings.TrimSpace(body.Pool)) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid pool name",
		})
	}
	if !devicePathRe.MatchString(body.OldDevice) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid oldDevice path",
		})
	}
	if !devicePathRe.MatchString(body.NewDevice) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid newDevice path",
		})
	}
	if body.OldDevice == body.NewDevice {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "old and new device must differ",
		})
	}
	if rootDev := zfsGetRootDevice(); rootDev != "" {
		if strings.HasPrefix(body.NewDevice, rootDev) {
			return ctx.JSON(http.StatusForbidden, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "refusing to use the system disk as a replacement vdev",
			})
		}
	}
	argv := []string{"replace"}
	if body.Force {
		argv = append(argv, "-f")
	}
	argv = append(argv, body.Pool, body.OldDevice, body.NewDevice)
	if out, err := zfsExec("zpool", argv...); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: fmt.Sprintf("zpool replace failed: %s — %s", err.Error(), strings.TrimSpace(string(out))),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// zfsGetRootDevice reads /proc/self/mountinfo for the device that backs
// `/`, then strips any partition suffix so the returned string matches
// "/dev/sdX" rather than "/dev/sdX2". Returns "" when discovery fails —
// callers treat that as "skip the system-disk check" rather than
// "refuse everything", because a missing rootfs probe shouldn't lock
// the operator out of pool creation entirely.
func zfsGetRootDevice() string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		// Each mountinfo line has the mount point in the 5th field.
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[4] != "/" {
			continue
		}
		// Last field is "source device path" after the " - <fstype>" marker.
		for i, f := range fields {
			if f == "-" && i+2 < len(fields) {
				dev := fields[i+2]
				// Strip trailing partition digits ("/dev/sda3" → "/dev/sda";
				// "/dev/nvme0n1p2" → "/dev/nvme0n1"). NVMe paths have a
				// "p" before the partition number, SATA paths don't.
				if strings.HasPrefix(dev, "/dev/nvme") {
					if idx := strings.LastIndex(dev, "p"); idx > 0 {
						return dev[:idx]
					}
					return dev
				}
				return strings.TrimRightFunc(dev, func(r rune) bool { return r >= '0' && r <= '9' })
			}
		}
	}
	return ""
}
