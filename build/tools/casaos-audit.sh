#!/usr/bin/env bash
###############################################################################
# CasaOS Full Server Audit — Migration Planning Tool
# LucentOpen Software · CasaNova Migration Project
#
# Produces a comprehensive audit of a running CasaOS server for migration
# planning. Outputs machine-readable JSON + human-readable text report.
#
# Usage:  sudo bash casaos-audit.sh
# Output: /tmp/casaos-audit-YYYYMMDD-HHMMSS/  (+  .tar.gz archive)
###############################################################################
set -euo pipefail
IFS=$'\n\t'

# ─── Constants ───────────────────────────────────────────────────────────────
AUDIT_VERSION="1.0.0"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
OUTDIR="/tmp/casaos-audit-${TIMESTAMP}"
REPORT="${OUTDIR}/report.txt"
JSON="${OUTDIR}/audit.json"

CASAOS_CONF_DIR="/etc/casaos"
CASAOS_DATA_DIR="/var/lib/casaos"
CASAOS_LOG_DIR="/var/log/casaos"
CASAOS_RUN_DIR="/var/run/casaos"
CASAOS_SHARE_DIR="/usr/share/casaos"

SERVICES=(
    casaos-gateway
    casaos-message-bus
    casaos-user-service
    casaos-local-storage
    casaos-app-management
    casaos-job-management
    casaos
)

BINARIES=(
    casaos
    casaos-gateway
    casaos-user-service
    casaos-local-storage
    casaos-app-management
    casaos-message-bus
    casaos-job-management
    casaos-cli
)

# ─── Colours / formatting ───────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

# ─── Utility helpers ────────────────────────────────────────────────────────

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
err()   { echo -e "${RED}[-]${NC} $*"; }
head1() { echo -e "\n${BOLD}${CYAN}═══════════════════════════════════════════════════════════════${NC}"; echo -e "${BOLD}${CYAN}  $*${NC}"; echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════════${NC}"; }
head2() { echo -e "\n${BOLD}  ── $* ──${NC}"; }

# Run a command, return output or fallback string on failure
safe_cmd() {
    local result
    if result=$("$@" 2>/dev/null); then
        echo "$result"
    else
        echo "(not available)"
    fi
}

# Append to report.txt
rpt() { echo "$@" >> "$REPORT"; }
rpt_section() {
    rpt ""
    rpt "==============================================================================="
    rpt "  $*"
    rpt "==============================================================================="
    rpt ""
}
rpt_sub() { rpt "  --- $* ---"; rpt ""; }

# Dump an sqlite3 database: tables list + each table as JSON
db_dump_json() {
    local dbfile="$1" outkey="$2"
    if [ ! -f "$dbfile" ]; then
        echo "null"
        return
    fi
    local tables
    tables=$(sqlite3 "$dbfile" ".tables" 2>/dev/null | tr -s ' \t' '\n' | sort | grep -v '^$') || true
    if [ -z "$tables" ]; then
        echo "{}"
        return
    fi
    local result="{"
    local first=true
    while IFS= read -r tbl; do
        [ -z "$tbl" ] && continue
        if [ "$first" = true ]; then first=false; else result+=","; fi
        local rows
        rows=$(sqlite3 -json "$dbfile" "SELECT * FROM \"${tbl}\";" 2>/dev/null) || rows="[]"
        [ -z "$rows" ] && rows="[]"
        result+="$(printf '"%s":%s' "$tbl" "$rows")"
    done <<< "$tables"
    result+="}"
    echo "$result"
}

# ─── Pre-flight ─────────────────────────────────────────────────────────────

if [ "$(id -u)" -ne 0 ]; then
    err "This script must be run as root.  Use:  sudo bash $0"
    exit 1
fi

head1 "CasaOS Server Audit v${AUDIT_VERSION}"
info "Timestamp : ${TIMESTAMP}"
info "Output    : ${OUTDIR}/"

mkdir -p "${OUTDIR}"/{db,config,docker,systemd,logs,json,storage,network,appstore}

# Install dependencies if missing
for pkg in jq sqlite3; do
    if ! command -v "$pkg" &>/dev/null; then
        warn "$pkg not found — installing..."
        apt-get update -qq && apt-get install -y -qq "$pkg" >/dev/null 2>&1 || warn "Failed to install $pkg — some sections may be incomplete"
    fi
done

# Start the JSON output
echo '{}' > "$JSON"

# Helper: merge a key into audit.json
json_merge() {
    local key="$1" value="$2"
    local tmp="${JSON}.tmp"
    jq --arg k "$key" --argjson v "$value" '. + {($k): $v}' "$JSON" > "$tmp" && mv "$tmp" "$JSON"
}

# Start report
cat > "$REPORT" << EOF
===============================================================================
  CasaOS Full Server Audit Report
  Generated: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
  Audit Version: ${AUDIT_VERSION}
  Hostname: $(hostname)
===============================================================================
EOF

###############################################################################
# 1. SYSTEM INFORMATION
###############################################################################
audit_system() {
    head1 "1/21  System Information"
    rpt_section "1. SYSTEM INFORMATION"

    local hostname_val arch kernel uptime_val boot_time tz
    hostname_val=$(hostname)
    arch=$(uname -m)
    kernel=$(uname -r)
    uptime_val=$(uptime -p 2>/dev/null || uptime)
    boot_time=$(who -b 2>/dev/null | awk '{print $3, $4}' || echo "unknown")
    tz=$(timedatectl 2>/dev/null | grep "Time zone" | awk '{print $3}' || cat /etc/timezone 2>/dev/null || echo "unknown")

    local cpu_model cpu_cores ram_total_kb ram_total_mb swap_total_kb swap_total_mb
    cpu_model=$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | xargs || echo "unknown")
    cpu_cores=$(nproc 2>/dev/null || echo "unknown")
    ram_total_kb=$(grep MemTotal /proc/meminfo | awk '{print $2}')
    ram_total_mb=$((ram_total_kb / 1024))
    swap_total_kb=$(grep SwapTotal /proc/meminfo | awk '{print $2}')
    swap_total_mb=$((swap_total_kb / 1024))

    local os_pretty
    os_pretty=$(grep PRETTY_NAME /etc/os-release 2>/dev/null | cut -d'"' -f2 || echo "unknown")

    rpt "  Hostname     : $hostname_val"
    rpt "  OS           : $os_pretty"
    rpt "  Kernel       : $kernel"
    rpt "  Architecture : $arch"
    rpt "  Uptime       : $uptime_val"
    rpt "  Boot time    : $boot_time"
    rpt "  Timezone     : $tz"
    rpt "  CPU          : $cpu_model ($cpu_cores cores)"
    rpt "  RAM          : ${ram_total_mb} MB"
    rpt "  Swap         : ${swap_total_mb} MB"
    rpt ""

    # Disk layout
    rpt_sub "Disk Layout (lsblk)"
    lsblk -f >> "$REPORT" 2>/dev/null || rpt "  (lsblk not available)"
    rpt ""
    rpt_sub "Filesystem Usage (df -h)"
    df -h >> "$REPORT" 2>/dev/null
    rpt ""
    rpt_sub "/etc/fstab"
    cat /etc/fstab >> "$REPORT" 2>/dev/null || rpt "  (not readable)"

    # JSON
    local lsblk_json
    lsblk_json=$(lsblk -Jf 2>/dev/null || echo '{}')
    json_merge "system" "$(jq -n \
        --arg hostname "$hostname_val" \
        --arg os "$os_pretty" \
        --arg kernel "$kernel" \
        --arg arch "$arch" \
        --arg uptime "$uptime_val" \
        --arg boot_time "$boot_time" \
        --arg timezone "$tz" \
        --arg cpu_model "$cpu_model" \
        --arg cpu_cores "$cpu_cores" \
        --argjson ram_mb "$ram_total_mb" \
        --argjson swap_mb "$swap_total_mb" \
        --argjson lsblk "$lsblk_json" \
        '{hostname:$hostname, os:$os, kernel:$kernel, arch:$arch, uptime:$uptime,
          boot_time:$boot_time, timezone:$timezone, cpu_model:$cpu_model,
          cpu_cores:$cpu_cores, ram_mb:$ram_mb, swap_mb:$swap_mb, lsblk:$lsblk}'
    )"
    info "System information collected"
}

###############################################################################
# 2. CASAOS INSTALLATION METADATA
###############################################################################
audit_casaos_install() {
    head1 "2/21  CasaOS Installation Metadata"
    rpt_section "2. CASAOS INSTALLATION METADATA"

    local binaries_json="{"
    local first=true
    for bin in "${BINARIES[@]}"; do
        local binpath="/usr/bin/${bin}"
        local ver="not installed" size="0"
        if [ -x "$binpath" ]; then
            ver=$("$binpath" -v 2>/dev/null || "$binpath" --version 2>/dev/null || echo "unknown")
            size=$(stat --printf="%s" "$binpath" 2>/dev/null || echo "0")
            rpt "  $bin: $ver ($(numfmt --to=iec "$size" 2>/dev/null || echo "${size}B"))"
        else
            rpt "  $bin: NOT INSTALLED"
        fi
        if [ "$first" = true ]; then first=false; else binaries_json+=","; fi
        binaries_json+="$(printf '"%s":{"version":"%s","size_bytes":%s,"installed":%s}' \
            "$bin" "$ver" "$size" "$([ -x "$binpath" ] && echo true || echo false)")"
    done
    binaries_json+="}"
    rpt ""

    # Manifest file
    local manifest="(not found)"
    if [ -f "${CASAOS_DATA_DIR}/manifest" ]; then
        manifest=$(wc -l < "${CASAOS_DATA_DIR}/manifest")
        cp "${CASAOS_DATA_DIR}/manifest" "${OUTDIR}/config/" 2>/dev/null
        rpt "  Manifest: ${manifest} tracked files"
    else
        rpt "  Manifest: not found"
    fi

    # Baseinfo
    local baseinfo="{}"
    if [ -f "${CASAOS_DATA_DIR}/baseinfo.conf" ]; then
        baseinfo=$(cat "${CASAOS_DATA_DIR}/baseinfo.conf" 2>/dev/null)
        cp "${CASAOS_DATA_DIR}/baseinfo.conf" "${OUTDIR}/config/"
        rpt "  Base info: found"
    fi

    # Migration status
    local migration_status="none"
    if [ -f "${CASAOS_DATA_DIR}/migration/app-management.status" ]; then
        migration_status=$(cat "${CASAOS_DATA_DIR}/migration/app-management.status" 2>/dev/null || echo "unreadable")
        rpt "  Migration status: ${migration_status}"
    fi

    json_merge "casaos_install" "$(jq -n \
        --argjson binaries "$binaries_json" \
        --arg manifest_lines "$manifest" \
        --arg migration_status "$migration_status" \
        '{binaries:$binaries, manifest_lines:$manifest_lines, migration_status:$migration_status}'
    )"
    info "Installation metadata collected"
}

###############################################################################
# 3. CASAOS CONFIGURATION FILES
###############################################################################
audit_config() {
    head1 "3/21  CasaOS Configuration Files"
    rpt_section "3. CASAOS CONFIGURATION FILES"

    if [ -d "$CASAOS_CONF_DIR" ]; then
        cp -r "$CASAOS_CONF_DIR"/* "${OUTDIR}/config/" 2>/dev/null || true
        rpt "  Config directory: ${CASAOS_CONF_DIR}"
        rpt "  Files:"
        ls -la "$CASAOS_CONF_DIR"/ >> "$REPORT" 2>/dev/null
        rpt ""

        # Parse key settings
        local gateway_port="unknown"
        if [ -f "${CASAOS_CONF_DIR}/gateway.ini" ]; then
            gateway_port=$(grep -i 'port' "${CASAOS_CONF_DIR}/gateway.ini" 2>/dev/null | head -1 | grep -oP '\d+' || echo "80")
            rpt_sub "Gateway Config (gateway.ini)"
            cat "${CASAOS_CONF_DIR}/gateway.ini" >> "$REPORT" 2>/dev/null
            rpt ""
        fi

        local appstore_urls="[]"
        if [ -f "${CASAOS_CONF_DIR}/app-management.conf" ]; then
            appstore_urls=$(grep -i 'appstore' "${CASAOS_CONF_DIR}/app-management.conf" 2>/dev/null | grep -oP 'https?://[^\s,]+' | jq -R . | jq -s . 2>/dev/null || echo '[]')
            rpt_sub "App Management Config"
            cat "${CASAOS_CONF_DIR}/app-management.conf" >> "$REPORT" 2>/dev/null
            rpt ""
        fi

        local usb_automount="unknown" mergerfs_enabled="unknown"
        if [ -f "${CASAOS_CONF_DIR}/local-storage.conf" ]; then
            usb_automount=$(grep -i 'usb_auto_mount' "${CASAOS_CONF_DIR}/local-storage.conf" 2>/dev/null | cut -d= -f2 | xargs || echo "unknown")
            mergerfs_enabled=$(grep -i 'enable_mergerfs\|mergerfs' "${CASAOS_CONF_DIR}/local-storage.conf" 2>/dev/null | cut -d= -f2 | xargs || echo "unknown")
            rpt_sub "Local Storage Config"
            cat "${CASAOS_CONF_DIR}/local-storage.conf" >> "$REPORT" 2>/dev/null
            rpt ""
        fi

        # Dump remaining config files
        for conf in "${CASAOS_CONF_DIR}"/*.conf "${CASAOS_CONF_DIR}"/*.ini; do
            [ -f "$conf" ] || continue
            local bname
            bname=$(basename "$conf")
            [[ "$bname" == "gateway.ini" || "$bname" == "app-management.conf" || "$bname" == "local-storage.conf" ]] && continue
            rpt_sub "$bname"
            cat "$conf" >> "$REPORT" 2>/dev/null
            rpt ""
        done

        # Env file
        if [ -f "${CASAOS_CONF_DIR}/env" ]; then
            rpt_sub "Environment File (/etc/casaos/env)"
            rpt "  (keys only — values redacted for security)"
            grep -oP '^[A-Za-z_][A-Za-z0-9_]*' "${CASAOS_CONF_DIR}/env" >> "$REPORT" 2>/dev/null
            # Copy but redact values
            sed 's/=.*/=<REDACTED>/' "${CASAOS_CONF_DIR}/env" > "${OUTDIR}/config/env.redacted" 2>/dev/null
            rpt ""
        fi

        json_merge "config" "$(jq -n \
            --arg gateway_port "$gateway_port" \
            --argjson appstore_urls "$appstore_urls" \
            --arg usb_automount "$usb_automount" \
            --arg mergerfs_enabled "$mergerfs_enabled" \
            --arg conf_dir "$CASAOS_CONF_DIR" \
            '{gateway_port:$gateway_port, appstore_urls:$appstore_urls,
              usb_automount:$usb_automount, mergerfs_enabled:$mergerfs_enabled,
              config_dir:$conf_dir}'
        )"
    else
        rpt "  WARNING: ${CASAOS_CONF_DIR} does not exist!"
        json_merge "config" '{"error":"config directory not found"}'
    fi
    info "Configuration files collected"
}

###############################################################################
# 4. SYSTEMD SERVICES
###############################################################################
audit_systemd() {
    head1 "4/21  Systemd Services"
    rpt_section "4. SYSTEMD SERVICES"

    local svc_json="["
    local first=true
    for svc in "${SERVICES[@]}"; do
        local enabled active loaded unit_file=""
        enabled=$(systemctl is-enabled "$svc" 2>/dev/null || echo "not-found")
        active=$(systemctl is-active "$svc" 2>/dev/null || echo "not-found")
        loaded=$(systemctl show "$svc" --property=LoadState 2>/dev/null | cut -d= -f2 || echo "not-found")

        rpt "  ${svc}:"
        rpt "    Loaded  : $loaded"
        rpt "    Enabled : $enabled"
        rpt "    Active  : $active"

        # Copy unit file
        for upath in /usr/lib/systemd/system/${svc}.service /etc/systemd/system/${svc}.service /lib/systemd/system/${svc}.service; do
            if [ -f "$upath" ]; then
                cp "$upath" "${OUTDIR}/systemd/" 2>/dev/null
                unit_file="$upath"
                break
            fi
        done

        # Recent restarts
        local restart_count
        restart_count=$(systemctl show "$svc" --property=NRestarts 2>/dev/null | cut -d= -f2 || echo "0")
        rpt "    Restarts: $restart_count"
        rpt ""

        if [ "$first" = true ]; then first=false; else svc_json+=","; fi
        svc_json+="$(jq -n \
            --arg name "$svc" --arg enabled "$enabled" --arg active "$active" \
            --arg loaded "$loaded" --arg unit_file "$unit_file" --arg restarts "$restart_count" \
            '{name:$name, enabled:$enabled, active:$active, loaded:$loaded, unit_file:$unit_file, restarts:$restarts}'
        )"
    done
    svc_json+="]"

    # Check for any other casaos-related units
    rpt_sub "All casaos-related systemd units"
    systemctl list-units --type=service --all --no-pager 2>/dev/null | grep -i casaos >> "$REPORT" || rpt "  (none found)"
    rpt ""

    # Failed services
    rpt_sub "Failed services (system-wide)"
    systemctl --failed --no-pager >> "$REPORT" 2>/dev/null || true
    rpt ""

    json_merge "systemd_services" "$svc_json"
    info "Systemd services audited"
}

###############################################################################
# 5. SQLITE DATABASES
###############################################################################
audit_databases() {
    head1 "5/21  SQLite Databases"
    rpt_section "5. SQLITE DATABASES"

    local db_files=(
        "${CASAOS_DATA_DIR}/db/casaOS.db"
        "${CASAOS_DATA_DIR}/db/user.db"
        "${CASAOS_DATA_DIR}/db/local-storage.db"
        "${CASAOS_DATA_DIR}/db/message-bus.db"
        "${CASAOS_RUN_DIR}/message-bus.db"
    )

    local db_json="{"
    local first=true

    for dbf in "${db_files[@]}"; do
        local bname
        bname=$(basename "$dbf")
        local key="${bname%.db}"
        key=$(echo "$key" | tr '.-' '_')

        if [ -f "$dbf" ]; then
            # Copy the database
            cp "$dbf" "${OUTDIR}/db/" 2>/dev/null
            local size
            size=$(stat --printf="%s" "$dbf" 2>/dev/null || echo "0")
            rpt "  $dbf: $(numfmt --to=iec "$size" 2>/dev/null || echo "${size}B")"

            # List tables
            local tables
            tables=$(sqlite3 "$dbf" ".tables" 2>/dev/null || echo "(error reading)")
            rpt "    Tables: $tables"

            # Row counts
            for tbl in $tables; do
                [ -z "$tbl" ] && continue
                local cnt
                cnt=$(sqlite3 "$dbf" "SELECT COUNT(*) FROM \"${tbl}\";" 2>/dev/null || echo "?")
                rpt "    - ${tbl}: ${cnt} rows"
            done
            rpt ""

            # Full JSON dump
            local dump
            dump=$(db_dump_json "$dbf" "$key")
            if [ "$first" = true ]; then first=false; else db_json+=","; fi
            db_json+="$(printf '"%s":{"file":"%s","size_bytes":%s,"tables":%s}' "$key" "$dbf" "$size" "$dump")"
        else
            rpt "  $dbf: NOT FOUND"
            rpt ""
            if [ "$first" = true ]; then first=false; else db_json+=","; fi
            db_json+="$(printf '"%s":null' "$key")"
        fi
    done
    db_json+="}"

    json_merge "databases" "$db_json"
    info "Databases dumped"
}

###############################################################################
# 6. JSON-PERSISTED STATE FILES
###############################################################################
audit_json_state() {
    head1 "6/21  JSON-Persisted State Files"
    rpt_section "6. JSON-PERSISTED STATE FILES"

    local known_json_files=(
        "${CASAOS_DATA_DIR}/app-management/schedules.json"
        "${CASAOS_DATA_DIR}/job-management/tasks.json"
        "${CASAOS_DATA_DIR}/gateway/routes.json"
        "${CASAOS_DATA_DIR}/user-service/oauth2_clients.json"
    )

    local state_json="{"
    local first=true

    for jf in "${known_json_files[@]}"; do
        local bname key
        bname=$(basename "$jf")
        key=$(echo "${bname%.json}" | tr '.-' '_')

        if [ -f "$jf" ]; then
            cp "$jf" "${OUTDIR}/json/" 2>/dev/null
            local content
            content=$(jq . "$jf" 2>/dev/null || cat "$jf")
            rpt "  $jf: found"
            rpt "$(echo "$content" | head -30 | sed 's/^/    /')"
            rpt ""
            if [ "$first" = true ]; then first=false; else state_json+=","; fi
            state_json+="$(printf '"%s":%s' "$key" "$(jq -c . "$jf" 2>/dev/null || echo 'null')")"
        else
            rpt "  $jf: not found"
            if [ "$first" = true ]; then first=false; else state_json+=","; fi
            state_json+="$(printf '"%s":null' "$key")"
        fi
    done

    # Discover any other JSON files under /var/lib/casaos
    rpt_sub "Other JSON files discovered under ${CASAOS_DATA_DIR}"
    local others
    others=$(find "${CASAOS_DATA_DIR}" -name "*.json" -type f 2>/dev/null | sort)
    if [ -n "$others" ]; then
        while IFS= read -r f; do
            # Skip already-collected ones
            local skip=false
            for kf in "${known_json_files[@]}"; do
                [ "$f" = "$kf" ] && skip=true && break
            done
            [ "$skip" = true ] && continue
            rpt "  $f ($(stat --printf='%s' "$f" 2>/dev/null || echo '?') bytes)"
            cp "$f" "${OUTDIR}/json/$(echo "$f" | tr '/' '_')" 2>/dev/null
        done <<< "$others"
    else
        rpt "  (none)"
    fi
    rpt ""

    state_json+="}"
    json_merge "json_state" "$state_json"
    info "JSON state files collected"
}

###############################################################################
# 7. USER ACCOUNTS & AUTH
###############################################################################
audit_users() {
    head1 "7/21  User Accounts & Auth"
    rpt_section "7. USER ACCOUNTS & AUTH"

    local users_json="[]"

    # Extract from user.db (WITHOUT passwords)
    local userdb="${CASAOS_DATA_DIR}/db/user.db"
    if [ -f "$userdb" ] && command -v sqlite3 &>/dev/null; then
        rpt_sub "CasaOS Users (from user.db)"
        users_json=$(sqlite3 -json "$userdb" "SELECT id, username, role, email, nickname, description, created_at, updated_at FROM o_users;" 2>/dev/null || echo '[]')
        [ -z "$users_json" ] && users_json="[]"
        echo "$users_json" | jq -r '.[] | "  User: \(.username) | Role: \(.role) | Email: \(.email // "n/a")"' >> "$REPORT" 2>/dev/null || rpt "  (could not parse user records)"
        rpt ""
    else
        rpt "  User database not found"
    fi

    # OAuth2 clients
    local oauth_json="[]"
    local oauth_file="${CASAOS_DATA_DIR}/user-service/oauth2_clients.json"
    if [ -f "$oauth_file" ]; then
        rpt_sub "OAuth2 Clients"
        oauth_json=$(jq '[.[] | {id, name, redirect_uris, allowed_scopes, created}]' "$oauth_file" 2>/dev/null || echo '[]')
        echo "$oauth_json" | jq -r '.[] | "  Client: \(.name) | ID: \(.id) | Scopes: \(.allowed_scopes // [] | join(", "))"' >> "$REPORT" 2>/dev/null
        rpt ""
    fi

    # Per-user data dirs
    rpt_sub "Per-user data directories"
    if [ -d "${CASAOS_DATA_DIR}" ]; then
        find "${CASAOS_DATA_DIR}" -maxdepth 1 -type d | while read -r d; do
            local dname
            dname=$(basename "$d")
            # User dirs are typically numeric IDs
            if [[ "$dname" =~ ^[0-9]+$ ]]; then
                rpt "  User dir: $d ($(du -sh "$d" 2>/dev/null | awk '{print $1}'))"
            fi
        done
    fi
    rpt ""

    json_merge "users" "$(jq -n --argjson users "$users_json" --argjson oauth "$oauth_json" '{users:$users, oauth2_clients:$oauth}')"
    info "User accounts audited"
}

###############################################################################
# 8. DOCKER ENVIRONMENT
###############################################################################
audit_docker_env() {
    head1 "8/21  Docker Environment"
    rpt_section "8. DOCKER ENVIRONMENT"

    if ! command -v docker &>/dev/null; then
        rpt "  Docker is NOT installed!"
        json_merge "docker_env" '{"installed":false}'
        warn "Docker not found"
        return
    fi

    local docker_ver docker_root storage_driver
    docker_ver=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo "unknown")
    docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo "unknown")
    storage_driver=$(docker info --format '{{.Driver}}' 2>/dev/null || echo "unknown")

    rpt "  Docker Version   : $docker_ver"
    rpt "  Root Directory   : $docker_root"
    rpt "  Storage Driver   : $storage_driver"
    rpt ""

    # Docker daemon.json
    rpt_sub "Docker Daemon Config (/etc/docker/daemon.json)"
    if [ -f /etc/docker/daemon.json ]; then
        jq . /etc/docker/daemon.json >> "$REPORT" 2>/dev/null || cat /etc/docker/daemon.json >> "$REPORT"
        cp /etc/docker/daemon.json "${OUTDIR}/docker/" 2>/dev/null
    else
        rpt "  (not found — using defaults)"
    fi
    rpt ""

    # Docker system info summary
    rpt_sub "Docker System Summary"
    local containers_total containers_running images_count volumes_count
    containers_total=$(docker info --format '{{.Containers}}' 2>/dev/null || echo "0")
    containers_running=$(docker info --format '{{.ContainersRunning}}' 2>/dev/null || echo "0")
    images_count=$(docker images -q 2>/dev/null | wc -l)
    volumes_count=$(docker volume ls -q 2>/dev/null | wc -l)

    rpt "  Containers total   : $containers_total"
    rpt "  Containers running : $containers_running"
    rpt "  Images             : $images_count"
    rpt "  Volumes            : $volumes_count"
    rpt ""

    # Disk usage
    rpt_sub "Docker Disk Usage"
    docker system df 2>/dev/null >> "$REPORT" || rpt "  (unavailable)"
    rpt ""

    json_merge "docker_env" "$(jq -n \
        --arg ver "$docker_ver" --arg root "$docker_root" --arg driver "$storage_driver" \
        --arg containers "$containers_total" --arg running "$containers_running" \
        --arg images "$images_count" --arg volumes "$volumes_count" \
        '{installed:true, version:$ver, root_dir:$root, storage_driver:$driver,
          containers_total:($containers|tonumber), containers_running:($running|tonumber),
          images:($images|tonumber), volumes:($volumes|tonumber)}'
    )"
    info "Docker environment audited"
}

###############################################################################
# 9. DOCKER CONTAINERS (complete inventory)
###############################################################################
audit_containers() {
    head1 "9/21  Docker Containers"
    rpt_section "9. DOCKER CONTAINERS (COMPLETE INVENTORY)"

    if ! command -v docker &>/dev/null; then
        json_merge "containers" '[]'
        return
    fi

    local container_ids
    container_ids=$(docker ps -a -q 2>/dev/null)
    if [ -z "$container_ids" ]; then
        rpt "  No containers found."
        json_merge "containers" '[]'
        return
    fi

    # Full inspect dump
    docker inspect $container_ids > "${OUTDIR}/docker/all-containers-inspect.json" 2>/dev/null || true

    # Summary table
    rpt "  $(printf '%-30s %-15s %-40s %-10s' 'NAME' 'STATUS' 'IMAGE' 'PORTS')"
    rpt "  $(printf '%0.s-' {1..95})"

    local containers_json="["
    local first=true

    for cid in $container_ids; do
        local name state image ports labels restart_policy network_mode is_casaos privileged
        name=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null | sed 's|^/||')
        state=$(docker inspect --format '{{.State.Status}}' "$cid" 2>/dev/null || echo "unknown")
        image=$(docker inspect --format '{{.Config.Image}}' "$cid" 2>/dev/null || echo "unknown")
        ports=$(docker inspect --format '{{range $p, $conf := .NetworkSettings.Ports}}{{$p}}->{{range $conf}}{{.HostPort}}{{end}} {{end}}' "$cid" 2>/dev/null || echo "")
        restart_policy=$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$cid" 2>/dev/null || echo "unknown")
        network_mode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$cid" 2>/dev/null || echo "unknown")
        privileged=$(docker inspect --format '{{.HostConfig.Privileged}}' "$cid" 2>/dev/null || echo "false")

        # CasaOS labels
        local compose_project compose_service
        compose_project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$cid" 2>/dev/null || echo "")
        compose_service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$cid" 2>/dev/null || echo "")
        is_casaos="false"
        if [ -n "$compose_project" ]; then
            # CasaOS-managed apps typically have compose project labels
            is_casaos="true"
        fi

        # Volumes
        local vol_count
        vol_count=$(docker inspect --format '{{len .Mounts}}' "$cid" 2>/dev/null || echo "0")

        # Health
        local health
        health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' "$cid" 2>/dev/null || echo "unknown")

        # Created
        local created
        created=$(docker inspect --format '{{.Created}}' "$cid" 2>/dev/null || echo "unknown")

        # Memory limit
        local mem_limit
        mem_limit=$(docker inspect --format '{{.HostConfig.Memory}}' "$cid" 2>/dev/null || echo "0")

        rpt "  $(printf '%-30s %-15s %-40s %-10s' "$name" "$state" "${image:0:40}" "${ports:0:10}")"

        if [ "$first" = true ]; then first=false; else containers_json+=","; fi
        containers_json+="$(jq -n \
            --arg id "$cid" --arg name "$name" --arg state "$state" --arg image "$image" \
            --arg ports "$ports" --arg restart "$restart_policy" --arg netmode "$network_mode" \
            --arg privileged "$privileged" --arg compose_project "$compose_project" \
            --arg compose_service "$compose_service" --arg is_casaos "$is_casaos" \
            --arg vol_count "$vol_count" --arg health "$health" --arg created "$created" \
            --arg mem_limit "$mem_limit" \
            '{id:$id, name:$name, state:$state, image:$image, ports:$ports,
              restart_policy:$restart, network_mode:$netmode, privileged:($privileged=="true"),
              compose_project:$compose_project, compose_service:$compose_service,
              is_casaos:($is_casaos=="true"), volume_count:($vol_count|tonumber),
              health:$health, created:$created, memory_limit:($mem_limit|tonumber)}'
        )"
    done
    containers_json+="]"
    rpt ""

    json_merge "containers" "$containers_json"
    info "Container inventory complete ($(echo "$container_ids" | wc -w) containers)"
}

###############################################################################
# 10. DOCKER COMPOSE STACKS
###############################################################################
audit_compose() {
    head1 "10/21  Docker Compose Stacks"
    rpt_section "10. DOCKER COMPOSE STACKS"

    local apps_dir="${CASAOS_DATA_DIR}/apps"
    local compose_json="["
    local first=true
    local compose_count=0

    # CasaOS apps directory
    if [ -d "$apps_dir" ]; then
        rpt_sub "CasaOS-Managed Apps (${apps_dir})"
        for appdir in "${apps_dir}"/*/; do
            [ -d "$appdir" ] || continue
            local appname
            appname=$(basename "$appdir")
            local compose_file=""
            for cf in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
                if [ -f "${appdir}${cf}" ]; then
                    compose_file="${appdir}${cf}"
                    break
                fi
            done

            if [ -n "$compose_file" ]; then
                compose_count=$((compose_count + 1))
                cp "$compose_file" "${OUTDIR}/docker/${appname}-compose.yml" 2>/dev/null

                # Extract x-casaos metadata
                local has_xcasaos="false"
                if grep -q 'x-casaos' "$compose_file" 2>/dev/null; then
                    has_xcasaos="true"
                fi

                local svc_count
                svc_count=$(grep -c '^\s\+[a-z].*:$\|^  [a-z].*:' "$compose_file" 2>/dev/null || echo "?")

                rpt "  ${appname}: ${compose_file} (x-casaos: ${has_xcasaos})"

                if [ "$first" = true ]; then first=false; else compose_json+=","; fi
                compose_json+="$(jq -n \
                    --arg name "$appname" --arg file "$compose_file" \
                    --arg xcasaos "$has_xcasaos" --arg managed "true" \
                    '{name:$name, compose_file:$file, has_xcasaos:($xcasaos=="true"), casaos_managed:true}'
                )"
            fi
        done
    else
        rpt "  CasaOS apps directory not found: ${apps_dir}"
    fi
    rpt ""
    rpt "  Total compose stacks found: ${compose_count}"
    rpt ""

    # Also check default Docker Compose locations
    rpt_sub "Other Compose files on system"
    local other_compose
    other_compose=$(find /opt /home /root /srv -name "docker-compose.y*ml" -o -name "compose.y*ml" 2>/dev/null | grep -v "${apps_dir}" | head -50)
    if [ -n "$other_compose" ]; then
        while IFS= read -r cf; do
            rpt "  $cf"
            if [ "$first" = true ]; then first=false; else compose_json+=","; fi
            compose_json+="$(jq -n --arg file "$cf" '{name:"external", compose_file:$file, has_xcasaos:false, casaos_managed:false}')"
        done <<< "$other_compose"
    else
        rpt "  (none found outside CasaOS)"
    fi
    rpt ""

    compose_json+="]"
    json_merge "compose_stacks" "$compose_json"
    info "Compose stacks audited (${compose_count} CasaOS-managed)"
}

###############################################################################
# 11. DOCKER IMAGES
###############################################################################
audit_images() {
    head1 "11/21  Docker Images"
    rpt_section "11. DOCKER IMAGES"

    if ! command -v docker &>/dev/null; then
        json_merge "images" '[]'
        return
    fi

    rpt_sub "Image Inventory"
    docker images --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}\t{{.ID}}" >> "$REPORT" 2>/dev/null
    rpt ""

    # JSON dump
    local images_json
    images_json=$(docker images --format '{{json .}}' 2>/dev/null | jq -s '.' 2>/dev/null || echo '[]')

    # Dangling images
    local dangling
    dangling=$(docker images -f "dangling=true" -q 2>/dev/null | wc -l)
    rpt "  Dangling images: ${dangling}"
    rpt ""

    json_merge "images" "$images_json"
    info "Image inventory complete"
}

###############################################################################
# 12. DOCKER VOLUMES & BIND MOUNTS
###############################################################################
audit_volumes() {
    head1 "12/21  Docker Volumes & Bind Mounts"
    rpt_section "12. DOCKER VOLUMES & BIND MOUNTS"

    if ! command -v docker &>/dev/null; then
        json_merge "volumes" '{"docker_volumes":[],"bind_mounts":[]}'
        return
    fi

    # Named volumes
    rpt_sub "Named Volumes"
    local vol_json="["
    local first=true
    local volumes
    volumes=$(docker volume ls -q 2>/dev/null)

    if [ -n "$volumes" ]; then
        while IFS= read -r vol; do
            [ -z "$vol" ] && continue
            local inspect
            inspect=$(docker volume inspect "$vol" 2>/dev/null)
            local mountpoint driver
            mountpoint=$(echo "$inspect" | jq -r '.[0].Mountpoint' 2>/dev/null || echo "unknown")
            driver=$(echo "$inspect" | jq -r '.[0].Driver' 2>/dev/null || echo "unknown")

            # Check if in use
            local used_by
            used_by=$(docker ps -a --filter "volume=${vol}" --format '{{.Names}}' 2>/dev/null | tr '\n' ',' | sed 's/,$//')
            local orphaned="true"
            [ -n "$used_by" ] && orphaned="false"

            rpt "  ${vol}: driver=${driver} mount=${mountpoint} used_by=${used_by:-ORPHANED}"

            if [ "$first" = true ]; then first=false; else vol_json+=","; fi
            vol_json+="$(jq -n --arg name "$vol" --arg mp "$mountpoint" --arg drv "$driver" \
                --arg used_by "$used_by" --arg orphaned "$orphaned" \
                '{name:$name, mountpoint:$mp, driver:$drv, used_by:$used_by, orphaned:($orphaned=="true")}')"
        done <<< "$volumes"
    else
        rpt "  (no named volumes)"
    fi
    vol_json+="]"
    rpt ""

    # Bind mounts from containers
    rpt_sub "Bind Mounts (from container inspects)"
    local bind_json="["
    first=true
    local cids
    cids=$(docker ps -a -q 2>/dev/null)
    if [ -n "$cids" ]; then
        for cid in $cids; do
            local cname
            cname=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null | sed 's|^/||')
            local mounts
            mounts=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{.Source}}->{{.Destination}} {{end}}{{end}}' "$cid" 2>/dev/null)
            if [ -n "$mounts" ]; then
                for m in $mounts; do
                    local src dst
                    src=$(echo "$m" | cut -d'>' -f1 | sed 's/-$//')
                    dst=$(echo "$m" | cut -d'>' -f2)
                    if [ "$first" = true ]; then first=false; else bind_json+=","; fi
                    bind_json+="$(jq -n --arg container "$cname" --arg src "$src" --arg dst "$dst" \
                        '{container:$container, host_path:$src, container_path:$dst}')"
                done
            fi
        done
    fi
    bind_json+="]"

    json_merge "volumes" "$(jq -n --argjson dv "$vol_json" --argjson bm "$bind_json" '{docker_volumes:$dv, bind_mounts:$bm}')"
    info "Volumes and bind mounts audited"
}

###############################################################################
# 13. DOCKER NETWORKS
###############################################################################
audit_networks() {
    head1 "13/21  Docker Networks"
    rpt_section "13. DOCKER NETWORKS"

    if ! command -v docker &>/dev/null; then
        json_merge "docker_networks" '[]'
        return
    fi

    local net_json="["
    local first=true
    local networks
    networks=$(docker network ls --format '{{.ID}}' 2>/dev/null)

    if [ -n "$networks" ]; then
        for nid in $networks; do
            local name driver scope containers_in_net
            name=$(docker network inspect --format '{{.Name}}' "$nid" 2>/dev/null || echo "unknown")
            driver=$(docker network inspect --format '{{.Driver}}' "$nid" 2>/dev/null || echo "unknown")
            scope=$(docker network inspect --format '{{.Scope}}' "$nid" 2>/dev/null || echo "unknown")
            containers_in_net=$(docker network inspect --format '{{range $k, $v := .Containers}}{{$v.Name}} {{end}}' "$nid" 2>/dev/null || echo "")

            rpt "  ${name} (driver: ${driver}, scope: ${scope})"
            [ -n "$containers_in_net" ] && rpt "    Containers: ${containers_in_net}"

            if [ "$first" = true ]; then first=false; else net_json+=","; fi
            net_json+="$(jq -n --arg name "$name" --arg driver "$driver" --arg scope "$scope" \
                --arg containers "$containers_in_net" \
                '{name:$name, driver:$driver, scope:$scope, containers:$containers}')"
        done
    fi
    net_json+="]"
    rpt ""

    # Full network inspect dump
    docker network inspect $(docker network ls -q 2>/dev/null) > "${OUTDIR}/docker/all-networks-inspect.json" 2>/dev/null || true

    json_merge "docker_networks" "$net_json"
    info "Docker networks audited"
}

###############################################################################
# 14. STORAGE & DISKS
###############################################################################
audit_storage() {
    head1 "14/21  Storage & Disks"
    rpt_section "14. STORAGE & DISKS"

    # Full lsblk
    rpt_sub "Block Devices"
    lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,UUID,MODEL,SERIAL,TRAN,RM >> "$REPORT" 2>/dev/null
    rpt ""

    # SMART health
    rpt_sub "SMART Health (per disk)"
    if command -v smartctl &>/dev/null; then
        for disk in /dev/sd? /dev/nvme?n?; do
            [ -b "$disk" ] || continue
            rpt "  ${disk}:"
            local smart_health
            smart_health=$(smartctl -H "$disk" 2>/dev/null | grep -i 'health\|result' || echo "    (SMART not available)")
            rpt "  $smart_health"
            # Temperature
            local temp
            temp=$(smartctl -A "$disk" 2>/dev/null | grep -i temperature | head -1 || echo "")
            [ -n "$temp" ] && rpt "  $temp"
            # Power on hours
            local poh
            poh=$(smartctl -A "$disk" 2>/dev/null | grep -i 'power_on_hours\|Power On Hours' | head -1 || echo "")
            [ -n "$poh" ] && rpt "  $poh"
            rpt ""
        done
    else
        rpt "  smartctl not installed (install smartmontools for disk health)"
    fi

    # MergerFS
    rpt_sub "MergerFS Configuration"
    if mount | grep -q mergerfs 2>/dev/null; then
        rpt "  MergerFS mounts:"
        mount | grep mergerfs >> "$REPORT"
        rpt ""
        grep mergerfs /etc/fstab >> "$REPORT" 2>/dev/null || true
    else
        rpt "  MergerFS not in use"
    fi
    rpt ""

    # /DATA directory
    rpt_sub "/DATA Directory Structure"
    if [ -d /DATA ]; then
        rpt "  Size: $(du -sh /DATA 2>/dev/null | awk '{print $1}')"
        find /DATA -maxdepth 2 -type d 2>/dev/null | head -50 | sed 's/^/  /' >> "$REPORT"
    else
        rpt "  /DATA does not exist"
    fi
    rpt ""

    # USB drives
    rpt_sub "USB Storage Devices"
    lsblk -o NAME,SIZE,TYPE,TRAN,MOUNTPOINT 2>/dev/null | grep -i usb >> "$REPORT" || rpt "  (no USB storage detected)"
    rpt ""

    # Save lsblk JSON for structured data
    local lsblk_full
    lsblk_full=$(lsblk -Jo NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,UUID,MODEL,SERIAL,TRAN,RM,RO 2>/dev/null || echo '{}')
    json_merge "storage" "$lsblk_full"
    info "Storage and disks audited"
}

###############################################################################
# 15. NETWORK CONFIGURATION
###############################################################################
audit_network() {
    head1 "15/21  Network Configuration"
    rpt_section "15. NETWORK CONFIGURATION"

    # IP addresses
    rpt_sub "Network Interfaces"
    ip -br addr >> "$REPORT" 2>/dev/null || ifconfig >> "$REPORT" 2>/dev/null
    rpt ""

    # Default gateway
    rpt_sub "Default Gateway"
    ip route | grep default >> "$REPORT" 2>/dev/null
    rpt ""

    # DNS
    rpt_sub "DNS Configuration"
    if [ -f /etc/resolv.conf ]; then
        cat /etc/resolv.conf >> "$REPORT"
    fi
    resolvectl status >> "${OUTDIR}/network/resolvectl.txt" 2>/dev/null || true
    rpt ""

    # Listening ports
    rpt_sub "Listening Ports"
    ss -tlnp >> "$REPORT" 2>/dev/null || netstat -tlnp >> "$REPORT" 2>/dev/null
    rpt ""
    ss -tlnp > "${OUTDIR}/network/listening-ports.txt" 2>/dev/null || true

    # Firewall
    rpt_sub "Firewall Rules"
    if command -v ufw &>/dev/null; then
        ufw status verbose >> "$REPORT" 2>/dev/null || rpt "  UFW: inactive or not accessible"
    else
        rpt "  UFW not installed"
    fi
    rpt ""
    rpt_sub "iptables rules"
    iptables -L -n --line-numbers >> "$REPORT" 2>/dev/null || rpt "  (not accessible)"
    rpt ""

    # Gateway routes
    rpt_sub "CasaOS Gateway Reverse Proxy Routes"
    local routes_file="${CASAOS_DATA_DIR}/gateway/routes.json"
    if [ -f "$routes_file" ]; then
        jq . "$routes_file" >> "$REPORT" 2>/dev/null || cat "$routes_file" >> "$REPORT"
    else
        rpt "  (routes.json not found)"
    fi
    rpt ""

    # TLS certs
    rpt_sub "TLS Certificates"
    local certs_found=false
    for certdir in "${CASAOS_DATA_DIR}" "${CASAOS_CONF_DIR}" /etc/ssl/private /etc/letsencrypt; do
        if [ -d "$certdir" ]; then
            local pems
            pems=$(find "$certdir" -name "*.pem" -o -name "*.crt" -o -name "*.key" 2>/dev/null | head -20)
            if [ -n "$pems" ]; then
                certs_found=true
                rpt "  Certificates in ${certdir}:"
                echo "$pems" | sed 's/^/    /' >> "$REPORT"
            fi
        fi
    done
    [ "$certs_found" = false ] && rpt "  No TLS certificates found in CasaOS paths"
    rpt ""

    # JSON — listening ports + interfaces
    local interfaces_json
    interfaces_json=$(ip -j addr 2>/dev/null || echo '[]')
    local listening_json
    listening_json=$(ss -tlnpH 2>/dev/null | awk '{print "{\"listen\":\""$4"\",\"process\":\""$6"\"}"}' | jq -s '.' 2>/dev/null || echo '[]')
    json_merge "network" "$(jq -n --argjson ifaces "$interfaces_json" --argjson listen "$listening_json" '{interfaces:$ifaces, listening:$listen}')"
    info "Network configuration audited"
}

###############################################################################
# 16. APP STORE CONFIGURATION
###############################################################################
audit_appstore() {
    head1 "16/21  App Store Configuration"
    rpt_section "16. APP STORE CONFIGURATION"

    local appstore_dir="${CASAOS_DATA_DIR}/appstore"

    # Configured URLs (from config)
    rpt_sub "Configured App Store URLs"
    if [ -f "${CASAOS_CONF_DIR}/app-management.conf" ]; then
        grep -i 'appstore\|store' "${CASAOS_CONF_DIR}/app-management.conf" >> "$REPORT" 2>/dev/null || rpt "  (no store URLs found in config)"
    fi
    rpt ""

    # Local cache
    rpt_sub "App Store Cache"
    if [ -d "$appstore_dir" ]; then
        rpt "  Cache directory: ${appstore_dir}"
        rpt "  Size: $(du -sh "$appstore_dir" 2>/dev/null | awk '{print $1}')"
        ls -la "$appstore_dir"/ >> "$REPORT" 2>/dev/null
        # Copy category list if present
        for f in "${appstore_dir}"/category-list.json "${appstore_dir}"/recommend-list.json "${appstore_dir}"/featured-apps.json; do
            [ -f "$f" ] && cp "$f" "${OUTDIR}/appstore/" 2>/dev/null
        done
    else
        rpt "  App store cache not found: ${appstore_dir}"
    fi
    rpt ""

    # Installed apps catalog
    rpt_sub "Installed Applications"
    local apps_dir="${CASAOS_DATA_DIR}/apps"
    local app_list="[]"
    if [ -d "$apps_dir" ]; then
        local apps=()
        for appdir in "${apps_dir}"/*/; do
            [ -d "$appdir" ] || continue
            local appname
            appname=$(basename "$appdir")
            local app_size
            app_size=$(du -sh "$appdir" 2>/dev/null | awk '{print $1}')
            rpt "  ${appname} (${app_size})"
            apps+=("$appname")
        done
        app_list=$(printf '%s\n' "${apps[@]}" | jq -R . | jq -s '.' 2>/dev/null || echo '[]')
        rpt ""
        rpt "  Total installed apps: ${#apps[@]}"
    else
        rpt "  No apps directory found"
    fi
    rpt ""

    json_merge "appstore" "$(jq -n --argjson apps "$app_list" '{installed_apps:$apps}')"
    info "App store configuration audited"
}

###############################################################################
# 17. BACKUP STATE
###############################################################################
audit_backups() {
    head1 "17/21  Backup State"
    rpt_section "17. BACKUP STATE"

    # Backup schedules
    local sched_file="${CASAOS_DATA_DIR}/app-management/schedules.json"
    rpt_sub "Backup Schedules"
    if [ -f "$sched_file" ]; then
        jq -r '.[] | "  App: \(.app_name) | Enabled: \(.enabled) | Cron: \(.cron) | Last: \(.last_run // "never")"' "$sched_file" >> "$REPORT" 2>/dev/null || rpt "  (could not parse schedules)"
    else
        rpt "  No backup schedules found"
    fi
    rpt ""

    # Backup files
    rpt_sub "Existing Backups"
    local backup_dir="${CASAOS_DATA_DIR}/backups"
    if [ -d "$backup_dir" ]; then
        rpt "  Backup directory: ${backup_dir}"
        rpt "  Total size: $(du -sh "$backup_dir" 2>/dev/null | awk '{print $1}')"
        find "$backup_dir" -maxdepth 2 -type f -name "*.tar.gz" -o -name "*.zip" 2>/dev/null | while read -r bf; do
            rpt "  $(basename "$bf") — $(stat --printf='%s' "$bf" 2>/dev/null | numfmt --to=iec 2>/dev/null || echo '?') — $(stat --printf='%y' "$bf" 2>/dev/null | cut -d. -f1)"
        done
    else
        rpt "  No backups directory found"
    fi
    rpt ""

    # Last backup path marker
    if [ -f "${CASAOS_DATA_DIR}/last-backup-path" ]; then
        rpt "  Last backup path: $(cat "${CASAOS_DATA_DIR}/last-backup-path")"
    fi
    rpt ""

    json_merge "backups" "$(jq -n --argjson scheds "$([ -f "$sched_file" ] && jq -c '.' "$sched_file" 2>/dev/null || echo 'null')" '{schedules:$scheds}')"
    info "Backup state audited"
}

###############################################################################
# 18. SCHEDULED TASKS / JOBS
###############################################################################
audit_jobs() {
    head1 "18/21  Scheduled Tasks / Jobs"
    rpt_section "18. SCHEDULED TASKS / JOBS"

    # CasaOS job management tasks
    local tasks_file="${CASAOS_DATA_DIR}/job-management/tasks.json"
    rpt_sub "CasaOS Scheduled Tasks"
    if [ -f "$tasks_file" ]; then
        local task_count
        task_count=$(jq '.tasks | length' "$tasks_file" 2>/dev/null || echo "?")
        rpt "  Tasks defined: ${task_count}"
        jq -r '.tasks | to_entries[] | .value | "  Task: \(.name) | Cron: \(.cron) | Enabled: \(.enabled) | Last updated: \(.updated // "n/a")"' "$tasks_file" >> "$REPORT" 2>/dev/null || rpt "  (could not parse tasks)"
        rpt ""
        rpt_sub "Task Execution History (last 5 per task)"
        jq -r '.history | to_entries[] | "  Task \(.key):" as $hdr | $hdr, (.value[-5:][] | "    \(.started_at) — \(.status) (exit: \(.exit_code // "?"))")' "$tasks_file" >> "$REPORT" 2>/dev/null || true
    else
        rpt "  No tasks file found"
    fi
    rpt ""

    # System crontab
    rpt_sub "System Crontab (/etc/crontab)"
    if [ -f /etc/crontab ]; then
        grep -v '^#' /etc/crontab | grep -v '^$' >> "$REPORT" 2>/dev/null || rpt "  (empty)"
    fi
    rpt ""

    # User crontabs
    rpt_sub "User Crontabs"
    if [ -d /var/spool/cron/crontabs ]; then
        for ct in /var/spool/cron/crontabs/*; do
            [ -f "$ct" ] || continue
            rpt "  $(basename "$ct"):"
            grep -v '^#' "$ct" | grep -v '^$' | sed 's/^/    /' >> "$REPORT" 2>/dev/null
        done
    else
        rpt "  (no user crontabs)"
    fi
    rpt ""

    # Root crontab
    crontab -l >> "${OUTDIR}/logs/root-crontab.txt" 2>/dev/null || true

    json_merge "scheduled_tasks" "$([ -f "$tasks_file" ] && jq -c '.' "$tasks_file" 2>/dev/null || echo 'null')"
    info "Scheduled tasks audited"
}

###############################################################################
# 19. LOGS & RECENT ERRORS
###############################################################################
audit_logs() {
    head1 "19/21  Logs & Recent Errors"
    rpt_section "19. LOGS & RECENT ERRORS"

    # CasaOS service logs
    rpt_sub "CasaOS Service Logs (last 200 lines each)"
    if [ -d "$CASAOS_LOG_DIR" ]; then
        for logfile in "${CASAOS_LOG_DIR}"/*.log; do
            [ -f "$logfile" ] || continue
            local bname
            bname=$(basename "$logfile")
            rpt "  --- ${bname} ---"
            tail -200 "$logfile" > "${OUTDIR}/logs/${bname}" 2>/dev/null
            # Count errors/panics
            local err_count panic_count
            err_count=$(grep -ci 'error\|fatal\|fail' "$logfile" 2>/dev/null || echo "0")
            panic_count=$(grep -ci 'panic\|SIGSEGV\|goroutine' "$logfile" 2>/dev/null || echo "0")
            rpt "    Total lines: $(wc -l < "$logfile" 2>/dev/null || echo '?')"
            rpt "    Error/fatal mentions: ${err_count}"
            rpt "    Panic/crash mentions: ${panic_count}"
            rpt ""
        done
    else
        rpt "  Log directory not found: ${CASAOS_LOG_DIR}"
    fi

    # Journalctl for each service
    rpt_sub "Journalctl (last 100 lines per service)"
    for svc in "${SERVICES[@]}"; do
        journalctl -u "$svc" -n 100 --no-pager > "${OUTDIR}/logs/journalctl-${svc}.txt" 2>/dev/null || true
        local jlines
        jlines=$(wc -l < "${OUTDIR}/logs/journalctl-${svc}.txt" 2>/dev/null || echo "0")
        rpt "  ${svc}: ${jlines} journal lines saved"
    done
    rpt ""

    # Docker container logs (last 50 lines each)
    rpt_sub "Docker Container Logs (last 50 lines each)"
    if command -v docker &>/dev/null; then
        local cids
        cids=$(docker ps -a -q 2>/dev/null)
        if [ -n "$cids" ]; then
            for cid in $cids; do
                local cname
                cname=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null | sed 's|^/||')
                docker logs --tail 50 "$cid" > "${OUTDIR}/logs/container-${cname}.log" 2>&1 || true
                rpt "  ${cname}: saved"
            done
        fi
    fi
    rpt ""

    # Crash/panic pattern scan across all logs
    rpt_sub "Crash/Panic Pattern Scan"
    local crash_count=0
    for lf in "${OUTDIR}"/logs/*.log "${OUTDIR}"/logs/*.txt; do
        [ -f "$lf" ] || continue
        local panics
        panics=$(grep -c 'panic\|SIGSEGV\|runtime error\|goroutine.*running' "$lf" 2>/dev/null || echo "0")
        if [ "$panics" -gt 0 ]; then
            rpt "  $(basename "$lf"): ${panics} crash indicators"
            crash_count=$((crash_count + panics))
        fi
    done
    [ "$crash_count" -eq 0 ] && rpt "  No crash patterns detected"
    rpt ""

    json_merge "logs" "$(jq -n --argjson crashes "$crash_count" '{crash_indicators:$crashes}')"
    info "Logs collected and analyzed"
}

###############################################################################
# 20. FILE SHARES & REMOTE MOUNTS
###############################################################################
audit_shares() {
    head1 "20/21  File Shares & Remote Mounts"
    rpt_section "20. FILE SHARES & REMOTE MOUNTS"

    # Samba
    rpt_sub "Samba Configuration"
    if [ -f /etc/samba/smb.conf ]; then
        rpt "  Samba config found: /etc/samba/smb.conf"
        cp /etc/samba/smb.conf "${OUTDIR}/config/smb.conf" 2>/dev/null
        # List shares
        rpt "  Shares defined:"
        grep '^\[' /etc/samba/smb.conf 2>/dev/null | grep -v '^\[global\]' | sed 's/^/    /' >> "$REPORT"
    else
        rpt "  Samba not configured"
    fi
    rpt ""

    # CasaOS shares from database
    rpt_sub "CasaOS Shares (from database)"
    local sharesdb="${CASAOS_DATA_DIR}/db/casaOS.db"
    if [ -f "$sharesdb" ] && command -v sqlite3 &>/dev/null; then
        sqlite3 "$sharesdb" "SELECT name, path, anonymous FROM o_shares;" 2>/dev/null | while IFS='|' read -r name path anon; do
            rpt "  Share: ${name} -> ${path} (anonymous: ${anon})"
        done || rpt "  (no shares or table not found)"
    fi
    rpt ""

    # Remote connections
    rpt_sub "Remote Connections (from database)"
    if [ -f "$sharesdb" ] && command -v sqlite3 &>/dev/null; then
        sqlite3 "$sharesdb" "SELECT host, port, username, mount_point, status FROM o_connections;" 2>/dev/null | while IFS='|' read -r host port user mp status; do
            rpt "  Connection: ${user}@${host}:${port} -> ${mp} (status: ${status})"
        done || rpt "  (no connections or table not found)"
    fi
    rpt ""

    # NFS exports
    rpt_sub "NFS Exports"
    if [ -f /etc/exports ]; then
        cat /etc/exports >> "$REPORT" 2>/dev/null
    else
        rpt "  No NFS exports configured"
    fi
    rpt ""

    # rclone
    rpt_sub "rclone Remotes"
    if command -v rclone &>/dev/null; then
        rclone listremotes >> "$REPORT" 2>/dev/null || rpt "  (no remotes configured)"
        # Check for active rclone mounts
        rpt "  Active rclone mounts:"
        mount | grep rclone >> "$REPORT" 2>/dev/null || rpt "    (none)"
    else
        rpt "  rclone not installed"
    fi
    rpt ""

    json_merge "shares" '"collected"'
    info "File shares and remote mounts audited"
}

###############################################################################
# 21. MIGRATION READINESS ASSESSMENT
###############################################################################
audit_migration_readiness() {
    head1 "21/21  Migration Readiness Assessment"
    rpt_section "21. MIGRATION READINESS ASSESSMENT"

    local total_containers=0 running_containers=0
    local casaos_managed=0 external_containers=0
    local privileged_count=0 host_network_count=0
    local total_data_size="unknown"

    if command -v docker &>/dev/null; then
        total_containers=$(docker ps -a -q 2>/dev/null | wc -l)
        running_containers=$(docker ps -q 2>/dev/null | wc -l)

        # Analyze containers
        local cids
        cids=$(docker ps -a -q 2>/dev/null)
        for cid in $cids; do
            local proj priv netmode
            proj=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$cid" 2>/dev/null || echo "")
            priv=$(docker inspect --format '{{.HostConfig.Privileged}}' "$cid" 2>/dev/null || echo "false")
            netmode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$cid" 2>/dev/null || echo "")

            if [ -n "$proj" ]; then
                casaos_managed=$((casaos_managed + 1))
            else
                external_containers=$((external_containers + 1))
            fi
            [ "$priv" = "true" ] && privileged_count=$((privileged_count + 1))
            [ "$netmode" = "host" ] && host_network_count=$((host_network_count + 1))
        done
    fi

    # Data sizes
    if [ -d /DATA ]; then
        total_data_size=$(du -sh /DATA 2>/dev/null | awk '{print $1}')
    fi
    local casaos_data_size
    casaos_data_size=$(du -sh "${CASAOS_DATA_DIR}" 2>/dev/null | awk '{print $1}' || echo "unknown")

    # Services health
    local services_healthy=0 services_total=${#SERVICES[@]}
    for svc in "${SERVICES[@]}"; do
        local active
        active=$(systemctl is-active "$svc" 2>/dev/null || echo "inactive")
        [ "$active" = "active" ] && services_healthy=$((services_healthy + 1))
    done

    rpt "  ┌─────────────────────────────────────────────────────────────┐"
    rpt "  │              MIGRATION READINESS SUMMARY                    │"
    rpt "  ├─────────────────────────────────────────────────────────────┤"
    rpt "  │                                                             │"
    rpt "  │  CasaOS Services:  ${services_healthy}/${services_total} healthy                           │"
    rpt "  │                                                             │"
    rpt "  │  Containers:                                                │"
    rpt "  │    Total          : ${total_containers}                                       │"
    rpt "  │    Running        : ${running_containers}                                       │"
    rpt "  │    CasaOS-managed : ${casaos_managed}                                       │"
    rpt "  │    External       : ${external_containers}                                       │"
    rpt "  │                                                             │"
    rpt "  │  Risk Flags:                                                │"
    rpt "  │    Privileged     : ${privileged_count}                                       │"
    rpt "  │    Host Network   : ${host_network_count}                                       │"
    rpt "  │                                                             │"
    rpt "  │  Data:                                                      │"
    rpt "  │    /DATA size     : ${total_data_size}                                  │"
    rpt "  │    CasaOS data    : ${casaos_data_size}                                  │"
    rpt "  │                                                             │"
    rpt "  └─────────────────────────────────────────────────────────────┘"
    rpt ""

    # Compatibility notes
    rpt_sub "Compatibility Notes"
    if [ "$privileged_count" -gt 0 ]; then
        rpt "  [!] ${privileged_count} privileged container(s) detected — review security implications"
    fi
    if [ "$host_network_count" -gt 0 ]; then
        rpt "  [!] ${host_network_count} container(s) using host network — may conflict with CasaNova gateway"
    fi
    if [ "$external_containers" -gt 0 ]; then
        rpt "  [i] ${external_containers} container(s) not managed by CasaOS — manual migration may be needed"
    fi
    if mount | grep -q mergerfs 2>/dev/null; then
        rpt "  [i] MergerFS in use — verify CasaNova storage service supports current config"
    fi
    rpt ""

    # CasaOS-managed containers can migrate as-is
    rpt_sub "Container Migration Compatibility"
    if command -v docker &>/dev/null; then
        for cid in $(docker ps -a -q 2>/dev/null); do
            local name image state priv netmode
            name=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null | sed 's|^/||')
            image=$(docker inspect --format '{{.Config.Image}}' "$cid" 2>/dev/null)
            state=$(docker inspect --format '{{.State.Status}}' "$cid" 2>/dev/null)
            priv=$(docker inspect --format '{{.HostConfig.Privileged}}' "$cid" 2>/dev/null)
            netmode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$cid" 2>/dev/null)

            local compat="COMPATIBLE"
            local flags=""
            [ "$priv" = "true" ] && compat="REVIEW" && flags+="privileged "
            [ "$netmode" = "host" ] && compat="REVIEW" && flags+="host-network "

            rpt "  $(printf '%-25s %-10s %-12s %s' "$name" "$state" "$compat" "$flags")"
        done
    fi
    rpt ""

    json_merge "migration_readiness" "$(jq -n \
        --argjson services_healthy "$services_healthy" \
        --argjson services_total "$services_total" \
        --argjson total_containers "$total_containers" \
        --argjson running "$running_containers" \
        --argjson managed "$casaos_managed" \
        --argjson external "$external_containers" \
        --argjson privileged "$privileged_count" \
        --argjson host_net "$host_network_count" \
        --arg data_size "$total_data_size" \
        --arg casaos_size "$casaos_data_size" \
        '{services_healthy:$services_healthy, services_total:$services_total,
          containers:{total:$total_containers, running:$running, casaos_managed:$managed, external:$external},
          risk_flags:{privileged:$privileged, host_network:$host_net},
          data:{data_dir_size:$data_size, casaos_dir_size:$casaos_size}}'
    )"
    info "Migration readiness assessment complete"
}

###############################################################################
# MAIN EXECUTION
###############################################################################

audit_system
audit_casaos_install
audit_config
audit_systemd
audit_databases
audit_json_state
audit_users
audit_docker_env
audit_containers
audit_compose
audit_images
audit_volumes
audit_networks
audit_storage
audit_network
audit_appstore
audit_backups
audit_jobs
audit_logs
audit_shares
audit_migration_readiness

# ─── Final summary ──────────────────────────────────────────────────────────

# Add metadata to JSON
json_merge "audit_metadata" "$(jq -n \
    --arg version "$AUDIT_VERSION" \
    --arg timestamp "$TIMESTAMP" \
    --arg hostname "$(hostname)" \
    '{version:$version, timestamp:$timestamp, hostname:$hostname}'
)"

# Compress
head1 "Packaging Audit Results"
ARCHIVE="/tmp/casaos-audit-${TIMESTAMP}.tar.gz"
tar -czf "$ARCHIVE" -C /tmp "casaos-audit-${TIMESTAMP}" 2>/dev/null

# Final output
echo ""
echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${GREEN}  AUDIT COMPLETE${NC}"
echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "  Output directory : ${OUTDIR}/"
echo -e "  Archive          : ${ARCHIVE}"
echo -e "  Archive size     : $(du -sh "$ARCHIVE" | awk '{print $1}')"
echo -e "  Report           : ${OUTDIR}/report.txt"
echo -e "  JSON data        : ${OUTDIR}/audit.json"
echo ""
echo -e "  Transfer the archive with:"
echo -e "    ${CYAN}scp ${ARCHIVE} user@workstation:~/casaos-audits/${NC}"
echo ""

# Quick stats
echo -e "  ${BOLD}Quick Stats:${NC}"
jq -r '
  "  Services healthy  : \(.migration_readiness.services_healthy)/\(.migration_readiness.services_total)",
  "  Containers total  : \(.migration_readiness.containers.total)",
  "  CasaOS-managed    : \(.migration_readiness.containers.casaos_managed)",
  "  Risk flags        : \(.migration_readiness.risk_flags.privileged) privileged, \(.migration_readiness.risk_flags.host_network) host-net",
  "  /DATA size        : \(.migration_readiness.data.data_dir_size)"
' "$JSON" 2>/dev/null || true
echo ""
