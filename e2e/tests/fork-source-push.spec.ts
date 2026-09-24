// A pane showing a pointer fork follows its source without being reopened
// (docs/architecture/thread-replica-sync.md#pointer-fork-stamps). A fork
// reads the rows before its cut from its source in place. A write that
// changes what the fork shows moves the fork's stamps; the backend then
// pushes the fork a `provider:item_event` `resync`, and a client showing
// the fork re-syncs its window (threadWindowRecovery.ts).
//
// One fork pane stays open through both halves:
//
//   - The source's agent keeps writing after the fork: a background agent
//     launched in the forked turn works and completes. The fork is a
//     snapshot, so none of it reaches the fork. The fork hides a launch
//     that is still running when it is made (forkUnsettledRowsTx), and the
//     agent's rows land past the fork's cut, so no spawn row, card or
//     child row appears in it, its rows stay as they were, and it is
//     pushed no resync.
//   - The source is deleted. The fork is pushed one resync, the rows it
//     read from the source leave the pane and the divider records the
//     deletion.
//
// The store's report of the forks a write moved and the app's emit are
// unit tested (fork_moves_test.go, app_fork_resync_test.go). This level
// shows the frame crossing a narrowed connection and the SPA's window
// following it.
import { expect, test } from './fixtures.js';
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
import { readWire, recordWire, type WireLog } from './transport-watch-helpers.js';

const SOURCE_TITLE = 'Fork push source';
const WORK_GATE = 'fork-push-work';

interface ItemEvent {
  action: string;
  threadId: string;
  item?: { completionOf?: string };
}

interface ThreadEvent {
  action: string;
  id?: string;
}

// A thread title appears in the sidebar and the pane header, and the
// source's title is a prefix of its fork's.
const exactly = (title: string) => new RegExp(`^${title.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`);

const rowShape = (rows: Item[]) => rows.map((row) => [row.id, row.kind, row.status, row.summary, row.meta]);

test.beforeEach(async ({ harness }) => {
  // Tool runs render expanded, so a spawn row would render as its own row.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

test('an open fork pane keeps its snapshot while the source’s agent writes and loses the source’s rows when it is deleted', async ({
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
  await expect(page.getByTestId('fork-divider-source')).toContainText(`Forked from ${SOURCE_TITLE}`);
  await expect(timeline.getByText('Ready.', { exact: true })).toBeVisible();
  await expect(timeline.getByText('Launching the reviewer.', { exact: true })).toBeVisible();
  // The source's launch is running; the fork does not show it.
  const launch = (await listItems(harness, sourceId)).find((row) => row.id === 'tu-outer');
  expect([launch?.status, launch?.isBackground]).toEqual(['running', true]);
  await expect(timeline.locator('[data-item-id="tu-outer"]')).toHaveCount(0);
  const forkRows = rowShape(await listItems(harness, fork.id));
  expect(forkRows.map(([id]) => id)).not.toContain('tu-outer');

  // The source's agent writes a child row and completes. The completion
  // row's upsert is the barrier: a resync those writes caused would be
  // emitted in the write's commit, before it.
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
  const resyncs = (id: string) =>
    harness.countEvents<ItemEvent>('provider:item_event', (event) => event.action === 'resync' && event.threadId === id);
  expect(resyncs(fork.id)).toBe(0);
  expect(rowShape(await listItems(harness, fork.id))).toEqual(forkRows);
  await expect(timeline.locator('[data-item-id="tu-outer"]')).toHaveCount(0);
  await expect(timeline.locator('[data-item-id="tu-grep"]')).toHaveCount(0);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await expect(timeline.getByText('Ready.', { exact: true })).toBeVisible();

  // Deleting the source, with the fork pane still open. The source's
  // `deleted` broadcast follows the delete's last write, so it is the
  // barrier for the resyncs the delete's transactions pushed.
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
  expect(resyncs(fork.id)).toBe(1);

  await expect(page.getByTestId('fork-divider')).toContainText(`Forked from ${SOURCE_TITLE}`);
  await expect(page.getByTestId('fork-divider-deleted')).toBeVisible();
  await expect(page.getByTestId('fork-divider-source')).toHaveCount(0);
  await expect(timeline.getByText('Ready.', { exact: true })).toHaveCount(0);
  await expect(timeline.getByText('Launching the reviewer.', { exact: true })).toHaveCount(0);
  await expect(page.getByTestId('chat-header-title')).toHaveText(fork.title);

  const detached = await listItems(harness, fork.id);
  const divider = detached.find((row) => row.toolName === 'fork_origin');
  expect(JSON.parse(divider?.meta ?? '{}')).toMatchObject({ sourceDeleted: true, sourceTitle: SOURCE_TITLE });
  expect(detached.map((row) => row.summary)).not.toContain('Ready.');
  // The connection is narrowed to the fork's pane: it was pushed the
  // fork's one resync and no other.
  const wire = await readWire(page);
  const pushedTo = wire.received.filter((event) => event.channel === 'provider:item_event' && event.action === 'resync');
  expect(pushedTo.map((event) => event.threadId)).toEqual([fork.id]);
});
