import { test, expect } from '../fixtures';
import type { Page } from '@playwright/test';

// Eight tabs on the same daemon (task-53fcbcff). A stream per tab used up the
// six HTTP/1.1 connections Chromium allows per host, so later tabs sat on
// "connecting" and every fetch queued forever. Now one tab holds the stream
// and relays it, so every tab loads and stays live.

const TABS = 8;

async function leaderOf(pages: Page[]): Promise<Page[]> {
  const roles = await Promise.all(pages.map(p => p.evaluate('typeof liveStream === "undefined" || !liveStream ? null : liveStream.role()')));
  return pages.filter((_, i) => roles[i] === 'leader');
}

test('eight tabs all load, stay live, and keep live updates after the leader closes', async ({ page, api }) => {
  test.setTimeout(120_000);
  const context = page.context();
  await page.goto('/task-status');
  const pages = [page];
  for (let i = 1; i < TABS; i++) {
    const p = await context.newPage();
    await p.goto(i % 2 ? '/task-status' : '/gates');
    pages.push(p);
  }

  for (const p of pages) {
    await expect(p.locator('#conn-badge')).toHaveText('live', { timeout: 20_000 });
  }
  expect(await leaderOf(pages)).toHaveLength(1);

  // A ninth tab still gets its API calls through.
  const extra = await context.newPage();
  await extra.goto('/task-status');
  const seeded = await api.createTask('Many tabs seeded');
  await expect(extra.locator('#ts-task-table').getByText(seeded.name)).toBeVisible({ timeout: 20_000 });
  await expect(extra.locator('#conn-badge')).toHaveText('live');
  pages.push(extra);

  const listTabs = () => pages.filter(p => !p.isClosed() && new URL(p.url()).pathname === '/task-status');

  const live1 = await api.createTask('Many tabs live');
  for (const p of listTabs()) {
    await expect(p.locator('#ts-task-table').getByText(live1.name)).toBeVisible({ timeout: 20_000 });
  }

  // Close the tab holding the stream; another takes over without a reload.
  const [leader] = await leaderOf(pages);
  await leader.close();
  const rest = pages.filter(p => !p.isClosed());
  await expect.poll(async () => (await leaderOf(rest)).length, { timeout: 20_000 }).toBe(1);

  const live2 = await api.createTask('Many tabs handoff');
  for (const p of listTabs()) {
    await expect(p.locator('#ts-task-table').getByText(live2.name)).toBeVisible({ timeout: 20_000 });
    await expect(p.locator('#conn-badge')).toHaveText('live');
  }
});
