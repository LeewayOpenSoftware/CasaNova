package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// TuningService reads and writes kernel tunables that live in sysfs/procfs:
// the CPU frequency governor and vm.swappiness. It is the natural home for
// these because casanova-driver.service is the one root service WITHOUT
// ProtectKernelTunables — the main casanova.service has it set, so /proc/sys
// and /sys are read-only there. Every write is validated against what the
// running kernel actually exposes (the sysfs file IS the allowlist), never a
// hardcoded list, so a value the kernel would reject can't be sent.
type TuningService interface {
	CPUGovernor() (CPUGovernorInfo, error)
	// persist=false applies the value WITHOUT recording it as the user's
	// preference in host-tuning.json — for transient callers like the power-
	// saving ladder, so a reboot's Reapply restores what the USER chose.
	SetCPUGovernor(governor string, persist bool) error
	SetEPP(pref string, persist bool) error
	Swappiness() (SwappinessInfo, error)
	SetSwappiness(value int) error
	// P2/P3
	IOSchedulers() ([]DiskScheduler, error)
	SetIOScheduler(disk, scheduler string) error
	SetReadAhead(disk string, kb int) error
	SetSpindown(disk, timeout string) error
	// StandbyNow puts a rotational disk into standby immediately (hdparm -y).
	// Transient by design: nothing is persisted, and any disk access wakes it.
	StandbyNow(disk string) error
	Sysctls() ([]SysctlEntry, error)
	SetSysctl(key, value string) error
	Zram() (ZramInfo, error)
	SetZram(enable bool, algorithm string, sizeMiB int) (ZramInfo, error)
	// Reapply re-applies the persisted tuning after a reboot, re-validating
	// each value against the current hardware. Best-effort: a value that no
	// longer applies is skipped with a warning, never a hard failure.
	Reapply() []string
}

const (
	sysfsCPUBase    = "/sys/devices/system/cpu"
	sysfsBlock      = "/sys/block"
	procSwappiness  = "/proc/sys/vm/swappiness"
	procMeminfo     = "/proc/meminfo"
	tuningStateFile = "/var/lib/casanova/host-tuning.json"
)

// CPUGovernorInfo is the state of the CPU frequency governor across all cores.
type CPUGovernorInfo struct {
	Available []string `json:"available"` // from scaling_available_governors
	Current   string   `json:"current"`   // the common governor, or "mixed"
	PerCore   []string `json:"perCore"`   // governor of each cpuN, in order
	Driver    string   `json:"driver"`    // scaling_driver, e.g. intel_pstate
	Supported bool     `json:"supported"` // false when no cpufreq (e.g. in a VM)
	// EPP (P3): energy_performance_preference, only present on intel_pstate /
	// amd-pstate in active mode. Empty EPPAvailable => not supported here.
	EPP          string   `json:"epp"`
	EPPAvailable []string `json:"eppAvailable"`
}

// SwappinessInfo is vm.swappiness plus read-only swap context for the UI.
type SwappinessInfo struct {
	Value        int   `json:"value"`        // /proc/sys/vm/swappiness [0..100]
	SwapTotalKiB int64 `json:"swapTotalKiB"` // MemInfo SwapTotal
	SwapFreeKiB  int64 `json:"swapFreeKiB"`  // MemInfo SwapFree
	MemTotalKiB  int64 `json:"memTotalKiB"`  // MemInfo MemTotal
}

type tuningService struct{}

func NewTuningService() TuningService { return &tuningService{} }

// writeSysAttr writes a single value to a sysfs/procfs attribute. sysfs
// attributes ignore the file mode and the O_TRUNC that os.WriteFile applies;
// a plain write of the value is the correct, idiomatic way to set them.
func writeSysAttr(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// cpuGovernorPaths returns every cpuN/cpufreq/scaling_governor path present,
// sorted by core index. Empty when the platform has no cpufreq (common under
// virtualization) — callers treat that as "unsupported", not an error.
func cpuGovernorPaths() []string {
	matches, _ := filepath.Glob(filepath.Join(sysfsCPUBase, "cpu[0-9]*", "cpufreq", "scaling_governor"))
	// Glob already returns lexically sorted names; cpu2 sorts before cpu10
	// lexically-wrong, but governor application is order-independent so the
	// display order being lexical (cpu0, cpu1, cpu10, cpu2) is cosmetic only.
	return matches
}

func (s *tuningService) CPUGovernor() (CPUGovernorInfo, error) {
	paths := cpuGovernorPaths()
	if len(paths) == 0 {
		return CPUGovernorInfo{Supported: false, Available: []string{}, PerCore: []string{}}, nil
	}

	info := CPUGovernorInfo{Supported: true}
	cpu0 := filepath.Join(sysfsCPUBase, "cpu0", "cpufreq")
	if avail := readSysAttr(cpu0, "scaling_available_governors"); avail != "" {
		info.Available = strings.Fields(avail)
	} else {
		info.Available = []string{}
	}
	info.Driver = readSysAttr(cpu0, "scaling_driver")

	info.PerCore = make([]string, 0, len(paths))
	for _, p := range paths {
		g := readSysAttr(filepath.Dir(p), "scaling_governor")
		info.PerCore = append(info.PerCore, g)
	}
	// Current = the common value across cores, else "mixed".
	info.Current = "mixed"
	if len(info.PerCore) > 0 {
		first := info.PerCore[0]
		same := true
		for _, g := range info.PerCore {
			if g != first {
				same = false
				break
			}
		}
		if same {
			info.Current = first
		}
	}
	// EPP (best-effort; absent on acpi-cpufreq / passive mode). cpu0 is the
	// cpufreq dir already resolved above.
	info.EPP = readSysAttr(cpu0, "energy_performance_preference")
	if avail := readSysAttr(cpu0, "energy_performance_available_preferences"); avail != "" {
		info.EPPAvailable = strings.Fields(avail)
	} else {
		info.EPPAvailable = []string{}
	}
	return info, nil
}

// SetEPP writes energy_performance_preference to every core, validated against
// energy_performance_available_preferences (the sysfs allowlist).
func (s *tuningService) SetEPP(pref string, persist bool) error {
	pref = strings.TrimSpace(pref)
	cpu0 := filepath.Join(sysfsCPUBase, "cpu0", "cpufreq")
	avail := strings.Fields(readSysAttr(cpu0, "energy_performance_available_preferences"))
	if len(avail) == 0 {
		return fmt.Errorf("energy performance preference is not available on this CPU")
	}
	if !contains(avail, pref) {
		return fmt.Errorf("EPP %q not available (allowed: %s)", pref, strings.Join(avail, ", "))
	}
	matches, _ := filepath.Glob(filepath.Join(sysfsCPUBase, "cpu[0-9]*", "cpufreq", "energy_performance_preference"))
	var failed int
	for _, p := range matches {
		if err := writeSysAttr(p, pref); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("EPP applied but %d core(s) rejected it", failed)
	}
	if persist {
		s.persist(func(st *tuningState) { st.EPP = pref })
	}
	return nil
}

func (s *tuningService) SetCPUGovernor(governor string, persist bool) error {
	governor = strings.TrimSpace(governor)
	info, err := s.CPUGovernor()
	if err != nil {
		return err
	}
	if !info.Supported {
		return fmt.Errorf("CPU frequency scaling is not available on this system")
	}
	// Membership check against what the kernel actually offers — the sysfs
	// file is the allowlist, so an invalid governor can never reach a write.
	if !contains(info.Available, governor) {
		return fmt.Errorf("governor %q not available (allowed: %s)", governor, strings.Join(info.Available, ", "))
	}

	var failed []string
	for _, p := range cpuGovernorPaths() {
		if err := writeSysAttr(p, governor); err != nil {
			failed = append(failed, filepath.Base(filepath.Dir(filepath.Dir(p))))
		}
	}
	if len(failed) > 0 {
		// Partial failure (e.g. big.LITTLE where one cluster rejects the
		// value). Report which cores so the UI can show a mixed state.
		return fmt.Errorf("governor applied but %d core(s) rejected it: %s", len(failed), strings.Join(failed, ", "))
	}
	if persist {
		s.persist(func(st *tuningState) { st.CPUGovernor = governor })
	}
	return nil
}

func (s *tuningService) Swappiness() (SwappinessInfo, error) {
	raw, err := os.ReadFile(procSwappiness)
	if err != nil {
		return SwappinessInfo{}, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return SwappinessInfo{}, fmt.Errorf("parse swappiness: %w", err)
	}
	info := SwappinessInfo{Value: v}
	info.MemTotalKiB = meminfoField("MemTotal")
	info.SwapTotalKiB = meminfoField("SwapTotal")
	info.SwapFreeKiB = meminfoField("SwapFree")
	return info, nil
}

func (s *tuningService) SetSwappiness(value int) error {
	// Clamp to the kernel-legal range. Every value in [0,100] is accepted by
	// the kernel and only affects performance, so clamping (not rejecting) is
	// the friendly, safe choice.
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	if err := writeSysAttr(procSwappiness, strconv.Itoa(value)); err != nil {
		return err
	}
	s.persist(func(st *tuningState) { v := value; st.Swappiness = &v })
	return nil
}

// ── Persistence ─────────────────────────────────────────────────────────
// A single JSON file records the user's chosen values so a reboot (which
// resets sysfs to kernel defaults) can re-apply them. Kept minimal: only the
// values the user actually set are stored (pointer/empty => "not managed").

type tuningState struct {
	Version      int               `json:"version"`
	CPUGovernor  string            `json:"cpu_governor,omitempty"`
	EPP          string            `json:"epp,omitempty"` // energy_performance_preference
	Swappiness   *int              `json:"swappiness,omitempty"`
	IOSchedulers map[string]string `json:"io_schedulers,omitempty"` // by-id -> scheduler
	ReadAhead    map[string]int    `json:"read_ahead,omitempty"`    // by-id -> read_ahead_kb
	Spindown     map[string]string `json:"spindown,omitempty"`      // by-id -> timeout key
	Sysctls      map[string]string `json:"sysctls,omitempty"`       // key -> value
	Zram         *ZramState        `json:"zram,omitempty"`
}

func loadTuningState() tuningState {
	st := tuningState{Version: 1}
	if raw, err := os.ReadFile(tuningStateFile); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

// tuningStateMu serialises the load-modify-save in persist(). Without it two
// concurrent mutations (e.g. governor + swappiness set back-to-back, or a set
// racing the boot reapply) could each read the old file and the later write
// would drop the earlier one's field.
var tuningStateMu sync.Mutex

func (s *tuningService) persist(mutate func(*tuningState)) {
	tuningStateMu.Lock()
	defer tuningStateMu.Unlock()
	st := loadTuningState()
	st.Version = 1
	mutate(&st)
	if raw, err := json.MarshalIndent(st, "", "  "); err == nil {
		// Atomic: this file is the ONLY record of every persisted knob
		// (governor, EPP, swappiness, spindown, sysctl...). os.WriteFile
		// truncates first, so one crash mid-write erased all of it at once
		// — and unlike a bad value, an absent file fails silently: Reapply
		// simply applies nothing and the box quietly runs untuned.
		_ = writeTuningFileAtomic(tuningStateFile, raw, 0o600)
	}
}

func (s *tuningService) Reapply() []string {
	st := loadTuningState()
	var notes []string
	if st.CPUGovernor != "" {
		// persist=false: the values come FROM the state file, no need to
		// rewrite it.
		if err := s.SetCPUGovernor(st.CPUGovernor, false); err != nil {
			notes = append(notes, "cpu governor: "+err.Error())
		}
	}
	if st.EPP != "" {
		if err := s.SetEPP(st.EPP, false); err != nil {
			notes = append(notes, "epp: "+err.Error())
		}
	}
	if st.Swappiness != nil {
		if err := s.SetSwappiness(*st.Swappiness); err != nil {
			notes = append(notes, "swappiness: "+err.Error())
		}
	}
	// Disk-indexed knobs: resolve the persisted by-id back to the current
	// kernel name (sdX can reorder across boots) and skip absent disks.
	for byID, sched := range st.IOSchedulers {
		if name := nameByID(byID); name != "" {
			if err := s.setIOSchedulerByName(name, sched); err != nil {
				notes = append(notes, "io-scheduler "+byID+": "+err.Error())
			}
		}
	}
	for byID, kb := range st.ReadAhead {
		if name := nameByID(byID); name != "" {
			_ = writeSysAttr(filepath.Join(sysfsBlock, name, "queue", "read_ahead_kb"), strconv.Itoa(kb))
		}
	}
	for byID, tk := range st.Spindown {
		if name := nameByID(byID); name != "" {
			if err := s.setSpindownByName(name, tk); err != nil {
				notes = append(notes, "spindown "+byID+": "+err.Error())
			}
		}
	}
	for key, val := range st.Sysctls {
		if err := s.SetSysctl(key, val); err != nil {
			notes = append(notes, "sysctl "+key+": "+err.Error())
		}
	}
	if st.Zram != nil && st.Zram.Enabled {
		if err := s.enableZram(st.Zram.Algorithm, st.Zram.SizeMiB); err != nil {
			notes = append(notes, "zram: "+err.Error())
		}
	}
	return notes
}

// ── helpers ─────────────────────────────────────────────────────────────

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// meminfoField returns a /proc/meminfo value in KiB (the unit meminfo uses for
// these fields), or 0 if absent/unparseable.
func meminfoField(key string) int64 {
	raw, err := os.ReadFile(procMeminfo)
	if err != nil {
		return 0
	}
	prefix := key + ":"
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(fields) >= 1 {
				if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// writeTuningFileAtomic publishes a file with rename(2) so a crash mid-write can
// never leave a torn copy at the real path. The file this protects is the
// one that decides whether UPS orchestration runs at all: a truncated JSON
// here fails to parse at boot, the orchestrator starts unconfigured, and
// nothing says so until the next power cut — the exact moment the loss is
// unrecoverable. fsync before rename, because rename ordering without
// content durability is how "atomic" writes still produce empty files after
// power loss.
func writeTuningFileAtomic(path string, data []byte, mode os.FileMode) error {
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
