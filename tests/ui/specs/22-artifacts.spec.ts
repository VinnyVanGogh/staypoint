import { execFileSync } from 'node:child_process';
import { test, expect, gotoTaskPage, openTaskPanelTab } from '../fixtures';

// Task documents (`staypoint task doc add`) show in the UI: the task page's
// Artifacts tab lists every doc key, renders the latest version as Markdown,
// switches versions and downloads; the sidebar Artifacts page lists documents
// across tasks, filterable by org, task and kind.

/** Inserts one version of a task document straight into the throwaway DB. */
function addDoc(taskId: string, key: string, version: number, content: string) {
  const db = process.env.STAYPOINT_UI_DB;
  if (!db) throw new Error('STAYPOINT_UI_DB not set: run via scripts/ui-e2e.sh');
  const q = (s: string) => `'${s.replace(/'/g, "''")}'`;
  execFileSync('sqlite3', [db,
    `INSERT INTO task_documents (task_id, doc_key, version, content) VALUES (${q(taskId)}, ${q(key)}, ${version}, ${q(content)});`]);
}

test('task page Artifacts tab renders documents with versions and download', async ({ page, api }) => {
  const task = await api.createTask('artifacts-tab', { work_kind: 'planning' });
  addDoc(task.id, 'plan', 1, '# Plan one\n\n- old step');
  addDoc(task.id, 'plan', 2, '# Plan two\n\n- new step\n\n<script>window.__pwned = 1</script>');
  addDoc(task.id, 'architecture', 1, '## Arch notes');

  await gotoTaskPage(page, task);
  await openTaskPanelTab(page, 'Artifacts');
  const panel = page.locator('#task-page-tabpanel-artifacts');
  await expect(page.locator('#task-page-tab-artifacts .task-page-tab-count')).toHaveText('2');
  await expect(panel.locator('.artifact-key-btn')).toHaveText([/architecture/, /plan\s*v2/]);

  // Opens on the first key; picking plan renders its latest version as Markdown.
  await expect(panel.locator('.artifact-viewer-body h2')).toHaveText('Arch notes');
  await panel.locator('.artifact-key-btn', { hasText: 'plan' }).click();
  const body = panel.locator('.artifact-viewer-body');
  await expect(body.locator('h1')).toHaveText('Plan two');
  await expect(body.locator('li')).toHaveText('new step');
  await expect(body).toContainText('<script>');
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();

  // Version history: v1 is still readable.
  const versions = panel.locator('.artifact-version-select');
  await expect(versions.locator('option')).toHaveCount(2);
  await versions.selectOption('1');
  await expect(body.locator('h1')).toHaveText('Plan one');
  await expect(panel.locator('.artifact-viewer-meta')).toContainText('Version 1 of 2');

  const [download] = await Promise.all([
    page.waitForEvent('download'),
    panel.locator('.artifact-download-btn').click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/-plan-v1\.md$/);
});

test('task page Artifacts tab says so when a task has no documents', async ({ page, api }) => {
  const task = await api.createTask('artifacts-empty');
  await gotoTaskPage(page, task);
  await openTaskPanelTab(page, 'Artifacts');
  await expect(page.locator('#task-page-tabpanel-artifacts')).toContainText('No documents on this task');
  await expect(page.locator('#task-page-tab-artifacts .task-page-tab-count')).toHaveText('');
});

test('Artifacts page lists documents across tasks, filters, and opens one', async ({ page, api }) => {
  const a = await api.createTask('artifacts-acme', { organization: 'AcmeArt' });
  const b = await api.createTask('artifacts-other', { organization: 'OtherArt' });
  addDoc(a.id, 'plan', 1, '# Acme plan');
  addDoc(a.id, 'bundle', 1, 'bundle body');
  addDoc(b.id, 'plan', 1, '# Other plan');

  await page.goto('/artifacts');
  const rows = page.locator('#artifacts-container .artifacts-row');
  const ours = rows.filter({ hasText: /artifacts-(acme|other)/ });
  await expect(ours).toHaveCount(3, { timeout: 20_000 });

  await page.locator('#artifacts-org-filter').selectOption('AcmeArt');
  await expect(rows).toHaveCount(2);
  await page.locator('#artifacts-kind-filter').selectOption('plan');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText(a.name);

  await page.locator('#artifacts-org-filter').selectOption('all');
  await page.locator('#artifacts-task-filter').selectOption(b.id);
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText(b.name);

  // A row opens the task on its Artifacts tab with that document showing.
  await rows.first().click();
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(b.name, { timeout: 20_000 });
  await expect(page.locator('#task-page-tab-artifacts')).toHaveAttribute('aria-selected', 'true');
  await expect(page.locator('#task-page-tabpanel-artifacts .artifact-viewer-body h1')).toHaveText('Other plan');
});

// task-e3fe2c0b: a document opens in full (the Brief's dialog) from the task
// page viewer and straight from the Artifacts page, not only Download.
test('artifacts open in the full view from the task page and the Artifacts page', async ({ page, api }) => {
  const task = await api.createTask('artifacts-full-view');
  addDoc(task.id, 'plan', 1, '# Full plan\n\n- everything\n\n<script>window.__pwned = 1</script>');

  await gotoTaskPage(page, task);
  await openTaskPanelTab(page, 'Artifacts');
  await page.locator('#task-page-tabpanel-artifacts .artifact-viewer-head .long-content-open').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog.locator('h1')).toHaveText('Full plan');
  await expect(dialog.locator('li')).toHaveText('everything');
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);

  await page.goto('/artifacts');
  const row = page.locator('#artifacts-container .artifacts-row', { hasText: 'artifacts-full-view' });
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  await row.getByRole('button', { name: 'View', exact: true }).click();
  await expect(page.getByRole('dialog').locator('h1')).toHaveText('Full plan');
  // View stays on the Artifacts page instead of opening the task.
  await expect(page).toHaveURL(/\/artifacts/);
  expect(await page.evaluate(() => (window as any).__pwned)).toBeUndefined();
});
