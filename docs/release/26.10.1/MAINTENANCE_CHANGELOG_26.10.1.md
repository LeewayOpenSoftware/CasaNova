# CasaNova 26.10.1 — Maintenance changelog

*2026-10-24. Every change between the 26.10 release and 26.10.1, grouped by kind. **[visible]** marks a change an operator or user can notice. Dates are UTC. Security details that would help an attacker are kept to what the fix itself reveals; the full analysis is in `SECURITY_AUDIT_26.10.1.md`.*

Release package: `casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz` (build `18.6.3.beta1.2410261443`), sha256 `300cf73a1d1c623daac201dfe6d5f6b6fefa9b0c2c6ff8b38c7372764e4314a8`. Every code fix was confirmed by an independent review, each with its own red→green test.

---

## Security fixes

### User service (authentication, sessions, 2FA)

- **Avatar path could point outside the user's directory** (F-01, critical). `PUT /v1/users/current` bound `avatar` straight from the body and `GET /v1/users/avatar` served that path as root. `PutUserInfo` now keeps the stored avatar; `GetUserAvatar` serves a stored path only when it resolves, symlinks included, inside the user's own directory, falling back to the default avatar otherwise.
- **Role change now revokes the target's sessions** (F-10). `PUT /v1/users/:id/role` only rewrote the database row while every service authorises from the role claim in the signed token. An actual role change now burns the target's refresh families and records a revocation cutoff; re-submitting the same role is a no-op. **[visible]** a user whose role changes must sign in again.
- **Replayed TOTP codes are refused at login** (F-14). The same 6-digit code was accepted any number of times within its ±1-step window. The newest accepted step is remembered per secret, in memory. **[visible]** a second use of the same code within ~90 s fails; a restart of the user service reopens one ~90 s window. Backup codes unchanged.
- **OIDC `id_token` is no longer accepted as an access token** (F-12). The id_token minted by `/v1/oauth2/token` carried no user id and `jwt.Validate` accepted it as principal 0 on every service, outside any revocation. `Validate` (Common) and `finalizeAccessClaims` (UserService) now refuse claims with id ≤ 0.
- **JSON request bodies capped at 1 MiB on v1 routes** (F-06). No layer limited the body of pre-auth routes such as `/v1/users/login`; one unauthenticated multi-GB POST could exhaust memory. Upload routes with their own caps (avatar 11 MiB, custom conf 2 MiB, image upload) are exempt. **[visible]** oversized bodies on those routes get 413 (400 when chunked).

### File API, storage authority and shares (`casanova` core)

- **System-disk switch withholds runtime directories and host-tool credentials** (F-02, critical). The withheld-secrets check only ran for paths outside authorized storage, but the AppData root is `/var/lib/casaos`, which holds the signing key and the user database; it also missed `/run/casanova/internal` (the inter-service secret) and host tools' own credentials (zerotier secrets, certificate private keys, `~/.ssh/id_*`, `rclone.conf`, `~/.docker/config.json`, Samba's private directory). The check now runs for every switched-on read and covers those names. **[visible]** those files stay listable but unreadable with the switch.
- **`GET /v3/file` asks the storage authority before serving** (F-03). The standalone `?token=` handler only applied the lexical blocked-prefix list and then `http.ServeFile`. It now sanitises on the resolved path and opens through `storageauth.OpenAuthorizedStrict` with the token's principal. **[visible]** accounts without rights on a path get 403 instead of the file.
- **v2 file search answers only what the account may list** (F-08). `POST /v2/search/file` checked only `PermFileRead` and then walked `/DATA`, `/mnt`, `/media` whole. It now consults the storage authority like the v1 search; admins keep the full search. **[visible]** a non-admin's search returns only listable areas, and nothing when the authority is not in force.
- **Public folder shares are contained to the shared folder** (F-07). The anonymous folder zip used the share's parent as containment root, so a symlink to a sibling was followed and served. New `AddFileWithin` keeps names relative while containing symlinks to the shared folder; a share whose origin was replaced by a symlink now answers 404.
- **Multi-item download contains symlinks per selected entry** (F-16). `GET /v1/file/download` used the selection's common prefix as the root, so a symlink inside a selected folder pointing to an unselected sibling was archived. Each entry is now archived with its own resolved location as the root.
- **Archive extraction refuses planted directory symlinks** (F-05). Only the final component of each member was Lstat'd; `MkdirAll`/`CreateTemp`/`Rename` resolve intermediate ones. Every existing component between the destination and the member is now checked. **[visible]** extraction stops with 400 (partial output kept) when a symlink is found.
- **`POST /v1/file` no longer follows a dangling symlink** (F-11). `os.Stat` reports a dangling link as missing and `os.Create` then follows it. Creation now uses `O_CREATE|O_EXCL|O_NOFOLLOW`. **[visible]** creating over a dangling symlink answers "file or dir exists".
- **Trash refuses roots whose `files/` or `info/` is a symlink** (F-04). A planted `.Trash-casanova/files` symlink let the daily purge delete outside the trash. `List` now Lstats root, `files/` and `info/` and skips (and logs) a planted root; `Delete` re-checks; `ensureTrashDir` creates per component. **[visible]** such a trash root is skipped rather than cleaned. Closed together with F-17 below.
- **Trash entry names must be one path component** (F-17). An `info/` file named `...trashinfo` (also `..trashinfo`, `.trashinfo`) produced the entry name `..`, `.` or empty; `Delete`, `Empty` and the daily purge then removed the whole trash of that disk, and `Restore` would have moved the trash root. `readEntry` skips such files and `Trash`, `Restore` and `Delete` refuse such names. Found while reviewing F-04; its regression test runs against a hand-built `..` entry.

### Gateway and message bus

- **502 bodies no longer echo the proxy error** (F-15). The reverse-proxy error handler wrote `err.Error()` (internal backend address and port) to the client. **[visible]** the body is now a generic "Backend unavailable"; the full error is still logged.
- **Event WebSocket bounds inbound frames** (F-09). The read pump of `SubscribeEventWS` buffered frames of any size. It now answers control frames, discards data frames without buffering, and drops the connection on a frame over 4 KiB. No client sends data on that socket.

### App management

- **App-store downloader refuses https→http redirects** (F-13). The SSRF-guarded client accepted a redirect from https to plain http, so a store registered over https could be served over plaintext by one redirect. **[visible]** a store that redirects from https to http no longer downloads; stores registered as `http://` are unchanged (R-04).
- **`GetDockerNetworks` and `AppUsageList` gain an explicit `requirePerm(PermAppView)`** (F-D1, defence in depth). Both already sat behind the v1 JWT group and `PermAppView` is held by every role, so no caller loses access; they were the only two handlers in the file relying solely on the outer middleware. New regression test.

## Toolchain

- **Every binary is compiled with go1.26.10.** This closes GO-2026-6809 / CVE-2026-111943 (`crypto/x509` did not distrust compromised intermediates; fixed only in go1.26.10 and go1.27.3, with no fix in the 1.24 series 26.10 was built with) and every standard-library advisory govulncheck reported on the 26.10 binaries.
  - `toolchain go1.26.10` in the 11 `go.mod` files; the `Makefile` exports `GOTOOLCHAIN=$(GO_TOOLCHAIN)`, so the go command fetches and uses exactly that release whatever Go is installed.
  - `make build` ends with `check-go`, which fails if any binary reports a different Go version (checked with `make check-go GO_TOOLCHAIN=go1.24.2`, which fails and lists every binary).
  - Validation: `go version` reports go1.26.10 on all 10 binaries; the GO-2026-6809 distrust list is present in all 10; `govulncheck -mode=binary` (v1.9.0) goes from 48–63 standard-library findings per binary to 0; a TLS probe through `net/http` and `x/net/http2` still verifies real public chains and rejects expired, self-signed, untrusted and wrong-host certificates.

## Dependencies

- **Compatible bumps of the modules with a reachable advisory.** Only modules where govulncheck (source mode) shows a call path to the vulnerable symbol were bumped, and only to a patch or minor release:
  - `golang.org/x/net` → v0.60.0, `golang.org/x/crypto` → v0.57.0, `golang.org/x/text` → v0.42.0 (all 11 modules);
  - `golang.org/x/image` v0.6.0 → v0.45.0 (CasaNova);
  - `golang-jwt/jwt/v4` v4.5.0 → v4.5.2 (all except CLI);
  - `gorilla/websocket` v1.5.0 → v1.5.3; `ulikunitz/xz` v0.5.11 → v0.5.15;
  - `google.golang.org/grpc` v1.74.2 → v1.83.1 and `go-jose/go-jose/v4` v4.0.5 → v4.1.4 (AppManagement);
  - `google.golang.org/protobuf` v1.30.0 → v1.33.0 (LocalStorage);
  - plus the indirect patch/minor updates minimal version selection pulls in (x/sys, x/sync, x/term, otel, google api…).
- x/crypto and x/net require `go 1.26.0`, so every `go.mod` moves `go 1.24` → `go 1.26.0` with `godebug default=go1.24`, which keeps the runtime defaults of 26.10 (`go list -f '{{.DefaultGODEBUG}}'` is unchanged).
- Reachable advisories per module (source mode), before → after: Common 23→4, Gateway 15→5, MessageBus 17→6, UserService 16→6, LocalStorage 58→27, JobManagement 17→7, AppManagement 60→30, Driver 10→1, Virt 10→1, CasaNova 34→7, CLI 10→9. What remains is in modules that need a major upgrade or a replacement (below), plus `x/crypto/openpgp` (GO-2026-5932), which has no fix.
- **The UI build refuses an unpatched Node.js** (`check-node`): `make frontend` accepts only Node ≥22.23.4, ≥24.22.1 or ≥26.12.1 of the same major line; `package.json` `engines` and `.nvmrc` say the same. Node ships nothing in the product; this closes the build-chain exposure to the October 2026 Node.js releases. **[visible to builders]** the build machine needs a patched Node.
- **Accepted risk:** `labstack/echo/v4` stays v4.12.0 (GO-2026-6293 / CVE-2026-55677, Gateway). The fixed releases remove the `middleware.JWTWithConfig` authentication layer used by 9 services, and the advisory's precondition (a protected group serving a broader static root) does not occur: the gateway's only static route serves the public UI. Moves to 27.04 with a migration to `echo-jwt`.
- **Deferred to 27.04 / a later 26.10.x:** major upgrades and replacements of unmaintained modules — `golang-jwt/jwt` v3, `mholt/archiver/v3` and `rardecode` (no fix published), `getkin/kin-openapi`, `rclone`, the containerd/docker client stack, azidentity/ntlmssp/otel/go-smb2.

## Release

- **Version 26.10.1**: `BASE_VERSION` 18.6.2 → 18.6.3 (above every installed 18.6.2.*, below 19.0.0 so 27.04 never sees it as an upgrade), `DISPLAY_VERSION` 26.10 → 26.10.1 in the Makefile, `CasaNova/common/constants.go` and the UI fallbacks (About, update card); channel beta. **[visible]** the About page shows 26.10.1.
- **Final package** rebuilt after the phone fixes: build `18.6.3.beta1.2410261443`. Earlier builds with the same file name (`18.6.3.beta1.2410260213`, sha256 `d05d00bf…`; `18.6.3.beta1.2410261427`, sha256 `757fc117…`) are superseded; tell them apart by build or sha256.
- **Packaging note:** `make package` also needs the installer/updater scripts and the default app catalogue under `build/full/`, which are not yet in this repository; the 26.10.1 package took them unchanged from the 26.10 package.

## User interface

- **Usable on phones** (tested at 390 px; below Tailwind's sm/md only, desktop unchanged): the top bar folds its app icons into a "More" menu; Settings becomes list → detail with a back link; settings rows, Containers and compose apps wrap instead of overlapping; Files puts the sidebar behind a "Places" toggle and the toolbar wraps instead of clipping Upload; a broken app icon falls back to the generic glyph; 100dvh layout so the page bottom is not under the browser toolbar.

## Documentation (no behaviour change)

- `docs/release/26.10.1/`: `SECURITY_AUDIT_26.10.1.md`, `CVE_MATRIX_26.10.1.md`, `RELEASE_NOTES_26.10.1.md`, `REGRESSION_RESULTS_26.10.1.md`, `PRODUCTION_VALIDATION_26.10.1.md` and this changelog.

## Not changed on purpose

- Global IP allow-list still exempts loopback, and tunnel traffic arrives over loopback (R-01): documented for 26.10.1, change planned for 27.04 with a behaviour-change note.
- VNC console and SSH terminal WebSockets have no read limit (R-09): a size cap is under consideration.
- No data migrations in 26.10.1; the diff touches no on-disk formats.
