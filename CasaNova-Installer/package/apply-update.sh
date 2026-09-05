#!/usr/bin/env bash
# CasaNova quick-update — rolling restart to minimize downtime.
# The gateway stays alive while backend services update, so Cloudflare
# tunnels don't go into backoff retry. Total downtime: ~3 seconds
# (just the gateway binary swap at the end).
# Usage: sudo bash apply-update.sh
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

WWW_DIR="/var/lib/casanova/www"
WWW_STAGING="${WWW_DIR}.cn-new"
WWW_BACKUP="${WWW_DIR}.cn-old"
BIN_STAGING="/usr/bin/.cn-update-new"
BIN_BACKUP="/usr/bin/.cn-update-old"
rm -rf "$WWW_STAGING" "$WWW_BACKUP" "$BIN_STAGING" "$BIN_BACKUP"

SERVICES_STOPPED=false

# On any failure from here on, undo whatever this run has swapped in and put
# the box back exactly the way it was running. A failed quick-update used to
# just stop: the frontend swap (`rm -rf www/*` + `cp -r`) had no going back
# once started, and a binary that failed mid-`cp` (after its old copy had
# already been `rm -f`'d) left the box with that service permanently missing
# and every non-gateway service stopped, with no path back except a manual
# reinstall. `return`-safe is not a concern here — this only ever runs while
# the script's own commands are in flight, driven by `trap ... ERR` below.
rollback() {
    trap - ERR
    echo "!!! Update failed — rolling back to the previous binaries/frontend." >&2

    if [ -d "$WWW_BACKUP" ]; then
        rm -rf "$WWW_DIR"
        mv "$WWW_BACKUP" "$WWW_DIR"
    fi
    if [ -d "$BIN_BACKUP" ]; then
        for old in "$BIN_BACKUP"/*; do
            [ -e "$old" ] || continue
            mv -f "$old" "/usr/bin/$(basename "$old")"
        done
    fi
    rm -rf "$WWW_STAGING" "$BIN_STAGING" "$BIN_BACKUP"

    if [ "$SERVICES_STOPPED" = true ]; then
        systemctl restart casanova-gateway 2>/dev/null || true
        sleep 2
        systemctl start casanova-message-bus 2>/dev/null || true
        sleep 1
        systemctl start casanova-user-service casanova-local-storage 2>/dev/null || true
        sleep 1
        systemctl start casanova-app-management casanova-job-management casanova 2>/dev/null || true
    fi
    echo "Rollback complete — previous version restored and services restarted." >&2
    exit 1
}
trap rollback ERR

echo "[1/5] Staging frontend + systemd units..."
mkdir -p "$WWW_STAGING"
cp -a "$DIR"/www/. "$WWW_STAGING"/
sync "$WWW_STAGING" 2>/dev/null || sync
cp "$DIR"/systemd/*.service /usr/lib/systemd/system/
systemctl daemon-reload

echo "[2/5] Updating backend services (gateway stays up)..."
# Stop non-gateway services in reverse dependency order
systemctl stop casanova casanova-job-management casanova-app-management casanova-local-storage casanova-user-service casanova-message-bus 2>/dev/null || true
SERVICES_STOPPED=true

# Stage every new binary first (nothing live is touched yet, so a failed copy
# here costs nothing) and swap each one in with an atomic rename. A rename
# repoints the directory entry rather than writing through the existing
# inode, so it can never hit "Text file busy" even if a service somehow still
# held the old binary open, and it can never leave /usr/bin holding a
# truncated file the way the previous `rm -f` + `cp` pair could on a copy
# that failed partway. The displaced binary is preserved in BIN_BACKUP (not
# deleted) so rollback() can put it straight back if a later binary in this
# same loop — or the health check below — fails.
mkdir -p "$BIN_STAGING" "$BIN_BACKUP"
for bin in "$DIR"/bin/*; do
  name="$(basename "$bin")"
  cp -p "$bin" "$BIN_STAGING/$name"
done
sync "$BIN_STAGING" 2>/dev/null || sync

for bin in "$DIR"/bin/*; do
  name="$(basename "$bin")"
  target="/usr/bin/$name"
  [ -e "$target" ] && cp -p "$target" "$BIN_BACKUP/$name"
  mv -f "$BIN_STAGING/$name" "$target"
done

# Frontend swap: two renames, staged content already synced above, so this
# never leaves the live tree empty or half-copied the way the old
# `rm -rf www/*` + `cp -r` pair could on a power cut or ENOSPC mid-copy.
[ -d "$WWW_DIR" ] && mv "$WWW_DIR" "$WWW_BACKUP"
mv "$WWW_STAGING" "$WWW_DIR"

echo "[3/5] Starting backend services..."
# Gateway must start first — it writes /var/run/casaos/management.url
# which all other services need to register their routes.
systemctl restart casanova-gateway
sleep 2
systemctl start casanova-message-bus
sleep 1
systemctl start casanova-user-service casanova-local-storage
sleep 1
systemctl start casanova-app-management casanova-job-management casanova

# Record what was applied. /var/lib/casanova/manifest.json is what the updater
# reads to decide whether a package supersedes the running install
# (pkg/maintenance: installedVersion), so a deploy path that skips it leaves
# that record frozen at whatever ran last — this script never wrote it, and on
# the production box the manifest sat six days stale while the binaries moved
# on. The updater was comparing candidates against a version that had not been
# installed for a week.
if [ -f "$DIR/manifest.json" ]; then
  mkdir -p /var/lib/casanova
  cp "$DIR/manifest.json" /var/lib/casanova/manifest.json
fi

echo "[4/5] Verifying..."
sleep 2
# Health check — a failed ping IS a failed update, not a warning that leaves
# the operator to notice on their own. Gate on it: a bare failing command
# here reaches the ERR trap above and rolls everything back to what was
# running before this script touched anything, instead of exiting 0 having
# silently left a broken gateway in place.
if ! curl -sf -o /dev/null http://localhost:80/ping 2>/dev/null && \
   ! curl -sf -o /dev/null http://localhost:85/ping 2>/dev/null; then
  echo "Gateway ping failed — rolling back." >&2
  false
fi
echo "Gateway is up. Done!"

echo "[5/5] Cleaning up..."
# Only reached on success: the previous binaries/frontend are no longer
# needed once the health check above has passed.
trap - ERR
rm -rf "$WWW_BACKUP" "$BIN_BACKUP"
echo "Refresh your browser (Ctrl+Shift+R for hard refresh)."
