import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import PlanSidebar from './PlanSidebar.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<PlanSidebar>', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Default: no plans. Individual tests override this before render.
    setBindingMock('ListThreadProposedPlans', async () => []);
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
    expect(getByText('First plan')).toBeInTheDocument();
    expect(getAllByTestId('plan-sidebar-row')[0]?.textContent).toContain('Latest plan');
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

  it('closes when the close button is clicked', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = await renderSidebar(pane);
    await fireEvent.click(getByTestId('plan-sidebar-close'));

    expect(pane.showPlanSidebar).toBe(false);
  });
});
