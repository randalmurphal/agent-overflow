import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setupEventListeners } from './events';
import { createThreadPane } from './thread.svelte';
import { findPaneShowingThread, registerPaneForTest, resetPanesForTest } from './panes.svelte';
import {
  getActiveTurn,
  getThreadStatus,
  hasPendingSend,
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { resetForTest as resetSendQueue } from './sendQueue.svelte';
import { resetLiveUsageSnapshotsForTest } from './threadContextWindow';
import { getThreads, getThreadLiveActivityAt, refreshThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import { getProjectLiveActivityAt, getProjects, refreshProjects, resetProjectsForTest } from './projects.svelte';
import {
  getProviderRateLimit,
  resetForTest as resetRateLimitsInfo,
} from './rateLimitsInfo.svelte';
import {
  getProviderStatus,
  resetForTest as resetProviderStatuses,
} from './providerStatus.svelte';
import { transportGapChannel } from '../transport/wsClient';
import { emitWailsEvent, resetWailsMocks, wailsListenerCount } from '../../test/mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  buildPane,
  makeItem,
  makeThread,
  stubScrollController,
} from '../../test/helpers/chat';
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

// Live-activity reads: streaming bumps live in per-entity boxes, not on
// the row objects (see threads/projects stores), so activity assertions
// go through the live getters. This also keeps the negative tests
// honest — a bump that lands in the box alone still trips them.
function liveThreadActivity(id: string): number | undefined {
  const thread = getThreads().find((t) => t.id === id);
  return thread ? getThreadLiveActivityAt(thread) : undefined;
}

function liveProjectActivity(id: string): number | undefined {
  const project = getProjects().find((p) => p.project.id === id);
  return project ? getProjectLiveActivityAt(project) : undefined;
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
    resetLiveUsageSnapshotsForTest();
    resetProjectsForTest();
    resetRateLimitsInfo();
    resetProviderStatuses();
    setBindingMock('AutoResumeThread', async () => {});
    setBindingMock('GetRateLimitsSnapshots', async () => []);
    setBindingMock('ListProviderAccounts', async () => []);
    resetPanesForTest();
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    resetPanesForTest();
    resetThreadStatuses();
    resetSendQueue();
  });

  it('registers and unregisters the unified listener set', () => {
    expect(wailsListenerCount('provider:approval')).toBe(1);
    expect(wailsListenerCount('provider:usage')).toBe(1);
    expect(wailsListenerCount('provider:model_fallback')).toBe(1);
    expect(wailsListenerCount('provider:status')).toBe(1);
    expect(wailsListenerCount('provider:session_account')).toBe(1);
    expect(wailsListenerCount('provider:account_usage_error')).toBe(1);
    expect(wailsListenerCount('provider:item_event')).toBe(1);
    expect(wailsListenerCount('provider:turn_started')).toBe(1);
    expect(wailsListenerCount('provider:turn_completed')).toBe(1);
    expect(wailsListenerCount('thread:updated')).toBe(1);
    expect(wailsListenerCount('workflow:error')).toBe(1);

    cleanup();

    expect(wailsListenerCount('provider:approval')).toBe(0);
    expect(wailsListenerCount('provider:usage')).toBe(0);
    expect(wailsListenerCount('provider:model_fallback')).toBe(0);
    expect(wailsListenerCount('provider:status')).toBe(0);
    expect(wailsListenerCount('provider:session_account')).toBe(0);
    expect(wailsListenerCount('provider:account_usage_error')).toBe(0);
    expect(wailsListenerCount('provider:item_event')).toBe(0);
    expect(wailsListenerCount('provider:turn_started')).toBe(0);
    expect(wailsListenerCount('provider:turn_completed')).toBe(0);
    expect(wailsListenerCount('thread:updated')).toBe(0);
    expect(wailsListenerCount('workflow:error')).toBe(0);

    cleanup = setupEventListeners();
  });

  it('hydrates rate limits retained before the frontend connected', async () => {
    cleanup();
    resetRateLimitsInfo();
    setBindingMock('GetRateLimitsSnapshots', async () => [{
      provider: 'codex',
      limits: [
        { limitId: 'codex', limitName: '', usedPercent: 31, windowMins: 300, resetsAt: 4102444800 },
        { limitId: 'codex', limitName: '', usedPercent: 30, windowMins: 10080, resetsAt: 4103049600 },
      ],
      updatedAt: 1783629000000,
    }]);

    cleanup = setupEventListeners();
    await vi.waitFor(() => {
      expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(31);
      expect(getProviderRateLimit('codex', 10080)?.usedPercent).toBe(30);
    });
  });

  it('hydrates the selected provider account when connecting after the startup probe', async () => {
    const accountInfo = await import('./accountInfo.svelte');
    cleanup();
    accountInfo.resetForTest();
    // The backend probed at boot and cached the result, so no
    // provider:account event will ever arrive on this connection. The
    // pull is the only hydration path.
    setBindingMock('ListProviderAccounts', async () => [
      {
        id: 'claude-account',
        provider: 'claude',
        email: 'user@example.com',
        subscriptionType: 'Claude Max',
        active: true,
        generation: 7,
      },
      {
        id: 'claude-other',
        provider: 'claude',
        email: 'other@example.com',
        subscriptionType: 'Claude Max',
        active: false,
        generation: 7,
      },
    ]);

    cleanup = setupEventListeners();
    await vi.waitFor(() => {
      expect(accountInfo.getProviderAccount('claude')?.accountId).toBe('claude-account');
      expect(accountInfo.getProviderAccount('claude')?.subscriptionType).toBe('Claude Max');
    });
    // Codex reported no accounts at all — the selection must read as
    // signed out, not as whatever an earlier session left behind.
    expect(accountInfo.getProviderAccount('codex')).toBeNull();
  });

  it('routes item_event upserts only to the matching pane', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'b');

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

  it('routes provider:todo_update to the matching pane and clears on empty/null steps', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'b');

    // A non-empty update populates only the matching pane.
    emitWailsEvent('provider:todo_update', {
      threadId: 'thread-a',
      steps: [{ step: 'write tests', status: 'pending' }],
    });
    expect(paneA.liveTodo?.steps).toEqual([{ step: 'write tests', status: 'pending' }]);
    expect(paneB.liveTodo).toBeNull();

    // An empty update is the backend's clear signal (Task* delete-to-empty) —
    // it must drop the list, not freeze on the last item.
    emitWailsEvent('provider:todo_update', { threadId: 'thread-a', steps: [] });
    expect(paneA.liveTodo).toBeNull();

    // A null steps payload (a Go nil slice on the wire) must also clear via the
    // handler's Array.isArray guard, not throw.
    emitWailsEvent('provider:todo_update', {
      threadId: 'thread-a',
      steps: [{ step: 're-added', status: 'pending' }],
    });
    expect(paneA.liveTodo?.steps).toHaveLength(1);
    emitWailsEvent('provider:todo_update', { threadId: 'thread-a', steps: null });
    expect(paneA.liveTodo).toBeNull();
  });

  it('evicts the cached snapshot when an active-thread item upsert changes the pane window', async () => {
    const cacheModule = await import('./threadItemCache');
    cacheModule.threadItemCache.clear();
    // Seed snapshots for two threads.
    cacheModule.threadItemCache.set('thread-a', {
      items: [makeItem({ id: 'cached-a', threadId: 'thread-a' })],
      oldestLoadedTurnIndex: 0,
      newestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      hasMoreNewer: false,
      latestSettledTurn: null,
    });
    cacheModule.threadItemCache.set('thread-other', {
      items: [makeItem({ id: 'cached-other', threadId: 'thread-other' })],
      oldestLoadedTurnIndex: 0,
      newestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      hasMoreNewer: false,
      latestSettledTurn: null,
    });
    expect(cacheModule.threadItemCache.size).toBe(2);

    await buildPane(makeThread({ id: 'thread-a' }));

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'fresh', threadId: 'thread-a', kind: 'assistant_text',
      }),
    });
    await nextFrame();

    // 'thread-a' changed in the active pane — its snapshot must be gone.
    expect(cacheModule.threadItemCache.get('thread-a')).toBeNull();
    // 'thread-other' was untouched — its snapshot survives.
    expect(cacheModule.threadItemCache.get('thread-other')).not.toBeNull();

    // An upsert naming a thread with no pane still evicts defensively:
    // we cannot value-dedupe a window nobody owns. This branch was
    // deliberately left as-is when the channel was narrowed — a client
    // that is not watching a thread simply stops getting these frames,
    // and its stale snapshot is caught on read instead (the open stamps
    // the window and SyncThreadWindow answers stale with a replacing
    // page). It is the one accepted degradation of the narrowing.
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-other',
      item: makeItem({ id: 'fresh-other', threadId: 'thread-other', kind: 'assistant_text' }),
    });
    await nextFrame();
    expect(cacheModule.threadItemCache.get('thread-other')).toBeNull();

    cacheModule.threadItemCache.clear();
  });

  it('preserves the cached snapshot for redundant active-thread item upserts', async () => {
    const cacheModule = await import('./threadItemCache');
    cacheModule.threadItemCache.clear();
    const item = makeItem({
      id: 'cached-a',
      threadId: 'thread-a',
      kind: 'assistant_text',
      summary: 'same',
    });
    await buildPane(makeThread({ id: 'thread-a' }), [item]);
    cacheModule.threadItemCache.set('thread-a', {
      items: [item],
      oldestLoadedTurnIndex: 0,
      newestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      hasMoreNewer: false,
      latestSettledTurn: null,
    });

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item },
    });
    await nextFrame();

    expect(cacheModule.threadItemCache.get('thread-a')).not.toBeNull();
    cacheModule.threadItemCache.clear();
  });

  it('stamps lastLiveContentAt when an assistant-text provider upsert changes the pane window', async () => {
    // A new text-like provider row arriving is live content — the events.ts
    // fan-out marks it so the chat scroll controller spring-chases
    // (content-keyed animation latch). lastLiveContentAt is 0 until
    // something streams, then carries a real performance.now() reading.
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    expect(pane.lastLiveContentAt).toBe(0);

    const before = performance.now();
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({ id: 'fresh', threadId: 'thread-a', kind: 'assistant_text' }),
    });
    await nextFrame();
    const after = performance.now();

    expect(pane.items.map((item) => item.id)).toEqual(['fresh']);
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
    // Bracket the stamp inside the real-clock window: this pins the stamp
    // to the SAME `performance.now()` timebase the MessageTimeline latch
    // reads. A regression that stamped via a different clock (e.g. Date.now,
    // ~1.7e12) would pass `> 0` but blow past `after` — and would make the
    // production latch compute a huge negative delta → permanently 'spring'.
    expect(pane.lastLiveContentAt).toBeGreaterThanOrEqual(before);
    expect(pane.lastLiveContentAt).toBeLessThanOrEqual(after);
  });

  it('does not stamp lastLiveContentAt through the events predicate for new Bash rows', async () => {
    // Scope: the per-row predicate (providerUpsertAdvancesLiveContent)
    // stamps inserts only for text-like kinds. Non-text appends stamp
    // through the pane's gated arm site instead
    // (armLiveContentAppendSpring — requires an attached scroll
    // controller, absent here; see 'stamps lastLiveContentAt when a
    // background completion batch lands post-turn' below and the
    // structural-append describe in thread.svelte.test.ts).
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'bash-1',
        threadId: 'thread-a',
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: ls',
        meta: JSON.stringify({ input: { command: 'ls' } }),
      }),
    });
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual(['bash-1']);
    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('does not stamp lastLiveContentAt for a redundant upsert that leaves the window unchanged', async () => {
    // The stamp lives inside `if (pane.upsertItems(...))` — gated on a real
    // change. An echo of an existing row dedupes to no change, so the
    // controller stays sync-pinned.
    const item = makeItem({
      id: 'cached-a',
      threadId: 'thread-a',
      kind: 'assistant_text',
      summary: 'same',
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item },
    });
    await nextFrame();

    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('stamps lastLiveContentAt when same-row Bash completion lands result chrome', async () => {
    // running→completed on an already mounted row grows it (result chrome,
    // output preview). That growth must spring like text — sync-pinning it
    // landed a whole-viewport teleport between spring glides
    // (bug-report-20260702T184236Z).
    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 1',
      meta: JSON.stringify({ input: { command: 'sleep 1' } }),
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: {
        ...item,
        status: 'completed',
        payloadId: 'payload-bash-1',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({
          command: 'sleep 1',
          exitCode: 0,
          lineCount: 1,
          preview: 'done',
        }),
        updatedAt: item.updatedAt + 1,
      },
    });
    await nextFrame();

    expect(pane.items[0].status).toBe('completed');
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
  });

  it('stamps lastLiveContentAt when a running Bash row grows its output preview', async () => {
    // Streaming command output has no wire delta channel — each flush
    // window re-upserts the row with fresh payloadMeta (preview/stats)
    // while status stays 'running'. That is the row visibly growing.
    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: make build',
      meta: JSON.stringify({ input: { command: 'make build' } }),
      payloadId: 'payload-bash-1',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({ command: 'make build', lineCount: 2, preview: 'compiling…' }),
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: {
        ...item,
        payloadMeta: JSON.stringify({ command: 'make build', lineCount: 9, preview: 'linking…' }),
        updatedAt: item.updatedAt + 1,
      },
    });
    await nextFrame();

    expect(pane.items[0].status).toBe('running');
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
  });

  it('does not stamp lastLiveContentAt for an updatedAt-only tool row bump', async () => {
    // A re-upsert whose only change is the timestamp renders nothing new;
    // it must not hold the spring latch open.
    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 30',
      meta: JSON.stringify({ input: { command: 'sleep 30' } }),
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item, updatedAt: item.updatedAt + 1 },
    });
    await nextFrame();

    // The bump must actually APPLY (itemsAreEqual includes updatedAt, so
    // this upsert passes the changed-items gate) — otherwise the no-stamp
    // assertion below would be vacuous.
    expect(pane.items[0].updatedAt).toBe(item.updatedAt + 1);
    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('stamps lastLiveContentAt when a running tool row is flagged background', async () => {
    // Both backgrounding paths persist an upsert whose only rendered
    // change is the isBackground flip (Claude: tool_lifecycle.go launch-row
    // refresh; Codex: stampCodexItemBackgrounded). The flip swaps mounted
    // row chrome (spinner → backgrounded-launch), so it must spring like
    // any other visible-field update.
    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: npm run watch',
      meta: JSON.stringify({ input: { command: 'npm run watch' } }),
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item, isBackground: true, updatedAt: item.updatedAt + 1 },
    });
    await nextFrame();

    expect(pane.items[0].isBackground).toBe(true);
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
  });

  it('stamps lastLiveContentAt when an approval decision lands on a mounted row', async () => {
    // Approval resolution persists only the decision (plus updatedAt) when
    // summary/toolName are already set (approvals.go) — decision chips are
    // rendered chrome, so the standalone flip must stamp.
    const item = makeItem({
      id: 'tool-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Edit',
      summary: 'Edit: src/app.ts',
      decision: '',
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item, decision: 'approved', updatedAt: item.updatedAt + 1 },
    });
    await nextFrame();

    expect(pane.items[0].decision).toBe('approved');
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
  });

  it('does not stamp lastLiveContentAt through the events predicate for a same-batch tool row insert plus completion', async () => {
    // Both upserts compare against the pre-batch snapshot, so the
    // completion still resolves down the insert path — correct, because a
    // row that mounted this flush is in its estimate phase for the whole
    // flush. Pins the once-per-batch snapshot: a refactor to per-upsert
    // lookup would silently start stamping these bursts through the
    // UNGATED events predicate. In production the append itself stamps
    // via the pane's gated arm site (no controller is attached here, so
    // that path stays closed — see the post-turn background-completion
    // test below). (This test is also why buildPane owns pane
    // registration: registering the same pane under a second key made
    // iterPanes() apply value-different batches twice, and the
    // re-application stamped spuriously.)
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    expect(pane.lastLiveContentAt).toBe(0);

    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: true',
      meta: JSON.stringify({ input: { command: 'true' } }),
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item,
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: {
        ...item,
        status: 'completed',
        payloadId: 'payload-bash-1',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({ command: 'true', exitCode: 0, lineCount: 0, preview: '' }),
        updatedAt: item.updatedAt + 1,
      },
    });
    await nextFrame();

    expect(pane.items[0].status).toBe('completed');
    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('stamps lastLiveContentAt when a background completion batch lands post-turn', async () => {
    // The reported regression: a backgrounded task completing after the
    // turn ended arrives as NEW rows (task-notification + tool_completion
    // sibling, with same-batch enrichment resolving down the insert
    // path), so the per-row predicate never stamped and the rows
    // teleported in once the structural one-shot lapsed — while the
    // identical rows arriving mid-turn glided (streaming kept the latch
    // fresh). With a scroll controller attached (a mounted timeline),
    // the pane's append arm must stamp the latch AND open the one-shot
    // so the completion spring-scrolls in exactly like it does while
    // the agent is working.
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [
      makeItem({ id: 'seed', threadId: 'thread-a', turnIndex: 0, itemIndex: 0 }),
    ]);
    const markStructuralContentPending = vi.fn();
    pane.attachScrollController(
      stubScrollController({ markStructuralContentPending }),
    );
    expect(pane.lastLiveContentAt).toBe(0);

    const notification = makeItem({
      id: 'task-notification:task-1',
      threadId: 'thread-a',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'notification',
      role: 'system',
      status: 'completed',
      summary: 'Background task completed',
      meta: JSON.stringify({ task_id: 'task-1', source: 'task_notification', output_file_state: 'loading' }),
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: notification,
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'bash-1:completion',
        threadId: 'thread-a',
        turnIndex: 0,
        itemIndex: 2,
        kind: 'tool_completion',
        role: 'assistant',
        status: 'completed',
        toolName: 'Bash',
        summary: 'Background command finished',
        completionOf: 'bash-1',
        meta: JSON.stringify({ task_id: 'task-1', status_source: 'task_notification' }),
      }),
    });
    // Enrichment re-upsert of the notification in the same batch — still
    // resolves down the insert path against the pre-batch snapshot.
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: {
        ...notification,
        meta: JSON.stringify({ task_id: 'task-1', source: 'task_notification', output_file_state: 'loaded' }),
        updatedAt: notification.updatedAt + 1,
      },
    });
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual([
      'seed',
      'task-notification:task-1',
      'bash-1:completion',
    ]);
    expect(markStructuralContentPending).toHaveBeenCalled();
    expect(pane.lastLiveContentAt).toBeGreaterThan(0);
  });

  it('does not let off-window new rows stamp over a visible no-op bump', async () => {
    // The stamp decision is based on rows actually APPLIED to the visible
    // pane window. The batch pairs a visible updatedAt-only bump (applied,
    // so the changed-items gate is open, but renders nothing new) with a
    // new text-like row below the loaded floor (dropped by the window
    // guard). Neither may keep spring mode alive — an implementation that
    // stamped from the incoming batch instead of the applied rows would
    // fail here.
    const item = makeItem({
      id: 'bash-1',
      threadId: 'thread-a',
      turnIndex: 5,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 1',
      meta: JSON.stringify({ input: { command: 'sleep 1' } }),
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [item]);
    expect(pane.oldestLoadedTurnIndex).toBe(5);
    expect(pane.lastLiveContentAt).toBe(0);

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: { ...item, updatedAt: item.updatedAt + 1 },
    });
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-a',
      item: makeItem({
        id: 'below-window',
        threadId: 'thread-a',
        turnIndex: 2,
        itemIndex: 0,
        kind: 'assistant_text',
        summary: 'older content',
      }),
    });
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual(['bash-1']);
    expect(pane.items[0].updatedAt).toBe(item.updatedAt + 1);
    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('drops item_event upserts whose item thread does not match the event envelope', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'b');

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
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'b');
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

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-a',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    await nextFrame();
    // Smoothing routes streaming text through a per-item rAF smoother;
    // flush it so the assertion sees the fully revealed accumulated text.
    paneA.__flushItemSmoothersForTest();
    paneB.__flushItemSmoothersForTest();

    expect(paneA.items.find((it) => it.id === 'text:0:0')?.summary).toBe('hello world');
    expect(paneB.items.find((it) => it.id === 'text:0:0')?.summary).toBe('hello');
  });

  it('adds and resolves pending approvals through provider:approval', async () => {
    const pane = await buildPane();

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

    const pane = createThreadPane();
    registerPaneForTest('main', pane);
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

  it('sets thread error status from a thread:error_notice', async () => {
    await buildPane();

    emitWailsEvent('thread:error_notice', { threadId: 'thread-1', itemId: 'error-1' });

    expect(getThreadStatus('thread-1')).toBe('error');
  });

  it('sets thread error status for a thread this client has no row or pane for', async () => {
    await buildPane();

    // The whole point of the wildcard carrier: the Failed pill is read on
    // threads with no surface, which the narrowed transcript stream no
    // longer reaches.
    emitWailsEvent('thread:error_notice', { threadId: 'thread-unmounted', itemId: 'error-9' });

    expect(getThreadStatus('thread-unmounted')).toBe('error');
  });

  it('does not set thread error status from an error item upsert', async () => {
    await buildPane();

    // The item carries the prose; the badge is the notice's job. Keeping
    // both would double-fire on the watched thread and fire on neither for
    // an unwatched one.
    const item = makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      summary: 'boom',
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    await nextFrame();

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  // Durable Plan Ready is a derived column of the thread row, so the
  // backend broadcasts the whole row from every proposed-plan write
  // (the in-turn persist, the implemented mark, and the ensure-state
  // settles). The frontend no longer derives it from plan item upserts:
  // it cannot, for a thread it is not watching.
  it('raises cached durable Plan Ready from a thread:updated full row', async () => {
    const cached = makeThread({ id: 'thread-1', hasActionableProposedPlan: false });
    setBindingMock('ListThreads', async () => [cached]);
    await refreshThreads();
    const pane = await buildPane(cached);

    emitWailsEvent('thread:updated', {
      action: 'full',
      thread: { ...cached, hasActionableProposedPlan: true },
    });

    expect(getThreads()[0]?.hasActionableProposedPlan).toBe(true);
    expect(pane.thread?.hasActionableProposedPlan).toBe(true);
  });

  it('clears cached durable Plan Ready from a thread:updated full row', async () => {
    const cached = makeThread({ id: 'thread-1', hasActionableProposedPlan: true });
    setBindingMock('ListThreads', async () => [cached]);
    await refreshThreads();
    const pane = await buildPane(cached);

    emitWailsEvent('thread:updated', {
      action: 'full',
      thread: { ...cached, hasActionableProposedPlan: false },
    });

    expect(getThreads()[0]?.hasActionableProposedPlan).toBe(false);
    expect(pane.thread?.hasActionableProposedPlan).toBe(false);
  });

  it('does not move durable Plan Ready from a proposed-plan item upsert', async () => {
    const cached = makeThread({ id: 'thread-1', hasActionableProposedPlan: false });
    setBindingMock('ListThreads', async () => [cached]);
    await refreshThreads();

    const item = makeItem({
      id: 'plan-1',
      threadId: 'thread-1',
      kind: 'tool_call',
      role: 'assistant',
      payloadKind: 'proposed_plan',
      status: 'completed',
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    await nextFrame();

    expect(getThreads()[0]?.hasActionableProposedPlan).toBe(false);
  });

  it('does not project thread running from ordered item_event upserts', async () => {
    await buildPane();

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
    expect(getThreadStatus('thread-1')).toBe('idle');

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

  it('does not derive status from item rows while timeline upserts wait for the frame batch', async () => {
    const pane = await buildPane();

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

    expect(getThreadStatus('thread-1')).toBe('idle');
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

  it('flushes a Codex spawn/wait completion burst into the active pane before refresh', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a', provider: 'codex' }));
    const items = [
      makeItem({
        id: 'assistant-before-review',
        threadId: 'thread-a',
        itemIndex: 32,
        kind: 'assistant_text',
        summary: 'The diff is tiny.',
      }),
      makeItem({
        id: 'wait-review',
        threadId: 'thread-a',
        itemIndex: 33,
        kind: 'tool_call',
        toolName: 'wait_agent',
        summary: 'wait_agent',
        meta: JSON.stringify({
          input: {
            tool: 'wait_agent',
            receiverThreadIds: ['child-review'],
            agentsStates: {
              'child-review': { status: 'completed', message: 'Review finished' },
            },
          },
        }),
      }),
      makeItem({
        id: 'complete-wait-review',
        threadId: 'thread-a',
        itemIndex: 68,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        summary: 'wait_agent',
      }),
      makeItem({
        id: 'complete-spawn-review',
        threadId: 'thread-a',
        itemIndex: 69,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        summary: 'collab_agent: review -> done',
        meta: JSON.stringify({ wait_carrier_id: 'wait-review', item_status: 'completed' }),
      }),
      makeItem({
        id: 'assistant-after-review',
        threadId: 'thread-a',
        itemIndex: 70,
        kind: 'assistant_text',
        summary: 'The review caught one edge I agree with.',
      }),
    ];

    for (const item of items) {
      emitWailsEvent('provider:item_event', {
        action: 'upsert',
        threadId: item.threadId,
        item,
      });
    }

    expect(pane.items).toEqual([]);
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual([
      'assistant-before-review',
      'wait-review',
      'complete-wait-review',
      'complete-spawn-review',
      'assistant-after-review',
    ]);
  });

  it('flushes a queued item_event batch on listener cleanup', async () => {
    const originalRAF = globalThis.requestAnimationFrame;
    const originalCancelRAF = globalThis.cancelAnimationFrame;
    vi.useFakeTimers();
    vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    try {
      const pane = await buildPane();

      const item = makeItem({ id: 'cancelled-flush', kind: 'terminal_interaction' });
      emitWailsEvent('provider:item_event', {
        action: 'upsert',
        threadId: item.threadId,
        item,
      });
      cleanup();
      cleanup = () => {};

      await vi.advanceTimersByTimeAsync(60);

      expect(pane.items.map((entry) => entry.id)).toEqual(['cancelled-flush']);
    } finally {
      vi.useRealTimers();
      vi.stubGlobal('requestAnimationFrame', originalRAF);
      vi.stubGlobal('cancelAnimationFrame', originalCancelRAF);
    }
  });

  it('applies same-frame upsert bursts as one timeline revision', async () => {
    const pane = await buildPane();
    const revisionBeforeBurst = pane.timelineRevision;

    const first = makeItem({ id: 'wait-1', kind: 'terminal_interaction', itemIndex: 2 });
    const second = makeItem({ id: 'wait-2', kind: 'terminal_interaction', itemIndex: 1 });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: first.threadId, item: first });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: second.threadId, item: second });

    expect(pane.items).toEqual([]);
    await nextFrame();

    expect(pane.items.map((item) => item.id)).toEqual(['wait-2', 'wait-1']);
    expect(pane.timelineRevision).toBe(revisionBeforeBurst + 1);
  });

  it('ignores stale item_event deltas after the item has completed', async () => {
    const pane = await buildPane();

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
    pane.__flushItemSmoothersForTest();
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
    pane.__flushItemSmoothersForTest();

    expect(pane.items.find((item) => item.id === 'text-1')?.summary).toBe('yield timeouts');
  });

  it('coalesces contiguous deltas without crossing upsert boundaries', async () => {
    const pane = await buildPane();

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
    pane.__flushItemSmoothersForTest();

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

  it('applies item_event meta to the matching row and replaces by reference', async () => {
    const pane = await buildPane();
    const base = makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'see src/foo.ts and more',
      meta: '',
    });
    pane.upsertItem(base);
    const before = pane.items.find((item) => item.id === 'text-1');
    expect(before).toBeDefined();

    const metaJson = '{"pathRefs":[{"path":"src/foo.ts"}]}';
    emitWailsEvent('provider:item_event', {
      action: 'meta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      meta: metaJson,
      updatedAt: 5,
    });
    await nextFrame();

    const after = pane.items.find((item) => item.id === 'text-1');
    expect(after?.meta).toBe(metaJson);
    // ChatMarkdown derives its path-link extension from item.meta and
    // re-runs only when the reference changes — mutating in place would
    // not trigger the $derived rebuild and links would stay raw until
    // the next upsert.
    expect(after).not.toBe(before);
    // The producer (UpdateItemMeta) intentionally does not bump
    // updated_at; the frontend must preserve it so the size priors + thread
    // cache don't treat this re-render as a content change.
    expect(after?.updatedAt).toBe(before?.updatedAt);
    expect(after?.summary).toBe(before?.summary);
  });

  it('flushes pending deltas before applying item_event meta', async () => {
    const pane = await buildPane();
    pane.upsertItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'see src/foo.ts',
      meta: '',
    }));

    // Same-batch delta + meta. The meta must land against the appended
    // text the user has already seen, so deltas have to flush FIRST.
    // End-state assertions (summary + meta) can't discriminate ordering
    // because `applyItemDelta` and `applyItemMeta` both spread from the
    // current row and preserve non-target fields — flipping their call
    // order produces the same final summary/meta. Spy on both methods
    // so the test asserts the actual call sequence the flush contract
    // promises, not just an incidentally-equivalent end state.
    const deltaSpy = vi.spyOn(pane, 'applyItemDelta');
    const metaSpy = vi.spyOn(pane, 'applyItemMeta');

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: ' padding',
      updatedAt: 4,
    });
    emitWailsEvent('provider:item_event', {
      action: 'meta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      meta: '{"pathRefs":[{"path":"src/foo.ts"}]}',
      updatedAt: 5,
    });
    await nextFrame();
    pane.__flushItemSmoothersForTest();

    expect(deltaSpy).toHaveBeenCalledOnce();
    expect(metaSpy).toHaveBeenCalledOnce();
    // Vitest exposes invocationCallOrder on every spy; it's a monotonic
    // counter across all spies. Asserting delta < meta proves the
    // flush loop ran the pending delta queue before applying the meta.
    expect(deltaSpy.mock.invocationCallOrder[0]).toBeLessThan(
      metaSpy.mock.invocationCallOrder[0],
    );

    const row = pane.items.find((item) => item.id === 'text-1');
    expect(row?.summary).toBe('see src/foo.ts padding');
    expect(row?.meta).toBe('{"pathRefs":[{"path":"src/foo.ts"}]}');
  });

  it('flushes pending upserts in the same batch before applying meta', async () => {
    const pane = await buildPane();

    // No prior upsert via pane.upsertItem — the row is created by the
    // queued upsert in the same batch as the meta. Both arrive in one
    // microtask; the meta event references the new row by id, so the
    // upsert MUST flush before the meta or applyItemMeta finds no
    // index and drops silently.
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'thread-1',
      item: makeItem({
        id: 'text-1',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'see src/foo.ts',
        meta: '',
      }),
    });
    emitWailsEvent('provider:item_event', {
      action: 'meta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      meta: '{"pathRefs":[{"path":"src/foo.ts"}]}',
      updatedAt: 5,
    });
    await nextFrame();

    const row = pane.items.find((item) => item.id === 'text-1');
    expect(row?.meta).toBe('{"pathRefs":[{"path":"src/foo.ts"}]}');
  });

  it('ignores item_event meta for a thread without a matching pane', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    pane.upsertItem(makeItem({
      id: 'text-1',
      threadId: 'thread-a',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'stable',
      meta: '',
    }));

    emitWailsEvent('provider:item_event', {
      action: 'meta',
      threadId: 'thread-other',
      itemId: 'text-1',
      kind: 'assistant_text',
      meta: '{"pathRefs":[{"path":"src/foo.ts"}]}',
      updatedAt: 5,
    });
    await nextFrame();

    expect(pane.items.find((item) => item.id === 'text-1')?.meta).toBe('');
  });

  it('drops item_event meta payloads that fail validation', async () => {
    const pane = await buildPane();
    pane.upsertItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'stable',
      meta: '',
    }));

    emitWailsEvent('provider:item_event', {
      action: 'meta',
      threadId: 'thread-1',
      itemId: 'text-1',
      kind: 'assistant_text',
      // meta intentionally non-string to trip isBoundedString
      meta: { pathRefs: [{ path: 'src/foo.ts' }] },
      updatedAt: 5,
    });
    await nextFrame();

    expect(pane.items.find((item) => item.id === 'text-1')?.meta).toBe('');
  });

  it('drops item_event payloads with unknown actions', async () => {
    const pane = await buildPane();
    pane.upsertItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'stable',
    }));

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
    await refreshThreads();

    emitWailsEvent('thread:updated', {
      action: 'full',
      thread: makeThread({
        id: 'thread-1',
        title: 'New title',
        model: 'claude-opus-4-1',
        lastReadAt: 100,
        latestTurnCompletedAt: 100,
      }),
    });

    expect(pane.thread?.title).toBe('New title');
    expect(pane.thread?.model).toBe('claude-opus-4-1');
    expect(pane.thread?.lastReadAt).toBe(300);
    expect(pane.thread?.latestTurnCompletedAt).toBe(300);
    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.model).toBe('claude-opus-4-1');
    expect(getThreads()[0]?.lastReadAt).toBe(300);
    expect(getThreads()[0]?.latestTurnCompletedAt).toBe(300);
  });

  it('merges a sessionRef patch into the sidebar row and pane copies', async () => {
    // A thread created mid-run has no sessionRef until the provider's
    // system/init assigns one; the backend announces the assignment as
    // a thread:updated patch. The sidebar row is what gates the Fork
    // context-menu item, so both copies must pick it up.
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', title: 'Fresh' }),
    ]);
    await refreshThreads();
    const pane = await buildPane(makeThread({ id: 'thread-1', title: 'Fresh' }));
    expect(getThreads()[0]?.sessionRef).toBeUndefined();

    emitWailsEvent('thread:updated', {
      action: 'patch',
      id: 'thread-1',
      sessionRef: 'session-abc',
    });

    expect(getThreads()[0]?.sessionRef).toBe('session-abc');
    expect(pane.thread?.sessionRef).toBe('session-abc');
    expect(getThreads()[0]?.title).toBe('Fresh');
  });

  // The convergence half of "every persisted thread-row mutation emits".
  // These four exercise the case the RPC return value cannot reach: a
  // client that did NOT issue the mutation. Each drives a row this pane
  // never touched and asserts the store lands where the mutating client's
  // own optimistic apply would have put it.
  describe('thread:updated converges a client that did not mutate', () => {
    it("applies every field a full row carries, not just the mutating client's field", async () => {
      setBindingMock('ListThreads', async () => [
        makeThread({
          id: 'thread-1',
          title: 'Before',
          model: 'claude-sonnet-4-6',
          reasoningEffort: 'medium',
          fastMode: false,
          contextWindow: 200000,
          branch: 'main',
          workspacePath: '/repo',
        }),
      ]);
      await refreshThreads();

      emitWailsEvent('thread:updated', {
        action: 'full',
        thread: makeThread({
          id: 'thread-1',
          title: 'After',
          model: 'claude-opus-4-1',
          reasoningEffort: 'xhigh',
          fastMode: true,
          contextWindow: 1000000,
          branch: 'feature/live',
          workspacePath: '/repo/wt',
          pinnedAt: 42,
          pinGroup: 1,
        }),
      });

      const row = getThreads()[0];
      expect(row?.title).toBe('After');
      expect(row?.model).toBe('claude-opus-4-1');
      expect(row?.reasoningEffort).toBe('xhigh');
      expect(row?.fastMode).toBe(true);
      expect(row?.contextWindow).toBe(1000000);
      expect(row?.branch).toBe('feature/live');
      expect(row?.workspacePath).toBe('/repo/wt');
      expect(row?.pinnedAt).toBe(42);
      expect(row?.pinGroup).toBe(1);
    });

    it('inserts a row this client has never seen from a listed event', async () => {
      setBindingMock('ListThreads', async () => [makeThread({ id: 'thread-1' })]);
      await refreshThreads();
      expect(getThreads().map((t) => t.id)).toEqual(['thread-1']);

      emitWailsEvent('thread:updated', {
        action: 'listed',
        thread: makeThread({ id: 'thread-new', title: 'Made elsewhere' }),
      });

      expect(getThreads().map((t) => t.id)).toEqual(['thread-new', 'thread-1']);
      expect(getThreads()[0]?.title).toBe('Made elsewhere');

      // A second listed frame for the same row (this client's own echo of
      // a creation it already prepended) must not duplicate it.
      emitWailsEvent('thread:updated', {
        action: 'listed',
        thread: makeThread({ id: 'thread-new', title: 'Made elsewhere' }),
      });
      expect(getThreads().map((t) => t.id)).toEqual(['thread-new', 'thread-1']);
    });

    it('drops the row from the sidebar on unlisted and on deleted', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread({ id: 'thread-1' }),
        makeThread({ id: 'thread-2' }),
      ]);
      await refreshThreads();

      emitWailsEvent('thread:updated', {
        action: 'unlisted',
        thread: makeThread({ id: 'thread-1', archived: true }),
      });
      expect(getThreads().map((t) => t.id)).toEqual(['thread-2']);

      emitWailsEvent('thread:updated', { action: 'deleted', id: 'thread-2' });
      expect(getThreads()).toEqual([]);
    });

    it('closes a pane whose thread was deleted elsewhere', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1' }));
      expect(pane.threadId).toBe('thread-1');

      emitWailsEvent('thread:updated', { action: 'deleted', id: 'thread-1' });

      expect(findPaneShowingThread('thread-1')).toBeNull();
    });

    it('leaves a full row for an unknown thread alone', async () => {
      setBindingMock('ListThreads', async () => [makeThread({ id: 'thread-1' })]);
      await refreshThreads();

      // 'full' says what the row IS, never that it belongs in the sidebar:
      // listing also depends on items and draft content, which only the
      // backend knows. Inventing the row here would show a draft the
      // mutating client's own sidebar does not.
      emitWailsEvent('thread:updated', {
        action: 'full',
        thread: makeThread({ id: 'thread-unknown' }),
      });

      expect(getThreads().map((t) => t.id)).toEqual(['thread-1']);
    });
  });

  it('projects a model fallback without overwriting the requested model', async () => {
    const pane = await buildPane(makeThread({
      id: 'thread-1',
      provider: 'claude',
      model: 'claude-fable-5',
    }));

    emitWailsEvent('provider:model_fallback', {
      threadId: 'thread-1',
      requestedModel: 'claude-fable-5',
      effectiveModel: 'claude-opus-4-8',
      reason: 'Fable safeguards flagged this message.',
      category: 'cyber',
      revision: 1,
    });

    expect(pane.thread?.model).toBe('claude-fable-5');
    expect(pane.activeModel).toBe('claude-opus-4-8');

    emitWailsEvent('provider:model_fallback', { threadId: 'thread-1', revision: 2 });
    expect(pane.activeModel).toBe('claude-fable-5');

    emitWailsEvent('provider:model_fallback', {
      threadId: 'thread-1',
      effectiveModel: 'claude-opus-4-8',
      revision: 1,
    });
    expect(pane.activeModel).toBe('claude-fable-5');
  });

  it('patches the sidebar row and every matching pane from thread:mode_changed, and toasts when needsReconnect', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', mode: 'chat' }),
    ]);
    await refreshThreads();
    const pane = await buildPane(makeThread({ id: 'thread-1', mode: 'chat' }));

    emitWailsEvent('thread:mode_changed', {
      threadId: 'thread-1',
      mode: 'plan',
      needsReconnect: true,
    });

    expect(getThreads()[0]?.mode).toBe('plan');
    expect(pane.thread?.mode).toBe('plan');
    expect(getToasts().some((toast) => toast.type === 'warning' && toast.message.includes('plan'))).toBe(true);
  });

  it('patches without a toast from thread:mode_changed when needsReconnect is false', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', mode: 'chat' }),
    ]);
    await refreshThreads();
    const pane = await buildPane(makeThread({ id: 'thread-1', mode: 'chat' }));
    const toastCountBefore = getToasts().length;

    emitWailsEvent('thread:mode_changed', {
      threadId: 'thread-1',
      mode: 'plan',
      needsReconnect: false,
    });

    expect(getThreads()[0]?.mode).toBe('plan');
    expect(pane.thread?.mode).toBe('plan');
    expect(getToasts().length).toBe(toastCountBefore);
  });

  it('patches the sidebar row and every matching pane from thread:runtime_mode_changed', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', runtimeMode: 'approval-required' }),
    ]);
    await refreshThreads();
    const paneA = await buildPane(makeThread({ id: 'thread-1', runtimeMode: 'approval-required' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-2', runtimeMode: 'approval-required' }), [], 'b');

    emitWailsEvent('thread:runtime_mode_changed', {
      threadId: 'thread-1',
      runtimeMode: 'full-access',
      needsReconnect: false,
    });

    expect(getThreads()[0]?.runtimeMode).toBe('full-access');
    expect(paneA.thread?.runtimeMode).toBe('full-access');
    expect(paneB.thread?.runtimeMode).toBe('approval-required');
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
    expect(liveThreadActivity('thread-stale')).toBe(100);
    expect(pane.thread?.updatedAt).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(100);
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
    expect(liveThreadActivity('thread-stale')).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(100);
  });

  // The user_text sidebar bump rides a thread:updated PATCH carrying
  // `updatedAt` and nothing else. Which user_text persists produce one is
  // the backend's call (triage.userTextCountsAsThreadActivity: top-level,
  // not wire-only) — wire-only and subagent-parented rows simply never
  // emit a patch, so the frontend has no predicate left to get wrong.
  it('bumps cached project activity from a thread:updated activity patch', async () => {
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

    emitWailsEvent('thread:updated', {
      action: 'patch',
      id: 'thread-stale',
      updatedAt: 10_000,
    });

    expect(liveThreadActivity('thread-stale')).toBe(10_000);
    // Neither the row array nor pane.thread is replaced by an activity
    // beat — per-beat object churn re-rendered every pane.thread reader.
    expect(pane.thread?.updatedAt).toBe(100);
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.updatedAt).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(10_000);
  });

  it('does NOT bump cached project activity from user_text item upserts', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    setBindingMock('ListProjects', async () => [
      projectWithCounts('project-stale', 100),
    ]);
    await refreshThreads();
    await refreshProjects();

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

    expect(liveThreadActivity('thread-stale')).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(100);
  });

  it('clears an error badge from a thread:updated activity patch', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-stale', projectId: 'project-stale', updatedAt: 100 }),
    ]);
    await refreshThreads();
    emitWailsEvent('thread:error_notice', { threadId: 'thread-stale', itemId: 'error-1' });
    expect(getThreadStatus('thread-stale')).toBe('error');

    emitWailsEvent('thread:updated', {
      action: 'patch',
      id: 'thread-stale',
      updatedAt: 10_000,
    });

    expect(getThreadStatus('thread-stale')).toBe('idle');
  });

  it('applies an activity patch for a thread this client holds no row for', async () => {
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    emitWailsEvent('thread:error_notice', { threadId: 'thread-unlisted', itemId: 'error-1' });

    // The patch branch's cached-row guard covers the FIELD merges only:
    // the activity bump and the badge clear both self-guard and must run
    // for a thread the sidebar has not listed.
    emitWailsEvent('thread:updated', {
      action: 'patch',
      id: 'thread-unlisted',
      updatedAt: 10_000,
      title: 'ignored without a row',
    });

    expect(getThreadStatus('thread-unlisted')).toBe('idle');
    expect(getThreads().find((thread) => thread.id === 'thread-unlisted')).toBeUndefined();
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

    expect(liveThreadActivity('thread-stale')).toBe(12_000);
    expect(liveProjectActivity('project-stale')).toBe(12_000);
  });

  it('does NOT bump cached project activity when provider:turn_completed is internal', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({
        id: 'thread-stale',
        projectId: 'project-stale',
        updatedAt: 100,
        latestTurnCompletedAt: 100,
      }),
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
      countsAsActivity: false,
    });
    await nextFrame();

    expect(liveThreadActivity('thread-stale')).toBe(100);
    expect(getThreads().find((thread) => thread.id === 'thread-stale')?.latestTurnCompletedAt).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(100);
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
    await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));

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
    expect(liveThreadActivity('thread-stale')).toBe(requestedAt);
    expect(liveProjectActivity('project-stale')).toBe(requestedAt);
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
    await buildPane(makeThread({
      id: 'thread-stale',
      projectId: 'project-stale',
      updatedAt: 100,
    }));

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
    expect(liveThreadActivity('thread-stale')).toBe(requestedAt);
    expect(liveProjectActivity('project-stale')).toBe(requestedAt);
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
    expect(liveThreadActivity('thread-stale')).toBe(100);
    expect(liveProjectActivity('project-stale')).toBe(100);
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

    emitWailsEvent('thread:updated', {
      action: 'full',
      thread: makeThread({
        id: 'thread-1',
        projectId: 'project-1',
        title: 'New title',
        updatedAt: 100,
      }),
    });

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

    expect(liveThreadActivity('thread-stale')).toBe(10_000);
    expect(liveProjectActivity('project-stale')).toBe(10_000);
  });

  it('refreshes the context meter via GetThread after a provider:usage transport gap', async () => {
    // Fix 7: provider:usage events run through the meter pipeline, not
    // refreshFromBackend, so a gap that drops a usage event leaves the
    // meter stale forever without an explicit re-fetch. The handler must
    // call GetThread per affected pane and re-seed `lastTokenUsage`.
    const stale = makeThread({
      id: 'thread-meter',
      provider: 'codex',
      contextWindow: 200000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 50000,
        maxTokens: 200000,
        contextPercent: 25,
      }),
    });
    const pane = await buildPane(stale);
    expect(pane.contextWindow?.usedTokens).toBe(50000);

    setBindingMock('GetThread', async (threadId: unknown) => {
      if (threadId !== 'thread-meter') return null;
      return {
        ...stale,
        lastTokenUsage: JSON.stringify({
          usedTokens: 175000,
          maxTokens: 200000,
          contextPercent: 87.5,
        }),
      };
    });
    setBindingMock('GetRateLimitsSnapshots', async () => [{
      provider: 'codex',
      limits: [
        { limitId: 'codex', limitName: '', usedPercent: 47, windowMins: 300, resetsAt: 4102444800 },
        { limitId: 'codex', limitName: '', usedPercent: 28, windowMins: 10080, resetsAt: 4103049600 },
      ],
      updatedAt: 1783629000000,
    }]);

    emitWailsEvent(transportGapChannel, {
      channel: 'provider:usage',
      seq: 7,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.contextWindow?.usedTokens).toBe(175000);
    expect(pane.contextWindow?.usedPercentage).toBe(87.5);
    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(47);
    expect(getProviderRateLimit('codex', 10080)?.usedPercent).toBe(28);
  });

  it('does not revert local read state when a provider:usage gap refreshes the thread row', async () => {
    const local = makeThread({
      id: 'thread-usage-read',
      lastReadAt: 2_000,
      latestTurnCompletedAt: 1_000,
    });
    const pane = await buildPane(local);
    const freshUsage = JSON.stringify({ usedTokens: 1_000, maxTokens: 200_000, contextPercent: 0.5 });
    // The re-fetched row's job is lastTokenUsage; its read marker can
    // predate the debounced MarkThreadRead persist and must merge
    // forward, not revert the pane copy.
    setBindingMock('GetThread', async () => ({
      ...local,
      lastReadAt: 500,
      lastTokenUsage: freshUsage,
    }));

    emitWailsEvent(transportGapChannel, {
      channel: 'provider:usage',
      seq: 8,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.thread?.lastTokenUsage).toBe(freshUsage);
    expect(pane.thread?.lastReadAt).toBe(2_000);
  });

  it('preserves a newer local read marker across a transport-gap thread resync', async () => {
    // ChatView marks a thread read locally and debounces the
    // MarkThreadRead persist; a gap-triggered ListThreads snapshot can
    // be read before that persist lands. Replacing rows verbatim would
    // revert lastReadAt and resurrect a "Completed" pill the focused
    // pane already cleared — and could never clear again, because the
    // read-mark effect keys off the (still-read) pane copy.
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', lastReadAt: 2_000, latestTurnCompletedAt: 1_000 }),
    ]);
    await refreshThreads();

    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', lastReadAt: 500, latestTurnCompletedAt: 1_000 }),
    ]);
    emitWailsEvent(transportGapChannel, {
      channel: 'thread:updated',
      seq: 3,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(getThreads()[0]?.lastReadAt).toBe(2_000);
    expect(getThreads()[0]?.latestTurnCompletedAt).toBe(1_000);
  });

  it('preserves an explicit unread marker across a transport-gap thread resync', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', lastReadAt: 0, latestTurnCompletedAt: 300 }),
    ]);
    await refreshThreads();

    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', lastReadAt: 500, latestTurnCompletedAt: 300 }),
    ]);
    emitWailsEvent(transportGapChannel, {
      channel: 'thread:updated',
      seq: 4,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(getThreads()[0]?.lastReadAt).toBe(0);
  });

  it('converges pane thread rows on completions backfilled by a transport-gap resync', async () => {
    // The final turn_completed fell into the gap: the backend snapshot
    // carries the completion but no pane ever saw the event. The resync
    // must fan the merged row out to panes — ChatView's read-mark
    // effect keys off pane.thread, so a pane left on the stale copy
    // can never clear the sidebar "Completed" pill.
    const stale = makeThread({
      id: 'thread-gap',
      lastReadAt: 350,
      latestTurnCompletedAt: 300,
    });
    const pane = await buildPane(stale);
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-gap', lastReadAt: 350, latestTurnCompletedAt: 900 }),
    ]);

    emitWailsEvent(transportGapChannel, {
      channel: 'provider:turn_completed',
      seq: 11,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(getThreads().find((t) => t.id === 'thread-gap')?.latestTurnCompletedAt).toBe(900);
    expect(pane.thread?.latestTurnCompletedAt).toBe(900);
    expect(pane.thread?.lastReadAt).toBe(350);
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

    emitWailsEvent('thread:updated', {
      action: 'full',
      thread: makeThread({
        id: 'thread-1',
        title: 'New title',
        lastReadAt: 300,
        latestTurnCompletedAt: 300,
      }),
    });

    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.lastReadAt).toBe(0);
  });

  it('updates global provider status from app-wide provider:status', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));

    emitWailsEvent('provider:status', providerStatusEvent({
      status: 'unauthenticated',
      message: 'Claude not authenticated',
    }));

    expect(pane.providerBanner).toBeUndefined();
    expect(getProviderStatus('claude')?.status).toBe('unauthenticated');

    emitWailsEvent('provider:status', providerStatusEvent({ status: 'ready', actionable: false }));
    expect(pane.providerBanner).toBeUndefined();
    expect(getProviderStatus('claude')?.status).toBe('ready');
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

  it('clears selected account state from a provider:account removal event', async () => {
    const accountInfo = await import('./accountInfo.svelte');
    accountInfo.resetForTest();
    accountInfo.setProviderAccount('claude', { subscriptionType: 'Claude Max' }, 'account-one');

    emitWailsEvent('provider:account', {
      provider: 'claude',
      account: {},
      cleared: true,
    });

    expect(accountInfo.getProviderAccount('claude')).toBeNull();
    accountInfo.resetForTest();
  });

  it('clears retired account limits from provider:usage', async () => {
    const rateLimits = await import('./rateLimitsInfo.svelte');
    rateLimits.resetForTest();
    rateLimits.setProviderRateLimits({
      provider: 'codex',
      accountId: 'retired',
      limits: [{
        limitId: 'codex',
        limitName: 'All models',
        usedPercent: 80,
        windowMins: 300,
        resetsAt: 4102444800,
      }],
      updatedAt: 1776283000,
    });

    emitWailsEvent('provider:usage', {
      action: 'rate_limits_removed',
      rateLimits: {
        provider: 'codex',
        accountId: 'retired',
        limits: [],
        updatedAt: 0,
      },
    });

    expect(rateLimits.getProviderRateLimits('codex', 'retired')).toEqual([]);
    rateLimits.resetForTest();
  });

  it('routes provider session identity only to the matching thread pane', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a', provider: 'codex' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b', provider: 'codex' }), [], 'b');

    emitWailsEvent('provider:session_account', {
      threadId: 'thread-a',
      provider: 'codex',
      accountId: 'account-old',
      account: { email: 'old@example.com', subscriptionType: 'pro' },
      connected: true,
    });

    expect(paneA.providerSessionAccount?.account.email).toBe('old@example.com');
    expect(paneB.providerSessionAccount).toBeNull();

    emitWailsEvent('provider:session_account', {
      threadId: 'thread-a',
      provider: 'codex',
      account: {},
      connected: false,
    });

    expect(paneA.providerSessionAccount).toBeNull();
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

  it('mid-turn usage events update the meter without rewriting the thread row', async () => {
    const thread = makeThread({ id: 'thread-1', contextWindow: 200000 });
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);

    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 60000,
      maxTokens: 200000,
      contextPercent: 30,
    });

    // Live meter at full cadence…
    expect(pane.contextWindow?.usedTokens).toBe(60000);
    // …but no per-event sidebar-row / pane.thread churn: the snapshot
    // sits in the side cache until the turn-completion flush.
    expect(getThreads().find((t) => t.id === 'thread-1')?.lastTokenUsage).toBeUndefined();
    expect(pane.thread?.lastTokenUsage).toBeUndefined();

    // A pane seeding the same thread mid-turn (thread switch) must get
    // the cached snapshot, not the stale row value.
    const pane2 = await buildPane(thread, [], 'secondary');
    expect(pane2.contextWindow?.usedTokens).toBe(60000);
    expect(pane2.contextWindow?.usedPercentage).toBe(30);
  });

  it('flushes the cached usage snapshot into the thread row at turn completion', async () => {
    const thread = makeThread({ id: 'thread-1', contextWindow: 200000 });
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);

    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 60000,
      maxTokens: 200000,
      contextPercent: 30,
    });
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
    });

    const persisted = JSON.parse(pane.thread?.lastTokenUsage ?? '{}') as { usedTokens?: number };
    expect(persisted.usedTokens).toBe(60000);
    const row = getThreads().find((t) => t.id === 'thread-1');
    expect(JSON.parse(row?.lastTokenUsage ?? '{}')).toMatchObject({ usedTokens: 60000 });
    // The row-driven re-seed keeps the meter on the same values.
    expect(pane.contextWindow?.usedTokens).toBe(60000);
  });

  it('a stale mid-turn usage snapshot cannot shadow provider:usage gap recovery', async () => {
    const stale = makeThread({
      id: 'thread-gap-shadow',
      provider: 'codex',
      contextWindow: 200000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 50000,
        maxTokens: 200000,
        contextPercent: 25,
      }),
    });
    const pane = await buildPane(stale);

    // A live usage event lands before the gap — the side cache now holds
    // a pre-gap snapshot.
    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-gap-shadow',
      usedTokens: 60000,
      maxTokens: 200000,
      contextPercent: 30,
    });
    expect(pane.contextWindow?.usedTokens).toBe(60000);

    // Gap recovery re-reads the persisted row; the DB value must win
    // over the cached pre-gap snapshot.
    setBindingMock('GetThread', async (threadId: unknown) => {
      if (threadId !== 'thread-gap-shadow') return null;
      return {
        ...stale,
        lastTokenUsage: JSON.stringify({
          usedTokens: 175000,
          maxTokens: 200000,
          contextPercent: 87.5,
        }),
      };
    });
    emitWailsEvent(transportGapChannel, {
      channel: 'provider:usage',
      seq: 9,
    });
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.contextWindow?.usedTokens).toBe(175000);
    expect(pane.contextWindow?.usedPercentage).toBe(87.5);
  });

  it('a pane.thread replacement mid-turn re-seeds the meter from the side cache, not the stale row', async () => {
    const thread = makeThread({
      id: 'thread-1',
      contextWindow: 200000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 10000,
        maxTokens: 200000,
        contextPercent: 5,
      }),
    });
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    expect(pane.contextWindow?.usedTokens).toBe(10000);

    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 60000,
      maxTokens: 200000,
      contextPercent: 30,
    });
    expect(pane.contextWindow?.usedTokens).toBe(60000);

    // A mode-change patch replaces pane.thread mid-turn (row still holds
    // the boot-time usage). The re-seed must keep the fresher cached
    // snapshot instead of rewinding the meter to 10000.
    emitWailsEvent('thread:mode_changed', {
      threadId: 'thread-1',
      mode: 'plan',
      needsReconnect: false,
    });
    expect(pane.thread?.mode).toBe('plan');
    expect(pane.contextWindow?.usedTokens).toBe(60000);
  });

  // Chat-rewrite routing: EventRateLimits folds onto provider:usage
  // via `action: 'rate_limits'`. The listener must NOT treat this as a
  // reset — the last-seen context-window ring stays in place — and it
  // MUST land the snapshot in the provider-keyed global store
  // (`rateLimitsInfo.svelte.ts`).
  it('routes EventRateLimits to the provider-global store without clobbering the context ring', async () => {
    const pane = await buildPane();

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
          { limitId: 'five_hour', limitName: '5h', usedPercent: 62.5, windowMins: 300, resetsAt: 4102444800 },
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
    expect(fiveHour?.resetsAt).toBe(4102444800);
  });

  // Claude emits one window per `rate_limit_event` (5h XOR 7d). A
  // subsequent event for the OTHER window must merge into the same
  // provider slot, not replace it. Codex emits both together; we test
  // the harder Claude case here because it's the merge-correctness pin.
  it('merges Claude single-window updates without clobbering the other window', async () => {
    await buildPane();

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 4102444800 },
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
          { limitId: 'seven_day', limitName: '7d', usedPercent: 51, windowMins: 10080, resetsAt: 4103049600 },
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
  it('keeps unknown-duration limits out of the toolbar lookup', async () => {
    await buildPane();

    emitWailsEvent('provider:usage', {
      action: 'rate_limits',
      threadId: 'thread-1',
      rateLimits: {
        provider: 'claude',
        limits: [
          { limitId: 'thirty_day', limitName: 'thirty_day', usedPercent: 10, windowMins: 0, resetsAt: 4102444800 },
          { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 4102444800 },
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
          { limitId: 'five_hour', limitName: '5h', usedPercent: 73, windowMins: 300, resetsAt: 4102444800 },
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
          { limitId: 'codex', limitName: '5h', usedPercent: 88, windowMins: 300, resetsAt: 4102444800 },
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
          { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 4102444800 },
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

  // `binary_stale` is thread-scoped: the CLI was replaced under THIS
  // session. It has no legacy binary-detect counterpart, so the kind maps
  // onto itself and must survive the unknown-kind drop.
  it('carries binary_stale, with its versions, to the matching pane only', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    const other = await buildPane(
      makeThread({ id: 'thread-2', provider: 'claude' }),
      [],
      'second',
    );

    emitWailsEvent('provider:status', {
      kind: 'binary_stale',
      provider: 'claude',
      threadId: 'thread-1',
      sessionVersion: '2.1.219',
      installedVersion: '2.1.257',
    } as unknown as ProviderStatusEvent);

    expect(pane.providerBanner?.status).toBe('binary_stale');
    expect(pane.providerBanner?.sessionVersion).toBe('2.1.219');
    expect(pane.providerBanner?.installedVersion).toBe('2.1.257');
    // A second pane on the same provider is running its own session on its
    // own binary — a thread-scoped status must not leak into it.
    expect(other.providerBanner).toBeUndefined();
    // Nor into the provider-global cache the next thread switch seeds from.
    expect(getProviderStatus('claude')?.status).not.toBe('binary_stale');
  });

  // The withdrawal speaks the legacy vocabulary: a thread-scoped
  // `status: 'ready'` with no kind. It withdraws the pane's own banner
  // (undefined) rather than pinning it empty (null), so the pane falls
  // back to the provider-global status like every unflagged pane.
  it('clears a thread-scoped banner on a ready event for that thread', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    emitWailsEvent('provider:status', {
      kind: 'binary_stale',
      provider: 'claude',
      threadId: 'thread-1',
      installedVersion: '2.1.257',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('binary_stale');

    emitWailsEvent('provider:status', {
      provider: 'claude',
      status: 'ready',
      threadId: 'thread-1',
    } as unknown as ProviderStatusEvent);

    expect(pane.providerBanner).toBeUndefined();
  });

  // The banner claims a RUNNING session is pinned to the old binary. Once
  // the session is gone the claim is false however it ended, and no ready
  // event is owed for a session that no longer exists.
  it('clears binary_stale when the session disconnects', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    emitWailsEvent('provider:status', {
      kind: 'binary_stale',
      provider: 'claude',
      threadId: 'thread-1',
      installedVersion: '2.1.257',
    } as unknown as ProviderStatusEvent);

    emitWailsEvent('provider:session_account', {
      provider: 'claude',
      threadId: 'thread-1',
      connected: false,
      account: {},
    });

    expect(pane.providerBanner).toBeUndefined();
  });

  // A withdrawn thread-scoped banner must not hide the provider-global one:
  // the pane falls back to what every other pane on that provider shows.
  it('falls back to the provider-global banner after a thread-scoped clear', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    emitWailsEvent('provider:status', {
      kind: 'binary_stale',
      provider: 'claude',
      threadId: 'thread-1',
      installedVersion: '2.1.257',
    } as unknown as ProviderStatusEvent);
    emitWailsEvent('provider:status', {
      kind: 'unauthenticated',
      provider: 'claude',
      message: 'Re-authenticate',
    } as unknown as ProviderStatusEvent);
    expect(pane.providerBanner?.status).toBe('binary_stale');

    emitWailsEvent('provider:status', {
      provider: 'claude',
      status: 'ready',
      threadId: 'thread-1',
    } as unknown as ProviderStatusEvent);

    expect(pane.providerBanner).toBeUndefined();
    expect(getProviderStatus('claude')?.status).toBe('unauthenticated');
  });

  it('leaves other banner kinds alone when the session disconnects', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    emitWailsEvent('provider:status', {
      kind: 'unauthenticated',
      provider: 'claude',
      threadId: 'thread-1',
      message: 'Re-authenticate',
    } as unknown as ProviderStatusEvent);

    emitWailsEvent('provider:session_account', {
      provider: 'claude',
      threadId: 'thread-1',
      connected: false,
      account: {},
    });

    expect(pane.providerBanner?.status).toBe('unauthenticated');
  });

  it('session death clears pending-send bridge state before turn start', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
    projectSendStarted('thread-1');

    emitWailsEvent('provider:session_died', {
      threadId: 'thread-1',
      exitCode: 1,
      reason: 'provider exited before turn start',
    });

    expect(hasPendingSend('thread-1')).toBe(false);
    expect(getThreadStatus('thread-1')).toBe('error');
    expect(pane.generalError).toBe('provider exited before turn start');
    expect(pane.generalErrorKind).toBe('session');
  });

  it('a fresh turn_started clears a stale session-death banner for the same thread', async () => {
    // Simulates the recovery UX: provider died → banner set →
    // user sends a new message → auto-reconnect (or lazy-start) →
    // turn_started arrives. The banner must auto-dismiss because
    // a wire-level turn-start is unambiguous proof the session is
    // alive again.
    const pane = await buildPane(makeThread({ id: 'thread-recovery', provider: 'claude' }));

    emitWailsEvent('provider:session_died', {
      threadId: 'thread-recovery',
      exitCode: 1,
      reason: 'session died on us',
    });
    expect(pane.generalError).toBe('session died on us');
    expect(pane.generalErrorKind).toBe('session');

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-recovery',
      turnId: 'turn-recover',
      turnIndex: 0,
      startedAt: 1234,
    });

    expect(pane.generalError).toBeNull();
    expect(pane.generalErrorKind).toBeNull();
  });

  it('turn_started preserves orthogonal generalError (e.g. failed rename) that shares the slot', async () => {
    // The grab-bag generalError slot is used by ~16 surfaces beyond
    // session-death (failed rename, git status, thread load, builtin
    // commands, etc). A new turn invalidates session errors but does
    // NOT invalidate those — a rename that failed is still a failure
    // even after the next prompt produces a turn.
    const pane = await buildPane(makeThread({ id: 'thread-other-err', provider: 'claude' }));

    pane.setGeneralError('Failed to rename thread');
    expect(pane.generalErrorKind).toBeNull();

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-other-err',
      turnId: 'turn-noclear',
      turnIndex: 0,
      startedAt: 1234,
    });

    expect(pane.generalError).toBe('Failed to rename thread');
    expect(pane.generalErrorKind).toBeNull();
  });

  it('turn_started for thread A does not clear a session-death banner on thread B', async () => {
    await buildPane(makeThread({ id: 'thread-aa', provider: 'claude' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-bb', provider: 'claude' }), [], 'b');

    emitWailsEvent('provider:session_died', {
      threadId: 'thread-bb',
      exitCode: 1,
      reason: 'thread-bb is dead',
    });
    expect(paneB.generalError).toBe('thread-bb is dead');

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-aa',
      turnId: 'turn-aa',
      turnIndex: 0,
      startedAt: 1234,
    });

    expect(paneB.generalError).toBe('thread-bb is dead');
    expect(paneB.generalErrorKind).toBe('session');
  });

  // --- Turn lifecycle routing (Wave 2) ---------------------------------------

  it('routes provider:turn_started to pane.setActiveTurn for the matching thread only', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'b');

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

    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-boundary',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
    });

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('parses tokenUsage JSON from provider:turn_completed into a typed summary', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));

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

  it('drops provider:turn_started for non-matching threadIds', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1' }));

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-other',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  // `user_message:reverted` mirrors the backend's
  // `DeleteConversationFromTurn` truncate. If the optimistic frontend
  // path missed a row (or this is the only path, e.g. cross-pane
  // reflection), this handler is the safety net that removes every
  // item at the reverted turn — not just the user_text.
  it('removes every item at the reverted turn on user_message:reverted', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    pane.upsertItems([
      makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'think:0:0', threadId: 'thread-a', turnIndex: 0, kind: 'thinking', role: 'assistant', status: 'streaming' }),
      makeItem({ id: 'retry:0', threadId: 'thread-a', turnIndex: 0, kind: 'api_retry', role: 'system', status: 'running' }),
    ]);
    expect(pane.items.map((it) => it.id).sort()).toEqual(['retry:0', 'think:0:0', 'u:0']);

    emitWailsEvent('user_message:reverted', {
      threadId: 'thread-a',
      userItemId: 'u:0',
      turnIndex: 0,
    });

    expect(pane.items).toEqual([]);
  });

  it('does not disturb earlier turns when handling user_message:reverted', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    pane.upsertItems([
      makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'a:0', threadId: 'thread-a', turnIndex: 0, kind: 'assistant_text', role: 'assistant' }),
      makeItem({ id: 'u:1', threadId: 'thread-a', turnIndex: 1, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'think:1:0', threadId: 'thread-a', turnIndex: 1, kind: 'thinking', role: 'assistant', status: 'streaming' }),
    ]);

    emitWailsEvent('user_message:reverted', {
      threadId: 'thread-a',
      userItemId: 'u:1',
      turnIndex: 1,
    });

    // Explicit-length assertion guards against an item silently
    // surviving at the reverted turn (sort would still pass if a
    // hidden id sorted before 'a:0').
    expect(pane.items).toHaveLength(2);
    expect(pane.items.map((it) => it.id).sort()).toEqual(['a:0', 'u:0']);
  });

  // The handler iterates every pane via iterPanes(); a regression that
  // breaks out of the loop early or scopes the truncate to the first
  // match would ship silently without this coverage. (e.g. two panes
  // viewing the same thread side-by-side.)
  it('mirrors the truncate to every pane viewing the reverted thread', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-x' }), [], 'a');
    const paneB = await buildPane(makeThread({ id: 'thread-x' }), [], 'b');
    const seed = (pane: typeof paneA): void => {
      pane.upsertItems([
        makeItem({ id: 'u:0', threadId: 'thread-x', turnIndex: 0, kind: 'user_text', role: 'user' }),
        makeItem({ id: 'think:0:0', threadId: 'thread-x', turnIndex: 0, kind: 'thinking', role: 'assistant', status: 'streaming' }),
      ]);
    };
    seed(paneA);
    seed(paneB);

    emitWailsEvent('user_message:reverted', {
      threadId: 'thread-x',
      userItemId: 'u:0',
      turnIndex: 0,
    });

    expect(paneA.items).toEqual([]);
    expect(paneB.items).toEqual([]);
  });

  // The strict `typeof payload.turnIndex !== 'number'` guard exists
  // because `payload.turnIndex` of 0 is a VALID revert target (first
  // turn of a fresh thread) and a truthy check would reject it. A
  // regression that loosens the guard to `!payload.turnIndex` would
  // strand the user message in the timeline; this test pins the
  // contract.
  it('rejects user_message:reverted events with non-number turnIndex', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    pane.upsertItem(makeItem({ id: 'u:0', threadId: 'thread-a', turnIndex: 0, kind: 'user_text', role: 'user' }));

    // Missing turnIndex — wire would not include the field.
    emitWailsEvent('user_message:reverted', {
      threadId: 'thread-a',
      userItemId: 'u:0',
    });
    expect(pane.items).toHaveLength(1);

    // NaN slips past `typeof === 'number'` in JS, so
    // removeItemsFromTurn's own Number.isFinite guard is the second
    // line of defense.
    emitWailsEvent('user_message:reverted', {
      threadId: 'thread-a',
      userItemId: 'u:0',
      turnIndex: Number.NaN,
    });
    expect(pane.items).toHaveLength(1);
  });

  // provider:queue_restored reports queued messages whose store rows
  // the backend deleted when it restored their content to the composer
  // draft (failed Codex resend, session death). Rows the event names
  // must leave the mounted timeline — without this, a deleted store
  // row keeps a ghost row on screen until the next full reload — and
  // rows it does not name must stay.
  it('removes only the restored rows from the timeline on provider:queue_restored', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-q' }));
    pane.upsertItems([
      makeItem({ id: 'u:keep', threadId: 'thread-q', turnIndex: 0, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'user:1:flush:1', threadId: 'thread-q', turnIndex: 1, kind: 'user_text', role: 'user' }),
      makeItem({ id: 'user:1:flush:2', threadId: 'thread-q', turnIndex: 1, kind: 'user_text', role: 'user' }),
    ]);

    emitWailsEvent('provider:queue_restored', {
      threadId: 'thread-q',
      reason: 'resend_failed',
      userItemIds: ['user:1:flush:1', 'user:1:flush:2'],
    });

    expect(pane.items.map((it) => it.id)).toEqual(['u:keep']);

    // Absent ids are a no-op, not an error (the session-death restore
    // can name rows that were never promoted into the timeline).
    emitWailsEvent('provider:queue_restored', {
      threadId: 'thread-q',
      reason: 'session_died',
      userItemIds: ['user:9:flush:9'],
    });
    expect(pane.items.map((it) => it.id)).toEqual(['u:keep']);
  });
  // ===== flushItemEventQueue per-item batching =====
  // A tool burst arrives on the wire as upserts, patches, and deltas of
  // many DIFFERENT rows interleaved. The flush must not fragment on
  // action-type transitions — only a genuine per-item conflict (the
  // same row pending in another buffer) forces an early apply. Each
  // fragment used to pay an O(window) items-array copy plus a full
  // structural timeline regroup, which is what made simultaneous bash
  // rows stutter (2026-06-12 streaming-jank investigation).
  it('applies an interleaved cross-item tool burst as a single upsert batch', async () => {
    const pane = await buildPane();

    // Seed two rows so the patch and delta below target existing items.
    const seededTool = makeItem({
      id: 'tool-existing', kind: 'tool_call', status: 'running', summary: 'running', itemIndex: 0,
    });
    const seededOut = makeItem({
      id: 'out-existing', kind: 'tool_call', status: 'streaming', summary: 'partial', itemIndex: 1,
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: seededTool.threadId, item: seededTool });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: seededOut.threadId, item: seededOut });
    await nextFrame();
    expect(pane.items).toHaveLength(2);

    const applySpy = vi.spyOn(pane, 'applyProviderItemUpserts');

    const tool1 = makeItem({ id: 'tool-1', kind: 'tool_call', status: 'running', turnIndex: 1, itemIndex: 0 });
    const tool2 = makeItem({ id: 'tool-2', kind: 'tool_call', status: 'running', turnIndex: 1, itemIndex: 1 });
    const tool3 = makeItem({ id: 'tool-3', kind: 'tool_call', status: 'running', turnIndex: 1, itemIndex: 2 });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: tool1.threadId, item: tool1 });
    emitWailsEvent('provider:item_event', {
      action: 'patch', threadId: seededTool.threadId, itemId: 'tool-existing', patch: { status: 'completed' },
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: tool2.threadId, item: tool2 });
    emitWailsEvent('provider:item_event', {
      action: 'delta', threadId: seededOut.threadId, itemId: 'out-existing', kind: 'tool_call', delta: ' more', updatedAt: 2,
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: tool3.threadId, item: tool3 });
    await nextFrame();

    expect(applySpy).toHaveBeenCalledTimes(1);
    expect(applySpy.mock.calls[0][0].map((item: Item) => item.id)).toEqual(['tool-1', 'tool-2', 'tool-3']);
    expect(pane.getItemById('tool-existing')?.status).toBe('completed');
    expect(pane.getItemById('out-existing')?.summary).toBe('partial more');
  });

  it('batches successive same-item upserts without fragmentation (last wins)', async () => {
    const pane = await buildPane();
    const applySpy = vi.spyOn(pane, 'applyProviderItemUpserts');

    // Codex foreground exec emits a full-item upsert per output chunk;
    // a chain for the same row must still land as one batch with
    // sequential (last-wins) semantics.
    for (const summary of ['line1', 'line1+2', 'line1+2+3']) {
      const item = makeItem({ id: 'exec-1', kind: 'tool_call', status: 'running', summary });
      emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });
    }
    await nextFrame();

    expect(applySpy).toHaveBeenCalledTimes(1);
    expect(pane.getItemById('exec-1')?.summary).toBe('line1+2+3');
    expect(pane.lastLiveContentAt).toBe(0);
  });

  it('preserves per-item apply order when upserts and deltas interleave on one row', async () => {
    const pane = await buildPane();

    const u1 = makeItem({ id: 'out-1', kind: 'tool_call', status: 'streaming', summary: 'A' });
    const u2 = makeItem({ id: 'out-1', kind: 'tool_call', status: 'streaming', summary: 'B' });
    const threadId = u1.threadId;

    // u1, +1, u2, +2 — u2 carries the full backend summary and
    // supersedes u1 and the first delta; the trailing delta extends u2.
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId, item: u1 });
    emitWailsEvent('provider:item_event', {
      action: 'delta', threadId, itemId: 'out-1', kind: 'tool_call', delta: '+1', updatedAt: 2,
    });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId, item: u2 });
    emitWailsEvent('provider:item_event', {
      action: 'delta', threadId, itemId: 'out-1', kind: 'tool_call', delta: '+2', updatedAt: 3,
    });
    await nextFrame();

    expect(pane.getItemById('out-1')?.summary).toBe('B+2');
  });

  it('runs no global-store projection off the item batch at all', async () => {
    await buildPane();

    // The projection this batch used to carry (thread status, sidebar
    // activity, durable plan status) has moved to wildcard channels, so an
    // item upsert now touches the pane and the caches and nothing global.
    // Neither before the frame nor after it.
    const item = makeItem({ id: 'err-1', kind: 'error', role: 'system', summary: 'boom' });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: item.threadId, item });

    expect(getThreadStatus('thread-1')).toBe('idle');
    await nextFrame();
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  // Multi-pane beats land in ONE frame by design. Rotating them (one
  // mounted thread per rAF) was built and refuted by a 3-pane
  // clone-replay A/B on 2026-08-26: busy p95 3.0 → 5.0/5.5ms, worst
  // frame no better. The merged flush amortizes per-flush fixed costs;
  // see the NOTE in eventsItemStream.ts#flushItemEventQueue.
  it('applies every mounted pane\'s beat in the same frame', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }), [], 'pane-a');
    const paneB = await buildPane(makeThread({ id: 'thread-b' }), [], 'pane-b');
    const applyA = vi.spyOn(paneA, 'applyProviderItemUpserts');
    const applyB = vi.spyOn(paneB, 'applyProviderItemUpserts');

    const a1 = makeItem({ id: 'a-1', threadId: 'thread-a', summary: 'A1' });
    const b1 = makeItem({ id: 'b-1', threadId: 'thread-b', summary: 'B1' });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: 'thread-a', item: a1 });
    emitWailsEvent('provider:item_event', { action: 'upsert', threadId: 'thread-b', item: b1 });
    await nextFrame();

    expect(applyA).toHaveBeenCalledTimes(1);
    expect(applyB).toHaveBeenCalledTimes(1);
    expect(paneA.getItemById('a-1')?.summary).toBe('A1');
    expect(paneB.getItemById('b-1')?.summary).toBe('B1');
  });

});
