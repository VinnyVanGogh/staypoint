import { test, expect } from '../fixtures';

// The throwaway daemon reports git commit "apitest", which is never in main,
// so the commit-hash DoD gate is always closed here. That makes the gate's
// blocked state deterministic to assert.

test('checklist shows items and the DoD commit gate status', async ({ page, api }) => {
  // Own the sprint: the default pick is the newest seeded sprint, which
  // depends on what earlier specs (or repeats) seeded.
  await api.seedChecklist('STA-236');
  const loaded = page.waitForResponse(r => r.url().includes('/api/checklist?sprint=STA-236') && r.ok());
  await page.goto('/checklist?sprint=STA-236');
  await loaded;
  const view = page.locator('#view-checklist');
  await expect(view).toHaveClass(/active/);

  await expect(page.locator('#checklist-progress')).toHaveText(/\d+\/\d+ reviewed/);
  const banner = page.locator('.checklist-commit-gate-banner');
  await expect(banner).toBeVisible();
  await expect(banner).toContainText('Commit-Hash Gate');
  await expect(banner).toContainText('Running Daemon: apitest');

  await banner.locator('.cl-gate-toggle-btn').click();
  await expect(banner.locator('.cl-gate-log-table tbody tr').first()).toBeVisible();

  await expect(page.locator('#checklist-container').getByText('Commit Gate Active').first()).toBeVisible();
});

// STA-670: boot renders the checklist at once, then re-renders it when the
// slow fleet load finishes. In a full-suite run the fleet load landed after
// the click and the rebuilt banner came back with the log collapsed. Hold the
// fleet load until the log is open so the re-render always comes second.
test('commit gate log stays open across the post-boot re-render', async ({ page, api }) => {
  await api.seedChecklist('STA-236');
  let releaseFleet!: () => void;
  const fleetHeld = new Promise<void>(r => { releaseFleet = r; });
  await page.route('**/api/fleet/overview', async route => {
    await fleetHeld;
    await route.continue();
  });

  const firstLoad = page.waitForResponse(r => r.url().includes('/api/checklist?sprint=STA-236') && r.ok());
  await page.goto('/checklist?sprint=STA-236');
  await firstLoad;

  const banner = page.locator('.checklist-commit-gate-banner');
  await expect(banner).toBeVisible();
  await banner.locator('.cl-gate-toggle-btn').click();
  const row = banner.locator('.cl-gate-log-table tbody tr').first();
  await expect(row).toBeVisible();

  // Tag the current banner so we can tell when the re-render has replaced it.
  await banner.evaluate(n => { (n as HTMLElement).dataset.stale = '1'; });
  const reload = page.waitForResponse(r => r.url().includes('/api/checklist?sprint=STA-236') && r.ok());
  releaseFleet();
  await reload;

  const fresh = page.locator('.checklist-commit-gate-banner:not([data-stale])');
  await expect(fresh).toBeVisible();
  await expect(fresh.locator('.cl-gate-log-table tbody tr').first()).toBeVisible();
  await expect(fresh.locator('.cl-gate-toggle-btn')).toHaveText(/^Hide Warning Log/);
});

// STA-415: commit codes and task IDs stay out of the text the Board reads.
// They sit in a collapsed "details" toggle on each item.
test('checklist hides commit codes and task IDs behind a details toggle', async ({ page, api }) => {
  await api.seedChecklist('STA-316');
  await page.goto('/checklist?sprint=STA-316');

  const items = page.locator('#checklist-container .checklist-item');
  await expect(items).toHaveCount(5);
  await expect(items.first().locator('.checklist-title')).toContainText('1. Create a task');
  await expect(page.locator('#checklist-container .checklist-section-name').first())
    .toHaveText('Try one task from start to finish');

  const first = items.first();
  const refs = first.locator('details.cl-refs');
  await expect(refs).not.toHaveAttribute('open', '');
  await expect(refs.locator('.cl-refs-body')).toBeHidden();
  await refs.locator('summary').click();
  await expect(refs.locator('.cl-refs-body')).toHaveText(/Build: [0-9a-f]{7}/);

  const visible = await page.locator('#checklist-container .checklist-body').evaluateAll(nodes =>
    nodes.map(n => {
      const c = n.cloneNode(true) as HTMLElement;
      c.querySelectorAll('details, textarea, button, .badge').forEach(x => x.remove());
      return c.textContent || '';
    }).join('\n'));
  expect(visible).not.toMatch(/\b(?:STA|MAN|PER|RUN|RES)-\d+\b/);
  expect(visible).not.toMatch(/\b(?=[0-9a-f]*\d)(?=[0-9a-f]*[a-f])[0-9a-f]{7,40}\b/);
});

test('checklist strips IDs from older sprint text too', async ({ page, api }) => {
  await api.seedChecklist('STA-236');
  await page.goto('/checklist?sprint=STA-236');
  const firstSection = page.locator('#checklist-container .checklist-section-name').first();
  await expect(firstSection).toHaveText('01. Table Sorting & Multi-Dimension Filters');
});
