import { registerPaneForTest } from '../../lib/stores/panes.svelte';
import { upsertProposedPlanForTests } from '../../lib/stores/proposedPlans.svelte';
import { getQueueForThread } from '../../lib/stores/sendQueue.svelte';
import { createThreadPane, type ThreadPane } from '../../lib/stores/thread.svelte';
import type { PaneScrollController } from '../../lib/stores/threadPaneShared';
import type { ItemDeltaEvent } from '../../lib/types/events';
import type { Item, Thread } from '../../lib/types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
import { compareCursors, cursorFromItem } from '../../lib/stores/threadItems';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';
import { __setTransportHelloForTest } from '../../lib/stores/transportStatus.svelte';

/**
 * A no-op `PaneScrollController` satisfying every required member, for tests
 * that attach a controller to observe one seam of it. Build stubs here rather
 * than as inline object literals: when the interface grows a required member,
 * one default lands it everywhere instead of breaking every test file that
 * hand-rolled the shape.
 */
export function stubScrollController(
  overrides: Partial<PaneScrollController> = {},
): PaneScrollController {
  return {
    pauseAutoScroll: () => () => {},
    autoScrollInFlight: () => false,
    observe: () => {},
    markStructuralContentPending: () => {},
    armWarmup: () => {},
    preserveScrollAnchor: async () => {},
    ...overrides,
  };
}

export function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    // Both halves of the checkout: git and review affordances address a
    // WorkspaceRef, so a fixture without a projectId is a thread whose git
    // controls correctly refuse to render.
    projectId: 'project-1',
    workspacePath: '/tmp/workspace',
    projectPath: '/tmp/workspace',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

export function makeItem(overrides: Partial<Item> = {}): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'hello',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    // The store stamps every persisted row; 0 is the origin a thread with
    // no item writes since the contract landed genuinely sits at.
    rev: 0,
    ...overrides,
  };
}

export function emitItemEventUpsert(item: Item): void {
  emitWailsEvent('provider:item_event', {
    action: 'upsert',
    threadId: item.threadId,
    item,
  });
}

export function emitItemEventRemove(
  threadId: string,
  itemId: string,
  kind = 'user_text',
): void {
  emitWailsEvent('provider:item_event', {
    action: 'remove',
    threadId,
    itemId,
    kind,
  });
}

export function emitItemEventDelta(delta: ItemDeltaEvent): void {
  emitWailsEvent('provider:item_event', {
    action: 'delta',
    ...delta,
  });
}

export function installTimelineScopeCapability(): void {
  __setTransportHelloForTest({ protocolVersion: 1, capabilities: ['timeline.scopes.v1', 'timeline.digests.v1'],
    backendId: '', backendName: '', serverTimeMs: 0, clockSkewMs: 0,
    bundleId: '', bundleVersion: '', minShellBuild: 0 });
}

export function installPaneMocks(items: Item[] = [], runs: ActivityRunStub[] = []): void {
  setBindingMock('GetTimelineUserMessageTicks', async () => []);
  setBindingMock('SwitchThread', async (threadId: unknown) =>
    makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }));
  // ChatView may mark the active thread read as completed turns settle;
  // default both read-state bindings to no-ops so component tests that
  // don't care don't have to stub them.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  // switchThread fires AutoResumeThread as a fire-and-forget leg inside
  // runParallelLoad; leaving it unmocked floods every buildPane-based test
  // with a swallowed "called without a mock" console.error. No-op default.
  setBindingMock('AutoResumeThread', async () => {});
  // The pane loads the initial slice of history via ListThreadSliceAround
  // on switch (works for both bottom-snapshot and saved-anchor cases).
  // `runs` are the page's activity run stubs: a run whose members the
  // page did not all ship is described by one, and the pane's registry
  // folds it against the rows it holds.
  // The page's cursors bound whole units (internal/store/paging.go):
  // a run whose members the page did not all ship still lies inside them.
  const first = items[0] ?? null;
  const last = items[items.length - 1] ?? null;
  const oldestCursor = runs.reduce(
    (cursor, stub) => {
      const edge = { turnIndex: stub.firstTurnIndex, itemIndex: stub.firstItemIndex, itemId: stub.firstItemId };
      return cursor === null || compareCursors(edge, cursor) < 0 ? edge : cursor;
    },
    first ? cursorFromItem(first) : null,
  );
  const newestCursor = runs.reduce(
    (cursor, stub) => {
      const edge = { turnIndex: stub.lastTurnIndex, itemIndex: stub.lastItemIndex, itemId: stub.lastItemId };
      return cursor === null || compareCursors(edge, cursor) > 0 ? edge : cursor;
    },
    last ? cursorFromItem(last) : null,
  );
  setBindingMock('ListThreadSliceAround', async (_thread: string, _anchor: string, _budget: number, options: { selection?: { scopeRootId?: string; tools?: boolean; digestItemId?: string } }) => {
    const selection = options.selection;
    const rootId = selection?.scopeRootId;
    const root = items.find(item => item.id === rootId);
    const selected = !rootId ? items : items.filter(item => item.parentId === rootId
      && (!selection?.tools || ['tool_call', 'tool_completion', 'terminal_interaction'].includes(item.kind)));
    const lifecycle = rootId ? [...items].reverse().find(item => item.meta?.includes(`"transcript_root_id":"${rootId}"`)) ?? root : undefined;
    const scope = root && lifecycle ? { root, lifecycle, completion: [...items].reverse().find(item => item.completionOf === lifecycle.id) } : undefined;
    if (scope && selection?.digestItemId) {
      Object.assign(scope, { digest: {
        promptId: selected.find(it => it.kind === 'user_text')?.id ?? '',
        answerId: [...selected].reverse().find(it => it.kind === 'assistant_text')?.id ?? '',
      } });
    }
    return {
      items: selected, scope,
      oldestCursor: rootId ? selected[0] ? cursorFromItem(selected[0]) : { turnIndex: -1, itemIndex: -1, itemId: '' } : oldestCursor,
      newestCursor: rootId ? selected.at(-1) ? cursorFromItem(selected.at(-1)!) : { turnIndex: -1, itemIndex: -1, itemId: '' } : newestCursor,
      oldestTurnIndex: selected[0]?.turnIndex ?? -1,
      newestTurnIndex: selected.at(-1)?.turnIndex ?? -1,
      hasMore: false, hasMoreOlder: false, hasMoreNewer: false,
      runs: rootId ? [] : runs,
    };
  });
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('GetThreadLiveState', async (threadId: string) => ({
    threadId,
    activeTurn: null,
    queueItems: [...getQueueForThread(threadId)],
    interactive: { approvals: [], userInputs: [] },
    todo: null,
  }));
  setBindingMock('ListItems', async () => items);
  setBindingMock('GetThreadItem', async (threadId: string, itemId: string) => items.find(item => item.threadId === threadId && item.id === itemId) ?? null);
  // Empty turn history by default. Tests that want to exercise rehydration
  // override this via setBindingMock('ListRecentTurns', ...) after calling
  // buildPane / installPaneMocks.
  setBindingMock('ListRecentTurns', async () => []);
  // UsageChip queries the lifetime usage bucket on mount. Default to "no
  // usage yet" so buildPane-based tests that don't care about the usage
  // chip don't have to stub it; tests exercising usage data override this.
  setBindingMock('GetUsageStats', async () => []);
  // Seed the proposed-plan cache for any plan items pushed into the pane.
  // Composer / PlanSidebar derive "current plan" from the cache (not pane.items)
  // so a test that sets up plans via buildPane(thread, [planItem]) needs the
  // cache to mirror that.
  const planItems = items.filter((item) => item.payloadKind === 'proposed_plan');
  for (const item of planItems) {
    upsertProposedPlanForTests(item);
  }
  // Mount-time refresh in Composer / PlanSidebar will throw on an unmocked
  // ListThreadProposedPlans and wipe the cache in the catch path. Auto-mock
  // the RPC to echo the seeded plans so the refresh is effectively a no-op
  // for tests that haven't installed their own mock. Skip when no plans were
  // seeded, so tests that set their own mock before buildPane (PlanSidebar
  // pattern) keep that mock.
  if (planItems.length > 0) {
    setBindingMock('ListThreadProposedPlans', async () => planItems);
  }
}

// For tests that call `pane.switchThread(...)` mid-test (as opposed to the
// initial load that `buildPane` performs) and today hand-roll the
// switch-leg mocks inline. Installs the full binding-mock set via
// `installPaneMocks`, then overrides `SwitchThread` to resolve `thread` so
// the mid-test switch lands on it.
export function installThreadSwitchMocks(thread: Thread, items: Item[] = []): void {
  installPaneMocks(items);
  setBindingMock('SwitchThread', async () => thread);
}

export async function buildPane(
  thread: Thread = makeThread(),
  items: Item[] = [],
  paneKey = 'main',
  runs: ActivityRunStub[] = [],
): Promise<ThreadPane> {
  installPaneMocks(items, runs);
  setBindingMock('SwitchThread', async () => thread);
  // The pane's own id matches the registry key: production panes are always
  // registered under their paneId, and pane-focus-gated behavior (the
  // composer's initial-focus pass checks getFocusedPaneId() === paneId)
  // depends on the ids lining up — the panes store focuses 'main' by
  // default, so the default paneKey yields a logically focused pane.
  const pane = createThreadPane({ paneId: paneKey });
  await pane.switchThread(thread);
  // Register so syncThread (and any other panes-iterating helper) can
  // reach this pane. Production code goes through ensureMainPane() which
  // already registers; tests instantiating createThreadPane directly
  // need this explicit step. Do NOT `getAllPanes().set(...)` the same
  // pane under a second key in tests: `iterPanes()` walks Map values
  // with no identity dedupe, so every event batch would apply twice —
  // value-identical re-applies dedupe silently, but value-different
  // bursts (insert + completion in one batch) mis-resolve against
  // post-apply state. Multi-pane tests pass distinct `paneKey`s instead.
  registerPaneForTest(paneKey, pane);
  return pane;
}
