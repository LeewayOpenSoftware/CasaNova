#!/usr/bin/env bash
# CasaNova hot-swap. One command. No flags.
#   sudo bash swap.sh
# Replaces bins + www + units. Leaves /etc/casanova and DBs alone.
set -uo pipefail
[ "$(id -u)" -eq 0 ] || exec sudo bash "$0" "$@"
B="$(cd "$(dirname "$0")" && pwd)"

# KEEP IN SYNC WITH build/tools/casaos-to-casanova.sh's CASANOVA_SERVICES_STOP
# and CASANOVA_SERVICES_START.
#
# casanova-driver and casanova-virt were missing from both lists here (the
# same class of bug fixed for casaos-to-casanova.sh under c-fix-rollback-
# hardening, but spread differently in this script): "bins" below copies
# every binary the glob finds, so the new driver/virt binaries DO land in
# /usr/bin — but the running driver/virt services were never stopped before
# that overwrite and never (re)started afterward, so they kept running the
# OLD in-memory binary indefinitely after a hot-swap that claimed to be
# done. No corruption, no crash — just a silently stale service that looks
# swapped and isn't.
STOP=(casanova casanova-job-management casanova-app-management casanova-local-storage casanova-user-service casanova-virt casanova-driver casanova-message-bus casanova-gateway)
START=(casanova-gateway casanova-message-bus casanova-driver casanova-virt casanova-user-service casanova-local-storage casanova-app-management casanova-job-management casanova)

echo "[+] stop"
systemctl stop "${STOP[@]}" 2>/dev/null || true

echo "[+] bins"
for f in "$B"/bin/*; do
  n=$(basename "$f")
  rm -f "/usr/bin/$n"
  install -m755 "$f" "/usr/bin/$n"
done

echo "[+] www"
rm -rf /var/lib/casanova/www/*
cp -r "$B"/www/* /var/lib/casanova/www/

echo "[+] units"
# Sweep stale unit files before installing the new set, so a unit that
# was renamed/removed across builds doesn't linger and crash-loop after
# daemon-reload (it would still match `systemctl start casanova*`).
rm -f /usr/lib/systemd/system/casanova*.service
cp "$B"/systemd/casanova*.service /usr/lib/systemd/system/ 2>/dev/null || true
systemctl daemon-reload

echo "[+] tools (casanova-update.sh, casaos-to-casanova.sh)"
# Without this the rollback endpoint would still point at an outdated
# tools dir after a hot-swap upgrade.
if [ -d "$B/tools" ]; then
  mkdir -p /usr/share/casanova/tools
  cp -a "$B"/tools/. /usr/share/casanova/tools/
  chmod 0755 /usr/share/casanova/tools/*.sh 2>/dev/null || true
fi

echo "[+] runtime dirs"
# Wave 35-P recovery: units declare RuntimeDirectory=casanova (and BindPaths
# into /run/casanova). After a reboot /run is fresh tmpfs and the dir does
# not exist yet, so systemd's mount-namespace setup fails with status=226
# /NAMESPACE before the binary even starts. install.sh handles this by
# dropping a tmpfiles.d snippet; the hot-swap path forgot, so a user who
# rebooted between runs ended up with casanova-message-bus / casanova-gateway
# in a permanent failed state. Install the snippet, materialise the dirs
# right now (so we don't need a reboot), and belt+braces mkdir the legacy
# /var/run/casaos paths the Go code still writes to.
if [ -f "$B"/tmpfiles.d/casanova.conf ]; then
  install -m644 "$B"/tmpfiles.d/casanova.conf /usr/lib/tmpfiles.d/casanova.conf
  systemd-tmpfiles --create /usr/lib/tmpfiles.d/casanova.conf 2>/dev/null || true
fi
mkdir -p /run/casanova /run/casaos /var/run/casanova /var/run/casaos
mkdir -p /var/lib/casanova /var/lib/casanova/db /var/lib/casanova/conf /var/log/casanova
mkdir -p /var/lib/casaos/appstore

# v0.1: seed bundled app catalog if the live store is empty. Same
# rationale as install.sh — a fresh-ish install (or one that lost
# /var/lib/casaos/appstore/ to a manual cleanup) would otherwise show
# an empty AppStore panel after the hot-swap.
if [ -d "$B/library/default" ]; then
  if [ ! -d /var/lib/casaos/appstore/default ] \
     || [ -z "$(ls -A /var/lib/casaos/appstore/default 2>/dev/null)" ]; then
    echo "[+] seed app catalog → /var/lib/casaos/appstore/default/"
    mkdir -p /var/lib/casaos/appstore/default
    cp -a "$B"/library/default/. /var/lib/casaos/appstore/default/
  fi
fi
if [ -f "$B/library/stores.json" ] && [ ! -f /var/lib/casaos/appstore/stores.json ]; then
  cp "$B/library/stores.json" /var/lib/casaos/appstore/stores.json
fi

echo "[+] start (no-block)"
# --no-block dispatches without waiting; we then poll. Without this a
# single hung unit (TimeoutStartSec defaults to 90s) freezes the loop.
for s in "${START[@]}"; do
  systemctl start --no-block "$s" 2>/dev/null || true
done

echo "[+] wait up to 30s for activation"
deadline=$(( $(date +%s) + 30 ))
while :; do
  bad=()
  for s in "${START[@]}"; do
    state=$(systemctl is-active "$s" 2>/dev/null || echo unknown)
    [ "$state" = active ] || bad+=("$s:$state")
  done
  [ ${#bad[@]} -eq 0 ] && break
  [ "$(date +%s)" -ge "$deadline" ] && break
  sleep 1
done

echo
echo "[+] status"
for s in "${START[@]}"; do
  state=$(systemctl is-active "$s" 2>/dev/null || echo unknown)
  printf "  %-30s %s\n" "$s" "$state"
done

if [ ${#bad[@]} -gt 0 ]; then
  echo
  echo "[!] not active after 30s:"
  for entry in "${bad[@]}"; do
    s="${entry%%:*}"
    echo "--- $s ---"
    journalctl -u "$s" -n 8 --no-pager 2>/dev/null | sed 's/^/    /'
  done
  exit 1
fi

if curl -sf --max-time 5 http://localhost/ping >/dev/null 2>&1; then
  echo "[+] gateway ping OK"
else
  echo "[!] no gateway ping — journalctl -u casanova-gateway -n 30"
  exit 1
fi

echo "[+] done"
