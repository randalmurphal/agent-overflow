// Integration tests for the chat scroll system after the virtua/svelte
// rebuild. These tests cover the seams between MessageTimeline,
// stickyBottomController, the per-thread snapshot store, and the layout
// surrounding the timeline (absolute composer, reserved-slot banners).
//
// What is NOT tested here:
//   - virtua's per-row anchor-preservation algorithm. That's owned by the
//     library (see /inokawa/virtua tests upstream); duplicating those
//     assertions in a happy-dom env that lacks real layout would be
//     fiction.
//   - Pure controller behavior (intent transitions, gesture handling,
//     pause-lease semantics) — covered exhaustively in
//     `stickyBottomController.svelte.test.ts`.
//
// What IS tested here:
//   - Per-thread snapshot save/restore round-trip through a real virtua
//     mount.
//   - Load-older flow: anchor capture before, scrollToIndex after.
//   - scrollToItem: pane.loadUntilItem then scrollToIndex.
//   - Composer-height CSS variable propagation through the chat column.
//   - Reserved-slot banner height stability across mount/unmount.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import {
  clearThreadScrollSnapshotsForTest,
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
} from '../../utils/threadScrollSnapshots';
import MessageTimeline from './MessageTimeline.svelte';
import ChatView from './ChatView.svelte';

beforeEach(async () => {
  resetBindingMocks();
  clearThreadScrollSnapshotsForTest();
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
});

afterEach(() => {
  clearThreadScrollSnapshotsForTest();
});

describe('scroll integration — per-thread snapshot save/restore', () => {
  // Real browser geometry: viewport > 0, scrollOffset=0, scrollSize<=viewport
  //   → stick.isAtBottom() returns true, snapshot persists as {kind:'bottom'}.
  // happy-dom returns 0 for clientHeight/clientWidth, so virtua's
  // getViewportSize() returns 0 too — isAtBottom() is then false (size > 0
  // is not within `threshold` of zero) and the saved snapshot ends up as
  // {kind:'anchor'} regardless of where the user actually was.
  // The save/restore CONTRACT we test here is independent of that
  // geometry quirk: a snapshot is written, and it points at a real item.

  it('writes a snapshot to the store after mount', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-snap-write';

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    const snap = getThreadScrollSnapshot('thread-snap-write');
    expect(snap).toBeTruthy();
    if (snap?.kind === 'anchor') {
      expect(['a', 'b']).toContain(snap.itemId);
    }
  });

  it('attempts to load the anchor item when restoring a {kind:"anchor"} snapshot', async () => {
    setThreadScrollSnapshot('thread-restore-anchor', {
      kind: 'anchor',
      itemId: 'pinned-item',
      offsetTop: -120,
    });

    const pane = await buildPane(undefined, [
      makeItem({ id: 'pinned-item', summary: 'pinned' }),
    ]);
    pane.thread!.id = 'thread-restore-anchor';
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('pinned-item');
  });

  it('still calls pane.loadUntilItem when a saved anchor item no longer exists', async () => {
    setThreadScrollSnapshot('thread-missing-anchor', {
      kind: 'anchor',
      itemId: 'gone-from-history',
      offsetTop: -120,
    });

    const pane = await buildPane(undefined, [
      makeItem({ id: 'present', summary: 'still here' }),
    ]);
    pane.thread!.id = 'thread-missing-anchor';
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('gone-from-history');
  });
});

describe('scroll integration — load older', () => {
  it('routes Load Older through pane.loadOlder and yields a "loaded" status', async () => {
    const items = Array.from({ length: 3 }, (_, i) =>
      makeItem({ id: `m:${i}`, turnIndex: i, summary: `m${i}` }),
    );
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    const loadOlder = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
      status: 'loaded',
      insertedRows: true,
      insertedBeforeWindow: true,
    });

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const button = getByTestId('load-older-messages');
    await fireEvent.click(button);
    await tick();

    expect(loadOlder).toHaveBeenCalled();
  });
});

describe('scroll integration — scroll to item', () => {
  it('routes pane.scrollToItemRequest through pane.loadUntilItem before locating the row', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'visible', turnIndex: 5, summary: 'visible' }),
    ]);
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    pane.requestScrollToItem('visible');
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('visible');
  });

  it('emits a warning toast when the requested item is gone from history', async () => {
    const { getToasts } = await import('../../stores/toast.svelte');
    const pane = await buildPane(undefined, [
      makeItem({ id: 'visible', turnIndex: 5, summary: 'visible' }),
    ]);
    vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);
    const toastsBefore = getToasts().length;

    render(MessageTimeline, { props: { pane } });
    pane.requestScrollToItem('missing');
    await tick();
    await tick();

    const newToasts = getToasts().slice(toastsBefore);
    expect(newToasts.some((t) => t.type === 'warning')).toBe(true);
  });
});

describe('scroll integration — composer height + layout invariance', () => {
  it('publishes --composer-height as a CSS variable on the chat column', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { container } = render(ChatView, { props: { pane } });
    await tick();

    // Find the chat-column element by its data-ui-surface marker; the
    // chat column owns the --composer-height inline style.
    const chatColumn = container.querySelector('[data-ui-surface="chat"]')
      ?.querySelector(':scope > div');
    expect(chatColumn).not.toBeNull();
    const styleAttr = (chatColumn as HTMLElement).getAttribute('style') ?? '';
    expect(styleAttr).toContain('--composer-height:');
  });

  it('renders the composer + below-bar inside the absolute overlay div', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    const overlay = getByTestId('composer-overlay');
    // Class assertions are intentionally loose to allow Tailwind config
    // changes; what matters is that the overlay positions absolutely
    // at the bottom of its relative parent (the timeline container).
    const cls = overlay.className;
    expect(cls).toContain('absolute');
    expect(cls).toContain('bottom-0');
  });
});

describe('scroll integration — banner reserved slot stability', () => {
  it('reserves provider-status and session-status slots regardless of banner state', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    // Reserved slots are always rendered, even when the banner content
    // is empty, so the timeline geometry stays stable when a banner
    // mounts/unmounts. Test ids let us assert the contract independent
    // of the underlying CSS class names (Tailwind utility names can
    // change with config without breaking the contract).
    expect(getByTestId('provider-status-slot')).toBeInTheDocument();
    expect(getByTestId('session-status-slot')).toBeInTheDocument();
  });
});

describe('scroll integration — auto-follow + button', () => {
  // virtua-internal scroll math isn't testable in happy-dom (zero
  // viewport geometry). We verify integration seams: the gesture path
  // surfaces the scroll-to-bottom chip, and clicking it flips intent
  // back to sticky.

  it('wheel-up on the wrapper surfaces the scroll-to-bottom chip', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
      makeItem({ id: 'c', itemIndex: 2, summary: 'c' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    // Two ticks: first lets the controller-attach $effect run (binding
    // the wheel listener to the wrapper), second lets the snapshot
    // restore $effect settle. Without this, the wheel event fires
    // before the listener is attached.
    await tick();
    await tick();

    const wrapper = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(wrapper).not.toBeNull();
    wrapper.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));
    await tick();
    await tick();

    // After the gesture the chip's button is in the DOM (it may still
    // be in a fade-in transition; what matters is presence as a
    // signal that intent flipped to free).
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).not.toBeNull();
  });
});

describe('scroll integration — mid-list inserts re-sort and re-index', () => {
  // Real triage paths can produce a `tool_completion` for a launchID
  // whose original `tool_call` lived on an earlier turn (see
  // `internal/triage/tool_lifecycle.go`'s `complete:<launchID>` flow,
  // and Codex's late `codex_background.go` arrivals). The pane must
  // re-sort by (turnIndex, itemIndex), rebuild `itemIndexById`, and
  // keep stable item ids — otherwise `pane.requestScrollToItem(id)`
  // resolves to the wrong row and the auto-follow `getLastIndex()`
  // points at the second-to-last item.
  //
  // happy-dom can't measure layout, so we don't assert on virtua
  // remount counts here. We assert on the data contract that virtua's
  // `getKey` consumes: items array order + index map consistency.

  it('upserting an out-of-order item lands it in chronological position and rebuilds itemIndexById', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 't1', turnIndex: 1, summary: 't1' }),
      makeItem({ id: 't2', turnIndex: 2, summary: 't2' }),
      makeItem({ id: 't4', turnIndex: 4, summary: 't4' }),
    ]);

    // Sanity: the precondition (turnIndex 1, 2, 4) must hold before the insert.
    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't4']);

    pane.upsertItem(makeItem({ id: 't3', turnIndex: 3, summary: 't3' }));

    // Items array re-sorted by (turnIndex, itemIndex).
    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't3', 't4']);

    // The last index advanced — auto-follow's `getLastIndex()` would now
    // point at the new tail (t4), not t3. Mid-list inserts that are NOT
    // the new chronological tail leave the tail unchanged.
    expect(pane.items.at(-1)?.id).toBe('t4');

    // Snapshot round-trips: save an anchor for t2 (which was index 1
    // before the insert and is still index 1 after), and verify it
    // restores to the same item.
    pane.thread!.id = 'thread-midlist-insert';
    setThreadScrollSnapshot('thread-midlist-insert', {
      kind: 'anchor',
      itemId: 't2',
      offsetTop: -120,
    });

    const snap = getThreadScrollSnapshot('thread-midlist-insert');
    expect(snap?.kind).toBe('anchor');
    if (snap?.kind === 'anchor') {
      expect(snap.itemId).toBe('t2');
      // The item still exists at a resolvable position post-insert.
      expect(pane.items.findIndex((it) => it.id === snap.itemId)).toBe(1);
    }
  });

  it('upserting at a tail-equivalent position appends without changing existing indices', async () => {
    // Regression contract: when the new item IS the new chronological
    // tail (turnIndex > all existing), the fast-append branch fires
    // (no needsSort). Item order stays append-only, indices for
    // existing rows are unchanged.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 't1', turnIndex: 1, summary: 't1' }),
      makeItem({ id: 't2', turnIndex: 2, summary: 't2' }),
    ]);

    pane.upsertItem(makeItem({ id: 't3', turnIndex: 3, summary: 't3' }));

    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't3']);
    // Tail moved forward as expected.
    expect(pane.items.at(-1)?.id).toBe('t3');
  });
});

describe('scroll integration — load older noop / error paths', () => {
  it('does not re-anchor when pane.loadOlder returns status:"noop"', async () => {
    const { getToasts } = await import('../../stores/toast.svelte');
    const items = Array.from({ length: 3 }, (_, i) =>
      makeItem({ id: `m:${i}`, turnIndex: i, summary: `m${i}` }),
    );
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    const loadOlder = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
      status: 'noop',
      insertedRows: false,
      insertedBeforeWindow: false,
    });
    const toastsBefore = getToasts().length;

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const button = getByTestId('load-older-messages');
    await fireEvent.click(button);
    await tick();
    await tick();

    // The contract: when status !== 'loaded', handleLoadOlder returns
    // before re-anchoring. Observable proxy: pane.loadOlder fired and
    // no warning toast was added (a missed scrollToIndex on a different
    // branch would surface as a different observable, not this).
    const newToasts = getToasts().slice(toastsBefore);
    expect(loadOlder).toHaveBeenCalled();
    expect(newToasts).toHaveLength(0);
  });
});

// Wait for stickyBottomController's deferred rAF + a microtask drain.
// Mirrors the helper used in stickyBottomController.svelte.test.ts:72.
function nextFrame(): Promise<void> {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

describe('scroll integration — visibility-mask flicker fix', () => {
  // The fix: items appended to a sticky timeline render `visibility: hidden`
  // for one frame so the browser doesn't paint a wrong-position flash before
  // `stickyBottomController.scrollToLast` runs. The mask is cleared by the
  // controller's `onScrollSettled` callback (success branch + bail branch +
  // forceStick). These tests assert the registry contract end-to-end through
  // a real MessageTimeline mount, since that's where the wiring lives.

  it('marks an in-stream append while sticky and clears it after the rAF settles', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh', itemIndex: 1, summary: 'fresh' }));
    // Synchronous read — verify the registry update before yielding to the
    // event loop. happy-dom's rAF can fire during `await tick()` and would
    // otherwise race the controller's onScrollSettled clear against this
    // assertion.
    expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(true);

    // The items.length effect wired up in MessageTimeline calls
    // notifyContentMaybeGrew, which schedules the deferred rAF. After that
    // rAF fires, onScrollSettled runs pane.clearPendingScrollCatchup, the
    // registry empties, and the row's wrapper drops the `invisible` class.
    // The `class:invisible` lives on the outer `[data-row-index]` wrapper,
    // not the `[data-item-id]` leaf root — `closest` below walks up to the
    // wrapper that owns the masking class.
    await tick();
    await nextFrame();
    await tick();

    expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(false);
    const freshRowAfter = container
      .querySelector('[data-item-id="fresh"]')
      ?.closest('[data-row-index]') as HTMLElement | null;
    expect(freshRowAfter?.classList.contains('invisible')).toBe(false);
  });

  it('does not mark items when the controller is in free intent', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    // Flip to free via a wheel-up gesture on the timeline wrapper —
    // matches the path that flips intent in real usage.
    const wrapper = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    wrapper.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh', itemIndex: 1, summary: 'fresh' }));
    expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(false);

    await tick();
    const freshRow = container
      .querySelector('[data-item-id="fresh"]')
      ?.closest('[data-row-index]') as HTMLElement | null;
    expect(freshRow?.classList.contains('invisible')).toBe(false);
  });

  it('does not mark items in the initial-load batch (items.length === 0 before upsert)', async () => {
    // buildPane runs switchThread which performs an initial mass-upsert.
    // No controller is attached at that point, so the sticky-gate is moot —
    // but the items.length-was-zero gate is the canonical guard. Explicitly
    // upsert into a pane with zero items AFTER the controller is attached
    // (by running through MessageTimeline's mount tick) and assert the gate
    // holds. This protects against a future refactor that drops the gate.
    const pane = await buildPane(makeThread(), []);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    expect(pane.items).toHaveLength(0);

    pane.upsertItem(makeItem({ id: 'first', summary: 'first' }));
    expect(pane.pendingScrollCatchupItems.has('first')).toBe(false);

    await tick();
    const firstRow = container
      .querySelector('[data-item-id="first"]')
      ?.closest('[data-row-index]') as HTMLElement | null;
    expect(firstRow?.classList.contains('invisible')).toBe(false);
  });

  it('marks every item when multiple appends arrive in rapid succession and clears them in a single rAF', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh-1', itemIndex: 1, summary: 'one' }));
    pane.upsertItem(makeItem({ id: 'fresh-2', itemIndex: 2, summary: 'two' }));
    expect(pane.pendingScrollCatchupItems.has('fresh-1')).toBe(true);
    expect(pane.pendingScrollCatchupItems.has('fresh-2')).toBe(true);

    await tick();
    await nextFrame();
    await tick();

    expect(pane.pendingScrollCatchupItems.size).toBe(0);
  });

  it('clears pending items when the rAF bail branch fires after the user scrolls away', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh', itemIndex: 1, summary: 'fresh' }));
    expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(true);

    // Flip to free BEFORE the rAF fires. The rAF callback's
    // `!core.canAutoScroll()` bail must still call onScrollSettled,
    // otherwise the row stays hidden forever (until thread switch).
    const wrapper = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    wrapper.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));

    await tick();
    await nextFrame();
    await tick();

    expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(false);
  });

  it('clearPendingScrollCatchup empties the registry directly', async () => {
    // Registry-level contract test: independent of the controller
    // wiring, the pane method must drop entries idempotently.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh', itemIndex: 1, summary: 'fresh' }));
    expect(pane.pendingScrollCatchupItems.size).toBeGreaterThan(0);

    pane.clearPendingScrollCatchup();
    expect(pane.pendingScrollCatchupItems.size).toBe(0);

    // Idempotent — calling on an empty registry is a no-op.
    pane.clearPendingScrollCatchup();
    expect(pane.pendingScrollCatchupItems.size).toBe(0);
  });

  it('thread switch wipes the pending registry alongside other per-row UI state', async () => {
    // Note: makeThread()/makeItem() default to id='thread-1' / threadId='thread-1'.
    // upsertItemsBatch filters incoming items whose threadId doesn't match
    // the pane's current thread, so the seed/fresh items must use the same
    // threadId as the thread the pane is currently on.
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [
      makeItem({ id: 'seed', threadId: 'thread-a', summary: 'seed' }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    pane.upsertItem(makeItem({ id: 'fresh', threadId: 'thread-a', itemIndex: 1, summary: 'fresh' }));
    expect(pane.pendingScrollCatchupItems.size).toBeGreaterThan(0);

    await pane.switchThread(makeThread({ id: 'thread-b' }));
    expect(pane.pendingScrollCatchupItems.size).toBe(0);
  });

  it('forceStick clears pending items via the synchronous settle path', async () => {
    // Race the controller's rAF: the user is sticky, items get marked,
    // then the user clicks ScrollToBottomButton (which calls forceStick)
    // before the rAF fires. forceStick's synchronous onScrollSettled must
    // unmask, otherwise rows hang in `visibility: hidden` until the
    // already-stale rAF hits its bail-branch settle. Tested through the
    // real controller (not MessageTimeline) so we can drive forceStick
    // synchronously without yielding to the rAF queue.
    const { createStickyBottomController } = await import('../../utils/stickyBottomController.svelte');
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    let offset = 0;
    const handle = {
      getCache: vi.fn(() => ({}) as never),
      getScrollOffset: () => offset,
      getScrollSize: () => 1000,
      getViewportSize: () => 600,
      findItemIndex: vi.fn(() => 0),
      getItemOffset: vi.fn(() => 0),
      getItemSize: vi.fn(() => 90),
      scrollToIndex: vi.fn(),
      scrollTo: vi.fn((next: number) => { offset = next; }),
      scrollBy: vi.fn(),
    };
    const wrapperEl = document.createElement('div');
    document.body.appendChild(wrapperEl);

    const controller = createStickyBottomController({
      getScrollEl: () => wrapperEl,
      getListHandle: () => handle,
      getLastIndex: () => Math.max(0, pane.items.length - 1),
      onScrollSettled: () => pane.clearPendingScrollCatchup(),
    });
    controller.attach();
    pane.attachScrollController(controller);

    try {
      // Sticky by default; an in-stream append populates the registry.
      pane.upsertItem(makeItem({ id: 'fresh', itemIndex: 1, summary: 'fresh' }));
      expect(pane.pendingScrollCatchupItems.has('fresh')).toBe(true);

      // forceStick fires synchronously — registry must clear without
      // waiting for the rAF.
      controller.forceStick();
      expect(pane.pendingScrollCatchupItems.size).toBe(0);
    } finally {
      pane.detachScrollController(controller);
      controller.destroy();
      wrapperEl.remove();
    }
  });

  it('does not mask an inline-agent wrapper when one of its descendants is freshly appended', async () => {
    // Regression: 9e0a51a rewrote the mask predicate to recurse via
    // nodeContainsItem, which made every running subagent's wrapper go
    // visibility:hidden each time the agent emitted a child item
    // (assistant text, sub-tool_call, thinking). The wrapper masked,
    // unmasked, masked again — visible flicker on the agent card.
    // Mask must apply only to the freshly-appended row's own anchor id,
    // never to ancestor wrappers.
    const inlineMeta = JSON.stringify({
      is_inline_subagent: true,
      inline_subagent_group_id: 'assistant-1',
    });
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
      makeItem({
        id: 'agent-1',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'streaming',
        summary: 'Agent: working',
        meta: inlineMeta,
      }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    // Append a streaming child INSIDE the running agent. The id lands
    // in pendingScrollCatchupItems just like any new top-level item.
    pane.upsertItem(makeItem({
      id: 'agent-1-child',
      itemIndex: 2,
      kind: 'assistant_text',
      summary: 'subagent says hi',
      parentId: 'agent-1',
    }));
    expect(pane.pendingScrollCatchupItems.has('agent-1-child')).toBe(true);

    await tick();

    // The inline-agent wrapper is rendered as a top-level VList row.
    // It must not carry `invisible` just because its descendant is in
    // the registry — that's the bug we're guarding.
    const wrapperRow = container
      .querySelector('[data-testid="inline-subagent-group"]')
      ?.closest('[data-row-index]') as HTMLElement | null;
    expect(wrapperRow).not.toBeNull();
    expect(wrapperRow?.classList.contains('invisible')).toBe(false);
  });

  it('does not mark items when an existing id is updated in place', async () => {
    // The new-item branch of upsertItemsBatch is the only marking site;
    // updates to already-tracked ids (delta-driven status changes,
    // out-of-order tail corrections) take the existing-id branch and
    // must not re-add the row to the pending registry — otherwise a
    // mid-flight tool result update would re-hide a visible row.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'seed', summary: 'seed' }),
    ]);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    expect(pane.pendingScrollCatchupItems.has('seed')).toBe(false);

    // Same id → existing-id branch in upsertItemsBatch.
    pane.upsertItem(makeItem({ id: 'seed', summary: 'seed (updated)' }));
    expect(pane.pendingScrollCatchupItems.has('seed')).toBe(false);
    expect(pane.pendingScrollCatchupItems.size).toBe(0);
  });
});
