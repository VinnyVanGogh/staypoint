import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { test, expect, gotoTaskPage, recordTaskBase } from '../fixtures';
import type { APIRequestContext, Page, Route } from '@playwright/test';

// STA-717: ship review card states for the PR merge modes. The card, its
// checks poll and the merge / send-back / approve responses are route-faked
// on top of a real task + card, so these prove the UI renders each state and
// sends the right request. Enforcement (head pin, override audit, GitHub's
// refusal) is covered by the Go tests in handlers_ship_review_pr_test.go.

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';
const reviewActions = (page: Page) => page.locator('#task-page-content .task-page-header');

type ShipTask = { id: string; organization: string; project: string; [k: string]: unknown };

async function createCardTask(request: APIRequestContext, label: string) {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'sta717-e2e-'));
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
  fs.writeFileSync(path.join(work, 'README.md'), 'init\n');
  run(['add', '.']);
  run(['commit', '-m', 'init']);
  execFileSync('git', ['init', '--bare', '-b', 'main', bare], { env, stdio: 'pipe' });
  run(['remote', 'add', 'origin', bare]);
  run(['push', 'origin', 'main']);

  const res = await request.post('/api/tasks', {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    data: { name: `${label} ${Date.now().toString(36)}`, organization: 'STA', project: 'ui-e2e', repo_path: work, git_branch: 'main' },
  });
  const task = (await res.json()) as ShipTask;
  await recordTaskBase(request, task.id, work);
  run(['checkout', '-b', `staypoint/${task.id}`]);
  fs.writeFileSync(path.join(work, 'task.txt'), 'task work\n');
  run(['add', '.']);
  run(['commit', '-m', 'task: add work file']);
  run(['push', 'origin', `staypoint/${task.id}`]);
  run(['checkout', 'main']);

  const up = await request.put(`/api/tasks/${encodeURIComponent(task.id)}/ship-review`, {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    data: { test_steps: ['1. Open the homepage'] },
  });
  expect(up.ok(), `upsert failed: ${await up.text()}`).toBeTruthy();
  const card = await up.json();
  const cleanup = () => { try { fs.rmSync(base, { recursive: true, force: true }); } catch {} };
  return { task: { ...task, organization: 'STA', project: 'ui-e2e' }, headSHA: card.head_sha as string, cleanup };
}

const PR_URL = 'https://github.com/o/r/pull/7';
const check = (name: string, bucket: string, state: string, extra: Record<string, unknown> = {}) => ({
  name, bucket, state, link: `https://github.com/o/r/actions/runs/11/job/${name.length}`, duration_sec: 42, ...extra,
});
const RUNNING = [check('Build & Test', 'pending', 'IN_PROGRESS'), check('Lint', 'pass', 'SUCCESS')];
const GREEN = [check('Build & Test', 'pass', 'SUCCESS', { duration_sec: 200 }), check('Lint', 'pass', 'SUCCESS')];
const RED = [check('Build & Test', 'fail', 'FAILURE'), check('Playwright UI Specs', 'pending', 'IN_PROGRESS'), check('Lint', 'pass', 'SUCCESS')];

// fakePRCard serves the real card as a pr_merge card waiting on CI, and the
// checks poll with the given checks.
async function fakePRCard(page: Page, taskId: string, headSHA: string, summary: string, checks: unknown[]) {
  await page.route(`**/api/tasks/${encodeURIComponent(taskId)}/ship-review`, async (route: Route) => {
    if (route.request().method() !== 'GET') return route.continue();
    const resp = await route.fetch();
    const card = await resp.json();
    await route.fulfill({
      response: resp,
      json: {
        ...card, merge_mode: 'pr_merge', effective_merge_mode: 'pr_merge', pr_number: 7, pr_url: PR_URL,
        pr_checks_sha: headSHA, pr_checks_at: '2026-10-05T10:00:00Z', pr_checks: checks, pr_checks_summary: summary,
      },
    });
  });
  await page.route(`**/api/tasks/${encodeURIComponent(taskId)}/ship-review/checks`, (route: Route) => route.fulfill({
    status: 200, contentType: 'application/json',
    body: JSON.stringify({ pr_number: 7, pr_url: PR_URL, head_sha: headSHA, pr_head_sha: headSHA, head_moved: false, checks, summary }),
  }));
}

test.describe('ship review PR merge mode card (STA-717)', () => {

  test('checks running: live per-check status, Merge off, warning with both buttons', async ({ page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode running');
    await fakePRCard(page, task.id, headSHA, 'running', RUNNING);
    await gotoTaskPage(page, task);

    const card = page.locator('.ship-review-card');
    await expect(card.locator('.ship-review-status-badge')).toHaveText('Checks running', { timeout: 10_000 });
    const link = card.locator('.ship-review-pr-link');
    await expect(link).toHaveText('PR #7');
    await expect(link).toHaveAttribute('href', PR_URL);

    const rows = card.locator('.ship-review-pr-check');
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toHaveAttribute('data-bucket', 'pending');
    await expect(rows.nth(0)).toContainText('Build & Test');
    await expect(rows.nth(0)).toContainText('in progress');
    await expect(rows.nth(0)).toContainText('42s');
    await expect(rows.nth(1).locator('.ship-review-pr-check-link')).toHaveAttribute('href', /actions\/runs\/11\/job\/4$/);

    const merge = reviewActions(page).locator('.ship-review-merge-btn');
    await expect(merge).toBeVisible();
    await expect(merge).toBeDisabled();
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toHaveCount(0);

    const warn = card.locator('.ship-review-pr-warning');
    await expect(warn).toBeVisible();
    await expect(warn).toContainText('1 check still running');
    await expect(warn.locator('.ship-review-pr-warning-item')).toHaveText(['⏳ Build & Test (in progress)']);
    await expect(warn.getByRole('button', { name: 'Merge anyway' })).toBeVisible();
    await expect(warn.getByRole('button', { name: /Send failures to agent/ })).toBeVisible();
    cleanup();
  });

  test('all green: Merge turns on, confirms PR #N <sha> → main, merges pinned', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode green');
    await fakePRCard(page, task.id, headSHA, 'passed', GREEN);
    let mergeBody: Record<string, unknown> | null = null;
    const mainSHA = 'abc123def4567890abc123def4567890abc12345';
    await page.route('**/ship-review/merge', async (route) => {
      mergeBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        main_sha: mainSHA, branch_deleted: true,
        card: { status: 'approved', head_sha: headSHA, main_sha: mainSHA, branch: `staypoint/${task.id}`, branch_deleted: true,
          merge_mode: 'pr_merge', pr_number: 7, pr_url: PR_URL, pr_checks: GREEN, pr_checks_summary: 'passed' },
      }) });
    });
    await gotoTaskPage(page, task);

    const card = page.locator('.ship-review-card');
    await expect(card.locator('.ship-review-status-badge')).toHaveText('Checks passed', { timeout: 10_000 });
    await expect(card.locator('.ship-review-pr-warning')).toBeHidden();
    const merge = reviewActions(page).locator('.ship-review-merge-btn');
    await expect(merge).toBeEnabled();
    await merge.click();
    const confirm = reviewActions(page).locator('.ship-review-merge-confirm');
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText(`Merge PR #7 ${headSHA.slice(0, 12)} → main`);
    await confirm.getByRole('button', { name: 'Merge PR' }).click();

    const final = page.locator('.ship-review-card--final');
    await expect(final).toBeVisible({ timeout: 10_000 });
    await expect(final).toContainText(mainSHA.slice(0, 12));
    await expect(final.locator('.ship-review-pr-link')).toHaveAttribute('href', PR_URL);
    expect(mergeBody).toEqual({ head_sha: headSHA });
    cleanup();
  });

  test('failed: warning lists failing checks; Merge anyway needs an extra confirm and sends override', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode failed');
    await fakePRCard(page, task.id, headSHA, 'failed', RED);
    let mergeBody: Record<string, unknown> | null = null;
    await page.route('**/ship-review/merge', async (route) => {
      mergeBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        main_sha: 'feedface00', card: { status: 'approved', head_sha: headSHA, main_sha: 'feedface00', pr_number: 7, pr_url: PR_URL, pr_checks: RED },
      }) });
    });
    await gotoTaskPage(page, task);

    const card = page.locator('.ship-review-card');
    await expect(card.locator('.ship-review-status-badge')).toHaveText('Checks failed', { timeout: 10_000 });
    await expect(reviewActions(page).locator('.ship-review-merge-btn')).toBeDisabled();
    const warn = card.locator('.ship-review-pr-warning');
    await expect(warn).toContainText('2 checks not green');
    await expect(warn.locator('.ship-review-pr-warning-item')).toHaveText([
      '✗ Build & Test (failure)', '⏳ Playwright UI Specs (in progress)',
    ]);

    await warn.getByRole('button', { name: 'Merge anyway' }).click();
    const confirm = reviewActions(page).locator('.ship-review-merge-anyway-confirm');
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText(`Merge PR #7 ${headSHA.slice(0, 12)} → main with 2 checks not green? This is logged as an override.`);
    expect(mergeBody).toBeNull(); // nothing sent before the extra confirm
    await confirm.locator('.ship-review-override-reason').fill('known flaky e2e');
    await confirm.getByRole('button', { name: 'Merge anyway' }).click();
    await expect(page.locator('.ship-review-card--final')).toBeVisible({ timeout: 10_000 });
    expect(mergeBody).toEqual({ head_sha: headSHA, override: true, override_reason: 'known flaky e2e' });
    cleanup();
  });

  test('failed: Send failures to agent pre-fills the Send back with names, failing lines and log links', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode send failures');
    await fakePRCard(page, task.id, headSHA, 'failed', RED);
    const comment = 'CI is not green on PR #7.\n\n### ✗ Build & Test\nLog: https://github.com/o/r/actions/runs/11/job/12\n```\n--- FAIL: TestMergeMode (0.01s)\n```\n';
    await page.route('**/ship-review/check-failures', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ comment, failures: [{ name: 'Build & Test', link: 'https://github.com/o/r/actions/runs/11/job/12', lines: ['--- FAIL: TestMergeMode (0.01s)'] }] }),
    }));
    let sendBody: Record<string, unknown> | null = null;
    await page.route('**/ship-review/send-back', async (route) => {
      sendBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'ok' }) });
    });
    await gotoTaskPage(page, task);

    const card = page.locator('.ship-review-card');
    await card.locator('.ship-review-pr-warning').getByRole('button', { name: /Send failures to agent/ }).click();
    const textarea = reviewActions(page).locator('.ship-review-form-textarea').first();
    await expect(textarea).toBeVisible();
    await expect(textarea).toHaveValue(comment);
    await expect(reviewActions(page)).toContainText('CI failures for the agent');
    await reviewActions(page).locator('.ship-review-sendback-submit-btn').click();
    await expect(page.locator('.ship-review-card--working')).toBeVisible({ timeout: 10_000 });
    expect(sendBody).toEqual({ comment: comment.trim(), ci_failures: true });
    cleanup();
  });

  test("GitHub's refusal is shown verbatim", async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode refused');
    await fakePRCard(page, task.id, headSHA, 'passed', GREEN);
    const reason = 'GraphQL: At least 1 approving review is required by reviewers with write access. (mergePullRequest)';
    await page.route('**/ship-review/merge', (route) => route.fulfill({
      status: 422, contentType: 'application/json', body: JSON.stringify({ error: 'github_refused', message: reason }),
    }));
    await gotoTaskPage(page, task);

    await reviewActions(page).locator('.ship-review-merge-btn').click();
    await reviewActions(page).locator('.ship-review-merge-confirm').getByRole('button', { name: 'Merge PR' }).click();
    await expect(reviewActions(page).locator('.ship-review-err-banner .ship-review-github-refusal-text')).toHaveText(reason, { timeout: 10_000 });
    await expect(page.locator('.ship-review-card--final')).toHaveCount(0);
    cleanup();
  });

  test('Open PR mode: Approve says it opens a PR, and the closed card shows the PR and its checks', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'PR mode open_pr');
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review`, async (route: Route) => {
      if (route.request().method() !== 'GET') return route.continue();
      const resp = await route.fetch();
      await route.fulfill({ response: resp, json: { ...(await resp.json()), effective_merge_mode: 'open_pr' } });
    });
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review/checks`, (route: Route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ head_moved: false, checks: GREEN, summary: 'passed', pr_head_sha: headSHA }),
    }));
    await page.route('**/ship-review/approve', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({
        merge_mode: 'open_pr', pr_number: 7, pr_url: PR_URL,
        card: { status: 'approved', head_sha: headSHA, main_sha: '', merge_mode: 'open_pr', pr_number: 7, pr_url: PR_URL,
          pr_checks: RUNNING, pr_checks_summary: 'running' },
      }),
    }));
    await gotoTaskPage(page, task);

    const approve = reviewActions(page).locator('.ship-review-approve-btn');
    await expect(approve).toHaveText('✓ Approve & open PR', { timeout: 10_000 });
    await approve.click();
    const confirm = reviewActions(page).locator('.ship-review-approve-confirm');
    await expect(confirm).toContainText(`Push ${headSHA.slice(0, 12)} and open a PR → main`);
    await expect(confirm).toContainText('StayPoint will not merge it.');
    await confirm.getByRole('button', { name: 'Open PR' }).click();

    const final = page.locator('.ship-review-card--final');
    await expect(final).toBeVisible({ timeout: 10_000 });
    await expect(final.locator('.ship-review-pr-link')).toHaveAttribute('href', PR_URL);
    await expect(final).toContainText('StayPoint does not merge it');
    // The poll replaces the running snapshot with GitHub's current checks.
    await expect(final.locator('.ship-review-pr-summary')).toHaveText('Checks passed', { timeout: 10_000 });
    await expect(final.locator('.ship-review-pr-check')).toHaveCount(2);
    cleanup();
  });
});
