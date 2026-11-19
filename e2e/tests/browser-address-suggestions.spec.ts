// The browser's address suggestions (docs/architecture/browser-tools.md,
// the pane's chrome). Typing offers the thread's other open tabs, then
// its dev servers, then the addresses its pages recently opened; arrows
// move, Enter takes the highlighted row, a click takes a row, and Escape
// closes the list before it reverts the field. A tab suggestion switches to
// that tab; any other navigates the field's tab. A cookie jar switch forgets
// the recent addresses. A dev server started in the thread's terminal after
// the pane opened is found by the scan the field's focus runs.
import { connect, createServer, type AddressInfo } from 'node:net';
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
  await harness.rpc('StartSession', threadId);
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
  const tabs = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
  const second = tabs.pages?.find((tab) => tab.url === 'https://example.test/second');
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'close', pageId: second?.id ?? '' });
  await expect(pane.getByTestId('browser-tab')).toHaveCount(1);
  await address.click();
  await address.pressSequentially('second');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toHaveAttribute('data-kind', 'recent');
  await rows.first().click();
  await expect.poll(activeURL).toBe('https://example.test/second');
  await expect(address).toHaveValue('https://example.test/second');

  // A cookie jar switch forgets them.
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'jar', jar: 'thread', confirmed: true });
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
  await harness.rpc('StartSession', threadId);
  await harness.open(page);
  await page.getByText('Browser dev server').click();
  await page.getByTestId('browser-companion-toggle').click();
  const pane = page.getByTestId('browser-pane');
  const address = pane.getByRole('textbox', { name: 'Address' });
  await expect(address).toBeFocused();

  // Started after the pane, in the thread's own terminal, so the scan
  // attributes it to the thread and only a read on focus can find it.
  const port = await freePort();
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

async function freePort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return port;
}
