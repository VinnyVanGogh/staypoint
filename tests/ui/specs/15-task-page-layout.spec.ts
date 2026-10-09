/**
 * spec 15: task page layout matches the STA-289 mock (STA-638)
 *
 * Written before the re-layout (STA-639). One test per step subtask, each
 * marked knownBug('STA-638-<step>') so the suite stays green until that step
 * lands. The step's PR deletes its own knownBug() call and KNOWN_BUGS entry and
 * must not edit or weaken the assertions; if one looks wrong, comment on
 * STA-638 and send it back to the test author.
 *
 *   step 2 (STA-640)  one screen at 1512x900, sticky header + primary actions,
 *                     stats strip, pinned composer
 *   step 3 (STA-641)  tabbed right panel: Review / Diff / Migrations / Brief
 *   step 4 (STA-642)  timeline grouped run -> subtask -> step, scrolls inside
 *                     its column, rows expand in place
 *   step 5 (STA-643)  long content opens in a role=dialog modal
 *   step 6 (STA-644)  800x900: stacked, segmented tabs, no horizontal overflow
 *
 * DOM contract the steps implement (names from STA-638 or chosen here):
 *   .task-page-header      title (.task-page-title), status pill (.pill),
 *                          identifier, primary action buttons
 *   .task-page-stats       elapsed, current step, files read/edited, lines +/-,
 *                          commands ok/failed, tokens, cost
 *   .task-page-composer    message box pinned to the bottom of the viewport
 *   .task-page-timeline    left column, scrolls internally
 *     .timeline-run[data-run-id]                        one per run
 *       .timeline-subtask[data-subtask-step-id=<id>]    a step that other steps
 *                                                       point at via parent_seq;
 *                                                       holds its own row and its
 *                                                       children's rows
 *         .timeline-row[data-step-id]                   existing buildRunStepRow
 *   .task-page-panel       right column with [role=tablist] / [role=tab] /
 *                          [role=tabpanel]
 *   [data-modal-trigger="brief" | "dev-log" | "agent-summary" | "migration-sql"]
 *                          button that opens the full content in [role=dialog]
 *
 * The heavy task is a real task (long description, comments) with synthetic
 * run steps, ship review card, migration and diff injected via page.route(),
 * the way 13-ship-review-overflow does, so no git repo or agent run is needed.
 * The first test checks the seed renders on any layout and saves
 * task-page-1512x900.png into the artifacts dir for the side-by-side check
 * against sta-289-mock.png.
 */

import type { Locator, Page } from '@playwright/test';
import {
  test, expect, gotoTaskPage, openTaskPanelTab, saveArtifactScreenshot,
  type StayPointAPI, type Task,
} from '../fixtures';

const WIDE = { width: 1512, height: 900 };
const NARROW = { width: 800, height: 900 };

// Markers sit at the very end of each long block, so seeing one proves the
// full content is shown, not a truncated preview.
const BRIEF_END = 'BRIEF-END-MARKER';
const DEVLOG_END = 'DEVLOG-END-MARKER';
const SUMMARY_END = 'SUMMARY-END-MARKER';
const SQL_END = 'SQL-END-MARKER';
const EXPAND_MARKER = 'EXPAND-MARKER-TestRetryAfter429';
const THINK_MARKER = 'THINK-MARKER-bucket-refill-math';

// An inline preview taller than half the viewport is not a preview.
const MAX_INLINE_PX = WIDE.height / 2;

type Step = {
  id: string; run_id: string; task_id: string; seq: number; parent_seq?: number;
  kind: string; title: string; body?: string; command?: string; status: string;
  started_at: string; ended_at: string; created_at: string;
};

type Seed = { task: Task; ident: string; steps: Step[]; runIds: string[] };

function longBrief(): string {
  const lines = ['## Goal', '', 'Add rate-limit headers to the events API.', ''];
  for (let i = 1; i <= 60; i++) {
    lines.push(`${i}. Requirement ${i}: the events endpoint must keep its existing contract while the limiter is in place, and this line is long on purpose.`);
  }
  lines.push('', BRIEF_END);
  return lines.join('\n');
}

function buildSteps(taskId: string, tag: string): { steps: Step[]; runIds: string[] } {
  const runIds = [`run-a-${tag}`, `run-b-${tag}`];
  const steps: Step[] = [];
  const add = (runIdx: number, startMs: number, seq: number, kind: string, title: string,
    extra: Partial<Step> = {}) => {
    const t0 = new Date(startMs + seq * 20_000);
    const t1 = new Date(t0.getTime() + 15_000);
    steps.push({
      id: `${runIds[runIdx]}-s${seq}`, run_id: runIds[runIdx], task_id: taskId, seq, kind, title,
      status: 'done', started_at: t0.toISOString(), ended_at: t1.toISOString(), created_at: t0.toISOString(),
      ...extra,
    });
  };

  // Run 1: 18 steps, two subtasks.
  const a = Date.now() - 3 * 3600_000;
  add(0, a, 1, 'wake', 'Woke up: assigned by Board');
  add(0, a, 2, 'route', 'Routed to claude · sonnet');
  add(0, a, 3, 'read', 'Read context', { body: '6 files · issue + 2 comments' });
  add(0, a, 4, 'think', 'Plan', { body: 'Token bucket per client, then headers.' });
  add(0, a, 5, 'message', 'Subtask 1 · Token-bucket limiter');
  add(0, a, 6, 'think', 'Thinking', { parent_seq: 5, body: THINK_MARKER });
  add(0, a, 7, 'edit', 'Edit internal/server/middleware.go', { parent_seq: 5 });
  add(0, a, 8, 'edit', 'Edit internal/server/server.go', { parent_seq: 5 });
  add(0, a, 9, 'run', 'go test ./internal/server/...', { parent_seq: 5, status: 'error', command: 'go test ./internal/server/...', body: '--- FAIL: TestRateLimit' });
  add(0, a, 10, 'think', 'Thinking', { parent_seq: 5, body: 'Burst was off by one.' });
  add(0, a, 11, 'edit', 'Edit internal/server/middleware.go', { parent_seq: 5 });
  add(0, a, 12, 'run', 'go test ./internal/server/...', { parent_seq: 5, command: 'go test ./internal/server/...', body: 'ok' });
  add(0, a, 13, 'checkpoint', 'Checkpoint a41c9e2', { parent_seq: 5 });
  add(0, a, 14, 'message', 'Subtask 2 · Headers + 429 response');
  add(0, a, 15, 'edit', 'Edit internal/server/events.go', { parent_seq: 14 });
  add(0, a, 16, 'run', 'go vet ./...', { parent_seq: 14, command: 'go vet ./...', body: '' });
  add(0, a, 17, 'run', 'go test ./...', { parent_seq: 14, command: 'go test ./...', body: 'ok' });
  add(0, a, 18, 'state', 'Finished: in_review');

  // Run 2 (after a send-back): 22 steps, two subtasks.
  const b = Date.now() - 3600_000;
  add(1, b, 1, 'wake', 'Woke up: sent back by Board');
  add(1, b, 2, 'route', 'Routed to claude · sonnet');
  add(1, b, 3, 'read', 'Read Board feedback', { body: 'Add Retry-After on 429.' });
  add(1, b, 4, 'think', 'Plan the fix', { body: 'Header on the 429 path only.' });
  add(1, b, 5, 'message', 'Subtask 1 · Retry-After header');
  add(1, b, 6, 'think', 'Thinking', { parent_seq: 5, body: 'Reset time from the bucket.' });
  add(1, b, 7, 'edit', 'Edit internal/server/middleware.go', { parent_seq: 5 });
  add(1, b, 8, 'edit', 'Edit internal/server/middleware_test.go', { parent_seq: 5 });
  add(1, b, 9, 'run', 'Run the 429 tests', {
    parent_seq: 5, command: 'go test -run TestRetryAfter ./internal/server/',
    body: `=== RUN   TestRetryAfter429\n--- PASS: TestRetryAfter429 (0.01s)\n${EXPAND_MARKER}`,
  });
  add(1, b, 10, 'read', 'Read internal/server/events.go', { parent_seq: 5 });
  add(1, b, 11, 'edit', 'Edit internal/server/events.go', { parent_seq: 5 });
  add(1, b, 12, 'run', 'go test ./...', { parent_seq: 5, command: 'go test ./...', body: 'ok' });
  add(1, b, 13, 'message', 'Subtask 2 · Docs + changelog');
  add(1, b, 14, 'read', 'Read docs/api.md', { parent_seq: 13 });
  add(1, b, 15, 'edit', 'Edit docs/api.md', { parent_seq: 13 });
  add(1, b, 16, 'edit', 'Edit CHANGELOG.md', { parent_seq: 13 });
  add(1, b, 17, 'think', 'Thinking', { parent_seq: 13, body: 'Mention the new headers.' });
  add(1, b, 18, 'run', 'make lint', { parent_seq: 13, command: 'make lint', body: 'ok' });
  add(1, b, 19, 'run', 'make test', { parent_seq: 13, command: 'make test', body: 'ok' });
  add(1, b, 20, 'checkpoint', 'Checkpoint 7be01d4', { parent_seq: 13 });
  add(1, b, 21, 'message', 'Ship review posted');
  add(1, b, 22, 'state', 'Finished: in_review');

  return { steps, runIds };
}

function shipCard(taskId: string) {
  const devLog: string[] = [];
  for (let i = 1; i <= 120; i++) {
    devLog.push(`[dev ${String(i).padStart(3, '0')}] compiled internal/server in 0.${i % 10}s, watching for changes on 127.0.0.1:3333 with a long log line`);
  }
  devLog.push(DEVLOG_END);
  const summary: string[] = ['## What changed', ''];
  for (let i = 1; i <= 50; i++) {
    summary.push(`- Change ${i}: the limiter now returns the right headers on every path, described at length for this test.`);
  }
  summary.push('', SUMMARY_END);
  const now = new Date().toISOString();
  return {
    id: `layout-card-${taskId}`,
    task_id: taskId,
    branch: `staypoint/${taskId}`,
    head_sha: 'a41c9e2b7be01d4c0ffee1234567890abcdef12',
    test_steps: [
      'Open the preview and call GET /api/events 6 times in a second.',
      'The 6th call returns 429 with Retry-After.',
      'Every 2xx response carries X-RateLimit-Remaining.',
    ],
    dev_url: 'http://127.0.0.1:3333/',
    dev_pid: 0,
    dev_state: 'ready',
    dev_log: devLog,
    status: 'pending',
    files_changed: ['internal/server/middleware.go', 'internal/server/server.go', 'internal/server/events.go'],
    check_runs: [{ command: 'go test ./...', exit_code: 0, output_tail: 'ok' }],
    agent_summary: summary.join('\n'),
    has_db_migration: true,
    run_number: 2,
    created_at: now,
    updated_at: now,
  };
}

function migrations() {
  const sql: string[] = ['-- rate limit buckets'];
  for (let i = 1; i <= 40; i++) {
    sql.push(`CREATE TABLE IF NOT EXISTS rate_bucket_${i} (client_id TEXT PRIMARY KEY, tokens INTEGER NOT NULL DEFAULT 5);`);
  }
  sql.push(`-- ${SQL_END}`);
  return {
    migrations: [{
      path: 'supabase/migrations/20261004120000_rate_limits.sql',
      sql: sql.join('\n'),
      risk_statements: [],
      additive_only: true,
      verification_checks: [],
      verification_query: 'select 1;',
    }],
    sql_editor_url: '',
    has_auto_verify: false,
  };
}

const DIFF = {
  diff: ' 3 files changed, 43 insertions(+), 6 deletions(-)',
  files: ['internal/server/middleware.go', 'internal/server/server.go', 'internal/server/events.go'],
  file_stats: [
    { path: 'internal/server/middleware.go', added: 22, removed: 3 },
    { path: 'internal/server/server.go', added: 6, removed: 2 },
    { path: 'internal/server/events.go', added: 15, removed: 1 },
  ],
  checkpoint_id: '',
};

const json = (body: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

/** Real task + comments via the API; run steps, review, migration and diff via page.route(). */
async function seedHeavyTask(page: Page, api: StayPointAPI): Promise<Seed> {
  const task = await api.createTask('Layout heavy', { description: longBrief() });
  await api.setStage(task.id, 'in_review');
  for (const c of ['Board: please add Retry-After.', 'Agent: on it.', 'Board: looks close.']) {
    await api.addComment(task.id, c);
  }
  const real = await api.getTask(task.id) as Task & { identifier?: string };
  const ident = real.identifier || `#${task.id.slice(0, 8)}`;
  const { steps, runIds } = buildSteps(task.id, task.id.slice(0, 8));
  const base = `**/api/tasks/${encodeURIComponent(task.id)}`;

  // Tokens and cost come from the task row; patch them into the real response.
  await page.route(base, async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    const res = await route.fetch();
    const body = await res.json();
    const t = body.task || body;
    t.spent_usd = 0.38;
    t.spent_tokens = 61234;
    await route.fulfill({ response: res, json: body });
  });
  await page.route(`${base}/run-steps`, (r) => r.fulfill(json({ steps })));
  await page.route(`${base}/ship-review`, (r) =>
    r.request().method() === 'GET' ? r.fulfill(json(shipCard(task.id))) : r.continue());
  await page.route(`${base}/migrations`, (r) =>
    r.request().method() === 'GET' ? r.fulfill(json(migrations())) : r.continue());
  await page.route(`${base}/diff`, (r) => r.fulfill(json(DIFF)));
  await page.route(`${base}/diff?*`, (r) => r.fulfill(json(DIFF)));

  return { task, ident, steps, runIds };
}

async function openHeavyTask(page: Page, api: StayPointAPI, viewport = WIDE): Promise<Seed> {
  await page.setViewportSize(viewport);
  const seed = await seedHeavyTask(page, api);
  await gotoTaskPage(page, seed.task);
  // Wait for the async parts (migrations panel) so later asserts see the full page.
  await expect(page.locator('.migration-file-card')).toHaveCount(1, { timeout: 12_000 });
  return seed;
}

/** Fails with the offending rect if the locator is not fully inside the viewport. */
async function expectInViewport(page: Page, loc: Locator, what: string) {
  await expect(loc, `${what} is not visible`).toBeVisible();
  const box = await loc.boundingBox();
  const vp = page.viewportSize()!;
  expect(box, `${what} has no box`).not.toBeNull();
  const b = box!;
  const inside = b.x >= -1 && b.y >= -1 && b.x + b.width <= vp.width + 1 && b.y + b.height <= vp.height + 1;
  expect(inside, `${what} at x=${Math.round(b.x)} y=${Math.round(b.y)} w=${Math.round(b.width)} h=${Math.round(b.height)} is outside ${vp.width}x${vp.height}`).toBe(true);
}

async function selectTab(page: Page, name: RegExp): Promise<Locator> {
  const panel = page.locator('.task-page-panel');
  const tab = panel.getByRole('tab', { name });
  // Assert first so a missing tab fails in expect's timeout, not the test's.
  await expect(tab, `no ${name} tab in .task-page-panel`).toBeVisible();
  await tab.click();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
  const panelId = await tab.getAttribute('aria-controls');
  const tabpanel = panelId ? page.locator(`#${panelId}`) : panel.getByRole('tabpanel');
  await expect(tabpanel).toBeVisible();
  return tabpanel;
}

const TABS = [/^\W*Review\b/, /^\W*Diff\b/, /^\W*Migrations\b/, /^\W*Brief\b/, /^\W*Artifacts\b/];
// Every tab in order: Decisions (task-e3fe2c0b) comes first, then the content
// tabs above.
const ALL_TABS = [/^\W*Decisions\b/, ...TABS];

test('seeded heavy task renders and is captured at 1512x900', async ({ page, api }) => {
  test.setTimeout(60_000);
  const { steps } = await openHeavyTask(page, api);
  // Selectors here are the existing renderers' own classes, which the
  // re-layout reuses, so this holds before and after STA-638.
  await expect(page.locator('#task-page-content .timeline-row')).toHaveCount(steps.length);
  await expect(page.locator('.ship-review-card')).toHaveCount(1);
  await expect(page.locator('.diff-file-row')).toHaveCount(DIFF.file_stats.length);
  await page.waitForTimeout(500); // let late async renders settle before the capture
  await saveArtifactScreenshot(page, 'task-page-1512x900.png');
});

test('step 2: one screen at 1512x900 with sticky header actions, stats strip and pinned composer', async ({ page, api }) => {
  test.setTimeout(90_000);
  const { task, ident } = await openHeavyTask(page, api);

  // Nothing at page level scrolls: the timeline and panel scroll internally.
  const overflow = await page.evaluate(() => {
    const over = (el: Element | null) => (el ? el.scrollHeight - el.clientHeight : -1);
    return {
      scrollingElement: over(document.scrollingElement),
      mainContent: over(document.getElementById('main-content')),
      taskPageContent: over(document.getElementById('task-page-content')),
    };
  });
  expect(overflow.scrollingElement, 'document.scrollingElement scrolls').toBeLessThanOrEqual(1);
  expect(overflow.mainContent, '#main-content scrolls (or is missing: -1 means missing)').toBeLessThanOrEqual(1);
  expect(overflow.mainContent).toBeGreaterThanOrEqual(0);
  expect(overflow.taskPageContent, '#task-page-content scrolls (or is missing: -1 means missing)').toBeLessThanOrEqual(1);
  expect(overflow.taskPageContent).toBeGreaterThanOrEqual(0);

  // Header: title, status pill, identifier, review actions, all on screen.
  const header = page.locator('#task-page-content .task-page-header');
  await expectInViewport(page, header, '.task-page-header');
  const hb = (await header.boundingBox())!;
  expect(hb.y, 'header is not at the top of the viewport').toBeLessThan(WIDE.height / 4);
  await expect(header.locator('.task-page-title')).toHaveText(task.name);
  await expect(header.locator('.pill').first()).toBeVisible();
  await expect(header).toContainText(ident);
  for (const name of [/Approve & Merge/, /Send Back/i, /^\W*Reject\b/]) {
    await expectInViewport(page, header.getByRole('button', { name }), `header button ${name}`);
  }

  // Stats strip: every stat from the mock, with the seeded values.
  const stats = page.locator('#task-page-content .task-page-stats');
  await expectInViewport(page, stats, '.task-page-stats');
  for (const label of [/elapsed/i, /step/i, /files read/i, /files edited/i, /lines/i, /c(om)?m(an)?ds/i, /tokens/i, /cost/i]) {
    await expect(stats, `stats strip has no ${label} stat`).toContainText(label);
  }
  await expect(stats).toContainText('+43');
  await expect(stats).toContainText(/[-−]6\b/);
  await expect(stats).toContainText('✓');
  await expect(stats).toContainText(/[✗×]/);
  await expect(stats).toContainText(/61(\.\d)?k/i);
  await expect(stats).toContainText('$0.38');

  // Composer pinned to the bottom edge, with the step-boundary hint.
  const composer = page.locator('#task-page-content .task-page-composer');
  await expectInViewport(page, composer, '.task-page-composer');
  const cb = (await composer.boundingBox())!;
  expect(WIDE.height - (cb.y + cb.height), 'composer is not pinned to the bottom').toBeLessThanOrEqual(48);
  await expect(composer.locator('textarea')).toBeVisible();
  const hint = await composer.evaluate((el) =>
    `${el.textContent || ''} ${(el.querySelector('textarea') as HTMLTextAreaElement | null)?.placeholder || ''}`);
  expect(hint).toMatch(/Delivered at the next step boundary/i);

  // While running, the header holds Pause / Stop instead.
  const running = await api.createTask('Layout running');
  await api.setStage(running.id, 'in_progress');
  await gotoTaskPage(page, running);
  const runHeader = page.locator('#task-page-content .task-page-header');
  await expectInViewport(page, runHeader.getByRole('button', { name: /Pause after (this )?step/i }), 'header Pause button');
  await expectInViewport(page, runHeader.getByRole('button', { name: /Stop( now)?\b/i }), 'header Stop button');
});

test('step 3: right panel tabs Decisions / Review / Diff / Migrations / Brief / Artifacts swap the existing content', async ({ page, api }) => {
  test.setTimeout(60_000);
  await openHeavyTask(page, api);

  const panel = page.locator('#task-page-content .task-page-panel');
  await expectInViewport(page, panel.getByRole('tablist'), '.task-page-panel tablist');
  const tabs = panel.getByRole('tab');
  await expect(tabs).toHaveCount(ALL_TABS.length);
  for (let i = 0; i < ALL_TABS.length; i++) {
    await expect(tabs.nth(i)).toHaveAccessibleName(ALL_TABS[i]);
  }

  // Each tab shows its existing renderer's content and hides the others.
  const content: Array<[RegExp, string, string]> = [
    [TABS[0], '.ship-review-card', 'What to test'],
    [TABS[1], '.diff-file-row', 'internal/server/middleware.go'],
    [TABS[2], '.migration-file-card', '20261004120000_rate_limits.sql'],
    [TABS[3], '.desc-view', 'Add rate-limit headers to the events API.'],
    [TABS[4], '.task-artifacts', 'No documents'],
  ];
  for (const [name, selector, text] of content) {
    const tabpanel = await selectTab(page, name);
    await expect(tabpanel.locator(selector).first(), `${name} tab does not show ${selector}`).toBeVisible();
    await expect(tabpanel).toContainText(text);
    for (const [, other] of content) {
      if (other === selector) continue;
      await expect(page.locator(other).first(), `${other} still visible on the ${name} tab`).toBeHidden();
    }
  }
});

test('step 4: timeline scrolls in its column, grouped run -> subtask -> step, rows expand in place', async ({ page, api }) => {
  test.setTimeout(60_000);
  const { steps, runIds } = await openHeavyTask(page, api);

  const timeline = page.locator('#task-page-content .task-page-timeline');
  await expectInViewport(page, timeline, '.task-page-timeline');

  // Scrolls internally, not the page.
  const scroll = await timeline.evaluate((el) => {
    const oy = getComputedStyle(el).overflowY;
    const before = el.scrollHeight - el.clientHeight;
    el.scrollTop = el.scrollHeight;
    const moved = el.scrollTop;
    el.scrollTop = 0;
    return { oy, before, moved, page: document.scrollingElement ? document.scrollingElement.scrollTop : 0 };
  });
  expect(['auto', 'scroll'], 'timeline overflow-y').toContain(scroll.oy);
  expect(scroll.before, '40 steps do not overflow the timeline').toBeGreaterThan(10);
  expect(scroll.moved, 'timeline did not scroll').toBeGreaterThan(0);
  expect(scroll.page, 'page scrolled with the timeline').toBe(0);

  // Grouping: run wrappers in order, subtask wrappers hold their parent's row
  // and every child row; top-level steps sit outside any subtask.
  await expect(timeline.locator('.timeline-run')).toHaveCount(runIds.length);
  for (let i = 0; i < runIds.length; i++) {
    await expect(timeline.locator('.timeline-run').nth(i)).toHaveAttribute('data-run-id', runIds[i]);
  }
  const expected = steps.map((s) => {
    const parent = s.parent_seq ? steps.find((p) => p.run_id === s.run_id && p.seq === s.parent_seq)! : null;
    const isSubtask = steps.some((c) => c.run_id === s.run_id && c.parent_seq === s.seq);
    return { id: s.id, run: s.run_id, subtask: parent ? parent.id : (isSubtask ? s.id : null) };
  });
  const problems = await timeline.evaluate((root, exp) => {
    const out: string[] = [];
    for (const e of exp) {
      const rows = root.querySelectorAll(`.timeline-row[data-step-id="${CSS.escape(e.id)}"]`);
      if (rows.length !== 1) { out.push(`${e.id}: ${rows.length} rows`); continue; }
      const row = rows[0] as HTMLElement;
      const run = (row.closest('.timeline-run') as HTMLElement | null)?.dataset.runId ?? null;
      const sub = (row.closest('.timeline-subtask') as HTMLElement | null)?.dataset.subtaskStepId ?? null;
      if (run !== e.run) out.push(`${e.id}: in run ${run}, want ${e.run}`);
      if (sub !== e.subtask) out.push(`${e.id}: in subtask ${sub}, want ${e.subtask}`);
    }
    return out;
  }, expected);
  expect(problems, problems.slice(0, 5).join('; ')).toEqual([]);

  // Clicking a row expands its real content in place (no modal).
  for (const [id, marker] of [[`${runIds[1]}-s9`, EXPAND_MARKER], [`${runIds[0]}-s6`, THINK_MARKER]]) {
    const row = timeline.locator(`.timeline-row[data-step-id="${id}"]`);
    await expect(row, `no row for ${id}`).toHaveCount(1);
    await row.locator('.timeline-row-summary').scrollIntoViewIfNeeded();
    await row.locator('.timeline-row-summary').click();
    const body = row.locator('.timeline-body');
    await expect(body, `${id} did not expand`).toBeVisible();
    await expect(body).toContainText(marker);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expectInViewport(page, row.locator('.timeline-row-summary'), `${id} summary after expand`);
  }
});

test('step 5: full brief, dev-server log, agent summary and migration SQL open in a modal', async ({ page, api }) => {
  test.setTimeout(90_000);
  await openHeavyTask(page, api);

  const cases: Array<{ tab: RegExp; inline: string; trigger: string; marker: string }> = [
    { tab: TABS[0], inline: '.ship-review-summary-body', trigger: 'agent-summary', marker: SUMMARY_END },
    { tab: TABS[0], inline: '.ship-review-devenv-log', trigger: 'dev-log', marker: DEVLOG_END },
    { tab: TABS[2], inline: '.migration-sql-block', trigger: 'migration-sql', marker: SQL_END },
    { tab: TABS[3], inline: '.desc-view', trigger: 'brief', marker: BRIEF_END },
  ];
  for (const c of cases) {
    const tabpanel = await selectTab(page, c.tab);

    // Inline: a bounded preview that contains its own overflow.
    const block = tabpanel.locator(c.inline).first();
    await expect(block, `${c.inline} not shown on its tab`).toBeVisible();
    const m = await block.evaluate((el) => {
      const cs = getComputedStyle(el);
      return {
        height: el.getBoundingClientRect().height,
        spillsY: el.scrollHeight > el.clientHeight + 1 && cs.overflowY === 'visible',
        spillsX: el.scrollWidth > el.clientWidth + 1 && cs.overflowX === 'visible',
      };
    });
    expect(m.height, `${c.inline} renders ${Math.round(m.height)}px tall inline`).toBeLessThanOrEqual(MAX_INLINE_PX);
    expect(m.spillsY, `${c.inline} content spills out of its block vertically`).toBe(false);
    expect(m.spillsX, `${c.inline} content spills out of its block horizontally`).toBe(false);

    // Full content in a dialog; Escape closes it.
    const trigger = tabpanel.locator(`[data-modal-trigger="${c.trigger}"]`);
    await expect(trigger, `no [data-modal-trigger="${c.trigger}"] on its tab`).toBeVisible();
    await trigger.click();
    const dialog = page.getByRole('dialog').filter({ hasText: c.marker });
    await expect(dialog, `${c.trigger} did not open a dialog with the full content`).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(1);
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog')).toHaveCount(0);
  }
});

// STA-679 (Board, live screenshot after step 5): the clipped agent-summary
// preview drew its last line under the green "Migration marked applied" banner.
// The preview must be a real bounded box: overflow hidden, a bottom fade, and
// the banner in normal flow below it. Written before the fix (STA-680); the
// fixer must not edit or weaken these assertions.
//
// Fade check: the summary body has an ::after pseudo-element with a gradient
// background-image and a non-zero height (the fade drawn over the clipped
// bottom edge). A mask-image fade would not pass; that choice is deliberate so
// the check is one concrete, observable thing.
const SUMMARY_LAST_LINE = 'Which block I copied: SUMMARY-LAST-LINE-STA-679';

function longSummary(): string {
  const parts: string[] = ['## What changed', ''];
  for (let i = 1; i <= 24; i++) {
    parts.push(`Paragraph ${i}. The limiter now sets X-RateLimit-Remaining on every 2xx and Retry-After on the 429 path, and this paragraph wraps across several lines on purpose so the preview has real height to clip.`, '');
  }
  parts.push(SUMMARY_LAST_LINE);
  return parts.join('\n');
}

function appliedMigrations() {
  const m = migrations();
  return {
    ...m,
    migrations: m.migrations.map((x) => ({ ...x, applied_at: '2026-10-04T12:00:00Z', applied_by: 'board' })),
  };
}

// Board (STA-644): the same check at the stacked 800x900 layout too.
for (const vp of [WIDE, NARROW]) {
const shot = vp === WIDE ? 'task-page-review-long-summary.png' : `task-page-review-long-summary-${vp.width}x${vp.height}.png`;
test(`STA-679: long agent-summary preview is clipped with a fade and stays above the applied-migration banner (${vp.width}x${vp.height})`, async ({ page, api }) => {
  test.setTimeout(60_000);
  await page.setViewportSize(vp);
  const seed = await seedHeavyTask(page, api);
  // Registered after seedHeavyTask's routes, so these win (Playwright runs the
  // most recently registered matching route first).
  const base = `**/api/tasks/${encodeURIComponent(seed.task.id)}`;
  await page.route(`${base}/ship-review`, (r) =>
    r.request().method() === 'GET'
      ? r.fulfill(json({ ...shipCard(seed.task.id), agent_summary: longSummary() }))
      : r.continue());
  await page.route(`${base}/migrations`, (r) =>
    r.request().method() === 'GET' ? r.fulfill(json(appliedMigrations())) : r.continue());
  await gotoTaskPage(page, seed.task);
  await openTaskPanelTab(page, 'Review');

  const card = page.locator('#task-page-content .task-page-panel .ship-review-card');
  const summary = card.locator('.ship-review-summary-body');
  const banner = card.locator('.ship-review-migration-banner--applied');
  // Visibility first, so a missing block fails here rather than as a timeout.
  await expect(summary, 'no .ship-review-summary-body on the Review tab').toBeVisible();
  await expect(summary).toContainText('Paragraph 1.');
  await expect(banner, 'applied-migration banner did not render').toBeVisible({ timeout: 12_000 });
  await expect(banner).toContainText('Migration marked applied');
  await banner.scrollIntoViewIfNeeded(); // elementFromPoint below needs it on screen
  await page.waitForTimeout(300); // let late async renders settle before measuring and the capture
  await saveArtifactScreenshot(page, shot);

  const m = await summary.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const section = el.closest('.ship-review-summary-section') || el;
    const b = document.querySelector('#task-page-content .ship-review-migration-banner--applied')!.getBoundingClientRect();
    const after = getComputedStyle(el, '::after');
    return {
      bottom: r.bottom,
      sectionBottom: section.getBoundingClientRect().bottom,
      height: r.height,
      bannerTop: b.top,
      overflowY: getComputedStyle(el).overflowY,
      overflows: el.scrollHeight > el.clientHeight + 1,
      afterContent: after.content,
      afterBg: after.backgroundImage,
      afterHeight: parseFloat(after.height) || 0,
    };
  });

  expect(m.overflows, 'seeded summary is not long enough to be clipped').toBe(true);
  expect(m.bottom, `summary bottom ${Math.round(m.bottom)} is below the banner top ${Math.round(m.bannerTop)}`)
    .toBeLessThanOrEqual(m.bannerTop + 0.5);
  expect(m.sectionBottom, `summary section bottom ${Math.round(m.sectionBottom)} is below the banner top ${Math.round(m.bannerTop)}`)
    .toBeLessThanOrEqual(m.bannerTop + 0.5);
  expect(m.overflowY, '.ship-review-summary-body overflow-y').toBe('hidden');
  expect(m.height, `summary preview renders ${Math.round(m.height)}px tall`).toBeLessThanOrEqual(MAX_INLINE_PX);
  expect(m.afterContent, '.ship-review-summary-body::after has no content (no fade)').not.toBe('none');
  expect(m.afterBg, '.ship-review-summary-body::after background is not a gradient fade').toMatch(/gradient\(/);
  expect(m.afterHeight, '.ship-review-summary-body::after has no height').toBeGreaterThan(0);

  // The last line is not drawn on top of the banner: whatever paints at the
  // banner's centre is the banner itself.
  const hit = await banner.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return !!top && (top === el || el.contains(top));
  });
  expect(hit, 'something other than the banner paints over its centre').toBe(true);

  // The full summary, last line included, is one click away.
  const trigger = card.locator('[data-modal-trigger="agent-summary"]');
  await expect(trigger).toBeVisible();
  await trigger.click();
  await expect(page.getByRole('dialog').filter({ hasText: SUMMARY_LAST_LINE })).toBeVisible();
});
}

test('step 6: at 800x900 the page stacks, tabs are a segmented control, nothing overflows horizontally', async ({ page, api }) => {
  test.setTimeout(90_000);
  await openHeavyTask(page, api, NARROW);

  const content = page.locator('#task-page-content');
  const timeline = content.locator('.task-page-timeline');
  const panel = content.locator('.task-page-panel');
  await expect(timeline).toBeVisible();
  await expect(panel).toBeVisible();
  const cw = await content.evaluate((el) => el.clientWidth);
  const tb = (await timeline.boundingBox())!;
  const pb = (await panel.boundingBox())!;
  const stacked = pb.y >= tb.y + tb.height - 2 || tb.y >= pb.y + pb.height - 2;
  expect(stacked, `timeline (y=${Math.round(tb.y)} h=${Math.round(tb.height)}) and panel (y=${Math.round(pb.y)} h=${Math.round(pb.height)}) are side by side`).toBe(true);
  expect(tb.width, 'timeline does not take the full width').toBeGreaterThanOrEqual(cw * 0.8);
  expect(pb.width, 'panel does not take the full width').toBeGreaterThanOrEqual(cw * 0.8);

  // Segmented control: all tabs on one row, filling the tablist, no overflow.
  await expect(panel.getByRole('tablist'), 'no tablist in .task-page-panel').toBeVisible();
  const seg = await panel.getByRole('tablist').evaluate((list) => {
    const tabs = Array.from(list.querySelectorAll('[role="tab"]')).map((t) => t.getBoundingClientRect());
    const tops = tabs.map((r) => Math.round(r.top));
    return {
      count: tabs.length,
      oneRow: Math.max(...tops) - Math.min(...tops) <= 2,
      filled: tabs.reduce((sum, r) => sum + r.width, 0) / list.clientWidth,
      overflows: list.scrollWidth > list.clientWidth + 1,
    };
  });
  expect(seg.count).toBe(ALL_TABS.length);
  expect(seg.oneRow, 'tabs wrap onto more than one row').toBe(true);
  expect(seg.filled, 'tabs do not fill the row like a segmented control').toBeGreaterThanOrEqual(0.9);
  expect(seg.overflows, 'tablist overflows horizontally').toBe(false);

  // No horizontal overflow anywhere, on every tab.
  for (const name of TABS) {
    await selectTab(page, name);
    const offenders = await page.evaluate(() => {
      const out: string[] = [];
      const se = document.scrollingElement!;
      if (se.scrollWidth > se.clientWidth + 1) out.push(`document ${se.scrollWidth} > ${se.clientWidth}`);
      const root = document.getElementById('task-page-content')!;
      const vw = document.documentElement.clientWidth;
      for (const el of [root, ...Array.from(root.querySelectorAll('*'))] as HTMLElement[]) {
        const cs = getComputedStyle(el);
        if (cs.display === 'none' || cs.display === 'inline' || cs.display === 'contents') continue;
        if (el.clientWidth === 0) continue;
        const name = `${el.tagName.toLowerCase()}.${String(el.className).trim().replace(/\s+/g, '.')}`;
        if (cs.overflowX === 'visible' && el.scrollWidth > el.clientWidth + 1) {
          out.push(`${name} scrollWidth ${el.scrollWidth} > clientWidth ${el.clientWidth}`);
        }
        const r = el.getBoundingClientRect();
        if (r.right > vw + 1) {
          let clipped = false;
          for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
            if (getComputedStyle(p).overflowX !== 'visible' && p.getBoundingClientRect().right <= vw + 1) { clipped = true; break; }
          }
          if (!clipped) out.push(`${name} right ${Math.round(r.right)} > viewport ${vw}`);
        }
      }
      return out;
    });
    expect(offenders, `${name}: ${offenders.slice(0, 5).join('; ')}`).toEqual([]);
  }
});
