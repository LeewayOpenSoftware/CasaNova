#!/usr/bin/env bash
###############################################################################
# CasaNova Self-Update Engine
# LucentOpen Software · v1.0.0
#
# This script is the workhorse behind CasaNova's self-update mechanism.
# It is called by the Go backend (via service/system.go) when the user
# triggers an update from the UI, or it can be run manually.
#
# What it does:
#   1. Validates the update package (tar.gz): the in-package checksum list must
#      be present, cover every file and match. That proves INTEGRITY only —
#      whoever builds a package also writes its checksum list. Authenticity is
#      checked only when a project signing key is installed on this box (see
#      SIGNING_KEY_FILE below); without that key the package is trusted because
#      it reached /var/lib/casanova/updates, and nothing more.
#   2. Backs up current binaries, frontend, systemd units — each copy is
#      verified against the source and recorded in a content census, because a
#      backup nobody can prove is intact is not a rollback path.
#   3. Stops CasaNova services in reverse dependency order
#   4. Extracts and deploys new files (binaries, frontend, systemd)
#   5. Preserves ALL user data: configs, databases, Docker, mounts
#   6. Restarts CasaNova services in dependency order
#   7. Verifies the new version is running
#   8. Reports status to a JSON file the backend can poll
#
# Usage:
#   # Apply an update package
#   sudo bash casanova-update.sh /path/to/casanova-v5.0.0.tar.gz
#
#   # Apply with progress reporting (backend mode)
#   sudo bash casanova-update.sh --status-file=/var/run/casanova/update-status.json \
#                                 /path/to/update.tar.gz
#
#   # Verify a package without applying
#   sudo bash casanova-update.sh --verify-only /path/to/update.tar.gz
#
# Exit codes:
#   0 = success
#   1 = pre-flight failure (bad package, no space, etc.)
#   2 = deployment failure (file copy error)
#   3 = verification failure (services didn't start)
#   4 = cancelled by user/backend
#
# Docker containers and user data are NEVER touched.
###############################################################################
set -euo pipefail

# ─── Constants ───────────────────────────────────────────────────────────────
# Every path the crash-consistent commit and the boot recovery mutate hangs off
# ROOT_PREFIX. It is empty on a real box; the recovery test harness points it at
# a sandbox so the "the power died mid-update" paths can be exercised for real
# instead of being reasoned about.
ROOT_PREFIX="${CASANOVA_UPDATE_ROOT:-}"
BIN_DIR="${ROOT_PREFIX}/usr/bin"
UNIT_DIR="${ROOT_PREFIX}/usr/lib/systemd/system"
TOOLS_DIR="${ROOT_PREFIX}/usr/share/casanova/tools"
WWW_DIR="${ROOT_PREFIX}/var/lib/casanova/www"
# Staging + retired frontend trees. Deliberately NOT www.new/www.old: operators
# already park manual copies under those names on live boxes (the production box
# has both), and this script deletes its own staging dirs unconditionally.
WWW_STAGE_DIR="${ROOT_PREFIX}/var/lib/casanova/www.update-new"
WWW_RETIRED_DIR="${ROOT_PREFIX}/var/lib/casanova/www.update-old"
MARKER_FILE="${ROOT_PREFIX}/var/lib/casanova/update-in-progress.json"
RECOVERY_UNIT_NAME="casanova-update-recovery.service"
RECOVERY_UNIT_FILE="${UNIT_DIR}/${RECOVERY_UNIT_NAME}"
RECOVERY_ENTRY_POINT="${TOOLS_DIR}/casanova-update.sh"
# `systemctl enable` records the [Install] section as a symlink here. It is the
# ONLY on-disk evidence that a unit will be pulled into a boot, which is why the
# arming code checks for this path instead of trusting an exit status.
RECOVERY_WANTED_BY="multi-user.target"
WANTS_DIR="${ROOT_PREFIX}/etc/systemd/system/${RECOVERY_WANTED_BY}.wants"
RECOVERY_WANTS_LINK="${WANTS_DIR}/${RECOVERY_UNIT_NAME}"

# Everything else the apply writes outside BIN_DIR/UNIT_DIR/WWW_DIR. Named here
# so the backup, the restore and the staging sweep all read from one list and
# cannot drift apart.
ETC_DIR="${ROOT_PREFIX}/etc/casanova"
ETC_LEGACY_DIR="${ROOT_PREFIX}/etc/casaos"
AVAHI_DIR="${ROOT_PREFIX}/etc/avahi"
AVAHI_SERVICES_DIR="${AVAHI_DIR}/services"
AVAHI_SERVICE_FILE="${AVAHI_SERVICES_DIR}/casanova.service"
RUN_LEGACY_DIR="${ROOT_PREFIX}/var/run/casaos"
TMPFILES_FILE="${ROOT_PREFIX}/usr/lib/tmpfiles.d/casanova.conf"
MANIFEST_FILE="${ROOT_PREFIX}/var/lib/casanova/manifest.json"
# Written last in Phase 2, so its presence means "the backup next to it is a
# complete census of what was installed before the update". Its content is the
# exact binary list that census covered: boot recovery may only conclude
# "this file is new, pull it out" for names that appear in it, which keeps the
# conclusion correct even when a later version grows the binary list.
# Written last in Phase 2 (see write_census). Format, one entry per line:
#
#     <sha256|->  <bin|systemd>/<name>  <bytes|->
#
# The digest is of the LIVE file that was copied, so a restore can prove the
# backup copy still matches what Phase 2 read — a truncated copy no longer looks
# like a valid one. "-" marks a name that was covered by the census but not
# present on the box (nothing to restore, but the name is still known to be old,
# which is what lets a rollback tell "the package added this" from "this was
# always here"). Backups written by earlier updaters hold bare names with no
# digest; those are read as unverifiable, never as verified (see census_check).
BACKUP_CENSUS_NAME=".backup-census"
BACKUP_CENSUS_PARTIAL=".backup-census.partial"
# Config files this apply CREATED. The apply never overwrites a config, so a
# rollback must not restore contents (that would clobber operator edits); it
# only has to withdraw the ones the package introduced, and this is the exact
# list — inferred nothing.
CREATED_CONFIGS_NAME="created-configs.txt"
# Quiesced database snapshot: taken with every service stopped (Phase 3), so it
# is a consistent copy, and restored by the rollback paths. Its own manifest is
# written last and is what makes the snapshot eligible for a restore.
DB_SNAPSHOT_DIR_NAME="db-snapshot"
DB_MANIFEST_NAME=".db-manifest"
# Directories holding SQLite databases the services own. Both are covered: the
# legacy path is still the configured DBPath on installed boxes.
DB_SOURCE_DIRS=("var/lib/casaos/db" "var/lib/casanova/db")
# Optional project signing key. When this file exists the package MUST carry a
# valid detached signature (<package>.sig) or the apply is refused. It is
# deliberately not auto-created: a key this script could generate would prove
# nothing about the package's origin.
SIGNING_KEY_FILE="${ETC_DIR}/update-signing.pub"

UPGRADE_BACKUP_BASE="${ROOT_PREFIX}/var/lib/casanova/backups"
STATUS_FILE=""
# Set by backup_restore_gate: how much the restore paths could prove about the
# backup they are about to install ("verified", "unverified", "rejected").
RESTORE_TRUST="unknown"
VERIFY_ONLY=false
BOOT_RECOVERY=false
INSTALL_RECOVERY_ONLY=false
PACKAGE_FILE=""
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
UPGRADE_BACKUP_DIR="${UPGRADE_BACKUP_BASE}/upgrade-${TIMESTAMP}"
LAST_UPGRADE_FILE="${UPGRADE_BACKUP_BASE}/.last-upgrade-backup"

CASANOVA_BINARIES=(casanova casanova-gateway casanova-user-service casanova-local-storage casanova-app-management casanova-message-bus casanova-job-management casanova-driver casanova-virt casanova-cli)
CASANOVA_SERVICES_STOP=(casanova casanova-job-management casanova-app-management casanova-local-storage casanova-local-storage-first casanova-user-service casanova-virt casanova-driver casanova-message-bus casanova-gateway)
CASANOVA_SERVICES_START=(casanova-gateway casanova-message-bus casanova-driver casanova-virt casanova-user-service casanova-local-storage-first casanova-local-storage casanova-app-management casanova-job-management casanova)

# ─── Colours ─────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
err()   { echo -e "${RED}[-]${NC} $*"; }
step()  { echo -e "\n${BOLD}${CYAN}══ $* ══${NC}"; }

# ─── Host daemon calls ───────────────────────────────────────────────────────
# systemctl, systemd-tmpfiles, docker and the localhost health probes always
# talk to the HOST — no path prefix can redirect them at a sandbox. Routing them
# through one wrapper is what makes ROOT_PREFIX safe end to end: with a prefix
# set they are either handed to a test stub or skipped, so exercising the apply
# path can never stop the services of the machine running the tests. With no
# prefix (every real box) this is a plain exec of the same command line.
host_cmd() {
    if [ -n "$ROOT_PREFIX" ]; then
        if [ -n "${CASANOVA_UPDATE_HOST_STUB:-}" ]; then
            "$CASANOVA_UPDATE_HOST_STUB" "$@"
            return $?
        fi
        return 0
    fi
    "$@"
}

# Fixed waits that only exist to let real services settle. Skipped under
# ROOT_PREFIX so the crash tests do not pay ~30s per apply for nothing.
settle() {
    if [ -n "$ROOT_PREFIX" ]; then
        return 0
    fi
    sleep "$1"
}

# ─── JSON string escaping ────────────────────────────────────────────────────
# Everything interpolated into the status file and the update report can carry
# text this script did not author: the version and channel come from the
# package's own metadata, paths come from the environment, and messages quote
# failures. One unescaped `"` used to be enough to make the status file
# unparseable, which the backend swallows silently — the progress bar then just
# stops moving with no explanation. Escapes the two characters JSON forbids raw
# in a string, then drops the control characters (they cannot appear in any
# legitimate value here, and dropping is safer than emitting \u escapes by hand).
json_escape() {
    local s="${1-}"
    s=${s//\\/\\\\}
    s=${s//\"/\\\"}
    printf '%s' "$s" | tr -d '\000-\037'
}

# ─── Status reporting ────────────────────────────────────────────────────────
# When called by the Go backend, write structured status to a JSON file
# that the /v1/sys/update/status endpoint can read and relay to the UI.
#
# The backend keeps showing the last stage it saw while that stage is a
# non-terminal one, so a run that dies without writing a terminal status leaves
# the UI pinned on "applying" forever. FINAL_STATUS_REPORTED is what the exit
# trap uses to guarantee one is always written.
FINAL_STATUS_REPORTED=false

report_status() {
    local stage="$1"
    local percent="$2"
    local message="$3"

    case "$stage" in
        failed|completed) FINAL_STATUS_REPORTED=true ;;
    esac

    if [ -n "$STATUS_FILE" ]; then
        cat > "$STATUS_FILE" << EOF
{
  "stage": "$(json_escape "$stage")",
  "progressPercent": ${percent},
  "message": "$(json_escape "$message")",
  "updatedAt": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "pid": $$
}
EOF
    fi
}

# ─── Crash-consistent commit primitives ──────────────────────────────────────
# `install`/`cp` over a live path truncate it and THEN write, so a power cut in
# that window leaves a half-written file at the final name: a truncated
# /usr/bin/casanova is a permanent crashloop, and the frontend equivalent
# (rm -rf www/* ; cp -r) leaves the box serving an empty UI. Everything below
# instead writes next to the target on the SAME filesystem, flushes it, and
# rename(2)s it into place — rename is atomic, so a crash can only ever leave
# the complete old file or the complete new one, never a torn one.

fsync_path() {
    # `sync FILE` (coreutils >= 8.24) flushes just this file/its filesystem;
    # fall back to a global sync on older userlands rather than skipping it —
    # an unflushed rename is exactly the durability hole we are closing.
    sync "$1" 2>/dev/null || sync
}

# The staging name both writers below use for a destination. Boot recovery
# derives its sweep list from this same helper, so a leftover temp can never be
# missed because two places spelled the name differently.
stage_name_of() {
    echo "$(dirname "$1")/.$(basename "$1").cn-new"
}

atomic_install() {
    local src="$1" dst="$2" mode="${3:-0755}"
    local dir base tmp
    dir=$(dirname "$dst")
    base=$(basename "$dst")
    tmp="${dir}/.${base}.cn-new"
    rm -f "$tmp"
    install -m "$mode" "$src" "$tmp"
    fsync_path "$tmp"
    mv -f "$tmp" "$dst"
    fsync_path "$dir"
}

# Same contract as atomic_install, but the content comes from stdin.
atomic_write() {
    local dst="$1" mode="${2:-0644}"
    local dir base tmp
    dir=$(dirname "$dst")
    base=$(basename "$dst")
    tmp="${dir}/.${base}.cn-new"
    rm -f "$tmp"
    cat > "$tmp"
    chmod "$mode" "$tmp"
    fsync_path "$tmp"
    mv -f "$tmp" "$dst"
    fsync_path "$dir"
}

# Copy a frontend tree next to the live one. Staging first means an ENOSPC or a
# broken package fails BEFORE the live UI is touched, instead of half-way
# through overwriting it.
stage_www() {
    local src="$1"
    rm -rf "$WWW_STAGE_DIR"
    mkdir -p "$WWW_STAGE_DIR"
    cp -a "${src}/." "$WWW_STAGE_DIR/"
    # Flush the whole staged tree: fsync on the directory alone would not make
    # the file contents durable, so a power cut could rename in an empty shell.
    sync
}

# Swap the staged tree in with two renames. The window where the live dir does
# not exist is two rename(2) calls wide, and boot recovery repairs exactly that
# window (see normalize_www).
swap_www() {
    rm -rf "$WWW_RETIRED_DIR"
    if [ -d "$WWW_DIR" ]; then
        mv "$WWW_DIR" "$WWW_RETIRED_DIR"
    fi
    mv "$WWW_STAGE_DIR" "$WWW_DIR"
    fsync_path "$(dirname "$WWW_DIR")"
    rm -rf "$WWW_RETIRED_DIR"
}

# Repair an interrupted swap_www: if the live dir is gone, put back whichever
# side survived — the retired tree first, because that is the one that was
# actually serving before the update started.
normalize_www() {
    if [ -d "$WWW_DIR" ]; then
        return 0
    fi
    if [ -d "$WWW_RETIRED_DIR" ]; then
        warn "Frontend swap was interrupted — restoring the previous tree."
        mv "$WWW_RETIRED_DIR" "$WWW_DIR"
    elif [ -d "$WWW_STAGE_DIR" ]; then
        warn "Frontend swap was interrupted — promoting the staged tree."
        mv "$WWW_STAGE_DIR" "$WWW_DIR"
    fi
}

# ─── Backup content census ───────────────────────────────────────────────────
# A backup is only a rollback path if what it holds can be proven to be what was
# installed. Recording names alone made every restore an act of faith: a backup
# whose bin/casanova was truncated to 0 bytes was installed over the running
# system, the marker was cleared, and the box never booted CasaNova again — the
# recovery had destroyed more than the crash it was repairing.
#
# So the producer (Phase 2) records the sha256 and size of the LIVE file it
# copied, and the consumers (boot recovery, online rollback) re-verify the
# backup copy against that record before installing a single byte. Verification
# is all-or-nothing and happens BEFORE any install, so a bad backup can never
# leave the box half-restored.

# Copy one file into the backup and prove the copy is intact, appending its
# census line. Every failure is returned, never swallowed: a backup that quietly
# lost a binary is worse than no backup at all, because the restore paths trust
# it.
backup_capture() {
    local src="$1" rel="$2" census="$3"
    local dst="${UPGRADE_BACKUP_DIR}/${rel}"
    local want have size

    mkdir -p "$(dirname "$dst")" || return 1
    want=$(sha256sum "$src" 2>/dev/null | awk '{print $1}')
    if [ -z "$want" ]; then
        err "Cannot read ${src} to back it up."
        return 1
    fi
    if ! cp -f "$src" "$dst"; then
        err "Cannot copy ${src} into the pre-update backup."
        return 1
    fi
    have=$(sha256sum "$dst" 2>/dev/null | awk '{print $1}')
    size=$(stat -c %s "$dst" 2>/dev/null || echo "")
    if [ -z "$have" ] || [ -z "$size" ] || [ "$have" != "$want" ]; then
        err "Backup copy of ${src} does not match the original (short write, or out of space)."
        return 1
    fi
    printf '%s  %s  %s\n' "$have" "$rel" "$size" >> "$census"
}

# Record a name the census covers that was NOT installed. Restores need this to
# tell "the package added this file" from "this file was always here".
census_add_absent() {
    printf '%s  %s  %s\n' "-" "$1" "-" >> "$2"
}

# Binary names the census covers, one per line. Understands both the content
# format and the bare-name format older updaters wrote.
census_names() {
    local backup="$1" line a b c extra
    [ -f "${backup}/${BACKUP_CENSUS_NAME}" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        [ -n "$line" ] || continue
        a=""; b=""; c=""; extra=""
        read -r a b c extra <<< "$line"
        if [ -n "$b" ] && [ -n "$c" ] && [ -z "$extra" ]; then
            case "$b" in
                bin/*) printf '%s\n' "${b#bin/}" ;;
            esac
        elif [ -n "$a" ] && [ -z "$b" ]; then
            printf '%s\n' "$a"
        fi
    done < "${backup}/${BACKUP_CENSUS_NAME}"
}

# Verify a backup against its census.
# Sets CENSUS_STATE to one of:
#   verified — every recorded file is present and matches its size and sha256
#   legacy   — a census with no content recorded (older updater): unverifiable
#   missing  — no census at all: unverifiable
#   mismatch — the backup contradicts its own census: it must NOT be installed
# CENSUS_PROBLEM carries the actionable detail for the journal and the report.
census_check() {
    local backup="$1"
    local file="${backup}/${BACKUP_CENSUS_NAME}"
    local line digest rel size extra path have have_size covered="" digested=0

    CENSUS_STATE="missing"
    CENSUS_PROBLEM="no census in ${backup}"
    [ -f "$file" ] || return 0

    while IFS= read -r line || [ -n "$line" ]; do
        [ -n "$line" ] || continue
        digest=""; rel=""; size=""; extra=""
        read -r digest rel size extra <<< "$line"
        # Anything that is not exactly three fields is a bare name from an older
        # updater (or noise): it records no content, so there is nothing to
        # verify and nothing to trust.
        if [ -z "$rel" ] || [ -n "$extra" ]; then
            continue
        fi
        [ "$digest" = "-" ] && continue
        digested=$((digested + 1))
        covered="${covered}|${rel}|"
        path="${backup}/${rel}"
        if [ ! -f "$path" ]; then
            CENSUS_STATE="mismatch"
            CENSUS_PROBLEM="${rel} is in the census but missing from ${backup}"
            return 0
        fi
        have_size=$(stat -c %s "$path" 2>/dev/null || echo "")
        if [ "$size" != "-" ] && [ "$have_size" != "$size" ]; then
            CENSUS_STATE="mismatch"
            CENSUS_PROBLEM="${rel} is ${have_size:-unreadable} bytes but was backed up as ${size} — the backup is truncated"
            return 0
        fi
        have=$(sha256sum "$path" 2>/dev/null | awk '{print $1}')
        if [ -z "$have" ] || [ "$have" != "$digest" ]; then
            CENSUS_STATE="mismatch"
            CENSUS_PROBLEM="${rel} does not match the sha256 recorded for it — the backup is corrupt"
            return 0
        fi
    done < "$file"

    if [ "$digested" -eq 0 ]; then
        CENSUS_STATE="legacy"
        CENSUS_PROBLEM="the census records names only (written by an older updater), so contents cannot be verified"
        return 0
    fi

    # A file the census does not cover is a file nobody vouched for. It would be
    # installed over the running system by the restore loops, so treat the whole
    # backup as untrustworthy rather than silently skipping it.
    for path in "${backup}"/bin/* "${backup}"/systemd/*; do
        [ -f "$path" ] || continue
        rel="$(basename "$(dirname "$path")")/$(basename "$path")"
        case "$covered" in
            *"|${rel}|"*) ;;
            *)
                CENSUS_STATE="mismatch"
                CENSUS_PROBLEM="${backup}/${rel} is not covered by the census — the backup is not the one it claims to be"
                return 0
                ;;
        esac
    done

    CENSUS_STATE="verified"
    CENSUS_PROBLEM=""
    return 0
}

# Gate every restore of binaries/units goes through.
# Returns 0 = the backup may be installed, non-zero = conserve, install nothing.
# Sets RESTORE_TRUST for the breadcrumb and the report.
backup_restore_gate() {
    local backup="$1" f

    census_check "$backup"
    case "$CENSUS_STATE" in
        verified)
            RESTORE_TRUST="verified"
            return 0
            ;;
        mismatch)
            RESTORE_TRUST="rejected"
            err "Pre-update backup failed verification: ${CENSUS_PROBLEM}"
            return 1
            ;;
    esac

    # No census, or a name-only one: contents cannot be proven. Refusing every
    # such backup would strand exactly the boxes whose backup was written by the
    # updater running today, so fall back to the one property that survives
    # without a record — a zero-byte file is never a binary or a unit — and say
    # out loud that this restore is unverified.
    RESTORE_TRUST="unverified"
    warn "Pre-update backup cannot be verified: ${CENSUS_PROBLEM}"
    for f in "${backup}"/bin/* "${backup}"/systemd/*.service; do
        [ -f "$f" ] || continue
        if [ ! -s "$f" ]; then
            err "Pre-update backup holds an EMPTY ${f}."
            err "Installing it would replace a working file with nothing — refusing the whole restore."
            RESTORE_TRUST="rejected"
            return 1
        fi
    done
    return 0
}

# ─── Quiesced database snapshot ──────────────────────────────────────────────
# Taken after the services are stopped, so nothing holds the SQLite files open
# and .db + -wal are a matched pair. The manifest is written last: its presence
# is what makes the snapshot eligible for a restore, so a snapshot interrupted
# half-way is simply never used.
#
# The rollback paths restore it because the new build migrates the schema on its
# first start (and drops legacy tables outright); without this, a rollback put
# old binaries on top of a database only the new ones understand.
snapshot_databases() {
    local dir src rel base name
    local out="${UPGRADE_BACKUP_DIR}/${DB_SNAPSHOT_DIR_NAME}"
    local manifest="${out}/${DB_MANIFEST_NAME}"
    local partial="${manifest}.partial"
    local captured=0

    rm -rf "$out"
    mkdir -p "$out" || { warn "Could not create ${out} — a rollback will not restore the databases."; return 1; }
    : > "$partial" || return 1

    for dir in "${DB_SOURCE_DIRS[@]}"; do
        [ -d "${ROOT_PREFIX}/${dir}" ] || continue
        for src in "${ROOT_PREFIX}/${dir}"/*.db; do
            [ -f "$src" ] || continue
            base=$(basename "$src")
            for name in "$base" "${base}-wal"; do
                [ -f "${ROOT_PREFIX}/${dir}/${name}" ] || continue
                rel="${DB_SNAPSHOT_DIR_NAME}/${dir}/${name}"
                if ! backup_capture "${ROOT_PREFIX}/${dir}/${name}" "$rel" "$partial"; then
                    warn "Database snapshot failed on ${dir}/${name} — a rollback will NOT restore the databases."
                    rm -f "$partial"
                    return 1
                fi
                captured=$((captured + 1))
            done
        done
    done

    if [ "$captured" -eq 0 ]; then
        rm -f "$partial"
        info "No databases found to snapshot"
        return 0
    fi

    sync
    if ! mv -f "$partial" "$manifest"; then
        warn "Could not seal the database snapshot manifest."
        rm -f "$partial"
        return 1
    fi
    fsync_path "$out"
    info "Database snapshot: ${captured} files (services stopped)"
    return 0
}

# Restore the quiesced snapshot. Callers MUST have every service stopped.
#
# Ordering is what keeps this crash-safe: the -wal/-shm of the generation being
# undone go first, then the database, then the snapshot's own -wal. Every
# intermediate state on disk is a database SQLite can open — never one paired
# with a WAL from a different generation, which is the state that corrupts. What
# the first step discards is the tail of the version being rolled back, which is
# what a rollback means. A cut anywhere here leaves the marker armed, and the
# next boot repeats the restore from the same (untouched) snapshot.
restore_databases() {
    local backup="$1"
    local out="${backup}/${DB_SNAPSHOT_DIR_NAME}"
    local manifest="${out}/${DB_MANIFEST_NAME}"
    local line digest rel size extra src dst mode pass restored=0

    if [ ! -f "$manifest" ]; then
        # Every backup written before the quiesced snapshot existed lands here.
        # Said out loud rather than passed over: the binaries went back, the data
        # did not, and that is a difference an operator has to know about.
        info "No database snapshot in ${backup} — the databases are left as they are."
        return 0
    fi

    # Same all-or-nothing rule as the binaries: verify the whole snapshot first.
    while IFS= read -r line || [ -n "$line" ]; do
        [ -n "$line" ] || continue
        digest=""; rel=""; size=""; extra=""
        read -r digest rel size extra <<< "$line"
        if [ -z "$rel" ] || [ -n "$extra" ] || [ "$digest" = "-" ]; then
            warn "Database snapshot manifest is malformed — not restoring the databases."
            return 1
        fi
        src="${backup}/${rel}"
        if [ ! -f "$src" ] || [ "$(stat -c %s "$src" 2>/dev/null || echo "")" != "$size" ] ||
           [ "$(sha256sum "$src" 2>/dev/null | awk '{print $1}')" != "$digest" ]; then
            err "Database snapshot does not match its manifest (${rel}) — leaving the live databases alone."
            return 1
        fi
    done < "$manifest"

    # Pass 1: drop the write-ahead logs of the databases being replaced. Left in
    # place they would be replayed on top of the restored file.
    while IFS= read -r line || [ -n "$line" ]; do
        [ -n "$line" ] || continue
        rel=""; read -r digest rel size extra <<< "$line"
        case "$rel" in
            *-wal) continue ;;
        esac
        dst="${ROOT_PREFIX}/${rel#${DB_SNAPSHOT_DIR_NAME}/}"
        rm -f "${dst}-wal" "${dst}-shm"
    done < "$manifest"

    # Pass 2: the databases, then their WALs.
    for pass in db wal; do
        while IFS= read -r line || [ -n "$line" ]; do
            [ -n "$line" ] || continue
            rel=""; read -r digest rel size extra <<< "$line"
            case "${pass}:${rel}" in
                db:*-wal) continue ;;
                wal:*-wal) ;;
                wal:*) continue ;;
            esac
            src="${backup}/${rel}"
            dst="${ROOT_PREFIX}/${rel#${DB_SNAPSHOT_DIR_NAME}/}"
            mkdir -p "$(dirname "$dst")"
            # Keep whatever mode the live file carries; a restore must not widen
            # (or narrow) access to a database as a side effect.
            mode=0644
            [ -f "$dst" ] && mode=$(stat -c %a "$dst" 2>/dev/null || echo 0644)
            atomic_install "$src" "$dst" "$mode"
            restored=$((restored + 1))
        done < "$manifest"
    done

    info "Restored ${restored} database files from the pre-update snapshot"
    return 0
}

# ─── Attempt marker ──────────────────────────────────────────────────────────
# The marker is the ONLY thing that tells the next boot "this box died mid
# update". It is armed once the pre-update backup is on stable storage and
# before the first destructive step, flipped to `committed` when every file is
# at its final name, and removed only on a clean outcome (verified success, or
# a rollback that finished). Its absence therefore means "the on-disk state is
# whatever it is on purpose".
write_marker() {
    local phase="$1"
    mkdir -p "$(dirname "$MARKER_FILE")"
    atomic_write "$MARKER_FILE" 0600 << EOF
{
  "phase": "${phase}",
  "from_version": "${CURRENT_VERSION:-unknown}",
  "to_version": "${NEW_VERSION:-unknown}",
  "backup_dir": "${UPGRADE_BACKUP_DIR}",
  "updated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "pid": $$
}
EOF
}

clear_marker() {
    rm -f "$MARKER_FILE"
    fsync_path "$(dirname "$MARKER_FILE")"
}

# Field reader that does not need python3 — boot recovery runs early, before
# casanova.service, and must not depend on an interpreter being installed.
marker_field() {
    # `|| true`: a missing/garbled marker must degrade to an empty field, not
    # abort recovery through `set -e` — the caller already treats empty as
    # "unknown, be conservative".
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$MARKER_FILE" 2>/dev/null | head -1 || true
}

# ─── Boot recovery unit ──────────────────────────────────────────────────────
# Installed by the updater itself rather than shipped in the package: the unit
# is only ever needed on a box where THIS script has already run (nothing else
# can leave a marker), and installing it before we arm the marker means even the
# very first update on an old box is covered. Idempotent, re-asserted on every
# update — same pattern as the avahi advertisement below.
# Returns non-zero when the unit is NOT armed. Callers must treat that as a
# hard failure: every caller is about to do (or has just done) something that
# only this unit can repair.
ensure_boot_recovery() {
    local self="${BASH_SOURCE[0]:-}"
    local before="" svc

    mkdir -p "$TOOLS_DIR" "$UNIT_DIR"

    # The unit runs this very script. Prefer the running file over the packaged
    # one: it is the copy we know understands --boot-recovery.
    if [ -n "$self" ] && [ -f "$self" ] && ! [ "$self" -ef "$RECOVERY_ENTRY_POINT" ]; then
        atomic_install "$self" "$RECOVERY_ENTRY_POINT" 0755
    fi
    if [ ! -x "$RECOVERY_ENTRY_POINT" ]; then
        err "No recovery entry point at ${RECOVERY_ENTRY_POINT} — boot recovery cannot be armed."
        return 1
    fi

    # Ordered before every unit that executes one of the binaries we may have to
    # restore — including the storage init oneshot, which runs early (Before=
    # docker.service) and would otherwise get to run a half-updated binary.
    for svc in "${CASANOVA_SERVICES_START[@]}"; do before+="${svc}.service "; done
    before+="casanova-local-storage-first.service"

    if ! atomic_write "$RECOVERY_UNIT_FILE" 0644 << EOF
[Unit]
Description=CasaNova interrupted-update recovery
# Only pays for itself when an update died half-applied; on every other boot
# systemd skips the unit outright because the marker is not there.
ConditionPathExists=${MARKER_FILE}
# /usr and /var must be mounted before we can restore anything. Deliberately
# ordering-only (Before=, never Requires=): if recovery itself fails the box has
# to finish booting anyway, so an operator can still get in over SSH.
After=local-fs.target
Before=${before}

[Service]
Type=oneshot
RemainAfterExit=yes
# No sandboxing on purpose: this has to write /usr/bin and /usr/lib/systemd.
ExecStart=${RECOVERY_ENTRY_POINT} --boot-recovery
TimeoutStartSec=300

[Install]
# Without this section systemd has nothing to enable: it prints "the unit files
# have no installation config", creates no symlink and still exits 0, so the
# unit would sit on disk and never run. Before= is ordering only — it pulls
# nothing into the boot transaction. ${RECOVERY_WANTED_BY} is the target every
# casanova*.service is already wanted by, which is what puts this unit in the
# same transaction and makes the Before= above take effect.
WantedBy=${RECOVERY_WANTED_BY}
EOF
    then
        err "Could not write ${RECOVERY_UNIT_FILE} — boot recovery cannot be armed."
        return 1
    fi

    if [ -z "$ROOT_PREFIX" ]; then
        systemctl daemon-reload || warn "daemon-reload failed after installing ${RECOVERY_UNIT_NAME}"
        # Exit status deliberately ignored: systemctl reports success both when
        # it creates the symlink and when it decides there is nothing to do.
        systemctl enable "$RECOVERY_UNIT_NAME" >/dev/null 2>&1 || true
    fi

    # Verify the enable instead of believing it, and repair it by hand when the
    # symlink is missing or dangling — the unit is worthless if nothing pulls it
    # into the boot transaction.
    if [ ! -L "$RECOVERY_WANTS_LINK" ] || [ ! -e "$RECOVERY_WANTS_LINK" ]; then
        mkdir -p "$WANTS_DIR"
        ln -sfn "$RECOVERY_UNIT_FILE" "$RECOVERY_WANTS_LINK"
        fsync_path "$WANTS_DIR"
        if [ -z "$ROOT_PREFIX" ]; then
            systemctl daemon-reload >/dev/null 2>&1 || true
        fi
    fi
    if [ ! -L "$RECOVERY_WANTS_LINK" ] || [ ! -e "$RECOVERY_WANTS_LINK" ]; then
        err "${RECOVERY_UNIT_NAME} is not wanted by ${RECOVERY_WANTED_BY}: ${RECOVERY_WANTS_LINK} is missing."
        err "Boot recovery would NOT run, so an interrupted update could not be repaired."
        return 1
    fi
    return 0
}

# Remove every temp file the two atomic writers can leave behind. Staging
# leftovers are always garbage: the commit either renamed them into place or
# never got that far. The named-destination list is derived from the same
# constants the writers use; the two globs cover the destinations whose file
# names come from the package (units, tools, config samples) and are anchored on
# the .cn-new suffix, which nothing but this script produces.
recovery_sweep_staging() {
    local b f
    local -a staged=(
        "$(stage_name_of "$MARKER_FILE")"
        "$(stage_name_of "$MANIFEST_FILE")"
        "$(stage_name_of "$TMPFILES_FILE")"
        "$(stage_name_of "$AVAHI_SERVICE_FILE")"
        "$(stage_name_of "$RECOVERY_UNIT_FILE")"
        "$(stage_name_of "$RECOVERY_ENTRY_POINT")"
    )
    for b in "${CASANOVA_BINARIES[@]}"; do
        staged+=("$(stage_name_of "${BIN_DIR}/${b}")")
    done
    for f in "${staged[@]}"; do
        rm -f "$f"
    done
    for f in "${UNIT_DIR}"/.*.cn-new "${TOOLS_DIR}"/.*.cn-new "${ETC_DIR}"/.*.cn-new; do
        [ -f "$f" ] || continue
        rm -f "$f"
    done
    # Database restores stage next to the live .db, so a cut during one leaves a
    # temp in the data directory. Swept here for the same reason as the rest:
    # nothing but this script writes that suffix, and a leftover is always
    # garbage — the rename either happened or never will.
    for b in "${DB_SOURCE_DIRS[@]}"; do
        for f in "${ROOT_PREFIX}/${b}"/.*.cn-new; do
            [ -f "$f" ] || continue
            rm -f "$f"
        done
    done
}

# Undo the parts of an apply that live outside bin/, the units and the frontend,
# and pull out what the package introduced.
#
# Restoring only the files a backup happens to contain is a MERGE, not a
# restore: whatever the new package added survives it. The manifest is the one
# that hurts — the UI and the update check read the version from there, so a box
# left with the new manifest over the old binaries reports a version it is not
# running and is never offered the update again.
#
# Shared by boot recovery and the online rollback so both end at the same,
# non-mixed state. Failures are NOT swallowed: under boot recovery they abort
# with the marker still armed, so the next boot retries (every step here is
# idempotent); the online rollback runs with `set +e` and pushes on to the
# service restart, which is what matters there.
restore_backup_extras() {
    local backup="$1"
    local quarantine="${backup}/quarantine"
    local complete=false
    local -a census=()
    local b t u name

    # The census is what tells a restore from a merge: without it (a backup taken
    # by an older updater, or one that never finished) files may be put back but
    # nothing may be concluded to be new. Conserving too much is the correct
    # failure here.
    if [ -f "${backup}/${BACKUP_CENSUS_NAME}" ]; then
        while IFS= read -r name; do
            if [ -n "$name" ]; then
                census+=("$name")
            fi
        done < <(census_names "$backup")
    fi
    if [ "${#census[@]}" -gt 0 ]; then
        complete=true
    else
        warn "Pre-update backup has no census — restoring files only, nothing will be removed."
    fi

    if [ -f "${backup}/manifest.json" ]; then
        atomic_install "${backup}/manifest.json" "$MANIFEST_FILE" 0644
        info "Restored the pre-update manifest"
    elif [ "$complete" = true ] && [ -f "$MANIFEST_FILE" ]; then
        rm -f "$MANIFEST_FILE"
        warn "Removed the new manifest: this box had none before the update."
    fi

    if [ -f "${backup}/tmpfiles.d/casanova.conf" ]; then
        atomic_install "${backup}/tmpfiles.d/casanova.conf" "$TMPFILES_FILE" 0644
        # Re-assert the old runtime dirs: systemd-tmpfiles-setup already ran with
        # the new file, and the services about to start expect the directories
        # the old one declares.
        if command -v systemd-tmpfiles >/dev/null 2>&1; then
            host_cmd systemd-tmpfiles --create "$TMPFILES_FILE" || true
        fi
    elif [ "$complete" = true ] && [ -f "$TMPFILES_FILE" ]; then
        rm -f "$TMPFILES_FILE"
    fi

    for t in "${backup}"/tools/*.sh; do
        [ -f "$t" ] || continue
        name=$(basename "$t")
        # The updater itself is deliberately NOT rolled back. Boot recovery is
        # running from it, and the pre-update copy can predate --boot-recovery
        # entirely: restoring it would leave a box whose recovery unit executes a
        # script that cannot recover. It carries no version identity either — the
        # manifest above does.
        if [ "$name" = "$(basename "$RECOVERY_ENTRY_POINT")" ]; then
            continue
        fi
        atomic_install "$t" "${TOOLS_DIR}/${name}" 0755
    done

    # Anything installed that the census proves was NOT there before the update
    # was introduced by the package being undone. Moved into the backup rather
    # than deleted: a census can be wrong across versions, and an operator can
    # always put a file back — nobody can put back a deleted one.
    if [ "$complete" = true ] && [ -f "${backup}/bin/casanova" ]; then
        for b in "${census[@]}"; do
            [ -e "${BIN_DIR}/${b}" ] || continue
            [ -f "${backup}/bin/${b}" ] && continue
            mkdir -p "${quarantine}/bin"
            if mv -f "${BIN_DIR}/${b}" "${quarantine}/bin/${b}"; then
                warn "Quarantined ${BIN_DIR}/${b}: the interrupted update introduced it."
            else
                warn "Could not quarantine ${BIN_DIR}/${b} — leaving it in place."
            fi
        done
    fi
    # Units are swept only when the backup demonstrably holds the unit set: the
    # Phase 2 copy swallows its errors, and an empty systemd/ dir must never be
    # read as "the box had no units".
    if [ "$complete" = true ] && [ -f "${backup}/systemd/casanova.service" ]; then
        for u in "${UNIT_DIR}"/casanova*.service; do
            [ -f "$u" ] || continue
            name=$(basename "$u")
            # The recovery unit is installed AFTER the backup is taken, so on a
            # first update it is legitimately absent from it. Quarantining it
            # would disarm the mechanism running this very repair.
            [ "$name" = "$RECOVERY_UNIT_NAME" ] && continue
            [ -f "${backup}/systemd/${name}" ] && continue
            mkdir -p "${quarantine}/systemd"
            if mv -f "$u" "${quarantine}/systemd/${name}"; then
                # Exact path, never a glob: leaving the enable symlink behind
                # would make systemd fail to load a unit that no longer exists.
                rm -f "${WANTS_DIR}/${name}"
                warn "Quarantined ${u}: the interrupted update introduced it."
            else
                warn "Could not quarantine ${u} — leaving it in place."
            fi
        done
    fi

    # Config files the apply CREATED. Contents are never restored: the apply
    # never overwrites a config, so the only thing a rollback owes here is
    # withdrawing what the package introduced — leaving them behind is how a box
    # ends up running old binaries against config keys only the new ones read.
    # The list is what Phase 4 recorded creating, so nothing is inferred and an
    # operator's own file is never touched.
    if [ -f "${backup}/${CREATED_CONFIGS_NAME}" ]; then
        while IFS= read -r name; do
            [ -n "$name" ] || continue
            [ -f "${ETC_DIR}/${name}" ] || continue
            mkdir -p "${quarantine}/etc-casanova"
            if mv -f "${ETC_DIR}/${name}" "${quarantine}/etc-casanova/${name}"; then
                warn "Withdrew ${ETC_DIR}/${name}: the update introduced it."
            else
                warn "Could not withdraw ${ETC_DIR}/${name} — leaving it in place."
            fi
        done < "${backup}/${CREATED_CONFIGS_NAME}"
    fi
}

# ─── Boot recovery ───────────────────────────────────────────────────────────
# Runs from the oneshot above, ordered before every CasaNova service. Safe to
# run twice: each step is either a no-op or an idempotent restore.
boot_recovery() {
    local phase backup from_v to_v b u restored=0

    if [ ! -f "$MARKER_FILE" ]; then
        info "No interrupted-update marker — nothing to recover."
        return 0
    fi

    phase=$(marker_field phase)
    backup=$(marker_field backup_dir)
    from_v=$(marker_field from_version)
    to_v=$(marker_field to_version)
    warn "Interrupted update detected (phase=${phase:-unknown}, ${from_v:-?} → ${to_v:-?})"

    recovery_sweep_staging

    normalize_www

    if [ "$phase" = "committed" ]; then
        # Every file reached its final name before the power went, so the box is
        # a complete install of the new version — only the post-apply health
        # check never ran. Reverting here would throw away a good update, so we
        # just drop the staging leftovers and stand down.
        info "Update files were fully committed — keeping ${to_v:-the new version}."
        rm -rf "$WWW_STAGE_DIR" "$WWW_RETIRED_DIR"
        clear_marker
        return 0
    fi

    # Died mid-commit: the installed binaries are a MIX of old and new, which is
    # the one state nothing on this box is designed to survive. Restore the
    # pre-update backup wholesale — a proven-bootable set beats a half-applied
    # one, and the operator can simply retry the update.
    if [ -z "$backup" ] || [ ! -d "${backup}/bin" ]; then
        err "Interrupted update, but the pre-update backup is unusable (${backup:-none})."
        err "Refusing to touch the installation, and KEEPING the marker so this stays visible."
        report_status "failed" 0 "Interrupted update: the pre-update backup is unusable — nothing was changed"
        return 0
    fi

    # Prove the backup before installing any of it. The same conservation rule as
    # the branch above, extended from "the directory exists" to "its contents are
    # the ones Phase 2 recorded": a restore that installs a truncated binary
    # bricks the box permanently, and clearing the marker afterwards would
    # disarm the only mechanism that could still notice.
    if ! backup_restore_gate "$backup"; then
        err "Refusing to restore from ${backup} — the installation is left exactly as found."
        err "The marker is KEPT, so this is retried on the next boot and stays visible to the operator."
        report_status "failed" 0 "Interrupted update: the pre-update backup failed verification — nothing was restored"
        return 0
    fi

    report_status "restarting" 50 "Recovering from an interrupted update..."

    for b in "${backup}"/bin/*; do
        [ -f "$b" ] || continue
        atomic_install "$b" "${BIN_DIR}/$(basename "$b")" 0755
        restored=$((restored + 1))
    done
    info "Restored ${restored} binaries from ${backup} (${RESTORE_TRUST})"

    for u in "${backup}"/systemd/*.service; do
        [ -f "$u" ] || continue
        atomic_install "$u" "${UNIT_DIR}/$(basename "$u")" 0644
    done

    restore_backup_extras "$backup"

    # Nothing has started yet — this unit is ordered before every CasaNova
    # service — so the databases are quiescent and safe to put back.
    restore_databases "$backup" || warn "Databases were NOT restored; the old binaries may see a newer schema."

    # The pre-update frontend copy is made with `|| true`, so it may itself be
    # truncated. Only swap it in when the recorded file count still matches:
    # keeping a possibly-newer UI beats overwriting a working one with a partial
    # backup, and the mismatch is loud in the journal either way.
    if [ -d "${backup}/www" ]; then
        local want have
        want=$(cat "${backup}/www.count" 2>/dev/null || echo "")
        have=$( { find "${backup}/www" -type f 2>/dev/null || true; } | wc -l )
        if [ -n "$want" ] && [ "$want" = "$have" ]; then
            stage_www "${backup}/www"
            swap_www
            info "Restored frontend (${have} files)"
        else
            warn "Pre-update frontend backup is incomplete (${have}/${want:-?} files) — keeping the tree on disk."
        fi
    fi
    # Whatever survived of the interrupted swap is garbage once the live tree is
    # settled (normalize_www above guarantees it exists if it ever did).
    rm -rf "$WWW_STAGE_DIR" "$WWW_RETIRED_DIR"

    atomic_write "${backup}/boot-recovery.json" 0644 << EOF || warn "Could not write the recovery breadcrumb into ${backup}"
{
  "recovered_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "phase_at_crash": "$(json_escape "${phase:-unknown}")",
  "restored_binaries": ${restored},
  "backup_trust": "$(json_escape "${RESTORE_TRUST:-unknown}")",
  "from_version": "$(json_escape "${from_v:-unknown}")",
  "to_version": "$(json_escape "${to_v:-unknown}")"
}
EOF

    host_cmd systemctl daemon-reload || true

    clear_marker
    err "Recovered from an interrupted update — rolled back to ${from_v:-the previous version}."
}

# ─── Argument parsing ────────────────────────────────────────────────────────
for arg in "$@"; do
    case "$arg" in
        --status-file=*) STATUS_FILE="${arg#*=}" ;;
        --verify-only)   VERIFY_ONLY=true ;;
        --boot-recovery) BOOT_RECOVERY=true ;;
        --install-recovery-unit) INSTALL_RECOVERY_ONLY=true ;;
        --url=*)         PACKAGE_FILE="${arg#*=}" ;; # online: download then apply
        --help|-h)
            echo "CasaNova Self-Update Engine"
            echo ""
            echo "Usage:"
            echo "  sudo bash $0 /path/to/update.tar.gz            # offline (local package)"
            echo "  sudo bash $0 --url=https://host/update.tar.gz  # online (download + verify)"
            echo "  sudo bash $0 --status-file=/var/run/casanova/update-status.json /path/to/update.tar.gz"
            echo "  sudo bash $0 --verify-only /path/to/update.tar.gz"
            echo "  sudo bash $0 --boot-recovery                   # repair an update that died half-applied"
            echo "  sudo bash $0 --install-recovery-unit           # (re)install the boot recovery oneshot"
            exit 0
            ;;
        --*) ;; # skip unknown flags
        *)
            # Positional arg = package file
            if [ -z "$PACKAGE_FILE" ]; then
                PACKAGE_FILE="$arg"
            fi
            ;;
    esac
done

# Maintenance modes take no package and must work on a box whose last update
# died half-applied, so they run before every package check below.
if [ "$BOOT_RECOVERY" = true ] || [ "$INSTALL_RECOVERY_ONLY" = true ]; then
    if [ "$(id -u)" -ne 0 ] && [ -z "$ROOT_PREFIX" ]; then
        err "This script must be run as root."
        exit 1
    fi
    if [ "$INSTALL_RECOVERY_ONLY" = true ]; then
        if ! ensure_boot_recovery; then
            err "Boot recovery could NOT be armed."
            exit 1
        fi
        info "Boot recovery unit installed: ${RECOVERY_UNIT_FILE}"
        info "Wanted by ${RECOVERY_WANTED_BY}: ${RECOVERY_WANTS_LINK}"
    fi
    if [ "$BOOT_RECOVERY" = true ]; then
        boot_recovery
    fi
    exit 0
fi

if [ -z "$PACKAGE_FILE" ]; then
    err "No package file specified."
    err "Usage: sudo bash $0 /path/to/casanova-update.tar.gz  (or --url=https://…)"
    exit 1
fi

# ─── Exit contract ───────────────────────────────────────────────────────────
# Armed BEFORE anything is downloaded or extracted, because the two things that
# must hold on every exit path are older than any of the phases below:
#   * the caller is told the attempt ended. The backend keeps replaying the last
#     non-terminal stage it saw, so a run that dies in Phase 1 or 2 used to pin
#     the UI on "Backing up current installation..." forever, with no way to
#     clear it short of another update.
#   * no extraction tree is left in /tmp. Each abandoned one is the size of the
#     unpacked package (hundreds of MiB), and they accumulate per attempt —
#     filling the filesystem that the next attempt's backup has to fit in.
# The rollback trap installed at the end of Phase 2 handles what happens to the
# INSTALLATION; this one only guarantees the two invariants above.
EXTRACT_DIR=""
DL=""
on_exit() {
    local rc=$?
    trap - EXIT
    if [ "$rc" -ne 0 ] && [ "$FINAL_STATUS_REPORTED" != true ]; then
        report_status "failed" 0 "Update aborted before reaching a result (exit ${rc}) — see the journal for the failing step" || true
    fi
    if [ -n "$EXTRACT_DIR" ]; then
        rm -rf "$EXTRACT_DIR" || true
    fi
    if [ -n "$DL" ]; then
        rm -f "$DL" "${DL}.sig" || true
    fi
    return $rc
}
trap on_exit EXIT
# A stop/interrupt must run the trap above rather than skipping straight past
# it: systemd sends SIGTERM when the operator cancels the transient unit.
trap 'exit 143' TERM
trap 'exit 130' INT

# Online mode: a URL is downloaded to a temp file and then applied exactly like
# a local package. Plain http is refused: the only thing standing between this
# script and an attacker-supplied set of root binaries would be a checksum list
# that travels inside the very archive being fetched.
case "$PACKAGE_FILE" in
    http://*)
        err "Refusing to fetch an update over plain http: ${PACKAGE_FILE}"
        err "The package's checksums live inside the package, so an unencrypted"
        err "transfer offers no protection at all. Use https, or download it first."
        report_status "failed" 0 "Refused an update URL that is not https"
        exit 1
        ;;
esac
case "$PACKAGE_FILE" in
    https://*)
        DL="$(mktemp /tmp/casanova-update-XXXXXX.tar.gz)"
        info "Downloading update: ${PACKAGE_FILE}"
        if ! curl -fSL --retry 3 --connect-timeout 15 -o "$DL" "$PACKAGE_FILE"; then
            err "Download failed: ${PACKAGE_FILE}"
            report_status "failed" 0 "Download failed"
            exit 1
        fi
        # Detached signature, when this box has a key to check it with. Fetched
        # here so the local and the online path reach the same verification
        # below with the same file layout.
        if [ -f "$SIGNING_KEY_FILE" ]; then
            if ! curl -fSL --retry 3 --connect-timeout 15 -o "${DL}.sig" "${PACKAGE_FILE}.sig"; then
                err "No signature at ${PACKAGE_FILE}.sig, but ${SIGNING_KEY_FILE} demands one."
                report_status "failed" 0 "Update package is not signed"
                exit 1
            fi
        fi
        PACKAGE_FILE="$DL"
        ;;
esac

if [ ! -f "$PACKAGE_FILE" ]; then
    err "Package file not found: ${PACKAGE_FILE}"
    exit 1
fi

# Root check. Same exemption as the maintenance modes above: with ROOT_PREFIX
# set every path written lands inside the sandbox it points at and every
# host-daemon call goes through host_cmd, so the run needs no privilege — that
# is what lets the crash tests drive a whole apply as an ordinary user.
if [ "$(id -u)" -ne 0 ] && [ -z "$ROOT_PREFIX" ]; then
    err "This script must be run as root."
    exit 1
fi

###############################################################################
# Phase 1: Extract and validate package
###############################################################################
step "Phase 1/6: Validating update package"
report_status "verifying" 5 "Extracting update package..."

# ─── Authenticity ────────────────────────────────────────────────────────────
# The checksum list lives INSIDE the archive, so it proves the archive is
# internally consistent and nothing else — whoever builds a package writes its
# checksums. A detached signature is the only thing that says who built it, and
# it can only be checked against a key this box already trusts.
#
# Enforcement is therefore keyed on that file existing: install the project key
# and every package must be signed; without it, say plainly that the package is
# trusted for having reached this box, and do not pretend otherwise.
#
# Contract for whoever produces the signature: <package>.tar.gz.sig is a
# detached signature over the package's sha256 rendered as 64 lowercase hex
# characters with NO trailing newline. Ed25519 and RSA/ECDSA keys both work.
if [ -f "$SIGNING_KEY_FILE" ]; then
    if ! command -v openssl >/dev/null 2>&1; then
        err "${SIGNING_KEY_FILE} demands a signed package, but openssl is not installed."
        report_status "failed" 0 "Cannot verify the package signature: openssl is missing"
        exit 1
    fi
    if [ ! -f "${PACKAGE_FILE}.sig" ]; then
        err "No detached signature at ${PACKAGE_FILE}.sig, but ${SIGNING_KEY_FILE} demands one."
        report_status "failed" 0 "Update package is not signed"
        exit 1
    fi
    # The signed message is the tarball's sha256, written as 64 lowercase hex
    # characters with no trailing newline — small enough that an Ed25519 key
    # (which openssl can only verify over a whole in-memory message) costs the
    # same as an RSA one, instead of buffering a 300+ MiB archive.
    SIG_MSG="$(mktemp /tmp/casanova-update-sig-XXXXXX)"
    printf '%s' "$(sha256sum "$PACKAGE_FILE" | awk '{print $1}')" > "$SIG_MSG"
    # Ed25519 keys cannot be used with `dgst -sha256`, and RSA/ECDSA keys cannot
    # be used with `pkeyutl -rawin`. Both invocations check the same key against
    # the same signature over the same message, so trying each in turn accepts
    # either key type without weakening what is proven.
    SIG_OK=false
    if openssl pkeyutl -verify -pubin -inkey "$SIGNING_KEY_FILE" -rawin \
            -in "$SIG_MSG" -sigfile "${PACKAGE_FILE}.sig" >/dev/null 2>&1; then
        SIG_OK=true
    elif openssl dgst -sha256 -verify "$SIGNING_KEY_FILE" \
            -signature "${PACKAGE_FILE}.sig" "$SIG_MSG" >/dev/null 2>&1; then
        SIG_OK=true
    fi
    rm -f "$SIG_MSG"
    if [ "$SIG_OK" != true ]; then
        err "Package signature does NOT verify against ${SIGNING_KEY_FILE}."
        err "Refusing to install binaries of unknown origin."
        report_status "failed" 0 "Update package signature is invalid"
        exit 1
    fi
    info "Package signature verified against ${SIGNING_KEY_FILE}"
else
    warn "Package origin is NOT verified: no signing key at ${SIGNING_KEY_FILE}."
    warn "The checksums below only prove the archive is internally consistent."
fi

EXTRACT_DIR=$(mktemp -d /tmp/casanova-update-XXXXXX)

info "Extracting: ${PACKAGE_FILE}"
if ! tar -xzf "$PACKAGE_FILE" -C "$EXTRACT_DIR" 2>/dev/null; then
    err "Failed to extract package. File may be corrupt."
    rm -rf "$EXTRACT_DIR"
    report_status "failed" 0 "Failed to extract package"
    exit 1
fi

# Find the package root (may be flat or nested in a single dir)
PKG_ROOT=""
if [ -f "${EXTRACT_DIR}/bin/casanova" ]; then
    PKG_ROOT="$EXTRACT_DIR"
elif [ -d "${EXTRACT_DIR}"/casanova-*/bin ]; then
    PKG_ROOT=$(ls -d "${EXTRACT_DIR}"/casanova-* | head -1)
else
    err "Invalid package structure — expected bin/casanova inside archive."
    rm -rf "$EXTRACT_DIR"
    report_status "failed" 0 "Invalid package structure"
    exit 1
fi

info "Package root: ${PKG_ROOT}"

# Read package metadata
NEW_VERSION="unknown"
PKG_CHANNEL="unknown"
PKG_ARCH="unknown"
if [ -f "${PKG_ROOT}/update-meta.json" ]; then
    NEW_VERSION=$(python3 -c "import json; print(json.load(open('${PKG_ROOT}/update-meta.json'))['version'])" 2>/dev/null || echo "unknown")
    PKG_CHANNEL=$(python3 -c "import json; print(json.load(open('${PKG_ROOT}/update-meta.json'))['channel'])" 2>/dev/null || echo "unknown")
    PKG_ARCH=$(python3 -c "import json; print(json.load(open('${PKG_ROOT}/update-meta.json'))['architecture'])" 2>/dev/null || echo "unknown")
    info "Package version: ${NEW_VERSION} (${PKG_CHANNEL}, ${PKG_ARCH})"
elif [ -f "${PKG_ROOT}/manifest.json" ]; then
    NEW_VERSION=$(python3 -c "import json; print(json.load(open('${PKG_ROOT}/manifest.json'))['version'])" 2>/dev/null || echo "unknown")
    PKG_ARCH=$(python3 -c "import json; print(json.load(open('${PKG_ROOT}/manifest.json'))['arch'])" 2>/dev/null || echo "unknown")
    info "Package version: ${NEW_VERSION} (from manifest)"
fi
# Metadata is package-controlled text that ends up in a JSON report and in log
# lines. Keep it to characters that cannot break either: anything else is a
# packaging bug, and "unknown" is a safe thing to report.
for meta_var in NEW_VERSION PKG_CHANNEL PKG_ARCH; do
    case "${!meta_var}" in
        *[!A-Za-z0-9._+-]*|"")
            warn "Ignoring an unusable ${meta_var} in the package metadata."
            printf -v "$meta_var" '%s' "unknown"
            ;;
    esac
done

report_status "verifying" 15 "Validating checksums..."

# ─── Integrity ───────────────────────────────────────────────────────────────
# Mandatory, and complete. Three holes used to make this decorative:
#   * a package without the list simply skipped verification ("skipping"), so
#     omitting one file defeated the whole check;
#   * a listed file that was absent from the archive was passed over in silence,
#     so a package missing half its binaries verified with "0 failures";
#   * a file present in the archive but ABSENT from the list was never looked
#     at, which is where an extra file would hide.
# All three now fail the update. Every package the packager produces carries a
# list generated over its whole tree, so this rejects nothing legitimate.
if [ ! -f "${PKG_ROOT}/checksums.sha256" ]; then
    err "Package has no checksums.sha256 — refusing to install unverifiable files."
    report_status "failed" 0 "Update package carries no checksum list"
    exit 1
fi

info "Verifying checksums..."
CHECKSUM_FAIL=0
CHECKSUM_TOTAL=0
while IFS= read -r csline || [ -n "$csline" ]; do
    [ -n "$csline" ] || continue
    # sha256sum's own format: 64 hex, two separator characters, then the name.
    # Parsed by offset rather than by splitting on spaces, because a name
    # containing a space would otherwise be truncated and read as "missing" —
    # and missing is now a hard failure. A line starting with '\' is sha256sum's
    # escaped form (backslash or newline in the name).
    if [ "${csline:0:1}" = "\\" ]; then
        csline="${csline:1}"
        expected_hash="${csline:0:64}"
        filepath="${csline:66}"
        filepath="${filepath//\\\\/\\}"
    else
        expected_hash="${csline:0:64}"
        filepath="${csline:66}"
    fi
    filepath="${filepath#./}"
    CHECKSUM_TOTAL=$((CHECKSUM_TOTAL + 1))
    if [ ! -f "${PKG_ROOT}/${filepath}" ]; then
        err "  Checksum entry has NO FILE in the package: ${filepath}"
        CHECKSUM_FAIL=$((CHECKSUM_FAIL + 1))
        continue
    fi
    actual_hash=$(sha256sum "${PKG_ROOT}/${filepath}" | awk '{print $1}')
    if [ "$expected_hash" != "$actual_hash" ]; then
        err "  Checksum MISMATCH: ${filepath}"
        CHECKSUM_FAIL=$((CHECKSUM_FAIL + 1))
    fi
done < "${PKG_ROOT}/checksums.sha256"

# The packager excludes exactly these two from the list it generates.
PKG_FILE_COUNT=$( { find "$PKG_ROOT" -type f \
    ! -name 'checksums.sha256' ! -name 'update-meta.json' 2>/dev/null || true; } | wc -l)
if [ "$PKG_FILE_COUNT" -ne "$CHECKSUM_TOTAL" ]; then
    err "Package holds ${PKG_FILE_COUNT} files but only ${CHECKSUM_TOTAL} are checksummed."
    err "Every file that gets installed has to be covered by the list."
    CHECKSUM_FAIL=$((CHECKSUM_FAIL + 1))
fi

if [ "$CHECKSUM_FAIL" -gt 0 ]; then
    err "${CHECKSUM_FAIL} checksum problem(s) across ${CHECKSUM_TOTAL} entries."
    report_status "failed" 0 "Checksum verification failed"
    exit 1
fi
info "All ${CHECKSUM_TOTAL} checksums verified"

# ─── Architecture ────────────────────────────────────────────────────────────
# Checked BEFORE anything is stopped or replaced. The health gate would catch an
# ELF that cannot run, but only after taking all ten services down and putting
# the box through a full rollback.
HOST_ARCH=$(uname -m)
if [ -n "${PKG_ARCH:-}" ] && [ "$PKG_ARCH" != "unknown" ]; then
    norm_arch() {
        case "$1" in
            x86_64|amd64)  echo "x86_64" ;;
            aarch64|arm64) echo "aarch64" ;;
            *)             echo "$1" ;;
        esac
    }
    if [ "$(norm_arch "$PKG_ARCH")" != "$(norm_arch "$HOST_ARCH")" ]; then
        err "Package is built for ${PKG_ARCH}, this machine is ${HOST_ARCH}."
        report_status "failed" 0 "Update package is for a different architecture (${PKG_ARCH})"
        exit 1
    fi
    info "Architecture: ${PKG_ARCH} matches ${HOST_ARCH}"
else
    warn "Package declares no architecture — cannot check it against ${HOST_ARCH}."
fi

# Validate required files exist
if [ ! -f "${PKG_ROOT}/bin/casanova" ]; then
    err "Package is missing bin/casanova"
    rm -rf "$EXTRACT_DIR"
    report_status "failed" 0 "Missing bin/casanova"
    exit 1
fi
BIN_COUNT=$(ls "${PKG_ROOT}/bin/" 2>/dev/null | wc -l)
info "Package contains ${BIN_COUNT} binaries"

report_status "verifying" 25 "Package validated: ${NEW_VERSION}"
info "Package validation passed"

# If --verify-only, stop here
if [ "$VERIFY_ONLY" = true ]; then
    echo ""
    info "Verification complete. Package is valid."
    info "  Version:  ${NEW_VERSION}"
    info "  Channel:  ${PKG_CHANNEL}"
    info "  Binaries: ${BIN_COUNT}"
    [ -d "${PKG_ROOT}/www" ] && info "  Frontend: $(find "${PKG_ROOT}/www" -type f | wc -l) files"
    [ -d "${PKG_ROOT}/systemd" ] && info "  Systemd:  $(ls "${PKG_ROOT}/systemd/"*.service 2>/dev/null | wc -l) units"
    rm -rf "$EXTRACT_DIR"
    exit 0
fi

###############################################################################
# Phase 2: Backup current installation
###############################################################################
step "Phase 2/6: Backing up current installation"
report_status "downloading" 30 "Backing up current installation..."

CURRENT_VERSION="unknown"
[ -x "${BIN_DIR}/casanova" ] && CURRENT_VERSION=$("${BIN_DIR}/casanova" -v 2>/dev/null || echo "unknown")
info "Current version: ${CURRENT_VERSION}"

mkdir -p "$UPGRADE_BACKUP_DIR"

# Save current version
echo "$CURRENT_VERSION" > "${UPGRADE_BACKUP_DIR}/version.txt"

# Backup binaries and units into the content census. Every copy is checked
# against its source, and a failure aborts the update HERE: nothing has been
# modified yet, so refusing costs an operator one retry, whereas continuing
# would mean applying an update whose rollback path is already broken.
CENSUS_PARTIAL="${UPGRADE_BACKUP_DIR}/${BACKUP_CENSUS_PARTIAL}"
rm -f "$CENSUS_PARTIAL" "${UPGRADE_BACKUP_DIR}/${BACKUP_CENSUS_NAME}"
: > "$CENSUS_PARTIAL"

mkdir -p "${UPGRADE_BACKUP_DIR}/bin"
BACKED_UP_BINS=0
for bin in "${CASANOVA_BINARIES[@]}"; do
    if [ -f "${BIN_DIR}/${bin}" ]; then
        if ! backup_capture "${BIN_DIR}/${bin}" "bin/${bin}" "$CENSUS_PARTIAL"; then
            err "Pre-update backup of ${bin} failed — refusing to apply ${NEW_VERSION}."
            err "Nothing has been changed on this box. Check free space on ${UPGRADE_BACKUP_BASE} and retry."
            report_status "failed" 0 "Could not back up ${bin} — update not applied"
            exit 1
        fi
        BACKED_UP_BINS=$((BACKED_UP_BINS + 1))
    else
        # Recorded as "covered but absent": that is what lets a rollback tell a
        # binary the package introduced from one that was always installed.
        census_add_absent "bin/${bin}" "$CENSUS_PARTIAL"
    fi
done
info "Backed up ${BACKED_UP_BINS} binaries (sha256 recorded)"

# Backup systemd units
mkdir -p "${UPGRADE_BACKUP_DIR}/systemd"
for unit in "${UNIT_DIR}"/casanova*.service; do
    [ -f "$unit" ] || continue
    if ! backup_capture "$unit" "systemd/$(basename "$unit")" "$CENSUS_PARTIAL"; then
        err "Pre-update backup of $(basename "$unit") failed — refusing to apply ${NEW_VERSION}."
        report_status "failed" 0 "Could not back up $(basename "$unit") — update not applied"
        exit 1
    fi
done

# Backup frontend
if [ -d "$WWW_DIR" ]; then
    mkdir -p "${UPGRADE_BACKUP_DIR}/www"
    # `/.` (not `/*`) so dotfiles come along — otherwise the count recorded
    # below could never match what boot recovery finds in the backup.
    cp -a "${WWW_DIR}/." "${UPGRADE_BACKUP_DIR}/www/" 2>/dev/null || true
    # The copy above swallows its errors, so record how many files the live tree
    # had: boot recovery needs a way to tell a complete backup from a truncated
    # one before it swaps a partial UI over a working one.
    { find "$WWW_DIR" -type f 2>/dev/null || true; } | wc -l > "${UPGRADE_BACKUP_DIR}/www.count"
    info "Backed up frontend"
fi

# Backup configs
if [ -d "$ETC_DIR" ]; then
    mkdir -p "${UPGRADE_BACKUP_DIR}/etc-casanova"
    cp -r "${ETC_DIR}"/* "${UPGRADE_BACKUP_DIR}/etc-casanova/" 2>/dev/null || true
fi

# Databases are NOT copied here. They are still open and in WAL mode at this
# point, so any copy taken now — tar included — captures the .db and its -wal at
# different instants. The snapshot is taken in Phase 3 instead, once every
# service is stopped, which is also the only state in which restoring it is
# safe. (Older backups on this box carry db-casaos.tar.gz / db-casanova.tar.gz
# from that live-copy era; nothing restores those, by design.)

# Every remaining thing the apply overwrites before the commit flip. Without
# these in the backup, boot recovery can only merge old binaries under new
# metadata — see the restore in boot_recovery.
if [ -f "$MANIFEST_FILE" ]; then
    cp "$MANIFEST_FILE" "${UPGRADE_BACKUP_DIR}/manifest.json" 2>/dev/null || true
fi
if [ -f "$TMPFILES_FILE" ]; then
    mkdir -p "${UPGRADE_BACKUP_DIR}/tmpfiles.d"
    cp "$TMPFILES_FILE" "${UPGRADE_BACKUP_DIR}/tmpfiles.d/" 2>/dev/null || true
fi
if [ -d "$TOOLS_DIR" ]; then
    mkdir -p "${UPGRADE_BACKUP_DIR}/tools"
    cp "${TOOLS_DIR}"/*.sh "${UPGRADE_BACKUP_DIR}/tools/" 2>/dev/null || true
fi

# Record running containers
host_cmd docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}' > "${UPGRADE_BACKUP_DIR}/running-containers.txt" 2>/dev/null || true

# Detect gateway port
GW_PORT="80"
if [ -f "${ETC_DIR}/gateway.ini" ]; then
    GW_PORT=$(grep -i '^port=' "${ETC_DIR}/gateway.ini" 2>/dev/null | cut -d= -f2 | tr -d ' ' || echo "80")
elif [ -f "${ETC_LEGACY_DIR}/gateway.ini" ]; then
    GW_PORT=$(grep -i '^port=' "${ETC_LEGACY_DIR}/gateway.ini" 2>/dev/null | cut -d= -f2 | tr -d ' ' || echo "80")
fi
[ -z "$GW_PORT" ] && GW_PORT="80"
echo "$GW_PORT" > "${UPGRADE_BACKUP_DIR}/.gateway-port"

# Save backup path for rollback
mkdir -p "$UPGRADE_BACKUP_BASE"
echo "$UPGRADE_BACKUP_DIR" > "$LAST_UPGRADE_FILE"

# Metadata
cat > "${UPGRADE_BACKUP_DIR}/backup-metadata.json" << EOF
{
  "type": "update",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "from_version": "$(json_escape "${CURRENT_VERSION}")",
  "to_version": "$(json_escape "${NEW_VERSION}")",
  "channel": "$(json_escape "${PKG_CHANNEL}")"
}
EOF

BACKUP_SIZE=$(du -sh "$UPGRADE_BACKUP_DIR" | awk '{print $1}')
info "Backup complete: ${UPGRADE_BACKUP_DIR} (${BACKUP_SIZE})"

# Flush the backup contents BEFORE the census that vouches for them. Arming the
# marker below points the next boot at this directory; if the power cut lands
# between the two, recovery finds no census and conserves rather than restoring
# from pages that never reached the platter.
sync

# Sealed last, by rename, so its presence proves every copy above finished and
# was verified. Boot recovery reads it as "this backup is a complete, checkable
# census of the pre-update install".
if ! mv -f "$CENSUS_PARTIAL" "${UPGRADE_BACKUP_DIR}/${BACKUP_CENSUS_NAME}"; then
    err "Could not seal the backup census — refusing to apply ${NEW_VERSION}."
    report_status "failed" 0 "Could not seal the pre-update backup census"
    exit 1
fi
fsync_path "$UPGRADE_BACKUP_DIR"
sync

# Arm the crash detector. From here until the marker is cleared, a box that
# comes back from a power cut knows it died mid-update and repairs itself
# instead of booting a mixed set of binaries (or no frontend at all).
# Hard gate: nothing below this line is safe on a box where the recovery unit
# would not run, and nothing above it has modified the installation yet, so
# refusing here leaves the box exactly as it was found.
if ! ensure_boot_recovery; then
    err "Refusing to apply ${NEW_VERSION}: interrupted-update recovery could not be armed."
    err "A power cut during the apply would leave this box on a mix of old and new binaries"
    err "with nothing to repair it. Nothing has been changed — fix the unit and retry."
    report_status "failed" 0 "Could not arm interrupted-update recovery"
    rm -rf "$EXTRACT_DIR"
    exit 1
fi
write_marker "applying"
info "Boot recovery armed: ${MARKER_FILE} (unit wanted by ${RECOVERY_WANTED_BY})"

report_status "downloading" 40 "Backup complete"

# Auto-rollback: restore the backup we just made if the apply or the
# post-apply health check fails — turns a broken half-applied update back
# into a working box without the operator running a manual rollback.
#
# What it actually achieved, for the report. "rolled_back" is only written after
# the restore really happened, so the report can never claim a rollback that was
# refused.
ROLLBACK_RESULT=""
rollback_to_backup() {
    # Disarm the ERR trap for the duration of the rollback: this function IS
    # the recovery path, so a transient non-zero here must NOT re-enter the
    # handler (infinite recursion) nor let `set -e` abort before the restart
    # loop — which would leave every service stopped (the exact outage this
    # tool exists to prevent).
    trap - ERR
    set +e
    err "Rolling back: ${1:-update failed}"
    report_status "restarting" 50 "Update failed — rolling back to previous version..."
    # Re-arm the marker BEFORE undoing anything. On the verification-failure path
    # it currently reads "committed", and a power cut half-way through this
    # function would then leave the next boot looking at a mixed set of binaries
    # that boot recovery reads as a finished install and refuses to touch.
    # Any phase other than "committed" makes it restore the backup wholesale,
    # which is exactly what this interrupted rollback was doing.
    write_marker "rolling-back" || warn "Could not re-arm the recovery marker for the rollback."
    for svc in "${CASANOVA_SERVICES_STOP[@]}"; do host_cmd systemctl stop "$svc" 2>/dev/null || true; done

    # Exactly the gate boot recovery uses, for exactly the same reason: putting
    # a backup copy that cannot be proven intact over a complete (if unhealthy)
    # installation trades a box that fails its health check for one that cannot
    # execute its own binaries. When it refuses, nothing is restored, the marker
    # stays armed, and the services are started again so the box is at least
    # reachable — the outcome is reported as rollback_failed, not rolled_back.
    if ! backup_restore_gate "$UPGRADE_BACKUP_DIR"; then
        ROLLBACK_RESULT="rollback_failed"
        err "Rollback ABORTED: ${CENSUS_PROBLEM:-the backup could not be verified}."
        err "Nothing was restored from ${UPGRADE_BACKUP_DIR}; the marker is kept armed."
        for svc in "${CASANOVA_SERVICES_START[@]}"; do host_cmd systemctl restart "$svc" 2>/dev/null || true; settle 1; done
        return 0
    fi

    if [ -d "${UPGRADE_BACKUP_DIR}/bin" ]; then
        for b in "${UPGRADE_BACKUP_DIR}"/bin/*; do
            [ -f "$b" ] || continue
            atomic_install "$b" "${BIN_DIR}/$(basename "$b")" 0755 || true
        done
    fi
    for u in "${UPGRADE_BACKUP_DIR}"/systemd/*.service; do
        [ -f "$u" ] || continue
        atomic_install "$u" "${UNIT_DIR}/$(basename "$u")" 0644 || true
    done
    # Same reason as in boot recovery: without this the box would run the old
    # binaries under the new manifest and report a version it is not running.
    restore_backup_extras "$UPGRADE_BACKUP_DIR"
    # The services are stopped, so the databases are quiescent: this is the only
    # window in which the pre-update snapshot can go back safely. Without it the
    # old binaries would run against a schema the new build already migrated.
    restore_databases "$UPGRADE_BACKUP_DIR" || warn "Databases were NOT restored; the old binaries may see a newer schema."
    # Stage + swap here too: the old `rm -rf www/*` then `cp -r` meant a crash
    # during the ROLLBACK left the box with no UI at all — the recovery path
    # must never be able to make things worse than the failure it is undoing.
    if [ -d "${UPGRADE_BACKUP_DIR}/www" ] && [ -n "$(ls -A "${UPGRADE_BACKUP_DIR}/www" 2>/dev/null)" ]; then
        if stage_www "${UPGRADE_BACKUP_DIR}/www"; then
            swap_www || true
        else
            warn "Could not stage the previous frontend — leaving the current one in place."
        fi
    fi
    host_cmd systemctl daemon-reload || true
    rm -f "${RUN_LEGACY_DIR}"/*.url 2>/dev/null || true
    # Start order restores the gateway first so management.url is regenerated
    # before the services that depend on it.
    for svc in "${CASANOVA_SERVICES_START[@]}"; do host_cmd systemctl restart "$svc" 2>/dev/null || true; settle 1; done
    # The box is back on the pre-update bits and internally consistent, so the
    # next boot has nothing to repair. Cleared last: if the rollback itself is
    # cut short, the marker survives and boot recovery finishes the job.
    ROLLBACK_RESULT="rolled_back"
    clear_marker || true
    err "Rollback complete — restored from ${UPGRADE_BACKUP_DIR} (${RESTORE_TRUST})."
}

# From here through the deploy, any failure (set -e) auto-rolls-back.
trap 'rollback_to_backup "apply error"; report_status "failed" 0 "Update failed and was rolled back"; rm -rf "$EXTRACT_DIR"; exit 3' ERR

###############################################################################
# Phase 3: Stop services
###############################################################################
step "Phase 3/6: Stopping CasaNova services"
report_status "applying" 45 "Stopping services..."

for svc in "${CASANOVA_SERVICES_STOP[@]}"; do
    if host_cmd systemctl is-active "$svc" &>/dev/null; then
        info "Stopping ${svc}..."
        host_cmd systemctl stop "$svc" 2>/dev/null || true
    fi
done
# Stop oneshot
host_cmd systemctl stop casanova-local-storage-first 2>/dev/null || true

info "All services stopped"

# Snapshot the databases now, and only now: nothing holds them open, so .db and
# its -wal are a matched pair. This is the artifact the rollback paths restore —
# the update's first start migrates the schema (and drops legacy tables), which
# is not something old binaries can undo by themselves.
# A failed snapshot does not abort the update: it costs data reversibility, not
# integrity, and the loud warning plus the missing manifest are what keep the
# rollback paths from restoring something they cannot vouch for.
snapshot_databases || warn "No database snapshot was taken — a rollback will not restore the databases."

report_status "applying" 55 "Services stopped"

###############################################################################
# Phase 4: Deploy new files
###############################################################################
step "Phase 4/6: Deploying ${NEW_VERSION}"
report_status "applying" 60 "Installing binaries..."

# Install binaries. Staged next to the target and renamed into place, so a
# power cut can never leave a truncated executable at /usr/bin/<name> (which
# used to mean an unbootable box). Renaming also sidesteps ETXTBSY entirely.
info "Installing binaries..."
for bin in "${PKG_ROOT}"/bin/*; do
    [ -f "$bin" ] || continue
    bname=$(basename "$bin")
    atomic_install "$bin" "${BIN_DIR}/${bname}" 0755
    echo "  ${BIN_DIR}/${bname}"
done

report_status "applying" 70 "Installing frontend..."

# Install systemd units
if [ -d "${PKG_ROOT}/systemd" ]; then
    info "Installing systemd units..."
    for unit in "${PKG_ROOT}"/systemd/casanova*.service; do
        [ -f "$unit" ] || continue
        atomic_install "$unit" "${UNIT_DIR}/$(basename "$unit")" 0644
    done
fi

# Install tmpfiles.d + apply immediately (runtime dirs for trusted sockets
# and the update staging dir must exist without waiting for a reboot).
if [ -f "${PKG_ROOT}/tmpfiles.d/casanova.conf" ]; then
    info "Installing tmpfiles.d..."
    atomic_install "${PKG_ROOT}/tmpfiles.d/casanova.conf" "$TMPFILES_FILE" 0644
    host_cmd systemd-tmpfiles --create "$TMPFILES_FILE" || true
fi

# Self-update the tools: without this, the copy in /usr/share/casanova/tools
# stays frozen at install time forever and script fixes never reach the box
# via the UI. Safe mid-run BECAUSE the install is a rename: bash keeps reading
# the inode it opened, so replacing the directory entry cannot make it execute
# half of the old script and half of the new one (overwriting in place could).
if [ -d "${PKG_ROOT}/tools" ]; then
    info "Installing updated tools..."
    mkdir -p "$TOOLS_DIR"
    for tool in "${PKG_ROOT}"/tools/*.sh; do
        [ -f "$tool" ] || continue
        atomic_install "$tool" "${TOOLS_DIR}/$(basename "$tool")" 0755
    done
fi

# Install frontend. Staged into a sibling directory and swapped in with two
# renames: the old `rm -rf www/*` + `cp -r` pair left the UI empty (or half
# copied) for the whole duration of the copy, and a power cut there meant the
# box came back serving nothing.
if [ -d "${PKG_ROOT}/www" ]; then
    info "Staging frontend..."
    stage_www "${PKG_ROOT}/www"
    info "Installing frontend..."
    swap_www
fi

# NEVER overwrite configs — only install new sample configs
# The user's configs in /etc/casanova/ are sacred.
if [ -d "${PKG_ROOT}/config" ]; then
    info "Checking for new config samples..."
    for sample in "${PKG_ROOT}"/config/*.sample; do
        [ -f "$sample" ] || continue
        target=$(basename "$sample" .sample)
        [ "$target" = "casaos.conf" ] && target="casanova.conf"
        if [ ! -f "${ETC_DIR}/${target}" ]; then
            atomic_install "$sample" "${ETC_DIR}/${target}" 0644
            # Recorded so a rollback can withdraw exactly what this apply added.
            # Left behind, these are old binaries reading a config written for
            # the new ones — and nothing else on the box knows they are new.
            printf '%s\n' "$target" >> "${UPGRADE_BACKUP_DIR}/${CREATED_CONFIGS_NAME}"
            info "  New config: ${ETC_DIR}/${target}"
        fi
    done
fi

# mDNS/DNS-SD advertisement so LAN clients discover the box (zimaos + generic
# http browsers). Static XML, written inline — the port tracks the real gateway
# port computed in Phase 2. ponytail: no packager change, re-asserted every update.
AVAHI_PORT="${GW_PORT:-85}"
if [ -d "$AVAHI_DIR" ] || command -v avahi-daemon >/dev/null 2>&1; then
    info "Publishing mDNS advertisement (port ${AVAHI_PORT})..."
    mkdir -p "$AVAHI_SERVICES_DIR"
    atomic_write "$AVAHI_SERVICE_FILE" 0644 << EOF
<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="yes">CasaNova on %h</name>
  <service>
    <type>_http._tcp</type>
    <port>${AVAHI_PORT}</port>
    <txt-record>vendor=casanova</txt-record>
    <txt-record>version=1.6.2</txt-record>
    <txt-record>path=/v2/zimaos/device/info</txt-record>
  </service>
  <service>
    <type>_zimaos._tcp</type>
    <port>${AVAHI_PORT}</port>
    <txt-record>vendor=casanova</txt-record>
    <txt-record>version=1.6.2</txt-record>
  </service>
</service-group>
EOF
    host_cmd systemctl enable avahi-daemon 2>/dev/null || true
    host_cmd systemctl reload-or-restart avahi-daemon 2>/dev/null || host_cmd systemctl start avahi-daemon 2>/dev/null || true
fi

# Copy manifest
if [ -f "${PKG_ROOT}/manifest.json" ]; then
    atomic_install "${PKG_ROOT}/manifest.json" "$MANIFEST_FILE" 0644
fi

# Reload systemd
host_cmd systemctl daemon-reload

# Every file is at its final name now. Flip the marker so a power cut from here
# on is read as "complete install, unverified" rather than "half applied": boot
# recovery then keeps the new version instead of reverting a good update.
write_marker "committed"

info "Deployment complete"
report_status "applying" 80 "Files deployed"

# Deploy succeeded. Past here, service-start hiccups are handled by the health
# check below (with retries + explicit rollback), not an instant ERR trap.
trap - ERR

###############################################################################
# Phase 5: Start services
###############################################################################
step "Phase 5/6: Starting CasaNova services"
report_status "restarting" 85 "Starting services..."

# Clean stale runtime files
rm -f "${RUN_LEGACY_DIR}"/*.url "${RUN_LEGACY_DIR}/management.url" 2>/dev/null || true

for svc in "${CASANOVA_SERVICES_START[@]}"; do
    info "Starting ${svc}..."
    host_cmd systemctl enable "$svc" 2>/dev/null || true
    if ! host_cmd systemctl start "$svc" 2>/dev/null; then
        warn "  ${svc} failed to start on first attempt"
    fi
    settle 2
done

# Retry failed services
for svc in "${CASANOVA_SERVICES_START[@]}"; do
    if [ "$(host_cmd systemctl is-active "$svc" 2>/dev/null)" != "active" ]; then
        info "  Retrying ${svc}..."
        host_cmd systemctl restart "$svc" 2>/dev/null || true
        settle 3
    fi
done

info "Services started"
report_status "restarting" 92 "Services restarted, verifying..."

###############################################################################
# Phase 6: Verify
###############################################################################
step "Phase 6/6: Verification"

settle 5  # Let services settle

SERVICES_OK=0
# The one question the UI asks after a failed update is WHICH service did not
# come up. That loop knew the answer and printed it to a log nobody reads; the
# report field the UI renders had no producer at all, so it was always empty.
FAILED_SVCS=()
for svc in "${CASANOVA_SERVICES_START[@]}"; do
    state=$(host_cmd systemctl is-active "$svc" 2>/dev/null || echo "inactive")
    if [ "$state" = "active" ]; then
        SERVICES_OK=$((SERVICES_OK + 1))
        info "  ${svc}: ${GREEN}active${NC}"
    else
        FAILED_SVCS+=("${svc} (${state})")
        err "  ${svc}: ${RED}${state}${NC}"
    fi
done

# JSON array, built here so the report heredoc stays a single writer.
SERVICES_FAILED_JSON="[]"
if [ "${#FAILED_SVCS[@]}" -gt 0 ]; then
    SERVICES_FAILED_JSON="["
    for entry in "${FAILED_SVCS[@]}"; do
        SERVICES_FAILED_JSON="${SERVICES_FAILED_JSON}\"$(json_escape "$entry")\", "
    done
    SERVICES_FAILED_JSON="${SERVICES_FAILED_JSON%, }]"
fi

# Gateway ping
GW_PING=false
if host_cmd curl -sf "http://localhost:${GW_PORT}/ping" &>/dev/null; then
    GW_PING=true
    info "Gateway ping: OK (port ${GW_PORT})"
else
    warn "Gateway ping: FAIL (port ${GW_PORT})"
fi

# Frontend
FRONTEND_OK=false
HTTP_CODE=$(host_cmd curl -sf -o /dev/null -w "%{http_code}" "http://localhost:${GW_PORT}/" 2>/dev/null || echo "000")
if [ "$HTTP_CODE" = "200" ]; then
    FRONTEND_OK=true
    info "Frontend: HTTP 200"
else
    warn "Frontend: HTTP ${HTTP_CODE}"
fi

# Docker containers
CONTAINERS_BEFORE=0
[ -f "${UPGRADE_BACKUP_DIR}/running-containers.txt" ] && CONTAINERS_BEFORE=$(wc -l < "${UPGRADE_BACKUP_DIR}/running-containers.txt")
# `|| true` inside the group: with pipefail a dead dockerd would abort the
# script HERE — after the commit flip and before the outcome is decided —
# leaving the marker armed and the UI stuck on "updating" forever.
CONTAINERS_AFTER=$( { host_cmd docker ps -q 2>/dev/null || true; } | wc -l)
info "Containers: ${CONTAINERS_BEFORE} before → ${CONTAINERS_AFTER} after"

# Determine result. Gate on the ACTUAL service count (tolerate 1 transient
# slow-starter) AND require the two that matter most — the main casanova
# service and the gateway — to be up. The old hardcoded ">=5 of 9" would
# report "completed" with the main service (and 3 others) down and skip the
# rollback entirely.
MIN_OK=$(( ${#CASANOVA_SERVICES_START[@]} - 1 ))
MAIN_OK=false; host_cmd systemctl is-active --quiet casanova 2>/dev/null && MAIN_OK=true
if [ "$SERVICES_OK" -ge "$MIN_OK" ] && [ "$MAIN_OK" = true ] && [ "$GW_PING" = true ]; then
    UPDATE_STATUS="completed"
    EXIT_CODE=0
    # Verified healthy: disarm the crash detector so the next boot leaves the
    # new version alone.
    clear_marker
elif [ "$SERVICES_OK" -ge "$MIN_OK" ] && [ "$MAIN_OK" = true ]; then
    UPDATE_STATUS="completed"
    EXIT_CODE=0
    clear_marker
    warn "Update applied but gateway ping failed — may need a moment"
else
    EXIT_CODE=3
    err "Update verification FAILED. ${SERVICES_OK}/${#CASANOVA_SERVICES_START[@]} services running (main active: ${MAIN_OK})."
    rollback_to_backup "verification failed — only ${SERVICES_OK}/${#CASANOVA_SERVICES_START[@]} services healthy (main active: ${MAIN_OK})"
    # Whatever the rollback actually managed. Reporting "rolled_back" when the
    # restore was refused would send the operator looking for the wrong problem.
    UPDATE_STATUS="${ROLLBACK_RESULT:-rollback_failed}"
    if [ "$UPDATE_STATUS" = "rolled_back" ]; then
        err "Auto-rolled back to the previous version — the box should be healthy again."
    else
        err "Auto-rollback could NOT run — this box is still on the new files and needs an operator."
    fi
fi

# Read the version LAST, after the branch above has decided and any rollback has
# already put its binaries back. Sampling it before that made every rolled-back
# update report the version it had just removed from disk.
INSTALLED_VER="unknown"
[ -x "${BIN_DIR}/casanova" ] && INSTALLED_VER=$("${BIN_DIR}/casanova" -v 2>/dev/null || echo "unknown")
info "Installed version: ${INSTALLED_VER}"

# Write final report. Every string field goes through json_escape: the versions
# and channel come from the package's own metadata and the backup path from the
# environment, and a stray quote here does not just corrupt a log — the backend
# hands backup_location from this file to a root script on rollback.
cat > "${UPGRADE_BACKUP_DIR}/update-report.json" << EOF
{
  "status": "$(json_escape "${UPDATE_STATUS}")",
  "from_version": "$(json_escape "${CURRENT_VERSION}")",
  "to_version": "$(json_escape "${NEW_VERSION}")",
  "installed_version": "$(json_escape "${INSTALLED_VER}")",
  "channel": "$(json_escape "${PKG_CHANNEL}")",
  "services_ok": ${SERVICES_OK},
  "services_total": ${#CASANOVA_SERVICES_START[@]},
  "services_failed": ${SERVICES_FAILED_JSON},
  "backup_trust": "$(json_escape "${RESTORE_TRUST}")",
  "gateway_port": ${GW_PORT},
  "gateway_ping": ${GW_PING},
  "frontend_ok": ${FRONTEND_OK},
  "containers_before": ${CONTAINERS_BEFORE},
  "containers_after": ${CONTAINERS_AFTER},
  "backup_location": "$(json_escape "${UPGRADE_BACKUP_DIR}")",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

# Report final status to the backend
if [ "$UPDATE_STATUS" = "completed" ]; then
    report_status "completed" 100 "Update to ${INSTALLED_VER} complete."
    echo ""
    info "Update successful! CasaNova ${INSTALLED_VER} is running."
    info "Report: ${UPGRADE_BACKUP_DIR}/update-report.json"
    info "Rollback: sudo bash casaos-to-casanova.sh --rollback-upgrade"
elif [ "$UPDATE_STATUS" = "rolled_back" ]; then
    report_status "failed" 0 "Update failed (${SERVICES_OK}/${#CASANOVA_SERVICES_START[@]} services running) — rolled back to ${INSTALLED_VER}"
else
    report_status "failed" 0 "Update failed and could NOT be rolled back — the box is on ${INSTALLED_VER} and needs an operator"
fi

# Cleanup (the exit trap repeats this — it also covers the paths that never
# reach here).
rm -rf "$EXTRACT_DIR"

exit $EXIT_CODE
