import { randomUUID } from 'node:crypto';
import { test, expect, gotoTaskPage } from '../fixtures';

// task-6c1ed91f: "Trust this task until…" on the task page and the Gates page.

function uniq(): string {
  return randomUUID().slice(0, 8);
}

test.describe('Task trust', () => {
  test('one Touch ID trusts the task; banner counts auto-approvals; merges still wait; Revoke ends it', async ({ api, boardPage }) => {
    const task = await api.createTask('trust');
    await api.setStage(task.id, 'in_progress');
    await gotoTaskPage(boardPage, task);

    const section = boardPage.locator('.task-trust-section');
    await expect(section.locator('.trust-create')).toContainText('Trust this task until…');
    await section.locator('.trust-preset').selectOption('4h');
    const created = boardPage.waitForResponse((r) => r.url().endsWith(`/api/tasks/${task.id}/trust`) && r.request().method() === 'POST');
    await section.locator('.trust-create-btn').click();
    const res = await created;
    expect(res.status(), await res.text()).toBe(201);
    // The body names a preset only: the server sets the expiry.
    expect(Object.keys(JSON.parse(res.request().postData() || '{}'))).toEqual(['preset']);
    await expect(section.locator('.trust-banner-text')).toHaveText(/^Trusted until \d\d:\d\d · 0 auto-approved$/);

    const cmd = `sudo ls trust-${uniq()}`;
    const ok = await api.createGateRequest(cmd, ['sudo: privilege escalation'], 'run-trust', task.id);
    expect(ok.status).toBe('approved');
    expect(ok.decided_by).toMatch(/^rule:\d+$/);
    const merge = await api.createGateRequest('gh pr merge 1 --squash', ['gh pr merge'], 'run-trust', task.id);
    expect(merge.status).toBe('pending');

    await gotoTaskPage(boardPage, task);
    await expect(section.locator('.trust-banner-text')).toContainText('1 auto-approved');
    await expect(section.locator('.trust-banner-text')).toContainText('1 waiting for you');
    await expect(section.locator('.trust-request-list').first()).toContainText(cmd);

    const revoked = boardPage.waitForResponse((r) => r.url().endsWith('/trust/revoke'));
    await section.locator('.trust-revoke-btn').click();
    expect((await revoked).status()).toBe(200);
    await expect(section.locator('.trust-create')).toBeVisible();
    const after = await api.createGateRequest(`sudo ls after-${uniq()}`, ['sudo: privilege escalation'], 'run-trust', task.id);
    expect(after.status).toBe('pending');
  });

  test('tev1 mode shows the amber banner and the morning summary', async ({ api, boardPage }) => {
    const task = await api.createTask('trust tev1');
    const now = new Date();
    const exp = new Date(now.getTime() + 6 * 3600_000).toISOString();
    const trust = {
      id: 9001, pattern: '*', match_kind: 'any', scope: 'task', scope_value: task.id, created_at: now.toISOString(),
      expires_at: exp, tev1: true, tev1_threshold: 0.7, active: true, auto_approved_count: 1, held_count: 0, deferred_count: 0,
      auto_approved: [],
      tev1_approved: [{ id: 'g1', cmdline: 'go test ./...', status: 'approved', decided_by: 'rule:9001:tev1', decided_at: now.toISOString() }],
      tev1_denied: [{ id: 'g2', cmdline: 'sqlite3 data.db .tables', status: 'denied', decided_by: 'rule:9001:tev1', decided_at: now.toISOString() }],
    };
    await boardPage.route(`**/api/tasks/${task.id}/trust`, (route) => route.fulfill({
      json: { task_id: task.id, active: trust, latest: trust, held: [], defer_minutes: 10, tev1_threshold: 0.7, tev1_configured: true, tev1_warning: 'x' },
    }));
    await boardPage.route('**/api/security/trusts', (route) => route.fulfill({ json: { trusts: [{ ...trust, task_name: task.name }] } }));

    await gotoTaskPage(boardPage, task);
    const section = boardPage.locator('.task-trust-section');
    await expect(section.locator('.trust-tev1-banner')).toContainText('unproven 4B model');
    await expect(section.locator('.trust-banner-text')).toContainText('tev1 decides (p ≥ 0.70)');
    await expect(section.locator('.trust-tev1-summary')).toContainText('1 approved, 1 denied');
    await expect(section.locator('.trust-tev1-summary')).toContainText('tev1 denied: sqlite3 data.db .tables');

    await boardPage.goto('/gates');
    const panel = boardPage.locator('.gates-trust-panel');
    await expect(panel.locator('.trust-tev1-banner')).toBeVisible();
    await expect(panel).toContainText(task.name);
    await expect(panel.locator('.trust-tev1-summary')).toContainText('tev1 approved: go test ./...');
  });

  test('enabling tev1 asks for confirmation with the warning', async ({ api, boardPage }) => {
    const task = await api.createTask('trust tev1 confirm');
    await boardPage.route(`**/api/tasks/${task.id}/trust`, async (route) => {
      if (route.request().method() === 'GET') {
        await route.fulfill({ json: { task_id: task.id, active: null, latest: null, held: [], defer_minutes: 10, tev1_configured: true,
          tev1_warning: 'tev1 is an unproven 4B model; on 2026-10-07 it denied a harmless go test' } });
        return;
      }
      await route.fallback();
    });
    await gotoTaskPage(boardPage, task);
    const section = boardPage.locator('.task-trust-section');
    await section.locator('.trust-tev1-toggle input').check();
    let message = '';
    boardPage.once('dialog', (d) => { message = d.message(); d.dismiss(); });
    let posted = false;
    boardPage.on('request', (r) => { if (r.method() === 'POST' && r.url().endsWith('/trust')) posted = true; });
    await section.locator('.trust-create-btn').click();
    await expect.poll(() => message).toContain('unproven 4B model');
    expect(posted).toBe(false);
  });
});
