import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import PlanSidebar from './PlanSidebar.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread, Item, PayloadMeta, ProposedPlanMeta } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function seedThread(id = 'thread-1'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function makeItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'item',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'message',
    role: 'assistant',
    summary: '',
    createdAt: 0,
    ...overrides,
  };
}

function makePlanMeta(meta: Partial<ProposedPlanMeta> = {}): ProposedPlanMeta {
  return {
    title: 'Test plan',
    lineCount: 4,
    charCount: 40,
    preview: 'Step 1\nStep 2\nStep 3',
    ...meta,
  };
}

async function buildPane(items: Item[] = [], metas: PayloadMeta[] = []) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => items);
  setBindingMock('ListPayloadMetas', async () => metas);
  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

describe('<PlanSidebar>', () => {
  beforeEach(() => {
    // Guard happy-dom missing Element.animate — fly transition relies on it.
    if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
      (Element.prototype as unknown as { animate: () => unknown }).animate = function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          cancel() {},
          finish() {},
          play() {},
          pause() {},
          addEventListener() {},
          removeEventListener() {},
        } as unknown;
      };
    }
  });

  it('renders nothing when the sidebar is closed', async () => {
    const pane = await buildPane();
    expect(pane.showPlanSidebar).toBe(false);
    const { queryByTestId } = render(PlanSidebar, { props: { pane } });
    expect(queryByTestId('plan-sidebar')).toBeNull();
  });

  it('opens and closes via pane.togglePlanSidebar / close button', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const { getByTestId } = render(PlanSidebar, { props: { pane } });
    expect(getByTestId('plan-sidebar')).toBeInTheDocument();

    await fireEvent.click(getByTestId('plan-sidebar-close'));
    // The fly transition leaves an inert node in place until it finishes;
    // asserting store state is the precise behavioural check.
    expect(pane.showPlanSidebar).toBe(false);
    // The element is marked inert so focus and interaction can't reach it.
    expect(getByTestId('plan-sidebar').hasAttribute('inert')).toBe(true);
  });

  it('renders an empty-state when no proposed plans are present', async () => {
    const pane = await buildPane([
      makeItem({ id: 'a1', summary: 'hello', itemIndex: 0 }),
    ]);
    pane.setShowPlanSidebar(true);
    const { getByTestId } = render(PlanSidebar, { props: { pane } });
    expect(getByTestId('plan-sidebar-empty').textContent ?? '').toMatch(/No plans yet/i);
  });

  it('lists proposed plans newest first with title, turn label, and preview', async () => {
    const items: Item[] = [
      makeItem({
        id: 'p1',
        kind: 'proposed_plan',
        payloadId: 'pl-1',
        itemIndex: 0,
        turnIndex: 0,
      }),
      makeItem({
        id: 'p2',
        kind: 'proposed_plan',
        payloadId: 'pl-2',
        itemIndex: 1,
        turnIndex: 2,
      }),
    ];
    const metas: PayloadMeta[] = [
      {
        id: 'pl-1',
        kind: 'proposed_plan',
        meta: JSON.stringify(makePlanMeta({ title: 'First plan', preview: 'alpha beta gamma' })),
        createdAt: 0,
      },
      {
        id: 'pl-2',
        kind: 'proposed_plan',
        meta: JSON.stringify(makePlanMeta({ title: 'Second plan', preview: 'delta epsilon' })),
        createdAt: 0,
      },
    ];
    const pane = await buildPane(items, metas);
    pane.setShowPlanSidebar(true);

    const { getAllByTestId, getByText } = render(PlanSidebar, { props: { pane } });
    const rows = getAllByTestId('plan-sidebar-row');
    expect(rows).toHaveLength(2);
    // Newest (p2) first.
    expect(rows[0].textContent ?? '').toMatch(/Second plan/);
    expect(rows[0].textContent ?? '').toMatch(/Turn 3/);
    expect(rows[0].textContent ?? '').toMatch(/delta epsilon/);
    expect(rows[1].textContent ?? '').toMatch(/First plan/);
    expect(rows[1].textContent ?? '').toMatch(/Turn 1/);
    // Belt-and-suspenders body text.
    expect(getByText(/alpha beta gamma/)).toBeInTheDocument();
  });

  it('clicking a row scrolls the matching timeline item into view', async () => {
    const items: Item[] = [
      makeItem({
        id: 'plan-abc',
        kind: 'proposed_plan',
        payloadId: 'pl-1',
        itemIndex: 0,
      }),
    ];
    const metas: PayloadMeta[] = [
      {
        id: 'pl-1',
        kind: 'proposed_plan',
        meta: JSON.stringify(makePlanMeta({ title: 'Scroll me' })),
        createdAt: 0,
      },
    ];
    const pane = await buildPane(items, metas);
    pane.setShowPlanSidebar(true);

    // Seed a matching anchor in the DOM.
    const anchor = document.createElement('div');
    anchor.setAttribute('data-item-id', 'plan-abc');
    let scrolled = false;
    anchor.scrollIntoView = (() => {
      scrolled = true;
    }) as unknown as HTMLElement['scrollIntoView'];
    document.body.appendChild(anchor);

    const { getByTestId } = render(PlanSidebar, { props: { pane } });
    await fireEvent.click(getByTestId('plan-sidebar-row'));
    expect(scrolled).toBe(true);

    document.body.removeChild(anchor);
  });

  it('surfaces a streaming indicator when pendingPlanUpdate is set', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    pane.setPendingPlanUpdate({ phase: 'drafting' });
    const { getByTestId } = render(PlanSidebar, { props: { pane } });
    expect(getByTestId('plan-sidebar-streaming').textContent ?? '').toMatch(/Plan updating/);
  });

  it('hides the streaming indicator when pendingPlanUpdate is null', async () => {
    const pane = await buildPane();
    pane.setShowPlanSidebar(true);
    const { queryByTestId } = render(PlanSidebar, { props: { pane } });
    expect(queryByTestId('plan-sidebar-streaming')).toBeNull();
  });
});
