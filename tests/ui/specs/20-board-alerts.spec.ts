import { test, expect } from '../fixtures';

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';

async function seedAlert(request: any, alert: {
  kind: string;
  severity: 'critical' | 'warning' | 'info';
  title: string;
  message: string;
  dedupe_key?: string;
}) {
  const res = await request.post('/api/board/alerts/test/seed', {
    headers: {
      Authorization: `Bearer ${TOKEN}`,
      'Content-Type': 'application/json',
    },
    data: alert,
  });
  expect(res.ok()).toBeTruthy();
  return await res.json();
}

test.describe('Board alerts', () => {
  test('alert appears via SSE without reload, dedupe bumps count, and dismiss removes it', async ({ boardPage: page, request }) => {
    // 1. Initial page is loaded at / with Board session fixture.
    await expect(page.locator('#conn-badge')).toHaveText('live', { timeout: 10_000 });

    // 2. Seed through the test-only POST /api/board/alerts/test/seed.
    const dedupeKey = `breaker-test-${Date.now()}`;
    await seedAlert(request, {
      kind: 'circuit_breaker_tripped',
      severity: 'critical',
      title: 'Circuit Breaker Tripped',
      message: 'Agent paused in workspace',
      dedupe_key: dedupeKey,
    });

    // 3. The banner [data-testid=board-alert] with the right data-severity appears without a page reload, via SSE.
    const alertLocator = page.locator('[data-testid="board-alert"]').filter({ hasText: 'Circuit Breaker Tripped' });
    await expect(alertLocator).toBeVisible({ timeout: 10_000 });
    await expect(alertLocator).toHaveAttribute('data-severity', 'critical');
    await expect(alertLocator).toContainText('Agent paused in workspace');

    // 4. Seeding the same dedupe_key again shows ×2.
    await seedAlert(request, {
      kind: 'circuit_breaker_tripped',
      severity: 'critical',
      title: 'Circuit Breaker Tripped',
      message: 'Agent paused in workspace updated',
      dedupe_key: dedupeKey,
    });

    await expect(alertLocator).toContainText('×2', { timeout: 10_000 });

    // 5. Dismiss with the Board-session fixture (boardPage, used by existing Board specs).
    // Always assert visibility before click().
    const dismissBtn = alertLocator.locator('[data-testid="board-alert-dismiss"]');
    await expect(dismissBtn).toBeVisible();
    await dismissBtn.click();

    // The alert disappears and stays gone after a reload.
    await expect(alertLocator).toBeHidden({ timeout: 10_000 });

    await page.reload();
    await expect(page.locator('#conn-badge')).toHaveText('live', { timeout: 10_000 });
    await expect(alertLocator).toBeHidden();
  });

  test('seed 5 alerts: only 3 are visible, plus a +2 more toggle; critical alerts come first', async ({ boardPage: page, request }) => {
    await expect(page.locator('#conn-badge')).toHaveText('live', { timeout: 10_000 });

    const nonce = Date.now().toString(36);
    // Seed 5 alerts: 2 info, 2 warning, 1 critical.
    // Critical alert is seeded last to ensure UI sorts critical alerts first rather than relying on insertion order.
    await seedAlert(request, {
      kind: 'quota_ready',
      severity: 'info',
      title: `Info 1 ${nonce}`,
      message: 'Quota ready 1',
      dedupe_key: `info-1-${nonce}`,
    });
    await seedAlert(request, {
      kind: 'quota_ready',
      severity: 'info',
      title: `Info 2 ${nonce}`,
      message: 'Quota ready 2',
      dedupe_key: `info-2-${nonce}`,
    });
    await seedAlert(request, {
      kind: 'quota_warning',
      severity: 'warning',
      title: `Warn 1 ${nonce}`,
      message: 'Quota warning 1',
      dedupe_key: `warn-1-${nonce}`,
    });
    await seedAlert(request, {
      kind: 'quota_warning',
      severity: 'warning',
      title: `Warn 2 ${nonce}`,
      message: 'Quota warning 2',
      dedupe_key: `warn-2-${nonce}`,
    });
    await seedAlert(request, {
      kind: 'circuit_breaker_tripped',
      severity: 'critical',
      title: `Critical 1 ${nonce}`,
      message: 'Breaker tripped',
      dedupe_key: `crit-1-${nonce}`,
    });

    // Seed 5 alerts: only 3 are visible, plus a +2 more toggle.
    const visibleAlerts = page.locator('[data-testid="board-alert"]:visible');
    await expect(visibleAlerts).toHaveCount(3, { timeout: 10_000 });

    // Critical alerts come first.
    await expect(visibleAlerts.first()).toHaveAttribute('data-severity', 'critical');
    await expect(visibleAlerts.first()).toContainText(`Critical 1 ${nonce}`);

    // +2 more toggle is visible.
    const moreToggle = page.locator('[data-testid="board-alerts"]').getByText('+2 more');
    await expect(moreToggle).toBeVisible();

    // Clicking the toggle shows the remaining alerts. Always assert visibility before click().
    await moreToggle.click();
    await expect(page.locator('[data-testid="board-alert"]:visible')).toHaveCount(5, { timeout: 5_000 });
  });
});
