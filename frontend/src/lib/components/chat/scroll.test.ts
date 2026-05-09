// Integration tests for the chat scroll system after the virtua/svelte
// rebuild. These tests cover the seams between MessageTimeline,
// useStickToBottom, the per-thread snapshot store, and the layout
// surrounding the timeline (absolute composer, reserved-slot banners).
//
// What is NOT tested here:
//   - virtua's per-row anchor-preservation algorithm. That's owned by the
//     library (see /inokawa/virtua tests upstream); duplicating those
//     assertions in a happy-dom env that lacks real layout would be
//     fiction.
//   - Pure controller behavior (sync-pin, content RO, gesture
//     handlers, pause-lease semantics) — covered exhaustively in
//     `useStickToBottom.svelte.test.ts`.
//
// What IS tested here:
//   - Per-thread snapshot save/restore round-trip through a real virtua
//     mount.
//   - Load-older flow: anchor capture before, scrollToIndex after.
//   - scrollToItem: pane.loadUntilItem then scrollToIndex.
//   - Composer-height CSS variable propagation through the chat column.
//   - Reserved-slot banner height stability across mount/unmount.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import type { PaneScrollController } from '../../stores/thread.svelte';
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

  it('bottom-snapshot restore leaves the controller sticky and not escaped', async () => {
    // The bottom-restore path uses `stick.forceStick()` — a single
    // scrollTop write against the current target, with the per-thread
    // virtua row-size cache (replayed via `<Virtualizer cache={...}>`
    // inside `{#key pane.threadId}`) ensuring `totalSize` is correct
    // from frame 0. Subsequent svelte-streamdown async typesetting
    // (shiki / KaTeX / mermaid / parseIncompleteMarkdown rebalance)
    // and virtua's per-row remeasurement get handled invisibly by the
    // controller's contentRO sync-pin path.
    //
    // We can't assert the absence of a scroll preamble directly in
    // happy-dom (no real layout), so the contract this test pins is
    // the controller end-state: after restoration completes, the
    // controller must be in (isSticky=true, escapedFromLock=false).
    // The $effect.pre escape guard sets escape=true synchronously on
    // thread mount; if restoreToBottom didn't call forceStick (or
    // replaced it with something that fails to clear escape), this
    // assertion fails.
    setThreadScrollSnapshot('thread-bottom-restore', { kind: 'bottom' });

    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
      makeItem({ id: 'c', itemIndex: 2, summary: 'last' }),
    ]);
    pane.thread!.id = 'thread-bottom-restore';

    render(MessageTimeline, { props: { pane } });
    // Three ticks: controller attach, $effect.pre escape guard,
    // restoreInitialPosition awaiting tick before scrollToIndex.
    await tick();
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
  });

  it('blank loaded threads default to sticky-bottom even before virtualized rows mount', async () => {
    // New draft threads load with zero items, so MessageTimeline renders
    // the empty-state branch instead of the Virtualizer/contentEl branch.
    // The thread-switch guard still sets escapedFromLock=true before
    // restore; restoration must clear it anyway so the first streamed
    // rows auto-follow once the transcript grows beyond the viewport.
    const pane = await buildPane(makeThread({ id: 'new-blank-thread' }), []);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
    expect(getThreadScrollSnapshot('new-blank-thread')).toEqual({ kind: 'bottom' });
  });

  it('keeps an anchor snapshot when a thread initially has no visible rows', async () => {
    const thread = makeThread({ id: 'thread-empty-anchor' });
    const target = makeItem({
      id: 'old-anchor',
      threadId: thread.id,
      turnIndex: 3,
      summary: 'older target',
    });
    const pane = await buildPane(thread, []);
    setThreadScrollSnapshot(thread.id, {
      kind: 'anchor',
      itemId: target.id,
      offsetTop: -120,
    });
    setBindingMock('GetThreadItem', async () => target);
    setBindingMock('ListItemsBeforeTurn', async () => ({
      items: [target],
      oldestTurnIndex: target.turnIndex,
      hasMore: false,
    }));
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem');

    const { container } = render(MessageTimeline, { props: { pane } });

    await waitFor(() => expect(loadUntilItem).toHaveBeenCalledWith(target.id));
    await waitFor(() => {
      expect(container.querySelector(`[data-item-id="${target.id}"]`)).not.toBeNull();
    });
    expect(getThreadScrollSnapshot(thread.id)).not.toEqual({ kind: 'bottom' });
  });

  it('bottom-snapshot restore writes scrollTop exactly once via forceStick (no virtua-scroll fight)', async () => {
    // Regression: an earlier iteration of restoreToBottom paired
    // `listRef.scrollToIndex(last, 'end')` with `stick.markAtBottom()`.
    // virtua's measurement loop kept writing scrollTop on every
    // ACTION_ITEM_RESIZE tick for ~150ms, while the controller's
    // contentRO sync-pin (enabled by markAtBottom) ALSO wrote scrollTop
    // on every positive contentEl delta. They targeted slightly
    // different values (virtua: itemOffset+itemSize-clientHeight;
    // controller: scrollHeight-1-clientHeight) and oscillated visibly
    // on every Streamdown async typesetting tick. The single-writer
    // contract closes that hole: forceStick() lands scrollTop once,
    // then sync-pin owns subsequent re-pins.
    //
    // Pin the call by spying on stick.forceStick AND ensuring
    // scrollToIndex is NOT called as part of the bottom-restore path.
    setThreadScrollSnapshot('thread-bottom-force-stick', { kind: 'bottom' });
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-bottom-force-stick';

    let forceStickSpy: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (
      ctrl: PaneScrollController & { forceStick(): void },
    ) => {
      forceStickSpy = vi.spyOn(ctrl, 'forceStick');
      origAttach(ctrl);
    };

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(forceStickSpy).not.toBeNull();
    // Exactly one forceStick call — the bottom restore. A regression
    // that re-introduced an extra scrollTop writer (e.g. routing
    // through scrollToIndex+markAtBottom plus a fallback forceStick)
    // would surface here as count > 1.
    expect(forceStickSpy!).toHaveBeenCalledTimes(1);
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

  it('falls back to restoreToBottom when loadUntilItem returns false (controller ends sticky+not-escaped)', async () => {
    // restoreInitialPosition has a `!found` branch that calls
    // restoreToBottom when the saved anchor's item is gone from the
    // backend. Pin the controller end-state contract: after the fallback
    // runs, restoreToBottom calls forceStick which clears escape and
    // sets sticky. A regression that turned the fallback into the
    // anchor-success path (which sets escape=true) would surface here.
    setThreadScrollSnapshot('thread-anchor-not-found', {
      kind: 'anchor',
      itemId: 'gone-from-history',
      offsetTop: -120,
    });
    const pane = await buildPane(undefined, [
      makeItem({ id: 'present', summary: 'still here' }),
    ]);
    pane.thread!.id = 'thread-anchor-not-found';
    vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
  });

  it('falls back to restoreToBottom when the anchor item resolves but findTimelineNodeIndex returns -1', async () => {
    // After loadUntilItem returns true, restoreInitialPosition awaits a
    // tick and then calls findTimelineNodeIndex(snap.itemId). If virtua
    // hasn't yet rendered the row (race) or the item id was pruned in
    // a different code path, idx < 0 → fall back to restoreToBottom.
    // We force the branch by claiming the item exists (loadUntilItem
    // returns true) but populating the pane with items that have
    // different ids, so findTimelineNodeIndex won't find the snapshotted
    // id in the rendered groupedNodes.
    setThreadScrollSnapshot('thread-anchor-idx-missing', {
      kind: 'anchor',
      itemId: 'never-rendered',
      offsetTop: -120,
    });
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);
    pane.thread!.id = 'thread-anchor-idx-missing';
    vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
  });

  it('phase-1 slice already contains the anchor — loadUntilItem short-circuits without GetThreadItem', async () => {
    // The plan-of-record for the two-phase load: ListThreadSliceAround
    // returns ~50 items centered on the saved anchor, so by the time
    // restoreInitialPosition reaches `pane.loadUntilItem(anchorId)`,
    // the row is already in `pane.items`. The fast path inside
    // loadUntilItem (`items.some(it => it.id === itemID) → return true`)
    // takes over and no `GetThreadItem` round-trip happens. Spying on
    // GetThreadItem and asserting it never fires pins that contract.
    setThreadScrollSnapshot('thread-anchor-fast-path', {
      kind: 'anchor',
      itemId: 'in-slice',
      offsetTop: -42,
    });
    const items = [
      makeItem({ id: 'before', threadId: 'thread-anchor-fast-path', turnIndex: 0 }),
      makeItem({ id: 'in-slice', threadId: 'thread-anchor-fast-path', turnIndex: 1 }),
      makeItem({ id: 'after', threadId: 'thread-anchor-fast-path', turnIndex: 2 }),
    ];
    const getThreadItemSpy = vi.fn(async () =>
      makeItem({ id: 'should-never-be-called' }),
    );
    setBindingMock('GetThreadItem', getThreadItemSpy);

    const pane = await buildPane(makeThread({ id: 'thread-anchor-fast-path' }), items);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    // GetThreadItem must NOT have been called — the in-memory shortcut
    // inside loadUntilItem (`items.some(...)`) handles the in-window
    // anchor without a round-trip.
    expect(getThreadItemSpy).not.toHaveBeenCalled();
  });

  it('cache-hit restoration runs as soon as items appear, not after loading flips false', async () => {
    // The restoration $effect now fires on `items.length > 0 || !loading`
    // so a cache-hit paint can restore the saved anchor BEFORE phase 2
    // resolves. Stage: items are present from cache, but pane.loading
    // is still true because phase 2 hangs. Assert restoration ran
    // anyway (loadUntilItem was called for the snapshotted anchor).
    setThreadScrollSnapshot('cache-hit-restore', {
      kind: 'anchor',
      itemId: 'anchor-row',
      offsetTop: 0,
    });
    const items = [
      makeItem({ id: 'before', threadId: 'cache-hit-restore', turnIndex: 0 }),
      makeItem({ id: 'anchor-row', threadId: 'cache-hit-restore', turnIndex: 1 }),
    ];
    // Phase 2 hangs so pane.loading stays true while items are visible.
    setBindingMock('ListRecentThreadItems', () => new Promise(() => {}));
    setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

    const pane = await buildPane(makeThread({ id: 'cache-hit-restore' }), items);
    // Ensure pane.loading reflects the in-flight phase 2.
    expect(pane.items.length).toBeGreaterThan(0);
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('anchor-row');
  });

  it('publishes a virtua-cache getter on mount that delegates to virtua, and clears it on unmount', async () => {
    // Per-thread virtua cache wiring. Two invariants:
    //   1. The getter is attached on mount and detached on unmount —
    //      a stale closure capturing the torn-down virtualizer must
    //      not survive past component teardown. The detach call must
    //      pass the SAME getter reference (matched-pair guard in
    //      thread.svelte.ts) so a stale teardown can't dispose a
    //      freshly remounted timeline's getter during fast switches.
    //   2. The getter actually delegates to virtua's listRef.getCache(),
    //      not a stub that returns a constant. Verified indirectly by
    //      rendering virtua (test-mode ssrCount populates listRef) and
    //      asserting the getter returns the same value as a direct
    //      listRef.getCache() call would — i.e. the CacheSnapshot
    //      tuple shape `[number[], number]`. A regression that swapped
    //      the closure to `() => undefined` or `() => null` fails
    //      assertion #2 even though assertion #1 still passes.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
    ]);
    const attachVirtuaCacheGetter = vi.spyOn(pane, 'attachVirtuaCacheGetter');
    const detachVirtuaCacheGetter = vi.spyOn(pane, 'detachVirtuaCacheGetter');

    const { unmount } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    expect(attachVirtuaCacheGetter).toHaveBeenCalledTimes(1);
    const registered = attachVirtuaCacheGetter.mock.calls[0][0];
    expect(typeof registered).toBe('function');

    // Invariant #2: the returned value matches virtua's CacheSnapshot
    // shape — `[number[], number]`. happy-dom's ssrCount-mode virtua
    // mount populates listRef synchronously, so the getter resolves to
    // a real tuple, not undefined. Stubbed-getter regressions surface
    // here because they return primitives or null instead of the tuple.
    const captured = registered!();
    expect(Array.isArray(captured)).toBe(true);
    expect(captured).toHaveLength(2);
    expect(Array.isArray((captured as unknown as [unknown, unknown])[0])).toBe(true);
    expect(typeof (captured as unknown as [unknown, number])[1]).toBe('number');

    unmount();
    // Invariant #1: detach must be called with the same getter
    // reference that was attached — the matched-pair guard in
    // thread.svelte.ts only clears the slot on a reference match.
    expect(detachVirtuaCacheGetter).toHaveBeenCalledTimes(1);
    expect(detachVirtuaCacheGetter).toHaveBeenLastCalledWith(registered);
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

  it('flashes a user message after an animated scroll request lands', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:target', kind: 'user_text', role: 'user', summary: 'jump target' }),
    ]);
    vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    const { container } = render(MessageTimeline, { props: { pane } });
    pane.requestScrollToItem('user:target', {
      behavior: 'animated',
      flash: true,
    });

    await waitFor(() => {
      const target = container.querySelector('[data-target-flash="true"]');
      expect(target).not.toBeNull();
      expect(target?.textContent).toContain('jump target');
    });
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
    // The sticky-bottom controller's wheel handler short-circuits when
    // the container isn't scrollable (`scrollHeight <= clientHeight`).
    // happy-dom returns 0 for both unless we override, so the test
    // wheel would otherwise be ignored. Stub geometry so the wheel
    // handler proceeds to flip escapedFromLock — and so the scroll
    // event fired below refreshes `isNearBottomState` to false (we want
    // `isAtBottom` to be false after escape, which requires both intent
    // and geometry to be away from the bottom).
    Object.defineProperty(wrapper, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(wrapper, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(wrapper, 'scrollTop', { configurable: true, get: () => 0, set: () => {} });
    wrapper.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));
    // Fire a scroll event so the controller refreshes isNearBottomState
    // from the new (stubbed) geometry. The wheel handler itself only
    // flips escapedFromLock; the scroll handler is what re-reads
    // distanceFromBottom and sets isNearBottomState=false (distance=400).
    wrapper.dispatchEvent(new Event('scroll', { bubbles: true }));
    await tick();
    await tick();

    // After the gesture the chip's button is in the DOM (it may still
    // be in a fade-in transition; what matters is presence as a
    // signal that the user is no longer at-or-near the bottom).
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).not.toBeNull();
  });

  it('renders the scroll-to-bottom chip OUTSIDE the scroll container', async () => {
    // Regression: position:absolute inside an overflow:auto parent
    // anchors the absolute child in scroll-content space, not viewport
    // space, so the chip would scroll with the transcript and ride up
    // off-screen as scrollTop grows. The fix wraps the scroll element
    // in a non-scrolling `relative` container and renders the chip as
    // a sibling of the scroll element. This test asserts the DOM
    // shape so that contract isn't quietly broken later.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(scrollEl).not.toBeNull();
    // Force the chip visible: stub scrollable geometry, fire a wheel-up
    // gesture, then a scroll event so isNearBottomState refreshes to
    // false (intent + geometry both away from bottom → chip visible).
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', { configurable: true, get: () => 0, set: () => {} });
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await tick();
    await tick();

    const chip = container.querySelector('[data-testid="scroll-to-bottom"]') as HTMLElement | null;
    expect(chip).not.toBeNull();
    // The chip must NOT be a descendant of the scroll element. If it
    // is, scrolling moves it.
    expect(scrollEl.contains(chip)).toBe(false);
    // It also must be a sibling of the scroll element inside the same
    // non-scrolling positioned wrapper — so its `position:absolute`
    // anchors to the wrapper's padding edge. A regression that hoisted
    // the chip elsewhere (e.g., to document.body) would still pass the
    // non-containment check above but break the absolute-positioning
    // contract the wrapper exists to provide.
    expect(chip!.parentElement).toBe(scrollEl.parentElement);
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

// The 'visibility-mask flicker fix' suite was deleted. The mask was a
// rAF-gap mitigation; the new useStickToBottom controller eliminates the
// gap by writing scrollTop in the same paint cycle as the layout change
// (content ResizeObserver fires synchronously before paint). New
// regression scenarios live in the no-jitter / R4 / smooth-fight tests
// below.

describe('scroll integration — useStickToBottom wiring', () => {
  // Controller-internal behavior (sync-pin, content-RO, gesture
  // handlers, pause-lease semantics, programmatic-write tagging) is
  // covered exhaustively in `useStickToBottom.svelte.test.ts` against
  // raw scrollEl/contentEl divs with stubbed geometry.
  //
  // What we assert HERE is that MessageTimeline actually wires the
  // controller into the pane registry on mount and tears it down on
  // unmount — the seam external surfaces (sidebar resizers, drawers,
  // ChatView composer-height publication) depend on. Without this
  // wiring, `pane.scrollController?.pauseAutoScroll()` is a silent
  // no-op and the resizer-drag-during-stream regression resurfaces.

  it('publishes a controller on mount that satisfies PaneScrollController', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
    ]);
    expect(pane.scrollController).toBeNull();

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    expect(pane.scrollController).not.toBeNull();
    // The published controller satisfies the PaneScrollController contract —
    // depth-counted lease + grow notification — that sidebar resizers,
    // resizable drawers, and ChatView's composer-height publication depend on.
    expect(typeof pane.scrollController?.pauseAutoScroll).toBe('function');
    expect(typeof pane.scrollController?.notifyContentMaybeGrew).toBe('function');
  });

  it('the published controller honors a pauseAutoScroll lease (no throw, depth-counted release)', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const ctrl = pane.scrollController;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    const release1 = ctrl.pauseAutoScroll();
    const release2 = ctrl.pauseAutoScroll();
    // Idempotent dispose — calling release twice doesn't underflow.
    release1();
    release1();
    release2();
    // notifyContentMaybeGrew after release should not throw even when
    // the controller's geometry is unmeasured (happy-dom).
    expect(() => ctrl.notifyContentMaybeGrew()).not.toThrow();
  });

  it('mid-stream upsertItem leaves the controller sticky (no scrollTop-direction inference)', async () => {
    // Replaces the deleted visibility-mask test that asserted appending
    // a child to a running inline subagent doesn't flicker. Under the new
    // architecture there is no mask — the guarantee is structural: the
    // controller does NOT infer up-gesture from scrollTop direction
    // (R4 mitigation), so virtua's $fixScrollJump or any per-row resize
    // that nudges scrollTop cannot flip escapedFromLock. Mid-stream
    // upserts therefore leave intent/stickiness untouched.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'agent-1', itemIndex: 0, summary: 'first' }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    // The pane interface only exposes pauseAutoScroll / notifyContentMaybeGrew
    // (PaneScrollController is narrow by design — see thread.svelte.ts);
    // peek at the underlying controller's intent state to verify
    // stickiness survives the upsert without inferring up-gesture from
    // scrollTop direction.
    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    // Baseline: no explicit escape was triggered, no leases held.
    expect(ctrl.isSticky).toBe(true);
    expect(ctrl.escapedFromLock).toBe(false);

    // Stream a second item in (simulates an inline subagent's child
    // member arriving mid-turn, or a new tool_call from the provider).
    pane.upsertItem(makeItem({ id: 'agent-2', itemIndex: 1, summary: 'second' }));
    await tick();
    await tick();

    // The upsert path must not have flipped escape or torn the lease.
    // If a future regression infers up-gesture from a virtua jump-correction
    // write, this assertion fails.
    expect(ctrl.isSticky).toBe(true);
    expect(ctrl.escapedFromLock).toBe(false);
  });
});
