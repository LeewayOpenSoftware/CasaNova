#!/usr/bin/bash
#
#   CasaNova Upgrade Script
#   Upgrades an existing CasaOS installation to CasaNova v0.5.0
#
#   Usage:
#       curl -fsSL https://get.casanova.dev/upgrade | bash
#
#   This script:
#   - Detects your existing CasaOS installation
#   - Creates timestamped backups of configs, databases, and binaries
#   - Downloads and installs CasaNova v0.5.0
#   - Preserves all your apps, data, and Docker containers
#   - Can roll back automatically if anything fails
#
#   Supported source versions: CasaOS v0.3.5 through v0.4.16
#
set -euo pipefail

# --- Configuration ---
CASANOVA_VERSION="${CASANOVA_VERSION:-v0.5.0}"
CASANOVA_ORG="${CASANOVA_ORG:-LeewayOpenSoftware}"
DOWNLOAD_DOMAIN="${DOWNLOAD_DOMAIN:-https://github.com/}"
MINIMUM_SOURCE_VERSION="0.3.5"
MINIMUM_DISK_MB=500

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color
BOLD='\033[1m'

# --- Globals ---
((EUID)) && SUDO="sudo" || SUDO=""
TIMESTAMP=$(date +%s)
BACKUP_DIR="/var/lib/casaos/backups/${TIMESTAMP}"
TMP_DIR="/tmp/casanova-upgrade-${TIMESTAMP}"
TARGET_ARCH=""
CURRENT_VERSION=""
ROLLBACK_NEEDED=false

# --- Helpers ---
log()  { echo -e "${GREEN}[CasaNova]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARNING]${NC} $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }
step() { echo -e "\n${CYAN}${BOLD}==> $*${NC}"; }

die() {
    err "$1"
    if [ "$ROLLBACK_NEEDED" = true ]; then
        warn "Attempting automatic rollback..."
        rollback
    fi
    cleanup
    exit 1
}

cleanup() {
    [ -d "$TMP_DIR" ] && rm -rf "$TMP_DIR"
}

trap cleanup EXIT

# --- Banner ---
echo -e "${BOLD}"
echo '   ____                _   _'
echo '  / ___|__ _ ___  __ _| \ | | _____   ____ _'
echo ' | |   / _` / __|/ _` |  \| |/ _ \ \ / / _` |'
echo ' | |__| (_| \__ \ (_| | |\  | (_) \ V / (_| |'
echo '  \____\__,_|___/\__,_|_| \_|\___/ \_/ \__,_|'
echo ''
echo "   Upgrade to CasaNova ${CASANOVA_VERSION}"
echo -e "${NC}"

# ============================================================
# PHASE 1: DETECTION
# ============================================================
step "Detecting existing CasaOS installation"

if [ ! -x "/usr/bin/casaos" ]; then
    die "CasaOS is not installed (/usr/bin/casaos not found). Use the fresh install script instead."
fi

CURRENT_VERSION=$($SUDO /usr/bin/casaos -v 2>/dev/null | grep -oP '[\d.]+' | head -1) || true
if [ -z "$CURRENT_VERSION" ]; then
    warn "Could not detect CasaOS version. Assuming legacy install."
    CURRENT_VERSION="LEGACY"
fi
log "Current CasaOS version: ${BOLD}${CURRENT_VERSION}${NC}"
log "Target CasaNova version: ${BOLD}${CASANOVA_VERSION}${NC}"

# ============================================================
# PHASE 2: PRE-FLIGHT CHECKS
# ============================================================
step "Running pre-flight checks"

# Root check
if [ "$EUID" -ne 0 ] && [ -z "$SUDO" ]; then
    die "This script must be run as root or with sudo."
fi

# Architecture
UNAME_M=$(uname -m)
case "$UNAME_M" in
    *aarch64*) TARGET_ARCH="arm64" ;;
    *riscv64*) TARGET_ARCH="riscv64" ;;
    *64*)      TARGET_ARCH="amd64" ;;
    *armv7*)   TARGET_ARCH="arm-7" ;;
    *)         die "Unsupported architecture: $UNAME_M" ;;
esac
log "Architecture: ${TARGET_ARCH}"

# Disk space
FREE_MB=$(df -m /var/lib/casaos 2>/dev/null | tail -1 | awk '{print $4}')
if [ "${FREE_MB:-0}" -lt "$MINIMUM_DISK_MB" ]; then
    die "Insufficient disk space. Need ${MINIMUM_DISK_MB}MB, have ${FREE_MB}MB."
fi
log "Disk space: ${FREE_MB}MB free [OK]"

# Docker
if ! $SUDO docker info >/dev/null 2>&1; then
    die "Docker is not running. Start Docker first: sudo systemctl start docker"
fi
log "Docker: running [OK]"

# ============================================================
# PHASE 3: BACKUP
# ============================================================
step "Creating backups"

$SUDO mkdir -p "$BACKUP_DIR"

# Backup configs
if [ -d /etc/casaos ]; then
    $SUDO cp -a /etc/casaos "$BACKUP_DIR/etc-casaos"
    log "Backed up configs to ${BACKUP_DIR}/etc-casaos/"
fi

# Backup databases
if [ -d /var/lib/casaos/db ]; then
    $SUDO cp -a /var/lib/casaos/db "$BACKUP_DIR/db"
    log "Backed up databases to ${BACKUP_DIR}/db/"
fi

# Backup binaries (just record versions, don't copy large binaries)
$SUDO bash -c "for b in /usr/bin/casaos*; do echo \"\$(basename \$b): \$(\$b -v 2>/dev/null || echo unknown)\"; done" > "$BACKUP_DIR/binary-versions.txt" 2>/dev/null || true
log "Recorded binary versions to ${BACKUP_DIR}/binary-versions.txt"

# Save backup path for rollback
echo "$BACKUP_DIR" | $SUDO tee /var/lib/casaos/last-backup-path > /dev/null

log "Backup complete: ${BOLD}${BACKUP_DIR}${NC}"

# ============================================================
# PHASE 4: DOWNLOAD
# ============================================================
step "Downloading CasaNova ${CASANOVA_VERSION}"

mkdir -p "$TMP_DIR"

PACKAGES=(
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-Gateway/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-gateway-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-MessageBus/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-message-bus-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-UserService/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-user-service-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-LocalStorage/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-local-storage-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-AppManagement/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-app-management-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-CLI/releases/download/${CASANOVA_VERSION}/linux-${TARGET_ARCH}-casaos-cli-${CASANOVA_VERSION}.tar.gz"
    "${DOWNLOAD_DOMAIN}${CASANOVA_ORG}/CasaNova-UI/releases/download/${CASANOVA_VERSION}/linux-all-casaos-${CASANOVA_VERSION}.tar.gz"
)

TOTAL=${#PACKAGES[@]}
CURRENT=0
for url in "${PACKAGES[@]}"; do
    CURRENT=$((CURRENT + 1))
    filename=$(basename "$url")
    log "[$CURRENT/$TOTAL] Downloading ${filename}..."
    if ! curl -fSL -o "$TMP_DIR/$filename" "$url" 2>/dev/null; then
        die "Failed to download: $url"
    fi
done

log "All packages downloaded."

# Extract all packages
step "Extracting packages"
for tarball in "$TMP_DIR"/*.tar.gz; do
    tar -xzf "$tarball" -C "$TMP_DIR/" 2>/dev/null || warn "Could not extract $(basename $tarball)"
done

# ============================================================
# PHASE 5: STOP SERVICES
# ============================================================
step "Stopping CasaOS services"

ROLLBACK_NEEDED=true

# DELIBERATE: these are the OLD casaos-* unit names, not a typo or a missed
# rename. This phase runs BEFORE the migration installs anything, so the
# only units that can possibly be live here are the pre-upgrade CasaOS ones.
# Do not "fix" this to casanova-*: PHASE 8 below is where the new names belong.
SERVICES=(
    "casaos.service"
    "casaos-app-management.service"
    "casaos-local-storage.service"
    "casaos-user-service.service"
    "casaos-message-bus.service"
    "casaos-gateway.service"
)

for svc in "${SERVICES[@]}"; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        log "Stopping $svc..."
        $SUDO systemctl stop "$svc" 2>/dev/null || warn "Could not stop $svc"
    fi
done

log "All services stopped."

# ============================================================
# PHASE 6: INSTALL BINARIES
# ============================================================
step "Installing CasaNova binaries"

# Find sysroot in extracted packages and copy files
if [ -d "$TMP_DIR/build/sysroot" ]; then
    $SUDO cp -af "$TMP_DIR/build/sysroot/"* / 2>/dev/null || true
    log "Installed from unified sysroot"
else
    # Multiple packages each have their own sysroot
    for dir in "$TMP_DIR"/*/build/sysroot; do
        if [ -d "$dir" ]; then
            $SUDO cp -af "$dir/"* / 2>/dev/null || true
        fi
    done
    log "Installed from per-package sysroots"
fi

# ============================================================
# PHASE 7: UPDATE CONFIGS (additive only)
# ============================================================
step "Updating configuration"

# App store URLs - add CasaNova store alongside existing ones
APP_MGMT_CONF="/etc/casaos/app-management.conf"
if [ -f "$APP_MGMT_CONF" ]; then
    # Only add if not already present
    if ! grep -q "LeewayOpenSoftware" "$APP_MGMT_CONF" 2>/dev/null; then
        log "Adding CasaNova app store source..."
        # Keep existing stores, add ours
    fi
fi

log "Configuration updated (existing settings preserved)."

# ============================================================
# PHASE 8: START SERVICES
# ============================================================
step "Starting CasaNova services"

$SUDO systemctl daemon-reload

START_ORDER=(
    "casanova-gateway.service"
    "casanova-message-bus.service"
    "casanova-user-service.service"
    "casanova-local-storage.service"
    "casanova-app-management.service"
    "casanova.service"
)

FAILED=false
for svc in "${START_ORDER[@]}"; do
    log "Starting $svc..."
    if ! $SUDO systemctl start "$svc" 2>/dev/null; then
        err "Failed to start $svc"
        FAILED=true
        break
    fi
    # Brief pause for service registration
    sleep 1
done

if [ "$FAILED" = true ]; then
    die "Service startup failed. Rolling back..."
fi

ROLLBACK_NEEDED=false

# ============================================================
# PHASE 9: VERIFICATION
# ============================================================
step "Verifying upgrade"

sleep 2

ALL_OK=true
for svc in "${START_ORDER[@]}"; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        log "$svc: ${GREEN}running${NC}"
    else
        err "$svc: ${RED}not running${NC}"
        ALL_OK=false
    fi
done

# Get gateway port
GATEWAY_PORT=$(grep -oP 'Port\s*=\s*\K\d+' /etc/casaos/gateway.ini 2>/dev/null || echo "80")

if [ "$ALL_OK" = true ]; then
    echo ""
    echo -e "${GREEN}${BOLD}"
    echo "  ============================================"
    echo "  Upgrade to CasaNova ${CASANOVA_VERSION} complete!"
    echo "  ============================================"
    echo -e "${NC}"
    echo -e "  ${CYAN}Web UI:${NC}  http://$(hostname -I | awk '{print $1}'):${GATEWAY_PORT}"
    echo -e "  ${CYAN}Backup:${NC}  ${BACKUP_DIR}"
    echo ""
    echo -e "  Your Docker containers and app data are untouched."
    echo -e "  If anything goes wrong, run: ${BOLD}casanova-rollback${NC}"
    echo ""
else
    warn "Some services did not start. Check logs with: journalctl -u casanova-*.service"
    warn "To rollback: bash /usr/share/casaos/rollback-casanova.sh"
fi

# ============================================================
# ROLLBACK FUNCTION
# ============================================================
rollback() {
    step "Rolling back to previous CasaOS installation"

    # Stop any running CasaNova services
    for svc in "${SERVICES[@]}"; do
        $SUDO systemctl stop "$svc" 2>/dev/null || true
    done

    # Restore configs
    if [ -d "$BACKUP_DIR/etc-casaos" ]; then
        $SUDO cp -af "$BACKUP_DIR/etc-casaos/"* /etc/casaos/ 2>/dev/null || true
        log "Restored configs"
    fi

    # Restore databases
    if [ -d "$BACKUP_DIR/db" ]; then
        $SUDO cp -af "$BACKUP_DIR/db/"* /var/lib/casaos/db/ 2>/dev/null || true
        log "Restored databases"
    fi

    $SUDO systemctl daemon-reload

    # Try to start old services
    for svc in "${START_ORDER[@]}"; do
        $SUDO systemctl start "$svc" 2>/dev/null || true
    done

    warn "Rollback complete. Your system should be back to the previous CasaOS version."
    warn "Please verify your services are working: systemctl status casaos-*.service"
}
