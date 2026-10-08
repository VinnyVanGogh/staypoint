/**
 * spec 22: the task page renders before its git-backed panels, and a bare
 * /tasks/<id> link ends on the canonical URL (STA-775)
 *
 * The page used to wait for /diff and /checkpoints (several git processes
 * each, seconds on a loaded machine) before drawing anything. It now renders
 * from the DB-backed calls and fills the Diff tab and the Lines stat later.
 *
 * A pasted /tasks/task-… link kept that URL: only in-app navigation replaced
 * it with /tasks/<org>/<project>/<id>.
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

/** Lists the task's org in /api/fleet/overview with an issue prefix. */
async function seedFleetOrg(page: Page, org: string, prefix: string) {
  await page.route('**/api/fleet/overview', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.organizations = [
      ...(body.organizations || []),
      {
        id: `org-${prefix.toLowerCase()}`,
        name: org,
        issue_prefix: prefix,
        task_counts: { total: 1, running: 0, active: 1, stopped: 0, blocked: 0, errored: 0, done: 0 },
        active_agents: 0,
        active_agents_by_provider: {},
        spent_usd: 0,
        spent_tokens: 0,
        tasks: [],
      },
    ];
    await route.fulfill({ response: res, json: body });
  });
}

test('a bare /tasks/<id> link is replaced with /tasks/<org>/<project>/<id>', async ({ page, api, baseURL }) => {
  const org = 'Canon Org';
  const project = `canon-${Date.now().toString(36)}`;
  const task = await api.createTask('Canonical URL', { organization: org, project });
  await seedFleetOrg(page, org, 'CAN');

  await page.goto(`${baseURL}/tasks/${encodeURIComponent(task.id)}`);
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
  await expect(page).toHaveURL(`${baseURL}/tasks/CAN/${project}/${task.id}`, { timeout: 20_000 });

  // replaceState, not pushState: Back leaves the page instead of returning to
  // the bare link.
  expect(await page.evaluate(() => history.state && history.state.canonicalPath)).toBe(`/tasks/CAN/${project}/${task.id}`);
});
