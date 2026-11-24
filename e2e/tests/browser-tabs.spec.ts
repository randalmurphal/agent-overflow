// Opening the browser from no tab (docs/architecture/browser-tools.md,
// companion pane). The chat header's browser button is there whenever the
// native browser is usable, tabs or not; with none it opens the pane on one
// new tab with its address bar focused, and pressed again it hides the pane
// and then shows that tab, adding none. A mod-clicked link in a thread with
// no tab opens the pane on a tab at the link. Closing the last tab closes the
// pane and leaves the button. The new-tab shortcut, pressed with the browser
// pane focused, opens a tab whose address bar takes focus with its text
// selected, and the next-tab shortcut steps back to the first. Switching the
// cookie jar with tabs open asks first, then suspends the tabs into the new
// jar in the same order with the same active tab, which the pane brings back
// at once; the others come back when used. A file opens
// through the address row's picker, by search and from the folder view,
// without typing its path. A tab's menu duplicates it and closes the others
// or those to its right, and the menu or mod+shift+t reopens the last closed
// tab where it was. The user's last selection wins when the backend
// receives two in reverse order: of two tabs clicked with no gap the
// second, and a click then next tab one past the clicked tab. A close chord
// the page claimed that arrives after a pane was promoted over the browser
// closes nothing; once the browser is uncovered it closes the page it was
// pressed in, also after the user selected another tab meanwhile. Mod+W and
// then the x of the same tab close one tab, whichever reaches the backend
// first. A tab dragged after a close to the right that has not reached the
// backend yet moves once that close has closed the tabs the user saw to its
// right. A drag names the tab it was dropped beside and is sent even when
// the strip shows it there already: dropped before three while a reopen of
// one is on its way, four lands before three once one is back in front, and
// dropped after two, which it already follows, while a reopen of three is
// on its way, it lands after two once three is back before it.
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';

interface BrowserState {
  activePageId?: string;
  visible?: boolean;
  jar?: string;
  pages?: Array<{ id: string; url: string; creating?: boolean; error?: string; suspended?: boolean }>;
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
  await harness.startSession(threadId);
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

  // Hide, then show again: the show shows the tab the thread has and adds
  // none, and no address takes focus.
  await expect(button).toHaveAttribute('aria-pressed', 'true');
  await button.click();
  await expect(pane).toHaveCount(0);
  await expect(button).toHaveAttribute('aria-pressed', 'false');
  await button.click();
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);
  await expect(address).not.toBeFocused();
  const shownAgain = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  expect(shownAgain.pages?.map((tab) => tab.id)).toEqual(opened.pages?.map((tab) => tab.id));
  expect(shownAgain.activePageId).toBe(opened.activePageId);
  expect(shownAgain.visible).toBe(true);

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
  await harness.startSession(threadId);
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

test('switching the cookie jar asks first, then brings the tabs back in order in the new jar', async ({ harness, page }) => {
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
  await dialog.getByRole('button', { name: 'Switch cookie jar' }).click();
  await expect(jar).toHaveAttribute('aria-label', 'Cookie jar: This browser only');
  // The tabs keep their ids, order and addresses; the shown one comes back
  // live in the new jar, the other stays suspended until it is used.
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return {
      jar: state.jar,
      ids: state.pages?.map((tab) => tab.id),
      urls: state.pages?.map((tab) => tab.url),
      active: state.activePageId,
      suspended: state.pages?.map((tab) => !!tab.suspended),
      failed: state.pages?.some((tab) => !!tab.error),
    };
  }).toEqual({ jar: 'thread', ids, urls, active: ids[0], suspended: [false, true], failed: false });
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);
  await expect(pane.getByRole('textbox', { name: 'Address' })).toHaveValue(urls[0]);
  await pane.getByTestId('browser-tab').nth(1).click();
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return state.pages?.map((tab) => !!tab.suspended);
  }).toEqual([false, false]);
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

async function tabURLs(harness: HarnessApp, threadId: string): Promise<string[]> {
  const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  return state.pages?.map((tab) => tab.url) ?? [];
}

test('the tab menu duplicates and closes tabs, and a closed tab reopens where it was', async ({ harness, page }) => {
  const threadId = await seedBrowserThread(harness, 'Browser tab menu');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser tab menu').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(3);

  await tabs.nth(0).click({ button: 'right' });
  const menu = page.getByRole('menu', { name: 'Tab actions' });
  await expect(menu).toBeVisible();
  await expect(menu.getByTestId('browser-tab-menu-reopen')).toBeDisabled();
  await menu.getByTestId('browser-tab-menu-duplicate').click();
  await expect(menu).toHaveCount(0);
  await expect.poll(() => tabURLs(harness, threadId)).toEqual([urls[0], urls[0], urls[1], urls[2]]);

  await tabs.nth(1).click({ button: 'right' });
  await menu.getByTestId('browser-tab-menu-close-right').click();
  await expect.poll(() => tabURLs(harness, threadId)).toEqual([urls[0], urls[0]]);

  // Reopen from the menu, then from the chord with the browser pane focused.
  await tabs.nth(0).click({ button: 'right' });
  await menu.getByTestId('browser-tab-menu-reopen').click();
  await expect.poll(() => tabURLs(harness, threadId)).toEqual([urls[0], urls[0], urls[2]]);
  await pane.getByRole('textbox', { name: 'Address' }).click();
  await page.keyboard.press('ControlOrMeta+Shift+t');
  await expect.poll(() => tabURLs(harness, threadId)).toEqual([urls[0], urls[0], urls[1], urls[2]]);
  await expect(tabs).toHaveCount(4);

  await tabs.nth(2).click({ button: 'right' });
  await menu.getByTestId('browser-tab-menu-close-others').click();
  await expect.poll(() => tabURLs(harness, threadId)).toEqual([urls[1]]);
  await expect.poll(async () => (await harness.rpc<{ closedTabs?: number }>('BrowserCompanionThreadState', threadId)).closedTabs).toBe(3);
});

const BROWSER_COMPANION_DO = 197228034;
// How long the socket keeps a held selection after sending the one the user
// made next, so the backend has that one before the held one arrives.
const REVERSED_DELIVERY_MS = 300;

test('the user\'s last tab selection wins when two arrive reversed', async ({ harness, page }) => {
  // The socket holds the first of two selections (a click or a step) and
  // sends it only after the second, so the backend receives the two in
  // reverse order every round.
  let held: { id: string; message: string } | null = null;
  let settledRounds = 0;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const kind = frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO ? frame.params?.[1]?.kind : '';
      if (kind === 'activate' || kind === 'step') {
        if (held === null) {
          held = { id: frame.id, message: String(message) };
          return;
        }
        const first = held.message;
        setTimeout(() => server.send(first), REVERSED_DELIVERY_MS);
      }
      server.send(message);
    });
    server.onMessage((message) => {
      socket.send(message);
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && held !== null && frame.id === held.id) {
        held = null;
        settledRounds++;
      }
    });
  });
  const threadId = await seedBrowserThread(harness, 'Browser tab clicks');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser tab clicks').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(3);
  let round = 0;
  const settled = async (want: number) => {
    // Both selections have answered, the first one last.
    await expect.poll(() => settledRounds, { message: `round ${round} settled` }).toBe(round + 1);
    expect(await activeURL(harness, threadId), `round ${round}`).toBe(urls[want]);
    await expect(tabs.nth(want)).toHaveAttribute('aria-selected', 'true');
    round++;
  };

  // Every ordered pair of the three tabs, each clicked in one task.
  for (const [first, second] of [[0, 1], [1, 2], [2, 0], [1, 0], [2, 1], [0, 2]]) {
    await tabs.evaluateAll((elements, [a, b]) => {
      (elements[a] as HTMLElement).click();
      (elements[b] as HTMLElement).click();
    }, [first, second]);
    await settled(second);
  }

  // A click and then next tab end one past the clicked tab.
  await pane.getByRole('textbox', { name: 'Address' }).click();
  for (const clicked of [0, 1, 2]) {
    await tabs.nth(clicked).click();
    await page.keyboard.press('Control+Tab');
    await settled((clicked + 1) % 3);
  }
});

// A chord pressed in the native page arrives later as an `accelerator`
// event naming the pane mount that showed the page. It acts in that pane or
// nowhere: a pane promoted over the browser meanwhile is not closed by it.
test('a chord from the page that arrives after a pane was promoted over it closes nothing', async ({ harness, page }) => {
  let mount = '';
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO && frame.params?.[1]?.mount) mount = frame.params[1].mount;
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  const threadId = await seedBrowserThread(harness, 'Browser chord replay');
  for (const address of ['https://example.test/one', 'https://example.test/two']) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser chord replay').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(2);
  // A click names the pane's mount on the wire.
  await tabs.nth(0).click();
  await expect.poll(() => mount).not.toBe('');
  await expect.poll(() => activeURL(harness, threadId)).toBe('https://example.test/one');

  const close = process.platform === 'darwin' ? { key: 'w', meta: true } : { key: 'w', ctrl: true };
  const pressInPage = () => harness.rpc('HarnessEmit', 'browser:companion-state', { kind: 'accelerator', threadId, mount, accelerator: close });
  const thread = page.locator('section[data-pane-kind="thread"]');
  const promote = thread.getByTestId('chat-header').getByTestId('pane-promote');
  await promote.click();
  await expect(thread).toHaveAttribute('data-pane-promoted', 'true');

  await pressInPage();
  // A frame sent after the chord on the same socket: once it shows, the
  // chord has been handled.
  await harness.rpc('HarnessEmit', 'thread:updated', { action: 'patch', id: threadId, title: 'Browser chord handled' });
  await expect(page.getByText('Browser chord handled').first()).toBeVisible();
  await expect(thread).toHaveCount(1);
  await expect(thread).toHaveAttribute('data-pane-promoted', 'true');
  expect(await tabURLs(harness, threadId)).toEqual(['https://example.test/one', 'https://example.test/two']);

  // With the browser no longer covered, the same chord closes its active tab.
  await promote.click();
  await expect(thread).toHaveAttribute('data-pane-promoted', 'false');
  await pressInPage();
  await expect.poll(() => tabURLs(harness, threadId)).toEqual(['https://example.test/two']);
  await expect(thread).toHaveCount(1);
});

// Mod+W pressed in page one reaches the window after the user clicked tab
// two: the close names the page it was pressed in, so one closes and two,
// the tab the click selected, stays.
test('a close chord from a page that arrives after another tab was selected closes that page', async ({ harness, page }) => {
  let mount = '';
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO && frame.params?.[1]?.mount) mount = frame.params[1].mount;
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  const threadId = await seedBrowserThread(harness, 'Browser late chord');
  const ids: string[] = [];
  for (const address of ['https://example.test/one', 'https://example.test/two']) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    ids.push(opened.activePageId ?? '');
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser late chord').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(2);
  await tabs.nth(0).click();
  await expect.poll(() => activeURL(harness, threadId)).toBe('https://example.test/one');
  // The chord pressed in page one is on its way while the user clicks two.
  await tabs.nth(1).click();
  await expect.poll(() => activeURL(harness, threadId)).toBe('https://example.test/two');
  await expect(tabs.nth(1)).toHaveAttribute('aria-selected', 'true');
  const close = process.platform === 'darwin' ? { key: 'w', meta: true } : { key: 'w', ctrl: true };
  await harness.rpc('HarnessEmit', 'browser:companion-state', { kind: 'accelerator', threadId, mount, pageId: ids[0], accelerator: close });
  await expect.poll(() => tabURLs(harness, threadId)).toEqual(['https://example.test/two']);
  await expect(tabs).toHaveCount(1);
});

// Mod+W on tab one, then one's x before the first close reaches the
// backend: both name tab one and take their places in the pane's order, so
// the second finds it gone and one tab closes, not two.
test('the keyboard close and the x of the same tab close one tab', async ({ harness, page }) => {
  // The first close's frame, held, and the way to the backend it goes by.
  const wire: { held?: string; send?: (message: string) => void } = {};
  const closes: string[] = [];
  const answered = new Set<string>();
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    wire.send = (message) => server.send(message);
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const kind = frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO ? frame.params?.[1]?.kind : '';
      if (kind === 'close') {
        closes.push(frame.id);
        if (wire.held === undefined) {
          wire.held = String(message);
          return;
        }
      }
      server.send(message);
    });
    server.onMessage((message) => {
      socket.send(message);
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && closes.includes(frame.id)) answered.add(frame.id);
    });
  });
  const threadId = await seedBrowserThread(harness, 'Browser double close');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three', 'https://example.test/four'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser double close').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(4);
  await tabs.nth(0).click();
  await expect.poll(() => activeURL(harness, threadId)).toBe(urls[0]);
  await expect(tabs.nth(0)).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press('ControlOrMeta+w');
  await expect.poll(() => wire.held !== undefined).toBe(true);
  // The x's close reaches the backend first and waits for the held one's
  // turn, which is then let go.
  await pane.getByTestId('browser-tab-close').nth(0).click();
  await expect.poll(() => closes.length).toBe(2);
  wire.send?.(wire.held ?? '');
  await expect.poll(() => answered.size).toBe(2);
  expect(await tabURLs(harness, threadId)).toEqual(urls.slice(1));
  await expect(tabs).toHaveCount(3);
});

/**
 * Holds the first `held` call on its way to the backend until `release`,
 * and counts the `held` and `move` calls sent and answered.
 */
async function holdBeforeMove(page: Page, held: string) {
  const wire: { held?: string; send?: (message: string) => void } = {};
  const sent: string[] = [];
  const answered = new Set<string>();
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    wire.send = (message) => server.send(message);
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const kind = frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO ? frame.params?.[1]?.kind : '';
      if (kind === held || kind === 'move') {
        sent.push(frame.id);
        if (kind === held && wire.held === undefined) {
          wire.held = String(message);
          return;
        }
      }
      server.send(message);
    });
    server.onMessage((message) => {
      socket.send(message);
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && sent.includes(frame.id)) answered.add(frame.id);
    });
  });
  return {
    held: () => wire.held !== undefined,
    release: () => wire.send?.(wire.held ?? ''),
    sent: () => sent.length,
    answered: () => answered.size,
  };
}

// Close Tabs to the Right on two, then one dragged after two before the
// close reaches the backend: the move takes its place after the close in
// the pane's order, so the close reads the strip the user saw, closes
// three, and one survives behind two.
test('a tab dragged after a close to the right waits for it', async ({ harness, page }) => {
  const wire = await holdBeforeMove(page, 'close-right');
  const threadId = await seedBrowserThread(harness, 'Browser move after close');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser move after close').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(3);

  await tabs.nth(1).click({ button: 'right' });
  const menu = page.getByRole('menu', { name: 'Tab actions' });
  await menu.getByTestId('browser-tab-menu-close-right').click();
  await expect.poll(() => wire.held()).toBe(true);
  // Dropped on the right half of two, one lands after it.
  const target = await tabs.nth(1).boundingBox();
  if (!target) throw new Error('tab two has no box');
  await tabs.nth(0).dragTo(tabs.nth(1), { targetPosition: { x: target.width * 0.75, y: target.height / 2 } });
  await expect.poll(() => wire.sent()).toBe(2);
  wire.release();
  await expect.poll(() => wire.answered()).toBe(2);
  expect(await tabURLs(harness, threadId)).toEqual([urls[1], urls[0]]);
  await expect(tabs).toHaveCount(2);
});

// One closed and then reopened, and while the reopen is on its way four
// dragged before three in the strip two three four the user sees, the move
// reaching the backend first: the move waits for the reopen's turn and
// lands beside three, so one two four three.
test('a tab dropped beside another lands beside it after a reopen before it', async ({ harness, page }) => {
  const wire = await holdBeforeMove(page, 'reopen');
  const threadId = await seedBrowserThread(harness, 'Browser move after reopen');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three', 'https://example.test/four'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser move after reopen').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(4);

  await tabs.nth(0).click({ button: 'right' });
  await page.getByRole('menu', { name: 'Tab actions' }).getByTestId('browser-tab-menu-close').click();
  await expect(tabs).toHaveCount(3);
  await tabs.nth(0).click({ button: 'right' });
  await page.getByRole('menu', { name: 'Tab actions' }).getByTestId('browser-tab-menu-reopen').click();
  await expect.poll(() => wire.held()).toBe(true);
  // Dropped on the left half of three, four lands before it.
  const target = await tabs.nth(1).boundingBox();
  if (!target) throw new Error('tab three has no box');
  await tabs.nth(2).dragTo(tabs.nth(1), { targetPosition: { x: target.width * 0.25, y: target.height / 2 } });
  await expect.poll(() => wire.sent()).toBe(2);
  wire.release();
  await expect.poll(() => wire.answered()).toBe(2);
  expect(await tabURLs(harness, threadId)).toEqual([urls[0], urls[1], urls[3], urls[2]]);
  await expect(tabs).toHaveCount(4);
});

// Three closed and then reopened, and while the reopen is on its way four
// dragged after two in the strip one two four the user sees, where four is
// already after two. The drop is sent all the same: at its turn, after the
// reopen put three back between two and four, it moves four after two, so
// one two four three.
test('a tab dropped beside the tab it already follows is moved there at its turn', async ({ harness, page }) => {
  const wire = await holdBeforeMove(page, 'reopen');
  const threadId = await seedBrowserThread(harness, 'Browser move beside neighbor');
  const urls = ['https://example.test/one', 'https://example.test/two', 'https://example.test/three', 'https://example.test/four'];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  await harness.open(page);
  await page.getByText('Browser move beside neighbor').click();
  const pane = page.getByTestId('browser-pane');
  const tabs = pane.getByTestId('browser-tab');
  await expect(tabs).toHaveCount(4);

  await tabs.nth(2).click({ button: 'right' });
  await page.getByRole('menu', { name: 'Tab actions' }).getByTestId('browser-tab-menu-close').click();
  await expect(tabs).toHaveCount(3);
  await tabs.nth(0).click({ button: 'right' });
  await page.getByRole('menu', { name: 'Tab actions' }).getByTestId('browser-tab-menu-reopen').click();
  await expect.poll(() => wire.held()).toBe(true);
  // Dropped on the right half of two, four lands after it.
  const target = await tabs.nth(1).boundingBox();
  if (!target) throw new Error('tab two has no box');
  await tabs.nth(2).dragTo(tabs.nth(1), { targetPosition: { x: target.width * 0.75, y: target.height / 2 } });
  await expect.poll(() => wire.sent()).toBe(2);
  wire.release();
  await expect.poll(() => wire.answered()).toBe(2);
  expect(await tabURLs(harness, threadId)).toEqual([urls[0], urls[1], urls[3], urls[2]]);
  await expect(tabs).toHaveCount(4);
});
