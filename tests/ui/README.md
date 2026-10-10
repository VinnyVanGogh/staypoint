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
| `04-run-now-timeline` | Run Now moves the task to `in_progress`; timeline shows steps recorded by the real `StepRecorder` |
| `05-interactions` | Accept, Reject, and answering a question by picking an option (STA-350) |
| `06-checklist` | Checklist items and the DoD commit-hash gate banner |
| `07-errors` | Unknown task id shows an error; unknown route is a 404; daemon going down shows an error |
| `15-task-page-layout` | Task page matches the STA-289 mock (STA-638): one screen at 1512×900, sticky header actions, stats strip, pinned composer, tabbed right panel, grouped timeline, modals for long content, 800px stacking. One test per step, each `knownBug('STA-638-<step>')` until that step lands. Saves `task-page-1512x900.png` to the artifacts dir for the side-by-side check. STA-679 test: a long agent summary on the Review tab is an overflow-hidden preview with a `::after` gradient fade, bounded height, and stays above the applied-migration banner, at 1512×900 and 800×900; saves `task-page-review-long-summary.png` (and `-800x900.png`) |
| `16-ship-review-live` | Ship review card on a `live_credentials` project (STA-727): exact red LIVE banner above Preview, Start dev server marked LIVE, inline confirm with the full warning before Start/Restart, Cancel sends nothing, confirm sends start-dev with the passkey headers and `confirm_live`; non-live Restart unchanged. Route-faked card; server gate covered by `handlers_ship_review_live_test.go` |
| `24-messages-thread` | Task page Messages thread (task-d13978fc): open, it spans stats row to composer over the timeline column only and the right panel tabs stay clickable; Board (`user`/`board`) comments render right (`chat-msg-user`, "You"), harness / agent-summary / other authors and daemon-written Board notices render left (`chat-msg-agent`, notices muted); 800px fits without overflow. Saves `task-page-messages-thread-1512x900.png` |
| `20-passkey-enroll` | Board passkey enrollment without a real action (STA-694): `boardSessionPage` (Board session, virtual authenticator, zero passkeys) shows the "No passkey enrolled" banner; enrolling from the banner or Settings → Security hides it and lists the passkey; Delete goes through the passkey gate and brings the banner back; enrolling mid-action asks "Continue with <action>?" and then performs it. A page without a Board session shows no banner. Both Enroll buttons surface STA-696's "Couldn't show the pairing code: …" and never prompt for a code; enrolling still works when passkeys were removed without the page seeing an SSE event |
| `23-task-stage` | "Move to…" stage control (task-40f0a2f0): the Board moves an agent-created backlog task to In review while the agent token gets 403 `board_session_required`; a refused move (no repo) shows the server message and keeps the stage; a page without a Board session is refused; Trust on a backlog task is "Move to todo and trust" under one Touch ID, with no dead-end alert |
| `23-many-tabs` | Eight tabs plus a ninth on one daemon (task-53fcbcff): every tab loads and shows `live`, exactly one tab holds the event stream, live updates reach every list tab, and closing the stream's tab hands it to another without a reload. Fails (the seventh tab never loads) if each tab opens its own EventSource |

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
