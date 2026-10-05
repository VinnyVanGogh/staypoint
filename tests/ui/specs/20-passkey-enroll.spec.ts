import type { Dialog, Page } from '@playwright/test';
import { test, expect } from '../fixtures';

// STA-694: the Board can enroll a passkey without clicking a real action.
// With a Board session and zero stored credentials the page shows a
// persistent "No passkey enrolled" banner; Settings → Security has an
// explicit Enroll passkey button and lists enrolled passkeys with a delete
// action (DELETE stays behind WrapBoardAction). Enrolling from inside an
// action flow offers "Continue with <action>?" instead of "try again".
//
// Real server state throughout: boardSessionPage wipes every credential via
// the TestMode-only clear-credentials endpoint, and a CDP virtual
// authenticator answers navigator.credentials.create/get().

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';

const banner = (page: Page) => page.locator('#board-passkey-banner');
const securitySection = (page: Page) => page.locator('#settings-security');
const passkeyRows = (page: Page) => securitySection(page).locator('.passkey-row');

/**
 * Answers the browser dialogs the enrollment flow raises: the pairing-code
 * prompt gets the code from the TestMode-only last-pairing-code endpoint (the
 * real daemon shows it in a StayPoint Board dialog), and every confirm is
 * accepted. Returns the messages seen, in order.
 */
function answerEnrollDialogs(page: Page): Array<{ type: string; message: string }> {
  const seen: Array<{ type: string; message: string }> = [];
  page.on('dialog', async (d: Dialog) => {
    seen.push({ type: d.type(), message: d.message() });
    if (d.type() === 'prompt' && /pairing code/i.test(d.message())) {
      const res = await page.request.get('/api/board/webauthn/test/last-pairing-code', {
        headers: { Authorization: `Bearer ${TOKEN}` },
      });
      const { code } = (await res.json()) as { code: string };
      await d.accept(code);
      return;
    }
    if (d.type() === 'confirm') {
      await d.accept();
      return;
    }
    await d.dismiss();
  });
  return seen;
}

/** Makes the daemon's pairing notifier fail with stderr ('' restores it). TestMode-only (STA-696). */
async function failPairingNotifier(page: Page, stderr: string) {
  const res = await page.request.put('/api/board/webauthn/test/pairing-notifier', {
    headers: { Authorization: `Bearer ${TOKEN}` },
    data: { error: stderr },
  });
  expect(res.ok(), `pairing-notifier: ${res.status()} ${await res.text()}`).toBeTruthy();
}

async function registered(page: Page): Promise<boolean> {
  const res = await page.request.get('/api/board/webauthn/status', {
    headers: { Authorization: `Bearer ${TOKEN}` },
  });
  expect(res.ok(), `status: ${res.status()} ${await res.text()}`).toBeTruthy();
  return ((await res.json()) as { registered: boolean }).registered;
}

test.describe('Board passkey enrollment (STA-694)', () => {
  test('without a Board session there is no passkey banner', async ({ page }) => {
    const status = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/status'));
    await page.goto('/settings');
    expect((await status).status()).toBe(403);
    await expect(page.locator('#settings-container .settings-section').first()).toBeVisible();
    await expect(banner(page)).toBeHidden();
  });

  test('banner shows with 0 credentials; enrolling from Settings hides it and lists the passkey', async ({ boardSessionPage: page }) => {
    expect(await registered(page)).toBe(false);
    const dialogs = answerEnrollDialogs(page);

    await page.goto('/settings');
    await expect(banner(page)).toBeVisible();
    await expect(banner(page)).toContainText('No passkey enrolled. Board actions are locked.');
    await expect(banner(page).getByRole('button', { name: 'Enroll passkey' })).toBeVisible();

    await expect(securitySection(page)).toContainText('No passkeys enrolled');
    await expect(passkeyRows(page)).toHaveCount(0);

    const finish = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/register/finish'));
    await securitySection(page).getByRole('button', { name: 'Enroll passkey' }).click();
    expect((await finish).status()).toBe(200);

    await expect(banner(page)).toBeHidden();
    await expect(passkeyRows(page)).toHaveCount(1);
    await expect(passkeyRows(page).first().locator('.passkey-created')).not.toBeEmpty();
    expect(await registered(page)).toBe(true);
    // The pairing code was asked for; nothing told the Board to "try again".
    expect(dialogs.some((d) => d.type === 'prompt' && /pairing code/i.test(d.message))).toBe(true);
    expect(dialogs.some((d) => /try (your action )?again/i.test(d.message))).toBe(false);
  });

  test('the banner Enroll passkey button enrolls and the banner disappears', async ({ boardSessionPage: page }) => {
    answerEnrollDialogs(page);
    await page.goto('/');
    await expect(banner(page)).toBeVisible();

    const finish = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/register/finish'));
    await banner(page).getByRole('button', { name: 'Enroll passkey' }).click();
    expect((await finish).status()).toBe(200);

    await expect(banner(page)).toBeHidden();
    expect(await registered(page)).toBe(true);
  });

  test('deleting the last passkey goes through the passkey gate and brings the banner back', async ({ boardSessionPage: page }) => {
    answerEnrollDialogs(page);
    await page.goto('/settings');
    const finish = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/register/finish'));
    await securitySection(page).getByRole('button', { name: 'Enroll passkey' }).click();
    expect((await finish).status()).toBe(200);
    await expect(passkeyRows(page)).toHaveCount(1);

    const del = page.waitForResponse((r) =>
      r.request().method() === 'DELETE' && /\/api\/board\/webauthn\/credentials\/[^/]+$/.test(r.url()));
    await passkeyRows(page).first().getByRole('button', { name: 'Delete' }).click();
    const delRes = await del;
    expect(delRes.status(), await delRes.text()).toBe(200);
    expect(delRes.request().headers()['x-webauthn-assertion'] || '').not.toBe('');

    await expect(passkeyRows(page)).toHaveCount(0);
    await expect(banner(page)).toBeVisible();
    expect(await registered(page)).toBe(false);
  });

  test('enrolling inside an action offers "Continue with" and then performs the action', async ({ boardSessionPage: page }) => {
    const dialogs = answerEnrollDialogs(page);
    await page.goto('/settings');
    const toggle = page.locator('#gate-ship-review');
    await expect(toggle).toBeEnabled();
    const before = await toggle.isChecked();

    const saved = page.waitForResponse((r) =>
      r.request().method() === 'POST' && r.url().endsWith('/api/settings/ship-review'));
    await toggle.click();
    const savedRes = await saved;
    expect(savedRes.status(), await savedRes.text()).toBe(200);
    await expect(toggle).toBeChecked({ checked: !before });

    const cont = dialogs.find((d) => d.type === 'confirm' && /^Passkey enrolled\. Continue with /.test(d.message));
    expect(cont, `dialogs seen: ${JSON.stringify(dialogs)}`).toBeTruthy();
    expect(cont!.message).toMatch(/ship review/i);
    expect(dialogs.some((d) => /try (your action )?again/i.test(d.message))).toBe(false);
    await expect(banner(page)).toBeHidden();

    // Put the shared daemon's setting back: the passkey is enrolled now, so
    // this goes straight through the virtual authenticator.
    const restored = page.waitForResponse((r) =>
      r.request().method() === 'POST' && r.url().endsWith('/api/settings/ship-review'));
    await toggle.click();
    expect((await restored).status()).toBe(200);
    await expect(toggle).toBeChecked({ checked: before });
  });

  // STA-696 made register/begin fail with "Couldn't show the pairing code: …"
  // when the daemon cannot show the code. The explicit Enroll buttons must
  // surface that cause and never ask for a code that never arrived.
  test('Settings and banner Enroll passkey surface a pairing-code delivery failure', async ({ boardSessionPage: page }) => {
    const stderr = 'execution error: No user interaction allowed. (-1713)';
    const dialogs = answerEnrollDialogs(page);
    await failPairingNotifier(page, stderr);
    try {
      await page.goto('/settings');
      await expect(banner(page)).toBeVisible();

      for (const button of [
        securitySection(page).getByRole('button', { name: 'Enroll passkey' }),
        banner(page).getByRole('button', { name: 'Enroll passkey' }),
      ]) {
        dialogs.length = 0;
        const begin = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/register/begin'));
        await button.click();
        expect((await begin).ok()).toBe(false);
        await expect.poll(() => dialogs.find((d) => d.type === 'alert')?.message ?? '').toContain("Couldn't show the pairing code:");
        const alert = dialogs.find((d) => d.type === 'alert')!;
        expect(alert.message).toContain(stderr);
        expect(alert.message).not.toContain('board_passkey_enrollment_required');
        expect(dialogs.map((d) => d.type)).not.toContain('prompt');
        await expect(button).toBeEnabled();
      }
      await expect(banner(page)).toBeVisible();
      expect(await registered(page)).toBe(false);
    } finally {
      await failPairingNotifier(page, '');
    }
  });

  // The page caches whether a passkey exists. If every passkey is removed
  // without a board_passkey_deleted event reaching the page, enrolling must
  // still work instead of demanding an assertion from a passkey that is gone.
  test('enrolling works when passkeys were removed behind the page\'s back', async ({ boardPage: page }) => {
    // boardPage enrolled after the page loaded; wait for the SSE refresh.
    await expect.poll(() => page.evaluate(() => (window as any).eval('boardPasskeyState.registered'))).toBe(true);
    const clear = await page.request.delete('/api/board/webauthn/test/clear-credentials', {
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
    expect(clear.ok()).toBeTruthy();

    const dialogs = answerEnrollDialogs(page);
    const result = await page.evaluate(async () =>
      (await (window as any).withBoardWebAuthn(async () => 'action ran', 'the test action')) ?? null);

    expect(result, `dialogs seen: ${JSON.stringify(dialogs)}`).toBe('action ran');
    expect(dialogs.some((d) => /enrollment_required|Enrollment failed/.test(d.message))).toBe(false);
    expect(dialogs.some((d) => d.message === 'Passkey enrolled. Continue with the test action?')).toBe(true);
    expect(await registered(page)).toBe(true);
  });
});
