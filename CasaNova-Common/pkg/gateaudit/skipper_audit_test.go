package gateaudit

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// No module may decide authentication from the peer's address.
//
// The gateway proxies the LAN and the Cloudflare tunnel over 127.0.0.1, so the
// socket peer carries no information about who is calling. Ten routers got this
// wrong; the tenth survived a grep sweep by being a named function, which is
// why this walks the AST.
//
// If a legitimate need ever arises, it is not "trust this IP" — it is a
// credential (utils/internalauth) or a listener only root can open.
func TestNoModuleAuthenticatesByIPAddress(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("the source tree is unreadable, so this check went blind: %v", err)
	}

	scanned := 0
	var all []IPDecision
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < 8 || e.Name()[:8] != "CasaNova" {
			continue
		}
		found, err := ScanIPAuth(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("scanning %s failed: %v", e.Name(), err)
		}
		scanned++
		all = append(all, found...)
	}
	if scanned < 5 {
		t.Fatalf("only %d CasaNova modules were scanned from %s — this check is "+
			"not seeing the tree it claims to audit", scanned, root)
	}

	for _, d := range all {
		if why, ok := reviewedIPAuth[shortPath(root, d.File)+":"+itoa(d.Line)]; ok {
			t.Logf("KNOWN, not fixed: %s:%d — %s", shortPath(root, d.File), d.Line, why)
			continue
		}
		t.Errorf("%s:%d: %s decides authentication from the request's peer "+
			"address. Every request arrives over 127.0.0.1 because the gateway "+
			"proxies it, so this admits the internet. Use "+
			"CasaNova-Common/utils/authskip (internal secret + reviewed route "+
			"allowlist) instead.", d.File, d.Line, d.Expr)
	}
}

// reviewedIPAuth records IP-based auth decisions that were READ and judged, and
// which are NOT yet fixed. An entry here is debt with a name, not an absolution:
// removing one without fixing the code re-opens the finding and this test says so.
//
// The bar for an entry is high — the nine router skippers this test was written
// for do not qualify, because the gateway proxies the internet to them over
// loopback. These do, for one specific reason: they are on a listener nothing
// proxies to.
var reviewedIPAuth = map[string]string{
	"CasaNova-Gateway/route/management_route.go:130": "the gateway's MANAGEMENT API, " +
		"which listens on its own net.Listen(localhost, \"0\") — a separate " +
		"ephemeral loopback socket, not the public :85 listener, and nothing " +
		"proxies to it. So unlike the nine router skippers, IsLoopback here really " +
		"does mean 'a process on this host'. RESIDUAL RISK: 'this host' is any " +
		"local user, not only root, and this API can create a route that hijacks " +
		"any path prefix — a local privilege escalation. FIX REQUIRES BOTH SIDES " +
		"AT ONCE: CasaNova-Common/external/gateway.go (CreateRoute/ChangePort/" +
		"GetPort) sends no credential at all today, so tightening the gateway " +
		"alone leaves every service unable to register its routes and the box " +
		"serves nothing. It also needs the secret provisioned before the gateway " +
		"— which starts first — validates against it.",
	"CasaNova-Gateway/route/management_route.go:232": "second skipper in the same " +
		"management router; same listener, same reason, same fix.",
}

// shortPath renders an absolute path in the module-relative form the entries
// above are written in ("CasaNova-Gateway/route/management_route.go"), so that
// they are readable and — the part that matters — STABLE.
//
// It derives that from root rather than searching the string for "CasaNova-".
// The search took the FIRST occurrence, which is the CHECKOUT DIRECTORY as soon
// as the checkout is itself named CasaNova-something. A release worktree at
// ~/Documentos/CasaNova-2610 produced the key
// "CasaNova-2610/CasaNova-Gateway/route/management_route.go:130", which matches
// nothing above, so both reviewed-and-judged decisions re-opened as fresh
// findings and the release gate failed on debt it had already been told about.
// It only ever worked because the everyday checkout is named "CasaNova OS",
// which has no hyphen. A key that depends on what someone called their
// directory is not a key.
func shortPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func itoa(n int) string { return strconv.Itoa(n) }
