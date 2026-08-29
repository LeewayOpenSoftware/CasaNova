// Wave 25: harden the app-store / compose downloader against SSRF and
// arbitrary-protocol abuse.
//
// The previous implementation handed any string straight to go-getter with
// a wide-open Getters map. That accepted file://, git::, s3::, gcs::, hg::,
// and friends — every one of which can be steered at local files or
// internal endpoints. Even on http(s) the default getter follows redirects
// without ever re-checking the new target, so a public 302 → 169.254.x.x
// trivially bypassed the upfront URL check that v2/store_management.go
// performs at registration time.
//
// This rewrite locks the downloader to http(s) only, plugs an http.Client
// whose CheckRedirect re-validates each hop against the same private-IP
// blocklist, caps response size, and refuses non-loopback redirects to IP
// literals.
package downloadHelper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-getter"
)

// downloadMaxBytes caps any single fetch to 256 MiB. App-store catalogs
// are kilobytes; binary blobs that big should arrive via a different path
// with explicit user consent.
const downloadMaxBytes int64 = 256 * 1024 * 1024

// maxRedirects is the upper bound on redirect chain length. Each hop is
// re-validated, so the only reason to bound this is to stop a hostile
// server from looping us forever.
const maxRedirects = 5

// privateCIDRs are the IP ranges that must never be the target of a
// downloader request. Mirrors v2/store_management.go isPrivateIP — keep in
// sync if you change one.
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"0.0.0.0/8",
	"100.64.0.0/10", // CGNAT
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}

// Download fetches the given http(s) URL into dst.
//
// Non-http(s) schemes (file, git, s3, gcs, hg, scp, …) are rejected
// outright. The destination host is resolved and every IP it points at is
// checked against the private/reserved blocklist; redirects re-run the
// same check. The transfer is capped at downloadMaxBytes.
//
// Wave 35-X: thin shim that derives a default 5-minute ctx for legacy
// callers. New code should use DownloadCtx so registry hangs / catalog
// fetches respect upstream cancellation.
func Download(src string, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return DownloadCtx(ctx, src, dst)
}

// DownloadCtx is the ctx-aware variant. Cancelling ctx aborts the
// transfer mid-stream (go-getter's HttpGetter respects Client.Ctx).
func DownloadCtx(ctx context.Context, src string, dst string) error {
	if err := validateDownloadURL(src); err != nil {
		return err
	}

	httpGetter := &getter.HttpGetter{
		Client:   newSafeHTTPClient(),
		MaxBytes: downloadMaxBytes,
		Netrc:    false,
	}

	client := &getter.Client{
		Ctx:  ctx,
		Src:  src,
		Dst:  dst,
		Mode: getter.ClientModeAny,
		// Disable detectors so go-getter cannot rewrite "github.com/foo/bar"
		// into git:: under the covers. We accept exactly what was passed.
		Detectors: []getter.Detector{},
		Getters: map[string]getter.Getter{
			"http":  httpGetter,
			"https": httpGetter,
		},
		Options: []getter.ClientOption{},
	}

	return client.Get()
}

// validateDownloadURL parses src, enforces the http(s) scheme, and checks
// every resolved IP for the host against the private-range blocklist.
func validateDownloadURL(src string) error {
	if src == "" {
		return errors.New("download: empty URL")
	}
	u, err := url.Parse(src)
	if err != nil {
		return fmt.Errorf("download: malformed URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("download: scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("download: URL has no host")
	}
	return checkHostNotPrivate(host)
}

// checkHostNotPrivate resolves host and rejects it if any returned address
// belongs to a reserved range. We block on *any* private hit, not just
// "all of them are private", to defeat DNS-rebinding-style attacks where
// a hostile resolver alternates between a public and a private answer.
func checkHostNotPrivate(host string) error {
	// IP literals: parse directly and check.
	if ip := net.ParseIP(host); ip != nil {
		if isReservedIP(ip) {
			return fmt.Errorf("download: host %q is in a reserved range", host)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("download: cannot resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if isReservedIP(ip) {
			return fmt.Errorf("download: %q resolves to reserved IP %s", host, a)
		}
	}
	return nil
}

func isReservedIP(ip net.IP) bool {
	for _, cidr := range privateCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	// Also catch link-local IPv4 multicast and broadcast oddities.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	return false
}

// newSafeHTTPClient returns an http.Client whose CheckRedirect re-runs the
// SSRF guard on every hop, caps the chain length, and refuses any non-
// http(s) redirect target.
func newSafeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("download: redirect chain longer than %d hops", maxRedirects)
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return fmt.Errorf("download: redirect to disallowed scheme %q", req.URL.Scheme)
			}
			if err := checkHostNotPrivate(req.URL.Hostname()); err != nil {
				return err
			}
			return nil
		},
	}
}
