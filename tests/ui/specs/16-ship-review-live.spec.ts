/**
 * spec 16: Ship review card on a live_credentials project (STA-727)
 *
 * A project whose previews run against production credentials is flagged
 * live_credentials; GET /ship-review exposes the flag on the card. On such a
 * card the UI:
 *   - shows a red banner `.ship-review-live-banner` with exactly
 *     "LIVE PRODUCTION DATA. Actions in this preview are real." above the
 *     Preview row;
 *   - marks the Start dev server button (`.ship-review-start-dev-btn`, shown on
 *     a pending card with no dev server yet) with "LIVE";
 *   - asks for an inline confirm (`.ship-review-live-confirm`, no browser
 *     dialog) with the full warning before every Start dev server / Restart;
 *     Cancel sends nothing, the confirm button sends start-dev with the Board
 *     passkey headers and {"confirm_live": true}.
 * Non-live cards keep today's Restart: one click, plain start-dev, no confirm.
 *
 * UI-state only: the card GET and start-dev responses are route-faked, so the
 * server never decides anything here. The server-side gate (403 / 409 /
 * audit) is covered by internal/server/handlers_ship_review_live_test.go.
 */

import { test, expect, gotoTaskPage } from '../fixtures';
import type { Page, Request } from '@playwright/test';

const LIVE_WARNING = 'LIVE PRODUCTION DATA. Actions in this preview are real.';
const DEV_URL = 'http://127.0.0.1:3999/';

function syntheticCard(taskId: string, opts: { live: boolean; devUrl?: string }) {
  return {
    id: `live-card-${taskId}`,
    task_id: taskId,
    branch: `staypoint/${taskId}`,
    head_sha: 'deadbeef12345678abcdef00',
    test_steps: ['Open the preview', 'Check the dashboard loads'],
    dev_url: opts.devUrl ?? '',
    dev_pid: 0,
    dev_state: '',
    status: 'pending',
    files_changed: ['src/app.ts'],
    check_runs: [],
    live_credentials: opts.live,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
}

// serveCard fakes GET /ship-review with the synthetic card and records every
// start-dev request, answering it 202 like the real handler.
async function serveCard(page: Page, taskId: string, card: Record<string, unknown>) {
  const startDev: Request[] = [];
  const id = encodeURIComponent(taskId);
  await page.route(`**/api/tasks/${id}/ship-review`, async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(card) });
  });
  await page.route(`**/api/tasks/${id}/ship-review/start-dev`, async (route) => {
    startDev.push(route.request());
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({ dev_url: DEV_URL, status: 'starting' }),
    });
  });
  return startDev;
}

function noBrowserDialogs(page: Page) {
  page.on('dialog', async (d) => {
    await d.dismiss();
    throw new Error(`Unexpected browser dialog (${d.type()}): ${d.message()}`);
  });
}

test.describe('ship review card: live_credentials project', () => {
  test('banner text is exact and sits above the Preview row', async ({ page, api }) => {
    const task = await api.createTask('sr-live-banner');
    await serveCard(page, task.id, syntheticCard(task.id, { live: true, devUrl: DEV_URL }));

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    const banner = card.locator('.ship-review-live-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toHaveText(LIVE_WARNING);

    const preview = card.locator('.ship-review-row', { hasText: 'Preview' });
    await expect(preview).toBeVisible();
    const bannerBox = await banner.boundingBox();
    const previewBox = await preview.boundingBox();
    expect(bannerBox && previewBox, 'banner and Preview row both laid out').toBeTruthy();
    expect(bannerBox!.y + bannerBox!.height).toBeLessThanOrEqual(previewBox!.y + 1);

    // The banner is red: its background (or border) carries a dominant red channel.
    const red = await banner.evaluate((el) => {
      const s = getComputedStyle(el);
      const rgb = (c: string) => (c.match(/\d+(\.\d+)?/g) || []).map(Number);
      const isRed = (c: string) => { const [r, g, b, a] = rgb(c); return a !== 0 && r >= 150 && r > g + 60 && r > b + 60; };
      return isRed(s.backgroundColor) || isRed(s.borderLeftColor) || isRed(s.borderTopColor);
    });
    expect(red, 'live banner must be red').toBe(true);
  });

  test('Start dev server button is marked LIVE', async ({ page, api }) => {
    const task = await api.createTask('sr-live-start-btn');
    await serveCard(page, task.id, syntheticCard(task.id, { live: true }));

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });
    await expect(card.locator('.ship-review-live-banner')).toHaveText(LIVE_WARNING);

    const startBtn = card.locator('.ship-review-start-dev-btn');
    await expect(startBtn).toBeVisible();
    await expect(startBtn).toContainText('Start dev server');
    await expect(startBtn).toContainText('LIVE');
  });

  test('Start dev server asks for confirm with the full warning; Cancel sends no start-dev', async ({ boardPage: page, api }) => {
    const task = await api.createTask('sr-live-cancel');
    const startDev = await serveCard(page, task.id, syntheticCard(task.id, { live: true }));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    const startBtn = card.locator('.ship-review-start-dev-btn');
    await expect(startBtn, 'live card must offer Start dev server').toBeVisible();
    await startBtn.click();
    const confirmForm = card.locator('.ship-review-live-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });
    await expect(confirmForm).toContainText(LIVE_WARNING);
    expect(startDev.length, 'start-dev must wait for the confirm').toBe(0);

    await confirmForm.getByRole('button', { name: /^Cancel$/i }).click();
    await expect(confirmForm).toBeHidden({ timeout: 2_000 });
    // Give a stray request time to show up before asserting none was sent.
    await page.waitForTimeout(500);
    expect(startDev.length, 'Cancel must not send start-dev').toBe(0);
  });

  test('confirm sends start-dev with passkey assertion and confirm_live', async ({ boardPage: page, api }) => {
    const task = await api.createTask('sr-live-confirm');
    const startDev = await serveCard(page, task.id, syntheticCard(task.id, { live: true }));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    const startBtn = card.locator('.ship-review-start-dev-btn');
    await expect(startBtn, 'live card must offer Start dev server').toBeVisible();
    await startBtn.click();
    const confirmForm = card.locator('.ship-review-live-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });

    const challenge = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/challenge'));
    await confirmForm.getByRole('button', { name: /start/i }).click();
    await challenge;
    await expect.poll(() => startDev.length, { timeout: 5_000 }).toBe(1);

    const req = startDev[0];
    expect(req.method()).toBe('POST');
    const headers = req.headers();
    expect(headers['x-webauthn-assertion'], 'passkey assertion header').toBeTruthy();
    expect(headers['x-webauthn-session'], 'passkey session header').toBeTruthy();
    expect(req.postDataJSON()).toMatchObject({ confirm_live: true });
  });

  test('Restart on a live card also needs the confirm', async ({ boardPage: page, api }) => {
    const task = await api.createTask('sr-live-restart');
    const startDev = await serveCard(page, task.id, syntheticCard(task.id, { live: true, devUrl: DEV_URL }));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    await card.locator('.ship-review-restart-btn').click();
    const confirmForm = card.locator('.ship-review-live-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });
    await expect(confirmForm).toContainText(LIVE_WARNING);
    await page.waitForTimeout(500);
    expect(startDev.length, 'live Restart must wait for the confirm').toBe(0);
  });

  test('non-live Restart is unchanged: no banner, no confirm, plain start-dev', async ({ page, api }) => {
    const task = await api.createTask('sr-nonlive-restart');
    const startDev = await serveCard(page, task.id, syntheticCard(task.id, { live: false, devUrl: DEV_URL }));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });
    await expect(card.locator('.ship-review-row', { hasText: 'Preview' })).toBeVisible();
    await expect(card.locator('.ship-review-live-banner')).toHaveCount(0);

    const restart = card.locator('.ship-review-restart-btn');
    await expect(restart).toHaveText('↺ Restart');
    await restart.click();
    await expect.poll(() => startDev.length, { timeout: 5_000 }).toBe(1);
    await expect(card.locator('.ship-review-live-confirm')).toHaveCount(0);

    const req = startDev[0];
    expect(req.headers()['x-webauthn-assertion'], 'non-live start-dev needs no passkey').toBeFalsy();
    const body = req.postData();
    expect(body ? JSON.parse(body).confirm_live : undefined, 'non-live start-dev sends no confirm_live').toBeUndefined();
  });
});

// STA-799: the server also gates start-dev when it cannot verify the task repo
// is not a live_credentials project (a live repo path it cannot stat). GET
// /ship-review reports that as live_gate=true, live_gate_reason=unverified_path
// with live_credentials=false. The card must say so and take the same Board
// passkey + confirm path, without claiming the project is live.
const UNVERIFIED_TEXT = 'could not be verified';

function unverifiedCard(taskId: string, devUrl = '') {
  return { ...syntheticCard(taskId, { live: false, devUrl }), live_gate: true, live_gate_reason: 'unverified_path' };
}

test.describe('ship review card: start-dev gated on an unverified repo path', () => {
  test('banner says the repo path could not be verified, not LIVE PRODUCTION DATA', async ({ page, api }) => {
    const task = await api.createTask('sr-unverified-banner');
    await serveCard(page, task.id, unverifiedCard(task.id));

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    const banner = card.locator('.ship-review-live-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText(UNVERIFIED_TEXT);
    await expect(banner).not.toContainText(LIVE_WARNING);
  });

  test('Start dev server confirms with the unverified warning and sends passkey + confirm_live', async ({ boardPage: page, api }) => {
    const task = await api.createTask('sr-unverified-confirm');
    const startDev = await serveCard(page, task.id, unverifiedCard(task.id));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    const startBtn = card.locator('.ship-review-start-dev-btn');
    await expect(startBtn, 'gated card must offer Start dev server').toBeVisible();
    await startBtn.click();
    const confirmForm = card.locator('.ship-review-live-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });
    await expect(confirmForm).toContainText(UNVERIFIED_TEXT);
    await expect(confirmForm).not.toContainText(LIVE_WARNING);
    await page.waitForTimeout(500);
    expect(startDev.length, 'start-dev must wait for the confirm').toBe(0);

    const challenge = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/challenge'));
    await confirmForm.getByRole('button', { name: /start/i }).click();
    await challenge;
    await expect.poll(() => startDev.length, { timeout: 5_000 }).toBe(1);

    const headers = startDev[0].headers();
    expect(headers['x-webauthn-assertion'], 'passkey assertion header').toBeTruthy();
    expect(headers['x-webauthn-session'], 'passkey session header').toBeTruthy();
    expect(startDev[0].postDataJSON()).toMatchObject({ confirm_live: true });
  });

  test('Restart on a gated card also needs the confirm', async ({ boardPage: page, api }) => {
    const task = await api.createTask('sr-unverified-restart');
    const startDev = await serveCard(page, task.id, unverifiedCard(task.id, DEV_URL));
    noBrowserDialogs(page);

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 12_000 });

    await card.locator('.ship-review-restart-btn').click();
    const confirmForm = card.locator('.ship-review-live-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });
    await expect(confirmForm).toContainText(UNVERIFIED_TEXT);
    await page.waitForTimeout(500);
    expect(startDev.length, 'gated Restart must wait for the confirm').toBe(0);
  });
});
