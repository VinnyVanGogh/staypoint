# Task stages and origins

`tasks.execution_stage` is the task's place in the workflow. `tasks.status` is
derived from it (`active`, `done`, `soft_deleted`) and is what most list
queries filter on. The constants live in `internal/governance/state_machine.go`.

## Stages

The set matches Paperclip's issue statuses, so an imported issue keeps its
meaning. Paperclip has no separate "running" status. In both systems a running
task is `in_progress`: `Harness.Claim` sets it when a run checks the task out.

| Stage | Paperclip | status | Runnable | Who sets it |
|---|---|---|---|---|
| `backlog` | backlog | active | no | Board, `staypoint import paperclip`, ship-review "Add tests" tasks |
| `todo` | todo | active | yes | default for new tasks; Board; unblock; recovery after a restart |
| `in_progress` | in_progress | active | yes (claimed) | `Harness.Claim` (a run started); Board (Run Now) |
| `in_review` | in_review | active | yes | harness on completion; Board |
| `blocked` | blocked | active | yes | `POST /api/tasks/{id}/block` / `task block` (needs a reason). The watchdog sets `is_blocked` without changing the stage; the board shows either as Blocked |
| `done` | done | done | no | Board / `task done` (needs a work product, no open children) |
| `cancelled` | cancelled | soft_deleted | no | Board; `task cancel`/delete; duplicate cleanup |

Run sub-states of `in_progress`, written only by the harness. They are not
settable from the API, CLI or board:

| Sub-state | Meaning | Board column |
|---|---|---|
| `paused` | run paused at a step boundary, still checked out | In Progress |
| `capped` | turn or budget cap hit; the daemon resets it to `todo` on restart | Todo |
| `stopped` | the Board stopped the run; Run Now starts a new one | Todo |

`rejected` is the governance review outcome (`in_review -> rejected`). It is
closed and not runnable.

### Not runnable

`backlog`, `done`, `cancelled` and `rejected` are never claimed:
`Harness.Claim` refuses them with `ErrNotRunnable`, so no wake (assignment,
comment, queue re-dispatch, MCP `staypoint_wake`) can start a run. Creating a
task in `backlog` does not notify the daemon, and `staypoint_wake` refuses a
non-runnable task outright. A backlog task (or one with no repo) is also never
"the active task" for a repo (`GetActiveTaskForRepo`: hooks, `staypoint
context`, statusline) and never receives the watcher's token spend.

### Transitions

Board-settable stages (`POST /api/tasks/{id}/stage`, `staypoint task stage`,
the TUI board) are `backlog`, `todo`, `in_progress`, `in_review`, `done` and
`cancelled`. The Board can move a task between any of them, with these rules:

- **Run Now from backlog**: setting `in_progress` on a `backlog` task first
  moves it to `todo` and then to `in_progress`. Both steps are logged as
  `stage_change` activity, and then the run is woken.
- **done**: refused while the task has open child tasks, unless the Board
  passes `override`.
- **cancelled**: closes the task (status `soft_deleted`, `deleted_at` set).
  Moving it to any other stage reopens it (status `active`).

The governance state machine (`POST /api/tasks/{id}/transition`, used for
reviewed or approved work) is stricter:

```
backlog     -> todo, cancelled
todo        -> in_progress, blocked, backlog, cancelled
in_progress -> in_review, blocked, todo, cancelled
in_review   -> done (review/approval gates), in_progress, blocked, rejected, cancelled
blocked     -> todo, in_progress, in_review, backlog, cancelled
cancelled   -> backlog, todo
done, rejected: terminal
```

## Origins

`tasks.origin` records where a task came from:

| Origin | Meaning |
|---|---|
| `native` | created in StayPoint (default) |
| `paperclip_import` | created by `staypoint import paperclip` |
| `legacy` | existed before origins were tracked (migration 33) |

Every list surface hides `legacy` tasks by default and has one switch to show
them:

- web board: **Show legacy** toggle (with **Group by company**);
- `GET /api/tasks?include_legacy=1` (or `origin=legacy`);
- `staypoint task list --legacy`;
- MCP `staypoint_task_list` with `include_legacy: true`;
- the TUI board (`staypoint board`) always hides them.

A legacy task is still reachable by id everywhere (task page, `task_get`,
CLI commands).

## Migration 33 (task_origin_and_legacy_cleanup)

Runs once, in one transaction:

1. Adds `tasks.origin` and marks every existing task `legacy`. It only does
   this when it adds the column, so a re-run never relabels newer tasks.
2. Normalizes organization `STA` to `StayPoint`.
3. Merges exact duplicate titles among active legacy tasks: within each group
   of identical trimmed names (case-sensitive) the oldest task is kept and the
   others are cancelled (status `soft_deleted`) with a comment naming the
   keeper. It never touches a task that has a work product (commit, branch,
   PR, file), a ship review card, a worktree base, a live checkout or running
   stage, or open child tasks.

Steps 2 and 3 are idempotent (`db.NormalizeTaskOrganizations`,
`db.MergeLegacyDuplicates`).
