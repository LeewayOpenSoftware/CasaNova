package service

import (
	"errors"
	"testing"
	"time"
)

// ── B10: poweroff must escalate and must never latch `acting` ─────────────

// fakePowerOff replaces the whole poweroff escalation ladder plus its waits, so
// a test drives the state machine without touching systemd, /proc or the clock.
type fakePowerOff struct {
	fired  []string        // rung names actually run, in order
	fail   map[string]bool // rungs that report failure
	queued map[string]bool // rungs that report success (accepted, box still up)
}

func setupPowerOffEnv(t *testing.T, f *fakePowerOff) {
	t.Helper()
	origRungs, origWait, origSleep := poweroffRungs, poweroffWait, powerOffSleep
	origStop, origList, origFetch := stopContainerList, listRunningContainers, fetchForza
	poweroffRungs = func() []powerOffRung {
		mk := func(name string, drastic bool) powerOffRung {
			return powerOffRung{name: name, drastic: drastic, run: func() error {
				f.fired = append(f.fired, name)
				if f.fail[name] {
					return errors.New(name + " refused")
				}
				return nil
			}}
		}
		return []powerOffRung{
			mk("systemctl poweroff", false),
			mk("poweroff", false),
			mk("forced", true),
			mk("sysrq", true),
			mk("syscall", true),
		}
	}
	poweroffWait = func(bool) {}
	powerOffSleep = func(time.Duration) {}
	// The ordered-shutdown prologue must never reach a real docker daemon.
	stopContainerList = func([]string) {}
	listRunningContainers = func() []string { return nil }
	// Default: Forza unreachable, i.e. "unknown", which must NOT abort.
	fetchForza = func(string) (ForzaPower, error) { return ForzaPower{}, errors.New("connection refused") }
	t.Cleanup(func() {
		poweroffRungs, poweroffWait, powerOffSleep = origRungs, origWait, origSleep
		stopContainerList, listRunningContainers, fetchForza = origStop, origList, origFetch
	})
}

func shutdownCfg() PowerConfig {
	c := defaultPowerConfig()
	// fetchForza is always mocked in these tests, but pollOnce now treats an
	// empty ForzaURL as "no UPS configured" and skips the fetch entirely —
	// these tests are exercising a configured UPS, so give it a fixture URL.
	c.ForzaURL = "http://192.0.2.10:9090"
	c.Enabled = true
	c.AutoShutdown = true
	return c
}

// The original bug: `systemctl poweroff` fails, the error is logged, and
// `acting` stays true forever — the poller stays alive but inert while the
// battery drains. Mutation that reproduces it: replace the tail of
// beginOrderedShutdown with a bare
//
//	if err := MyService.System().SystemShutdown(); err != nil { logger.Error(...) }
//
// and this test fails on the `acting` assertion.
func TestOrderedShutdownDoesNotLatchActingWhenPowerOffFails(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{
		"systemctl poweroff": true, "poweroff": true,
		"forced": true, "sysrq": true, "syscall": true,
	}}
	setupPowerOffEnv(t, f)
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	p.beginOrderedShutdown(ForzaPower{Advice: "shutdown_now"})

	p.mu.RLock()
	acting := p.acting
	retryAfter := p.shutdownRetryAfter
	p.mu.RUnlock()
	if acting {
		t.Fatal("acting latched true after a failed poweroff — the poller is now inert while the battery drains")
	}
	if retryAfter.IsZero() || time.Until(retryAfter) <= 0 {
		t.Fatalf("shutdownRetryAfter = %v, want a future instant so the retry is rate-limited", retryAfter)
	}
	if len(f.fired) != 5 {
		t.Fatalf("mechanisms tried = %v, want all five", f.fired)
	}
}

// Escalation order: every mechanism is tried, cleanest first. Mutation that
// reproduces the original (single `systemctl poweroff`, no escalation): only
// one rung fires and this fails.
func TestPowerOffEscalatesThroughEveryMechanism(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{
		"systemctl poweroff": true, "poweroff": true,
		"forced": true, "sysrq": true, "syscall": true,
	}}
	setupPowerOffEnv(t, f)
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	if aborted := p.executePowerOff(); aborted {
		t.Fatal("executePowerOff reported an abort, but Forza was unreachable — unknown must never cancel a committed shutdown")
	}
	want := []string{"systemctl poweroff", "poweroff", "forced", "sysrq", "syscall"}
	if len(f.fired) != len(want) {
		t.Fatalf("mechanisms = %v, want %v", f.fired, want)
	}
	for i := range want {
		if f.fired[i] != want[i] {
			t.Fatalf("mechanisms = %v, want %v (cleanest first)", f.fired, want)
		}
	}
}

// An accepted-but-ineffective rung still escalates: `systemctl poweroff`
// returning 0 while the shutdown transaction wedges is exactly the outage the
// review describes.
func TestPowerOffEscalatesAfterAnAcceptedButIneffectiveRequest(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{}} // every rung "succeeds", box stays up
	setupPowerOffEnv(t, f)
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	p.executePowerOff()

	if len(f.fired) != 5 {
		t.Fatalf("mechanisms = %v, want escalation to continue past an accepted-but-ineffective request", f.fired)
	}
}

// The escalation may back out only while NOTHING has been queued: cutting power
// to a box whose mains just came back is worse than the outage.
func TestPowerOffAbortsWhenMainsConfirmedBackBeforeAnythingQueued(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{"systemctl poweroff": true, "poweroff": true}}
	setupPowerOffEnv(t, f)
	fetchForza = func(string) (ForzaPower, error) {
		return ForzaPower{Advice: "normal", State: "online", Reachable: true}, nil
	}
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	if aborted := p.executePowerOff(); !aborted {
		t.Fatal("want abort once mains power is confirmed back and nothing was queued")
	}
	for _, name := range f.fired {
		if name == "forced" || name == "sysrq" || name == "syscall" {
			t.Fatalf("drastic mechanism %q ran on a box with confirmed mains power: %v", name, f.fired)
		}
	}
}

// Once systemd accepted a shutdown the teardown is already running: a later
// `normal` reading must NOT stop the escalation, or we strand a half-torn-down
// box.
func TestPowerOffDoesNotAbortOnceQueued(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{"poweroff": true, "forced": true, "sysrq": true, "syscall": true}}
	setupPowerOffEnv(t, f)
	fetchForza = func(string) (ForzaPower, error) {
		return ForzaPower{Advice: "normal", State: "online", Reachable: true}, nil
	}
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	if aborted := p.executePowerOff(); aborted {
		t.Fatal("aborted after systemd had already accepted the poweroff — the teardown cannot be undone")
	}
	if len(f.fired) != 5 {
		t.Fatalf("mechanisms = %v, want the full ladder once committed", f.fired)
	}
}

// A retry must be possible but rate-limited: the poller runs every 5s on
// battery and re-stopping every container that often would be its own outage.
func TestOrderedShutdownRetryIsRateLimitedThenAllowed(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{
		"systemctl poweroff": true, "poweroff": true,
		"forced": true, "sysrq": true, "syscall": true,
	}}
	setupPowerOffEnv(t, f)
	p := &PowerOrchestrator{cfg: shutdownCfg()}

	p.beginOrderedShutdown(ForzaPower{Advice: "shutdown_now"})
	firstRound := len(f.fired)

	p.beginOrderedShutdown(ForzaPower{Advice: "shutdown_now"}) // immediately again
	if len(f.fired) != firstRound {
		t.Fatalf("second attempt ran %d extra mechanisms inside the backoff window", len(f.fired)-firstRound)
	}

	p.mu.Lock()
	p.shutdownRetryAfter = time.Now().Add(-time.Second) // backoff elapsed
	p.mu.Unlock()
	p.beginOrderedShutdown(ForzaPower{Advice: "shutdown_now"})
	if len(f.fired) <= firstRound {
		t.Fatal("shutdown never retried after the backoff elapsed — the box would run until the battery dies")
	}
}

// The dangerous half of the escalation is the deadline: when it expires we cut
// power. It must NOT expire while systemd is genuinely tearing down (unmounting
// terabytes is slow, and forcing a cut there is the dirty shutdown we are
// preventing), and it MUST expire when nothing is happening.
func TestPoweroffWaitIsPatientOnlyWhileSystemdIsStopping(t *testing.T) {
	origSleep, origGrace, origForced, origCap, origStopping :=
		powerOffSleep, poweroffGrace, poweroffForcedGrace, poweroffPatienceCap, systemIsStopping
	powerOffSleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	poweroffGrace = 20 * time.Millisecond
	poweroffForcedGrace = 10 * time.Millisecond
	poweroffPatienceCap = 200 * time.Millisecond
	t.Cleanup(func() {
		powerOffSleep, poweroffGrace, poweroffForcedGrace, poweroffPatienceCap, systemIsStopping =
			origSleep, origGrace, origForced, origCap, origStopping
	})

	systemIsStopping = func() bool { return false }
	start := time.Now()
	poweroffWait(false)
	if idle := time.Since(start); idle >= poweroffPatienceCap {
		t.Fatalf("waited %v with systemd idle, want escalation at the %v grace", idle, poweroffGrace)
	}

	systemIsStopping = func() bool { return true }
	start = time.Now()
	poweroffWait(false)
	if busy := time.Since(start); busy < poweroffPatienceCap {
		t.Fatalf("waited only %v while systemd was tearing down, want patience up to %v — a forced cut mid-unmount is the dirty shutdown we are preventing",
			busy, poweroffPatienceCap)
	}

	// A drastic rung bypasses the unit teardown, so systemd's opinion is
	// irrelevant and waiting only burns battery.
	systemIsStopping = func() bool { return true }
	start = time.Now()
	poweroffWait(true)
	if forced := time.Since(start); forced >= poweroffPatienceCap {
		t.Fatalf("drastic rung waited %v, want the short %v window", forced, poweroffForcedGrace)
	}
}

// The escalation algorithm is only worth anything if the LIVE rung table is
// really several different mechanisms — every other test in this file replaces
// it with fakes, so nothing else would notice a ladder collapsed back into "run
// systemctl poweroff five times". Inspected, never executed: running a rung here
// would power off the developer's machine.
func TestRealPoweroffLadderIsSeveralDistinctMechanisms(t *testing.T) {
	rungs := poweroffRungs()
	if len(rungs) < 4 {
		t.Fatalf("live escalation has %d rungs, want the full ladder — a single mechanism is the original bug", len(rungs))
	}
	seen := map[string]bool{}
	for _, r := range rungs {
		if r.name == "" {
			t.Fatal("unnamed rung — the escalation logs would be unreadable during the one outage that matters")
		}
		if seen[r.name] {
			t.Fatalf("rung %q appears twice: retrying the same mechanism is not an escalation", r.name)
		}
		seen[r.name] = true
		if r.run == nil {
			t.Fatalf("rung %q has no implementation", r.name)
		}
	}
	// Cleanest first: the orderly rungs (which let systemd tear units down) must
	// precede every rung that cuts power without a teardown, and the drastic ones
	// must be MARKED drastic or executePowerOff would fire them without
	// re-checking that mains power is still out.
	orderly := 0
	for i, r := range rungs {
		if !r.drastic {
			if i != orderly {
				t.Fatalf("orderly rung %q sits after a drastic one: %v", r.name, rungNames(rungs))
			}
			orderly++
		}
	}
	if orderly == 0 || orderly == len(rungs) {
		t.Fatalf("ladder must mix orderly and drastic rungs, got %d orderly of %d: %v",
			orderly, len(rungs), rungNames(rungs))
	}
}

func rungNames(rungs []powerOffRung) []string {
	names := make([]string, 0, len(rungs))
	for _, r := range rungs {
		names = append(names, r.name)
	}
	return names
}

// WIRING for B10: the retry only matters if the REAL poll loop reaches it. This
// drives pollOnce end to end — hysteresis, the first attempt, the backoff, and
// the retry once it elapses. Mutation that reproduces the original bug: latch
// `acting` (drop its release in beginOrderedShutdown) and the retry assertion
// fails, because the `!acting` gate in pollOnce silences the poller forever.
func TestPollOnceDrivesTheShutdownRetryAfterAFailedPoweroff(t *testing.T) {
	f := &fakePowerOff{fail: map[string]bool{
		"systemctl poweroff": true, "poweroff": true,
		"forced": true, "sysrq": true, "syscall": true,
	}}
	setupPowerOffEnv(t, f)
	fetchForza = func(string) (ForzaPower, error) {
		return ForzaPower{State: "low_battery", OnBattery: true, Advice: "shutdown_now", Reachable: true}, nil
	}
	p := &PowerOrchestrator{cfg: shutdownCfg()} // ShutdownConfirm = 2

	p.pollOnce()
	if len(f.fired) != 0 {
		t.Fatalf("acted on a single shutdown_now poll: %v — the confirm hysteresis is gone", f.fired)
	}
	p.pollOnce()
	firstRound := len(f.fired)
	if firstRound == 0 {
		t.Fatal("poll loop never started the ordered shutdown after the confirm streak")
	}

	p.pollOnce() // still inside the backoff window
	if len(f.fired) != firstRound {
		t.Fatalf("poll loop re-ran the shutdown %d mechanisms deep inside the backoff", len(f.fired)-firstRound)
	}

	p.mu.Lock()
	p.shutdownRetryAfter = time.Now().Add(-time.Second)
	p.mu.Unlock()
	p.pollOnce()
	if len(f.fired) <= firstRound {
		t.Fatal("poll loop never retried the failed poweroff — the box runs until the battery dies, the exact dirty cut this module prevents")
	}
}

// ── B9: the ladder may not outlive the evidence that justified it ─────────

func TestBatteryConfirmedOnlyOnPositiveEvidence(t *testing.T) {
	cases := []struct {
		name string
		pw   ForzaPower
		want bool
	}{
		{"on_battery flag", ForzaPower{OnBattery: true}, true},
		{"state on_battery", ForzaPower{State: "on_battery"}, true},
		{"state low_battery", ForzaPower{State: "low_battery"}, true},
		{"advice prepare", ForzaPower{Advice: "prepare_shutdown"}, true},
		{"advice shutdown_now", ForzaPower{Advice: "shutdown_now"}, true},
		{"mains", ForzaPower{State: "online", Advice: "normal"}, false},
		{"garbage advice on a 200", ForzaPower{State: "unknown", Advice: "wat"}, false},
		{"empty body", ForzaPower{}, false},
	}
	for _, c := range cases {
		if got := batteryConfirmed(c.pw); got != c.want {
			t.Fatalf("%s: batteryConfirmed = %v, want %v", c.name, got, c.want)
		}
	}
}

// ladderApplied puts an orchestrator in the exact post-battery-event state the
// bug describes: levers applied, ladder Active, Forza silent since `age`.
func ladderApplied(t *testing.T, age time.Duration) (*PowerOrchestrator, *fakeTuning, *ladderTestEnv) {
	t.Helper()
	env := setupLadderEnv(t, []string{"navidrome"})
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
	}}
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	p.applyLadder(p.cfg)
	p.mu.Lock()
	p.ladderConfirmedAt = time.Now().Add(-age)
	p.mu.Unlock()
	return p, drv, env
}

func waitLadderIdle(t *testing.T, p *PowerOrchestrator) {
	t.Helper()
	for i := 0; i < 200; i++ {
		p.mu.RLock()
		busy := p.ladderBusy
		p.mu.RUnlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ladder goroutine never finished")
}

// The core of B9: without a single reading confirming battery, the degraded
// state expires. Mutation that reproduces the original bug: make
// enforceLadderTimeout return before tripping (e.g. drop the
// `unconfirmed >= maxUnconfirmed` branch) and this fails — the ladder stays
// applied forever.
func TestLadderRevertsWhenBatteryStaysUnconfirmed(t *testing.T) {
	p, drv, env := ladderApplied(t, 31*time.Minute)

	p.enforceLadderTimeout(p.cfg)
	waitLadderIdle(t, p)

	if st := p.LadderState(); st.Active {
		t.Fatalf("ladder still applied after %d min without confirmation: %+v", p.cfg.LadderMaxUnconfirmedMin, st)
	}
	if len(env.unpaused) != 1 || env.unpaused[0] != "navidrome" {
		t.Fatalf("unpaused = %v, want the frozen app released", env.unpaused)
	}
	if got := drv.governors; len(got) != 2 || got[1] != "ondemand" {
		t.Fatalf("governor calls = %v, want restore to ondemand", got)
	}
}

// The valve must not fire while Forza keeps confirming the outage — the module
// exists to stay degraded exactly then.
func TestLadderHoldsWhileBatteryIsConfirmed(t *testing.T) {
	p, drv, env := ladderApplied(t, time.Minute)

	p.enforceLadderTimeout(p.cfg)
	waitLadderIdle(t, p)

	if st := p.LadderState(); !st.Active {
		t.Fatal("ladder reverted while the battery state was freshly confirmed")
	}
	if len(env.unpaused) != 0 || len(drv.governors) != 1 {
		t.Fatalf("levers touched while still on battery: unpaused=%v governors=%v", env.unpaused, drv.governors)
	}
}

// A ladder recovered with an unknown clock must start counting, not trip on a
// zero timestamp.
func TestLadderValveIgnoresUnknownClock(t *testing.T) {
	p, _, env := ladderApplied(t, 0)
	p.mu.Lock()
	p.ladderConfirmedAt = time.Time{}
	p.mu.Unlock()

	p.enforceLadderTimeout(p.cfg)
	waitLadderIdle(t, p)

	if st := p.LadderState(); !st.Active {
		t.Fatal("ladder reverted on an unknown confirmation clock instead of starting to count")
	}
	if len(env.unpaused) != 0 {
		t.Fatalf("unpaused = %v, want none", env.unpaused)
	}
	p.mu.RLock()
	zero := p.ladderConfirmedAt.IsZero()
	p.mu.RUnlock()
	if zero {
		t.Fatal("confirmation clock left at zero — the valve would never trip")
	}
}

// WIRING: the valve has to be reached from the real poll loop, not just be
// callable. Mutation that reproduces the original bug: delete the
// `else { p.enforceLadderTimeout(cfg) }` branch in pollOnce and this fails
// while every other B9 test still passes.
func TestPollOnceTripsTheValveWhenForzaIsUnreachable(t *testing.T) {
	p, _, env := ladderApplied(t, 31*time.Minute)
	orig := fetchForza
	fetchForza = func(string) (ForzaPower, error) { return ForzaPower{}, errors.New("connection refused") }
	t.Cleanup(func() { fetchForza = orig })

	p.pollOnce()
	waitLadderIdle(t, p)

	if st := p.LadderState(); st.Active {
		t.Fatalf("poll loop left the ladder applied with a dead Forza: %+v", st)
	}
	if len(env.unpaused) != 1 {
		t.Fatalf("unpaused = %v, want the frozen app released by the poll loop", env.unpaused)
	}
}

// WIRING, other direction: a fresh reading that confirms battery refreshes the
// clock through pollOnce, so a long outage never trips the valve.
func TestPollOnceRefreshesConfirmationDuringARealOutage(t *testing.T) {
	p, _, env := ladderApplied(t, 31*time.Minute)
	orig := fetchForza
	fetchForza = func(string) (ForzaPower, error) {
		return ForzaPower{State: "on_battery", OnBattery: true, Advice: "prepare_shutdown", Reachable: true}, nil
	}
	t.Cleanup(func() { fetchForza = orig })

	p.pollOnce()
	waitLadderIdle(t, p)

	if st := p.LadderState(); !st.Active {
		t.Fatal("ladder reverted during a confirmed outage — the valve must measure silence, not elapsed time")
	}
	if len(env.unpaused) != 0 {
		t.Fatalf("unpaused = %v during a confirmed outage", env.unpaused)
	}
}

// A partial revert keeps the ladder Active; the valve must retry it, but not on
// every poll.
func TestLadderValveRetryIsRateLimited(t *testing.T) {
	env := setupLadderEnv(t, []string{"navidrome"})
	env.failUnp["navidrome"] = errors.New("transient docker daemon overload")
	drv := &fakeTuning{info: driverGovernorInfo{Supported: false}}
	cfg := ladderCfg()
	cfg.SpindownDisks = nil
	p := &PowerOrchestrator{cfg: cfg, driver: drv}
	p.applyLadder(cfg)
	p.mu.Lock()
	p.ladderConfirmedAt = time.Now().Add(-31 * time.Minute)
	p.mu.Unlock()

	attempts := 0
	inner := unpauseContainer
	unpauseContainer = func(name string) error {
		attempts++
		return inner(name)
	}

	p.enforceLadderTimeout(cfg)
	waitLadderIdle(t, p)
	p.mu.RLock()
	expired, expiredAt, stillActive := p.ladderExpired, p.ladderExpiredAt, p.ladder.Active
	p.mu.RUnlock()
	if !expired {
		t.Fatal("valve did not trip after the unconfirmed limit")
	}
	if !stillActive {
		t.Fatal("a container that could not be unpaused must keep the ladder tracked")
	}
	if attempts != 1 {
		t.Fatalf("unpause attempts = %d, want 1", attempts)
	}

	p.enforceLadderTimeout(cfg) // immediately again — inside the retry interval
	waitLadderIdle(t, p)
	if attempts != 1 {
		t.Fatalf("unpause attempts = %d after an immediate re-check, want the retry rate-limited", attempts)
	}

	// Past the retry interval the valve tries again — and this time docker
	// cooperates, so the ladder finally clears.
	delete(env.failUnp, "navidrome")
	p.mu.Lock()
	p.ladderExpiredAt = expiredAt.Add(-2 * ladderValveRetryInterval)
	p.mu.Unlock()
	p.enforceLadderTimeout(cfg)
	waitLadderIdle(t, p)
	if st := p.LadderState(); st.Active {
		t.Fatalf("valve never retried the partial revert: %+v", st)
	}
}

// An old config file (or an old UI payload) omits the new knob; it must land on
// the default instead of 0, which would revert the ladder instantly.
func TestPowerConfigNormalizesTheValveLimit(t *testing.T) {
	c := normalizePowerConfig(PowerConfig{})
	if c.LadderMaxUnconfirmedMin != defaultLadderMaxUnconfirmedMin {
		t.Fatalf("LadderMaxUnconfirmedMin = %d, want %d", c.LadderMaxUnconfirmedMin, defaultLadderMaxUnconfirmedMin)
	}
	if c.ShutdownConfirm != 2 || c.IdleWatts <= 0 || c.MaxWatts <= c.IdleWatts {
		t.Fatalf("normalization regressed the pre-existing knobs: %+v", c)
	}
	// ForzaURL has no default host: an empty UPS URL means "no UPS configured"
	// and must normalize to itself, not get filled in behind the caller's back.
	if c.ForzaURL != "" {
		t.Fatalf("normalization invented a ForzaURL out of thin air: %+v", c)
	}
}
