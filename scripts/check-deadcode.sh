#!/usr/bin/env bash
# check-deadcode.sh — fail when a function is unreachable from every binary
# under cmd/ and is not in the baseline (task-70e08aca).
#
# golangci-lint's `unused` only sees unexported code. An exported constructor
# with no production callers, like security.NewGate (the Red/Yellow trust tiers
# that were never enforced), passes it and every unit test. deadcode walks the
# call graph from each main package, tests excluded, so it reports it.
#
# Usage:
#   scripts/check-deadcode.sh            # check against the baseline
#   scripts/check-deadcode.sh --update   # rewrite the baseline (review the diff)
#
# New dead code: wire it up, delete it, or (rarely) add it to the baseline in
# the same PR with a reason in the description. Baseline entries that are no
# longer dead are reported so the list only shrinks.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BASELINE="$REPO/scripts/deadcode-baseline.txt"
DEADCODE_VERSION="v0.51.0"

cd "$REPO"
raw="$(go run "golang.org/x/tools/cmd/deadcode@${DEADCODE_VERSION}" ./cmd/...)"
# Line numbers drop out so unrelated edits above a dead function don't fail.
current="$(printf '%s\n' "$raw" | sed -E 's/:[0-9]+:[0-9]+: /: /' | grep -v '^$' | LC_ALL=C sort -u || true)"

if [ "${1:-}" = "--update" ]; then
    {
        echo "# Functions unreachable from every cmd/ binary (tests excluded), known"
        echo "# before scripts/check-deadcode.sh existed. Remove lines as code is wired"
        echo "# up or deleted; never add one without a reason in the PR."
        printf '%s\n' "$current"
    } > "$BASELINE"
    echo "deadcode baseline updated: $(printf '%s\n' "$current" | grep -c . || true) entries"
    exit 0
fi

known="$(grep -v '^#' "$BASELINE" | grep -v '^$' | LC_ALL=C sort -u || true)"
new="$(LC_ALL=C comm -23 <(printf '%s\n' "$current") <(printf '%s\n' "$known") | grep -v '^$' || true)"
gone="$(LC_ALL=C comm -13 <(printf '%s\n' "$current") <(printf '%s\n' "$known") | grep -v '^$' || true)"

if [ -n "$gone" ]; then
    echo "No longer dead; remove from scripts/deadcode-baseline.txt:"
    printf '%s\n' "$gone" | sed 's/^/  /'
fi
if [ -n "$new" ]; then
    echo "NEW DEAD CODE (no production caller from any cmd/ binary):"
    printf '%s\n' "$new" | sed 's/^/  /'
    echo "Wire it up or delete it. A gate with no production caller is not enforced."
    exit 1
fi
echo "deadcode: no new unreachable functions"
