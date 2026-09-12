#!/usr/bin/bash
#
#   CasaNova Offline Upgrade Package Builder
#
#   Builds a self-contained .run file that can be copied to a CasaOS server
#   via USB, SCP, or the built-in file manager, then executed from the terminal.
#
#   Usage:
#       ./build-upgrade-package.sh [arch]
#
#   Output: casanova-upgrade-v0.5.0-{arch}.run
#
#   The .run file is a self-extracting bash script containing:
#   - All 8 service binaries (pre-compiled)
#   - Migration tools
#   - Systemd units
#   - Config samples
#   - UI assets
#   - The upgrade logic
#
#   Users just: scp casanova-upgrade-v0.5.0-amd64.run user@server:/tmp/
#              then in CasaOS terminal: sudo bash /tmp/casanova-upgrade-v0.5.0-amd64.run
#
set -euo pipefail

VERSION="v0.5.0"
ARCH="${1:-amd64}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKSPACE="$(dirname "$SCRIPT_DIR")"
BUILD_DIR="/tmp/casanova-package-build-$$"
OUTPUT_FILE="${SCRIPT_DIR}/casanova-upgrade-${VERSION}-${ARCH}.run"

echo "Building CasaNova offline upgrade package"
echo "  Version: ${VERSION}"
echo "  Arch:    ${ARCH}"
echo "  Output:  ${OUTPUT_FILE}"
echo ""

# Clean
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR/payload"

# Build all services
echo "==> Building services for linux/${ARCH}..."
cd "$WORKSPACE"

REPOS=(
    "CasaNova-Gateway"
    "CasaNova-MessageBus"
    "CasaNova-UserService"
    "CasaNova-LocalStorage"
    "CasaNova-JobManagement"
    "CasaNova-AppManagement"
    "CasaNova"
    "CasaNova-CLI"
)

for repo in "${REPOS[@]}"; do
    echo "  Building ${repo}..."
    (cd "$repo" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$BUILD_DIR/payload/" ./...)

    # Also copy sysroot files (configs, systemd units, scripts)
    if [ -d "$repo/build/sysroot" ]; then
        cp -a "$repo/build/sysroot/"* "$BUILD_DIR/payload/" 2>/dev/null || true
    fi
    if [ -d "$repo/build/scripts" ]; then
        mkdir -p "$BUILD_DIR/payload/usr/share/casaos/scripts/"
        cp -a "$repo/build/scripts/"* "$BUILD_DIR/payload/usr/share/casaos/scripts/" 2>/dev/null || true
    fi
done

echo "==> Creating self-extracting installer..."

# Create the self-extracting .run file
# Structure: bash header script + compressed tar payload
cat > "$BUILD_DIR/header.sh" << 'UPGRADE_SCRIPT'
#!/usr/bin/bash
#
#   CasaNova Offline Upgrade Package
#   Self-extracting archive — just run it.
#
#   Usage:
#       sudo bash casanova-upgrade-v0.5.0-*.run
#
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'
BOLD='\033[1m'

log()  { echo -e "${GREEN}[CasaNova]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARNING]${NC} $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }
step() { echo -e "\n${CYAN}${BOLD}==> $*${NC}"; }

echo -e "${BOLD}"
echo '   ____                _   _'
echo '  / ___|__ _ ___  __ _| \ | | _____   ____ _'
echo ' | |   / _` / __|/ _` |  \| |/ _ \ \ / / _` |'
echo ' | |__| (_| \__ \ (_| | |\  | (_) \ V / (_| |'
echo '  \____\__,_|___/\__,_|_| \_|\___/ \_/ \__,_|'
echo ''
echo "   Offline Upgrade Package"
echo -e "${NC}"

# Must be root
if [ "$EUID" -ne 0 ]; then
    err "This must be run as root. Try: sudo bash $0"
    exit 1
fi

# Detect CasaOS
if [ ! -x "/usr/bin/casaos" ]; then
    err "CasaOS is not installed. This package upgrades an existing installation."
    exit 1
fi

CURRENT=$(/usr/bin/casaos -v 2>/dev/null | grep -oP '[\d.]+' | head -1 || echo "unknown")
log "Current version: ${CURRENT}"
log "Upgrading to: CasaNova v0.5.0"
echo ""

read -p "Proceed with upgrade? [y/N] " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

# Extract payload
ARCHIVE_START=$(awk '/^__ARCHIVE_BELOW__$/{print NR + 1; exit 0; }' "$0")
EXTRACT_DIR="/tmp/casanova-upgrade-$$"
mkdir -p "$EXTRACT_DIR"

step "Extracting package..."
tail -n+"$ARCHIVE_START" "$0" | tar -xzf - -C "$EXTRACT_DIR"

# Backup
TIMESTAMP=$(date +%s)
BACKUP_DIR="/var/lib/casaos/backups/${TIMESTAMP}"
step "Creating backup at ${BACKUP_DIR}"
mkdir -p "$BACKUP_DIR"
[ -d /etc/casaos ] && cp -a /etc/casaos "$BACKUP_DIR/etc-casaos" && log "Configs backed up"
[ -d /var/lib/casaos/db ] && cp -a /var/lib/casaos/db "$BACKUP_DIR/db" && log "Databases backed up"
echo "$BACKUP_DIR" > /var/lib/casaos/last-backup-path

# Stop services
step "Stopping services"
SERVICES=(casaos casaos-app-management casaos-local-storage casaos-user-service casaos-message-bus casaos-gateway)
for svc in "${SERVICES[@]}"; do
    systemctl stop "${svc}.service" 2>/dev/null || true
done
log "Services stopped"

# Install
step "Installing CasaNova binaries and configs"
cp -af "$EXTRACT_DIR/"* / 2>/dev/null || true
log "Files installed"

# Start services
step "Starting CasaNova services"
systemctl daemon-reload

START_ORDER=(casaos-gateway casaos-message-bus casaos-user-service casaos-local-storage casaos-app-management casaos)
FAILED=false
for svc in "${START_ORDER[@]}"; do
    log "Starting ${svc}..."
    if ! systemctl start "${svc}.service" 2>/dev/null; then
        err "Failed to start ${svc}"
        FAILED=true
        break
    fi
    sleep 1
done

# Verify
step "Verifying"
sleep 2
for svc in "${START_ORDER[@]}"; do
    if systemctl is-active --quiet "${svc}.service" 2>/dev/null; then
        echo -e "  ${GREEN}[OK]${NC}   ${svc}"
    else
        echo -e "  ${RED}[FAIL]${NC} ${svc}"
        FAILED=true
    fi
done

# Cleanup
rm -rf "$EXTRACT_DIR"

if [ "$FAILED" = true ]; then
    warn "Some services failed. Rolling back..."
    # Restore backup
    [ -d "$BACKUP_DIR/etc-casaos" ] && cp -af "$BACKUP_DIR/etc-casaos/"* /etc/casaos/ 2>/dev/null || true
    [ -d "$BACKUP_DIR/db" ] && cp -af "$BACKUP_DIR/db/"* /var/lib/casaos/db/ 2>/dev/null || true
    systemctl daemon-reload
    for svc in "${START_ORDER[@]}"; do
        systemctl start "${svc}.service" 2>/dev/null || true
    done
    err "Rollback complete. Check: journalctl -u 'casaos-*'"
    exit 1
fi

GATEWAY_PORT=$(grep -oP 'Port\s*=\s*\K\d+' /etc/casaos/gateway.ini 2>/dev/null || echo "80")
IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")

echo ""
echo -e "${GREEN}${BOLD}"
echo "  ============================================"
echo "  CasaNova v0.5.0 installed successfully!"
echo "  ============================================"
echo -e "${NC}"
echo -e "  ${CYAN}Web UI:${NC}   http://${IP}:${GATEWAY_PORT}"
echo -e "  ${CYAN}Backup:${NC}   ${BACKUP_DIR}"
echo -e "  ${CYAN}Rollback:${NC} sudo bash /usr/share/casaos/rollback-casanova.sh"
echo ""
echo "  Your Docker containers and data are untouched."
echo ""

exit 0
__ARCHIVE_BELOW__
UPGRADE_SCRIPT

# Append compressed payload
cd "$BUILD_DIR/payload"
tar -czf - . >> "$BUILD_DIR/header.sh"

# Create final .run file
mv "$BUILD_DIR/header.sh" "$OUTPUT_FILE"
chmod +x "$OUTPUT_FILE"

# Cleanup
rm -rf "$BUILD_DIR"

SIZE=$(du -h "$OUTPUT_FILE" | cut -f1)
echo ""
echo "==> Package built: ${OUTPUT_FILE} (${SIZE})"
echo ""
echo "To upgrade a CasaOS server:"
echo "  1. Copy the file to the server (USB, SCP, file manager upload)"
echo "  2. Open the CasaOS terminal"
echo "  3. Run: sudo bash /path/to/$(basename "$OUTPUT_FILE")"
