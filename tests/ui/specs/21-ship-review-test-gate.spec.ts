import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { test, expect, gotoTaskPage, recordTaskBase } from '../fixtures';
import type { APIRequestContext, Page, Route } from '@playwright/test';

// STA-734: the merge test gate on the ship review card. Most specs serve the
// test-coverage report and the merge responses by route so each warning type
// and the bypass flow render deterministically; the last spec runs the whole
// bypass against the real server and checks the backlog task it files.
// Detector rules, audit and dedupe are covered by the Go tests
// (internal/testgate, handlers_ship_review_testgate_test.go).

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';
const reviewActions = (page: Page) => page.locator('#task-page-content .task-page-header');

type ShipTask = { id: string; organization: string; project: string; [k: string]: unknown };

// createCardTask makes a real repo whose task branch changes `files`, and a
// pending card pinned to it.
async function createCardTask(request: APIRequestContext, label: string, files: Record<string, string> = { 'task.txt': 'task work\n' }) {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'sta734-e2e-'));
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
  // The server's merge commit needs an identity of its own.
  run(['config', 'user.email', 't@t.com']);
  run(['config', 'user.name', 'test']);
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
  for (const [p, body] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(work, p)), { recursive: true });
    fs.writeFileSync(path.join(work, p), body);
  }
  run(['add', '.']);
  run(['commit', '-m', 'task: change']);
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

type Warning = { kind: string; blocking: boolean; message: string; items?: string[] };

function report(headSHA: string, warnings: Warning[], extra: Record<string, unknown> = {}) {
  const missing: Record<string, string> = { no_ci: 'no CI', no_tests: 'no tests changed or added', uncovered: 'changed code with 0% coverage' };
  return {
    head_sha: headSHA, exempt_only: false, sources: ['calc/calc.go'], tests: [], exempt: [], test_workflows: [],
    untested_sources: ['calc/calc.go'], coverage: { available: false, note: 'no CI run for this commit' },
    warnings, blocking: warnings.some((w) => w.blocking),
    missing: warnings.filter((w) => w.blocking).map((w) => missing[w.kind] || w.kind), ...extra,
  };
}

const NO_CI: Warning = { kind: 'no_ci', blocking: true, message: 'This repo has no CI. Nothing checked this change automatically.' };
const NO_TESTS: Warning = { kind: 'no_tests', blocking: true, message: 'No tests changed or added. These changed source files have no test change:', items: ['calc/calc.go', 'web/app.js'] };
const NO_COVERAGE: Warning = { kind: 'no_coverage_data', blocking: false, message: "No coverage data: no CI run for this commit. Can't tell which changed lines run under tests." };
const UNCOVERED: Warning = { kind: 'uncovered', blocking: true, message: 'Changed code no test runs (0% coverage in CI artifact go-coverage):', items: ['calc/calc.go: functions Div; lines 10-11, 13'] };
const UNTESTED_SOURCES: Warning = { kind: 'untested_sources', blocking: false, message: 'Tests changed, but these source files have no matching test change:', items: ['web/app.js'] };

async function fakeTestCoverage(page: Page, taskId: string, body: unknown) {
  await page.route(`**/api/tasks/${encodeURIComponent(taskId)}/ship-review/test-coverage`, (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) }));
}

const TEST_TASK = { id: 'task-gap0001', name: 'Add tests for calc/calc.go (merged untested at `abc1234`)', url: '/tasks/STA/ui-e2e/task-gap0001', created: true };

test.describe('ship review merge test gate (STA-734)', () => {
  // Each spec builds a real repo and task first; slow on a loaded host.
  test.describe.configure({ timeout: 90_000 });

  test('no CI and no tests: both warnings block Approve; the no-coverage note says so', async ({ page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate no CI');
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [NO_CI, NO_TESTS, NO_COVERAGE]) });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'blocking', { timeout: 10_000 });
    await expect(sec.locator('.ship-review-section-title')).toContainText('Test coverage');
    await expect(sec.locator('.ship-review-test-coverage-status')).toHaveText('not tested: no CI, no tests changed or added');
    const warnings = sec.locator('.ship-review-test-warning');
    await expect(warnings).toHaveCount(3);
    await expect(warnings.nth(0)).toHaveAttribute('data-kind', 'no_ci');
    await expect(warnings.nth(0)).toContainText('This repo has no CI. Nothing checked this change automatically.');
    await expect(warnings.nth(0)).toHaveClass(/ship-review-test-warning--blocking/);
    await expect(warnings.nth(1)).toHaveAttribute('data-kind', 'no_tests');
    await expect(warnings.nth(1).locator('.ship-review-test-warning-item')).toHaveText(['calc/calc.go', 'web/app.js']);
    await expect(warnings.nth(2)).toHaveAttribute('data-kind', 'no_coverage_data');
    await expect(warnings.nth(2)).not.toHaveClass(/ship-review-test-warning--blocking/);
    await expect(warnings.nth(2)).toContainText('No coverage data: no CI run for this commit');

    const approve = reviewActions(page).locator('.ship-review-approve-btn');
    await expect(approve).toBeDisabled();
    await expect(approve).toHaveAttribute('title', /Not tested/);
    await expect(sec.getByRole('button', { name: 'Merge without tests' })).toBeVisible();
    cleanup();
  });

  // task-a5c42165: a direct-mode card has no PR, so CI never ran on it. That
  // is a note with a one-click Open PR, not a block.
  test('no CI run on a direct card: non-blocking note, Open PR sends open_pr_for_ci', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate no CI run');
    const NO_CI_RUN: Warning = { kind: 'no_ci_run', blocking: false, message: 'No CI run for this commit: open a PR to run CI.' };
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [NO_CI_RUN, NO_COVERAGE], {
      tests: ['calc/calc_test.go'], untested_sources: [], test_workflows: ['.github/workflows/ci.yml'],
    }) });
    let approveBody: Record<string, unknown> | null = null;
    await page.route('**/ship-review/approve', async (route) => {
      approveBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        merge_mode: 'pr_merge', pr_number: 7, pr_url: 'https://github.com/o/r/pull/7',
        card: { status: 'pending', head_sha: headSHA, merge_mode: 'pr_merge', pr_number: 7, pr_url: 'https://github.com/o/r/pull/7', effective_merge_mode: 'pr_merge', files_changed: [], test_steps: [] },
      }) });
    });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'ok', { timeout: 10_000 });
    const note = sec.locator('.ship-review-test-warning[data-kind="no_ci_run"]');
    await expect(note).toContainText('No CI run for this commit: open a PR to run CI.');
    await expect(note).not.toHaveClass(/ship-review-test-warning--blocking/);
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeEnabled();
    await expect(sec.getByRole('button', { name: 'Merge without tests' })).toBeHidden();

    await note.getByRole('button', { name: 'Open PR' }).click();
    await expect.poll(() => approveBody, { timeout: 10_000 }).not.toBeNull();
    expect(approveBody).toMatchObject({ head_sha: headSHA, open_pr_for_ci: true });
    cleanup();
  });

  test('coverage report: uncovered changed code blocks; partial test change is a hint only', async ({ page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate uncovered');
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [UNTESTED_SOURCES, UNCOVERED], {
      tests: ['calc/calc_test.go'], coverage: { available: true, source: 'CI artifact go-coverage' },
    }) });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'blocking', { timeout: 10_000 });
    const hint = sec.locator('.ship-review-test-warning[data-kind="untested_sources"]');
    await expect(hint).not.toHaveClass(/ship-review-test-warning--blocking/);
    await expect(hint.locator('.ship-review-test-warning-item')).toHaveText(['web/app.js']);
    const unc = sec.locator('.ship-review-test-warning[data-kind="uncovered"]');
    await expect(unc).toHaveClass(/ship-review-test-warning--blocking/);
    await expect(unc).toContainText('0% coverage in CI artifact go-coverage');
    await expect(unc.locator('.ship-review-test-warning-item')).toHaveText(['calc/calc.go: functions Div; lines 10-11, 13']);
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeDisabled();
    cleanup();
  });

  test('docs-only change (real report): exempt, Approve enabled, no bypass button', async ({ page, request }) => {
    const { task, cleanup } = await createCardTask(request, 'Gate exempt');
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'ok', { timeout: 15_000 });
    await expect(sec).toContainText('Docs, config or style only: no tests needed.');
    await expect(sec.getByRole('button', { name: 'Merge without tests' })).toBeHidden();
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeEnabled();
    cleanup();
  });

  test('gate check fails: fail closed with Retry and the bypass offered', async ({ page, request }) => {
    const { task, cleanup } = await createCardTask(request, 'Gate error');
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review/test-coverage`, (route: Route) =>
      route.fulfill({ status: 504, contentType: 'application/json', body: JSON.stringify({ error: 'test_gate_error', message: 'could not check test coverage: git timed out' }) }));
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'error', { timeout: 10_000 });
    await expect(sec).toContainText('git timed out');
    await expect(sec.getByRole('button', { name: 'Retry' })).toBeVisible();
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeDisabled();
    await expect(sec.getByRole('button', { name: 'Merge without tests' })).toBeVisible();
    cleanup();
  });

  test('bypass: extra confirm names what is missing, sends merge_without_tests, then links the created task', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate bypass');
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [NO_CI, NO_TESTS, NO_COVERAGE]) });
    let approveBody: Record<string, unknown> | null = null;
    const mainSHA = 'abc123def4567890abc123def4567890abc12345';
    await page.route('**/ship-review/approve', async (route) => {
      approveBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        main_sha: mainSHA, branch_deleted: true, merged_without_tests: true, test_task: TEST_TASK,
        card: { status: 'approved', head_sha: headSHA, main_sha: mainSHA },
      }) });
    });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'blocking', { timeout: 10_000 });
    await sec.getByRole('button', { name: 'Merge without tests' }).click();
    const confirm = reviewActions(page).locator('.ship-review-merge-without-tests-confirm');
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText(`Merge ${headSHA.slice(0, 12)} → main without tests? Missing:`);
    await expect(confirm.locator('.ship-review-merge-without-tests-missing-item')).toHaveText(['no CI', 'no tests changed or added']);
    await expect(confirm).toContainText('a backlog task to add the tests is created');
    expect(approveBody).toBeNull(); // nothing sent before the extra confirm
    await confirm.locator('.ship-review-merge-without-tests-reason').fill('hotfix, tests to follow');
    await confirm.getByRole('button', { name: 'Merge without tests' }).click();

    const final = page.locator('.ship-review-card--final');
    await expect(final).toBeVisible({ timeout: 10_000 });
    expect(approveBody).toEqual({ merge_without_tests: true, merge_without_tests_reason: 'hotfix, tests to follow', head_sha: headSHA });
    const link = final.locator('.ship-review-test-task-link');
    await expect(link).toHaveText(TEST_TASK.name);
    await expect(link).toHaveAttribute('href', TEST_TASK.url);
    await expect(final.locator('.ship-review-test-task-note')).toHaveText('Merged without tests: backlog task created.');
    cleanup();
  });

  test('server says untested (409): the bypass confirm opens with its list', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate 409');
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, []) });
    await page.route('**/ship-review/approve', (route) => route.fulfill({
      status: 409, contentType: 'application/json', body: JSON.stringify({
        error: 'untested', message: 'this change is not tested', missing: ['no CI'], test_coverage: report(headSHA, [NO_CI]),
      }),
    }));
    await gotoTaskPage(page, task);

    const approve = reviewActions(page).locator('.ship-review-approve-btn');
    await expect(approve).toBeEnabled({ timeout: 10_000 });
    await approve.click();
    await reviewActions(page).getByRole('button', { name: /Merge to main/i }).click();
    const confirm = reviewActions(page).locator('.ship-review-merge-without-tests-confirm');
    await expect(confirm).toBeVisible({ timeout: 10_000 });
    await expect(confirm.locator('.ship-review-merge-without-tests-missing-item')).toHaveText(['no CI']);
    await expect(page.locator('.ship-review-test-coverage')).toHaveAttribute('data-state', 'blocking');
    cleanup();
  });

  test('pr_merge: green checks still wait for the gate; bypass merges the PR without override', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate pr_merge');
    const PR_URL = 'https://github.com/o/r/pull/7';
    const GREEN = [{ name: 'Lint', bucket: 'pass', state: 'SUCCESS', link: '', duration_sec: 5 }];
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review`, async (route: Route) => {
      if (route.request().method() !== 'GET') return route.continue();
      const resp = await route.fetch();
      await route.fulfill({ response: resp, json: {
        ...(await resp.json()), merge_mode: 'pr_merge', effective_merge_mode: 'pr_merge', pr_number: 7, pr_url: PR_URL,
        pr_checks_sha: headSHA, pr_checks_at: '2026-10-05T10:00:00Z', pr_checks: GREEN, pr_checks_summary: 'passed',
      } });
    });
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review/checks`, (route: Route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ pr_number: 7, pr_url: PR_URL, head_sha: headSHA, pr_head_sha: headSHA, head_moved: false, checks: GREEN, summary: 'passed' }),
    }));
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [NO_TESTS, NO_COVERAGE], { test_workflows: ['.github/workflows/lint.yml'] }) });
    let mergeBody: Record<string, unknown> | null = null;
    await page.route('**/ship-review/merge', async (route) => {
      mergeBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        main_sha: 'feedface00', test_task: { ...TEST_TASK, name: 'Add tests for calc/calc.go (merged untested in PR #7 `abc1234`)' },
        card: { status: 'approved', head_sha: headSHA, main_sha: 'feedface00', pr_number: 7, pr_url: PR_URL, pr_checks: GREEN },
      }) });
    });
    await gotoTaskPage(page, task);

    await expect(page.locator('.ship-review-card .ship-review-status-badge')).toHaveText('Checks passed', { timeout: 10_000 });
    await expect(page.locator('.ship-review-test-coverage')).toHaveAttribute('data-state', 'blocking');
    const merge = reviewActions(page).locator('.ship-review-merge-btn');
    await expect(merge).toBeDisabled();
    await expect(merge).toHaveAttribute('title', /Not tested/);

    await page.locator('.ship-review-test-coverage').getByRole('button', { name: 'Merge without tests' }).click();
    const confirm = reviewActions(page).locator('.ship-review-merge-without-tests-confirm');
    await expect(confirm).toContainText(`Merge PR #7 ${headSHA.slice(0, 12)} → main without tests?`);
    await expect(confirm.locator('.ship-review-merge-without-tests-missing-item')).toHaveText(['no tests changed or added']);
    await confirm.getByRole('button', { name: 'Merge without tests' }).click();

    const final = page.locator('.ship-review-card--final');
    await expect(final).toBeVisible({ timeout: 10_000 });
    expect(mergeBody).toEqual({ merge_without_tests: true, merge_without_tests_reason: '', head_sha: headSHA });
    await expect(final.locator('.ship-review-test-task-link')).toHaveText('Add tests for calc/calc.go (merged untested in PR #7 `abc1234`)');
    cleanup();
  });

  // STA-766 review: before the PR exists, pr_merge Approve only opens the PR
  // and the server doesn't gate it, so a bypass there would audit nothing and
  // file no task. The section shows the verdict and says Merge PR enforces it.
  test('pr_merge before the PR exists: no bypass, Approve opens the PR, the gate waits for Merge PR', async ({ boardPage: page, request }) => {
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate pr_merge no PR');
    await page.route(`**/api/tasks/${encodeURIComponent(task.id)}/ship-review`, async (route: Route) => {
      if (route.request().method() !== 'GET') return route.continue();
      const resp = await route.fetch();
      await route.fulfill({ response: resp, json: { ...(await resp.json()), merge_mode: 'pr_merge', effective_merge_mode: 'pr_merge', pr_number: 0 } });
    });
    const AMBIGUOUS: Warning = { kind: 'coverage_ambiguous', blocking: false, message: 'Coverage unknown: CI artifact lcov lists more than one file that could be each of these, so it can\'t tell which one is this file:', items: ['src/index.ts'] };
    await fakeTestCoverage(page, task.id, { head_sha: headSHA, report: report(headSHA, [NO_TESTS, AMBIGUOUS], { coverage: { available: true, source: 'CI artifact lcov', ambiguous: ['src/index.ts'] } }) });
    let approveBody: Record<string, unknown> | null = null;
    await page.route('**/ship-review/approve', async (route) => {
      approveBody = JSON.parse(route.request().postData() || '{}');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ merge_mode: 'pr_merge', pr_number: 9, pr_url: 'https://github.com/o/r/pull/9' }) });
    });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'blocking', { timeout: 10_000 });
    await expect(sec.locator('.ship-review-test-warning[data-kind="coverage_ambiguous"]')).toContainText('src/index.ts');
    await expect(sec).not.toContainText('every changed line runs under a test');
    await expect(sec.getByRole('button', { name: 'Merge without tests' })).toBeHidden();
    await expect(sec.locator('.ship-review-test-coverage-enforced')).toHaveText(
      'Not enforced yet: Merge PR, once the PR is open, waits for this check, and Merge without tests is offered there.');

    const approve = reviewActions(page).locator('.ship-review-approve-btn');
    await expect(approve).toHaveText('✓ Approve & open PR');
    await expect(approve).toBeEnabled();
    await approve.click();
    await reviewActions(page).locator('.ship-review-approve-confirm').getByRole('button', { name: 'Push & run checks' }).click();
    await expect.poll(() => approveBody, { timeout: 10_000 }).not.toBeNull();
    expect(approveBody).toEqual({ head_sha: headSHA });
    cleanup();
  });

  test('real server: source change with no tests merges via bypass and files one backlog task', async ({ boardPage: page, request }) => {
    test.setTimeout(120_000);
    const { task, headSHA, cleanup } = await createCardTask(request, 'Gate real bypass', {
      'calc/calc.go': 'package calc\n\nfunc Add(a, b int) int { return a + b }\n',
    });
    await gotoTaskPage(page, task);

    const sec = page.locator('.ship-review-test-coverage');
    await expect(sec).toHaveAttribute('data-state', 'blocking', { timeout: 30_000 });
    await expect(sec.locator('.ship-review-test-warning[data-kind="no_ci"]')).toContainText('This repo has no CI.');
    await expect(sec.locator('.ship-review-test-warning[data-kind="no_tests"] .ship-review-test-warning-item')).toHaveText(['calc/calc.go']);
    await expect(reviewActions(page).locator('.ship-review-approve-btn')).toBeDisabled();

    await sec.getByRole('button', { name: 'Merge without tests' }).click();
    const confirm = reviewActions(page).locator('.ship-review-merge-without-tests-confirm');
    await confirm.locator('.ship-review-merge-without-tests-reason').fill('e2e bypass');
    await confirm.getByRole('button', { name: 'Merge without tests' }).click();

    const final = page.locator('.ship-review-card--final');
    await expect(final.locator('.ship-review-status-badge')).toContainText(/Approved/i, { timeout: 20_000 });
    const link = final.locator('.ship-review-test-task-link');
    const want = `Add tests for calc/calc.go (merged untested at \`${headSHA.slice(0, 7)}\`)`;
    await expect(link).toHaveText(want);

    // The task is real, parked in the backlog of the same project.
    const href = await link.getAttribute('href');
    // The link is the canonical /STA-123/slug URL; the reference is its first segment.
    const testTaskRef = (href || '').split('/')[1] || '';
    expect(testTaskRef).toMatch(/^[A-Z]+-\d+$/);
    const res = await request.get('/api/tasks?status=all', { headers: { Authorization: `Bearer ${TOKEN}` } });
    expect(res.ok()).toBeTruthy();
    const t = ((await res.json()).tasks as Array<Record<string, unknown>>).find((x) => x.identifier === testTaskRef) || {};
    expect(t.name).toBe(want);
    expect(t.execution_stage).toBe('backlog');
    expect(t.project).toBe('ui-e2e');

    // After a reload the closed card still links it.
    await page.reload();
    await expect(page.locator('.ship-review-card--final .ship-review-test-task-link')).toHaveText(want, { timeout: 10_000 });
    cleanup();
  });
});
