// stores/threadRevealGate.svelte.ts
//
// OWNS the reveal GATE: `revealBoundary`, the sequencer that derives it
// (`recomputeReveal`), and the transaction wrapper that makes every smoother
// mutation and its gate derivation one operation. Also owns the two
// disposal entry points that are nothing but "mutate the registry, then
// re-derive" (`disposeSmootherFor`, `disposeSmoothersForItems`).
//
// MUST NOT decide row text. Nothing here reads or writes an item's summary,
// routes a delta, or touches the retained reasoning tails —
// `threadRevealSmoothers.ts` owns those resources and
// `threadStreamingReveal.svelte.ts` owns the row-text chokepoint. The gate
// keys purely off smoother liveness plus (turnIndex, itemIndex) order.

import type { Item } from '../types/models';
import type { RevealBoundary } from '../utils/subagentGrouping';
import { compareItemsByTimelinePosition } from './threadItems';
import { createReentrantTrampoline } from '../utils/reentrantTrampoline';
import {
  throwCollectedErrors,
  type RevealSmootherRegistry,
} from './threadRevealSmoothers';

export interface RevealGateOptions {
  registry: RevealSmootherRegistry;
  /** Current item for an id, or undefined when not loaded. */
  getItemById(itemId: string): Item | undefined;
  /** The current item window, sorted by (turnIndex, itemIndex). */
  getItems(): Item[];
  /** Arm the structural-append spring (pane owns all its gates). */
  armStructuralSpring(): void;
}

export interface RevealGate {
  /** Reveal gate position; null = render everything. */
  readonly revealBoundary: RevealBoundary | null;
  recomputeReveal(): void;
  /**
   * A smoother mutation and its gate derivation are one operation. Cleanup
   * errors must not strand a boundary that still names the removed smoother.
   */
  mutateSmoothersAndRecompute(context: string, mutate: () => void): void;
  disposeSmootherFor(itemId: string): void;
  disposeSmoothersForItems(items: readonly { id: string }[]): void;
  /** Pane clear / thread switch: the gate has no window to describe. */
  clearBoundary(): void;
}

export function createRevealGate(options: RevealGateOptions): RevealGate {
  const { registry } = options;
  const itemSmoothers = registry.smoothers;
  // Reveal gate. While a turn streams, the timeline reveals one top-level
  // item at a time: the next row is withheld until the current item's
  // smoother drains. `revealBoundary` is the position of the item currently
  // being revealed (the "frontier"); MessageTimeline renders nodes up to and
  // including it and withholds anything after via `sliceRevealedNodes`. `null`
  // means no gate — render everything — the steady state outside live
  // streaming. The sequencer (`recomputeReveal`) is the sole writer; it keys
  // purely off smoother liveness + (turnIndex, itemIndex) order, never off
  // `getActiveTurn`, so a between-rounds activeturn flicker can't drop the
  // gate. Subagent children (`parentId` set) never become the frontier, so
  // parallel subagent branches are never serialized behind one another.
  let revealBoundary: RevealBoundary | null = $state(null);
  // Visibility is monotonic within the installed window. A caught-up row can
  // receive more text after a successor was released; that must not unmount
  // the successor on every burst. Keep only its identity/position, not text.
  let revealedThrough: (RevealBoundary & { id: string }) | null = null;

  function lastTopLevelAtOrBefore(boundary: RevealBoundary | null, strict = false): Item | null {
    const items = options.getItems();
    let end = items.length;
    if (boundary !== null) {
      let start = 0;
      while (start < end) {
        const mid = (start + end) >>> 1;
        const item = items[mid];
        const comparison = item.turnIndex - boundary.turnIndex || item.itemIndex - boundary.itemIndex;
        if (comparison < 0 || (!strict && comparison === 0)) start = mid + 1;
        else end = mid;
      }
    }
    for (let i = end - 1; i >= 0; i--) {
      if (!items[i].parentId) return items[i];
    }
    return null;
  }

  // Reveal-gate invariant: the pane's two item-window commit chokepoints
  // recompute after their full transaction, so a caller cannot publish a new
  // window while this boundary still describes the old one. Single-row wire
  // paths recompute inside their streaming-reveal operations. Thread switch
  // and clear dispose every outgoing smoother before installing an unrelated
  // window. There is no parallel reactive watcher over the timeline.

  // Two reveal boundaries are equal when both are null or share a position.
  // Mirrors the `sameActiveTurn` / `sameRhsPanel` equality helpers; the
  // change-guard in `recomputeReveal` uses it so `revealBoundary` is only
  // reassigned when the gate actually moves, not on every streaming chunk
  // (MessageTimeline's `rowDecorations` relies on that via `untrack`).
  function sameBoundary(
    a: RevealBoundary | null,
    b: RevealBoundary | null,
  ): boolean {
    if (a === null || b === null) return a === b;
    return a.turnIndex === b.turnIndex && a.itemIndex === b.itemIndex;
  }

  // A boundary change RELEASES rows only when it moves forward past rows
  // that still exist. An advance to a later frontier always newly reveals
  // that frontier row. A gate drop releases whatever top-level rows sit
  // after the old frontier — nothing, when the drop came from the lone
  // streaming row draining, or from a removal that truncated the tail
  // (revert-on-interrupt drops both frontier and withheld successor in
  // one call), where arming would open a phantom spring window over a
  // SHRINKING timeline. Late text cannot retract released rows. Evaluated
  // against CURRENT items, not previous-pass state, so removal-driven
  // recomputes can't arm off a stale successor observation.
  function boundaryChangeReleasesRows(
    prev: RevealBoundary,
    next: RevealBoundary | null,
  ): boolean {
    if (next !== null) {
      return next.turnIndex > prev.turnIndex
        || (next.turnIndex === prev.turnIndex && next.itemIndex > prev.itemIndex);
    }
    // Gate dropped: released rows exist iff a top-level row sits after the
    // old frontier. `items` is sorted by (turnIndex, itemIndex) and the
    // tail row is usually top-level, so the backward scan is O(1) in
    // practice.
    const items = options.getItems();
    for (let i = items.length - 1; i >= 0; i--) {
      const item = items[i];
      if (item.parentId) continue;
      return item.turnIndex > prev.turnIndex
        || (item.turnIndex === prev.turnIndex && item.itemIndex > prev.itemIndex);
    }
    return false;
  }

  /**
   * Reveal sequencer. Recomputes the reveal frontier from current smoother
   * state and (turnIndex, itemIndex) order, then:
   *   - publishes `revealBoundary` (the frontier's position, or null when no
   *     top-level smoother is mid-reveal — render everything),
   *   - pauses smoothers for withheld successors so they animate from their
   *     start when their turn comes rather than snapping in text that streamed
   *     while hidden, and resumes the frontier.
   *
   * The frontier is never rushed AND never skipped — for every kind, prose
   * and reasoning alike. A withheld successor simply waits for the frontier
   * to drain, in full, at the ordinary reveal cadence (capped by
   * MAX_ADAPTIVE_CHARS_PER_SEC; no rush regime exists any more — the
   * successor-waiting fast-drain was removed 2026-08-05 because the rush
   * read as an unwanted zoom and clustered the released rows into one janky
   * flush, and the bounded-backlog skip that briefly replaced it was
   * rejected because dropping characters is worse than waiting for them).
   *
   * That wait stays short in practice because the wire is BURSTY: tool-call
   * execution, API round-trips, and the model's own pauses are stretches
   * with no appends during which the frontier keeps draining and catches
   * back up to zero. The queue is self-correcting, so a backlog is a
   * transient condition, not a growing one. Do not "fix" a pileup by
   * skipping, rushing, or popping the frontier.
   *
   * The frontier starts at the earliest top-level (`!parentId`) item whose
   * smoother is still revealing, clamped to the last already-released row.
   * Subagent children are excluded so a streaming child never gates a sibling
   * branch or a top-level row. Resumed earlier text cannot retract rows the
   * reader has already seen, but still holds NEW successors behind its drain.
   *
   * INVARIANT: every path that mutates `items` or a smoother's liveness must
   * call this. There is deliberately NO reactive `$effect` watching `items`
   * (frontend/AGENTS.md forbids a parallel watcher over the timeline), so the
   * gate is kept in sync by explicit calls from `applyItemDelta`,
   * `applyItemPatch`, `upsertItemsBatch`, `onReveal` (on catch-up), and the
   * item-removal paths; `disposeAll` clears the boundary directly.
   *
   * Reentrancy: defensive guard against any synchronous `onReveal` fired
   * from within a pass calling back into this function (historically the
   * oversized-backlog snap did exactly that; today's pass only schedules
   * async work, but the guard is cheap and the corruption mode — the
   * outer pass overwriting the boundary and pause/resume decisions the
   * nested pass computed from fresher state — is silent).
   *
   * The re-run loop is CAPPED (`createReentrantTrampoline`). A pass that
   * re-enters on every lap would spin here inside one macrotask with nothing
   * reported — the same unbounded-synchronous-loop class as the quiet-work
   * scheduler and svelte's flush loops, and the same answer: abandon and
   * report.
   */
  const recomputeReveal = createReentrantTrampoline(
    'threadStreamingReveal.recomputeReveal',
    recomputeRevealPass,
  );

  function mutateSmoothersAndRecompute(
    context: string,
    mutate: () => void,
  ): void {
    const errors: unknown[] = [];
    try {
      mutate();
    } catch (error) {
      errors.push(error);
    }
    try {
      recomputeReveal();
    } catch (error) {
      errors.push(error);
    }
    if (errors.length === 1) throw errors[0];
    if (errors.length > 1) {
      throw new AggregateError(errors, `${context} and reveal recompute failed`);
    }
  }

  function disposeSmootherFor(itemId: string): void {
    mutateSmoothersAndRecompute(
      `streaming reveal smoother disposal for ${itemId}`,
      () => registry.disposeSmootherState(itemId),
    );
  }

  function disposeSmoothersForItems(items: readonly { id: string }[]): void {
    if (items.length === 0) return;
    mutateSmoothersAndRecompute('streaming reveal item disposal', () => {
      const errors: unknown[] = [];
      for (const item of items) {
        try {
          registry.disposeSmootherState(item.id);
        } catch (error) {
          errors.push(error);
        }
      }
      throwCollectedErrors(errors, 'streaming reveal item disposal failed');
    });
  }

  function recomputeRevealPass(): void {
    let frontier: Item | null = null;
    for (const [id, entry] of itemSmoothers) {
      const item = options.getItemById(id);
      if (!item || item.parentId) continue;
      if (entry.smoother.isCaughtUp()) continue;
      // Earliest position wins (<= 0 ⇒ item is at or before the frontier).
      if (
        frontier === null ||
        compareItemsByTimelinePosition(item, frontier) <= 0
      ) {
        frontier = item;
      }
    }

    // Clamp only to rows actually released by a previous transaction. Using
    // the current tail here would also release NEW successors prematurely.
    // Reverts/removals retire the marker with its item; a replacement at the
    // same position is new content and must obey sequencing again.
    const retained = revealedThrough && (
      options.getItemById(revealedThrough.id)
      ?? lastTopLevelAtOrBefore(revealedThrough, true)
    );
    if (frontier && retained && compareItemsByTimelinePosition(retained, frontier) > 0) {
      frontier = retained;
    }

    if (frontier) {
      const f = frontier;
      for (const [id, entry] of itemSmoothers) {
        const item = options.getItemById(id);
        if (!item || item.parentId) continue;
        // Withheld successors pause; the frontier (and any earlier top-level
        // smoother, though none should outrank it) resumes. Nothing else
        // happens here — the frontier drains at its own cadence whether or
        // not rows are queued behind it (see the header comment).
        if (compareItemsByTimelinePosition(item, f) > 0) entry.smoother.pause();
        else entry.smoother.resume();
      }
    } else {
      // Nothing is gating — make sure no smoother is left paused (the
      // frontier may have drained between recomputes).
      for (const entry of itemSmoothers.values()) entry.smoother.resume();
    }

    const next: RevealBoundary | null = frontier
      ? { turnIndex: frontier.turnIndex, itemIndex: frontier.itemIndex }
      : null;
    const released = frontier ?? lastTopLevelAtOrBefore(null);
    if (released?.id !== revealedThrough?.id
      || released?.turnIndex !== revealedThrough?.turnIndex
      || released?.itemIndex !== revealedThrough?.itemIndex) {
      revealedThrough = released
        ? { id: released.id, turnIndex: released.turnIndex, itemIndex: released.itemIndex }
        : null;
    }
    const prev = revealBoundary;
    if (!sameBoundary(prev, next)) {
      revealBoundary = next;
      // A boundary change that releases withheld rows mounts them via
      // MessageTimeline's reveal slice — rows already in `pane.items`, so
      // no wire upsert lands in that flush and `applyProviderItemUpserts`'s
      // arm never sees it. Arm the structural-append spring (and stamp
      // the live-content latch — mounting withheld rows IS content
      // advancing) here, synchronously with the release. `prev !== null`
      // skips the gate ENGAGING (which only withholds);
      // `boundaryChangeReleasesRows` skips drops that mount nothing
      // (lone row drained, tail removed). In practice the latch is
      // usually spring-fresh here (onReveal stamps every revealed
      // frame), so this mostly matters for releases landing after a
      // >500ms reveal gap.
      if (prev !== null && boundaryChangeReleasesRows(prev, next)) {
        options.armStructuralSpring();
      }
    }
  }

  return {
    get revealBoundary() {
      return revealBoundary;
    },
    recomputeReveal,
    mutateSmoothersAndRecompute,
    disposeSmootherFor,
    disposeSmoothersForItems,
    clearBoundary() {
      revealBoundary = null;
      revealedThrough = null;
    },
  };
}
