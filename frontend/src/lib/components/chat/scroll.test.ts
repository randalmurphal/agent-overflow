// Integration tests for the chat scroll system. These tests cover the
// seams between MessageTimeline, useStickToBottom, the per-thread
// snapshot store, and the layout surrounding the timeline (absolute
// composer, banner overlays).
//
// What is NOT tested here:
//   - The windowing engine's geometry math (size store, window
//     computation, compensation) — covered exhaustively in
//     `utils/virtual/*.test.ts` and the browser suite.
//   - Pure controller behavior (sync-pin, content RO, input-intent
//     handlers, pause-lease semantics) — covered exhaustively in
//     `utils/scroll/index.svelte.test.ts`.
//
// What IS tested here:
//   - Per-thread snapshot save/restore round-trip through a real
//     TimelineVirtualizer mount.
//   - Load-older flow: anchor capture before, scrollToIndex after.
//   - scrollToItem: pane.loadUntilItem then scrollToIndex.
//   - Composer-height CSS variable propagation through the chat column.
//   - Reserved-slot banner height stability across mount/unmount.

import { afterEach, beforeEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { projectTurnStarted, projectTurnCompleted } from '../../stores/threadStatuses.svelte';
import type { PaneScrollController, ThreadPane } from '../../stores/thread.svelte';
import {
  clearThreadScrollSnapshotsForTest,
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
} from '../../utils/threadScrollSnapshots';
import {
  clearAllThreadSizePriorsForTest,
  getReplayableSizePriors,
  peekThreadSizePriorsForTest,
} from '../../utils/virtual/priors';
import * as sizePriorsModule from '../../utils/virtual/priors';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import MessageTimeline from './MessageTimeline.svelte';
import ChatView from './ChatView.svelte';

// The pane-facing controller is a narrow adapter (MessageTimeline's
// `paneScrollController` literal): it exposes only pauseAutoScroll /
// observe / preserveScrollAnchor / preserveTimelineWindowAnchor, and the
// timeline's internal nudges call the underlying stick controller
// directly. Tests that need to read controller state (isSticky,
// escapedFromLock, …) or spy on internal calls capture the stick at
// creation time via this factory wrap.
const { createdStickControllers } = vi.hoisted(() => ({
  // Type annotations are erased at runtime, so referencing the imported
  // type inside the hoisted factory is safe.
  createdStickControllers: [] as import('../../utils/scroll/index.svelte').UseStickToBottomController[],
}));
vi.mock('../../utils/scroll/index.svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../utils/scroll/index.svelte')>();
  const wrappedCreate: typeof mod.createUseStickToBottomController = (options) => {
    const controller = mod.createUseStickToBottomController(options);
    createdStickControllers.push(controller);
    return controller;
  };
  return { ...mod, createUseStickToBottomController: wrappedCreate };
});

// The single stick controller created by the mounted timeline.
// MessageTimeline creates its stick during component init, before any
// effect attaches the pane adapter, so by the time a test (or an
// attachScrollController intercept) runs, the capture is already
// populated. Deliberately throws on more than one capture: every
// current test mounts exactly one timeline, and a future multi-pane
// test that trips this must index the capture array explicitly rather
// than inherit a silent "most recent wins" ambiguity.
function lastStick(): UseStickToBottomController {
  if (createdStickControllers.length > 1) {
    throw new Error(
      `lastStick() is ambiguous: ${createdStickControllers.length} controllers were created this test — index createdStickControllers explicitly`,
    );
  }
  const controller = createdStickControllers[0];
  if (!controller) throw new Error('no useStickToBottom controller has been created yet');
  return controller;
}

function waitForScrollIntent(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 5));
}

function waitForAnimationFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

function installResizeObserverCapture(): {
  callbacksByTarget: Map<HTMLElement, ResizeObserverCallback>;
  restore(): void;
} {
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

  return {
    callbacksByTarget,
    restore() {
      globalThis.ResizeObserver = originalRO;
    },
  };
}

function watchStickNotifications(pane: ThreadPane): {
  instantCalls(): number;
  liveCalls(): number;
  structuralMarks(): number;
  reset(): void;
} {
  let observeSpy: ReturnType<typeof vi.spyOn> | null = null;
  let structuralSpy: ReturnType<typeof vi.spyOn> | null = null;
  const originalAttach = pane.attachScrollController.bind(pane);
  pane.attachScrollController = (controller) => {
    // Spy on the stick, not the attached adapter: the timeline's internal
    // nudges (structural live-content effect, restore settle pass) call
    // stick.observe(...) directly and never cross the adapter.
    const stick = lastStick();
    observeSpy = vi.spyOn(stick, 'observe');
    structuralSpy = vi.spyOn(stick, 'markStructuralContentPending');
    originalAttach(controller);
  };
  const observeCalls = (kind: string): number =>
    observeSpy?.mock.calls.filter((call: unknown[]) => call[0] === kind).length ?? 0;
  return {
    instantCalls: () => observeCalls('content'),
    liveCalls: () => observeCalls('live-content'),
    structuralMarks: () => structuralSpy?.mock.calls.length ?? 0,
    reset: () => {
      observeSpy?.mockClear();
      structuralSpy?.mockClear();
    },
  };
}

beforeEach(async () => {
  createdStickControllers.length = 0;
  resetBindingMocks();
  clearThreadScrollSnapshotsForTest();
  clearAllThreadSizePriorsForTest();
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
});

afterEach(() => {
  clearThreadScrollSnapshotsForTest();
  clearAllThreadSizePriorsForTest();
});

describe('scroll integration — per-thread snapshot save/restore', () => {
  // Real browser geometry: viewport > 0, scrollOffset=0, scrollSize<=viewport
  //   → stick.isAtBottom() returns true, snapshot persists as {kind:'bottom'}.
  // happy-dom returns 0 for clientHeight/clientWidth, so the engine's
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

  // The measured-size priors are what let a revisited thread skip the
  // estimate→measure cascade (the thread-switch flicker). This proves the
  // wiring: a mount that settles persists a replayable entry stamped with the
  // thread's validity key. The actual replay-at-final-height behavior is
  // proven against the engine in utils/virtual/engine.test.ts (it can't be
  // observed in happy-dom, which reports zero geometry).
  it('persists replayable size priors for a thread after mount settles', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-size-cache';

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    // Persist fired once warm-up settled AND restoration completed (the
    // effect depends on both — see the ordering note in MessageTimeline).
    const entry = peekThreadSizePriorsForTest('thread-size-cache');
    expect(entry).toBeTruthy();
    // One slot per rendered row (measured or not).
    expect(entry?.sizes).toHaveLength(2);
    // The structure signature encodes both leaves (the reproducible key that
    // replaced the inert monotonic revision counter).
    expect(entry?.structureSig).toContain('L:a:');
    expect(entry?.structureSig).toContain('L:b:');
    expect(entry?.expansionSig).toBe('');

    // It round-trips through the validity gate with its captured key…
    expect(
      getReplayableSizePriors('thread-size-cache', {
        width: entry!.width,
        structureSig: entry!.structureSig,
        expansionSig: entry!.expansionSig,
      }),
    ).toBe(entry!.sizes);

    // …and a stale key (items changed → different structure) refuses the
    // replay → per-row estimate fallback.
    expect(
      getReplayableSizePriors('thread-size-cache', {
        width: entry!.width,
        structureSig: entry!.structureSig + '\nL:c:completed:1:0',
        expansionSig: entry!.expansionSig,
      }),
    ).toBeUndefined();
  });

  // The decisive regression: actually switch away and back, and prove the
  // replay resolves on return. The predecessor cache was once stamped with
  // `pane.timelineRevision`, a monotonic per-pane counter never restored on a
  // cache-hit re-entry — so every revisit computed a strictly-greater revision
  // than capture and the resolver always returned undefined (the architecture
  // trace saw revision 2→4→5 across A→B→A; severing the replay arm left the
  // whole scroll suite green). The structure signature is reproducible
  // for A's unchanged rows, so the settled sizes replay on return instead of
  // re-running the estimate→measure cascade. RED on the old key, GREEN now.
  it('resolves the replay priors when switching away and back (A→B→A)', async () => {
    const resolveSpy = vi.spyOn(sizePriorsModule, 'getReplayableSizePriors');
    // Restore via onTestFinished so a failed assertion below can't leak the spy
    // (which calls through) into sibling tests — the suite has no global mock restore.
    onTestFinished(() => resolveSpy.mockRestore());
    const aItems = [
      makeItem({ id: 'a0', threadId: 'thread-aba-a', summary: 'alpha one' }),
      makeItem({ id: 'a1', threadId: 'thread-aba-a', itemIndex: 1, summary: 'alpha two' }),
    ];
    const pane = await buildPane(makeThread({ id: 'thread-aba-a' }), aItems);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();

    // First visit captured A's settled size priors.
    const firstEntry = peekThreadSizePriorsForTest('thread-aba-a');
    expect(firstEntry).toBeTruthy();
    const firstSig = firstEntry!.structureSig;

    // Switch to an uncached thread B.
    const threadB = makeThread({ id: 'thread-aba-b' });
    const bItems = [makeItem({ id: 'b0', threadId: threadB.id, summary: 'beta' })];
    setBindingMock('SwitchThread', async () => threadB);
    setBindingMock('ListThreadSliceAround', async () => ({
      items: bItems,
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    await pane.switchThread(threadB);
    await tick();
    await tick();
    await waitForAnimationFrame();

    // Switch back to A — a threadItemCache hit, so A's node structure (and thus
    // the signature) reproduces exactly.
    resolveSpy.mockClear();
    setBindingMock('SwitchThread', async () => makeThread({ id: 'thread-aba-a' }));
    setBindingMock('ListThreadSliceAround', async () => ({
      items: aItems,
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    await pane.switchThread(makeThread({ id: 'thread-aba-a' }));
    await tick();
    await tick();
    await waitForAnimationFrame();

    // The component asked the resolver for A on the return and got a defined
    // snapshot — a cache HIT, not the estimate fallback. (If the spy ever stops
    // intercepting the component's import, the first assertion fails loudly
    // rather than passing vacuously.)
    const aCallIndexes = resolveSpy.mock.calls
      .map((call, i) => (call[0] === 'thread-aba-a' ? i : -1))
      .filter((i) => i >= 0);
    expect(aCallIndexes.length).toBeGreaterThan(0);
    expect(
      aCallIndexes.some((i) => {
        const result = resolveSpy.mock.results[i];
        return result.type === 'return' && result.value !== undefined;
      }),
    ).toBe(true);

    // And the key itself reproduced: the return-visit signature equals the
    // first-visit signature (the property the monotonic revision counter lacked).
    expect(peekThreadSizePriorsForTest('thread-aba-a')?.structureSig).toBe(firstSig);
  });

  it('saves an anchor snapshot after escape even inside the near-bottom band', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-escaped-near-bottom';

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    const scroll = getByTestId('message-timeline-scroll') as HTMLElement;
    let scrollTop = 350;
    Object.defineProperty(scroll, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scroll, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scroll, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (next: number) => { scrollTop = next; },
    });

    const ctrl = lastStick();

    clearThreadScrollSnapshotsForTest();
    ctrl.setEscapedFromLock(true);
    await fireEvent.scroll(scroll);
    await waitForScrollIntent();

    expect(ctrl.isAtBottom).toBe(false);
    const snap = getThreadScrollSnapshot('thread-escaped-near-bottom');
    expect(snap?.kind).toBe('anchor');
    if (snap?.kind === 'anchor') expect(['a', 'b']).toContain(snap.itemId);
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
    // and the engine's per-row remeasurement get handled invisibly by the
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

    const ctrl = lastStick();
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

    const ctrl = lastStick();
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
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [target],
      oldestTurnIndex: target.turnIndex,
      newestTurnIndex: target.turnIndex,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
    }));
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem');

    const { container } = render(MessageTimeline, { props: { pane } });

    await waitFor(() => expect(loadUntilItem).toHaveBeenCalledWith(target.id));
    await waitFor(() => {
      expect(container.querySelector(`[data-item-id="${target.id}"]`)).not.toBeNull();
    });
    expect(getThreadScrollSnapshot(thread.id)).not.toEqual({ kind: 'bottom' });
  });

  it('bottom-snapshot restore writes scrollTop exactly once via forceStick (no virtualizer-scroll fight)', async () => {
    // Regression: an earlier iteration of restoreToBottom paired
    // `listRef.scrollToIndex(last, 'end')` with `stick.markAtBottom()`.
    // virtua's measurement loop (this predates the bespoke engine) kept
    // writing scrollTop on every resize tick for ~150ms, while the controller's
    // contentRO sync-pin (enabled by markAtBottom) ALSO wrote scrollTop
    // on every positive contentEl delta. They targeted slightly
    // different values (virtualizer: itemOffset+itemSize-clientHeight;
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
    pane.attachScrollController = (ctrl: PaneScrollController) => {
      forceStickSpy = vi.spyOn(lastStick(), 'forceStick');
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

  it('bottom-snapshot restore schedules a rAF content-observation settle pass', async () => {
    // The synchronous forceStick at restore time lands scrollTop against
    // the geometry the engine reports at frame 0 from its initial estimates.
    // Late layout settling — composer-height RO updating scrollEl's
    // padding-bottom (padding-only growth doesn't refire the contentRO),
    // the engine's per-row remeasurement on the next frame, and the first
    // burst of Streamdown async typesetting (shiki / KaTeX / mermaid) —
    // can shift the bottom by a few pixels one frame after forceStick.
    // The user-visible symptom of dropping the trailing rAF was landing
    // "half a scroll tick from the bottom" intermittently. Pin the
    // contract by spying on the stick's observe() and asserting a
    // 'content' observation fires after one rAF tick.
    setThreadScrollSnapshot('thread-bottom-settle', { kind: 'bottom' });
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-bottom-settle';

    let observeSpy: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (ctrl: PaneScrollController) => {
      observeSpy = vi.spyOn(lastStick(), 'observe');
      origAttach(ctrl);
    };

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(observeSpy).not.toBeNull();
    const contentCalls = (): number =>
      observeSpy!.mock.calls.filter((call: unknown[]) => call[0] === 'content').length;
    const callsBeforeRaf = contentCalls();
    // Drive one animation frame so the trailing settle pass fires.
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect(contentCalls()).toBeGreaterThan(callsBeforeRaf);
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

    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
  });

  it('falls back to restoreToBottom when the anchor item resolves but findTimelineNodeIndex returns -1', async () => {
    // After loadUntilItem returns true, restoreAnchor awaits a
    // tick and then calls findTimelineNodeIndex(snap.itemId). If the
    // virtualizer hasn't yet rendered the row (race) or the item id was pruned in
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

    const ctrl = lastStick();
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
  // floor-progress predicate cleared the moment the floor cursor advanced,
  // so the anchor-restore programmatic scroll that followed
  // `pane.loadOlder()` re-fired the gate on the next tick. With the
  // user-input-armed gate, a successive button click loads exactly one
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
    pane.requestScrollToItem('user:target', { flash: true });

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

  it('composer-height growth notifies the scroll controller synchronously inside the RO callback', async () => {
    // Regression guard for the "appears then settles" symptom on uncached
    // loads. The previous composer-RO implementation deferred
    // the scroll notification to the next animation frame because a
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
    // the layout-relevant change, then notify the scroll controller
    // synchronously inside the same RO callback. The controller's
    // layout read applies the new CSS variable, so scrollHeight is
    // post-grow when scrollTop is written.
    //
    // This test stubs ResizeObserver so we can drive the composer-RO
    // callback with a specific height entry, then asserts that the
    // controller's live-capable notification count incremented inside
    // the synchronous callback (i.e. before any rAF could fire).
    const roCapture = installResizeObserverCapture();

    try {
      const pane = await buildPane(makeThread(), [
        makeItem({ id: 'tail', summary: 'tail' }),
      ]);

      let observeSpy: ReturnType<typeof vi.spyOn> | null = null;
      const origAttach = pane.attachScrollController.bind(pane);
      pane.attachScrollController = (ctrl: PaneScrollController) => {
        observeSpy = vi.spyOn(ctrl, 'observe');
        origAttach(ctrl);
      };

      const { getByTestId } = render(ChatView, { props: { pane } });
      await tick();
      // Flush the rAF that restoreToBottom queues for late layout
      // settling so it doesn't pollute the baseline call count.
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

      expect(observeSpy).not.toBeNull();
      const composerCalls = (): number =>
        observeSpy!.mock.calls.filter((call: unknown[]) => call[0] === 'composer-geometry').length;
      const callsBeforeFire = composerCalls();

      // Find the composer overlay and its registered callback. The
      // composer-RO is the one that observes `composerOverlay` in
      // ChatView's $effect — the only target with the
      // `composer-overlay` testid.
      const composerOverlay = getByTestId('composer-overlay');
      const composerCallback = roCapture.callbacksByTarget.get(composerOverlay);
      expect(composerCallback).toBeDefined();
      if (!composerCallback) return;

      // Synthesize a composer-height change. Composer overlay grew from
      // its initial height (120 default) to 200. The RO callback should
      // detect the change, write the CSS variable directly, AND call
      // the controller synchronously.
      const fakeEntry = {
        contentRect: { height: 200 } as DOMRectReadOnly,
      } as ResizeObserverEntry;
      composerCallback([fakeEntry], {} as ResizeObserver);

      // The synchronous observation must have happened before this
      // assertion runs (no rAF awaited). Previously the call was queued
      // inside a `requestAnimationFrame` and the assertion would fail
      // until a frame elapsed.
      expect(composerCalls()).toBeGreaterThan(callsBeforeFire);
    } finally {
      roCapture.restore();
    }
  });

  // NOTE: the width-oscillation regression (idle CPU/heap-churn incident
  // 2026-06-26) is intentionally NOT covered here. The loop is a real-RO +
  // real-layout feedback engine — each effect re-run recreates the surface
  // observer, whose fresh observe() schedules an initial content-box delivery
  // that disagrees with the border-box gBCR seed. happy-dom reports zero
  // geometry and its stub ResizeObserver does not auto-fire on observe(), so the
  // engine cannot exist here: any component-level assertion passes identically
  // with and without the fix (verified) and would be coverage theater. The
  // discriminating guard lives at the unit level in scrollSurfaceWidth.test.ts
  // ("reports the content-box width and never makes a synchronous layout
  // read"), which fails on the old gBCR-seeded behavior and passes on the
  // content-box-only helper. Do not re-add a happy-dom self-retrigger test.

  it('composer-height growth observes as composer-geometry (live-capable path)', async () => {
    // The 'composer-geometry' kind routes to the controller's
    // live-capable path, so active live output can spring through an
    // activity-rail height change instead of snapping. The kind→path
    // mapping itself is pinned by the controller-level
    // "observe('composer-geometry') spring-chases / sync-pins" pair in
    // utils/scroll/index.svelte.test.ts; what ChatView owns is reporting
    // the right observation kind.
    const roCapture = installResizeObserverCapture();

    try {
      const pane = await buildPane(makeThread(), [
        makeItem({ id: 'tail', summary: 'tail' }),
      ]);

      let observeSpy: ReturnType<typeof vi.spyOn> | null = null;
      const origAttach = pane.attachScrollController.bind(pane);
      pane.attachScrollController = (ctrl: PaneScrollController) => {
        observeSpy = vi.spyOn(ctrl, 'observe');
        origAttach(ctrl);
      };

      const { getByTestId } = render(ChatView, { props: { pane } });
      await tick();
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

      expect(observeSpy).not.toBeNull();
      observeSpy!.mockClear();

      const composerOverlay = getByTestId('composer-overlay');
      const composerCallback = roCapture.callbacksByTarget.get(composerOverlay);
      expect(composerCallback).toBeDefined();
      if (!composerCallback) return;

      pane.markLiveContentAdvanced();
      const fakeEntry = {
        contentRect: { height: 200 } as DOMRectReadOnly,
      } as ResizeObserverEntry;
      composerCallback([fakeEntry], {} as ResizeObserver);

      expect(observeSpy!.mock.calls).toEqual([['composer-geometry']]);
    } finally {
      roCapture.restore();
    }
  });

  it('composer-height growth writes --composer-height directly on chatColumn so layout reads see the new value', async () => {
    // Companion to the synchronous composer-observation test above. The
    // direct CSS-variable write is what makes the synchronous re-pin
    // correct — without it, `targetScrollTop()` would force layout with
    // the old --composer-height and pin to the pre-grow bottom. This
    // test asserts the inline style on chatColumn reflects the new
    // height BEFORE any tick/microtask/frame is awaited.
    const roCapture = installResizeObserverCapture();

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
      const composerCallback = roCapture.callbacksByTarget.get(composerOverlay);
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
      roCapture.restore();
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
    // static documents, but it actively fights the engine's remeasure
    // compensation AND the controller's contentRO sync-pin. Streamdown
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

  it('reserves a symmetric scrollbar gutter so centered rows stay aligned with the composer', async () => {
    // Regression guard: the styled `::-webkit-scrollbar` (app.css) is a
    // classic, space-consuming bar, not an overlay. Without a reserved
    // gutter the centered `mx-auto max-w-[62rem]` rows jump ~5px left — out
    // of alignment with ChatView's absolute composer overlay — the moment
    // the bar appears. `both-edges` is required, not single-edge `stable`:
    // WebKitGTK reserves the gutter only while the bar is actually present,
    // so a single edge still shifts the column on the idle→scrolling
    // transition; symmetric reservation holds the center in both states.
    // Verified empirically in WebKitGTK 6.0 2.52.3. The shift itself can't
    // be measured under happy-dom's zero geometry, so this guards the
    // directive's presence and exact value. See
    // docs/architecture/frontend-scroll.md.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);
    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    await tick();
    const scroll = getByTestId('message-timeline-scroll') as HTMLElement;
    expect(scroll.style.getPropertyValue('scrollbar-gutter')).toBe('stable both-edges');
  });
});

describe('scroll integration — banner overlay (no reserved height)', () => {
  it('overlays the status banners instead of reserving slot height', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    // Overlay pattern (replaces the old reserved slots): on the happy path
    // no banner renders and nothing reserves height under the header, so the
    // timeline never reflows. The wrapper is absolutely positioned, so even
    // when a banner shows it can't change the scroller's clientHeight. Test
    // ids assert the contract independent of Tailwind utility class names.
    expect(queryByTestId('provider-status-slot')).toBeNull();
    expect(queryByTestId('session-status-slot')).toBeNull();
    expect(getByTestId('provider-status-overlay').className).toContain('absolute');
  });
});

describe('scroll integration — auto-follow + button', () => {
  // Engine scroll math isn't observable in happy-dom (zero
  // viewport geometry). We verify integration seams: the input-intent path
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
    // handler can escape — and so the scroll event fired below refreshes
    // `isNearBottomState` to false (we want
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
    // Fire a scroll event so the controller refreshes isNearBottomState
    // from the new geometry.
    wrapper.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
    await tick();
    await tick();

    // After the input the chip's button is in the DOM (it may still
    // be in a fade-in transition; what matters is presence as a
    // signal that the user is no longer at-or-near the bottom).
    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(true);
    expect(ctrl.isAtBottom).toBe(false);
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

    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(true);
    expect(ctrl.isSticky).toBe(false);
    // Escape wins over the visual near-bottom band, so the chip appears
    // and auto-follow stays broken.
    expect(ctrl.isAtBottom).toBe(false);
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).not.toBeNull();

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.observe('content');
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

    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(true);
    expect(ctrl.isSticky).toBe(false);

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.observe('content');
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

    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);

    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1200 });
    pane.scrollController?.observe('content');
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
    // input, then a scroll event so isNearBottomState refreshes to
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

  it('Bug A: re-stick succeeds in a streaming-cadence sequence (60 Hz contentRO + wheel-down)', async () => {
    // Integration-level reproduction of the Opus-stream Bug A: the
    // user scrolls up during an active turn, then scrolls back down to
    // the bottom while contentRO is firing on the streaming cadence.
    // Pre-fix the deferred re-stick check would re-read
    // distanceFromBottom() AFTER a streaming chunk grew scrollHeight in
    // the 1ms window, miss the bottom, and leave escape stuck true.
    // Post-fix (Change 1) the synchronously-captured
    // distFromBottomAtEvent lets re-stick proceed.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
      makeItem({ id: 'c', itemIndex: 2, summary: 'c' }),
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
    let scrollHeight = 1000;
    Object.defineProperty(scrollEl, 'scrollHeight', {
      configurable: true,
      get: () => scrollHeight,
    });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = value; },
    });

    // 1. User wheels up — escape.
    const wheelUp = new WheelEvent('wheel', { deltaY: -50, bubbles: true });
    Object.defineProperty(wheelUp, 'target', { value: scrollEl });
    scrollEl.dispatchEvent(wheelUp);
    scrollTop = 100;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));
    await waitForScrollIntent();
    const ctrl = lastStick();
    expect(ctrl.escapedFromLock).toBe(true);

    // 2. User wheel-down — DOWN intent, browser scrolls to 397 (3 px
    //    above the actual bottom = 400 because scrollHeight=1000,
    //    clientHeight=600). Within the new 4 px epsilon (Change 1).
    const wheelDown = new WheelEvent('wheel', { deltaY: 100, bubbles: true });
    Object.defineProperty(wheelDown, 'target', { value: scrollEl });
    scrollEl.dispatchEvent(wheelDown);
    scrollTop = 397;
    scrollEl.dispatchEvent(new Event('scroll', { bubbles: true }));

    // 3. BEFORE the 1ms deferred check fires, simulate a streaming
    //    chunk: scrollHeight grows by 60 px. Pre-fix this would have
    //    pushed distanceFromBottom() to 63 — outside the 0.5 px
    //    epsilon — and re-stick would have failed. Post-fix the
    //    sync-captured distFromBottomAtEvent=3 still lets re-stick
    //    succeed.
    scrollHeight = 1060;
    pane.scrollController?.observe('content');
    await waitForScrollIntent();

    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isSticky).toBe(true);
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
  // happy-dom can't measure layout, so we don't assert on windowing
  // remount counts here. We assert on the data contract that the
  // virtualizer's `getKey` consumes: items array order + index map consistency.

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
  // The auto-load trigger fires from TimelineVirtualizer's `onscroll` prop
  // (handleTimelineScroll → maybeAutoLoadOlder → handleLoadOlder). Under
  // happy-dom + renderAll, the adapter's onscroll callback fires synchronously
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

  it('does not fire pane.loadOlder when oldestLoadedCursor is null (defensive null-floor exit)', async () => {
    // Edge case: backend returns hasMore=true with no items so the floor
    // cursor stays null. Without the null-floor early-return in the gate,
    // every scroll tick would re-enter loadOlder (which itself noops on a
    // null floor) — the progress guard's cursor compare would never engage.
    // Pin the defensive exit.
    const pane = await buildPane(undefined, []);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    Object.defineProperty(pane, 'oldestLoadedCursor', { configurable: true, get: () => null });
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
    Object.defineProperty(pane, 'oldestLoadedCursor', { configurable: true, get: () => ({ turnIndex: 5, itemIndex: 0 }) });
    const loadOlder = vi.spyOn(pane, 'loadOlder');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScroll(container);
    await tick();

    expect(loadOlder).not.toHaveBeenCalled();
  });
});

describe('scroll integration — auto-load-newer trigger', () => {
  // Mirror of the auto-load-older trigger at the bottom edge: the
  // Virtualizer's `onscroll` runs handleVirtuaScroll → maybeAutoLoadNewer →
  // handleLoadNewerAuto. These pin the cheap-gate guards (the positive
  // trigger + geometry math live in timelineScroll.test.ts where geometry
  // is deterministic; happy-dom reports zero row geometry).
  function dispatchScrollAtBottom(container: HTMLElement): HTMLElement {
    const scrollEl = container.querySelector(
      '[data-testid="message-timeline-scroll"]',
    ) as HTMLElement;
    expect(scrollEl).not.toBeNull();
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true, get: () => 400, set: () => {},
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

  it('does not fire pane.loadNewer when pane.hasMoreNewer is false', async () => {
    const items = [makeItem({ id: 'a', turnIndex: 5, summary: 'a' })];
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreNewer', { configurable: true, get: () => false });
    const loadNewer = vi.spyOn(pane, 'loadNewer');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScrollAtBottom(container);
    await tick();

    expect(loadNewer).not.toHaveBeenCalled();
  });

  it('does not fire pane.loadNewer when newestLoadedCursor is null (defensive null-ceiling exit)', async () => {
    const pane = await buildPane(undefined, []);
    Object.defineProperty(pane, 'hasMoreNewer', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingNewer', { configurable: true, get: () => false });
    Object.defineProperty(pane, 'newestLoadedCursor', { configurable: true, get: () => null });
    const loadNewer = vi.spyOn(pane, 'loadNewer');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScrollAtBottom(container);
    dispatchScrollAtBottom(container);
    await tick();

    expect(loadNewer).not.toHaveBeenCalled();
  });

  it('does not fire pane.loadNewer while a load is already in flight', async () => {
    const items = [makeItem({ id: 'a', turnIndex: 5, summary: 'a' })];
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreNewer', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingNewer', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'newestLoadedCursor', { configurable: true, get: () => ({ turnIndex: 5, itemIndex: 0 }) });
    const loadNewer = vi.spyOn(pane, 'loadNewer');

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    dispatchScrollAtBottom(container);
    await tick();

    expect(loadNewer).not.toHaveBeenCalled();
  });
});

// The 'visibility-mask flicker fix' suite was deleted. The mask was a
// rAF-gap mitigation; the new useStickToBottom controller eliminates the
// gap by writing scrollTop in the same paint cycle as the layout change
// (content ResizeObserver fires synchronously before paint). New
// regression scenarios live in the no-jitter / R4 / smooth-fight tests
// below.

describe('scroll integration — useStickToBottom wiring', () => {
  // Controller-internal behavior (sync-pin, content-RO, input-intent
  // handlers, pause-lease semantics, programmatic-write tagging) is
  // covered exhaustively in `utils/scroll/index.svelte.test.ts` against
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
    // depth-counted lease + geometry notifications — that sidebar resizers,
    // resizable drawers, and ChatView's composer-height publication depend on.
    expect(typeof pane.scrollController?.pauseAutoScroll).toBe('function');
    expect(typeof pane.scrollController?.observe).toBe('function');
    expect(typeof pane.scrollController?.preserveScrollAnchor).toBe('function');
    expect(typeof pane.scrollController?.preserveTimelineWindowAnchor).toBe('function');
  });

  it('vetoes timeline-window pruning when it would drop the visible anchor', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();
    await waitFor(() => {
      expect(getThreadScrollSnapshot(pane.threadId!)).toBeTruthy();
    });
    expect(container.querySelector('[data-item-id="a"]')).not.toBeNull();

    const ctrl = pane.scrollController;
    expect(ctrl?.preserveTimelineWindowAnchor).toBeTypeOf('function');
    if (!ctrl?.preserveTimelineWindowAnchor) return;

    lastStick().setEscapedFromLock(true);
    const run = vi.fn();
    const keepsItem = vi.fn(() => false);

    const result = ctrl.preserveTimelineWindowAnchor({ keepsItem, run });

    expect(result).toBe(false);
    expect(keepsItem).toHaveBeenCalledWith('a');
    expect(run).not.toHaveBeenCalled();
  });

  it('restores a kept visible anchor after timeline-window pruning', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);
    let ctrl: PaneScrollController | null = null;
    let applyScrollTarget: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (controller) => {
      ctrl = controller;
      applyScrollTarget = vi.spyOn(lastStick(), 'applyScrollTarget');
      origAttach(controller);
    };

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();
    await waitFor(() => {
      expect(getThreadScrollSnapshot(pane.threadId!)).toBeTruthy();
    });
    expect(container.querySelector('[data-item-id="a"]')).not.toBeNull();

    expect(ctrl).not.toBeNull();
    const controller = ctrl!;
    expect(controller.preserveTimelineWindowAnchor).toBeTypeOf('function');
    if (!controller.preserveTimelineWindowAnchor) return;

    lastStick().setEscapedFromLock(true);
    const run = vi.fn();
    const keepsItem = vi.fn((itemId: string) => itemId === 'a');

    const result = controller.preserveTimelineWindowAnchor({ keepsItem, run });

    expect(result).toBe(true);
    expect(keepsItem).toHaveBeenCalledWith('a');
    expect(run).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(applyScrollTarget).toHaveBeenCalledTimes(1);
    });
  });

  it('re-pins bottom after timeline-window pruning without consuming user intent', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);
    let ctrl: PaneScrollController | null = null;
    let forceStick: ReturnType<typeof vi.spyOn> | null = null;
    let markAtBottom: ReturnType<typeof vi.spyOn> | null = null;
    let applyScrollTarget: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (controller) => {
      ctrl = controller;
      const stick = lastStick();
      forceStick = vi.spyOn(stick, 'forceStick');
      markAtBottom = vi.spyOn(stick, 'markAtBottom');
      applyScrollTarget = vi.spyOn(stick, 'applyScrollTarget');
      origAttach(controller);
    };

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();
    await waitFor(() => {
      expect(getThreadScrollSnapshot(pane.threadId!)).toBeTruthy();
    });

    expect(ctrl).not.toBeNull();
    const controller = ctrl!;
    expect(controller.preserveTimelineWindowAnchor).toBeTypeOf('function');
    if (!controller.preserveTimelineWindowAnchor) return;

    lastStick().forceStick({ reason: 'restore' });
    forceStick?.mockClear();
    markAtBottom?.mockClear();
    applyScrollTarget?.mockClear();
    const run = vi.fn();

    const result = controller.preserveTimelineWindowAnchor({
      keepsItem: () => true,
      run,
    });

    expect(result).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(applyScrollTarget).toHaveBeenCalledTimes(1);
      expect(markAtBottom).toHaveBeenCalledTimes(1);
      expect(forceStick).not.toHaveBeenCalled();
    });
  });

  it('cancels a pending timeline-window prune restore on unmount', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);
    let ctrl: PaneScrollController | null = null;
    let markAtBottom: ReturnType<typeof vi.spyOn> | null = null;
    const origAttach = pane.attachScrollController.bind(pane);
    pane.attachScrollController = (controller) => {
      ctrl = controller;
      markAtBottom = vi.spyOn(lastStick(), 'markAtBottom');
      origAttach(controller);
    };

    const { unmount } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();
    await waitFor(() => {
      expect(getThreadScrollSnapshot(pane.threadId!)).toBeTruthy();
    });

    expect(ctrl).not.toBeNull();
    const controller = ctrl!;
    expect(controller.preserveTimelineWindowAnchor).toBeTypeOf('function');
    if (!controller.preserveTimelineWindowAnchor) return;

    lastStick().markAtBottom();
    markAtBottom?.mockClear();
    const run = vi.fn();

    const result = controller.preserveTimelineWindowAnchor({
      keepsItem: () => true,
      run,
    });
    unmount();
    await tick();

    expect(result).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
    expect(markAtBottom).not.toHaveBeenCalled();
  });

  it('host-layout reconciliation preserves the current sticky or escaped intent', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const stick = lastStick();

    expect(stick.isSticky).toBe(true);
    pane.scrollController?.observe('host-layout');
    expect(stick.escapedFromLock).toBe(false);
    expect(stick.isSticky).toBe(true);

    stick.setEscapedFromLock(true);
    expect(stick.isSticky).toBe(false);
    pane.scrollController?.observe('host-layout');
    expect(stick.escapedFromLock).toBe(true);
    expect(stick.isSticky).toBe(false);
  });

  it('host-layout reconciliation restores bottom intent when sticky state was stale but not escaped', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const stick = lastStick();

    stick.setEscapedFromLock(true);
    stick.setEscapedFromLock(false);
    expect(stick.escapedFromLock).toBe(false);
    expect(stick.isSticky).toBe(false);

    pane.scrollController?.observe('host-layout');

    await waitFor(() => {
      expect(stick.escapedFromLock).toBe(false);
      expect(stick.isSticky).toBe(true);
    });
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
    // A content observation after release should not throw even when
    // the controller's geometry is unmeasured (happy-dom).
    expect(() => ctrl.observe('content')).not.toThrow();
  });

  it('mid-stream upsertItem leaves the controller sticky (no scrollTop-direction inference)', async () => {
    // Replaces the deleted visibility-mask test that asserted appending
    // a child to a running inline subagent doesn't flicker. Under the new
    // architecture there is no mask — the guarantee is structural: the
    // controller does NOT infer up intent from scrollTop direction
    // (R4 mitigation), so an engine compensation or any per-row resize
    // that nudges scrollTop cannot flip escapedFromLock. Mid-stream
    // upserts therefore leave intent/stickiness untouched.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'agent-1', itemIndex: 0, summary: 'first' }),
    ]);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    // PaneScrollController is narrow by design — see thread.svelte.ts.
    // Peek at the underlying controller's intent state to verify stickiness
    // survives the upsert without inferring up intent from scrollTop direction.
    const ctrl = lastStick();
    // Baseline: no explicit escape was triggered, no leases held.
    expect(ctrl.isSticky).toBe(true);
    expect(ctrl.escapedFromLock).toBe(false);

    // Stream a second item in (simulates an inline subagent's child
    // member arriving mid-turn, or a new tool_call from the provider).
    pane.upsertItem(makeItem({ id: 'agent-2', itemIndex: 1, summary: 'second' }));
    await tick();
    await tick();

    // The upsert path must not have flipped escape or torn the lease.
    // If a future regression infers up intent from a compensation
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

    const ctrl = lastStick();
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

  it('does not arm the structural-append spring when switching into a streaming thread (cache miss)', async () => {
    // Regression for bug-report-20260622T041049Z. Switching INTO a thread
    // whose turn is live transitions `activeTurnStructuralSignature`
    // old-thread → new-thread, and on a cache MISS the slice loads
    // asynchronously AFTER the switch-generation bump — while pane.loading is
    // still true. Neither the switch nor the initial load is an in-turn
    // append, so the live-follow effect must NOT call
    // markStructuralContentPending() across the switch+load+settle. If it
    // does, the structural-append spring chases the post-restore
    // measurement backlog and the user sees a multi-hundred-px scroll on
    // switch. The fix re-baselines (without marking) while switchGeneration
    // changed OR pane.loading is true OR loading just toggled — covering the
    // async cache-miss slice that lands in a later flush than the bump.
    const threadA = makeThread({ id: 'thread-switch-spring-a' });
    const pane = await buildPane(threadA, [
      makeItem({
        id: 'a-text',
        threadId: threadA.id,
        kind: 'assistant_text',
        status: 'completed',
        summary: 'thread A tail',
      }),
    ]);
    // Thread A is itself mid-turn so its signature is non-empty: the switch
    // is a clean working→working transition the unfixed effect marks on
    // immediately, not just on the later slice load.
    projectTurnStarted(threadA.id, 'turn-a', 0, 1);
    // Clear the global registry box the instant it's created, via onTestFinished
    // so a failed assertion below can't leak this live turn into later tests
    // sharing the module-level registry. (Same for thread B once it starts.)
    onTestFinished(() => projectTurnCompleted(threadA.id, 'turn-a'));

    const notifyWatch = watchStickNotifications(pane);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();
    notifyWatch.reset();

    // Thread B is streaming in the background (backend already emitted
    // turn_started) and is NOT cached, so its slice loads async on switch.
    const threadB = makeThread({ id: 'thread-switch-spring-b' });
    const bItems = [
      makeItem({
        id: 'b-text-0',
        threadId: threadB.id,
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'thread B first',
      }),
    ];
    projectTurnStarted(threadB.id, 'turn-b', 0, 1);
    onTestFinished(() => projectTurnCompleted(threadB.id, 'turn-b'));
    setBindingMock('SwitchThread', async () => threadB);
    setBindingMock('ListThreadSliceAround', async () => ({
      items: bItems,
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    // Live-state hydrate must echo B's active turn; returning null would
    // clear the box (applyThreadLiveStateSnapshot) and empty the signature,
    // hiding the bug under a trivially-passing assertion.
    setBindingMock('GetThreadLiveState', async (tid: string) => ({
      threadId: tid,
      activeTurn:
        tid === threadB.id
          ? { threadId: threadB.id, turnId: 'turn-b', turnIndex: 0, startedAt: 1 }
          : null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    }));

    await pane.switchThread(threadB);
    await tick();
    await tick();
    await waitForAnimationFrame();
    await waitForScrollIntent();

    // The whole switch + async slice load + loading-settle must not arm the
    // structural-append spring.
    expect(notifyWatch.structuralMarks()).toBe(0);

    // ...but a genuine append to the now-settled, mounted thread B tail still
    // marks. The fix re-baselines on switch/load only — it does not disable
    // live-follow for real in-turn growth.
    notifyWatch.reset();
    pane.upsertItem(makeItem({
      id: 'b-text-1',
      threadId: threadB.id,
      turnIndex: 0,
      itemIndex: 1,
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'thread B second',
    }));
    await tick();
    expect(notifyWatch.structuralMarks()).toBeGreaterThan(0);
  });

  it('arms the structural-append spring for provider appends landing after turn end', async () => {
    // Regression for bug-report-20260702T193212Z. An interrupt settles the
    // turn (activeTurn → null) BEFORE the aftermath rows land (eager flush
    // echo, force-closed tool rows). `activeTurnStructuralSignature` is ''
    // without an active turn, so the live-follow effect never marks — and
    // the appends' growth sync-pinned as a 40-50px whole-viewport teleport.
    // The pane now arms the one-shot synchronously inside
    // applyProviderItemUpserts: turn-state-independent, and ordered before
    // the flush in which the virtualizer delivers the append's geometry.
    const thread = makeThread({ id: 'thread-post-turn-append' });
    const pane = await buildPane(thread, [
      makeItem({
        id: 'tail-text',
        threadId: thread.id,
        kind: 'assistant_text',
        status: 'completed',
        summary: 'settled tail',
      }),
    ]);
    projectTurnStarted(thread.id, 'turn-x', 0, 1);

    const notifyWatch = watchStickNotifications(pane);
    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitForAnimationFrame();

    // Interrupt: the turn settles before the aftermath rows arrive.
    projectTurnCompleted(thread.id, 'turn-x');
    await tick();
    notifyWatch.reset();

    pane.applyProviderItemUpserts([
      makeItem({
        id: 'killed-bash',
        threadId: thread.id,
        turnIndex: 0,
        itemIndex: 1,
        kind: 'tool_call',
        role: 'assistant',
        status: 'killed',
        toolName: 'Bash',
        summary: 'Bash: sleep 100',
      }),
    ]);

    // Synchronous — no tick: the arm must already be pending when the
    // append's same-flush geometry sample reaches the resolver.
    expect(notifyWatch.structuralMarks()).toBeGreaterThan(0);
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

  it('does not nudge sticky follow for same-row Bash completion churn', async () => {
    const thread = makeThread({ id: 'thread-bash-complete-follow' });
    const pane = await buildPane(thread, [
      makeItem({
        id: 'bash-1',
        threadId: thread.id,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
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
      id: 'bash-1',
      threadId: thread.id,
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      toolName: 'Bash',
      summary: 'Bash: sleep 1',
      payloadId: 'payload-bash-1',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({
        command: 'sleep 1',
        exitCode: 0,
        lineCount: 1,
        preview: 'done',
      }),
      meta: JSON.stringify({ input: { command: 'sleep 1' } }),
      updatedAt: 2,
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

    const ctrl = lastStick();

    // Switch to a different uncached thread.
    const threadB = makeThread({ id: 'thread-b-cross' });
    setBindingMock('SwitchThread', async () => threadB);
    setBindingMock('ListThreadSliceAround', async () => ({
      items: Array.from({ length: 10 }, (_, i) =>
        makeItem({ id: `b${i}`, threadId: 'thread-b-cross', itemIndex: i }),
      ),
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
    // Regression: on cache-miss thread loads, the engine's lazy mount-time
    // measurement underestimates totalSize at the estimate × N.
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
    // Uses > WARMUP_HIDE_THRESHOLD items so the visibility gate engages.
    const pane = await buildPane(undefined,
      Array.from({ length: 10 }, (_, i) =>
        makeItem({ id: `item-${i}`, itemIndex: i, summary: `item ${i}` }),
      ),
    );
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

describe('scroll integration — draft placeholder transitions', () => {
  // The `armWarmupWithReset()` + `armRestoreSnap()` block in
  // MessageTimeline.svelte's `$effect.pre` (armRestoreSnap carries the
  // defensive escape) is meant to suspend auto-follow until the restore
  // $effect runs.
  // But the restore $effect short-circuits on `!threadId`, and
  // `pane.threadId` returns null while `pane.draftPlaceholder` is set
  // (see thread.svelte.ts threadId getter). Without a draft-specific
  // branch in $effect.pre, real → draft pane transitions strand
  // `escapedFromLockState=true` permanently, surfacing the
  // scroll-to-bottom chip over the empty placeholder.

  it('real → draft transition keeps the controller sticky (chip hidden)', async () => {
    const realThread = makeThread({ id: 'thread-real' });
    const pane = await buildPane(realThread, [
      makeItem({ id: 'a', threadId: 'thread-real', summary: 'a' }),
      makeItem({ id: 'b', threadId: 'thread-real', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();

    const ctrl = lastStick();
    // Sanity: post-restore, the real thread is sticky-bottom.
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isAtBottom).toBe(true);
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).toBeNull();

    // Transition the same pane to a draft placeholder. This is the
    // path "+ New Thread" / ProjectPicker / open-draft hits in
    // production: pane.threadId flips to null while draftPlaceholder
    // is non-null.
    pane.startDraftPlaceholder(
      {
        id: 'project-draft',
        path: '/tmp/draft-project',
        name: 'Draft Project',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      },
      'chat',
    );
    expect(pane.threadId).toBeNull();
    expect(pane.hasDraftPlaceholder).toBe(true);

    await tick();
    await tick();

    // The restore $effect skipped (its `if (!threadId) return;` fires
    // on null), so the only thing keeping the controller sane on this
    // path is the $effect.pre draft branch flipping back to sticky-
    // bottom. Without that branch, escapedFromLockState would still
    // be true here and the chip would render.
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isAtBottom).toBe(true);
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).toBeNull();
  });

  it('draft → real transition still gates restore the normal way', async () => {
    // Inverse guard: the draft branch must NOT swallow the
    // armWarmup + armRestoreSnap (escape + consent) cycle for real
    // threads. If a future cleanup collapses both branches into
    // markAtBottom, this test fails — the new thread's restore would
    // never run because there is no escape to clear.
    const draftPane = await buildPane(undefined, []);
    draftPane.startDraftPlaceholder(
      {
        id: 'project-draft-2',
        path: '/tmp/draft-project-2',
        name: 'Draft Project 2',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      },
      'chat',
    );
    expect(draftPane.threadId).toBeNull();

    const { container } = render(MessageTimeline, { props: { pane: draftPane } });
    await tick();
    await tick();

    // Now transition to a real thread the way materializeDraftThread
    // does in production: switchThread to the materialized id.
    const materialized = makeThread({ id: 'thread-from-draft' });
    setBindingMock('SwitchThread', async () => materialized);
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [makeItem({ id: 'm1', threadId: 'thread-from-draft' })],
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    setBindingMock('GetThreadLiveState', async () => ({
      threadId: materialized.id,
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    }));
    setBindingMock('ListRecentTurns', async () => []);
    setBindingMock('ListThreadCheckpoints', async () => []);

    await draftPane.switchThread(materialized);
    await tick();
    await tick();

    const ctrl = lastStick();
    // After the real thread's restore runs, the controller is back
    // to sticky-bottom and the chip is hidden.
    expect(ctrl.escapedFromLock).toBe(false);
    expect(ctrl.isAtBottom).toBe(true);
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).toBeNull();
  });
});

describe('row geometry containment — application point', () => {
  // The settle-flicker fix lives in app.css as
  // `[data-row-geometry-content] { display: flow-root }`, keyed to that
  // attribute. rowMarginContainment.browser.test.ts proves the CSS RULE
  // contains the margin (real Chromium); this binary-free test proves the
  // APPLICATION POINT — MessageTimeline still stamps the attribute on every
  // row's measurement wrapper, so a rename/drop here fails a test instead of
  // silently disabling the BFC. happy-dom has no geometry, so it asserts
  // structure, not the containment itself.
  it('wraps every rendered row in exactly one [data-row-geometry-content] anchor', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'geo-a', summary: 'first' }),
      makeItem({ id: 'geo-b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-geometry-anchor';

    const { container } = render(MessageTimeline, { props: { pane } });
    await waitFor(() => {
      expect(container.querySelector('[data-row-index]')).not.toBeNull();
    });

    const rows = container.querySelectorAll('[data-row-index]');
    expect(rows.length).toBeGreaterThan(0);
    // One anchor per row, each nested inside its row: the BFC must wrap the
    // row's content for the trailing margin to be contained.
    expect(container.querySelectorAll('[data-row-geometry-content]').length).toBe(rows.length);
    for (const row of rows) {
      expect(row.querySelector('[data-row-geometry-content]')).not.toBeNull();
    }
  });
});
