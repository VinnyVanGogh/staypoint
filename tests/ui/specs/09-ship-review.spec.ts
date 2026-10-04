import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { test, expect, gotoTaskPage } from '../fixtures';
import type { APIRequestContext } from '@playwright/test';

// STA-535 regression coverage for ship-review UI bugs:
//   Bug 1 — dev_url missing from GET after UpsertCard auto-start
//   Bug 2 — head_moved path was dead code (apiFetch threw before JSON parse)
//   Bug 3 — approved / rejected final state was never rendered

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';

type ShipTask = { id: string; organization: string; project: string; [k: string]: unknown };

// createShipReviewTask creates a task with a real git repo.
// UpsertCard post-STA-533 always resolves "staypoint/<taskId>", so we:
//   1. Create the task via API (with repo_path pointing to the temp repo)
//   2. Create the staypoint/<taskId> branch after we have the task ID
async function createShipReviewTask(
  request: APIRequestContext,
  label: string,
): Promise<{ task: ShipTask; repoDir: string; cleanup: () => void }> {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'sta535-e2e-'));
  const work = path.join(base, 'work');
  const bare = path.join(base, 'bare.git');
  const env = {
    ...process.env,
    GIT_AUTHOR_NAME: 'test', GIT_AUTHOR_EMAIL: 't@t.com',
    GIT_COMMITTER_NAME: 'test', GIT_COMMITTER_EMAIL: 't@t.com',
  };
  const run = (args: string[]) => execFileSync('git', args, { cwd: work, env, stdio: 'pipe' });

  fs.mkdirSync(work, { recursive: true });
  execFileSync('git', ['init', '-b', 'main', work], { env, stdio: 'pipe' });
  run(['config', 'user.email', 't@t.com']);
  run(['config', 'user.name', 'test']);
  fs.writeFileSync(path.join(work, 'README.md'), 'init\n');
  run(['add', '.']);
  run(['commit', '-m', 'init']);
  execFileSync('git', ['init', '--bare', '-b', 'main', bare], { env, stdio: 'pipe' });
  run(['remote', 'add', 'origin', bare]);
  run(['push', 'origin', 'main']);

  const name = `${label} ${Date.now().toString(36)}`;
  const res = await request.post('/api/tasks', {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    data: { name, organization: 'STA', project: 'ui-e2e', repo_path: work, git_branch: 'main' },
  });
  const task = (await res.json()) as ShipTask;
  const taskId = task.id;

  const branch = `staypoint/${taskId}`;
  run(['checkout', '-b', branch]);
  fs.writeFileSync(path.join(work, 'task.txt'), 'task work\n');
  run(['add', '.']);
  run(['commit', '-m', 'task: add work file']);
  run(['push', 'origin', branch]);
  run(['checkout', 'main']);

  const cleanup = () => { try { fs.rmSync(base, { recursive: true, force: true }); } catch {} };
  return { task: { ...task, organization: 'STA', project: 'ui-e2e' }, repoDir: work, cleanup };
}

async function upsertShipReview(
  request: APIRequestContext,
  taskId: string,
  opts: { testSteps?: string[]; devUrl?: string } = {},
) {
  const res = await request.put(`/api/tasks/${encodeURIComponent(taskId)}/ship-review`, {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    data: {
      test_steps: opts.testSteps ?? ['1. Open the homepage', '2. Verify title'],
      ...(opts.devUrl ? { dev_url: opts.devUrl } : {}),
    },
  });
  return res;
}

test.describe('ship review card', () => {

  // ── Bug 1: Preview row visible when dev_url is present ────────────────────
  test('Preview row shows dev_url from card', async ({ page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review dev_url');
    test.info().annotations.push({ type: 'cleanup', description: task.id });
    const res = await upsertShipReview(request, task.id, { devUrl: 'http://127.0.0.1:8799' });
    expect(res.ok(), `upsert failed: ${await res.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);

    // The card section must appear.
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Preview row with the URL must be visible.
    const previewRow = card.locator('.ship-review-row', { hasText: 'Preview' });
    await expect(previewRow).toBeVisible();
    const link = previewRow.locator('.ship-review-dev-link');
    await expect(link).toContainText('127.0.0.1:8799');
    cleanup();
  });

  // ── Bug 2: head_moved alert includes new SHA ──────────────────────────────
  test('Approve shows head_moved alert with new SHA when 409 head_moved', async ({ boardPage: page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review head_moved');
    const res = await upsertShipReview(request, task.id);
    expect(res.ok(), `upsert failed: ${await res.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Intercept the approve call and return a 409 head_moved response.
    const fakeNewHead = 'abcdef123456deadbeef';
    await page.route('**/ship-review/approve', async (route) => {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'head_moved', new_head_sha: fakeNewHead }),
      });
    });

    // Use a single handler: confirm → accept silently; alert → capture message.
    // page.once + page.on both fire for the first dialog, causing a double-accept
    // crash ("Cannot accept dialog which is already handled!").
    let alertMessage = '';
    page.on('dialog', async (d) => {
      if (d.type() === 'confirm') {
        await d.accept();
      } else {
        alertMessage = d.message();
        await d.accept();
      }
    });

    await card.getByRole('button', { name: /Approve/i }).click();

    // Wait for the alert to fire and be captured.
    await expect.poll(() => alertMessage, { timeout: 5_000 })
      .toContain(fakeNewHead.slice(0, 8));

    // The alert must say "HEAD moved" and not just "409 Conflict" (dead-code path).
    expect(alertMessage).toMatch(/moved/i);

    // The approve button must still be present (card reload, not removed).
    await expect(card).toBeVisible({ timeout: 5_000 });
    cleanup();
  });

  // ── Bug 3: approved final state shows reviewed SHA → main SHA ─────────────
  test('Approve renders approved final state card with main SHA', async ({ boardPage: page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review approve final');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    // Get the current head_sha from the card so we can verify it in the final state.
    const cardRes = await request.get(`/api/tasks/${encodeURIComponent(task.id)}/ship-review`, {
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
    const cardData = await cardRes.json();
    const headSHA = cardData.head_sha as string;

    await gotoTaskPage(page, task);
    const section = page.locator('.ship-review-card');
    await expect(section).toBeVisible({ timeout: 10_000 });

    const fakeMainSHA = 'f00dbadf00dbadf00d';
    // Intercept approve to return success with a known main_sha.
    await page.route('**/ship-review/approve', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          main_sha: fakeMainSHA,
          card: { status: 'approved', head_sha: headSHA, main_sha: fakeMainSHA },
        }),
      });
    });

    // Auto-accept the confirm dialog.
    page.once('dialog', (d) => d.accept());

    await page.locator('.ship-review-card .ship-review-approve-btn').click();

    // The action buttons must disappear.
    await expect(page.locator('.ship-review-approve-btn')).toHaveCount(0, { timeout: 5_000 });

    // The final-state card must be visible.
    const finalCard = page.locator('.ship-review-card--final');
    await expect(finalCard).toBeVisible({ timeout: 5_000 });

    // It must show the approved badge.
    await expect(finalCard.locator('.ship-review-status-badge')).toContainText(/Approved/i);

    // It must show main SHA.
    await expect(finalCard).toContainText(fakeMainSHA.slice(0, 12));
    cleanup();
  });

  // ── Bug 3b: rejected final state shows reject comment ────────────────────
  test('Reject renders rejected final state card with comment', async ({ boardPage: page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review reject final');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const section = page.locator('.ship-review-card');
    await expect(section).toBeVisible({ timeout: 10_000 });

    // Intercept reject to return success.
    await page.route('**/ship-review/reject', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ status: 'ok' }),
      });
    });

    const rejectComment = 'Tests failed on mobile';

    // Handle dialogs: first prompt (reason), then confirm (delete branch).
    const dialogs: import('@playwright/test').Dialog[] = []; // eslint-disable-line @typescript-eslint/no-unused-vars
    page.on('dialog', async (d) => {
      dialogs.push(d);
      if (d.type() === 'prompt') await d.accept(rejectComment);
      else if (d.type() === 'confirm') await d.dismiss(); // don't delete branch
      else await d.accept();
    });

    await page.locator('.ship-review-card .ship-review-reject-btn').click();

    // The action buttons must disappear.
    await expect(page.locator('.ship-review-reject-btn')).toHaveCount(0, { timeout: 5_000 });

    // The final-state card must be visible with rejected badge.
    const finalCard = page.locator('.ship-review-card--final');
    await expect(finalCard).toBeVisible({ timeout: 5_000 });
    await expect(finalCard.locator('.ship-review-status-badge')).toContainText(/Rejected/i);

    // The reject comment must appear in the final card.
    await expect(finalCard).toContainText(rejectComment);
    cleanup();
  });

  // ── Negative: no passkey enrolled → enrollment prompt, action NOT performed ─
  // Intercepts /webauthn/challenge to return 412 (no credentials registered),
  // simulating a board session holder who hasn't enrolled a passkey yet.
  test('Board action without enrolled passkey shows enrollment prompt and does not call action endpoint', async ({ boardPage: page, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'WebAuthn negative enroll');

    const res = await upsertShipReview(request, task.id);
    expect(res.ok(), `upsert failed: ${await res.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Intercept challenge → 412 (simulates no passkey enrolled for this board session).
    await page.route('**/webauthn/challenge', async (route) => {
      await route.fulfill({
        status: 412,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'no credentials registered' }),
      });
    });

    // Track whether the action endpoint was ever called.
    let approveWasCalled = false;
    await page.route('**/ship-review/approve', async (route) => {
      approveWasCalled = true;
      await route.continue();
    });

    // Handle dialogs in order:
    //   1st confirm: "Approve and merge?" → accept
    //   2nd confirm: enrollment offer → dismiss (decline enrollment)
    let enrollmentPromptSeen = false;
    let dialogIndex = 0;
    page.on('dialog', async (d) => {
      dialogIndex++;
      if (dialogIndex === 1 && d.type() === 'confirm') {
        await d.accept(); // accept the "Approve and merge?" gate
      } else if (d.type() === 'confirm' && /enroll|passkey/i.test(d.message())) {
        enrollmentPromptSeen = true;
        await d.dismiss(); // decline enrollment
      } else {
        await d.dismiss();
      }
    });

    await card.getByRole('button', { name: /Approve/i }).click();

    // Give async operations time to settle.
    await page.waitForTimeout(800);

    expect(enrollmentPromptSeen).toBe(true);
    expect(approveWasCalled).toBe(false);
    cleanup();
  });

});
