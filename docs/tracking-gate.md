# Tracking gate (STA-854)

Managed Solution work must happen under a StayPoint task. The PreToolUse hook
(`staypoint hook pre-tool`) enforces this in work repos (`router.IsWorkRepo`).

## What is blocked

When the session is not attached to a task and the target is inside a gated repo:

- Claude Code: `Edit`, `Write`, `MultiEdit`, `NotebookEdit`.
- Shell (`Bash`, agy `run_command`): `git commit|push|merge|rebase|cherry-pick|revert|am|pull`,
  `gh pr create|merge`, output redirection or `tee` into a file inside the repo,
  `sqlite3 …staypoint.db`, and anything the parser cannot analyse (fail closed).
- Read-only tools and commands are never blocked. A redirect into `/tmp` from a
  work repo is allowed: only the write's target location counts.

## Unblocking

The block message prints the exact command, with the session id filled in:

    staypoint task create --org 'Managed Solution' --session <id> "<title>"   # local task + attach
    staypoint task attach <task-id> --session <id>                            # existing task

`--session` defaults to `$CLAUDE_CODE_SESSION_ID`. Attaching writes
`task_session_attachments` (migration 34) and an `interactive_session_attached`
entry on the task timeline. An attachment counts only while the task is
`active`. Daemon runs pass through via `STAYPOINT_TASK_ID`. The hook takes that
value from its own process environment, which is inherited from the agent CLI.
A Bash tool `export` cannot set it.

## Per-company setting

The gate is on by default for Managed Solution and off by default for everyone
else. A non-work repo takes its company from the `organization` of the newest
task with that `repo_path`, and otherwise counts as `Personal`. The setting is
stored in `settings_kv` under `gates.tracking.<company>`. You can read it with
`GET /api/settings/tracking-gate` or `staypoint gate tracking`. Changing it
requires `POST /api/settings/tracking-gate`, which is Board-only
(`WrapBoardAction`: Board cookie plus passkey assertion). STA-816's gates page
will use it.

## Fail closed

In a work repo, a missing or unreadable DB, or a failed query, blocks the write.
Personal repos stay open because their default is off.

## Board override

`staypoint gate override --minutes N [--company C] [--reason R]` (max 240):

1. The command refuses to run inside an agent session. It checks
   `STAYPOINT_TASK_ID`, `CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`,
   `CLAUDE_CODE_ENTRYPOINT`, `AI_AGENT`, `GEMINI_CLI` and `ANTIGRAVITY_*`. It
   also refuses when stdin or stdout is not a TTY. This is the soft layer.
2. It only *files* a security gate request (`run_id = tracking-gate-override`,
   cmdline `staypoint gate override --minutes N --company "C"`). The request
   shows up in the Board UI's pending gate requests.
3. **The hard layer:** the override exists only after the Board approves it with
   `POST /api/security/gate-requests/{id}/decide`, which is wrapped by
   `WrapBoardAction`. That means a Board session cookie plus a WebAuthn passkey
   assertion (Touch ID). An agent holding the daemon auth token can create a
   pending request but cannot approve it. The decision is logged to
   `security_gate_audit_log` and `board_audit_log`.
4. The window starts at approval (`decided_at`) and lasts N minutes. The hook
   parses company and minutes from the approved cmdline text, which is exactly
   what the Board saw. Approved requests with a different `run_id` (Red-tier
   approvals) never count.

Known limit: agents run as the same Unix user as StayPoint. An agent could write
`staypoint.db` directly with its own SQLite client and forge a row. The gate
blocks `sqlite3 …staypoint.db` in gated sessions, but it cannot stop every
same-uid write. Attaching to a task is the cheaper path, and it is tracked.

## agy (Gemini / Antigravity CLI)

`~/.local/bin/agy` supports lifecycle hooks: `PreToolUse`, `PostToolUse`,
`PreInvocation`, `PostInvocation` and `Stop` in `hooks.json` (the global root
is `~/.gemini/config/`). This was verified from the hook docs embedded in the
binary. A `PreToolUse` hook gets `{toolCall:{name,args}, conversationId,
workspacePaths}` and returns `{"decision":"allow|deny|ask|force_ask","reason"}`.
StayPoint returns `{}` to defer to agy and `deny` to block. It never returns
`allow`, which would skip agy's own permission prompts.

Board decision 2026-10-06: Gemini never writes code in work repos.

- **Launch refusal.** In a work repo, agy will not start through any of these
  paths. Having a task attached makes no difference.
  - `staypoint --gemini`, and routing that falls back to agy.
  - `staypoint --continue` and `staypoint --resume` into an agy session.
  - The `agy` and `ai` shell wrappers from `staypoint init --shell` or
    `--powershell`, including `agy --force`.

  The wrappers call the hidden `staypoint agy-guard "$PWD"`. The refusal
  message points to `claude --work`.
- **Defense in depth.** The agy hook denies every write to a non-doc file in a
  work repo, and every shell state change there. This applies even with an
  attachment, a daemon task id or a Board override. Doc files (`.md`, `.txt`,
  `.rst`, `.adoc`) fall through to the normal tracking rules.
- **Not covered.** Calling `command agy` or `~/.local/bin/agy` directly skips
  the wrapper. The hook still applies if it is registered. A project-level
  `.agents/hooks.json` reusing the name `staypoint-lifecycle` with
  `"enabled": false` might shadow the global entry. That case is not verified.
  agy's own guardrails flag edits to hooks.json as tampering.

## Registration

`staypoint hook install` registers:

- Claude Code (`~/.claude/settings.json`): `PreToolUse`, matcher
  `Edit|Write|MultiEdit|NotebookEdit|Bash`, command
  `~/.local/bin/staypoint hook pre-tool --tracking-only`. `--tracking-only`
  leaves the Red-tier Board-approval gate to daemon runs, as before.
- agy (`~/.gemini/config/hooks.json`): `staypoint-lifecycle.PreToolUse`,
  matcher `*`, command `~/.local/bin/staypoint hook pre-tool --format gemini`.

Daemon Claude runs get the hook through their per-run `--settings` file
(`internal/adapter/claude.go`).
