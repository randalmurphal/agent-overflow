// Integration tests for windowed thread history at the full App-mount
// level. These tests pin the end-to-end contracts that the unit and
// component tests exercise in isolation:
//
//   1) Load Older button — click → ListItemsBeforeTurn → prepend lands
//      in the DOM and the button clears when the backend reports
//      no more history.
//   2) MessageSearch hit with an out-of-window itemId — click triggers
//      pane.switchThread followed by pane.requestScrollToItem, which
//      MessageTimeline's $effect turns into loadUntilItem + scroll.
//   3) PlanSidebar current plan — renders from the dedicated plan
//      binding even when that plan was authored outside the visible
//      timeline window.
//
// The component-level tests in MessageTimeline.test.ts /
// MessageSearch.test.ts already cover the individual mechanics.
// These tests add value by stitching the full App mount together so
// a regression that only shows up with the router, sidebar, and
// command registry wired in can't sneak past the unit layer.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { makeItem } from '../helpers/chat';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

/**
 * Two-phase mount. The caller can install default mocks via
 * `installThreadViewDefaults` during `phaseBeforeActivation`, then
 * OVERWRITE the bindings they care about before the thread click
 * triggers a fresh fetch. The default helper in `_helpers.ts` installs
 * defaults and mounts in one step, which would clobber any custom
 * `ListThreadSliceAround` / `ListThreadProposedPlans` mock the test
 * set beforehand.
 */
async function mountAndActivateThread(
  thread: Thread,
  overrideBindings: () => void,
) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);
  // Tests install the custom mocks now, AFTER the defaults are in
  // place. The activation click below triggers a fresh fetch that
  // picks these up.
  overrideBindings();

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration — windowed thread history', () => {
  beforeEach(() => {
    resetAppState();
    setBindingMock('SendMessageWithOptions', async () => makeThread({ id: 'thread-1' }));
    setBindingMock('InterruptTurn', async () => {});
  });

  it('loads older messages through the button and prepends them into the DOM', async () => {
    // Seed the thread so the initial-slice load returns a tail window
    // with hasMore=true. The MessageTimeline must render the "Load older
    // messages" control; the click path must flow through
    // pane.loadOlder → ListItemsBeforeTurn → store prepend, and the
    // newly loaded item must be visible in the timeline.
    let beforeCall!: ReturnType<typeof setBindingMock>;
    const { findByTestId, queryByTestId, findByText } = await mountAndActivateThread(
      makeThread({ title: 'Windowed Thread' }),
      () => {
        setBindingMock('ListThreadSliceAround', async () => ({
          items: [
            makeItem({
              id: 'tail-1',
              threadId: 'thread-1',
              turnIndex: 10,
              itemIndex: 0,
              kind: 'user_text',
              role: 'user',
              summary: 'tail user',
            }),
          ],
          oldestTurnIndex: 10,
          hasMore: true,
        }));
        beforeCall = setBindingMock('ListItemsBeforeTurn', async () => ({
          items: [
            makeItem({
              id: 'older-1',
              threadId: 'thread-1',
              turnIndex: 8,
              itemIndex: 0,
              kind: 'user_text',
              role: 'user',
              summary: 'older-user-text',
            }),
            makeItem({
              id: 'older-2',
              threadId: 'thread-1',
              turnIndex: 9,
              itemIndex: 0,
              kind: 'user_text',
              role: 'user',
              summary: 'between-user-text',
            }),
          ],
          oldestTurnIndex: 8,
          hasMore: false,
        }));
      },
    );

    // Control is visible because hasMore=true from the initial tail.
    const button = await findByTestId('load-older-messages');
    expect(button).toBeInTheDocument();

    await fireEvent.click(button);
    // loadOlder awaits the binding + tick; both settled-turn rehydration
    // and scroll-anchor tick are observed by flush().
    await waitFor(() => expect(beforeCall).toHaveBeenCalled());
    expect(beforeCall.mock.calls[0][0]).toBe('thread-1');
    expect(beforeCall.mock.calls[0][1]).toBe(10); // floor before the prepend

    // The prepended row appears in the timeline.
    await findByText('older-user-text');
    await findByText('between-user-text');

    // The button clears when the backend reports no more pages.
    await waitFor(() => expect(queryByTestId('load-older-messages')).toBeNull());

    // Store state agrees with the UI: the floor descends, hasMoreHistory
    // is false, and loadingOlder is back to idle.
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    expect(pane.oldestLoadedTurnIndex).toBe(8);
    expect(pane.hasMoreHistory).toBe(false);
    expect(pane.loadingOlder).toBe(false);
  });

  it('MessageSearch hit routes the thread switch + requestScrollToItem → loadUntilItem', async () => {
    // Seed a hit whose itemId is NOT in the initial window. The flow
    // must: open the dialog, switchThread, publish the nonce. The
    // live MessageTimeline's $effect then fires loadUntilItem — which
    // calls GetThreadItem (item out of window) and ListItemsBeforeTurn
    // to page the target in.
    let getItemCall!: ReturnType<typeof setBindingMock>;
    let beforeCall!: ReturnType<typeof setBindingMock>;
    const { findByTestId } = await mountAndActivateThread(
      makeThread({ title: 'Windowed Thread' }),
      () => {
        setBindingMock('ListThreadSliceAround', async () => ({
          items: [
            makeItem({
              id: 'tail-1',
              threadId: 'thread-1',
              turnIndex: 10,
              itemIndex: 0,
              summary: 'recent',
            }),
          ],
          oldestTurnIndex: 10,
          hasMore: true,
        }));
        setBindingMock('SearchThreadMessages', async () => [
          {
            threadId: 'thread-1',
            threadTitle: 'Windowed Thread',
            provider: 'claude',
            itemId: 'old-hit',
            turnIndex: 3,
            itemKind: 'text',
            itemRole: 'assistant',
            summary: 'old body',
            matchType: 'item' as const,
          },
        ]);
        getItemCall = setBindingMock('GetThreadItem', async (_threadId: string, id: string) =>
          makeItem({
            id,
            threadId: 'thread-1',
            turnIndex: 3,
            itemIndex: 0,
            summary: 'hit body',
          }),
        );
        beforeCall = setBindingMock('ListItemsBeforeTurn', async () => ({
          items: [
            makeItem({
              id: 'old-hit',
              threadId: 'thread-1',
              turnIndex: 3,
              itemIndex: 0,
              summary: 'hit body',
            }),
          ],
          oldestTurnIndex: 3,
          hasMore: false,
        }));
      },
    );

    // Spy on requestScrollToItem on the live pane so the assertion
    // doesn't race with the scroll $effect consuming the nonce.
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    const scrollSpy = vi.spyOn(pane, 'requestScrollToItem');

    // Open MessageSearch through the public store so we don't depend on
    // a platform-specific keybinding configuration.
    const searchMod = await import('../../lib/stores/messageSearch.svelte');
    searchMod.openMessageSearch();
    await flush();

    const input = (await findByTestId('message-search-input')) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'old' } });
    await flush();

    const hitBtn = await findByTestId('message-search-hit-thread-1-old-hit');
    await fireEvent.click(hitBtn);
    await flush(10);

    expect(scrollSpy).toHaveBeenCalledWith('old-hit');
    // MessageTimeline's $effect should have picked up the nonce and
    // routed through loadUntilItem. loadUntilItem fetches the target
    // item, then pages backward via ListItemsBeforeTurn.
    await waitFor(() => expect(getItemCall).toHaveBeenCalledWith('thread-1', 'old-hit'));
    await waitFor(() => expect(beforeCall).toHaveBeenCalled());
    // The paged-in window now covers turn 3 — the store state confirms
    // the window actually grew rather than stopping at a no-op.
    await waitFor(() => expect(pane.oldestLoadedTurnIndex).toBe(3));
    expect(pane.items.some((it) => it.id === 'old-hit')).toBe(true);
  });

  it('PlanSidebar renders the current plan from the dedicated plan list outside the timeline window', async () => {
    // The sidebar owns the plan list through a dedicated binding — it
    // does NOT read from pane.items. This pins the out-of-window case:
    // a plan from turn 2 must still render while the visible timeline
    // starts at turn 10.
    const { findAllByText } = await mountAndActivateThread(
      makeThread({ title: 'Windowed Thread' }),
      () => {
        setBindingMock('ListThreadSliceAround', async () => ({
          items: [
            makeItem({
              id: 'tail',
              threadId: 'thread-1',
              turnIndex: 10,
              itemIndex: 0,
              summary: 'recent',
            }),
          ],
          oldestTurnIndex: 10,
          hasMore: false,
        }));
        setBindingMock('ListThreadProposedPlans', async () => [
          makeItem({
            id: 'plan-deep',
            threadId: 'thread-1',
            turnIndex: 2, // below the window floor (10)
            itemIndex: 0,
            kind: 'tool_call',
            payloadId: 'pp1',
            payloadKind: 'proposed_plan',
            payloadMeta: JSON.stringify({
              title: 'Deep plan',
              preview: 'old plan from early in the thread',
              lineCount: 1,
              charCount: 30,
            }),
          }),
        ]);
        setBindingMock('GetPayloadData', async () => ({
          data: '# Deep plan\n\nold plan from early in the thread',
        }));
        setBindingMock('GetThreadItem', async (_t: string, id: string) =>
          makeItem({
            id,
            threadId: 'thread-1',
            turnIndex: 2,
            itemIndex: 0,
            summary: 'plan body',
          }),
        );
        setBindingMock('ListItemsBeforeTurn', async () => ({
          items: [],
          oldestTurnIndex: 2,
          hasMore: false,
        }));
      },
    );

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    pane.setShowPlanSidebar(true);
    await flush();

    await findAllByText('Deep plan');
  });
});
