// Verifies that proposed_plan upserts populate the per-thread cache
// synchronously. Without this, getThreadCurrentProposedPlan returns stale
// data for ~100ms (the REFRESH_DEBOUNCE_MS window) after a new plan arrives,
// which forced PlanSidebar to read pane.items as a fallback and coupled it
// to chat streaming.

import { afterEach, describe, expect, it } from 'vitest';
import {
  upsertProposedPlanForTests,
  getThreadCurrentProposedPlan,
  getThreadProposedPlans,
  refreshThreadProposedPlans,
  resetProposedPlanCacheForTests,
} from './proposedPlans.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Item } from '../types/models';

function planItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'plan-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: 'ExitPlanMode',
    payloadKind: 'proposed_plan',
    payloadId: 'payload-1',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  };
}

afterEach(() => {
  resetProposedPlanCacheForTests();
  resetBindingMocks();
});

describe('proposed-plan cache sync upsert', () => {
  it('exposes a freshly-upserted plan immediately via getThreadCurrentProposedPlan', () => {
    expect(getThreadCurrentProposedPlan('thread-1')).toBeNull();

    const incoming = planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 });
    upsertProposedPlanForTests(incoming);

    // No timers, no awaits — the sync insert means the cache is current
    // the moment the upsert returns.
    expect(getThreadCurrentProposedPlan('thread-1')?.id).toBe('plan-1');
    expect(getThreadProposedPlans('thread-1')).toHaveLength(1);
  });

  it('replaces an existing plan in place when the same id is upserted again', () => {
    const original = planItem({
      id: 'plan-1',
      turnIndex: 1,
      itemIndex: 0,
      meta: '',
    });
    upsertProposedPlanForTests(original);

    const implemented = planItem({
      id: 'plan-1',
      turnIndex: 1,
      itemIndex: 0,
      meta: JSON.stringify({ planImplementedAt: 123 }),
      updatedAt: 2,
    });
    upsertProposedPlanForTests(implemented);

    const cached = getThreadProposedPlans('thread-1');
    expect(cached).toHaveLength(1);
    expect(cached[0]!.meta).toBe(JSON.stringify({ planImplementedAt: 123 }));
  });

  it('drops no-op upserts without replacing the cached item list', () => {
    const original = planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 });
    upsertProposedPlanForTests(original);
    const before = getThreadProposedPlans('thread-1');

    upsertProposedPlanForTests({ ...original });

    expect(getThreadProposedPlans('thread-1')).toBe(before);
  });

  it('keeps the latest by (turnIndex, itemIndex) when multiple plans are upserted', () => {
    upsertProposedPlanForTests(planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 }));
    upsertProposedPlanForTests(planItem({ id: 'plan-2', turnIndex: 3, itemIndex: 0 }));
    upsertProposedPlanForTests(planItem({ id: 'plan-3', turnIndex: 2, itemIndex: 0 }));

    expect(getThreadCurrentProposedPlan('thread-1')?.id).toBe('plan-2');
  });

  it('ignores items whose payloadKind is not proposed_plan', () => {
    upsertProposedPlanForTests(planItem({
      id: 'not-a-plan',
      payloadKind: undefined,
      payloadId: undefined,
    }));

    expect(getThreadCurrentProposedPlan('thread-1')).toBeNull();
  });

  it('populates the cache for a thread with no retained listener', () => {
    // The sync upsert path is independent of retainProposedPlanEventListener
    // so a thread that no PlanSidebar / Composer has subscribed to still
    // gets its cache populated when an event arrives. This matters for
    // pre-warming threads during tab restore / multi-pane futures.
    upsertProposedPlanForTests(planItem({ id: 'plan-x', threadId: 'cold-thread' }));
    expect(getThreadCurrentProposedPlan('cold-thread')?.id).toBe('plan-x');
  });
});

describe('proposed-plan refresh / sync upsert race', () => {
  it('keeps the existing item list reference when refresh returns identical items', async () => {
    const item = planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 });
    setBindingMock('ListThreadProposedPlans', async () => [item]);
    await refreshThreadProposedPlans('thread-1');
    const before = getThreadProposedPlans('thread-1');

    setBindingMock('ListThreadProposedPlans', async () => [{ ...item }]);
    await refreshThreadProposedPlans('thread-1');

    expect(getThreadProposedPlans('thread-1')).toBe(before);
  });

  it('does not let a stale RPC response wipe items upserted during the fetch', async () => {
    // Setup: the RPC for ListThreadProposedPlans is in flight.
    let resolveFetch!: (items: Item[]) => void;
    const fetchPromise = new Promise<Item[]>((resolve) => { resolveFetch = resolve; });
    setBindingMock('ListThreadProposedPlans', () => fetchPromise);

    // Kick off the refresh (it will await fetchPromise).
    const refreshDone = refreshThreadProposedPlans('thread-1');

    // While the RPC is in flight, an upsert event arrives with a NEW plan
    // the server hasn't seen yet (eventual consistency). The sync insert
    // populates the cache.
    upsertProposedPlanForTests(planItem({ id: 'plan-fresh', turnIndex: 5 }));
    expect(getThreadCurrentProposedPlan('thread-1')?.id).toBe('plan-fresh');

    // The stale fetch resolves with NO knowledge of plan-fresh. Without
    // the upsert-seq guard, this would replace entry.items = [] and wipe it.
    resolveFetch([]);
    await refreshDone;

    // Locally-observed item must survive — the next refresh will reconcile.
    expect(getThreadCurrentProposedPlan('thread-1')?.id).toBe('plan-fresh');
  });

  it('does not blank the cache when the RPC fails AND an upsert landed during it', async () => {
    let rejectFetch!: (err: Error) => void;
    const fetchPromise = new Promise<Item[]>((_, reject) => { rejectFetch = reject; });
    setBindingMock('ListThreadProposedPlans', () => fetchPromise);

    const refreshDone = refreshThreadProposedPlans('thread-1');
    upsertProposedPlanForTests(planItem({ id: 'plan-fresh', turnIndex: 5 }));
    rejectFetch(new Error('network'));
    await refreshDone;

    expect(getThreadCurrentProposedPlan('thread-1')?.id).toBe('plan-fresh');
  });
});
