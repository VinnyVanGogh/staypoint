import { test, expect, gotoTaskPage } from '../fixtures';

test('a comment typed on the task page shows in the thread and is stored', async ({ page, api }) => {
  const task = await api.createTask('Comments');
  await api.addComment(task.id, 'seeded comment from the API');

  await gotoTaskPage(page, task);
  const content = page.locator('#task-page-content');
  // The thread sits behind the "Messages (n)" toggle in the composer row (STA-643).
  await content.locator('#page-chat-toggle').click();
  await expect(content.getByText('seeded comment from the API').first()).toBeVisible();

  const body = `typed in the browser ${Date.now()}`;
  await content.locator('#page-chat-section textarea').fill(body);
  await content.locator('#page-chat-section').getByRole('button', { name: 'Send' }).click();

  await expect(page.locator('#page-chat-messages').getByText(body)).toBeVisible();
  await expect.poll(async () => (await api.listComments(task.id)).map(c => c.message || c.body)).toContain(body);

  await page.reload();
  await expect(content.getByText(body).first()).toBeVisible();
});
