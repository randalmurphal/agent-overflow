import { isItemStatusRegression } from './threadItems';
import type { Item, Thread } from '../types/models';
import type {
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
} from '../types/events';
import { type ApplyItemUpsertsToWindowResult, applyItemUpsertsToWindow } from './threadItemUpserts';
import type { ThreadTimelineWindow } from './threadTimelineWindow.svelte';
import type { ThreadSubagentMemory } from './threadSubagentMemory';
import type { ThreadStreamingReveal } from './threadStreamingReveal.svelte';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import { isSmoothLiveContentKind } from './threadPaneShared';

export interface ThreadItemStreamApplyOptions {
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  getItemById(id: string): Item | undefined;
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
  /** Wire append to the loaded tail: arm the structural spring AND stamp. */
  armLiveContentAppendSpring(): void;
  /** The pane's optimistic-row ledger — discharged by a wire echo. */
  optimisticItemIds: Set<string>;
  timelineWindow: ThreadTimelineWindow;
  subagentMemory?: ThreadSubagentMemory;
  scopeRootId?: string;
  streamingReveal: ThreadStreamingReveal;
  activityRuns: ThreadActivityRuns;
}

/** Distinct itemIds a pane will warn about before the ledger resets. */
const MAX_WARNED_MISSING_DELTA_IDS = 256;

export interface ThreadItemStreamApply {
  /**
   * Merge a batch of Items into the loaded window. Returns the applied
   * result, or null when nothing reached the window (empty batch, only
   * subagent children, rows refused admission, or no row changed).
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
 * upsert path (child routing, `applyItemUpsertsToWindow`, and the window
 * prune and reveal reconcile that ride it) and the three single-row wire
 * applications: delta, meta and field patch.
 *
 * The pane data layer remains the sole mutator of `items` and of the
 * id→index map: this factory writes rows through
 * `options.writeItemAt()` and window results through
 * `options.commitUpsertResult()`, so every assignment and every
 * revision bump still happens at the pane's own chokepoints. It
 * deliberately does NOT own the window's cursors (threadTimelineWindow),
 * the per-item smoothers and reveal gate (threadStreamingReveal), the
 * subagent live aggregates (threadSubagentMemory), or the switch/sync
 * pipeline (threadSwitchLoad) — it drives all four through their handles.
 */
export function createThreadItemStreamApply(
  options: ThreadItemStreamApplyOptions,
): ThreadItemStreamApply {
  const { activityRuns, itemIndexById, subagentMemory, streamingReveal, timelineWindow } =
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
    previousItems: readonly Item[],
  ): void {
    let errors: unknown[] | null = null;
    if (next.structureChanged) {
      try {
        timelineWindow.refreshCursorsAfterUpserts(next.changedItems, next.appendedItems.length > 0, previousItems);
      } catch (error) {
        (errors ??= []).push(error);
      }
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

  /**
   * Route a batch at the admission chokepoint. Rows of this surface's
   * scope merge into the window; every other row is a subagent child,
   * which the main pane records against its launch anchor's aggregate
   * (`threadSubagentMemory.admitChildren`) and a scoped surface ignores.
   * Children are admitted after the window commit so an anchor landing in
   * the same batch resolves them.
   */
  function upsertItemsBatch(
    incoming: Item[],
    optimisticItemIds?: ReadonlySet<string>,
  ): ApplyItemUpsertsToWindowResult | null {
    if (incoming.length === 0) return null;
    const scope = options.scopeRootId ?? '';
    let rows = incoming;
    let children: Item[] | null = null;
    for (let index = 0; index < incoming.length; index += 1) {
      if ((incoming[index].parentId ?? '') === scope) continue;
      rows = incoming.slice(0, index);
      children = [incoming[index]];
      for (let rest = index + 1; rest < incoming.length; rest += 1) {
        const item = incoming[rest];
        if ((item.parentId ?? '') === scope) rows.push(item);
        else children.push(item);
      }
      break;
    }
    const applied = rows.length > 0 ? upsertWindowRows(rows, optimisticItemIds) : null;
    if (children) subagentMemory?.admitChildren(children);
    return applied;
  }

  function upsertWindowRows(
    incoming: Item[],
    optimisticItemIds?: ReadonlySet<string>,
  ): ApplyItemUpsertsToWindowResult | null {
    const thread = options.getThread();
    return streamingReveal.withReconciledItems(incoming, (incoming) => {
      const previousItems = options.getItems();
      const next = applyItemUpsertsToWindow({
        current: previousItems,
        incoming,
        itemIndexById,
        optimisticItemIds,
        currentThreadId: thread?.id ?? null,
        scopeRootId: options.scopeRootId,
        oldestLoadedCursor: timelineWindow.oldestLoadedCursor,
        newestLoadedCursor: timelineWindow.newestLoadedCursor,
        oldestLoadedTurnIndex: timelineWindow.oldestLoadedTurnIndex,
        newestLoadedTurnIndex: timelineWindow.newestLoadedTurnIndex,
        hasMoreHistory: timelineWindow.hasMoreHistory,
        hasMoreNewer: timelineWindow.hasMoreNewer,
        runCoveringUnshipped: (item) => activityRuns.runCoveringUnshipped(item),
    });
    if (!next) return null;
    // Refused because the row belongs to a part of a held run the pane
    // does not hold: the record is marked dirty and the debounced stub
    // refresh restates the run (see `ApplyItemUpsertsToWindowOptions`).
    for (const runKey of next.dirtiedRunKeys) activityRuns.markRunDirty(runKey);
    if (next.droppedOlderItems) timelineWindow.noteDroppedOlderItems();
    if (next.droppedNewerItems) {
      timelineWindow.noteDroppedNewerItems();
    }
    if (!next.structureChanged && next.changedItems.length === 0) {
      // A merge that only refused rows reads as "nothing reached the
      // window" to callers, except a refusal past the newer edge.
      return next.droppedNewerItems ? next : null;
    }
    options.commitUpsertResult(next, (committed) => finishCommittedUpsert(committed, previousItems));
    return next;
    });
  }

  function applyProviderItemUpserts(
    incoming: Item[],
  ): ApplyItemUpsertsToWindowResult | null {
    const previousTail = options.getItems().at(-1);
    const applied = upsertItemsBatch(incoming, options.optimisticItemIds);
    if (applied && applied.appendedItems.length > 0 && !timelineWindow.hasMoreNewer) {
      activityRuns.noteLiveAppend(applied.appendedItems, previousTail);
    }
    // Discharging an optimistic marker belongs HERE, not in
    // `upsertItemsBatch`: the marker means "this row exists only in
    // this pane's hope", and only the wire can disprove that. Doing it
    // in the shared batch untracked the composer's own optimistic
    // insert on the very call that mounted it (an append lands in
    // `changedItems` too), which left `isOptimisticItem` permanently
    // false — the failed-send rollback never fired, and the
    // cache/replica filters that exist to keep phantoms out of the
    // durable tiers had nothing to filter.
    if (options.optimisticItemIds.size > 0) {
      for (const item of incoming) {
        if (item.threadId === options.getThread()?.id && itemIndexById.has(item.id)) {
          options.optimisticItemIds.delete(item.id);
        }
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
      if (options.scopeRootId !== undefined) return;
      // Expected miss: subagent children never enter the window, so their
      // deltas have nothing to write into. Scoped surfaces that hold the
      // row apply the same event to their own windows.
      if (subagentMemory?.isKnownChild(evt.itemId)) return;
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
    // A tracked child has no window row and no smoother to reconcile.
    if (index === undefined && subagentMemory?.applyChildPatch(evt)) return;
    const current = index === undefined ? undefined : options.getItems()[index];
    if (current && evt.patch.status && isItemStatusRegression(current, { status: evt.patch.status, updatedAt: evt.patch.updatedAt })) return;
    streamingReveal.applyPatch(evt.itemId, evt.patch);
  }

  return {
    upsertItemsBatch,
    applyProviderItemUpserts,
    applyItemDelta,
    applyItemMeta,
    applyItemPatch,
  };
}
