#!/usr/bin/bash
#
#   CasaNova Rollback Script
#   Restores a CasaOS installation from backup after a failed CasaNova upgrade.
#
#   Usage:
#       sudo bash /usr/share/casaos/rollback-casanova.sh
#       # or
#       sudo casanova-rollback
#
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'
BOLD='\033[1m'

log()  { echo -e "${GREEN}[Rollback]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARNING]${NC} $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# Must be root
if [ "$EUID" -ne 0 ]; then
    echo "This script must be run as root. Try: sudo $0"
    exit 1
fi

echo -e "${BOLD}CasaNova Rollback${NC}"
echo ""

# Find latest backup
BACKUP_PATH_FILE="/var/lib/casaos/last-backup-path"
if [ -f "$BACKUP_PATH_FILE" ]; then
    BACKUP_DIR=$(cat "$BACKUP_PATH_FILE")
else
    # Find most recent backup directory
    BACKUP_DIR=$(ls -td /var/lib/casaos/backups/*/ 2>/dev/null | head -1)
fi

if [ -z "$BACKUP_DIR" ] || [ ! -d "$BACKUP_DIR" ]; then
    err "No backup found. Cannot rollback."
    echo "Backup directories searched:"
    echo "  - Path from $BACKUP_PATH_FILE"
    echo "  - /var/lib/casaos/backups/*/"
    exit 1
fi

log "Found backup: ${BOLD}${BACKUP_DIR}${NC}"

# Show what's in the backup
if [ -f "$BACKUP_DIR/binary-versions.txt" ]; then
    echo ""
    echo "Previous versions:"
    cat "$BACKUP_DIR/binary-versions.txt" | sed 's/^/  /'
    echo ""
fi

# Confirm
read -p "Restore from this backup? [y/N] " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

# Stop services
# A failed upgrade may have already started some of the NEW casanova-*
# units before failing; stop those first so a half-started unit isn't
# still running (and fighting for ports/db locks) while we restore the
# old CasaOS state below.
CASANOVA_SERVICES=(
    "casanova.service"
    "casanova-app-management.service"
    "casanova-local-storage.service"
    "casanova-user-service.service"
    "casanova-message-bus.service"
    "casanova-gateway.service"
)

SERVICES=(
    "casaos.service"
    "casaos-app-management.service"
    "casaos-local-storage.service"
    "casaos-user-service.service"
    "casaos-message-bus.service"
    "casaos-gateway.service"
)

log "Stopping services..."
for svc in "${CASANOVA_SERVICES[@]}" "${SERVICES[@]}"; do
    systemctl stop "$svc" 2>/dev/null || true
done

# Restore configs
if [ -d "$BACKUP_DIR/etc-casaos" ]; then
    cp -af "$BACKUP_DIR/etc-casaos/"* /etc/casaos/ 2>/dev/null || true
    log "Restored configuration files"
else
    warn "No config backup found"
fi

# Restore databases
if [ -d "$BACKUP_DIR/db" ]; then
    cp -af "$BACKUP_DIR/db/"* /var/lib/casaos/db/ 2>/dev/null || true
    log "Restored databases"
else
    warn "No database backup found"
fi

# Reload and restart
systemctl daemon-reload

START_ORDER=(
    "casaos-gateway.service"
    "casaos-message-bus.service"
    "casaos-user-service.service"
    "casaos-local-storage.service"
    "casaos-app-management.service"
    "casaos.service"
)

log "Starting services..."
for svc in "${START_ORDER[@]}"; do
    systemctl start "$svc" 2>/dev/null || warn "Could not start $svc"
    sleep 1
done

# Verify
echo ""
log "Service status:"
for svc in "${START_ORDER[@]}"; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        echo -e "  ${GREEN}[OK]${NC}   $svc"
    else
        echo -e "  ${RED}[FAIL]${NC} $svc"
    fi
done

echo ""
log "Rollback complete. Check your services with: systemctl status casaos-*.service"
