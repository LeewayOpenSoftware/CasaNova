#!/usr/bin/env bash
###############################################################################
# Tests for casanova-package.sh — the packaging preflight and what it ships.
#
# WHY THIS EXISTS. The preflight demanded upgrade.sh and migrate.sh, which no
# live caller invokes by those names, and did NOT demand swap.sh, which is the
# only script the real USB update runbook executes on the target box. It was
# also missing from the copy loop, so the package shipped without it. Both
# facts together mean a package could pass the COMPLETE preflight, print
# "Package created successfully", and fail on the human's first command at
# three in the morning with the USB stick already in.
#
# A preflight that validates what nothing uses and does not validate what runs
# is not a safety net, it is an alibi. The only way that stays fixed is a test
# that fails when the list and the payload disagree — so this file asserts both
# directions: the packager REFUSES without a payload script, and the package it
# produces CONTAINS every one of them.
#
# Hermetic: it builds a complete fake project root in a temp directory and runs
# the real packager against it. Nothing is written inside the repository, and no
# 500MB build tree is needed. Run it with:
#
#   bash build/tools/casanova-package.test.sh     # or: make test-package
###############################################################################
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# The scripts the deployment actually uses. swap.sh is listed here for the same
# reason the whole file exists: it is the one the runbook runs first.
PAYLOAD_SCRIPTS="install.sh upgrade.sh migrate.sh apply-update.sh swap.sh"
TEST_VERSION="0.0.0-test"

FAILURES=0
fail() { echo "  FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }
pass() { echo "  ok:   $*"; }

# build_fixture <root> — a project root complete enough to package, so that
# anything the packager refuses is refused for the reason under test and not
# because the fixture was thin.
build_fixture() {
    local R="$1" f
    mkdir -p "${R}/build/full"/{bin,systemd,www,tmpfiles.d,config,library/default}

    # The packager greps UPDATE_TOOLS out of the real Makefile, so the fixture
    # uses the real one: if that list changes, this test follows it instead of
    # holding a stale copy of it.
    cp "${REPO_ROOT}/Makefile" "${R}/Makefile"
    cp -r "${REPO_ROOT}/build/tools" "${R}/build/tools"
    cp -r "${REPO_ROOT}/build/tools" "${R}/build/full/tools"

    printf '{"version": "%s"}\n' "${TEST_VERSION}" > "${R}/build/manifest.json"

    printf '#!/bin/sh\necho %s\n' "${TEST_VERSION}" > "${R}/build/full/bin/casanova"
    chmod +x "${R}/build/full/bin/casanova"
    echo '[Unit]' > "${R}/build/full/systemd/casanova-gateway.service"
    echo '<html></html>' > "${R}/build/full/www/index.html"
    echo 'd /run/casanova 0755 root root -' > "${R}/build/full/tmpfiles.d/casanova.conf"
    echo '{}' > "${R}/build/full/library/default/recommend-list.json"
    echo '# sample' > "${R}/build/full/config/casanova.conf.sample"
    for f in ${PAYLOAD_SCRIPTS}; do
        printf '#!/bin/sh\n: %s\n' "$f" > "${R}/build/full/${f}"
    done
}

run_packager() { # <root> -> writes log to <root>/log, echoes exit status
    local R="$1" rc
    ( cd "${R}" && bash "${R}/build/tools/casanova-package.sh" ) > "${R}/log" 2>&1
    rc=$?
    echo "${rc}"
}

# ─── 1. A complete build packages, and the package holds what runs ───────────
#
# The assertions name deployment consequences rather than file lists, because
# that is what a failure here means: the human's first command not working.
echo "1. a complete build directory produces a package containing every payload script"
R1=$(mktemp -d "${TMPDIR:-/tmp}/cn-pkg-test-XXXXXX")
build_fixture "${R1}"
RC=$(run_packager "${R1}")
TARBALL="${R1}/build/updates/casanova-${TEST_VERSION}-$(uname -m).tar.gz"

if [ "${RC}" != "0" ]; then
    fail "the packager refused a complete build directory (exit ${RC}); log:"
    sed 's/^/        /' "${R1}/log" >&2
elif [ ! -f "${TARBALL}" ]; then
    fail "no package at ${TARBALL} — the runbook's tarball name is derived from this path"
else
    pass "package built at $(basename "${TARBALL}")"
    ENTRIES=$(tar tzf "${TARBALL}")

    # update.md tells the operator to extract and then run a script by path, so
    # the layout is part of the contract, not an implementation detail.
    TOPLEVEL=$(printf '%s\n' "${ENTRIES}" | cut -d/ -f1 | sort -u)
    if [ "${TOPLEVEL}" != "casanova-${TEST_VERSION}" ]; then
        fail "the tarball's top level is '${TOPLEVEL}', not the single directory casanova-${TEST_VERSION} that update.md navigates"
    else
        pass "extracts to one directory, casanova-${TEST_VERSION}/"
    fi

    if printf '%s\n' "${ENTRIES}" | grep -q '/full/'; then
        fail "the tarball contains a full/ directory; update.md used to say it did, and that claim is what this asserts against"
    else
        pass "no full/ directory — the payload sits at the top level"
    fi

    for f in ${PAYLOAD_SCRIPTS}; do
        if printf '%s\n' "${ENTRIES}" | grep -qx "casanova-${TEST_VERSION}/${f}"; then
            pass "${f} shipped"
        else
            fail "${f} is NOT in the package, although the preflight demanded it — the check and the copy disagree"
        fi
    done
fi

# ─── 2. THE RED CASE: a missing payload script must stop the build ───────────
#
# One subtest per script, because "the preflight catches a missing file" was
# already true for four of the five while the fifth walked straight through.
echo "2. a build directory missing any payload script is REFUSED, by name"
for MISSING_ONE in ${PAYLOAD_SCRIPTS}; do
    R2=$(mktemp -d "${TMPDIR:-/tmp}/cn-pkg-test-XXXXXX")
    build_fixture "${R2}"
    rm -f "${R2}/build/full/${MISSING_ONE}"
    RC=$(run_packager "${R2}")

    if [ "${RC}" = "0" ]; then
        fail "${MISSING_ONE} was missing and the packager exited 0 — a mutilated package with a success message is exactly the failure this guards"
    elif ! grep -q "${MISSING_ONE}" "${R2}/log"; then
        fail "${MISSING_ONE} was missing; the packager failed but never named it, so whoever is packaging cannot tell what to fix"
    else
        pass "${MISSING_ONE} missing -> refused, and named in the output"
    fi
    rm -rf "${R2}"
done

rm -rf "${R1}"

echo
if [ "${FAILURES}" -eq 0 ]; then
    echo "casanova-package.sh: all checks passed"
    exit 0
fi
echo "casanova-package.sh: ${FAILURES} check(s) failed"
exit 1
