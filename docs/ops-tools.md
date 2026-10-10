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
A server with no approver refuses every prod and external write. The
daemon's socket server runs no ops tools at all: it has no trusted ops data
dir (see "Which servers run ops tools" below). `staypoint
mcp` asks the Board the same way the hook does. It creates a gate request whose
cmdline is the call's canonical form (`mcp__staypoint__pr_merge gh_repo=<owner/name>
pr=5 base=main method=merge head=<sha>`), then waits for the hook's budget of
8 minutes. If the Board has not decided by then, the call is not run. The
agent calls again with `approval_gate_id`. That approval only counts for the
same canonical call on the same task.

Only the Board approves these requests. A task trust (plain or tev1) or an
org trust never does: `security.AnalyzeForTrust` marks every
`mcp__staypoint__` cmdline protected, whatever its text holds, so the hold
does not depend on a text rule happening to match (Board review 2026-10-09).
A Board allow rule the Board itself wrote still applies.

### Which servers run ops tools

Only a server the harness started for a run (Board review #2, 2026-10-09).
An agent could otherwise run `printf '{}' | env -u STAYPOINT_TASK_ID
HOME=/tmp/fakehome PATH=/tmp/bin:$PATH staypoint mcp`: the server read
`/tmp/fakehome/.staypoint/config.toml`, so a dev host at any address with
prod services and `branch=main` ran as `dev_write`, and with no task there
was no activity row. Four things close that:

- **Run token.** The harness gives each run `STAYPOINT_RUN_MAC`, an
  HMAC-SHA256 of the task ID under `<data dir>/ops_key` (32 random bytes,
  mode 0600, created on first use; `internal/opstools/origin.go`). The
  server runs an ops tool only when `STAYPOINT_TASK_ID` is set and the token
  checks out. An agent can read its own run's token, but cannot mint one for
  another task without the key. A server with no task, another task's ID,
  or a missing or wrong token refuses every ops tool; the other `staypoint_*`
  tools are unaffected. The hook denies Read, Grep and Glob calls whose path
  names `ops_key`, `auth_token` or `board_token` under `.staypoint`, or that
  search the whole data dir.
- **Pinned config.** `staypoint mcp` loads its config from the account's
  home in the user database (`os/user`, not `$HOME`), resets `HOME` to it,
  and checks the run token against that data dir. A fake `HOME` changes
  nothing.
- **Scrubbed children.** Every process an ops tool starts (`gh`, `git`,
  `ssh`, `bash`) gets the server's environment minus `GH_*`, `GITHUB_*`,
  `GIT_*`, `XDG_CONFIG_HOME`, `BASH_ENV`, `ENV`, proxy and CA-bundle
  variables, `DYLD_*`/`LD_*` and exported shell functions, with `PATH` set to
  `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`. Commands
  are looked up on that PATH, not the server's. `GH_HOST` / `GH_CONFIG_DIR`
  can no longer point `dev_deploy_verify`'s `gh api` at another host.
- **Red in the shell.** `staypoint mcp`, and any `staypoint` call that sets
  or unsets `HOME`, `PATH`, `USER`, `TMPDIR`, `XDG_*`, `GH_*`, `GITHUB_*`,
  `GIT_*`, `STAYPOINT_*`, `DYLD_*` or `LD_*` (as a prefix or through `env`,
  including any `env` flag), is Red. The harness starts the MCP server
  itself; an agent never needs to.

A renamed copy of the binary (`go build -o /tmp/x ./cmd/staypoint && /tmp/x
mcp`) is not caught by name. It gets the same pinned config and the same
token check, so it can do only what the agent's own MCP tools already do,
and its calls are logged on the agent's own task.

Every call lands on the run timeline with its effect. Run-step titles look
like `dev_host_run [dev_write] host=mansol-dev action=restart
service=mansol-web`. The server also writes an `ops_tool` activity row on the
task, so calls that bypass the agent CLI are on record too.

## Tools

### `dev_host_run(host, action, ...)`

The tool runs `ssh -T -F <ssh_config> -o BatchMode=yes -o ConnectTimeout=10
-o HostName=<host_name> -o ProxyCommand=none -o ProxyJump=none -o
ControlMaster=no -o ControlPath=none -o PermitLocalCommand=no -o
StrictHostKeyChecking=yes <host> <command>`.

Both `host_name` and `ssh_config` are required, and `ssh_config` must be a
file inside the StayPoint data dir (e.g. `~/.staypoint/ssh_config`), which
agents cannot write. With `-F`, ssh reads only that file: not
`~/.ssh/config`, its `Include`s, or `/etc/ssh/ssh_config`. The `-o` pins
alone were not enough (Board review #2 #6): `~/.ssh/config` can still set
`Port`, `User`, `KnownHostsCommand`, `PKCS11Provider` and `IdentityAgent`,
and a `Match exec` line runs a local command while ssh parses it. Put the
host's `User`, `Port` and `IdentityFile` in the StayPoint file.

The remote command is built from a fixed table, and every value in it is
validated and single-quoted. The tool never sources env files with a shell.
On 2026-10-09, sourcing `django.env` through bash printed a secret fragment.

Output is redacted (`internal/opstools/redact.go`), on top of
`security.Redact`:

- Private key blocks, to the end of the output when the END line is missing
  (a capped `cat_file` can cut it off).
- Secret-named assignments (`SECRET_KEY`, `*PASSWORD*`, `*_PASS`, `*PWD*`,
  `*_PW`, `*_SK`, `*SALT*`, `*TOKEN*`, `*_KEY`, `key`, `*api_key*`, `*dsn*`,
  `*auth*`, `*session_id*`, `*cookie*` including `Set-Cookie:` headers, ...):
  a quoted value (backslash escapes honoured, Python triple quotes to their
  end) or else everything to the end of the line, following `\` line
  continuations onto the next lines. A bare `PASS:` is not a secret name, so
  verify output survives.
- Passwords in URLs, with an empty user (`redis://:pw@host`), up to the
  last `@` in the URL (`user:p@ss@host`), and with `/` or `?` in them
  (`user:pa/ss@host`). `host:8000/path?next=a@b` is a port, not a password,
  when the part before `:` is a dotted host or `localhost`.
- `mysql`/`mysqldump`/`mysqladmin ... -pPASSWORD` (`-P` is the port and
  stays).

Redaction masks values, never whole lines. A stdout line missing from a
result was a capture bug, not redaction (Board review #2 L2): the runner
gave stdout an `io.MultiWriter` and stderr the same buffer, and every
stdout line was lost from the combined output, so `dev_host_run` returned
only `exit 0`. One locked writer now takes both streams.

| Action | Effect | Parameters | Runs |
|---|---|---|---|
| `git_status` | read | `app`? | `git -C <dir> status --short --branch` |
| `git_log` | read | `lines` (≤200), `app`? | `git -C <dir> log --oneline -n N` |
| `ls` | read | `path`?, `app`? | `ls -la` on the resolved path |
| `cat_file` | read | `path`, `app`? | `head -c 256KiB` on the resolved path, text files only |
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
  - `cat_file` also needs a text file: an allowlisted extension (`.py`,
    `.txt`, `.md`, `.html`, `.css`, `.js`, `.json`, `.yaml`, `.toml`,
    `.cfg`, `.ini`, `.conf`, `.service`, `.sh`, `.xml`, templates, source
    files, ...) or a known name (`Dockerfile`, `Makefile`, `Procfile`,
    `README`, `LICENSE`, ...). Data files are refused, not returned
    unredacted: `db.sqlite3`, `*.db`, `*.log`, `*.pyc`, `*.pickle`, `*.sql`
    dumps, archives (Board review #2 M2: a Django `db.sqlite3` holds
    sessions, OAuth tokens and PII).
  - On the host, the path is resolved with `readlink -f` and the same
    checks run on the result, so a symlink can't get out of the app dir,
    reach an env file, or give a database a text name.

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
2. It checks that the working directory's `origin` (as `git remote get-url`
   resolves it) is `github.com/<repo>`, so a repointed origin can't make a
   fork's `dev-server` look deployed. It then fetches
   `+refs/heads/dev-server:refs/remotes/origin/dev-server` from that exact
   URL (not the remote's config, which could change in between) and stops
   with `NOT ON DEV` if the fetch fails. The fetch still honours local git
   config an agent can edit (`url.*.insteadOf`, `core.sshCommand`), so the
   anchor is GitHub's API: after the fetch, local `origin/dev-server` must be
   the commit the API reported for `refs/heads/dev-server`, or the script
   does not run. Local objects can still lie (`refs/replace`, a planted
   commit sharing a short sha's prefix), so GitHub resolves `sha` to one full
   commit id (`commits/<sha>`), that full id is what the script gets, and
   GitHub decides ancestry: `compare/<full>...<dev-server commit>` must be
   `identical` or `ahead`. The script and the rev-parse run with
   `GIT_NO_REPLACE_OBJECTS=1`.
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
own task (`STAYPOINT_TASK_ID`, proven by the run token; any other `task_id`
is refused):

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

### `task_comment(text)`, `task_doc(key, text)` (dev_write), `pr_body(repo, pr, text)`

These take the text directly, so there is no temp file and no heredoc.
`task_comment` posts as author `agent-comment` and has three guarantees:

- It never wakes the task, so an agent can't re-run itself.
- It never supersedes pending interactions, so an agent can't dismiss a
  card that waits for the Board.
- The harness never feeds it back into the prompt as if the Board wrote it.

`pr_body` reads the PR (`gh pr view`), takes its GitHub repo from the PR's
own URL, and runs `gh pr edit <pr> --repo <owner/name> --body-file -` with the
text on stdin. Its effect:

- `dev_write` only for the task's own PR: its head branch is the task's
  branch (`staypoint/<task id>`), it is in the task's own GitHub repo (the
  `origin` of the task's configured checkout, not the agent's working
  directory), and that repo is listed in `[gates.ops] own_repos`. Another
  task's PR or any other branch's in the same repo goes to the Board (Board
  review #2 #7). Worktrees share the checkout's git config, so an agent can
  repoint `origin`; it cannot add its repo to the Board's config list.
- `external_write` (Board) for anything else, including when the task's repo
  is unknown. The canonical call holds the text's full sha256, so the
  approval covers exactly that text.

The run timeline declares `pr_body` as `external_write` (the upper bound);
the result line says which effect it ran as.

### `pr_merge(repo, pr, base, method?)`

The effect depends on the base:

- `dev_write` when the base is `dev-server` or `dev`.
- `prod_write` for `main`, `prod`, or any other base (fail closed). The
  Board's night rule applies: prod merges run only at night unless urgent.

The tool reads the PR with `gh pr view` in `repo`. The GitHub `owner/name`
comes from the PR's URL and goes into the canonical call and the Board's
reason. The declared `base` must match the real base, and the PR must be
`OPEN`. After approval the tool reads the PR again from `repo` and refuses if
anything changed, including the GitHub repo (a remote repointed after the
approval). Immediately before the merge it reads the PR once more straight
from the REST API (`gh api repos/<owner/name>/pulls/<pr>`): base, head sha
and state must still be the decided base, the head the Board saw, and
`open` (Board review #2 M1). The merge runs as `gh pr merge <pr> --repo
<owner/name> --<method> --match-head-commit <head the Board saw>`, and
afterwards the tool checks the PR is `MERGED` into the decided base.

GitHub's merge call takes a head sha but not a base, so a base swapped in
the instant between the API read and the merge can't be prevented; the
post-merge check reports it as an error. The shell side of that race is
closed: `gh pr edit --base` / `-B` is Red, as is any `gh api` call with a
method other than GET/HEAD or a body (`PATCH .../pulls/<n>` included), so an
agent cannot retarget a PR without the Board.

## Hook: deny with a pointer

When a Bash command (Claude Code or agy) in a StayPoint run matches one of
the patterns these tools cover, the PreToolUse hook denies it with a pointer
to the tool. It does not hold the command for the Board. Outside a run
(no `STAYPOINT_TASK_ID`) the ops tools refuse, so the hook does not point
there; the command goes to the normal gate.

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
# GitHub repos tasks work in: pr_body edits the task's own PRs there unattended
own_repos = ["VinnyVanGogh/staypoint"]

[gates.ops.dev_hosts.mansol-dev]
host_name    = "<dev host address>"  # required: ssh connects here
ssh_config   = "/Users/<you>/.staypoint/ssh_config"  # required: ssh -F; must be inside the data dir
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
| Secret leak in output | Redaction on every result; env/key files are refused; a fixture env test and a table test of every gap the Board found cover it. |
| ssh config redirects the dev alias | A required StayPoint-owned `-F` file inside the data dir replaces `~/.ssh/config` (no `Match exec`, `Include`, `KnownHostsCommand` from it); `HostName`, no proxy, no control socket and strict host keys are pinned on the command line too. |
| Agent starts its own `staypoint mcp` with a fake HOME/config, no task, or another task's ID | Ops tools need the harness's per-run HMAC token; config is loaded from the real account home; child env scrubbed and PATH pinned; `staypoint mcp` and env-rewritten `staypoint` are Red; Read/Grep of the ops key is denied. |
| `gh pr edit --base main` during a dev merge | `gh pr edit --base` and `gh api` writes are Red; base, head and state are re-read from the REST API right before the merge. |
| `cat_file` returns a database or log | Text-file allowlist, checked locally and on the host after `readlink -f`. |
| Agent-pushed verify script runs as "read" | The repo must be in `verify_repos`; the trust anchor is GitHub's main blob, not local refs; the bytes are hashed and must match; local `origin` must be that repo. |
| Merging main by declaring `base=dev-server` | The declared base must equal GitHub's real base; the effect comes from the base. |
| PR changes while held | The canonical call includes the GitHub repo and head sha, `--match-head-commit` pins it, the PR is re-read after approval, and the merge names `--repo` explicitly. |
| Remote repointed after approval | The GitHub repo is re-resolved and must equal the approved one. |
| Trusted task's prod merge auto-approved | Ops-tool canonicals are protected under any trust and tev1. |
| `pr_body` edits someone else's PR | Unattended only for the task's own PR (head `staypoint/<task id>` in the task's own listed repo); otherwise the Board. |
| Approval reused for a different call | `approval_gate_id` must match the canonical cmdline and task id. |
| Bypass via `staypoint mcp` stdin or daemon socket | The gate lives in the server; with no approver, prod and external writes are refused. |
| Agent comment wakes itself, dismisses a Board card, or poses as the Board | `AddAgentComment`: fixed author `agent-comment`, no wake, no supersede, excluded from the prompt's comments. |
| Daemon unreachable | The approver fails closed. |

Not covered:

- Board approval runs through the existing gate-request flow, so the
  passkey and the advisors apply as they do today; trusts and tev1 never
  approve an ops-tool request.
- A Board-written allow rule (exact or prefix) that matches an ops-tool
  canonical still approves it: that is a Board decision.
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
