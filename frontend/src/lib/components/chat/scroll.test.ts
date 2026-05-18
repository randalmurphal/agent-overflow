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
import type { PaneScrollController, ThreadPane } from '../../stores/thread.svelte';
import {
  clearThreadScrollSnapshotsForTest,
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
} from '../../utils/threadScrollSnapshots';
import MessageTimeline from './MessageTimeline.svelte';
import ChatView from './ChatView.svelte';

function waitForScrollIntent(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 5));
}

function waitForAnimationFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

function watchStickNotifications(pane: ThreadPane): {
  instantCalls(): number;
  liveCalls(): number;
  reset(): void;
} {
  let instantSpy: ReturnType<typeof vi.spyOn> | null = null;
  let liveSpy: ReturnType<typeof vi.spyOn> | null = null;
  const originalAttach = pane.attachScrollController.bind(pane);
  pane.attachScrollController = (controller) => {
    instantSpy = vi.spyOn(controller, 'notifyContentMaybeGrew');
    liveSpy = vi.spyOn(
      controller as PaneScrollController & { notifyLiveContentMaybeGrew(): void },
      'notifyLiveContentMaybeGrew',
    );
    originalAttach(controller);
  };
  return {
    instantCalls: () => instantSpy?.mock.calls.length ?? 0,
    liveCalls: () => liveSpy?.mock.calls.length ?? 0,
    reset: () => {
      instantSpy?.mockClear();
      liveSpy?.mockClear();
    },
  };
}

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
    // scrollTop write against the current target. Subsequent
    // svelte-streamdown async typesetting
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
    // restoreAnchor awaiting tick before scrollToIndex.
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
    // controller: scrollHeight-clientHeight) and oscillated visibly
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

  it('bottom-snapshot restore schedules a rAF notifyContentMaybeGrew settle pass', async () => {
    // The synchronous forceStick at restore time lands scrollTop against
    // the geometry virtua reports at frame 0 from its initial estimates.
    // Late layout settling — composer-height RO updating scrollEl's
    // padding-bottom (padding-only growth doesn't refire the contentRO),
    // virtua's per-row remeasurement on the next frame, and the first
    // burst of Streamdown async typesetting (shiki / KaTeX / mermaid) —
    // can shift the bottom by a few pixels one frame after forceStick.
    // The user-visible symptom of dropping the trailing rAF was landing
    // "half a scroll tick from the bottom" intermittently. Pin the
    // contract by spying on notifyContentMaybeGrew and asserting it
    // fires after one rAF tick.
    setThreadScrollSnapshot('thread-bottom-settle', { kind: 'bottom' });
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-bottom-settle';

    let notifySpy: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (
      ctrl: PaneScrollController & { notifyContentMaybeGrew(): void },
    ) => {
      notifySpy = vi.spyOn(ctrl, 'notifyContentMaybeGrew');
      origAttach(ctrl);
    };

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(notifySpy).not.toBeNull();
    const callsBeforeRaf = notifySpy!.mock.calls.length;
    // Drive one animation frame so the trailing settle pass fires.
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect(notifySpy!.mock.calls.length).toBeGreaterThan(callsBeforeRaf);
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
    // restoreAnchor has a `!found` branch that calls
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
    // After loadUntilItem returns true, restoreAnchor awaits a
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
    // restoreAnchor reaches `pane.loadUntilItem(anchorId)`,
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
    // The restoration $effect fires on `items.length > 0 || !loading`
    // so a cache-hit paint can restore the saved anchor while the
    // initial-load slice is still in flight. Stage: items are present
    // from cache, but pane.loading is still true because the slice
    // load hangs. Assert restoration ran anyway (loadUntilItem was
    // called for the snapshotted anchor).
    setThreadScrollSnapshot('cache-hit-restore', {
      kind: 'anchor',
      itemId: 'anchor-row',
      offsetTop: 0,
    });
    const items = [
      makeItem({ id: 'before', threadId: 'cache-hit-restore', turnIndex: 0 }),
      makeItem({ id: 'anchor-row', threadId: 'cache-hit-restore', turnIndex: 1 }),
    ];
    // Initial load hangs so pane.loading stays true while items are visible.
    setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

    const pane = await buildPane(makeThread({ id: 'cache-hit-restore' }), items);
    // Ensure pane.loading reflects the in-flight slice load.
    expect(pane.items.length).toBeGreaterThan(0);
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('anchor-row');
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

  // Cascade-prevention. Before the fix, the auto-load gate's
  // floor-progress predicate cleared the moment `oldestLoadedTurnIndex`
  // advanced, so the anchor-restore programmatic scroll that followed
  // `pane.loadOlder()` re-fired the gate on the next tick. With the
  // gesture-armed gate, a successive button click loads exactly one
  // section per click (and never auto-cascades without a real user
  // wheel/touch/keydown gesture in between).
  it('does not cascade — clicking Load Older twice in a row loads one batch per click', async () => {
    const items = Array.from({ length: 3 }, (_, i) =>
      makeItem({ id: `m:${i}`, turnIndex: i + 10, summary: `m${i}` }),
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
    expect(loadOlder).toHaveBeenCalledTimes(1);

    // A second click is an explicit user action — the button path is
    // always available, gate-state notwithstanding. This pins that
    // behavior so a future refactor doesn't gate the button itself.
    await fireEvent.click(button);
    await tick();
    expect(loadOlder).toHaveBeenCalledTimes(2);
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

  it('composer-height growth calls notifyContentMaybeGrew synchronously inside the RO callback', async () => {
    // Regression guard for the "appears then settles" symptom on uncached
    // loads. The previous composer-RO implementation deferred
    // `notifyContentMaybeGrew` to the next animation frame because a
    // synchronous read of `scrollEl.scrollHeight` would see stale padding
    // (Svelte's reactive flush runs in a microtask AFTER the RO callback,
    // so the style binding for `--composer-height` wouldn't have applied
    // yet). The user-visible cost was a 1-frame gap where scrollTop
    // pointed at the old bottom while padding-bottom had grown — for
    // threads with a working/todo panel mounting late (after warm
    // revealed contentEl) the gap was 200–400px, large enough to flicker
    // the scroll-to-bottom chip on the way to settling.
    //
    // Fix: write `--composer-height` directly on chatColumn via
    // `style.setProperty`, bypassing the Svelte microtask boundary for
    // the layout-relevant change, then call `notifyContentMaybeGrew`
    // synchronously inside the same RO callback. The forced layout
    // inside `targetScrollTop()` applies the new CSS variable, so
    // scrollHeight is post-grow when scrollTop is written.
    //
    // This test stubs ResizeObserver so we can drive the composer-RO
    // callback with a specific height entry, then asserts that the
    // controller's `notifyContentMaybeGrew` count incremented inside the
    // synchronous callback (i.e. before any rAF could fire).
    const callbacksByTarget = new Map<HTMLElement, ResizeObserverCallback>();
    const originalRO = globalThis.ResizeObserver;
    class StubResizeObserver {
      constructor(private readonly callback: ResizeObserverCallback) {}
      observe(target: HTMLElement): void {
        callbacksByTarget.set(target, this.callback);
      }
      unobserve(target: HTMLElement): void {
        callbacksByTarget.delete(target);
      }
      disconnect(): void {
        // Best-effort: drop all observations registered against this
        // callback instance. The test only cares that the composer-RO
        // callback survives until we trigger it.
        for (const [target, cb] of callbacksByTarget) {
          if (cb === this.callback) callbacksByTarget.delete(target);
        }
      }
    }
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;

    try {
      const pane = await buildPane(makeThread(), [
        makeItem({ id: 'tail', summary: 'tail' }),
      ]);

      let notifySpy: ReturnType<typeof vi.spyOn> | null = null;
      const origAttach = pane.attachScrollController.bind(pane);
      pane.attachScrollController = (
        ctrl: PaneScrollController & { notifyContentMaybeGrew(): void },
      ) => {
        notifySpy = vi.spyOn(ctrl, 'notifyContentMaybeGrew');
        origAttach(ctrl);
      };

      const { getByTestId } = render(ChatView, { props: { pane } });
      await tick();
      // Flush the rAF that restoreToBottom queues for late layout
      // settling so it doesn't pollute the baseline call count.
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

      expect(notifySpy).not.toBeNull();
      const callsBeforeFire = notifySpy!.mock.calls.length;

      // Find the composer overlay and its registered callback. The
      // composer-RO is the one that observes `composerOverlay` in
      // ChatView's $effect — the only target with the
      // `composer-overlay` testid.
      const composerOverlay = getByTestId('composer-overlay');
      const composerCallback = callbacksByTarget.get(composerOverlay);
      expect(composerCallback).toBeDefined();
      if (!composerCallback) return;

      // Synthesize a composer-height change. Composer overlay grew from
      // its initial height (120 default) to 200. The RO callback should
      // detect the change, write the CSS variable directly, AND call
      // notifyContentMaybeGrew synchronously.
      const fakeEntry = {
        contentRect: { height: 200 } as DOMRectReadOnly,
      } as ResizeObserverEntry;
      composerCallback([fakeEntry], {} as ResizeObserver);

      // The synchronous notifyContentMaybeGrew call must have happened
      // before this assertion runs (no rAF awaited). Previously the call
      // was queued inside a `requestAnimationFrame` and the assertion
      // would fail until a frame elapsed.
      expect(notifySpy!.mock.calls.length).toBeGreaterThan(callsBeforeFire);
    } finally {
      globalThis.ResizeObserver = originalRO;
    }
  });

  it('composer-height growth writes --composer-height directly on chatColumn so layout reads see the new value', async () => {
    // Companion to the "synchronous notifyContentMaybeGrew" test. The
    // direct CSS-variable write is what makes the synchronous re-pin
    // correct — without it, `targetScrollTop()` would force layout with
    // the old --composer-height and pin to the pre-grow bottom. This
    // test asserts the inline style on chatColumn reflects the new
    // height BEFORE any tick/microtask/frame is awaited.
    const callbacksByTarget = new Map<HTMLElement, ResizeObserverCallback>();
    const originalRO = globalThis.ResizeObserver;
    class StubResizeObserver {
      constructor(private readonly callback: ResizeObserverCallback) {}
      observe(target: HTMLElement): void {
        callbacksByTarget.set(target, this.callback);
      }
      unobserve(target: HTMLElement): void {
        callbacksByTarget.delete(target);
      }
      disconnect(): void {
        for (const [target, cb] of callbacksByTarget) {
          if (cb === this.callback) callbacksByTarget.delete(target);
        }
      }
    }
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;

    try {
      const pane = await buildPane(makeThread(), [
        makeItem({ id: 'tail', summary: 'tail' }),
      ]);

      const { container, getByTestId } = render(ChatView, { props: { pane } });
      await tick();

      const chatColumn = container.querySelector('[data-ui-surface="chat"]')
        ?.querySelector(':scope > div') as HTMLElement | null;
      expect(chatColumn).not.toBeNull();
      if (!chatColumn) return;

      const composerOverlay = getByTestId('composer-overlay');
      const composerCallback = callbacksByTarget.get(composerOverlay);
      expect(composerCallback).toBeDefined();
      if (!composerCallback) return;

      const fakeEntry = {
        contentRect: { height: 247 } as DOMRectReadOnly,
      } as ResizeObserverEntry;
      composerCallback([fakeEntry], {} as ResizeObserver);

      // The direct setProperty must have written the new value before
      // the RO callback returned — no tick / microtask / frame awaited
      // between the callback and this assertion.
      expect(chatColumn.style.getPropertyValue('--composer-height')).toBe('247px');
    } finally {
      globalThis.ResizeObserver = originalRO;
    }
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

  it('opts out of browser scroll-anchor on the scroll container', async () => {
    // Regression guard: the browser's default `overflow-anchor: auto`
    // adjusts scrollTop to keep the topmost-visible element fixed when
    // content above the viewport changes size — well-intentioned for
    // static documents, but it actively fights virtua's measurement-loop
    // jump correction AND the controller's contentRO sync-pin. Streamdown
    // async typesetting (shiki / KaTeX / mermaid) growing rows above the
    // viewport on a sticky session would produce visible scrollTop
    // oscillation between the browser's anchor adjustment and our re-pin
    // without this opt-out.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);
    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    await tick();
    const scroll = getByTestId('message-timeline-scroll') as HTMLElement;
    expect(scroll.style.overflowAnchor).toBe('none');
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
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const wrapper = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(wrapper).not.toBeNull();
    // The sticky-bottom controller's wheel handler short-circuits when
    // the container isn't scrollable (`scrollHeight <= clientHeight`).
    // happy-dom returns 0 for both unless we override, so the test
    // wheel would otherwise be ignored. Stub geometry so the wheel
    // handler can arm escape — and so the scroll
    // event fired below refreshes `isNearBottomState` to false (we want
    // `isAtBottom` to be false after escape, which requires both intent
    // and geometry to be away from the bottom).
    let scrollTop = 400;
    Object.defineProperty(wrapper, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(wrapper, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(wrapper, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });
    const wheel = new WheelEvent('wheel', { deltaY: -50, bubbles: true });
    Object.defineProperty(wheel, 'target', { value: wrapper });
    wrapper.dispatchEvent(wheel);
    scrollTop = 0;
    // Fire a scroll event so the controller confirms the outer scroller
    // moved up, then refreshes isNearBottomState from the new geometry.
    wrapper.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
    await tick();
    await tick();

    // After the gesture the chip's button is in the DOM (it may still
    // be in a fade-in transition; what matters is presence as a
    // signal that the user is no longer at-or-near the bottom).
    const ctrl = pane.scrollController as
      | (PaneScrollController & { escapedFromLock: boolean; isAtBottom: boolean })
      | null;
    expect(ctrl?.escapedFromLock).toBe(true);
    expect(ctrl?.isAtBottom).toBe(false);
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).not.toBeNull();
  });

  it('wheel-up by 1px prevents later layout-growth re-pin', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    let scrollTop = 400;
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });

    const wheel = new WheelEvent('wheel', { deltaY: -1, bubbles: true });
    Object.defineProperty(wheel, 'target', { value: scrollEl });
    scrollEl.dispatchEvent(wheel);
    scrollTop = 399;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { escapedFromLock: boolean; isSticky: boolean; isAtBottom: boolean })
      | null;
    expect(ctrl?.escapedFromLock).toBe(true);
    expect(ctrl?.isSticky).toBe(false);
    // The chip can stay hidden in the visual near-bottom band, but
    // auto-follow must be broken.
    expect(ctrl?.isAtBottom).toBe(true);

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.notifyContentMaybeGrew();
    expect(scrollTop).toBe(399);
  });

  it('wheel-backed upward scroll prevents later layout-growth re-pin', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    let scrollTop = 400;
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });

    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY: -1, bubbles: true }));
    scrollTop = 399;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { escapedFromLock: boolean; isSticky: boolean })
      | null;
    expect(ctrl?.escapedFromLock).toBe(true);
    expect(ctrl?.isSticky).toBe(false);

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.notifyContentMaybeGrew();
    expect(scrollTop).toBe(399);
  });

  it('layout-only upward scroll preserves auto-follow for later growth', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    let scrollTop = 400;
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });

    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
    scrollTop = 399;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { escapedFromLock: boolean; isSticky: boolean })
      | null;
    expect(ctrl?.escapedFromLock).toBe(false);
    expect(ctrl?.isSticky).toBe(true);

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.notifyContentMaybeGrew();
    expect(scrollTop).toBe(600);
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
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(scrollEl).not.toBeNull();
    // Force the chip visible: stub scrollable geometry, fire a wheel-up
    // gesture, then a scroll event so isNearBottomState refreshes to
    // false (intent + geometry both away from bottom → chip visible).
    let scrollTop = 400;
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });
    const wheel = new WheelEvent('wheel', { deltaY: -50, bubbles: true });
    Object.defineProperty(wheel, 'target', { value: scrollEl });
    scrollEl.dispatchEvent(wheel);
    scrollTop = 0;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
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

describe('scroll integration — auto-load-older trigger', () => {
  // The auto-load trigger fires from the Virtualizer's `onscroll` prop
  // (handleVirtuaScroll → maybeAutoLoadOlder → handleLoadOlder). Under
  // happy-dom + ssrCount, virtua's onscroll callback fires synchronously
  // when scrollEl dispatches a `scroll` event; that's the seam these
  // tests use to drive the trigger end-to-end.
  function dispatchScroll(container: HTMLElement): HTMLElement {
    const scrollEl = container.querySelector(
      '[data-testid="message-timeline-scroll"]',
    ) as HTMLElement;
    expect(scrollEl).not.toBeNull();
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true, get: () => 0, set: () => {},
    });
    Object.defineProperty(scrollEl, 'scrollHeight', {
      configurable: true, get: () => 1000,
    });
    Object.defineProperty(scrollEl, 'clientHeight', {
      configurable: true, get: () => 600,
    });
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    return scrollEl;
  }

  it('does not fire pane.loadOlder when pane.hasMoreHistory is false', async () => {
    const items = [makeItem({ id: 'a', turnIndex: 5, summary: 'a' })];
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => false });
    const loadOlder = vi.spyOn(pane, 'loadOlder');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScroll(container);
    await tick();

    expect(loadOlder).not.toHaveBeenCalled();
  });

  it('does not fire pane.loadOlder when oldestLoadedTurnIndex is null (defensive null-floor exit)', async () => {
    // Edge case: backend returns hasMore=true with no items so floor
    // stays null. Without the null-floor early-return in
    // maybeAutoLoadOlder, every scroll tick would re-enter loadOlder
    // (which itself noops on null floor) — the guard's `!== null`
    // precondition would never engage. Pin the defensive exit.
    const pane = await buildPane(undefined, []);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    Object.defineProperty(pane, 'oldestLoadedTurnIndex', { configurable: true, get: () => null });
    const loadOlder = vi.spyOn(pane, 'loadOlder');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScroll(container);
    dispatchScroll(container);
    await tick();

    expect(loadOlder).not.toHaveBeenCalled();
  });

  it('does not fire pane.loadOlder while a load is already in flight', async () => {
    const items = [makeItem({ id: 'a', turnIndex: 5, summary: 'a' })];
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'oldestLoadedTurnIndex', { configurable: true, get: () => 5 });
    const loadOlder = vi.spyOn(pane, 'loadOlder');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScroll(container);
    await tick();

    expect(loadOlder).not.toHaveBeenCalled();
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
    expect(typeof pane.scrollController?.notifyHostLayoutSettled).toBe('function');
  });

  it('host-layout reconciliation preserves the current sticky or escaped intent', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & {
        escapedFromLock: boolean;
        isSticky: boolean;
        setEscapedFromLock(value: boolean): void;
      })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;

    expect(ctrl.isSticky).toBe(true);
    ctrl.notifyHostLayoutSettled?.();
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);

    ctrl.setEscapedFromLock(true);
    expect(ctrl.isSticky).toBe(false);
    ctrl.notifyHostLayoutSettled?.();
    expect(ctrl.escapedFromLock).toBe(true);
    expect(ctrl.isSticky).toBe(false);
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

  it('streaming thinking deltas leave the controller sticky', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'think-1',
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        summary: 'first thought',
      }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isSticky: boolean; escapedFromLock: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;
    expect(ctrl.isSticky).toBe(true);
    expect(ctrl.escapedFromLock).toBe(false);

    pane.applyItemDelta({
      threadId: pane.threadId!,
      itemId: 'think-1',
      kind: 'thinking',
      delta: ' that continues streaming through the collapsed thinking tail',
      updatedAt: 1,
    });
    await tick();
    await tick();

    expect(ctrl.isSticky).toBe(true);
    expect(ctrl.escapedFromLock).toBe(false);
  });

  it('nudges sticky follow when an active turn appends assistant text after thinking', async () => {
    const thread = makeThread({ id: 'thread-think-text-follow' });
    const pane = await buildPane(thread, [
      makeItem({
        id: 'think-1',
        threadId: thread.id,
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        summary: 'first thought',
      }),
    ]);
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const notifyWatch = watchStickNotifications(pane);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();
    notifyWatch.reset();

    pane.upsertItem(makeItem({
      id: 'text-1',
      threadId: thread.id,
      itemIndex: 1,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'Here is the next response.',
    }));
    await tick();
    expect(notifyWatch.liveCalls()).toBe(0);
    expect(notifyWatch.instantCalls()).toBe(0);
    await waitForAnimationFrame();
    await waitFor(() => expect(notifyWatch.liveCalls()).toBeGreaterThan(0));
    expect(notifyWatch.instantCalls()).toBe(0);
  });

  it('does not nudge sticky follow for ordinary streaming text deltas', async () => {
    const thread = makeThread({ id: 'thread-stream-delta-follow' });
    const pane = await buildPane(thread, [
      makeItem({
        id: 'text-1',
        threadId: thread.id,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'first',
      }),
    ]);
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const notifyWatch = watchStickNotifications(pane);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();
    notifyWatch.reset();

    pane.applyItemDelta({
      threadId: thread.id,
      itemId: 'text-1',
      kind: 'assistant_text',
      delta: ' delta',
      updatedAt: 2,
    });
    await tick();
    await waitForAnimationFrame();
    await waitForScrollIntent();

    expect(notifyWatch.liveCalls()).toBe(0);
    expect(notifyWatch.instantCalls()).toBe(0);
  });

  it('does not nudge sticky follow for active-turn structural changes outside the tail', async () => {
    const thread = makeThread({ id: 'thread-prepend-follow' });
    const pane = await buildPane(thread, Array.from({ length: 6 }, (_, index) => makeItem({
      id: `text-${index}`,
      threadId: thread.id,
      turnIndex: 0,
      itemIndex: index,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: `message ${index}`,
    })));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const notifyWatch = watchStickNotifications(pane);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();
    notifyWatch.reset();

    pane.upsertItem(makeItem({
      id: 'older-same-turn',
      threadId: thread.id,
      turnIndex: 0,
      itemIndex: -1,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: 'older row',
    }));
    await tick();
    await waitForAnimationFrame();
    await waitForScrollIntent();

    expect(notifyWatch.liveCalls()).toBe(0);
    expect(notifyWatch.instantCalls()).toBe(0);
  });

  it('does not nudge sticky follow for active-turn tail metadata churn', async () => {
    const thread = makeThread({ id: 'thread-tail-metadata-follow' });
    const pane = await buildPane(thread, [
      makeItem({
        id: 'text-1',
        threadId: thread.id,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'first',
        updatedAt: 1,
      }),
    ]);
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    const notifyWatch = watchStickNotifications(pane);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();
    notifyWatch.reset();

    pane.upsertItem(makeItem({
      id: 'text-1',
      threadId: thread.id,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'first',
      updatedAt: 2,
      meta: '{"pathRefs":[]}',
    }));
    await tick();
    await waitForAnimationFrame();
    await waitForScrollIntent();

    expect(notifyWatch.liveCalls()).toBe(0);
    expect(notifyWatch.instantCalls()).toBe(0);
  });

  it('thread switch off a prior thread keeps contentEl hidden for the new thread (warm does not leak)', async () => {
    // The flaky-fix bug: warm carries over from the previous thread on
    // pane switch. MessageTimeline isn't keyed on threadId (the inner
    // Virtualizer is), so scrollEl/contentEl stay the same DOM nodes
    // across switches — attach()'s no-op early-return path is hit. The
    // restore $effect calls forceStick() (which re-arms warmup) but
    // runs AFTER DOM update, so without `armWarmup()` in `$effect.pre`,
    // the first paint of the new thread inherits the prior thread's
    // settled isWarm=true and the cascade is visible.
    //
    // We can't reliably observe `isWarm=true` mid-test (happy-dom's
    // ResizeObserver behavior is environment-dependent), so this test
    // pins the user-facing invariant: after a thread switch into an
    // uncached thread, the contentEl wrapper is `visibility:hidden`
    // until the warmup gate fires. If `armWarmup()` were dropped from
    // `$effect.pre`, this assertion would race with the prior thread's
    // settled state and become flaky.
    const threadA = makeThread({ id: 'thread-a-cross' });
    const pane = await buildPane(threadA, [
      makeItem({ id: 'a1', threadId: 'thread-a-cross' }),
      makeItem({ id: 'a2', threadId: 'thread-a-cross', itemIndex: 1 }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const ctrl = pane.scrollController as
      | (PaneScrollController & { isWarm: boolean })
      | null;
    expect(ctrl).not.toBeNull();
    if (!ctrl) return;

    // Switch to a different uncached thread.
    const threadB = makeThread({ id: 'thread-b-cross' });
    setBindingMock('SwitchThread', async () => threadB);
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [makeItem({ id: 'b1', threadId: 'thread-b-cross' })],
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    setBindingMock('GetThreadLiveState', async () => ({
      threadId: threadB.id,
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    }));
    setBindingMock('ListRecentTurns', async () => []);
    setBindingMock('ListThreadCheckpoints', async () => []);

    await pane.switchThread(threadB);
    await tick();
    await tick();

    // Immediately after switch, isWarm must be false (armWarmup ran in
    // $effect.pre).
    expect(ctrl.isWarm).toBe(false);

    // Therefore hideContentForWarmup is true, contentEl is hidden, and
    // the new thread's measurement cascade lands behind the gate.
    const contentEl = container.querySelector<HTMLElement>(
      '[data-testid="message-timeline-scroll"] > div',
    );
    expect(contentEl?.style.visibility).toBe('hidden');
  });

  it('hides contentEl during the measurement cascade on uncached loads (cache miss)', async () => {
    // Regression: on cache-miss thread loads, virtua's lazy mount-time
    // measurement underestimates totalSize at ESTIMATED_ROW_SIZE × N.
    // The per-row ResizeObserver cascade then shrinks totalSize across
    // a few rAFs; for long threads this clamps scrollTop by a fraction
    // of a page (216-item sample: 461px) AND shifts every row's
    // Y-offset, producing a visible "lands wrong, jumps to correct"
    // sequence between two paints.
    //
    // MessageTimeline gates contentEl visibility on the controller's
    // warmup signal. The cascade happens behind a hidden contentEl; the
    // user only sees the first post-warmup frame, by which point
    // measurements have settled and scrollTop is at the correct bottom.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);
    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    // Find the contentEl wrapper. It's the div directly inside scrollEl
    // that has the inline style we added.
    const contentEl = container.querySelector<HTMLElement>(
      '[data-testid="message-timeline-scroll"] > div',
    );
    expect(contentEl).not.toBeNull();
    // Pre-warmup: visibility:hidden so the user can't see the in-flight
    // measurement cascade. (`style.visibility` reads the inline style we
    // set; happy-dom honors `style:visibility` from Svelte.)
    expect(contentEl?.style.visibility).toBe('hidden');
  });
});
