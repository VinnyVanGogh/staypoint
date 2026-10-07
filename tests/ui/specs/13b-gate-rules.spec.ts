import { randomUUID } from 'node:crypto';
import { test, expect } from '../fixtures';
import type { Page, Request } from '@playwright/test';

// STA-868: batch decisions under one Touch ID, the passkey grace window,
// Approve & remember allow rules, the Rules tab, and the AI review panel.

function uniq(): string {
  return randomUUID().slice(0, 8);
}

const isChallenge = (r: Request) => r.method() === 'POST' && r.url().endsWith('/api/board/webauthn/challenge');

function countChallenges(page: Page): () => number {
  let n = 0;
  page.on('request', (r) => { if (isChallenge(r)) n++; });
  return () => n;
}

test.describe('Gate rules and batch decisions', () => {
  test('Approve selected decides several requests with one Touch ID, then grace covers the next', async ({ api, boardPage }) => {
    const tag = uniq();
    const a = await api.createGateRequest(`sudo ls a-${tag}`, ['sudo: privilege escalation'], 'run-sta-868-batch');
    const b = await api.createGateRequest(`sudo ls b-${tag}`, ['sudo: privilege escalation'], 'run-sta-868-batch');
    const c = await api.createGateRequest(`sudo ls c-${tag}`, ['sudo: privilege escalation'], 'run-sta-868-batch');
    const challenges = countChallenges(boardPage);

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');
    const container = boardPage.locator('#gates-container');
    for (const g of [a, b]) {
      await container.locator(`tr[data-gate-id="${g.id}"] .gate-select`).check();
    }
    const batch = boardPage.waitForResponse((r) => r.url().endsWith('/api/security/gate-requests/decide-batch'));
    await boardPage.locator('#gates-toolbar .gate-batch-approve').click();
    const res = await batch;
    expect(res.status(), await res.text()).toBe(200);
    expect((await res.json()).decided).toBe(2);
    expect(challenges()).toBe(1);

    const approved = await api.listGateRequests('approved');
    for (const g of [a, b]) expect(approved.gate_requests.find((r) => r.id === g.id)?.status).toBe('approved');
    await expect(boardPage.locator('#gates-grace')).toContainText('Touch ID valid for');

    // Inside the grace window a single decision needs no new Touch ID.
    const decided = boardPage.waitForResponse((r) => /\/decide$/.test(r.url()));
    await container.locator(`tr[data-gate-id="${c.id}"] .gate-deny-btn`).click();
    expect((await decided).status()).toBe(200);
    expect(challenges()).toBe(1);
  });

  test('Approve & remember creates a task rule that auto-approves the same command', async ({ api, boardPage }) => {
    boardPage.on('dialog', (d) => d.accept());
    const task = await api.createTask('gate rule');
    const cmd = `sudo du -sh /var/tmp-${uniq()}`;
    const gr = await api.createGateRequest(cmd, ['sudo: privilege escalation'], 'run-sta-868-rule', task.id);

    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');
    const row = boardPage.locator(`#gates-container tr[data-gate-id="${gr.id}"]`);
    await row.locator('.gate-remember-btn').click();
    await expect(row.locator('.gate-remember-scope')).toHaveValue('task');
    const decided = boardPage.waitForResponse((r) => /\/decide$/.test(r.url()));
    await row.locator('.gate-remember-confirm').click();
    const res = await decided;
    expect(res.status(), await res.text()).toBe(200);

    const again = await api.createGateRequest(cmd, ['sudo: privilege escalation'], 'run-sta-868-rule', task.id);
    expect(again.status).toBe('approved');
    expect(again.decided_by).toMatch(/^rule:\d+$/);
    const other = await api.createGateRequest(cmd, ['sudo: privilege escalation'], 'run-sta-868-rule');
    expect(other.status).toBe('pending');

    await boardPage.locator('.gates-tab[data-tab="rules"]').click();
    const ruleRow = boardPage.locator('.gates-rules-table tbody tr').filter({ hasText: cmd });
    await expect(ruleRow).toContainText(`Task: ${task.id}`);
    await expect(ruleRow.locator('td').nth(5)).toContainText('1');

    const del = boardPage.waitForResponse((r) => r.request().method() === 'DELETE');
    await ruleRow.locator('.gate-rule-delete').click();
    expect((await del).status()).toBe(200);
    await expect(ruleRow).toHaveCount(0);
    const rules = await api.listGateRules();
    expect(rules.rules.find((r) => r.pattern === cmd)).toBeFalsy();
  });

  test('Review with AI reports a failure without blocking decisions', async ({ api, boardPage }) => {
    const cmd = `sudo ls review-${uniq()}`;
    const gr = await api.createGateRequest(cmd, ['sudo: privilege escalation'], 'run-sta-868-review');
    await boardPage.goto('/gates');
    await expect(boardPage.locator('#conn-badge')).toHaveText('live');
    await boardPage.locator('#gates-toolbar .gate-review-btn').click();
    await expect(boardPage.locator('.gates-review-panel')).toContainText('AI review failed');
    const row = boardPage.locator(`#gates-container tr[data-gate-id="${gr.id}"]`);
    await expect(row.locator('.gate-approve-btn')).toBeVisible();
  });
});
