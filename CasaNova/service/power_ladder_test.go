package service

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeTuning records driver calls and serves canned governor info.
type fakeTuning struct {
	mu        sync.Mutex
	info      driverGovernorInfo
	infoErr   error
	governors []string // SetCPUGovernor calls, in order
	epps      []string // SetEPP calls, in order
	standby   []string // StandbyDisk calls, in order
	failEPP   bool
}

func (f *fakeTuning) CPUGovernor() (driverGovernorInfo, error) { return f.info, f.infoErr }
func (f *fakeTuning) SetCPUGovernor(g string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.governors = append(f.governors, g)
	return nil
}
func (f *fakeTuning) SetEPP(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEPP {
		return errors.New("epp write failed")
	}
	f.epps = append(f.epps, p)
	return nil
}
func (f *fakeTuning) StandbyDisk(d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standby = append(f.standby, d)
	return nil
}

// ladderTestEnv swaps the docker/settle package hooks and the state file for
// one test, restoring them on cleanup.
type ladderTestEnv struct {
	running   []string
	paused    []string
	unpaused  []string
	failPause map[string]bool
	failUnp   map[string]error
}

func setupLadderEnv(t *testing.T, running []string) *ladderTestEnv {
	t.Helper()
	env := &ladderTestEnv{running: running, failPause: map[string]bool{}, failUnp: map[string]error{}}
	origList, origPause, origUnpause, origSettle := listRunningContainers, pauseContainer, unpauseContainer, ladderSettle
	origWatts := ladderWattsSample
	origFile := ladderStateFile
	listRunningContainers = func() []string { return env.running }
	pauseContainer = func(name string) error {
		if env.failPause[name] {
			return errors.New("pause failed")
		}
		env.paused = append(env.paused, name)
		return nil
	}
	unpauseContainer = func(name string) error {
		if e := env.failUnp[name]; e != nil {
			return e
		}
		env.unpaused = append(env.unpaused, name)
		return nil
	}
	ladderSettle = func() {}
	ladderWattsSample = func(PowerConfig) float64 { return 42 }
	ladderStateFile = filepath.Join(t.TempDir(), "ladder-state.json")
	t.Cleanup(func() {
		listRunningContainers, pauseContainer, unpauseContainer, ladderSettle = origList, origPause, origUnpause, origSettle
		ladderWattsSample = origWatts
		ladderStateFile = origFile
	})
	return env
}

func ladderCfg() PowerConfig {
	c := defaultPowerConfig()
	// fetchForza is always mocked in these tests, but pollOnce now treats an
	// empty ForzaURL as "no UPS configured" and skips the fetch entirely —
	// these tests are exercising a configured UPS, so give it a fixture URL.
	c.ForzaURL = "http://192.0.2.10:9090"
	c.Enabled = true
	c.SavingsLadder = true
	c.SpindownDisks = []string{"sda", "sdb"}
	c.PauseNonEssential = true
	c.EssentialApps = []string{"mariadb"}
	return c
}

func TestLadderApplyAllLevers(t *testing.T) {
	env := setupLadderEnv(t, []string{"mariadb", "navidrome", "jellyfin"})
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
		EPP: "balance_performance", EPPAvailable: []string{"power", "balance_performance"},
	}}
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}

	p.applyLadder(p.cfg)

	if got := drv.governors; len(got) != 1 || got[0] != "powersave" {
		t.Fatalf("governor calls = %v, want [powersave]", got)
	}
	if got := drv.epps; len(got) != 1 || got[0] != "power" {
		t.Fatalf("epp calls = %v, want [power]", got)
	}
	if got := drv.standby; len(got) != 2 || got[0] != "sda" || got[1] != "sdb" {
		t.Fatalf("standby calls = %v, want [sda sdb]", got)
	}
	// mariadb is essential — must NOT be paused.
	if len(env.paused) != 2 || env.paused[0] != "navidrome" || env.paused[1] != "jellyfin" {
		t.Fatalf("paused = %v, want [navidrome jellyfin]", env.paused)
	}
	st := p.LadderState()
	if !st.Active {
		t.Fatal("ladder should be active after apply")
	}
	wantLevers := []string{"governor_powersave", "epp_power", "disks_standby", "apps_paused"}
	if len(st.Levers) != len(wantLevers) {
		t.Fatalf("levers = %v, want %v", st.Levers, wantLevers)
	}
	for i := range wantLevers {
		if st.Levers[i] != wantLevers[i] {
			t.Fatalf("levers = %v, want %v", st.Levers, wantLevers)
		}
	}
	// Pre-state captured for the revert.
	p.mu.RLock()
	prevGov, prevEPP := p.ladder.PrevGovernor, p.ladder.PrevEPP
	p.mu.RUnlock()
	if prevGov != "ondemand" || prevEPP != "balance_performance" {
		t.Fatalf("prev state = (%q,%q), want (ondemand,balance_performance)", prevGov, prevEPP)
	}
	// State persisted for crash recovery.
	if _, err := os.Stat(ladderStateFile); err != nil {
		t.Fatalf("state file not persisted: %v", err)
	}
}

func TestLadderSkipsLeversAlreadyOptimal(t *testing.T) {
	setupLadderEnv(t, nil)
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "powersave", Available: []string{"ondemand", "powersave"},
		EPP: "power", EPPAvailable: []string{"power"},
	}}
	cfg := ladderCfg()
	cfg.SpindownDisks = nil
	cfg.PauseNonEssential = false
	p := &PowerOrchestrator{cfg: cfg, driver: drv}

	p.applyLadder(cfg)

	if len(drv.governors) != 0 || len(drv.epps) != 0 || len(drv.standby) != 0 {
		t.Fatalf("no driver mutations expected, got gov=%v epp=%v standby=%v",
			drv.governors, drv.epps, drv.standby)
	}
	// Ladder is still marked active (so revert hysteresis applies), just with
	// zero levers.
	if st := p.LadderState(); !st.Active || len(st.Levers) != 0 {
		t.Fatalf("ladder view = %+v, want active with no levers", st)
	}
}

func TestLadderRevertRestoresEverything(t *testing.T) {
	env := setupLadderEnv(t, []string{"mariadb", "navidrome"})
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
		EPP: "balance_performance", EPPAvailable: []string{"power", "balance_performance"},
	}}
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	p.applyLadder(p.cfg)

	p.revertLadder("test")

	if len(env.unpaused) != 1 || env.unpaused[0] != "navidrome" {
		t.Fatalf("unpaused = %v, want [navidrome]", env.unpaused)
	}
	if got := drv.epps; len(got) != 2 || got[1] != "balance_performance" {
		t.Fatalf("epp calls = %v, want restore to balance_performance", got)
	}
	if got := drv.governors; len(got) != 2 || got[1] != "ondemand" {
		t.Fatalf("governor calls = %v, want restore to ondemand", got)
	}
	if st := p.LadderState(); st.Active {
		t.Fatalf("ladder still active after full revert: %+v", st)
	}
	if _, err := os.Stat(ladderStateFile); !os.IsNotExist(err) {
		t.Fatalf("state file should be removed after full revert, stat err=%v", err)
	}
}

func TestLadderPartialRevertKeepsLeftovers(t *testing.T) {
	env := setupLadderEnv(t, []string{"navidrome"})
	env.failUnp["navidrome"] = errors.New("transient docker daemon overload")
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
	}}
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	p.applyLadder(p.cfg)

	p.revertLadder("test")

	// Unpause failed → the container stays tracked and the ladder stays
	// active so the next stable-normal poll retries.
	p.mu.RLock()
	st := p.ladder
	p.mu.RUnlock()
	if !st.Active || len(st.Paused) != 1 || st.Paused[0] != "navidrome" {
		t.Fatalf("ladder state after partial revert = %+v, want active with navidrome tracked", st)
	}
	if _, err := os.Stat(ladderStateFile); err != nil {
		t.Fatalf("state file must survive a partial revert: %v", err)
	}
}

func TestLadderBootRecovery(t *testing.T) {
	env := setupLadderEnv(t, nil)
	drv := &fakeTuning{}
	// Simulate a previous run that died with levers applied.
	prev := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	prev.commitLadder(LadderState{
		Active: true, Levers: []string{"governor_powersave", "apps_paused"},
		PrevGovernor: "ondemand", Paused: []string{"navidrome"},
	})

	fresh := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	fresh.recoverLadderOnBoot()

	if len(env.unpaused) != 1 || env.unpaused[0] != "navidrome" {
		t.Fatalf("boot recovery unpaused = %v, want [navidrome]", env.unpaused)
	}
	if got := drv.governors; len(got) != 1 || got[0] != "ondemand" {
		t.Fatalf("boot recovery governor calls = %v, want [ondemand]", got)
	}
	if st := fresh.LadderState(); st.Active {
		t.Fatalf("ladder should be cleared after boot recovery: %+v", st)
	}
	if _, err := os.Stat(ladderStateFile); !os.IsNotExist(err) {
		t.Fatalf("state file should be gone after boot recovery, stat err=%v", err)
	}
}

func TestLadderBootRecoveryNoFile(t *testing.T) {
	env := setupLadderEnv(t, nil)
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: &fakeTuning{}}
	p.recoverLadderOnBoot() // must be a silent no-op
	if len(env.unpaused) != 0 {
		t.Fatalf("no recovery actions expected, unpaused=%v", env.unpaused)
	}
}

func TestUnpauseAllLadderForShutdown(t *testing.T) {
	env := setupLadderEnv(t, []string{"navidrome", "jellyfin"})
	cfg := ladderCfg()
	cfg.SpindownDisks = nil
	drv := &fakeTuning{info: driverGovernorInfo{Supported: false}}
	p := &PowerOrchestrator{cfg: cfg, driver: drv}
	p.applyLadder(cfg)

	p.unpauseAllLadder()

	if len(env.unpaused) != 2 {
		t.Fatalf("unpaused = %v, want both paused containers", env.unpaused)
	}
}

func TestLadderRevertClearsGoneContainers(t *testing.T) {
	// After a host reboot no container is paused: docker answers "is not
	// paused" / "no such container". Those must count as revert SUCCESS or
	// the ladder wedges Active forever.
	env := setupLadderEnv(t, []string{"navidrome", "jellyfin"})
	env.failUnp["navidrome"] = errors.New("Error response from daemon: Container navidrome is not paused")
	env.failUnp["jellyfin"] = errors.New("Error: No such container: jellyfin")
	cfg := ladderCfg()
	cfg.SpindownDisks = nil
	drv := &fakeTuning{info: driverGovernorInfo{Supported: false}}
	p := &PowerOrchestrator{cfg: cfg, driver: drv}
	p.applyLadder(cfg)

	p.revertLadder("test")

	if st := p.LadderState(); st.Active {
		t.Fatalf("ladder must clear when every unpause failure means 'already gone': %+v", st)
	}
	if _, err := os.Stat(ladderStateFile); !os.IsNotExist(err) {
		t.Fatalf("state file should be removed, stat err=%v", err)
	}
}

func TestLadderApplyAbortsWhenShutdownActing(t *testing.T) {
	env := setupLadderEnv(t, []string{"navidrome"})
	drv := &fakeTuning{info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
	}}
	p := &PowerOrchestrator{cfg: ladderCfg(), driver: drv}
	p.acting = true // ordered shutdown in progress

	p.applyLadder(p.cfg)

	if len(drv.governors) != 0 || len(env.paused) != 0 {
		t.Fatalf("no lever may run once acting: gov=%v paused=%v", drv.governors, env.paused)
	}
	if st := p.LadderState(); st.Active {
		t.Fatalf("ladder must not activate during a shutdown: %+v", st)
	}
}

func TestLadderEPPFailureDoesNotAbort(t *testing.T) {
	setupLadderEnv(t, nil)
	drv := &fakeTuning{failEPP: true, info: driverGovernorInfo{
		Supported: true, Current: "ondemand", Available: []string{"ondemand", "powersave"},
		EPP: "balance_performance", EPPAvailable: []string{"power", "balance_performance"},
	}}
	cfg := ladderCfg()
	cfg.SpindownDisks = []string{"sda"}
	cfg.PauseNonEssential = false
	p := &PowerOrchestrator{cfg: cfg, driver: drv}

	p.applyLadder(cfg)

	// Governor + disks applied even though EPP failed; EPP not in levers and
	// no PrevEPP recorded (nothing to revert).
	st := p.LadderState()
	want := []string{"governor_powersave", "disks_standby"}
	if len(st.Levers) != len(want) || st.Levers[0] != want[0] || st.Levers[1] != want[1] {
		t.Fatalf("levers = %v, want %v", st.Levers, want)
	}
	p.mu.RLock()
	prevEPP := p.ladder.PrevEPP
	p.mu.RUnlock()
	if prevEPP != "" {
		t.Fatalf("PrevEPP = %q, want empty after failed EPP lever", prevEPP)
	}
}
