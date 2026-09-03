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
for sh in install.sh upgrade.sh migrate.sh apply-update.sh; do
    if [ -f "${BUILD_DIR}/${sh}" ]; then
        cp "${BUILD_DIR}/${sh}" "${STAGE_DIR}/${sh}"
    fi
done

# Copy manifest
if [ -f "${PROJECT_ROOT}/build/manifest.json" ]; then
    cp "${PROJECT_ROOT}/build/manifest.json" "${STAGE_DIR}/manifest.json"
fi

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
