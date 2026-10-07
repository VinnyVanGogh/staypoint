# Parallel run limits

The daemon (`staypointd`) runs several agent runs at once. Four caps decide
whether a run starts now or waits in the run queue (STA-773, STA-867).

| Cap | config.toml key | Default | Counts |
|---|---|---|---|
| Global | `max_concurrent_runs` | 9 | every running agent run |
| Per repo | `max_runs_per_repo` | 3 | runs in one git repo (each in its own task worktree) |
| Per folder | none (fixed) | 1 | runs in one plain, non-git folder |
| Per organization | `max_runs_per_org` | 3 | runs of tasks in one organization |
| Per organization, override | `[run_limits.orgs]` | none | replaces `max_runs_per_org` for a named organization |

The global default of 9 is 3 organizations x 3 runs. Quota still applies on top
of these caps: when every provider seat for a task's pool is locked by the
pacer, the run waits in the queue (reason `quota`) until a seat frees up.

## Configuring

`~/.staypoint/config.toml`. The three caps are top-level keys, so they must
come before any `[table]`:

```toml
max_concurrent_runs = 9
max_runs_per_repo = 3
max_runs_per_org = 3

[run_limits.orgs]
"Managed Solution" = 3
StayPoint = 4
Unassigned = 1
```

- A missing, zero or negative value uses the default.
- Organization names in `[run_limits.orgs]` match the task's organization
  case-insensitively. Tasks with no organization share the `Unassigned`
  bucket.
- The daemon reads these at startup and logs the values it applied
  (`run limits` line). Restart it after changing them.

## What counts as a repo or a folder

- A task whose `repo_path` is inside a git repo counts against that repo. Any
  path inside the repo (the main checkout, a subfolder, or a worktree) counts
  as the same repo.
- A task whose `repo_path` is an existing folder that is not in a git repo
  runs in that folder directly, with no worktree, so runs in the same folder
  share every file. Only one runs at a time.
- A task with no `repo_path` runs in its own scratch folder, so it is limited
  only by its organization and the global cap.

## The queue

A run refused by a cap is queued and starts on its own when capacity frees.
The task page shows `Queued: N runs ahead (reason)`, where the reason names the
cap holding the run:

| Wait | Shown as |
|---|---|
| `repo` | repo limit: this repo is at max_runs_per_repo |
| `folder` | repo limit: another run is using this non-git folder |
| `org` | org limit: this organization is at max_runs_per_org |
| `slots` | global limit: max_concurrent_runs reached |
| `quota` | quota: provider quota is locked |

The queue is first in, first out, with one exception so that one organization
cannot starve the others: when a run is held by its repo, folder or
organization cap, runs queued behind it from other repos or organizations
still start. Only the global cap stops everything behind it. A new run (not
yet queued) never takes capacity that an already queued run could start with.

## Git operations shared between runs in one repo

Runs in the same repo each get their own worktree (`.worktrees/<task>`), but
all worktrees of a repo share its refs, `packed-refs`, `.git/config` and the
`.git/worktrees` admin area. Git refuses rather than waits when two writers
meet (`cannot lock ref`, `Unable to create packed-refs.lock`), so the daemon
takes a per-repo lock around each of its git calls that writes that shared
state, for the length of that one call (`internal/gitexec/repolock.go`). Calls
are grouped so a slow fetch never holds up a quick ref write:

| Lock | Git calls |
|---|---|
| fetch | `fetch`, `pull`, `push`, `remote update` (task base fetch, git preflight, ship review merge and cleanup) |
| worktree | `worktree add/remove/move/repair/prune` (task and dev-server worktrees) |
| refs | `update-ref`, `branch`, `tag`, `pack-refs`, `gc`, `repack`, `prune`, writes to `config` and `remote` (task base pin, checkpoint refs, branch cleanup) |

Reads (`rev-parse`, `status`, `diff`, ...) and calls that touch only one
worktree's index and HEAD run without a lock. Waiting for a lock counts against
the git call's own timeout. Git commands an agent runs itself are outside this
lock; git's own lock files still protect them.
