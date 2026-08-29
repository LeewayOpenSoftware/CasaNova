package diagnosis

import (
	"context"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagehealth"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// BuildDefaultChecks returns the standard set of diagnostic checks for CasaNova.
// This is the suite that runs when a user clicks "Run Diagnostics" in the UI.
func BuildDefaultChecks() []Diagnoser {
	return []Diagnoser{
		CheckDockerDaemon,
		CheckDiskSpace,
		CheckMemory,
		CheckCPULoad,
		CheckGatewayPort,
		CheckInternetConnectivity,
		CheckDNSResolution,
		CheckSystemTime,
		CheckCasaNovaServices,
		CheckSSHRootLogin,
		CheckTokenRevocation,
		CheckStorageMaintenanceSafety,
	}
}

// CheckStorageMaintenanceSafety reports whether the guards that stop CasaNova
// formatting the wrong disk are still demonstrably working.
//
// It runs the non-destructive validation suite in pkg/storagehealth, whose
// load-bearing invariant is a negative boundary: this host's own root device
// must come back classified as protected and absent from the eligible set. A
// self-test that merely called the classifier and checked it returned something
// would pass just as happily with the classifier gutted.
//
// Nothing here writes to a block device or runs a mkfs or discard helper.
func CheckStorageMaintenanceSafety(ctx context.Context) Result {
	r := Result{
		Category: CategoryStorage,
		Check:    "storage-maintenance-safety",
		Title:    "Storage maintenance safety guards",
	}

	rep := storagehealth.Evaluate(ctx)
	switch rep.State {
	case storagehealth.StateHealthy:
		r.Severity = SeverityOK
		r.Summary = "Disk-protection, identity and job-state guards all validated"
		r.Details = "This host's own system disk is correctly classified as protected and is not " +
			"offered as a target; device identities are unique; the mount and holder tables are " +
			"readable; and job state survives a durable write/read round-trip."
		return r

	case storagehealth.StateDegraded:
		r.Severity = SeverityWarning
		r.Summary = "Destructive storage operations are being refused"
		r.Details = fmt.Sprintf("Failing invariant: %s. %s", rep.Invariant, rep.ActionableCause)
		r.Fix = rep.ActionableCause
		return r

	case storagehealth.StateValidationFailed:
		// A guard was demonstrated BROKEN. This is worse than unavailable.
		r.Severity = SeverityError
		r.Summary = "A disk-protection guard failed its own validation"
		r.Details = fmt.Sprintf("Failing invariant: %s. %s Do not run a format or a sanitize until "+
			"this is resolved.", rep.Invariant, rep.ActionableCause)
		r.Fix = rep.ActionableCause
		return r

	default:
		r.Severity = SeverityError
		r.Summary = "The storage-maintenance safety guards could not be validated"
		r.Details = fmt.Sprintf("Failing invariant: %s. %s Destructive storage operations are "+
			"refused rather than attempted unchecked.", rep.Invariant, rep.ActionableCause)
		r.Fix = rep.ActionableCause
		return r
	}
}

// CheckTokenRevocation reports whether the shared revocation authority is in
// force. This is the diagnostic the subsystem owes its operator: when it is
// unavailable every service refuses every token, and without this check the
// only symptom would be a box that suddenly rejects all logins with no stated
// reason. The failing invariant is reported verbatim.
func CheckTokenRevocation(ctx context.Context) Result {
	r := Result{
		Category: CategorySecurity,
		Check:    "token-revocation",
		Title:    "Token revocation authority",
	}

	h := tokenrevocation.CurrentHealth()
	switch h.Status {
	case tokenrevocation.StatusHealthy:
		r.Severity = SeverityOK
		r.Summary = fmt.Sprintf("Revocation is enforced (%d revoked account(s))", h.Entries)
		return r

	case tokenrevocation.StatusDegraded:
		r.Severity = SeverityWarning
		r.Summary = "Revocation is enforced from a cached copy"
		r.Details = fmt.Sprintf("The list at %s could not be re-read, so the last known copy is still "+
			"being applied. Accounts revoked from now on may not take effect on this service until it "+
			"can be read again. Failing invariant: %s (%s)", h.Path, h.Invariant, h.Detail)
		r.Fix = "Check that " + h.Path + " exists and is readable."
		return r

	default:
		// Unavailable or validation_failed: every token is being refused.
		r.Severity = SeverityError
		r.Summary = "Revocation cannot be verified — all sessions are being refused"
		r.Details = fmt.Sprintf("CasaNova refuses tokens rather than accept them unchecked when it "+
			"cannot consult the revocation list. Failing invariant: %s (%s)", h.Invariant, h.Detail)
		r.Fix = "Repair or remove " + h.Path + " and restart CasaNova. Removing it resets the box to " +
			"no revocations, so any deleted account's outstanding tokens would work again until they expire."
		return r
	}
}

// CheckDockerDaemon verifies the Docker daemon is running and reachable.
func CheckDockerDaemon(ctx context.Context) Result {
	r := Result{
		Category: CategoryContainer,
		Check:    "docker-daemon",
		Title:    "Docker daemon",
	}

	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.Output()
	if err != nil {
		r.Severity = SeverityError
		r.Summary = "Docker daemon is not reachable"
		r.Details = "The Docker daemon is not running or CasaNova cannot connect to it. " +
			"All containerized apps will be unavailable until this is fixed."
		r.Fix = "Start the Docker service: sudo systemctl start docker"
		return r
	}

	version := strings.TrimSpace(string(out))
	r.Severity = SeverityOK
	r.Summary = "Docker is running (version " + version + ")"
	return r
}

// CheckDiskSpace verifies the root filesystem has enough free space.
// Warning at 85% full, error at 95%.
func CheckDiskSpace(ctx context.Context) Result {
	r := Result{
		Category: CategoryStorage,
		Check:    "disk-space-root",
		Title:    "Root filesystem space",
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		r.Severity = SeverityError
		r.Summary = "Could not read root filesystem stats"
		r.Details = err.Error()
		return r
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	used := total - free
	usedPct := float64(used) / float64(total) * 100

	r.Summary = fmt.Sprintf("%.1f%% used (%s free of %s)",
		usedPct, humanBytes(free), humanBytes(total))

	switch {
	case usedPct >= 95:
		r.Severity = SeverityError
		r.Details = "The root filesystem is almost full. Services may start failing. " +
			"Remove unused containers, images, or log files immediately."
		r.Fix = "Run: docker system prune -a  (removes unused Docker data)"
	case usedPct >= 85:
		r.Severity = SeverityWarning
		r.Details = "The root filesystem is getting full. Clean up before it becomes critical."
		r.Fix = "Review disk usage: du -h --max-depth=1 / | sort -rh"
	default:
		r.Severity = SeverityOK
	}
	return r
}

// CheckMemory verifies there is enough free memory.
func CheckMemory(ctx context.Context) Result {
	r := Result{
		Category: CategorySystem,
		Check:    "memory",
		Title:    "System memory",
	}

	// MemAvailable (kernel's own estimate of reclaimable memory) — NOT
	// Sysinfo.Freeram, which excludes page cache/buffers and made a healthy
	// box with a warm cache report >85% "used" (chronic false warnings).
	total, avail, err := readMeminfoAvailable()
	if err != nil {
		r.Severity = SeverityError
		r.Summary = "Could not read memory info"
		return r
	}

	used := total - avail
	usedPct := float64(used) / float64(total) * 100

	r.Summary = fmt.Sprintf("%.1f%% used (%s available of %s)",
		usedPct, humanBytes(avail), humanBytes(total))

	switch {
	case usedPct >= 95:
		r.Severity = SeverityError
		r.Details = "System memory is almost exhausted. Containers may be killed by the OOM killer."
		r.Fix = "Restart memory-hungry containers or add swap space"
	case usedPct >= 85:
		r.Severity = SeverityWarning
		r.Details = "System memory usage is high. Monitor for OOM kills."
	default:
		r.Severity = SeverityOK
	}
	return r
}

// CheckCPULoad checks the system load average against CPU count.
func CheckCPULoad(ctx context.Context) Result {
	r := Result{
		Category: CategorySystem,
		Check:    "cpu-load",
		Title:    "CPU load",
	}

	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		r.Severity = SeverityError
		r.Summary = "Could not read load average"
		return r
	}

	// info.Loads is scaled by 65536
	load1 := float64(info.Loads[0]) / 65536.0
	numCPU := runtime.NumCPU()
	loadPerCPU := load1 / float64(numCPU)

	r.Summary = fmt.Sprintf("load: %.2f on %d CPUs (%.0f%% utilization)",
		load1, numCPU, loadPerCPU*100)

	switch {
	case loadPerCPU >= 2.0:
		r.Severity = SeverityError
		r.Details = "CPU is severely overloaded. Identify and stop runaway processes."
		r.Fix = "Check top processes: top -b -n1 | head -20"
	case loadPerCPU >= 1.0:
		r.Severity = SeverityWarning
		r.Details = "CPU load is high. Some apps may feel slow."
	default:
		r.Severity = SeverityOK
	}
	return r
}

// CheckGatewayPort verifies the gateway port is bound and responding.
func CheckGatewayPort(ctx context.Context) Result {
	r := Result{
		Category: CategoryNetwork,
		Check:    "gateway-port",
		Title:    "Gateway HTTP port",
	}

	// Probe the deployed default first (85), then the historical fallbacks.
	// TODO: read the live port from the gateway's management API instead of
	// probing a fixed list.
	dialer := net.Dialer{Timeout: 3 * time.Second}
	var okPort string
	for _, port := range []string{"85", "80", "8080"} {
		conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+port)
		if err == nil {
			conn.Close()
			okPort = port
			break
		}
	}
	if okPort == "" {
		r.Severity = SeverityError
		r.Summary = "Gateway is not accepting connections on port 85, 80 or 8080"
		r.Details = "The CasaNova gateway is not bound to an HTTP port. " +
			"The web UI and proxied apps will be unreachable."
		r.Fix = "Check service status: systemctl status casanova-gateway"
		return r
	}
	r.Summary = "Gateway responding on port " + okPort
	r.Severity = SeverityOK
	return r
}

// CheckInternetConnectivity tests outbound connectivity to a public endpoint.
func CheckInternetConnectivity(ctx context.Context) Result {
	r := Result{
		Category: CategoryNetwork,
		Check:    "internet",
		Title:    "Internet connectivity",
	}

	// Try connecting to multiple reliable endpoints
	targets := []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}
	for _, target := range targets {
		conn, err := net.DialTimeout("tcp", target, 3*time.Second)
		if err == nil {
			conn.Close()
			r.Severity = SeverityOK
			r.Summary = "Internet is reachable"
			return r
		}
	}

	r.Severity = SeverityWarning
	r.Summary = "Cannot reach public DNS servers"
	r.Details = "The server cannot establish outbound connections. " +
		"App updates, image pulls, and Let's Encrypt will fail."
	r.Fix = "Check network config: ip route; ip addr"
	return r
}

// CheckDNSResolution tests that DNS lookups work.
func CheckDNSResolution(ctx context.Context) Result {
	r := Result{
		Category: CategoryNetwork,
		Check:    "dns-resolution",
		Title:    "DNS resolution",
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, address)
		},
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := resolver.LookupHost(lookupCtx, "docker.io"); err != nil {
		r.Severity = SeverityError
		r.Summary = "DNS resolution is not working"
		r.Details = "Cannot resolve docker.io. Docker image pulls will fail."
		r.Fix = "Check /etc/resolv.conf and network configuration"
		return r
	}

	r.Severity = SeverityOK
	r.Summary = "DNS resolution is working"
	return r
}

// CheckSystemTime verifies the system clock is reasonable.
// A wrong clock breaks TLS certificates, logs, and scheduled jobs.
func CheckSystemTime(ctx context.Context) Result {
	r := Result{
		Category: CategorySystem,
		Check:    "system-time",
		Title:    "System clock",
	}

	// Simple sanity check: the year should be at least 2024
	// (if the BIOS clock reset, it'd be 1970 or 2000)
	now := time.Now()
	if now.Year() < 2025 { // dead-RTC detector (epoch reset), not drift — bump periodically
		r.Severity = SeverityError
		r.Summary = "System clock is clearly wrong: " + now.Format("2006-01-02")
		r.Details = "The system clock is in the past. TLS certificates, " +
			"scheduled jobs, and logs will all be affected."
		r.Fix = "Enable NTP: sudo systemctl enable --now systemd-timesyncd"
		return r
	}

	r.Severity = SeverityOK
	r.Summary = "System time: " + now.Format(time.RFC1123)
	return r
}

// CheckCasaNovaServices verifies every CasaNova systemd unit is active — the
// mockup's "All services: running" line, and the first real consumer of
// CategoryServices.
func CheckCasaNovaServices(ctx context.Context) Result {
	r := Result{
		Category: CategoryServices,
		Check:    "casanova-services",
		Title:    "CasaNova services",
	}
	units := []string{
		"casanova-gateway", "casanova-message-bus", "casanova-driver",
		"casanova-virt", "casanova-user-service", "casanova-local-storage",
		"casanova-app-management", "casanova-job-management", "casanova",
	}
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"is-active"}, units...)...).Output()
	// systemctl exits non-zero when ANY unit is inactive — the output still
	// has one state per line, so parse it regardless of err.
	states := strings.Fields(strings.TrimSpace(string(out)))
	if len(states) != len(units) {
		if err != nil {
			r.Severity = SeverityError
			r.Summary = "Could not query systemd"
			r.Details = err.Error()
			return r
		}
	}
	var down []string
	for i, st := range states {
		if st != "active" && i < len(units) {
			down = append(down, units[i]+" ("+st+")")
		}
	}
	if len(down) > 0 {
		r.Severity = SeverityError
		r.Summary = fmt.Sprintf("%d of %d services not active", len(down), len(units))
		r.Details = strings.Join(down, ", ")
		r.Fix = "sudo systemctl start " + strings.SplitN(down[0], " ", 2)[0]
		return r
	}
	r.Severity = SeverityOK
	r.Summary = fmt.Sprintf("All %d services active", len(units))
	return r
}

// CheckSSHRootLogin flags PermitRootLogin yes/without-config — the hardened
// install posture ZimaOS ships by default and CasaNova inherits from the host.
func CheckSSHRootLogin(ctx context.Context) Result {
	r := Result{
		Category: CategorySecurity,
		Check:    "ssh-root-login",
		Title:    "SSH root login",
	}
	raw, err := os.ReadFile("/etc/ssh/sshd_config")
	if err != nil {
		r.Severity = SeverityInfo
		r.Summary = "sshd_config not readable — SSH may not be installed"
		return r
	}
	permit := "" // unset = distro default (often prohibit-password)
	// sshd applies the FIRST matching PermitRootLogin, and options inside a
	// Match block only apply to that conditional context — so take the first
	// occurrence at top level and stop, rather than the previous last-wins
	// scan (which inverted sshd's semantics). If the effective config is
	// pulled in via Include (common on modern distros: sshd_config.d/*), we
	// can't see it from this one file, so report "unknown" instead of a
	// confident wrong answer.
	inMatch := false
	sawInclude := false
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "match "):
			inMatch = true
			continue
		case strings.HasPrefix(lower, "include "):
			sawInclude = true
			continue
		}
		if inMatch {
			continue // options here are conditional — not the global posture
		}
		if strings.HasPrefix(lower, "permitrootlogin") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				permit = strings.ToLower(fields[1])
				found = true
				break // first top-level match wins, like sshd
			}
		}
	}
	if !found && sawInclude {
		r.Severity = SeverityInfo
		r.Summary = "PermitRootLogin set via Include (drop-in config) — not evaluated"
		return r
	}
	switch permit {
	case "no", "prohibit-password", "without-password":
		r.Severity = SeverityOK
		r.Summary = "Root SSH login restricted (" + permitOrDefault(permit) + ")"
	case "":
		r.Severity = SeverityWarning
		r.Summary = "PermitRootLogin not set (distro default applies)"
		r.Fix = "Set 'PermitRootLogin no' in /etc/ssh/sshd_config and restart sshd"
	default:
		r.Severity = SeverityWarning
		r.Summary = "Root can log in over SSH with a password"
		r.Details = "PermitRootLogin is '" + permit + "'. A brute-forced root password owns the whole box."
		r.Fix = "Set 'PermitRootLogin no' in /etc/ssh/sshd_config and restart sshd"
	}
	return r
}

func permitOrDefault(v string) string {
	if v == "" {
		return "default"
	}
	return v
}

// readMeminfoAvailable parses MemTotal/MemAvailable from /proc/meminfo (bytes).
func readMeminfoAvailable() (total, avail uint64, err error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	parse := func(prefix string) (uint64, bool) {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, prefix) {
				f := strings.Fields(line)
				if len(f) >= 2 {
					if kb, perr := strconv.ParseUint(f[1], 10, 64); perr == nil {
						return kb * 1024, true
					}
				}
			}
		}
		return 0, false
	}
	var okT, okA bool
	total, okT = parse("MemTotal:")
	avail, okA = parse("MemAvailable:")
	if !okT {
		return 0, 0, fmt.Errorf("meminfo missing MemTotal")
	}
	// MemAvailable is absent on kernels older than 3.14. Fall back to a
	// conservative estimate (Free+Buffers+Cached) rather than failing the
	// whole check. NB: MemAvailable of literally 0 is a real (dire) value,
	// not a parse failure — the found flag distinguishes them.
	if !okA {
		free, _ := parse("MemFree:")
		buffers, _ := parse("Buffers:")
		cached, _ := parse("Cached:")
		avail = free + buffers + cached
	}
	return total, avail, nil
}

// humanBytes formats a byte count in human-readable units.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
