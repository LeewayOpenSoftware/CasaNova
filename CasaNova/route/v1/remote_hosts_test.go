package v1

// Fixtures here are synthetic, but shaped to match a real cloudflared/NPM
// capture byte-for-byte in every property the tests below actually exercise:
// the docker json-file line, the double encoding inside it (JSON, then a
// Go-quoted config string — exactly how cloudflared logs "Updated to new
// configuration"), and the NPM conf format nginx-proxy-manager generates. If
// either tool changes its format, these tests fail with the new reality
// instead of the parser silently returning nothing — an empty remote-host
// list looks identical to "no tunnel", so a silent parse regression would
// simply turn the feature off. Hostnames/IPs use RFC 2606 (example.com) and
// RFC 5737 (192.0.2.0/24, 198.51.100.0/24) documentation ranges — reserved
// for exactly this purpose, never a real address.

import (
	"strings"
	"testing"
)

// Five ingress rules, chosen to cover the five shapes resolveRemoteHosts must
// tell apart: a plain http rule with an explicit non-default port
// (movies, on this box), a rule that chains through NPM (cloud, whose raw
// target is NPM's own loopback:80 but whose real target — asserted below —
// is what NPM's conf says), a stale rule pointing at a machine that is not
// this box (stale, must be reported but flagged off-box), a non-http/https
// service that must never surface (game, tcp://), and the tunnel's catch-all
// 404 rule which carries no hostname at all and must be filtered before it
// ever reaches resolveRemoteHosts. id/originRequest/noTLSVerify/warp-routing
// are real cloudflared fields the Go struct does not parse (json.Unmarshal
// ignores unknown keys) — kept so the fixture still round-trips through a
// realistic full payload, not because the tests read them.
const realTunnelLogLine = `{"log":"2026-08-09T17:54:52Z INF Updated to new configuration config=\"{\\\"ingress\\\":[{\\\"hostname\\\":\\\"movies.example.com\\\",\\\"id\\\":\\\"1\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"http://192.0.2.10:9091\\\"},{\\\"hostname\\\":\\\"cloud.example.com\\\",\\\"originRequest\\\":{\\\"noTLSVerify\\\":true},\\\"service\\\":\\\"http://127.0.0.1:80\\\"},{\\\"hostname\\\":\\\"stale.example.com\\\",\\\"id\\\":\\\"2\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"https://198.51.100.23:9443\\\"},{\\\"hostname\\\":\\\"game.example.com\\\",\\\"id\\\":\\\"3\\\",\\\"originRequest\\\":{},\\\"service\\\":\\\"tcp://127.0.0.1:25565\\\"},{\\\"service\\\":\\\"http_status:404\\\"}],\\\"warp-routing\\\":{\\\"enabled\\\":false}}\" version=100\n","stream":"stdout","time":"2026-08-09T17:54:52.75Z"}`

const realNPMConf = `
# ------------------------------------------------------------
# cloud.example.com
# ------------------------------------------------------------
server {
  set $forward_scheme http;
  set $server         "192.0.2.10";
  set $port           18080;
  listen 80;
  server_name cloud.example.com;
}
`

func TestParseTunnelLogLineDoubleEncodedCapture(t *testing.T) {
	cfg := parseTunnelLogLine([]byte(realTunnelLogLine))
	if cfg == nil {
		t.Fatal("the fixture line did not parse — the double decoding (json file line, then Go-quoted config) is broken")
	}
	if len(cfg.Ingress) != 5 {
		t.Fatalf("ingress rules = %d, want 5 (incl. tcp and catch-all, filtered later)", len(cfg.Ingress))
	}
	if cfg.Ingress[0].Hostname != "movies.example.com" || cfg.Ingress[0].Service != "http://192.0.2.10:9091" {
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

func TestParseNPMConfGeneratedConf(t *testing.T) {
	h := parseNPMConf(realNPMConf)
	if h == nil {
		t.Fatal("the fixture NPM conf did not parse")
	}
	if h.ServerNames[0] != "cloud.example.com" || h.Host != "192.0.2.10" || h.Port != 18080 || h.Scheme != "http" {
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
	boxIPs := map[string]bool{"192.0.2.10": true}
	npmPorts := map[int]bool{80: true, 81: true, 443: true}

	got := resolveRemoteHosts(cfg, []npmProxyHost{*npm}, boxIPs, npmPorts)

	byName := map[string]RemoteHost{}
	for _, h := range got {
		byName[h.Hostname] = h
	}

	// Plain rule: passes through, on this box.
	if h := byName["movies.example.com"]; h.TargetPort != 9091 || !h.OnThisBox {
		t.Fatalf("plain rule mangled: %+v", h)
	}

	// The NPM chain: cloud's tunnel rule lands on NPM:80, and the real target
	// is what NPM's own conf says. Without the hop, the UI would match cloud
	// to whatever app publishes port 80 — on this box that is NPM itself.
	if h := byName["cloud.example.com"]; h.TargetPort != 18080 || h.TargetHost != "192.0.2.10" || !h.OnThisBox {
		t.Fatalf("NPM chain not resolved: %+v", h)
	}

	// The stale rule: reported, but flagged off-box so no app matches it.
	// Matching stale (192.0.2.10:9443) to a rule that forwards to a
	// different machine (198.51.100.23) would embed a hostname that answers
	// 'no route to host'.
	if h := byName["stale.example.com"]; h.OnThisBox {
		t.Fatalf("a rule pointing at another machine claimed to be on this box: %+v", h)
	}

	// Non-http services and the catch-all never make it out.
	if _, ok := byName["game.example.com"]; ok {
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
	got := resolveRemoteHosts(nil, []npmProxyHost{*npm}, map[string]bool{"192.0.2.10": true}, map[int]bool{})
	if len(got) != 1 || got[0].Source != "npm" || got[0].TargetPort != 18080 {
		t.Fatalf("NPM-only publication lost when no tunnel config exists: %+v", got)
	}
}
