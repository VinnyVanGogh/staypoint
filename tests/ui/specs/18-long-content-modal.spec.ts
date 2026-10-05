/**
 * spec 18: shared long-content dialog and the composer's Messages toggle (STA-643)
 *
 * Spec 15 step 5 covers each trigger opening its full content and Escape
 * closing it. This covers the rest of the dialog contract (focus moves in and
 * comes back, backdrop click closes, Escape does not close the page behind)
 * and the "Messages (n)" toggle that keeps the thread out of the pinned dock.
 */

import { test, expect, gotoTaskPage, openTaskPanelTab } from '../fixtures';

test('brief dialog takes focus, returns it to its trigger, and closes on backdrop click', async ({ page, api }) => {
  const task = await api.createTask('Modal focus', { description: 'Brief body\n\nBRIEF-FOCUS-MARKER' });
  await gotoTaskPage(page, task);
  await openTaskPanelTab(page, 'Brief');

  const trigger = page.locator('[data-modal-trigger="brief"]');
  await expect(trigger).toBeVisible();
  await trigger.click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toHaveCount(1);
  await expect(dialog).toContainText('BRIEF-FOCUS-MARKER');
  await expect(dialog).toHaveAttribute('aria-modal', 'true');
  await expect(dialog.getByRole('button', { name: 'Close' })).toBeFocused();

  // Escape closes only the dialog; the task page stays open and focus returns.
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.locator('#task-page-content .task-page-header')).toBeVisible();
  await expect(trigger).toBeFocused();

  // Backdrop click closes too.
  await trigger.click();
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await page.locator('.content-modal').click({ position: { x: 5, y: 5 } });
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test('thread is behind a Messages (n) toggle in the composer row', async ({ page, api }) => {
  const task = await api.createTask('Messages toggle');
  await api.addComment(task.id, 'harness preflight message');
  await gotoTaskPage(page, task);

  const dock = page.locator('#page-chat-section');
  const toggle = dock.locator('.task-page-composer #page-chat-toggle');
  await expect(toggle).toHaveText('Messages (1)');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(dock.locator('#page-chat-messages')).toBeHidden();

  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(dock.getByText('harness preflight message')).toBeVisible();

  await toggle.click();
  await expect(dock.locator('#page-chat-messages')).toBeHidden();
});
