// Opening the browser from no tab (docs/architecture/browser-tools.md,
// companion pane). The chat header's browser button is there whenever the
// native browser is usable, tabs or not; with none it opens the pane on one
// new tab with its address bar focused. A mod-clicked link in a thread with
// no tab opens the pane on a tab at the link. Closing the last tab closes the
// pane and leaves the button.
import { test, expect, type SeedResult } from './fixtures.js';

interface BrowserState {
  activePageId?: string;
  visible?: boolean;
  pages?: Array<{ id: string; url: string }>;
}

test('the header button and a mod-clicked link open the browser from no tab', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-tabs',
      repo: {},
      threads: [{
        title: 'Browser tabs',
        turns: [{
          userText: 'Where are the docs?',
          items: [{ kind: 'assistant_text', summary: 'See [the docs](https://example.test/docs).' }],
        }],
      }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await harness.open(page);
  await page.getByText('Browser tabs').click();

  const pane = page.getByTestId('browser-pane');
  const button = page.getByTestId('browser-companion-toggle');
  await expect(pane).toHaveCount(0);
  await expect(button).toHaveAttribute('aria-label', 'Open browser');

  await button.click();
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);
  await expect(pane.getByTestId('browser-tab')).toHaveAttribute('data-tab-state', 'ready');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();
  const opened = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  expect(opened.pages).toHaveLength(1);
  expect(opened.visible).toBe(true);

  await pane.getByRole('button', { name: 'Close tab' }).click();
  await expect(pane).toHaveCount(0);
  await expect(button).toHaveAttribute('aria-label', 'Open browser');

  await page.getByRole('link', { name: 'the docs' }).click({ modifiers: ['ControlOrMeta'] });
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return state.pages?.map((tab) => tab.url);
  }).toEqual(['https://example.test/docs']);
  await expect(address).toHaveValue('https://example.test/docs');
  await expect(address).not.toBeFocused();
});
