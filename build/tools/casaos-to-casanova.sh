#!/usr/bin/env bash
###############################################################################
# CasaOS → CasaNova Migration & Upgrade Assistant
# LucentOpen Software · v1.1.0
#
# Three modes of operation:
#
#   MIGRATE  — First-time upgrade from CasaOS to CasaNova. Backs up CasaOS
#              state, deploys CasaNova, verifies Docker containers intact.
#
#   UPGRADE  — Deploy a newer CasaNova dev build on top of an existing
#              CasaNova installation. Backs up current CasaNova state (DBs,
#              configs, frontend), hot-swaps binaries/frontend/systemd,
#              preserves ALL data (Docker, databases, user configs, mounts).
#              This is the development iteration tool — run it every time
#              you build a new version and want to push it to the test server.
#
#   UPDATE   — Apply a .tar.gz update package produced by casanova-package.sh.
#              Same as --upgrade but reads from a tar.gz instead of build/full/.
#              This is what the self-update engine calls internally.
#
# Usage:
#   # First-time CasaOS → CasaNova migration
#   sudo bash casaos-to-casanova.sh --build-dir=/path/to/build/full
#
#   # Deploy newer CasaNova dev build (keeps all data)
#   sudo bash casaos-to-casanova.sh --upgrade --build-dir=/path/to/build/full
#
#   # Apply a tar.gz update package
#   sudo bash casaos-to-casanova.sh --upgrade --package=/path/to/casanova-v0.9.1.tar.gz
#
#   # Rollback to CasaOS (after migration)
#   sudo bash casaos-to-casanova.sh --rollback
#
#   # Rollback to previous CasaNova build (after upgrade)
#   sudo bash casaos-to-casanova.sh --rollback-upgrade
#
#   # Remove old CasaOS files after successful migration
#   sudo bash casaos-to-casanova.sh --remove-legacy
#
#   # Preview any mode without making changes
#   sudo bash casaos-to-casanova.sh --dry-run --upgrade --build-dir=...
#
# Docker containers and their data are NEVER touched.
###############################################################################
set -euo pipefail

# ─── Constants ───────────────────────────────────────────────────────────────
SCRIPT_VERSION="1.1.0"
CASANOVA_VERSION="5.0.0"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BACKUP_BASE="/var/lib/casaos/backups"
UPGRADE_BACKUP_BASE="/var/lib/casanova/backups"
BACKUP_DIR="${BACKUP_BASE}/migration-${TIMESTAMP}"
UPGRADE_BACKUP_DIR="${UPGRADE_BACKUP_BASE}/upgrade-${TIMESTAMP}"
LAST_BACKUP_FILE="${BACKUP_BASE}/.last-migration-backup"
LAST_UPGRADE_FILE="${UPGRADE_BACKUP_BASE}/.last-upgrade-backup"

CASAOS_BINARIES=(casaos casaos-gateway casaos-user-service casaos-local-storage casaos-app-management casaos-message-bus casaos-cli)
CASAOS_SERVICES_STOP=(casaos casaos-app-management casaos-local-storage casaos-user-service casaos-message-bus casaos-gateway)

# KEEP IN SYNC WITH THE SERVICE LISTS BELOW.
#
# casanova-virt and casanova-driver were missing here while both appeared in
# CASANOVA_SERVICES_STOP/START two lines down: the upgrade stopped them, replaced
# them and started them, and never backed them up. Every rollback since they were
# introduced would therefore have restored eight of ten binaries and left those
# two at the NEW version — a mixed build nobody has ever tested, produced by the
# command an operator runs precisely when they want to undo a change.
# gates.sh G12 now fails if these lists drift again.
CASANOVA_BINARIES=(casanova casanova-gateway casanova-user-service casanova-local-storage casanova-app-management casanova-message-bus casanova-job-management casanova-driver casanova-virt casanova-cli)
CASANOVA_SERVICES_STOP=(casanova casanova-job-management casanova-app-management casanova-local-storage casanova-user-service casanova-virt casanova-driver casanova-message-bus casanova-gateway)
CASANOVA_SERVICES_START=(casanova-gateway casanova-message-bus casanova-driver casanova-virt casanova-user-service casanova-local-storage-first casanova-local-storage casanova-app-management casanova-job-management casanova)

# ─── Internal credential for verification calls ──────────────────────────────
#
# The verification step below asks the API how many apps are installed. It used
# to do that with a bare curl and no credential, which worked only because the
# JWT skipper trusted loopback — the same bug that let the internet read every
# app's compose file. Now that the skipper requires a real credential, this
# script has to present one.
#
# It runs as root, and the per-boot secret is a 0600 file in a 0700 directory
# both owned by root, so reading it is not a privilege escalation: anyone who
# can read it is already root. If it is missing (services not yet started, or an
# older release), we degrade to an unauthenticated call and the count simply
# comes back empty — the same as before. We never fail an upgrade over it.
internal_auth_header() {
    local f="/run/casanova/internal/service.secret"
    [ -r "$f" ] || return 1
    local s; s="$(cat "$f" 2>/dev/null)" || return 1
    [ -n "$s" ] || return 1
    printf 'X-CasaNova-Internal: %s' "$s"
}

api_app_count() {
    local gw_port="$1" hdr
    if ! hdr="$(internal_auth_header)"; then
        echo ""            # not measured — see the JSON/display split below
        return
    fi
    curl -sf --max-time 10 -H "$hdr" \
        "http://localhost:${gw_port}/v2/app_management/compose" 2>/dev/null \
        | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('data',{})))" 2>/dev/null \
        || echo ""
}

# NOT MEASURED IS NOT ZERO, AND IT IS NOT THE STRING "unknown" EITHER.
#
# api_apps_count goes into upgrade-report.json, which CasaNova parses to find
# the backup location for a rollback. Emitting `unknown` there produces invalid
# JSON and disables the rollback button — trading a wrong number for a corrupt
# report. Emitting 0 is worse in a different way: it presents an unmeasured
# count as a measurement, and "0 apps after upgrade" reads as a disaster.
#
# So the two consumers get different renderings of the same empty value: `null`
# for JSON, which is valid and means exactly "not measured", and "unknown" for
# the human, who can act on it.
apps_for_json()    { [ -n "$1" ] && printf '%s' "$1" || printf 'null'; }
apps_for_display() { [ -n "$1" ] && printf '%s' "$1" || printf 'unknown'; }

# ─── Colours ─────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
err()   { echo -e "${RED}[-]${NC} $*"; }
step()  { echo -e "\n${BOLD}${CYAN}══ $* ══${NC}"; }

# ─── Argument parsing ────────────────────────────────────────────────────────
BUILD_DIR=""
PACKAGE_FILE=""
DRY_RUN=false
DO_ROLLBACK=false
DO_ROLLBACK_UPGRADE=false
DO_REMOVE_LEGACY=false
DO_UPGRADE=false
SKIP_AUDIT=false
FORCE_CONFIG=false
START_TIME=$(date +%s)

for arg in "$@"; do
    case "$arg" in
        --build-dir=*) BUILD_DIR="${arg#*=}" ;;
        --package=*)   PACKAGE_FILE="${arg#*=}" ;;
        --dry-run)     DRY_RUN=true ;;
        --rollback)    DO_ROLLBACK=true ;;
        --rollback-upgrade) DO_ROLLBACK_UPGRADE=true ;;
        --remove-legacy) DO_REMOVE_LEGACY=true ;;
        --upgrade)     DO_UPGRADE=true ;;
        --skip-audit)  SKIP_AUDIT=true ;;
        --force-config) FORCE_CONFIG=true ;;
        --help|-h)
            echo "CasaOS → CasaNova Migration & Upgrade Assistant v${SCRIPT_VERSION}"
            echo ""
            echo "Usage:"
            echo "  sudo bash $0 --build-dir=/path/to/build/full                   # Migrate from CasaOS"
            echo "  sudo bash $0 --upgrade --build-dir=/path/to/build/full         # Deploy new CasaNova build"
            echo "  sudo bash $0 --upgrade --package=/path/to/update.tar.gz        # Apply update package"
            echo "  sudo bash $0 --rollback                                        # Rollback to CasaOS"
            echo "  sudo bash $0 --rollback-upgrade                                # Rollback to previous CasaNova build"
            echo "  sudo bash $0 --remove-legacy                                   # Remove old CasaOS files"
            echo "  sudo bash $0 --dry-run --upgrade --build-dir=...               # Preview upgrade"
            echo ""
            echo "Options:"
            echo "  --build-dir=PATH     Path to the CasaNova build/full/ directory"
            echo "  --package=FILE       Path to a .tar.gz update package"
            echo "  --upgrade            Deploy new dev build over existing CasaNova (preserves data)"
            echo "  --dry-run            Show what would happen without making changes"
            echo "  --rollback           Restore the last CasaOS backup (migration rollback)"
            echo "  --rollback-upgrade   Restore the previous CasaNova build (upgrade rollback)"
            echo "  --remove-legacy      Remove old CasaOS binaries/units after successful migration"
            echo "  --skip-audit         Skip the pre-migration audit (faster, less safe)"
            echo "  --force-config       Overwrite configs during upgrade (default: preserve existing)"
            exit 0
            ;;
        *) err "Unknown argument: $arg"; exit 1 ;;
    esac
done

# ─── Root check ──────────────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    err "This script must be run as root. Use: sudo bash $0 $*"
    exit 1
fi

###############################################################################
# ROLLBACK (CasaOS migration)
###############################################################################
rollback() {
    step "ROLLBACK — Restoring CasaOS from backup"

    if [ "$DRY_RUN" = true ]; then
        warn "DRY RUN MODE — showing what rollback would do"
    fi

    # Find the latest migration backup
    local backup=""
    if [ -f "$LAST_BACKUP_FILE" ]; then
        backup=$(cat "$LAST_BACKUP_FILE")
    fi
    if [ -z "$backup" ] || [ ! -d "$backup" ]; then
        backup=$(ls -td "${BACKUP_BASE}"/migration-* 2>/dev/null | head -1)
    fi
    if [ -z "$backup" ] || [ ! -d "$backup" ]; then
        err "No migration backup found in ${BACKUP_BASE}/"
        exit 1
    fi
    info "Using backup: ${backup}"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would stop CasaNova services"
        info "[dry-run] Would restore /etc/casaos/ from ${backup}/etc-casaos.tar.gz"
        info "[dry-run] Would restore databases from ${backup}/db.tar.gz"
        info "[dry-run] Would restore user data from ${backup}/conf.tar.gz"
        info "[dry-run] Would re-enable and start CasaOS services"
        echo ""
        info "DRY RUN complete. No changes were made."
        exit 0
    fi

    # Stop CasaNova services
    info "Stopping CasaNova services..."
    for svc in "${CASANOVA_SERVICES_STOP[@]}"; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
    done

    # Restore configs
    if [ -f "${backup}/etc-casaos.tar.gz" ]; then
        info "Restoring /etc/casaos/..."
        rm -rf /etc/casaos
        tar -xzf "${backup}/etc-casaos.tar.gz" -C /
    fi

    # Restore databases
    if [ -f "${backup}/db.tar.gz" ]; then
        info "Restoring databases..."
        tar -xzf "${backup}/db.tar.gz" -C /
    fi

    # Restore user config
    if [ -f "${backup}/conf.tar.gz" ]; then
        info "Restoring user data..."
        tar -xzf "${backup}/conf.tar.gz" -C /
    fi

    # Restore app compose metadata — backup_casaos saves this as
    # apps-metadata.tar.gz; without restoring it the rolled-back CasaOS loses
    # its /var/lib/casaos/apps compose files (installed apps look gone).
    if [ -f "${backup}/apps-metadata.tar.gz" ]; then
        info "Restoring app metadata..."
        tar -xzf "${backup}/apps-metadata.tar.gz" -C /
    fi

    # Re-enable CasaOS services
    info "Re-enabling CasaOS services..."
    systemctl daemon-reload
    for svc in casaos-gateway casaos-message-bus casaos-user-service casaos-local-storage casaos-app-management casaos; do
        systemctl enable "$svc" 2>/dev/null || true
        systemctl start "$svc" 2>/dev/null || true
        sleep 2
    done

    # Verify
    sleep 5
    local healthy=0
    for svc in casaos-gateway casaos-message-bus casaos-user-service casaos-local-storage casaos-app-management casaos; do
        if [ "$(systemctl is-active "$svc" 2>/dev/null)" = "active" ]; then
            healthy=$((healthy + 1))
            info "$svc: active"
        else
            warn "$svc: $(systemctl is-active "$svc" 2>/dev/null || echo 'unknown')"
        fi
    done

    echo ""
    if [ "$healthy" -ge 5 ]; then
        info "Rollback successful. CasaOS is restored (${healthy}/6 services active)."
    else
        warn "Rollback completed but only ${healthy}/6 services are active. Check logs."
    fi
    exit 0
}

###############################################################################
# ROLLBACK UPGRADE (CasaNova → previous CasaNova build)
###############################################################################
rollback_upgrade() {
    step "ROLLBACK UPGRADE — Restoring previous CasaNova build"

    if [ "$DRY_RUN" = true ]; then
        warn "DRY RUN MODE — showing what rollback-upgrade would do"
    fi

    local backup=""
    if [ -f "$LAST_UPGRADE_FILE" ]; then
        backup=$(cat "$LAST_UPGRADE_FILE")
    fi
    if [ -z "$backup" ] || [ ! -d "$backup" ]; then
        backup=$(ls -td "${UPGRADE_BACKUP_BASE}"/upgrade-* 2>/dev/null | head -1)
    fi
    if [ -z "$backup" ] || [ ! -d "$backup" ]; then
        err "No upgrade backup found in ${UPGRADE_BACKUP_BASE}/"
        exit 1
    fi
    info "Using backup: ${backup}"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would stop CasaNova services"
        info "[dry-run] Would restore binaries from ${backup}/bin/"
        info "[dry-run] Would restore systemd units from ${backup}/systemd/"
        [ -d "${backup}/www" ] && info "[dry-run] Would restore frontend from ${backup}/www/"
        [ -d "${backup}/etc-casanova" ] && info "[dry-run] Would restore configs from ${backup}/etc-casanova/"
        info "[dry-run] Would start CasaNova services"
        echo ""
        info "DRY RUN complete. No changes were made."
        exit 0
    fi

    # Stop CasaNova services
    info "Stopping CasaNova services..."
    for svc in "${CASANOVA_SERVICES_STOP[@]}"; do
        systemctl stop "$svc" 2>/dev/null || true
    done

    # From here the box has every service stopped. With `set -euo pipefail`, any
    # failure below (a bad backup, an install error) would abort the script here
    # and leave the box offline — the recovery path bricking the box. Arm a trap
    # that brings services back up on ANY error before propagating it, mirroring
    # what the migration path already does. Disarmed once the normal start loop
    # has run.
    trap 'ec=$?; trap - ERR; set +e; err "rollback-upgrade failed (exit ${ec}) after services were stopped — restarting them so the box stays reachable"; for svc in "${CASANOVA_SERVICES_START[@]}"; do systemctl start "$svc" 2>/dev/null || true; done; exit "${ec}"' ERR

    # Restore binaries
    if [ -d "${backup}/bin" ]; then
        info "Restoring binaries..."
        for bin in "${backup}"/bin/*; do
            [ -f "$bin" ] || continue
            local bname
            bname=$(basename "$bin")
            install -m 0755 "$bin" "/usr/bin/${bname}"
            echo "  /usr/bin/${bname}"
        done
    fi

    # Restore systemd units
    if [ -d "${backup}/systemd" ]; then
        info "Restoring systemd units..."
        cp "${backup}"/systemd/*.service /usr/lib/systemd/system/ 2>/dev/null || true
        systemctl daemon-reload
    fi

    # Restore frontend
    if [ -d "${backup}/www" ]; then
        info "Restoring frontend..."
        rm -rf /var/lib/casanova/www/*
        cp -r "${backup}"/www/* /var/lib/casanova/www/
    fi

    # Restore configs if they were force-overwritten
    if [ -d "${backup}/etc-casanova" ]; then
        info "Restoring configs..."
        cp -r "${backup}"/etc-casanova/* /etc/casanova/ 2>/dev/null || true
        # Also sync back to legacy path
        for conf in /etc/casanova/*; do
            [ -f "$conf" ] || continue
            local bname
            bname=$(basename "$conf")
            case "$bname" in
                casanova.conf) cp "$conf" /etc/casaos/casaos.conf 2>/dev/null || true ;;
                *) cp "$conf" "/etc/casaos/${bname}" 2>/dev/null || true ;;
            esac
        done
    fi

    # Start services
    info "Starting CasaNova services..."
    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        systemctl start "$svc" 2>/dev/null || true
        sleep 2
    done
    # Services are back up through the normal path; the safety trap has done its
    # job and must not fire on a later non-fatal step.
    trap - ERR

    # Verify
    sleep 3
    local healthy=0
    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        if [ "$(systemctl is-active "$svc" 2>/dev/null)" = "active" ]; then
            healthy=$((healthy + 1))
            info "$svc: active"
        else
            warn "$svc: $(systemctl is-active "$svc" 2>/dev/null || echo 'unknown')"
        fi
    done

    local prev_version="unknown"
    [ -f "${backup}/version.txt" ] && prev_version=$(cat "${backup}/version.txt")

    echo ""
    if [ "$healthy" -ge 5 ]; then
        info "Rollback successful. CasaNova ${prev_version} restored (${healthy}/7 services active)."
    else
        warn "Rollback completed but only ${healthy}/7 services are active. Check logs."
    fi
    exit 0
}

###############################################################################
# REMOVE LEGACY MODE
###############################################################################
remove_legacy() {
    step "REMOVE LEGACY — Cleaning old CasaOS files"

    # Verify CasaNova is running first
    local cn_active=0
    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        [ "$(systemctl is-active "$svc" 2>/dev/null)" = "active" ] && cn_active=$((cn_active + 1))
    done
    if [ "$cn_active" -lt 5 ]; then
        err "CasaNova is not fully running (${cn_active}/7 services). Fix CasaNova first before removing legacy files."
        exit 1
    fi

    info "CasaNova is running (${cn_active}/7 services). Proceeding with legacy cleanup."

    # Remove old binaries
    info "Removing CasaOS binaries..."
    for bin in "${CASAOS_BINARIES[@]}"; do
        if [ -f "/usr/bin/${bin}" ]; then
            rm -f "/usr/bin/${bin}"
            echo "  Removed /usr/bin/${bin}"
        fi
    done

    # Remove old systemd units
    info "Removing CasaOS systemd units..."
    for unit in /usr/lib/systemd/system/casaos-*.service /usr/lib/systemd/system/casaos.service; do
        if [ -f "$unit" ]; then
            rm -f "$unit"
            echo "  Removed $unit"
        fi
    done

    # Remove shell scripts
    if [ -d /usr/share/casaos ]; then
        info "Removing /usr/share/casaos/..."
        rm -rf /usr/share/casaos
    fi

    # Remove old logs
    if [ -d /var/log/casaos ]; then
        info "Removing old logs /var/log/casaos/..."
        rm -rf /var/log/casaos
    fi

    # NOTE: Keep /etc/casaos/ — gateway still reads from there
    # NOTE: Keep /var/lib/casaos/ — Go services still use it
    # NOTE: Keep /var/run/casaos/ — Go services write runtime files there
    warn "Keeping /etc/casaos/, /var/lib/casaos/, /var/run/casaos/ (still used by Go services)"

    systemctl daemon-reload

    echo ""
    info "Legacy CasaOS files removed. CasaNova is your sole system."
    info "Backups are preserved in ${BACKUP_BASE}/"
    exit 0
}

# Handle rollback/remove-legacy modes early
[ "$DO_ROLLBACK" = true ] && rollback
[ "$DO_ROLLBACK_UPGRADE" = true ] && rollback_upgrade
[ "$DO_REMOVE_LEGACY" = true ] && remove_legacy

###############################################################################
# UPGRADE MODE — CasaNova → newer CasaNova
###############################################################################
run_upgrade() {

    # If --package given, extract it to a temp dir and treat as --build-dir
    PACKAGE_TMPDIR=""
    if [ -n "$PACKAGE_FILE" ]; then
        if [ ! -f "$PACKAGE_FILE" ]; then
            err "Package file not found: ${PACKAGE_FILE}"
            exit 1
        fi
        PACKAGE_TMPDIR=$(mktemp -d /tmp/casanova-upgrade-XXXXXX)
        info "Extracting update package: ${PACKAGE_FILE}"
        tar -xzf "$PACKAGE_FILE" -C "$PACKAGE_TMPDIR"
        # The package may be flat or have a single top-level dir
        if [ -f "${PACKAGE_TMPDIR}/bin/casanova" ]; then
            BUILD_DIR="$PACKAGE_TMPDIR"
        elif [ -d "${PACKAGE_TMPDIR}"/casanova-*/bin ]; then
            BUILD_DIR=$(ls -d "${PACKAGE_TMPDIR}"/casanova-* | head -1)
        else
            err "Invalid package structure — expected bin/casanova inside archive"
            rm -rf "$PACKAGE_TMPDIR"
            exit 1
        fi
    fi

    if [ -z "$BUILD_DIR" ]; then
        err "Missing --build-dir or --package argument for upgrade mode."
        err "Usage: sudo bash $0 --upgrade --build-dir=/path/to/build/full"
        err "       sudo bash $0 --upgrade --package=/path/to/update.tar.gz"
        exit 1
    fi

    if [ ! -d "$BUILD_DIR" ] || [ ! -f "${BUILD_DIR}/bin/casanova" ]; then
        err "Build directory invalid or missing binaries: ${BUILD_DIR}/bin/casanova"
        [ -n "$PACKAGE_TMPDIR" ] && rm -rf "$PACKAGE_TMPDIR"
        exit 1
    fi

    # Detect incoming version from manifest.json or binary
    NEW_VERSION="unknown"
    if [ -f "${BUILD_DIR}/manifest.json" ]; then
        NEW_VERSION=$(python3 -c "import json; print(json.load(open('${BUILD_DIR}/manifest.json'))['version'])" 2>/dev/null || echo "unknown")
    fi
    if [ "$NEW_VERSION" = "unknown" ] && [ -x "${BUILD_DIR}/bin/casanova" ]; then
        NEW_VERSION=$("${BUILD_DIR}/bin/casanova" -v 2>/dev/null || echo "unknown")
    fi

    # Detect current version
    CURRENT_VERSION="unknown"
    if [ -x /usr/bin/casanova ]; then
        CURRENT_VERSION=$(/usr/bin/casanova -v 2>/dev/null || echo "unknown")
    fi

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════════════╗"
    echo "  ║     CasaNova Upgrade Assistant v${SCRIPT_VERSION}                  ║"
    echo "  ║              LucentOpen Software                         ║"
    echo "  ╠═══════════════════════════════════════════════════════════╣"
    echo "  ║  Current: ${CURRENT_VERSION}                                           ║"
    echo "  ║  Target:  ${NEW_VERSION}                                           ║"
    echo "  ╚═══════════════════════════════════════════════════════════╝"
    echo -e "${NC}"

    if [ "$DRY_RUN" = true ]; then
        warn "DRY RUN MODE — no changes will be made"
        echo ""
    fi

    # ── Upgrade Phase 1: Pre-flight ──────────────────────────────────────────
    step "Upgrade 1/6: Pre-flight Checks"

    if [ ! -x /usr/bin/casanova ] && [ ! -d /etc/casanova ]; then
        err "CasaNova is not installed. Use migration mode (without --upgrade) first."
        [ -n "$PACKAGE_TMPDIR" ] && rm -rf "$PACKAGE_TMPDIR"
        exit 1
    fi
    info "CasaNova installation detected (${CURRENT_VERSION})"

    # Check disk space
    local_free_mb=$(df -m /usr/bin | tail -1 | awk '{print $4}')
    if [ "$local_free_mb" -lt 300 ]; then
        err "Insufficient disk space: ${local_free_mb}MB free (need 300MB+)"
        [ -n "$PACKAGE_TMPDIR" ] && rm -rf "$PACKAGE_TMPDIR"
        exit 1
    fi
    info "Disk space: ${local_free_mb}MB free"

    # Check Docker — install it if absent (fresh box, or a CasaOS box that
    # somehow lost Docker). Skipped under --dry-run.
    if command -v docker &>/dev/null && docker info &>/dev/null; then
        local_container_count=$(docker ps -q 2>/dev/null | wc -l)
        info "Docker: running (${local_container_count} containers active)"
    elif [ "$DRY_RUN" = true ]; then
        warn "Docker not detected — would install Docker CE (skipped under --dry-run)"
        local_container_count=0
    else
        warn "Docker not detected — installing Docker CE from the official repo..."
        ensure_docker
        local_container_count=0
    fi

    info "Build directory: ${BUILD_DIR} ($(ls "${BUILD_DIR}/bin/" | wc -l) binaries)"
    info "Pre-flight checks passed"

    # ── Upgrade Phase 2: Backup current CasaNova ─────────────────────────────
    step "Upgrade 2/6: Backing up current CasaNova"

    mkdir -p "$UPGRADE_BACKUP_DIR"

    if [ "$DRY_RUN" = false ]; then
        # Save current version
        echo "$CURRENT_VERSION" > "${UPGRADE_BACKUP_DIR}/version.txt"

        # Backup binaries
        mkdir -p "${UPGRADE_BACKUP_DIR}/bin"
        for bin in "${CASANOVA_BINARIES[@]}"; do
            [ -f "/usr/bin/${bin}" ] && cp "/usr/bin/${bin}" "${UPGRADE_BACKUP_DIR}/bin/"
        done
        info "Backed up $(ls "${UPGRADE_BACKUP_DIR}/bin/" | wc -l) binaries"

        # Backup systemd units
        mkdir -p "${UPGRADE_BACKUP_DIR}/systemd"
        cp /usr/lib/systemd/system/casanova*.service "${UPGRADE_BACKUP_DIR}/systemd/" 2>/dev/null || true
        info "Backed up systemd units"

        # Backup frontend
        if [ -d /var/lib/casanova/www ]; then
            mkdir -p "${UPGRADE_BACKUP_DIR}/www"
            cp -r /var/lib/casanova/www/* "${UPGRADE_BACKUP_DIR}/www/" 2>/dev/null || true
            info "Backed up frontend"
        fi

        # Backup configs
        if [ -d /etc/casanova ]; then
            mkdir -p "${UPGRADE_BACKUP_DIR}/etc-casanova"
            cp -r /etc/casanova/* "${UPGRADE_BACKUP_DIR}/etc-casanova/" 2>/dev/null || true
            info "Backed up configs"
        fi

        # Backup databases
        if [ -d /var/lib/casaos/db ]; then
            tar -czf "${UPGRADE_BACKUP_DIR}/db-casaos.tar.gz" -C / var/lib/casaos/db 2>/dev/null || true
            info "Backed up CasaOS databases"
        fi
        if [ -d /var/lib/casanova/db ]; then
            tar -czf "${UPGRADE_BACKUP_DIR}/db-casanova.tar.gz" -C / var/lib/casanova/db 2>/dev/null || true
            info "Backed up CasaNova databases"
        fi

        # Record running containers
        docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}' > "${UPGRADE_BACKUP_DIR}/running-containers.txt" 2>/dev/null || true

        # Detect gateway port
        local gw_port="80"
        if [ -f /etc/casanova/gateway.ini ]; then
            gw_port=$(grep -i '^port=' /etc/casanova/gateway.ini 2>/dev/null | cut -d= -f2 | tr -d ' ' || echo "80")
        elif [ -f /etc/casaos/gateway.ini ]; then
            gw_port=$(grep -i '^port=' /etc/casaos/gateway.ini 2>/dev/null | cut -d= -f2 | tr -d ' ' || echo "80")
        fi
        [ -z "$gw_port" ] && gw_port="80"
        echo "$gw_port" > "${UPGRADE_BACKUP_DIR}/.gateway-port"

        # Save backup path for rollback
        echo "$UPGRADE_BACKUP_DIR" > "$LAST_UPGRADE_FILE"

        local total_size
        total_size=$(du -sh "$UPGRADE_BACKUP_DIR" | awk '{print $1}')
        info "Backup complete: ${UPGRADE_BACKUP_DIR} (${total_size})"

        # Backup metadata
        cat > "${UPGRADE_BACKUP_DIR}/backup-metadata.json" << ENDJSON
{
  "type": "upgrade",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "hostname": "$(hostname)",
  "from_version": "${CURRENT_VERSION}",
  "to_version": "${NEW_VERSION}",
  "backup_dir": "${UPGRADE_BACKUP_DIR}"
}
ENDJSON
    else
        info "[dry-run] Would backup binaries, systemd units, frontend, configs, databases"
    fi

    # ── Upgrade Phase 3: Stop services ───────────────────────────────────────
    step "Upgrade 3/6: Stopping CasaNova services"

    for svc in "${CASANOVA_SERVICES_STOP[@]}"; do
        if systemctl is-active "$svc" &>/dev/null; then
            info "Stopping ${svc}..."
            if [ "$DRY_RUN" = false ]; then
                systemctl stop "$svc" 2>/dev/null || true
            fi
        fi
    done
    # Also stop the local-storage-first oneshot
    if [ "$DRY_RUN" = false ]; then
        systemctl stop casanova-local-storage-first 2>/dev/null || true
    fi
    info "CasaNova services stopped"

    # ── Upgrade Phase 4: Deploy new build ────────────────────────────────────
    step "Upgrade 4/6: Deploying CasaNova ${NEW_VERSION}"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would install binaries from ${BUILD_DIR}/bin/"
        info "[dry-run] Would install systemd units from ${BUILD_DIR}/systemd/"
        info "[dry-run] Would install frontend from ${BUILD_DIR}/www/"
        [ "$FORCE_CONFIG" = true ] && info "[dry-run] Would overwrite configs (--force-config)"
    else
        # Install binaries
        info "Installing binaries..."
        for bin in "${BUILD_DIR}"/bin/*; do
            [ -f "$bin" ] || continue
            local bname
            bname=$(basename "$bin")
            install -m 0755 "$bin" "/usr/bin/${bname}"
            echo "  /usr/bin/${bname}"
        done

        # Install systemd units
        if [ -d "${BUILD_DIR}/systemd" ]; then
            info "Installing systemd units..."
            cp "${BUILD_DIR}"/systemd/casanova*.service /usr/lib/systemd/system/
        fi

        # Install frontend
        if [ -d "${BUILD_DIR}/www" ]; then
            info "Installing frontend..."
            mkdir -p /var/lib/casanova/www
            rm -rf /var/lib/casanova/www/*
            cp -r "${BUILD_DIR}"/www/* /var/lib/casanova/www/
        fi

        # Configs: only overwrite if --force-config, otherwise preserve existing
        if [ "$FORCE_CONFIG" = true ] && [ -d "${BUILD_DIR}/config" ]; then
            info "Overwriting configs (--force-config)..."
            for sample in "${BUILD_DIR}"/config/*.sample; do
                [ -f "$sample" ] || continue
                local target
                target=$(basename "$sample" .sample)
                [ "$target" = "casaos.conf" ] && target="casanova.conf"
                cp "$sample" "/etc/casanova/${target}"
                echo "  /etc/casanova/${target}"
            done
            # Sync to legacy path
            for conf in /etc/casanova/*; do
                [ -f "$conf" ] || continue
                local bname
                bname=$(basename "$conf")
                case "$bname" in
                    casanova.conf) cp "$conf" /etc/casaos/casaos.conf 2>/dev/null || true ;;
                    *) cp "$conf" "/etc/casaos/${bname}" 2>/dev/null || true ;;
                esac
            done
        else
            info "Preserving existing configs (use --force-config to overwrite)"
        fi

        # Copy manifest if present
        if [ -f "${BUILD_DIR}/manifest.json" ]; then
            cp "${BUILD_DIR}/manifest.json" /var/lib/casanova/manifest.json
        fi

        systemctl daemon-reload
        info "CasaNova ${NEW_VERSION} deployed"
    fi

    # ── Upgrade Phase 5: Start services ──────────────────────────────────────
    step "Upgrade 5/6: Starting CasaNova services"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would start services in order: ${CASANOVA_SERVICES_START[*]}"
    else
        # Clean stale runtime files
        rm -f /var/run/casaos/*.url /var/run/casaos/management.url 2>/dev/null || true

        for svc in "${CASANOVA_SERVICES_START[@]}"; do
            info "Starting ${svc}..."
            systemctl enable "$svc" 2>/dev/null || true
            if ! systemctl start "$svc" 2>/dev/null; then
                warn "  ${svc} failed to start on first attempt"
            fi
            sleep 2
        done

        # Retry any failed services
        for svc in "${CASANOVA_SERVICES_START[@]}"; do
            if [ "$(systemctl is-active "$svc" 2>/dev/null)" != "active" ]; then
                info "  Retrying ${svc}..."
                systemctl restart "$svc" 2>/dev/null || true
                sleep 3
            fi
        done
        info "CasaNova services started"
    fi

    # ── Upgrade Phase 6: Verify ──────────────────────────────────────────────
    step "Upgrade 6/6: Verification"

    local gw_port="80"
    [ -f "${UPGRADE_BACKUP_DIR}/.gateway-port" ] && gw_port=$(cat "${UPGRADE_BACKUP_DIR}/.gateway-port")
    [ -z "$gw_port" ] && gw_port="80"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would verify services, gateway on port ${gw_port}, containers"
    else
        sleep 5

        local services_ok=0
        for svc in "${CASANOVA_SERVICES_START[@]}"; do
            local state
            state=$(systemctl is-active "$svc" 2>/dev/null || echo "inactive")
            if [ "$state" = "active" ]; then
                services_ok=$((services_ok + 1))
                info "  ${svc}: ${GREEN}active${NC}"
            else
                err "  ${svc}: ${RED}${state}${NC}"
            fi
        done

        # Gateway ping
        local gw_ping=false
        if curl -sf --max-time 10 "http://localhost:${gw_port}/ping" &>/dev/null; then
            gw_ping=true
            info "Gateway ping: OK (port ${gw_port})"
        else
            warn "Gateway ping: FAIL (port ${gw_port})"
        fi

        # Frontend
        local frontend_ok=false
        local http_code
        http_code=$(curl -sf --max-time 10 -o /dev/null -w "%{http_code}" "http://localhost:${gw_port}/" 2>/dev/null || echo "000")
        if [ "$http_code" = "200" ]; then
            frontend_ok=true
            info "Frontend: HTTP 200"
        else
            warn "Frontend: HTTP ${http_code}"
        fi

        # Docker containers
        local containers_before=0
        [ -f "${UPGRADE_BACKUP_DIR}/running-containers.txt" ] && containers_before=$(wc -l < "${UPGRADE_BACKUP_DIR}/running-containers.txt")
        local containers_after
        containers_after=$(docker ps -q 2>/dev/null | wc -l)
        info "Containers: ${containers_before} before → ${containers_after} after"

        # API check
        local api_apps=0
        api_apps=$(api_app_count "${gw_port}")
        info "API: $(apps_for_display "${api_apps}") apps visible"

        # Verify installed version
        local installed_ver="unknown"
        [ -x /usr/bin/casanova ] && installed_ver=$(/usr/bin/casanova -v 2>/dev/null || echo "unknown")

        local end_time
        end_time=$(date +%s)
        local duration=$((end_time - START_TIME))

        local status="success"
        if [ "$services_ok" -lt 5 ] || [ "$gw_ping" = false ]; then
            status="failed"
        elif [ "$services_ok" -lt ${#CASANOVA_SERVICES_START[@]} ]; then
            status="partial"
        fi

        # Generate upgrade report
        cat > "${UPGRADE_BACKUP_DIR}/upgrade-report.json" << ENDJSON
{
  "type": "upgrade",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "status": "${status}",
  "from_version": "${CURRENT_VERSION}",
  "to_version": "${NEW_VERSION}",
  "installed_version": "${installed_ver}",
  "duration_seconds": ${duration},
  "services_ok": ${services_ok},
  "services_total": ${#CASANOVA_SERVICES_START[@]},
  "gateway_port": ${gw_port},
  "gateway_ping": ${gw_ping},
  "frontend_ok": ${frontend_ok},
  "containers_before": ${containers_before},
  "containers_after": ${containers_after},
  "api_apps_count": $(apps_for_json "${api_apps}"),
  "backup_location": "${UPGRADE_BACKUP_DIR}",
  "rollback_command": "sudo bash $0 --rollback-upgrade"
}
ENDJSON

        echo ""
        echo -e "${BOLD}${CYAN}"
        echo "  ╔═══════════════════════════════════════════════════════════╗"
        echo "  ║              CasaNova Upgrade Report                      ║"
        echo "  ╠═══════════════════════════════════════════════════════════╣"
        echo "  ║  Status:   ${status}                                        ║"
        echo "  ║  From:     ${CURRENT_VERSION}                                           ║"
        echo "  ║  To:       ${installed_ver}                                           ║"
        echo "  ║  Duration: ${duration}s                                             ║"
        echo "  ║  Services: ${services_ok}/${#CASANOVA_SERVICES_START[@]}                                           ║"
        echo "  ║  Gateway:  port ${gw_port} (ping: ${gw_ping})                      ║"
        echo "  ║  Frontend: ${frontend_ok}                                          ║"
        echo "  ║  Apps:     $(apps_for_display "${api_apps}")                                              ║"
        echo "  ╠═══════════════════════════════════════════════════════════╣"
        echo "  ║  Backup:   ${UPGRADE_BACKUP_DIR}"
        echo "  ║  Rollback: sudo bash $0 --rollback-upgrade"
        echo "  ╚═══════════════════════════════════════════════════════════╝"
        echo -e "${NC}"

        if [ "$status" = "failed" ]; then
            err "Upgrade verification FAILED. Run --rollback-upgrade to restore previous build."
        elif [ "$status" = "partial" ]; then
            warn "Upgrade completed with warnings."
        else
            info "Upgrade successful!"
        fi

        info "Report: ${UPGRADE_BACKUP_DIR}/upgrade-report.json"
    fi

    # Cleanup temp dir from package extraction
    [ -n "$PACKAGE_TMPDIR" ] && rm -rf "$PACKAGE_TMPDIR"

    if [ "$DRY_RUN" = true ]; then
        echo ""
        info "DRY RUN complete. No changes were made."
    fi
    exit 0
}

[ "$DO_UPGRADE" = true ] && run_upgrade

###############################################################################
# MIGRATION MODE (CasaOS → CasaNova)
###############################################################################

if [ -z "$BUILD_DIR" ]; then
    err "Missing --build-dir argument. Usage: sudo bash $0 --build-dir=/path/to/build/full"
    exit 1
fi

echo -e "${BOLD}${CYAN}"
echo "  ╔═══════════════════════════════════════════════════════════╗"
echo "  ║     CasaOS → CasaNova Migration Assistant v${SCRIPT_VERSION}       ║"
echo "  ║              LucentOpen Software                         ║"
echo "  ╚═══════════════════════════════════════════════════════════╝"
echo -e "${NC}"

if [ "$DRY_RUN" = true ]; then
    warn "DRY RUN MODE — no changes will be made"
    echo ""
fi

###############################################################################
# Ensure a container runtime — install Docker CE if absent
###############################################################################
# Install Docker from the OFFICIAL apt repo pinned to THIS box's live codename.
# Hardcoding/leaving a stale codename is how Docker silently freezes after a
# `do-release-upgrade` (which renames third-party repos to *.distUpgrade); we
# detect VERSION_CODENAME and drop any inert .distUpgrade copy. get.docker.com
# is the fallback for non-apt distros. Runs as root (checked at startup).
ensure_docker() {
    if command -v docker &>/dev/null && docker info &>/dev/null; then
        return 0
    fi
    info "Installing Docker CE..."
    if command -v apt-get &>/dev/null; then
        local os_id codename arch
        os_id=$(. /etc/os-release 2>/dev/null && echo "${ID:-ubuntu}")
        codename=$(. /etc/os-release 2>/dev/null && echo "${VERSION_CODENAME:-}")
        [ -z "${codename}" ] && codename=$(lsb_release -cs 2>/dev/null || echo stable)
        arch=$(dpkg --print-architecture 2>/dev/null || echo amd64)
        case "${os_id}" in debian|raspbian) os_id=debian ;; *) os_id=ubuntu ;; esac
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
    if ! docker info &>/dev/null; then
        err "Docker install failed — install Docker manually and re-run."
        exit 1
    fi
    info "Docker installed: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"
}

###############################################################################
# Phase 1: Pre-flight checks
###############################################################################
pre_flight_checks() {
    step "Phase 1/8: Pre-flight Checks"

    # Check CasaOS is installed
    if [ ! -f /usr/bin/casaos ] && [ ! -d /etc/casaos ]; then
        err "CasaOS is not installed on this system."
        exit 1
    fi
    info "CasaOS installation detected"

    # Record CasaOS versions
    for bin in "${CASAOS_BINARIES[@]}"; do
        if [ -x "/usr/bin/${bin}" ]; then
            local ver
            ver=$("/usr/bin/${bin}" -v 2>/dev/null || echo "unknown")
            info "  ${bin}: ${ver}"
        fi
    done

    # Check architecture
    local arch
    arch=$(uname -m)
    info "Architecture: ${arch}"

    # Check build directory
    if [ ! -d "$BUILD_DIR" ]; then
        err "Build directory not found: ${BUILD_DIR}"
        exit 1
    fi
    if [ ! -f "${BUILD_DIR}/bin/casanova" ]; then
        err "CasaNova binaries not found in ${BUILD_DIR}/bin/"
        exit 1
    fi
    info "Build directory: ${BUILD_DIR} ($(ls "${BUILD_DIR}/bin/" | wc -l) binaries)"

    # Check disk space
    local free_mb
    free_mb=$(df -m /usr/bin | tail -1 | awk '{print $4}')
    if [ "$free_mb" -lt 500 ]; then
        err "Insufficient disk space: ${free_mb}MB free (need 500MB+)"
        exit 1
    fi
    info "Disk space: ${free_mb}MB free"

    # Check Docker
    if ! command -v docker &>/dev/null || ! docker info &>/dev/null; then
        err "Docker is not running"
        exit 1
    fi
    local container_count
    container_count=$(docker ps -q 2>/dev/null | wc -l)
    info "Docker: running (${container_count} containers active)"

    info "Pre-flight checks passed"
}

###############################################################################
# Phase 2: Pre-migration audit
###############################################################################
audit_pre_migration() {
    step "Phase 2/8: Pre-migration Audit"

    mkdir -p "$BACKUP_DIR"

    # Record running containers
    docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}' > "${BACKUP_DIR}/running-containers.txt" 2>/dev/null
    local cnt
    cnt=$(wc -l < "${BACKUP_DIR}/running-containers.txt")
    info "Running containers: ${cnt}"

    # Record CasaOS service states
    local svc_json="{"
    local first=true
    for svc in casaos-gateway casaos-message-bus casaos-user-service casaos-local-storage casaos-app-management casaos; do
        local state
        state=$(systemctl is-active "$svc" 2>/dev/null || echo "not-found")
        if [ "$first" = true ]; then first=false; else svc_json+=","; fi
        svc_json+="\"${svc}\":\"${state}\""
    done
    svc_json+="}"

    # Config checksums
    local config_checksums=""
    if [ -d /etc/casaos ]; then
        config_checksums=$(find /etc/casaos -type f -exec md5sum {} \; 2>/dev/null | sort)
    fi

    # DB sizes
    local db_sizes=""
    if [ -d /var/lib/casaos/db ]; then
        db_sizes=$(du -b /var/lib/casaos/db/* 2>/dev/null | awk '{print "{\"file\":\""$2"\",\"bytes\":"$1"}"}' | paste -sd, -)
    fi

    # Gateway port
    local gw_port="80"
    if [ -f /etc/casaos/gateway.ini ]; then
        gw_port=$(grep -i '^port=' /etc/casaos/gateway.ini 2>/dev/null | cut -d= -f2 | tr -d ' ' || echo "80")
        [ -z "$gw_port" ] && gw_port="80"
    fi

    # Build pre-migration JSON
    cat > "${BACKUP_DIR}/pre-migration.json" << ENDJSON
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "hostname": "$(hostname)",
  "casaos_services": ${svc_json},
  "running_containers": ${cnt},
  "gateway_port": "${gw_port}",
  "db_sizes": [${db_sizes}],
  "architecture": "$(uname -m)",
  "os": "$(grep PRETTY_NAME /etc/os-release 2>/dev/null | cut -d'"' -f2 || echo 'unknown')"
}
ENDJSON

    info "Pre-migration snapshot saved to ${BACKUP_DIR}/pre-migration.json"
    echo "$gw_port" > "${BACKUP_DIR}/.gateway-port"
}

###############################################################################
# Phase 3: Backup CasaOS state
###############################################################################
backup_casaos() {
    step "Phase 3/8: Backing up CasaOS"

    # --dry-run must be side-effect-free: creating real backups and (worse)
    # repointing LAST_BACKUP_FILE / touching fstab would mutate rollback state
    # on a "preview" run. Describe and return.
    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would back up /etc/casaos, databases, user data and app"
        info "[dry-run] metadata to a new ${BACKUP_BASE}/migration-* dir (no writes now)."
        return 0
    fi

    mkdir -p "$BACKUP_DIR"

    # Configs
    if [ -d /etc/casaos ]; then
        info "Backing up /etc/casaos/..."
        tar -czf "${BACKUP_DIR}/etc-casaos.tar.gz" -C / etc/casaos 2>/dev/null
    fi

    # Databases. We back up BEFORE stopping CasaOS (so a failed backup leaves
    # CasaOS untouched), which means a hot snapshot — flush page cache first to
    # cut the torn-WAL risk. SQLite's WAL replays on next open, so a hot tar
    # restores cleanly in practice.
    if [ -d /var/lib/casaos/db ]; then
        info "Backing up databases..."
        sync
        tar -czf "${BACKUP_DIR}/db.tar.gz" -C / var/lib/casaos/db 2>/dev/null
    fi

    # User config
    if [ -d /var/lib/casaos/conf ]; then
        info "Backing up user data..."
        tar -czf "${BACKUP_DIR}/conf.tar.gz" -C / var/lib/casaos/conf 2>/dev/null
    fi

    # Apps metadata (compose files, NOT container data)
    if [ -d /var/lib/casaos/apps ]; then
        info "Backing up app metadata..."
        tar -czf "${BACKUP_DIR}/apps-metadata.tar.gz" -C / var/lib/casaos/apps 2>/dev/null
    fi

    # Manifest
    if [ -f /var/lib/casaos/manifest ]; then
        cp /var/lib/casaos/manifest "${BACKUP_DIR}/manifest"
    fi

    # Binary versions
    for bin in "${CASAOS_BINARIES[@]}"; do
        if [ -x "/usr/bin/${bin}" ]; then
            echo "${bin}: $("/usr/bin/${bin}" -v 2>/dev/null || echo 'unknown')"
        fi
    done > "${BACKUP_DIR}/binary-versions.txt"

    # Backup metadata
    local total_size
    total_size=$(du -sh "$BACKUP_DIR" | awk '{print $1}')
    cat > "${BACKUP_DIR}/backup-metadata.json" << ENDJSON
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "hostname": "$(hostname)",
  "backup_dir": "${BACKUP_DIR}",
  "total_size": "${total_size}",
  "casanova_version": "${CASANOVA_VERSION}"
}
ENDJSON

    # Save backup path for rollback
    echo "$BACKUP_DIR" > "$LAST_BACKUP_FILE"

    # Validate backup archives are non-zero
    local backup_valid=true
    for archive in "${BACKUP_DIR}"/*.tar.gz; do
        [ -f "$archive" ] || continue
        local asize
        asize=$(stat --printf='%s' "$archive" 2>/dev/null || echo "0")
        if [ "$asize" -eq 0 ]; then
            err "Backup archive is empty: ${archive}"
            backup_valid=false
        fi
    done
    if [ "$backup_valid" = false ]; then
        err "Backup validation failed — aborting migration."
        exit 1
    fi

    info "Backup complete: ${BACKUP_DIR} (${total_size})"
}

###############################################################################
# Phase 3b: Preserve mount points
###############################################################################
preserve_mounts() {
    step "Phase 3b: Preserving disk mount points"

    local mounts_added=0
    local fstab_backup="${BACKUP_DIR}/fstab.backup"

    # Backup current fstab
    cp /etc/fstab "$fstab_backup" 2>/dev/null

    # Find all real disk mounts (exclude tmpfs, overlay, snap, docker, proc, sys)
    while IFS=' ' read -r device mountpoint fstype _options _dump _pass; do
        # Skip non-disk mounts
        [[ "$device" != /dev/* ]] && continue
        [[ "$mountpoint" == "/" ]] && continue
        [[ "$mountpoint" == /boot* ]] && continue
        [[ "$mountpoint" == /snap* ]] && continue
        [[ "$mountpoint" == /var/lib/docker* ]] && continue

        # Check if already in fstab
        if grep -q "$mountpoint" /etc/fstab 2>/dev/null; then
            info "  ${mountpoint} already in fstab"
            continue
        fi

        # Get UUID for stable identification
        local uuid
        uuid=$(blkid -s UUID -o value "$device" 2>/dev/null || echo "")
        if [ -z "$uuid" ]; then
            warn "  ${device} → ${mountpoint}: no UUID found, using device path"
            local fstab_entry="${device} ${mountpoint} ${fstype} defaults,nofail 0 2"
        else
            local fstab_entry="UUID=${uuid} ${mountpoint} ${fstype} defaults,nofail 0 2"
        fi

        if [ "$DRY_RUN" = false ]; then
            echo "$fstab_entry" >> /etc/fstab
            info "  Added to fstab: ${mountpoint} (${device}, ${fstype})"
        else
            info "  [dry-run] Would add: ${fstab_entry}"
        fi
        mounts_added=$((mounts_added + 1))
    done < <(mount | awk '{print $1, $3, $5, $6}' | grep '^/dev/')

    if [ "$mounts_added" -eq 0 ]; then
        info "No additional mounts to preserve"
    else
        info "${mounts_added} mount(s) added to /etc/fstab"
        if [ "$DRY_RUN" = false ]; then
            systemctl daemon-reload 2>/dev/null || true
        fi
    fi
}

###############################################################################
# Phase 4: Stop CasaOS services
###############################################################################
stop_casaos() {
    step "Phase 4/8: Stopping CasaOS services"

    for svc in "${CASAOS_SERVICES_STOP[@]}"; do
        if systemctl is-active "$svc" &>/dev/null; then
            info "Stopping ${svc}..."
            if [ "$DRY_RUN" = false ]; then
                systemctl stop "$svc" 2>/dev/null || true
            fi
        fi
    done

    # Disable old services
    for svc in "${CASAOS_SERVICES_STOP[@]}"; do
        if [ "$DRY_RUN" = false ]; then
            systemctl disable "$svc" 2>/dev/null || true
        fi
    done

    # Also handle casaos-local-storage-first
    if [ "$DRY_RUN" = false ]; then
        systemctl stop casaos-local-storage-first 2>/dev/null || true
        systemctl disable casaos-local-storage-first 2>/dev/null || true
    fi

    info "CasaOS services stopped and disabled"
}

###############################################################################
# Phase 5: Deploy CasaNova
###############################################################################
deploy_casanova() {
    step "Phase 5/8: Deploying CasaNova ${CASANOVA_VERSION}"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would install binaries from ${BUILD_DIR}/bin/"
        info "[dry-run] Would install systemd units from ${BUILD_DIR}/systemd/"
        info "[dry-run] Would install frontend from ${BUILD_DIR}/www/"
        info "[dry-run] Would deploy app library from ${BUILD_DIR}/library/"
        info "[dry-run] Would create /etc/casanova/ with migrated configs"
        return
    fi

    # Install binaries
    info "Installing CasaNova binaries..."
    for bin in "${BUILD_DIR}"/bin/*; do
        local bname
        bname=$(basename "$bin")
        install -m 0755 "$bin" "/usr/bin/${bname}"
        echo "  /usr/bin/${bname} ($(stat --printf='%s' "$bin" | numfmt --to=iec 2>/dev/null || echo '?'))"
    done

    # Install systemd units
    info "Installing systemd units..."
    cp "${BUILD_DIR}"/systemd/casanova*.service /usr/lib/systemd/system/

    # Install frontend
    info "Installing frontend..."
    mkdir -p /var/lib/casanova/www
    rm -rf /var/lib/casanova/www/*
    cp -r "${BUILD_DIR}"/www/* /var/lib/casanova/www/

    # Create required directories
    info "Creating CasaNova directories..."
    mkdir -p /etc/casanova
    mkdir -p /var/lib/casanova/db
    mkdir -p /var/lib/casanova/backups
    mkdir -p /var/log/casanova
    mkdir -p /var/run/casanova
    # Legacy paths (Go code still uses these)
    mkdir -p /var/run/casaos
    mkdir -p /var/lib/casaos/db
    mkdir -p /var/lib/casaos/apps
    mkdir -p /var/lib/casaos/appstore
    mkdir -p /var/log/casaos

    # Deploy app library catalog (store registry + app definitions)
    if [ -d "${BUILD_DIR}/library" ]; then
        info "Deploying app library catalog..."
        mkdir -p /var/lib/casaos/appstore
        cp -r "${BUILD_DIR}"/library/* /var/lib/casaos/appstore/
        echo "  Deployed $(find "${BUILD_DIR}/library/default/Apps" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l) app definitions"
    fi

    # Migrate configs: /etc/casaos/ → /etc/casanova/
    info "Migrating configuration files..."
    if [ -f /etc/casaos/casaos.conf ]; then
        [ ! -f /etc/casanova/casanova.conf ] && cp /etc/casaos/casaos.conf /etc/casanova/casanova.conf
    fi
    if [ -f /etc/casaos/gateway.ini ]; then
        [ ! -f /etc/casanova/gateway.ini ] && cp /etc/casaos/gateway.ini /etc/casanova/gateway.ini
    fi
    for conf in /etc/casaos/*.conf; do
        [ -f "$conf" ] || continue
        local bname
        bname=$(basename "$conf")
        [ "$bname" = "casaos.conf" ] && continue  # already handled above
        [ ! -f "/etc/casanova/${bname}" ] && cp "$conf" "/etc/casanova/${bname}"
    done

    # Also install sample configs if nothing was migrated
    if [ -d "${BUILD_DIR}/config" ]; then
        for sample in "${BUILD_DIR}"/config/*.sample; do
            [ -f "$sample" ] || continue
            local target
            target=$(basename "$sample" .sample)
            # Map casaos.conf.sample → casanova.conf
            [ "$target" = "casaos.conf" ] && target="casanova.conf"
            [ ! -f "/etc/casanova/${target}" ] && cp "$sample" "/etc/casanova/${target}"
        done
    fi

    # Keep /etc/casaos/ populated (gateway viper reads from there)
    for conf in /etc/casanova/*; do
        [ -f "$conf" ] || continue
        local bname
        bname=$(basename "$conf")
        case "$bname" in
            casanova.conf) [ ! -f /etc/casaos/casaos.conf ] && cp "$conf" /etc/casaos/casaos.conf ;;
            *) [ ! -f "/etc/casaos/${bname}" ] && cp "$conf" "/etc/casaos/${bname}" ;;
        esac
    done

    # Copy manifest if present
    if [ -f "${BUILD_DIR}/manifest.json" ]; then
        cp "${BUILD_DIR}/manifest.json" /var/lib/casanova/manifest.json
    fi

    # Reload systemd
    systemctl daemon-reload

    info "CasaNova ${CASANOVA_VERSION} deployed"
}

###############################################################################
# Phase 6: Start CasaNova services
###############################################################################
start_casanova() {
    step "Phase 6/8: Starting CasaNova services"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would start services in order: ${CASANOVA_SERVICES_START[*]}"
        return
    fi

    # Clean stale runtime files
    rm -f /var/run/casaos/*.url /var/run/casaos/management.url 2>/dev/null

    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        info "Starting ${svc}..."
        systemctl enable "$svc" 2>/dev/null || true
        if ! systemctl start "$svc" 2>/dev/null; then
            warn "  ${svc} failed to start on first attempt"
        fi
        sleep 2
    done

    # Retry any failed services
    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        if [ "$(systemctl is-active "$svc" 2>/dev/null)" != "active" ]; then
            info "  Retrying ${svc}..."
            systemctl restart "$svc" 2>/dev/null || true
            sleep 3
        fi
    done

    info "CasaNova services started"
}

###############################################################################
# Phase 7: Verify
###############################################################################
verify_migration() {
    step "Phase 7/8: Verification"

    local gw_port="80"
    if [ -f "${BACKUP_DIR}/.gateway-port" ]; then
        gw_port=$(cat "${BACKUP_DIR}/.gateway-port")
    fi
    [ -z "$gw_port" ] && gw_port="80"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would verify services, gateway on port ${gw_port}, containers"
        return
    fi

    sleep 5  # Let services settle

    # Check services
    local services_ok=0
    local services_total=${#CASANOVA_SERVICES_START[@]}
    for svc in "${CASANOVA_SERVICES_START[@]}"; do
        local state
        state=$(systemctl is-active "$svc" 2>/dev/null || echo "inactive")
        if [ "$state" = "active" ]; then
            services_ok=$((services_ok + 1))
            info "  ${svc}: ${GREEN}active${NC}"
        else
            err "  ${svc}: ${RED}${state}${NC}"
        fi
    done

    # Gateway ping
    local gw_ping=false
    if curl -sf --max-time 10 "http://localhost:${gw_port}/ping" &>/dev/null; then
        gw_ping=true
        info "Gateway ping: OK (port ${gw_port})"
    else
        warn "Gateway ping: FAIL (port ${gw_port})"
    fi

    # Frontend
    local frontend_ok=false
    local http_code
    http_code=$(curl -sf --max-time 10 -o /dev/null -w "%{http_code}" "http://localhost:${gw_port}/" 2>/dev/null || echo "000")
    if [ "$http_code" = "200" ]; then
        frontend_ok=true
        info "Frontend: HTTP 200"
    else
        warn "Frontend: HTTP ${http_code}"
    fi

    # Docker containers — compare with pre-migration
    local containers_before=0
    if [ -f "${BACKUP_DIR}/running-containers.txt" ]; then
        containers_before=$(wc -l < "${BACKUP_DIR}/running-containers.txt")
    fi
    local containers_after
    containers_after=$(docker ps -q 2>/dev/null | wc -l)
    info "Containers: ${containers_before} before → ${containers_after} after"

    # Find missing containers
    local missing=""
    if [ -f "${BACKUP_DIR}/running-containers.txt" ]; then
        while IFS=$'\t' read -r name rest; do
            if ! docker ps --format '{{.Names}}' | grep -qx "$name" 2>/dev/null; then
                missing="${missing}${name} "
            fi
        done < "${BACKUP_DIR}/running-containers.txt"
    fi
    if [ -n "$missing" ]; then
        warn "Missing containers: ${missing}"
    else
        info "All containers accounted for"
    fi

    # Check mounts are still present
    local mount_issues=""
    while IFS= read -r line; do
        local mp
        mp=$(echo "$line" | awk '{print $2}')
        [[ "$mp" == "/" || "$mp" == /boot* || "$mp" == /snap* ]] && continue
        if ! mountpoint -q "$mp" 2>/dev/null; then
            mount_issues="${mount_issues}${mp} "
            warn "Mount missing: ${mp}"
        fi
    done < <(grep '^UUID\|^/dev/' /etc/fstab 2>/dev/null | grep -v '^#')
    if [ -z "$mount_issues" ]; then
        info "All fstab mounts present"
    fi

    # API check
    local api_apps=0
    api_apps=$(api_app_count "${gw_port}")
    info "API: $(apps_for_display "${api_apps}") apps visible"

    # Export verification results for the report
    VERIFY_SERVICES_OK=$services_ok
    VERIFY_SERVICES_TOTAL=$services_total
    VERIFY_GW_PING=$gw_ping
    VERIFY_GW_PORT=$gw_port
    VERIFY_FRONTEND=$frontend_ok
    VERIFY_CONTAINERS_BEFORE=$containers_before
    VERIFY_CONTAINERS_AFTER=$containers_after
    VERIFY_MISSING="$missing"
    VERIFY_API_APPS=$api_apps

    if [ "$services_ok" -lt 5 ] || [ "$gw_ping" = false ]; then
        echo ""
        err "Migration verification FAILED. Run --rollback to restore CasaOS."
        VERIFY_STATUS="failed"
    elif [ -n "$missing" ] || [ "$services_ok" -lt "$services_total" ]; then
        VERIFY_STATUS="partial"
        warn "Migration completed with warnings."
    else
        VERIFY_STATUS="success"
        info "Migration verification PASSED"
    fi
}

###############################################################################
# Phase 8: Post-migration report
###############################################################################
generate_report() {
    step "Phase 8/8: Post-migration Report"

    if [ "$DRY_RUN" = true ]; then
        info "[dry-run] Would generate report in ${BACKUP_DIR}/"
        return
    fi

    local end_time
    end_time=$(date +%s)
    local duration=$((end_time - START_TIME))

    # JSON report
    cat > "${BACKUP_DIR}/post-migration-report.json" << ENDJSON
{
  "migration_id": "${TIMESTAMP}",
  "status": "${VERIFY_STATUS:-unknown}",
  "casaos_version": "$(cat "${BACKUP_DIR}/binary-versions.txt" 2>/dev/null | head -1 | cut -d: -f2 | xargs || echo 'unknown')",
  "casanova_version": "${CASANOVA_VERSION}",
  "backup_location": "${BACKUP_DIR}",
  "services": {
$(for svc in "${CASANOVA_SERVICES_START[@]}"; do
    echo "    \"${svc}\": \"$(systemctl is-active "$svc" 2>/dev/null || echo 'unknown')\","
done | sed '$ s/,$//')
  },
  "containers": {
    "before": ${VERIFY_CONTAINERS_BEFORE:-0},
    "after": ${VERIFY_CONTAINERS_AFTER:-0},
    "missing": "$(echo "${VERIFY_MISSING:-}" | xargs)"
  },
  "gateway_port": ${VERIFY_GW_PORT:-80},
  "gateway_ping": ${VERIFY_GW_PING:-false},
  "frontend_ok": ${VERIFY_FRONTEND:-false},
  "api_apps_count": ${VERIFY_API_APPS:-0},
  "rollback_available": true,
  "duration_seconds": ${duration}
}
ENDJSON

    # Human-readable report
    cat > "${BACKUP_DIR}/post-migration-report.txt" << EOF
===============================================================================
  CasaOS → CasaNova Migration Report
  Date: $(date)
  Duration: ${duration} seconds
  Status: ${VERIFY_STATUS:-unknown}
===============================================================================

  CasaNova Version : ${CASANOVA_VERSION}
  Backup Location  : ${BACKUP_DIR}

  Services:
$(for svc in "${CASANOVA_SERVICES_START[@]}"; do
    local state
    state=$(systemctl is-active "$svc" 2>/dev/null || echo "unknown")
    if [ "$state" = "active" ]; then
        echo "    [OK]   ${svc}"
    else
        echo "    [FAIL] ${svc}: ${state}"
    fi
done)

  Containers:
    Before migration : ${VERIFY_CONTAINERS_BEFORE:-0}
    After migration  : ${VERIFY_CONTAINERS_AFTER:-0}
    Missing          : ${VERIFY_MISSING:-none}

  Gateway:
    Port     : ${VERIFY_GW_PORT:-80}
    Ping     : ${VERIFY_GW_PING:-false}
    Frontend : ${VERIFY_FRONTEND:-false}
    API apps : ${VERIFY_API_APPS:-0}

  Next steps:
    - Access CasaNova at http://$(hostname -I 2>/dev/null | awk '{print $1}'):${VERIFY_GW_PORT:-80}
    - Review this report and verify your apps work correctly
    - When satisfied, run:  sudo bash $0 --remove-legacy
    - If issues found, run: sudo bash $0 --rollback

  Rollback command: sudo bash $0 --rollback
===============================================================================
EOF

    info "Reports saved:"
    info "  JSON: ${BACKUP_DIR}/post-migration-report.json"
    info "  Text: ${BACKUP_DIR}/post-migration-report.txt"

    echo ""
    cat "${BACKUP_DIR}/post-migration-report.txt"
}

###############################################################################
# Main (migration flow)
###############################################################################
pre_flight_checks
if [ "$SKIP_AUDIT" = true ]; then
    info "Skipping pre-migration audit (--skip-audit)"
    mkdir -p "$BACKUP_DIR"
else
    audit_pre_migration
fi
backup_casaos
preserve_mounts

# From here we tear down CasaOS (stop_casaos disables its services). ARM
# auto-rollback: any failure through deploy/start/verify restores CasaOS from
# the backup instead of leaving the box with neither CasaOS nor a working
# CasaNova (a bricked management plane). rollback() is defined above and
# re-enables + starts the casaos-* services. Skipped under --dry-run since
# nothing is actually torn down.
if [ "$DRY_RUN" = false ]; then
    trap 'ec=$?; trap - ERR; set +e; err "Migration failed (exit ${ec}) after CasaOS teardown — auto-rolling back to CasaOS"; rollback; exit "${ec}"' ERR
fi
stop_casaos
deploy_casanova
start_casanova
verify_migration
trap - ERR   # migration succeeded — disarm auto-rollback
generate_report

if [ "$DRY_RUN" = true ]; then
    echo ""
    info "DRY RUN complete. No changes were made."
    info "Run without --dry-run to perform the actual migration."
fi
