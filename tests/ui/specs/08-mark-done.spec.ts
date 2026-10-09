import { test, expect, gotoTaskPage } from '../fixtures';

// Mark done button: visible for in_review tasks, calls POST /api/tasks/{id}/done,
// surfaces error inline (no work product → 409, watchdog block → 500).

test('Mark done button is visible when execution_stage is in_review', async ({ page, api, request }) => {
  const task = await api.createTask('Mark done visible');

  // Move the task to in_review via the stage endpoint.
  const headers = { Authorization: `Bearer ${process.env.STAYPOINT_API_TOKEN || ''}`, 'Content-Type': 'application/json' };
  const stageRes = await request.post(`/api/tasks/${encodeURIComponent(task.id)}/stage`, {
    headers,
    data: { stage: 'in_review' },
  });
  expect(stageRes.ok(), `stage→in_review failed: ${await stageRes.text()}`).toBeTruthy();

  const fresh = await api.getTask(task.id);
  expect(fresh.execution_stage).toBe('in_review');

  await gotoTaskPage(page, task);
  const btn = page.locator('#task-page-content .mark-done-btn');
  await expect(btn).toBeVisible({ timeout: 5_000 });
  await expect(btn).toContainText(/mark done/i);
});

test('Mark done and Cancel task show on a todo task; both are gone once it is cancelled (STA-861)', async ({ page, api }) => {
  const task = await api.createTask('Mark done any stage');
  expect(task.execution_stage).toBe('todo');

  await gotoTaskPage(page, task);
  const content = page.locator('#task-page-content');
  await expect(content.locator('.mark-done-btn')).toBeVisible({ timeout: 5_000 });
  await expect(content.locator('.cancel-task-btn')).toBeVisible();

  await api.setStage(task.id, 'cancelled');
  await gotoTaskPage(page, task);
  await expect(content.locator('.mark-done-btn')).toHaveCount(0);
  await expect(content.locator('.cancel-task-btn')).toHaveCount(0);
});

test('Board Mark done closes a task with no work product and keeps the note (STA-861)', async ({ boardPage: page, api }) => {
  const task = await api.createTask('Mark done board close');
  await api.setStage(task.id, 'in_review');

  await gotoTaskPage(page, task);
  const btn = page.locator('#task-page-content .mark-done-btn');
  await expect(btn).toBeVisible({ timeout: 5_000 });

  // No run and no card: no confirm, only the optional-note prompt.
  page.once('dialog', (d) => d.accept('closed from the ui-e2e suite'));
  const done = page.waitForResponse((r) => r.url().endsWith(`/api/tasks/${encodeURIComponent(task.id)}/done`));
  await btn.click();
  const res = await done;
  expect(res.ok(), `done failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  expect(JSON.parse(res.request().postData() || '{}')).toEqual({ board: true, note: 'closed from the ui-e2e suite' });
  await expect.poll(async () => (await api.getTask(task.id)).execution_stage).toBe('done');
});

test('Mark done shows the server refusal inline and stays enabled for retry', async ({ boardPage: page, api }) => {
  const task = await api.createTask('Mark done refused');
  await api.setStage(task.id, 'in_review');
  await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/done`, (route) => route.fulfill({
    status: 500,
    contentType: 'application/json',
    body: JSON.stringify({ error: 'watchdog_blocked', message: 'watchdog blocked the close' }),
  }));

  await gotoTaskPage(page, task);
  const content = page.locator('#task-page-content');
  const btn = content.locator('.mark-done-btn');
  await expect(btn).toBeVisible({ timeout: 5_000 });
  page.once('dialog', (d) => d.accept(''));
  await btn.click();

  const errDiv = content.locator('.mark-done-error');
  await expect(errDiv).toBeVisible({ timeout: 5_000 });
  await expect(errDiv).toContainText('watchdog_blocked');
  await expect(btn).toBeEnabled();
  await expect(btn).toHaveText(/mark done/i);
});
