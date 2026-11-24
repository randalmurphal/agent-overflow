// The browser's address suggestions (docs/architecture/browser-tools.md,
// the pane's chrome). Typing offers the thread's other open tabs, then
// its dev servers, then the addresses its pages recently opened; arrows
// move, Enter takes the highlighted row, a click takes a row, and Escape
// closes the list before it reverts the field. A tab suggestion switches to
// that tab; any other navigates the field's tab. A cookie jar switch forgets
// the recent addresses. A dev server started in the thread's terminal after
// the pane opened is found by the scan the field's focus runs. A draft
// belongs to its tab: a tab switch shows the new tab's address and Enter
// then navigates that tab to it. Text typed after choosing a tab suggestion
// is that tab's draft until the state shows the switch or the user selects
// another tab; then it is the shown tab's, though the switch's answer is
// still on its way.
import { connect } from 'node:net';
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { unusedPort } from '../src/ports.js';
import { test, expect, type SeedResult } from './fixtures.js';

interface BrowserState {
  activePageId?: string;
  pages?: Array<{ id: string; url: string }>;
}

test('typing offers open tabs and recent addresses, by keyboard and by mouse', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-suggestions',
      repo: {},
      threads: [{ title: 'Browser suggestions', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.startSession(threadId);
  const activeURL = async (): Promise<string> => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    return state.pages?.find((tab) => tab.id === state.activePageId)?.url ?? '';
  };

  await harness.open(page);
  await page.getByText('Browser suggestions').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();
  await address.fill('https://example.test/first');
  await address.press('Enter');
  await expect.poll(activeURL).toBe('https://example.test/first');
  await page.keyboard.press('ControlOrMeta+t');
  await expect(address).toBeFocused();
  await address.fill('https://example.test/second');
  await address.press('Enter');
  await expect.poll(activeURL).toBe('https://example.test/second');

  const list = page.getByTestId('browser-address-suggestions');
  const rows = list.getByRole('option');
  await page.keyboard.press('ControlOrMeta+l');
  await address.pressSequentially('first');
  // The other tab, offered once although it is also a recent address.
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toHaveAttribute('data-kind', 'tab');
  await expect(rows.first()).toContainText('Switch to tab');
  await address.press('ArrowDown');
  await expect(rows.first()).toHaveAttribute('aria-selected', 'true');
  await expect(address).toHaveValue('first');
  await address.press('Enter');
  await expect.poll(activeURL).toBe('https://example.test/first');
  await expect(list).toHaveCount(0);
  await expect(address).toHaveValue('https://example.test/first');

  // Escape closes the list and keeps the text, then reverts on the next press.
  await page.keyboard.press('ControlOrMeta+l');
  await address.pressSequentially('example');
  await expect(rows).toHaveCount(1);
  await address.press('Escape');
  await expect(list).toHaveCount(0);
  await expect(address).toHaveValue('example');
  await expect(address).toBeFocused();
  await address.press('Escape');
  await expect(address).toHaveValue('https://example.test/first');
  await expect(address).not.toBeFocused();

  // With the second tab closed, its address is a recent one; a click
  // navigates this tab to it.
  await pane.getByTestId('browser-tab-close').nth(1).click();
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);
  expect((await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId)).pages?.map((tab) => tab.url)).toEqual(['https://example.test/first']);
  await address.click();
  await address.pressSequentially('second');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toHaveAttribute('data-kind', 'recent');
  await rows.first().click();
  await expect.poll(activeURL).toBe('https://example.test/second');
  await expect(address).toHaveValue('https://example.test/second');

  // A cookie jar switch forgets them.
  await pane.getByTestId('browser-jar').click();
  await page.getByTestId('browser-jar-thread').click();
  await page.getByRole('dialog').getByRole('button', { name: 'Switch cookie jar' }).click();
  await expect(pane.getByTestId('browser-jar')).toHaveAttribute('aria-label', 'Cookie jar: This browser only');
  await address.click();
  await address.pressSequentially('first');
  await expect(address).toHaveValue('first');
  await expect(list).toHaveCount(0);
});

test('a dev server the thread runs is offered, read when the field takes focus', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-dev-server',
      repo: {},
      threads: [{ title: 'Browser dev server', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.startSession(threadId);
  await harness.open(page);
  await page.getByText('Browser dev server').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();

  // Started after the pane, in the thread's own terminal, so the scan
  // attributes it to the thread and only a read on focus can find it.
  const port = await unusedPort();
  const terminal = await harness.rpc<{ terminalID: string }>('OpenTerminal', threadId, { cwd: seed.projects[0].path });
  const server = `require('http').createServer((q,r)=>{r.setHeader('content-type','text/html');r.end('<title>dev</title>')}).listen(${port},'127.0.0.1')`;
  const command = `exec '${process.execPath}' -e "${server}"\n`;
  await harness.rpc('WriteTerminal', terminal.terminalID, Buffer.from(command).toString('base64'));
  try {
    // Waited for on the socket, not through a dev-server read, which would
    // push the list to the page and stand in for the read on focus.
    await expect.poll(() => listening(port), { timeout: 20_000 }).toBe(true);

    await pane.click({ position: { x: 5, y: 200 } });
    await page.keyboard.press('ControlOrMeta+l');
    await address.pressSequentially(String(port));
    const rows = page.getByTestId('browser-address-suggestions').getByRole('option');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toHaveAttribute('data-kind', 'dev-server');
    await address.press('ArrowDown');
    await address.press('Enter');
    await expect(address).toHaveValue(`http://localhost:${port}/`);
  } finally {
    await harness.rpc('CloseTerminal', terminal.terminalID);
  }
});

function listening(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = connect(port, '127.0.0.1');
    socket.once('connect', () => {
      socket.destroy();
      resolve(true);
    });
    socket.once('error', () => resolve(false));
  });
}

const BROWSER_COMPANION_DO = 197228034;

interface Navigation { pageId: string; address: string }

/**
 * The pane's navigations as they leave for the backend. Once `hold` names a
 * kind, the first call of that kind is held on its way to the backend until
 * `release`; once `holdReply` names one, the first call of that kind reaches
 * the backend, whose events pass, and only its answer is held until
 * `releaseReply`.
 */
async function watchWire(page: Page) {
  const wire: {
    navigations: Navigation[];
    hold?: string; held?: string; send?: (message: string) => void;
    holdReply?: string; replyId?: string; heldReply?: string; answer?: (message: string) => void;
  } = { navigations: [] };
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    wire.send = (message) => server.send(message);
    wire.answer = (message) => socket.send(message);
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const action = frame.type === 'rpc' && frame.methodId === BROWSER_COMPANION_DO ? frame.params?.[1] : null;
      if (action?.kind === 'navigate') wire.navigations.push({ pageId: action.pageId, address: action.address });
      if (action && wire.hold === action.kind && wire.held === undefined) {
        wire.held = String(message);
        return;
      }
      if (action && wire.holdReply === action.kind && wire.replyId === undefined) wire.replyId = String(frame.id);
      server.send(message);
    });
    server.onMessage((message) => {
      if (wire.replyId !== undefined && wire.heldReply === undefined) {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && String(frame.id) === wire.replyId) {
          wire.heldReply = String(message);
          return;
        }
      }
      socket.send(message);
    });
  });
  return {
    navigations: () => wire.navigations,
    hold: (kind: string) => { wire.hold = kind; },
    held: () => wire.held !== undefined,
    release: () => wire.send?.(wire.held ?? ''),
    holdReply: (kind: string) => { wire.holdReply = kind; },
    replyHeld: () => wire.heldReply !== undefined,
    releaseReply: () => wire.answer?.(wire.heldReply ?? ''),
  };
}

/** A thread with a tab on each of `urls`, the last active. */
async function seedTabs(harness: HarnessApp, title: string, urls: string[]): Promise<{ threadId: string; ids: string[] }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'browser-drafts', repo: {}, threads: [{ title, turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }] }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.startSession(threadId);
  const ids: string[] = [];
  for (const address of urls) {
    const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
    ids.push(opened.activePageId ?? '');
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId: opened.activePageId ?? '', address });
  }
  return { threadId, ids };
}

async function tabState(harness: HarnessApp, threadId: string): Promise<{ active: string; urls: string[] }> {
  const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  return { active: state.pages?.find((tab) => tab.id === state.activePageId)?.url ?? '', urls: state.pages?.map((tab) => tab.url) ?? [] };
}

test('a draft typed on a tab is not submitted to the tab switched to', async ({ harness, page }) => {
  const wire = await watchWire(page);
  const urls = ['https://example.test/one', 'https://example.test/two'];
  const { threadId, ids } = await seedTabs(harness, 'Browser draft switch', urls);
  await harness.open(page);
  await page.getByText('Browser draft switch').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(pane.getByTestId('browser-tab')).toHaveCount(2);
  await expect(address).toHaveValue(urls[1]);

  await address.click();
  await address.fill('https://unsubmitted-for-two.test/');
  await page.keyboard.press('Control+Tab');
  await expect.poll(async () => (await tabState(harness, threadId)).active).toBe(urls[0]);
  await expect(address).toHaveValue(urls[0]);
  await address.press('Enter');
  await expect.poll(() => wire.navigations()).toEqual([{ pageId: ids[0], address: urls[0] }]);
  expect((await tabState(harness, threadId)).urls).toEqual(urls);
});

test("text typed after choosing a tab suggestion is that tab's draft", async ({ harness, page }) => {
  const wire = await watchWire(page);
  const urls = ['https://example.test/one', 'https://example.test/two'];
  const { threadId, ids } = await seedTabs(harness, 'Browser draft suggestion', urls);
  await harness.open(page);
  await page.getByText('Browser draft suggestion').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toHaveValue(urls[1]);

  wire.hold('activate');
  await address.click();
  await address.pressSequentially('one');
  const rows = page.getByTestId('browser-address-suggestions').getByRole('option');
  await expect(rows).toHaveCount(1);
  await address.press('ArrowDown');
  await address.press('Enter');
  await expect.poll(() => wire.held()).toBe(true);
  // The switch is on its way; the user starts the address for that tab.
  await address.fill('https://next.test/');
  wire.release();
  await expect.poll(async () => (await tabState(harness, threadId)).active).toBe(urls[0]);
  await expect(pane.getByTestId('browser-tab').nth(0)).toHaveAttribute('aria-selected', 'true');
  await expect(address).toHaveValue('https://next.test/');
  await expect(address).toBeFocused();
  await address.press('Enter');
  await expect.poll(() => wire.navigations()).toEqual([{ pageId: ids[0], address: 'https://next.test/' }]);
});

test('text typed after a chosen tab switch was shown and the user moved on is the shown tab\'s', async ({ harness, page }) => {
  const wire = await watchWire(page);
  const urls = ['https://example.test/one', 'https://example.test/two'];
  const { threadId, ids } = await seedTabs(harness, 'Browser draft moved on', urls);
  await harness.open(page);
  await page.getByText('Browser draft moved on').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toHaveValue(urls[1]);

  // The switch to one reaches the backend and its state is shown; only its
  // answer is held.
  wire.holdReply('activate');
  await address.click();
  await address.pressSequentially('one');
  const rows = page.getByTestId('browser-address-suggestions').getByRole('option');
  await expect(rows).toHaveCount(1);
  await address.press('ArrowDown');
  await address.press('Enter');
  await expect.poll(() => wire.replyHeld()).toBe(true);
  await expect(pane.getByTestId('browser-tab').nth(0)).toHaveAttribute('aria-selected', 'true');

  // The user goes back to two and types an address there.
  await address.focus();
  await page.keyboard.press('Control+Tab');
  await expect.poll(async () => (await tabState(harness, threadId)).active).toBe(urls[1]);
  await expect(pane.getByTestId('browser-tab').nth(1)).toHaveAttribute('aria-selected', 'true');
  await address.fill('https://typed-on-two.test/');
  await address.press('Enter');
  await expect.poll(() => wire.navigations()).toEqual([{ pageId: ids[1], address: 'https://typed-on-two.test/' }]);
  wire.releaseReply();
  await expect.poll(async () => (await tabState(harness, threadId)).urls).toEqual([urls[0], 'https://typed-on-two.test/']);
});
