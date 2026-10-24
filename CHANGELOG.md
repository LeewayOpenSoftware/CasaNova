# CasaNova Changelog

All notable user-facing changes land here. Internal refactors that don't change
behaviour are recorded in commit history, not this file. Dates are in UTC.

## [26.10.1] — security maintenance — 2026-10-24

Security and maintenance update for 26.10. No new features, no data
migrations. Full details: `docs/release/26.10.1/` (release notes,
maintenance changelog, security audit, CVE matrix, regression and
production validation).

### Security

- **Files and credentials.** The avatar setting could serve any host file
  to a signed-in account, and the system-disk switch let the signing key,
  the user database and host-tool credentials be read; both are closed.
  `GET /v3/file` now asks the storage authority; v2 search answers only
  what the account may list; public folder shares, multi-item downloads,
  archive extraction, file creation and the trash no longer follow
  symlinks planted inside storage; crafted trash entries (`..`) can no
  longer make delete/empty/purge erase a disk's whole trash.
- **Sessions and 2FA.** A role change ends that user's sessions; a TOTP
  code works once; an OIDC `id_token` is no longer accepted as an access
  token.
- **Resilience.** 1 MiB cap on JSON bodies of the v1 user routes; the
  event WebSocket bounds inbound frames; gateway 502 bodies no longer show
  the backend address; the app-store downloader refuses https → http
  redirects; two docker handlers gain an explicit permission gate.

### Build and dependencies

- Every binary is compiled with **go1.26.10** (GO-2026-6809 /
  CVE-2026-111943 and all standard-library advisories of the go1.24 build).
  `toolchain go1.26.10` in every `go.mod`, `GOTOOLCHAIN` exported by the
  Makefile, and `make build` ends with `check-go`, which refuses a binary
  built with any other Go.
- Patch/minor bumps of the modules with a reachable advisory: x/net
  v0.60.0, x/crypto v0.57.0, x/text v0.42.0, x/image v0.45.0, jwt/v4
  v4.5.2, gorilla/websocket v1.5.3, ulikunitz/xz v0.5.15, grpc v1.83.1,
  go-jose/v4 v4.1.4, protobuf v1.33.0. `go 1.26.0` with
  `godebug default=go1.24` keeps the runtime defaults of 26.10.
- `make frontend` refuses an unpatched Node.js (`check-node`: 22.23.4 /
  24.22.1 / 26.12.1 or later in the same line); `engines` and `.nvmrc`
  match.
- Accepted risk: `labstack/echo/v4` stays v4.12.0 (GO-2026-6293); the fix
  removes the JWT middleware every service uses and the advisory's
  precondition does not occur in the gateway. Major upgrades and
  replacements of unmaintained modules are deferred to 27.04.

### Changed

- **UI usable on phones**: the top bar folds into a "More" menu, Settings
  is list → detail, Files hides the sidebar behind a "Places" toggle, and
  rows and toolbars wrap instead of overlapping.
- You must sign in again after an administrator changes your role; a
  second use of the same 2FA code within ~90 s is refused.
- Version: BASE_VERSION 18.6.3, DISPLAY_VERSION 26.10.1 (channel beta).

## [26.10] — bug-fix roll-up

Fixes only — no new features and no behaviour changes beyond the defects
described. Backported onto the 26.10 line from the 27.04 audit passes.

### Fixed

- **Format guard let the boot disk be repartitioned on eMMC / SD boxes.**
  The three "refuse to format the system disk" guards compared device
  names by string prefix against a root device derived by trimming
  trailing digits. `/dev/mmcblk0p1` — the root filesystem of every eMMC
  and SD-card box — trimmed to `/dev/mmcblk0p`, which neither equals nor
  prefixes `/dev/mmcblk0`, so the guard passed and the request could
  repartition the card the OS was running from. The same prefix test
  failed the other way on a 27-disk NAS, where `/dev/sda` is a prefix of
  `/dev/sdaa` and the 27th disk was refused as the system disk. Device
  identity now goes through `pkg/devid`, which encodes the kernel's
  actual partition-naming rules instead of string containment.
- **Starting an app ran its background work on an already-cancelled
  context.** Three handlers built a background context and cancelled it
  the moment the handler returned 200, before the goroutine they spawned
  had done anything. App start burned all ten of its "wait for the
  container to stop" retries in microseconds and then issued a start that
  was dead on arrival; the same function also closed the Docker client
  out from under the goroutine it had handed it to. Global env change and
  image pull additionally read the pooled `echo.Context` after their
  handler returned, so a published event could carry another user's
  request data.
- **Job-state health probe graded a failed write as healthy.** The probe
  shadowed `err` inside its write branch, so the durability check below
  always saw `nil` — defeating the exact failure the probe exists to
  catch.
- **Log tail reader panicked on a stat failure and leaked a descriptor.**
  `NewReadLineFromEnd` ignored the error from `Stat()` and dereferenced
  the nil `FileInfo`; its directory early-return left the open file
  behind. Both paths now close the file and return the error.
- **Malformed image reference panicked the update sweep.** The registry
  challenge lookup discarded the parse error and called a method on the
  nil reference, so a bad image ref reached from the update-availability
  sweep crashed instead of reporting the bad ref. The compose validator's
  `recover` had the mirror-image bug: an unchecked type assertion on the
  panic value re-panicked for anything that was not an `error`, crashing
  the validator that was meant to contain it.
- **Deep disk benchmark filled the filesystem.** The scratch size reached
  both benchmark engines as one human-readable string, and they do not
  read the same syntax: fio accepts `1G`, while `dd` was handed the same
  string as a *block count* against `bs=1M`. Deep mode therefore ran
  `dd bs=1M count=1G` — about 1 EiB — and wrote until the disk was full
  or the five-minute deadline killed it. The size is now carried as a
  plain count of MiB that each engine formats for itself.
- **Scheduled task runs reported no end time.** The runner stamped the
  finish time from a deferred function, but returned its record by value
  through an unnamed return, so the stamp landed after the copy and every
  run was recorded with a zero `ended_at`. Completed tasks looked like
  they had never finished and run durations were meaningless.
- **Peer socket and SMART probes hid their failures.** Two `json.Marshal`
  errors were "handled" by printing to stdout on every connect, and a
  failed marshal still broadcast a nil frame; the SMART cache-add and
  unmarshal errors were commented out entirely, so a disk whose payload
  stopped parsing failed silently. All are logged now.
- **Software-update card leaked polling timers.** The settings card
  started its 3-second poll from four places with no single owner: one
  handle was kept in a local the unmount hook could not reach, two others
  overwrote the shared handle without clearing what was already running,
  and a fourth armed after the component had already been torn down.
  Leaving or re-entering the page left intervals running for the life of
  the session.
- **Malformed file-manager drag payload reached the move API.** The tab
  drop handler parsed the drag clipboard straight into a typed array with
  no validation. Malformed JSON threw out of an async handler as an
  unhandled rejection — the drop silently did nothing — and a payload
  that parsed to a non-array passed the emptiness check and was sent to
  the batch-move endpoint after the local view had already moved.
- **Container health probe's worst case took six times longer to fail than
  on 27.04.** The concurrency cap on `CheckContainerHealth` was already
  backported, but its per-probe timeout was still 30s where 27.04 had
  already dropped to 5s, so the same 16-slot cap bounded the worst case at
  480s here against 80s there. Timeout is now 5s on both lines. **This is
  a behaviour change, not a pure bug fix**: an app that legitimately takes
  longer than 5s (but less than 30s) to answer its health check now flips
  to unhealthy instead of passing.
- **Compose-app health probe had no concurrency cap and a 30s timeout,
  unlike the equivalent container probe.** `ComposeApp.HealthCheck()` never
  received the hardening `CheckContainerHealth` got: no bound on how many
  probes can run at once, and a timeout six times longer than the 5s used
  elsewhere. Its route is gated by the same `PermAppView` permission guest
  already holds — the identical amplification vector, left open for
  compose apps. Ported the same 16-slot cap and 5s timeout (kept as a
  separate cap from the container one, since the two surfaces have
  different burst shapes). **Behaviour change**, same as above: a
  legitimately slow compose app between 5s and 30s now flips unhealthy.

## [0.1.0] — 26.06 — first public release

CasaNova is a fork of [CasaOS](https://github.com/IceWhaleTech/CasaOS).
v0.1 is the first publishable build — earlier `5.0.0.devN` tarballs were
internal test builds and the version reset to `0.1.0` is intentional.

### Added

- **Self-update from browser**. Upload a `casanova-*.tar.gz` from the UI;
  the system applies it without an SSH session. Also supports scanning
  USB drives + the server's internal `/var/lib/casanova/updates` directory.
- **Dedicated Server settings page** with live CPU / memory / disk gauges,
  hostname / arch / kernel / uptime card, and one-click reboot + shutdown.
- **SMART disk monitor** — periodic background check (15 min interval)
  publishes a `local-storage:smart:critical` / `:warning` event to the
  bus so the dashboard surfaces failing drives in the notification
  centre instead of only in `journalctl`.
- **Container terminal** in the dashboard kebab menu — opens an
  interactive `docker exec` shell over WebSocket.
- **Container log streamer** — `docker logs -f` piped to a WebSocket
  in the per-app detail view, with ping/pong keepalive.
- **App auto-update opt-in** via `casanova-auto-update=true` Docker label.
  Daily sweep; compose-managed stacks go through `compose.Up` so labels,
  networks, and store metadata are preserved (raw-clone path retained
  for standalone containers only).
- **CSP `img-src`** widened to permit selfhst / icon.casaos.io / jsdelivr
  app-icon CDNs so dashboard tiles render.
- **In-UI Update Manager** with SSE progress (downloading → verifying →
  installing → restarting → done / failed).
- **Trash / recycle bin** following the XDG `.Trash-` spec. Auto-purges
  items older than 30 days on a daily janitor pass.
- **Backup scheduler** — Restic-backed cron entries per app, with
  user-facing notifications on success and failure.
- **Job Management service** — scheduled task runner (cron + on-demand)
  with cancellable per-task contexts.
- **Audit log + alerts settings** pages backed by `/v1/sys/audit` and
  `/v1/sys/alerts` endpoints.
- **OAuth2 / OIDC server** in UserService (discovery, PKCE, JWKS).
- **HTTPS / Let's Encrypt** automatic certificate management with a
  self-signed LAN fallback that browsers can pin once.

### Changed

- **Versioning reset**. `5.0.0.devN` → `0.1.0`. Display label switched
  to calendar-versioned `26.06`. The install script reads the version
  from `manifest.json` instead of carrying a hardcoded literal.
- **Upstream phone-home stripped from defaults**. `ServerApi` and
  `Handshake` in `casanova.conf.sample` ship blank — CasaNova does not
  contact `api.casaos.io` unless you explicitly point it at an
  endpoint. The public app catalog (`appstore` URL in
  `app-management.conf.sample`) is still wired to IceWhale's
  CasaOS-AppStore by default because the catalog is community content,
  not a phone-home API.
- **Compose extension key**. Writers emit `x-casanova` as the canonical
  key. The reader (`common.ComposeExtension`) accepts both
  `x-casanova` and `x-casaos`, so compose files imported from upstream
  CasaOS stores keep working without modification.
- **Brand cleanup** — visible UI strings, fixtures, audit / notification
  examples, and Backup suggested paths now lead with CasaNova naming
  while keeping `/etc/casaos/` paths listed for users upgrading from
  CasaOS.
- **Sample config rename**: `casaos.conf.sample` → `casanova.conf.sample`.
  Runtime paths under `/etc/casaos/` and `/etc/casanova/` are unchanged
  for upgrade compatibility (install.sh dual-installs).

### Fixed (highlights from the v0.1 hardening passes)

- **SSH terminal Unauthorized**. Three layered bugs: middleware didn't
  strip the `Bearer ` prefix before JWT validation; WebSocket reused
  `?token=` for both the JWT and the SSH session ID (`?jwt=` now
  carries the access token); the gateway proxy stripped
  `Connection: Upgrade` headers from `101 Switching Protocols`
  responses, breaking the handshake.
- **Services dropping after hours/days**. MessageBus event /
  action / socket.io goroutines had no `recover()` — a single panic
  killed each subsystem permanently. UserService event subscriber
  gave up after 1000 reconnect attempts (~17 min) and had a fixed
  1KB read buffer that silently truncated larger frames. Both
  rewired with `recover()` + unbounded backoff reconnect + JSON
  codec reads.
- **Update manager (502, error 127, kill-itself)**. Update script ran
  inside the casanova service cgroup, so `systemctl stop casanova`
  during phase 4 also killed the script. Wrapped with
  `systemd-run --scope` so the updater escapes the parent unit.
  Update tools bootstrap from the tarball itself if `/usr/share/
  casanova/tools/` is missing.
- **CasaNova main service no longer panics if the gateway is slow at
  boot.** Adopted the retry-with-backoff route registration loop that
  every other service was already using.
- **Atomic writes** for every critical config file (smb.conf, smb_shares.json,
  certs.json, macvlan.json, entry.json, compose YAML, management.url,
  static.url, message-bus.address, TLS cert/key pair).
- **Crash-recovery for app-store + remote-store workdirs** — an
  interrupted catalog sync used to leave the live workdir missing
  and an orphan `.backup` directory. Startup scan restores it.
- **Bounded contexts** on every Docker, message-bus, and HTTP call
  that used to block forever on a hung daemon (~60 sites).
- **File-sharing WebSocket deadlock** caused by unbuffered broadcast
  channel + blocking sends from the cron pinger and every readPump.
  Replaced with buffered channel + non-blocking sends so one slow
  client can't stall the whole signaling fabric.
- **EXDEV-safe rename** for the mergerfs default-files migration — the
  previous code did `RMDir(target) + Rename(src,target)` which lost
  user data when `src` and `target` were on different filesystems.

### Known limitations

- Virtual machines are scaffolded but not implemented (dev-mode toggle
  hides the menu entry on stable builds).
- HTTP polling for container stats (every 1s). A WebSocket-driven stats
  push is on the roadmap for v0.2.
- CLI binary nil-deref on a few `WithResponse.JSON200` accessors —
  affects sysadmin CLI invocation only, not the daemon.

## Upgrade

### From upstream CasaOS

Run the install script. It detects `/etc/casaos/` config and copies it
into `/etc/casanova/`; the legacy paths are kept populated so any binary
that still references them (some service-to-service URLs) still finds
the same files. Disables legacy `casaos-*` systemd units in favour of
the new `casanova-*` set.

### From earlier `5.0.0.devN` test builds

Drop in the tarball as before. The update script does not enforce
monotonic semver, so the version dropping to `0.1.0` is intentional and
will not be refused. UI About page will show `26.06 (0.1.0)`.

[0.1.0]: https://github.com/LeewayOpenSoftware/CasaNova/releases/tag/v0.1.0
