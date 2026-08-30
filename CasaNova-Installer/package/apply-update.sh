#!/usr/bin/env bash
# CasaNova quick-update — rolling restart to minimize downtime.
# The gateway stays alive while backend services update, so Cloudflare
# tunnels don't go into backoff retry. Total downtime: ~3 seconds
# (just the gateway binary swap at the end).
# Usage: sudo bash apply-update.sh
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

echo "[1/5] Deploying frontend + systemd units..."
rm -rf /var/lib/casanova/www/*
cp -r "$DIR"/www/* /var/lib/casanova/www/
cp "$DIR"/systemd/*.service /usr/lib/systemd/system/
systemctl daemon-reload

echo "[2/5] Updating backend services (gateway stays up)..."
# Stop non-gateway services in reverse dependency order
systemctl stop casanova casanova-job-management casanova-app-management casanova-local-storage casanova-user-service casanova-message-bus 2>/dev/null || true

# Deploy binaries. Running binaries can't be overwritten on Linux
# ("Text file busy"), so rm first — the kernel keeps the old file
# alive via the running process's file handle until it exits.
for bin in "$DIR"/bin/*; do
  target="/usr/bin/$(basename "$bin")"
  rm -f "$target"
  cp "$bin" "$target"
done

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
# Quick health check
if curl -sf -o /dev/null http://localhost:80/ping 2>/dev/null || curl -sf -o /dev/null http://localhost:85/ping 2>/dev/null; then
  echo "Gateway is up. Done!"
else
  echo "Gateway ping failed — check: systemctl status casanova-gateway"
fi
echo "Refresh your browser (Ctrl+Shift+R for hard refresh)."
