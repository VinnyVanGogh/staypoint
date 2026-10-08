#!/usr/bin/env bash
# Require CI on main before anything merges (task-2114d4aa).
#
# #244-#251 merged onto a red main because main's protection required no
# status checks. This sets the required checks and keeps every other setting
# main's protection already has (enforce_admins, reviews, force-push and
# deletion rules), because GitHub's PUT replaces the whole protection.
#
# Usage:
#   scripts/apply-branch-protection.sh --dry-run   print the PUT body, change nothing
#   scripts/apply-branch-protection.sh --check     exit 0 iff the required checks are set
#   scripts/apply-branch-protection.sh --apply     write the protection
#
# Env: REPO (default: the gh repo of this checkout), BRANCH (default main).
#
# The check names are the job names ci.yml reports on a pull_request run.
# Build & Test runs only Go 1.25 on PRs (1.24 runs on push to main), so 1.24
# must not be required: a PR would wait forever for a check that never runs.
# Apply only once Playwright UI Specs passes on main, or every PR is blocked.
set -euo pipefail

mode="${1:-}"
case "$mode" in
--dry-run | --check | --apply) ;;
*)
	echo "usage: $0 --dry-run | --check | --apply" >&2
	exit 2
	;;
esac

command -v gh >/dev/null || { echo "gh not found" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq not found" >&2; exit 2; }

REPO="${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
BRANCH="${BRANCH:-main}"
# GitHub Actions' app id, so only Actions can satisfy the checks.
ACTIONS_APP_ID=15368

REQUIRED_CHECKS=(
	"Build & Test (ubuntu-latest, 1.25)"
	"Build & Test (macos-latest, 1.25)"
	"JS Unit Tests"
	"Playwright UI Specs"
)

want_json=$(printf '%s\n' "${REQUIRED_CHECKS[@]}" | jq -R . | jq -s -c 'sort')

current=$(gh api "repos/$REPO/branches/$BRANCH/protection" 2>/dev/null || echo '{}')
have_json=$(jq -c '[.required_status_checks.checks[]?.context] | sort' <<<"$current")

if [ "$mode" = "--check" ]; then
	if [ "$have_json" = "$want_json" ]; then
		echo "OK: $REPO $BRANCH requires $want_json"
		exit 0
	fi
	echo "MISSING: $REPO $BRANCH requires $have_json, want $want_json"
	exit 1
fi

# Carry over what main's protection has today; only the checks change.
body=$(jq -c --argjson want "$want_json" --argjson app "$ACTIONS_APP_ID" '{
	required_status_checks: {
		strict: false,
		checks: [$want[] | {context: ., app_id: $app}]
	},
	enforce_admins: (.enforce_admins.enabled // true),
	required_pull_request_reviews: (
		if .required_pull_request_reviews then {
			dismiss_stale_reviews: .required_pull_request_reviews.dismiss_stale_reviews,
			require_code_owner_reviews: .required_pull_request_reviews.require_code_owner_reviews,
			require_last_push_approval: .required_pull_request_reviews.require_last_push_approval,
			required_approving_review_count: .required_pull_request_reviews.required_approving_review_count
		} else null end),
	restrictions: null,
	required_linear_history: (.required_linear_history.enabled // false),
	allow_force_pushes: (.allow_force_pushes.enabled // false),
	allow_deletions: (.allow_deletions.enabled // false),
	required_conversation_resolution: (.required_conversation_resolution.enabled // false)
}' <<<"$current")

if [ "$mode" = "--dry-run" ]; then
	echo "PUT repos/$REPO/branches/$BRANCH/protection"
	jq . <<<"$body"
	exit 0
fi

if jq -e '.restrictions' <<<"$current" >/dev/null 2>&1; then
	echo "refusing: $BRANCH has push restrictions this script would drop" >&2
	exit 1
fi
gh api -X PUT "repos/$REPO/branches/$BRANCH/protection" --input - <<<"$body" >/dev/null
exec "$0" --check
