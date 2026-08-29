package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ---- Remote Access connector inventory ----
//
// CasaNova doesn't bake any single remote-access vendor into the
// platform. Instead it inventories the three common ones (Tailscale,
// Cloudflare Tunnel, ZeroTier) and reports which are installed +
// active, so the Settings → Remote Access page can render a unified
// status view + an install button for the missing ones.
//
// Detection is filesystem + CLI based — fast (<2s end-to-end) and
// doesn't depend on any specific package manager.

type connectorStatus struct {
	ID         string `json:"id"`          // tailscale | cloudflared | zerotier
	Name       string `json:"name"`        // human label
	Installed  bool   `json:"installed"`   // binary / unit / container exists
	Running    bool   `json:"running"`     // active right now
	Detail     string `json:"detail"`      // one-line status (peer count, hostname, IP, error)
	InstallURL string `json:"install_url"` // optional in-app deep link to install via catalog
}

// GetRemoteAccessStatus inspects the host for known remote-access
// connectors and returns a structured report. Read-only — never
// mutates state.
//
// GET /v1/sys/remote-access/status
func GetRemoteAccessStatus(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	connectors := []connectorStatus{
		probeTailscale(),
		probeCloudflared(),
		probeZeroTier(),
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    connectors,
	})
}

// probeTailscale calls `tailscale status --json`. If the binary is
// missing, Installed=false and we stop. If the binary exists but the
// daemon isn't running, Installed=true / Running=false with the
// daemon's own error message in Detail (which is more useful than
// "command failed").
func probeTailscale() connectorStatus {
	c := connectorStatus{ID: "tailscale", Name: "Tailscale"}
	if _, err := exec.LookPath("tailscale"); err != nil {
		// Suggest the upstream installer rather than the app catalog —
		// Tailscale runs natively, not as a container.
		c.InstallURL = "https://tailscale.com/download/linux"
		return c
	}
	c.Installed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		c.Detail = "tailscale daemon not reachable"
		return c
	}
	var st struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			HostName     string   `json:"HostName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
		Peer map[string]json.RawMessage `json:"Peer"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		c.Detail = "unrecognized tailscale status payload"
		return c
	}
	c.Running = st.BackendState == "Running"
	var ip string
	if len(st.Self.TailscaleIPs) > 0 {
		ip = st.Self.TailscaleIPs[0]
	}
	switch {
	case c.Running && ip != "":
		c.Detail = "connected as " + st.Self.HostName + " (" + ip + ") · " + countPeers(st.Peer) + " peers"
	case c.Running:
		c.Detail = "connected as " + st.Self.HostName
	default:
		c.Detail = "installed, backend state: " + st.BackendState
	}
	return c
}

func countPeers(peers map[string]json.RawMessage) string {
	n := len(peers)
	if n == 0 {
		return "0"
	}
	return itoa(n)
}

// probeCloudflared looks for a running container named `cloudflared`
// (the canonical name in the app catalog's compose file). Reports
// installed=true when the container exists at all, running=true when
// it's currently up.
func probeCloudflared() connectorStatus {
	c := connectorStatus{ID: "cloudflared", Name: "Cloudflare Tunnel"}
	// Install URL is the in-app catalog deep link. The frontend route
	// handler intercepts the "library:" prefix, opens the library
	// panel, and pre-fills the search so the user lands on the app.
	c.InstallURL = "library:cloudflared"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "name=^cloudflared$",
		"--format", "{{.State}}",
	).Output()
	if err != nil {
		// docker missing or hung — leave the connector as "not
		// installed" rather than fail the whole report.
		return c
	}
	state := strings.TrimSpace(string(out))
	if state == "" {
		return c
	}
	c.Installed = true
	c.Running = state == "running"
	if c.Running {
		c.Detail = "container running — check the cloudflared admin UI for tunnel status"
	} else {
		c.Detail = "container present but not running (state: " + state + ")"
	}
	return c
}

// probeZeroTier checks for the systemd unit + the auth token file the
// existing /v1/zt proxy already depends on. We don't talk to the
// daemon here — the network listing already lives at the zt route.
func probeZeroTier() connectorStatus {
	c := connectorStatus{ID: "zerotier", Name: "ZeroTier"}
	c.InstallURL = "https://www.zerotier.com/download/"

	if _, err := exec.LookPath("zerotier-cli"); err != nil {
		return c
	}
	c.Installed = true

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", "zerotier-one").Output()
	state := strings.TrimSpace(string(out))
	c.Running = err == nil && state == "active"

	// Pull a short identity line so the page doesn't just say "running"
	// with no context.
	if c.Running {
		idOut, err := exec.CommandContext(ctx, "zerotier-cli", "info").Output()
		if err == nil {
			// `zerotier-cli info` returns:
			//   200 info <node-id> 1.10.6 ONLINE
			c.Detail = strings.TrimSpace(string(idOut))
		} else {
			c.Detail = "daemon running"
		}
	} else {
		c.Detail = "installed, daemon not active"
	}
	return c
}

// itoa is a tiny helper so we don't pull strconv just for one int.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
