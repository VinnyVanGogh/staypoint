import type { Page } from '@playwright/test';
import { test, expect, type Task } from '../fixtures';

// STA-693: a local task has no identifier of its own. The fleet overview labels
// it "<issue prefix>-<first 6 chars of id>" (e.g. "RHI-task-e"), the UI merged
// that label into its task state, and the full-page link was built from it.
// No endpoint resolves the label, so the task page said "Task not found."

// app.js is a classic script, so its top-level `state` is a page global.
declare const state: { tasks: Record<string, { identifier?: string }> };

const ORG = 'Rhizome';
const PREFIX = 'RHI';

function fleetLabel(task: Task): string {
  return `${PREFIX}-${task.id.slice(0, 6)}`;
}

/** Adds a non-STA org to /api/fleet/overview listing the task under its display label. */
async function seedFleetLabel(page: Page, task: Task, project: string) {
  await page.route('**/api/fleet/overview', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const item = {
      id: task.id,
      identifier: fleetLabel(task),
      title: task.name,
      organization: ORG,
      project,
      status: 'active',
      execution_stage: 'todo',
      spent_usd: 0,
      spent_tokens: 0,
      is_blocked: false,
      updated_at: new Date().toISOString(),
    };
    body.organizations = [
      ...(body.organizations || []),
      {
        id: 'org-rhizome',
        name: ORG,
        issue_prefix: PREFIX,
        task_counts: { total: 1, running: 0, active: 1, stopped: 0, blocked: 0, errored: 0, done: 0 },
        active_agents: 0,
        active_agents_by_provider: {},
        spent_usd: 0,
        spent_tokens: 0,
        tasks: [item],
      },
    ];
    body.tasks = [...(body.tasks || []), item];
    await route.fulfill({ response: res, json: body });
  });
}

test.describe('task link without identifier (STA-693)', () => {
  test('peek view expand opens the full page for a non-STA task with identifier null', async ({ page, api }) => {
    const project = `rhizome-site-${Date.now().toString(36)}`;
    const task = await api.createTask('NoIdent', { organization: ORG, project });
    const label = fleetLabel(task);
    await seedFleetLabel(page, task, project);

    await page.goto('/task-status');
    // Precondition: the fleet label reached the UI's task state, as in production.
    await expect
      .poll(() => page.evaluate((id) => (state.tasks[id] || {}).identifier, task.id), { timeout: 20_000 })
      .toBe(label);

    await page.locator('#ts-task-table').getByText(task.name).click();
    const panel = page.locator('#detail-panel');
    await expect(panel).toBeVisible();
    await expect(panel).toContainText(task.name);
    // The peek URL carries the task's own org and project, not STA/default.
    await expect(page).toHaveURL(new RegExp(`/tasks/${PREFIX}/${project}/${task.id}$`));

    // Hold the task fetch: on a loaded daemon the page sits on its first URL for
    // many seconds, and a reload or copied link in that window must still resolve.
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route(`**/api/tasks/${task.id}`, async (route) => {
      await held;
      await route.continue();
    });
    await page.locator('#panel-open').click();
    await expect(page).toHaveURL(new RegExp(`/tasks/${PREFIX}/${project}/${task.id}$`));
    release();
    await page.unroute(`**/api/tasks/${task.id}`);

    const title = page.locator('#task-page-content .task-page-title');
    await expect(title).toHaveText(task.name, { timeout: 20_000 });
    await expect(page).toHaveURL(new RegExp(`/tasks/${PREFIX}/${project}/${task.id}$`));
    await expect(page.locator('#task-page-content')).not.toContainText('Task not found');

    // The link must survive a reload, which resolves it before fleet data loads.
    await page.reload();
    await expect(title).toHaveText(task.name, { timeout: 20_000 });
    await expect(page).toHaveURL(new RegExp(`/tasks/${PREFIX}/${project}/${task.id}$`));
  });

  test('legacy URL with the fleet label in the ident slot resolves by id', async ({ page, api }) => {
    const project = `rhizome-site-${Date.now().toString(36)}`;
    const task = await api.createTask('LegacyLabel', { organization: ORG, project });
    // The label was minted by the fleet overview, which maps Rhizome to RHI. The
    // e2e daemon's overview leaves local orgs out, so seed it as production has it.
    await seedFleetLabel(page, task, project);

    await page.goto(`/tasks/${PREFIX}/${project}/${fleetLabel(task)}`);
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    // The URL is rewritten to the resolvable task id.
    await expect(page).toHaveURL(new RegExp(`/${task.id}$`));
  });

  // STA-722: the label prefix is the task's own org, so a legacy RHI label must
  // not open a task from another org just because it is the only id match (the
  // RHI task may be deleted or past the task list cap). The full id stands in
  // for the 6-char id part so the match is unique however many tasks the run has.
  test('legacy label URL does not resolve to a task in another org', async ({ page, api }) => {
    const task = await api.createTask('OtherOrg'); // STA / ui-e2e
    const path = `/tasks/${PREFIX}/rhizome-gone/${PREFIX}-${task.id}`;

    await page.goto(path);
    await expect(page.locator('#task-page-content')).toContainText('Task not found', { timeout: 20_000 });
    await expect(page.locator('#task-page-content .task-page-title')).toHaveCount(0);
    expect(new URL(page.url()).pathname).toBe(path);
  });

  test('legacy label URL resolves without the overview when the org name is the prefix', async ({ page, api }) => {
    const task = await api.createTask('NamedByPrefix', { organization: PREFIX, project: 'rhizome-site' });
    await page.route('**/api/fleet/overview', (route) => route.abort());

    await page.goto(`/tasks/${PREFIX}/default/${fleetLabel(task)}`);
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    await expect(page).toHaveURL(new RegExp(`/${task.id}$`));
  });

  test('legacy label URL stays a miss when the org prefix cannot be loaded', async ({ page, api }) => {
    const project = `rhizome-site-${Date.now().toString(36)}`;
    const task = await api.createTask('NoFleet', { organization: ORG, project });
    // Without the overview nothing maps "Rhizome" to RHI; resolving anyway would
    // be a guess, and that guess is what lets another org's task through.
    await page.route('**/api/fleet/overview', (route) => route.abort());
    const path = `/tasks/${PREFIX}/${project}/${fleetLabel(task)}`;

    await page.goto(path);
    await expect(page.locator('#task-page-content')).toContainText('Task not found', { timeout: 20_000 });
    expect(new URL(page.url()).pathname).toBe(path);
  });
});
