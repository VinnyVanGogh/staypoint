/**
 * spec 20: the drawer (peek view) renders the task page layout (STA-700)
 *
 * The drawer (#detail-panel) used to be its own one-column layout with the
 * description first. It now renders the full page's layout through
 * renderTaskPage(): header with status and actions, stats strip, timeline,
 * and the Review / Diff / Migrations / Brief / Artifacts tabs, stacked to fit the drawer
 * and back in two columns once #panel-expand makes it wide.
 *
 * Also covered: Escape in a drawer dialog (content or file diff) closes only
 * the dialog, live run steps reach the drawer's timeline, and a slow drawer
 * load can't render a second copy of the layout after ↗ opened the full page.
 */

import type { Locator, Page } from '@playwright/test';
import {
  test, expect, openTaskPanelTab, saveArtifactScreenshot, simulateRunSteps, type Task,
} from '../fixtures';

const VIEWPORT = { width: 1512, height: 900 };
const DRAWER = '#panel-content';
const TABS = ['Review', 'Diff', 'Migrations', 'Brief', 'Artifacts'] as const;
const BRIEF_MARKER = 'DRAWER-BRIEF-MARKER';

async function openDrawer(page: Page, task: Task): Promise<Locator> {
  await page.goto('/task-status');
  await page.locator('#ts-task-table').getByText(task.name).click();
  const panel = page.locator('#detail-panel');
  await expect(panel).not.toHaveClass(/\bhidden\b/);
  await expect(panel.locator('.task-page-header .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
  return panel;
}

async function box(loc: Locator) {
  const b = await loc.boundingBox();
  expect(b, 'element has no layout box').not.toBeNull();
  return b!;
}

// Header, stats strip, timeline and every tab, at whatever width the drawer has.
async function expectTaskLayout(page: Page, panel: Locator, task: Task) {
  const header = panel.locator('.task-page-header');
  await expect(header.locator('.task-page-pills .pill').first()).toBeVisible();
  await expect(header.locator('.task-page-actions .run-now-btn')).toBeVisible();
  // The drawer has its own close / expand / open controls, not a Back button.
  await expect(header.locator('.task-page-back-btn')).toHaveCount(0);
  for (const id of ['#panel-open', '#panel-expand', '#panel-close']) {
    await expect(page.locator(id)).toBeVisible();
  }
  // The title and the action buttons stay clear of those controls.
  const controls = await box(page.locator('#detail-panel > .panel-header-actions'));
  for (const sel of ['.task-page-title', '.task-page-actions .run-now-btn']) {
    const b = await box(header.locator(sel));
    const overlaps = b.x < controls.x + controls.width && controls.x < b.x + b.width
      && b.y < controls.y + controls.height && controls.y < b.y + b.height;
    expect(overlaps, `${sel} overlaps the drawer's ↗ / ⛶ / × controls`).toBe(false);
  }

  const stats = panel.locator('.task-page-stats');
  await expect(stats).toBeVisible();
  await expect(stats.locator('.timeline-stat')).not.toHaveCount(0);

  // A step stepsim recorded (see spec 14) shows in the drawer's timeline.
  const timeline = panel.locator(`#timeline-steps-${task.id}`);
  await expect(timeline.locator('.timeline-row-run')
    .filter({ has: page.locator('.timeline-title', { hasText: 'Wait 1 second' }) })).toBeVisible();

  const content: Record<(typeof TABS)[number], Locator> = {
    Review: panel.locator('#task-page-tabpanel-review').getByText('Internal ID'),
    Diff: panel.locator('#task-page-tabpanel-diff .task-page-diff'),
    Migrations: panel.locator('#task-page-tabpanel-migrations'),
    Brief: panel.locator('#task-page-tabpanel-brief').getByText(BRIEF_MARKER),
    Artifacts: panel.locator('#task-page-tabpanel-artifacts .task-artifacts'),
  };
  for (const name of TABS) {
    await openTaskPanelTab(page, name, DRAWER);
    await expect(content[name]).toBeVisible();
    for (const other of TABS) {
      if (other !== name) {
        await expect(panel.locator(`#task-page-tabpanel-${other.toLowerCase()}`)).toBeHidden();
      }
    }
  }

  // Nothing overflows the drawer sideways, and only one copy of the layout
  // exists: its element ids are global.
  const overflow = await page.locator(DRAWER).evaluate((n) => n.scrollWidth - n.clientWidth);
  expect(overflow).toBeLessThanOrEqual(1);
  await expect(page.locator('.task-page-header')).toHaveCount(1);
}

test.describe('drawer renders the task page layout', () => {
  test.use({ viewport: VIEWPORT });

  test('at the default drawer width: stacked header, stats, timeline, tabs', async ({ page, api }) => {
    // Every tab plus geometry checks; like spec 15's layout tests, slow under load.
    test.setTimeout(90_000);
    const task = await api.createTask('Drawer layout', { description: `Drawer brief\n\n${BRIEF_MARKER}` });
    const steps = simulateRunSteps(task.id);
    expect(steps, 'StepRecorder persisted no steps').toBeGreaterThan(0);

    const panel = await openDrawer(page, task);
    expect((await box(panel)).width).toBeLessThanOrEqual(480);
    await expectTaskLayout(page, panel, task);

    // Stacked: the tabs sit under the timeline, both full drawer width.
    const timeline = await box(panel.locator('.task-page-timeline'));
    const side = await box(panel.locator('.task-page-panel'));
    expect(side.y).toBeGreaterThanOrEqual(timeline.y + timeline.height - 1);
    expect(Math.abs(side.width - timeline.width)).toBeLessThanOrEqual(1);

    // The composer stays reachable at the bottom of the drawer.
    await expect(panel.locator('.task-page-composer textarea')).toBeVisible();
    await openTaskPanelTab(page, 'Review', DRAWER);
    await saveArtifactScreenshot(page, 'drawer-default-1512x900.png');
  });

  test('in #panel-expand mode: two columns, timeline beside the tabs', async ({ page, api }) => {
    // Every tab plus geometry checks; like spec 15's layout tests, slow under load.
    test.setTimeout(90_000);
    const task = await api.createTask('Drawer expanded', { description: `Drawer brief\n\n${BRIEF_MARKER}` });
    const steps = simulateRunSteps(task.id);
    expect(steps).toBeGreaterThan(0);

    const panel = await openDrawer(page, task);
    await page.locator('#panel-expand').click();
    await expect(panel).toHaveClass(/full-page/);
    await expect.poll(async () => (await box(panel)).width).toBeGreaterThan(1000);
    await expectTaskLayout(page, panel, task);

    const timeline = await box(panel.locator('.task-page-timeline'));
    const side = await box(panel.locator('.task-page-panel'));
    expect(side.x).toBeGreaterThanOrEqual(timeline.x + timeline.width - 1);
    expect(Math.abs(side.y - timeline.y)).toBeLessThanOrEqual(1);
    await openTaskPanelTab(page, 'Review', DRAWER);
    await saveArtifactScreenshot(page, 'drawer-expanded-1512x900.png');

    // Collapsing again restacks it.
    await page.locator('#panel-expand').click();
    await expect(panel).not.toHaveClass(/full-page/);
    await expect.poll(async () => {
      const t = await box(panel.locator('.task-page-timeline'));
      const s = await box(panel.locator('.task-page-panel'));
      return s.y >= t.y + t.height - 1;
    }).toBe(true);
  });

  test('↗ opens the full page; the drawer copy is gone', async ({ page, api }) => {
    const task = await api.createTask('Drawer to page');
    await openDrawer(page, task);
    await page.locator('#panel-open').click();
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    await expect(page.locator('#detail-panel')).toHaveClass(/\bhidden\b/);
    await expect(page.locator('#panel-content .task-page-header')).toHaveCount(0);
    await expect(page.locator('#task-page-content .task-page-back-btn')).toBeVisible();
  });
});

test.describe('drawer behaviour', () => {
  test.use({ viewport: VIEWPORT });

  test('Escape in a drawer dialog closes only the dialog; the next Escape closes the drawer', async ({ page, api }) => {
    const task = await api.createTask('Drawer escape', { description: `Drawer brief\n\n${BRIEF_MARKER}` });
    const panel = await openDrawer(page, task);
    await openTaskPanelTab(page, 'Brief', DRAWER);

    const trigger = panel.locator('[data-modal-trigger="brief"]');
    const dialog = page.locator('.content-modal [role="dialog"]');
    await trigger.click();
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText(BRIEF_MARKER);

    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);
    await expect(panel).not.toHaveClass(/\bhidden\b/);

    // Closing with the dialog's ✕ (a body-level overlay, outside the drawer)
    // must not count as a click outside the drawer either.
    await trigger.click();
    await expect(dialog).toBeVisible();
    await dialog.getByRole('button', { name: 'Close' }).click();
    await expect(dialog).toHaveCount(0);
    await expect(panel).not.toHaveClass(/\bhidden\b/);

    await page.keyboard.press('Escape');
    await expect(panel).toHaveClass(/\bhidden\b/);
  });

  test('Escape or a backdrop click in the file diff closes only the diff', async ({ page, api }) => {
    const task = await api.createTask('Drawer diff escape');
    const panel = await openDrawer(page, task);
    const modal = page.locator('.file-diff-modal');
    const openDiff = () => page.evaluate(
      (id) => (window as unknown as { openFileDiffModal: (t: unknown, f: string, c: string) => void })
        .openFileDiffModal({ id }, 'README.md', ''),
      task.id,
    );

    await openDiff();
    await expect(modal).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(modal).toHaveCount(0);
    await expect(panel).not.toHaveClass(/\bhidden\b/);

    await openDiff();
    await expect(modal).toBeVisible();
    await modal.click({ position: { x: 5, y: 5 } });
    await expect(modal).toHaveCount(0);
    await expect(panel).not.toHaveClass(/\bhidden\b/);

    await page.keyboard.press('Escape');
    await expect(panel).toHaveClass(/\bhidden\b/);
  });

  test('the click that opens the drawer never closes it, however slow the open', async ({ page, api }) => {
    // The opening click bubbles on to the document click-outside handler. It
    // used to be told apart only by a 150ms clock check, so a main thread
    // stalled for longer (heavy load) closed the drawer it had just opened.
    const task = await api.createTask('Drawer slow open');
    await page.goto('/task-status');
    const row = page.locator('#ts-task-table').getByText(task.name);
    await expect(row).toBeVisible();
    await page.evaluate(() => {
      const w = window as unknown as { openDetail: (...a: unknown[]) => unknown };
      const orig = w.openDetail;
      w.openDetail = (...a: unknown[]) => {
        const r = orig(...a);
        const until = Date.now() + 300;
        while (Date.now() < until) { /* stall the click's dispatch */ }
        return r;
      };
    });

    await row.click();
    const panel = page.locator('#detail-panel');
    await expect(panel.locator('.task-page-header .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
    await expect(panel).not.toHaveClass(/\bhidden\b/);
  });

  test('a live run step lands in the drawer timeline', async ({ page, api }) => {
    const task = await api.createTask('Drawer live step');
    const panel = await openDrawer(page, task);
    const timeline = panel.locator(`#timeline-steps-${task.id}`);
    await expect(timeline.locator('.timeline-empty')).toBeVisible();

    const now = new Date().toISOString();
    await page.evaluate(({ taskId, now }) => {
      (window as unknown as { handleEvent: (e: unknown) => void }).handleEvent({
        type: 'run.step',
        data: {
          id: 'drawer-live-step-1', run_id: 'drawer-live-run', task_id: taskId, seq: 1,
          kind: 'tool', title: 'DRAWER-LIVE-STEP', status: 'done',
          started_at: now, ended_at: now, created_at: now,
        },
      });
    }, { taskId: task.id, now });

    await expect(timeline.locator('.timeline-row[data-step-id="drawer-live-step-1"]')).toBeVisible();
    await expect(timeline).toContainText('DRAWER-LIVE-STEP');
  });

  test('a slow drawer load does not render after ↗ opened the full page', async ({ page, api }) => {
    const task = await api.createTask('Drawer race');
    await page.goto('/task-status');
    await expect(page.locator('#ts-task-table').getByText(task.name)).toBeVisible();

    // Hold the drawer's task fetch; let the full page's go through.
    let release!: () => void;
    const held = new Promise<void>((r) => { release = r; });
    let calls = 0;
    let governance = 0;
    page.on('response', (r) => {
      if (r.url().includes(`/api/tasks/${task.id}/governance`)) governance += 1;
    });
    await page.route(`**/api/tasks/${task.id}`, async (route) => {
      calls += 1;
      if (calls === 1) await held;
      await route.continue();
    });

    await page.locator('#ts-task-table').getByText(task.name).click();
    await expect.poll(() => calls).toBe(1);
    await page.locator('#panel-open').click();
    await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });

    expect(governance).toBe(1);
    release();
    // The drawer's load asks for governance last, right before it would
    // render: once that answers, its render (or no render) has happened.
    await expect.poll(() => governance).toBe(2);
    await page.waitForTimeout(250);
    await expect(page.locator('#panel-content .task-page-header')).toHaveCount(0);
    await expect(page.locator('.task-page-header')).toHaveCount(1);
    await expect(page.locator(`#timeline-steps-${task.id}`)).toHaveCount(1);
  });
});
