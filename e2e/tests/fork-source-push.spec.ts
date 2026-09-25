// A pane showing a pointer fork is never told about its source's writes
// (docs/architecture/thread-replica-sync.md#pointer-fork-stamps). A fork
// reads the rows before its cut from the thread that holds them, and a row
// a fork shows never changes: the source's later rows land past the cut or
// are hidden from the fork, a revert moves the rows the fork shows to a
// hidden holder, and a deleted source becomes that holder in place. So the
// fork's rows, stamps and pane stay as they were, and no frame names it.
//
//   - The source's agent keeps writing after the fork: a background agent
//     launched in the forked turn works and completes. The fork hides the
//     launch that is still running when it is made (forkUnsettledRowsTx),
//     and the agent's rows land past the cut, so no spawn row, card or
//     child row appears in it.
//   - The source reverts every row the fork shows (edit and resend of its
//     first message). The source loses them; the fork's pane keeps them.
//   - The source is deleted. The fork still shows everything it showed, and
//     the holder that keeps the rows appears in no listing.
//
// The store's ownership transfer and its costs are unit tested
// (fork_holders_test.go, thread_delete_holder_test.go, fork_cost_test.go),
// and the holder's collection once its last fork goes is app tested
// (app_holder_collection_test.go). This level shows no frame crossing to a
// connection that shows the fork, and the SPA's fork pane unchanged.
import type { Page } from '@playwright/test';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
  type Item,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';
import { plainScenario } from './thread-tools-helpers.js';
import { readWire, recordWire, type WireLog } from './transport-watch-helpers.js';

const SOURCE_TITLE = 'Fork push source';
const REVERT_TITLE = 'Fork revert source';
const WORK_GATE = 'fork-push-work';
const EDITED = 'Start over with a different question.';

interface ItemEvent {
  action: string;
  threadId: string;
  item?: { completionOf?: string };
}

interface ThreadEvent {
  action: string;
  id?: string;
}

interface ThreadRow {
  id: string;
}

// A thread title appears in the sidebar and the pane header, and the
// source's title is a prefix of its fork's.
const exactly = (title: string) => new RegExp(`^${title.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`);

const rowShape = (rows: Item[]) => rows.map((row) => [row.id, row.kind, row.status, row.summary, row.meta]);

// Every item frame the harness saw for a thread, of any action.
const framesFor = (harness: HarnessApp, id: string) =>
  harness.countEvents<ItemEvent>('provider:item_event', (event) => event.threadId === id);

test.beforeEach(async ({ harness }) => {
  // Tool runs render expanded, so a spawn row would render as its own row.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

// Deletes sourceId through the RPC and waits for the page to receive its
// `deleted` broadcast, which follows the delete's last write.
async function deleteSource(harness: HarnessApp, page: Page, sourceId: string) {
  const sourceGone = harness.waitForEvent<ThreadEvent>(
    'thread:updated',
    (event) => event.action === 'deleted' && event.id === sourceId,
  );
  const received = page.waitForFunction(
    (id) =>
      (window as unknown as { __aoWire: WireLog }).__aoWire.received.some(
        (event) => event.channel === 'thread:updated' && event.action === 'deleted' && event.threadId === id,
      ),
    sourceId,
  );
  await harness.rpc('DeleteThread', sourceId);
  await sourceGone;
  await received;
  // The holder the source became is in no listing.
  const listed = await harness.rpc<ThreadRow[]>('ListThreads');
  expect(listed.map((thread) => thread.id)).not.toContain(sourceId);
}

// The page received no item frame for the fork.
async function expectNoForkFrames(page: Page, forkId: string) {
  const wire = await readWire(page);
  expect(wire.received.filter((event) => event.channel === 'provider:item_event' && event.threadId === forkId)).toEqual([]);
}

test('an open fork pane keeps its snapshot while the source’s agent writes and after the source is deleted', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('fork-source-push', [
      emit([
        ...textLines('msg-lead', 'Launching the reviewer.'),
        toolUseLine('msg-outer', 'tu-outer', 'Agent', {
          description: 'outer reviewer',
          subagent_type: 'outer-reviewer',
        }),
        taskStartedLine('task-outer', 'tu-outer', 'outer reviewer'),
        asyncAgentAckLine('tu-outer', 'task-outer', 'outer reviewer'),
        backgroundTasksChangedLine([
          { task_id: 'task-outer', task_type: 'local_agent', description: 'outer reviewer' },
        ]),
        RESULT_LINE,
      ]),
      // The fork is taken here, with the agent still running.
      { waitSignal: { name: WORK_GATE } },
      emit([
        toolUseLine('msg-grep', 'tu-grep', 'Grep', { pattern: 'drift' }, 'tu-outer'),
        toolResultLine('tu-grep', 'no matches', { parentToolUseId: 'tu-outer' }),
        taskUpdatedLine('task-outer', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-outer', 'tu-outer', 'Outer review done.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const sourceId = await seedAgentThread(harness, 'fork-source-push', SOURCE_TITLE);
  await recordWire(page);
  await harness.open(page);
  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: exactly(title) });
  await sidebarRow(SOURCE_TITLE).click();
  const mockId = await startMock(harness, sourceId);
  const turnDone = harness.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (event) => event.threadId === sourceId,
  );
  await harness.rpc('SendMessage', sourceId, 'review in the background', null);
  await turnDone;
  await waitForGate(harness, WORK_GATE);

  const fork = await harness.rpc<{ id: string; title: string }>('ForkThread', sourceId, null);
  await sidebarRow(fork.title).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText(fork.title);
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.getByText('Ready.', { exact: true })).toBeVisible();
  await expect(timeline.getByText('Launching the reviewer.', { exact: true })).toBeVisible();
  // The source's launch is running; the fork does not show it.
  const launch = (await listItems(harness, sourceId)).find((row) => row.id === 'tu-outer');
  expect([launch?.status, launch?.isBackground]).toEqual(['running', true]);
  await expect(timeline.locator('[data-item-id="tu-outer"]')).toHaveCount(0);
  const forkRows = rowShape(await listItems(harness, fork.id));
  expect(forkRows.map(([id]) => id)).not.toContain('tu-outer');

  // The source's agent writes a child row and completes. The completion
  // row's upsert is the barrier: a frame those writes sent the fork would
  // be emitted in the write's commit, before it.
  const agentDone = harness.waitForEvent<ItemEvent>(
    'provider:item_event',
    (event) =>
      event.threadId === sourceId && event.action === 'upsert' && event.item?.completionOf === 'tu-outer',
  );
  await advance(harness, mockId, WORK_GATE);
  await agentDone;
  const sourceRows = await listItems(harness, sourceId);
  expect(sourceRows.find((row) => row.id === 'tu-grep')?.parentId).toBe('tu-outer');
  expect(sourceRows.filter((row) => row.completionOf === 'tu-outer')).toHaveLength(1);

  // The fork shows what it showed when it was made.
  expect(framesFor(harness, fork.id)).toBe(0);
  expect(rowShape(await listItems(harness, fork.id))).toEqual(forkRows);
  await expect(timeline.locator('[data-item-id="tu-outer"]')).toHaveCount(0);
  await expect(timeline.locator('[data-item-id="tu-grep"]')).toHaveCount(0);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await expect(timeline.getByText('Ready.', { exact: true })).toBeVisible();

  // Deleting the source, with the fork pane still open.
  await deleteSource(harness, page, sourceId);
  await expect(sidebarRow(SOURCE_TITLE)).toHaveCount(0);
  expect(framesFor(harness, fork.id)).toBe(0);
  expect(rowShape(await listItems(harness, fork.id))).toEqual(forkRows);
  await expect(page.getByTestId('chat-header-title')).toHaveText(fork.title);
  await expect(timeline.getByText('Ready.', { exact: true })).toBeVisible();
  await expect(timeline.getByText('Launching the reviewer.', { exact: true })).toBeVisible();
  await expectNoForkFrames(page, fork.id);
});

test('an open fork pane keeps the rows its source reverts, then deletes', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    // Each session starts the scenario over, so the resend after the
    // revert is answered with the same text.
    scenario: plainScenario({ name: 'fork-revert-source', provider: 'claude', texts: ['Descending works too.'], afterTurns: 'repeatLast' }),
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'fork-revert-source',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [
          {
            title: REVERT_TITLE,
            provider: 'claude',
            turns: [
              { userText: 'How do I sort an array in JS?', items: [{ kind: 'assistant_text', summary: 'Use Array.prototype.sort.' }] },
              { userText: 'And in reverse?', items: [{ kind: 'assistant_text', summary: 'Reverse the comparator.' }] },
            ],
          },
        ],
      },
    ],
  });
  const sourceId = seed.projects[0].threadIds[0];
  await recordWire(page);
  await harness.open(page);
  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: exactly(title) });
  // A Claude thread forks once it has a session.
  await startMock(harness, sourceId);
  const turnDone = harness.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (event) => event.threadId === sourceId,
  );
  await harness.rpc('SendMessage', sourceId, 'And descending?', null);
  await turnDone;

  const fork = await harness.rpc<{ id: string; title: string }>('ForkThread', sourceId, null);
  const shown = ['How do I sort an array in JS?', 'Use Array.prototype.sort.', 'And in reverse?', 'Reverse the comparator.', 'And descending?', 'Descending works too.'];
  const forkRows = rowShape(await listItems(harness, fork.id));
  expect(forkRows.map(([, , , summary]) => summary)).toEqual(expect.arrayContaining(shown));
  await sidebarRow(fork.title).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText(fork.title);
  const timeline = page.getByTestId('message-timeline-scroll');
  const expectShown = async () => {
    for (const text of shown) await expect(timeline.getByText(text, { exact: true })).toBeVisible();
  };
  await expectShown();

  // The source reverts to its first message and resends it edited: every
  // row the fork shows leaves the source.
  const first = (await listItems(harness, sourceId)).find((row) => row.kind === 'user_text' && row.summary === shown[0]);
  const resent = harness.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (event) => event.threadId === sourceId,
  );
  await harness.rpc('RevertConversationAndResendMessage', sourceId, first?.id, { content: EDITED });
  await resent;
  const sourceSummaries = (await listItems(harness, sourceId)).map((row) => row.summary);
  expect(sourceSummaries).toEqual([EDITED, 'Descending works too.']);
  expect(framesFor(harness, fork.id)).toBe(0);
  expect(rowShape(await listItems(harness, fork.id))).toEqual(forkRows);
  await expectShown();
  await expect(timeline.getByText(EDITED, { exact: true })).toHaveCount(0);

  await deleteSource(harness, page, sourceId);
  await expect(sidebarRow(REVERT_TITLE)).toHaveCount(0);
  expect(framesFor(harness, fork.id)).toBe(0);
  expect(rowShape(await listItems(harness, fork.id))).toEqual(forkRows);
  await expectShown();
  await expectNoForkFrames(page, fork.id);
});
