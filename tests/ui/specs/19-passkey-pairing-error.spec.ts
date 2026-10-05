import { test, expect } from '../fixtures';

// STA-696: when the daemon cannot show the pairing code (osascript denied, no
// GUI session), enrollment must stop with the real cause instead of asking the
// Board for a code that never arrived.
//
// The TestMode-only endpoint makes the daemon's pairing notifier fail with a
// given stderr, so this runs the real register/begin path end to end.
test('enrollment shows why the pairing code could not be shown, and never asks for it', async ({ boardPage: page }) => {
  const token = process.env.STAYPOINT_API_TOKEN!;
  const stderr = 'execution error: No user interaction allowed. (-1713)';

  const setup = await page.evaluate(async ({ token, stderr }) => {
    const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
    // No passkey enrolled, so a Board action offers enrollment.
    const clear = await fetch('/api/board/webauthn/test/clear-credentials', { method: 'DELETE', headers });
    const fail = await fetch('/api/board/webauthn/test/pairing-notifier', {
      method: 'PUT', headers, body: JSON.stringify({ error: stderr }),
    });
    return { clear: clear.status, fail: fail.status };
  }, { token, stderr });
  expect(setup).toEqual({ clear: 200, fail: 200 });

  const dialogs: Array<{ type: string; message: string }> = [];
  page.on('dialog', async d => {
    dialogs.push({ type: d.type(), message: d.message() });
    if (d.type() === 'prompt') await d.dismiss();
    else await d.accept();
  });

  try {
    const result = await page.evaluate(async () =>
      (await (window as any).withBoardWebAuthn(async () => 'action ran')) ?? null);

    expect(result).toBeNull();
    expect(dialogs.map(d => d.type)).not.toContain('prompt');
    const alert = dialogs.find(d => d.type === 'alert');
    expect(alert?.message).toContain("Couldn't show the pairing code:");
    expect(alert?.message).toContain(stderr);
  } finally {
    await page.evaluate(async token => {
      await fetch('/api/board/webauthn/test/pairing-notifier', {
        method: 'PUT',
        headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ error: '' }),
      });
    }, token);
  }
});
