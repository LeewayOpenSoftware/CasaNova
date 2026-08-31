# CasaNova Changelog

All notable user-facing changes land here. Internal refactors that don't change
behaviour are recorded in commit history, not this file. Dates are in UTC.

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
