package v1

// Embed probe — asks, server-side, whether each app's web UI consents to
// being shown inside an iframe.
//
// The browser cannot answer this question itself: reading X-Frame-Options or
// Content-Security-Policy off a cross-origin response is exactly what CORS
// forbids, and a blocked iframe fails with no event the parent can observe.
// So the Apps section would either guess (and render blank panels for apps
// like Portainer that send `frame-ancestors 'none'`) or ask the box, which
// can simply look at the headers. This endpoint is the asking.
//
// Scope is deliberately tiny: the caller names ports, never hosts — every
// probe goes to 127.0.0.1. Redirects are followed only while they stay on
// loopback, so an app cannot bounce the probe into the LAN (a router admin
// page, a neighbour box) and have CasaNova fetch it. That is the whole SSRF
// surface, closed.

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

const (
	// embedProbeMaxTargets bounds one request's work. The box runs a few
	// dozen apps; 64 is generous without letting a single call scan a port
	// range.
	embedProbeMaxTargets = 64
	embedProbeTimeout    = 4 * time.Second
)

type EmbedProbeTarget struct {
	Port   int    `json:"port"`
	Scheme string `json:"scheme"` // "http" (default) or "https"
}

type EmbedProbeResult struct {
	Port   int    `json:"port"`
	Scheme string `json:"scheme"`
	// Status is the final HTTP status after loopback-confined redirects,
	// 0 when the port did not answer.
	Status int `json:"status"`
	// FrameOptions / FrameAncestors are the raw header values, so the UI can
	// show the operator exactly what the app said instead of a paraphrase.
	FrameOptions   string `json:"frameOptions"`
	FrameAncestors string `json:"frameAncestors"`
	// Embeddable is the verdict for a CROSS-origin iframe, which is what the
	// dashboard is to every app. SAMEORIGIN therefore counts as a refusal.
	Embeddable bool   `json:"embeddable"`
	Reason     string `json:"reason"`
}

// embedProbeClient never follows redirects on its own — probeOne walks them
// manually. Go's client buries the intermediate response when a later hop
// errors, and that response is exactly what the probe needs: nextcloud on
// this very box answers 302 → https://127.0.0.1:10081/login, https on a
// port that only speaks http, so the second hop can never succeed. With
// automatic redirects that TLS failure surfaced as "unreachable" for an app
// that had just answered. Walking hops by hand keeps every response we got.
var embedProbeClient = &http.Client{
	Timeout: embedProbeTimeout,
	Transport: &http.Transport{
		// Loopback-only self-signed certs; only headers are read.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// The probe fans out; do not let idle sockets accumulate.
		DisableKeepAlives: true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// loopbackHost reports whether a redirect target stays on this box. Anything
// else is refused: an app must not be able to bounce the probe into the LAN.
func loopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// frameAncestorsOf extracts the frame-ancestors directive from a CSP header,
// empty when absent.
func frameAncestorsOf(csp string) string {
	for _, directive := range strings.Split(csp, ";") {
		d := strings.TrimSpace(directive)
		if strings.HasPrefix(strings.ToLower(d), "frame-ancestors") {
			return d
		}
	}
	return ""
}

// embedVerdict decides whether a page with these headers loads in a
// cross-origin iframe. Split out and exported to tests so the truth table is
// pinned independently of the HTTP plumbing.
func embedVerdict(status int, frameOptions, frameAncestors string) (bool, string) {
	if status == 0 {
		return false, "unreachable"
	}
	xfo := strings.ToLower(strings.TrimSpace(frameOptions))
	switch {
	case strings.Contains(xfo, "deny"):
		return false, "x-frame-options: deny"
	case strings.Contains(xfo, "sameorigin"):
		// The dashboard is a different origin from host:port, so SAMEORIGIN
		// is a refusal here even though the word sounds permissive.
		return false, "x-frame-options: sameorigin"
	}
	fa := strings.ToLower(frameAncestors)
	if fa != "" {
		if strings.Contains(fa, "'none'") {
			return false, "frame-ancestors 'none'"
		}
		if !strings.Contains(fa, "*") {
			// A source list that names specific origins will not include the
			// dashboard's; claiming otherwise would render a blank panel.
			return false, "frame-ancestors restricts embedding"
		}
	}
	return true, ""
}

func probeOne(scheme string, port int) EmbedProbeResult {
	r := EmbedProbeResult{Port: port, Scheme: scheme}

	// Walk redirects by hand, keeping the best answer seen so far. A hop
	// that fails — TLS against an http port, a dead target — does not erase
	// the hops that succeeded: the last response IS the app's answer.
	target := fmt.Sprintf("%s://127.0.0.1:%d/", scheme, port)
	var last *http.Response
	for hop := 0; hop < 5; hop++ {
		resp, err := embedProbeClient.Get(target)
		if err != nil {
			break
		}
		if last != nil {
			last.Body.Close()
		}
		last = resp
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			break
		}
		loc, err := resp.Location()
		if err != nil || !loopbackHost(loc.Hostname()) {
			break // off-box redirect: the 3xx itself is the answer
		}
		target = loc.String()
	}
	if last == nil {
		r.Embeddable, r.Reason = embedVerdict(0, "", "")
		return r
	}
	defer last.Body.Close()
	r.Status = last.StatusCode
	r.FrameOptions = last.Header.Get("X-Frame-Options")
	r.FrameAncestors = frameAncestorsOf(last.Header.Get("Content-Security-Policy"))
	r.Embeddable, r.Reason = embedVerdict(r.Status, r.FrameOptions, r.FrameAncestors)
	return r
}

// PostEmbedProbe — POST /v1/sys/embed-probe {targets:[{port,scheme}]}.
//
// Gated on PermAppStart: guests hold PermSystemRead and PermAppView, and a
// loopback probe in a guest's hands is an internal port scanner. Users who
// may open apps may also ask whether those apps embed.
func PostEmbedProbe(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}
	var req struct {
		Targets []EmbedProbeTarget `json:"targets"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	if len(req.Targets) == 0 || len(req.Targets) > embedProbeMaxTargets {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("targets must number between 1 and %d", embedProbeMaxTargets),
		})
	}
	for i := range req.Targets {
		t := &req.Targets[i]
		if t.Port < 1 || t.Port > 65535 {
			return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "port out of range"})
		}
		switch t.Scheme {
		case "":
			t.Scheme = "http"
		case "http", "https":
		default:
			return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "scheme must be http or https"})
		}
	}

	results := make([]EmbedProbeResult, len(req.Targets))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t EmbedProbeTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeOne(t.Scheme, t.Port)
		}(i, t)
	}
	wg.Wait()

	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: results})
}
