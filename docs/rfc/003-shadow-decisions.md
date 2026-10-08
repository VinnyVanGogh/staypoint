# RFC 003: Shadow decisions (STA-433)

Status: plan, 2026-10-07. Parent task: STA-433 (`task-e9d9abbd`).

The local decision model runs next to every routine decision StayPoint makes, logs its pick
next to the real one, and never acts. The Board reads the agreement numbers on a Decisions
page and decides later whether the model may decide anything for real.

## Which model

Two Board instructions conflict:

- STA-433 comment, 2026-10-06: use Together AI (`decision.NewTogether`).
- Board rule, 2026-10-07, recorded in code: decision traffic stays on this machine and is never
  billed. See `decision.NewLocal` (`internal/decision/together.go:123`) and the gate advisor
  wiring in `cmd/staypointd/main.go:255-258`.

The later rule wins. Shadow mode uses `decision.NewLocal()` (Ollama `/v1/systemone`, model
`tev1-4b`). It does not use `NewTogether`, even when `TOGETHER_API_KEY` is set. If Ollama is not
running, every shadow row records an error, and the page shows that error. That is the
expected result, not a blocker. If the Board wants Together after all, the change is one line
in the wiring subtask.

## What exists today (code map)

| Piece | Where | State |
|---|---|---|
| Decision client | `internal/decision/together.go` (`New`, `NewTogether`, `NewLocal`, `Decide`, `DecideWithReason`) | Done (STA-309, PR #243). |
| Shadow voter | `internal/governance/advisory.go` (`SetAdvisoryClient`, `shadowVote`) | Never wired: `SetAdvisoryClient` has no callers. It logs to slog only and writes no DB row. |
| Shadow call site | `internal/governance/state_machine.go:170` (`ExecuteTransition`, after commit) | Fires on every stage transition. |
| Decision log table | `internal/db/db.go:1312`, migration 39 `decision_log` (`subject_kind`, `subject_id`, `advisor`, `model`, `recommendation`, `reason`, `latency_ms`, `error`, `final_decision`, `decided_by`, `decided_at`) | Exists. Only security gates write to it (`gates.LogAdvice`, `internal/gates/advisor.go:47`). |
| Gate advisor (STA-868) | `gates.RunAdvisory` (`internal/gates/advisor.go:234`) with `TogetherAdvisor{Client: decision.NewLocal()}` | Already shadow mode for security gates. Use it as the pattern. |
| Completion check | `Interceptor.InterceptCompletion` (`internal/orchestrator/interceptor.go:82`), called at `internal/orchestrator/harness.go:809` and `:961` | No log. |
| Routing by kind of work | `router.ResolveRouteChoice` (`internal/router/route.go:447`), called from `cmd/staypointd/route.go:76` | No log. |
| Interaction dispositions | `context.ResolveTaskInteraction` / `ResolveInteraction` (`internal/context/interactions.go:289-303`), from `internal/server/handlers_tasks.go:739` and `cmd/staypoint/task_interactions_cmd.go` | No log. |
| Run logger (STA-434) | `internal/logging` (`WithComponent`, `WithRunContext`) | Landed. Shadow code logs through it. No second logger. |
| Web UI | `internal/server/webui/` (`index.html`, `app.js`, `gates.js`, `lib/gates.js`) | The Gates view already shows advisor rows. There is no Decisions page. |

## Decision types logged

| `subject_kind` | Question | Options | Current decider | Final decision recorded |
|---|---|---|---|---|
| `security_gate` | (existing) approve this Red-tier command? | approve / deny | Board or rule | Already done. |
| `stage_transition` | Is moving task from X to Y the right next step? | proceed / hold | Caller of `ExecuteTransition` | `proceed` (the move happened). |
| `completion_check` | Can this task be marked complete given these check results? | approve / reject | `InterceptCompletion` | `approve` or `reject`, plus failed checks as the reason. |
| `route_kind` | Which agent should run this task (kind, repo type)? | candidate slots in chain order | `ResolveRouteChoice` | the chosen slot key, or `all_locked`. |
| `interaction` | How should this pending interaction be resolved? | accepted / rejected / … (the valid statuses) | Board or agent resolving it | the status given. |

`subject_id` is the task id (or interaction id or gate id). One "decision" is a group of rows
with the same `subject_kind` and `subject_id` and a new `decision_key` (see the schema subtask):
one row for the current decider (`advisor = 'current'`) and one for each shadow advisor
(`advisor = 'local'`).

## Design

1. **Schema (migration 40).** Add `decision_key TEXT`, `task_id TEXT`, `question TEXT`,
   `options_json TEXT` and `pick TEXT` to `decision_log`, plus an index on
   `(subject_kind, created_at)`. Existing gate rows keep working: the new columns default to `''`.
2. **`internal/decision/shadow` package.** `Recorder.Record(ctx, Decision)`:
   - It writes the current row synchronously, in a cheap insert. Errors are logged and ignored.
   - It then starts a goroutine that calls `client.Decide` with a hard timeout (default 10s)
     and writes the local row: the pick, latency, and an error or timeout.
   - It returns before the model answers. The model's answer is never returned to the
     caller, so there is no code path from the answer back to state.
   - It does nothing when shadow mode is off (the kill switch) or when the client is nil.
   - Concurrency is bounded (semaphore, default 4). When it is full, the local row records
     `error = "skipped: busy"` instead of queueing without limit.
3. **Wiring.** `governance.SetAdvisoryClient` is replaced by a recorder installed from
   `cmd/staypointd/main.go`. The state machine, interceptor, router call site and interaction
   resolver each call `Record` after their own decision is final.
4. **Kill switch.** `shadow_decisions` in `config.Config` (TOML or JSON, default `true`), with
   env override `STAYPOINT_SHADOW_DECISIONS=0`. It is read at startup. When off, there are no
   rows from the local advisor and no model calls. The current rows are still written, so the
   log keeps working.
5. **API.** `GET /api/decisions?type=&limit=` returns the decisions, each with its current pick,
   local pick, latency, error and agree flag. `GET /api/decisions/stats` returns the agreement
   rate per type, counting errors separately: an error is not counted as a disagreement.
6. **Decisions page.** A new nav entry in `webui/index.html` and a `decisions.js` (pure helpers
   in `lib/decisions.js` so they can be unit tested, the same as `lib/gates.js`). The page has
   a table: time, task, type, question, current pick, local pick, agree mark (✓, ✗, or
   "error"), and latency. An error shows its error text in red, never a blank cell. A strip at
   the top shows the agreement rate per type.

## Assumptions

| Assumption | How it was checked |
|---|---|
| `SetAdvisoryClient` has no callers | `grep -rn SetAdvisoryClient` lists only the definition. Verified. |
| `decision_log` already exists and can be extended | Migration 39 in `internal/db/db.go:1272`. Verified. |
| Ollama with `tev1-4b` is installed on this Mac | Not verified in this planning run. The setup subtask checks it and writes it down in `docs/`. |
| STA-434 logger is `internal/logging` | `WithRunContext` exists. That this is "the STA-434 logger" is **unverified**: confirm on STA-434 before the wiring subtask. |
| `InterceptCompletion` runs once per completion attempt | Two call sites (`harness.go:809`, `:961`). One row is logged per call. Verified by reading the code. |

## Risks and threat model

- **The shadow answer changes state.** Mitigation: `Record` returns nothing, and the model
  call runs in a goroutine after the real decision is final. Test: a fake client that panics
  or returns "hold"/"reject" leaves the task stage, interception result and route unchanged.
- **The shadow call delays the real decision.** Mitigation: async, with a timeout and a
  semaphore. Test: a fake client that sleeps 30s must not slow `ExecuteTransition`,
  `InterceptCompletion` or `ResolveRouteChoice` by more than 50ms.
- **Prompt content leaks.** Questions contain task names and stage names only, never file
  contents or secrets. The model is local only (`NewLocal` refuses non-loopback URLs).
- **DB growth.** Each decision writes about 2 rows. A retention sweep (keep 30 days) is a
  follow-up, filed and not built here.
- **Goroutine leak.** The semaphore plus the timeout context limit it. A panic in the client is
  recovered inside the goroutine.

## Test plan (adversarial first)

1. A fake client that returns the opposite pick: state is unchanged, and the row records a
   disagreement.
2. A fake client that blocks past the timeout: the caller returns in <50ms, and the row says
   `error=timeout`.
3. A fake client that panics: the daemon survives, and the row records the panic as an error.
4. Kill switch off: zero local rows and zero client calls, and current rows are still written.
5. Semaphore full: the row says `skipped: busy`, and nothing is queued.
6. Migration 40 on a DB that already has gate rows: the old rows still read on the Gates page.
7. API stats: errors are excluded from the agreement-rate denominator and counted separately.
8. JS helpers: an error row renders "error", never an empty string.
9. End to end: a real task run on the dev daemon writes rows of at least 3 types, and the
   Decisions page shows them (screenshot plus the daemon log lines).

## Subtasks (one topic each, test-first before implementation)

1. Schema: migration 40 extends `decision_log` (tests first, then the migration).
2. `decision/shadow` recorder: async, with timeout, semaphore, kill switch and panic recovery
   (adversarial tests 1-5 first).
3. Wire the recorder into the state machine (replacing `SetAdvisoryClient`/`shadowVote`), the
   completion interceptor, route resolution and interaction resolve. Config kill switch in
   `cmd/staypointd`.
4. Decisions API: list and stats endpoints, with tests.
5. Decisions web UI page: `lib/decisions.js` tests, then the page.
6. Local runtime setup doc plus end-to-end verification on the dev daemon, done by a fresh
   agent: Ollama `tev1-4b` install steps in `docs/`, a screenshot, and log lines.

## Out of scope

- Letting the model decide anything for real.
- Fine-tuning.
- Using the Together cloud API for shadow mode (Board rule 2026-10-07).
