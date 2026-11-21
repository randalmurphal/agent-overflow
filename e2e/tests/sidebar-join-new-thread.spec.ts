// A screen that connects while another screen's new thread is being created
// lists that thread in its sidebar. The sidebar's catalog read (ListThreads)
// lists a never-sent thread only once it is real, here by its draft content,
// so the save that gives it content is what announces the thread as listed,
// to every screen connected by then.
// Covers: a screen that loads its catalog between the thread's creation and
// its first draft save lists the thread once the save lands; a screen whose
// catalog answer was read before the thread existed and arrives after the
// announcement keeps the thread; and the same join at the natural timing,
// a second screen opened right after the first one starts typing.
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { methodIdByName } from './offhost-helpers.js';

const threadRows = (harness: HarnessApp) => harness.rpc<Array<{ id: string }>>('HarnessListThreadRows');

async function seedProject(harness: HarnessApp): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'join-new-thread', repo: { commits: [{ message: 'init', files: { 'README.md': '# seed\n' } }] } }],
  });
}

interface Event { channel?: string; data?: { action?: string; thread?: { id?: string } } }
interface Frame extends Event { type?: string; id?: string | number; methodId?: number; events?: Event[] }

/** Holds the page's SaveDraft calls until released. */
async function holdDraftSaves(page: Page) {
  const saveMethod = methodIdByName('SaveDraft');
  const held: Array<() => void> = [];
  let holding = true;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as Frame;
      if (holding && frame.type === 'rpc' && frame.methodId === saveMethod) {
        held.push(() => server.send(message));
        return;
      }
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  return {
    count: () => held.length,
    release: () => { holding = false; held.splice(0).forEach((send) => send()); },
  };
}

/**
 * Counts the page's ListThreads calls and their answers, and holds the
 * answers while `holding` is set. Records the threads announced to the page
 * as listed.
 */
async function watchThreadLists(page: Page, holding: boolean) {
  const listMethod = methodIdByName('ListThreads');
  const calls = new Set<string>();
  const answered = new Set<string>();
  const held: Array<() => void> = [];
  const announced = new Set<string>();
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as Frame;
      if (frame.type === 'rpc' && frame.methodId === listMethod) calls.add(String(frame.id));
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as Frame;
      if (frame.type === 'rpc' && calls.has(String(frame.id))) {
        answered.add(String(frame.id));
        if (holding) {
          held.push(() => socket.send(message));
          return;
        }
      }
      const events = frame.type === 'batch' ? frame.events ?? [] : frame.type === 'event' ? [frame] : [];
      for (const event of events) {
        if (event.channel === 'thread:updated' && event.data?.action === 'listed' && event.data.thread?.id) announced.add(event.data.thread.id);
      }
      socket.send(message);
    });
  });
  return {
    /** Every call sent so far has been answered by the backend. */
    settled: () => calls.size > 0 && answered.size === calls.size,
    announced: (id: string) => announced.has(id),
    release: () => { holding = false; held.splice(0).forEach((send) => send()); },
  };
}

test('a screen that loads its catalog before the new thread’s first draft save lists the thread', async ({ harness, page, browser }) => {
  await seedProject(harness);
  const saves = await holdDraftSaves(page);
  const context = await browser.newContext();
  try {
    await harness.open(page);
    await page.getByTestId('project-item-new-thread').first().click();
    await page.getByLabel('Message Input').fill('half a thought');
    await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
    await expect.poll(saves.count, { message: 'the draft save never fell due' }).toBeGreaterThan(0);

    const other = await context.newPage();
    const lists = await watchThreadLists(other, false);
    await harness.open(other);
    await expect(other.getByTestId('project-item-new-thread').first()).toBeVisible();
    await expect.poll(lists.settled, { message: 'the catalog read never answered' }).toBe(true);
    await expect(other.getByTestId('thread-row')).toHaveCount(0);

    saves.release();
    await expect(other.getByTestId('thread-row')).toHaveCount(1);
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
  } finally {
    await context.close();
  }
});

test('a catalog answer read before the new thread existed does not remove it after it is announced', async ({ harness, page, browser }) => {
  await seedProject(harness);
  const context = await browser.newContext();
  try {
    const other = await context.newPage();
    const lists = await watchThreadLists(other, true);
    await harness.open(page);
    await harness.open(other);
    await expect.poll(lists.settled, { message: 'the catalog read never answered' }).toBe(true);

    await page.getByTestId('project-item-new-thread').first().click();
    await page.getByLabel('Message Input').fill('half a thought');
    const listed = async () => (await harness.rpc<Array<{ id: string }> | null>('ListThreads')) ?? [];
    await expect.poll(async () => (await listed()).length).toBe(1);
    const [row] = await listed();
    // The announcement reaches the screen ahead of its held catalog answer.
    // Until that answer, the screen's sidebar has not listed this computer
    // and shows the loading row instead of its rows.
    await expect.poll(() => lists.announced(row.id), { message: 'the thread was never announced' }).toBe(true);
    await expect(other.getByTestId('sidebar-catalog-status')).toBeVisible();
    await expect(other.getByTestId('thread-row')).toHaveCount(0);
    lists.release();
    await expect(other.getByTestId('project-item-new-thread').first()).toBeVisible();
    await expect(other.getByTestId('thread-row')).toHaveCount(1);
    await expect(other.locator(`[data-sidebar-thread-id="${row.id}"]`)).toHaveCount(1);
  } finally {
    await context.close();
  }
});

test('a screen opened right after another screen starts typing in a new thread lists it', async ({ harness, page, browser }) => {
  await seedProject(harness);
  const context = await browser.newContext();
  try {
    const other = await context.newPage();
    await harness.open(page);
    await page.getByTestId('project-item-new-thread').first().click();
    await page.getByLabel('Message Input').fill('half a thought');
    // The row exists from here; its first draft save follows the composer's
    // debounce.
    await expect.poll(async () => (await threadRows(harness)).length, { intervals: [10] }).toBe(1);
    await harness.open(other);
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(other.getByTestId('thread-row')).toHaveCount(1);
  } finally {
    await context.close();
  }
});
