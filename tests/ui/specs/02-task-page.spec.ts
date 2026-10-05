import { test, expect, gotoTaskPage, openTaskPanelTab, taskPagePath } from '../fixtures';

test.describe('task detail full page', () => {
  test('renders the task and survives a reload of the deep link', async ({ page, api }) => {
    const task = await api.createTask('Deep link');
    const path = taskPagePath(task);

    await gotoTaskPage(page, task);
    const content = page.locator('#task-page-content');
    await openTaskPanelTab(page, 'Review');
    await expect(content.getByText('Internal ID')).toBeVisible();
    await expect(content.locator('.panel-field-value', { hasText: task.id })).toBeVisible();

    await page.reload();
    await expect(content.locator('.task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    expect(new URL(page.url()).pathname).toBe(path);
    await expect(page.locator('#view-task-page')).toHaveClass(/active/);
  });

  test('opening a task from the list lands on its detail', async ({ page, api }) => {
    const task = await api.createTask('Open from list');
    await page.goto('/task-status');
    await page.locator('#ts-task-table').getByText(task.name).click();
    await expect(page.locator('#detail-panel')).toBeVisible();
    await expect(page.locator('#detail-panel')).toContainText(task.name);
  });
});
