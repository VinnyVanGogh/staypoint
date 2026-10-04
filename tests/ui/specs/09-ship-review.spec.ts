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
//
// STA-586 regression coverage for ship-review lifecycle UX:
//   Bug 4 — after Send back the card must show working-state spinner
//   Bug 5 — head_moved: inline banner instead of browser alert()
//   Bug 6 — Reject: inline form with reason + delete-branch checkbox
//   Bug 7 — Send back: inline form with feedback textarea

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

  // ── Bug 2 / STA-586 Bug 5: head_moved shows inline banner (no browser alert) ──
  test('Approve shows head_moved inline banner with new SHA when 409 head_moved', async ({ boardPage: page, api: _api, request }) => {
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

    // Fail if any browser dialog fires — dialogs are replaced by inline UI.
    page.on('dialog', async (d) => {
      await d.dismiss();
      throw new Error(`Unexpected browser dialog (${d.type()}): ${d.message()}`);
    });

    await card.getByRole('button', { name: /Approve/i }).click();

    // The inline head-moved banner must appear with the new SHA and a re-pin button.
    const banner = card.locator('.ship-review-head-moved-banner');
    await expect(banner).toBeVisible({ timeout: 5_000 });
    await expect(banner).toContainText(fakeNewHead.slice(0, 12));
    await expect(banner).toContainText(/re-pin/i);

    // The approve button must still be present.
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

    // Approve no longer uses a confirm() dialog — click directly.
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

  // ── Bug 3b / STA-586 Bug 6: Reject uses inline form (no browser prompt/confirm) ──
  test('Reject renders rejected final state card with comment via inline form', async ({ boardPage: page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review reject final');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const section = page.locator('.ship-review-card');
    await expect(section).toBeVisible({ timeout: 10_000 });

    // Intercept reject to return success.
    let capturedBody: Record<string, unknown> = {};
    await page.route('**/ship-review/reject', async (route) => {
      capturedBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ status: 'ok' }),
      });
    });

    // Fail if any browser dialog fires — should use inline form.
    page.on('dialog', async (d) => {
      await d.dismiss();
      throw new Error(`Unexpected browser dialog (${d.type()}): ${d.message()}`);
    });

    const rejectComment = 'Tests failed on mobile';

    // Click Reject to open the inline form.
    await page.locator('.ship-review-card .ship-review-reject-btn').click();

    // The inline form must be visible.
    const rejectForm = section.locator('.ship-review-inline-form').last();
    await expect(rejectForm).toBeVisible({ timeout: 3_000 });

    // Fill in reason.
    await rejectForm.locator('textarea').fill(rejectComment);

    // Verify delete-branch checkbox is present (unchecked by default).
    const delChk = rejectForm.locator('.ship-review-del-branch-chk');
    await expect(delChk).toBeVisible();
    await expect(delChk).not.toBeChecked();

    // Submit without checking delete branch.
    await rejectForm.locator('.ship-review-reject-submit-btn').click();

    // The final-state card must be visible with rejected badge.
    const finalCard = page.locator('.ship-review-card--final');
    await expect(finalCard).toBeVisible({ timeout: 5_000 });
    await expect(finalCard.locator('.ship-review-status-badge')).toContainText(/Rejected/i);

    // The reject comment must appear in the final card.
    await expect(finalCard).toContainText(rejectComment);

    // The API call must include comment and delete_branch=false.
    expect(capturedBody.comment).toBe(rejectComment);
    expect(capturedBody.delete_branch).toBe(false);
    cleanup();
  });

  // ── STA-586 Bug 6b: delete_branch checkbox sends delete_branch=true ──────
  test('Reject with delete-branch checked sends delete_branch=true', async ({ page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review reject del branch');
    await upsertShipReview(request, task.id);

    await gotoTaskPage(page, task);
    const section = page.locator('.ship-review-card');
    await expect(section).toBeVisible({ timeout: 10_000 });

    let capturedBody: Record<string, unknown> = {};
    await page.route('**/ship-review/reject', async (route) => {
      capturedBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'ok' }) });
    });

    await page.locator('.ship-review-card .ship-review-reject-btn').click();
    const rejectForm = section.locator('.ship-review-inline-form').last();
    await expect(rejectForm).toBeVisible({ timeout: 3_000 });

    await rejectForm.locator('.ship-review-del-branch-chk').check();
    await rejectForm.locator('.ship-review-reject-submit-btn').click();

    await expect(page.locator('.ship-review-card--final')).toBeVisible({ timeout: 5_000 });
    expect(capturedBody.delete_branch).toBe(true);
    cleanup();
  });

  // ── STA-586 Bug 7: Send back uses inline form ────────────────────────────
  test('Send back uses inline form and shows working-state spinner', async ({ page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review send back');
    await upsertShipReview(request, task.id);

    await gotoTaskPage(page, task);
    const section = page.locator('.ship-review-card');
    await expect(section).toBeVisible({ timeout: 10_000 });

    // Intercept send-back to return success (no real agent).
    await page.route('**/ship-review/send-back', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'ok' }) });
    });

    // Fail if any browser dialog fires.
    page.on('dialog', async (d) => {
      await d.dismiss();
      throw new Error(`Unexpected browser dialog (${d.type()}): ${d.message()}`);
    });

    // Click Send Back to open the inline form.
    await page.locator('.ship-review-card .ship-review-sendback-btn').click();

    const sendBackForm = section.locator('.ship-review-inline-form').first();
    await expect(sendBackForm).toBeVisible({ timeout: 3_000 });

    // Submit with feedback.
    await sendBackForm.locator('textarea').fill('Fix the tests first');
    await sendBackForm.locator('.ship-review-sendback-submit-btn').click();

    // After send-back succeeds, the working-state card must appear.
    const workingCard = page.locator('.ship-review-card--working');
    await expect(workingCard).toBeVisible({ timeout: 5_000 });
    await expect(workingCard).toContainText(/agent working/i);
    await expect(workingCard).toContainText(/run 2/i);

    // The spinner element must be present.
    await expect(workingCard.locator('.ship-review-spinner')).toBeVisible();

    // No action buttons should be present in the working state.
    await expect(workingCard.locator('.ship-review-approve-btn')).toHaveCount(0);
    cleanup();
  });

  // ── STA-586 Bug 4: no duplicate messages in "Send to Agent" section ───────
  test('Send to Agent section has no duplicate comments above composer', async ({ page, api: _api, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review no duplicate');
    await upsertShipReview(request, task.id);

    // Post a comment so there's something to duplicate.
    await request.post(`/api/tasks/${task.id}/comments`, {
      headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
      data: { message: 'Board comment for duplicate test', author: 'board' },
    });

    await gotoTaskPage(page, task);
    await expect(page.locator('.ship-review-card')).toBeVisible({ timeout: 10_000 });

    // The "Send to Agent" section must exist with the messages list inside it.
    const chatSection = page.locator('#page-chat-section');
    await expect(chatSection).toBeVisible();

    // page-chat-messages must live inside page-chat-section (single source of truth).
    await expect(chatSection.locator('#page-chat-messages')).toHaveCount(1);

    // The comment must appear exactly once — no duplicate Activity section.
    await expect(page.getByText('Board comment for duplicate test')).toHaveCount(1);

    // The textarea composer must be present.
    await expect(chatSection.locator('textarea')).toBeVisible();
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

