// Wave 25: compose-level security validation.
//
// compose-go's built-in schema validation only enforces *spec* correctness —
// it does not block dangerous-but-spec-legal fields like `privileged: true`,
// `network_mode: host`, or bind-mounting `/var/run/docker.sock`. Even when
// schema validation is enabled, those slip straight through to Docker.
//
// On top of that, our own callers pass `skipValidation=true` in several
// install/update paths to keep partial / templated YAML loadable. That makes
// the schema check effectively optional and unsafe to rely on as a security
// boundary.
//
// This file adds a hard, always-on policy gate that runs *after* parse and
// rejects YAML that would let an attacker root the host. The rules are
// deliberately strict — easier to relax later than to retrofit blocks once
// hostile compose files are circulating.
package service

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// dangerousDevicePattern matches host system-disk families and raw memory
// nodes. Enumerating /dev/sd[a-d] alone missed sd[e-z], /dev/vd*, /dev/xvd*,
// /dev/mmcblk* (SBC/Pi boot disk) and /dev/nvme partitions — any of which,
// passed through to a container, is raw read/write to the host system disk.
var dangerousDevicePattern = regexp.MustCompile(`^/dev/(mem|kmem|kmsg|port|loop-control)$|^/dev/(disk|block)(/|$)|^/dev/(sd|hd|vd|xvd)[a-z]|^/dev/mmcblk[0-9]|^/dev/nvme[0-9]`)

// isHealthCheckHostnameSafe rejects hostnames that would let a hostile
// compose file turn the in-process health probe into an SSRF.
//
// Allowed shapes:
//   - "localhost", "127.0.0.1", "::1"
//   - a bare service name with no dots and no slashes (compose-internal DNS;
//     resolves only on the user-defined network, never to a public host)
//
// Everything else — IP literals outside the loopback range, FQDNs, link-
// local metadata, IPv6 ULA — is replaced with the localhost default.
func isHealthCheckHostnameSafe(host string) bool {
	if host == "" {
		return false
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "127.0.0.1" || lower == "::1" {
		return true
	}
	// IP literal? Only loopback is allowed.
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Compose service names cannot contain dots, slashes, colons, or @.
	// Anything more elaborate is treated as an FQDN and rejected.
	if strings.ContainsAny(host, "./:@\\?#") {
		return false
	}
	return true
}

// dangerousCapabilities are Linux capabilities a container must not be granted
// ON TOP of Docker's default set. ALL is included because some compose files
// use it as a shorthand.
//
// Only capabilities ABOVE the default belong here. Docker already grants CHOWN,
// DAC_OVERRIDE, FOWNER, FSETID, KILL, MKNOD, NET_BIND_SERVICE, NET_RAW, SETFCAP,
// SETGID, SETPCAP, SETUID, SYS_CHROOT and AUDIT_WRITE to every container without
// being asked, so listing one of those would refuse a cap_add the container was
// going to hold anyway — a denial that reads like protection and grants none.
// Their absence below is correct and deliberate.
var dangerousCapabilities = map[string]struct{}{
	"ALL":             {},
	"SYS_ADMIN":       {}, // mount, namespaces, cgroups
	"SYS_MODULE":      {}, // load kernel modules
	"SYS_RAWIO":       {}, // raw I/O ports, /dev/mem
	"SYS_PTRACE":      {}, // ptrace any process incl. host PID 1 if shared ns
	"SYS_BOOT":        {}, // reboot the host
	"SYS_TIME":        {}, // change host clock
	"NET_ADMIN":       {}, // configure host network if net=host
	"DAC_READ_SEARCH": {}, // bypass file read perms
	"MAC_ADMIN":       {},
	"MAC_OVERRIDE":    {},

	// The three above the default set that leak the KERNEL rather than reach
	// the host directly. None of them is root in one step, which is why they
	// were easy to leave out; each hands over kernel addresses, and kASLR is
	// what stands between a memory-corruption bug and a working exploit.
	"BPF":     {}, // load BPF programs; with PERFMON, read kernel memory
	"PERFMON": {}, // perf_event_open on the kernel: symbols and addresses
	"SYSLOG":  {}, // syslog(2): kernel log incl. raw pointers, defeating kptr_restrict
}

// dangerousSecurityOpts are security_opt values that disable the kernel
// LSM / seccomp boundaries that contain a compromised container.
var dangerousSecurityOpts = []string{
	"apparmor:unconfined",
	"apparmor=unconfined",
	"seccomp:unconfined",
	"seccomp=unconfined",
	"label:disable",
	"label=disable",
	"no-new-privileges:false",
	"no-new-privileges=false",
	"systempaths=unconfined",
}

// dangerousBindSources are host paths whose bind-mount into a container is
// equivalent to giving root on the host. Each entry matches the path itself
// and any path that starts with it as a directory prefix.
var dangerousBindSources = []string{
	"/",
	"/boot",
	"/dev",
	"/etc",
	"/lib",
	"/lib64",
	"/proc",
	"/root",
	"/run/docker.sock",
	"/run/containerd",
	"/sbin",
	"/sys",
	"/usr",
	"/var/lib/casanova",
	"/var/lib/casaos",
	"/var/lib/docker",
	"/var/lib/kubelet",
	"/var/run/docker.sock",
	"/var/run/containerd",
}

// dangerousDevices are character/block devices whose exposure to a
// container leaks kernel memory, host disks, or DMA-capable IO.
var dangerousDevices = []string{
	"/dev/mem",
	"/dev/kmem",
	"/dev/kmsg",
	"/dev/port",
	"/dev/sda",
	"/dev/sdb",
	"/dev/sdc",
	"/dev/sdd",
	"/dev/nvme",
	"/dev/loop-control",
	"/dev/disk",
	"/dev/block",
}

// securityFinding is one dangerous setting discovered on a service. Key is a
// stable identifier (service name + setting [+ the specific path/value]) used
// to distinguish "this dangerous setting already existed on the installed
// app" from "this one was just introduced".
type securityFinding struct {
	Key     string
	Message string
}

// serviceFindings returns EVERY dangerous setting on a service (not just the
// first), each with a stable Key so an existing-app edit/update can preserve
// settings that were already present while still rejecting new ones.
func serviceFindings(svc types.ServiceConfig) []securityFinding {
	var out []securityFinding
	add := func(key, msg string) {
		out = append(out, securityFinding{Key: svc.Name + "|" + key, Message: msg})
	}

	if svc.Privileged {
		add("privileged", fmt.Sprintf("compose security: service %q sets privileged: true (denied)", svc.Name))
	}
	if isHostNamespace(svc.NetworkMode) {
		add("network_mode", fmt.Sprintf("compose security: service %q uses network_mode: %s (denied)", svc.Name, svc.NetworkMode))
	}
	if isHostNamespace(svc.Pid) {
		add("pid", fmt.Sprintf("compose security: service %q uses pid: %s (denied)", svc.Name, svc.Pid))
	}
	if isHostNamespace(svc.UserNSMode) {
		add("userns_mode", fmt.Sprintf("compose security: service %q uses userns_mode: %s (denied)", svc.Name, svc.UserNSMode))
	}
	if isHostNamespace(svc.Ipc) {
		add("ipc", fmt.Sprintf("compose security: service %q uses ipc: %s (denied)", svc.Name, svc.Ipc))
	}
	if isHostNamespace(svc.Uts) {
		add("uts", fmt.Sprintf("compose security: service %q uses uts: %s (denied)", svc.Name, svc.Uts))
	}
	if isHostNamespace(svc.CgroupParent) && strings.Contains(svc.CgroupParent, "host") {
		add("cgroup_parent", fmt.Sprintf("compose security: service %q uses cgroup_parent referencing host (denied)", svc.Name))
	}
	// `cgroup:` is a DIFFERENT field from `cgroup_parent:` — compose-go declares
	// them on adjacent lines (types/types.go:43-44) and this gate read only the
	// first. `cgroup: host` is docker's --cgroupns=host: the container sees the
	// host's whole cgroup hierarchy under its real paths instead of a rooted
	// private view. That is the namespace half of the release_agent escape; the
	// other half, a writable /sys/fs/cgroup bind, is already denied above — which
	// is why this is the sixth host namespace on a list that enumerated five, not
	// an escape on its own. Nothing else in the module looked at svc.Cgroup
	// either, so the field travelled from YAML to Docker unread.
	if isHostNamespace(svc.Cgroup) {
		add("cgroup", fmt.Sprintf("compose security: service %q uses cgroup: %s (denied)", svc.Name, svc.Cgroup))
	}

	// The five checks above ask "is this the HOST's namespace". The other way a
	// service leaves its own namespace is `container:<name|id>`, which joins a
	// namespace belonging to a container this project does not own — another
	// installed app, or one of CasaNova's own services, whose container names are
	// predictable. network_mode, pid and ipc all accept that form.
	//
	// `service:<name>` is deliberately NOT refused: it names a service in the
	// SAME project, so it shares a namespace the app already owns end to end.
	// The prefix comes from compose-go's own constant rather than a literal, so
	// the two cannot drift apart.
	for _, ns := range []struct{ field, value string }{
		{"network_mode", svc.NetworkMode},
		{"pid", svc.Pid},
		{"ipc", svc.Ipc},
	} {
		if ref := foreignContainerNamespace(ns.value); ref != "" {
			add(ns.field+":container:"+ref,
				fmt.Sprintf("compose security: service %q uses %s: %s, joining a container outside this project (denied)", svc.Name, ns.field, ns.value))
		}
	}

	for _, cap := range svc.CapAdd {
		// ToUpper BEFORE TrimPrefix — a lowercase "cap_sys_admin" would
		// otherwise skip the case-sensitive TrimPrefix("CAP_") and stay
		// prefixed, never matching the un-prefixed dangerousCapabilities keys.
		key := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(cap)), "CAP_")
		if _, bad := dangerousCapabilities[key]; bad {
			add("cap:"+key, fmt.Sprintf("compose security: service %q requests capability %q (denied)", svc.Name, cap))
		}
	}
	// device_cgroup_rules can grant a container access to sensitive device
	// majors (e.g. "c 1:1 rwm" = /dev/mem) which the default CAP_MKNOD can then
	// materialize with mknod — a host-memory escape needing no devices: entry.
	if len(svc.DeviceCgroupRules) > 0 {
		add("device_cgroup_rules", fmt.Sprintf("compose security: service %q sets device_cgroup_rules (denied)", svc.Name))
	}

	for _, opt := range svc.SecurityOpt {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(opt), " ", ""))
		for _, bad := range dangerousSecurityOpts {
			if normalized == bad {
				add("secopt:"+normalized, fmt.Sprintf("compose security: service %q sets security_opt %q (denied)", svc.Name, opt))
			}
		}
	}

	for _, vol := range svc.Volumes {
		// Only bind mounts can hit host paths. Named volumes / tmpfs / image
		// volumes are isolated by Docker so they're fine.
		if vol.Type != "" && vol.Type != "bind" {
			continue
		}
		src := strings.TrimSpace(vol.Source)
		if src == "" || !strings.HasPrefix(src, "/") {
			// Empty or non-absolute = named volume reference; safe.
			continue
		}
		if isDangerousBindSource(src) {
			add("bind:"+filepath.Clean(src), fmt.Sprintf("compose security: service %q bind-mounts host path %q (denied)", svc.Name, vol.Source))
		}
	}

	for _, dev := range svc.Devices {
		src := strings.TrimSpace(dev.Source)
		if src == "" {
			continue
		}
		if isDangerousDevice(src) {
			add("dev:"+filepath.Clean(src), fmt.Sprintf("compose security: service %q exposes device %q (denied)", svc.Name, dev.Source))
		}
	}
	return out
}

// ValidateComposeSecurity is the NEW/IMPORT gate: it rejects a project if any
// service requests a privilege that crosses the container/host boundary. The
// first violation is returned — callers surface it as a 400-class error. This
// is the strict path used for untrusted compose files being scheduled for the
// first time.
func ValidateComposeSecurity(project *ComposeApp) error {
	// ponytail: collects all findings then returns the first, O(settings) not
	// O(1). Negligible for real compose files; short-circuit inside
	// projectFindings only if a project ever carries hundreds of fields.
	if f := projectFindings(project); len(f) > 0 {
		return errors.New(f[0].Message)
	}
	return nil
}

// projectFindings returns EVERY dangerous setting in a project — the whole rule
// set, in one place, for BOTH gates.
//
// It exists because the rules had drifted apart between the two. The
// named-volume-bind check lived inline in ValidateComposeSecurity and was
// simply absent from ValidateComposeSecurityForUpdate, so a user with
// PermAppConfigure could add a top-level `o=bind device=/etc` volume through
// ApplyComposeAppSettings — which routes to the update gate only — and it
// passed, while the identical volume was rejected at install. The rule was
// never wrong; it was only in one of the two places that needed it. Having one
// collection function means the next rule cannot be half-added the same way.
//
// The other half of the same defect was a channel nobody walked at all:
// neither gate looked at project.Secrets or project.Configs, both of which
// take a `file:` host path that Docker mounts into the container. That is the
// same host bind the volume rule exists to stop, three fields further along in
// the same document — so it is checked with the same denylist rather than a
// second policy that could disagree with the first.
//
// Findings from the three project-level maps are sorted by name so the
// reported violation is stable; service findings keep their existing order.
func projectFindings(project *ComposeApp) []securityFinding {
	if project == nil {
		return nil
	}
	var out []securityFinding

	// Project-level named volumes: a `local` driver with driver_opts o=bind /
	// type=none is a HOST BIND MOUNT in disguise. A service references it by
	// name (non-absolute source), so the per-service bind check skips it as
	// "safe named volume" — but the volume's device is an arbitrary host path.
	for _, name := range sortedKeys(project.Volumes) {
		if src := bindVolumeDevice(project.Volumes[name]); src != "" && isDangerousBindSource(src) {
			out = append(out, securityFinding{
				Key:     "volume|" + name + "|" + filepath.Clean(src),
				Message: fmt.Sprintf("compose security: named volume %q bind-mounts host path %q (denied)", name, src),
			})
		}
	}

	// A file-backed secret or config is a host bind wearing a different key.
	// `secrets: {s: {file: /etc/shadow}}` hands the container the host file at
	// /run/secrets/s; a config does the same at its target. Docker honours
	// both through the API path and through `docker compose -f … up -d`.
	for _, name := range sortedKeys(project.Secrets) {
		if src := strings.TrimSpace(project.Secrets[name].File); src != "" && isDangerousBindSource(src) {
			out = append(out, securityFinding{
				Key:     "secret|" + name + "|" + filepath.Clean(src),
				Message: fmt.Sprintf("compose security: secret %q reads host file %q (denied)", name, src),
			})
		}
	}
	for _, name := range sortedKeys(project.Configs) {
		if src := strings.TrimSpace(project.Configs[name].File); src != "" && isDangerousBindSource(src) {
			out = append(out, securityFinding{
				Key:     "config|" + name + "|" + filepath.Clean(src),
				Message: fmt.Sprintf("compose security: config %q reads host file %q (denied)", name, src),
			})
		}
	}

	for _, svc := range project.Services {
		out = append(out, serviceFindings(svc)...)
	}
	return out
}

// sortedKeys orders a project-level map's names so that which violation gets
// reported does not depend on Go's map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// bindVolumeDevice returns the host device path of a top-level `local` bind
// volume (driver_opts o=bind / type=none), else "". These are host bind mounts
// expressed as named volumes.
func bindVolumeDevice(vol types.VolumeConfig) string {
	if vol.Driver != "" && vol.Driver != "local" {
		return ""
	}
	o := vol.DriverOpts["o"]
	t := vol.DriverOpts["type"]
	if strings.Contains(o, "bind") || t == "none" || t == "bind" {
		return strings.TrimSpace(vol.DriverOpts["device"])
	}
	return ""
}

// ValidateComposeSecurityForUpdate is the EDIT/UPDATE gate for an
// already-installed app. It permits dangerous settings that were ALREADY
// present on the installed app (so a running app's network_mode: host, or an
// existing privileged flag, survives a tag/image/title change) while still
// rejecting any newly-introduced dangerous setting. A nil oldProject means
// "no baseline", which collapses to the strict behaviour (every finding is
// treated as new).
func ValidateComposeSecurityForUpdate(newProject, oldProject *ComposeApp) error {
	if newProject == nil {
		return nil
	}
	// Both sides come from projectFindings, so this gate sees exactly the rules
	// the strict one does. It used to walk only Services, which is why a
	// top-level bind volume added through ApplyComposeAppSettings — the path
	// that runs this gate and skips the strict one — went straight through
	// while being rejected at install.
	existing := map[string]struct{}{}
	for _, f := range projectFindings(oldProject) {
		existing[f.Key] = struct{}{}
	}
	for _, f := range projectFindings(newProject) {
		if _, ok := existing[f.Key]; !ok {
			return fmt.Errorf("%s — newly added on edit; not permitted unless explicitly allowed", f.Message)
		}
	}
	return nil
}

func isHostNamespace(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "host")
}

// foreignContainerNamespace returns the container named by a
// `container:<name|id>` namespace reference, or "" for every other form.
//
// The v1 CreateContainer path (service/container.go) also recognises this
// shape, and it was checked before writing this one — but it cannot be reused,
// because it reaches the OPPOSITE verdict: it is an allowlist of network modes
// whose whole purpose is to ADMIT `container:` alongside bridge/host/none. The
// shape is the same, the policy is not, and the divergence between the two
// engines is a decision rather than a bug, so nothing there was touched. What
// is reused instead is compose-go's own types.ContainerPrefix, so this check
// and the parser cannot drift.
//
// The prefix is matched case-insensitively. Docker itself is case-sensitive
// here, so an odd-cased value would be read by Docker as a plain network name —
// matching loosely can only ever deny something that would not have worked.
func foreignContainerNamespace(mode string) string {
	trimmed := strings.TrimSpace(mode)
	if len(trimmed) <= len(types.ContainerPrefix) {
		return ""
	}
	if !strings.EqualFold(trimmed[:len(types.ContainerPrefix)], types.ContainerPrefix) {
		return ""
	}
	return strings.TrimSpace(trimmed[len(types.ContainerPrefix):])
}

// isDangerousBindSource canonicalizes the source path and checks whether it
// matches or descends into any forbidden host directory. Symlink resolution
// is intentionally NOT done here — the path comes from a YAML literal and we
// want to block both literal and prefix matches; symlink games at start time
// are caught by the install path's filepath.Clean before mkdir.
func isDangerousBindSource(src string) bool {
	clean := filepath.Clean(src)
	if matchesDangerousBind(clean) {
		return true
	}
	// filepath.Clean is a STRING operation: it cannot see that an allowed
	// path is a symlink INTO a denied one. Docker resolves the host path when
	// it mounts, so a link planted at an allowed location — say
	// /DATA/AppData/<app>/hostlink -> /var/run — mounts the target and hands
	// the container the docker socket, while the lexical check above reads
	// only "/DATA/AppData/...", which is allowed. Check where it actually
	// lands as well. This can only ever ADD a denial: a source whose
	// resolved form is harmless keeps the verdict it had.
	if resolved := resolveBindSource(clean); resolved != clean {
		return matchesDangerousBind(resolved)
	}
	return false
}

// matchesDangerousBind is the lexical denylist test, unchanged — split out so
// it can be applied to both the literal path and its resolved form.
func matchesDangerousBind(clean string) bool {
	sep := string(filepath.Separator)
	for _, prefix := range dangerousBindSources {
		if clean == prefix {
			return true
		}
		if strings.HasPrefix(clean, prefix+sep) {
			// The mount is the forbidden path or a child of it.
			return true
		}
		if strings.HasPrefix(prefix+sep, clean+sep) {
			// The mount is an ANCESTOR of a forbidden path — e.g. mounting
			// /var/run (or /var, or /) to reach /var/run/docker.sock and
			// escape to the host. Blocking the socket alone (Wave 25) was
			// bypassable by mounting its parent dir; this closes that.
			return true
		}
	}
	return false
}

// resolveBindSource reports where clean actually lands on disk, resolving the
// deepest ancestor that exists and re-appending the components that do not.
//
// A bind source frequently does NOT exist yet — Docker creates it — and a path
// that does not exist cannot be a symlink to anywhere, so an unresolvable one
// keeps its lexical verdict rather than being guessed at. os.Lstat, not
// os.Stat, so a dangling link still counts as existing.
func resolveBindSource(clean string) string {
	dir := clean
	var missing []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return clean
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
		dir = parent
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return clean
	}
	return filepath.Join(append([]string{resolved}, missing...)...)
}

func isDangerousDevice(src string) bool {
	clean := filepath.Clean(src)
	if dangerousDevicePattern.MatchString(clean) {
		return true
	}
	for _, prefix := range dangerousDevices {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}
