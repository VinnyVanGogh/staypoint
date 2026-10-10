import { test, expect, BOARD_TOKEN, saveArtifactScreenshot } from '../fixtures';

// Board action plans (task-e1b24d66): a Board session proposes a batch of
// Board actions; the Board reviews them on /board/plans/<id>, unticks what it
// does not want and signs the rest with ONE passkey assertion (here a real
// WebAuthn ceremony against the CDP virtual authenticator, so the daemon's
// bound challenge is verified with real crypto).

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';

async function proposePlan(request: any, actions: unknown[]): Promise<string> {
  const res = await request.post('/api/board/plans', {
    headers: { Authorization: `Bearer ${TOKEN}`, 'X-Board-Token': BOARD_TOKEN, 'Content-Type': 'application/json' },
    data: { proposer: 'ui-e2e board session', actions },
  });
  expect(res.status(), await res.text()).toBe(201);
  return (await res.json()).plan.id;
}

test('an agent token alone cannot propose a plan', async ({ request, api }) => {
  const task = await api.createTask('Plan agent refused');
  const res = await request.post('/api/board/plans', {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    data: { actions: [{ task_id: task.id, action: 'mark_done' }] },
  });
  expect(res.status()).toBe(403);
});

test('one Touch ID runs the ticked rows only and shows per-row results', async ({ boardPage: page, api, request }) => {
  const keep = await api.createTask('Plan keep');
  const skip = await api.createTask('Plan skip');
  await api.setStage(keep.id, 'in_review');
  await api.setStage(skip.id, 'in_review');
  const id = await proposePlan(request, [
    { task_id: keep.id, action: 'mark_done', text: 'closed by a signed plan', reason: 'review done' },
    { task_id: 'task-missing', action: 'unblock', reason: 'this row fails' },
    { task_id: skip.id, action: 'mark_done', reason: 'Board unticks this' },
  ]);

  await page.goto(`/board/plans/${id}`);
  const rows = page.locator('#board-plans-container .board-plan-row');
  await expect(rows).toHaveCount(3, { timeout: 5_000 });
  await expect(rows.nth(0)).toContainText('closed by a signed plan');
  await expect(rows.nth(0)).toContainText('review done');
  await expect(page.locator('#board-plans-container')).toContainText('ui-e2e board session');

  await rows.nth(2).locator('.board-plan-check').uncheck();
  await saveArtifactScreenshot(page, 'board-plan-review.png');
  const sign = page.locator('.board-plan-sign-btn');
  await expect(sign).toContainText('Sign & run 2 actions');
  const exec = page.waitForResponse((r) => r.url().endsWith(`/api/board/plans/${id}/execute`));
  await sign.click();
  const res = await exec;
  expect(res.status(), await res.text()).toBe(200);

  await expect(rows.nth(0).locator('.board-plan-result')).toContainText('ok', { timeout: 10_000 });
  await expect(rows.nth(1).locator('.board-plan-result')).toContainText('failed');
  await expect(rows.nth(2).locator('.board-plan-result')).toContainText('not run');
  await saveArtifactScreenshot(page, 'board-plan-results.png');
  await expect.poll(async () => (await api.getTask(keep.id)).execution_stage).toBe('done');
  expect((await api.getTask(skip.id)).execution_stage).toBe('in_review');
  await expect(page.locator('.board-plan-sign-btn')).toHaveCount(0);
});
