#!/usr/bin/env bash
###############################################################################
# CasaNova Update Package Builder
# LucentOpen Software · v1.0.0
#
# Packages the contents of build/full/ into a self-contained .tar.gz update
# package that casanova-update.sh (or the backend's self-update engine) can
# apply to a running CasaNova instance.
#
# Package layout inside the tar.gz:
#   casanova-<version>/
#   ├── bin/                     # Go binaries
#   ├── systemd/                 # .service files
#   ├── www/                     # Vue frontend build
#   ├── config/                  # Sample configs (.sample — never overwrite live)
#   ├── manifest.json            # Build metadata (version, SHAs, sizes)
#   ├── checksums.sha256         # SHA-256 of every file for verification
#   └── update-meta.json         # Package metadata (version, arch, built_at)
#
# Usage:
#   bash casanova-package.sh                                    # Auto-detect from build/full/
#   bash casanova-package.sh --build-dir=/path/to/build/full    # Explicit path
#   bash casanova-package.sh --output=/path/to/output.tar.gz    # Custom output path
#   bash casanova-package.sh --channel=dev                      # Set release channel
#
# The output file is placed in build/updates/ by default.
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
err()   { echo -e "${RED}[-]${NC} $*"; }

# ─── Arguments ───────────────────────────────────────────────────────────────
BUILD_DIR=""
OUTPUT_FILE=""
CHANNEL="dev"

for arg in "$@"; do
    case "$arg" in
        --build-dir=*) BUILD_DIR="${arg#*=}" ;;
        --output=*)    OUTPUT_FILE="${arg#*=}" ;;
        --channel=*)   CHANNEL="${arg#*=}" ;;
        --help|-h)
            echo "CasaNova Update Package Builder"
            echo ""
            echo "Usage:"
            echo "  bash $0                                    # Auto-detect build/full/"
            echo "  bash $0 --build-dir=/path/to/build/full    # Explicit path"
            echo "  bash $0 --output=/path/to/output.tar.gz    # Custom output"
            echo "  bash $0 --channel=dev|stable|beta           # Release channel"
            exit 0
            ;;
        *) err "Unknown argument: $arg"; exit 1 ;;
    esac
done

# Auto-detect build directory
if [ -z "$BUILD_DIR" ]; then
    BUILD_DIR="${PROJECT_ROOT}/build/full"
fi

# Validate
if [ ! -d "$BUILD_DIR" ] || [ ! -f "${BUILD_DIR}/bin/casanova" ]; then
    err "Build directory invalid: ${BUILD_DIR}"
    err "Expected ${BUILD_DIR}/bin/casanova to exist."
    exit 1
fi

# ─── Preflight: every artifact that must never be missing ────────────────────
# The copies further down were each guarded by their own `if [ -f ]` / `if [ -d ]`,
# so a build directory with no installer, no updater, no tmpfiles.d and no
# bundled library produced a package and exited 0. That is the same shape as a
# service manager that starts zero services and reports success: the artifact
# is broken in exactly the way nobody looks at, and the exit code says it is fine.
#
# Every entry below is required because something DEREFERENCES it, not because
# it looks important:
#   apply-update.sh   the package cannot be applied at all without it
#   install.sh        installs tools/ to /usr/share/casanova/tools, which is
#                     where PostUpdateRollback executes casaos-to-casanova.sh
#   upgrade.sh        in-place upgrade path used by casaos-to-casanova.sh
#   migrate.sh        migration path shipped with the full payload
#   www/ systemd/     apply-update.sh copies BOTH unguarded (`cp -r "$DIR"/www/*`
#                     and `cp "$DIR"/systemd/*.service`) — missing here becomes a
#                     failure on the USER'S box, mid-update, not here
#   tmpfiles.d/       without it a fresh box never gets /run/casanova/internal
#                     (trusted sockets) or /var/lib/casanova/updates
#   library/          bundled Restic/compose plugin; offline installs need it,
#                     and its absence silently halves the package
#   tools/            the update mechanism itself; a package without it leaves
#                     the previous release's updater frozen on every box
#
#   manifest.json     apply-update.sh installs it as /var/lib/casanova/manifest.json,
#                     which is what the updater reads to know what is installed;
#                     shipping without it was one of the two clean-install bugs.
#                     It is required on a LOWER bar than the rest, and the reason
#                     is worth keeping: the Makefile GENERATES it (both
#                     build/manifest.json and build/full/manifest.json), so it is
#                     REGENERABLE. Demanding a regenerable artifact costs nobody
#                     anything — you re-run make. Demanding a non-regenerable one
#                     changes WHO CAN PACKAGE AT ALL. High bar for the nine above,
#                     low bar for this one.
#
# NOT listed, deliberately, because its absence is legitimate:
#   config/           SAMPLE configs, installed by upgrade.sh only when nothing
#                     was migrated — a fallback, not a payload
#
# All of them are reported in ONE pass: failing on the first missing file sends
# whoever is packaging round the loop once per artifact.
MISSING=""
for f in install.sh upgrade.sh migrate.sh apply-update.sh; do
    [ -f "${BUILD_DIR}/${f}" ] || MISSING="${MISSING}\n  missing file:      ${BUILD_DIR}/${f}"
done
for d in systemd www tmpfiles.d library tools; do
    [ -d "${BUILD_DIR}/${d}" ] || MISSING="${MISSING}\n  missing directory: ${BUILD_DIR}/${d}"
done
# Not under BUILD_DIR: the packager reads the manifest from the project root,
# and `make full` writes it to both places.
[ -f "${PROJECT_ROOT}/build/manifest.json" ] || \
    MISSING="${MISSING}\n  missing file:      ${PROJECT_ROOT}/build/manifest.json (run \`make full\`; needs python3)"
if [ -n "$MISSING" ]; then
    err "Build directory is not packageable: ${BUILD_DIR}"
    # shellcheck disable=SC2059
    printf "${MISSING}\n" >&2
    err "These are not optional. A package without them installs nothing, applies"
    err "nothing, or silently ships the previous release's updater."
    err "Note that install.sh / upgrade.sh / migrate.sh / apply-update.sh, tmpfiles.d/"
    err "and library/ are NOT produced by \`make full\` and are NOT in git (see the"
    err "clean: target's comment) — a worktree that never received them cannot build"
    err "a package, and that is the condition this check exists to report."
    exit 1
fi

# ─── Read version ────────────────────────────────────────────────────────────
VERSION="unknown"
if [ -f "${PROJECT_ROOT}/build/manifest.json" ]; then
    VERSION=$(python3 -c "import json; print(json.load(open('${PROJECT_ROOT}/build/manifest.json'))['version'])" 2>/dev/null || echo "unknown")
fi
if [ "$VERSION" = "unknown" ] && [ -x "${BUILD_DIR}/bin/casanova" ]; then
    VERSION=$("${BUILD_DIR}/bin/casanova" -v 2>/dev/null || echo "unknown")
fi

ARCH=$(uname -m)
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TIMESTAMP=$(date +%Y%m%d%H%M%S)

# Package name
PKG_NAME="casanova-${VERSION}"
if [ -z "$OUTPUT_FILE" ]; then
    mkdir -p "${PROJECT_ROOT}/build/updates"
    OUTPUT_FILE="${PROJECT_ROOT}/build/updates/${PKG_NAME}-${ARCH}.tar.gz"
fi

echo -e "${BOLD}${CYAN}"
echo "  CasaNova Update Package Builder"
echo "  Version: ${VERSION}"
echo "  Arch:    ${ARCH}"
echo "  Channel: ${CHANNEL}"
echo "  Output:  ${OUTPUT_FILE}"
echo -e "${NC}"

# ─── Create staging directory ────────────────────────────────────────────────
STAGING=$(mktemp -d /tmp/casanova-package-XXXXXX)
STAGE_DIR="${STAGING}/${PKG_NAME}"
mkdir -p "$STAGE_DIR"

# Copy binaries
info "Copying binaries..."
cp -r "${BUILD_DIR}/bin" "${STAGE_DIR}/bin"
BIN_COUNT=$(ls "${STAGE_DIR}/bin/" | wc -l)
info "  ${BIN_COUNT} binaries"

# Copy systemd units
if [ -d "${BUILD_DIR}/systemd" ]; then
    info "Copying systemd units..."
    cp -r "${BUILD_DIR}/systemd" "${STAGE_DIR}/systemd"
fi

# Copy frontend
if [ -d "${BUILD_DIR}/www" ]; then
    info "Copying frontend..."
    cp -r "${BUILD_DIR}/www" "${STAGE_DIR}/www"
    WWW_COUNT=$(find "${STAGE_DIR}/www" -type f | wc -l)
    info "  ${WWW_COUNT} files"
fi

# Copy tmpfiles.d — without it a fresh/updated box never gets
# /run/casanova/internal (trusted sockets) nor /var/lib/casanova/updates
# (the Update Manager's own staging dir) created at boot.
if [ -d "${BUILD_DIR}/tmpfiles.d" ]; then
    info "Copying tmpfiles.d..."
    cp -r "${BUILD_DIR}/tmpfiles.d" "${STAGE_DIR}/tmpfiles.d"
fi

# Copy sample configs
if [ -d "${BUILD_DIR}/config" ]; then
    info "Copying config samples..."
    cp -r "${BUILD_DIR}/config" "${STAGE_DIR}/config"
fi

# Copy bundled runtime library (Restic, Docker compose plugin, etc.) — required
# for offline installs / migrations on hosts without internet access. Was
# missing from earlier versions of this packager and made the resulting tar.gz
# ~50% smaller than the working dev23 baseline.
if [ -d "${BUILD_DIR}/library" ]; then
    info "Copying bundled library..."
    cp -r "${BUILD_DIR}/library" "${STAGE_DIR}/library"
    LIB_SIZE=$(du -sh "${STAGE_DIR}/library" | awk '{print $1}')
    info "  ${LIB_SIZE}"
fi

# Copy bundled tooling (audit script, migration helpers, packager itself).
if [ -d "${BUILD_DIR}/tools" ]; then
    info "Copying bundled tools..."
    cp -r "${BUILD_DIR}/tools" "${STAGE_DIR}/tools"
fi

# Copy install / upgrade / migrate / apply-update scripts shipped with the
# full payload. casaos-to-casanova.sh and casanova-update.sh need these to
# perform an in-place upgrade on the target host.
# Unconditional: the preflight above already refused to get here without them.
for sh in install.sh upgrade.sh migrate.sh apply-update.sh; do
    cp "${BUILD_DIR}/${sh}" "${STAGE_DIR}/${sh}"
done

# Copy manifest
# Unconditional: the preflight already refused to get here without it.
cp "${PROJECT_ROOT}/build/manifest.json" "${STAGE_DIR}/manifest.json"

# ─── Generate checksums ─────────────────────────────────────────────────────
info "Generating checksums..."
(cd "$STAGE_DIR" && find . -type f ! -name 'checksums.sha256' ! -name 'update-meta.json' -exec sha256sum {} \; | sort) > "${STAGE_DIR}/checksums.sha256"
CHECKSUM_COUNT=$(wc -l < "${STAGE_DIR}/checksums.sha256")
info "  ${CHECKSUM_COUNT} files checksummed"

# ─── Generate package metadata ───────────────────────────────────────────────
TOTAL_SIZE=$(du -sb "$STAGE_DIR" | awk '{print $1}')

cat > "${STAGE_DIR}/update-meta.json" << EOF
{
  "package_format": 1,
  "version": "${VERSION}",
  "channel": "${CHANNEL}",
  "architecture": "${ARCH}",
  "built_at": "${BUILT_AT}",
  "package_timestamp": "${TIMESTAMP}",
  "total_size_bytes": ${TOTAL_SIZE},
  "binaries": ${BIN_COUNT},
  "requires_reboot": false,
  "min_version": null,
  "changelog": null
}
EOF

info "Package metadata written"

# ─── Create tar.gz ───────────────────────────────────────────────────────────
info "Creating archive..."
(cd "$STAGING" && tar -czf "$OUTPUT_FILE" "${PKG_NAME}")

# Cleanup staging
rm -rf "$STAGING"

# Final stats
PKG_SIZE=$(stat --printf='%s' "$OUTPUT_FILE" | numfmt --to=iec 2>/dev/null || echo "?")
PKG_SHA=$(sha256sum "$OUTPUT_FILE" | awk '{print $1}')

echo ""
echo -e "${BOLD}${GREEN}Package created successfully!${NC}"
echo ""
echo "  File:     ${OUTPUT_FILE}"
echo "  Size:     ${PKG_SIZE}"
echo "  SHA-256:  ${PKG_SHA}"
echo "  Version:  ${VERSION}"
echo "  Channel:  ${CHANNEL}"
echo "  Arch:     ${ARCH}"
echo ""
echo "To apply this update:"
echo "  sudo bash casaos-to-casanova.sh --upgrade --package=${OUTPUT_FILE}"
echo "  — or —"
echo "  sudo bash casanova-update.sh ${OUTPUT_FILE}"
