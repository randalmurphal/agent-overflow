// Antigravity (agy) ACP session end-to-end check against the real backend,
// compiled ao-mockprovider ACP adapter, and Svelte frontend.
import { test, expect, type SeedResult } from './fixtures.js';

interface Item {
  kind: string;
  summary: string;
}

test('agy thread runs an ACP turn and renders in the UI and Settings', async ({ page, harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'agy-e2e',
        repo: {},
        threads: [{ title: 'Antigravity ACP check', provider: 'agy' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];

  await harness.rpc('StartSession', threadId);
  await harness.rpc('SendMessage', threadId, 'hello antigravity', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId, true);
      return items.find((i) => i.kind === 'assistant_text')?.summary ?? '';
    })
    .toContain('Hello from Antigravity mock.');

  await harness.open(page);
  await page.getByText('Antigravity ACP check').first().click();
  await expect(page.getByText('Hello from Antigravity mock.')).toBeVisible();

  await page.getByTestId('sidebar-settings-button').click();
  const agyTab = page
    .getByRole('tablist', { name: 'Settings Sections' })
    .getByRole('tab', { name: /^Antigravity/ });
  await expect(agyTab).toBeVisible();
  await agyTab.click();
  await expect(page.getByRole('heading', { name: 'Antigravity' })).toBeVisible();
});
