# Web UI end-to-end suite (STA-353)

Playwright tests that drive the real StayPoint web UI
(`internal/server/webui/`) in headless Chromium against a throwaway daemon.
This is the browser companion to the API suite in `tests/api` (STA-346).

## Run it

```bash
scripts/ui-e2e.sh                               # whole suite
scripts/ui-e2e.sh -g "interaction"              # tests whose title matches
scripts/ui-e2e.sh specs/03-comments.spec.ts     # one file
STAYPOINT_UI_STRICT=1 scripts/ui-e2e.sh         # ignore the known-bug list
E2E_KEEP_TMP=1 scripts/ui-e2e.sh                # keep temp DBs and daemon logs
```

Needs Go, Node 18+ and Python 3. The first run installs `@playwright/test`
into `tests/ui/node_modules` and the matching Chromium build.

## What the script does

1. Builds `cmd/staypoint-apitest-server` (the STA-346 harness) and
   `tests/ui/stepsim`.
2. Starts `tests/api/paperclip_stub.py` so `/api/fleet/*` never reaches the
   live Paperclip API.
3. Starts two daemons on ephemeral ports, each with a fresh SQLite file and
   `HOME` in a temp dir: the main one for every spec, and an expendable one
   that the "daemon goes down" spec kills.
4. Runs `playwright test` with the daemon URL, token and DB path in the
   environment, then stops everything and deletes the temp dir.

The real DB (`~/.staypoint/staypoint.db`), the launchd daemon on `:41421` and
the live Paperclip API are never touched.

## Rules for specs

- **Seed via API, assert via UI.** Use the `api` fixture (`fixtures.ts`) to
  create tasks, comments and interactions. Use the browser only for what a
  person does: navigate, click, type, read.
- The `page` fixture logs in the way a person does: it visits `/?token=…`,
  which the daemon swaps for a session cookie.
- Give fixtures unique names (`api.createTask(label)` does this). All specs
  share one daemon.
- Open task pages with `gotoTaskPage()`; it waits for the first render, which
  can take several seconds on a loaded machine.

## Coverage

| Spec | Flow |
| --- | --- |
| `01-task-list` | List loads with a seeded task; live update over SSE; New Task form |
| `02-task-page` | Full page `/tasks/:org/:project/:id` renders; deep link survives reload; list row opens the task |
| `03-comments` | Comment typed in the UI shows in the thread, is stored, survives reload |
| `04-run-now-timeline` | Run Now moves the task to `in_progress` (the UI's only status control); timeline shows steps recorded by the real `StepRecorder` |
| `05-interactions` | Accept, Reject, and answering a question by picking an option (STA-350) |
| `06-checklist` | Checklist items and the DoD commit-hash gate banner |
| `07-errors` | Unknown task id shows an error; unknown route is a 404; daemon going down shows an error |
| `15-task-page-layout` | Task page matches the STA-289 mock (STA-638): one screen at 1512×900, sticky header actions, stats strip, pinned composer, tabbed right panel, grouped timeline, modals for long content, 800px stacking. One test per step, each `knownBug('STA-638-<step>')` until that step lands. Saves `task-page-1512x900.png` to the artifacts dir for the side-by-side check. STA-679 test: a long agent summary on the Review tab is an overflow-hidden preview with a `::after` gradient fade, bounded height, and stays above the applied-migration banner, at 1512×900 and 800×900; saves `task-page-review-long-summary.png` (and `-800x900.png`) |

The throwaway daemon has no agent runner, so Run Now cannot start a real run.
`stepsim` stands in for one: it drives `orchestrator.StepRecorder` against the
throwaway DB the way the harness does, and the spec checks the timeline shows
what was recorded.

## Known bugs

`KNOWN_BUGS` in `fixtures.ts` lists product bugs the suite has found but that
are not fixed in this branch. A spec that hits one calls `knownBug(id)`, which
marks it as expected to fail: the suite stays green while the bug is open.
When the bug is fixed the spec passes, Playwright reports an unexpected pass,
and the suite goes red. That is the cue to delete the `knownBug()` call and
the list entry.

Run with `STAYPOINT_UI_STRICT=1` to ignore the list. The final pre-cutover run
(STA-318) should be strict.

## When a spec fails

Each failing test leaves a screenshot, a Playwright trace and an
`error-context.md` in `tests/ui/artifacts/test-results/<test>/` (override with
`E2E_ARTIFACTS=dir`). There is also an HTML report in
`tests/ui/artifacts/html-report`.

```bash
npx --prefix tests/ui playwright show-trace tests/ui/artifacts/test-results/<test>/trace.zip
npx --prefix tests/ui playwright show-report tests/ui/artifacts/html-report
```
