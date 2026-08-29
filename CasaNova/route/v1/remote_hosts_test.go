package v1

// Fixtures here are captures from the production box, not inventions: the
// docker json-file line carries cloudflared's "Updated to new configuration"
// exactly as logged on 2026-08-29, and the NPM conf mirrors the generated
// file for nube.neinrev8.work. If either tool changes its format, these
// tests fail with the new reality instead of the parser silently returning
// nothing — an empty remote-host list looks identical to "no tunnel", so a
// silent parse regression would simply turn the feature off.

import (
	"strings"
	"testing"
)

// One real docker log line, abbreviated to three rules that cover the three
// interesting shapes: a plain rule, the NPM chain (nube), and the stale rule
// pointing at a machine that no longer exists (crafty → .109).
const realTunnelLogLine = `{"log":"2026-08-09T17:54:52Z INF Updated to new configuration config=\"{\\\"ingress\\\":[{\\\"hostname\\\":\\\"peliculas.neinrev8.work\\\",\\\"id\\\":\\\"2\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"http://192.168.1.188:8097\\\"},{\\\"hostname\\\":\\\"nube.neinrev8.work\\\",\\\"originRequest\\\":{\\\"noTLSVerify\\\":true},\\\"service\\\":\\\"http://127.0.0.1:80\\\"},{\\\"hostname\\\":\\\"crafty.neinrev8.work\\\",\\\"id\\\":\\\"12\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"https://192.168.1.109:8111\\\"},{\\\"hostname\\\":\\\"mcraftone.neinrev8.work\\\",\\\"id\\\":\\\"11\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"tcp://127.0.0.1:25565\\\"},{\\\"service\\\":\\\"http_status:404\\\"}],\\\"warp-routing\\\":{\\\"enabled\\\":false}}\" version=100\n","stream":"stdout","time":"2026-08-09T17:54:52.75Z"}`

const realNPMConf = `
# ------------------------------------------------------------
# nube.neinrev8.work
# ------------------------------------------------------------
server {
  set $forward_scheme http;
  set $server         "192.168.1.188";
  set $port           10081;
  listen 80;
  server_name nube.neinrev8.work;
}
`

func TestParseTunnelLogLineRealCapture(t *testing.T) {
	cfg := parseTunnelLogLine([]byte(realTunnelLogLine))
	if cfg == nil {
		t.Fatal("the real log line did not parse — the double decoding (json file line, then Go-quoted config) is broken")
	}
	if len(cfg.Ingress) != 5 {
		t.Fatalf("ingress rules = %d, want 5 (incl. tcp and catch-all, filtered later)", len(cfg.Ingress))
	}
	if cfg.Ingress[0].Hostname != "peliculas.neinrev8.work" || cfg.Ingress[0].Service != "http://192.168.1.188:8097" {
		t.Fatalf("first rule mangled: %+v", cfg.Ingress[0])
	}
}

func TestParseTunnelLogLineIgnoresOrdinaryLines(t *testing.T) {
	for _, line := range []string{
		`{"log":"2026-08-29T17:31:02Z ERR error=\"Unable to reach the origin service\" ingressRule=12\n","stream":"stderr","time":"x"}`,
		`not json at all`,
		`{"log":"Updated to new configuration but with no config value\n"}`,
	} {
		if cfg := parseTunnelLogLine([]byte(line)); cfg != nil {
			t.Fatalf("parsed a config out of a line that has none: %q", line)
		}
	}
}

func TestParseNPMConfRealCapture(t *testing.T) {
	h := parseNPMConf(realNPMConf)
	if h == nil {
		t.Fatal("the real NPM conf did not parse")
	}
	if h.ServerNames[0] != "nube.neinrev8.work" || h.Host != "192.168.1.188" || h.Port != 10081 || h.Scheme != "http" {
		t.Fatalf("mapping mangled: %+v", h)
	}
}

func TestParseNPMConfRejectsHalfAMapping(t *testing.T) {
	if h := parseNPMConf("server {\n  server_name algo.example.com;\n}\n"); h != nil {
		t.Fatalf("a conf with no forward target produced a host: %+v — half a mapping embeds a hostname that answers 502", h)
	}
}

func TestResolveRemoteHostsTruthTable(t *testing.T) {
	cfg := parseTunnelLogLine([]byte(realTunnelLogLine))
	npm := parseNPMConf(realNPMConf)
	boxIPs := map[string]bool{"192.168.1.188": true}
	npmPorts := map[int]bool{80: true, 81: true, 443: true}

	got := resolveRemoteHosts(cfg, []npmProxyHost{*npm}, boxIPs, npmPorts)

	byName := map[string]RemoteHost{}
	for _, h := range got {
		byName[h.Hostname] = h
	}

	// Plain rule: passes through, on this box.
	if h := byName["peliculas.neinrev8.work"]; h.TargetPort != 8097 || !h.OnThisBox {
		t.Fatalf("plain rule mangled: %+v", h)
	}

	// The NPM chain: nube's tunnel rule lands on NPM:80, and the real target
	// is what NPM's own conf says. Without the hop, the UI would match nube
	// to whatever app publishes port 80 — on this box that is NPM itself.
	if h := byName["nube.neinrev8.work"]; h.TargetPort != 10081 || h.TargetHost != "192.168.1.188" || !h.OnThisBox {
		t.Fatalf("NPM chain not resolved: %+v", h)
	}

	// The stale rule: reported, but flagged off-box so no app matches it.
	// Matching crafty (192.168.1.188:8111) to a rule that forwards to
	// 192.168.1.109 would embed a hostname that answers 'no route to host'.
	if h := byName["crafty.neinrev8.work"]; h.OnThisBox {
		t.Fatalf("a rule pointing at another machine claimed to be on this box: %+v", h)
	}

	// Non-http services and the catch-all never make it out.
	if _, ok := byName["mcraftone.neinrev8.work"]; ok {
		t.Fatal("a tcp:// service surfaced as a web remote host")
	}
	if len(got) != 3 {
		names := []string{}
		for _, h := range got {
			names = append(names, h.Hostname)
		}
		t.Fatalf("hosts = %v, want exactly the 3 web rules", strings.Join(names, ", "))
	}
}

func TestResolveRemoteHostsNPMOnlyHostSurvivesWithoutTunnel(t *testing.T) {
	npm := parseNPMConf(realNPMConf)
	got := resolveRemoteHosts(nil, []npmProxyHost{*npm}, map[string]bool{"192.168.1.188": true}, map[int]bool{})
	if len(got) != 1 || got[0].Source != "npm" || got[0].TargetPort != 10081 {
		t.Fatalf("NPM-only publication lost when no tunnel config exists: %+v", got)
	}
}
