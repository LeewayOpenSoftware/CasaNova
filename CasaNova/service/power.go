package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/notifications"
	"github.com/shirou/gopsutil/v3/cpu"
	"go.uber.org/zap"
)

// PowerOrchestrator makes CasaNova UPS-aware by consuming the Forza power
// daemon's rich contract (GET /api/power, schema forza.power/v1) and reacting
// to the `advice` field: on `prepare_shutdown` it enters a power-saving ladder
// (P1b — governor/spindown/pause via the driver), and on `shutdown_now` it does
// an ORDERED shutdown (stop apps, then poweroff) before the battery dies.
//
// SAFETY: opt-in. It does NOTHING (never touches the box) until the user
// enables it in the config. The estimator is on the Forza side — we consume its
// calibrated soc/runtime/scenarios, we do NOT re-derive from a blind upsc.
type PowerOrchestrator struct {
	mu      sync.RWMutex
	cfg     PowerConfig
	last    ForzaPower // most recent reading (or zero if never/unreachable)
	lastErr string     // last poll error, "" when healthy
	// hysteresis: consecutive polls seen in each terminal advice before acting
	shutdownStreak int
	acting         bool // an ordered shutdown is in progress (one-shot)

	// P1b — power-saving ladder state (see power_ladder.go).
	ladder       LadderState
	ladderBusy   bool         // an apply/revert goroutine is running (one at a time)
	normalStreak int          // consecutive `normal` polls (revert hysteresis)
	prepareSeen  bool         // logged the prepare_shutdown transition already
	lastAdvice   string       // previous poll's advice — drives transition notifications
	driver       tuningClient // test override; nil = real unix-socket client

	// Max-degradation valve. Running on battery is a state we CONFIRM, never
	// one we assume from silence: ladderConfirmedAt is the last time a fresh
	// Forza reading positively said we are on battery. When it goes stale the
	// ladder is reverted even though we never saw a `normal` advice, so a dead
	// UPS daemon can't leave the box degraded (paused apps, powersave governor)
	// forever.
	ladderConfirmedAt time.Time
	ladderExpired     bool      // the valve already tripped for this ladder (notify once)
	ladderExpiredAt   time.Time // last valve-driven revert attempt (retry rate limit)

	// Poweroff retry state. `acting` must never latch on a poweroff that did
	// not happen, or the poller goes inert while the battery drains; instead we
	// release it and let a later poll retry after this instant.
	shutdownRetryAfter time.Time

	// Direction-2 peer state (register/report to Forza). In-memory: register
	// is idempotent by host, so we just re-register on boot.
	peerID     string
	peerToken  string
	peerMAC    string      // cached primary-NIC MAC
	lastAction *PeerAction // last power lever we applied, sent as feedback
	lastWatts  float64     // last reported self-estimated draw
	lastMode   string      // last reported power_mode
}

// PeerReportState summarizes Direction-2 reporting for the UI.
type PeerReportState struct {
	Registered bool    `json:"registered"`
	PeerID     string  `json:"peerId"`
	LastWatts  float64 `json:"lastWatts"`
	PowerMode  string  `json:"powerMode"`
	MAC        string  `json:"mac"`
}

// PowerConfig is persisted at powerConfigFile. Disabled by default.
type PowerConfig struct {
	Enabled       bool     `json:"enabled"`       // master switch — false => observe only
	ForzaURL      string   `json:"forzaUrl"`      // e.g. http://192.0.2.10:9090; empty = no UPS configured
	AutoShutdown  bool     `json:"autoShutdown"`  // allow shutdown_now to actually power off
	EssentialApps []string `json:"essentialApps"` // app/container names stopped LAST
	// Number of consecutive shutdown_now polls required before acting, so a
	// single transient reading can't power off the box.
	ShutdownConfirm int `json:"shutdownConfirm"`
	// Direction 2 — peer reporting. When ForzaToken is set (the shared
	// api_token from the Forza daemon), CasaNova registers with Forza and
	// reports its draw/mode so Forza treats it as topology ground truth and
	// can Wake-on-LAN it back when power returns. Empty => reporting off.
	ForzaToken string `json:"forzaToken"`
	// ForzaTokenSet is what read callers get instead of the token itself. It is
	// never persisted (the token's own presence is the source of truth).
	ForzaTokenSet bool    `json:"forzaTokenSet"`
	IdleWatts     float64 `json:"idleWatts"` // host draw at ~0% CPU (calibration knob)
	MaxWatts      float64 `json:"maxWatts"`  // host draw at 100% CPU
	// P1b — power-saving ladder on prepare_shutdown (all opt-in, default off).
	SavingsLadder     bool     `json:"savingsLadder"`     // master switch for the ladder
	SpindownDisks     []string `json:"spindownDisks"`     // kernel names (sda…) to standby; empty = skip
	PauseNonEssential bool     `json:"pauseNonEssential"` // docker-pause non-essential apps on battery
	// Maximum minutes the ladder may stay applied without a fresh reading
	// confirming we are still on battery. Past it the ladder is reverted and
	// the user is warned. <=0 => defaultLadderMaxUnconfirmedMin.
	LadderMaxUnconfirmedMin int `json:"ladderMaxUnconfirmedMin"`
}

// defaultLadderMaxUnconfirmedMin is deliberately far longer than any plausible
// Forza blip (the poller retries every 20s while the daemon is unreachable, so
// this is ~90 consecutive failures) yet far shorter than "until someone
// notices". Reverting mid-outage costs battery runtime; staying degraded
// forever costs the service.
const defaultLadderMaxUnconfirmedMin = 30

// ladderValveRetryInterval rate-limits the valve-driven revert so a partial
// revert (a container docker refuses to unpause) retries periodically instead
// of on every 5s poll.
const ladderValveRetryInterval = time.Minute

func defaultPowerConfig() PowerConfig {
	return PowerConfig{
		Enabled:                 false,
		ForzaURL:                "",
		AutoShutdown:            false,
		EssentialApps:           []string{},
		ShutdownConfirm:         2,
		IdleWatts:               40,
		MaxWatts:                120,
		LadderMaxUnconfirmedMin: defaultLadderMaxUnconfirmedMin,
	}
}

const powerConfigFile = "/var/lib/casanova/power-config.json"

// ── Forza contract (forza.power/v1) ──────────────────────────────────────
// Additive schema — unknown fields ignored. critical_window can be null, so
// every consumer must default it (seen live: null while confidence=low).

type ForzaPower struct {
	Schema        string          `json:"schema"`
	UPSName       string          `json:"ups_name"`
	State         string          `json:"state"` // online|on_battery|low_battery|unknown
	OnBattery     bool            `json:"on_battery"`
	SoCPct        float64         `json:"soc_pct"`
	RuntimeSec    int             `json:"runtime_sec"`
	EstRuntimeMin float64         `json:"est_runtime_min"`
	LoadPct       float64         `json:"load_pct"`
	EstWatts      *float64        `json:"est_watts"`
	InputVoltage  float64         `json:"input_voltage"`
	OutputVoltage float64         `json:"output_voltage"`
	Advice        string          `json:"advice"` // normal|prepare_shutdown|shutdown_now
	Estimator     *ForzaEstimator `json:"estimator"`
	TS            string          `json:"ts"`
	// synthesized locally:
	FetchedAt string `json:"fetchedAt"`
	Reachable bool   `json:"reachable"`
}

type ForzaEstimator struct {
	SoCPct              float64              `json:"soc_pct"`
	Confidence          string               `json:"confidence"` // low|medium|high
	EstRuntimeMin       float64              `json:"est_runtime_min"`
	DischargeRatePctMin float64              `json:"discharge_rate_pct_min"`
	ChargeRatePctHr     float64              `json:"charge_rate_pct_hr"`
	AgeCorrection       float64              `json:"age_correction_factor"`
	Scenarios           []ForzaScenario      `json:"scenarios"`
	CriticalWindow      *ForzaCriticalWindow `json:"critical_window"`
}

type ForzaScenario struct {
	Label      string  `json:"label"`
	LoadPct    float64 `json:"load_pct"`
	RuntimeMin float64 `json:"runtime_min"`
}

type ForzaCriticalWindow struct {
	ShutdownTriggerMin  float64 `json:"shutdown_trigger_min"`
	BatteryLowThreshold float64 `json:"battery_low_threshold"`
	ShutdownDelaySec    int     `json:"shutdown_delay_sec"`
}

// defaultCriticalWindow is only a fail-safe fallback: Forza now ALWAYS
// populates critical_window (shutdown_trigger_min is computed from the learned
// discharge rate, battery_low_threshold is the configured/20% floor,
// shutdown_delay_sec is Forza's config = 120 by default). We read those live
// values whenever present; this default applies only if the field is missing
// (e.g. an old daemon or a truncated response).
func defaultCriticalWindow() ForzaCriticalWindow {
	return ForzaCriticalWindow{ShutdownTriggerMin: 5, BatteryLowThreshold: 20, ShutdownDelaySec: 120}
}

var powerOrch = &PowerOrchestrator{cfg: loadPowerConfig()}

// Power returns the global orchestrator.
func Power() *PowerOrchestrator { return powerOrch }

func loadPowerConfig() PowerConfig {
	c := defaultPowerConfig()
	if raw, err := os.ReadFile(powerConfigFile); err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return normalizePowerConfig(c)
}

// normalizePowerConfig replaces out-of-range knobs with their defaults. Shared
// by the loader and the setter so an old config file and an old UI payload
// (which simply omits a newer field) both land on the same safe values.
func normalizePowerConfig(c PowerConfig) PowerConfig {
	d := defaultPowerConfig()
	if c.ShutdownConfirm < 1 {
		c.ShutdownConfirm = d.ShutdownConfirm
	}
	if strings.TrimSpace(c.ForzaURL) == "" {
		c.ForzaURL = d.ForzaURL
	}
	if c.IdleWatts <= 0 {
		c.IdleWatts = d.IdleWatts
	}
	if c.MaxWatts <= c.IdleWatts {
		c.MaxWatts = d.MaxWatts
	}
	if c.LadderMaxUnconfirmedMin <= 0 {
		c.LadderMaxUnconfirmedMin = d.LadderMaxUnconfirmedMin
	}
	return c
}

// Config / Status accessors (used by the route layer).
func (p *PowerOrchestrator) Config() PowerConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *PowerOrchestrator) SetConfig(c PowerConfig) error {
	c = normalizePowerConfig(c)
	p.mu.Lock()
	// An empty incoming token means "unchanged", never "clear it". The read
	// endpoint redacts ForzaToken (it was being served to anyone who could
	// reach the box — guest holds PermSystemRead, so the shared UPS credential
	// went out unauthenticated over the tunnel), so the settings form now loads
	// with the field blank and would otherwise wipe the stored token on every
	// save. To change it the operator types a new one; to clear it, unset the
	// Forza URL.
	if c.ForzaToken == "" {
		c.ForzaToken = p.cfg.ForzaToken
	}
	// If the shared token or Forza URL changed, drop the peer credentials so we
	// re-register cleanly against the new target.
	if c.ForzaToken != p.cfg.ForzaToken || c.ForzaURL != p.cfg.ForzaURL {
		p.peerID, p.peerToken = "", ""
	}
	p.cfg = c
	p.mu.Unlock()
	raw, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll("/var/lib/casanova", 0o755)
	// 0600: this file holds the shared Forza credential, so it must not be
	// world-readable to every uid on the host (including any container that
	// bind-mounts /var/lib/casanova).
	return writeFileAtomic(powerConfigFile, raw, 0o600)
}

// writeFileAtomic publishes a file with rename(2) so a crash mid-write can
// never leave a torn copy at the real path. The file this protects is the
// one that decides whether UPS orchestration runs at all: a truncated JSON
// here fails to parse at boot, the orchestrator starts unconfigured, and
// nothing says so until the next power cut — the exact moment the loss is
// unrecoverable. fsync before rename, because rename ordering without
// content durability is how "atomic" writes still produce empty files after
// power loss.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RedactedConfig is Config for callers that only need to READ it.
//
// GET /v1/sys/host/power/config is gated on PermSystemRead, which the guest
// role holds — and a request with no token resolves to guest — so the full
// struct, ForzaToken included, was being served unauthenticated to anyone who
// could reach the box. On this deployment that means the public internet: the
// 64-character shared UPS credential was retrievable with a single curl and no
// header. The token never leaves the daemon now; callers get a boolean so the
// UI can still show whether one is configured.
func (p *PowerOrchestrator) RedactedConfig() PowerConfig {
	c := p.Config()
	c.ForzaTokenSet = c.ForzaToken != ""
	c.ForzaToken = ""
	return c
}

// Status returns the last reading + health for the UI/API.
func (p *PowerOrchestrator) Status() (ForzaPower, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.last, p.lastErr
}

// PeerState returns the Direction-2 reporting summary for the UI.
func (p *PowerOrchestrator) PeerState() PeerReportState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return PeerReportState{
		Registered: p.peerToken != "",
		PeerID:     p.peerID,
		LastWatts:  p.lastWatts,
		PowerMode:  p.lastMode,
		MAC:        p.peerMAC,
	}
}

// Start launches the background poller. Safe to call once at boot; it observes
// even when disabled (so the UI card shows UPS state), but only ACTS on the box
// when cfg.Enabled.
func (p *PowerOrchestrator) Start(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in power orchestrator", zap.Any("panic", r))
			}
		}()
		// Heal any ladder leftovers from a restart mid-battery-event before
		// the first poll can re-apply.
		p.recoverLadderOnBoot()
		for {
			interval := p.pollOnce()
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// pollOnce fetches the Forza contract and drives the state machine. Returns the
// delay until the next poll (adaptive: fast on battery, slow on mains).
func (p *PowerOrchestrator) pollOnce() time.Duration {
	cfg := p.Config()
	if strings.TrimSpace(cfg.ForzaURL) == "" {
		// No UPS configured — nothing to poll, nothing to report, and no
		// fetch error to manufacture every cycle.
		p.mu.Lock()
		p.last = ForzaPower{}
		p.lastErr = ""
		p.mu.Unlock()
		return 60 * time.Second
	}
	pw, err := fetchForza(cfg.ForzaURL)
	p.mu.Lock()
	// Refresh the "we are still on battery" evidence BEFORE anything else so
	// the max-degradation valve below measures silence, not processing order.
	if err == nil && batteryConfirmed(pw) {
		p.ladderConfirmedAt = time.Now()
	}
	if err != nil {
		// Fail-SAFE: the Forza daemon being unreachable does NOT mean the power
		// is out — never shut down on a fetch error. Just record it; keep the
		// last known reading for the UI.
		p.lastErr = err.Error()
		p.shutdownStreak = 0
	} else {
		p.last = pw
		p.lastErr = ""
	}
	advice := ""
	if err == nil {
		advice = pw.Advice
	}
	acting := p.acting
	if advice == "shutdown_now" {
		p.shutdownStreak++
	} else {
		p.shutdownStreak = 0
	}
	if advice == "normal" {
		p.normalStreak++
	} else {
		p.normalStreak = 0
	}
	if advice != "prepare_shutdown" && advice != "shutdown_now" {
		p.prepareSeen = false
	}
	streak := p.shutdownStreak
	normalStreak := p.normalStreak
	ladderActive := p.ladder.Active
	prevAdvice := p.lastAdvice
	if err == nil {
		p.lastAdvice = advice
	}
	p.mu.Unlock()

	// Notification-center entries on power-state TRANSITIONS (regardless of
	// Enabled — even observe-only mode should tell the user the mains died).
	// Transition-driven so a long outage produces one entry, not one per poll.
	if err == nil && advice != prevAdvice {
		if ns := Notifications(); ns != nil {
			switch {
			case advice == "prepare_shutdown" && prevAdvice != "shutdown_now":
				msg := fmt.Sprintf("Mains power lost — running on UPS battery (%.0f%% charge, ~%.0f min left).",
					socOf(pw), runtimeOf(pw))
				if cfg.Enabled && cfg.SavingsLadder {
					msg += " Engaging the power-saving ladder."
				}
				_ = ns.Warn(notifications.CategorySystem, "UPS on battery", msg)
			case advice == "normal" && (prevAdvice == "prepare_shutdown" || prevAdvice == "shutdown_now"):
				_ = ns.Info(notifications.CategorySystem, "Mains power restored",
					"Grid power is back; the UPS is recharging. Any power-saving measures are being reverted.")
			}
		}
	}

	// React only when enabled and we have a fresh reading.
	if cfg.Enabled && err == nil && !acting {
		switch advice {
		case "shutdown_now":
			if cfg.AutoShutdown && streak >= cfg.ShutdownConfirm {
				p.beginOrderedShutdown(pw)
			}
		case "prepare_shutdown":
			p.mu.Lock()
			first := !p.prepareSeen
			p.prepareSeen = true
			p.mu.Unlock()
			if first {
				logger.Info("UPS on battery — prepare_shutdown",
					zap.Float64("soc", socOf(pw)), zap.Float64("est_runtime_min", runtimeOf(pw)))
			}
			// P1b: enter the power-saving ladder (opt-in). Idempotent — the
			// Active/busy guards make repeat polls no-ops.
			if cfg.SavingsLadder {
				p.maybeStartLadder(cfg)
			}
		}
	}

	// Ladder revert: mains power back and stable (2 polls ≈ 30s on the normal
	// cadence), or the user opted out mid-event. Fail-safe by omission: an
	// unreachable Forza (err != nil) resets no lever — we stay in powersave
	// until we KNOW mains is back.
	if ladderActive {
		if err == nil && advice == "normal" && normalStreak >= 2 {
			p.maybeRevertLadder("mains power restored")
		} else if !cfg.Enabled || !cfg.SavingsLadder {
			p.maybeRevertLadder("ladder disabled")
		} else {
			// Neither a confirmed recovery nor an opt-out: this is where an
			// unreachable Forza (or a 200 with garbage advice) used to park the
			// box in a degraded state forever.
			p.enforceLadderTimeout(cfg)
		}
	}

	// Direction 2 — register + report our power state to Forza so it becomes
	// topology ground truth (and can Wake-on-LAN us back). Best-effort: this
	// NEVER blocks or affects the shutdown-safety logic above.
	if strings.TrimSpace(cfg.ForzaToken) != "" {
		p.reportPeer(cfg, pw, err == nil, advice, acting)
	}

	// Adaptive cadence: tight when on battery / preparing, relaxed on mains.
	if err != nil {
		return 20 * time.Second
	}
	switch advice {
	case "shutdown_now", "prepare_shutdown":
		return 5 * time.Second
	default:
		return 15 * time.Second
	}
}

// batteryConfirmed reports whether a fresh reading POSITIVELY says we are still
// running on battery. Anything else — an unreachable daemon, a 200 with an
// advice we don't know, a reading that says mains — is *not* a confirmation and
// therefore lets the max-degradation valve's clock run.
func batteryConfirmed(pw ForzaPower) bool {
	if pw.OnBattery || pw.State == "on_battery" || pw.State == "low_battery" {
		return true
	}
	return pw.Advice == "prepare_shutdown" || pw.Advice == "shutdown_now"
}

// enforceLadderTimeout reverts the ladder once the degraded state has outlived
// the evidence that justified it (cfg.LadderMaxUnconfirmedMin without a single
// reading confirming battery). Staying in powersave is cheap; leaving apps
// frozen and disks parked for days because the UPS daemon died on a box with
// perfectly good mains is not — and unlike the shutdown path, reverting can
// never damage anything, so "I don't know" must resolve to "restore service".
//
// The revert is announced (log + notification): a limit being reached is an
// event the operator has to see, never a silent state change.
func (p *PowerOrchestrator) enforceLadderTimeout(cfg PowerConfig) {
	maxUnconfirmed := time.Duration(cfg.LadderMaxUnconfirmedMin) * time.Minute
	if maxUnconfirmed <= 0 {
		maxUnconfirmed = defaultLadderMaxUnconfirmedMin * time.Minute
	}
	now := time.Now()

	p.mu.Lock()
	if p.ladderConfirmedAt.IsZero() {
		// Unknown clock (state recovered from disk, or a ladder that started
		// before this field existed): start counting now instead of tripping
		// the valve on a zero timestamp.
		p.ladderConfirmedAt = now
		p.mu.Unlock()
		return
	}
	unconfirmed := now.Sub(p.ladderConfirmedAt)
	if unconfirmed < maxUnconfirmed {
		p.mu.Unlock()
		return
	}
	// The ladder can survive the first revert attempt (a container docker
	// refuses to unpause keeps it Active), so rate-limit the retries and notify
	// only on the transition.
	if p.ladderExpired && now.Sub(p.ladderExpiredAt) < ladderValveRetryInterval {
		p.mu.Unlock()
		return
	}
	first := !p.ladderExpired
	p.ladderExpired = true
	p.ladderExpiredAt = now
	lastErr := p.lastErr
	p.mu.Unlock()

	reason := fmt.Sprintf("battery state unconfirmed for %s", unconfirmed.Round(time.Minute))
	logger.Error("power-saving ladder exceeded its unconfirmed-battery limit — reverting",
		zap.Duration("unconfirmed", unconfirmed.Round(time.Second)),
		zap.Duration("limit", maxUnconfirmed),
		zap.String("last_forza_error", lastErr))
	if first {
		if ns := Notifications(); ns != nil {
			detail := "the UPS daemon stopped confirming it"
			if lastErr != "" {
				detail = "the UPS daemon is unreachable (" + lastErr + ")"
			}
			_ = ns.Warn(notifications.CategorySystem, "Power-saving measures reverted",
				fmt.Sprintf("The power-saving ladder had been applied for %s without any reading confirming the UPS is still on battery — %s. Restoring normal operation. Check the UPS link before the next outage.",
					unconfirmed.Round(time.Minute), detail))
		}
	}
	p.maybeRevertLadder(reason)
}

// powerModeFor maps our current view to the free-form power_mode vocabulary
// Forza suggested (normal·powersave·throttled·critical).
func powerModeFor(advice string, acting bool) string {
	if acting {
		return "critical"
	}
	switch advice {
	case "shutdown_now":
		return "critical"
	case "prepare_shutdown":
		return "powersave" // P1b will make this literally true (governor/spindown)
	default:
		return "normal"
	}
}

func socOf(pw ForzaPower) float64 {
	if pw.Estimator != nil && pw.Estimator.SoCPct > 0 {
		return pw.Estimator.SoCPct
	}
	return pw.SoCPct
}
func runtimeOf(pw ForzaPower) float64 {
	if pw.Estimator != nil && pw.Estimator.EstRuntimeMin > 0 {
		return pw.Estimator.EstRuntimeMin
	}
	return pw.EstRuntimeMin
}

// beginOrderedShutdown stops apps (non-essential first, essential last) then
// powers off. One-shot; guarded by p.acting.
func (p *PowerOrchestrator) beginOrderedShutdown(pw ForzaPower) {
	p.mu.Lock()
	if p.acting {
		p.mu.Unlock()
		return
	}
	// A previous attempt failed outright. Retry — the battery is still
	// draining — but not on every 5s poll: give the transient cause (busy
	// logind, saturated D-Bus) room to clear.
	if now := time.Now(); now.Before(p.shutdownRetryAfter) {
		p.mu.Unlock()
		return
	}
	p.acting = true
	essential := map[string]bool{}
	for _, e := range p.cfg.EssentialApps {
		essential[strings.ToLower(strings.TrimSpace(e))] = true
	}
	p.mu.Unlock()

	// Every way out of this function means the box is STILL RUNNING — a poweroff
	// that worked kills the process inside executePowerOff and never returns
	// here. So the latch is released on return, panic included: leaving `acting`
	// true would gate the whole poller off and let the battery die, which is the
	// dirty cut this module exists to prevent. The backoff is what keeps the
	// retry from re-running the sequence on every 5s battery poll.
	defer func() {
		p.mu.Lock()
		p.acting = false
		p.shutdownRetryAfter = time.Now().Add(shutdownRetryBackoff)
		p.mu.Unlock()
	}()

	logger.Error("UPS shutdown_now — beginning ORDERED shutdown",
		zap.Float64("soc", socOf(pw)), zap.Float64("est_runtime_min", runtimeOf(pw)))

	// Leave a record the user finds after the outage: the box didn't crash,
	// it protected itself. Written before stopping anything so the row is
	// durable even if the poweroff races the DB flush.
	if ns := Notifications(); ns != nil {
		_ = ns.Err(notifications.CategorySystem, "UPS critical — shutting down",
			fmt.Sprintf("Battery critically low (%.0f%% charge, ~%.0f min left). Stopping apps in order and powering off.",
				socOf(pw), runtimeOf(pw)))
	}

	// If the ladder paused apps, unfreeze them first: docker stop's SIGTERM
	// can't reach a frozen process, so a paused container would sit out the
	// 10s grace and get SIGKILLed — the opposite of a clean stop. p.acting is
	// already set, which makes an in-flight applyLadder abort at its next
	// lever/pause check — wait (bounded) for it to drain so the Paused list we
	// unpause below is final.
	for i := 0; i < 600; i++ { // ≤60s; each ladder docker/driver op is ≤30s bounded
		p.mu.RLock()
		busy := p.ladderBusy
		p.mu.RUnlock()
		if !busy {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.unpauseAllLadder()

	running := listRunningContainers()
	var nonEss, ess []string
	for _, name := range running {
		if essential[strings.ToLower(name)] {
			ess = append(ess, name)
		} else {
			nonEss = append(nonEss, name)
		}
	}
	// Non-essential first (free the battery + reduce write pressure), then
	// essential (databases etc. get the cleanest possible stop last).
	stopContainerList(nonEss)
	stopContainerList(ess)

	// Tell Forza we're going down (best-effort, short) so it can update the
	// topology and Wake-on-LAN us back when power returns.
	cfg := p.Config()
	if strings.TrimSpace(cfg.ForzaToken) != "" {
		p.RecordAction("ordered_shutdown", p.lastWatts, 0)
		p.reportPeer(cfg, pw, true, "shutdown_now", true)
	}

	logger.Error("ordered shutdown complete — powering off")
	aborted := p.executePowerOff()

	if ns := Notifications(); ns != nil {
		if aborted {
			_ = ns.Warn(notifications.CategorySystem, "Automatic shutdown cancelled",
				"Mains power came back while the shutdown was being retried and nothing had been powered off yet, so the box was kept running. Apps were stopped and are being restarted by their restart policy.")
		} else {
			_ = ns.Err(notifications.CategorySystem, "Automatic poweroff FAILED",
				"Every shutdown mechanism failed (systemd, the poweroff binary, SysRq and the kernel syscall). The box is still running on UPS battery — power it off by hand NOW.")
		}
	}
}

// ── poweroff escalation (B10) ─────────────────────────────────────────────
// This is the last safety net before the battery dies, so a single failed
// `systemctl poweroff` may not be the end of it: logind can be busy, D-Bus
// saturated, the shutdown transaction can wedge on a hung unit. Each rung below
// is a genuinely DIFFERENT mechanism, not a retry of the same call, ordered
// cleanest-first.

var (
	// poweroffGrace is how long we stay alive watching an ACCEPTED request take
	// effect before moving on. Every container is already stopped at this
	// point, so a healthy systemd poweroff finishes in seconds — 90s means
	// "wedged", not "slow".
	poweroffGrace = 90 * time.Second
	// poweroffPatienceCap bounds the EXTRA time we grant while systemd reports
	// it is genuinely tearing down. Forcing a power cut in the middle of
	// unmounting 5+ TB is precisely the dirty cut we are trying to avoid, so a
	// slow-but-progressing shutdown is never interrupted before this cap.
	poweroffPatienceCap = 10 * time.Minute
	// poweroffForcedGrace is the window after a DRASTIC rung: those bypass the
	// unit teardown entirely, so they either cut power within seconds or never
	// will, and waiting longer only burns battery.
	poweroffForcedGrace = 10 * time.Second
	// poweroffRejectedPause is the breather after a rung that failed outright —
	// nothing was queued, so there is nothing to wait for.
	poweroffRejectedPause = 3 * time.Second
	// shutdownRetryBackoff keeps a totally failed shutdown from re-running the
	// whole ordered sequence on every battery-cadence poll.
	shutdownRetryBackoff = 2 * time.Minute
)

// powerOffRung is one mechanism for taking the box down. `drastic` marks the
// ones that cut power without a unit teardown; those are the only ones we
// reconsider before firing.
type powerOffRung struct {
	name    string
	drastic bool
	run     func() error
}

// poweroffRungs is a func (not a slice) so it is re-built per attempt and so a
// test can replace the whole ladder. Order: systemd's normal transaction, the
// sysvinit-compat binary (different argv path, still a clean shutdown),
// systemd's forced path (kills processes, remounts read-only), the kernel's
// SysRq handler, and finally reboot(2) — which needs no PATH, no D-Bus and no
// writable /proc.
var poweroffRungs = func() []powerOffRung {
	return []powerOffRung{
		{name: "systemctl poweroff", run: func() error { return MyService.System().SystemShutdown() }},
		{name: "poweroff", run: func() error { return powerRunCommand("poweroff") }},
		{name: "systemctl --force poweroff", drastic: true, run: func() error { return powerRunCommand("systemctl", "--force", "poweroff") }},
		{name: "sysrq power-off", drastic: true, run: func() error { return sysrqPowerOff() }},
		{name: "reboot(2) power-off", drastic: true, run: func() error { return kernelPowerOff() }},
	}
}

// executePowerOff walks the escalation ladder. It only ever RETURNS on failure:
// if any mechanism works, this process dies inside poweroffWait. The bool says
// whether we stopped because mains power came back (true) rather than because
// every mechanism failed (false).
func (p *PowerOrchestrator) executePowerOff() (aborted bool) {
	queued := false
	for _, rung := range poweroffRungs() {
		// While nothing has been queued the box is intact, so backing out is
		// still possible — and cutting power to a box whose mains just came
		// back is worse than the outage we were reacting to. Once systemd has
		// accepted a shutdown we are committed: the teardown is already
		// running and there is nothing left to save.
		if rung.drastic && !queued && p.mainsConfirmedBack() {
			logger.Error("poweroff escalation aborted — mains power confirmed back and nothing was queued",
				zap.String("next_mechanism", rung.name))
			return true
		}
		if err := rung.run(); err != nil {
			logger.Error("poweroff mechanism failed — escalating",
				zap.String("mechanism", rung.name), zap.Error(err))
			powerOffSleep(poweroffRejectedPause)
			continue
		}
		queued = true
		logger.Error("poweroff requested — waiting for the box to go down",
			zap.String("mechanism", rung.name))
		poweroffWait(rung.drastic)
		logger.Error("still running after an accepted poweroff — escalating",
			zap.String("mechanism", rung.name))
	}
	return false
}

// mainsConfirmedBack asks Forza directly (not the cached reading) whether the
// outage is over. Only a fresh, positive answer counts: an unreachable daemon
// means "unknown", and unknown must never cancel a shutdown we already
// committed to — that asymmetry is the whole fail-safe.
func (p *PowerOrchestrator) mainsConfirmedBack() bool {
	pw, err := fetchForza(p.Config().ForzaURL)
	if err != nil {
		return false
	}
	return pw.Advice == "normal" && !batteryConfirmed(pw)
}

// poweroffWait blocks while an accepted request takes effect. If it worked we
// are killed inside the loop and never return; returning at all means the box
// is still up. A drastic rung either cuts power within seconds or never will —
// there is no unit teardown to be patient about — so it gets a short fixed
// window. For the orderly rungs, while systemd reports it IS stopping we keep
// waiting up to poweroffPatienceCap rather than forcing a cut over a slow
// unmount of several terabytes.
var poweroffWait = func(drastic bool) {
	start := time.Now()
	for {
		powerOffSleep(2 * time.Second)
		elapsed := time.Since(start)
		if drastic {
			if elapsed >= poweroffForcedGrace {
				return
			}
			continue
		}
		if elapsed >= poweroffPatienceCap {
			return
		}
		if elapsed >= poweroffGrace && !systemIsStopping() {
			return
		}
	}
}

// powerOffSleep is a var so tests don't spend real minutes in the ladder.
var powerOffSleep = time.Sleep

// systemIsStopping reports whether systemd has entered the shutdown
// transaction. It separates "the poweroff is slow" (keep waiting) from "the
// poweroff never started" (escalate). Any failure answers false: if we cannot
// ask, we must not extend our patience indefinitely.
var systemIsStopping = func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// `is-system-running` exits non-zero for every state except "running", so
	// the exit code is ignored and only the printed state is read.
	out, _ := exec.CommandContext(ctx, "systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)) == "stopping"
}

// powerRunCommand runs one shutdown binary, bounded so a hung helper cannot
// stall the escalation.
var powerRunCommand = func(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// powerSyncDisks flushes the page cache before any forced power cut, so the
// drastic rungs lose as little as possible.
var powerSyncDisks = func() { syscall.Sync() }

// sysrqPowerOff drives the kernel's SysRq handler: no systemd, no D-Bus, no
// PATH. Writing 'o' to a kernel whose sysrq mask forbids poweroff succeeds and
// does NOTHING, so the mask is checked (and raised) first — at this point the
// alternative is the battery dying mid-write.
var sysrqPowerOff = func() error {
	powerSyncDisks()
	if raw, err := os.ReadFile("/proc/sys/kernel/sysrq"); err == nil {
		mask, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		// 1 = everything allowed; bit 128 = the reboot/poweroff commands.
		if convErr == nil && mask != 1 && mask&128 == 0 {
			if werr := os.WriteFile("/proc/sys/kernel/sysrq", []byte("1"), 0o644); werr != nil {
				logger.Error("could not enable sysrq for the emergency power-off", zap.Error(werr))
			}
		}
	}
	return os.WriteFile("/proc/sysrq-trigger", []byte("o"), 0o200)
}

// kernelPowerOff is the final backstop: the reboot(2) syscall itself, which
// depends on nothing but the kernel and CAP_SYS_BOOT.
var kernelPowerOff = func() error {
	powerSyncDisks()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

// runningContainerNames lists running docker container names.
func runningContainerNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names
}

// stopContainerList is the shutdown path's hook for stopping containers — a var
// (like the ladder's pause/unpause hooks) so the ordered-shutdown state machine
// can be tested without a docker daemon and without ever touching real
// containers.
var stopContainerList = stopContainers

// stopContainers gracefully stops each container (10s grace) sequentially.
func stopContainers(names []string) {
	for _, n := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "10", n).CombinedOutput(); err != nil {
			logger.Error("failed to stop container during UPS shutdown",
				zap.String("name", n), zap.String("out", strings.TrimSpace(string(out))))
		}
		cancel()
	}
}

// fetchForza is how the state machine reaches the Forza contract — a package
// var so tests can drive a full poll without a live daemon.
var fetchForza = fetchForzaPower

// fetchForzaPower GETs the Forza power contract and defaults critical_window.
func fetchForzaPower(baseURL string) (ForzaPower, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/power"
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ForzaPower{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ForzaPower{}, fmt.Errorf("forza returned %s", resp.Status)
	}
	var pw ForzaPower
	if err := json.NewDecoder(resp.Body).Decode(&pw); err != nil {
		return ForzaPower{}, fmt.Errorf("bad forza response: %w", err)
	}
	if pw.Estimator != nil && pw.Estimator.CriticalWindow == nil {
		cw := defaultCriticalWindow()
		pw.Estimator.CriticalWindow = &cw
	}
	pw.Reachable = true
	pw.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	return pw, nil
}

// ── Direction 2: peer register/report to Forza (P2) + MAC/WoL (P3) ───────
// Forza's /api/peers contract: register with the shared api_token (Bearer),
// get a per-peer token, then report every tick (per-peer Bearer). A report
// with on_ups:true makes Forza treat CasaNova as topology ground truth. The
// registered MAC lets Forza Wake-on-LAN us back when power returns.

type PeerAction struct {
	Action      string  `json:"action"`
	WattsBefore float64 `json:"watts_before"`
	WattsAfter  float64 `json:"watts_after"`
	At          string  `json:"at"`
}

type peerRegisterReq struct {
	Name         string   `json:"name"`
	Host         string   `json:"host"`
	Platform     string   `json:"platform"`
	PowerSource  string   `json:"power_source"`
	Capabilities []string `json:"capabilities"`
	MAC          string   `json:"mac"`
}

type peerRegisterResp struct {
	PeerID string `json:"peer_id"`
	Token  string `json:"token"`
}

type peerReportReq struct {
	PowerSource string      `json:"power_source"`
	Watts       float64     `json:"watts"`
	OnUPS       bool        `json:"on_ups"`
	BatteryPct  *float64    `json:"battery_pct,omitempty"`
	PowerMode   string      `json:"power_mode"`
	LastAction  *PeerAction `json:"last_action,omitempty"`
}

// RecordAction stores the last power lever we applied so the next report feeds
// it back to Forza's estimator (watts_before/after = the real saving).
func (p *PowerOrchestrator) RecordAction(action string, wattsBefore, wattsAfter float64) {
	p.mu.Lock()
	p.lastAction = &PeerAction{
		Action: action, WattsBefore: wattsBefore, WattsAfter: wattsAfter,
		At: time.Now().UTC().Format(time.RFC3339),
	}
	p.mu.Unlock()
}

// reportPeer ensures we're registered, then sends one report. Best-effort.
func (p *PowerOrchestrator) reportPeer(cfg PowerConfig, pw ForzaPower, fresh bool, advice string, acting bool) {
	p.mu.RLock()
	token := p.peerToken
	p.mu.RUnlock()
	if token == "" {
		if err := p.registerWithForza(cfg); err != nil {
			logger.Info("Forza peer register failed", zap.Error(err))
			return
		}
		p.mu.RLock()
		token = p.peerToken
		p.mu.RUnlock()
	}

	watts := p.estimateWatts(cfg)
	mode := powerModeFor(advice, acting)
	p.mu.Lock()
	p.lastWatts = watts
	p.lastMode = mode
	action := p.lastAction
	p.mu.Unlock()

	body := peerReportReq{
		PowerSource: "ups", // fed through the UPS (Forza's own vocab)
		Watts:       watts,
		OnUPS:       true,
		PowerMode:   mode,
		LastAction:  action,
	}
	// battery_pct only when the reading is fresh — a stale soc would feed
	// Forza's estimator garbage during its own outage.
	if fresh {
		soc := socOf(pw)
		body.BatteryPct = &soc
	}
	if err := p.forzaPost(cfg.ForzaURL, "/api/peers/report", token, body, nil); err != nil {
		// Stale/rotated token → drop it so we re-register next tick.
		if strings.Contains(err.Error(), "401") {
			p.mu.Lock()
			p.peerToken, p.peerID = "", ""
			p.mu.Unlock()
		}
		logger.Info("Forza peer report failed", zap.Error(err))
	}
}

// registerWithForza registers this box (idempotent by host on Forza's side).
func (p *PowerOrchestrator) registerWithForza(cfg PowerConfig) error {
	hostIP, mac := outboundIPAndMAC(cfg.ForzaURL)
	hostname, _ := os.Hostname()
	p.mu.Lock()
	p.peerMAC = mac
	p.mu.Unlock()

	// Always send the FULL profile — Forza overwrites capabilities on a partial
	// re-register, so a slim body would drop our wake/report caps.
	body := peerRegisterReq{
		Name:         hostname,
		Host:         hostIP,
		Platform:     "linux",
		PowerSource:  "ups",
		Capabilities: []string{"report", "wake", "shutdown"},
		MAC:          mac,
	}
	var resp peerRegisterResp
	if err := p.forzaPost(cfg.ForzaURL, "/api/peers/register", cfg.ForzaToken, body, &resp); err != nil {
		return err
	}
	if resp.Token == "" {
		return fmt.Errorf("register returned empty token")
	}
	p.mu.Lock()
	p.peerID, p.peerToken = resp.PeerID, resp.Token
	p.mu.Unlock()
	logger.Info("registered with Forza power daemon",
		zap.String("peer_id", resp.PeerID), zap.String("mac", mac), zap.String("host", hostIP))
	return nil
}

// estimateWatts derives self-draw from CPU load and the host power model.
// ponytail: linear idle→max model; IdleWatts/MaxWatts are calibration knobs —
// tune them against the Forza UPS reading, the real hardware isn't on paper.
func (p *PowerOrchestrator) estimateWatts(cfg PowerConfig) float64 {
	pcts, err := cpu.Percent(0, false)
	load := 0.0
	if err == nil && len(pcts) > 0 {
		load = pcts[0]
	}
	w := cfg.IdleWatts + (cfg.MaxWatts-cfg.IdleWatts)*load/100.0
	return math.Round(w*10) / 10
}

// forzaPost POSTs JSON to Forza with an optional Bearer token; decodes into out
// if non-nil (report has no body → pass nil).
func (p *PowerOrchestrator) forzaPost(baseURL, path, bearer string, body, out interface{}) error {
	raw, _ := json.Marshal(body)
	client := &http.Client{Timeout: 6 * time.Second}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forza %s -> %s", path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// outboundIPAndMAC returns the local IP + NIC MAC used to reach Forza — the
// identity Forza sees (host key) and the MAC it needs for Wake-on-LAN.
func outboundIPAndMAC(forzaURL string) (ip string, mac string) {
	host := forzaURL
	if u, err := url.Parse(forzaURL); err == nil && u.Host != "" {
		host = u.Host
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	conn, err := net.Dial("udp", net.JoinHostPort(h, "9")) // route selection only; no packet sent
	if err != nil {
		return "", ""
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", ""
	}
	local := ua.IP
	ip = local.String()
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return ip, ifi.HardwareAddr.String()
			}
		}
	}
	return ip, ""
}
