import { tick } from 'svelte';
import type { Item, Project, Thread } from '../types/models';
import { asProviderID } from '../types/providers';
import type { Checkpoint } from '../types/checkpoint';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
  TodoStep,
  ProviderStatusEvent,
  SubagentNotificationEvent,
  UserInputRequest,
} from '../types/events';
import type {
  CheckpointCapturedEvent,
  CheckpointErrorEvent,
  CheckpointRevertedEvent,
  CheckpointUnavailableEvent,
} from '../types/checkpoint';
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import type {
  ActiveOptionSet,
  ClarificationRequest,
  DesignViewport,
} from '../types/design';
import {
  CloseThreadTerminals,
  CreateThread,
  ListRecentTurns,
  ListThreadCheckpoints,
  MoveThreadTerminals,
  SwitchThread,
  AutoResumeThread,
} from './bindings';
import { prependThread, removeThread, replaceThread } from './threads.svelte';
import { leaseDuringSettle } from '../utils/scrollLeaseDuringTransition';
import {
  clearWorktreeIntent,
  migrateWorktreeIntent,
  seedDefaultWorktreeIntentForDraft,
} from './worktreeIntent.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

import { addToast } from './toast.svelte';
import { createThreadCheckpointState, type ThreadCheckpointState } from './threadCheckpoints.svelte';
import { createGitStatusSlot, type GitStatusSlot } from './gitStatus.svelte';
import {
  closeCompanion,
  companionForSource,
  isCompanionOpen,
  openCompanion,
  toggleCompanion,
} from './companionPanes.svelte';
import { openReviewCompanion } from './reviewPane.svelte';
import { errString } from '../utils/errors';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { SubagentFoldAggregate } from '../utils/subagentFold';
import { clearTokensForThread } from '../utils/tokenCacheReactive.svelte';
import {
  MAX_CACHED_SNAPSHOT_ITEMS,
  threadItemCache,
  type ThreadItemSnapshot,
} from './threadItemCache';
import {
  type ApplyItemUpsertsToWindowResult,
  applyItemUpsertsToWindow,
  itemsAreEqual,
  itemsForThread,
  reconcileItemWindow,
} from './threadItems';
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { coldLoadItemsApplied, coldLoadSwitchStart } from '../utils/coldLoadTrace';
import { clearThreadSizePriors } from '../utils/virtual/priors';
import { ListThreadSliceAround } from './bindings';
import { sameNormalizedPath } from '../utils/path';
import {
  clearThreadTerminalState,
  getExistingThreadTerminalState,
  migrateThreadTerminalState,
} from '../components/terminal/terminalStore.svelte';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  getActiveTurn,
  projectTurnCompleted,
  projectTurnStarted,
  type ActiveTurn,
} from './threadStatuses.svelte';
import { createLiveTodoState } from './liveTodoState.svelte';
import { createThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import {
  turnRowToSettled,
  type SettledTurn,
  type TurnRow,
} from './threadTurnProjection';
import { createThreadRowUiState } from './threadRowUiState.svelte';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import { createThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { createThreadSubagentMemory } from './threadSubagentMemory';
import { createThreadLiveStateHydration } from './threadLiveStateHydration';
import {
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';
import { createThreadChannelState } from './threadChannelState.svelte';
import { createThreadDesignState } from './threadDesignState.svelte';
import {
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  SLICE_AROUND_ITEM_BUDGET,
  SPINNER_THRESHOLD_MS,
  isSmoothLiveContentKind,
  nowForLiveContent,
  threadUsesDiscussionSurface,
  type DraftThreadPlaceholder,
  type DraftPlaceholderDefaults,
  type DraftPlaceholderMode,
  type LoadOlderResult,
  type PaneScrollController,
  type ScrollToItemRequest,
  type ScrollToItemOptions,
  type ThreadPaneOptions,
} from './threadPaneShared';

// ActiveTurn now lives in threadStatuses.svelte.ts (single source of
// truth for the global active-turn registry). Re-exported here so
// downstream importers (events.ts, panes, tests) don't have to rewire
// their imports for the move.
export type { ActiveTurn } from './threadStatuses.svelte';

export { parseTokenUsage } from './threadTurnProjection';
export type { SettledTurn } from './threadTurnProjection';

export {
  __setSmoothingClockForTest,
  paneWorkspacePath,
} from './threadPaneShared';
export type {
  DraftPlaceholderDefaults,
  DraftPlaceholderMode,
  DraftThreadPlaceholder,
  LoadOlderResult,
  PaneScrollController,
  ScrollToItemOptions,
  TimelineWindowAnchorOperation,
} from './threadPaneShared';

export {
  __resetActivityRailUiPrefsForTest,
  __resetLiveTodoUiPrefsForTest,
  dropActivityRailUiPrefs,
  dropLiveTodoUiPrefs,
  LIVE_TODO_AUTOHIDE_MS,
} from './liveTodoState.svelte';
export type { LiveTodo } from './liveTodoState.svelte';

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, unified timeline items, approvals,
 * context/banner state, and mode-specific UI. Components receive a
 * ThreadPane as a prop.
 */
export function createThreadPane(options: ThreadPaneOptions = {}) {
  const paneId = options.paneId ?? 'pane';
  let thread: Thread | null = $state(null);
  let draftPlaceholder: DraftThreadPlaceholder | null = $state(null);
  let items: Item[] = $state([]);
  // Structural revision for timeline projections that should skip
  // summary-only streaming deltas. Bump whenever the item window's array
  // changes shape or identity; `applyItemDelta` intentionally does not bump.
  let timelineRevision = $state(0);
  // Non-reactive timestamp of the last LIVE timeline content advance — a
  // smoother reveal, an overwrite patch, a text-like provider row, or a
  // visible-field update to an already mounted row (tool output preview,
  // running→completed result chrome; see events.ts
  // providerUpsertAdvancesLiveContent).
  // Read imperatively by the scroll controller
  // (MessageTimeline's `animationMode` getter) to choose spring vs
  // sync-pin; see utils/springAnimationLatch.ts. Deliberately NOT
  // `$state`: it is stamped up to ~60×/sec during a drain and is never
  // read in a reactive scope, so `$state` would churn every dependent
  // derivation for no benefit. Bulk loads (thread switch, load-older) and
  // the optimistic user-send / rollback-restore upserts deliberately do
  // NOT stamp it, so they stay sync-pinned.
  let lastLiveContentAt = 0;
  function stampLiveContent(): void {
    lastLiveContentAt = nowForLiveContent();
  }
  const itemIndexById: Map<string, number> = new Map();
  function getItemById(itemId: string): Item | undefined {
    const index = itemIndexById.get(itemId);
    return index === undefined ? undefined : items[index];
  }
  function isPayloadReferenced(threadId: string, payloadId: string): boolean {
    return items.some((item) =>
      item.threadId === threadId && item.payloadId === payloadId,
    );
  }
  const rowUiState = createThreadRowUiState({
    getItemById,
    isPayloadReferenced,
  });
  // Per-item smoother + reveal-gate machinery lives in
  // threadStreamingReveal.svelte.ts. Every items-mutation path that can
  // change which top-level rows exist relative to a live smoother must
  // call `streamingReveal.recomputeReveal()` (or `.disposeAll()`).
  const streamingReveal = createThreadStreamingReveal({
    getItemById,
    getItemIndex: (itemId) => itemIndexById.get(itemId),
    getItems: () => items,
    setItemAt: (index, item) => {
      items[index] = item;
    },
    stampLiveContent,
    armStructuralSpring,
    appendLivePayloadDeltaForItem: rowUiState.appendLivePayloadDeltaForItem,
  });
  // Windowed-history / paging machinery (loaded-window cursors and flags,
  // the prune paths, and the four load methods) lives in
  // threadTimelineWindow.svelte.ts. `scrollController` is declared later
  // in this closure — the accessor arrow is safe to capture ahead of its
  // textual declaration. `subagentMemory` (owns child hydration) is a
  // `const` also declared later — wrapping its call in an arrow keeps
  // the `subagentMemory` property read lazy (deferred until the arrow is
  // actually invoked, well after the whole closure finishes
  // constructing), so it never hits the TDZ; a direct
  // `subagentMemory.hydrateChildren` reference here would throw
  // immediately instead.
  const timelineWindow = createThreadTimelineWindow({
    getItems: () => items,
    replaceTimelineItems,
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    recomputeReveal: streamingReveal.recomputeReveal,
    getScrollController: () => scrollController,
    hydrateSubagentChildren: (rootItemID) =>
      subagentMemory.hydrateChildren(rootItemID),
  });
  const pendingInteractiveState = createThreadPendingInteractiveState();
  let contextWindow: ContextWindow | null = $state(null);
  // Rate-limit snapshots live in the global `rateLimitsInfo.svelte.ts`
  // store keyed by provider — they are an account property, not a
  // thread property. Components read via `getProviderRateLimit(provider,
  // windowMins)` directly. Keeping them out of per-pane state means
  // they survive thread switches, turn completions, and metadata
  // updates with no defensive logic on the pane side.
  let providerBanner: ProviderStatusEvent | null | undefined =
    $state(undefined);
  // generalError is the grab-bag pane-level error slot surfaced by
  // ProviderStatusBanner for non-wire failures: thread load failures,
  // composer send failures, git action failures, reconnect failures.
  // It is deliberately distinct from providerBanner (which mirrors the
  // provider's own session/auth/rate-limit state) — consumers treat
  // them as two independent reasons to show the top-of-pane banner.
  let generalError: string | null = $state(null);
  // generalErrorKind tags the source of the current generalError so the
  // turn-start clear path can target session-death banners specifically
  // without wiping orthogonal errors (rename failed, git status, thread
  // load) that happen to be visible. `null` ≡ "any kind" or "no error";
  // `'session'` ≡ the message came from a provider session_died event.
  let generalErrorKind: 'session' | null = $state(null);
  let loading: boolean = $state(false);
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
  // sendInFlight is the optimistic stop-button gate. The composer flips
  // it true the moment the user clicks Send and clears it in `finally`.
  // Used by SendButton to render the stop variant before
  // `provider:turn_started` arrives, and by the thread.interrupt
  // keybinding's `when` clause so Esc clears the prompt during the
  // dispatch window. Cleared on thread switch in clear() so the pane
  // doesn't carry sending state into the next thread.
  let sendInFlight: boolean = $state(false);
  const optimisticItemIds = new Set<string>();
  // materializingThreadPromise coalesces concurrent ensureMaterializedThread
  // callers — composer input, paste/upload, send, toolbar pickers — into a
  // single CreateThread call. Cleared in `finally` so a subsequent
  // placeholder can materialize on its own.
  let materializingThreadPromise: Promise<string | null> | null = null;
  const invalidatedDraftTerminalIds = new Set<string>();
  let showTerminal: boolean = $state(false);
  // One-shot "focus the terminal once it exists" intent. Set by
  // runTerminalToggle on a drawer open (cold start) and by pane.focusLeft/Right
  // when navigating INTO an already-mounted terminal pane (warm start). It is
  // `$state` so the terminal surface can consume it reactively in a $effect:
  // the warm path mutates it on a live surface, which a plain closure `let`
  // (consumed once in onMount) would miss. The latch still survives the async
  // gap between "open requested" and "lazy drawer chunk loaded + mounted" — it
  // stays set until the surface mounts and reads it. Replaces the old
  // fire-once FOCUS_TERMINAL_EVENT, whose listener didn't exist yet when the
  // event fired on a cold first open (the lazy import hadn't resolved).
  let pendingTerminalFocus = $state(false);
  // Checkpoint bookkeeping is per-pane and resets on thread switch so
  // per-message checkpoint affordances never flash stale availability.
  const checkpoints: ThreadCheckpointState = createThreadCheckpointState();

  // Live git-status for this pane's workspace. Owns the single gitwatch
  // subscription (driven by ChatHeaderActions via attach); GitActionsControl
  // and the header diff/PR badges read it. Reset on thread switch like
  // checkpoints so a stale count never flashes for the incoming thread.
  const gitStatus: GitStatusSlot = createGitStatusSlot();

  const channelState = createThreadChannelState();
  const designState = createThreadDesignState();

  // Turn-lifecycle state. The active turn lives in the global registry
  // in threadStatuses.svelte.ts (read directly via `getActiveTurn` at
  // every call site so the source of truth is traceable); the load-
  // bearing benefit is that switching threads no longer clears the
  // working indicator for a turn that's still in flight on the
  // departing thread. `latestSettledTurn` stays per-pane for read-state
  // and trace/debug consumers; on thread switch we rehydrate it from the
  // most recent `ListRecentTurns` row whose `completedAt` is non-null.
  let latestSettledTurn: SettledTurn | null = $state(null);
  // Session-scoped model actually serving the thread after a provider
  // fallback. The durable thread.model remains the user's requested model.
  let effectiveModel = $state('');
  let effectiveModelRevision = 0;
  let effectiveModelBackendRevision = 0;
  function updateEffectiveModel(model: string): void {
    effectiveModel = model.trim();
    effectiveModelRevision += 1;
  }
  const liveTodoState = createLiveTodoState();
  // Thread live-state hydration protocol (GetThreadLiveState +
  // ListPendingInteractiveRequests fallback, projected onto the global
  // active-turn/send-queue registries and onto pendingInteractiveState /
  // liveTodoState) lives in threadLiveStateHydration.ts. Instantiated
  // here because both dependencies above must already exist.
  const liveStateHydration = createThreadLiveStateHydration({
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    pendingInteractiveState,
    liveTodoState,
    getEffectiveModelRevision: () => effectiveModelRevision,
    hydrateEffectiveModel: (model, backendRevision, expectedMutationRevision) => {
      if (effectiveModelRevision !== expectedMutationRevision) return;
      if (backendRevision < effectiveModelBackendRevision) return;
      effectiveModelBackendRevision = backendRevision;
      updateEffectiveModel(model);
    },
  });
  // Subagent notification log. The backend emits
  // `provider:subagent_notification` as a pass-through; no UI consumes it
  // today, but keeping a bounded in-pane log lets future surfaces (tray,
  // toast) subscribe without re-wiring the channel. We cap at a small
  // number of most-recent entries so the array can't grow unbounded in a
  // session that generates many notifications.
  let subagentNotifications: SubagentNotificationEvent[] = $state([]);
  const subagentNotificationLimit = 32;

  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow paged fetch from thread A cannot clobber thread B's
   * items when the user flips between them quickly. Also exposed
   * publicly via the `switchGeneration` getter so MessageTimeline's
   * `$effect.pre` can detect same-thread re-switch (the
   * revert-to-checkpoint flow) and re-run its restore reset path —
   * must be `$state` for that effect dependency to track.
   */
  let switchGeneration = $state(0);

  // Subagent transcript-memory domain (the live-eviction fold registry,
  // settled-child eviction policy, and on-demand child hydration) lives
  // in threadSubagentMemory.ts. `replaceTimelineItems` is declared later
  // in this closure — safe to capture ahead of its textual declaration
  // because it's a hoisted function declaration, not a `const`.
  const subagentMemory = createThreadSubagentMemory({
    getItems: () => items,
    getItemIndex: (itemId) => itemIndexById.get(itemId),
    replaceTimelineItems,
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    recomputeReveal: streamingReveal.recomputeReveal,
    isSubagentGroupExpanded: rowUiState.isSubagentGroupExpanded,
  });

  /**
   * Nonce bumped when the pane wants the active MessageTimeline to scroll
   * to a specific item. Scroll side effects are DOM operations that
   * shouldn't live on the store, so the store publishes an intent and
   * the timeline reads it reactively. Consumers compare the most
   * recently observed nonce against `scrollToItemRequest.nonce` and
   * react when it changes. `itemId` is the target id; an empty string
   * means "no outstanding request". `behavior` and `flash` let the
   * owner of the actual scroll container decide how visible the jump
   * should be without exposing DOM methods through the pane.
   */
  let scrollToItemRequest: ScrollToItemRequest = $state({
    itemId: '',
    nonce: 0,
    flash: false,
  });

  /**
   * Live registration slot for the timeline's sticky-bottom controller.
   * MessageTimeline registers its controller on mount so external surfaces
   * (inspector panels, resizable panes) can acquire a `pauseAutoScroll()` lease while a
   * gesture is in flight, preventing auto-follow from yanking the view
   * mid-drag. The factory only knows about the minimal surface
   * (`PaneScrollController`) — it never depends on the virtualizer or the DOM
   * controller's full type, so the contract stays cheap to honour.
   */
  let scrollController: PaneScrollController | null = $state(null);

  // Monotonic token that cancels superseded structural nudges: bumped by
  // every armStructuralSpring() call so only the latest scheduled nudge
  // fires. Switch/reload/clear staleness is covered by the
  // `switchGeneration` capture in the nudge itself, matching the store's
  // universal post-await staleness idiom.
  let structuralNudgeToken = 0;

  // WebKit suspends rAF for hidden/minimized windows while wire batches
  // keep flushing on timeouts, so a bare rAF await would park one nudge
  // chain per append-bearing flush until the window is restored. Race a
  // short timeout against the frame: the nudge is a cheap escape-aware
  // re-check, so firing it on the timeout path while hidden is harmless,
  // and each chain's lifetime stays bounded either way.
  const HIDDEN_FRAME_FALLBACK_MS = 32;
  function nextAnimationFrame(): Promise<void> {
    return new Promise((resolve) => {
      if (typeof requestAnimationFrame !== 'function') {
        setTimeout(resolve, 0);
        return;
      }
      let settled = false;
      const settle = (): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timeoutHandle);
        cancelAnimationFrame(rafHandle);
        resolve();
      };
      const rafHandle = requestAnimationFrame(settle);
      const timeoutHandle = setTimeout(settle, HIDDEN_FRAME_FALLBACK_MS);
    });
  }

  /**
   * Arm the structural-append spring and schedule its follow-up nudge.
   * The pane data layer is the sole owner of this decision; the two call
   * sites are `applyProviderItemUpserts` (a wire append to the loaded
   * tail) and `recomputeRevealPass` (the reveal gate releasing withheld
   * rows). Scroll writes still belong to the controller — the pane only
   * talks to the registered `PaneScrollController` surface, the same
   * seam the `scrollToItemRequest` intent publishes through when a
   * scroll needs virtualizer index resolution.
   *
   * The arm runs synchronously with the data change — strictly before the
   * Svelte flush in which the virtualizer measures the new/released rows
   * and delivers their geometry — so the growth itself is spring-eligible,
   * not just the remeasure that follows it. An effect-based arm loses that
   * ordering race (bug-report-20260702T193212Z).
   *
   * The nudge (observe('live-content') after flush + one frame) re-checks
   * the bottom once the DOM has settled. A thinking row tail-pins its
   * clipped body internally, so its visible movement often does not grow
   * the outer timeline row; when the next top-level row mounts, contentRO
   * timing alone can miss the first bottom target, especially with
   * Streamdown's async markdown layout still growing the row.
   * 'live-content' honors spring mode / the just-armed structural window
   * and is escape-aware, so a user scrolled away is never yanked.
   *
   * Gates, shared by every caller:
   * - `loading`: the whole switch+load settle is a restore, not an
   *   in-turn append (bug-report-20260622T041049Z class); the warm gate
   *   independently pins the post-restore settle.
   * - discussion surface: those panes swap the chat timeline for
   *   ChannelView, which attaches ITS OWN controller here; timeline item
   *   changes render nothing, and arming would open a 250ms spring
   *   window on unrelated channel-message growth.
   */
  function armStructuralSpring(): void {
    const controller = scrollController;
    if (!controller) return;
    if (loading) return;
    if (threadUsesDiscussionSurface(thread)) return;
    controller.markStructuralContentPending();
    const token = ++structuralNudgeToken;
    const generation = switchGeneration;
    void (async () => {
      await tick();
      await nextAnimationFrame();
      if (token !== structuralNudgeToken) return;
      if (generation !== switchGeneration) return;
      if (scrollController !== controller) return;
      controller.observe('live-content');
    })();
  }

  function rebuildItemIndexes(nextItems: Item[]): void {
    itemIndexById.clear();
    for (let index = 0; index < nextItems.length; index += 1) {
      const item = nextItems[index];
      itemIndexById.set(item.id, index);
    }
  }

  function disposeDroppedItemState(
    previous: readonly Item[],
    nextItems: readonly Item[],
    exhaustedScope?: ReadonlySet<string>,
  ): void {
    if (previous.length === 0) return;
    const keptIds = new Set(nextItems.map((item) => item.id));
    const droppedItems: Item[] = [];
    for (const item of previous) {
      if (keptIds.has(item.id)) continue;
      droppedItems.push(item);
    }
    if (droppedItems.length === 0) return;
    // Dropped rows can include hydrated subagent children — re-arm their
    // anchors for hydration. See threadSubagentMemory.ts
    // `resetHydrationExhausted` for the full rationale.
    subagentMemory.resetHydrationExhausted(exhaustedScope);
    for (const item of droppedItems) streamingReveal.disposeSmootherFor(item.id);
    rowUiState.disposeItems(droppedItems);
  }

  function replaceTimelineItems(
    nextItems: Item[],
    options: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
    } = {},
  ): boolean {
    if (items === nextItems) return false;
    const previous = options.disposeDropped ? items : [];
    items = nextItems;
    rebuildItemIndexes(items);
    // Fold↔items chokepoint: folds are only meaningful while their
    // anchor row is loaded — once an anchor leaves the window, the
    // next load of its region decorates from SQLite. Every wholesale
    // window replacement (prune, reconcile, revert, cache install,
    // eviction) flows through here, so one sweep after the index
    // rebuild keeps the registry consistent everywhere. The upsert
    // fast path bypasses this function but never drops existing rows.
    // Eviction callers record their folds BEFORE replacing, with the
    // anchors still loaded, so those folds are retained.
    subagentMemory.retainFoldAnchors();
    if (options.disposeDropped) {
      disposeDroppedItemState(previous, items, options.exhaustedScope);
    }
    timelineRevision++;
    return true;
  }

  // Subagent eviction policy (evictableAnchorIdFor, collectSettledSubtree,
  // commitSubagentEvictions, evictSettledChildren, evictCollapsedSubtree)
  // lives in threadSubagentMemory.ts as `subagentMemory`.
  // The per-item smoother + reveal-gate sequencer (disposeSmootherFor,
  // disposeAll, recomputeReveal, getOrCreateSmoothing, etc.) live in
  // threadStreamingReveal.svelte.ts as `streamingReveal`. Every
  // items-mutation path that can change which top-level rows exist
  // relative to a live smoother must call `streamingReveal.recomputeReveal()`
  // (or `.disposeAll()`, which clears the boundary).

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

    const next = applyItemUpsertsToWindow({
      current: items,
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
    if (next.droppedNewerItems) {
      timelineWindow.noteDroppedNewerItems();
    }
    if (!next.structureChanged && next.changedItems.length === 0) {
      return next;
    }
    if (optimisticItemIds.size > 0) {
      for (const changed of next.changedItems) {
        optimisticItemIds.delete(changed.id);
      }
    }
    items = next.items;
    if (next.indexesNeedRebuild) {
      rebuildItemIndexes(items);
    } else {
      const firstAppendIndex = items.length - next.appendedItems.length;
      for (let index = 0; index < next.appendedItems.length; index += 1) {
        itemIndexById.set(
          next.appendedItems[index].id,
          firstAppendIndex + index,
        );
      }
    }
    if (next.structureChanged) timelineRevision++;
    if (next.appendedItems.length > 0) {
      timelineWindow.refreshCursorsAfterTailAppend();
    }
    // Live eviction runs before the window-cap check so settled subagent
    // children never count toward the prune trigger — the cap effectively
    // bounds renderable rows, matching the backend pagers' top-level-only
    // budget since 6187d039.
    subagentMemory.evictSettledChildren(next.changedItems);
    if (next.appendedItems.length > 0 && !timelineWindow.hasMoreNewer) {
      timelineWindow.pruneToRecentWindowIfNeeded();
    }

    streamingReveal.reconcileUpsertedItems(next.changedItems);

    // Design-mode side-channel: scan assistant text for structured
    // `aoflow-design` payloads and project them onto pane state. Cheap
    // when no payload is present (the parser short-circuits on the
    // missing fence prefix); designState owns dedupe across streaming
    // deltas and resets it on thread switch.
    if (thread?.mode === 'design') {
      for (const item of next.changedItems) {
        designState.applyAssistantPayloadsForItem(item, thread);
      }
    }
    // A newly-appended successor row should withhold behind the streaming
    // frontier and trigger its fast-drain; a terminal upsert that disposed
    // the frontier should drop the gate. Recompute once per batch.
    streamingReveal.recomputeReveal();
    return next;
  }

  // Thread live-state hydration protocol (applyPendingInteractiveSnapshot,
  // hydratePendingInteractiveRequests, applyThreadLiveStateSnapshot,
  // hydrateThreadLiveState) lives in threadLiveStateHydration.ts as
  // `liveStateHydration`.

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
        if (capturedGen !== switchGeneration) return;
        onSuccess(result);
      } catch (err) {
        if (capturedGen !== switchGeneration) return;
        console.error(`Failed to ${label}:`, err);
        onError?.(err);
      }
    })();
  }

  // Child-transcript hydration for a subagent launch anchor
  // (hydrateSubagentChildren) lives in threadSubagentMemory.ts as
  // `subagentMemory.hydrateChildren`.

  async function refreshCheckpointsForThread(threadID: string): Promise<void> {
    const checkpointRows = ((await ListThreadCheckpoints(threadID)) ??
      []) as Checkpoint[];
    if (thread?.id !== threadID) return;
    const sorted = [...checkpointRows].sort((a, b) => a.turnIndex - b.turnIndex);
    checkpoints.setCheckpoints(sorted);
  }

  /**
   * Snapshot the outgoing thread into the LRU cache (when worth it),
   * and the partitioned shiki token cache.
   * Same-thread re-switch (revert-to-checkpoint flows) skips the
   * snapshot AND force-evicts the cache entry so the incoming load
   * fetches fresh state instead of flashing the stale view through
   * `cache.get`. Streamed events evict inactive-thread cache entries
   * defensively, and evict active-thread entries only when the upsert
   * changes the visible item window; redundant active-thread echoes
   * keep the warm re-entry snapshot intact.
   */
  function snapshotOutgoingPane(incomingThreadId: string): void {
    const outgoingThreadId = thread?.id ?? null;
    const sameThreadReswitch = outgoingThreadId === incomingThreadId;
    if (
      outgoingThreadId &&
      !sameThreadReswitch &&
      !loading &&
      items.length > 0 &&
      items.length <= MAX_CACHED_SNAPSHOT_ITEMS
    ) {
      // The timeline's row-size priors are snapshotted, but NOT here: they
      // live in MessageTimeline (`utils/virtual/priors.ts`), keyed by the
      // scroll-pane width + structure signature + expansion signature that make
      // the sizes valid — all component state the store can't see. The store
      // has no `listRef` to call `takeSnapshot()` on anyway. That keyed replay
      // is what lets a re-entry skip the estimate→measure cascade safely; here
      // we cache only the items.
      threadItemCache.set(outgoingThreadId, {
        items,
        oldestLoadedCursor: timelineWindow.oldestLoadedCursor,
        newestLoadedCursor: timelineWindow.newestLoadedCursor,
        oldestLoadedTurnIndex: timelineWindow.oldestLoadedTurnIndex,
        newestLoadedTurnIndex: timelineWindow.newestLoadedTurnIndex,
        hasMoreHistory: timelineWindow.hasMoreHistory,
        hasMoreNewer: timelineWindow.hasMoreNewer,
        latestSettledTurn,
        // Folded subagent children travel with the snapshot: the cached
        // items deliberately exclude evicted rows, so without the fold a
        // warm re-entry would render collapsed cards with zeroed counts
        // until the next live event or hydration.
        subagentFolds: subagentMemory.snapshotFolds(),
      });
    }
    if (sameThreadReswitch) {
      threadItemCache.evict(incomingThreadId);
      // Revert-to-checkpoint mutates this thread's items in place; its
      // measured-size priors are now stale (the structure/content key would
      // also refuse them, but evict to free them promptly — same as the item cache).
      clearThreadSizePriors(incomingThreadId);
    }
    if (outgoingThreadId) {
      // Free Shiki tokens cached against the outgoing thread. The shared
      // cache is partitioned by threadId so this is a clean segmental
      // drop; new lines tokenized for the incoming thread start from a
      // fresh per-thread namespace.
      clearTokensForThread(outgoingThreadId);
    }
  }

  /**
   * Wipe pane-scoped state to the empty/default shape for the incoming
   * thread: transient fields, turn-lifecycle pointers, live-todo state,
   * and checkpoint bookkeeping. Pure mutation of pane state — no cache or
   * outgoing-thread side effects.
   */
  function resetIncomingPaneState(newThread: Thread): void {
    pendingInteractiveState.clear();
    contextWindow = seedContextWindow(newThread);
    providerBanner = undefined;
    generalError = null;
    generalErrorKind = null;
    sendInFlight = false;
    optimisticItemIds.clear();
    channelState.clear();
    designState.reset();
    // Bottom-drawer state is pane-scoped: opening the terminal on thread
    // A should not spill into thread B.
    showTerminal = false;

    // Turn-lifecycle reset. The active-turn registry lives in
    // threadStatuses.svelte.ts and is keyed by threadId, so a thread
    // switch does NOT clear it — a turn that's still in flight on
    // another thread keeps lighting the working indicator when the user
    // comes back. latestSettledTurn is per-pane; rehydrate it from
    // ListRecentTurns OR from the cache when available. Clear first so
    // a rehydration failure leaves the pane in a consistent state.
    latestSettledTurn = null;
    updateEffectiveModel('');
    subagentNotifications = [];

    liveTodoState.resetForThread(newThread.id);
    checkpoints.clearForThread();
    gitStatus.reset();
  }

  function placeholderHasTerminalState(placeholderId: string): boolean {
    return (
      showTerminal ||
      (getExistingThreadTerminalState(placeholderId)?.tabs.length ?? 0) > 0
    );
  }

  function closeDraftPlaceholderTerminals(placeholderId: string): void {
    if (!placeholderHasTerminalState(placeholderId)) return;
    invalidatedDraftTerminalIds.add(placeholderId);
    showTerminal = false;
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
    invalidatedDraftTerminalIds.add(placeholderId);
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
      showTerminal = false;
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

    loading = true;
    if (cached) {
      replaceTimelineItems(cached.items);
      subagentMemory.restoreFolds(cached.subagentFolds);
      subagentMemory.clearHydrationState();
      timelineWindow.installFromSnapshot(cached);
      latestSettledTurn = cached.latestSettledTurn;
    } else {
      replaceTimelineItems([]);
      subagentMemory.resetForFreshThread();
      timelineWindow.resetForFreshThread();
    }
    rowUiState.clear();
    streamingReveal.disposeAll();
    // Reset the live-content stamp so a recent stamp from the OUTGOING
    // thread can't bleed into the incoming one. Without this, switching
    // away from an actively-streaming thread leaves `lastLiveContentAt`
    // recent; the warm gate re-flips within the 500ms hold window, and
    // the incoming (settled) thread's late async-typesetting reflow would
    // read 'spring' off the stale stamp and chase its settled content.
    // A streaming incoming thread re-stamps on its first reveal/delta.
    lastLiveContentAt = 0;
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
    draftPlaceholder = null;
    thread = newThread;
    if (newThread.mode !== 'design') {
      const preview = companionForSource(paneId, 'design-preview');
      if (preview) closeCompanion(preview.paneId);
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
        if (gen !== switchGeneration) return;
        if (switched?.id === newThread.id) {
          const currentContextWindow = contextWindow;
          thread = switched;
          contextWindow = currentContextWindow
            ? normalizeContextWindowForThread(currentContextWindow, switched)
            : seedContextWindow(switched);
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
        await liveStateHydration.hydrateThreadLiveState(
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

    // Single initial slice via `ListThreadSliceAround`. Empty
    // anchor id resolves to the tail at the backend, so this binding
    // covers both bottom-snapshot and saved-anchor restores. Skip on
    // cache hit — the cached items already cover the visible window
    // and the cache is invalidated on `applyItemUpserts` so it's
    // never stale. Older items page in lazily via `pane.loadOlder()`
    // (driven by the auto-load trigger in `MessageTimeline.svelte`
    // and the manual "Load older" button as fallback).
    const loadItemsPromise = cached
      ? Promise.resolve()
      : withGenGuard(
          'load items',
          gen,
          () =>
            ListThreadSliceAround(
              newThread.id,
              sliceAnchorId,
              SLICE_AROUND_ITEM_BUDGET,
            ),
          (paged) => {
            timelineWindow.applyInitialSlice(paged, newThread.id);
            // Only the initial switch load marks this — loadOlder/loadNewer
            // paging never calls applyInitialSlice.
            coldLoadItemsApplied(paneId, (paged.items ?? []).length);
          },
          (err) => {
            // Cache miss + load failure leaves the timeline blank and
            // raises a hard error. (Cache hits skip the load entirely
            // so they can't reach this branch.)
            replaceTimelineItems([]);
            timelineWindow.resetAfterLoadError();
            generalError = `Failed to load thread items: ${errString(err)}`;
            generalErrorKind = null;
            addToast('error', 'Failed to load thread items');
          },
        );

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
            latestSettledTurn = turnRowToSettled(settled);
          }
        }
      },
    );

    const checkpointsPromise = withGenGuard(
      'load checkpoints',
      gen,
      () => refreshCheckpointsForThread(newThread.id),
      () => {},
      (err) => {
        checkpoints.setError(`Failed to load checkpoints: ${errString(err)}`);
      },
    );

    await Promise.allSettled([
      switchPromise,
      liveStatePromise,
      loadItemsPromise,
      recentTurnsPromise,
      checkpointsPromise,
      autoResumePromise,
    ]);
    return { liveStateHydrationConsumed };
  }

  return {
    // --- Getters (reactive reads) ---
    get paneId() {
      return paneId;
    },
    get thread() {
      return thread;
    },
    get threadId() {
      return draftPlaceholder ? null : (thread?.id ?? null);
    },
    get activeModel() {
      return effectiveModel || thread?.model || '';
    },
    get effectiveModel() {
      return effectiveModel;
    },
    get terminalThreadId() {
      return thread?.id ?? null;
    },
    get draftPlaceholder() {
      return draftPlaceholder;
    },
    get hasDraftPlaceholder() {
      return draftPlaceholder !== null;
    },
    get canCompose() {
      return Boolean(thread || draftPlaceholder);
    },
    get items() {
      return items;
    },
    // Imperative read for the scroll controller's content-keyed spring
    // latch. Non-reactive on purpose (see the `lastLiveContentAt`
    // declaration); callers must read it inside an imperative context,
    // not a `$derived`/`$effect`.
    get lastLiveContentAt() {
      return lastLiveContentAt;
    },
    // Stamp a live content advance from a site OUTSIDE the pane's own
    // mutation methods — specifically the live provider-upsert fan-out in
    // events.ts (a new row arriving). The optimistic user-send echo and
    // rollback-restore call `upsertItems` directly and intentionally do
    // NOT route through here, so they stay sync-pinned.
    markLiveContentAdvanced: stampLiveContent,
    setDraftPlaceholderMode(mode: DraftPlaceholderMode): boolean {
      if (!draftPlaceholder || !thread) return false;
      const now = Date.now();
      draftPlaceholder = { ...draftPlaceholder, mode };
      thread = {
        ...thread,
        mode,
        updatedAt: now,
      };
      switchGeneration++;
      return true;
    },
    applyDraftPlaceholderDefaults(defaults: DraftPlaceholderDefaults): boolean {
      if (!draftPlaceholder || !thread) return false;
      const provider = asProviderID(defaults.provider) ?? thread.provider;
      thread = {
        ...thread,
        provider,
        model: defaults.model ?? thread.model,
        reasoningEffort: (defaults.reasoningEffort ??
          thread.reasoningEffort) as Thread['reasoningEffort'],
        fastMode: defaults.fastMode ?? thread.fastMode,
        contextWindow: defaults.contextWindow ?? thread.contextWindow,
        runtimeMode: (defaults.runtimeMode ??
          thread.runtimeMode) as Thread['runtimeMode'],
        updatedAt: Date.now(),
      };
      contextWindow = seedContextWindow(thread);
      switchGeneration++;
      return true;
    },
    applyDraftPlaceholderWorkspace(workspace: {
      workspacePath: string;
      worktreePath?: string;
      branch?: string;
    }): boolean {
      if (!draftPlaceholder || !thread) return false;
      const workspacePath = workspace.workspacePath.trim();
      if (!workspacePath) return false;
      if (!sameNormalizedPath(workspacePath, thread.workspacePath)) {
        closeDraftPlaceholderTerminals(draftPlaceholder.id);
      }
      thread = {
        ...thread,
        workspacePath,
        worktreePath: workspace.worktreePath ?? '',
        branch: workspace.branch ?? thread.branch,
        updatedAt: Date.now(),
      };
      switchGeneration++;
      return true;
    },
    dematerializeEmptyDraftThread(): boolean {
      if (draftPlaceholder || !thread || items.length > 0) return false;
      const current = thread;
      if (current.mode !== 'chat' && current.mode !== 'plan') return false;
      if (!current.projectId || !current.projectPath) return false;
      const now = Date.now();
      const mode = current.mode as DraftPlaceholderMode;
      const placeholder: DraftThreadPlaceholder = {
        id: `draft:${paneId}:${current.projectId}:${mode}:${now}`,
        projectId: current.projectId,
        projectName: '',
        projectPath: current.projectPath,
        mode,
        createdAt: now,
      };
      migrateWorktreeIntent(current.id, placeholder.id);
      draftPlaceholder = placeholder;
      thread = {
        ...current,
        id: placeholder.id,
        title: 'New Thread',
        createdAt: now,
        updatedAt: now,
        isDraft: true,
      };
      removeThread(current.id);
      switchGeneration++;
      return true;
    },
    /**
     * "Locked in" — the user has sent at least one message, so the
     * provider/model selection is committed for this thread. UI
     * affordances that should hide while the thread is still in its
     * pre-send configuration phase (rate-limit rings, model picker
     * disable) read this getter rather than re-deriving from
     * `items.length`.
     */
    get isLocked() {
      return items.length > 0;
    },
    get timelineRevision() {
      return timelineRevision;
    },
    getItemById,
    get pendingApprovals() {
      return pendingInteractiveState.approvals;
    },
    get pendingUserInputs() {
      return pendingInteractiveState.userInputs;
    },
    get contextWindow() {
      return contextWindow;
    },
    get providerBanner() {
      return providerBanner;
    },
    get generalError() {
      return generalError;
    },
    get generalErrorKind() {
      return generalErrorKind;
    },
    get loading() {
      return loading;
    },
    /**
     * Spinner-flash gate. The MessageTimeline reads this instead of
     * `loading` so a sub-100ms switch (cache hit, fast LAN, fast SQL)
     * never shows the spinner — the view transitions straight to the
     * loaded content. Above the threshold the spinner fades in. See
     * `SPINNER_THRESHOLD_MS`.
     */
    get showLoadingSpinner() {
      // Items present is the second half of the gate: a cache hit paints
      // synchronously even while the recent-turns / live-state fetches
      // still run (loading=true), and we must not flash a spinner over
      // visible content. Single source of truth here so call sites
      // stay simple.
      return loading && pastSpinnerThreshold && items.length === 0;
    },
    /**
     * True between the moment the user clicks Send and the moment
     * SendMessage resolves (success or failure). The composer uses
     * this to render the optimistic stop button before
     * `provider:turn_started` lands; the keybindings dispatcher uses
     * it to enable Esc → thread.interrupt during the same window.
     */
    get sendInFlight() {
      return sendInFlight;
    },
    get showTerminal() {
      return showTerminal;
    },
    get checkpoints() {
      return checkpoints;
    },
    get gitStatus() {
      return gitStatus;
    },
    canAdoptOpenedTerminal(
      threadID: string,
      workspacePath: string | undefined,
    ): boolean {
      if (!threadID) return false;
      if (invalidatedDraftTerminalIds.has(threadID)) return false;
      if (draftPlaceholder?.id === threadID) {
        if (!showTerminal || !thread) return false;
        if (
          workspacePath !== undefined &&
          !sameNormalizedPath(workspacePath, thread.workspacePath)
        ) {
          return false;
        }
        return true;
      }
      return thread?.id === threadID;
    },
    refreshCheckpoints: refreshCheckpointsForThread,
    applyCheckpointCaptured(payload: CheckpointCapturedEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      void refreshCheckpointsForThread(payload.threadId);
    },
    applyCheckpointUnavailable(
      payload: CheckpointUnavailableEvent | null,
    ): void {
      if (!payload || payload.threadId !== thread?.id) return;
      checkpoints.markUnavailable(payload.reason);
      checkpoints.setError(
        'Workspace is not a git repo. Checkpoint diffs are unavailable.',
      );
    },
    applyCheckpointError(payload: CheckpointErrorEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      checkpoints.setError(`Checkpoint failed: ${payload.error}`);
    },
    applyCheckpointReverted(payload: CheckpointRevertedEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      void refreshCheckpointsForThread(payload.threadId);
    },
    /**
     * Most recent completed turn, or null if the thread has no settled
     * turns yet. Populated from `provider:turn_completed` pushes and
     * from thread-switch rehydration.
     */
    get latestSettledTurn() {
      return latestSettledTurn;
    },
    /**
     * Bounded recent subagent notifications. No UI consumer today; stored
     * so a future tray / toast surface can subscribe without the pane
     * needing a new channel.
     */
    get subagentNotifications() {
      return subagentNotifications;
    },
    /**
     * Inclusive floor of the loaded history window. Consumers use this
     * to render "Load older messages" and, in scroll-to-item flows, to
     * decide whether a target coordinate is already in view.
     */
    get oldestLoadedCursor() {
      return timelineWindow.oldestLoadedCursor;
    },
    get newestLoadedCursor() {
      return timelineWindow.newestLoadedCursor;
    },
    get oldestLoadedTurnIndex() {
      return timelineWindow.oldestLoadedTurnIndex;
    },
    get newestLoadedTurnIndex() {
      return timelineWindow.newestLoadedTurnIndex;
    },
    get pendingTimelineShiftAtHead() {
      return timelineWindow.pendingTimelineShiftAtHead;
    },
    get hasMoreHistory() {
      return timelineWindow.hasMoreHistory;
    },
    get hasMoreNewer() {
      return timelineWindow.hasMoreNewer;
    },
    get hasDeferredRecentWindowPrune() {
      return timelineWindow.hasDeferredRecentWindowPrune;
    },
    retryDeferredRecentWindowPrune(): void {
      timelineWindow.retryDeferredRecentWindowPrune();
    },
    get loadingOlder() {
      return timelineWindow.loadingOlder;
    },
    get loadingNewer() {
      return timelineWindow.loadingNewer;
    },
    debugMemoryStats() {
      const streamingStats = streamingReveal.debugStats();
      return {
        itemIndexEntries: itemIndexById.size,
        rowUiState: rowUiState.debugStats(),
        itemSmoothers: streamingStats.itemSmoothers,
        liveThinkingTails: streamingStats.liveThinkingTails,
        optimisticItems: optimisticItemIds.size,
        oldestLoadedCursor: timelineWindow.oldestLoadedCursor,
        newestLoadedCursor: timelineWindow.newestLoadedCursor,
      };
    },
    /**
     * Scroll-to-item intent published by pane-level callers (search
     * hits, plan sidebar clicks, tray rows). MessageTimeline reacts to
     * nonce changes — the timeline compares the observed nonce against
     * the current value and runs `scrollToItem(itemId)` when it
     * advances. `itemId === ''` means "no request".
     */
    get scrollToItemRequest() {
      return scrollToItemRequest;
    },
    get channelMessages() {
      return channelState.messages;
    },
    get channelStatus() {
      return channelState.status;
    },
    get channelTurnCount() {
      return channelState.turnCount;
    },
    get channelMaxTurns() {
      return channelState.maxTurns;
    },
    get channelAwaitingResponse() {
      return channelState.awaitingResponse;
    },
    get channelCurrentSpeakerRole() {
      return channelState.currentSpeakerRole;
    },
    get channelParticipants() {
      return channelState.participants;
    },
    get channelLiveTail() {
      return channelState.liveTail;
    },
    /**
     * Non-reactive `performance.now()` stamp of the last live discussion
     * advance (a new channel message, or live-tail growth). Read
     * imperatively by ChannelView's scroll controller `animationMode` —
     * mirrors `pane.lastLiveContentAt`'s chat-surface role. See
     * `threadChannelState.svelte.ts`.
     */
    get channelLastLiveContentAt() {
      return channelState.lastLiveContentAt;
    },
    get pendingClarification() {
      return designState.pendingClarification;
    },
    get activeOptionSet() {
      return designState.activeOptionSet;
    },
    get designViewport() {
      return designState.designViewport;
    },
    get showPlanSidebar() {
      return isCompanionOpen(paneId, 'plan');
    },
    get showReviewPane() {
      return isCompanionOpen(paneId, 'review');
    },
    get showDesignPreviewPanel() {
      return isCompanionOpen(paneId, 'design-preview');
    },
    /**
     * Monotonically increasing counter bumped at the top of every
     * `switchThread`, `clear`, `startDraftPlaceholder`, and
     * `adoptMaterializedDraftThread` call. Exposed so consumers can
     * detect a same-thread re-switch — the path the revert-to-checkpoint
     * flow takes when it calls `switchThread(currentThread)` to reload
     * items in place. `pane.threadId` doesn't change on that path, so
     * any reset logic keyed purely on the thread id (the
     * MessageTimeline restore-effect.pre, in particular) would miss the
     * event and leave stale scroll state (the regression: revert lands
     * at the very top, showing "Load older messages"). Track this
     * alongside `pane.threadId` and run the reset branch when EITHER
     * value changes.
     */
    get switchGeneration() {
      return switchGeneration;
    },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      // Bump the switch generation BEFORE any synchronous mutation so
      // any in-flight prior switch's late resolutions are invalidated
      // before we touch pane state. `gen` is read by every async leg
      // below and by the outer finally to decide whether the spinner
      // can be cleared (a concurrent switch keeps it up).
      const gen = ++switchGeneration;
      if (draftPlaceholder) {
        closeDraftPlaceholderTerminals(draftPlaceholder.id);
      }
      draftPlaceholder = null;
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
        if (gen !== switchGeneration) return;
        loading = false;
      } finally {
        // Defense in depth against an uncaught exception (a synchronous
        // throw between bumping `gen` and runParallelLoad's own gen
        // checks) leaving `loading=true` stranded. Only clear when no
        // newer switch has superseded ours — a concurrent switch is
        // supposed to keep the indicator up.
        if (gen === switchGeneration) {
          loading = false;
        }
        if (liveStateHydrationToken !== 0 && !liveStateHydrationConsumed) {
          finishThreadLiveStateHydration(newThread.id, liveStateHydrationToken);
        }
      }
    },

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
    async refreshFromBackend(): Promise<void> {
      const currentThread = thread;
      if (!currentThread) return;
      const gen = switchGeneration;
      let liveStateHydrationToken = beginThreadLiveStateHydration(
        currentThread.id,
      );
      try {
        try {
          const anchorItemId = timelineWindow.hasMoreNewer
            ? (items.at(-1)?.id ?? '')
            : '';
          const paged = await ListThreadSliceAround(
            currentThread.id,
            anchorItemId,
            ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
          );
          if (gen !== switchGeneration) return;
          const nextItems = reconcileItemWindow(
            itemsForThread((paged.items ?? []) as Item[], currentThread.id),
            items,
          );
          replaceTimelineItems(nextItems, { disposeDropped: true });
          timelineWindow.applyWindowMetadataFromPaged(paged);
        } catch (err) {
          if (gen !== switchGeneration) return;
          console.error('Failed to refresh thread items after gap:', err);
          return;
        }
        try {
          const recent = (await ListRecentTurns(currentThread.id, 2)) as
            | TurnRow[]
            | null;
          if (gen !== switchGeneration) return;
          if (recent && recent.length > 0) {
            const settled = recent.find(
              (row) =>
                row.completedAt !== null && row.completedAt !== undefined,
            );
            if (settled) {
              latestSettledTurn = turnRowToSettled(settled);
            }
          }
        } catch (err) {
          if (gen !== switchGeneration) return;
          console.error('Failed to refresh recent turns after gap:', err);
        }
        pendingInteractiveState.prepareForLiveStateHydration();
        await liveStateHydration.hydrateThreadLiveState(
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
    },

    clear(): void {
      // Any intent staged against the (about-to-be-discarded) placeholder
      // id must die with it — otherwise repeated "+ New" clicks, thread
      // switches, or pane closes leak entries keyed by ids the rest of
      // the app no longer reads. Cleanup is keyed on the placeholder id
      // because real threads keep their entries until the thread itself
      // is removed by the backend.
      if (draftPlaceholder) {
        closeDraftPlaceholderTerminals(draftPlaceholder.id);
        clearWorktreeIntent(draftPlaceholder.id);
      }
      thread = null;
      updateEffectiveModel('');
      draftPlaceholder = null;
      replaceTimelineItems([]);
      subagentMemory.clearFolds();
      rowUiState.clear();
      streamingReveal.disposeAll();
      // Clearing to empty: drop the live-content stamp too (see
      // installCacheOrFreshState — keeps a stale stamp from springing the
      // next thread's settled content). The switchGeneration bump below
      // also cancels any in-flight structural nudge.
      lastLiveContentAt = 0;
      pendingInteractiveState.clear();
      contextWindow = null;
      providerBanner = undefined;
      generalError = null;
      generalErrorKind = null;
      loading = false;
      sendInFlight = false;
      optimisticItemIds.clear();
      showTerminal = false;
      channelState.clear();
      designState.reset();
      // activeTurn lives in the global registry (threadStatuses) and is
      // cleared by projectTurnCompleted; clearing it from a pane.clear()
      // would race with an in-flight turn on the same thread that
      // belongs to a different pane. The pane's getter just stops
      // returning a value once thread is null below.
      latestSettledTurn = null;
      subagentNotifications = [];
      liveTodoState.resetForEmptyPane();
      // Same shape: a switchThread that ran clear() mid-flight could
      // otherwise leave the spinner-threshold timer pending. When it
      // fires it would flip pastSpinnerThreshold true against an empty
      // pane (showLoadingSpinner gates on items.length===0 + loading,
      // both of which clear() leaves false here, so user-visible
      // surface is unaffected — but the leak is real).
      if (spinnerThresholdTimer !== null) {
        clearTimeout(spinnerThresholdTimer);
        spinnerThresholdTimer = null;
      }
      pastSpinnerThreshold = false;
      timelineWindow.resetForFreshThread();
      subagentMemory.clearHydrationState();
      // See switchThread: both `timelineWindow`'s internal
      // `pagingGeneration` and `scrollToItemRequest.nonce` stay
      // monotonic for the pane's lifetime so no consumer observes a
      // regressed counter.
      checkpoints.clearForThread();
      gitStatus.reset();
      // Invalidate any in-flight switchThread so its late resolutions can't
      // repopulate the pane we just cleared.
      switchGeneration++;
    },

    startDraftPlaceholder(
      project: Project,
      mode: DraftPlaceholderMode = 'chat',
      defaults?: DraftPlaceholderDefaults,
    ): void {
      // clear() drops any intent staged against the prior placeholder id,
      // so "+ New" on top of an existing placeholder doesn't leak entries.
      this.clear();
      const now = Date.now();
      const placeholder: DraftThreadPlaceholder = {
        id: `draft:${paneId}:${project.id}:${mode}:${now}`,
        projectId: project.id,
        projectName: project.name,
        projectPath: project.path,
        mode,
        createdAt: now,
      };
      draftPlaceholder = placeholder;
      // Seed defaults mirror what CreateThread would have used. When the
      // caller couldn't fetch them (offline, race, etc.) we still render
      // a usable placeholder — the toolbar pickers fall back to their
      // own resolution paths.
      const seededProvider = asProviderID(defaults?.provider);
      thread = {
        id: placeholder.id,
        title: 'New Thread',
        provider: seededProvider ?? 'codex',
        workspacePath: defaults?.workspacePath || project.path,
        projectPath: project.path,
        projectId: project.id,
        mode,
        model: defaults?.model ?? '',
        reasoningEffort: defaults?.reasoningEffort as Thread['reasoningEffort'],
        fastMode: defaults?.fastMode,
        contextWindow: defaults?.contextWindow,
        runtimeMode: defaults?.runtimeMode as Thread['runtimeMode'],
        branch: defaults?.branch,
        createdAt: now,
        updatedAt: now,
        archived: false,
        // Match the backend projection: a synthetic placeholder has no
        // items, so isDraft is the truth even before the row exists.
        // Any consumer reading pane.thread?.isDraft gets the right
        // answer in both placeholder and materialized phases.
        isDraft: true,
      };
      switchGeneration++;
    },

    async materializeDraftPlaceholder(): Promise<Thread | null> {
      const placeholder = draftPlaceholder;
      if (!placeholder) return thread;
      const current = thread;
      const created = (await CreateThread({
        projectId: placeholder.projectId,
        provider: current?.provider,
        model: current?.model,
        mode: current?.mode ?? placeholder.mode,
        reasoningEffort: current?.reasoningEffort,
        fastMode: current?.fastMode,
        contextWindow: current?.contextWindow,
        runtimeMode: current?.runtimeMode,
        worktreePath: current?.worktreePath,
        workspaceOverride: current?.workspacePath,
        branch: current?.branch,
      })) as Thread;
      return created;
    },

    adoptMaterializedDraftThread(materializedThread: Thread): void {
      if (!draftPlaceholder) return;
      draftPlaceholder = null;
      thread = materializedThread;
      contextWindow = seedContextWindow(materializedThread);
      switchGeneration++;
    },

    /**
     * Materialize a draft placeholder into a real thread row, or return the
     * existing thread id when one is already present. Coalesces concurrent
     * callers so composer-input, paste/upload, and send don't each race
     * to `CreateThread`. Resolves to null when the pane
     * has neither a thread nor a placeholder, or when the placeholder was
     * replaced (e.g. another "+ New" click) before the create resolved —
     * the stale-create guard checks the placeholder id at completion.
     *
     * Side effects on success: seeds the default worktree intent for the
     * new thread, prepends it to the sidebar threads registry, adopts it
     * on the pane, and points the pane's registered composer-draft store
     * at the new thread id (so typed text saved against the placeholder
     * id flushes through to the real thread row).
     */
    async ensureMaterializedThread(): Promise<string | null> {
      const existingId = draftPlaceholder ? null : (thread?.id ?? null);
      if (existingId) return existingId;
      const placeholder = draftPlaceholder;
      if (!placeholder) return null;
      if (materializingThreadPromise) return materializingThreadPromise;
      const placeholderId = placeholder.id;
      materializingThreadPromise = (async () => {
        try {
          const created = await this.materializeDraftPlaceholder();
          if (!created) return null;
          if (draftPlaceholder?.id !== placeholderId) return null;
          await migrateDraftPlaceholderTerminals(placeholderId, created.id);
          // Re-key any intent staged against the placeholder id BEFORE we
          // adopt the real thread. Worktree/branch picks made on the
          // placeholder otherwise become orphaned when lookups switch to
          // the materialized thread id.
          migrateWorktreeIntent(placeholderId, created.id);
          seedDefaultWorktreeIntentForDraft(created);
          prependThread(created);
          this.adoptMaterializedDraftThread(created);
          const draftStore = getComposerDraftForPane(paneId);
          if (draftStore) draftStore.adoptThread(created.id);
          return created.id;
        } catch (err) {
          console.error('Failed to create draft thread:', err);
          this.setGeneralError(`Failed to create thread: ${errString(err)}`);
          return null;
        } finally {
          materializingThreadPromise = null;
        }
      })();
      return materializingThreadPromise;
    },

    /** Fetch the next batch of older turns and prepend them to the window. See threadTimelineWindow.svelte.ts. */
    loadOlder(): Promise<LoadOlderResult> {
      return timelineWindow.loadOlder();
    },

    /** Ensure `itemID` is present in the loaded window. See threadTimelineWindow.svelte.ts. */
    loadUntilItem(itemID: string): Promise<boolean> {
      return timelineWindow.loadUntilItem(itemID);
    },

    /**
     * Hydrate the child transcript under a subagent launch anchor —
     * called by SubagentGroup when an expanded card's loaded children
     * trail its decorated descendant count. Deduped per anchor id;
     * see threadSubagentMemory.ts `hydrateChildren`.
     */
    ensureSubagentChildren(rootItemID: string): Promise<boolean> {
      return subagentMemory.hydrateChildren(rootItemID);
    },

    /** Fetch the next batch of newer turns and append them to the window. See threadTimelineWindow.svelte.ts. */
    loadNewer(): Promise<LoadOlderResult> {
      return timelineWindow.loadNewer();
    },

    /** Reload the tail slice around the thread's most recent item. See threadTimelineWindow.svelte.ts. */
    loadRecentTail(): Promise<boolean> {
      return timelineWindow.loadRecentTail();
    },

    /**
     * Publish a scroll-to-item intent for the MessageTimeline to pick
     * up. Consumers call this instead of reaching into the timeline
     * directly — keeps DOM operations inside the component that owns
     * the scroll container, and lets the pane mediate window loading
     * if the target isn't visible yet. The timeline handler is
     * responsible for awaiting `loadUntilItem` before scrolling.
     */
    requestScrollToItem(
      itemID: string,
      options: ScrollToItemOptions = {},
    ): void {
      if (!itemID) return;
      scrollToItemRequest = {
        itemId: itemID,
        nonce: scrollToItemRequest.nonce + 1,
        flash: options.flash ?? false,
      };
    },

    /**
     * Registered scroll controller for this pane. Read by surfaces that
     * need to suspend auto-follow during a gesture. Call
     * `pause = pane.scrollController?.pauseAutoScroll()`
     * on pointerdown and `pause?.()` on pointerup/cancel — the lease is
     * idempotent so a stray double-release is safe.
     */
    get scrollController(): PaneScrollController | null {
      return scrollController;
    },

    /** MessageTimeline calls this on mount; clears on destroy. */
    attachScrollController(controller: PaneScrollController): void {
      scrollController = controller;
    },

    detachScrollController(controller: PaneScrollController): void {
      // Only clear if the registered controller matches — protects
      // against a stale teardown disposing a freshly remounted pane's
      // controller during fast thread switches.
      if (scrollController === controller) {
        scrollController = null;
      }
    },

    // --- Mutations (called by event router) ---

    addApproval(approval: ApprovalRequest): void {
      pendingInteractiveState.addApproval(approval);
    },

    removeApproval(requestId: string): void {
      pendingInteractiveState.removeApproval(requestId);
    },

    addUserInput(request: UserInputRequest): void {
      pendingInteractiveState.addUserInput(request);
    },

    removeUserInput(requestId: string): void {
      pendingInteractiveState.removeUserInput(requestId);
    },

    /**
     * One-item compatibility wrapper around the batched upsert path.
     * Event routing uses `upsertItems` so bursts of wait rows and payload
     * enrichments hit the timeline in one paint.
     */
    upsertItem(item: Item): boolean {
      return upsertItemsBatch([item]) !== null;
    },

    /**
     * Merge a batch of Items from `provider:item_event` into the timeline.
     * The final state is still the backend-authored transcript, but bursts
     * only allocate/sort/bump revision once.
     */
    upsertItems(incoming: Item[]): boolean {
      return upsertItemsBatch(incoming) !== null;
    },

    /**
     * Provider event fan-out needs the applied changed rows, not just a
     * boolean, so scroll latches are based on visible-window changes after
     * the pane has filtered below-floor history rows.
     */
    applyProviderItemUpserts(
      incoming: Item[],
    ): ApplyItemUpsertsToWindowResult | null {
      const applied = upsertItemsBatch(incoming);
      // A wire append to the loaded tail arms the structural-append
      // spring and its follow-up nudge (see `armStructuralSpring`, which
      // also owns the loading/discussion gates). Turn-state-independent,
      // so appends after turn end (interrupt echo, force-closed tool
      // rows) arm too — an effect keyed on the active turn never saw
      // those and they landed as instant whole-viewport teleports
      // (bug-report-20260702T193212Z). Optimistic-send and
      // rollback-restore rows route through `upsertItems` above,
      // deliberately outside this arm.
      if (applied && applied.appendedItems.length > 0) {
        armStructuralSpring();
      }
      return applied;
    },

    /**
     * Remove a single item from the pane's timeline by id. Returns the
     * removed Item so optimistic callers (revert-on-interrupt) can
     * re-insert it on rollback. Idempotent: returns null when the row
     * is already gone, so a late `user_message:reverted` event after
     * the optimistic remove is a no-op.
     */
    removeItemById(itemId: string): Item | null {
      const idx = itemIndexById.get(itemId);
      if (idx === undefined) return null;
      const removed = items[idx];
      replaceTimelineItems(items.filter((it) => it.id !== itemId));
      streamingReveal.disposeSmootherFor(itemId);
      rowUiState.disposeItems([removed]);
      streamingReveal.recomputeReveal();
      if (thread) {
        threadItemCache.evict(thread.id);
        clearThreadSizePriors(thread.id);
      }
      return removed;
    },

    /**
     * Remove every item with `turnIndex >= fromTurnIndex` from the
     * pane's timeline. Mirrors the backend `DeleteConversationFromTurn`
     * truncate that revert-on-interrupt and explicit revert run under
     * the thread lock — only `user_message:reverted` notifies the user
     * row, so synthetic siblings on the same turn (thinking, api_retry,
     * error, notification, terminal_interaction waits) would otherwise
     * strand in the timeline without backing SQLite rows.
     *
     * Returns the removed items in their previous order so optimistic
     * callers can restore them via `upsertItems` on rollback (the
     * plain-interrupt fallback when the backend predicate disagrees).
     * Idempotent: returns `[]` when no rows match.
     */
    removeItemsFromTurn(fromTurnIndex: number): Item[] {
      if (!Number.isFinite(fromTurnIndex)) return [];
      const removed: Item[] = [];
      const kept: Item[] = [];
      for (const it of items) {
        if (it.turnIndex >= fromTurnIndex) removed.push(it);
        else kept.push(it);
      }
      if (removed.length === 0) return removed;
      replaceTimelineItems(kept);
      for (const r of removed) streamingReveal.disposeSmootherFor(r.id);
      rowUiState.disposeItems(removed);
      streamingReveal.recomputeReveal();
      if (thread) {
        threadItemCache.evict(thread.id);
        clearThreadSizePriors(thread.id);
      }
      return removed;
    },

    /**
     * Test-only synchronous flush of every per-item streaming smoother
     * in this pane. Snaps each active smoother so items[].summary
     * reflects the full received text immediately, then disposes the
     * entry. Used by tests that assert summary content right after
     * applying deltas without waiting for the smoother's rAF schedule.
     * Not part of the production surface.
     */
    __flushItemSmoothersForTest(): void {
      streamingReveal.__flushForTest();
    },

    /**
     * Test-only count of live per-item streaming smoothers. Lets dispose-
     * contract regressions assert directly on the map size for kinds with
     * no other observable (assistant_text has no live-tail accessor). Not
     * part of the production surface.
     */
    __itemSmootherCountForTest(): number {
      return streamingReveal.__smootherCountForTest();
    },

    applyItemDelta(evt: ItemDeltaEvent): void {
      if (!evt.itemId || !evt.delta) return;
      if (thread && evt.threadId !== thread.id) return;
      const index = itemIndexById.get(evt.itemId);
      if (index === undefined) {
        // The wire contract from triage is: the upsert that creates a
        // streaming row ALWAYS precedes any delta for that row
        // (handleTextDelta in internal/triage/stream_items.go inserts
        // on first delta + emits the upsert before the delta event).
        // Hitting this branch means a transport gap, a replay race, or
        // a missed init left us with a delta whose row doesn't exist
        // yet. Log so the regression isn't silent — under the old
        // parallel-slice architecture this case was masked by
        // `liveDeltaChunks` buffering, which we no longer have.
        console.warn('[thread] applyItemDelta: no row for itemId', evt.itemId);
        return;
      }
      const current = items[index];
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
        items[index] = {
          ...current,
          summary: current.summary + evt.delta,
          updatedAt: evt.updatedAt,
        };
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
    },

    applyItemMeta(evt: ItemMetaEvent): void {
      // Re-validated meta blob for an in-flight row. Today's only
      // producer is triage's streaming path-link allowlist: each text
      // flush re-runs the validator and pushes the resulting pathRefs
      // JSON so anchors render mid-stream. The producer dedupes
      // identical merges so by the time this fires the meta is
      // genuinely new.
      if (!evt.itemId) return;
      if (thread && evt.threadId !== thread.id) return;
      const index = itemIndexById.get(evt.itemId);
      if (index === undefined) return;
      const current = items[index];
      if (current.meta === evt.meta) return;
      // Replace the entry rather than mutating in place: ChatMarkdown's
      // $derived path-link extension keys off `item.meta`, so a fresh
      // reference is the reactive signal that re-runs the extension
      // build. updatedAt is preserved — triage's UpdateItemMeta does
      // not bump updated_at, and we don't want this re-render to look
      // like a content change to the size priors / threadItemCache.
      items[index] = { ...current, meta: evt.meta };
    },

    applyItemPatch(evt: ItemPatchEvent): void {
      if (!evt.itemId) return;
      if (thread && evt.threadId !== thread.id) return;
      const index = itemIndexById.get(evt.itemId);
      if (index === undefined) {
        // Patch arrived for a row we no longer track (race after
        // removal). Make sure any orphaned smoother is cleaned up.
        streamingReveal.disposeSmootherFor(evt.itemId);
        return;
      }
      const current = items[index];
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
      const next = { ...items[index] };
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
          stampLiveContent();
        }
      }
      if (evt.patch.meta !== undefined) next.meta = evt.patch.meta;
      if (evt.patch.decision !== undefined) next.decision = evt.patch.decision;
      if (evt.patch.updatedAt !== undefined)
        next.updatedAt = evt.patch.updatedAt;
      if (itemsAreEqual(current, next)) return;
      items[index] = next;
      // Streaming children settle through THIS path, not upserts —
      // triage's doSettleStreamingText/Thinking emit field patches.
      // Without this hook, settled text rows under collapsed cards
      // would stay in pane memory for the rest of the turn.
      subagentMemory.evictSettledChildren([next]);
    },

    // ---- Per-row UI state (survives windowing remount) ----
    expansionStateFor: rowUiState.expansionStateFor,
    retainExpansionStateFor: rowUiState.retainExpansionStateFor,
    expansionStateForPayload: rowUiState.expansionStateForPayload,
    retainExpansionStateForPayload: rowUiState.retainExpansionStateForPayload,
    isSubagentGroupExpanded: rowUiState.isSubagentGroupExpanded,
    /**
     * Expansion toggle with live eviction on collapse: the settled rows
     * of a card the user just closed fold out of pane memory (counts and
     * preview survive via the fold registry; the rows re-hydrate from
     * SQLite on the next expand). Active rows stay — the delta pipeline
     * requires streaming rows to exist in the window.
     */
    toggleSubagentGroupExpanded(groupKey: string): boolean {
      const willExpand = rowUiState.toggleSubagentGroupExpanded(groupKey);
      if (!willExpand) subagentMemory.evictCollapsedSubtree(groupKey);
      return willExpand;
    },
    /** Live fold aggregate for a launch anchor — MessageTimeline threads
     *  this into the grouping pipeline. Reads are revision-driven: every
     *  fold mutation rides a timelineRevision bump. */
    subagentLiveAggregate(anchorId: string): SubagentFoldAggregate | undefined {
      return subagentMemory.aggregate(anchorId);
    },
    diffCardExpandedOverride: rowUiState.diffCardExpandedOverride,
    setDiffCardExpanded: rowUiState.setDiffCardExpanded,
    /** Validity stamp for replaying a measured-size priors snapshot across a
     *  thread switch — see utils/virtual/priors.ts. */
    expansionSignature: rowUiState.expansionSignature,
    attachmentCacheFor: rowUiState.attachmentCacheFor,
    pruneRowUiState: rowUiState.pruneRowUiState,
    // Live smoother-revealed text for a streaming thinking row.
    // Returns null when no smoother is active (settled rows, non-thinking
    // rows, pre-stream cache hits) — callers fall back to `item.summary`.
    // See threadStreamingReveal.svelte.ts `itemLiveThinkingTail` for why
    // ThinkingBlock prefers this over the trimmed-summary sliding window.
    liveThinkingTailForItem(itemId: string): string | null {
      return streamingReveal.liveThinkingTailFor(itemId);
    },

    // Snap every behind smoother straight to its full received text on
    // visibilitychange → visible. See threadStreamingReveal.svelte.ts
    // `snapAllToReceived` for the full rationale.
    snapSmoothersToReceived(): void {
      streamingReveal.snapAllToReceived();
    },

    /**
     * Reveal gate for the timeline. While a turn streams, this is the
     * (turnIndex, itemIndex) of the top-level item currently revealing;
     * MessageTimeline withholds nodes after it via `sliceRevealedNodes` so
     * the next row waits for the current item's reveal to drain. `null`
     * outside live streaming — render everything. See
     * threadStreamingReveal.svelte.ts `recomputeReveal`.
     */
    get revealBoundary(): RevealBoundary | null {
      return streamingReveal.revealBoundary;
    },

    setGeneralError(message: string | null): void {
      generalError = message;
      // Untagged write: any prior session-kind tag is invalidated by a
      // newer message landing in the same slot. The kind tracks the slot's
      // current occupant, not a history.
      generalErrorKind = null;
    },

    setSessionError(message: string): void {
      generalError = message;
      generalErrorKind = 'session';
    },

    clearGeneralError(): void {
      generalError = null;
      generalErrorKind = null;
    },

    /**
     * Clears the banner only if the current message came from a provider
     * session_died event. Called from the `provider:turn_started` handler
     * so a fresh turn auto-dismisses the stale "session died" banner
     * without clobbering orthogonal errors (rename failed, git status,
     * thread load) that happen to be visible in the same slot.
     */
    clearSessionError(): void {
      if (generalErrorKind === 'session') {
        generalError = null;
        generalErrorKind = null;
      }
    },

    setSendInFlight(value: boolean): void {
      sendInFlight = value;
    },

    trackOptimisticItem(id: string): void {
      optimisticItemIds.add(id);
    },

    isOptimisticItem(id: string): boolean {
      return optimisticItemIds.has(id);
    },

    untrackOptimisticItem(id: string): void {
      optimisticItemIds.delete(id);
    },

    setContextWindow(data: ContextWindow): void {
      contextWindow = normalizeContextWindowForThread(data, thread);
    },

    clearContextWindow(): void {
      contextWindow = null;
    },

    setProviderBanner(status: ProviderStatusEvent | null | undefined): void {
      providerBanner = status;
    },

    // --- Turn lifecycle mutations ---

    /**
     * Flip the pane into "turn in flight" on `provider:turn_started`. Safe
     * to call repeatedly — a re-emission (Claude re-init after interrupt)
     * maps back to the same turnId and leaves startedAt as the
     * authoritative first-wall-clock the working indicator anchors on.
     * Idempotent by turnId: a second call with the same id preserves the
     * existing startedAt so the on-screen counter doesn't reset mid-turn.
     */
    /**
     * Pane facade for `provider:turn_started`. Production goes through
     * the wire-push handler in eventsProvider.ts → projectTurnStarted
     * directly; this method is the test-and-explicit-control entry point.
     */
    setActiveTurn(turn: ActiveTurn): void {
      const tid = thread?.id ?? '';
      if (!tid) return;
      projectTurnStarted(tid, turn.turnId, turn.turnIndex, turn.startedAt);
    },

    /**
     * Settle the current turn on `provider:turn_completed`. Writes
     * `latestSettledTurn` for thread-switch rehydration/read state and
     * clears the global active-turn registry via projectTurnCompleted.
     */
    settleTurn(settled: SettledTurn): void {
      const tid = thread?.id ?? '';
      if (tid) {
        projectTurnCompleted(tid, settled.turnId, {
          aborted: settled.aborted,
          errorMessage: settled.errorMessage,
        });
      }
      latestSettledTurn = settled;
      // Any smoother still behind keeps revealing at the normal cadence
      // (adaptive catch-up, PerItemSmoother) — there is deliberately no
      // end-of-turn fast-drain: the rushed reveal read as jank, and a
      // long final message finishing a few seconds after the wire
      // settles is the accepted trade for uniform reveal speed. The
      // reveal sequencer still fast-drains/snaps when a successor row
      // is waiting, so nothing structural is ever blocked behind the
      // tail row's backlog.
      // Run the deferred window prune now that the turn is quiet — the
      // streaming-append path skips it while a turn is active so the
      // head-drop repaint never lands mid-stream.
      if (!timelineWindow.hasMoreNewer) {
        timelineWindow.pruneToRecentWindowIfNeeded();
      }
    },

    /**
     * Optimistic clear used by the Esc / Stop interrupt path. Drops
     * the live turn from the global registry synchronously so the
     * spinner / Stop button flip to idle in the same render tick as
     * the keystroke — matching Claude Code's `resetLoadingState()`
     * (REPL.tsx:2106-2163) and the Codex TUI's spinner clear on
     * `EventMsg::TurnAborted`. The real `provider:turn_completed`
     * arrives shortly after and re-runs the same path (idempotent on
     * already-cleared registry). Does NOT clear `latestSettledTurn`
     * so read-state/trace surfaces keep the previous settled turn.
     */
    clearActiveTurn(): void {
      const tid = thread?.id ?? '';
      if (!tid) return;
      const current = getActiveTurn(tid);
      if (current) {
        projectTurnCompleted(tid, current.turnId, { aborted: true });
      }
    },

    /**
     * Reset both turn-lifecycle slots without rehydrating. Used by
     * the frontend on explicit "clear this pane" paths that aren't a
     * full switchThread — e.g. a user-triggered stop that leaves the
     * pane in a known-quiet state until the next wire push.
     */
    clearTurnState(): void {
      const tid = thread?.id ?? '';
      if (tid) {
        const current = getActiveTurn(tid);
        if (current) {
          projectTurnCompleted(tid, current.turnId, { aborted: true });
        }
      }
      latestSettledTurn = null;
    },

    // --- Live todo (activity rail Todos segment) ---

    get liveTodo() {
      return liveTodoState.liveTodo;
    },
    get liveTodoShowAll() {
      return liveTodoState.liveTodoShowAll;
    },

    /**
     * Replace the live-todo snapshot. Called from the
     * `provider:todo_update` listener for both Claude TodoWrite and
     * Codex update_plan. Empty step arrays clear the panel rather than
     * render an empty state. When every step is `completed`, schedule
     * the auto-hide timer; any subsequent update cancels the pending
     * timer so a late "now there's a new step" snapshot revives the
     * panel cleanly.
     *
     * Open/show-all state is intentionally NOT reset here — those are
     * per-thread user preferences (liveTodoUiPrefs) that should survive
     * the todo list briefly disappearing and reappearing within a thread.
     */
    setLiveTodo(steps: TodoStep[]): void {
      // The provider:todo_update listener (eventsProvider.ts:
      // applyTodoUpdate) is the wire boundary and validates `steps` is
      // an array before calling here; trust the input from that point on.
      // Subtract steps that the previous all-completed cycle already
      // cleared so the agent's full-list re-emission doesn't repaint
      // those rows under a new logical todo cycle.
      liveTodoState.setLiveTodo(steps);
    },

    /**
     * Drop the live-todo snapshot without waiting for the auto-hide
     * timer. Per-thread UI prefs are NOT cleared — the user's "I had
     * this open" preference persists across todo-clear and across
     * thread switches within the same session.
     *
     * Explicit clear also resets the "cleared cycle" set: the user's
     * mental model is "no todos, fresh start", and any subsequent
     * snapshot should be shown verbatim rather than filtered against
     * a prior auto-hide cycle.
     */
    clearLiveTodo(): void {
      liveTodoState.clearLiveTodo();
    },

    /** Toggle the "Show X more…" reveal under the truncated list. */
    toggleLiveTodoShowAll(): void {
      liveTodoState.toggleLiveTodoShowAll(thread?.id ?? null);
    },

    // --- Activity rail (consolidated working/todos/background) ---

    get activityRailTodosOpen() {
      return liveTodoState.activityRailTodosOpen;
    },
    get activityRailBackgroundOpen() {
      return liveTodoState.activityRailBackgroundOpen;
    },
    get activityRailInputCollapsed() {
      return liveTodoState.activityRailInputCollapsed;
    },

    /** Toggle the Todos accordion body inside the activity rail. */
    toggleActivityRailTodos(): void {
      liveTodoState.toggleActivityRailTodos(thread?.id ?? null);
    },

    /** Toggle the Background accordion body inside the activity rail. */
    toggleActivityRailBackground(): void {
      liveTodoState.toggleActivityRailBackground(thread?.id ?? null);
    },

    /**
     * Collapse/expand the pending-user-input popup from the activity-rail
     * chip. Per-thread sticky like the todos/background toggles: the state
     * survives thread switches and is inherited by the next input request
     * in the same thread (the chip stays visible while collapsed, so an
     * inherited-collapsed request is always one click from expanded).
     */
    toggleActivityRailInputCollapsed(): void {
      liveTodoState.toggleActivityRailInputCollapsed(thread?.id ?? null);
    },

    /**
     * Append a subagent notification. No UI consumer today; bounded by
     * subagentNotificationLimit so a misbehaving provider can't grow the
     * array without bound. Oldest entries fall off the front once the
     * cap is exceeded.
     */
    appendSubagentNotification(evt: SubagentNotificationEvent): void {
      const next = subagentNotifications.concat(evt);
      if (next.length > subagentNotificationLimit) {
        subagentNotifications = next.slice(
          next.length - subagentNotificationLimit,
        );
      } else {
        subagentNotifications = next;
      }
    },

    replaceThread(nextThread: Thread): void {
      if (
        thread &&
        (thread.provider !== nextThread.provider || thread.model !== nextThread.model)
      ) {
        updateEffectiveModel('');
      }
      thread = nextThread;
      contextWindow = seedContextWindow(nextThread);
    },

    setEffectiveModel(model: string): void {
      updateEffectiveModel(model);
    },

    applyEffectiveModel(model: string, revision: number): void {
      if (!Number.isSafeInteger(revision) || revision < effectiveModelBackendRevision) return;
      effectiveModelBackendRevision = revision;
      updateEffectiveModel(model);
    },

    setShowTerminal(value: boolean): void {
      // Bottom drawer mount/unmount reflows the chat column. Hold a brief
      // lease on a real visibility change so the controller's content-RO
      // sync-pin no-ops while the column's clientHeight is settling.
      if (value !== showTerminal) leaseDuringSettle(scrollController);
      showTerminal = value;
      // Scope the focus intent to the current open session: if the drawer is
      // hidden before it ever mounted to consume the request (e.g. a rapid
      // open→close), drop it so a later visibility-only reopen — or a
      // thread-restore that mounts the drawer with showTerminal persisted —
      // doesn't inherit a stale "steal focus" intent.
      if (!value) pendingTerminalFocus = false;
    },

    /**
     * Latch intent to move DOM focus into the terminal once its drawer mounts.
     * Called by runTerminalToggle BEFORE setShowTerminal(true) so the flag is
     * already set when the (lazily-loaded) drawer's onMount consumes it,
     * however many frames later the import resolves.
     */
    requestTerminalFocus(): void {
      pendingTerminalFocus = true;
    },

    /**
     * Read-and-clear the terminal focus intent. Returns true at most once per
     * requestTerminalFocus() so a drawer remount (e.g. {#key threadId}) can't
     * re-grab focus the user didn't ask for.
     */
    consumeTerminalFocusRequest(): boolean {
      const requested = pendingTerminalFocus;
      pendingTerminalFocus = false;
      return requested;
    },

    togglePlanSidebar(): void {
      toggleCompanion(paneId, 'plan');
    },

    setShowPlanSidebar(value: boolean): void {
      if (value) openCompanion(paneId, 'plan');
      else {
        const companion = companionForSource(paneId, 'plan');
        if (companion) closeCompanion(companion.paneId);
      }
    },

    toggleReviewPane(): void {
      const companion = companionForSource(paneId, 'review');
      if (companion) {
        closeCompanion(companion.paneId);
        return;
      }
      if (thread?.id) void openReviewCompanion(paneId, thread.id);
    },

    setShowReviewPane(value: boolean): void {
      if (value) {
        if (thread?.id) void openReviewCompanion(paneId, thread.id);
      }
      else {
        const companion = companionForSource(paneId, 'review');
        if (companion) closeCompanion(companion.paneId);
      }
    },

    toggleDesignPreviewPanel(): void {
      if (thread?.mode !== 'design') return;
      toggleCompanion(paneId, 'design-preview');
    },

    setShowDesignPreviewPanel(value: boolean): void {
      if (thread?.mode !== 'design') return;
      if (value) openCompanion(paneId, 'design-preview');
      else {
        const companion = companionForSource(paneId, 'design-preview');
        if (companion) closeCompanion(companion.paneId);
      }
    },

    /** Single-message merge for a live `discussion:message` push, or the
     * message `PostChannelMessage` itself returns on a successful post. */
    applyChannelMessage(message: ChannelMessage): void {
      channelState.applyMessage(message);
    },

    /** Bulk merge for an initial channel load or gap-recovery resync
     * page — see `eventsDiscussion.ts`'s `refreshDiscussionChannel`. */
    applyChannelMessages(messages: ChannelMessage[]): void {
      channelState.applyMessageBatch(messages);
    },

    /** Full deliberation-FSM snapshot apply, shared by the initial load
     * and every `discussion:state` push. */
    applyChannelState(payload: ChannelStatePayload): void {
      channelState.applyState(payload);
    },

    clearChannel(): void {
      channelState.clear();
    },

    // --- Design-mode mutations ---

    /**
     * Set the agent's clarification request. Pass null when the user
     * has answered (the panel sends the answers as a regular user
     * message; it then clears local state by calling this with null).
     */
    setPendingClarification(request: ClarificationRequest | null): void {
      designState.setPendingClarification(request);
    },

    /**
     * Activate (or clear) the side-by-side options grid. `null` returns
     * the pane to the main preview.
     */
    setActiveOptionSet(set: ActiveOptionSet | null): void {
      designState.setActiveOptionSet(set);
    },

    setDesignViewport(viewport: DesignViewport): void {
      designState.setDesignViewport(viewport);
    },

    clearDesign(): void {
      designState.reset();
    },

    /**
     * Hydrate `activeOptionSet` from the per-thread workdir. Called on:
     *
     *  - file watcher events (`design:options-update`) so a fresh set
     *    or new index.html landing in an existing set is reflected
     *    immediately;
     *  - design pane mount so a refresh / app restart re-derives the
     *    picker from disk instead of dropping in-memory state.
     *
     * Backend-side LatestDesignOptionSet is the source of truth: it
     * picks the most recently-touched set under `options/` that has
     * at least one option containing index.html and no `.picked`
     * marker. The watcher's setId hint is informational only — using
     * "latest" instead of "set the watcher named" gives us a uniform
     * model where pick-dismissal (which writes a `.picked` marker)
     * naturally clears the panel on the next refresh.
     *
     * Best-effort: a binding error is logged but not surfaced —
     * failing to hydrate the panel is preferable to dragging a toast
     * onto the user every time a transient mid-write fires the
     * watcher.
     */
    async applyDesignOptionsUpdate(
      threadId: string,
      _setId: string,
    ): Promise<void> {
      await designState.applyDesignOptionsUpdate(() => thread, threadId);
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
