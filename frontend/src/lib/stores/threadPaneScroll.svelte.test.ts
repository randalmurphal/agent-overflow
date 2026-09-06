// stores/threadPaneScroll.svelte.test.ts
//
// threadPaneScroll.svelte.ts through the pane: controller attach/detach,
// the structural-append and warm-gate arms, and the live-content stamp the
// scroll animation latches its instant-vs-spring choice on.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { __setSmoothingClockForTest, createThreadPane } from './thread.svelte';
import { type Item } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread, stubScrollController } from '../../test/helpers/chat';
import {
  FakeSmoothingClock,
  flushMicrotasks,
  installThreadPaneTestEnv,
} from '../../test/helpers/threadPane';

describe('threadPaneScroll', () => {
  beforeEach(installThreadPaneTestEnv);

  // The pane data layer is the sole owner of structural-append spring
  // arming (`armStructuralSpring`): a wire append and a reveal-gate
  // release both arm SYNCHRONOUSLY with the data change
  // (bug-report-20260702T193212Z): an effect-based arm runs after the
  // virtualizer's same-flush geometry delivery, so the append's own
  // growth resolved as an instant sync-pin; and the effect's turn-keyed
  // signature is blind to appends landing after turn end (interrupt
  // echo, force-closed tool rows). Each arm also schedules a post-flush
  // 'live-content' observe so growth that never fires a content-geometry
  // delta still gets a bottom re-check.
  describe('scroll-controller registration', () => {
    // The slot is single-occupancy and its only guard is object identity, so
    // it has to survive going through the store unchanged. A plain `$state`
    // proxies it and every `===` against it fails silently: the detach guard
    // stops matching, the slot never empties, and a torn-down controller —
    // holding the detached timeline subtree — stays reachable from the pane.
    it('hands back the same object that registered', () => {
      const pane = createThreadPane();
      const stick = stubScrollController();

      pane.attachScrollController(stick);

      expect(pane.scrollController).toBe(stick);
    });

    it('clears the slot when the surface that registered tears down', () => {
      const pane = createThreadPane();
      const stick = stubScrollController();
      pane.attachScrollController(stick);

      pane.detachScrollController(stick);

      expect(pane.scrollController).toBeNull();
    });

    it('ignores a stale teardown from the surface it already replaced', () => {
      // MessageTimeline → ChannelView (or a fast thread switch): the outgoing
      // surface's teardown can land after the incoming one has registered, and
      // must not disown a live controller. Then the incoming one's own teardown
      // still empties the slot — a guard that rejected everything would look
      // identical here and leak on the last unmount.
      const pane = createThreadPane();
      const outgoing = stubScrollController();
      const incoming = stubScrollController();
      pane.attachScrollController(outgoing);
      pane.attachScrollController(incoming);

      pane.detachScrollController(outgoing);
      expect(pane.scrollController).toBe(incoming);

      pane.detachScrollController(incoming);
      expect(pane.scrollController).toBeNull();
    });
  });

  describe('structural-append arm (pane data layer)', () => {
    function attachMockScrollController(pane: ReturnType<typeof createThreadPane>) {
      const markStructuralContentPending = vi.fn();
      const observe = vi.fn();
      pane.attachScrollController(
        stubScrollController({ observe, markStructuralContentPending }),
      );
      return { markStructuralContentPending, observe };
    }

    it('arms synchronously when a provider upsert appends in-window', async () => {
      const thread = makeThread({ id: 'thread-arm' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'bash-1',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // No tick/flush before the assertion: the arm must be ordered
      // before the flush in which the virtualizer measures the new row
      // and delivers its geometry sample.
      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
    });

    it('stamps live content alongside the wire-append arm', async () => {
      // A wire append entering the loaded tail is live content: besides
      // the 250ms one-shot, it must open the full
      // LIVE_CONTENT_ACTIVE_HOLD_MS rolling window so the controller
      // keeps expecting the appended rows' follow-up growth (payload
      // preview, markdown, highlight spans) and holds the spring
      // sentinel alive across the gaps between those deliveries rather
      // than cancelling on each arrival.
      const thread = makeThread({ id: 'thread-arm-stamp' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      attachMockScrollController(pane);
      expect(pane.lastLiveContentAt).toBe(0);

      const before = performance.now();
      pane.applyProviderItemUpserts([
        makeItem({
          id: 'bash-1:completion',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_completion',
          role: 'assistant',
          status: 'completed',
          toolName: 'Bash',
          summary: 'Background command finished',
          completionOf: 'bash-1',
        }),
      ]);
      const after = performance.now();

      // Stamped synchronously with the apply, on the same
      // performance.now() timebase the MessageTimeline latch reads.
      expect(pane.lastLiveContentAt).toBeGreaterThanOrEqual(before);
      expect(pane.lastLiveContentAt).toBeLessThanOrEqual(after);
    });

    it('pane.armStructuralSpring (composer optimistic send) arms without stamping', async () => {
      // The composer's send is deliberately a one-shot: one append wants
      // one spring window, not 500ms of spring eligibility for
      // unrelated reflows.
      const thread = makeThread({ id: 'thread-arm-composer' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.armStructuralSpring();

      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
      expect(pane.lastLiveContentAt).toBe(0);
    });

    it('does not arm for update-only batches', async () => {
      const thread = makeThread({ id: 'thread-arm-upd' });
      const seed = makeItem({
        id: 'bash-1',
        threadId: thread.id,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: ls',
      });
      const pane = await buildPane(thread, [seed]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        { ...seed, status: 'completed', updatedAt: seed.updatedAt + 1 },
      ]);

      // Mounted-row updates ride the live-content latch
      // (providerUpsertAdvancesLiveContent), not the one-shot.
      expect(markStructuralContentPending).not.toHaveBeenCalled();
    });

    it('does not arm for below-floor history rows', async () => {
      const thread = makeThread({ id: 'thread-arm-floor' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'tail', threadId: thread.id, turnIndex: 5, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'old', threadId: thread.id, turnIndex: 2, itemIndex: 0 }),
      ]);

      // Dropped by the window floor guard — never applied, never armed.
      expect(markStructuralContentPending).not.toHaveBeenCalled();
    });

    it('does not arm while the switch slice is still loading', async () => {
      const threadA = makeThread({ id: 'thread-arm-load-a' });
      const pane = await buildPane(threadA, [
        makeItem({ id: 'a-tail', threadId: threadA.id }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      const threadB = makeThread({ id: 'thread-arm-load-b' });
      const bItem = makeItem({
        id: 'b-0',
        threadId: threadB.id,
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'thread B first',
      });
      setBindingMock('SwitchThread', async () => threadB);
      let releaseSlice!: (v: {
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () => new Promise((resolve) => { releaseSlice = resolve; }),
      );

      const switching = pane.switchThread(threadB);
      // Let switchThread reach its awaits; the deferred slice keeps
      // `loading` true (cache miss).
      await flushMicrotasks();
      expect(pane.loading).toBe(true);

      // A streaming upsert arriving mid-load must not arm — and must not
      // stamp the latch either (the stamp shares the arm's gates): the
      // whole switch+load settle is a restore, not an in-turn append
      // (bug-report-20260622T041049Z class).
      pane.applyProviderItemUpserts([bItem]);
      expect(markStructuralContentPending).not.toHaveBeenCalled();
      expect(pane.lastLiveContentAt).toBe(0);

      releaseSlice({ items: [bItem], oldestTurnIndex: 0, hasMore: false });
      await switching;

      // A genuine append to the settled window arms (and stamps) again.
      pane.applyProviderItemUpserts([
        makeItem({
          id: 'b-1',
          threadId: threadB.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: pwd',
        }),
      ]);
      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
      expect(pane.lastLiveContentAt).toBeGreaterThan(0);
    });

    it("schedules the post-flush 'live-content' nudge alongside the arm", async () => {
      const thread = makeThread({ id: 'thread-arm-nudge' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'row-1',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // Never synchronous: the nudge waits for the Svelte flush plus one
      // frame so the virtualizer has published the new row before the
      // controller re-checks the bottom.
      expect(observe).not.toHaveBeenCalled();
      await vi.waitFor(() => {
        expect(observe).toHaveBeenCalledWith('live-content');
      });
    });

    it('arms when the reveal gate releases a withheld successor', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-reveal' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        // Short delta: enough lag to engage the gate, small enough that
        // the frontier finishes within the drain loop below.
        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        // The successor arms once through the wire-append path and is
        // withheld behind the frontier.
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        markStructuralContentPending.mockClear();

        // Drain the frontier. The boundary drop that RELEASES the tool
        // row mounts it with no wire upsert in that flush, so only the
        // reveal-site arm can make its growth spring-eligible.
        pane.applyItemPatch({ threadId: thread.id, itemId: 'front', kind: 'assistant_text',
          patch: { status: 'completed', updatedAt: 3 } });
        for (let frame = 0; frame < 500 && pane.revealBoundary !== null; frame++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm when the gate drops because the lone streaming row drained', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-lone-drain' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        // Nothing is waiting behind the frontier: when it drains and the
        // gate drops, no rows mount — arming would open a pointless
        // spring window on whatever grows next.
        pane.applyItemPatch({ threadId: thread.id, itemId: 'front', kind: 'assistant_text',
          patch: { status: 'completed', updatedAt: 3 } });
        for (let frame = 0; frame < 500 && pane.revealBoundary !== null; frame++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm on discussion-mode panes, whose controller belongs to ChannelView', async () => {
      const thread = makeThread({
        id: 'thread-arm-disc',
        mode: 'discussion',
        discussionId: 'disc-1',
      });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending, observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'row-1', threadId: thread.id, turnIndex: 0, itemIndex: 1 }),
      ]);

      // The chat timeline is swapped out for ChannelView in discussion
      // mode, so the registered controller watches channel messages —
      // arming it would spring unrelated channel growth for 250ms. The
      // append stamp shares the gate (the pane's timeline latch has no
      // reader on this surface).
      expect(markStructuralContentPending).not.toHaveBeenCalled();
      expect(pane.lastLiveContentAt).toBe(0);
      // Outwait the nudge's flush + frame (and its hidden-window timeout
      // fallback) so a skipped mark that still scheduled the observe
      // would be caught.
      await new Promise((resolve) => setTimeout(resolve, 60));
      expect(observe).not.toHaveBeenCalled();
    });

    it('cancels a scheduled nudge when the thread switches before it fires', async () => {
      const threadA = makeThread({ id: 'thread-nudge-cancel-a' });
      const pane = await buildPane(threadA, [
        makeItem({ id: 'seed', threadId: threadA.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'row-1',
          threadId: threadA.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // Switch before the nudge's flush + frame elapses. The nudge's
      // switchGeneration capture must cancel it — a post-switch
      // observe('live-content') would re-check the bottom of a freshly
      // restored, unrelated timeline.
      const threadB = makeThread({ id: 'thread-nudge-cancel-b' });
      setBindingMock('SwitchThread', async () => threadB);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(threadB);

      await new Promise((resolve) => setTimeout(resolve, 60));
      expect(observe).not.toHaveBeenCalled();
    });

    it('does not arm when a revert removes the frontier and its withheld successor', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-revert' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 1,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: thread.id,
            turnIndex: 1,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        expect(pane.revealBoundary).toEqual({ turnIndex: 1, itemIndex: 0 });
        markStructuralContentPending.mockClear();

        // Revert-on-interrupt truncates the tail: frontier AND withheld
        // successor go in one call. The boundary drops, but nothing
        // mounts — the timeline SHRANK — so arming would open a phantom
        // spring window over the revert settle.
        pane.removeItemsFromTurn(1);
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the reveal gate when a backend refresh removes its frontier', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-reveal-refresh' });
        const frontier = makeItem({
          id: 'frontier',
          threadId: thread.id,
          turnIndex: 1,
          itemIndex: 0,
          kind: 'assistant_text',
          status: 'streaming',
          summary: '',
        });
        const pane = await buildPane(thread, [frontier]);

        pane.applyItemDelta({
          threadId: thread.id,
          itemId: frontier.id,
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 1, itemIndex: 0 });

        setBindingMock('ListThreadSliceAround', async () => ({
          items: [],
          oldestTurnIndex: -1,
          newestTurnIndex: -1,
          hasMore: false,
          hasMoreOlder: false,
          hasMoreNewer: false,
        }));
        // While the row is still STREAMING, a page that lacks it is
        // expected — streaming rows persist per-item on completion, so
        // the refresh retains it (and its gate) rather than tearing the
        // block being streamed out of the timeline.
        await pane.refreshFromBackend();
        expect(pane.items.map((item) => item.id)).toEqual(['frontier']);
        expect(pane.revealBoundary).toEqual({ turnIndex: 1, itemIndex: 0 });

        // Once the row has SETTLED, the backend page is authoritative:
        // a refresh whose page lacks it removes the row and the gate
        // cannot outlive its frontier.
        pane.applyProviderItemUpserts([
          { ...frontier, status: 'completed', summary: 'streamed words arriving' },
        ]);
        await pane.refreshFromBackend();

        expect(pane.items).toEqual([]);
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm across a switch away from an engaged reveal gate', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const threadA = makeThread({ id: 'thread-arm-switch-a' });
        const pane = await buildPane(threadA, [
          makeItem({
            id: 'front',
            threadId: threadA.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: threadA.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: threadA.id,
            turnIndex: 0,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        markStructuralContentPending.mockClear();

        // disposeAllSmoothers nulls the boundary directly at switch
        // start (no publish through the reveal pass), and the loading
        // gate covers the slice load — the whole switch must not arm.
        const threadB = makeThread({ id: 'thread-arm-switch-b' });
        setBindingMock('SwitchThread', async () => threadB);
        setBindingMock('ListThreadSliceAround', async () => ({
          items: [],
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        await pane.switchThread(threadB);
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  // The warm-up gate is armed at the switch edge, but on the FETCH path
  // the pane then sits empty for the whole round trip — and an empty
  // mount window still delivers a zero-height content-geometry sample,
  // which the gate reads as cascade evidence and opens on ~QUIET_MS
  // later. So by the time the slice lands, the gate is already open and
  // the estimate cascade runs in front of the reader. The pane data
  // layer re-closes it as part of applying that slice, synchronously
  // with the item mutation (see PaneScrollController.armWarmup).
  describe('warm-gate re-arm on initial slice', () => {
    function attachWarmupSpy(pane: ReturnType<typeof createThreadPane>) {
      const armWarmup = vi.fn();
      pane.attachScrollController(stubScrollController({ armWarmup }));
      return armWarmup;
    }

    it('re-arms when the initial slice mounts rows into an empty pane', async () => {
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const thread = makeThread({ id: 'thread-cold' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'b', threadId: thread.id, turnIndex: 0, itemIndex: 1 }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(thread);

      expect(armWarmup).toHaveBeenCalledTimes(1);
    });

    it('does not re-arm for a thread whose slice is empty', async () => {
      // A brand-new thread mounts nothing to cascade. Holding the gate
      // closed would leave the pane behind an empty 2.5s failsafe and
      // sync-pin the first streamed tokens instead of gliding them.
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const thread = makeThread({ id: 'thread-empty' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(thread);

      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('does not re-arm on a cache-restore switch', async () => {
      // Cached items are present synchronously at the switch edge, so
      // the arm made there already covers their mount — and there is no
      // initial slice to apply.
      const thread = makeThread({ id: 'thread-cached' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const other = makeThread({ id: 'thread-other' });
      setBindingMock('SwitchThread', async (id: unknown) =>
        id === thread.id ? thread : other,
      );
      setBindingMock('ListThreadSliceAround', async (threadId: unknown) => ({
        // The restore still syncs, and the page REPLACES the painted
        // rows — so the mock has to answer for the thread being asked
        // about, not with a blanket empty window.
        items:
          threadId === thread.id
            ? [makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 })]
            : [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(other);

      const armWarmup = attachWarmupSpy(pane);
      await pane.switchThread(thread);

      expect(pane.items.map((it) => it.id)).toEqual(['a']);
      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('does not re-arm for streaming appends or older paging', async () => {
      // Both mount against content the reader is already looking at;
      // hiding that is a blank flash, not a cascade defense.
      const thread = makeThread({ id: 'thread-live' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 5, itemIndex: 0 }),
      ]);
      const armWarmup = attachWarmupSpy(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'next', threadId: thread.id, turnIndex: 5, itemIndex: 1 }),
      ]);
      expect(armWarmup).not.toHaveBeenCalled();

      setBindingMock('ListThreadItemsBefore', async () => ({
        items: [makeItem({ id: 'older', threadId: thread.id, turnIndex: 4, itemIndex: 0 })],
        oldestTurnIndex: 4,
        hasMore: false,
      }));
      await pane.loadOlder();
      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('re-arms once per switch across a rapid switch away and back', async () => {
      // Each switch's own slice application is its own re-arm; a
      // superseded switch's late response is gen-guarded out and must
      // not add one.
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const threadA = makeThread({ id: 'thread-ab-a' });
      const threadB = makeThread({ id: 'thread-ab-b' });
      setBindingMock('SwitchThread', async (id: unknown) =>
        id === threadA.id ? threadA : threadB,
      );
      setBindingMock('ListThreadSliceAround', async (id: unknown) => ({
        items: [makeItem({ id: `${id}-row`, threadId: id as string, turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      const first = pane.switchThread(threadA);
      const second = pane.switchThread(threadB);
      await Promise.all([first, second]);

      expect(armWarmup).toHaveBeenCalledTimes(1);
      expect(pane.threadId).toBe(threadB.id);
    });
  });

  // `pane.lastLiveContentAt` is the source the chat scroll controller
  // reads to decide whether more content is expected imminently
  // (MessageTimeline's liveContentActiveNow → isLiveContentActive).
  // It does not choose spring vs sync-pin — growth always glides.
  // Through the
  // PANE-INTERNAL paths exercised here (`upsertItems`, `applyItemDelta`,
  // `applyItemPatch`), it must advance ONLY on genuine smooth live
  // timeline content arriving — text reveals, final-summary patches —
  // and must NOT advance on thread switch, non-smooth delta growth,
  // bulk history loads, meta-only updates, or the optimistic-send /
  // rollback paths that drive `upsertItems` directly. (The provider
  // upsert fan-out in events.ts additionally stamps visible-field
  // updates to mounted rows of ANY kind — tool output previews,
  // completion chrome; those rules are covered in events.test.ts.)
  // Each test ticks the fake clock to a nonzero base first so a
  // `=== 0` assertion genuinely means "never stamped" rather than
  // "stamped at time 0".
  describe('live-content stamp (scroll animation latch source)', () => {
    // Long backlog so the smoother reveals across many frames (never
    // caught up in 2-3 ticks). 60 words ≈ 230 chars, which at the
    // adaptive ceiling is >40 frames. Short words on purpose: one 16ms
    // frame's budget at the ceiling is ~5 chars, so a 3-char word unit
    // means frames 1-3 each land a reveal (and therefore a stamp).
    const longText = (n: number) =>
      Array.from({ length: n }, (_, i) => `w${i}`).join(' ') + ' ';

    it('stamps on each smoother reveal frame, never on switch/upsert/delta-append', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(100); // base now()=100 so the `=== 0` checks are real
        const pane = await buildPane(makeThread({ id: 'stamp-reveal' }));
        // Switching into a thread (bulk slice load) is not live content.
        expect(pane.lastLiveContentAt).toBe(0);

        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-reveal',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        // Creating the streaming row is not yet a reveal.
        expect(pane.lastLiveContentAt).toBe(0);

        pane.applyItemDelta({
          threadId: 'stamp-reveal',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(60),
          updatedAt: 2,
        });
        // A smoothed delta only FEEDS the smoother; the reveal (and its
        // stamp) lands on the next rAF tick, not synchronously here.
        expect(pane.lastLiveContentAt).toBe(0);

        clock.tickFrame(16); // now()=116, first reveal fires onReveal
        expect(pane.lastLiveContentAt).toBe(116);
        clock.tickFrame(16); // now()=132, more words reveal
        expect(pane.lastLiveContentAt).toBe(132);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps stamping through the drain tail after a turn-completed patch', async () => {
      // The bug-2 case: the wire turn completes (getActiveTurn → null)
      // while the smoother still has seconds of word-by-word text to
      // reveal. Those trailing reveals must keep stamping so the tail
      // springs instead of jumping.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'stamp-drain' }));
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-drain',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'stamp-drain',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(120),
          updatedAt: 2,
        });

        clock.tickFrame(16); // now()=16, partial reveal — far from caught up
        expect(pane.lastLiveContentAt).toBe(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // Turn completes on the wire: a bare status patch with no summary.
        pane.applyItemPatch({
          threadId: 'stamp-drain',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', updatedAt: 3 },
        });
        // The bare status patch itself adds no stamp (rigorous no-stamp
        // proof for status/meta patches is the next test); the smoother
        // survives because it is not caught up.
        expect(pane.lastLiveContentAt).toBe(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // Reveals continue AFTER completion → stamps continue advancing.
        clock.tickFrame(16);
        expect(pane.lastLiveContentAt).toBe(32);
        clock.tickFrame(16);
        expect(pane.lastLiveContentAt).toBe(48);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not stamp on a non-smoothed streaming delta (bypasses the smoother)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(16); // base now()=16
        const pane = await buildPane(makeThread({ id: 'stamp-nonsmooth' }));
        // tool_call is not a smoothable kind — applyItemDelta writes
        // summary directly. It deliberately does not stamp the spring latch:
        // command output geometry is measured by its own renderer, and
        // sync-pinning is less janky than animating transient estimates.
        pane.upsertItem(
          makeItem({
            id: 'tool:0:0',
            threadId: 'stamp-nonsmooth',
            kind: 'tool_call',
            role: 'assistant',
            status: 'streaming',
            summary: 'out',
            updatedAt: 1,
          }),
        );
        expect(pane.lastLiveContentAt).toBe(0);

        pane.applyItemDelta({
          threadId: 'stamp-nonsmooth',
          itemId: 'tool:0:0',
          kind: 'tool_call',
          delta: 'put',
          updatedAt: 2,
        });
        expect(pane.lastLiveContentAt).toBe(0);
        expect(pane.items[0].summary).toBe('output');
        expect(pane.__itemSmootherCountForTest()).toBe(0); // never smoothed
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('stamps on a direct-summary patch, not on status-only or meta-only patches', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(10); // base now()=10
        const pane = await buildPane(makeThread({ id: 'stamp-patch' }));
        // Settled row: no smoother, so a later summary patch writes
        // directly through applyItemPatch's direct-summary branch.
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-patch',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'completed',
            summary: 'hello',
            updatedAt: 1,
          }),
        );
        expect(pane.lastLiveContentAt).toBe(0);

        // Status-only patch: no summary growth → no stamp.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { status: 'errored', updatedAt: 2 },
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Meta-only patch: no summary growth → no stamp.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { meta: '{"pathRefs":[]}' },
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Direct summary overwrite (no smoother present) → stamps.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { summary: 'hello world' },
        });
        expect(pane.lastLiveContentAt).toBe(10);
        expect(pane.items[0].summary).toBe('hello world');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not stamp on applyItemMeta, bulk merge, or direct upsertItems; markLiveContentAdvanced does', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(20); // base now()=20
        // Bulk slice load on switch (mergeMissingItemsById) is history,
        // not live content — must not stamp.
        const pane = await buildPane(makeThread({ id: 'stamp-neg' }), [
          makeItem({ id: 'seed:0:0', threadId: 'stamp-neg', summary: 'pre' }),
        ]);
        expect(pane.lastLiveContentAt).toBe(0);

        // applyItemMeta is the streaming path-link allowlist — meta only,
        // never content height → never stamps.
        pane.applyItemMeta({
          threadId: 'stamp-neg',
          itemId: 'seed:0:0',
          kind: 'assistant_text',
          meta: '{"pathRefs":[]}',
          updatedAt: 2,
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Driving pane.upsertItems directly (the Composer optimistic-send
        // echo and revertOnInterrupt rollback paths) must NOT stamp — only
        // the events.ts provider fan-out marks live content. This is what
        // keeps a user's own sent message and rollback restores sync-pinned.
        pane.upsertItems([
          makeItem({
            id: 'new:1:0',
            threadId: 'stamp-neg',
            turnIndex: 1,
            kind: 'assistant_text',
            summary: 'fresh',
          }),
        ]);
        expect(pane.lastLiveContentAt).toBe(0);

        // The public seam events.ts calls on a changed provider upsert.
        pane.markLiveContentAdvanced();
        expect(pane.lastLiveContentAt).toBe(20);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resets lastLiveContentAt on thread switch (no stale stamp bleeds into the next thread)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(100);
        const pane = await buildPane(makeThread({ id: 'A' }));
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'A',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'A',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(60),
          updatedAt: 2,
        });
        clock.tickFrame(16); // reveal stamps A as recently streaming
        expect(pane.lastLiveContentAt).toBe(116);

        // Switch to a settled thread B. A's recent stamp must NOT carry
        // over — otherwise B's late typesetting reflow (which never stamps)
        // would read 'spring' off A's timestamp within the 500ms hold and
        // chase B's settled content. The reset makes the latch read
        // 'instant' for B until B itself streams.
        await pane.switchThread(makeThread({ id: 'B' }));
        expect(pane.lastLiveContentAt).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });
});
