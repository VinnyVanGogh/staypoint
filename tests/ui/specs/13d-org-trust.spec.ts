import { randomUUID } from 'node:crypto';
import { test, expect } from '../fixtures';

// task-33692ffb: "Trust this organization until…" in Settings.

test.describe('Org trust', () => {
  test('one Touch ID trusts the organization; deploys still wait; Revoke ends it', async ({ api, boardPage }) => {
    const org = `OT${randomUUID().slice(0, 6)}`;
    const task = await api.createTask('org trust', { organization: org });
    await api.setStage(task.id, 'in_progress');
    await boardPage.goto('/settings');

    const section = boardPage.locator('.org-trust-section');
    await expect(section.locator('.org-trust-create')).toContainText('Trust this organization until…');
    await section.locator('.org-trust-org').fill(org);
    await section.locator('.trust-preset').selectOption('4h');
    boardPage.on('dialog', (d) => d.accept());
    const created = boardPage.waitForResponse((r) => r.url().endsWith('/api/settings/org-trust') && r.request().method() === 'POST');
    await section.locator('.trust-create-btn').click();
    const res = await created;
    expect(res.status(), await res.text()).toBe(201);
    // The body names the org and a preset only: the server sets the expiry.
    expect(Object.keys(JSON.parse(res.request().postData() || '{}')).sort()).toEqual(['org', 'preset']);
    const banner = section.locator(`.org-trust-banner[data-org="${org.toLowerCase()}"]`);
    await expect(banner.locator('.trust-banner-text')).toHaveText(new RegExp(`^${org.toLowerCase()}: Trusted until (\\w+ )?\\d\\d:\\d\\d · 0 auto-approved$`));

    const ok = await api.createGateRequest(`sudo ls ot-${randomUUID().slice(0, 6)}`, ['sudo: privilege escalation'], 'run-ot', task.id);
    expect(ok.status).toBe('approved');
    const deploy = await api.createGateRequest('wrangler deploy', ['deploy'], 'run-ot', task.id);
    expect(deploy.status).toBe('pending');

    await boardPage.reload();
    await expect(banner.locator('.trust-banner-text')).toContainText('1 auto-approved');
    await expect(banner.locator('.trust-banner-text')).toContainText('1 waiting for you');

    const revoked = boardPage.waitForResponse((r) => r.url().endsWith('/org-trust/revoke'));
    await banner.locator('.trust-revoke-btn').click();
    expect((await revoked).status()).toBe(200);
    await expect(banner).toHaveCount(0);
    const after = await api.createGateRequest(`sudo ls after-${randomUUID().slice(0, 6)}`, ['sudo: privilege escalation'], 'run-ot', task.id);
    expect(after.status).toBe('pending');
  });
});
