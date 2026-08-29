# Token Revocation Authority

Architectural decision record for CN-SEC-03 (M1/M2 in the enterprise review).

## The defect

Deleting a user, or terminating their sessions, stored the decision in
`sync.Map` `RevokedUsers`, declared in `CasaNova-UserService/route/v1/user.go`.

Two independent failures follow, both read directly from the tree:

1. **Only UserService consults it.** `grep -rn "revoked\|Revoked"` across the
   route packages of CasaNova (200+ routes: files, apps, storage, power, host
   administration), AppManagement, LocalStorage, Driver, Virt and
   JobManagement returns *nothing*. Every one of those services validates the
   token's signature and expiry and stops there. A deleted user's access token
   keeps working against all of them for its remaining lifetime — up to 3 hours
   for an access token, and their refresh token keeps minting new ones for up
   to 7 days.

2. **The map is in memory.** The signing key is deliberately persisted as PEM
   under `DBPath` (`loadOrCreateJWTKey`, with a comment naming the
   "restart-nukes-all-sessions bug" it exists to fix). So tokens survive a
   restart while the record of their revocation does not. Restart UserService
   and even *it* forgets.

`jwt.IsRevokedFunc` was declared for exactly this purpose, is checked by
`jwt.JWT()` — and is assigned by nobody, in any module. `jwt.JWT()` itself has
no callers. The hook has never rejected a request. This is the sixth instance
in this codebase of correct code that never executes.

## Decision: architectural, because the local patch is impossible

A local patch means "add a revocation check to each service's middleware".
It cannot restore the invariant:

- `RevokedUsers` lives in another **process**. The other six services cannot
  read it at all — there is no API, no shared memory, no bus message for it.
- Even inside UserService the state is lost on restart, so a local check would
  enforce a list that empties itself.

The vulnerable trust relationship is *"each service independently decides a
signed token is still valid"*. Blocking one payload leaves it intact. So the
fix introduces the missing component: a single revocation authority all
services consult.

```
ARCHITECTURAL CHANGE   Shared, persistent, self-provisioning revocation
                       authority (CasaNova-Common/pkg/tokenrevocation),
                       consulted by every service's JWT middleware.
THREAT ELIMINATED      A revoked or deleted principal continuing to act on
                       every service until natural token expiry, and
                       revocation being forgotten across restarts.
WHY LOCAL FIX          The state is per-process and in-memory; other services
  IS INSUFFICIENT      have no way to read it and it does not survive restart.
NEW TRUST BOUNDARY     One file, one writer (the user service), many readers.
                       Readers trust it the way they already trust the JWKS
                       address file in the runtime directory.
NEW PRIVILEGES         None. No new process, socket, port or capability. The
                       writer already runs as the user-management authority.
NEW FAILURE MODES      Unreadable or corrupt list; a stale reader view.
                       Addressed under Fail-closed and Recovery below.
PROVISIONING           Provision() creates the directory and file with the
                       intended mode, repairs a wrong mode, migrates an older
                       schema, and is a no-op when already correct.
SELF-VALIDATION        SelfTest() exercises the negative boundary: it revokes
                       a reserved probe principal, asserts a token issued
                       before that instant is REJECTED and one issued after is
                       ACCEPTED, then removes the probe. It proves the
                       property, not that a component exists.
FAIL-CLOSED            If provisioning or self-test fails, the authority
                       reports validation_failed and IsRevoked answers "yes"
                       for every principal: tokens are refused rather than
                       silently accepted unchecked. There is no fallback to
                       the old unchecked path.
RECOVERY               Startup converges: absent -> created; correct -> no-op;
                       wrong mode -> repaired; corrupt -> quarantined to
                       .corrupt-<n> and reported, never silently rewritten
                       (rewriting would erase real revocations = fail open).
                       Emergency path: remove the file and restart, which
                       provisions a fresh healthy list.
MIGRATION              The file carries a schema version. Version 0 (the
                       unversioned first draft) is read as version 1. An
                       unknown future version is refused, not guessed at.
TEST PLAN              Provision, reprovision, repair of a wrong mode,
                       corrupt-file handling, the enforcement boundary itself,
                       cross-restart persistence, and fail-closed behaviour.
                       Mutation: reverting any service's middleware check must
                       fail its test.
PERFORMANCE COST       One stat per second per process, plus a map lookup per
                       request. The public key cache already accepts 10s of
                       staleness for a value of comparable weight.
ROLLBACK               Delete the package and the middleware calls. No
                       persistent side effects beyond one file; removing it
                       returns the system to the previous behaviour.
```

## Attacking the proposal

*If the authority is itself compromised, misconfigured, missing, outdated,
crashed or partially initialised, what does the attacker gain?*

- **Attacker can write the file.** They must already be root — the file is
  root-owned under `/var/lib/casanova`. At that point they own the signing key
  too and can mint any token they like. The authority grants them nothing new.
- **Attacker deletes the file.** Provision recreates it empty, which un-revokes
  previously revoked users. This is the strongest attack, and it requires root.
  Mitigated in the same way as key theft: it is out of the model, and the
  deletion is visible in the health state as a list that lost its entries.
- **Attacker corrupts the file.** Reads fail, the authority reports
  validation_failed, and every token is refused. Denial of service, not
  bypass — and still requires root.
- **Authority partially initialised.** `IsRevoked` consults status first; an
  uninitialised authority is `unavailable`, which denies. There is no window in
  which it answers "not revoked" because it has not loaded yet.
- **Reader has a stale view.** Bounded by the 1s refresh. A revocation takes
  effect within a second on every service; the window is far shorter than the
  3-hour token lifetime it replaces.

The authority holds no secrets — user IDs and timestamps only — so reading it
grants nothing. Its authority is deliberately minimal: it can refuse tokens,
never issue or elevate them.
