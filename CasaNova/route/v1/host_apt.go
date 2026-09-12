package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
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

// Host APT management: check for OS package updates (read-only — it refreshes
// the package lists and SIMULATES the upgrade, installing nothing) and apply
// them from the UI. The upgrade runs as a transient systemd SERVICE (never
// --scope) so it escapes casanova.service's ProtectSystem=full sandbox (dpkg
// writes /usr and /etc), survives casanova restarting mid-upgrade, and logs to
// a file instead of the service's journal fd (the SIGPIPE lesson). The API
// never accepts a package name — the whole class of injection is designed out.

const (
	aptUpgradeUnit = "casanova-apt-upgrade"
	// The transient unit runs with --collect (so the fixed name is always
	// reusable), which garbage-collects the unit on exit — erasing its
	// ActiveState/ExecMainStatus. A collected SUCCESS and a collected FAILURE
	// are then indistinguishable via `systemctl show`. So we capture the real
	// exit code in this file via a wrapper and read IT for success/failed.
	aptUpgradeStatusFile = "/var/lib/casanova/logs/apt-upgrade.status"
	aptCheckMax          = 3 * time.Minute
)

// aptUpgradeLog and aptCacheFile are vars (not consts) so a test can point
// them at a temp file instead of the real /var/lib/casanova paths.
var (
	aptUpgradeLog = "/var/lib/casanova/logs/apt-upgrade.log"
	aptCacheFile  = "/var/lib/casanova/apt-status.json"
)

// launchAptUnit starts a privileged apt/unattended-upgrade command as a
// transient one-shot service. It wraps the (constant) command so the child's
// exit code is recorded to aptUpgradeStatusFile, which survives --collect and
// lets GetAptUpgradeStatus report success vs failed reliably. The inner command
// is a compile-time constant — no client input — so /bin/bash -c is safe here.
// aptUnitArgs builds the systemd-run invocation. Split out so a test can pin
// its one load-bearing flag without needing systemd.
//
// --no-block is that flag. Without it, systemd-run waits for the start job to
// complete — and for Type=oneshot the start job IS the command, so a call
// meant to fire-and-forget instead blocked for apt's full runtime. The
// handler above answered "Upgrade started" only after the upgrade had
// finished; the browser client, whose timeout is 10 s, gave up at 10 and told
// the user the upgrade "could not be started" while apt kept running to a
// successful finish behind the error toast. (Measured on the box: a 6 s
// oneshot returns in 6 s without the flag, 0 s with it.)
//
// Launch errors are not silenced by --no-block: enqueueing the job — name
// collision with a unit already running, invalid property — still fails
// synchronously, which is what keeps the "already running" guard honest.
func aptUnitArgs(description, innerCmd string) []string {
	wrapped := innerCmd + "; ec=$?; echo $ec > " + aptUpgradeStatusFile + "; exit $ec"
	return []string{
		"--no-block",
		"--unit=" + aptUpgradeUnit,
		"--description=" + description,
		"--collect",
		"--property=Type=oneshot",
		"--setenv=DEBIAN_FRONTEND=noninteractive",
		"--property=StandardOutput=append:" + aptUpgradeLog,
		"--property=StandardError=append:" + aptUpgradeLog,
		"--",
		"/bin/bash", "-c", wrapped,
	}
}

func launchAptUnit(description, innerCmd string) ([]byte, error) {
	os.MkdirAll("/var/lib/casanova/logs", 0o755)
	os.Remove(aptUpgradeStatusFile) // drop the previous run's result
	cmd := exec.Command("systemd-run", aptUnitArgs(description, innerCmd)...)
	return cmd.CombinedOutput()
}

var (
	aptCheckRunning bool
	aptCheckMu      sync.Mutex
)

type AptPackage struct {
	Name           string `json:"name"`
	CurrentVersion string `json:"currentVersion"`
	NewVersion     string `json:"newVersion"`
	Security       bool   `json:"security"`
}

type AptStatus struct {
	Supported bool   `json:"supported"` // false on non-apt distros
	CheckedAt string `json:"checkedAt"` // RFC3339, "" if never
	Checking  bool   `json:"checking"`  // a check is in flight
	Stale     bool   `json:"stale"`     // NOTHING could be refreshed (lock, network, no repo answered)
	// FailedRepos names the repositories that did not refresh, when SOME did.
	// A single dead third-party PPA makes `apt-get update` exit non-zero even
	// though every other repository updated fine; reporting that as a blanket
	// "lists could not be refreshed" tells the administrator nothing they can
	// act on, and hides that the package data is in fact current. Naming the
	// offender is the difference between a usable panel and an alarm.
	FailedRepos    []string     `json:"failedRepos,omitempty"`
	Total          int          `json:"total"`
	Security       int          `json:"security"`
	Packages       []AptPackage `json:"packages"`
	RebootRequired bool         `json:"rebootRequired"`
	RebootPackages []string     `json:"rebootPackages"`
}

// GET /v1/sys/host/apt/updates — the cached result of the last check.
func GetAptUpdates(ctx echo.Context) error {
	// requireSystemPrincipal: see route/v1/antivirus.go's GetAntivirusStatus
	// comment — guest holds PermSystemRead, so a bare requirePerm never
	// refused a tokenless caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	st := readAptCache()
	if _, err := exec.LookPath("apt-get"); err != nil {
		st.Supported = false
	} else {
		st.Supported = true
	}
	aptCheckMu.Lock()
	st.Checking = aptCheckRunning
	aptCheckMu.Unlock()
	st.RebootRequired, st.RebootPackages = rebootRequired()
	// Packages carries, per row, the INSTALLED version and whether the update
	// is a security one: an exact, machine-readable map of which known-
	// vulnerable versions this box is running right now. That is
	// reconnaissance, and its twin GetAptUpgradeStatus already draws the line
	// in this same place — coarse state for any authenticated caller, the
	// privileged CONTENT for PermSystemConfig.
	//
	// Total and Security (the COUNTS) deliberately stay: they say "this box is
	// behind" without naming a target, which is exactly the needs-attention
	// indicator the non-admin card renders. Note the counts cannot be the only
	// thing hidden — Security is len(rows where security), so publishing the
	// rows and hiding the tally would not be a control at all.
	//
	// FailedRepos is the inventory of this box's (often private) third-party
	// sources, and only an admin can act on it: PostAptCheck is
	// PermSystemConfig. It carries no credential — apt's own URI printer
	// strips userinfo before it reaches the Err: line we parse.
	//
	// Packages MUST serialize as [] and never null: the tag has no omitempty,
	// and AptUpdateCard.vue's dockerInUpgrade computed reads
	// apt.packages.some(...) with no optional chaining on /app/settings/updates
	// — a route that is NOT admin-only. A null there turns this security fix
	// into a client-side crash for every non-admin viewer.
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermSystemConfig) {
		st.Packages = []AptPackage{}
		st.FailedRepos = nil
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    st,
	})
}

// POST /v1/sys/host/apt/check — refresh lists + simulate upgrade in the
// background. Returns immediately; poll GET /apt/updates for the result.
func PostAptCheck(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "apt is not available on this system",
		})
	}

	aptCheckMu.Lock()
	if aptCheckRunning {
		aptCheckMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{Success: common_err.SERVICE_ERROR, Message: "A check is already in progress"})
	}
	aptCheckRunning = true
	aptCheckMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in apt check goroutine", zap.Any("panic", r))
			}
		}()
		runCtx, cancel := context.WithTimeout(context.Background(), aptCheckMax)
		defer cancel()
		defer func() {
			aptCheckMu.Lock()
			aptCheckRunning = false
			aptCheckMu.Unlock()
		}()
		runAptCheck(runCtx)
	}()

	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: "Check started"})
}

// runAptCheck refreshes the lists (best-effort; a lock held by unattended-
// upgrades just means we simulate against existing lists and mark the result
// stale) and parses the simulated upgrade. Argv is 100% constant.
func runAptCheck(ctx context.Context) {
	st := AptStatus{Supported: true, CheckedAt: time.Now().UTC().Format(time.RFC3339)}

	upd := exec.CommandContext(ctx, "apt-get", "update", "-o", "DPkg::Lock::Timeout=10")
	// LC_ALL=C pins apt's output to English. The parsing below keys off "Hit:",
	// "Get:" and "Err:", and this box runs a Spanish locale where those become
	// "Obj:" and "Err:" — a localised daemon would silently parse nothing and
	// report every refresh as a total failure.
	upd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C", "LANG=C")
	updOut, updErr := upd.CombinedOutput()
	if updErr != nil {
		refreshed, failed := parseAptUpdateOutput(string(updOut))
		st.FailedRepos = failed
		// Only a refresh where NOTHING came back is stale. If some repositories
		// answered, the package lists are current for those and the simulation
		// below is trustworthy; flagging it stale would cry wolf every time a
		// third-party PPA goes 404.
		st.Stale = refreshed == 0
	}

	sim := exec.CommandContext(ctx, "apt-get", "-s", "-q", "-o", "DPkg::Lock::Timeout=10", "dist-upgrade")
	sim.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := sim.Output()
	if err != nil {
		// Keep the previous cache rather than clobbering it with nothing.
		logger.Info("apt dist-upgrade simulation failed", zap.Error(err))
		st.Packages = readAptCache().Packages
	} else {
		st.Packages = parseAptInstLines(string(out))
	}
	st.Total = len(st.Packages)
	for _, p := range st.Packages {
		if p.Security {
			st.Security++
		}
	}
	st.RebootRequired, st.RebootPackages = rebootRequired()
	writeAptCache(st)
}

// parseAptUpdateOutput counts the repositories that answered and names the ones
// that did not. Requires apt to have run under LC_ALL=C.
//
//	Hit:1 http://ar.archive.ubuntu.com/ubuntu noble InRelease
//	Err:12 https://ppa.launchpadcontent.net/jonathonf/ffmpeg-4/ubuntu noble Release
//	  404  Not Found [IP: 185.125.189.188 443]
func parseAptUpdateOutput(out string) (refreshed int, failed []string) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Hit:"), strings.HasPrefix(line, "Get:"):
			refreshed++
		case strings.HasPrefix(line, "Err:"):
			// "Err:12 <url> <suite> <file>" — keep the URL, which is what tells
			// the administrator which source to fix or remove.
			fields := strings.Fields(line)
			if len(fields) >= 2 && !seen[fields[1]] {
				seen[fields[1]] = true
				failed = append(failed, fields[1])
			}
		}
	}
	return refreshed, failed
}

// parseAptInstLines pulls "Inst" lines out of `apt-get -s dist-upgrade`:
//
//	Inst libfoo [1.0-1] (1.0-2 Ubuntu:24.04/noble-updates [amd64])
//
// Security is inferred from the origin archive containing "-security".
func parseAptInstLines(out string) []AptPackage {
	var pkgs []AptPackage
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		p := AptPackage{Name: fields[1]}
		// [oldver] is the 3rd field when a package is being upgraded (absent
		// for a brand-new install pulled in as a dependency).
		if len(fields) >= 3 && strings.HasPrefix(fields[2], "[") {
			p.CurrentVersion = strings.Trim(fields[2], "[]")
		}
		// New version is the first token inside the parentheses.
		if i := strings.Index(line, "("); i >= 0 {
			inner := line[i+1:]
			if j := strings.Index(inner, ")"); j >= 0 {
				inner = inner[:j]
			}
			if parts := strings.Fields(inner); len(parts) >= 1 {
				p.NewVersion = parts[0]
			}
		}
		p.Security = strings.Contains(strings.ToLower(line), "-security")
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// POST /v1/sys/host/apt/upgrade — apply all available updates via a transient
// systemd service. strictJWT + PermSystemConfig gate it (see route wiring).
func PostAptUpgrade(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{Success: common_err.SERVICE_ERROR, Message: "apt is not available"})
	}
	// Refuse to start a second upgrade while one is active.
	if s := showUnitProp(aptUpgradeUnit+".service", "ActiveState"); s == "active" || s == "activating" {
		return ctx.JSON(http.StatusConflict, model.Result{Success: common_err.SERVICE_ERROR, Message: "An upgrade is already running"})
	}

	// Transient SERVICE (not --scope): fresh namespace so dpkg can write /usr
	// and /etc, own cgroup so it survives if apt restarts casanova, own log
	// file so a closed journal fd can't SIGPIPE it. confold noninteractive
	// answers every prompt without a TTY.
	if out, err := launchAptUnit("CasaNova APT upgrade",
		"apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=60 upgrade"); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to launch upgrade: " + strings.TrimSpace(string(out)),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: "Upgrade started"})
}

type AptUpgradeStatus struct {
	State          string   `json:"state"` // idle|running|success|failed
	ExitCode       int      `json:"exitCode"`
	Tail           []string `json:"tail"` // last log lines
	RebootRequired bool     `json:"rebootRequired"`
	RebootPackages []string `json:"rebootPackages"`
}

// GET /v1/sys/host/apt/upgrade/status — poll the transient unit + log tail.
func GetAptUpgradeStatus(ctx echo.Context) error {
	// requireSystemPrincipal: see GetAptUpdates above.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	// Tail defaults to an empty (non-nil) slice, not the zero value's nil:
	// the JSON tag has no omitempty, so a nil slice serializes as `"tail":
	// null` and the UI reads `upgrade.tail.length` unconditionally once state
	// isn't idle — a null there is a client-side crash, not a hidden field.
	st := AptUpgradeStatus{State: "idle", Tail: []string{}}
	unit := aptUpgradeUnit + ".service"
	active := showUnitProp(unit, "ActiveState")
	if active == "active" || active == "activating" || active == "deactivating" || active == "reloading" {
		st.State = "running"
	} else if raw, err := os.ReadFile(aptUpgradeStatusFile); err == nil {
		// The wrapper recorded the real child exit code, which survives
		// --collect (systemctl's ExecMainStatus does not). Absent file +
		// inactive unit ⇒ genuinely idle (never ran, or reset at next launch).
		code, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		st.ExitCode = code
		if code == 0 {
			st.State = "success"
		} else {
			st.State = "failed"
		}
	}
	st.RebootRequired, st.RebootPackages = rebootRequired()
	// Tail is the raw stdout/stderr of a privileged apt-get run: it names
	// every package and exact version the upgrade touched — the same
	// reconnaissance value as AptStatus.Packages above (line ~141), which
	// gets PermSystemConfig for the identical reason: an exact, machine-
	// readable map of which known-vulnerable versions this box just ran.
	//
	// It is NOT gated because the log could carry embedded Basic Auth
	// credentials from a private repo URL — it can't. Reproduced directly:
	// `apt-get update` against a source URL with a userinfo component
	// (https://user:pass@host/...) never printed the credentials in any
	// Ign:/Err:/W: line; apt's own URI printer strips userinfo before it
	// reaches output, same behavior FailedRepos above already relies on.
	// Triggering an upgrade already requires PermSystemConfig (PostAptUpgrade
	// above); reading the full raw output of that privileged run should
	// require the same tier, not the broader PermSystemRead this handler is
	// gated on for its coarse status. A merely authenticated, non-admin
	// caller still gets state/exit code/reboot info — enough for a "system
	// needs attention" indicator — without the raw log.
	if rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermSystemConfig) {
		st.Tail = tailFile(aptUpgradeLog, 40)
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: st})
}

// ── helpers ──────────────────────────────────────────────────────────────

func readAptCache() AptStatus {
	st := AptStatus{Supported: true, Packages: []AptPackage{}}
	if raw, err := os.ReadFile(aptCacheFile); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Packages == nil {
		st.Packages = []AptPackage{}
	}
	return st
}

// writeAptCache publishes by rename: the background check goroutine writes
// this file while GET /apt/updates reads it, and WriteFile's truncate-then-
// write let a poll land mid-write, fail to parse, and silently render the
// panel as "Total=0, no updates" while the check was still running. Rename
// makes each read see either the old snapshot or the new one, never half.
func writeAptCache(st AptStatus) {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll("/var/lib/casanova", 0o755)
	tmp := aptCacheFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, aptCacheFile)
}

// rebootRequired reads the Debian/Ubuntu marker files.
func rebootRequired() (bool, []string) {
	if !fileExists("/var/run/reboot-required") && !fileExists("/run/reboot-required") {
		return false, nil
	}
	var pkgs []string
	for _, p := range []string{"/var/run/reboot-required.pkgs", "/run/reboot-required.pkgs"} {
		if raw, err := os.ReadFile(p); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					pkgs = append(pkgs, l)
				}
			}
			break
		}
	}
	return true, pkgs
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ── Unattended security upgrades (2.3) ───────────────────────────────────

const autoUpgradesConf = "/etc/apt/apt.conf.d/20auto-upgrades"

// Constant, embedded content — the ONLY thing we ever write into /etc/apt, and
// it never contains client input. Enables the daily update + the security
// upgrade channel.
const autoUpgradesBody = `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::Download-Upgradeable-Packages "1";
APT::Periodic::AutocleanInterval "7";
`

type AptAutoStatus struct {
	Supported bool `json:"supported"` // unattended-upgrades installed
	Enabled   bool `json:"enabled"`   // conf present + timers active
}

// GET /v1/sys/host/apt/auto — unattended-upgrades status.
func GetAptAuto(ctx echo.Context) error {
	// requireSystemPrincipal: see GetAptUpdates above.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	st := AptAutoStatus{}
	_, err := exec.LookPath("unattended-upgrade")
	st.Supported = err == nil
	// Enabled = the conf turns Unattended-Upgrade on AND the timer is active.
	if raw, err := os.ReadFile(autoUpgradesConf); err == nil {
		if strings.Contains(string(raw), `Unattended-Upgrade "1"`) {
			if showUnitProp("apt-daily-upgrade.timer", "ActiveState") == "active" {
				st.Enabled = true
			}
		}
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: st})
}

// PUT /v1/sys/host/apt/auto  body: {"enabled":true}
func PutAptAuto(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: err.Error()})
	}
	if body.Enabled {
		// Materialise the constant conf in our writable staging dir, then let
		// PID 1 install it into /etc (RO for us). Content is constant → safe.
		os.MkdirAll("/var/lib/casanova/staging", 0o755)
		stage := "/var/lib/casanova/staging/20auto-upgrades"
		if err := os.WriteFile(stage, []byte(autoUpgradesBody), 0o644); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
		if out, err := exec.Command("systemd-run", "--collect", "--wait", "--property=Type=oneshot",
			"--", "/usr/bin/install", "-m644", stage, autoUpgradesConf).CombinedOutput(); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "install conf: " + strings.TrimSpace(string(out))})
		}
		if out, err := exec.Command("systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer").CombinedOutput(); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "enable timers: " + strings.TrimSpace(string(out))})
		}
	} else {
		if out, err := exec.Command("systemctl", "disable", "--now", "apt-daily-upgrade.timer").CombinedOutput(); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: strings.TrimSpace(string(out))})
		}
	}
	return GetAptAuto(ctx)
}

// POST /v1/sys/host/apt/security-now — run `unattended-upgrade` immediately as
// a transient service (same safety model as the full upgrade). Applies ONLY the
// security channel — never `apt install <pkg>`.
func PostAptSecurityNow(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("unattended-upgrade"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{Success: common_err.SERVICE_ERROR, Message: "unattended-upgrades is not installed"})
	}
	if s := showUnitProp(aptUpgradeUnit+".service", "ActiveState"); s == "active" || s == "activating" {
		return ctx.JSON(http.StatusConflict, model.Result{Success: common_err.SERVICE_ERROR, Message: "An upgrade is already running"})
	}
	// Reuse the apt-upgrade unit name + log + status file so the existing
	// status poller (GET /apt/upgrade/status) tracks this too.
	if out, err := launchAptUnit("CasaNova security upgrade", "unattended-upgrade -v"); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: "launch: " + strings.TrimSpace(string(out))})
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: "Security upgrade started"})
}

func tailFile(path string, lines int) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all
}
