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
// The collaborators it composes, each with its own header:
//   - threadRevealSmoothers.ts — the smoother map, retained reasoning tails,
//     assistant sink registry (the RESOURCES).
//   - threadRevealGate.svelte.ts — `revealBoundary`, the sequencer, and the
//     mutate-then-re-derive transaction (the QUEUE/GATE).
//   - threadRevealRouting.ts — per-delta direct-vs-authoritative routing and
//     the patch decision tree (the ROW WRITES).
//   - threadRowPromotions.svelte.ts — rows held back while the row they
//     replace finishes on screen (the HELD REPLACEMENTS).
//
// Also owns the pane-wide sweeps that cross them: visibility-resume snap,
// thread-switch disposal, and the test flush.

import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import type { LiveRevealStream } from '../utils/payloadExpansion.svelte';
import type { TextWindow } from '../utils/liveText';
import { classifyRevealText, classifyStreamedText } from './threadRevealText';
import type { RevealBoundary } from '../utils/subagentGrouping';
import { isSmoothLiveContentKind, isReasoningTailKind } from './threadPaneShared';
import {
  type StreamingAssistantRenderContext,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';
import type { ProvenAppend } from '../markdown';
import { createRevealGate } from './threadRevealGate.svelte';
import { UNSTAMPED_ITEM_REV } from './threadWindowDigest';
import { createRevealRouting } from './threadRevealRouting';
import {
  createRevealSmootherRegistry,
  isSnapStatus,
  throwCollectedErrors,
} from './threadRevealSmoothers';
import {
  createThreadRowPromotions,
  type PresentingRowPromotion,
  type RowPromotionPatch,
  type RowPromotionPresenter,
} from './threadRowPromotions.svelte';

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
  scopeRootId?: string;
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
    streamEnd: number | undefined,
  ): void;
  /** Stamp the live-content latch (pane's stampLiveContent). */
  stampLiveContent(item: Item): void;
  /** Arm the structural-append spring and stamp the live-content latch
   *  (pane's armLiveContentAppendSpring — pane owns all its gates). */
  armStructuralSpring(): void;
  /**
   * Called at the end of every reveal pass, after the boundary is
   * published — i.e. after every window commit and every smoother
   * mutation. The pane hands its rendered flush rows to the send-queue
   * store from here (threadFlushRowReveal.ts). Read-only with respect to
   * items, smoothers and the gate.
   */
  onRevealSettled?(): void;
  /** rowUiState.appendLivePayloadDeltaForItem — live reasoning-tail payload append. */
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    stream: LiveRevealStream,
    delta: string,
    end: number,
    payloadVersion?: unknown,
  ): void;
  /**
   * A streaming row missed text (`RevealRoutingOptions.onStreamGap`): the
   * owner re-reads its window.
   */
  onStreamGap?(itemId: string): void;
  /**
   * Install a row this module held back, through the pane's upsert path
   * so the window, its indexes and the timeline revision follow the row's
   * new kind. The hold is already released when this runs.
   */
  commitPromotedRow(row: Item): void;
}

export interface ThreadStreamingReveal {
  /** Reveal gate position; null = render everything. */
  readonly revealBoundary: RevealBoundary | null;
  /** applyItemDelta's smooth-kind path: place the delta at `offset` in the
   *  row's stream, get-or-create the smoother seeded with the row, append,
   *  recompute the gate (`threadRevealRouting.ts`). */
  appendStreamingDelta(
    current: Item,
    delta: string,
    offset: number | undefined,
    updatedAt: number,
  ): void;
  /** Reconcile text and commit the row before deriving the gate, including on failure. */
  applyPatch(itemId: string, patch: ItemPatchEvent['patch']): Item | null;
  /** True while a smoother owns the row's displayed text. */
  isSmoothing(itemId: string): boolean;
  /**
   * Rows a patch proved to hold incomplete text. A settled read of one
   * supersedes the pane's row (`snapshotRowSupersedesLive`).
   */
  staleRowIds(): ReadonlySet<string>;
  /** A row holds deltas past its text, or is stale: a re-read is owed. */
  hasStreamGaps(): boolean;
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
   * The required commit callback installs the result; gate synchronization
   * belongs to this operation and runs even if preparation or commit throws.
   * `opensStreams` marks live provider upserts, whose streaming rows gate
   * from this commit (`RevealRouting.openStreams`).
   */
  withReconciledItems<T>(
    incoming: readonly Item[],
    commit: (items: Item[]) => T,
    opensStreams?: boolean,
  ): T;
  disposeSmootherFor(itemId: string): void;
  disposeSmoothersForItems(items: readonly { id: string }[]): void;
  disposeAll(): void;
  /** Resume/reconnect catch-up (pane's snapSmoothersToReceived, incl. its recomputeReveal). */
  snapAllToReceived(): void;
  /**
   * Full revealed text for a reasoning-tail row, or null. Live while the
   * row streams, and RETAINED across a content-consistent settle so the
   * collapsed clamp never re-wraps in front of the reader; dropped on
   * overwrite/removal, by the offscreen prune, and on thread switch. Not
   * retained for a text over the retention budget.
   */
  liveThinkingTailFor(itemId: string): string | null;
  /**
   * The end of that text the collapsed clamp renders: a window while the
   * row streams, and once it settles the whole text, or the last window
   * when the whole text is over the retention budget. Dropped on the same
   * paths as `liveThinkingTailFor`.
   */
  liveThinkingWindowFor(itemId: string): TextWindow | null;
  /** Row-UI prune hook: drop retained settled tails not in the retention set. */
  pruneSettledThinkingTails(retainedItemIds: ReadonlySet<string>): void;
  /**
   * How many rows the reveal queue is still draining: live per-item
   * smoothers, plus held rows whose smoother has settled. The cheap half of
   * `debugStats()`: no walk over the retained thinking tails, so a poll loop (the
   * harness's reveal-drain probe) can ask it every few hundred milliseconds
   * without becoming part of the load it is measuring.
   */
  smootherCount(): number;
  debugStats(): {
    itemSmoothers: number;
    liveThinkingTails: number;
    liveThinkingTailChars: number;
  };
  /**
   * Reactive. Held rows a timeline is presenting
   * (threadRowPromotions.svelte.ts): the pane still holds the row each
   * replaces, and installs the held row when the presenter completes it.
   */
  readonly presentingRowPromotions: readonly PresentingRowPromotion[];
  /** Offer a mounted timeline the presentation of held rows. */
  registerRowPromotionPresenter(presenter: RowPromotionPresenter): () => void;
  /** The presenter finished the hold for `itemId`: install its row. */
  completeRowPromotion(itemId: string): void;
  /**
   * Fold changed fields into the row held for `itemId`. False when none is
   * held, and the caller writes the pane's row.
   */
  patchHeldRow(itemId: string, fields: RowPromotionPatch): boolean;
  /**
   * Test-only: snap + dispose every smoother (body of pane's
   * __flushItemSmoothersForTest) and install every held row.
   */
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
  const promotions = createThreadRowPromotions({ commit: installHeldRow });
  const gate = createRevealGate({
    get scopeRootId() { return options.scopeRootId; },
    registry,
    getItemById: options.getItemById,
    getItems: options.getItems,
    armStructuralSpring: options.armStructuralSpring,
    heldItemIds: () => promotions.ids(),
    onRevealSettled: () => {
      queuePromotionSettle();
      options.onRevealSettled?.();
    },
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
    onStreamGap: (itemId) => options.onStreamGap?.(itemId),
  });

  function disposeAll(): void {
    promotions.clear();
    // Boundary last and unconditionally: a smoother that failed to dispose
    // must not leave a frontier naming it behind, and the registry's own
    // AggregateError is still what reaches the caller.
    try {
      registry.disposeEverything();
    } finally {
      gate.clearBoundary();
    }
  }

  /**
   * A reasoning row whose settled read is prose under the same id: the
   * provider reclassified the block when it closed (a Claude narration
   * block, docs/references/claude-wire.md). Taking that read as it arrives
   * would cut the reveal at its cursor and move the row out of its activity
   * run in the same frame.
   */
  function isPromotion(current: Item, incoming: Item): boolean {
    return current.kind === 'thinking' && incoming.kind === 'assistant_text'
      && incoming.status !== 'streaming' && !isSnapStatus(incoming.status);
  }

  /**
   * Hold `incoming` back when it promotes the pane's row, or when a hold
   * already keeps a row for the id. Returns the row to publish in its
   * place (the pane's own), or null when `incoming` is not held.
   */
  function holdReplacement(incoming: Item, current: Item | undefined): Item | null {
    if (current === undefined || incoming === current) return null;
    if (promotions.has(incoming.id)) {
      // A read of the shape the pane still shows reconciles as usual.
      if (incoming.kind === current.kind) return null;
      promotions.hold(incoming);
      return current;
    }
    // With no smoother there is no reveal to finish: the read installs.
    if (!itemSmoothers.has(incoming.id) || !isPromotion(current, incoming)) return null;
    // The held row carries the whole text, so nothing is owed a re-read.
    registry.clearStale(incoming.id);
    registry.dropHeldDeltas(incoming.id);
    promotions.hold(incoming);
    return current;
  }

  function holdNeedsSettle(itemId: string): boolean {
    if (promotions.phase(itemId) !== 'draining') return false;
    if (options.getItemById(itemId) === undefined) return true;
    const entry = itemSmoothers.get(itemId);
    return entry === undefined || entry.smoother.isCaughtUp();
  }

  let promotionSettleQueued = false;

  /**
   * Runs at the end of every gate pass. A drained hold settles in a
   * microtask, not in the pass: one event flush applies its upserts before
   * its deltas, so the upsert that starts a hold can be followed in the
   * same task by the last of the row's text, and installing a row from
   * inside a pass would nest one window commit in another.
   */
  function queuePromotionSettle(): void {
    if (promotionSettleQueued || promotions.size === 0) return;
    let needed = false;
    for (const itemId of promotions.ids()) {
      if (!holdNeedsSettle(itemId)) continue;
      needed = true;
      break;
    }
    if (!needed) return;
    promotionSettleQueued = true;
    queueMicrotask(() => {
      promotionSettleQueued = false;
      settleDrainedPromotions();
    });
  }

  /**
   * Move every hold whose reveal has shown its last word to a presenter,
   * or install it when none takes it. A hold whose row left the window
   * ends the same way, and its install replaces nothing.
   */
  function settleDrainedPromotions(): void {
    const drained: string[] = [];
    for (const itemId of promotions.ids()) {
      if (holdNeedsSettle(itemId)) drained.push(itemId);
    }
    if (drained.length === 0) return;
    try {
      gate.mutateSmoothersAndRecompute('row promotion drain', () => {
        const errors: unknown[] = [];
        for (const itemId of drained) {
          promotions.markDrained(itemId);
          try {
            // The row keeps rendering the tail it revealed while a
            // presenter runs; the smoother has no text left to give.
            registry.settleSmootherRetainingTail(itemId);
          } catch (error) {
            errors.push(error);
          }
        }
        throwCollectedErrors(errors, 'row promotion drain failed');
      });
    } finally {
      // Unconditional: a drained hold nobody presents would gate forever.
      promotions.presentDrained();
    }
  }

  /** The commit of a hold: the pane's row gives way to the held one. */
  function installHeldRow(row: Item): void {
    gate.mutateSmoothersAndRecompute(`row promotion commit for ${row.id}`, () => {
      // The row left the window while it was held: there is nothing to
      // replace, and an upsert would put a row back that the window dropped.
      const replaces = options.getItemById(row.id) !== undefined;
      const errors: unknown[] = [];
      try {
        // Nothing of the replaced row's reveal carries over to the new kind.
        registry.disposeRemovedItem(row.id);
      } catch (error) {
        errors.push(error);
      }
      try {
        if (replaces) options.commitPromotedRow(row);
      } catch (error) {
        errors.push(error);
      }
      throwCollectedErrors(errors, `row promotion commit failed for ${row.id}`);
    });
  }

  /** Rows the reveal queue still owes the reader: smoothers plus bare holds. */
  function revealingRowCount(): number {
    let count = registry.smootherCount();
    for (const itemId of promotions.ids()) {
      if (!itemSmoothers.has(itemId)) count += 1;
    }
    return count;
  }

  function applyPatch(itemId: string, patch: ItemPatchEvent['patch']): Item | null {
    // The pane's row is on its way out; the patch describes the held one.
    if (promotions.patch(itemId, patch)) return null;
    return routing.applyPatch(itemId, patch);
  }

  function disposeSmootherFor(itemId: string): void {
    promotions.drop(itemId);
    gate.disposeSmootherFor(itemId);
  }

  function disposeSmoothersForItems(items: readonly { id: string }[]): void {
    if (promotions.size > 0) {
      for (const item of items) promotions.drop(item.id);
    }
    gate.disposeSmoothersForItems(items);
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
    // A held promotion publishes the pane's reasoning row in place of the
    // prose, and that row's summary is a trimmed view like any other.
    if (isReasoningTailKind(prepared.kind)) return prepared;
    assertRevealCursorNotRewound(incoming.id, cursor, prepared.summary);
    return prepared;
  }

  function prepareItemReplacementRaw(incoming: Item): Item {
    const current = options.getItemById(incoming.id);
    const held = holdReplacement(incoming, current);
    if (held !== null) return held;
    const entry = itemSmoothers.get(incoming.id);
    if (registry.staleIds.has(incoming.id) && incoming.status !== 'streaming' && incoming !== current) {
      // A settled read of a row whose text this pane knows is incomplete
      // wins the row outright, over the smoother's received text too.
      registry.clearStale(incoming.id);
      if (entry) registry.disposeSmootherState(incoming.id);
      return incoming;
    }
    if (!entry) return keepFurtherStream(incoming, current);
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
    // Two streaming reads with positions compare by position; any other
    // pair by text.
    const placed = incoming.status === 'streaming' && incoming.streamEnd !== undefined
      && entry.receivedEnd !== undefined
      ? classifyStreamedText(incoming.summary, incoming.streamEnd, entry.receivedEnd)
      : undefined;
    const relation = placed?.relation ?? classifyRevealText(incoming.kind, incoming.summary, received);
    if (relation === 'replacement') {
      registry.disposeSmootherState(incoming.id);
      return incoming;
    }
    if (relation === 'extension') {
      entry.setLatestUpdatedAt(incoming.updatedAt);
      entry.append(placed ? placed.suffix : incoming.summary.slice(received.length));
    }
    // A snapshot may be a prefix, or a reasoning preview from inside received
    // text. Preserve the displayed cursor even if that snapshot is terminal.
    const trailsTheCursor = relation === 'trailing';

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
    // The published text is the cursor's, so is its position.
    const streamEnd = incoming.status === 'streaming' ? current.streamEnd : undefined;
    // A reveal still behind publishes less than the text `incoming.rev`
    // names, so the row is unstamped until the settling reveal adopts it.
    const revealing = itemSmoothers.has(incoming.id) && !entry.smoother.isCaughtUp();
    if (revealing && !trailsTheCursor) entry.setSettledRev(incoming.rev);
    if (
      incoming.summary === current.summary &&
      incoming.streamEnd === streamEnd &&
      incoming.updatedAt >= current.updatedAt
    ) return incoming;
    return {
      ...incoming,
      summary: current.summary,
      streamEnd,
      updatedAt: Math.max(incoming.updatedAt, current.updatedAt),
      rev: revealing ? UNSTAMPED_ITEM_REV : trailsTheCursor ? current.rev : incoming.rev,
    };
  }

  /**
   * A row with no smoother never goes back to less of its stream: a read of
   * a streaming row that ends before the text the pane holds keeps the
   * pane's text, position and revision.
   */
  function keepFurtherStream(incoming: Item, current: Item | undefined): Item {
    if (
      current === undefined || incoming === current ||
      incoming.status !== 'streaming' || current.status !== 'streaming' ||
      incoming.streamEnd === undefined || current.streamEnd === undefined ||
      incoming.streamEnd >= current.streamEnd
    ) return incoming;
    return {
      ...incoming,
      summary: current.summary,
      streamEnd: current.streamEnd,
      rev: current.rev,
      updatedAt: Math.max(incoming.updatedAt, current.updatedAt),
    };
  }

  function prepareItemReplacements(incoming: readonly Item[]): Item[] {
    if (incoming.length === 0) return incoming as Item[];
    const staleIds = registry.staleIds;
    let prepared: Item[] | null = null;
    const errors: unknown[] = [];
    for (let index = 0; index < incoming.length; index++) {
      const item = incoming[index];
      if (
        item.streamEnd === undefined && !itemSmoothers.has(item.id) &&
        !staleIds.has(item.id) && !promotions.has(item.id)
      ) continue;
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
    throwCollectedErrors(errors, 'streaming reveal item replacement preparation failed');
    return prepared ?? incoming as Item[];
  }

  function withReconciledItems<T>(
    incoming: readonly Item[],
    commit: (items: Item[]) => T,
    opensStreams = false,
  ): T {
    let result!: T;
    gate.mutateSmoothersAndRecompute('timeline item reconciliation', () => {
      const prepared = prepareItemReplacements(incoming);
      result = commit(prepared);
      routing.afterRowsCommitted(prepared);
      if (opensStreams) routing.openStreams(prepared);
    });
    return result;
  }

  /**
   * Snap every behind smoother straight to its full received text, and
   * install every held row without presenting it: after this the pane
   * shows everything it received.
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
   * Completed reconnect recovery uses this same drain after replay and gap
   * snapshots land, so scroll measurement includes the full recovered text.
   * Snapping catches the visible text up to the wire in one frame. Still-
   * streaming rows resume live animation from there (snap leaves the smoother
   * usable); terminal rows dispose through the same onReveal cleanup any
   * caught-up smoother uses. `snap()` no-ops on a caught-up smoother, so this
   * is safe to call unconditionally and costs nothing when nothing is behind.
   */
  function snapAllToReceived(): void {
    if (itemSmoothers.size === 0 && promotions.size === 0) return;
    try {
      snapSmoothersToReceived();
    } finally {
      promotions.completeAll();
    }
  }

  function snapSmoothersToReceived(): void {
    if (itemSmoothers.size === 0) return;
    gate.mutateSmoothersAndRecompute('streaming reveal visibility snap', () => {
      const errors: unknown[] = [];
      // snap() → onReveal can dispose+delete entries (terminal rows), so
      // iterate a snapshot rather than the live map. One broken mounted
      // representation must not prevent the other panes' rows from catching
      // up after a hidden interval.
      for (const [id, entry] of Array.from(itemSmoothers)) {
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
   * without waiting for the smoother's rAF schedule. Held rows install
   * with it, unpresented. Not part of the production surface.
   */
  function __flushForTest(): void {
    try {
      flushSmoothersForTest();
    } finally {
      promotions.completeAll();
    }
  }

  function flushSmoothersForTest(): void {
    gate.mutateSmoothersAndRecompute('streaming reveal test flush', () => {
      const errors: unknown[] = [];
      // snap() → onReveal can settle+delete entries (terminal rows), so
      // iterate a snapshot rather than the live map.
      for (const [id, entry] of Array.from(itemSmoothers)) {
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
    applyPatch,
    isSmoothing,
    staleRowIds: () => registry.staleIds,
    hasStreamGaps: () => registry.hasHeldDeltas() || registry.staleIds.size > 0,
    get assistantRevealRegistrationGeneration() {
      return assistantReveal.registrationGeneration;
    },
    registerAssistantRevealSink,
    assistantParserSource,
    assistantSourceAppend,
    reconcileItemWrite,
    withReconciledItems,
    disposeSmootherFor,
    disposeSmoothersForItems,
    disposeAll,
    snapAllToReceived,
    liveThinkingTailFor: registry.liveThinkingTailFor,
    liveThinkingWindowFor: registry.liveThinkingWindowFor,
    pruneSettledThinkingTails: registry.pruneSettledThinkingTails,
    smootherCount: revealingRowCount,
    debugStats: registry.debugStats,
    get presentingRowPromotions() {
      return promotions.presenting;
    },
    registerRowPromotionPresenter: promotions.registerPresenter,
    completeRowPromotion: promotions.complete,
    patchHeldRow: promotions.patch,
    __flushForTest,
    __smootherCountForTest: registry.smootherCount,
  };
}
