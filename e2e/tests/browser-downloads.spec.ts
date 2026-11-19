// Downloads in the browser pane (docs/architecture/browser-tools.md,
// downloads). A download begun in a tab puts a downloads button in the
// address row, which shows a running download's progress and opens the list
// of the thread's downloads: a running one can be canceled there, a finished
// one opens in a new tab when a tab shows its type, and the list clears to
// nothing. A finished download is saved in the Downloads folder of the
// harness home under a name that overwrites nothing, is announced once with
// Show in folder, and one that fails is announced with its reason. The fake
// engine's download.invalid addresses stand in for a server
// (fake_downloads.go).
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { test, expect, type SeedResult } from './fixtures.js';

interface BrowserState {
  activePageId?: string;
  pages?: Array<{ id: string; url: string }>;
  downloads?: Array<{ id: string; name: string; state: string }>;
}

test('a download shows in the address row, can be canceled, opened and cleared, and announces its end', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-downloads',
      repo: {},
      threads: [{ title: 'Browser downloads', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await harness.open(page);
  await page.getByText('Browser downloads').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();
  await address.fill('https://example.test/files');
  await address.press('Enter');
  const button = pane.getByTestId('browser-downloads');
  await expect(button).toHaveCount(0);

  // A download held at half its size.
  await address.fill('https://download.invalid/archive.zip?bytes=4000&hold');
  await address.press('Enter');
  await expect(button).toHaveAttribute('aria-label', 'Downloads, 1 running');
  await expect(pane.getByTestId('browser-downloads-progress')).toHaveAttribute('data-step', '0.5');
  await button.click();
  const list = page.getByTestId('browser-downloads-list');
  const rows = list.getByTestId('browser-download');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toHaveAttribute('data-state', 'in_progress');
  await expect(rows.first().getByTestId('browser-download-status')).toHaveText('2 KB of 4 KB · example.test');
  await rows.first().getByTestId('browser-download-cancel').click();
  await expect(rows.first()).toHaveAttribute('data-state', 'canceled');
  await expect(button).toHaveAttribute('aria-label', 'Downloads');
  await page.keyboard.press('Escape');
  await expect(list).toHaveCount(0);

  // A finished download announces itself once, with Show in folder. A
  // report.pdf already in the Downloads folder makes it the next free name,
  // one with a space in it.
  const home = harness.bootstrap.homeDir;
  if (!home) throw new Error('The download spec requires an isolated home');
  await mkdir(path.join(home, 'Downloads'), { recursive: true });
  await writeFile(path.join(home, 'Downloads', 'report.pdf'), 'earlier');
  await address.fill('https://download.invalid/report.pdf?bytes=2048');
  await address.press('Enter');
  const finished = page.getByTestId('toast').filter({ hasText: /Downloaded report(?: \(\d+\))?\.pdf/ });
  await expect(finished).toHaveCount(1);
  await expect(finished.getByRole('button', { name: 'Show in folder' })).toBeVisible();
  // The page the download began on stays.
  await expect(address).toHaveValue('https://example.test/files');

  // A failed download is announced with its reason.
  await address.fill('https://download.invalid/broken.bin?bytes=10&fail');
  await address.press('Enter');
  await expect(page.getByTestId('toast').filter({ hasText: 'broken.bin did not download: the server closed the connection' })).toHaveCount(1);

  await button.click();
  await expect(rows).toHaveCount(3);
  await expect(rows.nth(0)).toHaveAttribute('data-state', 'failed');
  await expect(rows.nth(1)).toHaveAttribute('data-state', 'completed');
  await expect(rows.nth(2)).toHaveAttribute('data-state', 'canceled');

  // Open shows the finished file in a new tab, from the Downloads folder of
  // the harness home, as the desktop engines save it.
  await rows.nth(1).getByTestId('browser-download-open').click();
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);
  const listed = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  const firstName = listed.downloads?.find((item) => item.state === 'completed')?.name ?? '';
  expect(firstName).toMatch(/^report \(\d+\)\.pdf$/);
  // The file URL escapes the name (a space is %20), so the opened tab is
  // compared by its decoded file name.
  const savedName = (url: string): string => {
    if (!url.startsWith('file:')) return '';
    const path = new URL(url).pathname.split('/');
    if (path.at(-3) !== 'home' || path.at(-2) !== 'Downloads') return '';
    return decodeURIComponent(path.at(-1) ?? '');
  };
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return savedName(state.pages?.find((tab) => tab.id === state.activePageId)?.url ?? '');
  }).toBe(firstName);

  // The same name again is saved beside the first, never over it.
  await pane.getByTestId('browser-tab').first().click();
  // The selection is a round trip; the address shows the tab once it lands.
  await expect(address).toHaveValue('https://example.test/files');
  await address.fill('https://download.invalid/report.pdf?bytes=2048');
  await address.press('Enter');
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return (state.downloads ?? []).filter((item) => item.state === 'completed').map((item) => item.name);
  }).toHaveLength(2);
  const both = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  const names = (both.downloads ?? []).filter((item) => item.state === 'completed').map((item) => item.name);
  expect(names[1]).toBe(firstName);
  expect(names[0]).not.toBe(firstName);
  expect(names[0]).toMatch(/^report \(\d+\)\.pdf$/);

  // Clearing empties the list, and the button goes with it.
  await button.click();
  await list.getByTestId('browser-downloads-clear').click();
  await expect(button).toHaveCount(0);
  const cleared = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  expect(cleared.downloads ?? []).toEqual([]);
});
