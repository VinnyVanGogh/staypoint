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
| `backlog` | backlog | active | no | Board, `staypoint import paperclip`, ship-review "Add tests" tasks, every agent-created task |
| `todo` | todo | active | yes | default for new tasks; Board; unblock; recovery after a restart |
| `in_progress` | in_progress | active | yes (claimed) | `Harness.Claim` (a run started); Board (Run Now) |
| `in_review` | in_review | active | yes | harness on completion; Board |
| `blocked` | blocked | active | yes | `POST /api/tasks/{id}/block` / `task block` (needs a reason). The watchdog sets `is_blocked` without changing the stage; the board shows either as Blocked |
| `done` | done | done | no | `task done` (needs a work product: `--pr <url>` or `task product add`; no open children). The Board's task-page Mark done (Board gate, `{"board": true}`) needs no work product and records its note on the timeline |
| `cancelled` | cancelled | soft_deleted | no | Board; `task cancel`/delete; duplicate cleanup |

Run sub-states of `in_progress`, written only by the harness. They are not
settable from the API, CLI or board:

| Sub-state | Meaning | Board column |
|---|---|---|
| `paused` | run paused at a step boundary, still checked out | In Progress |
| `capped` | turn or budget cap hit; the daemon resets it to `todo` on restart | Todo |
| `stopped` | the Board stopped the run; not runnable: only Run Now (or a Board stage change) resumes it, a comment or other wake does not | Todo |
| `error` | the run ended in error (the harness writes its disposition as the stage); not runnable, like `stopped`: only Run Now resumes it | Todo |

`rejected` is the governance review outcome (`in_review -> rejected`). It is
closed and not runnable.

### Not runnable

`backlog`, `stopped`, `error`, `done`, `cancelled` and `rejected` are never
claimed: `Harness.Claim` refuses them with `ErrNotRunnable`, so no wake
(assignment, comment, decision-card answer, queue re-dispatch, MCP
`staypoint_wake`) can start a run.

Decision cards (task-e3fe2c0b): answering a card wakes the task unless the
answer is sent with `"resume": false` ("Accept only"). On a task that is not
waiting mid-run (anything but `todo`, `in_progress`, `blocked`, `paused`) the
web UI defaults to Accept only; "Accept & resume" there records the answer and
then presses Run Now, so Run Now's Board, hold and backlog gates apply. Creating a
task in `backlog` does not notify the daemon, and `staypoint_wake` refuses a
non-runnable task outright. A comment does not notify the daemon for a
non-runnable task, or while a Board stop is pending for the run still holding
the task.

Interactive tasks (STA-861): `staypoint task create --org`/`--session` creates
the task in `backlog`, and `staypoint task attach` parks an unclaimed `todo`
task in `backlog`, so the daemon never starts a run that duplicates the
attached session's work. The Board's Run Now still runs it. A backlog task (or one with no repo) is also never
"the active task" for a repo (`GetActiveTaskForRepo`: hooks, `staypoint
context`, statusline) and never receives the watcher's token spend.

### Surprise starts: agent-created tasks and org holds (2026-10-07)

Creating a task in a runnable stage wakes it at once, so two rules keep
tasks from starting without the Board:

- **The requested stage is honoured on every create path**, children
  included: `POST /api/tasks` with `parent_id` and `execution_stage: backlog`
  creates a parked child that is never woken. `staypoint task create
  --backlog` parks the local task too.
- **Agent-created tasks always start in `backlog`** with origin `agent`,
  whatever stage was asked for. Agent-created means: `staypoint_task_create_child`
  (MCP), `POST /api/tasks` without a Board session (the agent token alone),
  or `staypoint task create` inside an agent session (`CLAUDECODE`,
  `STAYPOINT_TASK_ID`, ... set). Only the Board moves such a task from a
  parked stage (`backlog`, `cancelled`, `stopped`) to a runnable one: the
  stage endpoint needs the Board session cookie, `staypoint task stage`
  refuses inside agent sessions or without a terminal, and
  `POST /api/tasks/{id}/transition` refuses it outright. A prod-targeting
  task (name says prod/production, or its repo is a `live_credentials`
  project) also needs the passkey (Touch ID), so it can only leave backlog
  from the Board UI. Closing (`done`, `cancelled`) needs no Board.

**Org hold** is a Board-only switch per organization (`settings_kv`
`org_hold.<lower(org)>`). While it is on, `Harness.Claim` refuses every
task in that organization (`ErrOrgHeld`), so no wake can run one; the
assignment, comment, blocker, MCP `staypoint_wake` and Run Now paths refuse
it first and log a `wake_held` activity row. Set it from Settings or the
organization card in the Board UI (`POST /api/settings/org-hold`, Board
session + passkey), or place it with `staypoint org hold <org>` from the
Board's own terminal. Lifting it needs Touch ID, so the CLI cannot; agents
can read it (`GET /api/settings/org-hold`) but never set it. Held
organizations show a `HELD` badge.

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

### Stage vs Running in the web UI (task-3387cad2)

Every task list (Overview, Global Task Status, Recent Tasks, All Tasks,
Kanban cards, Projects cards, org pages, the fleet modal) and the task page
header show two separate things:

- **Stage**: Backlog, Todo, In progress, In review, Blocked, Done or
  Cancelled (`lib/taskstate.js` `taskStageKey`). Lists never say "active":
  `tasks.status = active` only means "not closed", and the Board read it as
  running.
- **Running**: a badge with the run's elapsed time, shown only while a run is
  in flight: `checkout_run_id` is set **and** the daemon's run slot is held
  (`orchestrator.GlobalRunSlots.LiveRuns`). Stage `in_progress` alone is not
  running; a stopped or crashed run leaves it behind.

`GET /api/runs/live` lists the runs in flight (`task_id`, `run_id`,
`organization`, `started_at`). The UI polls it every 10 s and on run, queue
and task events. The Running badges, the Running filter, the sidebar's
"N running" per organization and the overview's Running KPI all read it.
`GET /api/tasks` and `GET /api/tasks/{id}` carry the same `running` and
`run_started_at`, and `GET /api/tasks?running=1|0` filters on it. The fleet
overview's `task_counts.running` counts the same live runs. The CLI's
`staypoint fleet` can't see the daemon's slots, so it counts any checkout.
A run started by `staypoint run` in another process holds no daemon slot, so
it doesn't show as running.

Each list has a **Running** (any / running / not running) filter and a
**Stage** multi-select next to Organization, Project and Priority. Each page
keeps its own filter in `localStorage` (`staypoint_list_filter_<page>`).

The fleet overview lists each Paperclip company under the organization the
import mapped it to (`paperclipimport.OrganizationFor`: "RuneLite Plugins" ->
RuneLite, "Research & Intelligence" -> Research, "Maintenance sweep across
repos" -> Maintenance). It used to list both names. This is a display merge
only: no task's `organization` changes. A local task wins over the frozen
Paperclip issue that has its ID or title. Paperclip issues are legacy, so they
are left out of the counts unless legacy is included, and none of them counts
as running.

## Origins

`tasks.origin` records where a task came from:

| Origin | Meaning |
|---|---|
| `native` | created in StayPoint (default) |
| `paperclip_import` | created by `staypoint import paperclip` |
| `legacy` | existed before origins were tracked (migration 33) |

Every list surface hides `legacy` tasks and **archived imports** (origin
`paperclip_import` in stage `done` or `cancelled`: the "Paperclip archive"
parents and their children) by default. One switch shows both:

- web board: **Show archive & legacy** toggle (with **Group by company**);
- `GET /api/tasks?include_legacy=1` or `include_archive=1` (or `origin=legacy`);
- `staypoint task list --legacy` / `--archive` (add `--all` for done tasks);
- MCP `staypoint_task_list` with `include_legacy` / `include_archive`;
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

## Paperclip import (`staypoint import paperclip`)

A Board command that runs on the Board's terminal. It is refused when
`STAYPOINT_TASK_ID` is set, and the real import asks for confirmation (type
`import`) on a TTY.

- It reads every company, and for each one every issue, whatever its status. The issue list
  endpoint returns 500 rows when no `limit` is given, so the import pages with
  `limit=200&offset=N` until it gets a short page. Long descriptions come back
  truncated in the list (`descriptionTruncated`), so for open issues it
  fetches the full text from `GET /api/issues/{id}`. Archived issues keep the
  truncated text plus a note naming the Paperclip identifier, unless
  `--full-archive-descriptions` is passed (one slow request each).
- **Open issues** (`backlog`, `todo`, `in_progress`, `in_review`, `blocked`)
  are imported as described below.
- **Finished issues** (`done`, `cancelled`) go flat under a second per-company
  parent, `Paperclip archive — <Company>`, which is itself `done`. Each one is
  a `done` task (status `done`) or a `cancelled` task (status `soft_deleted`).
  Its Paperclip `completedAt` / `cancelledAt` becomes `updated_at` (and
  `deleted_at` for cancelled). They are never claimed or woken, and lists hide
  them by default (see Origins).
- It creates one parent per company, `Paperclip backlog — <Company>`, in
  organization `StayPoint` (STA), `Managed Solution` (MAN), `Research` (RES),
  `Maintenance` (PER) or `RuneLite` (RUN). An unknown company uses its own
  name.
- Each issue becomes a child task with title `[STA-772] <title>`, the
  Paperclip description and priority, stage `backlog`, no assignee, origin
  `paperclip_import`, `source_ref` set to the identifier and `source_id` to
  the uuid. The task page shows "Imported from STA-772". Assignees, agents,
  comments and status history are not imported.
- Paperclip parent→child links are kept when the parent is also open. Nesting
  deeper than `tasks.max_child_depth` (the company parent is depth 0) is
  flattened under the deepest ancestor that fits, and the description says
  which issue was its Paperclip parent. Children are inserted directly, so
  `tasks.max_children` does not limit the import; the global setting is not
  changed.
- `repo_path` comes from the Paperclip project's primary workspace `cwd`, or
  else from its codebase `localFolder`. Paperclip's managed folders are never
  used. If neither is set the repo stays empty. A task with no repo can't
  move to a runnable stage (409 / `ErrNoRepo`) until
  `staypoint task set-repo <id> <path>` or `PUT /api/tasks/{id}/repo` gives it
  one.
- Running it again is safe: `source_id` has a unique index (the archive
  parent uses `archive:<company id>`), and issues already imported are
  skipped. An issue imported while open stays where it is if it is finished
  later. A later issue is added under the existing parents.
- `--dry-run` opens the task database read-only and runs no migrations, then
  prints per-company counts and sample titles. `--company STA,MAN` limits the
  import to those companies.

Migration 35 (`task_source_ref`) adds `tasks.source_ref`, `tasks.source_id`
(with the unique index) and, on databases that lack it, `tasks.priority`.
