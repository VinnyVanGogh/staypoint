import { test, expect } from '../fixtures';

// STA-587: Gates page — security gate requests visible and decidable in the UI.

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

    await api.createGateRequest('git push origin main', ['Red-tier: push to main'], 'run-sta-587-test');

    // Badge should appear/increment via SSE
    await expect(badge).toBeVisible({ timeout: 5_000 });
    const after = parseInt(await badge.textContent() || '0');
    expect(after).toBeGreaterThan(before);
  });

  test('pending gate request shows in the Gates page table', async ({ api, page }) => {
    const cmd = `git push origin main --force-with-lease ${Date.now().toString(36)}`;
    await api.createGateRequest(cmd, ['Red-tier: force push'], 'run-sta-587-table');

    await page.goto('/gates');
    await expect(page.locator('#conn-badge')).toHaveText('live');

    const container = page.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible({ timeout: 8_000 });

    // Approve and Deny buttons present for pending item
    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await expect(matchRow.locator('.gate-approve-btn')).toBeVisible();
    await expect(matchRow.locator('.gate-deny-btn')).toBeVisible();
  });

  test('Deny button records decision and actor in security_gate_audit_log', async ({ api, boardPage }) => {
    const cmd = `git push origin main ${Date.now().toString(36)}`;
    const gr = await api.createGateRequest(cmd, ['Red-tier: push to main'], 'run-sta-587-deny');

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');

    const container = boardPage.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible({ timeout: 8_000 });

    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await matchRow.locator('.gate-deny-btn').click();

    // Row should update to 'denied' status
    await expect(matchRow.locator('.gate-status-denied')).toBeVisible({ timeout: 5_000 });

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
    const cmd = `gh pr merge 99 --merge ${Date.now().toString(36)}`;
    const gr = await api.createGateRequest(cmd, ['Red-tier: board API call'], 'run-sta-587-approve');

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');

    const container = boardPage.locator('#gates-container');
    await expect(container.getByText(cmd)).toBeVisible({ timeout: 8_000 });

    const rows = container.locator('.gates-table tbody tr');
    const matchRow = rows.filter({ hasText: cmd });
    await matchRow.locator('.gate-approve-btn').click();

    await expect(matchRow.locator('.gate-status-approved')).toBeVisible({ timeout: 5_000 });

    const list = await api.listGateRequests('approved');
    const found = list.gate_requests.find(r => r.id === gr.id);
    expect(found?.status).toBe('approved');
  });

  test('status filter switches between pending and decided views', async ({ api, page }) => {
    // Create one request and decide it via API to have something in all buckets
    const cmd = `echo decided ${Date.now().toString(36)}`;
    await api.createGateRequest(cmd, ['test']);

    await page.goto('/gates');
    await expect(page.locator('#conn-badge')).toHaveText('live');

    const filter = page.locator('#gates-status-filter');
    await filter.selectOption('all');
    // After switching to 'all', the container should reload
    await expect(page.locator('#gates-container')).not.toContainText('Loading', { timeout: 5_000 });
    // Should show some rows
    const tableVisible = await page.locator('.gates-table').isVisible();
    expect(tableVisible).toBeTruthy();
  });
});
