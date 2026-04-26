import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import PlanSidebar from './PlanSidebar.svelte';
import { buildPane, emitItemEventUpsert, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { setupEventListeners } from '../../stores/events';
import { resetProposedPlanCacheForTests } from '../../stores/proposedPlans.svelte';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<PlanSidebar>', () => {
  let cleanupEvents: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetBindingMocks();
    resetProposedPlanCacheForTests();
    // Default: no plans. Individual tests override this before render.
    setBindingMock('ListThreadProposedPlans', async () => []);
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({ data: '# Plan\n\nBody' }));
    cleanupEvents = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    resetProposedPlanCacheForTests();
    cleanupEvents?.();
  });

  async function renderSidebar(pane: Awaited<ReturnType<typeof buildPane>>) {
    const result = render(PlanSidebar, { props: { pane } });
    // Fetch + $effect + reactive derivation settle in two ticks.
    await tick();
    await tick();
    return result;
  }

  it('renders proposed-plan rows sourced from ListThreadProposedPlans newest first', async () => {
    // The backend returns rows in newest-first order already; the
    // sidebar preserves that ordering. Seed out-of-order here to pin
    // the contract that the component doesn't re-sort — if a future
    // implementation tries to get clever it will fail this test.
    setBindingMock('ListThreadProposedPlans', async () => [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        itemIndex: 0,
        kind: 'tool_call',
        payloadId: 'p2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Latest plan',
          preview: 'two',
          lineCount: 1,
          charCount: 3,
        }),
      }),
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadId: 'p1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'First plan',
          preview: 'one',
          lineCount: 1,
          charCount: 3,
        }),
      }),
    ]);
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getAllByTestId, getByText } = await renderSidebar(pane);

    expect(getByText('Latest plan')).toBeInTheDocument();
    expect(getByText(/First plan/)).toBeInTheDocument();
    expect(getAllByTestId('plan-sidebar-row')[0]?.textContent).toContain('Turn 2');
  });

  it('renders the empty state when the backend returns no plans', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = await renderSidebar(pane);
    expect(getByTestId('plan-sidebar-empty')).toBeInTheDocument();
  });

  it('clicking a row publishes a scroll-to-item request on the pane', async () => {
    setBindingMock('ListThreadProposedPlans', async () => [
      makeItem({
        id: 'plan-xyz',
        kind: 'tool_call',
        payloadId: 'p-xyz',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'Plan XYZ', preview: 'x' }),
      }),
    ]);
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const spy = vi.spyOn(pane, 'requestScrollToItem');

    const { getByTestId } = await renderSidebar(pane);
    await fireEvent.click(getByTestId('plan-sidebar-row'));

    expect(spy).toHaveBeenCalledWith('plan-xyz');
  });

  it('reloads plan body when switching between versions', async () => {
    setBindingMock('ListThreadProposedPlans', async () => [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        kind: 'tool_call',
        payloadId: 'payload-2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'Second plan', preview: 'second', lineCount: 1, charCount: 6 }),
      }),
      makeItem({
        id: 'plan-1',
        turnIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'First plan', preview: 'first', lineCount: 1, charCount: 5 }),
      }),
    ]);
    setBindingMock('GetPayloadData', async (_threadId: string, payloadId: string) => ({
      data: payloadId === 'payload-1' ? '# First body' : '# Second body',
    }));
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { findByText, getByTestId } = await renderSidebar(pane);
    await findByText('Second body');

    await fireEvent.change(getByTestId('plan-version-select'), { target: { value: 'plan-1' } });

    await findByText('First body');
  });

  it('preserves an explicitly selected older plan when the list refreshes', async () => {
    let plansForRefresh = [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        kind: 'tool_call',
        payloadId: 'payload-2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'Second plan', preview: 'second', lineCount: 1, charCount: 6 }),
      }),
      makeItem({
        id: 'plan-1',
        turnIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'First plan', preview: 'first', lineCount: 1, charCount: 5 }),
      }),
    ];
    setBindingMock('ListThreadProposedPlans', async () => plansForRefresh);
    setBindingMock('GetPayloadData', async (_threadId: string, payloadId: string) => ({
      data: payloadId === 'payload-1' ? '# First body' : '# Second body',
    }));
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { findByText, getByTestId } = await renderSidebar(pane);
    await findByText('Second body');

    await fireEvent.change(getByTestId('plan-version-select'), { target: { value: 'plan-1' } });
    await findByText('First body');

    plansForRefresh = [...plansForRefresh];
    emitItemEventUpsert({
      id: 'plan-2',
      threadId: pane.thread!.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'proposed_plan',
      createdAt: 0,
      updatedAt: 0,
    });

    await new Promise((r) => setTimeout(r, 150));
    const select = getByTestId('plan-version-select') as HTMLSelectElement;
    expect(select.value).toBe('plan-1');
  });

  it('treats open-for-item as a one-shot selection request', async () => {
    const plans = [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        kind: 'tool_call',
        payloadId: 'payload-2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'Second plan', preview: 'second', lineCount: 1, charCount: 6 }),
      }),
      makeItem({
        id: 'plan-1',
        turnIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'First plan', preview: 'first', lineCount: 1, charCount: 5 }),
      }),
    ];
    setBindingMock('ListThreadProposedPlans', async () => plans);
    const pane = await buildPane();
    pane.openPlanSidebarForItem('plan-1');

    const { getByTestId } = await renderSidebar(pane);
    const select = getByTestId('plan-version-select') as HTMLSelectElement;
    expect(select.value).toBe('plan-1');

    await fireEvent.change(select, { target: { value: 'plan-2' } });
    emitItemEventUpsert({
      id: 'plan-2',
      threadId: pane.thread!.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'proposed_plan',
      createdAt: 0,
      updatedAt: 0,
    });
    await new Promise((r) => setTimeout(r, 150));

    expect(select.value).toBe('plan-2');
  });

  it('closes when the close button is clicked', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = await renderSidebar(pane);
    await fireEvent.click(getByTestId('plan-sidebar-close'));

    expect(pane.showPlanSidebar).toBe(false);
  });

  it('re-fetches the plan list when a proposed_plan upsert lands for this thread', async () => {
    // Initial list is empty; after the provider upsert for a new plan
    // lands the sidebar must call ListThreadProposedPlans again (with
    // debounce) and render the new row. This pins the core refresh
    // path that keeps the sidebar in sync during live turns.
    let plansForRefresh: ReturnType<typeof makeItem>[] = [];
    let fetchCount = 0;
    setBindingMock('ListThreadProposedPlans', async () => {
      fetchCount += 1;
      return plansForRefresh;
    });

    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const { findByText, queryByText } = await renderSidebar(pane);

    // First render: empty state + 1 fetch from the mount $effect.
    expect(queryByText('Freshly proposed')).toBeNull();
    expect(fetchCount).toBe(1);

    // Stage the post-upsert response.
    plansForRefresh = [
      makeItem({
        id: 'plan-fresh',
        kind: 'tool_call',
        payloadId: 'p-fresh',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Freshly proposed',
          preview: 'content',
          lineCount: 1,
          charCount: 7,
        }),
      }),
    ];

    emitItemEventUpsert({
      id: 'plan-fresh',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'proposed_plan',
      createdAt: 0,
      updatedAt: 0,
    });

    // The debounce window is 100 ms; wait for the text to appear.
    await findByText('Freshly proposed', {}, { timeout: 500 });
    expect(fetchCount).toBeGreaterThanOrEqual(2);
  });

  it('ignores provider upserts with mismatched thread id or non-plan kind', async () => {
    let fetchCount = 0;
    setBindingMock('ListThreadProposedPlans', async () => {
      fetchCount += 1;
      return [];
    });
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    await renderSidebar(pane);
    expect(fetchCount).toBe(1);

    // Wrong thread.
    emitItemEventUpsert({
      id: 'x',
      threadId: 'other-thread',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'proposed_plan',
      createdAt: 0,
      updatedAt: 0,
    });
    // Different payload kind.
    emitItemEventUpsert({
      id: 'y',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'diff',
      createdAt: 0,
      updatedAt: 0,
    });

    // Give the debounce window a chance to fire if the filter was wrong.
    await new Promise((r) => setTimeout(r, 150));
    // Refresh count still at the mount fetch — filter held.
    expect(fetchCount).toBe(1);
    await waitFor(() => expect(fetchCount).toBe(1));
  });

  it('discards a stale refresh whose promise resolves after a newer one', async () => {
    // The proposed-plan cache uses a per-call `fetchSeq`. If
    // two refreshes are in flight and the OLDER one resolves LAST,
    // its result must not overwrite the newer cached rows.
    // Without the guard, a slow initial fetch could overwrite a fast
    // post-upsert fetch with stale rows.
    let releaseFirst!: (rows: ReturnType<typeof makeItem>[]) => void;
    const firstFetchPending = new Promise<ReturnType<typeof makeItem>[]>((r) => {
      releaseFirst = r;
    });
    let call = 0;
    setBindingMock('ListThreadProposedPlans', async () => {
      call += 1;
      if (call === 1) return firstFetchPending;
      // Second call resolves immediately with fresh rows.
      return [
        makeItem({
          id: 'plan-fresh',
          kind: 'tool_call',
          payloadId: 'p-fresh',
          payloadKind: 'proposed_plan',
          payloadMeta: JSON.stringify({
            title: 'Fresh plan',
            preview: 'fresh',
            lineCount: 1,
            charCount: 5,
          }),
        }),
      ];
    });

    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const { findByText, queryByText } = await renderSidebar(pane);
    // First fetch is pending; empty state visible.
    expect(queryByText('Fresh plan')).toBeNull();

    // Trigger a second fetch via a proposed_plan upsert. The debounce
    // window is 100 ms; wait for the fresh rows to land.
    emitItemEventUpsert({
      id: 'plan-fresh',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      summary: '',
      payloadKind: 'proposed_plan',
      createdAt: 0,
      updatedAt: 0,
    });
    await findByText('Fresh plan', {}, { timeout: 500 });

    // NOW resolve the original pending fetch with a stale row. The
    // sidebar must NOT swap 'Fresh plan' out for 'Stale plan'.
    releaseFirst([
      makeItem({
        id: 'plan-stale',
        kind: 'tool_call',
        payloadId: 'p-stale',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Stale plan',
          preview: 'stale',
          lineCount: 1,
          charCount: 5,
        }),
      }),
    ]);
    // Give the microtask queue time to resolve.
    await new Promise((r) => setTimeout(r, 50));

    // Fresh plan still there; stale plan never reaches the DOM.
    await findByText('Fresh plan');
    expect(queryByText('Stale plan')).toBeNull();
  });
});
