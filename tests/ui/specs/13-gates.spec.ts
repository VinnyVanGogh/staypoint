import { randomUUID } from 'node:crypto';
import { test, expect } from '../fixtures';
import type { Locator, Page, Request, Response } from '@playwright/test';

// STA-587: Gates page — security gate requests visible and decidable in the UI.

/** Suffix that keeps each test's gate row unique across repeats and reruns. */
function uniq(): string {
  return randomUUID().slice(0, 8);
}

const isDecide = (r: Request | Response) => {
  const req = 'request' in r ? r.request() : r;
  return req.method() === 'POST' && /\/api\/security\/gate-requests\/[^/]+\/decide$/.test(req.url());
};

const isGateList = (status: string) => (url: URL) =>
  url.pathname === '/api/security/gate-requests' && url.searchParams.get('status') === status;

/**
 * Clicks a decision button and waits for the decide call to land. The click
 * goes through withBoardWebAuthn (challenge + virtual authenticator) first, and
 * any failure there only shows an alert, which Playwright dismisses; asserting
 * on the response makes that failure visible instead of a missing-row timeout.
 */
async function decide(page: Page, row: Locator, button: '.gate-approve-btn' | '.gate-deny-btn') {
  const decided = page.waitForResponse(isDecide);
  await row.locator(button).click();
  const res = await decided;
  expect(res.status(), `decide: ${await res.text()}`).toBe(200);
}

test.describe('Gates page', () => {
  test('sidebar badge shows pending count after a gate request is created', async ({ api, page }) => {
    // Start fresh: ensure connection is live
    await page.goto('/gates');
    await expect(page.locator('#conn-badge')).toHaveText('live');

    // Badge hidden when no pending requests exist for this session
    // (other specs may have pending gates; we just check the page loads)
    const badge = page.locator('#gates-badge');
    // Creating a new gate request should increment the badge
    const before = await badge.isVisible() ? parseInt(await badge.textContent() || '0') : 0;

    await api.createGateRequest(`git push origin main ${uniq()}`, ['Red-tier: push to main'], 'run-sta-587-test');

    // Badge should appear/increment via SSE
    await expect(badge).toBeVisible();
    await expect.poll(async () => parseInt(await badge.textContent() || '0')).toBeGreaterThan(before);
  });

  test('pending gate request shows in the Gates page table', async ({ api, page }) => {
    const cmd = `git push origin main --force-with-lease ${uniq()}`;
    await api.createGateRequest(cmd, ['Red-tier: force push'], 'run-sta-587-table');

    await page.goto('/gates');
    await expect(page.locator('#conn-badge')).toHaveText('live');

    const container = page.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible();

    // Approve and Deny buttons present for pending item
    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await expect(matchRow.locator('.gate-approve-btn')).toBeVisible();
    await expect(matchRow.locator('.gate-deny-btn')).toBeVisible();
  });

  test('Deny button records decision and actor in security_gate_audit_log', async ({ api, boardPage }) => {
    const cmd = `git push origin main ${uniq()}`;
    const gr = await api.createGateRequest(cmd, ['Red-tier: push to main'], 'run-sta-587-deny');

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');

    const container = boardPage.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible();

    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await decide(boardPage, matchRow, '.gate-deny-btn');

    // Row should update to 'denied' status
    await expect(matchRow.locator('.gate-status-denied')).toBeVisible();

    // Verify decision persisted in security_gate_requests
    const list = await api.listGateRequests('denied');
    const found = list.gate_requests.find(r => r.id === gr.id);
    expect(found).toBeTruthy();
    expect(found?.status).toBe('denied');

    // Verify audit log entry was written with actor_id = "board"
    const audit = await api.listGateAuditLog(gr.id);
    const entry = audit.audit_log.find(e => e.event_type === 'security_gate_decided');
    expect(entry).toBeTruthy();
    expect(entry?.actor_id).toBe('board');
    expect(entry?.from_status).toBe('pending');
    expect(entry?.to_status).toBe('denied');
  });

  test('Approve button records decision', async ({ api, boardPage }) => {
    const cmd = `gh pr merge 99 --merge ${uniq()}`;
    const gr = await api.createGateRequest(cmd, ['Red-tier: board API call'], 'run-sta-587-approve');

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');

    const container = boardPage.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible();

    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await decide(boardPage, matchRow, '.gate-approve-btn');

    await expect(matchRow.locator('.gate-status-approved')).toBeVisible();

    const list = await api.listGateRequests('approved');
    const found = list.gate_requests.find(r => r.id === gr.id);
    expect(found?.status).toBe('approved');
  });

  // STA-651: the daemon publishes security_gate_decided over SSE before it
  // writes the decide response. When the SSE handler runs first it re-renders
  // with the old 'pending' filter, and decideGate then re-renders with 'all'.
  // If the 'pending' list comes back last it used to replace the table and the
  // decided row vanished. This forces that ordering instead of waiting for CI
  // timing to hit it.
  test('a late pending-list response does not wipe the decided row', async ({ api, boardPage }) => {
    const cmd = `git push origin main ${uniq()}`;
    await api.createGateRequest(cmd, ['Red-tier: push to main'], 'run-sta-651-race');

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');

    const container = boardPage.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible();
    const matchRow = container.locator('.gates-table tbody tr').filter({ hasText: cmd });

    // Hold the decide response until the SSE event has triggered its render.
    const sseRender = boardPage.waitForRequest((r) => isGateList('pending')(new URL(r.url())));
    await boardPage.route(/\/api\/security\/gate-requests\/[^/]+\/decide$/, async (route) => {
      const res = await route.fetch();
      await sseRender;
      await route.fulfill({ response: res });
    });
    // Serve the 'all' list straight away and hold every 'pending' list until
    // after it, so the SSE render's response is the one that arrives last.
    let allServed!: () => void;
    const allServedP = new Promise<void>((r) => { allServed = r; });
    await boardPage.route(isGateList('all'), async (route) => {
      await route.fulfill({ response: await route.fetch() });
      allServed();
    });
    await boardPage.route(isGateList('pending'), async (route) => {
      const res = await route.fetch();
      await allServedP;
      await route.fulfill({ response: res });
    });

    // Three 'pending' lists follow the click: the SSE handler's badge refresh
    // and render, then decideGate's own badge refresh after its render. Once
    // the last one has finished, the stale render has had its chance to land.
    let pendingFinished = 0;
    const staleDone = boardPage.waitForEvent('requestfinished', (r) =>
      isGateList('pending')(new URL(r.url())) && ++pendingFinished >= 3);
    await decide(boardPage, matchRow, '.gate-deny-btn');
    await staleDone;

    await expect(matchRow.locator('.gate-status-denied')).toBeVisible();
    await expect(matchRow).toHaveCount(1);
  });

  test('status filter switches between pending and decided views', async ({ api, page }) => {
    // Create one request and decide it via API to have something in all buckets
    const cmd = `echo decided ${uniq()}`;
    await api.createGateRequest(cmd, ['test']);

    await page.goto('/gates');
    await expect(page.locator('#conn-badge')).toHaveText('live');

    const filter = page.locator('#gates-status-filter');
    await filter.selectOption('all');
    // After switching to 'all', the container should reload
    await expect(page.locator('#gates-container')).not.toContainText('Loading');
    // Should show some rows
    await expect(page.locator('.gates-table')).toBeVisible();
  });
});
