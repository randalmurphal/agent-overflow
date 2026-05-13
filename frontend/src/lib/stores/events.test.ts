import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  QUEUE_STATE_REASON_FLUSH_FAILED,
  QUEUE_STATE_REASON_FLUSH_STARTED,
  setupEventListeners,
} from './events';
import { createThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';
import {
  getActiveTurn,
  getThreadStatus,
  hasPendingSend,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { resetForTest as resetSendQueue } from './sendQueue.svelte';
import { getThreads, refreshThreads } from './threads.svelte';
import { getProjects, refreshProjects, resetProjectsForTest } from './projects.svelte';
import {
  getProviderRateLimit,
  resetForTest as resetRateLimitsInfo,
} from './rateLimitsInfo.svelte';
import { transportGapChannel } from '../transport/wsClient';
import { emitWailsEvent, resetWailsMocks, wailsListenerCount } from '../../test/mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import type { ProviderStatusEvent } from '../types/events';
import type { Item, ProjectWithCounts } from '../types/models';

function providerStatusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found',
    actionable: true,
    ...overrides,
  };
}

function projectWithCounts(id: string, lastActive = 0): ProjectWithCounts {
  return {
    project: {
      id,
      path: `/tmp/${id}`,
      name: id,
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    },
    threadCount: 1,
    lastActive,
  };
}

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

describe('setupEventListeners', () => {
  let cleanup: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetBindingMocks();
    resetThreadStatuses();
    resetSendQueue();
    resetProjectsForTest();
    resetRateLimitsInfo();
    getAllPanes().clear();
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    getAllPanes().clear();
    resetThreadStatuses();
    resetSendQueue();
  });

  it('registers and unregisters the unified listener set', () => {
    expect(wailsListenerCount('provider:approval')).toBe(1);
    expect(wailsListenerCount('provider:usage')).toBe(1);
    expect(wailsListenerCount('provider:status')).toBe(1);
    expect(wailsListenerCount('provider:item_event')).toBe(1);
    expect(wailsListenerCount('provider:turn_started')).toBe(1);
    expect(wailsListenerCount('provider:turn_completed')).toBe(1);
    expect(wailsListenerCount('provider:subagent_notification')).toBe(1);
    expect(wailsListenerCount('thread:updated')).toBe(1);

    cleanup();

    expect(wailsListenerCount('provider:approval')).toBe(0);
    expect(wailsListenerCount('provider:usage')).toBe(0);
    expect(wailsListenerCount('provider:status')).toBe(0);
    expect(wailsListenerCount('provider:item_event')).toBe(0);
    expect(wailsListenerCount('provider:turn_started')).toBe(0);
    expect(wailsListenerCount('provider:turn_completed')).toBe(0);
    expect(wailsListenerCount('provider:subagent_notification')).toBe(0);
    expect(wailsListenerCount('thread:updated')).toBe(0);

    cleanup = setupEventListeners();
  });

  it('routes item_event upserts only to the matching pane', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }));
    const paneB = await buildPane(makeThread({ id: 'thread-b' }));
    getAllPanes().set('a', paneA);
    getAllPanes().set('b', paneB);

    const item = makeItem({
      id: 'tool-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      status: 'running',
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: item.threadId,
      item,
    });
    await nextFrame();

    expect(paneA.items.map((item) => item.id)).toEqual(['tool-1']);
    expect(paneB.items).toEqual([]);
  });

  it('evicts the cached snapshot for every thread touched by an item upsert batch', async () => {
    const cacheModule = await import('./threadItemCache');
    cacheModule.threadItemCache.clear();
    // Seed snapshots for two threads.
    cacheModule.threadItemCache.set('thread-a', {
      items: [makeItem({ id: 'cached-a', threadId: 'thread-a' })],
      oldestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      latestSettledTurn: null,
    });
    cacheModule.threadItemCache.set('thread-other', {
      items: [makeItem({ id: 'cached-other', threadId: 'thread-other' })],
      oldestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      latestSettledTurn: null,
    });
    expect(cacheModule.threadItemCache.size).toBe(2);

    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    getAllPanes().set('a', pane);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'fresh', threadId: 'thread-a', kind: 'assistant_text',
      }),
    });
    await nextFrame();

    // 'thread-a' was touched — its snapshot must be gone.
    expect(cacheModule.threadItemCache.get('thread-a')).toBeNull();
    // 'thread-other' was untouched — its snapshot survives.
    expect(cacheModule.threadItemCache.get('thread-other')).not.toBeNull();

    cacheModule.threadItemCache.clear();
  });

  it('drops item_event upserts whose item thread does not match the event envelope', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }));
    const paneB = await buildPane(makeThread({ id: 'thread-b' }));
    getAllPanes().set('a', paneA);
    getAllPanes().set('b', paneB);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'cross-thread',
        threadId: 'thread-b',
        kind: 'assistant_text',
        status: 'streaming',
      }),
    });
    await nextFrame();

    expect(paneA.items).toEqual([]);
    expect(paneB.items).toEqual([]);
    expect(getThreadStatus('thread-a')).toBe('idle');
    expect(getThreadStatus('thread-b')).toBe('idle');
  });

  it('drops item_event upserts without a stable item id', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    getAllPanes().set('a', pane);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: '',
        threadId: 'thread-a',
        kind: 'assistant_text',
        status: 'streaming',
      }),
    });
    await nextFrame();

    expect(pane.items).toEqual([]);
    expect(getThreadStatus('thread-a')).toBe('idle');
  });

  it('routes item_event deltas only to the matching pane', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }));
    const paneB = await buildPane(makeThread({ id: 'thread-b' }));
    paneA.upsertItem(makeItem({
      id: 'text:0:0',
      threadId: 'thread-a',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    paneB.upsertItem(makeItem({
      id: 'text:0:0',
      threadId: 'thread-b',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
    getAllPanes().set('a', paneA);
    getAllPanes().set('b', paneB);

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-a',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    await nextFrame();

    expect(paneA.items.find((it) => it.id === 'text:0:0')?.summary).toBe('hello world');
    expect(paneB.items.find((it) => it.id === 'text:0:0')?.summary).toBe('hello');
  });

  it('adds and resolves pending approvals through provider:approval', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:approval', {
      action: 'request',
      threadId: 'thread-1',
      request: {
        requestId: 'req-1',
        threadId: 'thread-1',
        toolName: 'bash',
        description: 'Allow bash?',
        input: null,
        title: 'Approve bash',
      },
    });

    expect(pane.pendingApprovals).toHaveLength(1);
    expect(getThreadStatus('thread-1')).toBe('pending-approval');

    emitWailsEvent('provider:approval', {
      action: 'resolve',
      threadId: 'thread-1',
      requestId: 'req-1',
      decision: 'approved',
    });

    expect(pane.pendingApprovals).toEqual([]);
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('records resolve tombstones even when a pane missed the original request', async () => {
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock('SwitchThread', async (threadId: unknown) =>
      makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }));
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
    }));
    setBindingMock('ListPendingInteractiveRequests', () => new Promise((resolve) => {
      releaseSnapshot = resolve;
    }));
    setBindingMock('ListRecentTurns', async () => []);
    setBindingMock('ListThreadCheckpoints', async () => []);

    const pane = createThreadPane();
    getAllPanes().set('main', pane);
    const switching = pane.switchThread(makeThread({ id: 'thread-1' }));
    for (let i = 0; i < 5 && !releaseSnapshot; i++) {
      await Promise.resolve();
    }
    expect(releaseSnapshot).toBeDefined();

    emitWailsEvent('provider:approval', {
      action: 'resolve',
      threadId: 'thread-1',
      requestId: 'approval-1',
      decision: 'approved',
    });
    emitWailsEvent('provider:user_input', {
      action: 'resolve',
      threadId: 'thread-1',
      requestId: 'input-1',
      decision: 'answered',
    });
    releaseSnapshot({
      approvals: [{
        requestId: 'approval-1',
        threadId: 'thread-1',
        toolName: 'bash',
        description: 'Allow bash?',
        input: null,
        title: 'Approve bash',
      }],
      userInputs: [{
        requestId: 'input-1',
        threadId: 'thread-1',
        toolName: 'user_input',
        title: 'User Input Required',
        questions: [{
          id: 'scope',
          header: 'Scope',
          question: 'Choose a scope',
        }],
      }],
    });
    await switching;

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.pendingUserInputs).toEqual([]);
  });

  it('sets thread error status from an error item upsert', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    const item = makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      summary: 'boom',
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    await nextFrame();

    expect(getThreadStatus('thread-1')).toBe('error');
  });

  it('clears cached durable Plan Ready when a proposed plan is implemented', async () => {
    const cached = makeThread({
      id: 'thread-1',
      hasActionableProposedPlan: true,
    });
    setBindingMock('ListThreads', async () => [cached]);
    await refreshThreads();
    const pane = await buildPane(cached);
    getAllPanes().set('main', pane);

    const item = makeItem({
      id: 'plan-1',
      threadId: 'thread-1',
      kind: 'tool_call',
      role: 'assistant',
      payloadKind: 'proposed_plan',
      status: 'completed',
      meta: '{"planImplementedAt":123}',
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    await nextFrame();

    expect(getThreads()[0]?.hasActionableProposedPlan).toBe(false);
    expect(pane.thread?.hasActionableProposedPlan).toBe(false);
  });

  it('ignores user-authored proposed-plan payloads when patching durable Plan Ready', async () => {
    const cached = makeThread({
      id: 'thread-1',
      hasActionableProposedPlan: false,
    });
    setBindingMock('ListThreads', async () => [cached]);
    await refreshThreads();

    const item = makeItem({
      id: 'user-plan',
      threadId: 'thread-1',
      kind: 'user_text',
      role: 'user',
      payloadKind: 'proposed_plan',
      status: 'completed',
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    await nextFrame();

    expect(getThreads()[0]?.hasActionableProposedPlan).toBe(false);
  });

  it('projects running -> idle from ordered item_event upserts', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    const streamingItem = makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: streamingItem.threadId,
      item: streamingItem,
    });
    await nextFrame();
    expect(getThreadStatus('thread-1')).toBe('running');

    const completedItem = makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: completedItem.threadId,
      item: completedItem,
    });
    await nextFrame();
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('projects item status synchronously while timeline upserts wait for the frame batch', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    const streamingItem = makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: streamingItem.threadId,
      item: streamingItem,
    });

    expect(getThreadStatus('thread-1')).toBe('running');
    expect(pane.items).toEqual([]);

    await nextFrame();
    expect(pane.items.map((item) => item.id)).toEqual(['text-1']);
  });

  it('flushes item_event batches on a bounded timeout when animation frames are delayed', async () => {
    const originalRAF = globalThis.requestAnimationFrame;
    const originalCancelRAF = globalThis.cancelAnimationFrame;
    vi.useFakeTimers();
    vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    try {
      const pane = await buildPane();
      getAllPanes().set('main', pane);

      const item = makeItem({ id: 'timeout-flush', kind: 'terminal_interaction' });
      emitWailsEvent('provider:item_event', {
        action: 'upsert',
        threadId: item.threadId,
        item,
      });

      expect(pane.items).toEqual([]);
      await vi.advanceTimersByTimeAsync(60);

      expect(pane.items.map((item) => item.id)).toEqual(['timeout-flush']);
    } finally {
      vi.useRealTimers();
      vi.stubGlobal('requestAnimationFrame', originalRAF);
      vi.stubGlobal('cancelAnimationFrame', originalCancelRAF);
    }
  });

  it('cancels a queued item_event batch on listener cleanup', async () => {
    const originalRAF = globalThis.requestAnimationFrame;
    const originalCancelRAF = globalThis.cancelAnimationFrame;
    vi.useFakeTimers();
    vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    try {
      const pane = await buildPane();
      getAllPanes().set('main', pane);

      const item = makeItem({ id: 'cancelled-flush', kind: 'terminal_interaction' });
      emitWailsEvent('provider:item_event', {
        action: 'upsert',
        threadId: item.threadId,
        item,
      });
      cleanup();
      cleanup = () => {};

      await vi.advanceTimersByTimeAsync(60);

      expect(pane.items).toEqual([]);
    } finally {
      vi.useRealTimers();
      vi.stubGlobal('requestAnimationFrame', originalRAF);
      vi.stubGlobal('cancelAnimationFrame', originalCancelRAF);
    }
  });

  it('applies same-frame upsert bursts as one timeline revision', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    const first = makeItem({ id: 'wait-1', kind: 'terminal_interaction', itemIndex: 2 });
    const second = makeItem({ id: 'wait-2', kind: 'terminal_interaction', itemIndex: 1 });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: first.threadId, item: first });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: second.threadId, item: second });

    expect(pane.items).toEqual([]);
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual(['wait-2', 'wait-1']);
    expect(pane.timelineRevision).toBe(1);
  });

  it('ignores stale item_event deltas after the item has completed', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    const streamingItem = makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'yield ',
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: streamingItem.threadId,
      item: streamingItem,
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: 'timeouts',
      updatedAt: 2,
    });
    await nextFrame();
    expect(pane.items.find((item) => item.id === 'text-1')?.summary).toBe('yield timeouts');

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-1',
      item: makeItem({
        id: 'text-1',
        kind: 'assistant_text',
        status: 'completed',
        summary: 'yield timeouts',
      }),
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: ' stale',
      updatedAt: 3,
    });
    await nextFrame();

    expect(pane.items.find((item) => item.id === 'text-1')?.summary).toBe('yield timeouts');
  });

  it('coalesces contiguous deltas without crossing upsert boundaries', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: 'pre ',
      updatedAt: 1,
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-1',
      item: makeItem({
        id: 'text-1',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'base ',
      }),
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: 'post',
      updatedAt: 2,
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: ' stream',
      updatedAt: 3,
    });
    await nextFrame();

    // The events.ts batch flushes pending deltas around every upsert
    // boundary (`flushPendingDeltas()` at the upsert, then
    // `flushPendingUpserts()` before the next delta queues). Under the
    // collapsed architecture the upsert REPLACES `items[i]` with its
    // canonical summary — the upsert is authoritative and supersedes
    // any unflushed deltas before it. Deltas after the upsert append
    // to the upsert's summary. The "doesn't cross" contract is: the
    // post-upsert delta group sees the upsert's base, not the
    // pre-upsert delta state.
    expect(pane.items.find((item) => item.id === 'text-1')?.summary).toBe('base post stream');
  });

  it('drops item_event payloads with unknown actions', async () => {
    const pane = await buildPane();
    pane.upsertItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'stable',
    }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_event', {
      action: 'mystery',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: ' mutated',
      updatedAt: 2,
    });
    await nextFrame();

    expect(pane.items.find((item) => item.id === 'text-1')?.summary).toBe('stable');
  });

  it('updates cached thread rows from thread:updated', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({
        id: 'thread-1',
        title: 'Old',
        model: 'claude-sonnet-4-6',
        lastReadAt: 300,
        latestTurnCompletedAt: 300,
      }),
    ]);
    await refreshThreads();

    const pane = await buildPane(makeThread({
      id: 'thread-1',
      title: 'Old',
      model: 'claude-sonnet-4-6',
      lastReadAt: 200,
      latestTurnCompletedAt: 200,
    }));
    getAllPanes().set('main', pane);
    await refreshThreads();

    emitWailsEvent('thread:updated', makeThread({
      id: 'thread-1',
      title: 'New title',
      model: 'claude-opus-4-1',
      lastReadAt: 100,
      latestTurnCompletedAt: 100,
    }));

    expect(pane.thread?.title).toBe('New title');
    expect(pane.thread?.model).toBe('claude-opus-4-1');
    expect(pane.thread?.lastReadAt).toBe(300);
    expect(pane.thread?.latestTurnCompletedAt).toBe(300);
    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.model).toBe('claude-opus-4-1');
    expect(getThreads()[0]?.lastReadAt).toBe(300);
    expect(getThreads()[0]?.latestTurnCompletedAt).toBe(300);
  });

  it('does NOT bump cached project activity from non-user_text item_event upserts', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
      makeThread({ id: 'thread-fresh', projectId: 'project-fresh', updatedAt: 9000 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
      projectWithCounts('project-fresh', 9000),
    ]);
    await refreshThreads();
    await refreshProjects();
    const pane = await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-stale',
      item: makeItem({
        id: 'item-new',
        threadId: 'thread-stale',
        kind: 'assistant_text',
        updatedAt: 10_000,
      }),
    });
    await nextFrame();

    // assistant_text upserts must not advance the sidebar activity —
    // that's the bug fix for "sidebar reshuffles on every chunk".
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(100);
    expect(pane.thread?.updatedAt).toBe(100);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(100);
  });

  it('does NOT bump cached project activity from item_event deltas', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
      makeThread({ id: 'thread-fresh', projectId: 'project-fresh', updatedAt: 9000 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
      projectWithCounts('project-fresh', 9000),
    ]);
    await refreshThreads();
    await refreshProjects();

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-stale',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: 'streamed',
      updatedAt: 10_000,
    });
    await nextFrame();

    // Streaming deltas are the most-frequent firing path; they must
    // never advance the sidebar timestamp.
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(100);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(100);
  });

  it('bumps cached project activity from user_text item_event upserts', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
      makeThread({ id: 'thread-fresh', projectId: 'project-fresh', updatedAt: 9000 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
      projectWithCounts('project-fresh', 9000),
    ]);
    await refreshThreads();
    await refreshProjects();
    const pane = await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-stale',
      item: makeItem({
        id: 'user:0',
        threadId: 'thread-stale',
        kind: 'user_text',
        updatedAt: 10_000,
      }),
    });
    await nextFrame();

    // user_text is one of three sidebar-bump boundaries: send →
    // surface the thread to the top.
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(10_000);
    expect(pane.thread?.updatedAt).toBe(10_000);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(10_000);
  });

  it('bumps cached project activity on provider:turn_completed', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
    ]);
    await refreshThreads();
    await refreshProjects();

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-stale',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 100,
      completedAt: 12_000,
      stopReason: 'end_turn',
      assistantMessageId: '',
      tokenUsage: '',
      aborted: false,
      errorMessage: '',
    });
    await nextFrame();

    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(12_000);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(12_000);
  });

  it('bumps cached project activity on provider:approval request', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
    ]);
    await refreshThreads();
    await refreshProjects();
    const pane = await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));
    getAllPanes().set('main', pane);

    const requestedAt = Date.now();
    emitWailsEvent('provider:approval', {
      action: 'request',
      threadId: 'thread-stale',
      requestedAt,
      request: {
        requestId: 'req-1',
        threadId: 'thread-stale',
        turnId: 'turn-1',
        kind: 'tool_call',
        toolName: 'Bash',
        title: 'Approve command',
      },
    });
    await nextFrame();

    // Wire-pushed requestedAt is what the backend wrote to
    // threads.updated_at via MarkThreadActivity; the cached value
    // should match exactly so live order and persisted order agree.
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(requestedAt);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(requestedAt);
  });

  it('bumps cached project activity on provider:user_input request', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
    ]);
    await refreshThreads();
    await refreshProjects();
    const pane = await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));
    getAllPanes().set('main', pane);

    const requestedAt = Date.now();
    emitWailsEvent('provider:user_input', {
      action: 'request',
      threadId: 'thread-stale',
      requestedAt,
      request: {
        requestId: 'input-1',
        threadId: 'thread-stale',
        turnId: 'turn-1',
        toolName: 'user_input',
        title: 'Choose one',
        questions: [
          {
            id: 'scope',
            question: 'Which scope?',
            options: [
              { label: 'turn', description: 'Just this turn' },
            ],
          },
        ],
      },
    });
    await nextFrame();

    // Wire-pushed requestedAt is what the backend wrote to
    // threads.updated_at; the cached value should match exactly so live
    // and persisted order agree.
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(requestedAt);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(requestedAt);
  });

  it('does NOT bump activity on provider:approval resolve', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
    ]);
    await refreshThreads();
    await refreshProjects();

    emitWailsEvent('provider:approval', {
      action: 'resolve',
      threadId: 'thread-stale',
      requestId: 'req-1',
      decision: 'approved',
    });
    await nextFrame();

    // Approval resolutions ride on the user's reply (a user_text
    // upsert path) — no separate bump here.
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(100);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(100);
  });

  it('does not regress project activity from stale thread:updated events', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', projectId: 'project-1', title: 'Old', updatedAt: 500 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-1', 500),
    ]);
    await refreshThreads();
    await refreshProjects();

    emitWailsEvent('thread:updated', makeThread({
      id: 'thread-1',
      projectId: 'project-1',
      title: 'New title',
      updatedAt: 100,
    }));

    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.updatedAt).toBe(500);
    expect(getProjects()[0]?.lastActive).toBe(500);
  });

  it('refreshes sidebar project activity after provider event transport gaps', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
      makeThread({ id: 'thread-fresh', projectId: 'project-fresh', updatedAt: 9000 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
      projectWithCounts('project-fresh', 9000),
    ]);
    await refreshThreads();
    await refreshProjects();

    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 10_000 }),
      makeThread({ id: 'thread-fresh', projectId: 'project-fresh', updatedAt: 9000 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 10_000),
      projectWithCounts('project-fresh', 9000),
    ]);

    emitWailsEvent(transportGapChannel, {
      channel: 'provider:item_event',
      seq: 42,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(10_000);
    expect(getProjects().find((project) => project.project.id === 'project-stale')?.lastActive).toBe(10_000);
  });

  it('preserves an explicit unread marker when thread:updated is stale', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({
        id: 'thread-1',
        title: 'Old',
        lastReadAt: 0,
        latestTurnCompletedAt: 300,
      }),
    ]);
    await refreshThreads();

    emitWailsEvent('thread:updated', makeThread({
      id: 'thread-1',
      title: 'New title',
      lastReadAt: 300,
      latestTurnCompletedAt: 300,
    }));

    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.lastReadAt).toBe(0);
  });

  it('updates pane providerBanner from provider:status', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:status', providerStatusEvent({
      status: 'unauthenticated',
      message: 'Claude not authenticated',
    }));

    expect(pane.providerBanner?.status).toBe('unauthenticated');

    emitWailsEvent('provider:status', providerStatusEvent({ status: 'ready', actionable: false }));
    expect(pane.providerBanner).toBeNull();
  });

  it('hydrates accountInfo store from provider:account', async () => {
    const accountInfo = await import('./accountInfo.svelte');
    accountInfo.resetForTest();

    emitWailsEvent('provider:account', {
      provider: 'claude',
      account: { subscriptionType: 'Claude Max', tokenSource: 'oauth', apiProvider: 'firstParty' },
    });

    expect(accountInfo.getProviderAccount('claude')?.subscriptionType).toBe('Claude Max');
    expect(accountInfo.getProviderAccount('codex')).toBeNull();

    emitWailsEvent('provider:account', {
      provider: 'codex',
      account: { subscriptionType: 'pro', apiProvider: 'openai' },
    });

    expect(accountInfo.getProviderAccount('codex')?.subscriptionType).toBe('pro');
    // Both providers populate independently — Claude entry survives the codex update.
    expect(accountInfo.getProviderAccount('claude')?.subscriptionType).toBe('Claude Max');

    accountInfo.resetForTest();
  });

  it('drops malformed provider:account events before hitting the store', async () => {
    const accountInfo = await import('./accountInfo.svelte');
    accountInfo.resetForTest();

    // Unknown provider name — should be ignored, not silently inserted.
    emitWailsEvent('provider:account', {
      provider: 'mystery',
      account: { subscriptionType: 'enterprise' },
    });
    // account is a string instead of object — passes a "truthy account"
    // gate but type-narrowed validation must drop it.
    emitWailsEvent('provider:account', {
      provider: 'claude',
      account: 'not-an-object',
    });
    // No account at all.
    emitWailsEvent('provider:account', { provider: 'claude' });

    expect(accountInfo.getProviderAccount('claude')).toBeNull();
    expect(accountInfo.getProviderAccount('codex')).toBeNull();

    accountInfo.resetForTest();
  });

  it('updates and clears the context meter through provider:usage', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 2048,
      maxTokens: 200000,
      contextPercent: 1.024,
    });
    expect(pane.contextWindow).toEqual({
      usedTokens: 2048,
      maxTokens: 200000,
      usedPercentage: 1.024,
      autoCompactPercent: 90,
      autoCompactTokenLimit: 180000,
    });

    emitWailsEvent('provider:usage', {
      action: 'reset',
      threadId: 'thread-1',
    });
    expect(pane.contextWindow).toBeNull();
  });

  // Chat-rewrite routing: EventRateLimits folds onto provider:usage
  // via `action: 'rate_limits'`. The listener must NOT treat this as a
  // reset — the last-seen context-window ring stays in place — and it
  // MUST land the snapshot in the provider-keyed global store
  // (`rateLimitsInfo.svelte.ts`).
  it('routes EventRateLimits to the provider-global store without clobbering the context ring', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    // Seed a real context window first; the rate-limits event must not
    // wipe this state.
    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 5000,
      maxTokens: 200000,
      contextPercent: 2.5,
    });
    expect(pane.contextWindow?.usedTokens).toBe(5000);

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'five_hour', limitName: '5h', usedPercent: 62.5, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    // Context window untouched.
    expect(pane.contextWindow?.usedTokens).toBe(5000);
    expect(pane.contextWindow?.maxTokens).toBe(200000);

    // Global store populated with the 5h entry under provider 'claude'.
    const fiveHour = getProviderRateLimit('claude', 300);
    expect(fiveHour).not.toBeNull();
    expect(fiveHour?.usedPercent).toBe(62.5);
    expect(fiveHour?.resetsAt).toBe(1776283200);
  });

  // Claude emits one window per `rate_limit_event` (5h XOR 7d). A
  // subsequent event for the OTHER window must merge into the same
  // provider slot, not replace it. Codex emits both together; we test
  // the harder Claude case here because it's the merge-correctness pin.
  it('merges Claude single-window updates without clobbering the other window', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'seven_day', limitName: '7d', usedPercent: 51, windowMins: 10080, resetsAt: 1776981600 },
        ],
        updatedAt: 1776283500,
      },
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(30);
    expect(getProviderRateLimit('claude', 10080)?.usedPercent).toBe(51);
  });

  // Unknown rate-limit types arrive with windowMins=0 from the parser
  // (Claude's `windowMinsForRateLimitType` fallback). The store must
  // drop those rather than write a 0-keyed slot — the toolbar reads
  // `getProviderRateLimit(provider, 300)` / `(provider, 10080)` and a
  // stray 0 entry would let an unrenderable window fill memory forever.
  it('filters out windowMins=0 entries so unknown rate-limit types do not pollute the store', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'thirty_day', limitName: 'thirty_day', usedPercent: 10, windowMins: 0, resetsAt: 1776283200 },
          { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);
    expect(getProviderRateLimit('claude', 0)).toBeNull();
  });

  // Rate-limit data is account-wide, not thread-wide. A snapshot for
  // provider 'claude' is visible to every pane that reads with
  // provider 'claude' — including freshly-switched-to threads. This
  // is the opposite of the legacy per-pane behavior and the whole
  // point of the global store. Keep the user expectation pinned:
  // "values persist even if you switch to a new thread."
  it('rate-limit snapshots are visible across thread switches', async () => {
    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-A',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'five_hour', limitName: '5h', usedPercent: 73, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    // Even before any pane registers itself for thread-B, the global
    // store carries the value — RateLimitMeter reads from it directly.
    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(73);
  });

  // Provider isolation: a Codex snapshot must not bleed into the
  // Claude slot, even though both flow through the same UsageEvent
  // listener. The store keys by `snapshot.provider`.
  it('isolates provider slots so codex snapshots do not bleed into claude', async () => {
    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'codex',
        limits: [
          { limitId: 'primary', limitName: '5h', usedPercent: 88, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(88);
    expect(getProviderRateLimit('claude', 300)).toBeNull();
  });

  // Empty snapshots (no `limits` entries) arrive in edge cases — the
  // store must NOT wipe its last-known state. Holding on to the
  // previous good value is the entire reason for the global store
  // (the per-pane prior implementation had a wipe-on-replaceThread
  // bug that flickered the rings during a session).
  it('does not wipe last-known values on an empty rate_limits snapshot', async () => {
    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776283200 },
        ],
        updatedAt: 1776283000,
      },
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);

    // Empty-limits snapshot — must be a no-op, not a wipe.
    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [],
        updatedAt: 1776284000,
      },
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);
  });

  // EventSessionStatus routing: persistent kinds surface on
  // provider:status (banner update). Only boot-time presence kinds
  // remain on this channel — retry vocabulary moved to the api_retry
  // timeline row, and session-death drives provider:session_died →
  // pane.generalError instead.
  it('routes persistent EventSessionStatus to provider:status; drops unknown kinds', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:status', {
      kind: 'unauthenticated',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'Re-authenticate',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('unauthenticated');
    expect(pane.providerBanner?.message).toBe('Re-authenticate');

    emitWailsEvent('provider:status', {
      kind: 'version_incompatible',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'Update Claude CLI',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('version_too_old');

    emitWailsEvent('provider:status', {
      kind: 'binary_missing',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'Install Claude CLI',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('not_found');

    // Unknown kind → dropped. Use a console.warn spy to confirm the
    // emit landed on the "drop with warn" path rather than silently
    // mutating banner state. Retry vocabulary (rate_limited_retrying,
    // transient_retry, ok) is now in the closed unknown set.
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    emitWailsEvent('provider:status', {
      kind: 'rate_limited_retrying',
      provider: 'claude',
      threadId: 'thread-1',
    } as unknown as ProviderStatusEvent);
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('unknown kind'));
    warnSpy.mockRestore();
  });

  // --- Turn lifecycle routing (Wave 2) ---------------------------------------

  it('routes provider:turn_started to pane.setActiveTurn for the matching thread only', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }));
    const paneB = await buildPane(makeThread({ id: 'thread-b' }));
    getAllPanes().set('a', paneA);
    getAllPanes().set('b', paneB);

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-a',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
    });

    expect(getActiveTurn(paneA.threadId)).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    expect(getActiveTurn(paneA.threadId) !== null).toBe(true);
    expect(getActiveTurn(paneB.threadId)).toBeNull();
    expect(getActiveTurn(paneB.threadId) !== null).toBe(false);
  });

  it('routes provider:turn_completed to pane.settleTurn and clears activeTurn', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 'thread-1' })]);
    await refreshThreads();
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
    });

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    expect(pane.latestSettledTurn?.turnId).toBe('turn-1');
    expect(pane.latestSettledTurn?.assistantMessageId).toBe('text:0:3');
    expect(pane.latestSettledTurn?.completedAt).toBe(2000);
    expect(pane.latestSettledTurn?.stopReason).toBe('end_turn');
    expect(pane.thread?.latestTurnCompletedAt).toBe(2000);
    expect(getThreads().find((thread) => thread.id === 'thread-1')?.latestTurnCompletedAt).toBe(2000);
  });

  it('turns on the pending-send bridge when the backend starts flushing queued items', async () => {
    expect(hasPendingSend('thread-1')).toBe(false);

    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
      reason: QUEUE_STATE_REASON_FLUSH_STARTED,
    });

    expect(hasPendingSend('thread-1')).toBe(true);
    expect(getThreadStatus('thread-1')).toBe('running');

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-after-flush',
      turnIndex: 1,
      startedAt: 3000,
    });

    expect(hasPendingSend('thread-1')).toBe(false);
    expect(getActiveTurn('thread-1')).toEqual({
      turnId: 'turn-after-flush',
      turnIndex: 1,
      startedAt: 3000,
    });
  });

  it('does not turn on the pending-send bridge for boundary queue snapshots', async () => {
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-boundary',
      turnIndex: 0,
      startedAt: 1000,
    });

    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
    });

    expect(hasPendingSend('thread-1')).toBe(false);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-boundary',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
    });

    expect(hasPendingSend('thread-1')).toBe(false);
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('clears the pending-send bridge when queued-item flush fails before a new turn starts', async () => {
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
      reason: QUEUE_STATE_REASON_FLUSH_STARTED,
    });
    expect(hasPendingSend('thread-1')).toBe(true);

    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
      reason: QUEUE_STATE_REASON_FLUSH_FAILED,
    });

    expect(hasPendingSend('thread-1')).toBe(false);
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('parses tokenUsage JSON from provider:turn_completed into a typed summary', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      // The wire payload's `tokenUsage` is a JSON-encoded string because
      // triage round-trips it through SQLite's token_usage_json column.
      // We accept snake_case too (Claude's wire shape).
      tokenUsage: JSON.stringify({
        input_tokens: 120,
        output_tokens: 45,
        cache_read_input_tokens: 10,
        total_cost_usd: 0.0034,
      }),
    });

    expect(pane.latestSettledTurn?.tokenUsage).toEqual({
      inputTokens: 120,
      outputTokens: 45,
      cacheReadInputTokens: 10,
      totalCostUsd: 0.0034,
    });
  });

  it('tolerates malformed tokenUsage without crashing — tokenUsage becomes null', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      tokenUsage: '{this is not json',
    });

    expect(pane.latestSettledTurn).not.toBeNull();
    expect(pane.latestSettledTurn?.tokenUsage).toBeNull();
  });

  it('routes provider:turn_completed.aborted into the settled projection', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'interrupted',
      aborted: true,
      errorMessage: 'user interrupted',
    });

    expect(pane.latestSettledTurn?.aborted).toBe(true);
    expect(pane.latestSettledTurn?.errorMessage).toBe('user interrupted');
    expect(pane.latestSettledTurn?.stopReason).toBe('interrupted');
  });

  it('routes provider:subagent_notification to the matching pane', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:subagent_notification', {
      threadId: 'thread-1',
      meta: JSON.stringify({ agentId: 'child-a', status: 'completed' }),
    });

    expect(pane.subagentNotifications).toHaveLength(1);
    expect(pane.subagentNotifications[0].threadId).toBe('thread-1');
    expect(pane.subagentNotifications[0].meta).toContain('child-a');
  });

  it('drops provider:turn_started for non-matching threadIds', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-other',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });
});
