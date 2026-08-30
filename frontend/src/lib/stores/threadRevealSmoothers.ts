// stores/threadRevealSmoothers.ts
//
// OWNS the per-item reveal RESOURCES of one thread pane: the smoother map,
// the retained reasoning-tail texts and the summaries they settled with, and
// the assistant-reveal sink registry they publish through. Every create,
// settle and dispose of those resources happens here, so the lifetime rules
// (a tail outlives its smoother across a content-consistent settle; a
// removal drops both; a store-side char budget bounds an unmounted pane)
// live in one file.
//
// MUST NOT touch the reveal GATE. This module never reads or writes
// `revealBoundary` and never calls `recomputeReveal` — a smoother mutation
// and its gate derivation are one transaction, and the transaction belongs
// to `threadRevealGate.svelte.ts`, which wraps every call in here.
// It also never routes a delta: `threadRevealRouting.ts` owns the
// direct-vs-authoritative decision and calls back in for the resources.

import type { Item, ItemKind } from '../types/models';
import { SvelteMap } from 'svelte/reactivity';
import type { PerItemSmoother } from '../markdown/smoothing/PerItemSmoother';
import {
  THINKING_TAIL_RUNES,
  isReasoningTailKind,
  trimToTailRunes,
} from './threadPaneShared';
import {
  createThreadAssistantReveal,
  type ThreadAssistantReveal,
} from './threadAssistantReveal.svelte';

/**
 * Per-item smoothing handle stored in the `itemSmoothers` map. Holds the
 * PerItemSmoother plus a closure setter that lets `appendStreamingDelta`
 * push the latest wire `updatedAt` into the smoother's reveal callback
 * without re-creating the closure.
 */
export interface ItemSmoothing {
  smoother: PerItemSmoother;
  setLatestUpdatedAt(at: number): void;
}

/** Statuses whose patch is the documented authoritative-summary handover. */
export function isSnapStatus(status: Item['status'] | undefined): boolean {
  return status === 'errored' || status === 'killed' || status === 'declined';
}

/**
 * Whether `summary` is the row's published view of `received`. Reasoning-tail
 * rows publish a tail-TRIMMED view, so equality alone would read a settle
 * re-assert as an overwrite and dump the unrevealed backlog wholesale.
 */
export function summaryRepresentsReceived(
  kind: ItemKind | string | undefined,
  summary: string,
  received: string,
): boolean {
  return summary === received ||
    (kind !== undefined &&
      isReasoningTailKind(kind) &&
      summary === trimToTailRunes(received, THINKING_TAIL_RUNES));
}

/**
 * Push the part of `summary` past `received` into the smoother as a delta,
 * so an extending authoritative summary finishes the reveal naturally
 * instead of snapping. Returns false when `summary` does not extend
 * `received` — the caller then takes its own authoritative path.
 */
export function absorbReceivedSuffix(
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

/** Preserve a lone original error for callers that match its diagnostic. */
export function throwCollectedErrors(
  errors: readonly unknown[],
  context: string,
): void {
  if (errors.length === 1) throw errors[0];
  if (errors.length > 1) throw new AggregateError(errors, context);
}

export interface RevealSmootherRegistryOptions {
  /** Current item for an id, or undefined when not loaded. */
  getItemById(itemId: string): Item | undefined;
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The current item window, sorted by (turnIndex, itemIndex). */
  getItems(): Item[];
  /** Reactive write-through of one row (pane does `items[index] = item`). */
  setItemAt(index: number, item: Item): void;
}

export interface RevealSmootherRegistry {
  /**
   * The live smoother map. Exposed directly rather than behind get/set
   * wrappers because the gate iterates it on every recompute and the
   * routing path reads it per delta; a wrapper would be one more call on
   * both hot paths and buy nothing the ownership comment above does not.
   */
  readonly smoothers: SvelteMap<string, ItemSmoothing>;
  /** The per-item mounted-sink registry these smoothers publish through. */
  readonly assistantReveal: ThreadAssistantReveal;
  /** Dispose a smoother and DROP the row's live tail (removal / overwrite). */
  disposeSmootherState(itemId: string): void;
  /** Dispose at settle, RETAINING the tail and recording its settle summary. */
  settleSmootherRetainingTail(itemId: string): void;
  /** Reveal everything the smoother holds, then drop it and its tail. */
  snapAndDisposeSmoother(itemId: string, entry: ItemSmoothing): void;
  /** Thread switch / pane clear: every smoother, tail and sink at once. */
  disposeEverything(): void;
  /**
   * Seed text for a smoother being (re-)created for `itemId`. A retained
   * settled tail is the full text the row is still rendering, so a smoother
   * re-created after that settle must resume from it rather than from the
   * tail-trimmed summary — but only while the tail still describes the row.
   */
  seedFromRetainedTail(itemId: string, initialReceived: string): string;
  /** Record the full revealed text of a live reasoning-tail row. */
  recordLiveTail(itemId: string, revealed: string): void;
  /** Full revealed text for a reasoning-tail row, or null. */
  liveThinkingTailFor(itemId: string): string | null;
  /** Row-UI prune hook: drop retained settled tails outside the retention set. */
  pruneSettledThinkingTails(retainedItemIds: ReadonlySet<string>): void;
  smootherCount(): number;
  debugStats(): {
    itemSmoothers: number;
    liveThinkingTails: number;
    liveThinkingTailChars: number;
  };
}

export function createRevealSmootherRegistry(
  options: RevealSmootherRegistryOptions,
): RevealSmootherRegistry {
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
  // (disposeSmootherState), the offscreen row-UI prune reclaims settled
  // entries once the row leaves retention (pruneSettledThinkingTails),
  // a store-side char budget bounds panes whose timeline is unmounted
  // (evictSettledTailsOverBudget — the prune is a MessageTimeline quiet
  // pass and never runs there, e.g. while Settings replaces the pane
  // strip), and disposeEverything clears everything on thread switch.
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

  function disposeEverything(): void {
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
    if (errors.length > 0) {
      throw new AggregateError(errors, 'streaming reveal disposal failed');
    }
  }

  function seedFromRetainedTail(itemId: string, initialReceived: string): string {
    // A retained settled tail is the full text the row is still
    // rendering; a smoother re-created after that settle (a replay
    // upsert flipping the row back to streaming, then a delta) must
    // seed from it — seeding from the tail-trimmed summary would shrink
    // the rendered string and re-wrap the clamp. Only when consistent:
    // the summary recorded at settle must still be the current summary,
    // else the row was overwritten since and the stale tail is dropped
    // here rather than left to shadow the resumed reveal for a frame.
    const retainedTail = itemLiveThinkingTail.get(itemId);
    if (retainedTail === undefined) return initialReceived;
    if (settledTailSummaries.get(itemId) === initialReceived) return retainedTail;
    itemLiveThinkingTail.delete(itemId);
    settledTailSummaries.delete(itemId);
    return initialReceived;
  }

  function recordLiveTail(itemId: string, revealed: string): void {
    itemLiveThinkingTail.set(itemId, revealed);
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

  return {
    smoothers: itemSmoothers,
    assistantReveal,
    disposeSmootherState,
    settleSmootherRetainingTail,
    snapAndDisposeSmoother,
    disposeEverything,
    seedFromRetainedTail,
    recordLiveTail,
    liveThinkingTailFor,
    pruneSettledThinkingTails,
    smootherCount,
    debugStats,
  };
}
