// A backend refresh that was reading while the reader jumped.
//
// Coverage: a reconnect refreshes every pane (`computerHydration`). One
// pane's refresh is held at its live-state read, the reader jumps to the
// first message through the nav rail, and the held read is released. The
// refresh's page describes the tail window it started from, so it is not
// applied: the jumped rows stay on screen with Load newer and without
// Load older, and the refresh reruns against the jumped window. A later
// refresh keeps the position. Run in a thread pane and in a side chat
// pane. Unit coverage of the rule: threadWindowSupersede.svelte.test.ts.
import type { Locator, Page, WebSocketRoute } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';
import { methodNameById } from './offhost-helpers.js';
import { plainScenario, setScenario, threadRows } from './thread-tools-helpers.js';

const TITLE = 'Refresh across a jump';
const TURNS = 160;

async function seedThread(harness: HarnessApp): Promise<{ threadId: string; path: string }> {
  const turns = Array.from({ length: TURNS }, (_, i) => ({
    userText: `Rail question ${i}`,
    items: [{ kind: 'assistant_text', summary: `Rail answer ${i}\n\n${'A detailed answer. '.repeat(40)}` }],
  }));
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'refresh-jump', repo: {}, threads: [{ title: TITLE, provider: 'claude', turns }] }],
  });
  return { threadId: seed.projects[0].threadIds[0], path: seed.projects[0].path };
}

/** The page's socket, proxied to hold one thread's live-state read and to drop the connection. */
async function proxyPageSocket(page: Page) {
  let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  let holdLiveStateOf: string | null = null;
  const held: Array<() => void> = [];
  const recentTurnReads = new Map<string, number>();
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    connection = { page: socket, server };
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; method?: string; methodId?: number; params?: unknown[] };
      if (frame.type === 'rpc') {
        const name = frame.method ?? methodNameById(frame.methodId ?? 0);
        const threadId = typeof frame.params?.[0] === 'string' ? frame.params[0] : '';
        // Read right after a refresh installs its page (`runBackendRefresh`).
        if (name === 'ListRecentTurns') recentTurnReads.set(threadId, (recentTurnReads.get(threadId) ?? 0) + 1);
        if (name === 'GetThreadLiveState' && threadId && threadId === holdLiveStateOf) {
          holdLiveStateOf = null;
          held.push(() => server.send(message));
          return;
        }
      }
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  return {
    held,
    installs: (threadId: string) => recentTurnReads.get(threadId) ?? 0,
    /** Drop the connection; the client reconnects and refreshes every pane. */
    async reconnect(options: { holdLiveStateOf?: string } = {}): Promise<void> {
      const current = connection!;
      holdLiveStateOf = options.holdLiveStateOf ?? null;
      await current.page.close({ code: 1012 });
      await current.server.close();
    },
    release(): void {
      for (const send of held.splice(0)) send();
    },
  };
}

type PageWire = Awaited<ReturnType<typeof proxyPageSocket>>;

async function expectJumpedWindow(pane: Locator): Promise<void> {
  await expect(pane.getByText('Rail question 0', { exact: true })).toBeInViewport();
  await expect(pane.getByTestId('load-newer-messages')).toBeVisible();
  await expect(pane.getByTestId('load-older-messages')).toHaveCount(0);
}

async function refreshAcrossJump(wire: PageWire, pane: Locator, threadId: string): Promise<void> {
  const installs = wire.installs(threadId);
  await wire.reconnect({ holdLiveStateOf: threadId });
  await expect.poll(() => wire.held.length).toBe(1);

  await pane.getByRole('button', { name: 'Jump to first message', exact: true }).click();
  await expectJumpedWindow(pane);

  wire.release();
  await expect.poll(() => wire.installs(threadId)).toBeGreaterThan(installs);
  await expectJumpedWindow(pane);

  const settled = wire.installs(threadId);
  await wire.reconnect();
  await expect.poll(() => wire.installs(threadId)).toBeGreaterThan(settled);
  await expectJumpedWindow(pane);
}

test('a refresh read before a jump leaves the jumped rows in a thread pane', async ({ harness, page }) => {
  const wire = await proxyPageSocket(page);
  const { threadId } = await seedThread(harness);
  await harness.open(page);
  await page.getByText(TITLE, { exact: true }).click();
  const pane = page.locator('section[data-pane-kind="thread"]');
  await expect(pane.getByText(`Rail question ${TURNS - 1}`, { exact: true })).toBeVisible();

  await refreshAcrossJump(wire, pane, threadId);
});

test('a refresh read before a jump leaves the jumped rows in a side chat pane', async ({ harness, page }) => {
  const wire = await proxyPageSocket(page);
  const { threadId, path } = await seedThread(harness);
  await setScenario(harness, path, plainScenario({ name: 'refresh-jump-source', provider: 'claude', texts: ['Session ready.'] }));
  await harness.open(page);
  await page.getByText(TITLE, { exact: true }).click();
  const source = page.locator('section[data-pane-kind="thread"]');
  // One turn gives the source a session to fork.
  const completed = harness.waitForEvent('provider:turn_completed');
  await source.getByLabel('Message Input').fill('start a session');
  await source.getByTestId('composer-send').click();
  await completed;
  await source.getByLabel('Message Input').fill('/side-chat');
  await source.getByTestId('composer-send').click();
  const side = page.locator('section[data-pane-kind="side-chat"]');
  await expect(side.getByTestId('side-chat-preparing')).toHaveCount(0);
  await expect(side.getByText('Session ready.', { exact: true })).toBeVisible();
  const fork = (await threadRows(harness)).find((row) => row.forkedFromThreadId === threadId && row.mode === 'scratch');
  expect(fork).toBeDefined();

  await refreshAcrossJump(wire, side, fork!.id);
  // The source pane's own refresh was never held and stays at its tail.
  await expect(source.getByText('Session ready.', { exact: true })).toBeInViewport();
});
