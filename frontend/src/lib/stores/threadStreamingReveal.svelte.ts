import type { Item, ItemKind } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import type { RevealBoundary } from '../utils/subagentGrouping';
import { SvelteMap } from 'svelte/reactivity';
import { PerItemSmoother } from '../markdown/smoothing/PerItemSmoother';
import {
  THINKING_TAIL_RUNES,
  getSmoothingClockForTest,
  isReasoningTailKind,
  isSmoothLiveContentKind,
  trimToTailRunes,
} from './threadPaneShared';
import { compareItemsByTimelinePosition } from './threadItems';
import { getSettings } from './settings.svelte';
import {
  COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  thinkingPayloadVersionForItem,
} from '../utils/payloadVersion';

export interface ThreadStreamingRevealOptions {
  /** Current item for an id, or undefined when not loaded. */
  getItemById(itemId: string): Item | undefined;
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The current item window, sorted by (turnIndex, itemIndex). Re-read on every call — the pane reassigns the array. */
  getItems(): Item[];
  /** Reactive write-through of one row (pane does `items[index] = item`). */
  setItemAt(index: number, item: Item): void;
  /** Stamp the live-content latch (pane's stampLiveContent). */
  stampLiveContent(): void;
  /** Arm the structural-append spring (pane's armStructuralSpring — pane owns all its gates). */
  armStructuralSpring(): void;
  /** rowUiState.appendLivePayloadDeltaForItem — live reasoning-tail payload append. */
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    delta: string,
    payloadVersion?: unknown,
    previousLiveTail?: string,
  ): void;
}

export interface ThreadStreamingReveal {
  /** Reveal gate position; null = render everything. */
  readonly revealBoundary: RevealBoundary | null;
  /** applyItemDelta's smooth-kind path: get-or-create smoother seeded with
   *  currentSummary, push wire updatedAt, append the delta, recompute the gate. */
  appendStreamingDelta(
    itemId: string,
    currentSummary: string,
    delta: string,
    updatedAt: number,
  ): void;
  /** applyItemPatch's smoother decision tree (snap statuses, extend-vs-overwrite,
   *  caught-up terminal dispose, bare-status dispose) followed by an UNCONDITIONAL
   *  recomputeReveal — even when no smoother exists for the id. */
  applyPatch(itemId: string, patch: ItemPatchEvent['patch']): void;
  /** True while a smoother owns the row's summary writes (pane's `stillSmoothing` check). */
  isSmoothing(itemId: string): boolean;
  /** upsertItemsBatch's reconcile block. Does NOT recompute — the batch caller
   *  recomputes once at the end, preserving current per-batch semantics. */
  reconcileUpsertedItems(changedItems: readonly Item[]): void;
  recomputeReveal(): void;
  disposeSmootherFor(itemId: string): void;
  disposeAll(): void;
  /** visibilitychange snap (body of pane's snapSmoothersToReceived, incl. its recomputeReveal). */
  snapAllToReceived(): void;
  /** Live smoother-revealed text for a streaming thinking row, or null. */
  liveThinkingTailFor(itemId: string): string | null;
  debugStats(): { itemSmoothers: number; liveThinkingTails: number };
  /** Test-only: snap + dispose every smoother (body of pane's __flushItemSmoothersForTest). */
  __flushForTest(): void;
  /** Test-only: live smoother count. */
  __smootherCountForTest(): number;
}

/**
 * Per-item smoothing handle stored in the `itemSmoothers` map. Holds the
 * PerItemSmoother plus a closure setter that lets `appendStreamingDelta`
 * push the latest wire `updatedAt` into the smoother's reveal callback
 * without re-creating the closure.
 */
interface ItemSmoothing {
  smoother: PerItemSmoother;
  setLatestUpdatedAt(at: number): void;
}

/**
 * Owns the per-item streaming smoothers and the one-item-at-a-time reveal
 * gate for a thread pane's timeline. The pane data layer remains the sole
 * mutator of `items` — this factory reads the current window through
 * `options.getItems()` / `options.getItemIndex()` and writes rows back
 * through `options.setItemAt()`, so `items[idx] = ...` assignments still
 * happen inside the pane's own reactive scope.
 */
export function createThreadStreamingReveal(
  options: ThreadStreamingRevealOptions,
): ThreadStreamingReveal {
  // Per-item streaming smoothers keyed by item id. Created lazily on
  // the first streaming delta for a smoothable row (assistant_text /
  // thinking); disposed on row removal, status snap, or pane clear.
  // Sibling to itemIndexById / rowUiState so all three life-cycle
  // ride the same clear paths.
  // SvelteMap so `isSmoothing` is reactive across create/dispose:
  // AssistantMessage derives its rendered streaming mode from it
  // (status flips to terminal at WIRE settle, while the drain keeps
  // revealing for seconds — the render must stay in streaming mode
  // until the smoother disposes, or the volatile-tail markdown guards
  // drop while text is still growing). The map mutates per smoother
  // LIFECYCLE (create/dispose), never per reveal frame, so the
  // reactive wrapper costs nothing on the hot path.
  const itemSmoothers: SvelteMap<string, ItemSmoothing> = new SvelteMap();
  // Live full revealed text for streaming thinking rows, keyed by item
  // id. Sibling to `itemSmoothers`: written from every onReveal and
  // deleted on every smoother dispose path. Decouples the collapsed
  // ThinkingBlock render from `items[].summary` (which is trimmed to
  // THINKING_TAIL_RUNES for memory and persistence). The trimmed summary
  // sliding-window forces the collapsed `<span>{bodyText}</span>` to
  // re-wrap its full string on every reveal — `whitespace-pre-wrap`
  // + `max-h-[3lh] overflow-hidden` + `scrollTop = scrollHeight` then
  // shifts the visible 3 lines wholesale whenever a char drop near the
  // start lets a word cross a wrap boundary, producing the user-visible
  // "5 words appear at once past 400 runes" symptom. Reading the live
  // tail instead gives the span monotonically-growing content so wrap
  // layout never reshuffles older text — only the bottom 3 lines scroll
  // up as content arrives. SvelteMap so Map.get inside a $derived
  // re-runs on Map.set. Cleared in the same paths that clear
  // itemSmoothers.
  const itemLiveThinkingTail: SvelteMap<string, string> = new SvelteMap();
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

  // Reveal-gate invariant: any wholesale `items` replacement that can
  // change which top-level rows exist relative to a live smoother must
  // pair with `recomputeReveal()` (or `disposeAll()`, which clears the
  // boundary). Current callers all hold this: switchThread / clear →
  // disposeAll; removeItem / removeItemsForTurns → recomputeReveal. The
  // `loadOlder` merge is the deliberate exception — it only prepends
  // OLDER rows (before any streaming frontier by (turnIndex, itemIndex)),
  // which can be neither the frontier nor a gated successor, so the
  // boundary is unaffected and no recompute is needed. A new mutation
  // path that can append rows during a turn MUST call recomputeReveal —
  // there is no reactive backstop (a parallel $effect over the timeline
  // is forbidden; see frontend/AGENTS.md).

  function disposeSmootherFor(itemId: string): void {
    const entry = itemSmoothers.get(itemId);
    if (!entry) return;
    entry.smoother.dispose();
    itemSmoothers.delete(itemId);
    itemLiveThinkingTail.delete(itemId);
  }

  function disposeAll(): void {
    for (const entry of itemSmoothers.values()) entry.smoother.dispose();
    itemSmoothers.clear();
    itemLiveThinkingTail.clear();
    revealBoundary = null;
  }

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
  // SHRINKING timeline. A retreat (a replay delta re-creating a smoother
  // for an earlier row) only withholds and never releases. Evaluated
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
   *     while hidden, and resumes the frontier,
   *   - fast-drains the frontier when any later top-level row is already
   *     waiting, so the next row appears quickly (rate-ceilinged — see
   *     FAST_DRAIN_MAX_CHARS_PER_SEC) instead of stalling behind a long
   *     (often collapsed) thinking block.
   *
   * The frontier is the earliest top-level (`!parentId`) item whose smoother
   * is still revealing. Subagent children are excluded so a streaming child
   * never gates a sibling branch or a top-level row.
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
   */
  let recomputingReveal = false;
  let recomputeRevealAgain = false;
  function recomputeReveal(): void {
    if (recomputingReveal) {
      recomputeRevealAgain = true;
      return;
    }
    recomputingReveal = true;
    try {
      do {
        recomputeRevealAgain = false;
        recomputeRevealPass();
      } while (recomputeRevealAgain);
    } finally {
      recomputingReveal = false;
    }
  }

  function recomputeRevealPass(): void {
    const items = options.getItems();
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

    if (frontier) {
      const f = frontier;
      // A successor is any later TOP-LEVEL row. `items` is sorted by
      // (turnIndex, itemIndex), so scan FORWARD from the frontier's index
      // instead of the whole array — the common case (streaming the tail
      // row with nothing after it yet) then costs O(1), not O(items), on
      // the per-chunk hot path.
      let hasSuccessor = false;
      const frontierIdx = options.getItemIndex(f.id) ?? -1;
      for (let i = frontierIdx + 1; i < items.length; i++) {
        if (!items[i].parentId) {
          hasSuccessor = true;
          break;
        }
      }
      for (const [id, entry] of itemSmoothers) {
        const item = options.getItemById(id);
        if (!item || item.parentId) continue;
        // Withheld successors pause; the frontier (and any earlier top-level
        // smoother, though none should outrank it) resumes.
        if (compareItemsByTimelinePosition(item, f) > 0) entry.smoother.pause();
        else entry.smoother.resume();
      }
      if (hasSuccessor) {
        // Drain at the elevated (finite) per-tick cap so the finish
        // reads as motion. Deliberately NO snap valve for oversized
        // backlogs: a wholesale reveal is a single giant re-parse plus
        // a multi-viewport layout jump, and the successor waiting a few
        // extra seconds behind a rate-ceilinged drain is the better
        // trade (the drain rate is bounded by
        // FAST_DRAIN_MAX_CHARS_PER_SEC, so even an essay-sized backlog
        // reveals as fast intentional streaming).
        itemSmoothers.get(f.id)?.smoother.requestFastDrain();
      }
    } else {
      // Nothing is gating — make sure no smoother is left paused (the
      // frontier may have drained between recomputes).
      for (const entry of itemSmoothers.values()) entry.smoother.resume();
    }

    const next: RevealBoundary | null = frontier
      ? { turnIndex: frontier.turnIndex, itemIndex: frontier.itemIndex }
      : null;
    const prev = revealBoundary;
    if (!sameBoundary(prev, next)) {
      revealBoundary = next;
      // A boundary change that releases withheld rows mounts them via
      // MessageTimeline's reveal slice — rows already in `pane.items`, so
      // no wire upsert lands in that flush and `applyProviderItemUpserts`'s
      // arm never sees it. Arm the structural-append spring here,
      // synchronously with the release. `prev !== null` skips the gate
      // ENGAGING (which only withholds); `boundaryChangeReleasesRows`
      // skips drops that mount nothing (lone row drained, tail removed).
      // In practice the latch is usually spring-fresh here (onReveal
      // stamps every revealed frame), so this mostly matters for releases
      // landing after a >500ms reveal gap.
      if (prev !== null && boundaryChangeReleasesRows(prev, next)) {
        options.armStructuralSpring();
      }
    }
  }

  // The payload-expansion namespace a reasoning-tail row reads from, matched by
  // the row component so a mid-stream live delta lands where an expand will
  // read it.
  function reasoningExpansionStateKey(kind: ItemKind | string): string {
    return kind === 'compaction_reasoning'
      ? COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY
      : THINKING_PAYLOAD_EXPANSION_STATE_KEY;
  }

  function getOrCreateSmoothing(
    itemId: string,
    initialReceived: string,
  ): ItemSmoothing {
    const existing = itemSmoothers.get(itemId);
    if (existing) return existing;

    // Closure state for this item's smoother. Updated by each delta
    // and read inside `onReveal` so the row's `updatedAt` stays close
    // to wire time even as the smoother lags.
    let latestUpdatedAt = 0;
    // Previous revealed text — passed as `previousLiveTail` when a
    // thinking row's live-payload expansion is active so the live tail
    // stays in sync with the smoothed cursor.
    let previousRevealed = initialReceived;

    const smoother = new PerItemSmoother({
      initialReceived,
      // Seed revealed = received so a mid-flight feature deploy or
      // turn-resume sees no visible snap.
      initialRevealed: initialReceived,
      // Reveal the whole received backlog per wire chunk (one mutation
      // per chunk, a few Hz) instead of the animated 48–60Hz cadence.
      // Two independent settings want this, for different reasons:
      //   - lowPowerMode: minimise per-frame render work. The live
      //     volatile tail is still shown, just without the word-by-word
      //     animation.
      //   - streamingEnabled === false: the user opted out of live
      //     streaming and wants text to appear one committed markdown
      //     block at a time (ChatMarkdown withholds the volatile tail).
      //     For that gate to reflect WIRE arrival rather than a
      //     rate-limited crawl, the smoother must pass `received`
      //     straight through — otherwise a committed block would only
      //     surface after the animation had already inched through it.
      // The two stay orthogonal: low power governs the reveal ANIMATION;
      // the streaming toggle governs whether the in-progress block is
      // shown at all. All the onReveal invariants (live-content stamp,
      // reasoning tail, terminal auto-dispose, gate recompute) run
      // unchanged — the snap just delivers the whole backlog in one
      // reveal.
      revealImmediately: () =>
        getSettings().lowPowerMode || !getSettings().streamingEnabled,
      clock: getSmoothingClockForTest(),
      onReveal: (revealed, delta) => {
        const idx = options.getItemIndex(itemId);
        if (idx === undefined) {
          smoother.dispose();
          itemSmoothers.delete(itemId);
          itemLiveThinkingTail.delete(itemId);
          return;
        }
        // A reveal is genuine live content advancing the bottom — stamp
        // so the controller spring-chases it. Runs every revealed frame,
        // INCLUDING the multi-second drain tail after the wire turn ends
        // (the smoother keeps revealing until caught up), which is what
        // makes the end-of-turn tail spring instead of jump.
        options.stampLiveContent();
        const current = options.getItems()[idx];
        const prevRevealed = previousRevealed;
        previousRevealed = revealed;
        // Reasoning-tail rows (thinking + compaction_reasoning) keep the
        // summary tail-trimmed for memory; assistant_text keeps the full
        // revealed text.
        const isReasoningTail = isReasoningTailKind(current.kind);
        const nextSummary = isReasoningTail
          ? trimToTailRunes(revealed, THINKING_TAIL_RUNES)
          : revealed;
        // Keep the row's `updatedAt` monotonic. A status-only patch
        // (e.g. bare `{status: 'completed', updatedAt: T}`) can land
        // between deltas and bump `current.updatedAt` past the
        // smoother's last-known wire delta; the older value must not
        // overwrite it when the next rAF reveal lands.
        const nextItem = {
          ...current,
          summary: nextSummary,
          updatedAt: Math.max(latestUpdatedAt, current.updatedAt),
        };
        options.setItemAt(idx, nextItem);
        if (isReasoningTail) {
          itemLiveThinkingTail.set(itemId, revealed);
          options.appendLivePayloadDeltaForItem(
            nextItem.id,
            reasoningExpansionStateKey(nextItem.kind),
            delta,
            thinkingPayloadVersionForItem(nextItem),
            prevRevealed,
          );
        }
        // Auto-cleanup once the stream has settled AND the smoother has
        // caught up. After that point no more deltas will arrive and
        // the smoother is dormant; holding the map slot would just
        // wait for the next thread switch. Terminal-status paths
        // (upsert reconcile and `applyItemPatch`'s snap branch) both
        // dispose synchronously before any further rAF fires, so this
        // never tramples an authoritative summary.
        if (current.status !== 'streaming' && smoother.isCaughtUp()) {
          smoother.dispose();
          itemSmoothers.delete(itemId);
          itemLiveThinkingTail.delete(itemId);
        }
        // Advance the reveal gate the moment the frontier catches up so the
        // withheld successor reveals in the same frame, without waiting on an
        // unrelated wire event.
        if (smoother.isCaughtUp()) recomputeReveal();
      },
    });

    const entry: ItemSmoothing = {
      smoother,
      setLatestUpdatedAt(at) {
        latestUpdatedAt = at;
      },
    };
    itemSmoothers.set(itemId, entry);
    return entry;
  }

  /** applyItemDelta's smooth-kind path: get-or-create smoother seeded with
   *  currentSummary, push wire updatedAt, append the delta, recompute the gate. */
  function appendStreamingDelta(
    itemId: string,
    currentSummary: string,
    delta: string,
    updatedAt: number,
  ): void {
    const entry = getOrCreateSmoothing(itemId, currentSummary);
    entry.setLatestUpdatedAt(updatedAt);
    entry.smoother.appendDelta(delta);
    // A new smoothed row (or fresh lag on the frontier) may move the gate;
    // recompute so a withheld successor pauses and the frontier fast-drains.
    recomputeReveal();
  }

  /** applyItemPatch's smoother decision tree (snap statuses, extend-vs-overwrite,
   *  caught-up terminal dispose, bare-status dispose) followed by an UNCONDITIONAL
   *  recomputeReveal — even when no smoother exists for the id. */
  function applyPatch(itemId: string, patch: ItemPatchEvent['patch']): void {
    const smoothing = itemSmoothers.get(itemId);
    const nextStatus = patch.status;
    // `errored`, `killed`, and `declined` all represent terminal
    // states where the user has either explicitly stopped the
    // stream or the provider failed it. In all three, we want the
    // already-streamed text to be fully visible before the patch's
    // summary (which may include an "[interrupted] " prefix or
    // similar) takes over — so snap synchronously and dispose.
    const isSnapStatus =
      nextStatus === 'errored' ||
      nextStatus === 'killed' ||
      nextStatus === 'declined';

    // Cancel / interrupt / error: synchronously reveal everything in
    // the smoother before applying the patch, then dispose. The
    // patch's own summary (e.g. "[interrupted] …") then lands as
    // the final visible text without being overwritten by a trailing
    // rAF tick.
    if (smoothing && isSnapStatus) {
      smoothing.smoother.snap();
      disposeSmootherFor(itemId);
    } else if (smoothing && patch.summary !== undefined) {
      // Status flipping to completed (or any non-snap patch) may
      // carry a final summary. If it extends what the smoother has
      // already received, push the suffix as a delta so the smoother
      // finishes the reveal naturally. If it doesn't extend (an
      // overwrite or a backwards correction), snap and dispose so
      // the patch's summary wins cleanly.
      const received = smoothing.smoother.getReceived();
      const patchSummary = patch.summary;
      // Reasoning-tail rows (thinking + compaction_reasoning) persist —
      // and settle with — the tail-trimmed preview, not the full text
      // (triage's thinkingSummaryPreview; both sides trim to the last
      // THINKING_TAIL_RUNES code points with no marker, so the strings
      // are byte-identical). A settle patch whose summary equals the
      // trimmed received text is a re-assert of what the smoother
      // already has, NOT an overwrite; treating it as a mismatch would
      // snap+dispose mid-drain and dump the unrevealed backlog
      // wholesale (the Codex thinking completion shape, and the
      // completion patch from persistCompletedBlockEmitStreaming).
      const item = options.getItemById(itemId);
      const summaryMatchesReceived =
        patchSummary === received ||
        (item !== undefined &&
          isReasoningTailKind(item.kind) &&
          patchSummary === trimToTailRunes(received, THINKING_TAIL_RUNES));
      if (summaryMatchesReceived) {
        if (
          nextStatus !== undefined &&
          nextStatus !== 'streaming' &&
          smoothing.smoother.isCaughtUp()
        ) {
          // Terminal status AND nothing left to reveal. No further rAF
          // tick will fire, so the onReveal auto-cleanup can't dispose
          // — do it here or the smoother (and its itemLiveThinkingTail
          // entry) leaks until the next thread switch. This is the
          // completion shape wherever content-block-stop carries
          // ContentPresent=true (Codex always; Claude recovered
          // blocks): the settle re-asserts the summary the smoother
          // already received. The bare-status branch below only covers
          // the case where that equal summary is OMITTED from the
          // patch. A not-yet-caught-up smoother keeps draining and
          // disposes via onReveal once it catches up (applyItemPatch
          // skips the direct summary write while it lives).
          disposeSmootherFor(itemId);
        }
      } else if (patchSummary.startsWith(received)) {
        if (patch.updatedAt !== undefined) {
          smoothing.setLatestUpdatedAt(patch.updatedAt);
        }
        smoothing.smoother.appendDelta(patchSummary.slice(received.length));
      } else {
        smoothing.smoother.snap();
        disposeSmootherFor(itemId);
      }
    } else if (
      smoothing &&
      nextStatus !== undefined &&
      nextStatus !== 'streaming' &&
      smoothing.smoother.isCaughtUp()
    ) {
      // Bare status patch transitioning out of streaming with no
      // summary (e.g. `{status: 'completed', updatedAt: T}`). The
      // `onReveal` auto-cleanup only fires on a subsequent rAF tick;
      // if the smoother is already caught up, no further ticks will
      // arrive and the `itemSmoothers` + `itemLiveThinkingTail`
      // entries would leak until the next thread switch. Non-caught-
      // up smoothers keep streaming text and dispose via `onReveal`
      // once they catch up (the status check at line 732).
      disposeSmootherFor(itemId);
    }

    // Snap/dispose above may have cleared the frontier (interrupt, error,
    // completion); recompute so the gate drops and any withheld tail rows
    // reveal. Runs before the early `itemsAreEqual` return in applyItemPatch.
    recomputeReveal();
  }

  function isSmoothing(itemId: string): boolean {
    return itemSmoothers.has(itemId);
  }

  /** upsertItemsBatch's reconcile block. Does NOT recompute — the batch caller
   *  recomputes once at the end, preserving current per-batch semantics. */
  function reconcileUpsertedItems(changedItems: readonly Item[]): void {
    // Reconcile per-item smoothers with the upsert state. A completion /
    // failure upsert replaces items[index] entirely, so a still-running
    // smoother would write stale partial reveals back over the new
    // summary on its next tick. Dispose on any terminal-status upsert.
    // For streaming upserts whose summary extends what the smoother has
    // already received, append the suffix so the smoother continues
    // toward the new target; on a non-extending mismatch, dispose so
    // the next delta seeds a fresh smoother from the new summary.
    if (itemSmoothers.size === 0) return;
    for (const it of changedItems) {
      const entry = itemSmoothers.get(it.id);
      if (!entry) continue;
      if (it.status !== 'streaming') {
        entry.smoother.dispose();
        itemSmoothers.delete(it.id);
        itemLiveThinkingTail.delete(it.id);
        continue;
      }
      if (!isSmoothLiveContentKind(it.kind)) continue;
      const received = entry.smoother.getReceived();
      if (it.summary === received) continue;
      if (
        it.summary.length > received.length &&
        it.summary.startsWith(received)
      ) {
        entry.smoother.appendDelta(it.summary.slice(received.length));
      } else {
        entry.smoother.dispose();
        itemSmoothers.delete(it.id);
        itemLiveThinkingTail.delete(it.id);
      }
    }
  }

  /**
   * Snap every behind smoother straight to its full received text.
   *
   * Wired to `visibilitychange → visible` (App.svelte). `requestAnimationFrame`
   * is suspended while the tab is hidden, but the WebSocket keeps delivering
   * deltas into each smoother's `received` buffer. A turn that streamed — or
   * fully completed — in the background therefore leaves smoothers with a
   * large unrevealed backlog that, on return, would otherwise crawl in at the
   * per-tick cap (~840 cps): a multi-KB response typing itself out for
   * seconds even though it is already done. Before the per-item smoother this
   * never happened — `applyItemDelta` wrote `summary += delta` directly, so a
   * hidden tab showed the full text the instant it regained focus; the rAF
   * reveal gate reintroduced the lag, so this restores the prior behavior on
   * resume without giving up the live-streaming animation.
   *
   * Snapping catches the visible text up to the wire in one frame. Still-
   * streaming rows resume live animation from there (snap leaves the smoother
   * usable); terminal rows dispose through the same onReveal cleanup any
   * caught-up smoother uses. `snap()` no-ops on a caught-up smoother, so this
   * is safe to call unconditionally and costs nothing when nothing is behind.
   */
  function snapAllToReceived(): void {
    if (itemSmoothers.size === 0) return;
    // snap() → onReveal can dispose+delete entries (terminal rows), so
    // iterate a snapshot rather than the live map.
    for (const entry of [...itemSmoothers.values()]) entry.smoother.snap();
    recomputeReveal();
  }

  function liveThinkingTailFor(itemId: string): string | null {
    return itemLiveThinkingTail.get(itemId) ?? null;
  }

  function debugStats(): { itemSmoothers: number; liveThinkingTails: number } {
    return {
      itemSmoothers: itemSmoothers.size,
      liveThinkingTails: itemLiveThinkingTail.size,
    };
  }

  /**
   * Test-only synchronous flush of every per-item streaming smoother
   * in this pane. Snaps each active smoother so items[].summary
   * reflects the full received text immediately, then disposes the
   * entry. Used by tests that assert summary content right after
   * applying deltas without waiting for the smoother's rAF schedule.
   * Not part of the production surface.
   */
  function __flushForTest(): void {
    for (const [id, entry] of itemSmoothers) {
      entry.smoother.snap();
      entry.smoother.dispose();
      itemSmoothers.delete(id);
      itemLiveThinkingTail.delete(id);
    }
  }

  function __smootherCountForTest(): number {
    return itemSmoothers.size;
  }

  return {
    get revealBoundary() {
      return revealBoundary;
    },
    appendStreamingDelta,
    applyPatch,
    isSmoothing,
    reconcileUpsertedItems,
    recomputeReveal,
    disposeSmootherFor,
    disposeAll,
    snapAllToReceived,
    liveThinkingTailFor,
    debugStats,
    __flushForTest,
    __smootherCountForTest,
  };
}
