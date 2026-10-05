/**
 * spec 17: live run steps land in the right timeline group (STA-642)
 *
 * Spec 15 checks the grouping a page load renders. Here every step arrives as
 * a run.step SSE event on an empty timeline, fed straight into handleEvent
 * (app.js is a classic script, so it is a global), and each row must land in
 * its run and subtask the same way: a child creates its parent's subtask
 * wrapper, a running -> done upsert stays in place, and a second run gets its
 * own wrapper with "Run N" headers.
 */

import type { Page } from '@playwright/test';
import { test, expect, gotoTaskPage } from '../fixtures';

type Step = {
  id: string; run_id: string; task_id: string; seq: number; parent_seq?: number;
  kind: string; title: string; body?: string; status: string;
  started_at: string; ended_at: string; created_at: string;
};

function mk(taskId: string, run: string, seq: number, kind: string, title: string, extra: Partial<Step> = {}): Step {
  const at = new Date(Date.now() - (60 - seq) * 1_000).toISOString();
  return {
    id: `${run}-s${seq}`, run_id: run, task_id: taskId, seq, kind, title,
    status: 'done', started_at: at, ended_at: at, created_at: at, ...extra,
  };
}

async function send(page: Page, step: Step) {
  await page.evaluate((data) => (window as any).handleEvent({ type: 'run.step', data }), step);
}

/** run id and subtask step id (or null) that hold the row for stepId. */
async function placement(page: Page, stepId: string) {
  return page.evaluate((id) => {
    const rows = document.querySelectorAll(`#task-page-content .timeline-row[data-step-id="${CSS.escape(id)}"]`);
    if (rows.length !== 1) return { rows: rows.length, run: null, subtask: null };
    const row = rows[0] as HTMLElement;
    return {
      rows: 1,
      run: (row.closest('.timeline-run') as HTMLElement | null)?.dataset.runId ?? null,
      subtask: (row.closest('.timeline-subtask') as HTMLElement | null)?.dataset.subtaskStepId ?? null,
    };
  }, stepId);
}

test('live run steps land in their run and subtask groups', async ({ page, api }) => {
  const task = await api.createTask('Timeline live grouping');
  await gotoTaskPage(page, task);
  const steps = page.locator(`#timeline-steps-${task.id}`);
  await expect(steps.locator('.timeline-empty')).toBeVisible();

  const tag = task.id.slice(0, 8);
  const runA = `live-a-${tag}`;
  const runB = `live-b-${tag}`;

  await send(page, mk(task.id, runA, 1, 'wake', 'Woke up: assigned by Board'));
  await send(page, mk(task.id, runA, 2, 'message', 'Subtask 1 · Limiter'));
  await expect(steps.locator('.timeline-empty')).toHaveCount(0);
  await expect(steps.locator('.timeline-run')).toHaveCount(1);
  expect(await placement(page, `${runA}-s2`)).toEqual({ rows: 1, run: runA, subtask: null });

  // The first child turns its parent into a subtask; both rows sit inside it.
  await send(page, mk(task.id, runA, 3, 'edit', 'Edit middleware.go', { parent_seq: 2 }));
  await expect(steps.locator('.timeline-subtask')).toHaveCount(1);
  expect(await placement(page, `${runA}-s2`)).toEqual({ rows: 1, run: runA, subtask: `${runA}-s2` });
  expect(await placement(page, `${runA}-s3`)).toEqual({ rows: 1, run: runA, subtask: `${runA}-s2` });

  // A running step that later finishes is replaced in place, not duplicated.
  await send(page, mk(task.id, runA, 4, 'run', 'go test ./...', { parent_seq: 2, status: 'running' }));
  await send(page, mk(task.id, runA, 4, 'run', 'go test ./...', { parent_seq: 2, body: 'ok' }));
  expect(await placement(page, `${runA}-s4`)).toEqual({ rows: 1, run: runA, subtask: `${runA}-s2` });
  await expect(steps.locator(`.timeline-row[data-step-id="${runA}-s4"] .timeline-exit-pass`)).toBeVisible();

  // A top-level step after the subtask sits outside it.
  await send(page, mk(task.id, runA, 5, 'state', 'Finished: in_review'));
  expect(await placement(page, `${runA}-s5`)).toEqual({ rows: 1, run: runA, subtask: null });
  // One real run: no run header yet.
  await expect(steps.locator('.timeline-run-header')).toHaveCount(0);

  // A second run gets its own wrapper; once it does real work both runs are headed.
  await send(page, mk(task.id, runB, 1, 'wake', 'Woke up: sent back by Board'));
  await send(page, mk(task.id, runB, 2, 'message', 'Subtask 1 · Retry-After'));
  await send(page, mk(task.id, runB, 3, 'think', 'Thinking', { parent_seq: 2, body: 'reset from the bucket' }));
  await expect(steps.locator('.timeline-run')).toHaveCount(2);
  await expect(steps.locator('.timeline-run').nth(1)).toHaveAttribute('data-run-id', runB);
  expect(await placement(page, `${runB}-s3`)).toEqual({ rows: 1, run: runB, subtask: `${runB}-s2` });
  await expect(steps.locator('.timeline-run-header')).toHaveText([/^Run 1\b/, /^Run 2\b/]);

  // The same row count a reload renders.
  await expect(steps.locator('.timeline-row')).toHaveCount(8);
});
