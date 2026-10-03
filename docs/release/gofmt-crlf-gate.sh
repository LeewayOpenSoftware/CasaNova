#!/usr/bin/env bash
# gofmt-crlf-gate.sh — gofmt over CRLF-normalized content, isolating real
# formatting defects from files that are CRLF but otherwise gofmt-clean.
#
# Plain `gofmt -l` cannot tell these apart: a CRLF file is flagged for its
# line endings even when the code inside it is correctly formatted, so on
# this tree ~276-279 CRLF-only false positives bury the ~3 files per line
# that are genuinely misformatted (measured directly against this gate).
#
# A file is flagged HERE only if its content, with \r stripped, STILL
# differs from gofmt's output on that same stripped content. That is a real
# formatting defect independent of which line-ending convention the file
# uses on disk. This gate never rewrites anything — it only reads — so it is
# safe to run over files whose CRLF must be preserved
# (CasaNova-Common/utils/jwt/jwt.go, CasaNova-AppManagement/common/message.go):
# they are CRLF-normalized only in a throwaway pipe for the comparison.
#
# Usage:
#   ./docs/release/gofmt-crlf-gate.sh                # every tracked .go file
#   ./docs/release/gofmt-crlf-gate.sh CasaNova-Gateway CasaNova-LocalStorage
#
# Runs from wherever it's invoked: ROOT is the git worktree of the current
# directory, not the directory the script itself lives in — so it can be
# copied/symlinked into CI, or run against a sibling worktree by cd'ing
# there first and invoking this script by path.
#
# Exit 0, silent: clean. Exit 1, one path per line on stdout: real defects,
# ready to feed straight into `gofmt -w`.

set -uo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "gofmt-crlf-gate.sh: not inside a git worktree" >&2
  exit 2
}
cd "$ROOT" || exit 2

if [ "$#" -gt 0 ]; then
  FILES="$(git ls-files -- "$@" | grep '\.go$' || true)"
else
  FILES="$(git ls-files -- '*.go' || true)"
fi

bad=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue
  if [ -n "$(tr -d '\r' <"$f" | gofmt -l /dev/stdin 2>/dev/null)" ]; then
    bad+=("$f")
  fi
done <<<"$FILES"

if [ ${#bad[@]} -eq 0 ]; then
  exit 0
fi
printf '%s\n' "${bad[@]}"
exit 1
