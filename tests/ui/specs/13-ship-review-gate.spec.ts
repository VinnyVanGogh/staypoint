import { test, expect, gotoTaskPage, openTaskPanelTab } from '../fixtures';

// STA-568: while a ship review card is pending/sent_back, Run Now and Mark done
// must be absent. The card itself must appear within 1 s of the page title.

test('pending card: no Run Now, no Mark done, card visible within 1s', async ({ page, api, request }) => {
  const task = await api.createTask('Ship review gate pending');

  // Move task to in_review so Mark done would normally show.
  const headers = { Authorization: `Bearer ${process.env.STAYPOINT_API_TOKEN || ''}`, 'Content-Type': 'application/json' };
  await request.post(`/api/tasks/${encodeURIComponent(task.id)}/stage`, { headers, data: { stage: 'in_review' } });

  // Seed a pending ship review card.
  await api.upsertShipReview(task.id, 'pending');

  await gotoTaskPage(page, task);
  const content = page.locator('#task-page-content');

  // Buttons must be absent — not hidden, absent.
  await expect(content.locator('.run-now-btn')).toHaveCount(0);
  await expect(content.locator('.mark-done-btn')).toHaveCount(0);

  // Card must be visible within 1 s of the title (gotoTaskPage already confirmed the title).
  await expect(content.locator(`#ship-review-${task.id}`)).toBeVisible({ timeout: 1_000 });
  await expect(content.locator(`#ship-review-${task.id} .ship-review-status-badge`)).toContainText(/pending/i);
});

test('sent_back card: no Run Now, no Mark done, card visible within 1s', async ({ page, api, request }) => {
  const task = await api.createTask('Ship review gate sent_back');

  const headers = { Authorization: `Bearer ${process.env.STAYPOINT_API_TOKEN || ''}`, 'Content-Type': 'application/json' };
  await request.post(`/api/tasks/${encodeURIComponent(task.id)}/stage`, { headers, data: { stage: 'in_review' } });

  await api.upsertShipReview(task.id, 'sent_back');

  await gotoTaskPage(page, task);
  // Only a pending review opens the panel on Review (STA-641).
  await openTaskPanelTab(page, 'Review');
  const content = page.locator('#task-page-content');

  await expect(content.locator('.run-now-btn')).toHaveCount(0);
  await expect(content.locator('.mark-done-btn')).toHaveCount(0);

  await expect(content.locator(`#ship-review-${task.id}`)).toBeVisible({ timeout: 1_000 });
  await expect(content.locator(`#ship-review-${task.id} .ship-review-status-badge`)).toContainText(/sent back/i);
});

test('no card: Run Now visible for todo task', async ({ page, api }) => {
  const task = await api.createTask('Ship review gate no card');
  // task starts as todo — no ship review card seeded

  await gotoTaskPage(page, task);
  const content = page.locator('#task-page-content');

  await expect(content.locator('.run-now-btn')).toBeVisible({ timeout: 3_000 });
});

test('approved card: no Run Now, no Mark done, final card visible', async ({ page, api, request }) => {
  const task = await api.createTask('Ship review gate approved');

  const headers = { Authorization: `Bearer ${process.env.STAYPOINT_API_TOKEN || ''}`, 'Content-Type': 'application/json' };
  await request.post(`/api/tasks/${encodeURIComponent(task.id)}/stage`, { headers, data: { stage: 'in_review' } });

  await api.upsertShipReview(task.id, 'approved');

  await gotoTaskPage(page, task);
  // Only a pending review opens the panel on Review (STA-641).
  await openTaskPanelTab(page, 'Review');
  const content = page.locator('#task-page-content');

  await expect(content.locator('.run-now-btn')).toHaveCount(0);
  await expect(content.locator('.mark-done-btn')).toHaveCount(0);

  await expect(content.locator(`#ship-review-${task.id}`)).toBeVisible({ timeout: 1_000 });
  await expect(content.locator(`#ship-review-${task.id} .ship-review-status-badge`)).toContainText(/approved/i);
});
