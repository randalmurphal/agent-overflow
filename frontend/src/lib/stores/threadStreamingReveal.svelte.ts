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
import { createReentrantTrampoline } from '../utils/reentrantTrampoline';
import { getSettings } from './settings.svelte';
import {
  COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  thinkingPayloadVersionForItem,
} from '../utils/payloadVersion';
import {
  type StreamingAssistantRenderContext,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';
import { createThreadAssistantReveal } from './threadAssistantReveal.svelte';
import type { ProvenAppend } from '../markdown';

/**
 * THE REVEAL INVARIANT (see `stores/AGENTS.md` § The reveal invariant).
 *
 * While a smoother owns an assistant row, the row's published text IS
 * that smoother's reveal cursor. A reconciliation may leave it there,
 * hand the smoother a longer suffix to drain, or hand ownership over
 * with a summary that WINS the row — snapping forward. It may never
 * publish text the reader has already been shown less of: text that
 * rewinds behind the cursor is the one rule five separate 2026-08 perf
 * bugs each broke a different way (aad27067 is the latest).
 *
 * Dev/test only, and DEAD-CODE-ELIMINATED from the production bundle:
 * both operands fold to literals under `vite build`, so the guarded
 * blocks and `getRevealed()` materialization never reach a shipped
 * frame. Mirrors `utils/scroll/observers.ts`'s gate.
 */
const ASSERT_REVEAL_INVARIANT =
  import.meta.env.DEV || import.meta.env.MODE === 'test';

/**
 * Tripwire for the invariant above. Exported for its own unit test —
 * once the chokepoint is correct no production path can trip it, so the
 * guard would otherwise be untested code.
 *
 * `cursor` is the smoother's revealed text at the moment the
 * reconciliation started; `published` is the summary about to become
 * observable. A published string SHORTER than the cursor and a prefix
 * of it is a visible rewind (the 2026-08-29 shape: 1021 chars revealed
 * to ~130, republished at ~130 and stranded). A published string that
 * DIVERGES is a legitimate authoritative overwrite and passes.
 */
export function assertRevealCursorNotRewound(
  itemId: string,
  cursor: string,
  published: string,
): void {
  if (published.length >= cursor.length) return;
  if (!cursor.startsWith(published)) return;
  throw new Error(
    `reveal invariant violated for ${itemId}: reconciliation published ` +
      `${published.length} chars behind the smoother cursor at ${cursor.length}. ` +
      'Row text may never rewind behind an active reveal — keep the smoother ' +
      'draining, or let a genuinely divergent summary win the row outright.',
  );
}

export interface ThreadStreamingRevealOptions {
  /** Current item for an id, or undefined when not loaded. */
  getItemById(itemId: string): Item | undefined;
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The current item window, sorted by (turnIndex, itemIndex). Re-read on every call — the pane reassigns the array. */
  getItems(): Item[];
  /** Reactive write-through of one row (pane does `items[index] = item`). */
  setItemAt(index: number, item: Item): void;
  /**
   * Commit a literal suffix that the reveal router already constructed and
   * preflighted. This mutates the raw row without waking Svelte because every
   * mounted representation receives the same suffix through its direct sink.
   */
  appendDirectAssistantLiteral(
    index: number,
    itemId: string,
    append: ProvenAppend,
    updatedAt: number,
  ): void;
  /** Stamp the live-content latch (pane's stampLiveContent). */
  stampLiveContent(): void;
  /** Arm the structural-append spring and stamp the live-content latch
   *  (pane's armLiveContentAppendSpring — pane owns all its gates). */
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
  /** Bumped whenever pane-wide disposal clears every mounted DOM sink. */
  readonly assistantRevealRegistrationGeneration: number;
  registerAssistantRevealSink(
    itemId: string,
    sink: StreamingAssistantRevealSink,
  ): () => void;
  assistantParserSource(
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ): string;
  /** Opaque lineage proof for the append that produced this exact source. */
  assistantSourceAppend(itemId: string, source: string): ProvenAppend | undefined;
  reconcileItemWrite(previous: Item, next: Item): void;
  /**
   * Prepare rows that are about to replace loaded rows. A full-row echo or
   * snapshot may contain more of a streaming item than the readable reveal has
   * reached. Keep the visible summary at the smoother cursor and absorb any
   * new suffix into the smoother before the replacement becomes observable.
   */
  prepareItemReplacements(incoming: readonly Item[]): Item[];
  recomputeReveal(): void;
  disposeSmootherFor(itemId: string): void;
  disposeSmoothersForItems(items: readonly { id: string }[]): void;
  disposeAll(): void;
  /** visibilitychange snap (body of pane's snapSmoothersToReceived, incl. its recomputeReveal). */
  snapAllToReceived(): void;
  /**
   * Full revealed text for a reasoning-tail row, or null. Live while the
   * row streams, and RETAINED across a content-consistent settle so the
   * collapsed clamp never re-wraps in front of the reader; dropped on
   * overwrite/removal, by the offscreen prune, and on thread switch.
   */
  liveThinkingTailFor(itemId: string): string | null;
  /** Row-UI prune hook: drop retained settled tails not in the retention set. */
  pruneSettledThinkingTails(retainedItemIds: ReadonlySet<string>): void;
  /**
   * How many per-item smoothers are live right now — i.e. how many rows the
   * reveal queue is still draining. The cheap half of `debugStats()`: one
   * map size, no walk over the retained thinking tails, so a poll loop (the
   * harness's reveal-drain probe) can ask it every few hundred milliseconds
   * without becoming part of the load it is measuring.
   */
  smootherCount(): number;
  debugStats(): {
    itemSmoothers: number;
    liveThinkingTails: number;
    liveThinkingTailChars: number;
  };
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
  const assistantReveal = createThreadAssistantReveal({
    getItemIndex: options.getItemIndex,
    getItems: options.getItems,
    setItemAt: options.setItemAt,
    hasSmoother: (itemId) => itemSmoothers.has(itemId),
  });
  // Live full revealed text for streaming thinking rows, keyed by item
  // id. Written from every onReveal. Decouples the collapsed
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
  // re-runs on Map.set.
  //
  // Lifetime: an entry OUTLIVES its smoother across a settle
  // (settleSmootherRetainingTail) — falling back to the trimmed summary
  // at settle re-wraps the clamp's visible lines in front of the reader,
  // because wrap depends on where the string starts and the trim starts
  // mid-sentence. Consistency is enforced at READ time, not by writer
  // discipline: a settled entry is served only while the row's current
  // summary still equals the summary recorded at settle
  // (`settledTailSummaries` below), so any later authoritative summary
  // write — a correction patch, a terminal re-upsert, a whole-window
  // replace — silently invalidates the tail without every write path
  // needing to know it exists. Removal paths drop the entry
  // (disposeSmootherFor), the offscreen row-UI prune reclaims settled
  // entries once the row leaves retention (pruneSettledThinkingTails),
  // a store-side char budget bounds panes whose timeline is unmounted
  // (evictSettledTailsOverBudget — the prune is a MessageTimeline quiet
  // pass and never runs there, e.g. while Settings replaces the pane
  // strip), and disposeAll clears everything on thread switch.
  const itemLiveThinkingTail: SvelteMap<string, string> = new SvelteMap();
  // For each SETTLED retained tail: the row summary recorded at settle
  // (always `trimToTailRunes(tail)` — the settle paths wrote or verified
  // it). `liveThinkingTailFor` compares it against the row's CURRENT
  // summary to decide whether the retained text still describes the row.
  // Plain Map on purpose: reads happen inside row `$derived`s that
  // already track `item.summary` (the prop) and the SvelteMap entry, so
  // every mutation that matters re-runs them without this map being
  // reactive itself. Entries pair 1:1 with settled tail entries; every
  // path that deletes from `itemLiveThinkingTail` deletes here too.
  const settledTailSummaries: Map<string, string> = new Map();
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

  // Reveal-gate invariant: the pane's two item-window commit chokepoints
  // recompute after their full transaction, so a caller cannot publish a new
  // window while this boundary still describes the old one. Single-row wire
  // paths recompute inside their streaming-reveal operations. Thread switch
  // and clear dispose every outgoing smoother before installing an unrelated
  // window. There is no parallel reactive watcher over the timeline.

  // Dispose a smoother and DROP the row's live tail. Correct for every
  // removal/overwrite caller: the row is gone, or its summary no longer
  // matches what was revealed. The tail delete runs unconditionally,
  // BEFORE the missing-smoother early return — a settled row's retained
  // tail (settleSmootherRetainingTail below) has no smoother left, and a
  // removal arriving after that settle must still clear it.
  function disposeSmootherState(itemId: string): void {
    const errors: unknown[] = [];
    try {
      assistantReveal.discardItem(itemId);
    } catch (error) {
      errors.push(error);
    }
    itemLiveThinkingTail.delete(itemId);
    settledTailSummaries.delete(itemId);
    const entry = itemSmoothers.get(itemId);
    if (entry) {
      try {
        entry.smoother.dispose();
      } catch (error) {
        errors.push(error);
      } finally {
        itemSmoothers.delete(itemId);
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(
        errors,
        `streaming reveal smoother disposal failed for ${itemId}`,
      );
    }
  }

  // Store-side bound on settled-tail retention, independent of any
  // mounted timeline: the offscreen row-UI prune is a MessageTimeline
  // quiet pass, and a pane can keep settling reasoning rows while its
  // timeline is unmounted (Settings replaces the whole pane strip; the
  // wire keeps fanning deltas to matching panes). Sized for dozens of
  // large thinking blocks — far more than any visible window needs —
  // while capping what a long unattended session can pin.
  const SETTLED_TAIL_BUDGET_CHARS = 131_072;

  function evictSettledTailsOverBudget(): void {
    let totalChars = 0;
    for (const [id, text] of itemLiveThinkingTail) {
      if (!itemSmoothers.has(id)) totalChars += text.length;
    }
    if (totalChars <= SETTLED_TAIL_BUDGET_CHARS) return;
    // Map iteration order is insertion order, which for settled entries
    // is stream order — evict oldest first. Live entries are never
    // evicted; their reveal owns them.
    for (const [id, text] of itemLiveThinkingTail) {
      if (totalChars <= SETTLED_TAIL_BUDGET_CHARS) break;
      if (itemSmoothers.has(id)) continue;
      itemLiveThinkingTail.delete(id);
      settledTailSummaries.delete(id);
      totalChars -= text.length;
    }
  }

  // Dispose a smoother at settle, RETAINING the live-tail entry (present
  // only for reasoning-tail rows) and recording the summary it settled
  // with. The collapsed clamp is already rendering exactly that string;
  // swapping to the tail-trimmed summary at settle re-wraps the visible
  // lines — the "think text shifts right as the response mounts" flicker
  // — because wrap layout depends on where the string starts and the
  // trim starts mid-sentence. The recorded summary is what makes the
  // retention safe WITHOUT trusting callers: `liveThinkingTailFor`
  // serves the tail only while the row's current summary still equals
  // it, so a later summary rewrite invalidates the tail at read time.
  function settleSmootherRetainingTail(itemId: string): void {
    const entry = itemSmoothers.get(itemId);
    if (!entry) return;
    const errors: unknown[] = [];
    try {
      assistantReveal.clearPresentation(itemId);
    } catch (error) {
      errors.push(error);
    }
    try {
      entry.smoother.dispose();
    } catch (error) {
      errors.push(error);
    } finally {
      itemSmoothers.delete(itemId);
    }
    const tail = itemLiveThinkingTail.get(itemId);
    if (tail !== undefined) {
      settledTailSummaries.set(itemId, trimToTailRunes(tail, THINKING_TAIL_RUNES));
      evictSettledTailsOverBudget();
    }
    if (errors.length > 0) {
      throw new AggregateError(
        errors,
        `streaming reveal smoother settle failed for ${itemId}`,
      );
    }
  }

  function disposeAll(): void {
    const errors: unknown[] = [];
    for (const entry of itemSmoothers.values()) {
      try {
        entry.smoother.dispose();
      } catch (error) {
        errors.push(error);
      }
    }
    itemSmoothers.clear();
    itemLiveThinkingTail.clear();
    settledTailSummaries.clear();
    try {
      assistantReveal.disposeAll();
    } catch (error) {
      errors.push(error);
    }
    revealBoundary = null;
    if (errors.length > 0) {
      throw new AggregateError(errors, 'streaming reveal disposal failed');
    }
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

  /**
   * A smoother mutation and its gate derivation are one operation. Cleanup
   * errors must not strand a boundary that still names the removed smoother.
   * Preserve a lone original error for callers that match its diagnostic;
   * report both failures when the cleanup and the derivation fail.
   */
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

  function throwCollectedErrors(errors: readonly unknown[], context: string): void {
    if (errors.length === 1) throw errors[0];
    if (errors.length > 1) throw new AggregateError(errors, context);
  }

  function runRevealTransaction(itemId: string, reveal: () => void): void {
    try {
      reveal();
      return;
    } catch (failure) {
      // PerItemSmoother advances its cursor before invoking this callback. A
      // failed row write, payload update, or sink transition cannot be retried
      // on another frame. Drop the now-unusable smoother and re-derive the
      // gate before surfacing the failure, or one bad row permanently
      // withholds every successor behind its stale frontier.
      const errors: unknown[] = [failure];
      if (itemSmoothers.has(itemId)) {
        try {
          disposeSmootherState(itemId);
        } catch (error) {
          errors.push(error);
        }
      }
      try {
        recomputeReveal();
      } catch (error) {
        errors.push(error);
      }
      throwCollectedErrors(
        errors,
        `streaming reveal callback recovery failed for ${itemId}`,
      );
    }
  }

  function disposeSmootherFor(itemId: string): void {
    mutateSmoothersAndRecompute(
      `streaming reveal smoother disposal for ${itemId}`,
      () => disposeSmootherState(itemId),
    );
  }

  function disposeSmoothersForItems(items: readonly { id: string }[]): void {
    if (items.length === 0) return;
    mutateSmoothersAndRecompute('streaming reveal item disposal', () => {
      const errors: unknown[] = [];
      for (const item of items) {
        try {
          disposeSmootherState(item.id);
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

    // A retained settled tail is the full text the row is still
    // rendering; a smoother re-created after that settle (a replay
    // upsert flipping the row back to streaming, then a delta) must
    // seed from it — seeding from the tail-trimmed summary would shrink
    // the rendered string and re-wrap the clamp. Only when consistent:
    // the summary recorded at settle must still be the current summary,
    // else the row was overwritten since and the stale tail is dropped
    // here rather than left to shadow the resumed reveal for a frame.
    const retainedTail = itemLiveThinkingTail.get(itemId);
    if (retainedTail !== undefined) {
      if (settledTailSummaries.get(itemId) === initialReceived) {
        initialReceived = retainedTail;
      } else {
        itemLiveThinkingTail.delete(itemId);
        settledTailSummaries.delete(itemId);
      }
    }

    // Closure state for this item's smoother. Updated by each delta
    // and read inside `onReveal` so the row's `updatedAt` stays close
    // to wire time even as the smoother lags.
    let latestUpdatedAt = 0;
    // Full previous revealed text. Appending each emitted delta here keeps a
    // canonical cons string without asking the smoother to join its whole
    // received buffer. Reasoning also passes the previous value into its live
    // payload expansion so that view stays on the same cursor.
    let previousRevealed = initialReceived;

    const smoother = new PerItemSmoother({
      initialReceived,
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
      onReveal: (delta, _revealedEnd, previousCodeUnit) => runRevealTransaction(itemId, () => {
        const idx = options.getItemIndex(itemId);
        if (idx === undefined) {
          disposeSmootherFor(itemId);
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
        // Reasoning-tail rows (thinking + compaction_reasoning) keep the
        // summary tail-trimmed for memory; assistant_text keeps the full
        // revealed text.
        const isReasoningTail = isReasoningTailKind(current.kind);
        const settling = current.status !== 'streaming' && smoother.isCaughtUp();
        const routedThroughAssistantReveal = !isReasoningTail &&
          !settling &&
          current.summary === prevRevealed;
        if (routedThroughAssistantReveal) {
          assistantReveal.publish(
            itemId,
            previousCodeUnit,
            prevRevealed,
            delta,
            (nextSummary, mode, append) => {
              // Keep the smoother cursor and canonical row on the exact same
              // string. Building `prevRevealed + delta` independently here
              // created two growing cons trees per reveal and retained both
              // for the lifetime of the turn.
              const updatedAt = Math.max(latestUpdatedAt, current.updatedAt);
              switch (mode) {
                case 'direct':
                  options.appendDirectAssistantLiteral(
                    idx,
                    itemId,
                    append,
                    updatedAt,
                  );
                  break;
                case 'authoritative':
                  options.setItemAt(idx, {
                    ...current,
                    summary: nextSummary,
                    updatedAt,
                  });
                  break;
                default:
                  mode satisfies never;
              }
              previousRevealed = nextSummary;
            },
          );
        } else {
          // Keep the row's `updatedAt` monotonic. A status-only patch
          // (e.g. bare `{status: 'completed', updatedAt: T}`) can land
          // between deltas and bump `current.updatedAt` past the
          // smoother's last-known wire delta; the older value must not
          // overwrite it when the next rAF reveal lands.
          const updatedAt = Math.max(latestUpdatedAt, current.updatedAt);
          if (!isReasoningTail && current.summary === prevRevealed) {
            // Publish before the reactive write. Its reconciliation hook can
            // then preserve the complete pending direct suffix instead of
            // claiming only this last delta after an equal-length rewrite.
            assistantReveal.commitAuthoritativeAppend(
              itemId,
              prevRevealed,
              delta,
              (revealed) => {
                previousRevealed = revealed;
                options.setItemAt(idx, {
                  ...current,
                  summary: revealed,
                  updatedAt,
                });
              },
            );
          } else {
            const revealed = prevRevealed + delta;
            previousRevealed = revealed;
            if (!isReasoningTail) assistantReveal.discardItem(itemId);
            const nextSummary = isReasoningTail
              ? trimToTailRunes(revealed, THINKING_TAIL_RUNES)
              : revealed;
            const nextItem = {
              ...current,
              summary: nextSummary,
              updatedAt,
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
          }
        }
        // Auto-cleanup once the stream has settled AND the smoother has
        // caught up. After that point no more deltas will arrive and
        // the smoother is dormant; holding the map slot would just
        // wait for the next thread switch. Terminal-status paths
        // (upsert reconcile and `applyItemPatch`'s snap branch) both
        // dispose synchronously before any further rAF fires, so this
        // never tramples an authoritative summary. The live tail is
        // retained — this reveal just wrote the summary as the trimmed
        // view of it, the definition of a content-consistent settle.
        if (smoother.isCaughtUp()) {
          // Advance the reveal gate even when terminal sink cleanup fails.
          // The smoother has already moved its cursor before this callback,
          // so returning with the old frontier would withhold every successor
          // despite there being no remaining reveal work.
          mutateSmoothersAndRecompute(
            `streaming reveal settle for ${itemId}`,
            () => {
              if (current.status !== 'streaming') {
                settleSmootherRetainingTail(itemId);
              }
            },
          );
        }
      }),
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
    // recompute so a withheld successor pauses behind the frontier.
    recomputeReveal();
  }

  /** applyItemPatch's smoother decision tree (snap statuses, extend-vs-overwrite,
   *  caught-up terminal dispose, bare-status dispose) followed by an UNCONDITIONAL
   *  recomputeReveal — even when no smoother exists for the id. */
  function snapAndDisposeSmoother(itemId: string, smoothing: ItemSmoothing): void {
    const errors: unknown[] = [];
    try {
      smoothing.smoother.snap();
    } catch (error) {
      errors.push(error);
    }
    try {
      disposeSmootherState(itemId);
    } catch (error) {
      errors.push(error);
    }
    throwCollectedErrors(errors, `streaming reveal snap disposal failed for ${itemId}`);
  }

  function isSnapStatus(status: Item['status'] | undefined): boolean {
    return status === 'errored' || status === 'killed' || status === 'declined';
  }

  function applyPatchState(itemId: string, patch: ItemPatchEvent['patch']): void {
    const smoothing = itemSmoothers.get(itemId);
    const nextStatus = patch.status;
    // `errored`, `killed`, and `declined` all represent terminal
    // states where the user has either explicitly stopped the
    // stream or the provider failed it. In all three, we want the
    // already-streamed text to be fully visible before the patch's
    // summary (which may include an "[interrupted] " prefix or
    // similar) takes over — so snap synchronously and dispose.
    // Cancel / interrupt / error: synchronously reveal everything in
    // the smoother before applying the patch, then dispose. The
    // patch's own summary (e.g. "[interrupted] …") then lands as
    // the final visible text without being overwritten by a trailing
    // rAF tick.
    if (smoothing && isSnapStatus(nextStatus)) {
      snapAndDisposeSmoother(itemId, smoothing);
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
      if (summaryRepresentsReceived(item?.kind, patchSummary, received)) {
        if (
          nextStatus !== undefined &&
          nextStatus !== 'streaming' &&
          smoothing.smoother.isCaughtUp()
        ) {
          // Terminal status AND nothing left to reveal. No further rAF
          // tick will fire, so the onReveal auto-cleanup can't dispose
          // — do it here or the smoother leaks until the next thread
          // switch. This is the completion shape wherever
          // content-block-stop carries ContentPresent=true (Codex
          // always; Claude recovered blocks): the settle re-asserts
          // the summary the smoother already received — content-
          // consistent by the equality check above, so the live tail
          // is retained. The bare-status branch below only covers the
          // case where that equal summary is OMITTED from the patch. A
          // not-yet-caught-up smoother keeps draining and disposes via
          // onReveal once it catches up (applyItemPatch skips the
          // direct summary write while it lives).
          settleSmootherRetainingTail(itemId);
        }
      } else {
        const absorbed = absorbReceivedSuffix(
          smoothing,
          patchSummary,
          received,
          patch.updatedAt,
        );
        if (!absorbed) snapAndDisposeSmoother(itemId, smoothing);
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
      // arrive and the `itemSmoothers` entry would leak until the next
      // thread switch. The row's summary was last written by onReveal
      // as the trimmed view of the revealed text — content-consistent,
      // so the live tail is retained. Non-caught-up smoothers keep
      // streaming text and dispose via `onReveal` once they catch up.
      settleSmootherRetainingTail(itemId);
    }

  }

  function applyPatch(itemId: string, patch: ItemPatchEvent['patch']): void {
    // Snap/dispose above may clear the frontier (interrupt, error,
    // completion). Derive the gate even when sink cleanup reports an error so
    // a failed reset cannot leave successor rows withheld indefinitely. This
    // still runs before applyItemPatch's early `itemsAreEqual` return.
    mutateSmoothersAndRecompute(
      `streaming reveal patch for ${itemId}`,
      () => applyPatchState(itemId, patch),
    );
  }

  function isSmoothing(itemId: string): boolean {
    return itemSmoothers.has(itemId);
  }

  function registerAssistantRevealSink(
    itemId: string,
    sink: StreamingAssistantRevealSink,
  ): () => void {
    return assistantReveal.register(itemId, sink);
  }

  function assistantParserSource(
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ): string {
    return assistantReveal.parserSource(itemId, canonicalSource, renderContext);
  }

  function assistantSourceAppend(
    itemId: string,
    source: string,
  ): ProvenAppend | undefined {
    return assistantReveal.sourceAppend(itemId, source);
  }

  function reconcileItemWrite(previous: Item, next: Item): void {
    assistantReveal.reconcileItemWrite(previous, next);
  }

  function summaryRepresentsReceived(
    kind: ItemKind | string | undefined,
    summary: string,
    received: string,
  ): boolean {
    return summary === received ||
      (kind !== undefined &&
        isReasoningTailKind(kind) &&
        summary === trimToTailRunes(received, THINKING_TAIL_RUNES));
  }

  function absorbReceivedSuffix(
    entry: ItemSmoothing,
    summary: string,
    received: string,
    updatedAt: number | undefined,
  ): boolean {
    if (summary.length <= received.length || !summary.startsWith(received)) {
      return false;
    }
    if (updatedAt !== undefined) entry.setLatestUpdatedAt(updatedAt);
    entry.smoother.appendDelta(summary.slice(received.length));
    return true;
  }

  /**
   * The row-text chokepoint of every wholesale commit: `commitTimelineItems`
   * and `upsertItemsBatch` both publish exactly what this returns. Guard
   * the reveal invariant here — one place covers fold eviction, prune,
   * revert, replica paint and cache install alike.
   *
   * Only assistant PROSE is compared. Reasoning-tail rows publish a
   * tail-TRIMMED view of the cursor, so their length is not a reveal
   * position; snap statuses (`errored` / `killed` / `declined`) are the
   * documented authoritative-summary handover, which snaps the smoother
   * first and then lets the patch's own text ("[interrupted] …") win.
   */
  function prepareItemReplacement(incoming: Item): Item {
    if (!ASSERT_REVEAL_INVARIANT) return prepareItemReplacementRaw(incoming);
    const guardedEntry =
      isSmoothLiveContentKind(incoming.kind) &&
      !isReasoningTailKind(incoming.kind) &&
      !isSnapStatus(incoming.status)
        ? itemSmoothers.get(incoming.id)
        : undefined;
    if (!guardedEntry) return prepareItemReplacementRaw(incoming);
    const cursor = guardedEntry.smoother.getRevealed();
    const prepared = prepareItemReplacementRaw(incoming);
    assertRevealCursorNotRewound(incoming.id, cursor, prepared.summary);
    return prepared;
  }

  function prepareItemReplacementRaw(incoming: Item): Item {
    const entry = itemSmoothers.get(incoming.id);
    if (!entry) return incoming;
    const current = options.getItemById(incoming.id);
    if (!current) {
      disposeSmootherState(incoming.id);
      return incoming;
    }
    if (isSnapStatus(incoming.status)) {
      snapAndDisposeSmoother(incoming.id, entry);
      return incoming;
    }

    if (
      !isSmoothLiveContentKind(incoming.kind) ||
      incoming.kind !== current.kind
    ) {
      disposeSmootherState(incoming.id);
      return incoming;
    }

    const received = entry.smoother.getReceived();
    const incomingSummary = incoming.summary;
    let belongsToCurrentStream = summaryRepresentsReceived(
      incoming.kind,
      incomingSummary,
      received,
    ) || absorbReceivedSuffix(
      entry,
      incomingSummary,
      received,
      incoming.updatedAt,
    );
    // True when the incoming summary is BEHIND the cursor rather than a
    // re-assert of it: the row must keep its own visible text on the way
    // out, whatever happens to the smoother.
    let trailsTheCursor = false;
    if (
      !belongsToCurrentStream &&
      incomingSummary.length < received.length &&
      received.startsWith(incomingSummary)
    ) {
      // A summary that is a strict prefix of `received` belongs to the
      // current stream WHATEVER the row's status. Two producers make it:
      // SQLite/replica snapshots trailing the wire-visible delta stream,
      // and the drain's own partial row re-entering through a wholesale
      // commit (fold eviction, prune, reconcile pass the KEPT rows back
      // through here). The second one is terminal during the post-settle
      // drain — the completion patch flips status while the smoother
      // still owns the summary — and disposing there stranded the row at
      // the partial reveal forever, because the patch's summary write was
      // already skipped in favor of the smoother (incident 2026-08-29:
      // final assistant text froze at ~130 of 1021 chars whenever a
      // subagent child settled inside the drain window).
      belongsToCurrentStream = true;
      trailsTheCursor = true;
    }

    if (!belongsToCurrentStream) {
      disposeSmootherState(incoming.id);
      return incoming;
    }

    entry.setLatestUpdatedAt(Math.max(incoming.updatedAt, current.updatedAt));
    // A terminal echo is authoritative about lifecycle, but not a reason to
    // bypass the readable cursor when its summary is the same append-only
    // stream. Keep the terminal status while the smoother drains the unseen
    // suffix. Mismatches and snap statuses took their authoritative paths
    // above.
    if (incoming.status !== 'streaming' && entry.smoother.isCaughtUp()) {
      settleSmootherRetainingTail(incoming.id);
      // A settle re-assert carries the text the smoother already revealed,
      // so it publishes itself. A snapshot that merely TRAILS the cursor
      // carries less, and handing the row over to it truncates text the
      // reader has already been shown — the same rewind the drain-window
      // case below avoids, reached when the drain happened to finish
      // first. Fall through to the current-summary return instead.
      if (!trailsTheCursor) return incoming;
    }
    if (
      incoming.summary === current.summary &&
      incoming.updatedAt >= current.updatedAt
    ) return incoming;
    return {
      ...incoming,
      summary: current.summary,
      updatedAt: Math.max(incoming.updatedAt, current.updatedAt),
    };
  }

  function prepareItemReplacements(incoming: readonly Item[]): Item[] {
    if (incoming.length === 0 || itemSmoothers.size === 0) return incoming as Item[];
    let prepared: Item[] | null = null;
    const errors: unknown[] = [];
    for (let index = 0; index < incoming.length; index++) {
      const item = incoming[index];
      try {
        const next = prepareItemReplacement(item);
        if (next !== item) {
          prepared ??= incoming.slice() as Item[];
          prepared[index] = next;
        }
      } catch (error) {
        errors.push(error);
      }
    }
    // Preparation mutates smoother ownership before the caller installs the
    // incoming window. A sink-reset failure can therefore abort the commit
    // after its smoother was already removed. Re-derive against the still-
    // current window before the error escapes so the old row is never left
    // behind a boundary whose owner no longer exists.
    if (errors.length > 0) {
      try {
        recomputeReveal();
      } catch (error) {
        errors.push(error);
      }
    }
    throwCollectedErrors(errors, 'streaming reveal item replacement preparation failed');
    return prepared ?? incoming as Item[];
  }

  /**
   * Snap every behind smoother straight to its full received text.
   *
   * Wired to `visibilitychange → visible` (App.svelte). `requestAnimationFrame`
   * is suspended while the tab is hidden, but the WebSocket keeps delivering
   * deltas into each smoother's `received` buffer. A turn that streamed — or
   * fully completed — in the background therefore leaves smoothers with a
   * large unrevealed backlog that, on return, would otherwise crawl in at
   * MAX_ADAPTIVE_CHARS_PER_SEC: a multi-KB response typing itself out for
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
    mutateSmoothersAndRecompute('streaming reveal visibility snap', () => {
      const errors: unknown[] = [];
      // snap() → onReveal can dispose+delete entries (terminal rows), so
      // iterate a snapshot rather than the live map. One broken mounted
      // representation must not prevent the other panes' rows from catching
      // up after a hidden interval.
      for (const [id, entry] of [...itemSmoothers]) {
        try {
          entry.smoother.snap();
        } catch (error) {
          errors.push(error);
        }
        const current = options.getItemById(id);
        if (itemSmoothers.has(id) && current === undefined) {
          try {
            disposeSmootherState(id);
          } catch (error) {
            errors.push(error);
          }
        } else if (
          itemSmoothers.has(id) &&
          current?.status !== 'streaming' &&
          entry.smoother.isCaughtUp()
        ) {
          try {
            settleSmootherRetainingTail(id);
          } catch (error) {
            errors.push(error);
          }
        }
      }
      throwCollectedErrors(errors, 'streaming reveal visibility snap failed');
    });
  }

  function liveThinkingTailFor(itemId: string): string | null {
    const tail = itemLiveThinkingTail.get(itemId);
    if (tail === undefined) return null;
    // Live entry: the reveal that writes the tail also writes the
    // summary as its trimmed view every frame — consistent by
    // construction, no validation needed.
    if (itemSmoothers.has(itemId)) return tail;
    // Settled entry: serve it only while the row's current summary is
    // still the one recorded at settle. This is the whole consistency
    // story for retained tails — a correction patch, a terminal
    // re-upsert (triage re-persists a completed thinking row when a
    // late content-present stop's text differs), or a whole-window
    // replace rewrites the summary without knowing this map exists, and
    // the mismatch invalidates the tail right here at read time.
    const item = options.getItemById(itemId);
    return item !== undefined && item.summary === settledTailSummaries.get(itemId)
      ? tail
      : null;
  }

  /**
   * Offscreen row-UI prune hook: drop retained (settled) live tails for
   * rows outside the retention window, bounding what
   * `settleSmootherRetainingTail` keeps. A row with a live smoother
   * never loses its tail here regardless of retention — an active
   * reveal owns its entry (streaming rows are always retained anyway,
   * but the guard makes that structural rather than hoped-for). The
   * pruned row re-renders from its trimmed summary on its next mount.
   * "The re-wrap happens offscreen" leans on the retention window
   * (ROW_UI_RETAIN_NODE_BUFFER + the tail band) comfortably exceeding
   * the virtualizer's render overscan — a retained set that shrank
   * below what is mounted would re-wrap visible rows on the quiet
   * cadence.
   */
  function pruneSettledThinkingTails(retainedItemIds: ReadonlySet<string>): void {
    for (const itemId of itemLiveThinkingTail.keys()) {
      if (retainedItemIds.has(itemId)) continue;
      if (itemSmoothers.has(itemId)) continue;
      itemLiveThinkingTail.delete(itemId);
      settledTailSummaries.delete(itemId);
    }
    assistantReveal.pruneRecords(retainedItemIds);
  }

  function smootherCount(): number {
    return itemSmoothers.size;
  }

  function debugStats(): {
    itemSmoothers: number;
    liveThinkingTails: number;
    liveThinkingTailChars: number;
  } {
    // Chars, not just count: entries now hold full reasoning texts past
    // settle, so the count alone hides the memory that matters.
    let liveThinkingTailChars = 0;
    for (const text of itemLiveThinkingTail.values()) {
      liveThinkingTailChars += text.length;
    }
    return {
      itemSmoothers: itemSmoothers.size,
      liveThinkingTails: itemLiveThinkingTail.size,
      liveThinkingTailChars,
    };
  }

  /**
   * Test-only synchronous flush of every per-item streaming smoother
   * in this pane. Snaps each active smoother so items[].summary
   * reflects the full received text immediately, then settles the
   * entry EXACTLY as production settle does — smoother disposed, tail
   * retained with its settle summary recorded — so tests that flush to
   * a settled state observe the same retention the app ships. Used by
   * tests that assert summary content right after applying deltas
   * without waiting for the smoother's rAF schedule. Not part of the
   * production surface.
   */
  function __flushForTest(): void {
    mutateSmoothersAndRecompute('streaming reveal test flush', () => {
      const errors: unknown[] = [];
      // snap() → onReveal can settle+delete entries (terminal rows), so
      // iterate a snapshot rather than the live map.
      for (const [id, entry] of [...itemSmoothers]) {
        try {
          entry.smoother.snap();
        } catch (error) {
          errors.push(error);
        }
        if (itemSmoothers.has(id)) {
          try {
            settleSmootherRetainingTail(id);
          } catch (error) {
            errors.push(error);
          }
        }
      }
      throwCollectedErrors(errors, 'streaming reveal test flush failed');
    });
  }

  function __smootherCountForTest(): number {
    return smootherCount();
  }

  return {
    get revealBoundary() {
      return revealBoundary;
    },
    appendStreamingDelta,
    applyPatch,
    isSmoothing,
    get assistantRevealRegistrationGeneration() {
      return assistantReveal.registrationGeneration;
    },
    registerAssistantRevealSink,
    assistantParserSource,
    assistantSourceAppend,
    reconcileItemWrite,
    prepareItemReplacements,
    recomputeReveal,
    disposeSmootherFor,
    disposeSmoothersForItems,
    disposeAll,
    snapAllToReceived,
    liveThinkingTailFor,
    pruneSettledThinkingTails,
    smootherCount,
    debugStats,
    __flushForTest,
    __smootherCountForTest,
  };
}
