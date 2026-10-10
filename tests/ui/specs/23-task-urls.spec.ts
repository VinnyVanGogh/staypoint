import { test, expect } from '../fixtures';

// task-eb38c245: the task page lives at /STA-123/<slug>. The slug is
// decoration (lookup is by number), old /tasks/... links 301 there, and the
// project shows on the page instead of in the URL.

test.describe('readable task URLs', () => {
  test('unknown reference is a 404 with "Task not found"', async ({ page }) => {
    const res = await page.goto('/STA-987654321/nothing-here');
    expect(res?.status()).toBe(404);
    await expect(page.locator('#task-page-content')).toContainText('Task not found', { timeout: 20_000 });
  });

  test('old URL and wrong slug land on the canonical URL', async ({ page, api }) => {
    const task = await api.createTask('Readable URL');
    expect(task.identifier).toMatch(/^STA-\d+$/);
    const canonical = `/${task.identifier}/${task.slug}`;
    const content = page.locator('#task-page-content');

    for (const from of [
      `/tasks/STA/ui-e2e/${task.id}`,
      `/tasks/${task.id}`,
      `/${task.identifier}/a-wrong-slug`,
      `/${task.identifier!.toLowerCase()}`,
    ]) {
      await page.goto(from);
      await expect(content.locator('.task-page-title')).toHaveText(task.name, { timeout: 20_000 });
      expect(new URL(page.url()).pathname, `from ${from}`).toBe(canonical);
    }

    // The bare reference serves the page and settles on the slug form.
    await page.goto(`/${task.identifier}`);
    await expect(content.locator('.task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    await expect(page).toHaveURL(new RegExp(`${canonical}$`));
    // Project on the page, not in the URL.
    await expect(content.locator('.task-page-breadcrumb')).toContainText('ui-e2e');
  });
});
