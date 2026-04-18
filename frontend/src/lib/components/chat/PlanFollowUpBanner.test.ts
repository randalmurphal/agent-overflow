import { describe, expect, it, beforeAll } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import PlanFollowUpBanner from './PlanFollowUpBanner.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import type { Thread, Item } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

beforeAll(() => {
  // slide transition needs Element.animate.
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

async function buildPane(items: Item[] = []) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => items);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

function makeDraft() {
  // Use a real draft store with zero debounce so setContent changes are
  // observable immediately.
  setBindingMock('GetDraft', async (id: string) => ({
    threadId: id,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('SaveDraft', async () => {});
  const draft = createComposerDraftStore({ debounceMs: 0 });
  return draft;
}

describe('<PlanFollowUpBanner>', () => {
  it('hides when the pane has no items', async () => {
    const pane = await buildPane();
    const draft = makeDraft();
    const { queryByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    expect(queryByTestId('plan-followup-banner')).toBeNull();
  });

  it('hides when the latest item is an assistant text message, not a plan', async () => {
    const pane = await buildPane([
      makeItem({ id: 'p1', kind: 'proposed_plan', payloadId: 'pl-1', itemIndex: 0 }),
      makeItem({ id: 'a1', kind: 'message', summary: 'moving on', itemIndex: 1 }),
    ]);
    const draft = makeDraft();
    const { queryByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    expect(queryByTestId('plan-followup-banner')).toBeNull();
  });

  it('shows when the latest item is a proposed plan', async () => {
    const pane = await buildPane([
      makeItem({ id: 'u1', role: 'user', summary: 'plan this', itemIndex: 0 }),
      makeItem({ id: 'p1', kind: 'proposed_plan', payloadId: 'pl-1', itemIndex: 1 }),
    ]);
    const draft = makeDraft();
    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    expect(getByTestId('plan-followup-banner')).toBeInTheDocument();
  });

  it('Dismiss hides the banner for that specific plan', async () => {
    const pane = await buildPane([
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
    ]);
    const draft = makeDraft();
    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    expect(getByTestId('plan-followup-banner')).toBeInTheDocument();

    await fireEvent.click(getByTestId('plan-followup-dismiss'));
    expect(pane.dismissedPlanItemId).toBe('plan-A');
    // slide transition leaves the node inert until it finishes; asserting
    // inert covers both "hidden from a11y" and "cannot be clicked".
    expect(getByTestId('plan-followup-banner').hasAttribute('inert')).toBe(true);
  });

  it('dismissal is per-plan: a fresh proposed plan re-shows the banner', async () => {
    const pane = await buildPane([
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
    ]);
    const draft = makeDraft();
    pane.setDismissedPlanItemId('plan-A');

    const { queryByTestId, getByTestId, rerender } = render(PlanFollowUpBanner, {
      props: { pane, draft },
    });
    expect(queryByTestId('plan-followup-banner')).toBeNull();

    // A newer plan lands in the timeline — banner should re-appear because
    // the dismissed id no longer matches the latest item.
    setBindingMock('ListItems', async () => [
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
      makeItem({ id: 'plan-B', kind: 'proposed_plan', payloadId: 'pl-B', itemIndex: 1 }),
    ]);
    pane.finalizeTurn();
    // Let the ListItems promise resolve.
    await Promise.resolve();
    await Promise.resolve();
    // finalizeTurn also clears dismissedPlanItemId so plan-B is live.
    expect(pane.dismissedPlanItemId).toBeNull();

    await rerender({ pane, draft });
    expect(getByTestId('plan-followup-banner')).toBeInTheDocument();
  });

  it('Implement pre-fills the composer draft without sending', async () => {
    const pane = await buildPane([
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
    ]);
    const draft = makeDraft();
    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });

    expect(draft.content).toBe('');
    await fireEvent.click(getByTestId('plan-followup-implement'));
    expect(draft.content).toMatch(/Please implement the plan above/);
  });

  it('Implement appends to an existing draft instead of overwriting', async () => {
    const pane = await buildPane([
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
    ]);
    const draft = makeDraft();
    draft.setContent('Also do this');
    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-implement'));
    expect(draft.content).toMatch(/Also do this/);
    expect(draft.content).toMatch(/Please implement the plan above/);
  });

  it('Review scrolls the plan into view', async () => {
    const pane = await buildPane([
      makeItem({ id: 'plan-A', kind: 'proposed_plan', payloadId: 'pl-A', itemIndex: 0 }),
    ]);
    const draft = makeDraft();

    const anchor = document.createElement('div');
    anchor.setAttribute('data-item-id', 'plan-A');
    let scrolled = false;
    anchor.scrollIntoView = (() => {
      scrolled = true;
    }) as unknown as HTMLElement['scrollIntoView'];
    document.body.appendChild(anchor);

    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-review'));
    expect(scrolled).toBe(true);

    document.body.removeChild(anchor);
  });
});
