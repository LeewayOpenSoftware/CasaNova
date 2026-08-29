# LEG-001 — The file API confines by deny-list, not by containment

Flagship finding of the legacy saturation audit. Classification **G — legacy on a
critical path** (authorization + storage + privilege boundary).

## OLD ASSUMPTION

CasaOS was a single-user LAN appliance. Its file manager served "your own box",
reachable from your own network, and the threat it defended against was a user
wandering into system directories and breaking their install. A **deny-list** of
system paths — `/etc`, `/usr`, `/var`, `/proc`, … — expressed that model exactly:
*keep people out of the machinery, everything else is theirs.*

`utils.SanitizeFilePath` still implements that model. `BlockedSystemPrefixes` is a
fixed list of 15 system directories; `DataRoots` exists but is used only as an
**exemption** (a path that resolves under a storage root is allowed even when it
lands beneath a blocked prefix, so an install whose storage lives under `/var`
keeps working). Nothing in the primitive requires a path to be inside anything.

## CURRENT REALITY

CasaNova changed the world around that primitive:

- The API is **exposed to the internet** through a Cloudflare tunnel.
- Every service **runs as root**.
- There is a **multi-user role model**. `rbac.go` documents the `user` role as
  *"operation of their own files and apps the admin granted"* and grants it
  `PermFileRead | PermFileWrite | PermFileDelete`.

But `/home`, `/tmp`, `/` and **every top-level directory not on the fixed list**
are outside the deny-list, so they are permitted. And the notion of *"their own
files"* that the role model promises **does not exist in the implementation**:

```
grep -rn "user_id|username|homeDir|perUser" route/v1/file.go   → no scoping
```

Somebody already hit this and patched one caller. `route/v1/file.go:2145` carries
the comment:

> *"validatePath blocks system dirs but still permits `/` and `/home`, from which
> the search would enumerate the entire filesystem's names/paths. Require root
> under a storage mount."*

The search endpoint got its own confinement check. The primitive it calls was left
alone, and so were the other 38 call sites. This is the project's recurring
"fixed only one caller" pattern, in its purest form: the gap was seen, understood,
written down, and closed for exactly one handler.

## EVIDENCE

`CONFIRMED` — probe against the real primitive (probe deleted afterwards):

```
/DATA/foo                            Sanitize=/DATA/foo                  err=<nil>
/home/victim/.ssh/authorized_keys    Sanitize=/home/victim/.ssh/...      err=<nil>
/tmp/anything                        Sanitize=/tmp/anything              err=<nil>
/                                    Sanitize=/                          err=<nil>
/home                                Sanitize=/home                      err=<nil>
/nonexistent-toplevel/x              Sanitize=/nonexistent-toplevel/x    err=<nil>
/etc/shadow                          err=access to /etc is not allowed
/var/lib/casaos/db/jwt_ec_private.pem err=access to /var is not allowed
```

`CONFIRMED` — the write actually lands. Driving the sanitizer exactly as a handler
does, then performing the write it authorises:

```
SanitizeFilePath(<base>/home/victim/.ssh/authorized_keys) -> "<same path>" err=<nil>
*** WRITTEN OUTSIDE EVERY STORAGE ROOT: .../home/victim/.ssh/authorized_keys
    = "ssh-rsa ATTACKER\n" ***
```

`CONFIRMED` — no per-user scoping exists in the file API (grep above).

`INFERENCE` — the composite claim *"an authenticated non-admin `user` can read and
write anywhere outside the deny-list, as root, over the internet"*. Each link is
verified by reading the code — the gate is `requirePerm(PermFileRead/Write)`, the
`user` role holds both, `validatePath` delegates to `utils.SanitizeFilePath`, and
the handler then performs the operation on the returned path — but the chain has
not been driven end to end through a live HTTP request with a real user token.
Labelled INFERENCE deliberately rather than promoted.

Guest is **not** affected: `guestPerms` deliberately carries no file permission,
and its comment records that `PermFileRead` was removed from it for this reason.

## REQUIRED INVARIANT

> Every file-API operation resolves inside a storage area this appliance is
> configured to serve. Membership is decided by an allow-list derived from actual
> storage configuration — never by absence from a fixed list of system paths.

The deny-list keeps its job as defence in depth. It stops being the boundary.

## SIBLING SURFACES

39 call sites across six files (`route/v1/file.go`, `route/v1/trash.go`,
`route/v1/file_archive.go`, `route/v1/file_share_links.go`, `route/v2/file.go`,
`service/file_upload.go`). The correct fix is **SHARED — change the authority**,
not 39 local patches; patching callers is what produced this finding.

Related but distinct, found while tracing: `CasaNova-LocalStorage` carries a
byte-identical copy of `FixAndCleanPath` **without** the hardened layer built on
top of it, and three different mount-point validators disagree about whether
`/DATA` is an allowed mount root (`route/v1/rbac_gate.go` and
`route/v2/rbac_gate.go` allow `/mnt`, `/media`, `/DATA`; the inline check in
`route/v1/cloud.go` allows only `/mnt` and `/media`). Tracked separately.

## FIX CLASS: ARCHITECTURAL

A hardcoded allow-list of `/DATA`, `/mnt`, `/media` would be **worse than the bug**
for anyone whose storage lives elsewhere — a disk mounted at `/storage`, a share
under `/home/data`. Those installs work today and would start returning 403 with
no diagnosis. The boundary has to be **derived from what the appliance actually
serves**, not from a constant:

- the canonical `DataRoots`, as the fail-safe floor;
- filesystems the storage service reports as mounted;
- configured share and AppData roots.

Contract the new authority must satisfy (per the zero-manual-care rule):

| Requirement | How |
| --- | --- |
| Auto-provision | roots derived at startup and refreshed; no operator step |
| Idempotent | recomputing yields the same set; no side effects |
| Auto-validate | prove at startup that a path outside the set is refused — the negative boundary, not "the list is non-empty" |
| Fail-safe | if derivation fails, fall back to the static `DataRoots`, never to "allow everything" |
| Observable | health check reports the effective roots and why a path was refused |
| Wired | one authority; a sweep test asserts no handler resolves a path any other way |

## USER-VISIBLE SEMANTIC CHANGE — flagged, not decided unilaterally

Anyone who today browses `/home` or `/tmp` through the file manager will receive a
refusal. That is the point of the fix, but it is a product decision with more than
one defensible answer, so it is surfaced rather than silently chosen:

1. **Strict** — storage roots only. Safest; breaks any workflow outside them.
2. **Derived + configurable** (recommended) — storage roots plus operator-declared
   extra roots, so an unusual layout is a one-line configuration instead of a
   silent 403.
3. **Extend the deny-list** — add `/home`, `/tmp`, `/`. Smallest diff, and the
   wrong shape: the next unlisted top-level directory is open again.

## STATUS

`INVESTIGATE_FURTHER → MODERNIZE`. Not implemented yet: two hardening workflows are
editing `route/v1/file.go` and `service/file_upload.go` concurrently, and editing
the same files during their verification is what produced a false CRITICAL in an
earlier round. Implementation waits for a settled tree.
