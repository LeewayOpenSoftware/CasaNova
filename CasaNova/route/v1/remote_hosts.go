package v1

// Remote hosts — which of the box's apps already have a public HTTPS origin.
//
// When the dashboard is reached through the Cloudflare tunnel, the page is
// HTTPS and every LAN URL is useless: http:// iframes are mixed content and
// the visitor cannot reach 192.168.x.x anyway. But this box already publishes
// per-app hostnames — tunnel ingress rules and NPM proxy hosts — maintained
// by the operator in the tools built for that job. This endpoint READS those
// sources; it never writes them, and it needs no credentials for either:
//
//   - cloudflared runs a remotely-managed tunnel, so its ingress rules live
//     in Cloudflare's cloud, not on disk. But cloudflared prints the full
//     configuration to its log every time one is pushed ("Updated to new
//     configuration"), and the container's log file is on the host. The last
//     such line IS the current config.
//
//   - Nginx Proxy Manager materialises every proxy host as a generated nginx
//     conf under <data>/nginx/proxy_host/. server_name + set $server/$port
//     lines carry the whole mapping.
//
// A tunnel rule may point at NPM instead of an app (this box: nube → NPM:80
// → nextcloud:10081), so hostnames present in both sources are resolved one
// chain hop through NPM. Rules whose final target is not this box are
// reported but flagged — on this very box the crafty rule still points at a
// machine that no longer exists, and matching an app to a dead rule would
// embed a hostname that cannot answer.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type RemoteHost struct {
	// Hostname is the public name, e.g. "peliculas.neinrev8.work".
	Hostname string `json:"hostname"`
	// TargetHost/TargetPort/TargetScheme describe where the rule finally
	// lands after NPM chain resolution.
	TargetHost   string `json:"targetHost"`
	TargetPort   int    `json:"targetPort"`
	TargetScheme string `json:"targetScheme"`
	// Source names which system publishes the hostname.
	Source string `json:"source"` // "cloudflared" | "npm"
	// OnThisBox is false when the rule's target is some other machine — the
	// UI must not match such a rule to a local app, because the public name
	// would answer with someone else's service or nothing at all.
	OnThisBox bool `json:"onThisBox"`
}

// ---------------------------------------------------------------- parsing

// tunnel config as printed by cloudflared.
type tunnelConfig struct {
	Ingress []struct {
		Hostname string `json:"hostname"`
		Service  string `json:"service"`
	} `json:"ingress"`
}

var tunnelConfigRE = regexp.MustCompile(`config="((?:[^"\\]|\\.)*)"`)

// parseTunnelLogLine extracts the ingress config from one docker json-file
// log line, or nil when the line carries none. The value is doubly encoded:
// the file line is JSON ({"log":"..."}), and inside the log text the config
// is a Go-quoted string.
func parseTunnelLogLine(fileLine []byte) *tunnelConfig {
	var entry struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal(fileLine, &entry); err != nil {
		return nil
	}
	if !strings.Contains(entry.Log, "Updated to new configuration") {
		return nil
	}
	m := tunnelConfigRE.FindStringSubmatch(entry.Log)
	if m == nil {
		return nil
	}
	unquoted, err := strconv.Unquote(`"` + m[1] + `"`)
	if err != nil {
		return nil
	}
	var cfg tunnelConfig
	if err := json.Unmarshal([]byte(unquoted), &cfg); err != nil {
		return nil
	}
	return &cfg
}

// npmProxyHost is one generated proxy_host conf.
type npmProxyHost struct {
	ServerNames []string
	Scheme      string
	Host        string
	Port        int
}

var (
	npmServerNameRE = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	npmSetRE        = regexp.MustCompile(`(?m)^\s*set\s+\$(forward_scheme|server|port)\s+"?([^";\s]+)"?\s*;`)
)

// parseNPMConf pulls the mapping out of one nginx conf. Nil when the file
// does not carry a complete mapping — half a mapping is not a remote host.
func parseNPMConf(conf string) *npmProxyHost {
	names := npmServerNameRE.FindStringSubmatch(conf)
	if names == nil {
		return nil
	}
	h := &npmProxyHost{ServerNames: strings.Fields(names[1])}
	for _, m := range npmSetRE.FindAllStringSubmatch(conf, -1) {
		switch m[1] {
		case "forward_scheme":
			h.Scheme = m[2]
		case "server":
			h.Host = m[2]
		case "port":
			h.Port, _ = strconv.Atoi(m[2])
		}
	}
	if h.Scheme == "" || h.Host == "" || h.Port == 0 || len(h.ServerNames) == 0 {
		return nil
	}
	return h
}

// isThisBox reports whether a rule target lands on this machine.
func isThisBox(host string, boxIPs map[string]bool) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || boxIPs[host]
	}
	return false
}

// resolveRemoteHosts merges both sources into the final list. Pure: inputs
// are already-parsed structures plus the box's own addresses, so the whole
// truth table is testable without docker or a filesystem.
//
// npmPublishedPorts names the host ports NPM itself listens on (80/443 by
// convention, but taken from the caller): a tunnel rule that lands on one of
// them is a chain through NPM, and the hostname's real target is whatever
// NPM's own conf for that same hostname says.
func resolveRemoteHosts(cfg *tunnelConfig, npmHosts []npmProxyHost, boxIPs map[string]bool, npmPublishedPorts map[int]bool) []RemoteHost {
	npmByName := map[string]npmProxyHost{}
	for _, h := range npmHosts {
		for _, n := range h.ServerNames {
			npmByName[strings.ToLower(n)] = h
		}
	}

	out := []RemoteHost{}
	seen := map[string]bool{}

	if cfg != nil {
		for _, rule := range cfg.Ingress {
			if rule.Hostname == "" {
				continue // the catch-all 404 rule
			}
			u, err := url.Parse(rule.Service)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				continue // tcp:// minecraft, http_status:404, ...
			}
			port, _ := strconv.Atoi(u.Port())
			if port == 0 {
				if u.Scheme == "https" {
					port = 443
				} else {
					port = 80
				}
			}
			host := u.Hostname()
			scheme := u.Scheme

			// Chain hop: tunnel → NPM → app.
			if isThisBox(host, boxIPs) && npmPublishedPorts[port] {
				if npm, ok := npmByName[strings.ToLower(rule.Hostname)]; ok {
					host, port, scheme = npm.Host, npm.Port, npm.Scheme
				}
			}

			out = append(out, RemoteHost{
				Hostname:     rule.Hostname,
				TargetHost:   host,
				TargetPort:   port,
				TargetScheme: scheme,
				Source:       "cloudflared",
				OnThisBox:    isThisBox(host, boxIPs),
			})
			seen[strings.ToLower(rule.Hostname)] = true
		}
	}

	// NPM hosts not already published through the tunnel. They are reachable
	// only if the operator points DNS at the box some other way, but they are
	// still published names and the UI can offer them.
	for _, h := range npmHosts {
		for _, n := range h.ServerNames {
			if seen[strings.ToLower(n)] {
				continue
			}
			out = append(out, RemoteHost{
				Hostname:     n,
				TargetHost:   h.Host,
				TargetPort:   h.Port,
				TargetScheme: h.Scheme,
				Source:       "npm",
				OnThisBox:    isThisBox(h.Host, boxIPs),
			})
		}
	}
	return out
}

// ---------------------------------------------------------------- gathering

const remoteHostsCacheTTL = 5 * time.Minute

var (
	remoteHostsMu         sync.Mutex
	remoteHostsCache      []RemoteHost
	remoteHostsCached     time.Time
	remoteHostsRefreshing bool
)

// dockerOut runs the docker CLI with a hard deadline. Without one, a wedged
// dockerd hangs the caller forever — and this file's callers used to hold
// remoteHostsMu while gathering, so one hang would have queued every later
// request behind it until the echo workers ran out. The deadline turns a
// wedged daemon into "no remote hosts this round", which the UI already
// degrades to gracefully.
func dockerOut(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	return string(out), err
}

// findContainerByImage returns the name of the first running container whose
// image contains the fragment.
func findContainerByImage(fragment string) string {
	out, err := dockerOut("ps", "--format", "{{.Image}}\t{{.Names}}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && strings.Contains(parts[0], fragment) {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

// tunnelConfigFromLogs reads the container's json log file and keeps the
// LAST configuration line. The log path comes from docker inspect, so it is
// docker's own answer, not a guess.
func tunnelConfigFromLogs(container string) *tunnelConfig {
	path, err := dockerOut("inspect", "--format", "{{.LogPath}}", container)
	if err != nil {
		return nil
	}
	f, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		logger.Info("remote-hosts: tunnel log unreadable", zap.Error(err))
		return nil
	}
	defer f.Close()

	var last *tunnelConfig
	scanner := newLargeLineScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Cheap pre-filter before JSON work: the marker is rare.
		if !strings.Contains(string(line), "Updated to new configuration") {
			continue
		}
		if cfg := parseTunnelLogLine(line); cfg != nil {
			last = cfg
		}
	}
	return last
}

// npmHostsFromDisk locates NPM's data volume via docker inspect and parses
// every generated proxy_host conf.
func npmHostsFromDisk(container string) ([]npmProxyHost, map[int]bool) {
	published := map[int]bool{}
	out, err := dockerOut("inspect", "--format",
		`{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}|{{range $p, $b := .NetworkSettings.Ports}}{{$p}} {{end}}`, container)
	if err != nil {
		return nil, published
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
	dataDir := parts[0]
	if len(parts) == 2 {
		// "80/tcp 81/tcp 443/tcp" — the ports NPM itself answers on.
		for _, p := range strings.Fields(parts[1]) {
			if n, err := strconv.Atoi(strings.TrimSuffix(p, "/tcp")); err == nil {
				published[n] = true
			}
		}
	}
	if dataDir == "" {
		return nil, published
	}
	confs, _ := filepath.Glob(filepath.Join(dataDir, "nginx", "proxy_host", "*.conf"))
	hosts := []npmProxyHost{}
	for _, c := range confs {
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		if h := parseNPMConf(string(b)); h != nil {
			hosts = append(hosts, *h)
		}
	}
	return hosts, published
}

// newLargeLineScanner returns a line scanner that tolerates docker log lines
// far beyond bufio's 64 KiB default — the configuration line alone runs to a
// few KiB and there is no upper bound on what an app logs in one line.
func newLargeLineScanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return sc
}

func boxAddresses() map[string]bool {
	ips := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			ips[ipnet.IP.String()] = true
		}
	}
	return ips
}

// GetRemoteHosts — GET /v1/sys/remote-hosts.
//
// Same gate as the embed probe and for the same reason: the response maps the
// box's internal port layout, which is not guest material.
func GetRemoteHosts(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}

	// The mutex guards the CACHE FIELDS only — never the gather. Holding it
	// across the docker execs and the log scan would serialize every caller
	// behind the slowest one; instead, one goroutine wins the right to
	// refresh and the rest are served the previous list immediately. A
	// slightly stale answer beats a convoy.
	remoteHostsMu.Lock()
	fresh := time.Since(remoteHostsCached) < remoteHostsCacheTTL && remoteHostsCache != nil
	cached := remoteHostsCache
	refreshing := remoteHostsRefreshing
	if !fresh && !refreshing {
		remoteHostsRefreshing = true
	}
	remoteHostsMu.Unlock()

	if fresh || refreshing {
		if cached == nil {
			cached = []RemoteHost{}
		}
		return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: cached})
	}

	var cfg *tunnelConfig
	if c := findContainerByImage("cloudflared"); c != "" {
		cfg = tunnelConfigFromLogs(c)
	}
	var npmHosts []npmProxyHost
	npmPorts := map[int]bool{}
	if c := findContainerByImage("nginx-proxy-manager"); c != "" {
		npmHosts, npmPorts = npmHostsFromDisk(c)
	}
	resolved := resolveRemoteHosts(cfg, npmHosts, boxAddresses(), npmPorts)

	remoteHostsMu.Lock()
	remoteHostsCache = resolved
	remoteHostsCached = time.Now()
	remoteHostsRefreshing = false
	remoteHostsMu.Unlock()
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: resolved})
}
