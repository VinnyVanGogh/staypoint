import { test, expect, gotoTaskPage, type Task } from '../fixtures';
import type { Page, Locator } from '@playwright/test';

// task-3387cad2: every task list shows the Stage and, separately, a Running
// badge only while a run is in flight. "ACTIVE" is gone. Running and Stage
// filters narrow each list and persist per page. The sidebar's org number is
// the org's live runs.
//
// The throwaway daemon has no runner: setLiveRun() takes a run slot and checks
// the task out the way Harness.Claim does. A task moved to in_progress with
// Run Now and no run behind it is the case the Board saw labelled as running.

type Seeded = { live: Task; todo: Task; stale: Task; org: string };

async function seed(api: import('../fixtures').StayPointAPI, label: string): Promise<Seeded> {
  const org = `E2E Run ${Date.now().toString(36)}${Math.random().toString(36).slice(2, 5)}`;
  const live = await api.createTask(`${label} live`, { organization: org });
  const todo = await api.createTask(`${label} todo`, { organization: org });
  const stale = await api.createTask(`${label} stale`, { organization: org });
  await api.setLiveRun(live.id, true);
  await api.setStage(stale.id, 'in_progress');
  return { live, todo, stale, org };
}

async function release(api: import('../fixtures').StayPointAPI, s: Seeded) {
  await api.setLiveRun(s.live.id, false).catch(() => {});
}

// row finds the element listing task on the current page.
function row(page: Page, scope: string, task: Task): Locator {
  return page.locator(scope).filter({ hasText: task.name }).first();
}

async function expectRunning(r: Locator) {
  await expect(r.locator('.run-badge')).toBeVisible();
  await expect(r.locator('.run-badge')).toContainText('Running');
}

async function expectNotRunning(r: Locator) {
  await expect(r).toBeVisible();
  await expect(r.locator('.run-badge')).toHaveCount(0);
}

const LIST_PAGES = [
  { name: 'Global Task Status', path: '/task-status', rows: '#ts-task-tbody tr' },
  { name: 'Recent Tasks', path: '/recent-tasks', rows: '#recent-tasks-feed .activity-item' },
  { name: 'All Tasks', path: '/all-tasks', rows: '#all-tasks-tbody tr' },
  { name: 'Overview table', path: '/', rows: '#global-task-tbody tr' },
  { name: 'Kanban', path: '/kanban', rows: '#kanban-board .task-card' },
];

test.describe('Running vs Stage on task lists', () => {
  for (const p of LIST_PAGES) {
    test(`${p.name}: a running task shows Running, todo and stale in-progress do not`, async ({ page, api }) => {
      test.setTimeout(60_000);
      const s = await seed(api, p.name);
      try {
        // Lists built from the fleet overview lag new tasks by its ~10 s
        // server cache; reload until all three are listed.
        await expect.poll(async () => {
          await page.goto(p.path);
          await row(page, p.rows, s.stale).waitFor({ timeout: 5_000 }).catch(() => {});
          let n = 0;
          for (const t of [s.live, s.todo, s.stale]) n += Math.min(1, await page.locator(p.rows).filter({ hasText: t.name }).count());
          return n;
        }, { timeout: 40_000, intervals: [2_000] }).toBe(3);
        await expectRunning(row(page, p.rows, s.live));
        await expectNotRunning(row(page, p.rows, s.todo));
        await expectNotRunning(row(page, p.rows, s.stale));
        if (p.path !== '/kanban') {
          // The stage pill says where the task is; it never says "active".
          await expect(row(page, p.rows, s.todo).locator('.stage-pill')).toHaveText(/todo/i);
          await expect(row(page, p.rows, s.stale).locator('.stage-pill')).toHaveText(/in progress/i);
        }
        for (const t of [s.live, s.todo, s.stale]) {
          expect(await row(page, p.rows, t).innerText()).not.toMatch(/\bactive\b/i);
        }

        // The run ends: the badge goes without a reload.
        await api.setLiveRun(s.live.id, false);
        await expectNotRunning(row(page, p.rows, s.live));
      } finally {
        await release(api, s);
      }
    });
  }

  test('Running filter narrows the list and persists per page', async ({ page, api }) => {
    const s = await seed(api, 'Filter');
    try {
      await page.goto('/task-status');
      const rows = '#ts-task-tbody tr';
      await expect(row(page, rows, s.todo)).toBeVisible();

      const running = page.locator('.list-filters[data-page="task-status"] .list-filter-running');
      await running.selectOption('yes');
      await expectRunning(row(page, rows, s.live));
      await expect(page.locator(rows).filter({ hasText: s.todo.name })).toHaveCount(0);
      await expect(page.locator(rows).filter({ hasText: s.stale.name })).toHaveCount(0);
      for (const r of await page.locator(rows).all()) {
        const text = await r.innerText();
        if (/No tasks match/.test(text)) continue;
        await expect(r.locator('.run-badge')).toHaveCount(1);
      }

      // Persisted for this page only.
      await page.reload();
      await expect(page.locator('.list-filters[data-page="task-status"] .list-filter-running')).toHaveValue('yes');
      await expect(page.locator(rows).filter({ hasText: s.todo.name })).toHaveCount(0);
      await page.goto('/all-tasks');
      await expect(page.locator('.list-filters[data-page="all-tasks"] .list-filter-running')).toHaveValue('all');
      await expect(row(page, '#all-tasks-tbody tr', s.todo)).toBeVisible();

      // Not running: the stale in-progress task is here, the live one is not.
      await page.locator('.list-filters[data-page="all-tasks"] .list-filter-running').selectOption('no');
      await expect(row(page, '#all-tasks-tbody tr', s.stale)).toBeVisible();
      await expect(page.locator('#all-tasks-tbody tr').filter({ hasText: s.live.name })).toHaveCount(0);

      // Stage multi-select: In progress only.
      await page.locator('.list-filters[data-page="all-tasks"] .list-filter-running').selectOption('all');
      await page.locator('.list-filters[data-page="all-tasks"] .stage-filter-summary').click();
      await page.locator('.list-filters[data-page="all-tasks"] .stage-filter-menu input[value="in_progress"]').check();
      await expect(page.locator('.list-filters[data-page="all-tasks"] .stage-filter-summary')).toHaveText('Stage: In progress');
      await expect(row(page, '#all-tasks-tbody tr', s.live)).toBeVisible();
      await expect(row(page, '#all-tasks-tbody tr', s.stale)).toBeVisible();
      await expect(page.locator('#all-tasks-tbody tr').filter({ hasText: s.todo.name })).toHaveCount(0);
    } finally {
      await release(api, s);
      await page.evaluate(() => {
        for (const k of Object.keys(localStorage)) if (k.startsWith('staypoint_list_filter_')) localStorage.removeItem(k);
      }).catch(() => {});
    }
  });

  test('sidebar org count equals that org\'s live runs', async ({ page, api }) => {
    test.setTimeout(60_000);
    const s = await seed(api, 'Sidebar');
    try {
      const second = await api.createTask('Sidebar live 2', { organization: s.org });
      await api.setLiveRun(second.id, true);
      const live = (await api.liveRuns()).filter(r => r.organization === s.org);
      expect(live).toHaveLength(2);

      // The org list comes from the fleet overview (cached ~10 s server side).
      const item = page.locator(`#sidebar-org-tree .sidebar-org-item[data-org="${s.org}"]`);
      await expect.poll(async () => {
        await page.goto('/');
        return item.count();
      }, { timeout: 40_000, intervals: [2_000] }).toBe(1);
      await expect(item.locator('.sidebar-org-running')).toHaveText('· 2 running');

      await api.setLiveRun(second.id, false);
      await expect(item.locator('.sidebar-org-running')).toHaveText('· 1 running', { timeout: 15_000 });
    } finally {
      await release(api, s);
    }
  });

  test('task page header: stage badge, Running only while live', async ({ page, api }) => {
    const s = await seed(api, 'Header');
    try {
      await gotoTaskPage(page, s.live);
      const badges = page.locator(`#task-page-state-${s.live.id}`);
      await expect(badges.locator('.stage-pill')).toHaveText(/in progress/i);
      await expect(badges.locator('.run-badge')).toBeVisible();

      await api.setLiveRun(s.live.id, false);
      await expect(page.locator(`#task-page-state-${s.live.id} .run-badge`)).toHaveCount(0, { timeout: 15_000 });
      await expect(page.locator(`#task-page-state-${s.live.id} .stage-pill`)).toBeVisible();

      await gotoTaskPage(page, s.stale);
      await expect(page.locator(`#task-page-state-${s.stale.id} .stage-pill`)).toHaveText(/in progress/i);
      await expect(page.locator(`#task-page-state-${s.stale.id} .run-badge`)).toHaveCount(0);
    } finally {
      await release(api, s);
    }
  });
});
