// stores/threadStreamingReveal.svelte.ts
//
// The pane's streaming-reveal composition root, and — the reason it is a
// root and not a barrel — the SINGLE ROW-TEXT CHOKEPOINT:
// `prepareItemReplacement` decides the text of every row a wholesale commit
// publishes, and `assertRevealCursorNotRewound` guards it in place. The
// chokepoint and its guard are never separated; splitting them would leave
// each wholesale-commit path free to rediscover the rewind bugs of 2026-08
// one at a time.
//
// The three collaborators it composes, each with its own header:
//   - threadRevealSmoothers.ts — the smoother map, retained reasoning tails,
//     assistant sink registry (the RESOURCES).
//   - threadRevealGate.svelte.ts — `revealBoundary`, the sequencer, and the
//     mutate-then-re-derive transaction (the QUEUE/GATE).
//   - threadRevealRouting.ts — per-delta direct-vs-authoritative routing and
//     the patch decision tree (the ROW WRITES).
//
// Also owns the pane-wide sweeps that cross all three: visibility-resume
// snap, thread-switch disposal, and the test flush.

import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import type { RevealBoundary } from '../utils/subagentGrouping';
import { isSmoothLiveContentKind, isReasoningTailKind } from './threadPaneShared';
import {
  type StreamingAssistantRenderContext,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';
import type { ProvenAppend } from '../markdown';
import { createRevealGate } from './threadRevealGate.svelte';
import { createRevealRouting } from './threadRevealRouting';
import {
  absorbReceivedSuffix,
  createRevealSmootherRegistry,
  isSnapStatus,
  summaryRepresentsReceived,
  throwCollectedErrors,
} from './threadRevealSmoothers';

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
  const registry = createRevealSmootherRegistry({
    getItemById: options.getItemById,
    getItemIndex: options.getItemIndex,
    getItems: options.getItems,
    setItemAt: options.setItemAt,
  });
  const itemSmoothers = registry.smoothers;
  const assistantReveal = registry.assistantReveal;
  const gate = createRevealGate({
    registry,
    getItemById: options.getItemById,
    getItems: options.getItems,
    armStructuralSpring: options.armStructuralSpring,
  });
  const routing = createRevealRouting({
    registry,
    gate,
    getItemById: options.getItemById,
    getItemIndex: options.getItemIndex,
    getItems: options.getItems,
    setItemAt: options.setItemAt,
    appendDirectAssistantLiteral: options.appendDirectAssistantLiteral,
    stampLiveContent: options.stampLiveContent,
    appendLivePayloadDeltaForItem: options.appendLivePayloadDeltaForItem,
  });

  function disposeAll(): void {
    // Boundary last and unconditionally: a smoother that failed to dispose
    // must not leave a frontier naming it behind, and the registry's own
    // AggregateError is still what reaches the caller.
    try {
      registry.disposeEverything();
    } finally {
      gate.clearBoundary();
    }
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
      registry.disposeSmootherState(incoming.id);
      return incoming;
    }
    if (isSnapStatus(incoming.status)) {
      registry.snapAndDisposeSmoother(incoming.id, entry);
      return incoming;
    }

    if (
      !isSmoothLiveContentKind(incoming.kind) ||
      incoming.kind !== current.kind
    ) {
      registry.disposeSmootherState(incoming.id);
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
      (received.startsWith(incomingSummary) ||
        (isReasoningTailKind(incoming.kind) && received.includes(incomingSummary)))
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
      //
      // Reasoning-tail rows publish the last THINKING_TAIL_RUNES of the
      // cursor, so past that length the same two producers hand back a
      // summary that is an INTERIOR slice of `received`, never a prefix.
      // Read as divergent, it disposed the smoother mid-drain and dropped
      // the unrevealed backlog: the next wire delta re-seeded from the
      // trimmed summary and the live tail carried a permanent hole until
      // a reload (2026-09-01: "Now I'm working" + "ining the user's.").
      // Containment is exact here for the same reason textOverlap.ts
      // relies on it — a false match needs the reasoning to repeat a
      // 400-rune passage verbatim.
      belongsToCurrentStream = true;
      trailsTheCursor = true;
    }

    if (!belongsToCurrentStream) {
      registry.disposeSmootherState(incoming.id);
      return incoming;
    }

    entry.setLatestUpdatedAt(Math.max(incoming.updatedAt, current.updatedAt));
    // A terminal echo is authoritative about lifecycle, but not a reason to
    // bypass the readable cursor when its summary is the same append-only
    // stream. Keep the terminal status while the smoother drains the unseen
    // suffix. Mismatches and snap statuses took their authoritative paths
    // above.
    if (incoming.status !== 'streaming' && entry.smoother.isCaughtUp()) {
      registry.settleSmootherRetainingTail(incoming.id);
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
        gate.recomputeReveal();
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
    gate.mutateSmoothersAndRecompute('streaming reveal visibility snap', () => {
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
            registry.disposeSmootherState(id);
          } catch (error) {
            errors.push(error);
          }
        } else if (
          itemSmoothers.has(id) &&
          current?.status !== 'streaming' &&
          entry.smoother.isCaughtUp()
        ) {
          try {
            registry.settleSmootherRetainingTail(id);
          } catch (error) {
            errors.push(error);
          }
        }
      }
      throwCollectedErrors(errors, 'streaming reveal visibility snap failed');
    });
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
    gate.mutateSmoothersAndRecompute('streaming reveal test flush', () => {
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
            registry.settleSmootherRetainingTail(id);
          } catch (error) {
            errors.push(error);
          }
        }
      }
      throwCollectedErrors(errors, 'streaming reveal test flush failed');
    });
  }

  return {
    get revealBoundary() {
      return gate.revealBoundary;
    },
    appendStreamingDelta: routing.appendStreamingDelta,
    applyPatch: routing.applyPatch,
    isSmoothing,
    get assistantRevealRegistrationGeneration() {
      return assistantReveal.registrationGeneration;
    },
    registerAssistantRevealSink,
    assistantParserSource,
    assistantSourceAppend,
    reconcileItemWrite,
    prepareItemReplacements,
    recomputeReveal: gate.recomputeReveal,
    disposeSmootherFor: gate.disposeSmootherFor,
    disposeSmoothersForItems: gate.disposeSmoothersForItems,
    disposeAll,
    snapAllToReceived,
    liveThinkingTailFor: registry.liveThinkingTailFor,
    pruneSettledThinkingTails: registry.pruneSettledThinkingTails,
    smootherCount: registry.smootherCount,
    debugStats: registry.debugStats,
    __flushForTest,
    __smootherCountForTest: registry.smootherCount,
  };
}
