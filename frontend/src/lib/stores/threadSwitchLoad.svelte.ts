import type { Item, Thread } from '../types/models';
import type {
  ContextWindow,
  ProviderSessionAccountEvent,
  ProviderStatusEvent,
} from '../types/events';
import type { TimelineCursor } from '../../../bindings/agent-overflow/internal/store/models';
import {
  AutoResumeThread,
  CloseThreadTerminals,
  ListRecentTurns,
  ListThreadSliceAround,
  MoveThreadTerminals,
  SwitchThread,
  SyncThreadWindow,
  type SyncThreadWindowResult,
} from './bindings';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import {
  closeCompanion,
  closeCompanionsForSource,
  companionForSource,
} from './companionPanes.svelte';
import { evictDiffSpansForThread } from '../utils/diffSpanCache.svelte';
import {
  MAX_CACHED_SNAPSHOT_ITEMS,
  threadItemCache,
  type ThreadItemSnapshot,
} from './threadItemCache';
import {
  applySyncPage,
  itemsForThread,
  reconcileItemWindow,
  type TimelineCursorLike,
} from './threadItems';
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import {
  coldLoadItemsApplied,
  coldLoadPaintSource,
  coldLoadSwitchStart,
  coldLoadSyncStatus,
  type ColdLoadPaintSource,
} from '../utils/coldLoadTrace';
import { clearThreadSizePriors } from '../utils/virtual/priors';
import {
  getReplicaWindow,
  putReplicaWindow,
  removeReplicaWindow,
  replicaToken,
  type ReplicaBody,
} from '../replica';
import {
  UNKNOWN_STAMP_VALUE,
  adoptEventStamp,
  dropThreadHistoryStamp,
  getThreadHistoryStamp,
  recordAttestedStamp,
  type ThreadHistoryStamp,
} from './threadHistoryStamps';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { getBackendIdentity, observeBackendGeneration } from '../transport/backendIdentity';
import {
  clearThreadTerminalState,
  getExistingThreadTerminalState,
  migrateThreadTerminalState,
} from '../components/terminal/terminalStore.svelte';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
} from './threadStatuses.svelte';
import {
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';
import {
  turnRowToSettled,
  type SettledTurn,
  type TurnRow,
} from './threadTurnProjection';
import type { ThreadTimelineWindow } from './threadTimelineWindow.svelte';
import type { ThreadSubagentMemory } from './threadSubagentMemory';
import type { ThreadLiveStateHydration } from './threadLiveStateHydration';
import type { ThreadStreamingReveal } from './threadStreamingReveal.svelte';
import type { ThreadRowUiState } from './threadRowUiState.svelte';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { ThreadChannelState } from './threadChannelState.svelte';
import type { ThreadDesignState } from './threadDesignState.svelte';
import type { ThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import type { LiveTodoState } from './liveTodoState.svelte';
import {
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  REPLICA_WRITE_BACK_DELAY_MS,
  SLICE_AROUND_ITEM_BUDGET,
  SPINNER_THRESHOLD_MS,
  type DraftThreadPlaceholder,
  type PaneScrollController,
} from './threadPaneShared';

export interface ThreadSwitchLoadOptions {
  /** Pane id — companion-pane ownership and the cold-load trace key. */
  paneId: string;
  getThread(): Thread | null;
  /** Install the incoming (or backend-refreshed) thread row on the pane. */
  setThread(next: Thread): void;
  getDraftPlaceholder(): DraftThreadPlaceholder | null;
  /** Drop the placeholder — the pane is committing to a real thread. */
  clearDraftPlaceholder(): void;
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  /** The pane's items-replacement chokepoint (index rebuild, fold retention, dispose, revision bump). */
  replaceTimelineItems(
    nextItems: Item[],
    options?: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
    },
  ): boolean;
  /** Pane switch generation — captured at switch start, compared after every await. */
  getSwitchGeneration(): number;
  /** Bump it and return the new value (`switchThread`'s entry invalidation). */
  bumpSwitchGeneration(): number;
  getLoading(): boolean;
  setLoading(value: boolean): void;
  /**
   * Zero the pane's non-reactive live-content stamp, so a recent stamp
   * from the OUTGOING thread cannot spring the incoming one's settled
   * content.
   */
  resetLiveContentStamp(): void;
  /** Registered pane scroll controller (or null) — the switch-away priors capture. */
  getScrollController(): PaneScrollController | null;
  /** Re-close the warm-up gate for a first content mount. Returns whether it armed. */
  armInitialSliceWarmup(): boolean;
  getLatestSettledTurn(): SettledTurn | null;
  setLatestSettledTurn(next: SettledTurn | null): void;
  getContextWindow(): ContextWindow | null;
  setContextWindow(next: ContextWindow | null): void;
  /** The pane's untagged general-error slot write (clears `generalErrorKind`). */
  setGeneralError(message: string | null): void;
  setProviderBanner(status: ProviderStatusEvent | null | undefined): void;
  setProviderSessionAccount(account: ProviderSessionAccountEvent | null): void;
  setSendInFlight(value: boolean): void;
  /**
   * RAW bottom-drawer visibility write — deliberately not the pane's
   * public `setShowTerminal`, which also holds a settle lease and drops
   * the focus latch. These paths are resets, not user toggles.
   */
  getShowTerminal(): boolean;
  setShowTerminal(value: boolean): void;
  /** Session-scoped effective model; `''` on every reset. */
  setEffectiveModel(model: string): void;
  /** The pane's optimistic-row ledger, read for the stamped-tier filters and cleared on switch. */
  optimisticItemIds: Set<string>;
  /** Placeholder terminal ids this pane has already torn down or migrated. */
  invalidatedDraftTerminalIds: Set<string>;
  timelineWindow: ThreadTimelineWindow;
  subagentMemory: ThreadSubagentMemory;
  rowUiState: ThreadRowUiState;
  activityRuns: ThreadActivityRuns;
  streamingReveal: ThreadStreamingReveal;
  channelState: ThreadChannelState;
  designState: ThreadDesignState;
  pendingInteractiveState: ThreadPendingInteractiveState;
  liveTodoState: LiveTodoState;
  liveStateHydration: ThreadLiveStateHydration;
}

export interface ThreadSwitchLoad {
  /**
   * Spinner-flash gate. `loading` flips true the moment `switchThread`
   * starts; this only resolves true after `SPINNER_THRESHOLD_MS`, so a
   * sub-100ms switch never paints a spinner. The pane pairs it with
   * `items.length === 0` in `showLoadingSpinner`.
   */
  readonly pastSpinnerThreshold: boolean;
  /** Point the pane at `newThread`: snapshot the outgoing one, reset, paint, converge. */
  switchThread(newThread: Thread): Promise<void>;
  /** Re-fetch the visible window after a transport gap, without resetting pane UI state. */
  refreshFromBackend(): Promise<void>;
  /** Drop every cached copy of a thread's window (L1, priors, replica, stamp). */
  dropCachedWindow(threadId: string): void;
  /** Tear down the terminals a draft placeholder opened. */
  closeDraftPlaceholderTerminals(placeholderId: string): void;
  /** Move a draft placeholder's terminals onto the thread it materialized into. */
  migrateDraftPlaceholderTerminals(
    placeholderId: string,
    materializedThreadId: string,
  ): Promise<void>;
  /**
   * Ids the wire touched while a window sync was in flight, or null when
   * no load leg is armed. The upsert path adds to it; `applySyncPage`
   * reads it so a page cannot drop a row that arrived after its read
   * snapshot.
   */
  getLiveTouchedDuringSync(): Set<string> | null;
  /**
   * Release everything this module holds for the pane's current thread:
   * the in-flight sync ledger, the window attestation, the replica
   * write-back timer and the spinner-threshold timer. Called by the
   * pane's `clear()`, which owns the rest of the reset.
   */
  resetPipeline(): void;
}

// Cursor sentinel for a replica window that carries none. `turnIndex`
// below zero fails `cursorIsValid`, so the timeline window falls back to
// deriving bounds from the painted rows exactly as it does for a paged
// response that omits them.
const NO_CURSOR: TimelineCursor = { turnIndex: -1, itemIndex: -1, itemId: '' };

function wireCursor(cursor: TimelineCursorLike | null): TimelineCursor {
  if (!cursor) return NO_CURSOR;
  return {
    turnIndex: cursor.turnIndex,
    itemIndex: cursor.itemIndex,
    itemId: cursor.itemId ?? '',
  };
}

// Shared empty set for the "no live arrivals" page application. Frozen
// by construction: nothing writes through this reference.
const EMPTY_ID_SET: ReadonlySet<string> = new Set<string>();

/**
 * Owns a thread pane's switch / window-sync / replica pipeline: the
 * outgoing-pane snapshot (L1 + durable write-back), the incoming reset,
 * the cache-or-replica paint, the single `SyncThreadWindow` convergence,
 * the parallel hydration fan-out, and the post-gap `refreshFromBackend`
 * re-pull. It also owns the state that only this pipeline touches — the
 * window attestation (docs/specs/thread-replica-sync.md §3.4), the
 * replica write-back timer, the in-flight live-arrival ledger, and the
 * spinner-flash gate.
 *
 * The pane data layer remains the sole mutator of `items` and of every
 * reactive pane field: this factory reads and writes them through
 * `options`, so the assignments still happen inside the pane's own
 * reactive scope. It deliberately does NOT own the timeline mutation
 * chokepoints (`replaceTimelineItems` and the commit/dispose machinery
 * behind it), the scroll-arm decisions (`armInitialSliceWarmup`), the
 * switch generation itself — `MessageTimeline` tracks it as `$state` on
 * the pane — or `clear()`, which resets far more pane state than this
 * pipeline knows about and calls `resetPipeline()` for the rest.
 */
export function createThreadSwitchLoad(
  options: ThreadSwitchLoadOptions,
): ThreadSwitchLoad {
  const { paneId } = options;
  /**
   * Spinner-flash gate. `loading` flips true the instant `switchThread`
   * starts so the rest of the pane sees "load in progress", but the
   * MessageTimeline reads `showLoadingSpinner` instead — that getter
   * stays false for SPINNER_THRESHOLD_MS so a sub-100ms switch (cache
   * hit, fast LAN, fast SQL) never paints the spinner. Above the
   * threshold the spinner fades in; under it the timeline transitions
   * straight to the loaded content. Matches the Doherty perception
   * threshold (~100ms = "instant" to the user).
   */
  let pastSpinnerThreshold: boolean = $state(false);
  let spinnerThresholdTimer: ReturnType<typeof setTimeout> | null = null;
  /**
   * Ids the wire touched while a window sync was in flight. Non-null
   * only for the duration of the item-load leg, so the ordinary upsert
   * path pays one branch and nothing more. `applySyncPage` reads it to
   * keep rows that post-date the page's read snapshot — without it,
   * opening a thread mid-stream would drop the row it is streaming into.
   */
  let liveTouchedDuringSync: Set<string> | null = null;
  /**
   * The attestation for the window THIS PANE currently holds
   * (docs/specs/thread-replica-sync.md §3.4). Attestation is a property
   * of a window, not of a thread id: a globally-looked-up attested stamp
   * can describe a page this pane never received (its write-back never
   * fired, the pane later repainted from an older replica envelope, and
   * the sync that would have converged them threw). Pairing that stamp
   * with these rows is exactly the permanent false `fresh` the spec's
   * understate rule exists to prevent, so the pane carries its own.
   *
   * Set when a sync page installs, when a page-less `fresh` confirms the
   * rows already painted from an attested source, and — critically — to
   * the ENVELOPE's own stamp the moment a replica window is painted, so
   * a sync failure leaves the pane holding what its rows actually
   * descend from. Cleared by anything that changes the window's
   * provenance (thread install, clear, structural cut). Live upserts
   * leave it alone: they arrive through the wire for this thread and
   * only make the rows newer than the stamp, which is the safe
   * direction.
   *
   * `generation` pins it to the backend history lineage it was minted
   * under. A restored database re-mints the generation and wipes the
   * stamp registry, the replica and L1; this field is how a pane that
   * was not syncing at that moment declines to write its dead-lineage
   * rows into the new replica.
   */
  let windowAttestation:
    | { epoch: number; rev: number; generation: string }
    | null = null;
  /**
   * Pending debounced replica write-back for the OPEN thread. The
   * switch-away snapshot is the primary write; this one exists so a
   * crash (or a session that simply never switches away again) still
   * leaves the thread the user is reading in the replica.
   */
  let replicaWriteBackTimer: ReturnType<typeof setTimeout> | null = null;

  /**
   * Run an async leg of `switchThread`'s parallel fan-out and apply its
   * result via `onSuccess` only if the switch generation hasn't moved
   * on. Failures are logged under `label` and routed to optional
   * `onError` (also gen-guarded). The shared helper keeps the
   * gen-guard cadence in one place — adding a new leg is a one-line
   * change instead of a copy of a try/catch block whose early-return
   * order is easy to get wrong.
   */
  function withGenGuard<T>(
    label: string,
    capturedGen: number,
    fn: () => Promise<T>,
    onSuccess: (result: T) => void,
    onError?: (err: unknown) => void,
  ): Promise<void> {
    return (async () => {
      try {
        const result = await fn();
        if (capturedGen !== options.getSwitchGeneration()) return;
        onSuccess(result);
      } catch (err) {
        if (capturedGen !== options.getSwitchGeneration()) return;
        console.error(`Failed to ${label}:`, err);
        onError?.(err);
      }
    })();
  }

  /**
   * Drop every cached copy of a thread's window: the in-memory
   * snapshot, the measured-size priors, the durable replica entry, and
   * the history stamp that described them. Called wherever the pane
   * mutates history structurally (revert, un-send, same-thread reload)
   * or learns the thread is gone — one door, so a new call site cannot
   * evict half of it and leave a stale paint behind the other half.
   */
  function dropCachedWindow(threadId: string): void {
    threadItemCache.evict(threadId);
    clearThreadSizePriors(threadId);
    dropThreadHistoryStamp(threadId);
    // The pane's own attestation describes the window we are dropping,
    // so it dies with it — a structural cut leaves rows no sync ever
    // returned, and re-using the pre-cut stamp for them would persist a
    // window under a stamp that never described it.
    if (options.getThread()?.id === threadId) windowAttestation = null;
    void removeReplicaWindow(threadId);
  }

  function cancelReplicaWriteBack(): void {
    if (replicaWriteBackTimer === null) return;
    clearTimeout(replicaWriteBackTimer);
    replicaWriteBackTimer = null;
  }

  /**
   * Record that a sync answer attested the window the pane holds RIGHT
   * NOW. Called after the rows are installed, never before — the
   * attestation describes them, so it must not outrun them.
   */
  function attestCurrentWindow(epoch: number, rev: number): void {
    windowAttestation = {
      epoch,
      rev,
      generation: getBackendIdentity().generation,
    };
  }

  /**
   * The pane's live window minus the rows that exist only in this pane's
   * hope, plus whether anything was dropped.
   *
   * An optimistic row is a bet on a send: until the wire echoes it,
   * nothing on the backend corresponds to it, so a window containing one
   * is not a window any rev ever had. Both stamped tiers refuse it, and
   * the refusal is the UNDERSTATE side of §3.4 in both directions — the
   * marker itself can be wrong (a row whose echo arrived under a
   * different id stays marked), so "filter and keep the stamp" could
   * just as easily pair a stamp with a window missing a REAL row.
   * Dropping the stamp costs one window fetch and cannot lie either way.
   *
   * The cursors travel with the rows: one naming a dropped row would
   * anchor the next page fetch at an id the backend has never seen;
   * nulling it lets the restore re-derive from the rows that survived.
   */
  function optimisticFreeWindow(): {
    rows: Item[];
    dropped: boolean;
    oldestCursor: TimelineCursorLike | null;
    newestCursor: TimelineCursorLike | null;
  } {
    const items = options.getItems();
    const optimisticItemIds = options.optimisticItemIds;
    const oldestCursor = options.timelineWindow.oldestLoadedCursor ?? null;
    const newestCursor = options.timelineWindow.newestLoadedCursor ?? null;
    if (optimisticItemIds.size === 0) {
      return { rows: items, dropped: false, oldestCursor, newestCursor };
    }
    const rows = items.filter((item) => !optimisticItemIds.has(item.id));
    const isOptimisticCursor = (cursor: TimelineCursorLike | null): boolean =>
      Boolean(cursor?.itemId && optimisticItemIds.has(cursor.itemId));
    return {
      rows,
      dropped: rows.length !== items.length,
      oldestCursor: isOptimisticCursor(oldestCursor) ? null : oldestCursor,
      newestCursor: isOptimisticCursor(newestCursor) ? null : newestCursor,
    };
  }

  /**
   * Persist the pane's live window under the attestation that describes
   * IT (see `windowAttestation`). No attestation, no write: an
   * event-carried stamp can name a rev whose content never reached this
   * client, and persisted that would be a permanent false `fresh`
   * (docs/specs/thread-replica-sync.md §3.4).
   *
   * Live events that landed after the attestation only make the rows
   * NEWER than the stamp, which is the safe direction — the next open
   * asks with an understated stamp and pays one window fetch.
   */
  function persistReplicaWindow(threadId: string): void {
    if (options.getThread()?.id !== threadId) return;
    const attestation = windowAttestation;
    if (!attestation) return;
    // A generation re-mint invalidated every stamp read from the old
    // lineage, including this one; the rows it names belong to a history
    // the backend no longer has.
    if (attestation.generation !== getBackendIdentity().generation) return;
    // Unlike L1, the replica cannot hold rows without a stamp — the
    // envelope IS the pairing. A window with an unconfirmed row in it
    // has nothing to pair, so it simply is not written; the previous
    // envelope stays valid under its own stamp until the next settled
    // sync writes over it.
    const { rows, dropped } = optimisticFreeWindow();
    if (dropped) return;
    if (rows.length === 0) return;
    void putReplicaWindow(threadId, {
      epoch: attestation.epoch,
      rev: attestation.rev,
      savedAt: Date.now(),
      items: rows,
      oldestCursor: options.timelineWindow.oldestLoadedCursor ?? null,
      newestCursor: options.timelineWindow.newestLoadedCursor ?? null,
      hasMoreOlder: options.timelineWindow.hasMoreHistory,
      hasMoreNewer: options.timelineWindow.hasMoreNewer,
      latestSettledTurn: options.getLatestSettledTurn(),
      subagentFolds: options.subagentMemory.snapshotFolds(),
    });
  }

  function scheduleReplicaWriteBack(threadId: string): void {
    cancelReplicaWriteBack();
    replicaWriteBackTimer = setTimeout(() => {
      replicaWriteBackTimer = null;
      if (options.getThread()?.id !== threadId) return;
      persistReplicaWindow(threadId);
    }, REPLICA_WRITE_BACK_DELAY_MS);
  }

  /**
   * Paint a replica window onto the freshly-reset pane. Goes through the
   * same `applyInitialSlice` chokepoint a server slice does — these rows
   * are paint-only and the sync page replaces them, so nothing about the
   * install may differ from the real thing.
   */
  function paintReplicaWindow(body: ReplicaBody, threadId: string): void {
    options.timelineWindow.applyInitialSlice(
      {
        items: body.items,
        oldestCursor: wireCursor(body.oldestCursor),
        newestCursor: wireCursor(body.newestCursor),
        oldestTurnIndex: body.oldestCursor?.turnIndex ?? -1,
        newestTurnIndex: body.newestCursor?.turnIndex ?? -1,
        hasMore: body.hasMoreOlder,
        hasMoreOlder: body.hasMoreOlder,
        hasMoreNewer: body.hasMoreNewer,
      },
      threadId,
    );
    options.subagentMemory.restoreFolds(body.subagentFolds ?? null);
    options.subagentMemory.clearHydrationState();
    // Only when the envelope has one: the ListRecentTurns leg may
    // already have landed with the authoritative row.
    if (body.latestSettledTurn) {
      options.setLatestSettledTurn(body.latestSettledTurn);
    }
  }

  /**
   * Snapshot the outgoing thread into the LRU cache (when worth it),
   * and evict its highlight-span cache entries.
   * Same-thread re-switch skips the
   * snapshot AND force-evicts the cache entry so the incoming load
   * fetches fresh state instead of flashing the stale view through
   * `cache.get`. Streamed events evict inactive-thread cache entries
   * defensively, and evict active-thread entries only when the upsert
   * changes the visible item window; redundant active-thread echoes
   * keep the warm re-entry snapshot intact.
   */
  function snapshotOutgoingPane(incomingThreadId: string): void {
    const outgoingThreadId = options.getThread()?.id ?? null;
    const sameThreadReswitch = outgoingThreadId === incomingThreadId;
    // FIRST, before anything below re-points the pane: the mounted timeline
    // captures its row-size priors while `items` and the engine's measured
    // sizes still describe the thread we are leaving. Unconditional — the
    // priors are keyed by thread, so a same-thread reload benefits too, and
    // the timeline's own gates decide whether there is anything to store.
    options.getScrollController()?.persistSizePriors?.();
    // The pending write-back describes the pane we are leaving; the
    // snapshot below writes the same window synchronously with the
    // stamp it is paired with, so the timer has nothing left to do.
    cancelReplicaWriteBack();
    // L1 is a stamped tier too: `resetIncomingPaneState` clears the
    // optimistic-id ledger, so an optimistic row cached here comes back
    // on a warm re-entry as an untracked phantom — one the next attested
    // answer would re-bless and the write-back would then persist
    // durably. It is dropped from the rows AND costs the snapshot its
    // stamp (see optimisticFreeWindow).
    const l1 = optimisticFreeWindow();
    if (
      outgoingThreadId &&
      !sameThreadReswitch &&
      !options.getLoading() &&
      l1.rows.length > 0 &&
      l1.rows.length <= MAX_CACHED_SNAPSHOT_ITEMS
    ) {
      // Row-size priors are not stored HERE: they live in MessageTimeline
      // (`utils/virtual/priors.ts`), keyed by the scroll-pane width +
      // structure signature + expansion signature that make the sizes
      // valid — all component state the store can't see, and the store has
      // no `listRef` to call `takeSnapshot()` on anyway. That is why they
      // are ASKED for at the top of this function instead. That keyed
      // replay is what lets a re-entry skip the estimate→measure cascade
      // safely; here we cache only the items.
      threadItemCache.set(outgoingThreadId, {
        items: l1.rows,
        oldestLoadedCursor: l1.oldestCursor,
        newestLoadedCursor: l1.newestCursor,
        oldestLoadedTurnIndex: options.timelineWindow.oldestLoadedTurnIndex,
        newestLoadedTurnIndex: options.timelineWindow.newestLoadedTurnIndex,
        hasMoreHistory: options.timelineWindow.hasMoreHistory,
        hasMoreNewer: options.timelineWindow.hasMoreNewer,
        latestSettledTurn: options.getLatestSettledTurn(),
        // Folded subagent children travel with the snapshot: the cached
        // items deliberately exclude evicted rows, so without the fold a
        // warm re-entry would render collapsed cards with zeroed counts
        // until the next live event or hydration.
        subagentFolds: options.subagentMemory.snapshotFolds(),
        // Paired, not looked up on the next open: the stamp is only
        // usable as `haveEpoch`/`haveRev` for the rows it described when
        // it was read. See ThreadItemSnapshot#historyStamp. Dropping a
        // row drops the pairing with it — these rows are no longer the
        // window any stamp described.
        historyStamp: l1.dropped ? null : getThreadHistoryStamp(outgoingThreadId),
      });
      persistReplicaWindow(outgoingThreadId);
    }
    if (sameThreadReswitch) {
      // A same-thread re-switch mutates this thread's items in place, so
      // every cached copy of the previous shape is now a lie — including
      // the durable one, which would otherwise paint the pre-revert rows
      // on the next cold open. Measured-size priors go for the same
      // reason (the structure/content key would refuse them anyway;
      // dropping frees them promptly).
      dropCachedWindow(incomingThreadId);
    }
    if (outgoingThreadId) {
      // Free highlight spans cached against the outgoing thread. The
      // shared cache tracks per-key thread ownership, so entries a
      // still-open thread also requested survive the drop.
      evictDiffSpansForThread(outgoingThreadId);
    }
  }

  /**
   * Wipe pane-scoped state to the empty/default shape for the incoming
   * thread: transient fields, turn-lifecycle pointers, and live-todo
   * state. Pure mutation of pane state — no cache or outgoing-thread
   * side effects.
   */
  function resetIncomingPaneState(newThread: Thread): void {
    options.pendingInteractiveState.clear();
    options.setContextWindow(seedContextWindow(newThread));
    options.setProviderBanner(undefined);
    options.setProviderSessionAccount(null);
    options.setGeneralError(null);
    options.setSendInFlight(false);
    options.optimisticItemIds.clear();
    options.channelState.clear();
    options.designState.reset();
    // Bottom-drawer state is pane-scoped: opening the terminal on thread
    // A should not spill into thread B.
    options.setShowTerminal(false);

    // Turn-lifecycle reset. The active-turn registry lives in
    // threadStatuses.svelte.ts and is keyed by threadId, so a thread
    // switch does NOT clear it — a turn that's still in flight on
    // another thread keeps lighting the working indicator when the user
    // comes back. latestSettledTurn is per-pane; rehydrate it from
    // ListRecentTurns OR from the cache when available. Clear first so
    // a rehydration failure leaves the pane in a consistent state.
    options.setLatestSettledTurn(null);
    options.setEffectiveModel('');

    options.liveTodoState.resetForThread(newThread.id);
  }

  function placeholderHasTerminalState(placeholderId: string): boolean {
    return (
      options.getShowTerminal() ||
      (getExistingThreadTerminalState(placeholderId)?.tabs.length ?? 0) > 0
    );
  }

  function closeDraftPlaceholderTerminals(placeholderId: string): void {
    if (!placeholderHasTerminalState(placeholderId)) return;
    options.invalidatedDraftTerminalIds.add(placeholderId);
    options.setShowTerminal(false);
    clearThreadTerminalState(placeholderId);
    void CloseThreadTerminals(placeholderId).catch((err) => {
      console.error('Failed to close placeholder terminals:', err);
      addToast('error', `Could not close terminal: ${errString(err)}`);
    });
  }

  async function migrateDraftPlaceholderTerminals(
    placeholderId: string,
    materializedThreadId: string,
  ): Promise<void> {
    if (!placeholderHasTerminalState(placeholderId)) return;
    options.invalidatedDraftTerminalIds.add(placeholderId);
    try {
      const summaries = await MoveThreadTerminals(
        placeholderId,
        materializedThreadId,
      );
      migrateThreadTerminalState(
        placeholderId,
        materializedThreadId,
        summaries ?? [],
      );
    } catch (err) {
      console.error('Failed to move placeholder terminals:', err);
      clearThreadTerminalState(placeholderId);
      options.setShowTerminal(false);
      addToast('error', `Could not keep terminal open: ${errString(err)}`);
    }
  }

  /**
   * Look up the incoming thread's cached snapshot and saved scroll
   * anchor, install the snapshot (or fresh empty state) onto the pane,
   * and reset per-row UI registries. Returns the snapshot (so the
   * initial load can decide to skip the fetch on cache hit) and the
   * anchor item id (empty string means tail-load).
   */
  function installCacheOrFreshState(newThread: Thread): {
    cached: ThreadItemSnapshot | null;
    sliceAnchorId: string;
  } {
    const cached = threadItemCache.get(newThread.id);
    const scrollSnapshot = getThreadScrollSnapshot(newThread.id);
    const sliceAnchorId =
      scrollSnapshot?.kind === 'anchor' ? scrollSnapshot.itemId : '';

    options.setLoading(true);
    // The incoming window is nothing this pane has had attested yet. An
    // L1 snapshot carries its own pairing, so it can re-establish the
    // attestation immediately — but only when the stamp it was cached
    // with was itself sync-attested.
    windowAttestation = null;
    if (cached) {
      options.replaceTimelineItems(cached.items);
      options.subagentMemory.restoreFolds(cached.subagentFolds);
      options.subagentMemory.clearHydrationState();
      options.timelineWindow.installFromSnapshot(cached);
      options.setLatestSettledTurn(cached.latestSettledTurn);
      if (cached.historyStamp?.attested) {
        attestCurrentWindow(cached.historyStamp.epoch, cached.historyStamp.rev);
      }
    } else {
      options.replaceTimelineItems([]);
      options.subagentMemory.resetForFreshThread();
      options.timelineWindow.resetForFreshThread();
    }
    options.rowUiState.clear();
    options.activityRuns.clear();
    options.streamingReveal.disposeAll();
    // Reset the live-content stamp so a recent stamp from the OUTGOING
    // thread can't bleed into the incoming one. Without this, switching
    // away from an actively-streaming thread leaves `lastLiveContentAt`
    // recent; the warm gate re-flips within the 500ms hold window, and
    // the incoming (settled) thread's late async-typesetting reflow would
    // read 'spring' off the stale stamp and chase its settled content.
    // A streaming incoming thread re-stamps on its first reveal/delta.
    options.resetLiveContentStamp();
    // Arm the live-arrival ledger HERE, not in the load leg: the pane is
    // already committed to the incoming thread, so a wire upsert can
    // land before the leg's first await resolves. Rows recorded from
    // this point survive the attested page that replaces the paint (see
    // applySyncPage) — without the early arm, a thread opened while it
    // was streaming would lose the row it was streaming into.
    liveTouchedDuringSync = new Set();
    return { cached, sliceAnchorId };
  }

  /**
   * Arm the spinner-flash gate. `loading` flips true the moment
   * `switchThread` starts; `showLoadingSpinner` only resolves to true
   * after `SPINNER_THRESHOLD_MS` AND when items.length === 0. Cache
   * hits never see the spinner because items render immediately;
   * sub-100ms cache misses skip it because the initial slice
   * populates items before the timer fires.
   */
  function armSpinnerThreshold(): void {
    if (spinnerThresholdTimer !== null) {
      clearTimeout(spinnerThresholdTimer);
      spinnerThresholdTimer = null;
    }
    pastSpinnerThreshold = false;
    spinnerThresholdTimer = setTimeout(() => {
      pastSpinnerThreshold = true;
      spinnerThresholdTimer = null;
    }, SPINNER_THRESHOLD_MS);
  }

  /**
   * Commit the incoming thread to the pane.
   */
  function commitIncomingThread(newThread: Thread): void {
    // Companion panes (plan / design-preview / review / take-control)
    // belong to the thread they were opened for. Switching this pane to
    // a DIFFERENT thread closes them instead of retargeting them; a
    // same-thread re-switch keeps them open. Closing
    // happens synchronously, before any effect flush sees the new
    // thread, so a mounted companion body never re-renders against a
    // thread it wasn't opened for.
    const outgoing = options.getThread();
    if (outgoing && outgoing.id !== newThread.id) {
      closeCompanionsForSource(paneId);
    }
    options.clearDraftPlaceholder();
    options.setThread(newThread);
    if (newThread.mode !== 'design') {
      const preview = companionForSource(paneId, 'design-preview');
      if (preview) closeCompanion(preview.paneId);
    }
  }

  /**
   * Apply a settled `SyncThreadWindow` answer to the pane.
   *
   * `fresh` applies nothing: the stamp match itself attests the rows
   * already painted, and they become the live window as-is. `stale` and
   * `rewritten` both carry a page, and the page REPLACES the painted
   * window — no cache-sourced row survives a reconcile, which is what
   * makes the write-back safe (every persisted row descends from an
   * attested page). `gone` drops every cached copy and empties the pane.
   */
  function applySyncResponse(
    response: SyncThreadWindowResult,
    newThread: Thread,
    paintSource: ColdLoadPaintSource,
    sentStamp: ThreadHistoryStamp | null,
    lineageChanged: boolean,
  ): void {
    const threadId = newThread.id;
    coldLoadSyncStatus(paneId, response.status);
    if (response.status === 'gone') {
      dropCachedWindow(threadId);
      options.replaceTimelineItems([], { disposeDropped: true });
      options.timelineWindow.resetAfterLoadError();
      options.setLatestSettledTurn(null);
      options.setGeneralError('This thread no longer exists.');
      return;
    }
    const page = response.page;
    if (page) {
      const incoming = itemsForThread((page.items ?? []) as Item[], threadId);
      const next = applySyncPage(
        incoming,
        options.getItems(),
        liveTouchedDuringSync ?? EMPTY_ID_SET,
      );
      options.replaceTimelineItems(next, { disposeDropped: true });
      options.timelineWindow.applyWindowMetadataFromPaged(page);
      // The warm-gate re-arm belongs to a FIRST content mount only. A
      // page landing over an already-painted window (L1 or replica) is a
      // reconcile the reader may already be looking at; re-closing the
      // gate there would blank content that is on screen.
      //
      // A lineage change is the exception that proves it: the painted
      // rows belong to a history the backend no longer has, so this page
      // does not reconcile them, it replaces all of them. That is a
      // first content mount in everything but name, and it re-arms —
      // synchronously with the mutation, before the flush that mounts
      // the rows (see armInitialSliceWarmup / frontend-scroll.md).
      if (paintSource === 'none' || lineageChanged) {
        const rearmed = options.armInitialSliceWarmup();
        coldLoadItemsApplied(paneId, options.getItems().length, rearmed);
      }
    }
    // Attested last: the stamp describes the rows now installed, so it
    // must not be recorded before they are. A page is a full attestation
    // (the rows arrived with the stamp, one transaction). A page-less
    // `fresh` only attests as much as the stamp we SENT was worth: an
    // echo of an event-carried stamp confirms the server's counter, not
    // that this client received every frame up to it, so upgrading it to
    // attested here would launder it straight into the replica.
    if (page || sentStamp?.attested) {
      recordAttestedStamp(threadId, response.epoch, response.rev);
      // The pane's own copy: this answer attested the window it is
      // holding, which is what a write-back may pair rows with.
      attestCurrentWindow(response.epoch, response.rev);
    } else {
      adoptEventStamp(threadId, response.epoch, response.rev);
      // A page-less answer over an unattested source leaves the pane
      // with rows nothing has attested — including whatever earlier
      // attestation the install carried, which this answer did not
      // confirm.
      windowAttestation = null;
    }
    scheduleReplicaWriteBack(threadId);
  }

  /**
   * The cold-open item leg: paint whatever durable copy exists, then
   * converge it against the backend with one `SyncThreadWindow` call
   * (docs/specs/thread-replica-sync.md §6.1).
   *
   * Ordering, and why it is not a race:
   *
   *  - An L1 hit has already painted synchronously in
   *    `installCacheOrFreshState`, and its snapshot carries the stamp
   *    that described those rows.
   *  - On an L1 miss the IndexedDB read runs BEFORE the RPC is issued.
   *    That is the cache-validator read, not a lost opportunity for
   *    concurrency: a `fresh` answer obliges the client to keep the rows
   *    the stamp matched, so a stamp may only be sent once the content
   *    it describes is in hand. Reading first makes "answered fresh over
   *    nothing" unrepresentable rather than a case to recover from. The
   *    read is local, bounded by the replica's own watchdog, and on a
   *    disabled or failing replica resolves null immediately.
   *  - Every open fires the sync, cache hit included. The old
   *    skip-on-cache-hit was a real staleness hole (another attached
   *    client can rewrite history while a thread sits in the LRU); a
   *    `fresh` answer closes it for ~100 bytes.
   */
  async function runItemWindowSync(
    newThread: Thread,
    gen: number,
    cached: ThreadItemSnapshot | null,
    sliceAnchorId: string,
  ): Promise<void> {
    const threadId = newThread.id;
    let paintSource: ColdLoadPaintSource = cached ? 'l1' : 'none';
    let haveStamp: ThreadHistoryStamp | null = cached?.historyStamp ?? null;

    const ask = (stamp: ThreadHistoryStamp | null): Promise<SyncThreadWindowResult> =>
      SyncThreadWindow(threadId, {
        anchorItemId: sliceAnchorId,
        itemBudget: SLICE_AROUND_ITEM_BUDGET,
        haveEpoch: stamp ? stamp.epoch : UNKNOWN_STAMP_VALUE,
        haveRev: stamp ? stamp.rev : UNKNOWN_STAMP_VALUE,
      });

    try {
      if (!cached) {
        const body = await getReplicaWindow(threadId, replicaToken());
        if (gen !== options.getSwitchGeneration()) return;
        if (body) {
          paintReplicaWindow(body, threadId);
          paintSource = 'replica';
          haveStamp = { epoch: body.epoch, rev: body.rev, attested: true };
          // The envelope's own stamp, paired with the envelope's own
          // rows. If the sync below never lands (transport hiccup) the
          // pane keeps the paint — and must keep the stamp that
          // describes it, not whatever the registry happens to hold for
          // this thread from a page this pane never received.
          attestCurrentWindow(body.epoch, body.rev);
          // Replica rows are structural content mounting into an empty
          // pane, so they re-close the warm gate exactly as an initial
          // slice does — synchronously with the mutation, before the
          // flush that mounts them.
          const rearmed = options.armInitialSliceWarmup();
          coldLoadItemsApplied(paneId, options.getItems().length, rearmed);
        }
      }
      coldLoadPaintSource(paneId, paintSource);

      // The lineage this leg BELIEVES it is talking to, captured before
      // the ask. `observeBackendGeneration` reports whether the
      // observation moved the global identity, which is a different
      // question: with two panes in flight across one flip, only the
      // first to answer moves it, and the second would be told "no
      // change" about the very lineage change that invalidates its
      // painted rows. The global observation still has to happen — it
      // is what wipes the replica, the stamp registry and L1 — but the
      // decision this leg makes is per-leg.
      const believed = getBackendIdentity();
      let sentStamp = haveStamp;
      let response = await ask(sentStamp);
      if (gen !== options.getSwitchGeneration()) return;
      // The response carries the backend's LIVE generation — the one
      // channel that observes a mid-session database restore, since the
      // manifest is only refetched on reconnect. A change here has
      // already wiped the replica, the stamp registry, and the L1 cache
      // (subscribers run synchronously); what remains is this leg's own
      // state: the stamp it sent and the rows it painted belong to the
      // dead lineage, so a page-less answer — even `fresh`, ESPECIALLY
      // `fresh`, which can be a coincidental counter match across
      // lineages — cannot be trusted and is re-asked stampless.
      const observed = observeBackendGeneration(response.generation);
      const lineageChanged =
        observed ||
        (believed.backendId !== '' &&
          believed.generation !== '' &&
          typeof response.generation === 'string' &&
          response.generation !== '' &&
          response.generation !== believed.generation);
      if (response.status !== 'gone' && !response.page && (lineageChanged || paintSource === 'none')) {
        if (!lineageChanged) {
          // A page-less answer means "keep what you have" and we have
          // nothing — the stamp we sent outlived its rows. The pairing
          // rules are supposed to make this unreachable, so report it
          // and re-ask without a stamp rather than leaving the pane
          // empty.
          console.error(
            `replica: sync answered "${response.status}" with no page and nothing painted; refetching`,
          );
          reportFrontendDiagnostic(
            'replica: page-less sync answer with nothing painted',
            `thread=${threadId} status=${response.status}`,
          );
        }
        sentStamp = null;
        response = await ask(sentStamp);
        if (gen !== options.getSwitchGeneration()) return;
      }
      applySyncResponse(response, newThread, paintSource, sentStamp, lineageChanged);
    } catch (err) {
      if (gen !== options.getSwitchGeneration()) return;
      console.error('Failed to sync thread window:', err);
      if (paintSource !== 'none') {
        // A painted window is what the pane had a moment ago and is
        // strictly better than blanking it; the next open re-converges.
        return;
      }
      options.replaceTimelineItems([]);
      options.timelineWindow.resetAfterLoadError();
      options.setGeneralError(`Failed to load thread items: ${errString(err)}`);
      addToast('error', 'Failed to load thread items');
    } finally {
      // Only when this leg is still the current one: a newer switch has
      // already armed its own set, and clearing it here would blind that
      // switch to the arrivals it is about to reconcile against.
      if (gen === options.getSwitchGeneration()) liveTouchedDuringSync = null;
    }
  }

  /**
   * Run the five independent backend fetches that hydrate a thread
   * switch in parallel. Serializing them was the dominant source of
   * switch latency; under `Promise.allSettled` the wall-clock cost is
   * bounded by the slowest leg, not their sum. Each leg gen-guards its
   * own pane writes so a thread swap mid-flight invalidates late
   * resolutions. `switchPromise` and `liveStatePromise` keep their
   * bespoke shapes (the former logs unconditionally; the latter
   * consumes the live-state hydration token); the three canonical
   * paged/list legs go through `withGenGuard`.
   *
   * Returns `{ liveStateHydrationConsumed }` so the caller can decide
   * whether its outer `finally` still needs to call
   * `finishThreadLiveStateHydration` — the live-state leg always
   * consumes the token through `hydrateThreadLiveState`'s own
   * `finally`, but if the leg is invalidated before reaching
   * `hydrateThreadLiveState` (it isn't, today, but the contract is
   * explicit) the caller would still be on the hook.
   */
  async function runParallelLoad(
    newThread: Thread,
    gen: number,
    cached: ThreadItemSnapshot | null,
    sliceAnchorId: string,
    liveStateHydrationToken: number,
  ): Promise<{ liveStateHydrationConsumed: boolean }> {
    let liveStateHydrationConsumed = false;
    const switchPromise = (async () => {
      try {
        const switched = (await SwitchThread(newThread.id)) as
          | Thread
          | undefined;
        if (gen !== options.getSwitchGeneration()) return;
        if (switched?.id === newThread.id) {
          const currentContextWindow = options.getContextWindow();
          options.setThread(switched);
          options.setContextWindow(
            currentContextWindow
              ? normalizeContextWindowForThread(currentContextWindow, switched)
              : seedContextWindow(switched),
          );
        }
      } catch (err) {
        console.error('Failed to notify backend of thread switch:', err);
        addToast('warning', 'Backend was not notified of thread switch');
      }
    })();

    const autoResumePromise = (async () => {
      try {
        await AutoResumeThread(newThread.id);
      } catch (err) {
        // The Go binding only returns an error for the GetThread DB lookup
        // or a transport failure — both root causes the parallel SwitchThread
        // call above hits at the same time and surfaces via its own toast.
        // Session-start failures fire from a backend goroutine through
        // emitErrorToThread, not this binding's return path. A user-visible
        // toast here would double-report the same root cause.
        console.error('Thread auto-resume failed:', err);
      }
    })();

    const liveStatePromise = (async () => {
      try {
        await options.liveStateHydration.hydrateThreadLiveState(
          newThread.id,
          gen,
          liveStateHydrationToken,
        );
      } finally {
        // hydrateThreadLiveState always passes the token through to
        // finishThreadLiveStateHydration in its own finally, so by the
        // time we get here the token is consumed. Flag it so the outer
        // switchThread finally doesn't double-finish.
        liveStateHydrationConsumed = true;
      }
    })();

    // Single initial window via `SyncThreadWindow`. Empty anchor id
    // resolves to the tail at the backend, so it covers both
    // bottom-snapshot and saved-anchor restores. Older items page in
    // lazily via `pane.loadOlder()` (driven by the auto-load trigger in
    // `MessageTimeline.svelte` and the manual "Load older" button).
    const loadItemsPromise = runItemWindowSync(newThread, gen, cached, sliceAnchorId);

    // Two rows of safety so a crashed-then-completed sequence can skip
    // over the in-flight row and still find the prior settled one.
    const recentTurnsPromise = withGenGuard(
      'rehydrate recent turns',
      gen,
      () => ListRecentTurns(newThread.id, 2) as Promise<TurnRow[] | null>,
      (recent) => {
        if (recent && recent.length > 0) {
          const settled = recent.find(
            (row) => row.completedAt !== null && row.completedAt !== undefined,
          );
          if (settled) {
            options.setLatestSettledTurn(turnRowToSettled(settled));
          }
        }
      },
    );

    await Promise.allSettled([
      switchPromise,
      liveStatePromise,
      loadItemsPromise,
      recentTurnsPromise,
      autoResumePromise,
    ]);
    return { liveStateHydrationConsumed };
  }

  async function switchThread(newThread: Thread): Promise<void> {
    // Bump the switch generation BEFORE any synchronous mutation so
    // any in-flight prior switch's late resolutions are invalidated
    // before we touch pane state. `gen` is read by every async leg
    // below and by the outer finally to decide whether the spinner
    // can be cleared (a concurrent switch keeps it up).
    const gen = options.bumpSwitchGeneration();
    const placeholder = options.getDraftPlaceholder();
    if (placeholder) {
      closeDraftPlaceholderTerminals(placeholder.id);
    }
    options.clearDraftPlaceholder();
    // Live-state hydration token. The live-state leg always consumes
    // it through `hydrateThreadLiveState`'s own finally; the outer
    // finally below only finishes it as defense-in-depth against a
    // synchronous throw before runParallelLoad runs.
    let liveStateHydrationConsumed = false;
    let liveStateHydrationToken = 0;
    try {
      snapshotOutgoingPane(newThread.id);
      resetIncomingPaneState(newThread);
      const { cached, sliceAnchorId } = installCacheOrFreshState(newThread);
      // Cold-load instrumentation (dev-trace only; see coldLoadTrace.ts).
      // Draft-placeholder flows (startDraftPlaceholder /
      // adoptMaterializedDraftThread) never call switchThread, so
      // there's nothing to skip here — every switchThread call is a
      // real cold-load candidate. Discussion-surface threads DO reach
      // this point but mount no MessageTimeline to fire the warm-edge
      // mark; their session simply sits open until the next switch
      // overwrites it (see coldLoadSwitchStart).
      coldLoadSwitchStart(paneId, newThread.id, cached ? 'cache-restore' : 'fetch');
      armSpinnerThreshold();
      liveStateHydrationToken = beginThreadLiveStateHydration(newThread.id);
      commitIncomingThread(newThread);
      const result = await runParallelLoad(
        newThread,
        gen,
        cached,
        sliceAnchorId,
        liveStateHydrationToken,
      );
      liveStateHydrationConsumed = result.liveStateHydrationConsumed;
      if (gen !== options.getSwitchGeneration()) return;
      options.setLoading(false);
    } finally {
      // Defense in depth against an uncaught exception (a synchronous
      // throw between bumping `gen` and runParallelLoad's own gen
      // checks) leaving `loading=true` stranded. Only clear when no
      // newer switch has superseded ours — a concurrent switch is
      // supposed to keep the indicator up.
      if (gen === options.getSwitchGeneration()) {
        options.setLoading(false);
        // `runItemWindowSync` clears this in its own finally, so this
        // is a no-op on every path that reached it. It is the cover
        // for the paths that did not: a synchronous throw between the
        // generation bump and the leg starting would otherwise leave
        // the ledger armed for the pane's lifetime, and every later
        // upsert would accumulate into a set the next page
        // application reads as live arrivals it must not drop.
        liveTouchedDuringSync = null;
      }
      if (liveStateHydrationToken !== 0 && !liveStateHydrationConsumed) {
        finishThreadLiveStateHydration(newThread.id, liveStateHydrationToken);
      }
    }
  }

  /**
   * Re-fetch the visible window from the backend without resetting
   * pane-scoped UI state (terminal / diff panel / draft). Used by the
   * transport-gap consumer when a missed event window forces a full
   * reconcile of the active pane. Honours the switch generation so a
   * thread swap mid-fetch invalidates the late resolution.
   *
   * Coarse on purpose — when we know we lost events, the cheap fix is
   * to re-pull from SQLite which is the authoritative history cache.
   * Surgical reconciliation would need the channel + seq window the
   * transport doesn't expose to the consumer today.
   */
  async function refreshFromBackend(): Promise<void> {
    const currentThread = options.getThread();
    if (!currentThread) return;
    const gen = options.getSwitchGeneration();
    let liveStateHydrationToken = beginThreadLiveStateHydration(
      currentThread.id,
    );
    try {
      try {
        const anchorItemId = options.timelineWindow.hasMoreNewer
          ? (options.getItems().at(-1)?.id ?? '')
          : '';
        const paged = await ListThreadSliceAround(
          currentThread.id,
          anchorItemId,
          ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        );
        if (gen !== options.getSwitchGeneration()) return;
        const nextItems = reconcileItemWindow(
          itemsForThread((paged.items ?? []) as Item[], currentThread.id),
          options.getItems(),
        );
        options.replaceTimelineItems(nextItems, { disposeDropped: true });
        options.timelineWindow.applyWindowMetadataFromPaged(paged);
      } catch (err) {
        if (gen !== options.getSwitchGeneration()) return;
        console.error('Failed to refresh thread items after gap:', err);
        return;
      }
      try {
        const recent = (await ListRecentTurns(currentThread.id, 2)) as
          | TurnRow[]
          | null;
        if (gen !== options.getSwitchGeneration()) return;
        if (recent && recent.length > 0) {
          const settled = recent.find(
            (row) =>
              row.completedAt !== null && row.completedAt !== undefined,
          );
          if (settled) {
            options.setLatestSettledTurn(turnRowToSettled(settled));
          }
        }
      } catch (err) {
        if (gen !== options.getSwitchGeneration()) return;
        console.error('Failed to refresh recent turns after gap:', err);
      }
      options.pendingInteractiveState.prepareForLiveStateHydration();
      await options.liveStateHydration.hydrateThreadLiveState(
        currentThread.id,
        gen,
        liveStateHydrationToken,
      );
      liveStateHydrationToken = 0;
    } finally {
      if (liveStateHydrationToken !== 0) {
        finishThreadLiveStateHydration(
          currentThread.id,
          liveStateHydrationToken,
        );
      }
    }
  }

  function resetPipeline(): void {
    // A switchThread that ran clear() mid-flight could otherwise leave
    // the spinner-threshold timer pending. When it fires it would flip
    // pastSpinnerThreshold true against an empty pane
    // (showLoadingSpinner gates on items.length===0 + loading, both of
    // which clear() leaves false — so user-visible surface is
    // unaffected, but the leak is real).
    if (spinnerThresholdTimer !== null) {
      clearTimeout(spinnerThresholdTimer);
      spinnerThresholdTimer = null;
    }
    // Same shape for the replica write-back: it would fire against a
    // pane that no longer holds the thread it was scheduled for.
    cancelReplicaWriteBack();
    // The window this attested is gone, and so is the ledger of live
    // arrivals the in-flight sync leg was collecting: only
    // `runItemWindowSync`'s finally cleared the latter, so a pane
    // cleared mid-load (or a leg that threw before reaching it) left
    // it armed, and every later upsert kept appending to a set the
    // NEXT page application would then read as "arrived during my
    // sync" and refuse to drop.
    windowAttestation = null;
    liveTouchedDuringSync = null;
    pastSpinnerThreshold = false;
  }

  return {
    get pastSpinnerThreshold() {
      return pastSpinnerThreshold;
    },
    switchThread,
    refreshFromBackend,
    dropCachedWindow,
    closeDraftPlaceholderTerminals,
    migrateDraftPlaceholderTerminals,
    getLiveTouchedDuringSync: () => liveTouchedDuringSync,
    resetPipeline,
  };
}
