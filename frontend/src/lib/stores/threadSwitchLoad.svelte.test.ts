// stores/threadSwitchLoad.svelte.test.ts
//
// threadSwitchLoad.svelte.ts through the pane: the cached-window restore vs
// cold initial load, the spinner-flash gate over the two, and the size-prior
// capture a switch has to take BEFORE the incoming thread lands.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread, stubScrollController } from '../../test/helpers/chat';
import { flushMicrotasks, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { prependThread, removeThread } from './threads.svelte';

describe('threadSwitchLoad', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('switchThread cache + initial load', () => {
    it('paints cached items synchronously on re-entry without waiting for the network', async () => {
      const pane = createThreadPane();
      const items = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
      ];
      // Initial switch: cache is empty, the load returns the items.
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);

      // Switch away — outgoing thread snapshot lands in the cache.
      await pane.switchThread(makeThread({ id: 'other' }));

      // Make the load hang so the cache is the only painter on re-entry.
      // (Cache hit short-circuits the load; this hang would only apply
      // if the cache lookup failed.)
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

      // Kick off the re-entry but DON'T await — assert items are
      // already painted from cache.
      const switching = pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);
      expect(pane.oldestLoadedTurnIndex).toBe(0);
      // Don't actually await — the load mock hangs forever; cache hit
      // means we never wait on it anyway.
      void switching;
    });

    it('caches the window on pane close so a reopen paints synchronously (bug-report-20260822T020840Z)', async () => {
      const pane = createThreadPane();
      const threadRow = makeThread({ id: 't-close' });
      // snapshotForClose caches only threads the store still lists (a
      // deletion flow evicts first and must stay evicted).
      prependThread(threadRow);
      try {
        const items = [
          makeItem({ id: 'a', threadId: 't-close', turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'b', threadId: 't-close', turnIndex: 1, itemIndex: 0 }),
        ];
        setBindingMock('ListThreadSliceAround', async () => ({
          items,
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        await pane.switchThread(threadRow);
        expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);

        // The destroyPane sequence: snapshot, then clear.
        pane.snapshotForClose();
        pane.clear();
        expect(pane.items).toEqual([]);

        // Reopen with the network hung — the close-time cache is the
        // only possible painter.
        setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
        const switching = pane.switchThread(makeThread({ id: 't-close' }));
        expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);
        void switching;
      } finally {
        removeThread('t-close');
      }
    });

    it('close-time snapshot skips a thread the store no longer lists (deletion stays evicted)', async () => {
      const pane = createThreadPane();
      const items = [
        makeItem({ id: 'a', threadId: 't-deleted', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      // Never prepended to the threads store — the deletion flow's
      // removeThread has already run by the time panes close.
      await pane.switchThread(makeThread({ id: 't-deleted' }));
      pane.snapshotForClose();
      pane.clear();

      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
      const switching = pane.switchThread(makeThread({ id: 't-deleted' }));
      await Promise.resolve();
      expect(pane.items).toEqual([]); // no cache resurrection
      void switching;
    });

    it('skips the cache write when the outgoing pane is empty', async () => {
      const pane = createThreadPane();
      // Empty thread — first switch yields no items.
      await pane.switchThread(makeThread({ id: 'empty' }));
      expect(pane.items).toEqual([]);

      // Switch away to a thread with items.
      const other = [
        makeItem({ id: 'x', threadId: 'other', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: other,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'other' }));

      // Make the load hang. With no cached items the empty re-entry
      // would have to wait on the network — we assert items stays []
      // before it resolves.
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

      // Re-enter the empty thread. No cached items → items stays [].
      const switching = pane.switchThread(makeThread({ id: 'empty' }));
      // Yield once for the load's microtask.
      await Promise.resolve();
      expect(pane.items).toEqual([]);
      // Don't actually await — the load hangs forever.
      void switching;
    });

    it('initial-load result preserves items appended via streamed events during the load', async () => {
      const pane = createThreadPane();
      // Stage: load hangs so a streamed upsert can land before its
      // result.
      let releaseLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseLoad = resolve;
          }),
      );

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed event arrives mid-load — upsert into the same items
      // array. mergeMissingItemsById in the load callback must keep it.
      pane.upsertItem(
        makeItem({
          id: 'streamed',
          threadId: 't',
          turnIndex: 1,
          itemIndex: 0,
        }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['streamed']);

      // Load returns the canonical view. Triage's persist-then-emit
      // contract means the load SHOULD include 'streamed'; simulate
      // that.
      releaseLoad({
        items: [
          makeItem({ id: 'load', threadId: 't', turnIndex: 0, itemIndex: 0 }),
          makeItem({
            id: 'streamed',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      });
      await switching;

      // Both items survive; no duplicates from mergeMissingItemsById.
      const ids = pane.items.map((it) => it.id);
      expect(ids).toEqual(['load', 'streamed']);
    });

    it('a same-thread re-switch invalidates the in-flight load result', async () => {
      const pane = createThreadPane();
      // First switch: load hangs.
      let releaseFirstLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseFirstLoad = resolve;
          }),
      );

      const firstSwitch = pane.switchThread(makeThread({ id: 't' }));
      // The item leg consults the durable replica before it issues the
      // RPC, so the hanging mock is reached a few microtasks in rather
      // than on the switch's own tick.
      await flushMicrotasks();

      // Second switch comes in before the first resolves. Backend
      // returns a fresh canonical view.
      const secondItems = [
        makeItem({ id: 'second', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      const secondSwitch = pane.switchThread(makeThread({ id: 't' }));
      await secondSwitch;

      expect(pane.items.map((it) => it.id)).toEqual(['second']);

      // Now release the first switch's load with stale data using
      // an id DISJOINT from `secondItems`. Without the gen-guard,
      // mergeMissingItemsById would happily slot 'stale-only' in next
      // to 'second' (no id collision). The assertion below confirms
      // the guard short-circuits the callback before the merge runs.
      releaseFirstLoad({
        items: [makeItem({ id: 'stale-only', threadId: 't', turnIndex: 99 })],
        oldestTurnIndex: 99,
        hasMore: true,
      });
      await firstSwitch;

      expect(pane.items.map((it) => it.id)).toEqual(['second']);
    });

    it('forces a fresh fetch on same-thread re-switch (revert-then-switch UX)', async () => {
      const pane = createThreadPane();
      // First load returns [a, b].
      const initialItems = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initialItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);

      // Revert removes 'b'. Same-thread re-switch should NOT cache the
      // pre-revert view and read it back — that would flash 'b' before
      // the load corrects. Stage the load to return only 'a'.
      const revertedItems = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: revertedItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      // 'b' must never appear after the re-switch resolves. The
      // pre-revert items would be the cached snapshot if the
      // sameThreadReswitch guard were missing.
      expect(pane.items.map((it) => it.id)).toEqual(['a']);
    });

    it('bumps switchGeneration on every switchThread (including same-thread re-switch)', async () => {
      // A forced in-place reload calls pane.switchThread(currentThread).
      // pane.threadId does not change on that path, so MessageTimeline's
      // restore $effect.pre would miss the event if it keyed only on
      // pane.threadId. Exposing switchGeneration gives the timeline a
      // second discriminator so the reset path (restoredThreadId = null,
      // armWarmup, armRestoreSnap) still fires and the viewport restores
      // to bottom instead of sticking at scrollTop=0 with the "Load older
      // messages" banner visible. This test locks in the contract
      // MessageTimeline depends on; the timeline-side behavior is
      // covered by the integration test for revert flow.
      const pane = createThreadPane();
      const initial = pane.switchGeneration;

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const afterFirst = pane.switchGeneration;
      expect(afterFirst).toBeGreaterThan(initial);

      // Different thread — generation bumps as expected.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      const afterSecond = pane.switchGeneration;
      expect(afterSecond).toBeGreaterThan(afterFirst);

      // Same-thread re-switch (the revert path). Without the bump,
      // MessageTimeline's restore reset path would never fire.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      const afterReswitch = pane.switchGeneration;
      expect(afterReswitch).toBeGreaterThan(afterSecond);
    });

    it('switchGeneration getter is reactive: $effect re-fires on same-thread re-switch', async () => {
      // Imperative reads of `pane.switchGeneration` between awaits would
      // pass even if the underlying `let` weren't `$state` — they just
      // observe whatever value the getter happens to return. But
      // MessageTimeline's `$effect.pre` consumes the getter inside a
      // reactive scope; if the backing storage isn't `$state`, the
      // dependency never registers and the effect never re-fires on
      // same-thread re-switch. Symptom: revert still lands at the very
      // top with "Load older messages" visible, exactly the bug this
      // fix targets. This test mounts a real $effect on the getter and
      // asserts the effect re-fires after each bump.
      const pane = createThreadPane();
      const observed: number[] = [];

      const stop = $effect.root(() => {
        $effect(() => {
          observed.push(pane.switchGeneration);
        });
      });

      try {
        flushSync();
        const baseline = observed.length;

        await pane.switchThread(makeThread({ id: 'thread-a' }));
        flushSync();
        expect(observed.length).toBeGreaterThan(baseline);

        // Same-thread re-switch — the load-bearing case.
        await pane.switchThread(makeThread({ id: 'thread-a' }));
        flushSync();
        // Must increase again: a non-$state getter would NOT re-fire the
        // effect (Svelte 5 reactivity requires $state for tracking).
        expect(observed.at(-1)).toBeGreaterThan(observed[baseline] ?? -1);
      } finally {
        stop();
      }
    });

    it('mergeMissingItemsById preserves the existing item reference for unchanged rows', async () => {
      const pane = createThreadPane();
      // Initial load returns [a]; streaming upserts a fresh copy of a
      // mid-load so we can assert the load's merge keeps the
      // upserted reference rather than overwriting it.
      let releaseLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseLoad = resolve;
          }),
      );

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed upsert lands BEFORE the load resolves, seeding `a`.
      pane.upsertItem(
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      );
      const aRefBeforeLoad = pane.items[0];
      expect(aRefBeforeLoad.id).toBe('a');

      // Load returns [a (different shell), b]. Reference-preservation
      // contract says we keep the upserted `a` ref and only allocate
      // `b`.
      releaseLoad({
        items: [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      });
      await switching;

      // a's reference survives unchanged; b is fresh.
      expect(pane.items[0]).toBe(aRefBeforeLoad);
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);
    });

    it('does not cache the outgoing pane while it is still loading', async () => {
      const pane = createThreadPane();
      // First switch hangs forever — outgoing items never resolve.
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
      void pane.switchThread(makeThread({ id: 'first' }));
      // Yield so the load gets to the top of switchThread.
      await Promise.resolve();
      expect(pane.loading).toBe(true);

      // Switch to a fresh thread. The outgoing pane is loading so the
      // cache write must be skipped — otherwise we'd snapshot an
      // empty in-flight pane and a future switch back would paint
      // empty even though the real thread has content.
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'second' }));

      const cacheModule = await import('./threadItemCache');
      expect(cacheModule.threadItemCache.get('first')).toBeNull();
    });

    it('runs all backend fetches in parallel rather than serialising them', async () => {
      const pane = createThreadPane();
      // Each mock records its own start timestamp on entry. With
      // parallelisation, all five start within a microtask of each
      // other; with the legacy serialised flow, ListRecentTurns would
      // wait for ListThreadSliceAround to resolve.
      const startedAt: Record<string, number> = {};
      let nextSlot = 0;
      const stamp = (name: string) => () => {
        startedAt[name] = nextSlot++;
        return new Promise(() => {}); // hang forever
      };
      setBindingMock('SwitchThread', stamp('SwitchThread'));
      setBindingMock('GetThreadLiveState', stamp('GetThreadLiveState'));
      setBindingMock('ListThreadSliceAround', stamp('ListThreadSliceAround'));
      setBindingMock('ListRecentTurns', stamp('ListRecentTurns'));

      // Don't await — every mock hangs intentionally.
      void pane.switchThread(makeThread({ id: 't' }));

      // Yield enough microtasks for all four Promise constructors to
      // run (each one assigns its slot synchronously inside the
      // `() => new Promise(() => {})` body).
      for (let i = 0; i < 8; i++) await Promise.resolve();

      // All four must have started. The exact ordering between them
      // is non-deterministic by design; we only assert that no fetch
      // is missing — which it would be under serialisation.
      expect(Object.keys(startedAt).sort()).toEqual([
        'GetThreadLiveState',
        'ListRecentTurns',
        'ListThreadSliceAround',
        'SwitchThread',
      ]);
    });

    it('loads the switch window exactly once (single-load contract)', async () => {
      // Pin the no-Phase-2 invariant: if a wider-window probe ever
      // creeps back into the switch path, the residual flicker
      // (wide prepend → applyJump fight with the controller's
      // sync-pin) returns.
      const calls: string[] = [];
      setBindingMock('ListThreadSliceAround', async () => {
        calls.push('ListThreadSliceAround');
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));
      expect(calls).toEqual(['ListThreadSliceAround']);
    });

    it('uses the scroll snapshot anchor when calling ListThreadSliceAround', async () => {
      const { setThreadScrollSnapshot, clearThreadScrollSnapshotsForTest } =
        await import('../utils/threadScrollSnapshots');
      clearThreadScrollSnapshotsForTest();
      setThreadScrollSnapshot('t', {
        kind: 'anchor',
        itemId: 'wanted',
        offsetTop: -42,
      });

      const pane = createThreadPane();
      let observedAnchor = '';
      setBindingMock(
        'ListThreadSliceAround',
        async (threadID: unknown, anchorID: unknown, _count: unknown) => {
          observedAnchor = String(anchorID ?? '');
          void threadID;
          return { items: [], oldestTurnIndex: -1, hasMore: false };
        },
      );
      await pane.switchThread(makeThread({ id: 't' }));
      expect(observedAnchor).toBe('wanted');
      clearThreadScrollSnapshotsForTest();
    });

    it('passes empty anchor when the scroll snapshot is the bottom kind', async () => {
      const { setThreadScrollSnapshot, clearThreadScrollSnapshotsForTest } =
        await import('../utils/threadScrollSnapshots');
      clearThreadScrollSnapshotsForTest();
      setThreadScrollSnapshot('t', { kind: 'bottom' });

      const pane = createThreadPane();
      let observedAnchor = 'unset';
      setBindingMock(
        'ListThreadSliceAround',
        async (threadID: unknown, anchorID: unknown, _count: unknown) => {
          observedAnchor = String(anchorID ?? '');
          void threadID;
          return { items: [], oldestTurnIndex: -1, hasMore: false };
        },
      );
      await pane.switchThread(makeThread({ id: 't' }));
      expect(observedAnchor).toBe('');
      clearThreadScrollSnapshotsForTest();
    });

    it('cache hit completes loading=false even when SwitchThread fails', async () => {
      const pane = createThreadPane();
      const items = [makeItem({ id: 'cached', threadId: 't', turnIndex: 0 })];
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      await pane.switchThread(makeThread({ id: 'other' }));

      // SwitchThread fails — toast fires but the rest of the load
      // continues. loading must still flip false at the end.
      setBindingMock('SwitchThread', async () => {
        throw new Error('mock backend down');
      });
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.loading).toBe(false);
      // Items still surface from the cache.
      expect(pane.items.map((it) => it.id)).toEqual(['cached']);
    });

    it('a stale-gen rejection of the initial load does not blank items or stamp generalError', async () => {
      // Pins withGenGuard's contract: when capturedGen !== switchGeneration,
      // onError must NOT run. A regression that flipped the gen-check
      // order would let a slow load from switch #1 write generalError
      // and items=[] against the pane that switch #2 already populated.
      const pane = createThreadPane();
      // First switch: load hangs forever (a Promise that will be
      // rejected later).
      let rejectFirstLoad!: (err: Error) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((_, reject) => {
            rejectFirstLoad = reject;
          }),
      );
      const firstSwitch = pane.switchThread(makeThread({ id: 'first' }));
      // See above: the replica read precedes the RPC, so let the leg
      // reach the hanging mock before replacing it.
      await flushMicrotasks();

      // Second switch supersedes; populates with real data.
      const secondItems = [
        makeItem({
          id: 'live',
          threadId: 'second',
          turnIndex: 0,
          itemIndex: 0,
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'second' }));
      expect(pane.items.map((it) => it.id)).toEqual(['live']);
      expect(pane.generalError).toBeNull();

      // Now reject the first switch's load. Stale-gen guard MUST
      // suppress the onError side effects.
      rejectFirstLoad(new Error('initial load backend down'));
      await firstSwitch;

      // Items unchanged — second switch's data still painted.
      expect(pane.items.map((it) => it.id)).toEqual(['live']);
      // generalError still null — stale onError did not stamp.
      expect(pane.generalError).toBeNull();
    });
  });

  describe('switchThread spinner-flash gate', () => {
    it('cache hit never flips showLoadingSpinner true even past the threshold', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        const items = [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ];
        setBindingMock('ListThreadSliceAround', async () => ({
          items,
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        await pane.switchThread(makeThread({ id: 't' }));
        await pane.switchThread(makeThread({ id: 'other' }));

        // Re-enter — initial load hangs so loading=true persists.
        setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
        void pane.switchThread(makeThread({ id: 't' }));
        await Promise.resolve();
        // Items painted from cache.
        expect(pane.items.length).toBe(1);

        // Advance well past the 100ms threshold.
        vi.advanceTimersByTime(500);
        await Promise.resolve();
        // Spinner stayed false because items.length > 0.
        expect(pane.showLoadingSpinner).toBe(false);
      } finally {
        vi.useRealTimers();
      }
    });

    it('above-threshold empty load shows the spinner', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        // Initial load hangs so items stays empty and loading stays true.
        setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
        void pane.switchThread(makeThread({ id: 't' }));
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(false);

        vi.advanceTimersByTime(150);
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(true);
      } finally {
        vi.useRealTimers();
      }
    });

    it('sub-threshold load with items present never shows the spinner', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        const items = [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ];
        setBindingMock('ListThreadSliceAround', async () => ({
          items,
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        const switching = pane.switchThread(makeThread({ id: 't' }));
        // Resolve fully before threshold elapses.
        await switching;

        // Threshold timer was already cleared when loading flipped
        // false; advancing time should not re-trigger anything.
        vi.advanceTimersByTime(500);
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(false);
        expect(pane.loading).toBe(false);
        expect(pane.items.length).toBe(1);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe('switch-away size-priors capture', () => {
    // The mounted timeline holds state the store cannot reach — row-size
    // priors, keyed by scroll-pane width and expansion signature — and the
    // pane's items are the last thing that makes them capturable. Every
    // downstream hook (the timeline's own $effect.pre, the restore effect)
    // runs after `items` has already been replaced, which is why the store
    // asks at the top of `switchThread` instead.
    it('asks the controller to capture BEFORE the incoming thread lands', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-out' }), [
        makeItem({ id: 'row', threadId: 'thread-out' }),
      ]);
      const seenAtCapture: Array<string | null> = [];
      pane.attachScrollController(
        stubScrollController({
          persistSizePriors: () => {
            seenAtCapture.push(pane.threadId);
            seenAtCapture.push(pane.items[0]?.threadId ?? null);
          },
        }),
      );

      await pane.switchThread(makeThread({ id: 'thread-in' }));

      // Called once, and with the OUTGOING thread still in place — a capture
      // taken any later would pair the outgoing engine's measured sizes with
      // the incoming thread's rows.
      expect(seenAtCapture).toEqual(['thread-out', 'thread-out']);
    });

    it('tolerates a pane with no controller attached', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-out' }), [
        makeItem({ id: 'row', threadId: 'thread-out' }),
      ]);

      await expect(pane.switchThread(makeThread({ id: 'thread-in' }))).resolves.toBeUndefined();
    });
  });
});
