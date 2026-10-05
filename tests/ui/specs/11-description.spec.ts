import { test, expect, gotoTaskPage, openTaskPanelTab } from '../fixtures';

test.describe('task description', () => {
  test('new-task form sends description, task page shows it', async ({ page, api }) => {
    // Open the New Task modal
    await page.goto('/task-status');
    await page.getByRole('button', { name: /\+ New Task/i }).click();

    const modal = page.locator('#create-task-modal');
    await expect(modal).toBeVisible();

    // Fill required name field and the description textarea
    const taskName = `Desc test ${Date.now().toString(36)}`;
    await modal.locator('#ct-name').fill(taskName);
    await modal.locator('#ct-description').fill('**Initial description** for STA-544');

    // Submit
    await modal.locator('#create-task-submit').click();

    // Wait for navigation to new task page
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(taskName, { timeout: 20_000 });

    // Description visible on task page
    const descSection = page.locator('.task-page-section').filter({ hasText: 'Description' });
    await expect(descSection).toContainText('Initial description');
  });

  test('task page description is editable and persists across reload', async ({ page, api }) => {
    const task = await api.createTask('EditDesc', { description: 'Original desc' });
    await gotoTaskPage(page, task);
    await openTaskPanelTab(page, 'Brief');

    const descSection = page.locator('.task-page-section').filter({ hasText: 'Description' }).first();

    // Initial description renders
    await expect(descSection).toContainText('Original desc');

    // Click the edit button
    const editBtn = descSection.locator('.desc-edit-btn');
    await editBtn.click();

    // Edit form appears, textarea contains current value
    const textarea = descSection.locator('.desc-textarea');
    await expect(textarea).toBeVisible();
    await expect(textarea).toHaveValue('Original desc');

    // Update the description
    await textarea.fill('Updated description content');

    // Save
    await descSection.locator('.desc-save-btn').click();

    // Edit form hides, updated text shows
    await expect(textarea).not.toBeVisible();
    await expect(descSection).toContainText('Updated description content');

    // Reload — change must persist
    await page.reload();
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    const descAfterReload = page.locator('.task-page-section').filter({ hasText: 'Description' }).first();
    await expect(descAfterReload).toContainText('Updated description content');
  });

  test('cancel edit restores original view without saving', async ({ page, api }) => {
    const task = await api.createTask('CancelDesc', { description: 'Keep this' });
    await gotoTaskPage(page, task);
    await openTaskPanelTab(page, 'Brief');

    const descSection = page.locator('.task-page-section').filter({ hasText: 'Description' }).first();
    await descSection.locator('.desc-edit-btn').click();

    const textarea = descSection.locator('.desc-textarea');
    await textarea.fill('Should not be saved');

    await descSection.locator('.desc-cancel-btn').click();

    // Edit form gone, original text still there
    await expect(textarea).not.toBeVisible();
    await expect(descSection).toContainText('Keep this');
    await expect(descSection).not.toContainText('Should not be saved');
  });
});
