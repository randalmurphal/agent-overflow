import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setupEventListeners } from './events';
import { getAllPanes } from './panes.svelte';
import { getThreadStatus, resetForTest as resetThreadStatuses } from './threadStatuses.svelte';
import { getThreads, refreshThreads } from './threads.svelte';
import { emitWailsEvent, resetWailsMocks, wailsListenerCount } from '../../test/mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import type { ProviderStatusEvent } from '../types/events';

function providerStatusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found',
    actionable: true,
    ...overrides,
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
    getAllPanes().clear();
    setBindingMock('ListThreads', async () => []);
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    getAllPanes().clear();
    resetThreadStatuses();
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

    expect(paneA.liveItemSummaries['text:0:0']).toBe('hello world');
    expect(paneB.liveItemSummaries['text:0:0']).toBe('hello');
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
    expect(pane.liveItemSummaries['text-1']).toBe('yield timeouts');

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
    expect(pane.liveItemSummaries['text-1']).toBeUndefined();
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

    expect(pane.liveItemSummaries['text-1']).toBe('base pre post stream');
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

    expect(pane.liveItemSummaries['text-1']).toBe('stable');
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
  // reset — the last-seen context-window ring stays in place so the
  // meter keeps rendering its existing value while the popover picks up
  // the new rate-limits row (future work — see the "Future work" note in
  // applyUsageEvent's rate_limits branch).
  it('routes EventRateLimits to provider:usage without clobbering the context ring', async () => {
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

    // Context window is unchanged; the rate-limits payload is a sibling
    // signal on the same channel rather than a ring update.
    expect(pane.contextWindow?.usedTokens).toBe(5000);
    expect(pane.contextWindow?.maxTokens).toBe(200000);
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

    expect(paneA.activeTurn).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    expect(paneA.isTurnActive).toBe(true);
    expect(paneB.activeTurn).toBeNull();
    expect(paneB.isTurnActive).toBe(false);
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
    expect(pane.isTurnActive).toBe(true);

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
    });

    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);
    expect(pane.latestSettledTurn?.turnId).toBe('turn-1');
    expect(pane.latestSettledTurn?.assistantMessageId).toBe('text:0:3');
    expect(pane.latestSettledTurn?.completedAt).toBe(2000);
    expect(pane.latestSettledTurn?.stopReason).toBe('end_turn');
    expect(pane.thread?.latestTurnCompletedAt).toBe(2000);
    expect(getThreads().find((thread) => thread.id === 'thread-1')?.latestTurnCompletedAt).toBe(2000);
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

    expect(pane.activeTurn).toBeNull();
    expect(pane.isTurnActive).toBe(false);
  });
});
