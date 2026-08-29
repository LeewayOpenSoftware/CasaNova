# CasaNova Changelog

All notable user-facing changes land here. Internal refactors that don't change
behaviour are recorded in commit history, not this file. Dates are in UTC.

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
