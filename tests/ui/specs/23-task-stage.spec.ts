import * as os from 'node:os';
import { test, expect, gotoTaskPage, clearTaskRepo } from '../fixtures';

// task-40f0a2f0: the Board changes a task's stage from the task page. Agent
// tasks land in backlog and only the Board can move them out.

test.describe('Task stage control', () => {
  test('Board moves an agent-created backlog task to In review; the agent token still gets 403', async ({ api, boardPage }) => {
    const task = await api.createAgentTask('stage to review', { repo_path: os.tmpdir() });
    expect(task.execution_stage).toBe('backlog');

    const refused = await api.setStageAsAgent(task.id, 'in_review');
    expect(refused.status).toBe(403);
    expect(refused.body.error).toBe('board_session_required');
    expect((await api.getTask(task.id)).execution_stage).toBe('backlog');

    await gotoTaskPage(boardPage, task);
    const select = boardPage.locator('#task-page-content .task-stage-select');
    await expect(select).toBeVisible();
    // The current stage is not offered; Cancel has its own button.
    await expect(select.locator('option[value="backlog"]')).toHaveCount(0);
    await expect(select.locator('option[value="cancelled"]')).toHaveCount(0);

    const posted = boardPage.waitForResponse((r) => r.url().endsWith(`/api/tasks/${task.id}/stage`) && r.request().method() === 'POST');
    await select.selectOption('in_review');
    const res = await posted;
    expect(res.status(), await res.text()).toBe(200);
    expect(JSON.parse(res.request().postData() || '{}')).toEqual({ stage: 'in_review' });
    await expect.poll(async () => (await api.getTask(task.id)).execution_stage).toBe('in_review');
    await expect(boardPage.locator('#task-page-content .task-stage-select option[value="in_review"]')).toHaveCount(0);
  });

  test('a refused move shows the server message and leaves the stage alone', async ({ api, boardPage }) => {
    // No repo: the server refuses any runnable stage until one is set.
    const task = await api.createAgentTask('stage refused');
    expect(task.execution_stage).toBe('backlog');
    clearTaskRepo(task.id);
    expect((await api.getTask(task.id)).repo_path || '').toBe('');

    await gotoTaskPage(boardPage, task);
    const content = boardPage.locator('#task-page-content');
    const posted = boardPage.waitForResponse((r) => r.url().endsWith(`/api/tasks/${task.id}/stage`) && r.request().method() === 'POST');
    await content.locator('.task-stage-select').selectOption('todo');
    expect((await posted).status()).toBe(409);
    await expect(content.locator('.task-stage-error')).toBeVisible();
    await expect(content.locator('.task-stage-error')).toContainText('set-repo');
    expect((await api.getTask(task.id)).execution_stage).toBe('backlog');
    // The control is usable again for another try.
    await expect(content.locator('.task-stage-select')).toBeEnabled();
  });

  test('a page without a Board session is refused with the server error', async ({ api, page }) => {
    const task = await api.createAgentTask('stage no board', { repo_path: os.tmpdir() });
    await gotoTaskPage(page, task);
    const content = page.locator('#task-page-content');
    await content.locator('.task-stage-select').selectOption('in_review');
    await expect(content.locator('.task-stage-error')).toContainText('board_session_required');
    expect((await api.getTask(task.id)).execution_stage).toBe('backlog');
  });

  test('Trust on a backlog task offers "Move to todo and trust" under one Touch ID', async ({ api, boardPage }) => {
    const task = await api.createAgentTask('trust from backlog', { repo_path: os.tmpdir() });
    await gotoTaskPage(boardPage, task);

    const section = boardPage.locator('.task-trust-section');
    const btn = section.locator('.trust-create-btn');
    await expect(btn).toHaveText('Move to todo and trust (Touch ID)');
    let dialogs = 0;
    boardPage.on('dialog', (d) => { dialogs++; void d.dismiss(); });
    const created = boardPage.waitForResponse((r) => r.url().endsWith(`/api/tasks/${task.id}/trust`) && r.request().method() === 'POST');
    await btn.click();
    const res = await created;
    expect(res.status(), await res.text()).toBe(201);
    expect(JSON.parse(res.request().postData() || '{}')).toEqual({ preset: 'overnight', move_to_todo: true });
    expect(dialogs, 'no dead-end alert').toBe(0);
    expect((await api.getTask(task.id)).execution_stage).toBe('todo');
    await expect(section.locator('.trust-banner-text')).toContainText('Trusted until');
  });
});
