import { test, expect, gotoTaskPage, simulateRunSteps } from '../fixtures';

// STA-551: run/command rows show plain-language titles; full command on expand.

test('timeline run row: description used as title when present', async ({ page, api }) => {
  const task = await api.createTask('Timeline titles desc');
  await gotoTaskPage(page, task);
  const steps = page.locator(`#timeline-steps-${task.id}`);
  await expect(steps.locator('.timeline-empty')).toBeVisible();

  const persisted = simulateRunSteps(task.id);
  expect(persisted, 'stepsim persisted no steps (see daemon/stepsim log)').toBeGreaterThan(0);

  await page.reload();
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });

  // The Bash step with description should show the description, not the raw command.
  const descRow = steps
    .locator('.timeline-row-run')
    .filter({ has: page.locator('.timeline-title', { hasText: 'Wait 1 second' }) });
  await expect(descRow).toBeVisible();
  // The raw command must not appear in the collapsed title.
  await expect(descRow.locator('.timeline-title')).not.toHaveText('sleep 1');
});

test('timeline run row: command used as fallback title when no description', async ({ page, api }) => {
  const task = await api.createTask('Timeline titles cmd');
  await gotoTaskPage(page, task);
  const steps = page.locator(`#timeline-steps-${task.id}`);
  await expect(steps.locator('.timeline-empty')).toBeVisible();

  simulateRunSteps(task.id);

  await page.reload();
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });

  // The Bash step without description should show the command as the title.
  const cmdRow = steps
    .locator('.timeline-row-run')
    .filter({ has: page.locator('.timeline-title', { hasText: 'echo hello' }) });
  await expect(cmdRow).toBeVisible();
});

test('timeline run row: full command shown in expanded body', async ({ page, api }) => {
  const task = await api.createTask('Timeline expand cmd');
  await gotoTaskPage(page, task);
  const steps = page.locator(`#timeline-steps-${task.id}`);

  simulateRunSteps(task.id);

  await page.reload();
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });

  // Click the description-titled row to expand it.
  const descRow = steps
    .locator('.timeline-row-run')
    .filter({ has: page.locator('.timeline-title', { hasText: 'Wait 1 second' }) });
  await expect(descRow).toBeVisible();
  await descRow.locator('.timeline-row-summary').click();

  const body = descRow.locator('.timeline-body');
  await expect(body).not.toHaveClass(/hidden/);
  // Verbatim command (not the description) must appear in the expanded body.
  await expect(body).toContainText('$ sleep 1');
});
