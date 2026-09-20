// `/side-chat`: the person's own fork of the thread in front of them.
//
// Coverage: the composer command forks the focused pane's thread mid-turn
// into a hidden scratch thread (`ForkSideChat`) and opens it in a companion
// pane beside its source, without touching the running turn or the sidebar;
// Keep promotes the scratch thread into an ordinary sidebar thread
// (`PromoteScratchThread`) and swaps the companion for a normal pane;
// closing the side chat, closing the source pane it hangs from, and
// switching that pane to another thread delete the scratch thread; and a
// reload restores the source pane alone, because no side-chat pane is ever
// persisted. Spec: docs/specs/agent-thread-tools.md.
import { test, expect, type SeedResult } from './fixtures.js';
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { plainScenario, setScenario, threadRows, threadToolsScenario } from './thread-tools-helpers.js';

const SOURCE_TITLE = 'Main work';
const SIDE_CHAT_TITLE = `Side chat: ${SOURCE_TITLE}`;

function sourcePane(page: Page) {
  return page.locator('section[data-pane-kind="thread"]');
}

function sideChatPane(page: Page) {
  return page.locator('section[data-pane-kind="side-chat"]');
}

/**
 * Seed one project with the thread a side chat is cut from, plus any other
 * thread a test switches to. Each carries a turn so the sidebar lists it.
 */
async function seedSource(
  harness: HarnessApp,
  otherTitles: string[] = [],
): Promise<{ threadId: string; path: string; otherIds: string[] }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'side-chat',
        repo: {},
        threads: [SOURCE_TITLE, ...otherTitles].map((title) => ({
          title,
          provider: 'claude',
          turns: [
            {
              userText: 'start the migration',
              items: [{ kind: 'assistant_text', summary: 'The migration starts in schema.sql.' }],
            },
          ],
        })),
      },
    ],
  });
  const [threadId, ...otherIds] = seed.projects[0].threadIds;
  return { threadId, path: seed.projects[0].path, otherIds };
}

/** Run one turn in the source pane so the thread has a session to fork. */
async function runOneTurn(page: Page, harness: HarnessApp, text: string): Promise<void> {
  await sourcePane(page).getByLabel('Message Input').fill(text);
  await sourcePane(page).getByTestId('composer-send').click();
  await harness.waitForEvent('provider:turn_completed');
}

/** Type the command the user types, and wait for the pane it opens. */
async function runSideChat(page: Page): Promise<void> {
  await sourcePane(page).getByLabel('Message Input').fill('/side-chat');
  await sourcePane(page).getByTestId('composer-send').click();
  await expect(sideChatPane(page)).toHaveCount(1);
}

interface ScratchRow {
  id: string;
  title: string;
  mode: string;
  forkedFromThreadId?: string;
}

/** localStorage key this context's appStorage bucket lives under. */
const APP_STORAGE_BUCKET_KEY = 'agent-overflow:uistate:bucket';

/**
 * The pane layout this browser context persisted, as the raw JSON it holds.
 * That bucket is the durable copy a reload restores from.
 */
async function persistedPaneLayout(page: Page): Promise<string> {
  return await page.evaluate((key) => {
    const bucket = JSON.parse(localStorage.getItem(key) ?? '{}') as Record<string, string>;
    return bucket.paneLayout ?? '';
  }, APP_STORAGE_BUCKET_KEY);
}

/** The scratch fork of `source`, or undefined once it is deleted. */
async function scratchFork(harness: HarnessApp, source: string): Promise<ScratchRow | undefined> {
  const rows = await threadRows(harness);
  return rows.find((row) => row.forkedFromThreadId === source && row.mode === 'scratch');
}

test('/side-chat forks a running thread into a pane beside it and leaves the turn alone', async ({
  harness,
  page,
}) => {
  const source = await seedSource(harness);
  // The turn parks, so the fork is taken while the source is mid-turn.
  await setScenario(
    harness,
    source.path,
    threadToolsScenario({
      name: 'side-chat-source',
      provider: 'claude',
      turns: [{ steps: [{ gate: 'hold-turn' }], text: 'Migration finished.' }],
    }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await sourcePane(page).getByLabel('Message Input').fill('run the migration');
  await sourcePane(page).getByTestId('composer-send').click();
  const parked = await harness.waitForEvent<{ mockId: string; report: { kind: string } }>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal',
  );

  await runSideChat(page);

  // The fork is a hidden thread that carries the source's history, and it
  // is nowhere in the sidebar.
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();
  expect(fork!.title).toBe(SIDE_CHAT_TITLE);
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Side chat' })).toHaveCount(0);
  await expect(sideChatPane(page).getByTestId('assistant-message-body')).toContainText(
    'The migration starts in schema.sql.',
  );

  // The source pane keeps its own turn: the fork neither interrupted it nor
  // took its stream.
  await harness.rpc('HarnessMockCommand', parked.mockId, { type: 'advance', name: 'hold-turn' });
  await harness.waitForEvent('provider:turn_completed');
  await expect(sourcePane(page).getByTestId('assistant-message-body').last()).toContainText(
    'Migration finished.',
  );
});

test('Keep turns the side chat into an ordinary thread in the same slot', async ({
  harness,
  page,
}) => {
  const source = await seedSource(harness);
  await setScenario(
    harness,
    source.path,
    plainScenario({
      name: 'side-chat-keep',
      provider: 'claude',
      texts: ['Looking at it now.'],
    }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await runOneTurn(page, harness, 'check the migration');
  await runSideChat(page);
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();

  await sideChatPane(page).getByTestId('side-chat-keep').click();

  // The companion becomes an ordinary thread pane, and the thread it held
  // is now in the sidebar with the mode it was forked from.
  await expect(sideChatPane(page)).toHaveCount(0);
  await expect(sourcePane(page)).toHaveCount(2);
  await expect(page.getByTestId('thread-row').filter({ hasText: SIDE_CHAT_TITLE })).toBeVisible();
  await expect
    .poll(async () => (await threadRows(harness)).find((row) => row.id === fork!.id)?.mode)
    .toBe('chat');
});

test('closing the side chat deletes the thread it held', async ({ harness, page }) => {
  const source = await seedSource(harness);
  await setScenario(
    harness,
    source.path,
    plainScenario({ name: 'side-chat-close', provider: 'claude', texts: ['On it.'] }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await runOneTurn(page, harness, 'look at the migration');
  await runSideChat(page);
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();

  await sideChatPane(page).getByTestId('pane-close').click();
  await expect(sideChatPane(page)).toHaveCount(0);
  await expect
    .poll(async () => (await threadRows(harness)).some((row) => row.id === fork!.id))
    .toBe(false);
});

test('closing the source pane takes its side chat and the thread with it', async ({
  harness,
  page,
}) => {
  const source = await seedSource(harness);
  await setScenario(
    harness,
    source.path,
    plainScenario({ name: 'side-chat-source-close', provider: 'claude', texts: ['On it.'] }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await runOneTurn(page, harness, 'read the migration');
  await runSideChat(page);
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();

  await sourcePane(page).getByTestId('pane-close').click();
  await expect(sideChatPane(page)).toHaveCount(0);
  await expect
    .poll(async () => (await threadRows(harness)).some((row) => row.id === fork!.id))
    .toBe(false);
});

test('switching the source pane to another thread closes its side chat and deletes the fork', async ({
  harness,
  page,
}) => {
  const source = await seedSource(harness, ['Other work']);
  const other = source.otherIds[0];
  await setScenario(
    harness,
    source.path,
    plainScenario({ name: 'side-chat-switch', provider: 'claude', texts: ['On it.'] }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await runOneTurn(page, harness, 'read the migration');
  await runSideChat(page);
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();

  // The side chat is a fork of the thread the pane showed when it opened;
  // the pane moving to another thread takes it down like a close would.
  await page.getByTestId('thread-row').filter({ hasText: 'Other work' }).click();
  await expect(sideChatPane(page)).toHaveCount(0);
  await expect(sourcePane(page)).toHaveCount(1);
  await expect(sourcePane(page).locator(`[data-ui-surface="chat"][data-thread-id="${other}"]`)).toBeVisible();
  await expect
    .poll(async () => (await threadRows(harness)).some((row) => row.id === fork!.id))
    .toBe(false);
});

test('a reload brings the source pane back without its side chat', async ({ harness, page }) => {
  const source = await seedSource(harness);
  await setScenario(
    harness,
    source.path,
    plainScenario({ name: 'side-chat-reload', provider: 'claude', texts: ['On it.'] }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await runOneTurn(page, harness, 'reread the migration');
  await runSideChat(page);
  const fork = await scratchFork(harness, source.threadId);
  expect(fork).toBeDefined();

  // The persisted layout is the source pane alone: a side chat is left out
  // of it deliberately, since its thread cannot outlive the pane. Waiting
  // for the source pane's own entry is what makes the reload below restore
  // a layout written while the side chat was open.
  await expect.poll(() => persistedPaneLayout(page)).toContain(source.threadId);
  expect(await persistedPaneLayout(page)).not.toContain(fork!.id);

  await page.reload();
  await expect(sourcePane(page)).toHaveCount(1);
  await expect(sourcePane(page).getByTestId('assistant-message-body').first()).toContainText(
    'The migration starts in schema.sql.',
  );
  await expect(sideChatPane(page)).toHaveCount(0);
  // Nor is the thread it held anywhere a person can reach it: the boot
  // sweep on the next restart is what finally deletes it.
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Side chat' })).toHaveCount(0);
});
