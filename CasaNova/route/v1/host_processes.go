package v1

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
	"github.com/shirou/gopsutil/v3/process"
)

// Read-only process viewer. Top processes by memory, with the owning systemd
// unit / container resolved from /proc/PID/cgroup. NO kill/renice in this phase
// — the UI links container-owned processes back to "restart from Apps". Purely
// observational, so no write surface at all.
type ProcInfo struct {
	PID     int32   `json:"pid"`
	Name    string  `json:"name"`
	User    string  `json:"user"`
	CPUPct  float64 `json:"cpuPct"` // approximate (lifetime average)
	MemRSSB uint64  `json:"memRssBytes"`
	Owner   string  `json:"owner"` // "docker:<name>", "unit:<x>.service", or ""
}

var dockerCgroupRe = regexp.MustCompile(`docker[-/]([0-9a-f]{12})`)
var unitCgroupRe = regexp.MustCompile(`([a-zA-Z0-9_.@-]+\.service)`)

// GET /v1/sys/host/processes?limit=25
func GetHostProcesses(ctx echo.Context) error {
	// requireSystemPrincipal: see route/v1/antivirus.go's GetAntivirusStatus
	// comment — guest holds PermSystemRead, so a bare requirePerm never
	// refused a tokenless caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	limit := 25
	if l := ctx.QueryParam("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	procs, err := process.Processes()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	// Two passes on purpose. Only the top-N by memory are ever returned, but
	// the enrichment per process is expensive — Username() does a user
	// database lookup, and ownerFromCgroup reads /proc/<pid>/cgroup — and this
	// endpoint is polled every 5s. Rank on the cheap field first, then pay for
	// detail on the handful of rows that survive. Output is identical.
	type ranked struct {
		p   *process.Process
		rss uint64
	}
	all := make([]ranked, 0, len(procs))
	for _, p := range procs {
		mi, err := p.MemoryInfo()
		if err != nil || mi == nil {
			continue
		}
		all = append(all, ranked{p: p, rss: mi.RSS})
	}
	// Top-N by resident memory (the metric that actually matters on a NAS).
	sort.Slice(all, func(i, j int) bool { return all[i].rss > all[j].rss })
	if len(all) > limit {
		all = all[:limit]
	}

	out := make([]ProcInfo, 0, len(all))
	for _, r := range all {
		pi := ProcInfo{PID: r.p.Pid, MemRSSB: r.rss}
		pi.Name, _ = r.p.Name()
		pi.User, _ = r.p.Username()
		pi.CPUPct, _ = r.p.CPUPercent()
		pi.Owner = ownerFromCgroup(r.p.Pid)
		out = append(out, pi)
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: out})
}

func ownerFromCgroup(pid int32) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(int(pid)) + "/cgroup")
	if err != nil {
		return ""
	}
	s := string(raw)
	if m := dockerCgroupRe.FindStringSubmatch(s); m != nil {
		return "docker:" + m[1]
	}
	// Prefer a real .service unit, skipping the generic slices.
	for _, m := range unitCgroupRe.FindAllString(s, -1) {
		if !strings.HasPrefix(m, "system.slice") && m != "-.service" {
			return "unit:" + m
		}
	}
	return ""
}
