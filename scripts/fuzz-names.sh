#!/usr/bin/env bash
# Runs the name-matching fuzz targets (internal/names and the checks built on
# it: protected branch names, org holds, prod detection) for FUZZTIME each.
# go test -fuzz takes one target per run, hence the loop. Their seed corpora
# also run in every plain `go test ./...`.
#
#   scripts/fuzz-names.sh            # 20s per target
#   FUZZTIME=5m scripts/fuzz-names.sh
set -euo pipefail
cd "$(dirname "$0")/.."

FUZZTIME="${FUZZTIME:-20s}"
targets=(
  "./internal/names FuzzNormalize"
  "./internal/shipreview FuzzProtectedName"
  "./internal/context FuzzNameTargetsProd"
  "./internal/governance FuzzOrgHold"
)
for t in "${targets[@]}"; do
  read -r pkg fn <<<"$t"
  echo "== $fn ($pkg, $FUZZTIME)"
  go test "$pkg" -run '^$' -fuzz "^${fn}\$" -fuzztime "$FUZZTIME"
done
