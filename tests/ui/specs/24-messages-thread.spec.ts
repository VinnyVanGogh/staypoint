/**
 * spec 24: the task page's Messages thread (task-d13978fc)
 *
 * Open, the thread fills the timeline column, from just under the stats row
 * down to the composer, and covers the timeline only: the right panel stays
 * visible and clickable. Only what the Board wrote goes right (chat-msg-user);
 * everything else, including notices the daemon writes as author 'board' on
 * the Board's behalf, goes left (chat-msg-agent).
 */

import type { Locator, Page } from '@playwright/test';
import { test, expect, gotoTaskPage, openTaskPanelTab, saveArtifactScreenshot } from '../fixtures';

const WIDE = { width: 1512, height: 900 };
const NARROW = { width: 800, height: 900 };

async function box(l: Locator) {
  const b = await l.boundingBox();
  expect(b, 'element has a layout box').not.toBeNull();
  return b!;
}

async function openThread(page: Page) {
  const toggle = page.locator('#page-chat-toggle');
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  const thread = page.locator('#page-chat-messages');
  await expect(thread).toBeVisible();
  return thread;
}

test('open thread fills the timeline column only; the right panel stays usable', async ({ page, api }) => {
  await page.setViewportSize(WIDE);
  const task = await api.createTask('Thread size');
  await api.addComment(task.id, 'agent says hello', 'harness');
  await gotoTaskPage(page, task);

  const content = page.locator('#task-page-content');
  const thread = await openThread(page);

  const t = await box(thread);
  const stats = await box(content.locator('.task-page-stats'));
  const timeline = await box(content.locator('.task-page-timeline'));
  const dock = await box(page.locator('#page-chat-section'));

  // Top: just under the stats row, where the timeline column starts.
  // Bottom: the composer's top edge.
  expect(t.y).toBeGreaterThanOrEqual(stats.y + stats.height - 1);
  expect(t.y - (stats.y + stats.height)).toBeLessThan(16);
  expect(Math.abs(t.y - timeline.y)).toBeLessThan(2);
  expect(Math.abs(t.y + t.height - dock.y)).toBeLessThan(2);
  // Covers the timeline column and nothing to its right.
  expect(Math.abs(t.x - timeline.x)).toBeLessThan(2);
  expect(t.x + t.width).toBeLessThanOrEqual(timeline.x + timeline.width + 1);
  expect(t.height).toBeGreaterThan(WIDE.height / 2);

  // The right panel is still clickable with the thread open.
  await openTaskPanelTab(page, 'Brief');
  await openTaskPanelTab(page, 'Diff');
  await expect(thread).toBeVisible();

  // Closed: only the composer row.
  await page.locator('#page-chat-toggle').click();
  await expect(thread).toBeHidden();
});

test('Board messages go right; agent, harness, summary and Board notices go left', async ({ page, api }) => {
  await page.setViewportSize(WIDE);
  const task = await api.createTask('Thread sides');
  await api.addComment(task.id, 'HARNESS-MSG preflight done', 'harness');
  await api.addComment(task.id, 'SUMMARY-MSG all tests pass', 'agent-summary');
  await api.addComment(task.id, 'CLI-MSG note from an agent shell', 'someone');
  await api.addComment(task.id, 'BOARD-MSG please also fix the tests', 'board');
  await api.addComment(task.id, 'USER-MSG typed in the web UI', 'user');
  await api.addComment(task.id, 'Board approved held action gr-1: ls; perform it now. NOTICE-MSG', 'board');
  await gotoTaskPage(page, task);
  const thread = await openThread(page);

  const row = (marker: string) => thread.locator('.chat-msg').filter({ hasText: marker });
  for (const m of ['HARNESS-MSG', 'SUMMARY-MSG', 'CLI-MSG', 'NOTICE-MSG']) {
    await expect(row(m)).toHaveClass(/\bchat-msg-agent\b/);
  }
  await expect(row('NOTICE-MSG')).toHaveClass(/\bchat-msg-notice\b/);
  for (const m of ['BOARD-MSG', 'USER-MSG']) {
    await expect(row(m)).toHaveClass(/\bchat-msg-user\b/);
    await expect(row(m).locator('.chat-msg-meta')).toContainText('You');
  }

  // Left and right are real sides of the thread, not just class names.
  const left = await box(row('HARNESS-MSG'));
  const right = await box(row('BOARD-MSG'));
  const t = await box(thread);
  expect(left.x - t.x).toBeLessThan(40);
  expect(t.x + t.width - (right.x + right.width)).toBeLessThan(40);
  await saveArtifactScreenshot(page, 'task-page-messages-thread-1512x900.png');
});

test('narrow screen: the open thread may take the full width', async ({ page, api }) => {
  await page.setViewportSize(NARROW);
  const task = await api.createTask('Thread narrow');
  await api.addComment(task.id, 'NARROW-MSG hello', 'harness');
  await gotoTaskPage(page, task);
  const thread = await openThread(page);
  await expect(thread.getByText('NARROW-MSG hello')).toBeVisible();
  const t = await box(thread);
  expect(t.x + t.width).toBeLessThanOrEqual(NARROW.width + 1);
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
