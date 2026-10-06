#!/usr/bin/env bash
# Rebuild staypointd from source and restart the LaunchAgent.
# Run this after any git pull or code change.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
# go build resolves the module from the cwd, so build from the repo root.
cd "$REPO"
BINARY="$HOME/.local/bin/staypointd"
CLI_BINARY="$HOME/.local/bin/staypoint"
PLIST="$HOME/Library/LaunchAgents/com.staypoint.daemon.plist"
LABEL="com.staypoint.daemon"
DEPLOY_LOG="$HOME/.staypoint/deploys.log"

ALLOW_DEV_BUILD=0
for arg in "$@"; do
    case "$arg" in
        --allow-dev-build) ALLOW_DEV_BUILD=1 ;;
        --allow-unmerged)
            echo "  ! --allow-unmerged is now --allow-dev-build; treating it as that."
            ALLOW_DEV_BUILD=1 ;;
        *)
            echo "✗ Unknown argument: $arg (the only flag is --allow-dev-build)" >&2
            exit 2 ;;
    esac
done

# Every install and every refused install gets one line in $DEPLOY_LOG: when,
# the outcome, who ran it and from what parent process, on which host, and
# what it would deploy.
FULL_SHA=""
DIRTY=false
IN_MAIN=false
DEV_BUILD=false
log_deploy() {
    local parent
    parent="$(ps -o command= -p "$PPID" 2>/dev/null | tr '\t\n' '  ' | cut -c1-200 || true)"
    mkdir -p "$(dirname "$DEPLOY_LOG")"
    printf '%s\tresult=%s\tuser=%s\thost=%s\tsha=%s\tdirty=%s\tin_main=%s\tdev_build=%s\trepo=%s\tparent=%s\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "${USER:-$(id -un)}" "$(hostname)" \
        "${FULL_SHA:-none}" "$DIRTY" "$IN_MAIN" "$DEV_BUILD" "$REPO" "$parent" >> "$DEPLOY_LOG"
}

# refuse <log-reason> <headline> [detail lines...]
refuse() {
    log_deploy "refused:$1"
    echo "✗ ABORTING: $2" >&2
    shift 2
    for line in "$@"; do echo "  $line" >&2; done
    exit 1
}

# Deploy guards (STA-805). The live daemon must be a reviewed build: a clean
# tree whose HEAD is on origin/main. On 2026-10-06 a session working in the
# shared checkout installed a build with 12 uncommitted files, which rolled the
# daemon back past four merged PRs and ran unreviewed code. --allow-dev-build
# deploys anyway, and the daemon then reports dev_build: true in /api/health
# and in the web UI header so nobody mistakes it for a main build.
FULL_SHA="$(git -C "$REPO" rev-parse HEAD 2>/dev/null || true)"
if [ -z "$FULL_SHA" ]; then
    refuse no-git "$REPO is not a git checkout, so the build would have no commit to report."
fi
COMMIT="$(git -C "$REPO" rev-parse --short HEAD)"
# Untracked files count: a new .go file changes the build as much as an edit.
PORCELAIN="$(git -C "$REPO" status --porcelain)"
[ -n "$PORCELAIN" ] && DIRTY=true
git -C "$REPO" fetch --quiet origin main 2>/dev/null \
    || echo "  ! Could not fetch origin/main; checking against the local origin/main ref."
if ! git -C "$REPO" rev-parse -q --verify "origin/main^{commit}" >/dev/null; then
    refuse no-origin-main "$REPO has no origin/main ref, so there is nothing to check HEAD against."
fi
git -C "$REPO" merge-base --is-ancestor HEAD origin/main 2>/dev/null && IN_MAIN=true

if [ "$ALLOW_DEV_BUILD" = 0 ]; then
    if [ "$DIRTY" = true ]; then
        refuse dirty "$REPO has uncommitted changes:" \
            "$(echo "$PORCELAIN" | head -20)" \
            "Deploy from a clean checkout of origin/main (e.g. a dedicated worktree)," \
            "or pass --allow-dev-build to deploy this tree as a dev build."
    fi
    if [ "$IN_MAIN" != true ]; then
        refuse not-in-main "HEAD ($COMMIT — $(git -C "$REPO" log --format=%s -1 HEAD)) is not on origin/main." \
            "Merge it first, or pass --allow-dev-build to deploy it as a dev build."
    fi
else
    DEV_BUILD=true
    cat <<EOF
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!!  DEV BUILD (--allow-dev-build)
!!!  This daemon will NOT be a reviewed main build.
!!!    commit:          $COMMIT
!!!    on origin/main:  $IN_MAIN
!!!    uncommitted:     $DIRTY
!!!  /api/health and the web UI header will show dev_build: true.
!!!  Redeploy from a clean origin/main checkout when you are done.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
EOF
fi

BUILD_LABEL="$COMMIT"
[ "$DIRTY" = true ] && BUILD_LABEL="$COMMIT-dirty"

# macOS privacy grants (TCC, e.g. "access files in your Documents folder") are
# keyed to the binary's designated requirement. An ad-hoc signature's
# requirement is its cdhash, which changes on every rebuild, so each rebuild
# re-prompts. Signing with a real certificate makes the requirement
# "identifier + certificate", which survives rebuilds. Check the certificate
# before building so a failure never leaves an ad-hoc binary installed.
# Override the identity with STAYPOINT_SIGN_IDENTITY; set STAYPOINT_ALLOW_ADHOC=1
# to deliberately build ad-hoc (e.g. a machine with no certificate).
SIGN_IDENTITY=""
if [ "$(uname)" = "Darwin" ]; then
    set +e
    CERT_OUT="$("$REPO/scripts/check-signing-cert.sh")"
    CERT_STATUS=$?
    set -e
    echo "$CERT_OUT" | grep -v '^[A-Z_]*=' || true
    SIGN_IDENTITY="$(echo "$CERT_OUT" | sed -n 's/^IDENTITY=//p')"
    if [ "$CERT_STATUS" -eq 1 ]; then
        if [ "${STAYPOINT_ALLOW_ADHOC:-0}" = "1" ]; then
            echo "  ! STAYPOINT_ALLOW_ADHOC=1: building ad-hoc. macOS WILL re-prompt for permissions after every rebuild."
            SIGN_IDENTITY="-"
        else
            echo "✗ ABORTING: no valid signing certificate. Building ad-hoc would bring back the"
            echo "  'staypointd would like to access your Documents folder' popup on every rebuild."
            echo "  Fix the certificate (see above), or rerun with STAYPOINT_ALLOW_ADHOC=1 to accept that."
            exit 1
        fi
    elif [ "$CERT_STATUS" -eq 2 ]; then
        echo "  !!! RENEW THE SIGNING CERTIFICATE SOON (see above). Building with it for now."
    fi
fi

sign() {
    if [ -n "$SIGN_IDENTITY" ] && [ "$SIGN_IDENTITY" != "-" ]; then
        codesign -s "$SIGN_IDENTITY" -f --timestamp=none -i "$1" "$2"
    else
        codesign -s - -f -i "$1" "$2"
    fi
}

# Build both binaries next to their install paths and move them into place
# only after the checks below pass, so a refused build never replaces the
# binary launchd restarts.
mkdir -p "$(dirname "$BINARY")"
STAGED="$BINARY.staging"
CLI_STAGED="$CLI_BINARY.staging"
trap 'rm -f "$STAGED" "$CLI_STAGED"' EXIT
echo "→ Building staypointd from $REPO (commit: $BUILD_LABEL) ..."
go build -ldflags "-X main.GitCommit=$BUILD_LABEL -X main.commit=$BUILD_LABEL -X main.DevBuild=$DEV_BUILD" \
    -o "$STAGED" "$REPO/cmd/staypointd"

# The binary must say which commit it was built from. The 2026-10-06 build
# reported git_commit "none"; refuse anything nobody could trace.
VCS_REVISION="$(go version -m "$STAGED" 2>/dev/null \
    | awk '$1 == "build" && index($2, "vcs.revision=") == 1 { sub("vcs.revision=", "", $2); print $2 }')"
if [ -z "$VCS_REVISION" ]; then
    refuse no-vcs-revision "the built binary has no vcs.revision, so it could not say which commit it runs." \
        "Build from a git checkout, and check GOFLAGS for -buildvcs=false."
fi
if [ "$VCS_REVISION" != "$FULL_SHA" ]; then
    refuse vcs-mismatch "the built binary has vcs.revision $VCS_REVISION but HEAD is $FULL_SHA."
fi
sign com.staypoint.daemon "$STAGED"

# Build the staypoint CLI alongside the daemon so the PreToolUse hook binary
# (STAYPOINT_HOOK_BIN) is always at the same commit as the daemon (STA-525).
echo "→ Building staypoint CLI from $REPO (commit: $BUILD_LABEL) ..."
go build -ldflags "-X main.GitCommit=$BUILD_LABEL -X main.commit=$BUILD_LABEL" -o "$CLI_STAGED" "$REPO/cmd/staypoint"
sign com.staypoint.cli "$CLI_STAGED"

mv -f "$STAGED" "$BINARY"
mv -f "$CLI_STAGED" "$CLI_BINARY"
log_deploy installed
echo "  Built: $BINARY ($(staypointd -version 2>/dev/null || echo 'ok'))"
echo "  Built: $CLI_BINARY"
echo "  Logged to $DEPLOY_LOG"

# Record which commits this binary contains. The checklist commit gate reads
# this instead of running git: under launchd, macOS blocks the daemon from the
# repo in ~/Documents until it has Documents access, and git hangs rather than
# failing, so every commit looked missing.
MANIFEST="$HOME/.staypoint/build-manifest.json"
mkdir -p "$HOME/.staypoint"
if [ "$COMMIT" = "none" ]; then
    rm -f "$MANIFEST"
else
    git -C "$REPO" fetch -q origin main 2>/dev/null || echo "  ! Could not fetch origin/main; checking against the local ref."
    MAIN_REF=origin/main
    git -C "$REPO" rev-parse -q --verify "$MAIN_REF^{commit}" >/dev/null || MAIN_REF=main
    IN_MAIN=false
    git -C "$REPO" merge-base --is-ancestor HEAD "$MAIN_REF" 2>/dev/null && IN_MAIN=true
    [ "$IN_MAIN" = true ] || echo "  ! $COMMIT is not in $MAIN_REF: the checklist commit gate will stay closed."
    {
        printf '{"commit":"%s","full_sha":"%s","in_main":%s,"main_sha":"%s","dirty":%s,"built_at":"%s","ancestors":[' \
            "$COMMIT" "$(git -C "$REPO" rev-parse HEAD)" "$IN_MAIN" \
            "$(git -C "$REPO" rev-parse --short "$MAIN_REF" 2>/dev/null)" "$DIRTY" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        git -C "$REPO" rev-list HEAD | sed 's/.*/"&"/' | paste -sd, -
        printf ']}\n'
    } > "$MANIFEST.tmp"
    mv "$MANIFEST.tmp" "$MANIFEST"
    echo "  Build manifest: $MANIFEST (in main: $IN_MAIN, dirty: $DIRTY)"
fi

cat <<EOF > "$PLIST"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.staypoint.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BINARY</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$HOME</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/go/bin</string>
        <key>HOME</key>
        <string>$HOME</string>
        <key>STAYPOINT_REPO_ROOT</key>
        <string>$REPO</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/staypointd.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/staypointd.err</string>
</dict>
</plist>
EOF

# Daily renewal reminder: a notification banner (never a modal) once the
# signing certificate is within 30 days of expiry, and every day after.
REMINDER_LABEL="com.staypoint.cert-reminder"
REMINDER_PLIST="$HOME/Library/LaunchAgents/$REMINDER_LABEL.plist"
REMINDER_BIN="$HOME/.local/bin/staypoint-check-signing-cert"
if [ "$(uname)" = "Darwin" ]; then
    # Copy, not reference: the repo checkout may be a worktree that gets deleted.
    install -m 0755 "$REPO/scripts/check-signing-cert.sh" "$REMINDER_BIN"
    cat <<EOF > "$REMINDER_PLIST"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$REMINDER_LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$REMINDER_BIN</string>
        <string>--notify</string>
        <string>--quiet</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>10</integer>
        <key>Minute</key>
        <integer>0</integer>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/staypoint-cert-reminder.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/staypoint-cert-reminder.log</string>
</dict>
</plist>
EOF
    launchctl unload "$REMINDER_PLIST" 2>/dev/null || true
    launchctl load "$REMINDER_PLIST" 2>/dev/null || true
fi

# Pre-evaluate checklist contracts while running in the user's shell (which has
# ~/Documents TCC access). The daemon runs under launchd without that grant, so
# each file_pattern/command contract hangs ~1.2 s instead of failing, causing
# the 32 s evaluate endpoint timeout (see STA-279). Writing the cache here lets
# the daemon read results without touching ~/Documents.
echo "→ Pre-evaluating checklist contracts (STA-279 TCC workaround) ..."
if ! "$BINARY" eval-contracts --sprint STA-236 --repo-root "$REPO" 2>&1; then
    echo "  ! eval-contracts failed — daemon will fall back to live evaluation (may be slow without TCC)."
fi

# `launchctl print` exits non-zero when the job is not loaded. Do not use
# `launchctl list | grep -q`: grep exits on the first match, launchctl takes
# SIGPIPE, and under pipefail the pipeline fails even though the job is there.
DOMAIN="gui/$(id -u)"
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
    echo "→ Stopping $LABEL ..."
    launchctl unload "$PLIST" 2>/dev/null || true
fi

echo "→ Starting $LABEL ..."
launchctl load "$PLIST" 2>/dev/null || true
launchctl kickstart -k "$DOMAIN/$LABEL" 2>/dev/null || true

# Poll instead of checking once: the daemon can take a few seconds to open its
# DB and bind the HTTP port. Success means /api/health answers with the commit
# just built, which also proves the old binary is not the one still serving.
HEALTH_URL="http://127.0.0.1:41421/api/health"
START_TIMEOUT="${STAYPOINT_START_TIMEOUT:-20}"
PID=""
HEALTH_COMMIT=""
deadline=$((SECONDS + START_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
    PID="$(launchctl print "$DOMAIN/$LABEL" 2>/dev/null | awk '!found && $1 == "pid" && $2 == "=" { print $3; found = 1 }' || true)"
    TOKEN="$(cat "$HOME/.staypoint/auth_token" 2>/dev/null || true)"
    if [ -n "$PID" ] && [ -n "$TOKEN" ]; then
        HEALTH_COMMIT="$(curl -s -m 2 -H "Authorization: Bearer $TOKEN" "$HEALTH_URL" 2>/dev/null \
            | sed -n 's/.*"git_commit":"\([^"]*\)".*/\1/p' || true)"
        [ "$HEALTH_COMMIT" = "$BUILD_LABEL" ] && break
    fi
    sleep 1
done

if [ -n "$PID" ] && [ "$HEALTH_COMMIT" = "$BUILD_LABEL" ]; then
    echo "✓ $LABEL is running (pid $PID, /api/health git_commit $HEALTH_COMMIT)."
elif [ -n "$PID" ] && [ -n "$HEALTH_COMMIT" ]; then
    echo "✗ $LABEL is up (pid $PID) but /api/health reports $HEALTH_COMMIT, expected $BUILD_LABEL — check /tmp/staypointd.err"
    exit 1
elif [ -n "$PID" ]; then
    echo "✗ $LABEL has pid $PID but /api/health did not answer within ${START_TIMEOUT}s — check /tmp/staypointd.err"
    exit 1
else
    echo "✗ $LABEL failed to start within ${START_TIMEOUT}s — check /tmp/staypointd.err"
    exit 1
fi

TOKEN=$(cat "$HOME/.staypoint/auth_token" 2>/dev/null || echo "")

# Repo self-check (STA-687). After a redeploy, git children in a repo have
# hung with no error. The daemon probes every repo at startup (stat, open,
# git rev-parse, git status) and reports the first failing step and its raw
# error in /api/health. Wait for that check and print each failing repo's
# message as the daemon wrote it.
REPO_CHECK_TIMEOUT="${STAYPOINT_REPO_CHECK_TIMEOUT:-30}"
REPOS_URL="http://127.0.0.1:41421/api/health/repos"
HEALTH_BODY=""
deadline=$((SECONDS + REPO_CHECK_TIMEOUT))
while [ -n "$TOKEN" ] && [ "$SECONDS" -lt "$deadline" ]; do
    HEALTH_BODY="$(curl -s -m 2 -H "Authorization: Bearer $TOKEN" "$HEALTH_URL" 2>/dev/null || true)"
    case "$HEALTH_BODY" in *'"repo_access":{"checked":true'*) break ;; esac
    HEALTH_BODY=""
    sleep 1
done
if [ -z "$TOKEN" ]; then
    echo "  ! No auth token at ~/.staypoint/auth_token; skipped the repo check. See $REPOS_URL."
elif [ -z "$HEALTH_BODY" ]; then
    echo "  ! Repo check did not finish within ${REPO_CHECK_TIMEOUT}s; see $REPOS_URL."
elif ! command -v plutil >/dev/null 2>&1; then
    echo "  ! plutil not found; can't summarise the repo check. See $REPOS_URL."
else
    PROBLEMS=""
    MISSING=""
    i=0
    while CAUSE="$(printf '%s' "$HEALTH_BODY" | plutil -extract "repo_access.inaccessible.$i.cause" raw -o - - 2>/dev/null)"; do
        MSG="$(printf '%s' "$HEALTH_BODY" | plutil -extract "repo_access.inaccessible.$i.message" raw -o - - 2>/dev/null || true)"
        case "$CAUSE" in
            missing) MISSING="$MISSING      - $MSG
" ;;
            *) PROBLEMS="$PROBLEMS      ✗ $MSG
" ;;
        esac
        i=$((i + 1))
    done
    if [ -n "$PROBLEMS" ]; then
        echo "!!! staypointd's repo check failed for these repos (first failing step and raw error):"
        printf '%s' "$PROBLEMS"
        echo "    The daemon rechecks every 10 minutes. Full detail: $REPOS_URL"
    else
        echo "✓ Repo check: every existing repo is readable and git works in it."
    fi
    if [ -n "$MISSING" ]; then
        echo "  These checked repo paths no longer exist (from dev configs, active tasks or the harness root):"
        printf '%s' "$MISSING"
    fi
fi

if [ -n "$TOKEN" ]; then
    PORT=$(grep "HTTP and SSE server active" /tmp/staypointd.err 2>/dev/null | grep -oE '127\.0\.0\.1:[0-9]+' | tail -1 | cut -d: -f2 || echo "41421")
    echo ""
    echo "  Web UI: http://127.0.0.1:${PORT}/?token=${TOKEN}"
fi
