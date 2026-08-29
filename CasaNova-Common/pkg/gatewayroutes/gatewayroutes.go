// Package gatewayroutes is the single source of truth for which CasaNova
// service owns which gateway path prefix.
//
// # Why this package exists
//
// The gateway keeps a flat map of prefix -> backend target and dispatches a
// request to the LONGEST registered prefix that matches on a path-segment
// boundary (see CasaNova-Gateway/service/management.go, GetProxy +
// pathHasPrefix). Two consequences follow, and both have already bitten us in
// production:
//
//  1. If two services register the SAME prefix, the map entry is simply
//     overwritten. Every service also runs a 30s re-registration watchdog, so
//     the route does not settle on a winner — it FLAPS between the two
//     backends every 30 seconds. Half of the requests 404.
//
//  2. If service A registers a generic prefix ("/v1/container") and service B
//     registers routes underneath it without registering a longer prefix of
//     its own, B's handlers never receive traffic. The code compiles, the
//     handlers exist, they can even be security-hardened — and they are dead.
//     That is exactly how container log streaming ("/v1/container/:id/logs")
//     ended up returning 404 while the correct handler sat unused in another
//     binary.
//
// Neither failure is visible from inside a single service: no build error, no
// panic, no log line. The only defence is to declare ownership in ONE place
// and to check the declaration mechanically. That is what this package is.
//
// # How to use it
//
// A service registers exactly the prefixes returned by Prefixes(owner). If a
// service needs a new prefix, it is added here first — Validate (exercised by
// this package's tests) refuses declarations that collide with another owner
// or that silently carve a hole in another owner's subtree.
//
// A path parameter can never be part of a prefix ("/v1/container/:id/logs" is
// not a prefix — ":id" is arbitrary), so a service that wants to serve a
// ":id"-shaped route MUST own the generic parent prefix. Resolve() makes that
// rule testable from inside each service: a route registered on a router whose
// concrete path does not Resolve() to that service is unreachable.
package gatewayroutes

import (
	"fmt"
	"sort"
	"strings"
)

// Owner identifies a backend service that registers routes at the gateway.
// The string values are for diagnostics only; the gateway keys on the target
// URL, not on these names.
type Owner string

const (
	OwnerCasaNova      Owner = "casanova"
	OwnerAppManagement Owner = "app-management"
	OwnerLocalStorage  Owner = "local-storage"
	OwnerUserService   Owner = "user-service"
	OwnerMessageBus    Owner = "message-bus"
	OwnerJobManagement Owner = "job-management"
	OwnerDriver        Owner = "driver"
	OwnerVirt          Owner = "virt"
	OwnerStaticUI      Owner = "static-ui"
)

// RootPrefix is the catch-all the static UI server registers. It is excluded
// from overlap analysis: every other prefix is inside it by construction, so
// treating it as a normal claim would require declaring every route in the
// system as a carve-out and would drown the real conflicts in noise.
const RootPrefix = "/"

// Claim is one service's declared gateway surface.
type Claim struct {
	// Prefixes are registered verbatim by the owner's main(). Services that
	// consume this package MUST pass exactly Prefixes(owner) to the gateway;
	// anything else re-opens the drift this package exists to prevent.
	Prefixes []string

	// Derived are prefixes the owner computes at runtime (typically
	// strings.TrimRight(swagger.Servers[0].URL) for the generated v2 API) and
	// registers in addition to Prefixes. They are declared here so overlap
	// analysis sees the complete picture, but they are NOT returned by
	// Prefixes() — the owner keeps registering them from its own constant so
	// the OpenAPI spec stays authoritative for its own base path.
	Derived []string
}

// manifest is the declaration. Keep it sorted by owner and, within an owner,
// grouped the way the service's main() reads.
var manifest = map[Owner]Claim{
	OwnerCasaNova: {
		Prefixes: []string{
			"/v1/sys",
			"/v1/port",
			"/v1/file",
			"/v1/folder",
			"/v1/batch",
			"/v1/trash",
			"/v1/image",
			"/v1/samba",
			"/v1/notify",
			// Destructive storage maintenance (external formatter, free-space
			// sanitization). It needs its own published prefix: without one the
			// static UI's "/" catch-all answers every call, and the handlers —
			// RBAC-gated, revalidating, audited — would never run. Nothing else
			// claims /v1/maintenance; local-storage owns /v1/disks and
			// /v1/storage, which are the read/mount APIs and are untouched.
			"/v1/maintenance",
			// Cloud-storage providers (Google Drive / Dropbox / OneDrive), the
			// original CasaOS v1 meaning of "driver". casanova-driver serves
			// HARDWARE drivers and owns /v2/driver only — both services used to
			// claim /v1/driver, which made the route flap between them every
			// 30s (the watchdog re-registration interval).
			"/v1/driver",
			"/v1/cloud",
			"/v1/recover",
			"/v1/other",
			"/v1/zt",
			"/v1/test",
			"/v1/doc",
			// Public file-share links. Deliberately outside the JWT'd /v1
			// tree — the whole point is an unauthenticated URL. It also has to
			// be published in its own right: without this claim the static UI's
			// "/" catch-all swallowed it, so every share link this service
			// handed a user ("https://<host>/public/share/<id>") answered 404.
			"/public/share",
			// Container endpoints that are NOT part of app-management's
			// per-container API. Each is a static carve-out out of
			// app-management's "/v1/container" (see carveouts below); a
			// route with a path parameter in the middle cannot be carved out
			// this way, which is why ":id"-shaped container routes belong to
			// app-management and to nobody else.
			"/v1/container/all",
			"/v1/container/stats",
			"/v1/container/image",
			// ZimaOS-client compatibility shim + global file search.
			"/v2/zimaos",
			"/v2/mod_management",
			"/v2/search",
		},
		Derived: []string{"/v2/casaos", "/doc/v2/casaos", "/v3/file"},
	},
	OwnerAppManagement: {
		Prefixes: []string{
			"/v1/apps",
			// Restic-backed backup schedules. The handlers have existed and
			// been RBAC-gated for a while, but the prefix was never published,
			// so every call to the backup API returned 404 from the static
			// UI's "/" catch-all and the whole feature was unreachable.
			// Read requires PermBackupRead (user + admin), every mutation
			// requires PermBackupWrite (admin only); a tokenless caller
			// resolves to guest, which holds neither.
			"/v1/backup",
			// Generic prefix: app-management owns the WHOLE /v1/container
			// subtree apart from the carve-outs declared below. Every
			// "/v1/container/:id/..." route must therefore live in this
			// service — the gateway cannot route a path parameter.
			"/v1/container",
			"/v1/app-categories",
		},
		Derived: []string{"/v1doc/v1/app_management", "/v2/app_management", "/doc/v2/app_management"},
	},
	OwnerLocalStorage: {
		Prefixes: []string{"/v1/usb", "/v1/disks", "/v1/storage"},
		Derived:  []string{"/v2/local_storage", "/doc/v2/local_storage"},
	},
	OwnerUserService: {
		Prefixes: []string{"/v1/users", "/.well-known/jwks.json"},
		Derived:  []string{"/v2/users", "/doc/v2/users"},
	},
	OwnerMessageBus: {
		Derived: []string{"/v2/message_bus", "/doc/v2/message_bus"},
	},
	OwnerJobManagement: {
		Derived: []string{"/v2/job_management", "/doc/v2/job_management"},
	},
	OwnerDriver: {
		Derived: []string{"/v2/driver", "/doc/v2/driver"},
	},
	OwnerVirt: {
		Derived: []string{"/v2/virtualization"},
	},
	OwnerStaticUI: {
		Prefixes: []string{RootPrefix},
	},
}

// carveout records an intentional hole punched in another owner's subtree.
// Declaring it is mandatory: Validate rejects an undeclared overlap, so the
// next person who adds "/v1/container/whatever" to the wrong service gets a
// failing test instead of a silently dead endpoint.
type carveout struct {
	Prefix string
	Owner  Owner
	Reason string
}

var carveouts = []carveout{
	{
		Prefix: "/v1/container/all",
		Owner:  OwnerCasaNova,
		Reason: "container discovery lists unmanaged containers too; app-management only knows compose projects",
	},
	{
		Prefix: "/v1/container/stats",
		Owner:  OwnerCasaNova,
		Reason: "resource stats are sampled by the main service alongside host metrics",
	},
	{
		Prefix: "/v1/container/image",
		Owner:  OwnerCasaNova,
		Reason: "registry tag lookup lives with the main service's outbound HTTP client",
	},
}

// Prefixes returns the prefixes the given owner must register at the gateway.
// The result is a fresh slice: callers append their runtime-derived paths to
// it, and must not be able to mutate the manifest by doing so.
func Prefixes(owner Owner) []string {
	claim := manifest[owner]
	out := make([]string, len(claim.Prefixes))
	copy(out, claim.Prefixes)
	return out
}

// AllPrefixes returns every declared prefix (Prefixes + Derived) for an owner.
func AllPrefixes(owner Owner) []string {
	claim := manifest[owner]
	out := make([]string, 0, len(claim.Prefixes)+len(claim.Derived))
	out = append(out, claim.Prefixes...)
	out = append(out, claim.Derived...)
	return out
}

// Owners returns every declared owner, sorted, so tests iterate deterministically.
func Owners() []Owner {
	out := make([]Owner, 0, len(manifest))
	for owner := range manifest {
		out = append(out, owner)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HasPrefix reports whether prefix is a path-SEGMENT prefix of path.
//
// This mirrors pathHasPrefix in CasaNova-Gateway/service/management.go. Plain
// strings.HasPrefix would let "/v1" match "/v1foo" and hand a request to the
// wrong backend. Keep the two implementations in step: the gateway is the one
// that actually routes, this one is what the tests reason with.
func HasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+"/")
}

// Resolve reports which service the gateway will hand the given request path
// to, using the gateway's longest-prefix-wins rule.
//
// It returns false only when nothing matches at all, which cannot happen while
// the static UI owns "/" but is kept honest so a caller never mistakes "no
// route" for "my route".
func Resolve(path string) (Owner, bool) {
	return resolve(manifest, path)
}

func resolve(m map[Owner]Claim, path string) (Owner, bool) {
	type entry struct {
		prefix string
		owner  Owner
	}
	entries := make([]entry, 0, 64)
	for owner, claim := range m {
		for _, p := range claim.Prefixes {
			entries = append(entries, entry{p, owner})
		}
		for _, p := range claim.Derived {
			entries = append(entries, entry{p, owner})
		}
	}
	// Longest prefix wins; ties broken by owner name so the result is stable
	// (the gateway's sort is length-only and therefore unstable on ties — a
	// tie is a duplicate claim, which Validate rejects outright).
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].prefix) != len(entries[j].prefix) {
			return len(entries[i].prefix) > len(entries[j].prefix)
		}
		return entries[i].owner < entries[j].owner
	})
	for _, e := range entries {
		if HasPrefix(path, e.prefix) {
			return e.owner, true
		}
	}
	return "", false
}

// Validate checks the manifest for the two failure modes described in the
// package doc. It is called by this package's tests; a violation is a build
// -time (test-time) failure, not something to discover on a production box.
func Validate() error {
	return validate(manifest, carveouts)
}

// validate takes the manifest as an argument so the tests can feed it
// mutated copies — a rule that cannot be shown to reject a bad manifest is
// not a rule.
func validate(m map[Owner]Claim, cos []carveout) error {
	claimedBy := map[string]Owner{}
	var all []string

	owners := make([]Owner, 0, len(m))
	for owner := range m {
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	for _, owner := range owners {
		seen := map[string]bool{}
		claim := m[owner]
		prefixes := make([]string, 0, len(claim.Prefixes)+len(claim.Derived))
		prefixes = append(prefixes, claim.Prefixes...)
		prefixes = append(prefixes, claim.Derived...)
		for _, prefix := range prefixes {
			if err := checkPrefixShape(prefix); err != nil {
				return fmt.Errorf("owner %q: %w", owner, err)
			}
			if seen[prefix] {
				return fmt.Errorf("owner %q declares %q twice", owner, prefix)
			}
			seen[prefix] = true
			if other, dup := claimedBy[prefix]; dup {
				return fmt.Errorf("prefix %q is claimed by both %q and %q: "+
					"the gateway keeps one target per prefix and both services re-register every 30s, "+
					"so the route would flap between the two backends", prefix, other, owner)
			}
			claimedBy[prefix] = owner
			all = append(all, prefix)
		}
	}

	declared := map[string]carveout{}
	for _, c := range cos {
		if c.Reason == "" {
			return fmt.Errorf("carve-out %q has no reason", c.Prefix)
		}
		if claimedBy[c.Prefix] != c.Owner {
			return fmt.Errorf("carve-out %q is declared for owner %q but the manifest gives it to %q",
				c.Prefix, c.Owner, claimedBy[c.Prefix])
		}
		if _, dup := declared[c.Prefix]; dup {
			return fmt.Errorf("carve-out %q declared twice", c.Prefix)
		}
		declared[c.Prefix] = c
	}

	// Every nesting of one owner's prefix inside another owner's prefix must
	// be an explicitly declared carve-out. An UNdeclared nesting is how a
	// subtree quietly changes hands.
	for _, inner := range all {
		if inner == RootPrefix {
			continue
		}
		for _, outer := range all {
			if outer == inner || outer == RootPrefix {
				continue
			}
			if !HasPrefix(inner, outer) {
				continue
			}
			if claimedBy[inner] == claimedBy[outer] {
				continue // same service on both sides: no ownership question
			}
			if _, ok := declared[inner]; !ok {
				return fmt.Errorf("prefix %q (owner %q) is nested inside %q (owner %q) "+
					"but is not declared as a carve-out: the inner service silently takes over part of "+
					"the outer service's subtree", inner, claimedBy[inner], outer, claimedBy[outer])
			}
		}
	}

	// A stale carve-out is its own hazard: it documents a hole that no longer
	// exists, so the next reader trusts a boundary the gateway does not draw.
	for _, c := range cos {
		nested := false
		for _, outer := range all {
			if outer == c.Prefix || outer == RootPrefix {
				continue
			}
			if HasPrefix(c.Prefix, outer) && claimedBy[outer] != c.Owner {
				nested = true
				break
			}
		}
		if !nested {
			return fmt.Errorf("carve-out %q is stale: it is not nested inside any other owner's prefix", c.Prefix)
		}
	}

	return nil
}

func checkPrefixShape(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("empty prefix")
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("prefix %q must start with /", prefix)
	}
	if prefix != RootPrefix && strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("prefix %q must not end with / (the gateway matches on segment boundaries)", prefix)
	}
	if strings.Contains(prefix, ":") || strings.Contains(prefix, "*") {
		return fmt.Errorf("prefix %q contains a path parameter: the gateway routes on literal prefixes only, "+
			"so a service that needs such a route must own the generic parent prefix", prefix)
	}
	return nil
}
