package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	exec2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/exec"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/port"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/version"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/LeewayOpenSoftware/CasaNova/types"
	"github.com/labstack/echo/v4"
	psnet "github.com/shirou/gopsutil/v3/net"
	"github.com/tidwall/gjson"
)

// requireSystemPrincipal gates a handler that exposes internal system state on
// an AUTHENTICATED principal FIRST, and only then on an RBAC permission.
//
// Why the permission alone is not a boundary here. A tokenless request reaches
// these handlers with no role on the context, and RoleFromContext fails closed
// to guest — which deliberately holds PermSystemRead. `requirePerm(ctx,
// PermSystemRead)` therefore answers "allowed" for an anonymous caller.
// The v1 JWT middleware rejects unauthenticated requests at the perimeter, but
// handler-level defense-in-depth is essential: historically the middleware
// skipped loopback requests and exposed these handlers to the internet.
// Requiring an authenticated principal ensures that even if called outside the
// group middleware or tested directly, anonymous callers are stopped.
//
// UserIDFromContext is non-zero only after that middleware validated a real
// token: SetRoleInContext stores claims.ID, and the only token issuer
// (CasaNova-UserService) signs the database user id, which starts at 1. So it
// is the identity signal, and the permission argument then decides WHICH
// authenticated principals may read. No production peer calls the endpoints
// below without a user token (checked across the ten modules, the CLI, the
// installer and the shipped UI); a future peer would present the internal
// secret via requirePermInternal instead of being let in by omission.
//
// Returns an *echo.HTTPError and never writes the response itself: a helper
// that denies with ctx.JSON returns the nil the serializer hands back, and the
// canonical `if err := gate(…); err != nil { return err }` call site then falls
// through and does the work anyway — see requirePerm's comment for the live
// case where that shipped.
// WHY THIS EXISTS AND WHEN IT IS THE ONLY CORRECT GATE.
//
// A tokenless request resolves to RoleGuest, and guestPerms deliberately holds
// PermAppView and PermSystemRead. If an endpoint relying only on
// `requirePerm(ctx, rbac.PermSystemRead)` is ever invoked with an anonymous
// context (such as when remounted outside the group JWT middleware or tested
// directly), it ALLOWS THE CALLER. It reads like a gate, it survives review,
// and it enforces nothing against an unauthenticated context.
//
// So: use requirePerm when the permission is already beyond guest
// (PermFileRead, PermStorageRead, PermSystemNetwork, anything admin). Use this
// one whenever the permission is inside guestPerms — and when in doubt use this
// one, because being too strict shows up as a 401 an operator can see.
//
// It lives here rather than in rbac_gate.go for historical reasons, and that
// cost something: it was duplicated once by someone who went looking for the
// gate helpers where the gate helpers are. If you are adding another, put it in
// rbac_gate.go and move this one with it.
func requireSystemPrincipal(ctx echo.Context, p rbac.Permission) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, model.Result{
			Success: common_err.ERROR_AUTH_TOKEN,
			Message: "authentication required",
		})
	}
	return requirePerm(ctx, p)
}

// @Summary check version
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/version/check [get]
//
// The version payload stays readable without a token: this handler is mounted
// on /v1/sys/version/check OUTSIDE the JWT group (route/v1.go) so the login
// screen can show a version before anyone has one, and the same string is
// already served publicly by /v1/sys/version/current.
//
// The notification INSERT is not part of that bargain. AddLog is an
// unconditional `db.Create` with no de-duplication, so on a box that has
// ServerInfo.ServerApi configured every anonymous GET appended a row to the
// notification table — an unauthenticated write primitive, reachable from the
// internet, that grows the database and floods the owner's notification list.
// Recording it only for an identified caller keeps the behaviour the WebUI
// sees (it polls /v1/sys/version inside the JWT group, with a token) and
// removes the anonymous write. It does NOT de-duplicate the row; the repeated
// notification for a signed-in poller is a separate, pre-existing defect.
func GetSystemCheckVersion(ctx echo.Context) error {
	need, version := version.IsNeedUpdate(service.MyService.Casa().GetCasaosVersion())
	if need && commonmw.UserIDFromContext(ctx) != 0 {
		installLog := model2.AppNotify{}
		installLog.State = 0
		installLog.Message = "New version " + version.Version + " is ready, ready to upgrade"
		installLog.Type = types.NOTIFY_TYPE_NEED_CONFIRM
		installLog.CreatedAt = strconv.FormatInt(time.Now().Unix(), 10)
		installLog.UpdatedAt = strconv.FormatInt(time.Now().Unix(), 10)
		installLog.Name = "CasaOS System"
		service.MyService.Notify().AddLog(installLog)
	}
	data := make(map[string]interface{}, 3)
	data["need_update"] = need
	data["version"] = version
	data["current_version"] = common.VERSION
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/update [post]
func SystemUpdate(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	need, version := version.IsNeedUpdate(service.MyService.Casa().GetCasaosVersion())
	if need {
		service.MyService.System().UpdateSystemVersion(version.Version)
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary  get logs
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/error/logs [get]
//
// GET /v1/sys/logs returns the TAIL OF THE DAEMON LOG (up to 4 MiB of
// /var/log/casaos/casanova.log). That is not a "paste this into a GitHub
// issue" payload on this product, whatever it was upstream — traced against
// what this build actually writes, the file carries absolute paths inside the
// user's data area (upload, rename and archive errors), Samba share names and
// share paths, cloud-remote account usernames, LAN/interface addresses,
// systemd unit and service names, upload-session identifiers and the stdout of
// update scripts that run as root. Read together it is a map of the box and of
// its owner's files.
//
// So it needs an identified caller AND PermSystemConfig — the same permission
// the maintenance module requires for its read-only scan, and for the same
// reason: enumerating the user's paths is reconnaissance even though it mutates
// nothing. PermSystemRead would have been decorative here, since guest holds it
// and a tokenless caller resolves to guest.
func GetCasaOSErrorLogs(ctx echo.Context) error {
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	line, _ := strconv.Atoi(utils.DefaultQuery(ctx, "line", "100"))
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: service.MyService.System().GetCasaOSLogs(line)})
}

// GetSystemConfigDebug builds the system profile (GET /v1/sys/debug).
//
// It shells out to helper.sh GetSysInfo, so the body contains `uname -a` — the
// exact kernel build and architecture — plus /etc/issue or the distro release
// string, total RAM, core count, the system disk's total and used bytes and the
// configured update server's version. That is a targeting profile for whoever
// is deciding which local-privilege-escalation to try against a box exposed by
// a tunnel, and it is exactly the audience that must not have it.
//
// Same gate as the log tail: an identified caller with PermSystemConfig. The
// endpoint exists so the OWNER can attach a profile to a bug report, and only
// an admin files those.
func GetSystemConfigDebug(ctx echo.Context) error {
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	array := service.MyService.System().GetSystemConfigDebug()
	disk := service.MyService.System().GetDiskInfo()
	sys := service.MyService.System().GetSysInfo()
	version := service.MyService.Casa().GetCasaosVersion()
	var bugContent string = fmt.Sprintf(`
	 - OS: %s
	 - CasaOS Version: %s
	 - Disk Total: %v 
	 - Disk Used: %v 
	 - System Info: %s
	 - Remote Version: %s
	 - Browser: $Browser$ 
	 - Version: $Version$
`, sys.OS, common.VERSION, disk.Total>>20, disk.Used>>20, array, version.Version)

	//	array = append(array, fmt.Sprintf("disk,total:%v,used:%v,UsedPercent:%v", disk.Total>>20, disk.Used>>20, disk.UsedPercent))

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: bugContent})
}

// @Summary get casaos server port
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/port [get]
func GetCasaOSPort(ctx echo.Context) error {
	// Not routed today (route/v1.go has the registration commented out), but
	// gated anyway: the listening port of the backend is internal topology,
	// and a handler that is one uncommented line away from being public should
	// not be the thing that decides it is harmless.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    config.ServerInfo.HttpPort,
		})
}

// @Summary edit casaos server port
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Param port json string true "port"
// @Success 200 {string} string "ok"
// @Router /sys/port [put]
func PutCasaOSPort(ctx echo.Context) error {
	// Also unrouted today. It rewrites casanova.conf and moves the backend's
	// listening port, which is a system-config mutation by any reading.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	portStr := json["port"]
	portNumber, err := strconv.Atoi(portStr)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: err.Error(),
			})
	}

	isAvailable := port.IsPortAvailable(portNumber, "tcp")
	if !isAvailable {
		return ctx.JSON(common_err.SERVICE_ERROR,
			model.Result{
				Success: common_err.PORT_IS_OCCUPIED,
				Message: common_err.GetMsg(common_err.PORT_IS_OCCUPIED),
			})
	}
	service.MyService.System().UpSystemPort(strconv.Itoa(portNumber))
	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
		})
}

// @Summary active killing casaos
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/restart [post]
func PostKillCasaOS(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	// Trigger a graceful restart by sending SIGTERM to ourselves.
	// The signal handler in main.go will drain connections and shut down cleanly.
	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		p.Signal(os.Interrupt)
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "CasaNova is restarting...",
	})
}

// @Summary get system hardware info
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/hardware/info [get]
// dmiRead pulls a single /sys/class/dmi/id/<name> value if readable.
// Returns "" if the file is missing (no DMI on this host — common on
// Raspberry Pi and other ARM SoCs) or unreadable. Trims trailing
// newlines because the DMI files all end in \n.
func dmiRead(name string) string {
	b, err := os.ReadFile("/sys/class/dmi/id/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func GetSystemHardwareInfo(ctx echo.Context) error {
	// Board vendor, product name, BIOS version, CPU model and drive model —
	// the DMI identity of the machine. Enough to look up the firmware's known
	// vulnerabilities before touching it, and it was readable by anyone who
	// could reach the tunnel. Any signed-in principal may see it (the Server
	// card renders it for every role), nobody anonymous may.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	data := make(map[string]string, 8)
	data["drive_model"] = service.MyService.System().GetDeviceTree()
	data["arch"] = runtime.GOARCH

	if cpu := service.MyService.System().GetCpuInfo(); len(cpu) > 0 {
		data["cpu_model"] = cpu[0].ModelName
	}

	// Mainboard fields — only populated when DMI is present (x86 systems
	// and a handful of ARM dev boards). The hardware-detection layer in
	// the frontend (matchBoard) was wired up but the prior backend never
	// returned these fields, so it dead-coded to null on every host.
	if v := dmiRead("board_name"); v != "" {
		data["board_name"] = v
	}
	if v := dmiRead("board_vendor"); v != "" {
		data["board_vendor"] = v
	}
	if v := dmiRead("product_name"); v != "" {
		data["product_name"] = v
	}
	if v := dmiRead("sys_vendor"); v != "" {
		data["sys_vendor"] = v
	}
	if v := dmiRead("bios_version"); v != "" {
		data["bios_version"] = v
	}

	return ctx.JSON(common_err.SUCCESS,
		model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    data,
		})
}

// @Summary system utilization
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/utilization [get]
func GetSystemUtilization(ctx echo.Context) error {
	// The dashboard's 2 s poll. Its payload is not only percentages: it
	// embeds the physical NICs with their IPv4 addresses and MAC addresses,
	// the system disk's size and free space, uptime and load. The MAC/IP pair
	// alone identifies the hardware and the internal network to an anonymous
	// caller. Identified + PermSystemRead keeps every logged-in role's widgets
	// working (user and guest both hold it) and drops the tokenless reader.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	data := make(map[string]interface{})
	cpu := service.MyService.System().GetCpuPercent()
	num := service.MyService.System().GetCpuCoreNum()
	cpuModel := "arm"
	if cpu := service.MyService.System().GetCpuInfo(); len(cpu) > 0 {
		if strings.Count(strings.ToLower(strings.TrimSpace(cpu[0].ModelName)), "intel") > 0 {
			cpuModel = "intel"
		} else if strings.Count(strings.ToLower(strings.TrimSpace(cpu[0].ModelName)), "amd") > 0 {
			cpuModel = "amd"
		}
	}
	cpuData := make(map[string]interface{})
	cpuData["percent"] = cpu
	cpuData["num"] = num
	cpuData["temperature"] = service.MyService.System().GetCPUTemperature()
	cpuData["power"] = service.MyService.System().GetCPUPower()
	cpuData["model"] = cpuModel

	data["cpu"] = cpuData
	data["mem"] = service.MyService.System().GetMemInfo()

	data["net"] = buildNetInfo(liveNetInfoSources())

	// Uptime (seconds since boot) — the Server card renders "Tiempo activo".
	//
	// Computed from a boot-time reference captured once, because the obvious
	// call — GetSysInfo().Uptime — runs gopsutil's full host.Info(): a /proc
	// sweep plus distro re-detection, every 3 s, on the most-polled endpoint
	// of the dashboard, to answer a question whose only moving part is the
	// wall clock.
	data["uptime"] = uptimeSeconds()

	// Load averages (1/5/15 min) — the Uptime widget renders these. Read
	// straight from /proc/loadavg (cheap, no extra dependency). Absent on
	// non-Linux hosts, in which case the field is simply omitted.
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			l1, _ := strconv.ParseFloat(f[0], 64)
			l5, _ := strconv.ParseFloat(f[1], 64)
			l15, _ := strconv.ParseFloat(f[2], 64)
			data["loadavg"] = []float64{l1, l5, l15}
		}
	}

	// Disk info — the frontend StorageWidget reads sysDisk from this endpoint.
	disk := service.MyService.System().GetDiskInfo()
	data["sysDisk"] = map[string]interface{}{
		"used":   disk.Used,
		"size":   disk.Total,
		"avail":  disk.Free,
		"health": true,
	}

	systemMap := service.MyService.Notify().GetSystemTempMap()
	systemMap.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if entry, ok := value.(service.TempEntry); ok {
				data[k] = entry.Value
			} else {
				data[k] = value
			}
		}
		return true
	})
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// @Summary get cpu info
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/cpu [get]
func GetSystemCupInfo(ctx echo.Context) error {
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	cpu := service.MyService.System().GetCpuPercent()
	num := service.MyService.System().GetCpuCoreNum()
	data := make(map[string]interface{})
	data["percent"] = cpu
	data["num"] = num
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// @Summary get mem info
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/mem [get]
func GetSystemMemInfo(ctx echo.Context) error {
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	mem := service.MyService.System().GetMemInfo()
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: mem})
}

// @Summary get disk info
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/disk [get]
func GetSystemDiskInfo(ctx echo.Context) error {
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	disk := service.MyService.System().GetDiskInfo()
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: disk})
}

// @Summary get Net info
// @Produce  application/json
// @Accept application/json
// @Tags sys
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /sys/net [get]
func GetSystemNetInfo(ctx echo.Context) error {
	// Was requirePerm(PermSystemRead), which was NOT an authentication
	// boundary — guest holds PermSystemRead and a tokenless caller resolves to
	// guest — so this payload (per-NIC IPv4 and MAC addresses) was readable
	// anonymously. requireSystemPrincipal adds the missing half: a validated
	// token first, the permission second. The amplification property is still
	// the structural one documented below (a constant number of directory
	// listings per request), not this check.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: buildNetInfo(liveNetInfoSources())})
}

// netInfoSources is the set of system probes the network payload needs.
//
// It exists so the builder can be exercised with counting fakes: the property
// worth pinning is HOW MANY times each probe runs, not how long a request
// takes. A wall-clock budget would be a flaky test; a call count is exact.
type netInfoSources struct {
	// counters enumerates every interface the kernel accounts for in
	// /proc/net/dev — including one veth per running container.
	counters func() []psnet.IOCountersStat
	// physicalNICs lists the physical interface names. Each call costs two
	// os.ReadDir over /sys plus a sort, so it must be called ONCE per
	// request, never once per counters() entry.
	physicalNICs func() []string
	state        func(name string) string
	detail       func(name string) (ipv4 string, mac string, speed int)
}

func liveNetInfoSources() netInfoSources {
	sys := service.MyService.System()
	return netInfoSources{
		counters:     sys.GetNetInfo,
		physicalNICs: func() []string { return sys.GetNet(true) },
		state:        sys.GetNetState,
		detail:       sys.GetNetDetail,
	}
}

// buildNetInfo turns the kernel's per-interface counters into the payload both
// /v1/sys/utilization and /v1/sys/net return.
//
// The shape it enforces: physicalNICs() is called exactly once and its result
// is turned into a set, so the work per request is O(number of interfaces)
// with a CONSTANT number of directory listings. The previous /v1/sys/net
// version called physicalNICs() inside the loop over counters(), i.e. two
// os.ReadDir over /sys for every veth of every running container — measured at
// 128 os.ReadDir (6.0ms) against the 2 (0.13ms) its already-hoisted sibling in
// /v1/sys/utilization did, and growing with the container count. Sharing one
// builder is what keeps the two from drifting apart again.
//
// state() and detail() are still per-interface: they take a name, so they can
// only be called for interfaces that made the cut (the physical ones, ~1-3
// here), not once per /proc/net/dev entry.
//
// Output order and item fields are unchanged: counters() order is preserved
// and each entry still yields at most one item.
func buildNetInfo(src netInfoSources) []model.IOCountersStat {
	physical := src.physicalNICs()
	isPhysical := make(map[string]struct{}, len(physical))
	for _, name := range physical {
		// GetNet appends an empty sentinel that the old `n.Name == name`
		// comparison could never match; keep it unmatchable.
		if name == "" {
			continue
		}
		isPhysical[name] = struct{}{}
	}

	newNet := []model.IOCountersStat{}
	now := time.Now().Unix()
	for _, n := range src.counters() {
		if _, ok := isPhysical[n.Name]; !ok {
			continue
		}
		item := model.IOCountersStat{
			Name:        n.Name,
			BytesSent:   n.BytesSent,
			BytesRecv:   n.BytesRecv,
			PacketsSent: n.PacketsSent,
			PacketsRecv: n.PacketsRecv,
		}
		item.State = strings.TrimSpace(src.state(n.Name))
		item.Time = now
		item.IPv4, item.MAC, item.Speed = src.detail(n.Name)
		newNet = append(newNet, item)
	}
	return newNet
}

// GetSystemProxy relays one upstream GET on behalf of the caller.
//
// Caller, budget, destination and response are all decided in
// outbound_fetch.go — see that file for why a permission check alone does not
// remove the anonymous caller, and why the upstream Content-Type is never
// echoed back at this origin. What used to be here was
// destination hardening only: unauthenticated callers from the internet could
// make the box fetch arbitrary URLs and got the bytes back under the upstream's
// own Content-Type at CasaNova's origin (same-origin XSS + open proxy).
func GetSystemProxy(ctx echo.Context) error {
	// Caller gate runs BEFORE any client-supplied input is parsed: an
	// anonymous request must not reach the URL parser or the resolver,
	// let alone a socket.
	if err := requireOutboundFetchCaller(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	release, err := acquireOutboundSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	target, err := validateOutboundURL(ctx.QueryParam("url"))
	if err != nil {
		return err
	}

	reqCtx, reqCancel := context.WithTimeout(ctx.Request().Context(), outboundFetchTimeout)
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: "invalid url"})
	}
	resp, err := outboundSafeClient.Do(req)
	if err != nil {
		// Deliberately generic: the dialer's error names the resolved
		// address it refused, which would turn this into an internal-network
		// oracle for whoever is allowed to call it.
		return echo.NewHTTPError(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "upstream request failed"})
	}
	defer resp.Body.Close()

	return writeInertUpstreamResponse(ctx, resp, outboundMaxResponseBytes)
}

// PostTroubleshoot runs a whitelisted repair action. Each action maps
// to one or more systemctl restart commands. The set of allowed actions
// is hardcoded — no arbitrary service names from the client.
func PostTroubleshoot(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	action := ctx.Param("action")

	// Map action names to the ordered list of services to restart.
	actionMap := map[string][]string{
		"fix-docker-network": {"docker"},
		"restart-gateway":    {"casanova-gateway"},
		"restart-all": {
			"casanova-message-bus",
			"casanova-user-service",
			"casanova-local-storage",
			"casanova-app-management",
			"casanova-job-management",
			"casanova",
			"casanova-gateway",
		},
		"flush-dns": {}, // handled separately
	}

	services, ok := actionMap[action]
	if !ok {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: fmt.Sprintf("unknown troubleshoot action: %s", action),
		})
	}

	// Special case: flush DNS caches
	if action == "flush-dns" {
		cmd := exec2.Command("resolvectl", "flush-caches")
		if out, err := cmd.CombinedOutput(); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: fmt.Sprintf("flush-dns failed: %s (%s)", string(out), err.Error()),
			})
		}
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "DNS cache flushed",
		})
	}

	var errors []string
	for _, svc := range services {
		if err := service.MyService.System().RestartService(svc); err != nil {
			errors = append(errors, err.Error())
		}
	}

	if len(errors) > 0 {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: strings.Join(errors, "; "),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: fmt.Sprintf("Action '%s' completed successfully", action),
	})
}

func PutSystemState(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	state := ctx.Param("state")
	if strings.ToLower(state) == "off" {
		service.MyService.System().SystemShutdown()
	} else if strings.ToLower(state) == "restart" {
		service.MyService.System().SystemReboot()
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: "The operation will be completed shortly."})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  type query string true " udp/tcp"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/getport [get]
func GetPort(ctx echo.Context) error {
	// GET /v1/port — the app-install wizard's "suggest a free port". The
	// /v1/port prefix is published to the gateway (the shared route manifest),
	// so before this gate it answered anonymous callers from the tunnel and
	// told them which ports the host has free — the inverse of a port scan,
	// delivered by the box itself, with no packet ever sent to the scanned
	// ports. Any signed-in principal may ask; nobody anonymous may.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	t := utils.DefaultQuery(ctx, "type", "tcp")
	var p int
	// Try up to 50 times to find an available port. Without a limit, this
	// loops forever if all ports are exhausted.
	for i := 0; i < 50; i++ {
		p, _ = port.GetAvailablePort(t)
		if port.IsPortAvailable(p, t) {
			return ctx.JSON(common_err.SUCCESS, &model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: p})
		}
	}
	return ctx.JSON(http.StatusServiceUnavailable, &model.Result{Success: common_err.SERVICE_ERROR, Message: "no available ports found"})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  port path int true ""
// @Param  type query string true " udp/tcp"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/check/{port} [get]
func PortCheck(ctx echo.Context) error {
	// GET /v1/port/state/:port — same family as GetPort and the sharper half:
	// it answers "is port N in use on this host" for any port the caller
	// names, one request each. That is a full loopback-visible port scan of
	// services that never face the network, so it needs a signed-in caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	p, _ := strconv.Atoi(ctx.Param("port"))
	if p < 1 || p > 65535 {
		return ctx.JSON(http.StatusBadRequest, &model.Result{Success: common_err.CLIENT_ERROR, Message: "port must be between 1 and 65535"})
	}
	t := utils.DefaultQuery(ctx, "type", "tcp")
	return ctx.JSON(common_err.SUCCESS, &model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: port.IsPortAvailable(p, t)})
}

func GetSystemEntry(ctx echo.Context) error {
	// GET /v1/sys/entry concatenates every module's entry.json under
	// /var/lib/casaos/www/modules — the installed-module inventory with each
	// module's declared entry point. An installed-software list is the first
	// thing an attacker wants and the dashboard is the only consumer, so it
	// needs a signed-in reader.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	entry := service.MyService.System().GetSystemEntry()
	str := json.RawMessage(entry)
	if !gjson.ValidBytes(str) {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: entry, Data: json.RawMessage("[]")})
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: str})
}

// uptimeSeconds derives uptime from a boot instant captured on first use.
// Boot time does not change while the process lives; only the clock does.
var uptimeOnce sync.Once
var bootedAt time.Time

func uptimeSeconds() uint64 {
	uptimeOnce.Do(func() {
		up := service.MyService.System().GetSysInfo().Uptime // the one expensive call
		bootedAt = time.Now().Add(-time.Duration(up) * time.Second)
	})
	return uint64(time.Since(bootedAt) / time.Second)
}
