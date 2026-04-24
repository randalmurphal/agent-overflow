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

    expect(paneA.items.map((item) => item.id)).toEqual(['tool-1']);
    expect(paneB.items).toEqual([]);
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

    expect(getThreadStatus('thread-1')).toBe('error');
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
    expect(getThreadStatus('thread-1')).toBe('idle');
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

  it('updates cached thread rows from thread:updated', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', title: 'Old', model: 'claude-sonnet-4-6' }),
    ]);
    await refreshThreads();

    const pane = await buildPane(makeThread({ id: 'thread-1', title: 'Old', model: 'claude-sonnet-4-6' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('thread:updated', makeThread({
      id: 'thread-1',
      title: 'New title',
      model: 'claude-opus-4-1',
    }));

    expect(pane.thread?.title).toBe('New title');
    expect(pane.thread?.model).toBe('claude-opus-4-1');
    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.model).toBe('claude-opus-4-1');
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
  // provider:status (banner update); transient kinds drop silently.
  it('routes persistent EventSessionStatus to provider:status; drops transient', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    getAllPanes().set('main', pane);

    // Persistent kind → banner appears. The router emits the rewrite
    // shape (`kind` + `threadId` + `provider`); the listener maps kind
    // onto the legacy `status` vocabulary internally so the existing
    // ProviderStatusBanner renders unchanged.
    emitWailsEvent('provider:status', {
      kind: 'rate_limited_retrying',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'Retrying — rate limited',
      // `status` + `actionable` aren't populated by the router, but the
      // existing type requires them. Cast to satisfy the shape; the
      // handler derives `status` from `kind`.
    } as unknown as ProviderStatusEvent);

    expect(pane.providerBanner).not.toBeNull();
    // rate_limited_retrying folds onto the warning-styled `version_too_old`
    // legacy status — see KIND_TO_LEGACY_STATUS in events.ts for why.
    expect(pane.providerBanner?.status).toBe('version_too_old');

    // transient_retry also folds onto `version_too_old` so the banner
    // renders warning-styled regardless of the precise retry cause — the
    // banner Message is where the cause is surfaced.
    emitWailsEvent('provider:status', {
      kind: 'transient_retry',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'server_error',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('version_too_old');
    expect(pane.providerBanner?.message).toBe('server_error');

    // Clear banner with kind=ok (spec: "ok" → clear signal).
    emitWailsEvent('provider:status', {
      kind: 'ok',
      provider: 'claude',
      threadId: 'thread-1',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner).toBeNull();

    // Unknown kind → dropped. Use a console.warn spy to confirm the
    // emit landed on the "drop with warn" path rather than silently
    // mutating banner state.
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    emitWailsEvent('provider:status', {
      kind: 'not_a_real_kind',
      provider: 'claude',
      threadId: 'thread-1',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner).toBeNull();
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
