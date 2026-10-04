#!/usr/bin/env bash
# ui-e2e.sh: run the StayPoint web UI Playwright suite headless (STA-353).
#
# Uses the same throwaway harness as scripts/api-e2e.sh (STA-346): builds
# cmd/staypoint-apitest-server, starts it on a fresh SQLite file in a temp dir
# with HOME pointed at that temp dir, and points its /api/fleet/* proxies at
# tests/api/paperclip_stub.py. A second, expendable daemon is started for the
# "daemon goes down" spec, which kills it. The real StayPoint DB
# (~/.staypoint/staypoint.db), the launchd daemon on :41421 and the live
# Paperclip API are never touched.
#
# Usage:
#   scripts/ui-e2e.sh                       # whole suite
#   scripts/ui-e2e.sh -g "interaction"      # extra args go to `playwright test`
#   scripts/ui-e2e.sh specs/03-comments.spec.ts
#
# Env:
#   E2E_ARTIFACTS=dir      screenshots, traces and HTML report on failure
#                          (default: tests/ui/artifacts)
#   E2E_KEEP_TMP=1         keep the temp dir (DBs, daemon logs) for debugging
#   STAYPOINT_UI_STRICT=1  ignore the known-bug list in tests/ui/fixtures.ts
#                          (use for the final pre-cutover run)
#
# Exit status is playwright's: non-zero if any spec fails.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI_DIR="$ROOT/tests/ui"
API_DIR="$ROOT/tests/api"

case "${1:-}" in
  -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
esac

for bin in go node npx python3; do
  command -v "$bin" >/dev/null || { echo "ui-e2e: $bin is required" >&2; exit 2; }
done
for f in "$ROOT/cmd/staypoint-apitest-server/main.go" "$API_DIR/paperclip_stub.py"; do
  [ -f "$f" ] || { echo "ui-e2e: missing $f (the STA-346 api-e2e harness)" >&2; exit 2; }
done

TMP="$(mktemp -d "${TMPDIR:-/tmp}/staypoint-ui-e2e.XXXXXX")"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  if [ "${E2E_KEEP_TMP:-0}" = "1" ]; then
    echo "ui-e2e: kept $TMP"
  else
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT INT TERM

wait_for() { # wait_for <seconds> <command...>
  local deadline=$(( $(date +%s) + $1 )); shift
  until "$@"; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 0.2
  done
}

TOKEN="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"

echo "ui-e2e: building staypoint-apitest-server and stepsim"
(cd "$ROOT" && go build -o "$TMP/staypoint-apitest-server" ./cmd/staypoint-apitest-server)
(cd "$ROOT" && go build -o "$TMP/stepsim" ./tests/ui/stepsim)

python3 "$API_DIR/paperclip_stub.py" "$TMP/stub.port" &
PIDS+=("$!")
wait_for 10 test -s "$TMP/stub.port" || { echo "ui-e2e: paperclip stub did not start" >&2; exit 1; }
STUB_URL="http://127.0.0.1:$(cat "$TMP/stub.port")"

start_daemon() { # start_daemon <name>; sets DAEMON_URL and DAEMON_PID
  local name="$1" dir="$TMP/$1"
  mkdir -p "$dir/home"
  # env -u: the daemon's fleet proxies attach PAPERCLIP_API_KEY to every call.
  env -u PAPERCLIP_API_KEY -u PAPERCLIP_COMPANY_ID -u PAPERCLIP_API_URL \
    HOME="$dir/home" STAYPOINT_API_TOKEN="$TOKEN" \
    "$TMP/staypoint-apitest-server" --db "$dir/staypoint.db" --paperclip-url "$STUB_URL" \
    >"$dir/daemon.out" 2>"$dir/daemon.err" &
  DAEMON_PID=$!
  PIDS+=("$DAEMON_PID")
  if ! wait_for 30 grep -q '^READY ' "$dir/daemon.out"; then
    echo "ui-e2e: $name daemon did not start" >&2
    cat "$dir/daemon.err" >&2
    exit 1
  fi
  DAEMON_URL="$(sed -n 's/^READY //p' "$dir/daemon.out")"
  DAEMON_BOARD_TOKEN="$(sed -n 's/^BOARD_TOKEN //p' "$dir/daemon.out")"
  case "$DAEMON_URL" in
    *:41421) echo "ui-e2e: refusing $DAEMON_URL: 41421 is the real staypointd port" >&2; exit 1 ;;
  esac
}

start_daemon main
MAIN_URL="$DAEMON_URL"
MAIN_BOARD_TOKEN="$DAEMON_BOARD_TOKEN"
start_daemon expendable
DOWN_URL="$DAEMON_URL"
DOWN_PID="$DAEMON_PID"
echo "ui-e2e: throwaway daemon at $MAIN_URL (db: $TMP/main/staypoint.db)"
echo "ui-e2e: expendable daemon at $DOWN_URL (pid $DOWN_PID)"

cd "$UI_DIR"
if [ ! -x node_modules/.bin/playwright ]; then
  echo "ui-e2e: installing tests/ui dependencies"
  npm ci --no-audit --no-fund
fi
# No-op when the pinned Chromium is already cached.
npx playwright install chromium >/dev/null

ARTIFACTS="${E2E_ARTIFACTS:-$UI_DIR/artifacts}"
rm -rf "$ARTIFACTS"

status=0
STAYPOINT_UI_BASE_URL="$MAIN_URL" \
STAYPOINT_UI_DB="$TMP/main/staypoint.db" \
STAYPOINT_UI_STEPSIM="$TMP/stepsim" \
STAYPOINT_UI_DOWN_BASE_URL="$DOWN_URL" \
STAYPOINT_UI_DOWN_PID="$DOWN_PID" \
STAYPOINT_UI_ARTIFACTS="$ARTIFACTS" \
STAYPOINT_API_TOKEN="$TOKEN" \
STAYPOINT_BOARD_TOKEN="$MAIN_BOARD_TOKEN" \
  npx playwright test "$@" || status=$?

if [ "$status" -ne 0 ]; then
  echo "ui-e2e: FAILED; screenshots and traces in $ARTIFACTS/test-results" >&2
  echo "ui-e2e: open a trace with: npx --prefix tests/ui playwright show-trace <trace.zip>" >&2
  echo "ui-e2e: daemon stderr tail:" >&2
  tail -20 "$TMP/main/daemon.err" >&2 || true
  exit "$status"
fi
echo "ui-e2e: PASSED"
