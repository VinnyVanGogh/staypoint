# Ops MCP tools with declared effects

task-7d279c9d. Agents used to reach the dev box, StayPoint's own state and
GitHub through shell commands the gate had to guess about: `ssh mansol-dev
'...'`, `cat ~/.staypoint/...`, `cat > /tmp/x.md <<EOF`, `git show ... | bash
-s`, `gh pr merge`. Since 2026-10-01 the Board approved 562 of them by hand.
These tools replace those commands. Each tool **declares its effect** and
**validates its parameters**, so the gate decides from the declaration, not
from shell text.

They are part of the existing `staypoint` MCP server (`staypoint mcp`, tools
`mcp__staypoint__*`). There is no new server.

## Effects and the gate

| Effect | Meaning | Gate |
|---|---|---|
| `read` | changes nothing | runs |
| `dev_write` | changes a dev host, a dev branch, or StayPoint's own records | runs (unattended-run policy: dev deploys OK) |
| `prod_write` | changes production or a production branch | Board gate request; waits for the Board (passkey) |
| `external_write` | changes a third-party service | Board gate request |

An unknown effect needs the Board (fail closed). `internal/opstools/effect.go`
holds the `Decide` function.

**The MCP server enforces the gate itself**, not the PreToolUse hook. The tools
can be reached through the agent CLI, through `staypoint mcp` on piped stdin,
or through the daemon's IPC socket, and a gate in the server covers all three.
A server with no approver refuses every prod and external write: the
daemon's socket server has none, and neither does any test server. `staypoint
mcp` asks the Board the same way the hook does. It creates a gate request whose
cmdline is the call's canonical form (`mcp__staypoint__pr_merge repo=... pr=5
base=main method=merge head=<sha>`), then waits for the hook's budget of
8 minutes. If the Board has not decided by then, the call is not run. The
agent calls again with `approval_gate_id`. That approval only counts for the
same canonical call on the same task.

Every call lands on the run timeline with its effect. Run-step titles look
like `dev_host_run [dev_write] host=mansol-dev action=restart
service=mansol-web`. The server also writes an `ops_tool` activity row on the
task, so calls that bypass the agent CLI are on record too.

## Tools

### `dev_host_run(host, action, ...)`

The tool runs `ssh -T -o BatchMode=yes -o ConnectTimeout=10 <host> <command>`.
The remote command is built from a fixed table, and every value in it is
validated and single-quoted. Output is redacted: the usual secret patterns,
secret-named assignments (`SECRET_KEY=`, `DB_PASSWORD=`, `api_key:`), and
passwords inside URLs. The tool never sources env files with a shell. On
2026-10-09, sourcing `django.env` through bash printed a secret fragment.

| Action | Effect | Parameters | Runs |
|---|---|---|---|
| `git_status` | read | `app`? | `git -C <dir> status --short --branch` |
| `git_log` | read | `lines` (≤200), `app`? | `git -C <dir> log --oneline -n N` |
| `ls` | read | `path`?, `app`? | `ls -la` on the resolved path |
| `cat_file` | read | `path`, `app`? | `head -c 256KiB` on the resolved path |
| `journal_tail` | read | `service`, `lines` (≤2000) | `journalctl -u <svc> -n N --no-pager` |
| `systemctl_status` | read | `service` | `systemctl status <svc> --no-pager -n 20` |
| `http_get_local` | read | `port`, `path` | `curl -sS -m 10 http://127.0.0.1:<port><path>` |
| `git_ff_pull` | dev_write | `branch` | refuses unless the checkout is already on `branch`, then `git pull --ff-only origin <branch>` |
| `collectstatic` | dev_write | `app` | `cd <app dir> && <python> manage.py collectstatic --noinput` |
| `restart` | dev_write | `service` | `[sudo -n] systemctl restart <svc> && systemctl is-active <svc>` |
| `pip_sync` | dev_write | `app` | `cd <app dir> && <python> -m pip install -r <requirements>` |

Validation rules:

- The host must be in `[gates.hosts] dev`. A host that is also listed in
  `prod` counts as prod.
- The host must have a `[gates.ops.dev_hosts.<host>]` table.
- Services, branches and apps must come from that table.
- `ls` and `cat_file` paths are checked twice:
  - Locally, the path must stay under the app dir and must not name an
    env, key or credential file (`.env*`, `*.env`, `*secret*`,
    `*credential*`, `*.pem`/`*.key`/`*.p12`, `id_*`, `.netrc`, `.pgpass`,
    `.ssh`).
  - On the host, the path is resolved with `readlink -f` and the same two
    checks run on the result, so a symlink can't get out of the app dir or
    reach an env file.

### `dev_deploy_verify(repo, sha, page_checks[])`, effect read

This replaces `git show origin/dev-server:scripts/verify_dev_deploy.sh | bash
-s -- <sha> <path>=<marker>...` (MAN-255).

`repo` is a GitHub `owner/name`, and it must be listed in `[gates.ops]
verify_repos`.

1. The tool resolves `refs/heads/dev-server` on GitHub to a commit (`gh api
   repos/<repo>/git/ref/heads/dev-server`), so a tag an agent pushed under
   the same name can't stand in for the branch. It reads the script at that
   commit (`.../contents/...?ref=<commit>`), hashes the bytes locally, and
   checks they match the blob sha GitHub reports. It resolves `main` the same
   way.
2. It refreshes `origin/dev-server` in the working directory.
3. It runs exactly those bytes with `bash -s -- <sha> <checks>`.
4. It returns the PASS/FAIL lines and the final `DEV DEPLOY VERIFIED` /
   `NOT ON DEV` line. If the script never prints a verdict, the tool adds a
   `NOT ON DEV` line.

Agents can push `dev-server`, and they control their local refs and
remotes, so the trust anchor is GitHub. The script runs only when its blob
equals the blob on the repo's `main` branch on GitHub (merged by the Board),
or is listed in `[gates.ops] verify_script_blobs`. Without that check, a
"read" tool would run any code an agent pushed. When the check refuses, the
output names the blob, so the Board can review it and add it.

### `staypoint_query(query, ...)`, effect read

These are named queries only; there is no free-form SQL. They cover the run's
own task (when `STAYPOINT_TASK_ID` is set, any other `task_id` is refused):

- `task`
- `comments`
- `documents`
- `document(key)`
- `handoffs`
- `handoff(name)`: regular files in `~/.staypoint/handoffs/<task>/` only, no
  symlinks, no paths
- `run_steps(limit)`
- `run_errors(limit)`
- `gate_requests(limit)`

Output is redacted.

### `task_comment(text)`, `task_doc(key, text)`, `pr_body(repo, pr, text)`, effect dev_write

These take the text directly, so there is no temp file and no heredoc.
`task_comment` posts as author `agent-comment` and has three guarantees:

- It never wakes the task, so an agent can't re-run itself.
- It never supersedes pending interactions, so an agent can't dismiss a
  card that waits for the Board.
- The harness never feeds it back into the prompt as if the Board wrote it. `pr_body` runs `gh pr edit <pr> --body-file -` and passes the
text on stdin.

### `pr_merge(repo, pr, base, method?)`

The effect depends on the base:

- `dev_write` when the base is `dev-server` or `dev`.
- `prod_write` for `main`, `prod`, or any other base (fail closed). The
  Board's night rule applies: prod merges run only at night unless urgent.

The tool reads the PR with `gh pr view`. The declared `base` must match the
real base, and the PR must be `OPEN`. The merge is pinned with
`--match-head-commit <head the Board saw>`. After approval the tool reads the
PR again and refuses if anything changed.

## Hook: deny with a pointer

When a Bash command (Claude Code or agy) matches one of the patterns these
tools cover, the PreToolUse hook denies it with a pointer to the tool. It
does not hold the command for the Board:

| Shell | Pointer |
|---|---|
| `ssh <host in [gates.hosts] dev> ...` | `dev_host_run` |
| anything running `verify_dev_deploy.sh` | `dev_deploy_verify` |
| `cat/ls/head/tail/sqlite3/grep/find/jq/... ~/.staypoint/...` | `staypoint_query` |
| `cat > /tmp/x.md <<EOF` / `cat <<EOF > /tmp/x.md` | `task_comment` / `pr_body` / `task_doc` |
| `gh [flags] pr merge` | `pr_merge` |

ssh to a prod or unknown host is not redirected. It still goes to the Board
gate. The task brief's `Ops-Tools:` line tells agents about the tools up
front.

## Config

```toml
[gates.hosts]
dev  = ["mansol-dev"]
prod = ["mansol-prod"]

[gates.ops]
# GitHub repos dev_deploy_verify may run scripts/verify_dev_deploy.sh from
verify_repos = ["<owner>/<repo>"]
# Extra Board-reviewed blob shas of that script (beyond the one on main)
verify_script_blobs = []

[gates.ops.dev_hosts.mansol-dev]
app_dir      = "/var/www/mansol_apps"
services     = ["mansol-web", "mansol-worker"]
branches     = ["dev-server"]        # default: dev-server, dev
sudo_restart = true                  # restart via `sudo -n systemctl restart`

[gates.ops.dev_hosts.mansol-dev.apps.billing]
dir          = "/var/www/mansol_apps/billing"
python       = "/var/www/mansol_apps/venv/bin/python"
requirements = "requirements.txt"
```

`[gates.hosts]` is the host-class list from task-97b4fa02. The ssh classifier
there should read the same table.

## Threat model

| Risk | Mitigation |
|---|---|
| Tool used to reach prod | Host must be in `[gates.hosts] dev`, and a host also listed in prod is prod. The host name is validated (no `-o...`, no `user@`). |
| Shell injection through parameters | Every value comes from a regex and/or the config allow-list and is single-quoted; unknown JSON fields are rejected. |
| Path traversal or symlink escape in `cat_file`/`ls` | Lexical check plus an on-host `readlink -f` re-check; a test runs the remote guard against a real symlinked tree. |
| Secret leak in output | Redaction on every result; env/key files are refused; a fixture env test covers it. |
| Agent-pushed verify script runs as "read" | The repo must be in `verify_repos`; the trust anchor is GitHub's main blob, not local refs; the bytes are hashed and must match. |
| Merging main by declaring `base=dev-server` | The declared base must equal GitHub's real base; the effect comes from the base. |
| PR changes while held | The canonical call includes the head sha, `--match-head-commit` pins it, and the PR is re-read after approval. |
| Approval reused for a different call | `approval_gate_id` must match the canonical cmdline and task id. |
| Bypass via `staypoint mcp` stdin or daemon socket | The gate lives in the server; with no approver, prod and external writes are refused. |
| Agent comment wakes itself, dismisses a Board card, or poses as the Board | `AddAgentComment`: fixed author `agent-comment`, no wake, no supersede, excluded from the prompt's comments. |
| Daemon unreachable | The approver fails closed. |

Not covered:

- Board approval runs through the existing gate-request flow, so the
  passkey and the advisors (including tev1 for trusted tasks) apply as they
  do today.
- The night rule appears as reason text for the Board. It isn't enforced by
  a clock.

## Ranking query (Phase 2 input)

Phase 2 starts from the next most-approved categories. Run this against
`~/.staypoint/staypoint.db`, as the Board, from a shell that isn't an agent:

```sql
-- Board approvals by reason since a date
SELECT reasons_json, COUNT(*) AS n
FROM security_gate_requests
WHERE status = 'approved' AND decided_by = 'board' AND created_at >= '2026-10-01'
GROUP BY reasons_json
ORDER BY n DESC
LIMIT 40;

-- Board approvals by leading program
SELECT substr(trim(cmdline), 1, instr(trim(cmdline) || ' ', ' ') - 1) AS program, COUNT(*) AS n
FROM security_gate_requests
WHERE status = 'approved' AND decided_by = 'board' AND created_at >= '2026-10-01'
GROUP BY program
ORDER BY n DESC
LIMIT 40;
```

Phase 1 numbers (from the Board, 2026-10-09):

- ~60 ssh to mansol-dev
- 90 reads under `~/.staypoint`
- 54 `cat > /tmp/*.md <<EOF`
- 32 `python3 - <<EOF` edits
- 105 opaque scripts
- ~25 `verify_dev_deploy.sh`
- 14 `gh pr merge`

`python3 - <<EOF` edits and opaque scripts are not covered yet. They are the
first Phase 2 candidates once the query has been re-run.
