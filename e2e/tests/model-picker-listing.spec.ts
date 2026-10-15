// Model picker contents follow the provider's own model list: the Claude
// submenu offers what the CLI's latest answer lists (the mock lists Opus 5 as
// its default, Opus 4.7 and Haiku 4.5), keeps the row of the model the thread
// is on even when the CLI no longer lists it, and leaves out every other
// shipped model. A new thread with no remembered choice starts on the CLI's default.
import { test, expect } from './fixtures.js';

test('the Claude picker offers what the CLI lists and keeps the thread on its unlisted model', async ({ harness, page }) => {
  const seed = await harness.rpc<{ projects: { projectId: string; threadIds: string[] }[] }>('HarnessSeed', {
    projects: [{
      name: 'picker-listing',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
      threads: [{
        title: 'On an unlisted model',
        provider: 'claude',
        model: 'claude-opus-4-6',
        turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }],
      }],
    }],
  });

  // The startup probe of the mock CLI is what reports the list.
  await expect.poll(async () => {
    const catalog = await harness.rpc<{ provenance: string }>('GetModelsForProvider', 'claude');
    return catalog.provenance;
  }).toBe('probed');

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'On an unlisted model' }).click();
  await page.getByTestId('composer-model-menu-trigger').click();
  const menu = page.getByRole('menu', { name: 'Model and Provider' });
  await menu.getByRole('menuitem', { name: 'Claude', exact: true }).hover();

  const models = page.locator('[data-popover]').getByRole('menuitemcheckbox').or(
    page.locator('[data-popover]').getByRole('menuitemradio'),
  ).or(page.locator('[data-popover]').getByRole('menuitem'));
  await expect(models.filter({ hasText: 'Opus 5' }).first()).toBeVisible();
  await expect(models.filter({ hasText: 'Haiku 4.5' }).first()).toBeVisible();
  // The thread's own model keeps its row though the CLI does not list it.
  await expect(models.filter({ hasText: 'Opus 4.6' }).first()).toBeVisible();
  // Shipped models the CLI does not list, and no thread runs, are not offered.
  for (const absent of ['Sonnet 4.6', 'Opus 4.5', 'Fable 5', 'Haiku 5.5']) {
    await expect(models.filter({ hasText: absent })).toHaveCount(0);
  }

  const defaults = await harness.rpc<{ model: string }>('GetThreadDefaults', {
    projectId: seed.projects[0].projectId,
    provider: 'claude',
  });
  // The thread's profile is the remembered choice when the seed recorded
  // one; with none the default is the CLI's (Opus 5), never the shipped head.
  expect(['claude-opus-5', 'claude-opus-4-6']).toContain(defaults.model);
  expect(defaults.model).not.toBe('claude-fable-5');
});
