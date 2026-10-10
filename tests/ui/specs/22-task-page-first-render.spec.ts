/**
 * spec 22: the task page renders before its git-backed panels, and a bare
 * /tasks/<id> link ends on the canonical URL (STA-775)
 *
 * The page used to wait for /diff and /checkpoints (several git processes
 * each, seconds on a loaded machine) before drawing anything. It now renders
 * from the DB-backed calls and fills the Diff tab and the Lines stat later.
 *
 * A pasted /tasks/task-… link kept that URL. The server now 301s it to the
 * canonical /STA-123/<slug> URL (task-eb38c245).
 */

import type { Page, Route } from '@playwright/test';
import { test, expect, gotoTaskPage, openTaskPanelTab } from '../fixtures';

const DIFF = {
  diff: ' 1 file changed, 3 insertions(+), 1 deletion(-)',
  files: ['internal/server/middleware.go'],
  file_stats: [{ path: 'internal/server/middleware.go', added: 3, removed: 1 }],
  checkpoint_id: '',
};

const json = (body: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

function linesVal(page: Page, taskId: string) {
  return page.locator(`#timeline-stats-${taskId} .timeline-stat`, {
    has: page.locator('.timeline-stat-label', { hasText: 'Lines' }),
  }).locator('.timeline-stat-val');
}

test('task page renders while the diff is still loading', async ({ page, api }) => {
  const task = await api.createTask('First render');
  const base = `**/api/tasks/${encodeURIComponent(task.id)}`;

  // Hold /diff and /checkpoints until the test releases them.
  let release!: () => void;
  const released = new Promise<void>((r) => { release = r; });
  const held: string[] = [];
  const hold = (body: unknown) => async (r: Route) => {
    held.push(r.request().url());
    await released;
    await r.fulfill(json(body));
  };
  await page.route(`${base}/diff`, hold(DIFF));
  await page.route(`${base}/checkpoints`, hold({ checkpoints: [] }));

  await gotoTaskPage(page, task);
  await expect.poll(() => held.length, { message: 'both git-backed calls were made' }).toBe(2);

  await openTaskPanelTab(page, 'Diff');
  await expect(page.locator('#task-page-content .task-page-diff-loading')).toHaveText('Loading diff…');
  await expect(linesVal(page, task.id)).toHaveText('…');

  release();
  await expect(page.locator('#task-page-content .task-page-diff-loading')).toHaveCount(0, { timeout: 10_000 });
  await expect(page.locator('#task-page-content .diff-file-list')).toContainText('middleware.go');
  await expect(linesVal(page, task.id)).toHaveText('+3 −1');
});

test('a stale diff for the previous task does not paint over the next one', async ({ page, api }) => {
  const a = await api.createTask('Stale A');
  const b = await api.createTask('Stale B');

  let release!: () => void;
  const released = new Promise<void>((r) => { release = r; });
  await page.route(`**/api/tasks/${encodeURIComponent(a.id)}/diff`, async (r) => {
    await released;
    await r.fulfill(json(DIFF));
  });

  await gotoTaskPage(page, a);
  await page.evaluate((id) => (window as unknown as { openTaskPage: (id: string) => void }).openTaskPage(id), b.id);
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(b.name, { timeout: 20_000 });
  release();

  await openTaskPanelTab(page, 'Diff');
  await page.waitForTimeout(1_000);
  await expect(page.locator('#task-page-content .diff-file-list')).not.toContainText('middleware.go');
  await expect(linesVal(page, b.id)).toHaveText('+0 −0');
});

test('a bare /tasks/<id> link is redirected to /<ORG>-<n>/<slug> (task-eb38c245)', async ({ page, api, baseURL }) => {
  const project = `canon-${Date.now().toString(36)}`;
  const task = await api.createTask('Canonical URL', { organization: 'Canon Org', project });
  // The org key comes from the server, not the fleet overview: an unknown org
  // takes its first three letters.
  expect(task.identifier).toMatch(/^CAN-\d+$/);

  const res = await page.goto(`${baseURL}/tasks/${encodeURIComponent(task.id)}`);
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
  await expect(page).toHaveURL(`${baseURL}/${task.identifier}/${task.slug}`);
  // A server redirect, so the bare link never enters history and Back leaves
  // the page instead of returning to it.
  expect(res?.request().redirectedFrom()?.url()).toBe(`${baseURL}/tasks/${task.id}`);
});
