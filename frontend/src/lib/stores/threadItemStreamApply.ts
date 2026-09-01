import type { Item, Thread } from '../types/models';
import type {
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
} from '../types/events';
import {
  type ApplyItemUpsertsToWindowResult,
  applyItemUpsertsToWindow,
  itemsAreEqual,
} from './threadItems';
import type { ThreadTimelineWindow } from './threadTimelineWindow.svelte';
import type { ThreadSubagentMemory } from './threadSubagentMemory';
import type { ThreadStreamingReveal } from './threadStreamingReveal.svelte';
import { isSmoothLiveContentKind } from './threadPaneShared';

export interface ThreadItemStreamApplyOptions {
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  /**
   * The pane's id→index map for the loaded window. Handed over by
   * reference because `applyItemUpsertsToWindow` takes the map itself;
   * every write to it belongs to the pane's own index maintenance
   * (`commitUpsertResult` and the wholesale chokepoints).
   */
  itemIndexById: ReadonlyMap<string, number>;
  getThread(): Thread | null;
  /**
   * The pane's ONE in-place row write — bumps the row-UI retention and
   * activity-run summary revisions from the comparison it is already
   * holding. Every single-row replacement here goes through it.
   */
  writeItemAt(index: number, next: Item): void;
  /**
   * The pane's upsert-result commit chokepoint: installs
   * `next.items`, maintains the id→index map, and bumps the structural
   * / retention / activity-run revisions the result reports. Lives with
   * the wholesale chokepoints in the pane so no items assignment or
   * revision bump escapes it.
   */
  commitUpsertResult(
    next: ApplyItemUpsertsToWindowResult,
    afterCommit: (committed: ApplyItemUpsertsToWindowResult) => void,
  ): void;
  /** Stamp the pane's non-reactive live-content latch. */
  stampLiveContent(): void;
  /** Wire append to the loaded tail: arm the structural spring AND stamp. */
  armLiveContentAppendSpring(): void;
  /** The pane's optimistic-row ledger — discharged by a wire echo. */
  optimisticItemIds: Set<string>;
  timelineWindow: ThreadTimelineWindow;
  subagentMemory: ThreadSubagentMemory;
  streamingReveal: ThreadStreamingReveal;
}

/** Distinct itemIds a pane will warn about before the ledger resets. */
const MAX_WARNED_MISSING_DELTA_IDS = 256;

export interface ThreadItemStreamApply {
  /**
   * Merge a batch of Items into the loaded window. Returns the applied
   * result, or null when nothing reached the window (empty batch, all
   * rows folded or refused admission, or no row changed).
   */
  upsertItemsBatch(incoming: Item[]): ApplyItemUpsertsToWindowResult | null;
  /** `upsertItemsBatch` plus optimistic-marker discharge and the append spring. */
  applyProviderItemUpserts(
    incoming: Item[],
  ): ApplyItemUpsertsToWindowResult | null;
  /** Append a streaming text delta to a loaded row (smoothed or direct). */
  applyItemDelta(evt: ItemDeltaEvent): void;
  /** Replace a loaded row's re-validated meta blob. */
  applyItemMeta(evt: ItemMetaEvent): void;
  /** Apply a field patch (status / summary / meta / decision) to a loaded row. */
  applyItemPatch(evt: ItemPatchEvent): void;
}

/**
 * Owns a thread pane's streaming item-application machine: the batched
 * upsert path (`applyItemUpsertsToWindow` + the admission and fold
 * swallows, live eviction, window prune, and reveal reconcile that ride it)
 * and the three single-row wire applications
 * — delta, meta and field patch.
 *
 * The pane data layer remains the sole mutator of `items` and of the
 * id→index map: this factory writes rows through
 * `options.writeItemAt()` and window results through
 * `options.commitUpsertResult()`, so every assignment and every
 * revision bump still happens at the pane's own chokepoints. It
 * deliberately does NOT own the window's cursors (threadTimelineWindow),
 * the per-item smoothers and reveal gate (threadStreamingReveal), the
 * subagent fold policy (threadSubagentMemory), or the switch/sync
 * pipeline (threadSwitchLoad) — it drives all four through their handles.
 */
export function createThreadItemStreamApply(
  options: ThreadItemStreamApplyOptions,
): ThreadItemStreamApply {
  const { itemIndexById, subagentMemory, streamingReveal, timelineWindow } =
    options;

  /**
   * Ids already reported by `applyItemDelta`'s missing-row warning. A
   * genuine gap produces a delta storm for one row, and a per-delta warn
   * buries the first (and only interesting) report; the cap keeps the
   * ledger from becoming a leak of its own across a long session —
   * re-warning after a reset is a cheaper failure than growth.
   */
  const warnedMissingDeltaIds = new Set<string>();

  /**
   * Domain work that must observe an installed upsert window and finish before
   * the pane derives its reveal boundary. Each independent leg still runs when
   * another fails, then the pane's commit finalizer reports the aggregate after
   * it has synchronized the gate.
   */
  function finishCommittedUpsert(
    next: ApplyItemUpsertsToWindowResult,
  ): void {
    let errors: unknown[] | null = null;
    if (next.appendedItems.length > 0) {
      try {
        timelineWindow.refreshCursorsAfterTailAppend();
      } catch (error) {
        (errors ??= []).push(error);
      }
    }
    // Live eviction runs before the window-cap check, and the cap itself
    // counts only top-level rows (matching the backend pagers'
    // top-level-only budget): children an open companion or expanded card
    // keeps loaded — which eviction deliberately never folds — must not
    // push the prune into evicting the conversation (incident 2026-08-31).
    try {
      subagentMemory.evictSettledChildren(next.changedItems);
    } catch (error) {
      (errors ??= []).push(error);
    }
    if (next.appendedItems.length > 0 && !timelineWindow.hasMoreNewer) {
      try {
        timelineWindow.pruneToRecentWindowIfNeeded();
      } catch (error) {
        (errors ??= []).push(error);
      }
    }
    if (errors) {
      throw new AggregateError(errors, 'timeline item upsert post-commit work failed');
    }
  }

  function upsertItemsBatch(
    incoming: Item[],
  ): ApplyItemUpsertsToWindowResult | null {
    if (incoming.length === 0) return null;

    // Re-delivered upserts for folded children (transport replay after a
    // reconnect) must not re-insert rows the fold already counted. The
    // canonical row lives in SQLite — persisted before the event was
    // emitted — so the count survives the swallow; an enriched echo's
    // new content (e.g. a completion re-persisted with an inline diff
    // upgrade) surfaces when expansion rehydrates the transcript.
    if (incoming.some((it) => subagentMemory.isEvicted(it.id))) {
      incoming = incoming.filter((it) => !subagentMemory.isEvicted(it.id));
      if (incoming.length === 0) return null;
    }

    const thread = options.getThread();
    incoming = streamingReveal.prepareItemReplacements(incoming);
    const next = applyItemUpsertsToWindow({
      current: options.getItems(),
      incoming,
      itemIndexById,
      currentThreadId: thread?.id ?? null,
      oldestLoadedCursor: timelineWindow.oldestLoadedCursor,
      newestLoadedCursor: timelineWindow.newestLoadedCursor,
      oldestLoadedTurnIndex: timelineWindow.oldestLoadedTurnIndex,
      newestLoadedTurnIndex: timelineWindow.newestLoadedTurnIndex,
      hasMoreHistory: timelineWindow.hasMoreHistory,
      hasMoreNewer: timelineWindow.hasMoreNewer,
    });
    if (!next) return null;
    // Admission is decided inside the merge itself (see
    // `rejectedParentedItems`): a new child lands only when its anchor
    // is loaded or landed earlier in the same batch, so the
    // floor/ceiling filters can never strip an anchor out from under a
    // child that was vouched for separately. This is the LIVE-STREAM
    // boundary only — cache restore and replica paint install rows
    // wholesale through `replaceTimelineItems` (server windows are
    // top-level-only; snapshots hold rows this pane already admitted),
    // and the window-sync page install enforces the same contract via
    // `reconcileSnapshotPage.orphanedLiveChildren`.
    subagentMemory.recordAdmission(
      next.appendedItems,
      next.rejectedParentedItems,
    );
    if (next.droppedNewerItems) {
      timelineWindow.noteDroppedNewerItems();
    }
    if (!next.structureChanged && next.changedItems.length === 0) {
      // A merge that produced only admission rejections reads as
      // "nothing reached the window" to callers — the rejections were
      // recorded above and are not theirs to see.
      return next.droppedNewerItems ? next : null;
    }
    options.commitUpsertResult(next, finishCommittedUpsert);
    return next;
  }

  function applyProviderItemUpserts(
    incoming: Item[],
  ): ApplyItemUpsertsToWindowResult | null {
    const applied = upsertItemsBatch(incoming);
    // Discharging an optimistic marker belongs HERE, not in
    // `upsertItemsBatch`: the marker means "this row exists only in
    // this pane's hope", and only the wire can disprove that. Doing it
    // in the shared batch untracked the composer's own optimistic
    // insert on the very call that mounted it (an append lands in
    // `changedItems` too), which left `isOptimisticItem` permanently
    // false — the failed-send rollback never fired, and the
    // cache/replica filters that exist to keep phantoms out of the
    // durable tiers had nothing to filter.
    if (applied && options.optimisticItemIds.size > 0) {
      for (const changed of applied.changedItems) {
        options.optimisticItemIds.delete(changed.id);
      }
    }
    // A wire append to the loaded tail arms the structural-append
    // spring, stamps the live-content latch, and schedules the
    // follow-up nudge (see `armLiveContentAppendSpring`;
    // `armStructuralSpring` owns the loading/discussion gates).
    // Turn-state-independent, so appends after turn end (interrupt
    // echo, force-closed tool rows, background-task completion
    // siblings) arm too — an effect keyed on the active turn never saw
    // those and they landed as instant whole-viewport teleports
    // (bug-report-20260702T193212Z). Rollback-restore rows route
    // through `upsertItems` above, deliberately outside this arm;
    // the composer's optimistic user-send arms at its own call site
    // (`pane.armStructuralSpring()` before its upsert) without the
    // stamp.
    if (applied && applied.appendedItems.length > 0) {
      options.armLiveContentAppendSpring();
    }
    return applied;
  }

  function applyItemDelta(evt: ItemDeltaEvent): void {
    if (!evt.itemId || !evt.delta) return;
    const thread = options.getThread();
    if (thread && evt.threadId !== thread.id) return;
    const index = itemIndexById.get(evt.itemId);
    if (index === undefined) {
      // Expected miss: the row was refused window admission because its
      // anchor isn't loadable here, so its deltas have nothing to write
      // into. SQLite has the streamed text; hydration renders it if the
      // anchor comes back. Consulted only AFTER the index miss — a
      // loaded row always applies whatever the ledger says, which is
      // what makes a stale swallow entry harmless.
      if (subagentMemory.isSwallowedChild(evt.itemId)) return;
      // The wire contract from triage is: the upsert that creates a
      // streaming row ALWAYS precedes any delta for that row
      // (handleTextDelta in internal/triage/stream_items.go inserts
      // on first delta + emits the upsert before the delta event).
      // Hitting this branch means a transport gap, a replay race, or
      // a missed init left us with a delta whose row doesn't exist
      // yet. Log so the regression isn't silent — under the old
      // parallel-slice architecture this case was masked by
      // `liveDeltaChunks` buffering, which we no longer have.
      if (!warnedMissingDeltaIds.has(evt.itemId)) {
        if (warnedMissingDeltaIds.size >= MAX_WARNED_MISSING_DELTA_IDS) {
          warnedMissingDeltaIds.clear();
        }
        warnedMissingDeltaIds.add(evt.itemId);
        console.warn('[thread] applyItemDelta: no row for itemId', evt.itemId);
      }
      return;
    }
    const current = options.getItems()[index];
    if (current.status !== 'streaming') return;

    // Tool calls, errors, notifications, etc. bypass the smoother —
    // they have their own renderers and don't benefit from
    // word-aligned reveal. Replace the entry rather than mutating in
    // place so the virtualizer's per-row ResizeObserver stays quiet on
    // unchanged rows; the streaming row is genuinely growing, so a
    // fresh reference is the correct signal. Defensive branch: triage
    // emits `action=delta` only for smooth kinds today
    // (stream_items.go / compaction_reasoning.go), so this never runs.
    // If a non-smooth delta producer ever appears, mounted-row growth
    // should stamp the spring latch here for parity with the upsert
    // path (eventsItemStream.ts providerUpsertAdvancesLiveContent).
    if (!isSmoothLiveContentKind(current.kind)) {
      options.writeItemAt(index, {
        ...current,
        summary: current.summary + evt.delta,
        updatedAt: evt.updatedAt,
      });
      return;
    }

    // Smoothable kinds (assistant_text + the reasoning-tail kinds
    // thinking and compaction_reasoning): route the wire delta through
    // the per-item smoother. The smoother's onReveal callback owns all
    // subsequent writes to items[index].summary and to the live payload tail.
    streamingReveal.appendStreamingDelta(
      evt.itemId,
      current.summary,
      evt.delta,
      evt.updatedAt,
    );
  }

  function applyItemMeta(evt: ItemMetaEvent): void {
    // Re-validated meta blob for an in-flight row. Today's only
    // producer is triage's streaming path-link allowlist: each text
    // flush re-runs the validator and pushes the resulting pathRefs
    // JSON so anchors render mid-stream. The producer dedupes
    // identical merges so by the time this fires the meta is
    // genuinely new.
    if (!evt.itemId) return;
    const thread = options.getThread();
    if (thread && evt.threadId !== thread.id) return;
    const index = itemIndexById.get(evt.itemId);
    if (index === undefined) return;
    const current = options.getItems()[index];
    if (current.meta === evt.meta) return;
    // Replace the entry rather than mutating in place: ChatMarkdown's
    // $derived path-link extension keys off `item.meta`, so a fresh
    // reference is the reactive signal that re-runs the extension
    // build. updatedAt is preserved — triage's UpdateItemMeta does
    // not bump updated_at, and we don't want this re-render to look
    // like a content change to the size priors / threadItemCache.
    options.writeItemAt(index, { ...current, meta: evt.meta });
  }

  function applyItemPatch(evt: ItemPatchEvent): void {
    if (!evt.itemId) return;
    const thread = options.getThread();
    if (thread && evt.threadId !== thread.id) return;
    const index = itemIndexById.get(evt.itemId);
    if (index === undefined) {
      // Patch arrived for a row we no longer track (race after
      // removal). Make sure any orphaned smoother is cleaned up.
      streamingReveal.disposeSmootherFor(evt.itemId);
      return;
    }
    const current = options.getItems()[index];
    // Smoother decision tree (snap statuses, extend-vs-overwrite,
    // caught-up terminal dispose, bare-status dispose) plus the
    // UNCONDITIONAL recompute that follows it — see
    // threadStreamingReveal.svelte.ts `applyPatch`. Snap/dispose there
    // may have cleared the frontier (interrupt, error, completion);
    // the recompute drops the gate and reveals any withheld tail rows
    // before the early `itemsAreEqual` return below.
    streamingReveal.applyPatch(evt.itemId, evt.patch);

    // Spread from items[index], NOT the pre-snap `current` capture: a
    // snap above rewrote items[index].summary to the full revealed text
    // via onReveal. Spreading `current` would discard that write, so a
    // terminal patch that OMITS a summary (a kill/error that doesn't
    // re-send text) would silently revert to the partial pre-snap
    // summary and lose the already-streamed tail. With items[index] the
    // snap's full text is the base; a present patch summary still
    // overrides it below.
    const next = { ...options.getItems()[index] };
    if (evt.patch.status !== undefined) next.status = evt.patch.status;
    if (evt.patch.summary !== undefined) {
      // If a smoother is still active for this item AND the patch
      // summary was absorbed as a smoother delta above (extends
      // received), let the smoother own the visible summary write.
      // Otherwise (no smoother, snapped, or overwrite path), apply
      // the patch summary directly. After-snap, items[index].summary
      // already contains the full revealed text; the patch summary
      // then replaces it with the final wire shape (e.g. interrupted
      // prefix).
      const stillSmoothing = streamingReveal.isSmoothing(evt.itemId);
      if (!stillSmoothing) {
        next.summary = evt.patch.summary;
        // Final/overwrite summary written directly (no smoother to own
        // the reveal) — genuine content landing at the bottom. Stamp so
        // a turn that completes mid-stream still spring-lands its tail.
        // Meta-only / status-only patches never reach here (gated on
        // `evt.patch.summary !== undefined` above), so they stay instant.
        options.stampLiveContent();
      }
    }
    if (evt.patch.meta !== undefined) next.meta = evt.patch.meta;
    if (evt.patch.decision !== undefined) next.decision = evt.patch.decision;
    if (evt.patch.updatedAt !== undefined)
      next.updatedAt = evt.patch.updatedAt;
    if (itemsAreEqual(current, next)) return;
    options.writeItemAt(index, next);
    // Streaming children settle through THIS path, not upserts —
    // triage's doSettleStreamingText/Thinking emit field patches.
    // Without this hook, settled text rows under collapsed cards
    // would stay in pane memory for the rest of the turn.
    subagentMemory.evictSettledChildren([next]);
  }

  return {
    upsertItemsBatch,
    applyProviderItemUpserts,
    applyItemDelta,
    applyItemMeta,
    applyItemPatch,
  };
}
