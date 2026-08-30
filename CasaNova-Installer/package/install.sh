#!/bin/bash
set -euo pipefail

# CasaNova Installation Script
# Deploys binaries, frontend, systemd units, and configs to the live system.
# Version is sourced from manifest.json — never hardcoded.

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# Read version + display label from manifest.json so install messaging
# never disagrees with the actual binaries shipping. Falls back to a
# safe "unknown" string if the file is missing rather than embedding a
# stale literal.
read_manifest() {
    local key="$1"
    if command -v python3 >/dev/null 2>&1 && [ -f "$SCRIPT_DIR/manifest.json" ]; then
        python3 -c "import json; print(json.load(open('$SCRIPT_DIR/manifest.json')).get('$key',''))" 2>/dev/null
    fi
}
PKG_VERSION="$(read_manifest version)"
PKG_DISPLAY="$(read_manifest display_version)"
[ -z "$PKG_VERSION" ] && PKG_VERSION="unknown"
[ -z "$PKG_DISPLAY" ] && PKG_DISPLAY="$PKG_VERSION"
RELEASE_LABEL="${PKG_DISPLAY} (${PKG_VERSION})"

# --- Step 1: Root check ---
if [ "$(id -u)" -ne 0 ]; then
    error "This script must be run as root. Use: sudo $0"
fi

info "Installing CasaNova ${RELEASE_LABEL}..."

# --- Step 1b: Ensure a container runtime (Docker) ---
# CasaNova's app management drives the Docker daemon (units Require it), so a
# fresh box needs Docker before services start. Install from the OFFICIAL apt
# repo pinned to THIS box's live codename — a stale/hardcoded codename silently
# freezes Docker after a release-upgrade (do-release-upgrade renames 3rd-party
# repos to *.distUpgrade). get.docker.com is the fallback for non-apt distros.
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    info "Docker present: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"
else
    warn "Docker not detected — installing Docker CE..."
    if command -v apt-get >/dev/null 2>&1; then
        os_id=$(. /etc/os-release 2>/dev/null && echo "${ID:-ubuntu}")
        codename=$(. /etc/os-release 2>/dev/null && echo "${VERSION_CODENAME:-}")
        [ -z "$codename" ] && codename=$(lsb_release -cs 2>/dev/null || echo stable)
        arch=$(dpkg --print-architecture 2>/dev/null || echo amd64)
        case "$os_id" in debian|raspbian) os_id=debian ;; *) os_id=ubuntu ;; esac
        install -m 0755 -d /etc/apt/keyrings
        curl -fsSL "https://download.docker.com/linux/${os_id}/gpg" \
            | gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg || true
        chmod a+r /etc/apt/keyrings/docker.gpg 2>/dev/null || true
        echo "deb [arch=${arch} signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/${os_id} ${codename} stable" \
            > /etc/apt/sources.list.d/docker.list
        rm -f /etc/apt/sources.list.d/docker.list.distUpgrade 2>/dev/null || true
        apt-get update -y || true
        apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || true
    else
        curl -fsSL https://get.docker.com | sh || true
    fi
    systemctl enable --now docker 2>/dev/null || true
    if docker info >/dev/null 2>&1; then
        info "Docker installed: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"
    else
        warn "Docker install did not complete — install it manually; app management needs it."
    fi
fi

# --- Step 2: Stop running services ---
info "Stopping existing services..."
SERVICES_ORDERED=(
    casanova
    casanova-job-management
    casanova-app-management
    casanova-local-storage
    casanova-user-service
    casanova-gateway
    casanova-message-bus
)
for svc in "${SERVICES_ORDERED[@]}"; do
    systemctl stop "$svc" 2>/dev/null || true
done
# Also stop legacy casaos services
for svc in $(systemctl list-units --type=service --no-pager --no-legend 'casaos-*' 2>/dev/null | awk '{print $1}'); do
    systemctl stop "$svc" 2>/dev/null || true
done

# --- Step 3: Copy binaries ---
info "Installing binaries to /usr/bin/..."
install -m 0755 "$SCRIPT_DIR/bin/casanova"                /usr/bin/casanova
install -m 0755 "$SCRIPT_DIR/bin/casanova-gateway"        /usr/bin/casanova-gateway
install -m 0755 "$SCRIPT_DIR/bin/casanova-user-service"   /usr/bin/casanova-user-service
install -m 0755 "$SCRIPT_DIR/bin/casanova-local-storage"  /usr/bin/casanova-local-storage
install -m 0755 "$SCRIPT_DIR/bin/casanova-app-management" /usr/bin/casanova-app-management
install -m 0755 "$SCRIPT_DIR/bin/casanova-message-bus"    /usr/bin/casanova-message-bus
install -m 0755 "$SCRIPT_DIR/bin/casanova-job-management" /usr/bin/casanova-job-management
install -m 0755 "$SCRIPT_DIR/bin/casanova-driver"         /usr/bin/casanova-driver
install -m 0755 "$SCRIPT_DIR/bin/casanova-virt"           /usr/bin/casanova-virt
install -m 0755 "$SCRIPT_DIR/bin/casanova-cli"            /usr/bin/casanova-cli

# --- Step 4: Copy systemd units ---
info "Installing systemd units..."
# Sweep stale units first — when a service is renamed across versions
# the old .service file would otherwise persist alongside the new one
# and crash-loop trying to exec a binary that no longer exists.
rm -f /usr/lib/systemd/system/casanova*.service
cp "$SCRIPT_DIR"/systemd/casanova*.service /usr/lib/systemd/system/

# Wave 35-P: ship the tmpfiles.d snippet so /run/casanova and /run/casaos
# survive reboots even on hosts whose systemd is too old to honour
# RuntimeDirectory= reliably, and so direct-from-CLI invocations also see
# the runtime dirs created.
if [ -f "$SCRIPT_DIR/tmpfiles.d/casanova.conf" ]; then
    info "Installing tmpfiles.d/casanova.conf..."
    install -m 0644 "$SCRIPT_DIR/tmpfiles.d/casanova.conf" /usr/lib/tmpfiles.d/casanova.conf
    systemd-tmpfiles --create /usr/lib/tmpfiles.d/casanova.conf 2>/dev/null || true
fi

# --- Step 5: Copy frontend ---
info "Installing frontend to /var/lib/casanova/www/..."
mkdir -p /var/lib/casanova/www
rm -rf /var/lib/casanova/www/*
cp -r "$SCRIPT_DIR"/www/* /var/lib/casanova/www/

# --- Step 5b: Install tools (self-update engine, packaging helpers) ---
if [ -d "$SCRIPT_DIR/tools" ]; then
    info "Installing tools to /usr/share/casanova/tools/..."
    mkdir -p /usr/share/casanova/tools
    install -m 0755 "$SCRIPT_DIR"/tools/*.sh /usr/share/casanova/tools/
fi

# --- Step 6: Create required directories ---
# CasaNova paths
mkdir -p /etc/casanova
mkdir -p /var/run/casanova
mkdir -p /var/lib/casanova/db
mkdir -p /var/lib/casanova/www
mkdir -p /var/log/casanova

# /DATA — the user-data root. Six unit files name it in ReadWritePaths, and
# systemd refuses to start a unit whose ReadWritePaths does not exist: the
# service dies with 226/NAMESPACE ("Failed to set up mount namespacing")
# before its binary runs at all. On an upgraded box /DATA already exists from
# an earlier CasaOS install, which is why this was never noticed; on a clean
# install casanova, app-management, local-storage and job-management all
# failed to start, with journalctl showing only "Failed with result
# exit-code" unless you read past the restart loop.
mkdir -p /DATA
chmod 0755 /DATA

# The updater's record of what is installed. Without it installedVersion()
# returns "" and every candidate package looks newer than the running system.
if [ -f "$SCRIPT_DIR/manifest.json" ]; then
  cp "$SCRIPT_DIR/manifest.json" /var/lib/casanova/manifest.json
fi

# Legacy casaos paths — the Go source still defaults to these paths;
# the config samples reference them and the binaries use them at runtime.
# Until a full source rename, these must exist.
mkdir -p /var/run/casaos
mkdir -p /var/lib/casaos/db
mkdir -p /var/lib/casaos/apps
mkdir -p /var/lib/casaos/appstore
mkdir -p /var/lib/casaos/conf
mkdir -p /var/log/casaos
mkdir -p /usr/share/casaos/shell

# --- Step 6b: Seed the bundled app catalog ---
#
# v0.1 root-cause fix: fresh installs used to land with an empty
# /var/lib/casaos/appstore/default/, so the AppStore panel rendered
# the panel chrome but had zero apps to show, and every install attempt
# 500'd with "store app not found". The tarball ships the catalog at
# library/default/ — we copy it in only when the live store is empty
# so a user who edited their catalog (added apps, removed Cloudflared,
# etc.) doesn't get clobbered on every install.
if [ -d "$SCRIPT_DIR/library/default" ]; then
    if [ ! -d /var/lib/casaos/appstore/default ] \
       || [ -z "$(ls -A /var/lib/casaos/appstore/default 2>/dev/null)" ]; then
        info "Seeding bundled app catalog → /var/lib/casaos/appstore/default/..."
        mkdir -p /var/lib/casaos/appstore/default
        cp -a "$SCRIPT_DIR"/library/default/. /var/lib/casaos/appstore/default/
    else
        info "App catalog already populated, skipping seed."
    fi
fi

# Seed the registry index (stores.json) if AppManagement hasn't run yet.
# It will rebuild from defaults on first boot, but pre-seeding means the
# very first AppStore-panel open shows the catalog without waiting for
# the daemon's first write to land.
if [ -f "$SCRIPT_DIR/library/stores.json" ] \
   && [ ! -f /var/lib/casaos/appstore/stores.json ]; then
    info "Seeding store registry index..."
    cp "$SCRIPT_DIR/library/stores.json" /var/lib/casaos/appstore/stores.json
fi

# --- Step 7: Copy sample configs if not already present ---
if [ ! -f /etc/casanova/casanova.conf ]; then
    info "Installing default configuration files..."

    # Check for legacy migration
    if [ -d /etc/casaos ] && [ "$(ls -A /etc/casaos 2>/dev/null)" ]; then
        warn "Found legacy /etc/casaos/ config directory. Migrating to /etc/casanova/..."
        for f in /etc/casaos/*; do
            [ -f "$f" ] && [ ! -f "/etc/casanova/$(basename "$f")" ] && cp "$f" /etc/casanova/ 2>/dev/null
        done || true
    fi

    # Copy sample configs with renamed filenames (only if target doesn't exist)
    copy_if_absent() { [ ! -f "$2" ] && [ -f "$1" ] && cp "$1" "$2"; return 0; }
    # v0.1: sample filename is `casanova.conf.sample` (earlier
    # 5.0.0.devN tarballs shipped it as `casaos.conf.sample`). The
    # rename is install-side only — runtime paths under /etc/casanova/
    # and /etc/casaos/ are unchanged for upgrade compatibility.
    copy_if_absent "$SCRIPT_DIR/config/casanova.conf.sample"         /etc/casanova/casanova.conf
    copy_if_absent "$SCRIPT_DIR/config/gateway.ini.sample"           /etc/casanova/gateway.ini
    copy_if_absent "$SCRIPT_DIR/config/user-service.conf.sample"     /etc/casanova/user-service.conf
    copy_if_absent "$SCRIPT_DIR/config/local-storage.conf.sample"    /etc/casanova/local-storage.conf
    copy_if_absent "$SCRIPT_DIR/config/app-management.conf.sample"   /etc/casanova/app-management.conf
    copy_if_absent "$SCRIPT_DIR/config/message-bus.conf.sample"      /etc/casanova/message-bus.conf
    copy_if_absent "$SCRIPT_DIR/config/job-management.conf.sample"   /etc/casanova/job-management.conf
    copy_if_absent "$SCRIPT_DIR/config/driver.conf.sample"           /etc/casanova/driver.conf
    copy_if_absent "$SCRIPT_DIR/config/virt-management.conf.sample"  /etc/casanova/virt-management.conf
fi

# --- Step 7b: Populate legacy config path for services that still read /etc/casaos/ ---
# The gateway and some other services use viper/hardcoded paths to /etc/casaos/.
# Until the Go source is fully renamed, we need configs in both places.
mkdir -p /etc/casaos
for conf in /etc/casanova/*; do
    [ -f "$conf" ] || continue
    bname=$(basename "$conf")
    # Map casanova names back to casaos names where needed
    case "$bname" in
        casanova.conf) [ ! -f /etc/casaos/casaos.conf ] && cp "$conf" /etc/casaos/casaos.conf ;;
        gateway.ini)   [ ! -f /etc/casaos/gateway.ini ] && cp "$conf" /etc/casaos/gateway.ini ;;
        *)             [ ! -f "/etc/casaos/$bname" ]    && cp "$conf" "/etc/casaos/$bname" ;;
    esac
done

# Wave 35-P: gateway.ini and other configs may carry secrets (API tokens,
# bcrypt salts, TLS material). install.sh used to leave them with the
# default 0644 from cp — world-readable. Tighten to 0640 root:root for
# every config in both copies. The casanova services run as root for
# now, so root-only ownership is enough; once a dedicated `casanova`
# group exists this should chgrp casanova and stay 0640.
chmod 0640 /etc/casanova/* 2>/dev/null || true
chmod 0640 /etc/casaos/*   2>/dev/null || true
chmod 0750 /etc/casanova /etc/casaos 2>/dev/null || true

# --- Step 8: Disable legacy casaos services ---
for svc in $(systemctl list-unit-files --type=service --no-pager --no-legend 'casaos-*' 2>/dev/null | awk '{print $1}'); do
    info "Disabling legacy service: $svc"
    systemctl disable "$svc" 2>/dev/null || true
done

# --- Step 9: Reload systemd and enable services ---
info "Reloading systemd daemon..."
systemctl daemon-reload

SERVICES_START_ORDER=(
    casanova-gateway
    casanova-message-bus
    casanova-user-service
    # Remounts the data disks and assembles the mergerfs pool, and docker is
    # ordered behind it. It was shipped with an [Install] section but never
    # added here, so it stayed disabled on every box and its ordering
    # guarantee never applied: containers could bind-mount /DATA while it was
    # still a bare rootfs directory. Must come before the storage daemon.
    casanova-local-storage-first
    casanova-local-storage
    casanova-app-management
    casanova-job-management
    casanova-driver
    casanova-virt
    casanova
)

info "Enabling and starting services in dependency order..."
for svc in "${SERVICES_START_ORDER[@]}"; do
    systemctl enable "$svc" 2>/dev/null || true
    if ! systemctl start "$svc" 2>/dev/null; then
        warn "Failed to start $svc — will retry after other services"
    fi
    sleep 2
done

# Retry any failed services once (dependency timing)
for svc in "${SERVICES_START_ORDER[@]}"; do
    if [ "$(systemctl is-active "$svc" 2>/dev/null)" != "active" ]; then
        info "Retrying $svc..."
        systemctl restart "$svc" 2>/dev/null || warn "$svc still failed"
        sleep 2
    fi
done

# --- Step 10: Health check ---
info "Running health checks..."
sleep 3

HEALTH_OK=true
if curl -sf http://localhost/ping >/dev/null 2>&1; then
    info "Gateway health check: PASS"
else
    warn "Gateway health check: FAIL (may still be starting)"
    HEALTH_OK=false
fi

# --- Step 11: Status summary ---
echo ""
echo "============================================"
echo "  CasaNova ${RELEASE_LABEL} Installation Complete"
echo "============================================"
echo ""

for svc in "${SERVICES_START_ORDER[@]}"; do
    STATUS=$(systemctl is-active "$svc" 2>/dev/null || echo "unknown")
    if [ "$STATUS" = "active" ]; then
        echo -e "  ${GREEN}●${NC} $svc: $STATUS"
    else
        echo -e "  ${RED}●${NC} $svc: $STATUS"
    fi
done

echo ""
echo "Binary versions:"
for bin in casanova casanova-gateway casanova-user-service casanova-local-storage \
           casanova-app-management casanova-message-bus casanova-job-management casanova-cli; do
    if [ -x "/usr/bin/$bin" ]; then
        VER=$("/usr/bin/$bin" -v 2>/dev/null || "/usr/bin/$bin" --version 2>/dev/null || echo "$PKG_VERSION")
        echo "  $bin: $VER"
    fi
done

echo ""
echo "Config:   /etc/casanova/"
echo "Frontend: /var/lib/casanova/www/"
echo "Logs:     journalctl -u casanova-* -f"
echo ""

if [ "$HEALTH_OK" = true ]; then
    info "Installation successful! Access CasaNova at http://$(hostname -I | awk '{print $1}'):80"
else
    warn "Installation complete but health check failed. Check service logs with:"
    echo "  journalctl -u casanova-gateway -n 50 --no-pager"
fi
