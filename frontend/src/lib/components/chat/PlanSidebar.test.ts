import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
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
    await tick();
    await tick();
    return result;
  }

  it('renders only the current proposed plan from the newest-first plan list', async () => {
    setBindingMock('ListThreadProposedPlans', async () => [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        itemIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Latest plan',
          preview: 'latest preview',
          lineCount: 1,
          charCount: 14,
        }),
      }),
      makeItem({
        id: 'plan-1',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'First plan',
          preview: 'old preview',
          lineCount: 1,
          charCount: 11,
        }),
      }),
    ]);
    setBindingMock('GetPayloadData', async (_threadId: string, payloadId: string) => ({
      data: payloadId === 'payload-2' ? '# Latest body' : '# First body',
    }));
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { findByText, queryByText } = await renderSidebar(pane);

    await findByText('Latest body');
    expect(queryByText('First plan')).toBeNull();
    expect(queryByText('First body')).toBeNull();
  });

  it('renders the empty state when the backend returns no plans', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = await renderSidebar(pane);
    expect(getByTestId('plan-sidebar-empty')).toBeInTheDocument();
  });

  it('closes when the close button is clicked', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = await renderSidebar(pane);
    await fireEvent.click(getByTestId('plan-sidebar-close'));

    expect(pane.showPlanSidebar).toBe(false);
  });

  it('re-fetches the current plan when a proposed_plan upsert lands for this thread', async () => {
    let plansForRefresh: ReturnType<typeof makeItem>[] = [];
    let fetchCount = 0;
    setBindingMock('ListThreadProposedPlans', async () => {
      fetchCount += 1;
      return plansForRefresh;
    });

    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const { findByText, queryByText } = await renderSidebar(pane);

    expect(queryByText('Freshly proposed')).toBeNull();
    expect(fetchCount).toBe(1);

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

    await findByText('Freshly proposed', {}, { timeout: 500 });
    expect(fetchCount).toBeGreaterThanOrEqual(2);
  });

  it('switches to a newly refreshed current plan instead of keeping older plan state', async () => {
    let plansForRefresh = [
      makeItem({
        id: 'plan-1',
        turnIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'First plan',
          preview: 'first',
          lineCount: 1,
          charCount: 5,
        }),
      }),
    ];
    setBindingMock('ListThreadProposedPlans', async () => plansForRefresh);
    setBindingMock('GetPayloadData', async (_threadId: string, payloadId: string) => ({
      data: payloadId === 'payload-2' ? '# Second body' : '# First body',
    }));
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { findByText, queryByText } = await renderSidebar(pane);
    await findByText('First body');

    plansForRefresh = [
      makeItem({
        id: 'plan-2',
        turnIndex: 1,
        kind: 'tool_call',
        payloadId: 'payload-2',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Second plan',
          preview: 'second',
          lineCount: 1,
          charCount: 6,
        }),
      }),
      ...plansForRefresh,
    ];
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

    await findByText('Second body', {}, { timeout: 500 });
    expect(queryByText('First body')).toBeNull();
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

    await new Promise((r) => setTimeout(r, 150));
    expect(fetchCount).toBe(1);
    await waitFor(() => expect(fetchCount).toBe(1));
  });

  it('discards a stale refresh whose promise resolves after a newer one', async () => {
    let releaseFirst!: (rows: ReturnType<typeof makeItem>[]) => void;
    const firstFetchPending = new Promise<ReturnType<typeof makeItem>[]>((r) => {
      releaseFirst = r;
    });
    let call = 0;
    setBindingMock('ListThreadProposedPlans', async () => {
      call += 1;
      if (call === 1) return firstFetchPending;
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
    expect(queryByText('Fresh plan')).toBeNull();

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
    await new Promise((r) => setTimeout(r, 50));

    await findByText('Fresh plan');
    expect(queryByText('Stale plan')).toBeNull();
  });
});
