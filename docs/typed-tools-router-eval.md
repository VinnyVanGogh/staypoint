# Typed-tools router eval

StayPoint task `task-4c0af644`, under the design in `task-3b4575f7` (typed read/write MCP tools).
This eval replays every gate hold and every agent bash command through a rule-based router. It
answers two questions: which typed tool would own each command, and which holds actually needed
the Board.

- Script: `scripts/typed_tools_router_eval.py`. It opens `~/.staypoint/staypoint.db` read-only
  (`mode=ro`, `query_only`) and masks tokens, passwords, emails, IPs, GUIDs and long opaque
  strings in everything it prints.
  - `python3 -I scripts/typed_tools_router_eval.py` prints the JSON report.
  - `--selftest` runs 33 labelled regression cases; all 33 pass.
  - `--classify '<cmd>'` routes a single command.
- Snapshot: 404 `security_gate_requests` (2026-10-03 to 2026-10-08 17:44Z) and 4,898 `run_steps`
  rows with `kind='run'`. 2,382 of those carry shell text (2,199 distinct): 2,253 in `command`
  plus 129 where the shell text sits in `title`.
- Excluded: the 736 Gemini/agy `run_command` rows (no text recorded) and the MCP/subagent rows.

## How the router works

- **Splitting.** A command is split on `&&`, `||`, `;`, `|`, `&` and newlines, honouring quotes
  and escapes. `$(...)` and backtick bodies are routed recursively, and so are heredoc bodies
  fed to an interpreter or to `ssh`. When a heredoc writes a file that the same command later
  runs (`cat > q.sql <<EOF … sqlcmd -i q.sql`, `cat > p.sh … bash p.sh`), the router classifies
  the file's contents.
- **Precedence.** Each segment gets a category. The record takes the most privileged one:
  `staypoint_self > git_push > sql_write > ssh_write > http_write > open_pr > file_edit_script > other > bash_write > build_test > sql_read > ssh_read > http_read > bash_read`.
- **Mentioning is not executing.** `bash -n scripts/reinstall-daemon.sh`, `git add …reinstall-daemon.sh`,
  `cat`, `grep` and `launchctl list` are reads. `staypoint_self` is only:
  - running `reinstall-daemon.sh`, `launchctl kickstart|bootout…` on staypoint, `staypointd`, or
    `go install` / `cp` of the staypoint binary;
  - writing into `~/.staypoint`;
  - a write to the daemon API (`:41421`);
  - printing `~/.staypoint/auth_token`. Inside `$(...)` the token read becomes `secret_fetch`
    on the outer call instead.
- **Environment.** Taken from target names: `*-prod-*` / `prod` gives prod; `dev`, `dev-server`,
  `mansol-dev` and `staging` give dev; localhost and sqlite give local. I added an `external`
  value for GitHub and other third-party hosts, since policy treats them differently.
- **Buckets.**

  | Bucket | Meaning |
  |---|---|
  | A | A typed **read** tool auto-allows it: `*_read`, any env, with read-only enforced by the tool. |
  | B | Non-prod write inside the worktree, `/tmp` or dev that trust rules auto-allow: build/test, local git, push to a non-main branch, PR, dev ssh/sql writes. |
  | C | Genuinely needs the Board: prod or external writes, push or merge to main, StayPoint self-ops. |
  | D | Opaque (a script file written earlier, an unknown binary, `$VAR` exec) or a write outside the worktree. Needs a rewrite or a review. |

## Gate holds (404)

### Per category

| Category | Holds | % | Approved | Denied (individual) | Bulk-denied | Pending | Env |
|---|---:|---:|---:|---:|---:|---:|---|
| bash_write | 79 | 19.6 | 20 | 0 | 58 | 1 | local |
| build_test | 71 | 17.6 | 9 | 0 | 61 | 1 | local |
| bash_read | 62 | 15.3 | 19 | 0 | 42 | 1 | local |
| file_edit_script | 53 | 13.1 | 39 | 0 | 12 | 2 | local |
| sql_read | 32 | 7.9 | 29 | 0 | 0 | 3 | 16 prod, 15 local, 1 unknown |
| ssh_read | 28 | 6.9 | 28 | 0 | 0 | 0 | dev |
| git_push | 26 | 6.4 | 11 | 3 | 11 | 1 | external (4 to main) |
| other | 16 | 4.0 | 11 | 0 | 4 | 1 | local |
| ssh_write | 12 | 3.0 | 12 | 0 | 0 | 0 | dev |
| http_read | 8 | 2.0 | 8 | 0 | 0 | 0 | mixed |
| open_pr | 7 | 1.7 | 4 | 0 | 3 | 0 | external |
| staypoint_self | 5 | 1.2 | 0 | 5 | 0 | 0 | local |
| http_write | 4 | 1.0 | 1 | 0 | 3 | 0 | local (StayPoint CLI) |
| sql_write | 1 | 0.2 | 1 | 0 | 0 | 0 | dev |

How to read the status columns:
- **Bulk-denied:** 194 requests that the Board deferred, then denied together in one minute
  (2026-10-08 15:44Z) as a queue clear. They are not judgements of individual commands.
- **Individually decided:** 192 approved and 8 denied.
- **Pending:** 10 still open, 6 of them deferred.

### Would a typed tool have needed the Board?

| Bucket | Holds | % | Approved | Denied (individual) | Bulk-denied | Pending |
|---|---:|---:|---:|---:|---:|---:|
| A: typed read tool auto-allows | 130 | 32.2 | 84 | 0 | 42 | 4 |
| B: non-prod or in-worktree write, trust auto-allows | 238 | 58.9 | 94 | 0 | 141 | 3 |
| C: genuinely Board | **9** | **2.2** | 1 | **8** | 0 | 0 |
| D: opaque, needs a rewrite or review | 27 | 6.7 | 13 | 0 | 11 | 3 |

**Every individually denied request (8 of 8) lands in C.** The other 9 in 10 (A+B, 91%) are
reads or local/dev writes that a typed tool would auto-allow. Bucket C contains:
- three `git push origin HEAD:main`;
- five attempts to call Board-only daemon endpoints (`ship-review/approve`,
  `gate-requests/<id>/decide`) or to print the auth token, including a forged
  `-b staypoint_board=x` cookie;
- one `gh pr merge` to a work repo's default branch. This is the only approved request in C.

### Top false-positive causes in today's classifier

"Auto-allowable" means held by today's classifier but routed to bucket A or B. A hold can carry
more than one reason, so the rows overlap.

| Today's reason | Holds | Auto-allowable | Root cause |
|---|---:|---:|---|
| Self-protect rule ("rebuilds/replaces StayPoint or its guards") | 143 | 143 (100%) | A path or word mention: `bash -n` / `git add` of `reinstall-daemon.sh`, a commit message that names it. The router found **0 executions** of reinstall among them. |
| Opaque script (bash/python runs a file) | 108 | 103 | Heredoc `python3 - <<EOF` edits and `bash -n` syntax checks |
| SSH remote shell | 38 | 38 | Any `ssh host 'cmd'` is held. 24 were pure reads (`systemctl status`, `journalctl`, `git log`). |
| Unparseable (heredoc / quotes) | 37 | 36 | The parser gives up on heredocs and multi-line `-m` messages |
| Prod-write rule | 29 | 28 | The word "prod" in commit/PR text or a script name, and prod **SELECTs** (16) |
| Sensitive path `~/.staypoint` | 28 | 27 | `ls` or `sqlite3 -readonly` of the db, and token use inside `$(...)` |
| Nested agent / StayPoint env | 23 | 23 | `grep agy`, `readlink ~/.local/bin/agy`, `env -u STAYPOINT_TASK_ID go test` |
| Indirect `$VAR` / `$(...)` / eval | 22 | 17 | Secrets pulled with `$(az keyvault …)` / `$(az account get-access-token)`, and `$(cat token)` |

Masked examples:

- **Heredoc edits** (39 held heredoc file edits):
  - `python3 - <<'EOF' \n p='internal/workspace/base.go' \n s=open(p).read() \n s=s.replace('''…` → "unparseable command: unterminated single quote"
  - `python3 - <<'EOF' \n p='docs/user-lifecycle-webroot.md'; s=open(p).read() …` → "bash: runs an opaque script"
  - `cat >> internal/server/handlers_ship_review.go <<'EOF' … PUT /api/tasks/{id}/test/base …` → "command writes to an external API"
- **Path mentions** (143 self-protect holds, 0 real self-ops):
  - `bash -n scripts/reinstall-daemon.sh` → "rebuilds, restarts or replaces StayPoint"
  - `git add .github/workflows/ci.yml CONTRIBUTING.md scripts/reinstall-daemon.sh scripts/git-hooks/pre-commit` → same
  - `git fetch --all --prune -q; git status -sb | head -3; git log --oneline -3 origin/main; ls -a; …` → same
- **SELECT read as a prod write** (16 prod `sql_read` holds; the MAN-602 pattern):
  - `cd /tmp && timeout 90 sqlcmd -S sql-…-prod-eastus… -d sqldb-…-prod-eastus --authentication-method ActiveDirectoryAzCli -Q "SET LOCK_TIMEOUT 5000; SELECT id, name F…"` → "may write to prod"
  - `cat > /tmp/man602_q3.sql <<'EOF' \n SET NOCOUNT ON; \n -- per tenant: latest snapshot … SELECT COUNT(*) …` then `sqlcmd -i` → same
  - `git commit -m "fix(names): NUL cut … \"x\\x00 to prod\" hid prod …"` → same (the word "prod" in a commit message)
- **Secrets fetched with `$(...)`** (18 holds flagged `secret_fetch`):
  - `export SQLCMDPASSWORD=$(az account get-access-token --resource https://database.windows.net/ …); sqlcmd -S …-prod… -Q "SELECT …"` → "may write to prod" / "indirect command"
  - `V=kv-…-prod; g(){ az keyvault secret show --vault-name $V -n "$1" --query value -o tsv; }; export PARTNER_CENTER_REFRESH_TOKEN=***` → "indirect command"
  - `TOK=$(cat ~/.staypoint/auth_token); curl -s -H "Authorization: Bearer ***" http://localhost:41421/api/tasks/…` → "touches sensitive path"

## All agent bash commands (run_steps, 2,382 commands)

What share of all agent bash each typed tool would own. The tool-family view folds local git
writes (`add`/`commit`/`checkout`) into `git`.

| Tool family | Commands | % of agent bash | Gate holds | % of holds |
|---|---:|---:|---:|---:|
| bash_read | 1,373 | 57.6 | 62 | 15.3 |
| build_test | 284 | 11.9 | 71 | 17.6 |
| git (commit, push to branch, PR; main stays gated) | 189 | 7.9 | 85 | 21.0 |
| file edits by script (should be native Edit/Write) | 173 | 7.3 | 53 | 13.1 |
| http (read 103, write 42) | 145 | 6.1 | 12 | 3.0 |
| other / opaque | 89 | 3.7 | 16 | 4.0 |
| bash_write (non-git) | 59 | 2.5 | 27 | 6.7 |
| ssh (read 29, write 12) | 41 | 1.7 | 40 | 9.9 |
| sql (read 27, write 1) | 28 | 1.2 | 33 | 8.2 |
| staypoint_self | 1 | 0.0 | 5 | 1.2 |

- Bucket mix over all agent bash: A 64.3%, B 31.1%, C 0.2%, D 4.4%.
- Prod-targeted commands in run_steps: 16, all reads (11 `sql_read`, 4 `http_read`, 1 `bash_read`).
- SSH and SQL are rare in agent bash (2.9%) but account for 18% of holds. Almost every SSH or SQL
  command is held, because the classifier cannot tell a read from a write.

## Recommendation: the first three tools

1. **`bash_read`**: read-only by enforcement, through an allowlisted command set and no writes
   outside `/tmp`. A `staypoint_self` exec check runs on the parsed command, never on path
   mentions.
   - It covers 57.6% of all agent bash and 15.3% of holds.
   - Fixing "mention ≠ execute" in the same change removes the largest false-positive class
     (143 self-protect holds, none of them real).
2. **`build_test`**: `go build/vet/test`, `npm/bun test|build|lint`, `scripts/check-*`,
   `scripts/ui-e2e.sh`, fuzz and lint. It is pinned to the task worktree and auto-allowed.
   - It covers 11.9% of agent bash and 17.6% of holds. All 71 build/test holds were bulk-denied
     or approved, none denied.
3. **`git`**: `git_commit` (stages explicit paths), `git_push` (branch enum excludes
   main/master/default, so pushing there is a separate gated function), and `open_pr`.
   - It covers 7.9% of agent bash and 21.0% of holds (the largest single family).
   - Pushes and merges to main stay Board-only, and they are 4 of the 9 genuine Board holds.

Together these three would have auto-cleared **214 of 404 holds (53%)** and cover **77% of all
agent bash**, while every request the Board actually denied stays gated.

Two cheap follow-ups, before any further tools:

- **Steer heredoc edits to native Edit/Write.** This is a PreToolUse refusal, not an MCP tool.
  It clears another 53 holds (13%) and needs no new tool.
- **Next tools: `sql_read` and `ssh_read`, with vault credential references.**
  - Only 2.9% of agent bash, but 73 holds, all approved or pending.
  - This is where secrets travel through argv today (`$(az keyvault …)` /
    `$(az account get-access-token)`), so the vault and read-only login gain the most here.

## Caveats and assumptions

- The router is heuristic. Known gaps:
  - Commit messages with unbalanced escaped quotes sometimes split oddly.
  - A `cd` is tracked only within one command line.
  - A script written by an earlier command stays opaque (bucket D).
- Unverified assumptions:
  - The bare ssh alias `mansol` is prod. No held command uses it.
  - `verify_dev_deploy.sh` is a read-only dev verifier. It was classified `ssh_read`/dev without
    reading the script.
- `staypoint task comment|create|block` is counted as a local `http_write` (B). A StayPoint MCP
  tool already covers these.
- The DB is live. Re-running the script gives current numbers, and the brief's earlier figure of
  401 holds has since grown to 404.
