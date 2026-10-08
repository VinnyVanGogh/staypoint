import { test, expect, gotoTaskPage } from '../fixtures';

// Regression coverage for STA-350: cards must actually resolve, and a
// question must be answerable by picking one of its options.

test.describe('interaction cards', () => {
  test('Accept resolves a confirmation', async ({ page, api }) => {
    const task = await api.createTask('Interaction accept');
    const ix = await api.createInteraction(task.id, 'request_confirmation', { prompt: 'Ship the accept path?' });

    await gotoTaskPage(page, task);
    const card = page.locator('.interaction-card', { hasText: 'Ship the accept path?' });
    await expect(card).toBeVisible();
    await card.getByRole('button', { name: 'Accept only', exact: true }).click();

    await expect(card.locator('.interaction-resolved-badge')).toHaveText(/Accepted · answer only/);
    await expect.poll(async () => (await api.listInteractions(task.id)).find(i => i.id === ix.id)?.status).toBe('accepted');

    await page.reload();
    await expect(page.locator('.interaction-card', { hasText: 'Ship the accept path?' })).toHaveCount(0);
  });

  test('Reject resolves a confirmation', async ({ page, api }) => {
    const task = await api.createTask('Interaction reject');
    const ix = await api.createInteraction(task.id, 'request_confirmation', { prompt: 'Ship the reject path?' });

    await gotoTaskPage(page, task);
    const card = page.locator('.interaction-card', { hasText: 'Ship the reject path?' });
    await card.getByRole('button', { name: 'Reject' }).click();

    await expect(card.locator('.interaction-resolved-badge')).toHaveText(/Rejected/);
    await expect.poll(async () => (await api.listInteractions(task.id)).find(i => i.id === ix.id)?.status).toBe('rejected');
  });

  test('a question is answered by selecting an option', async ({ page, api }) => {
    const task = await api.createTask('Interaction question');
    const ix = await api.createInteraction(task.id, 'ask_user_questions', {
      questions: [{ question: 'Which database?', options: ['sqlite', 'postgres'] }],
    });

    await gotoTaskPage(page, task);
    const card = page.locator('.interaction-card', { hasText: 'Which database?' });
    await expect(card).toBeVisible();

    const option = card.getByRole('button', { name: 'postgres' });
    await option.click({ timeout: 3_000 });
    await expect(option).toHaveClass(/selected/);
    await expect(card.getByRole('button', { name: 'sqlite' })).not.toHaveClass(/selected/);

    await card.getByRole('button', { name: 'Accept only', exact: true }).click();
    await expect(card.locator('.interaction-resolved-badge')).toHaveText(/Accepted/);

    await expect
      .poll(async () => JSON.stringify((await api.listInteractions(task.id)).find(i => i.id === ix.id)?.response ?? null))
      .toContain('postgres');
  });

  // task-e3fe2c0b: pending cards live on a Decisions tab that opens by
  // default, counts them, and offers Accept & resume / Accept only / Reject.
  test('pending cards open on the Decisions tab with both accept actions', async ({ page, api }) => {
    const task = await api.createTask('Interaction decisions tab');
    await api.createInteraction(task.id, 'request_confirmation', { prompt: 'Decide on the tab?' });

    await gotoTaskPage(page, task);
    const tab = page.locator('#task-page-tab-decisions');
    await expect(tab).toHaveAttribute('aria-selected', 'true');
    await expect(tab.locator('.task-page-tab-count')).toHaveText('1');
    const card = page.locator('#task-page-tabpanel-decisions .interaction-card', { hasText: 'Decide on the tab?' });
    await expect(card).toBeVisible();
    await expect(card.getByRole('button', { name: 'Accept & resume', exact: true })).toBeVisible();
    await expect(card.getByRole('button', { name: 'Accept only', exact: true })).toBeVisible();
    await expect(card.getByRole('button', { name: 'Reject', exact: true })).toBeVisible();
  });
});
