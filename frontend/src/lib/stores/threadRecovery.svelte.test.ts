import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { createThreadPane } from './thread.svelte';
import { getActiveTurn, isThreadWorking, projectTurnStarted } from './threadStatuses.svelte';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setCarriedSessionScopes } from '../transport/scopes';
import { noteThread } from '../transport/entityIndex';
import { itemEventQueued, itemEventsSettled } from './itemEventSettlement';
import { getFlushedForThread, markItemsFlushed } from './sendQueue.svelte';

let pane: ReturnType<typeof createThreadPane> | undefined;
beforeEach(() => { resetStagedBackends(); installThreadPaneTestEnv(); setBindingMock('AutoResumeThread', async () => undefined); });
afterEach(() => { pane?.clear(); pane = undefined; });

it('reconnect clears a delivered message from an authoritative empty live snapshot', async () => {
  pane = await buildPane(makeThread({ id: 'recovery-regression' }));
  markItemsFlushed(pane.threadId!, [{ queueItemId: 'q', userItemId: 'u', message: 'already delivered' }]);
  setBindingMock('GetThreadLiveState', async () => ({ threadId: pane!.threadId, activeTurn: null, queueItems: [], flushedItems: [] }));
  await pane.refreshFromBackend(true);
  expect.soft(getFlushedForThread(pane.threadId)).toEqual([]);
  expect(isThreadWorking(pane.threadId)).toBe(false);
});

it('waits for old replay and queued item mutations before reading current state', async () => {
  pane = await buildPane(makeThread({ id: 'recovery-replay' }));
  noteThread(pane.threadId!, 'recovery-remote');
  const backend = stageBackend({ id: 'recovery-remote' });
  setCarriedSessionScopes('recovery-remote', ['threads:read', 'threads:operate']);
  const read = setBindingMock('GetThreadLiveState', async () => ({ threadId: pane!.threadId, activeTurn: null, queueItems: [], flushedItems: [] }));
  backend.replay('start');
  const refreshing = pane.refreshFromBackend(true);
  projectTurnStarted(pane.threadId!, 'old-turn', 75, 100);
  itemEventQueued();
  backend.replay('complete');
  await Promise.resolve();
  expect(read).not.toHaveBeenCalled();
  markItemsFlushed(pane.threadId!, [{ queueItemId: 'q', userItemId: 'u', message: 'already delivered' }]);
  itemEventsSettled(1);
  await refreshing;
  expect(read).toHaveBeenCalledTimes(1);
  expect(getActiveTurn(pane.threadId)).toBeNull();
  expect(getFlushedForThread(pane.threadId)).toEqual([]);
  expect(isThreadWorking(pane.threadId)).toBe(false);
});

it('a replayed streaming row cannot replace its completed snapshot during initial loading', async () => {
  pane = createThreadPane();
  const thread = makeThread({ id: 'recovery-regression' });
  let answer!: (value: unknown) => void;
  setBindingMock('SyncThreadWindow', () => new Promise(resolve => { answer = resolve; }));
  const opening = pane.switchThread(thread);
  await vi.waitFor(() => expect(answer).toBeTypeOf('function'));
  const oldRow = makeItem({ id: 'thinking', threadId: thread.id, kind: 'thinking', status: 'streaming', turnIndex: 75, itemIndex: 22, summary: 'old partial', updatedAt: 100 });
  pane.upsertItems([oldRow]);
  pane.applyItemDelta({ threadId: thread.id, itemId: oldRow.id, kind: 'thinking', delta: ' tail', updatedAt: 110 });
  const finalRow = { ...oldRow, status: 'completed' as const, summary: 'finished reasoning', updatedAt: 200 };
  const finalAnswer = makeItem({ id: 'answer', threadId: thread.id, turnIndex: 75, itemIndex: 63, kind: 'assistant_text', status: 'completed', summary: 'final answer' });
  answer({ status: 'stale', epoch: 33, rev: 6425, generation: '', page: { items: [finalRow, finalAnswer], oldestTurnIndex: 75, newestTurnIndex: 75, hasMore: false, hasMoreOlder: false, hasMoreNewer: false } });
  await opening;
  expect(pane.items.some(item => item.id === 'answer')).toBe(true);
  expect.soft(pane.items.find(item => item.id === 'thinking')?.status).toBe('completed');
  expect(pane.revealBoundary).toBeNull();
});

it('gap recovery replaces a pre-existing streaming row with its completed snapshot', async () => {
  pane = await buildPane(makeThread({ id: 'recovery-regression-control' }));
  const threadId = pane.threadId!;
  const oldRow = makeItem({ id: 'thinking', threadId, kind: 'thinking', status: 'streaming', turnIndex: 75, itemIndex: 22, summary: 'old partial', updatedAt: 100 });
  pane.upsertItems([oldRow]);
  pane.applyItemDelta({ threadId, itemId: oldRow.id, kind: 'thinking', delta: ' tail', updatedAt: 110 });
  markItemsFlushed(threadId, [{ queueItemId: 'q', userItemId: 'u', message: 'already delivered' }]);
  projectTurnStarted(threadId, 'old-turn', 75, 100);
  const finalRow = { ...oldRow, status: 'completed' as const, summary: 'finished reasoning', updatedAt: 200 };
  const finalAnswer = makeItem({ id: 'answer', threadId, turnIndex: 75, itemIndex: 63, kind: 'assistant_text', status: 'completed', summary: 'final answer' });
  setBindingMock('ListThreadSliceAround', async () => ({ items: [finalRow, finalAnswer], oldestTurnIndex: 75, newestTurnIndex: 75, hasMore: false, hasMoreOlder: false, hasMoreNewer: false }));
  setBindingMock('GetThreadLiveState', async () => ({ threadId, activeTurn: null, queueItems: [], flushedItems: [] }));
  await pane.refreshFromBackend(true);
  expect(getFlushedForThread(threadId)).toEqual([]);
  expect(isThreadWorking(threadId)).toBe(false);
  expect.soft(pane.items.find(item => item.id === 'thinking')?.status).toBe('completed');
  expect(pane.revealBoundary).toBeNull();
});

it('control: an uncontended cold load correctly installs completed history and idle state', async () => {
  pane = createThreadPane();
  const thread = makeThread({ id: 'recovery-regression-clean' });
  const row = makeItem({ id: 'thinking', threadId: thread.id, kind: 'thinking', status: 'completed', turnIndex: 75, itemIndex: 22, summary: 'finished reasoning', updatedAt: 200 });
  const answer = makeItem({ id: 'answer', threadId: thread.id, kind: 'assistant_text', status: 'completed', turnIndex: 75, itemIndex: 63, summary: 'final answer' });
  setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', epoch: 33, rev: 6425, generation: '', page: { items: [row, answer], oldestTurnIndex: 75, newestTurnIndex: 75, hasMore: false, hasMoreOlder: false, hasMoreNewer: false } }));
  await pane.switchThread(thread);
  expect(pane.items.find(item => item.id === 'thinking')?.status).toBe('completed');
  expect(pane.items.some(item => item.id === 'answer')).toBe(true);
  expect(pane.revealBoundary).toBeNull();
  expect(isThreadWorking(thread.id)).toBe(false);
});

it.each(['completed', 'errored', 'declined', 'killed'] as const)('does not reopen a %s row when an old start or patch arrives after recovery', async (status) => {
  pane = await buildPane(makeThread({ id: 'recovery-terminal' }));
  const row = makeItem({ id: 'text', threadId: pane.threadId!, kind: 'assistant_text', status, summary: 'settled text' });
  pane.upsertItems([row]);
  pane.applyProviderItemUpserts([{ ...row, status: 'streaming', summary: 'old partial' }]);
  pane.applyItemPatch({ threadId: pane.threadId!, itemId: row.id, kind: row.kind, patch: { status: 'streaming' } });
  pane.applyItemDelta({ threadId: pane.threadId!, itemId: row.id, kind: row.kind, delta: 'old delta', updatedAt: 100 });
  expect(pane.items[0]).toMatchObject({ status, summary: 'settled text' });
  expect(pane.revealBoundary).toBeNull();
});

it('serializes reconnect recovery behind an initial load', async () => {
  pane = createThreadPane();
  let answer!: (value: unknown) => void;
  setBindingMock('SyncThreadWindow', () => new Promise(resolve => { answer = resolve; }));
  const opening = pane.switchThread(makeThread({ id: 'recovery-initial' }));
  await vi.waitFor(() => expect(answer).toBeTypeOf('function'));
  const history = setBindingMock('ListThreadSliceAround', async () => ({ items: [], hasMoreOlder: false, hasMoreNewer: false }));
  const refreshing = pane.refreshFromBackend(true);
  expect(history).not.toHaveBeenCalled();
  answer({ status: 'stale', epoch: 1, rev: 1, generation: '', page: { items: [], hasMoreOlder: false, hasMoreNewer: false } });
  await Promise.all([opening, refreshing]);
  expect(history).toHaveBeenCalledTimes(1);
});
