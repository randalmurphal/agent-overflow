// Antigravity (agy) ACP session end-to-end check against the real backend,
// compiled ao-mockprovider ACP adapter, and Svelte frontend.
import { test, expect, type SeedResult } from './fixtures.js';

interface Item {
  kind: string;
  summary: string;
}

test('agy thread switches via model picker, runs ACP turns across reconnect, and renders in UI and Settings', async ({ page, harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'agy-e2e',
        repo: {},
        threads: [{ title: 'Antigravity ACP check', provider: 'claude' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];

  // Switch the thread to Antigravity through the real model picker RPC.
  const updated = await harness.rpc<{ provider: string; model: string }>(
    'UpdateThreadModelSelection',
    threadId,
    'agy',
    'gemini-4-neon',
  );
  expect(updated.provider).toBe('agy');

  await harness.startSession(threadId);
  await harness.send(threadId, 'hello antigravity');
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.items<Item>(threadId, true);
      return items.find((i) => i.kind === 'assistant_text')?.summary ?? '';
    })
    .toContain('Hello from Antigravity mock.');

  // Disconnect and reconnect, then send a second turn and verify both turns survive.
  await harness.stopSession(threadId);
  await harness.startSession(threadId);
  await harness.send(threadId, 'second turn after reconnect');
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.items<Item>(threadId, true);
      return items.filter((i) => i.kind === 'assistant_text').length;
    })
    .toBeGreaterThanOrEqual(2);

  await harness.open(page);
  await page.getByText('Antigravity ACP check').first().click();
  await expect(page.getByText('Hello from Antigravity mock.').first()).toBeVisible();

  await page.getByTestId('sidebar-settings-button').click();
  const agyTab = page
    .getByRole('tablist', { name: 'Settings Sections' })
    .getByRole('tab', { name: /^Antigravity/ });
  await expect(agyTab).toBeVisible();
  await agyTab.click();
  await expect(page.getByRole('heading', { name: 'Antigravity' })).toBeVisible();
});
