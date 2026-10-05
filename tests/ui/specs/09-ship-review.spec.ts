import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { test, expect, gotoTaskPage } from '../fixtures';
import type { APIRequestContext, Page } from '@playwright/test';

// The Approve & Merge / Send Back / Reject buttons and their inline forms live
// in the task page's sticky header, not inside .ship-review-card (STA-640).
const reviewActions = (page: Page) => page.locator('#task-page-content .task-page-header');

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

  // UI-state rendering only; enforcement covered by Gates specs + Go tests.
  // These specs fulfill /ship-review/approve and /ship-review/reject with
  // page.route, so the real server never decides anything. They prove the card
  // renders each response shape correctly, NOT that the server enforces the
  // Board passkey gate. Do not cite them as enforcement coverage.
  test.describe('UI-state rendering only (route-faked action responses)', () => {

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

    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    // Inline confirm form must appear; click Merge to main to proceed.
    await expect(reviewActions(page).locator('.ship-review-approve-confirm')).toBeVisible({ timeout: 3_000 });
    await reviewActions(page).getByRole('button', { name: /Merge to main/i }).click();

    // The inline head-moved banner must appear with the new SHA and a re-pin button.
    const banner = reviewActions(page).locator('.ship-review-head-moved-banner');
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

    // Approve shows inline confirm form — click Approve then Merge to main.
    await reviewActions(page).locator('.ship-review-approve-btn').click();
    await expect(page.locator('.ship-review-approve-confirm')).toBeVisible({ timeout: 3_000 });
    await page.locator('.ship-review-approve-confirm').getByRole('button', { name: /Merge to main/i }).click();

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
    await reviewActions(page).locator('.ship-review-reject-btn').click();

    // The inline form must be visible.
    const rejectForm = reviewActions(page).locator('.ship-review-inline-form:not(.ship-review-approve-confirm)').last();
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
  test('Reject with delete-branch checked sends delete_branch=true', async ({ boardPage: page, api: _api, request }) => {
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

    await reviewActions(page).locator('.ship-review-reject-btn').click();
    const rejectForm = reviewActions(page).locator('.ship-review-inline-form:not(.ship-review-approve-confirm)').last();
    await expect(rejectForm).toBeVisible({ timeout: 3_000 });

    await rejectForm.locator('.ship-review-del-branch-chk').check();
    await rejectForm.locator('.ship-review-reject-submit-btn').click();

    await expect(page.locator('.ship-review-card--final')).toBeVisible({ timeout: 5_000 });
    expect(capturedBody.delete_branch).toBe(true);
    cleanup();
  });

  // ── STA-586 Bug 7: Send back uses inline form ────────────────────────────
  test('Send back uses inline form and shows working-state spinner', async ({ boardPage: page, api: _api, request }) => {
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
    await reviewActions(page).locator('.ship-review-sendback-btn').click();

    const sendBackForm = reviewActions(page).locator('.ship-review-inline-form').first();
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
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toHaveCount(0);
    cleanup();
  });

  // ── STA-657: SSE re-render whose card fetch fails clears header actions ───
  // The ship_review_* handler removes the card before refetching it. If that
  // GET fails, no card is rendered, so the header must not keep Approve /
  // Send Back / Reject wired to the removed card.
  test('ship_review SSE with failed card fetch clears header review buttons', async ({ boardPage: page, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Ship review SSE fetch fail');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    await expect(page.locator('.ship-review-card')).toBeVisible({ timeout: 10_000 });
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toHaveCount(1);

    let failedGets = 0;
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review`, async (route) => {
      if (route.request().method() !== 'GET') return route.fallback();
      failedGets++;
      await route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'boom' }) });
    });

    await page.evaluate((tid) => {
      (window as unknown as { handleEvent: (e: unknown) => void })
        .handleEvent({ type: 'ship_review_updated', data: { task_id: tid } });
    }, task.id);

    await expect.poll(() => failedGets, { timeout: 5_000 }).toBeGreaterThan(0);
    await expect(page.locator('.ship-review-card')).toHaveCount(0);
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toHaveCount(0);
    await expect(reviewActions(page).locator('.ship-review-sendback-btn')).toHaveCount(0);
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


  // ── STA-637: branch delete failure is a non-blocking warning with Retry ───
  test('Approve with failed branch delete shows warning; Retry shows Branch deleted', async ({ boardPage: page, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Branch delete warning');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    const deleteError = 'git push origin --delete: remote rejected';
    await page.route('**/ship-review/approve', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          main_sha: 'cafef00dcafef00d',
          branch_deleted: false,
          branch_delete_error: deleteError,
          warning: `merged; branch delete failed: ${deleteError}`,
          card: { status: 'approved', branch_deleted: false, branch_delete_error: deleteError },
        }),
      });
    });
    let retryCalls = 0;
    await page.route('**/ship-review/delete-branch', async (route) => {
      retryCalls++;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ branch_deleted: true, card: { status: 'approved', branch_deleted: true } }),
      });
    });

    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    await reviewActions(page).getByRole('button', { name: /Merge to main/i }).click();

    // Merge stands: final approved card, plus the non-blocking warning.
    const finalCard = page.locator('.ship-review-card--final');
    await expect(finalCard.locator('.ship-review-status-badge')).toContainText(/Approved/i, { timeout: 5_000 });
    const warning = finalCard.locator('.ship-review-branch-warning');
    await expect(warning).toContainText(`merged; branch delete failed: ${deleteError}`);
    await expect(finalCard).not.toContainText('Branch deleted');

    await warning.getByRole('button', { name: /Retry/i }).click();
    await expect(page.locator('.ship-review-card--final .ship-review-branch-deleted')).toHaveText('Branch deleted', { timeout: 5_000 });
    await expect(page.locator('.ship-review-branch-warning')).toHaveCount(0);
    expect(retryCalls).toBe(1);
    cleanup();
  });

  });

  // ── Negative: no passkey enrolled → enrollment prompt, action NOT performed ─
  // Real server state, no faked responses: the boardPage fixture enrolls a
  // passkey, then this spec wipes every stored credential through the
  // TestMode-only DELETE /api/board/webauthn/test/clear-credentials. The real
  // /webauthn/challenge must then answer 412, the client must offer enrollment
  // and never reach /ship-review/approve, and the real WrapBoardAction gate must
  // refuse a direct approve with 403 board_passkey_enrollment_required.
  test('Board action without enrolled passkey shows enrollment prompt and does not call action endpoint', async ({ boardPage: page, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'WebAuthn negative enroll');

    const res = await upsertShipReview(request, task.id);
    expect(res.ok(), `upsert failed: ${await res.text()}`).toBeTruthy();

    // page.request shares the browser context's cookies, so the staypoint_board
    // session cookie set by the fixture rides along (the route is board-session gated).
    const clear = await page.request.delete('/api/board/webauthn/test/clear-credentials', {
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
    expect(clear.ok(), `clear-credentials failed: ${clear.status()} ${await clear.text()}`).toBeTruthy();

    const cardURL = `/api/tasks/${encodeURIComponent(task.id)}/ship-review`;
    const before = await (await request.get(cardURL, { headers: { Authorization: `Bearer ${TOKEN}` } })).json();

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Passthrough spy only: never fulfills, so any hit reaches the real server.
    let approveWasCalled = false;
    await page.route('**/ship-review/approve', async (route) => {
      approveWasCalled = true;
      await route.continue();
    });

    // Approve no longer shows an "Approve and merge?" confirm dialog — the first
    // dialog will be the enrollment prompt from withBoardWebAuthn when no passkey enrolled.
    let enrollmentPromptSeen = false;
    page.on('dialog', async (d) => {
      if (d.type() === 'confirm' && /enroll|passkey/i.test(d.message())) {
        enrollmentPromptSeen = true;
        await d.dismiss(); // decline enrollment
      } else {
        await d.dismiss();
      }
    });

    const challenge = page.waitForResponse((r) => r.url().includes('/api/board/webauthn/challenge'));
    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    // Inline confirm form must appear; click Merge to main to trigger withBoardWebAuthn.
    await expect(reviewActions(page).locator('.ship-review-approve-confirm')).toBeVisible({ timeout: 3_000 });
    await reviewActions(page).getByRole('button', { name: /Merge to main/i }).click();

    // The real server, not a route, must report zero passkeys.
    const challengeRes = await challenge;
    expect(challengeRes.status()).toBe(412);

    await expect.poll(() => enrollmentPromptSeen, { timeout: 5_000 }).toBe(true);
    expect(approveWasCalled).toBe(false);

    // Server-side enforcement: even with the Board session cookie, a direct
    // approve is refused before the handler runs.
    const direct = await page.request.post(`${cardURL}/approve`, {
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
    expect(direct.status()).toBe(403);
    expect((await direct.json()).error).toBe('board_passkey_enrollment_required');

    // Card state is unchanged and the action is still offered.
    const after = await (await request.get(cardURL, { headers: { Authorization: `Bearer ${TOKEN}` } })).json();
    expect(after.status).toBe(before.status);
    expect(after.head_sha).toBe(before.head_sha);
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeEnabled();
    expect(approveWasCalled).toBe(false);
    cleanup();
  });

  // ── Approve inline confirm: cancel suppresses request; confirm proceeds ────
  test('Approve inline confirm shows SHA and branch; Cancel suppresses request; Merge proceeds', async ({ boardPage: page, request }) => {
    const { task, cleanup } = await createShipReviewTask(request, 'Approve inline confirm');
    const res = await upsertShipReview(request, task.id);
    expect(res.ok(), `upsert failed: ${await res.text()}`).toBeTruthy();

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Track approve calls.
    let approveCallCount = 0;
    await page.route('**/ship-review/approve', async (route) => {
      approveCallCount++;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ main_sha: 'deadbeef00000000', card: { status: 'approved' } }),
      });
    });

    // Fail if any browser dialog fires — everything is inline.
    page.on('dialog', async (d) => {
      await d.dismiss();
      throw new Error(`Unexpected browser dialog (${d.type()}): ${d.message()}`);
    });

    // Click Approve → inline confirm form must appear.
    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    const confirmForm = reviewActions(page).locator('.ship-review-approve-confirm');
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });

    // Form must mention the target branch.
    await expect(confirmForm).toContainText('main');

    // No request yet.
    expect(approveCallCount).toBe(0);

    // Cancel hides the form without making a request.
    await confirmForm.getByRole('button', { name: /Cancel/i }).click();
    await expect(confirmForm).toBeHidden({ timeout: 2_000 });
    expect(approveCallCount).toBe(0);

    // Click Approve again → Merge to main → final card appears.
    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    await expect(confirmForm).toBeVisible({ timeout: 3_000 });
    await confirmForm.getByRole('button', { name: /Merge to main/i }).click();
    await expect(page.locator('.ship-review-card--final')).toBeVisible({ timeout: 5_000 });
    expect(approveCallCount).toBe(1);
    cleanup();
  });

  // ── STA-637: real Approve & merge deletes the task branch ─────────────────
  // Not route-faked: the real server merges into the bare remote and deletes
  // staypoint/<taskId> there, and the final card reports it.
  test('Approve & merge deletes the task branch and the final card shows Branch deleted', async ({ boardPage: page, request }) => {
    const { task, repoDir, cleanup } = await createShipReviewTask(request, 'Approve deletes branch');
    const upsert = await upsertShipReview(request, task.id);
    expect(upsert.ok(), `upsert failed: ${await upsert.text()}`).toBeTruthy();

    const branch = `staypoint/${task.id}`;
    const bare = execFileSync('git', ['-C', repoDir, 'remote', 'get-url', 'origin']).toString().trim();
    const remoteBranches = () => execFileSync('git', ['-C', bare, 'branch', '--list', branch]).toString().trim();
    expect(remoteBranches()).not.toBe('');

    await gotoTaskPage(page, task);
    const card = page.locator('.ship-review-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    await reviewActions(page).getByRole('button', { name: /Approve/i }).click();
    await reviewActions(page).getByRole('button', { name: /Merge to main/i }).click();

    const finalCard = page.locator('.ship-review-card--final');
    await expect(finalCard.locator('.ship-review-status-badge')).toContainText(/Approved/i, { timeout: 15_000 });
    await expect(finalCard.locator('.ship-review-branch-deleted')).toHaveText('Branch deleted');
    await expect(finalCard).toContainText(branch);
    expect(remoteBranches()).toBe('');

    // Survives a reload: the outcome is persisted on the card.
    await page.reload();
    await expect(page.locator('.ship-review-card--final .ship-review-branch-deleted')).toHaveText('Branch deleted', { timeout: 10_000 });
    cleanup();
  });

});

