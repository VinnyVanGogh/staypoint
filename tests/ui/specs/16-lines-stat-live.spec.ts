/**
 * spec 16: the stats strip's Lines +/- stays live during a run (STA-658)
 *
 * Lines sums the whole-run diff's file_stats. The page loads that diff once,
 * so before STA-658 an edit step arriving over SSE re-rendered the strip with
 * the same stale numbers until a reload. Here the run step arrives over a
 * routed /api/events stream and /diff answers with bigger numbers once the
 * edit has "happened"; the strip must pick them up without a reload.
 */

import type { Page } from '@playwright/test';
import { test, expect, gotoTaskPage } from '../fixtures';

const BEFORE = {
  diff: ' 1 file changed, 3 insertions(+), 1 deletion(-)',
  files: ['internal/server/middleware.go'],
  file_stats: [{ path: 'internal/server/middleware.go', added: 3, removed: 1 }],
  checkpoint_id: '',
};

const AFTER = {
  diff: ' 2 files changed, 10 insertions(+), 4 deletions(-)',
  files: ['internal/server/middleware.go', 'internal/server/server.go'],
  file_stats: [
    { path: 'internal/server/middleware.go', added: 7, removed: 3 },
    { path: 'internal/server/server.go', added: 3, removed: 1 },
  ],
  checkpoint_id: '',
};

const json = (body: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

function step(taskId: string, seq: number, kind: string, title: string, status: string) {
  const at = new Date(Date.now() - (10 - seq) * 1_000).toISOString();
  return {
    id: `run-live-${taskId.slice(0, 8)}-s${seq}`, run_id: `run-live-${taskId.slice(0, 8)}`,
    task_id: taskId, seq, kind, title, status, started_at: at, ended_at: at, created_at: at,
  };
}

function linesVal(page: Page, taskId: string) {
  return page.locator(`#timeline-stats-${taskId} .timeline-stat`, {
    has: page.locator('.timeline-stat-label', { hasText: 'Lines' }),
  }).locator('.timeline-stat-val');
}

test('Lines +/- updates after an edit step without a reload', async ({ page, api }) => {
  const task = await api.createTask('Lines live');
  const base = `**/api/tasks/${encodeURIComponent(task.id)}`;

  // A run in progress: woke up, nothing edited yet.
  await page.route(`${base}/run-steps`, (r) => r.fulfill(json({ steps: [step(task.id, 1, 'wake', 'Woke up: Run Now', 'done')] })));

  // /diff reports the bigger numbers only once the edit step has been sent.
  let edited = false;
  const diffCalls: string[] = [];
  const serveDiff = (r: Parameters<Parameters<Page['route']>[1]>[0]) => {
    diffCalls.push(r.request().url());
    return r.fulfill(json(edited ? AFTER : BEFORE));
  };
  await page.route(`${base}/diff`, serveDiff);
  await page.route(`${base}/diff?*`, serveDiff);

  // SSE: empty streams (the client reconnects every 3s) until the test
  // releases the edit step, which goes out exactly once.
  let release = false;
  const editStep = step(task.id, 2, 'edit', 'Edit internal/server/server.go', 'done');
  await page.route('**/api/events*', (r) => {
    let body = '';
    if (release && !edited) {
      edited = true;
      body = `data: ${JSON.stringify({ id: 1, type: 'run.step', data: editStep })}\n\n`;
    }
    return r.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body });
  });

  await gotoTaskPage(page, task);
  const lines = linesVal(page, task.id);
  await expect(lines).toHaveText('+3 −1', { timeout: 10_000 });

  // Marks this document; a reload would drop it.
  await page.evaluate(() => { (window as unknown as { __sta658?: boolean }).__sta658 = true; });
  const callsBefore = diffCalls.length;

  release = true;
  await expect(page.locator(`#timeline-steps-${task.id} [data-step-id="${editStep.id}"]`)).toBeVisible({ timeout: 10_000 });
  await expect(lines).toHaveText('+10 −4', { timeout: 10_000 });

  expect(await page.evaluate(() => (window as unknown as { __sta658?: boolean }).__sta658), 'page reloaded').toBe(true);
  expect(diffCalls.length - callsBefore, 'one edit step should cost one /diff call').toBe(1);
});
