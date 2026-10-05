#!/usr/bin/env bash
# check-test-home.sh — run `go test` under a throwaway HOME and fail if the
# tests wrote user files there (STA-741).
#
# A test that resolves ~/.staypoint or ~/Desktop without its own temp HOME
# writes into the developer's real home on a normal run: it overwrites the
# live ~/.staypoint/handoff.json pointer and creates ~/Desktop.
#
# Usage:
#   scripts/check-test-home.sh [go test args...]
#   scripts/check-test-home.sh                       # ./internal/... ./cmd/... -count=1
#   scripts/check-test-home.sh -v -race ./...        # what CI runs
#
# Exit 1 and prints "TEST HOME LEAK: <path>" for each leaked path. Otherwise
# exits with go test's own status.
set -uo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

if [ $# -eq 0 ]; then
    set -- ./internal/... ./cmd/... -count=1
fi

# Go keeps its module and build caches under HOME by default. Pin them to the
# real locations first, or the empty HOME re-downloads every module.
GOMODCACHE="$(go env GOMODCACHE)"
GOCACHE="$(go env GOCACHE)"
GOPATH="$(go env GOPATH)"
export GOMODCACHE GOCACHE GOPATH

# macOS runners set TMPDIR with a trailing slash. os.UserHomeDir returns HOME
# verbatim, so a "T//staypoint-test-home" HOME never matches the cleaned
# filepath.Join paths tests compare against.
TMP_BASE="${TMPDIR:-/tmp}"
TMP_BASE="${TMP_BASE%/}"
TEST_HOME="$(mktemp -d "${TMP_BASE:-/tmp}/staypoint-test-home.XXXXXX")"
trap 'rm -rf "$TEST_HOME"' EXIT

(cd "$REPO" && HOME="$TEST_HOME" go test "$@")
TEST_STATUS=$?

LEAKS=()
for p in "$TEST_HOME"/.staypoint/handoff* "$TEST_HOME"/.agent-mesh/handoff* "$TEST_HOME/Desktop"; do
    [ -e "$p" ] && LEAKS+=("${p#"$TEST_HOME"/}")
done

if [ ${#LEAKS[@]} -gt 0 ]; then
    for p in "${LEAKS[@]}"; do
        echo "TEST HOME LEAK: ~/$p"
        if [ -d "$TEST_HOME/$p" ]; then
            (cd "$TEST_HOME/$p" && find . -mindepth 1 | sed 's|^\.|    |' | head -20)
        fi
    done
    echo "Tests wrote into HOME. Give the offending tests t.Setenv(\"HOME\", t.TempDir()) or an explicit temp path." >&2
    exit 1
fi

echo "TEST HOME CLEAN (go test exit $TEST_STATUS)"
exit $TEST_STATUS
