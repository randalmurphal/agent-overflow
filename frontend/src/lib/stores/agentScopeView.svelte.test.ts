import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createAgentScopeView, type AgentScopeView } from './agentScopeView.svelte';
import { createThreadPane } from './thread.svelte';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { applyItemStreamEvent, flushItemEventQueue } from './eventsItemStream';
import { applyTimelineMutation } from './timelineSurfaces';
import { setThreadScrollSnapshot, clearThreadScrollSnapshotsForTest } from '../utils/threadScrollSnapshots';
import { setBackendIdentityFromBootstrap, __resetBackendIdentityForTest } from '../transport/backendIdentity';
import { cursorFromItem } from './threadItems';
import type { Item } from '../types/models';
import { ActivityRunStub, type PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { ACTIVE_TIMELINE_WINDOW_MAX_ITEMS, ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS } from './threadPaneShared';

const threadId = 'scope-thread';
const root = makeItem({ id: 'agent', threadId, kind: 'tool_call', toolName: 'Agent', status: 'running' });
const row = (id: string, index: number, extra: Partial<Item> = {}) => makeItem({ id, threadId, parentId: root.id, itemIndex: index, ...extra });
const empty = { turnIndex: -1, itemIndex: -1, itemId: '' };
function page(items: Item[], older = false, newer = false): PagedItems {
  return { items, runs: [], scope: { root, lifecycle: root }, hasMore: older,
    hasMoreOlder: older, hasMoreNewer: newer, oldestTurnIndex: items[0]?.turnIndex ?? -1,
    newestTurnIndex: items.at(-1)?.turnIndex ?? -1,
    oldestCursor: items[0] ? { ...cursorFromItem(items[0]), itemId: items[0].id } : empty,
    newestCursor: items.at(-1) ? { ...cursorFromItem(items.at(-1)!), itemId: items.at(-1)!.id } : empty };
}
const views: AgentScopeView[] = [];
async function setup(items = [root, row('child', 1)]) {
  installPaneMocks(items);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: threadId }));
  return pane;
}
async function open(pane: Awaited<ReturnType<typeof setup>>, toolsOnly = false, scopeId = root.id) {
  const view = createAgentScopeView(pane, scopeId, { viewKey: toolsOnly ? 'tray' : 'agent', toolsOnly, openAgentPane: vi.fn() });
  views.push(view); view.start();
  await vi.waitFor(() => expect(view.pane.loading).toBe(false));
  expect(view.error).toBeNull();
  return view;
}
function push(item: Item) { applyItemStreamEvent({ action: 'upsert', threadId, item }); flushItemEventQueue(); }
beforeEach(() => {
  installTimelineScopeCapability();
  resetBindingMocks();
  clearThreadScrollSnapshotsForTest();
});
afterEach(() => { for (const view of views.splice(0)) view.dispose(); resetPanesForTest(); });

describe('live subagent children', () => {
  it('reach the agent pane and an expanded card, never the thread window', async () => {
    const pane = await setup([root]);
    const agentView = await open(pane);
    const card = createAgentScopeView(pane, root.id, { viewKey: 'card:agent', toolsOnly: true, openAgentPane: vi.fn() });
    views.push(card); card.start();
    await vi.waitFor(() => expect(card.pane.loading).toBe(false));
    const hostItems = pane.items;
    const hostRevision = pane.timelineRevision;

    push(row('live-tool', 1, { kind: 'tool_call', toolName: 'Bash', status: 'running', summary: 'go test' }));
    push(row('live-text', 2, { kind: 'assistant_text', status: 'streaming', summary: 'thinking aloud' }));
    push(row('live-tool', 1, { kind: 'tool_call', toolName: 'Bash', status: 'completed', summary: 'go test', updatedAt: 5 }));

    expect(agentView.items.map(item => item.id)).toEqual(['live-tool', 'live-text']);
    expect(agentView.pane.getItemById('live-tool')?.status).toBe('completed');
    expect(card.items.map(item => item.id)).toEqual(['live-tool']);
    expect(pane.items).toBe(hostItems);
    expect(pane.timelineRevision).toBe(hostRevision);
    expect(pane.subagentLiveAggregate(root.id)).toMatchObject({ count: 2, terminalPreview: 'go test' });
  });
});

describe('independent agent timeline', () => {
  it('loads direct rows and completion siblings without borrowing host rows', async () => {
    const items = [root, row('child', 1), row('nested', 2, { kind: 'tool_call', toolName: 'Agent' }),
      row('grandchild', 3, { parentId: 'nested' }), row('nested-done', 4, { kind: 'tool_completion', completionOf: 'nested' }),
      row('scope-done', 5, { parentId: undefined, kind: 'tool_completion', completionOf: root.id })];
    const pane = await setup(items);
    const view = await open(pane);
    expect(view.items.map(item => item.id)).toEqual(['child', 'nested', 'nested-done']);
    expect(view.items.every(item => !item.parentId)).toBe(true);
    expect(view.pane.getItemById('child')?.parentId).toBe(root.id);
    pane.removeItemById('child', threadId);
    pane.removeItemById(root.id, threadId);
    expect(view.items.map(item => item.id)).toContain('child');
    expect(view.root?.id).toBe(root.id);
    expect(view.gone).toBe(false);
  });

  it('loads an agent whose launch is outside the main window and keeps empty scopes valid', async () => {
    const pane = await setup([root]);
    pane.removeItemById(root.id, threadId);
    const view = await open(pane);
    expect(view.root?.id).toBe(root.id);
    expect(view.items).toEqual([]);
    expect(view.gone).toBe(false);
  });

  it('keeps tools-only tray state independent of full transcript, scroll and leases', async () => {
    const pane = await setup([root, row('prose', 1), row('think', 2, { kind: 'thinking' }),
      row('tool', 3, { kind: 'tool_call', toolName: 'Bash' })]);
    const full = await open(pane), tray = await open(pane, true);
    expect(full.items).toHaveLength(3);
    expect(tray.items.map(item => item.id)).toEqual(['tool']);
    expect(full.pane.activityRuns).not.toBe(tray.pane.activityRuns);
    expect(full.pane.activityRuns).not.toBe(pane.activityRuns);
    full.pane.requestScrollToItem('think');
    expect(tray.pane.scrollToItemRequest.itemId).toBe('');
    expect(pane.scrollToItemRequest.itemId).toBe('');
    full.pane.setUserMessageExpanded('prose', true);
    expect(pane.isUserMessageExpanded('prose')).toBe(false);
    full.pane.pruneRowUiState({ itemIds: new Set(), payloads: new Set(), groupKeys: new Set() });
    expect(tray.items).toHaveLength(1);
  });

  it('pages older and newer history with the scope and preserves the other surface', async () => {
    const pane = await setup([root]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', epoch: 1, rev: 1, generation: 'test', page: page([row('middle', 20)], true, true) }));
    const view = await open(pane);
    const older = setBindingMock('ListItemsBeforeCursor', async () => page([row('early', 10)], false, true));
    const newer = setBindingMock('ListItemsAfterCursor', async () => page([row('late', 30)], true, false));
    await view.pane.loadOlder(); await view.pane.loadNewer();
    expect(view.items.map(item => item.id)).toEqual(['early', 'middle', 'late']);
    expect(older.mock.calls[0][3]).toMatchObject({ selection: { scopeRootId: root.id } });
    expect(newer.mock.calls[0][3]).toMatchObject({ selection: { scopeRootId: root.id } });
    expect(pane.items.map(item => item.id)).toEqual([root.id]);
  });

  it('receives ordered upserts, delta, metadata, patch, moves and removals', async () => {
    const pane = await setup([root]); const view = await open(pane);
    push(row('stream', 1, { kind: 'tool_call', status: 'streaming', summary: 'a' }));
    applyItemStreamEvent({ action: 'delta', threadId, itemId: 'stream', kind: 'tool_call', delta: 'b', updatedAt: 2 });
    applyItemStreamEvent({ action: 'meta', threadId, itemId: 'stream', kind: 'tool_call', meta: '{"test":1}', updatedAt: 2 });
    applyItemStreamEvent({ action: 'patch', threadId, itemId: 'stream', kind: 'tool_call', patch: { status: 'completed', rev: 2 } });
    flushItemEventQueue();
    expect(view.pane.getItemById('stream')).toMatchObject({ summary: 'ab', meta: '{"test":1}', status: 'completed' });
    push(row('stream', 1, { parentId: 'another', rev: 3 }));
    expect(view.pane.getItemById('stream')).toBeUndefined();
    push(row('second', 2));
    applyItemStreamEvent({ action: 'remove', threadId, itemId: 'second' }); flushItemEventQueue();
    expect(view.pane.getItemById('second')).toBeUndefined();
  });

  it('prunes its window by the rows of its own scope', async () => {
    const pane = await setup([root]);
    const loaded = Array.from({ length: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS }, (_, index) => row(`r${index}`, index + 1));
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page(loaded) }));
    const view = await open(pane);
    expect(view.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_MAX_ITEMS);
    push(row('tail', ACTIVE_TIMELINE_WINDOW_MAX_ITEMS + 1));
    expect(view.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
    expect(view.items.at(-1)?.id).toBe('tail');
    expect(view.pane.hasMoreHistory).toBe(true);
  });

  it('does not admit live history below its loaded floor and leaves it pageable', async () => {
    const pane = await setup([root]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('current', 20)]) }));
    const view = await open(pane);
    push(row('backfill', 10));
    expect(view.items.map(item => item.id)).toEqual(['current']);
    expect(view.pane.hasMoreHistory).toBe(true);
  });

  it('preserves live changes when a snapshot is in flight', async () => {
    const pane = await setup([root, row('child', 1)]); const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    push(row('child', 1, { summary: 'newer live text', rev: 99 }));
    push(row('appended', 2));
    resolve({ status: 'stale', page: page([row('child', 1, { summary: 'old snapshot' })]) });
    await refresh;
    expect(view.pane.getItemById('child')?.summary).toBe('newer live text');
    expect(view.pane.getItemById('appended')).toBeDefined();
    expect(view.pane.newestLoadedCursor?.itemId).toBe('appended');
  });

  it('keeps a live child while an opening carrier resolves to its transcript root', async () => {
    const carrier = { ...root, id: 'resume', meta: `{"transcript_root_id":"${root.id}"}` };
    const pane = await setup([root, carrier]);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const view = createAgentScopeView(pane, carrier.id, { viewKey: 'resume', openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    push(row('live-child', 1));
    resolve({ status: 'stale', page: { ...page([]), scope: { root, lifecycle: carrier } } });
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    expect(view.items.map(item => item.id)).toEqual(['live-child']);
  });

  it('rechecks only the run touched during a scoped snapshot', async () => {
    const pane = await setup([root]);
    const first = row('first-tool', 1, { kind: 'tool_call', toolName: 'Bash' });
    const separator = row('prose', 2, { kind: 'assistant_text' });
    const second = row('second-tool', 3, { kind: 'tool_call', toolName: 'Bash' });
    const runStub = (item: Item) => new ActivityRunStub({
      firstItemId: item.id, lastItemId: item.id,
      firstTurnIndex: item.turnIndex, firstItemIndex: item.itemIndex,
      lastTurnIndex: item.turnIndex, lastItemIndex: item.itemIndex,
      memberCount: 1, loadedFirstItemId: item.id, loadedLastItemId: item.id,
      unshippedBefore: 0, unshippedAfter: 0, unshippedDigest: '0000000000000000',
    });
    const stubs = [runStub(first), runStub(second)];
    const snapshot = { ...page([first, separator, second]), runs: stubs };
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: snapshot }));
    const view = await open(pane);
    const fetch = setBindingMock('ListActivityRunMembers', async (_threadId: string, request: { runFirstItemId: string }) => ({
      items: [], stub: stubs.find(stub => stub.firstItemId === request.runFirstItemId),
    }));
    let finish: ((value: unknown) => void) | undefined;
    setBindingMock('SyncThreadWindow', () => new Promise(resolve => { finish = resolve; }));
    const unrelatedRead = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(finish).toBeTypeOf('function'));
    push(row('other-agent-tool', 4, { parentId: 'other-agent', kind: 'tool_call', toolName: 'Bash' }));
    push({ ...separator, summary: 'updated prose', rev: 8 });
    finish!({ status: 'stale', page: snapshot });
    await unrelatedRead;
    expect(view.pane.getItemById(separator.id)?.summary).toBe('updated prose');
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(fetch).not.toHaveBeenCalled();

    finish = undefined;
    const touchedRead = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(finish).toBeTypeOf('function'));
    push({ ...first, summary: 'updated while reading', rev: 9 });
    finish!({ status: 'stale', page: snapshot });
    await touchedRead;
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    expect(fetch.mock.calls[0][1]).toMatchObject({ runFirstItemId: first.id, limit: 0 });
  });

  it('does not resurrect a removed row from an in-flight snapshot', async () => {
    const pane = await setup([root, row('child', 1)]); const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend(); await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    applyTimelineMutation(threadId, { kind: 'remove', itemId: 'child' });
    resolve({ status: 'stale', page: page([row('child', 1)]) }); await refresh;
    expect(view.items).toEqual([]);
  });

  it('updates lifecycle context on a fresh response without replacing transcript rows', async () => {
    const pane = await setup([root, row('child', 1)]); const view = await open(pane);
    const previous = view.items;
    const carrier = { ...root, id: 'resume', status: 'completed', createdAt: 20 };
    const completion = { ...root, id: 'done', completionOf: 'resume', kind: 'tool_completion', status: 'completed', updatedAt: 30 };
    setBindingMock('SyncThreadWindow', async () => ({ status: 'fresh', scope: { root, lifecycle: carrier, completion } }));
    await view.pane.refreshFromBackend();
    expect(view.items).toBe(previous);
    expect(view.lifecycle?.id).toBe('resume');
    expect(view.lifecycleCompletion?.id).toBe('done');
    expect(view.pane.timelineTurns.settled).toMatchObject({ startedAt: 20, completedAt: 30 });
  });

  it('keeps settled lifecycle context when an older running upsert arrives, but admits a newer execution', async () => {
    const settled: Item = { ...root, status: 'completed', updatedAt: 30 };
    const pane = await setup([settled]);
    const view = await open(pane);
    push({ ...root, status: 'running', updatedAt: 20 });
    expect(pane.getItemById(root.id)?.status).toBe('completed');
    expect(view.lifecycle?.status).toBe('completed');
    expect(view.pane.timelineTurns.activeKey).toBeNull();
    push({ ...root, status: 'running', updatedAt: 40 });
    expect(view.lifecycle?.status).toBe('running');
    expect(view.pane.timelineTurns.activeKey).toBe(0);
  });

  it('closes only on authoritative deletion and drops late replies after release', async () => {
    const pane = await setup([root]); const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend(); await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    view.dispose(); resolve({ status: 'stale', page: page([row('late', 1)]) });
    await refresh.catch(() => {});
    expect(view.items).toEqual([]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'gone' }));
    const reopened = await open(pane);
    expect(reopened.gone).toBe(true);
  });

  it('exposes a failed load for retry rather than reporting an empty transcript', async () => {
    const pane = await setup([root]);
    setBindingMock('SyncThreadWindow', async () => { throw new Error('network unavailable'); });
    const view = createAgentScopeView(pane, root.id, { viewKey: 'failure', openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(view.error).toContain('network unavailable'));
    expect(view.gone).toBe(false);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('recovered', 1)]) }));
    await view.pane.retryHistoryLoad();
    expect(view.error).toBeNull(); expect(view.items[0]?.id).toBe('recovered');
  });
  it('does not restore a deleted root from an in-flight response', async () => {
    const pane = await setup(); const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    applyTimelineMutation(threadId, { kind: 'remove', itemId: root.id });
    resolve({ status: 'stale', page: page([row('stale', 2)]) });
    await refresh;
    expect(view.gone).toBe(true);
    expect(view.pane.getItemById('stale')).toBeUndefined();
  });

  it('separates two panes showing the same transcript and releases only its own window', async () => {
    const first = await setup();
    const second = createThreadPane({ paneId: 'second' });
    await second.switchThread(makeThread({ id: threadId }));
    const a = await open(first), b = await open(second);
    a.pane.setUserMessageExpanded('child', true);
    expect(b.pane.isUserMessageExpanded('child')).toBe(false);
    a.dispose(); a.dispose();
    push(row('later', 2));
    expect(a.items).toEqual([]);
    expect(b.items.map(item => item.id)).toContain('later');
    const reopened = await open(first);
    expect(reopened.pane.isUserMessageExpanded('child')).toBe(false);
    second.clear();
  });

  it('revalidates a newly loaded row if its delta arrived during the read', async () => {
    const pane = await setup([root]); const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    applyTimelineMutation(threadId, { kind: 'delta', event: { threadId, itemId: 'new-row', kind: 'tool_call', delta: 'latest', updatedAt: 3 } });
    resolve({ status: 'stale', page: page([row('new-row', 1, { summary: 'older' })]) });
    await refresh;
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('new-row', 1, { summary: 'olderlatest' })]) }));
    await vi.waitFor(() => expect(view.pane.getItemById('new-row')?.summary).toBe('olderlatest'));
  });

  it('invalidates a root removed by a conversation cut during a read', async () => {
    const pane = await setup(); const view = await open(pane);
    applyTimelineMutation(threadId, { kind: 'revert', event: { threadId, userItemId: 'prompt', turnIndex: 0, keptAnchorTurnItemIds: [] } });
    expect(view.items).toEqual([]);
    expect(view.gone).toBe(true);
  });

  it('uses the canonical root returned for a resume carrier in later reads and live updates', async () => {
    const carrier = { ...root, id: 'resume', itemIndex: 10, meta: JSON.stringify({ transcript_root_id: root.id }) };
    const pane = await setup([root, carrier, row('child', 1)]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: { ...page([row('child', 1)]), scope: { root, lifecycle: carrier } } }));
    const view = await open(pane, false, carrier.id);
    expect(view.root?.id).toBe(root.id);
    push(row('canonical-child', 11, { kind: 'tool_call', toolName: 'Read' }));
    expect(view.pane.getItemById('canonical-child')?.parentId).toBe(root.id);
    const sync = vi.fn(async () => ({ status: 'fresh', scope: { root, lifecycle: carrier } }));
    setBindingMock('SyncThreadWindow', sync);
    await view.pane.refreshFromBackend();
    expect(sync.mock.calls[0]).toEqual([threadId, expect.objectContaining({ selection: { scopeRootId: root.id, tools: false } })]);
  });

  it.each([undefined, ''])('reconciles live child updates before a resume carrier resolves, with completionOf=%s', async (completionOf) => {
    const carrier = { ...root, id: 'resume', itemIndex: 10, meta: JSON.stringify({ transcript_root_id: root.id }) };
    const pane = await setup([root, carrier]);
    let resolve!: (value: unknown) => void;
    const initial = setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const view = createAgentScopeView(pane, carrier.id, { viewKey: 'agent', openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(initial).toHaveBeenCalledOnce());
    push(row('child', 11, { summary: 'latest child content', completionOf }));
    const recovery = setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: {
      ...page([row('child', 11, { summary: 'latest child content' })]), scope: { root, lifecycle: carrier },
    } }));
    resolve({ status: 'stale', page: { ...page([row('child', 11, { summary: 'older content' })]), scope: { root, lifecycle: carrier } } });
    await vi.waitFor(() => expect(view.items[0]?.summary).toBe('latest child content'));
    expect(view.pane.loading).toBe(false);
    await vi.waitFor(() => expect(recovery).toHaveBeenCalledOnce());
    expect(recovery.mock.calls[0][1]).toMatchObject({ selection: { scopeRootId: root.id } });
  });

  it('retains loaded rows on both sides of an escaped reading anchor during recovery', async () => {
    const pane = await setup([root]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('early', 1), row('middle', 5), row('late', 10)], false, true) }));
    const view = await open(pane);
    setThreadScrollSnapshot(`${pane.paneId}:${threadId}~agent:${root.id}`, { kind: 'anchor', itemId: 'middle', offsetTop: 20 });
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('middle', 5)], true, true) }));
    setBindingMock('ListItemsBeforeCursor', async () => page([row('early', 1)], false, true));
    setBindingMock('ListItemsAfterCursor', async () => page([row('late', 10)], true, true));
    await view.pane.refreshFromBackend();
    expect(view.items.map(item => item.id)).toEqual(['early', 'middle', 'late']);
    expect(view.pane.hasMoreNewer).toBe(true);
  });

  it('does not overwrite user paging with an older recovery snapshot', async () => {
    const pane = await setup([root]);
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page: page([row('middle', 5)], true) }));
    const view = await open(pane);
    let resolve!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(done => { resolve = done; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
    setBindingMock('ListItemsBeforeCursor', async () => page([row('early', 1)], false, true));
    await view.pane.loadOlder();
    resolve({ status: 'stale', page: page([row('middle', 5)], true) });
    await refresh;
    expect(view.items.map(item => item.id)).toEqual(['early', 'middle']);
    expect(view.pane.hasMoreHistory).toBe(false);
  });

  it('coalesces refresh requests without cancelling a slow remote snapshot', async () => {
    const pane = await setup(); const view = await open(pane);
    const resolves: Array<(value: unknown) => void> = [];
    const sync = vi.fn(() => new Promise(done => resolves.push(done)));
    setBindingMock('SyncThreadWindow', sync);
    const first = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(sync).toHaveBeenCalledTimes(1));
    const second = view.pane.refreshFromBackend();
    const third = view.pane.refreshFromBackend();
    expect(sync).toHaveBeenCalledTimes(1);
    resolves[0]({ status: 'stale', page: page([row('first-read', 1)]) });
    await vi.waitFor(() => expect(sync).toHaveBeenCalledTimes(2));
    expect(view.items[0]?.id).toBe('first-read');
    resolves[1]({ status: 'stale', page: page([row('next-read', 2)]) });
    await Promise.all([first, second, third]);
    expect(view.items[0]?.id).toBe('next-read');
  });

  it('keeps late nested completions in the scope without changing completed launch history', async () => {
    const completed: Item = { ...root, status: 'completed', summary: 'original launch' };
    const nested = row('nested', 1, { kind: 'tool_call', toolName: 'Agent', status: 'completed' });
    const pane = await setup([completed, nested]); const view = await open(pane);
    push(row('nested-done', 2, { kind: 'tool_completion', completionOf: nested.id, summary: 'nested result' }));
    expect(view.items.map(item => item.id)).toEqual(['nested', 'nested-done']);
    expect(pane.getItemById(root.id)).toMatchObject({ summary: 'original launch', status: 'completed' });
  });

  it('owns streaming reveal and releases it without touching another scope', async () => {
    const nested = row('nested', 1, { kind: 'tool_call', toolName: 'Agent', status: 'running' });
    const pane = await setup([root, nested]);
    const outer = await open(pane), inner = await open(pane, false, nested.id);
    push(row('stream', 2, { kind: 'assistant_text', status: 'streaming', summary: 'start' }));
    applyTimelineMutation(threadId, { kind: 'delta', event: { threadId, itemId: 'stream', kind: 'assistant_text', delta: ' continued', updatedAt: 3 } });
    expect(outer.pane.__itemSmootherCountForTest()).toBe(1);
    expect(inner.pane.__itemSmootherCountForTest()).toBe(0);
    outer.pane.__flushItemSmoothersForTest();
    expect(outer.pane.getItemById('stream')?.summary).toBe('start continued');
    outer.dispose();
    expect(outer.pane.__itemSmootherCountForTest()).toBe(0);
    push(row('grandchild', 3, { parentId: nested.id }));
    expect(inner.items.map(item => item.id)).toEqual(['grandchild']);
    expect(outer.items).toEqual([]);
  });

  it('revalidates against a new replica generation reported by a scoped read', async () => {
    const pane = await setup();
    setBackendIdentityFromBootstrap('scope-computer', 'g1', 'Scope computer');
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', generation: 'g1', page: page([row('old-generation', 1)]) }));
    const view = await open(pane);
    const generation = view.pane.switchGeneration;
    const sync = vi.fn(async () => ({ status: 'stale', generation: 'g2', page: page([row('new-generation', 2)]) }));
    setBindingMock('SyncThreadWindow', sync);
    try {
      await view.pane.refreshFromBackend();
      await vi.waitFor(() => expect(view.items.map(item => item.id)).toEqual(['new-generation']));
      expect(view.pane.switchGeneration).toBeGreaterThan(generation);
      expect(sync.mock.calls.length).toBeGreaterThanOrEqual(2);
      expect(view.pane.loading).toBe(false);
    } finally { view.dispose(); __resetBackendIdentityForTest(); }
  });

  it('keeps a newer paging request busy when a superseded request finishes', async () => {
    const pane = await setup(); const view = await open(pane);
    const resolves: Array<(value: PagedItems) => void> = [];
    setBindingMock('ListThreadSliceAround', () => new Promise<PagedItems>(resolve => resolves.push(resolve)));
    const first = view.pane.loadRecentTail();
    const second = view.pane.loadRecentTail();
    resolves[0](page([row('stale-tail', 3)]));
    await first;
    expect(view.pane.loadingNewer).toBe(true);
    resolves[1](page([row('current-tail', 4)]));
    await second;
    expect(view.pane.loadingNewer).toBe(false);
    expect(view.items.map(item => item.id)).toEqual(['current-tail']);
  });

  it('does not undo a jump when recovery started during the target lookup', async () => {
    const pane = await setup(); const view = await open(pane);
    let target!: (value: Item) => void;
    setBindingMock('GetThreadItem', () => new Promise<Item>(resolve => { target = resolve; }));
    const jump = view.pane.loadUntilItem('target');
    let snapshot!: (value: unknown) => void;
    setBindingMock('SyncThreadWindow', () => new Promise(resolve => { snapshot = resolve; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(snapshot).toBeTypeOf('function'));
    setBindingMock('ListThreadSliceAround', async () => page([row('target', 50)], true, true));
    target(row('target', 50));
    expect(await jump).toBe('loaded');
    snapshot({ status: 'stale', page: page([row('child', 1)]) });
    await refresh;
    expect(view.items.map(item => item.id)).toEqual(['target']);
    expect(view.pane.hasMoreNewer).toBe(true);
  });

  it('keeps a newer jump busy when the superseded jump finishes', async () => {
    const pane = await setup(); const view = await open(pane);
    setBindingMock('GetThreadItem', async (_threadId: string, itemId: string) => row(itemId, 50));
    const resolves: Array<(value: PagedItems) => void> = [];
    setBindingMock('ListThreadSliceAround', () => new Promise<PagedItems>(resolve => resolves.push(resolve)));
    const first = view.pane.loadUntilItem('first-target');
    await vi.waitFor(() => expect(resolves).toHaveLength(1));
    const second = view.pane.loadUntilItem('second-target');
    await vi.waitFor(() => expect(resolves).toHaveLength(2));
    resolves[0](page([row('first-target', 50)]));
    expect(await first).toBe('superseded');
    expect(view.pane.loadingOlder).toBe(true);
    resolves[1](page([row('second-target', 50)]));
    expect(await second).toBe('loaded');
    expect(view.pane.loadingOlder).toBe(false);
    expect(view.items.map(item => item.id)).toEqual(['second-target']);
  });

});

describe('execution digest timeline', () => {
  it('keeps completion bounds through live context patches and rejects later children', async () => {
    const pane = await setup([root]);
    const answer = row('answer', 1);
    const done = makeItem({ id: 'done', threadId, kind: 'tool_completion', completionOf: root.id, itemIndex: 4, status: 'completed' });
    const digest = { after: { turnIndex: 0, itemIndex: 0 }, before: { turnIndex: 0, itemIndex: 4 }, promptId: '', answerId: answer.id };
    const sync = setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', generation: 'test', page: {
      ...page([answer]), scope: { root, lifecycle: root, completion: done, digest },
    } }));
    const view = createAgentScopeView(pane, root.id, { viewKey: 'card:done', digestItemId: done.id, openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    expect(sync.mock.calls[0][1]).toMatchObject({ selection: { scopeRootId: root.id, digestItemId: done.id } });
    applyTimelineMutation(threadId, { kind: 'patch', event: { threadId, itemId: root.id, kind: 'tool_call', patch: { updatedAt: 10, rev: 10 } } });
    push(row('inside', 2, { kind: 'tool_call', toolName: 'Bash' }));
    push(row('outside', 8, { kind: 'tool_call', toolName: 'Bash' }));
    expect(view.items.map(it => it.id)).toEqual(['answer', 'inside']);
    expect(view.lifecycleCompletion?.id).toBe(done.id);
    view.dispose();
    push(row('after-dispose', 1, { kind: 'tool_call', toolName: 'Bash' }));
    expect(view.items).toEqual([]);
  });

  it('refreshes a new final report but does not reread history for each text delta', async () => {
    const pane = await setup([root]);
    let answer = row('answer', 3, { status: 'streaming' });
    const sync = setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', generation: 'test', page: {
      ...page([answer]), scope: { root, lifecycle: root, digest: { promptId: '', answerId: answer.id } },
    } }));
    const view = createAgentScopeView(pane, root.id, { viewKey: 'card:root', digestItemId: root.id, openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    for (let i = 0; i < 10; i++) applyTimelineMutation(threadId, { kind: 'delta', event: { threadId, itemId: answer.id, kind: 'assistant_text', delta: ' more', updatedAt: i + 2 } });
    expect(sync).toHaveBeenCalledOnce();
    answer = row('next-answer', 5, { status: 'streaming' });
    push(answer);
    await vi.waitFor(() => expect(view.items.map(it => it.id)).toEqual(['next-answer']));
    expect(sync).toHaveBeenCalledTimes(2);
    expect(view.pane.getItemById('next-answer')?.parentId).toBe(root.id);
    // The child lives in the scoped surface only, never the thread window.
    expect(pane.getItemById('next-answer')).toBeUndefined();
  });

  it.each(['upsert', 'delta', 'move'] as const)('reconciles a newly selected answer overlapping a live %s', async (change) => {
    const pane = await setup([root]);
    const previous = row('previous-answer', 1);
    const latest = row('latest-answer', 3);
    const digestPage = (answer: Item): PagedItems => ({ ...page([answer]),
      scope: { root, lifecycle: root, digest: { promptId: '', answerId: answer.id } } });
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', generation: 'test', page: digestPage(previous) }));
    const view = createAgentScopeView(pane, root.id, { viewKey: 'card:root', digestItemId: root.id, openAgentPane: vi.fn() });
    views.push(view); view.start();
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    let complete: ((value: unknown) => void) | undefined;
    setBindingMock('SyncThreadWindow', () => new Promise(resolve => { complete = resolve; }));
    const refresh = view.pane.refreshFromBackend();
    await vi.waitFor(() => expect(complete).toBeDefined());
    push(latest);
    if (change === 'delta') applyTimelineMutation(threadId, { kind: 'delta', event: { threadId, itemId: latest.id, kind: 'assistant_text', delta: ' newest', updatedAt: latest.updatedAt + 1 } });
    if (change === 'move') push({ ...latest, parentId: 'other-scope' });
    complete!({ status: 'stale', generation: 'test', page: digestPage(latest) });
    await refresh;
    expect(view.items.map(it => it.id)).toEqual(change === 'move' ? [] : [latest.id]);
    if (change === 'delta') expect(view.items[0].summary).toBe(latest.summary + ' newest');
  });
});
