# Web UI style audit (task-a2401862)

Board, 2026-10-09: parts of the web UI rendered with the browser's default
look. This is the list of controls that did, what each was fixed with, and the
check that keeps new ones out.

## How the audit works

`tests/ui/specs/23-style-audit.spec.ts` drives headless Chromium against the
throwaway daemon (`scripts/ui-e2e.sh`). It visits:

- every sidebar view (`/`, `/projects`, `/agents`, `/kanban`, `/recent-tasks`,
  `/task-status`, `/all-tasks`, `/cost`, `/boss`, `/checklist`, `/gates`,
  `/pull-requests`, `/settings`, `/routines`, `/artifacts`, `/skills`,
  `/connectors`, `/audit`, `/logs`)
- the full task page of a parent task (one child, a pending ship review card, a
  task document), on each tab: Review, Diff, Migrations, Brief, Artifacts
- the same task in the drawer (`#detail-panel`), on each tab
- Settings with a dev environment config saved through the real Board passkey
  flow
- the New Task modal, the Fleet modal (all three tabs), and the content modal

On each, `tests/ui/style-audit.ts` checks every visible `button`, text-like
`input`, `select` and `textarea`. It compares the computed style with a control
of the same tag inside a shadow root, which page CSS cannot reach, so the
reference is whatever Chromium's defaults are. A control is flagged when:

- **background+border**: background colour, border style and border width all
  equal the default (the grey button / white field look), or
- **font**: font size and family both equal the default (13.33px system-ui).

Checkboxes, radios, range, file, colour and hidden inputs are skipped: they are
meant to look native.

Set `STAYPOINT_STYLE_AUDIT_DIR=dir` to also write a cropped screenshot and a
`findings.jsonl` line per finding.

## Findings

All were flagged for both background+border and font. Screenshots are from the
run before the fix, in `docs/ui-style-audit/`.

| # | Route | Element | Screenshot | Fix |
|---|-------|---------|------------|-----|
| 1 | `/settings` › Dev Environment Config | Saved configs list (`.settings-dev-config-row`): repo path, command and merge mode printed as one run-together line, no labels | `settings-dev-environment-config-6.png` (form below the list) | Each config is a `settings-row settings-row-clickable` row: repo path, then labelled *Dev command*, *Setup steps*, *Target branch* lines; merge mode (and *Work repo*) on the right, like the other Settings rows |
| 2 | `/settings` › Dev Environment Config | Repo path input `input.settings-dev-config-input` | `settings-dev-environment-config-0.png` | `.settings-dev-config-input` joins the `.form-field input` rule (surface2 background, border token, 6px radius, 13px app font) |
| 3 | `/settings` › Dev Environment Config | Dev command input | `settings-dev-environment-config-1.png` | as 2 |
| 4 | `/settings` › Dev Environment Config | Setup steps `textarea` | `settings-dev-environment-config-2.png` | as 2, plus `resize: vertical` |
| 5 | `/settings` › Dev Environment Config | Merge mode `select.settings-dev-config-merge-mode-select` | `settings-dev-environment-config-3.png` | as 2 |
| 6 | `/settings` › Dev Environment Config | Target branch input | `settings-dev-environment-config-4.png` | as 2 |
| 7 | `/settings` › Dev Environment Config | gh config dir input | `settings-dev-environment-config-5.png` | as 2 |
| 8 | `/settings` › Dev Environment Config | `button.settings-dev-config-save` "Save config" | `settings-dev-environment-config-6.png` | `btn btn-primary`; form gets padding and field labels styled like `.form-field label` |
| 9 | Task page and drawer, header actions | `button.run-children-btn` "▶ Run all children" | `task-page-review-tab-0.png` | `btn btn-secondary` |
| 10 | Task page and drawer, Review tab › Test coverage | `button.ship-review-merge-without-tests-btn` "Merge without tests" | `task-page-review-tab-1.png` | Shares the `.ship-review-reject-submit-btn` rule (red outline), the same style as the confirm button it opens |
| 11 | Task page and drawer, Review tab › Details | `select.task-kind-select` (Kind of work) | `task-page-review-tab-2.png` | joins the `.form-field select` rule |
| 12 | Task page and drawer, Review tab › Details | `select.task-provider-select` (Provider) | `task-page-review-tab-3.png` | joins the `.form-field select` rule |
| 13 | New Task modal | `textarea#ct-description` | `new-task-modal-0.png` | `.form-field textarea` added to the `.form-field input, .form-field select` rule |

No other route, tab or modal in the list above had a flagged control.

## Limits

- The audit sees the states the throwaway daemon can produce: no live
  Paperclip data (the stub answers `/api/fleet/*`), no running agent, no open
  pull requests. Controls that only appear in those states are not covered.
- The first run used a hard `expect`, so on the task page and drawer only the
  Review tab was measured before the fix. The spec now uses `expect.soft` and
  measures all five tabs; see the task thread for the post-fix run.

## The regression check

`specs/23-style-audit.spec.ts` runs with the rest of the UI suite and fails,
listing each control, when any audited route shows a browser-default button,
input, select or textarea.
