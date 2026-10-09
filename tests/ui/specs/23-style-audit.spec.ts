/**
 * spec 23: no control renders with the browser's default look (task-a2401862)
 *
 * Visits every sidebar view, the full task page and each of its tabs, the
 * task drawer, Settings with a saved dev environment config, and the New Task,
 * Fleet and content modals. On each it looks for buttons, text inputs,
 * selects and textareas whose computed background+border or font is still
 * Chromium's default (see tests/ui/style-audit.ts) and fails if it finds any.
 *
 * STAYPOINT_STYLE_AUDIT_DIR=dir also writes a screenshot and a findings.jsonl
 * line per finding there; docs/ui-style-audit.md was built from that output.
 */

import type { Page } from '@playwright/test';
import {
  test, expect, gotoTaskPage, openTaskPanelTab, addTaskDocument, type StayPointAPI, type Task,
} from '../fixtures';
import { findUnstyledControls, recordFindings, describeFindings } from '../style-audit';

const VIEWPORT = { width: 1512, height: 900 };
const REPORT_DIR = process.env.STAYPOINT_STYLE_AUDIT_DIR || '';

const VIEWS = [
  'overview', 'projects', 'agents', 'kanban', 'recent-tasks', 'task-status', 'all-tasks',
  'cost', 'boss', 'checklist', 'gates', 'pull-requests', 'settings', 'routines',
  'artifacts', 'skills', 'connectors', 'audit', 'logs',
] as const;

async function audit(page: Page, where: string) {
  // Let async renders (fetch-then-paint sections) settle before measuring.
  await page.waitForTimeout(600);
  const found = await findUnstyledControls(page);
  if (REPORT_DIR) await recordFindings(page, where, found, REPORT_DIR);
  // Soft, so one test reports every tab/state it walks, not just the first.
  expect.soft(found, `unstyled controls on ${where}:\n${describeFindings(found)}`).toEqual([]);
}

async function seedParentWithChild(api: StayPointAPI): Promise<Task> {
  const parent = await api.createTask('style audit parent');
  await api.createTask('style audit child', { parent_id: parent.id });
  await api.upsertShipReview(parent.id);
  addTaskDocument(parent.id, 'plan', '# Plan\n\nStyle audit document.');
  return parent;
}

test.describe('style audit: no browser-default controls', () => {
  test.use({ viewport: VIEWPORT });

  for (const view of VIEWS) {
    test(`view ${view}`, async ({ page }) => {
      await page.goto(view === 'overview' ? '/' : `/${view}`);
      await expect(page.locator(`#view-${view}`)).toHaveClass(/\bactive\b/);
      await audit(page, `/${view}`);
    });
  }

  test('task page and every tab', async ({ page, api }) => {
    const task = await seedParentWithChild(api);
    await gotoTaskPage(page, task);
    await expect(page.locator('.run-children-btn')).toBeVisible();
    for (const tab of ['Review', 'Diff', 'Migrations', 'Brief', 'Artifacts'] as const) {
      await openTaskPanelTab(page, tab);
      await audit(page, `task page / ${tab} tab`);
    }
  });

  test('task drawer', async ({ page, api }) => {
    const task = await seedParentWithChild(api);
    await page.goto('/task-status');
    await page.locator('#ts-task-table').getByText(task.name).click();
    const panel = page.locator('#detail-panel');
    await expect(panel.locator('.task-page-header .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    for (const tab of ['Review', 'Diff', 'Migrations', 'Brief', 'Artifacts'] as const) {
      await openTaskPanelTab(page, tab, '#panel-content');
      await audit(page, `task drawer / ${tab} tab`);
    }
  });

  test('settings with a saved dev environment config', async ({ boardPage: page }) => {
    await page.goto('/settings');
    const form = page.locator('.settings-dev-config-form');
    await form.locator('input').first().fill(`/tmp/style-audit-${Date.now().toString(36)}`);
    await form.locator('input').nth(1).fill('npx vite --port 5173 --strictPort');
    await form.getByRole('button', { name: 'Save config' }).click();
    await expect(page.locator('.settings-dev-config-status')).toHaveText(/Saved/, { timeout: 15_000 });
    await expect(page.locator('.settings-dev-config-row').first()).toBeVisible();
    await page.locator('.settings-dev-config-form').scrollIntoViewIfNeeded();
    await audit(page, '/settings (dev environment config)');
  });

  test('New Task modal', async ({ page }) => {
    await page.goto('/task-status');
    await page.getByRole('button', { name: '+ New Task' }).click();
    await expect(page.locator('#create-task-modal')).toBeVisible();
    await audit(page, 'New Task modal');
  });

  test('Fleet modal, every tab', async ({ page }) => {
    await page.goto('/');
    for (const tab of ['organizations', 'tasks', 'agents']) {
      await page.evaluate((t) => (window as unknown as { openFleetInfoModal: (t: string) => void }).openFleetInfoModal(t), tab);
      await expect(page.locator('#fleet-modal')).not.toHaveClass(/\bhidden\b/);
      await audit(page, `Fleet modal / ${tab}`);
      await page.locator('#fleet-modal-close').click();
    }
  });

  test('content modal', async ({ page }) => {
    await page.goto('/');
    await page.evaluate(() => {
      const body = document.createElement('pre');
      body.textContent = 'Long content';
      (window as unknown as { openContentModal: (a: string, b: Node) => void }).openContentModal('Style audit', body);
    });
    await expect(page.locator('.content-modal')).toBeVisible();
    await audit(page, 'content modal');
  });
});
