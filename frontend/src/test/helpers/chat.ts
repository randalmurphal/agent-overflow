import { getAllPanes } from '../../lib/stores/panes.svelte';
import { upsertProposedPlanForTests } from '../../lib/stores/proposedPlans.svelte';
import { getQueueForThread } from '../../lib/stores/sendQueue.svelte';
import { createThreadPane, type ThreadPane } from '../../lib/stores/thread.svelte';
import type { ItemDeltaEvent } from '../../lib/types/events';
import type { Item, Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';

export function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
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

export function emitItemEventDelta(delta: ItemDeltaEvent): void {
  emitWailsEvent('provider:item_event', {
    action: 'delta',
    ...delta,
  });
}

export function installPaneMocks(items: Item[] = []): void {
  setBindingMock('SwitchThread', async (threadId: unknown) =>
    makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }));
  // ChatView may mark the active thread read as completed turns settle;
  // default both read-state bindings to no-ops so component tests that
  // don't care don't have to stub them.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  // The pane loads the initial slice of history via ListThreadSliceAround
  // on switch (works for both bottom-snapshot and saved-anchor cases).
  // ListRecentThreadItems is the canonical wider-window binding used by
  // the transport-gap recovery path (`refreshFromBackend`); component
  // tests that don't exercise that path leave the default empty.
  setBindingMock('ListThreadSliceAround', async () => ({
    items,
    oldestTurnIndex: items.length > 0 ? items[0].turnIndex : -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentThreadItems', async () => ({
    items,
    oldestTurnIndex: items.length > 0 ? items[0].turnIndex : -1,
    hasMore: false,
  }));
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
  // Empty turn history by default. Tests that want to exercise rehydration
  // override this via setBindingMock('ListRecentTurns', ...) after calling
  // buildPane / installPaneMocks.
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
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

export async function buildPane(
  thread: Thread = makeThread(),
  items: Item[] = [],
): Promise<ThreadPane> {
  installPaneMocks(items);
  setBindingMock('SwitchThread', async () => thread);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  // Register so syncThread (and any other panes-iterating helper) can
  // reach this pane. Production code goes through getMainPane() which
  // already registers; tests instantiating createThreadPane directly
  // need this explicit step.
  getAllPanes().set('main', pane);
  return pane;
}
