// Opening the browser from no tab (docs/architecture/browser-tools.md,
// companion pane). The chat header's browser button is there whenever the
// native browser is usable, tabs or not; with none it opens the pane on one
// new tab with its address bar focused. A mod-clicked link in a thread with
// no tab opens the pane on a tab at the link. Closing the last tab closes the
// pane and leaves the button. The new-tab shortcut, pressed with the browser
// pane focused, opens a tab whose address bar takes focus with its text
// selected, and the next-tab shortcut steps back to the first. Switching the
// cookie jar with tabs open asks first, then reopens the tabs in the new jar
// in the same order with the same active tab. A file opens
// through the address row's picker, by search and from the folder view,
// without typing its path.
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';

interface BrowserState {
  activePageId?: string;
  visible?: boolean;
  jar?: string;
  pages?: Array<{ id: string; url: string; creating?: boolean; error?: string }>;
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

async function seedBrowserThread(harness: HarnessApp, title: string): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-chrome',
      repo: {
        commits: [{
          message: 'init',
          files: {
            'docs/report.html': '<!doctype html><title>Quarterly report</title><p>report</p>',
            'site/index.html': '<!doctype html><title>Site index</title><p>index</p>',
          },
        }],
      },
      threads: [{ title, turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  return threadId;
}

async function activeURL(harness: HarnessApp, threadId: string): Promise<string> {
  const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  return state.pages?.find((tab) => tab.id === state.activePageId)?.url ?? '';
}

test('the new-tab shortcut focuses the new tab\'s address with its text selected', async ({ harness, page }) => {
  const threadId = await seedBrowserThread(harness, 'Browser shortcuts');
  await harness.open(page);
  await page.getByText('Browser shortcuts').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();
  await address.fill('https://example.test/first');
  await address.press('Enter');
  await expect.poll(() => activeURL(harness, threadId)).toBe('https://example.test/first');
  await expect(address).not.toBeFocused();

  // The browser pane is the focused pane (its address had focus): the chord
  // opens a tab there, and its address takes focus.
  await page.keyboard.press('ControlOrMeta+t');
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);
  await expect(address).toBeFocused();
  await expect(address).toHaveValue('');
  await address.fill('https://example.test/second');
  await address.press('Enter');
  await expect(address).toHaveValue('https://example.test/second');
  // Focusing the address again selects all of it.
  await page.keyboard.press('ControlOrMeta+l');
  await expect(address).toBeFocused();
  expect(await address.evaluate((el: HTMLInputElement) => [el.selectionStart, el.selectionEnd]))
    .toEqual([0, 'https://example.test/second'.length]);

  await page.keyboard.press('Control+Tab');
  await expect.poll(() => activeURL(harness, threadId)).toBe('https://example.test/first');
  await expect(address).toHaveValue('https://example.test/first');
});

test('switching the cookie jar asks first, then reopens the tabs in order in the new jar', async ({ harness, page }) => {
  const threadId = await seedBrowserThread(harness, 'Browser jars');
  const urls = ['https://example.test/one', 'https://example.test/two'];
  const ids: string[] = [];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    const pageId = opened.activePageId ?? '';
    ids.push(pageId);
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId, address });
  }
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId: ids[0] });
  await harness.open(page);
  await page.getByText('Browser jars').click();
  const pane = page.getByTestId('browser-pane');
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);

  const jar = pane.getByTestId('browser-jar');
  await expect(jar).toHaveAttribute('aria-label', 'Cookie jar: This worktree');
  await jar.click();
  await page.getByTestId('browser-jar-thread').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Switch to This browser only?');
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  expect((await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId)).jar).toBe('worktree');

  await jar.click();
  await page.getByTestId('browser-jar-thread').click();
  await dialog.getByRole('button', { name: 'Switch and reopen' }).click();
  await expect(jar).toHaveAttribute('aria-label', 'Cookie jar: This browser only');
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return {
      jar: state.jar,
      urls: state.pages?.map((tab) => tab.url),
      active: state.pages?.findIndex((tab) => tab.id === state.activePageId),
      reopened: state.pages?.every((tab) => !ids.includes(tab.id) && !tab.creating && !tab.error),
    };
  }).toEqual({ jar: 'thread', urls, active: 0, reopened: true });
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);
  await expect(pane.getByRole('textbox', { name: 'Address' })).toHaveValue(urls[0]);
});

test('a file opens from the picker by search and from the folder view', async ({ harness, page }) => {
  const threadId = await seedBrowserThread(harness, 'Browser files');
  await harness.open(page);
  await page.getByText('Browser files').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  await expect(pane.getByTestId('browser-tab')).toHaveAttribute('data-tab-state', 'ready');

  await pane.getByRole('button', { name: 'Open file' }).click();
  const picker = page.getByTestId('browser-file-picker');
  await expect(picker).toBeVisible();
  const search = picker.getByTestId('browser-file-search');
  await expect(search).toBeFocused();
  await search.fill('report');
  await expect(picker.getByTestId('browser-file-result')).toHaveText(['docs/report.html']);
  await search.press('Enter');
  await expect(picker).toHaveCount(0);
  await expect.poll(() => activeURL(harness, threadId)).toMatch(/^file:\/\/.*\/docs\/report\.html$/);
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);

  await pane.getByRole('button', { name: 'Open file' }).click();
  await picker.getByTestId('directory-browser-entry').filter({ hasText: 'site' }).click();
  await picker.getByTestId('directory-browser-entry').filter({ hasText: 'index.html' }).click();
  await expect(picker).toHaveCount(0);
  await expect.poll(() => activeURL(harness, threadId)).toMatch(/^file:\/\/.*\/site\/index\.html$/);
});
