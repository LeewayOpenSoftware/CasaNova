# CasaNova OS

A home-server operating system that takes the best of [CasaOS][casaos] and the
best of [ZimaOS][zima] and reassembles them around a hardened core: every
long-running goroutine has a `recover()`, every Docker call has a deadline,
every critical config file is fsync'd. Built for someone who wants a
dashboard their container farm boots from, not a fragile demo.

CasaNova ships eight Go microservices that all talk through a single TLS
gateway, plus a Vue 3 frontend that uses no GraphQL, no SSR, and no
build-time templating tricks. Browser-uploadable self-update means you
never need to SSH in to install a new release.

[casaos]: https://github.com/IceWhaleTech/CasaOS
[zima]: https://github.com/IceWhaleTech/ZimaOS

## Status

**v0.1 — 26.06 — first public release.** See [CHANGELOG.md](CHANGELOG.md).

This is a fork of CasaOS. We keep the upstream attribution on the About
page, read the upstream `x-casaos` compose extension key for backward
compatibility, and default the app catalog to IceWhale's community
CasaOS-AppStore (you can swap or add catalogs in
`/etc/casanova/app-management.conf`).

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│  casanova-gateway      HTTPS / TLS reverse proxy + rate limit  │
│  port 80 / 443                                                 │
└─────────────┬──────────────────────────────────────────────────┘
              │  /v1/sys/*           /v2/app_management/*
              │  /v1/file/*          /v1/local-storage/*
              │  /v1/users/*         /v1/job-management/*
              │                                                  
   ┌──────────┴──────────┐  ┌────────────────────┐               
   │  casanova           │  │  casanova-app-     │  pulls,        
   │  (main dashboard +  │  │  management        │  installs,     
   │   file manager +    │  │                    │  recreates     
   │   SMB / certs / …)  │  │                    │  containers    
   └──────────┬──────────┘  └────────────────────┘                
              │                                                   
   ┌──────────┴──────────┐  ┌────────────────────┐                
   │  casanova-          │  │  casanova-         │                
   │  local-storage      │  │  user-service      │  JWT, OAuth2, 
   │                     │  │                    │  TOTP          
   │  disks, mounts,     │  │                    │                
   │  SMART, mergerfs    │  │                    │                
   └─────────────────────┘  └────────────────────┘                
                                                                  
   ┌──────────┴──────────┐  ┌────────────────────┐                
   │  casanova-          │  │  casanova-job-     │  cron, one-off 
   │  message-bus        │  │  management        │  scripts       
   │  (event + action    │  │                    │                
   │   pub/sub + ws)     │  │                    │                
   └─────────────────────┘  └────────────────────┘                
```

All services are in this monorepo:

| Directory                    | Service                                              |
|------------------------------|------------------------------------------------------|
| `CasaNova/`                  | Main dashboard + file manager + SMB / certs / …      |
| `CasaNova-Gateway/`          | HTTPS reverse proxy with rate limiting and shield    |
| `CasaNova-AppManagement/`    | Docker container + compose stack lifecycle           |
| `CasaNova-UserService/`      | Auth: JWT issue/verify, OAuth2 server, TOTP, RBAC    |
| `CasaNova-LocalStorage/`     | Disks, mounts, mergerfs, SMART, rclone               |
| `CasaNova-MessageBus/`       | Event + action pub/sub over WebSocket + socket.io    |
| `CasaNova-JobManagement/`    | Scheduled tasks (cron) + ad-hoc runs                 |
| `CasaNova-Common/`           | Shared types, helpers, generated SDK clients         |
| `CasaNova-UI-v2/`            | Vue 3 + Pinia + PrimeVue frontend                    |
| `CasaNova-CLI/`              | `casanova-cli` command-line entry point              |
| `build/full/`                | Tarball staging: binaries, frontend, systemd units   |

## Installing

### Fresh install (or upgrade from CasaOS)

Download the latest tarball from Releases, then on the server:

```bash
mkdir -p /tmp/casanova-install
tar xzf casanova-0.1.0.dev1.*-x86_64.tar.gz -C /tmp/casanova-install
sudo bash /tmp/casanova-install/install.sh
```

The script stops any running `casaos-*` units, drops binaries into
`/usr/bin/`, the frontend into `/var/lib/casanova/www/`, systemd units
into `/usr/lib/systemd/system/`, and config samples into both
`/etc/casanova/` and `/etc/casaos/` (the latter is kept populated so
service-to-service URLs keep resolving while the in-source rename
finishes). It starts the seven services in dependency order, retries
any that lose the gateway-route race, and runs a health check against
`http://localhost/ping` before reporting success.

Open `http://<server-ip>/` in a browser. Default admin user is set on
the welcome screen.

### Upgrade from an earlier `5.0.0.devN` test build

Same path, including over the existing install — no special steps. The
update script does not enforce monotonic semver, so the version
dropping to `0.1.0` is intentional and will not be refused. You can
also drop the tarball through the **Settings → Update** UI; it will
upload, validate, and apply without an SSH session.

## Building from source

Requires Go ≥ 1.22, Node ≥ 20, and a Linux host (the `pkg/udev`
package and a couple of systemctl calls aren't portable).

```bash
# Backend — eight services, all rooted at the repo level
for d in CasaNova CasaNova-Gateway CasaNova-AppManagement \
         CasaNova-UserService CasaNova-LocalStorage \
         CasaNova-MessageBus CasaNova-JobManagement CasaNova-CLI; do
  (cd "$d" && go build -o "../build/full/bin/$(basename $d | tr A-Z a-z)" .)
done

# Frontend — outputs into build/full/www/ automatically
(cd CasaNova-UI-v2 && npm ci && npm run build)

# Package
(cd build && tar czf "casanova-$(date +%y%m%d%H%M)-x86_64.tar.gz" -C full .)
```

The Makefile at repo root wraps the same steps with consistent
version-injection via `-ldflags`.

## Configuration

Configs live in `/etc/casanova/` (and are mirrored to `/etc/casaos/`
for upgrade compatibility):

- `casanova.conf` — main daemon: paths, ports, optional remote update
  server endpoint.
- `gateway.ini` — reverse proxy port, optional TLS material location.
- `app-management.conf` — public app-store catalog URLs (you can
  comment them out and provide your own).
- `user-service.conf` — JWT keypair location, OAuth2 client allowlist.
- `local-storage.conf` — mergerfs settings, USB auto-mount toggle.
- `message-bus.conf`, `job-management.conf` — runtime + log paths.

CasaNova does **not** phone home in the default configuration.
`ServerApi` and `Handshake` ship blank; set them only if you operate
your own update server.

## Self-update

Three independent paths cover the most common install scenarios:

1. **Browser upload** — `Settings → Update → Upload .tar.gz`. The
   backend validates the manifest, atomic-renames into the canonical
   `casanova-VERSION-ARCH.tar.gz` name, and applies it through the
   same engine the other two paths use.
2. **Search server storage** — scans `/var/lib/casanova/updates/` for
   any package with a valid manifest and presents them in the UI.
3. **USB / mounted disk** — scans `/media/`, `/mnt/`, and
   `/run/media/` for the same.

The update engine (`casanova-update.sh`) is bundled with every
release. If it's missing on the running system (e.g. because the
prior install didn't deploy `/usr/share/casanova/tools/`), the backend
extracts a fresh copy from the tarball being applied.

## Contributing

Open an issue first for anything bigger than a typo. We follow the
[Conventional Commits][cc] convention loosely (subject lines under
72 chars, `feat:` / `fix:` / `chore:` prefixes). Internal milestone
markers (`Wave 27`, `Wave 35-X`, etc.) are an audit trail — keep adding
them with new fixes if you find them useful; they're meaningful enough
that they ship in the code comments.

[cc]: https://www.conventionalcommits.org

## Acknowledgements

CasaNova would not exist without [IceWhale Technology][icewhale] and
the CasaOS community. The dashboard layout, the app catalog metadata
format, the install philosophy, and the entire app-store concept came
from upstream. We forked specifically to harden the runtime,
restructure self-update, and rebuild the frontend on Vue 3 + PrimeVue
— not to replace the project.

[icewhale]: https://github.com/IceWhaleTech

## License

Apache 2.0 — matching upstream CasaOS. See [LICENSE](LICENSE) if
present (forked attribution is preserved on the About page either
way).
