package service

// power_ladder.go — P1b: the power-saving ladder. On a fresh UPS
// `prepare_shutdown` advice the orchestrator applies cheap draw-reducing
// levers (CPU governor → powersave, EPP → power, spin down configured HDDs,
// optionally docker-pause non-essential apps) and reports each one to Forza
// via RecordAction so its estimator learns the real per-lever saving. When
// mains power returns (stable `normal`) every lever is reverted in reverse
// order. Everything is best-effort and opt-in (cfg.SavingsLadder): a failed
// lever is logged and skipped, and the ladder NEVER interferes with the
// ordered-shutdown safety path.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/shirou/gopsutil/v3/cpu"
	"go.uber.org/zap"
)

const (
	// driverInternalSocket is the casanova-driver 0600 unix socket for
	// sibling root services (inside a 0700 root-only dir). Keep in sync with
	// the server constant in CasaNova-Driver/route/v2/internal_socket.go.
	driverInternalSocket = "/run/casanova/internal/driver.sock"
	// leverSettle is how long to wait after a lever before sampling watts for
	// RecordAction — long enough for one peer report (5s cadence on battery)
	// to carry each lever's last_action to Forza.
	leverSettle = 10 * time.Second
)

// ladderStateFile survives a service restart mid-event so leftover levers (a
// paused container, a powersave governor) can be reverted on boot. Var so
// tests can point it at a temp dir.
var ladderStateFile = "/var/lib/casanova/power-ladder-state.json"

// LadderState is what the ladder has applied and how to undo it. Persisted to
// ladderStateFile after every mutation.
type LadderState struct {
	Active       bool     `json:"active"`
	Levers       []string `json:"levers"`                 // applied, in order
	PrevGovernor string   `json:"prevGovernor,omitempty"` // "" = not changed
	PrevEPP      string   `json:"prevEPP,omitempty"`      // "" = not changed
	Paused       []string `json:"paused,omitempty"`       // containers we paused
	StartedAt    string   `json:"startedAt,omitempty"`
}

// LadderView is the read-only summary exposed on GET /v1/sys/host/power.
type LadderView struct {
	Active bool     `json:"active"`
	Levers []string `json:"levers"`
}

// tuningClient is the minimal driver surface the ladder needs — an interface
// so tests can fake the driver without a unix socket.
type tuningClient interface {
	CPUGovernor() (driverGovernorInfo, error)
	SetCPUGovernor(governor string) error
	SetEPP(pref string) error
	StandbyDisk(disk string) error
}

// driverGovernorInfo mirrors the fields of the driver's CPUGovernorInfo that
// the ladder consumes (CasaNova-Driver/service/tuning.go).
type driverGovernorInfo struct {
	Available    []string `json:"available"`
	Current      string   `json:"current"`
	Supported    bool     `json:"supported"`
	EPP          string   `json:"epp"`
	EPPAvailable []string `json:"eppAvailable"`
}

// Container ops as package vars so the ladder test can fake docker.
var (
	listRunningContainers = runningContainerNames
	pauseContainer        = func(name string) error { return dockerSimple("pause", name) }
	unpauseContainer      = func(name string) error { return dockerSimple("unpause", name) }
	ladderSettle          = func() { time.Sleep(leverSettle) }
	// ladderWattsSample measures draw over its OWN 1s window: cpu.Percent with
	// interval>0 takes private snapshots, so — unlike estimateWatts's
	// interval=0 form, which shares gopsutil's package-global last-times with
	// the poller — the ladder's watts_before/after can't be skewed by a
	// concurrent report tick.
	ladderWattsSample = func(cfg PowerConfig) float64 {
		pcts, err := cpu.Percent(time.Second, false)
		load := 0.0
		if err == nil && len(pcts) > 0 {
			load = pcts[0]
		}
		w := cfg.IdleWatts + (cfg.MaxWatts-cfg.IdleWatts)*load/100.0
		return math.Round(w*10) / 10
	}
)

// unpauseGone reports docker-unpause failures that mean the container no
// longer needs unpausing (already unpaused, stopped, or removed) — after a
// host reboot the paused state is gone, so treating these as retryable would
// wedge the ladder Active forever.
func unpauseGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not paused") ||
		strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "not found")
}

// unpauseList unpauses each container, returning only the ones that genuinely
// might still be frozen (transient docker errors).
func unpauseList(names []string) []string {
	var remaining []string
	for _, name := range names {
		if err := unpauseContainer(name); err != nil {
			if unpauseGone(err) {
				logger.Info("ladder: container already unpaused/gone",
					zap.String("container", name), zap.Error(err))
				continue
			}
			logger.Error("ladder: unpause failed", zap.String("container", name), zap.Error(err))
			remaining = append(remaining, name)
		}
	}
	return remaining
}

func dockerSimple(verb, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", verb, name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker %s %s: %v: %s", verb, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── driver client over the internal unix socket ──────────────────────────

type driverSocketClient struct {
	c *http.Client
}

var (
	driverClientOnce sync.Once
	driverClient     *driverSocketClient
)

// defaultDriverClient returns the singleton socket client. Construction never
// dials — an absent socket surfaces as a per-call error the ladder logs.
func defaultDriverClient() *driverSocketClient {
	driverClientOnce.Do(func() {
		dialer := &net.Dialer{}
		driverClient = &driverSocketClient{c: &http.Client{
			// hdparm calls inside the driver are bounded at 20s.
			Timeout: 25 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", driverInternalSocket)
				},
			},
		}}
	})
	return driverClient
}

// call performs a request against the driver over the socket and unwraps the
// standard {success, message, data} envelope into out (out may be nil).
func (d *driverSocketClient) call(method, path string, body, out interface{}) error {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	// The "unix" host is decorative — DialContext ignores it and dials the
	// socket. (Same pattern as CasaNova-Common/external/message_bus.go.)
	req, err := http.NewRequest(method, "http://unix"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		Success int             `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("driver %s: bad response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("driver %s -> %d: %s", path, resp.StatusCode, envelope.Message)
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func (d *driverSocketClient) CPUGovernor() (driverGovernorInfo, error) {
	var info driverGovernorInfo
	err := d.call(http.MethodGet, "/v2/driver/tuning/cpu-governor", nil, &info)
	return info, err
}

func (d *driverSocketClient) SetCPUGovernor(governor string) error {
	// persist=false: ladder changes are transient — the user's boot-reapplied
	// preference in host-tuning.json must never be overwritten by a battery
	// event (a reboot then restores the USER's governor, which is also the
	// crash-recovery path).
	return d.call(http.MethodPut, "/v2/driver/tuning/cpu-governor?persist=false",
		map[string]string{"governor": governor}, nil)
}

func (d *driverSocketClient) SetEPP(pref string) error {
	return d.call(http.MethodPut, "/v2/driver/tuning/epp?persist=false",
		map[string]string{"epp": pref}, nil)
}

func (d *driverSocketClient) StandbyDisk(disk string) error {
	return d.call(http.MethodPost, "/v2/driver/tuning/spindown/standby",
		map[string]string{"disk": disk}, nil)
}

// ── ladder state machine (methods on PowerOrchestrator) ──────────────────

// driverFor returns the injected test client or the real socket client.
func (p *PowerOrchestrator) driverFor() tuningClient {
	if p.driver != nil {
		return p.driver
	}
	return defaultDriverClient()
}

// LadderState returns the read-only view for the API/UI.
func (p *PowerOrchestrator) LadderState() LadderView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	levers := make([]string, len(p.ladder.Levers))
	copy(levers, p.ladder.Levers)
	return LadderView{Active: p.ladder.Active, Levers: levers}
}

// maybeStartLadder fires the apply goroutine once per battery event.
func (p *PowerOrchestrator) maybeStartLadder(cfg PowerConfig) {
	p.mu.Lock()
	if p.ladder.Active || p.ladderBusy {
		p.mu.Unlock()
		return
	}
	p.ladderBusy = true
	// Start the max-degradation clock (B9): the poll that got us here IS a
	// confirmation that we are on battery, and every later confirmation
	// refreshes it. If they stop arriving, enforceLadderTimeout undoes this.
	p.ladderConfirmedAt = time.Now()
	p.ladderExpired = false
	p.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in power-saving ladder", zap.Any("panic", r))
			}
			p.mu.Lock()
			p.ladderBusy = false
			p.mu.Unlock()
		}()
		p.applyLadder(cfg)
	}()
}

// maybeRevertLadder fires the revert goroutine when mains power is back (or
// the user opted out mid-event). Never during an ordered shutdown — the box is
// powering off and the shutdown path owns the unpausing.
func (p *PowerOrchestrator) maybeRevertLadder(reason string) {
	p.mu.Lock()
	if !p.ladder.Active || p.ladderBusy || p.acting {
		p.mu.Unlock()
		return
	}
	p.ladderBusy = true
	p.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in power-ladder revert", zap.Any("panic", r))
			}
			p.mu.Lock()
			p.ladderBusy = false
			p.mu.Unlock()
		}()
		p.revertLadder(reason)
	}()
}

// ladderContinue re-reads the live config and reports whether the ladder may
// keep applying levers: still enabled + opted-in, and no ordered shutdown has
// started. Re-reading per lever means a config change mid-apply (user opts
// out, edits essential apps) takes effect at the next lever instead of the
// whole ladder running on a stale snapshot.
func (p *PowerOrchestrator) ladderContinue() (PowerConfig, bool) {
	cfg := p.Config()
	p.mu.RLock()
	acting := p.acting
	p.mu.RUnlock()
	return cfg, cfg.Enabled && cfg.SavingsLadder && !acting
}

// applyLadder runs every enabled lever in order, feeding each applied lever to
// Forza (RecordAction + settle so the 5s battery-cadence report carries it).
// Cooperative cancellation: it re-checks ladderContinue before every lever and
// every pause, so an ordered shutdown (p.acting) or a user opt-out aborts it
// within one bounded docker/driver call.
func (p *PowerOrchestrator) applyLadder(PowerConfig) {
	drv := p.driverFor()
	cfg, cont := p.ladderContinue()
	if !cont {
		return
	}
	st := LadderState{Active: true, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	logger.Info("UPS prepare_shutdown — applying power-saving ladder")

	// Lever 1+2: CPU governor → powersave, EPP → power. One GET captures the
	// pre-state for both. persist=false: the ladder must never overwrite the
	// user's boot-reapplied tuning preference.
	info, err := drv.CPUGovernor()
	if err != nil {
		logger.Info("ladder: driver unreachable for governor pre-state; skipping CPU levers", zap.Error(err))
	} else if info.Supported {
		if info.Current != "" && info.Current != "powersave" && contains(info.Available, "powersave") {
			before := ladderWattsSample(cfg)
			if err := drv.SetCPUGovernor("powersave"); err != nil {
				logger.Info("ladder: governor lever failed", zap.Error(err))
			} else {
				st.PrevGovernor = info.Current
				st.Levers = append(st.Levers, "governor_powersave")
				p.commitLadder(st)
				ladderSettle()
				p.RecordAction("governor_powersave", before, ladderWattsSample(cfg))
			}
		}
		if cfg, cont = p.ladderContinue(); !cont {
			p.commitLadder(st)
			return
		}
		if info.EPP != "" && info.EPP != "power" && contains(info.EPPAvailable, "power") {
			before := ladderWattsSample(cfg)
			if err := drv.SetEPP("power"); err != nil {
				logger.Info("ladder: EPP lever failed", zap.Error(err))
			} else {
				st.PrevEPP = info.EPP
				st.Levers = append(st.Levers, "epp_power")
				p.commitLadder(st)
				ladderSettle()
				p.RecordAction("epp_power", before, ladderWattsSample(cfg))
			}
		}
	}

	// Lever 3: immediate standby for the configured non-critical HDDs.
	// Transient — any access wakes them, so there is no revert lever.
	if cfg, cont = p.ladderContinue(); !cont {
		p.commitLadder(st)
		return
	}
	if len(cfg.SpindownDisks) > 0 {
		before := ladderWattsSample(cfg)
		spun := 0
		for _, disk := range cfg.SpindownDisks {
			disk = strings.TrimSpace(disk)
			if disk == "" {
				continue
			}
			if err := drv.StandbyDisk(disk); err != nil {
				logger.Info("ladder: disk standby failed", zap.String("disk", disk), zap.Error(err))
			} else {
				spun++
			}
		}
		if spun > 0 {
			st.Levers = append(st.Levers, "disks_standby")
			p.commitLadder(st)
			ladderSettle()
			p.RecordAction("disks_standby", before, ladderWattsSample(cfg))
		}
	}

	// Lever 4 (opt-in): docker-pause non-essential apps. Pause (not stop) so
	// the revert is an instant unpause with no state lost.
	if cfg, cont = p.ladderContinue(); !cont {
		p.commitLadder(st)
		return
	}
	if cfg.PauseNonEssential {
		essential := map[string]bool{}
		for _, e := range cfg.EssentialApps {
			essential[strings.ToLower(strings.TrimSpace(e))] = true
		}
		before := ladderWattsSample(cfg)
		for _, name := range listRunningContainers() {
			if essential[strings.ToLower(name)] {
				continue
			}
			// An ordered shutdown started mid-loop → stop pausing NOW; the
			// shutdown's unpause pass (which waits for this goroutine) then
			// sees the final committed list.
			if _, cont = p.ladderContinue(); !cont {
				break
			}
			if err := pauseContainer(name); err != nil {
				logger.Info("ladder: pause failed", zap.String("container", name), zap.Error(err))
				continue
			}
			st.Paused = append(st.Paused, name)
			// Persist after EVERY pause: a crash mid-loop must not leave an
			// untracked frozen container.
			p.commitLadder(st)
		}
		if len(st.Paused) > 0 {
			st.Levers = append(st.Levers, "apps_paused")
			p.commitLadder(st)
			ladderSettle()
			p.RecordAction("apps_paused", before, ladderWattsSample(cfg))
		}
	}

	p.commitLadder(st)
	logger.Info("power-saving ladder applied", zap.Strings("levers", st.Levers))
}

// revertLadder undoes the levers in reverse order and clears the state.
// "Already gone" unpause failures count as done (unpauseGone) — after a host
// reboot no container is paused, and treating that as retryable would wedge
// the ladder Active forever. RecordAction fires only when something actually
// changed, so a retrying partial revert can't spam Forza's last_action.
func (p *PowerOrchestrator) revertLadder(reason string) {
	p.mu.RLock()
	st := p.ladder
	cfg := p.cfg
	p.mu.RUnlock()
	drv := p.driverFor()
	logger.Info("reverting power-saving ladder", zap.String("reason", reason), zap.Strings("levers", st.Levers))
	before := ladderWattsSample(cfg)
	changed := false

	// Unpause first — frozen apps are the most user-visible lever.
	if len(st.Paused) > 0 {
		remaining := unpauseList(st.Paused)
		changed = changed || len(remaining) < len(st.Paused)
		st.Paused = remaining
	}

	if st.PrevEPP != "" {
		if err := drv.SetEPP(st.PrevEPP); err != nil {
			logger.Error("ladder revert: EPP restore failed", zap.Error(err))
		} else {
			st.PrevEPP = ""
			changed = true
		}
	}
	if st.PrevGovernor != "" {
		if err := drv.SetCPUGovernor(st.PrevGovernor); err != nil {
			logger.Error("ladder revert: governor restore failed", zap.Error(err))
		} else {
			st.PrevGovernor = ""
			changed = true
		}
	}
	// Disks: nothing to undo (standby ends on first access).

	if len(st.Paused) == 0 && st.PrevEPP == "" && st.PrevGovernor == "" {
		p.clearLadder()
	} else {
		// Partial revert: keep the leftovers persisted and retry on the next
		// stable-normal poll.
		st.Active = true
		p.commitLadder(st)
	}
	if changed {
		p.RecordAction("ladder_revert", before, ladderWattsSample(cfg))
	}
}

// unpauseAllLadder is called on ordered shutdown: docker stop delivers SIGTERM,
// which a frozen (paused) process can never handle — unpause first so every
// app gets a clean stop. Governor/EPP are irrelevant when powering off. The
// caller (beginOrderedShutdown) has already set p.acting and drained the apply
// goroutine, so the committed Paused list is final; we still commit the result
// so nothing is double-tracked.
func (p *PowerOrchestrator) unpauseAllLadder() {
	p.mu.RLock()
	st := p.ladder
	p.mu.RUnlock()
	if len(st.Paused) == 0 {
		return
	}
	st.Paused = unpauseList(st.Paused)
	p.commitLadder(st)
}

// commitLadder stores st in memory and on disk (crash safety). Write-to-temp +
// rename so a hard power cut mid-write can't leave a truncated state file
// (this file exists precisely for power-loss scenarios).
func (p *PowerOrchestrator) commitLadder(st LadderState) {
	p.mu.Lock()
	p.ladder = st
	p.mu.Unlock()
	raw, _ := json.MarshalIndent(st, "", "  ")
	os.MkdirAll("/var/lib/casanova", 0o755)
	tmp := ladderStateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		logger.Error("failed to persist ladder state", zap.Error(err))
		return
	}
	if err := os.Rename(tmp, ladderStateFile); err != nil {
		logger.Error("failed to persist ladder state", zap.Error(err))
	}
}

// clearLadder resets memory state and removes the state file.
func (p *PowerOrchestrator) clearLadder() {
	p.mu.Lock()
	p.ladder = LadderState{}
	// Nothing is degraded any more, so the max-degradation valve has nothing to
	// measure: reset it or the next ladder would inherit a tripped flag and
	// skip its notification.
	p.ladderConfirmedAt = time.Time{}
	p.ladderExpired = false
	p.ladderExpiredAt = time.Time{}
	p.mu.Unlock()
	os.Remove(ladderStateFile)
}

// recoverLadderOnBoot heals leftovers from a restart mid-event: unpause any
// still-tracked containers and restore governor/EPP. The driver's own boot
// Reapply restores the user's persisted tuning too, so this is belt and
// suspenders. If the outage is still ongoing the poller simply re-applies the
// ladder on the next prepare_shutdown poll.
func (p *PowerOrchestrator) recoverLadderOnBoot() {
	raw, err := os.ReadFile(ladderStateFile)
	if err != nil {
		return
	}
	var st LadderState
	if err := json.Unmarshal(raw, &st); err != nil || !st.Active {
		os.Remove(ladderStateFile)
		return
	}
	logger.Info("recovering power-saving ladder leftovers from previous run",
		zap.Strings("levers", st.Levers))
	p.mu.Lock()
	p.ladder = st
	// Fresh clock: if the revert below is only partial, the leftovers must get
	// the full unconfirmed window before the valve complains — the state file's
	// age says nothing about the current outage.
	p.ladderConfirmedAt = time.Now()
	p.ladderExpired = false
	p.mu.Unlock()
	p.revertLadder("boot recovery")
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
