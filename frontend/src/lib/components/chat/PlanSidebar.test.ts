import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import PlanSidebar from './PlanSidebar.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<PlanSidebar>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('renders proposed-plan payload rows newest first when open', async () => {
    const pane = await buildPane(undefined, [
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
    ]);
    pane.setShowPlanSidebar(true);

    const { getAllByTestId, getByText } = render(PlanSidebar, { props: { pane } });

    expect(getByText('Latest plan')).toBeInTheDocument();
    expect(getByText('First plan')).toBeInTheDocument();
    expect(getAllByTestId('plan-sidebar-row')[0]?.textContent).toContain('Latest plan');
  });

  it('does not render a separate streaming banner', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { queryByTestId } = render(PlanSidebar, { props: { pane } });

    expect(queryByTestId('plan-sidebar-streaming')).toBeNull();
  });

  it('closes when the close button is clicked', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);

    const { getByTestId } = render(PlanSidebar, { props: { pane } });
    await fireEvent.click(getByTestId('plan-sidebar-close'));

    expect(pane.showPlanSidebar).toBe(false);
  });
});
