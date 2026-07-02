import { tick } from 'svelte';
import type { Item, ItemKind, Project, Thread } from '../types/models';
import { asProviderID } from '../types/providers';
import type { Checkpoint, DiffPanelTab } from '../types/checkpoint';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
  PendingInteractiveRequests,
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
import type { ChannelMessage } from '../types/discussion';
import type {
  ActiveOptionSet,
  ClarificationRequest,
  DesignViewport,
} from '../types/design';
import {
  CloseThreadTerminals,
  CreateThread,
  GetThreadItem,
  GetThreadLiveState,
  ListPendingInteractiveRequests,
  ListItemsBeforeCursor,
  ListItemsAfterCursor,
  ListRecentTurns,
  ListSubagentDescendants,
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
import { createDiffPanelState, type DiffPanelState } from './diffPanel.svelte';
import { createGitStatusSlot, type GitStatusSlot } from './gitStatus.svelte';
import {
  createRhsPanelSlot,
  type DiffSidebarUIState,
  type RhsPanel,
  type RhsPanelSlot,
} from './rhsPanelSlot.svelte';
import { errString } from '../utils/errors';
import type { RevealBoundary } from '../utils/subagentGrouping';
import {
  isItemActive,
  normalizePreviewText,
  subagentLaunchKind,
} from '../utils/subagentGrouping';
import type { SubagentFoldAggregate } from '../utils/subagentFold';
import { createSubagentFoldRegistry } from '../utils/subagentFold';
import { clearTokensForThread } from '../utils/tokenCacheReactive.svelte';
import {
  MAX_CACHED_SNAPSHOT_ITEMS,
  threadItemCache,
  type ThreadItemSnapshot,
} from './threadItemCache';
import {
  type ApplyItemUpsertsToWindowResult,
  applyItemUpsertsToWindow,
  compareCursors,
  compareItemToCursor,
  compareItemsByTimelinePosition,
  cursorFromItem,
  cursorIsValid,
  itemsAreEqual,
  itemsForThread,
  mergeItemsById,
  mergeMissingItemsById,
  reconcileItemWindow,
  type TimelineCursorLike,
} from './threadItems';
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { clearThreadSizePriors } from '../utils/virtual/priors';
import { ListThreadSliceAround } from './bindings';
import { sameNormalizedPath } from '../utils/path';
import {
  clearThreadTerminalState,
  getExistingThreadTerminalState,
  migrateThreadTerminalState,
} from '../components/terminal/terminalStore.svelte';
import {
  COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  thinkingPayloadVersionForItem,
} from '../utils/payloadVersion';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  getActiveTurn,
  isThreadLiveStateHydrationCurrent,
  projectTurnCompleted,
  projectTurnStarted,
  replaceInteractiveRequestsForThread,
  sameActiveTurn,
  type ActiveTurn,
} from './threadStatuses.svelte';
import {
  getQueueRevisionForThread,
  queueItemFromWire,
  replaceFlushedForThread,
  replaceQueueForThread,
  type FlushedItem,
  type QueueItem as SendQueueItem,
} from './sendQueue.svelte';
import { createLiveTodoState } from './liveTodoState.svelte';
import { createThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import {
  turnRowToSettled,
  type SettledTurn,
  type TurnRow,
} from './threadTurnProjection';
import { createThreadRowUiState } from './threadRowUiState.svelte';
import {
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';
import {
  createThreadChannelState,
  type ThreadChannelStatus,
} from './threadChannelState.svelte';
import { createThreadDesignState } from './threadDesignState.svelte';
import type { ThreadLiveState } from '../../../bindings/agent-overflow/models';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import { SvelteMap } from 'svelte/reactivity';
import {
  END_OF_TURN_DRAIN_MS,
  FAST_DRAIN_SNAP_LAG_CHARS,
  PerItemSmoother,
} from '../markdown/smoothing/PerItemSmoother';
import {
  LOAD_OLDER_ITEM_BUDGET,
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  SLICE_AROUND_ITEM_BUDGET,
  SPINNER_THRESHOLD_MS,
  THINKING_TAIL_RUNES,
  getSmoothingClockForTest,
  isReasoningTailKind,
  isSmoothLiveContentKind,
  loadOlderResult,
  nowForLiveContent,
  sameRhsPanel,
  threadUsesDiscussionSurface,
  trimToTailRunes,
  type DraftThreadPlaceholder,
  type DraftPlaceholderDefaults,
  type DraftPlaceholderMode,
  type LiveStateHydrationGuard,
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

// Diff-sidebar UI types are owned by stores/rhsPanelSlot.svelte.ts.
// Re-exported here so callers that import from this module
// continue to find them at the same path.
export type { DiffSidebarUIState, RhsPanel } from './rhsPanelSlot.svelte';

/**
 * Per-item smoothing handle stored in the pane's `itemSmoothers` map.
 * Holds the PerItemSmoother plus a closure setter that lets
 * `applyItemDelta` push the latest wire `updatedAt` into the
 * smoother's reveal callback without re-creating the closure.
 */
interface ItemSmoothing {
  smoother: PerItemSmoother;
  setLatestUpdatedAt(at: number): void;
}

interface PrunedWindow {
  items: Item[];
  oldestCursor: TimelineCursorLike | null;
  newestCursor: TimelineCursorLike | null;
}

type PrunedWindowApplyResult = 'applied' | 'deferred';
type PrunedWindowVetoPolicy = 'defer' | 'force';

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
  // Per-item streaming smoothers keyed by item id. Created lazily on
  // the first streaming delta for a smoothable row (assistant_text /
  // thinking); disposed on row removal, status snap, or pane clear.
  // Sibling to itemIndexById / rowUiState so all three life-cycle
  // ride the same clear paths.
  const itemSmoothers: Map<string, ItemSmoothing> = new Map();
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
  // Diff panel is per-pane; created once and reset on thread switch so its
  // caches don't leak between threads.
  const diffPanel: DiffPanelState = createDiffPanelState();

  // Live git-status for this pane's workspace. Owns the single gitwatch
  // subscription (driven by ChatHeaderActions via attach); GitActionsControl
  // and the header diff/PR badges read it. Reset on thread switch like
  // diffPanel so a stale count never flashes for the incoming thread.
  const gitStatus: GitStatusSlot = createGitStatusSlot();

  const channelState = createThreadChannelState();
  const designState = createThreadDesignState();

  // Shared right-side panel slot. The shell width and the active panel are
  // saved per thread so plan/diff/payload views swap inside one stable pane
  // instead of mounting separate sidebars with separate width stores.
  const rhsPanelSlot: RhsPanelSlot = createRhsPanelSlot(paneId);

  /**
   * Single source of truth for which RHS panel is open. The store is the
   * durable-for-session thread snapshot; diffPanel.open is kept in sync as a
   * compatibility flag for existing commands/tests.
   *
   * Adding another RHS feature later should mean extending RhsPanel and adding
   * one render branch in the shell, not adding another full-width sidebar.
   */
  function activatePanel(target: RhsPanel | null): void {
    // Right-edge sidebars (plan / diff / diff-payload) reflow the chat
    // column when they open or close. Hold a brief lease so the
    // controller's content-RO sync-pin no-ops while the column's
    // clientWidth is settling — preventing the timeline from yanking
    // mid-transition.
    const current = rhsPanelSlot.activePanel;
    const willChange = !sameRhsPanel(current, target);
    if (!willChange) {
      if (target?.kind !== 'diff-checkpoint' && diffPanel.open) {
        diffPanel.close();
      }
      if (target?.kind === 'diff-checkpoint' && !diffPanel.open) {
        diffPanel.open_();
      }
      return;
    }
    leaseDuringSettle(scrollController, 250);

    if (target?.kind !== 'diff-checkpoint' && diffPanel.open) {
      diffPanel.close();
    }
    if (!target) {
      rhsPanelSlot.closeForThread(thread?.id);
      return;
    }
    rhsPanelSlot.open(target);
    if (target.kind === 'diff-checkpoint') {
      diffPanel.open_();
    }
  }

  // Turn-lifecycle state. The active turn lives in the global registry
  // in threadStatuses.svelte.ts (read directly via `getActiveTurn` at
  // every call site so the source of truth is traceable); the load-
  // bearing benefit is that switching threads no longer clears the
  // working indicator for a turn that's still in flight on the
  // departing thread. `latestSettledTurn` stays per-pane for read-state
  // and trace/debug consumers; on thread switch we rehydrate it from the
  // most recent `ListRecentTurns` row whose `completedAt` is non-null.
  let latestSettledTurn: SettledTurn | null = $state(null);
  const liveTodoState = createLiveTodoState();
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

  /**
   * Windowed-history state. The pane holds a contiguous tail of the
   * thread's items (~50 items on initial load); older history loads
   * on demand via `loadOlder()` or `loadUntilItem()`.
   *
   *  - `oldestLoadedCursor` / `newestLoadedCursor` are the inclusive
   *    item-coordinate bounds of the single contiguous logical window.
   *    The turn-index fields are compatibility projections for tests and
   *    existing consumers; they are not used as memory boundaries.
   *  - `hasMoreHistory` drives the "Load older" button's visibility.
   *  - `hasMoreNewer` drives the bottom "newer messages" gap.
   *  - loading flags disable the matching controls while a fetch is in flight.
   *
   * Upsert events whose item coordinates fall below the window floor
   * are silently dropped — the canonical copy lives in SQLite and will
   * be pulled in the next time the user loads older history. See
   * `upsertItem` below.
   */
  let oldestLoadedCursor: TimelineCursorLike | null = $state(null);
  let newestLoadedCursor: TimelineCursorLike | null = $state(null);
  let oldestLoadedTurnIndex: number | null = $state(null);
  let newestLoadedTurnIndex: number | null = $state(null);
  let hasMoreHistory: boolean = $state(false);
  let hasMoreNewer: boolean = $state(false);
  let recentWindowPrunePending: boolean = $state(false);
  let loadingOlder: boolean = $state(false);
  let loadingNewer: boolean = $state(false);

  /**
   * Direction hint for the virtualizer's `shift` on the NEXT timeline length
   * change: `true` when the change happens at the HEAD (older rows prepended
   * by `loadOlder`, or the head dropped by `loadNewer`'s prune), `false` for
   * tail changes. MessageTimeline binds this to
   * `<TimelineVirtualizer shift={...}>`.
   *
   * Without it the engine treats every length change as tail growth and
   * misindexes its entire size store on a prepend — forcing a re-measure of
   * every visible row (the "scrollbar jumps around" load jank). Set
   * synchronously immediately before the `items` mutation so the engine reads
   * the right value in the same flush, and reset in the paging method's
   * `finally`. Only `loadOlder` / `loadNewer` touch it; the streaming-prune
   * path keeps its own anchor-restore (preserveTimelineWindowAnchor) and
   * leaves this `false`. The prepend/append and the prune are deliberately
   * split across two flushes so a coalesced head-grow + tail-shrink can't
   * collapse into one net length change a single `shift` can't represent.
   */
  let pendingTimelineShiftAtHead: boolean = $state(false);

  /**
   * Separate generation counter for `loadOlder` / `loadUntilItem` so a
   * second click doesn't race with a slow first fetch. `switchGeneration`
   * covers thread swaps; this guards against same-thread concurrent
   * paging fetches (double-click, keyboard repeat).
   */
  let pagingGeneration = 0;

  /**
   * Subagent-children hydration dedupe, keyed by launch anchor item id.
   * `inFlight` stops a re-running expansion effect from double-fetching;
   * `exhausted` marks anchors whose last fetch added nothing new, so a
   * stale decorated descendant count on the anchor's meta can't loop
   * the expansion effect against a backend with nothing more to give.
   * Both reset on thread switch / clear.
   */
  const subagentHydrationInFlight = new Set<string>();
  const subagentHydrationExhausted = new Set<string>();

  /**
   * Live-eviction fold for subagent children (see utils/subagentFold.ts).
   * Terminal child rows leave pane memory once nothing can render them —
   * collapsed inline cards, backgrounded launches, Codex spawns — and
   * their count/preview fold in here so the collapsed card stays honest.
   * SQLite keeps the rows (triage persists before emitting); expansion
   * re-hydrates and `reclaim`s the ids. Every fold mutation rides a
   * `replaceTimelineItems` revision bump, which is what re-runs the
   * grouping derivation that reads these aggregates.
   */
  const subagentFolds = createSubagentFoldRegistry();

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
   * (sidebar resizers, inspector panels, anything that opens a drawer over
   * the chat column) can acquire a `pauseAutoScroll()` lease while a
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
    // Dropped rows can include hydrated subagent children. Their
    // anchors must become hydratable again — a stale exhausted marker
    // would otherwise suppress the next expansion fetch and wedge the
    // card on its loading placeholder. Live-eviction callers know
    // exactly which anchors lost rows and pass them as
    // `exhaustedScope`; unrelated markers survive, because clearing
    // wholesale at eviction cadence re-arms any expanded card whose
    // loaded count persistently trails its total into a refetch per
    // eviction. Bulk window replacements (prune, reconcile, revert)
    // clear wholesale: mapping a dropped grandchild back to its launch
    // root would need an ancestor walk over rows we just dropped, and
    // the cost of breadth is one no-op refetch per re-expanded anchor.
    if (exhaustedScope) {
      for (const anchorId of exhaustedScope) {
        subagentHydrationExhausted.delete(anchorId);
      }
    } else {
      subagentHydrationExhausted.clear();
    }
    for (const item of droppedItems) disposeSmootherFor(item.id);
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
    subagentFolds.retainAnchors((anchorId) => itemIndexById.has(anchorId));
    if (options.disposeDropped) {
      disposeDroppedItemState(previous, items, options.exhaustedScope);
    }
    timelineRevision++;
    return true;
  }

  function cloneCursor(
    cursor: TimelineCursorLike | null | undefined,
  ): TimelineCursorLike | null {
    return cursorIsValid(cursor)
      ? {
          turnIndex: cursor.turnIndex,
          itemIndex: cursor.itemIndex,
          itemId: cursor.itemId ?? '',
        }
      : null;
  }

  function cursorForBinding(cursor: TimelineCursorLike): {
    turnIndex: number;
    itemIndex: number;
    itemId: string;
  } {
    return {
      turnIndex: cursor.turnIndex,
      itemIndex: cursor.itemIndex,
      itemId: cursor.itemId ?? '',
    };
  }

  function oldestCursorFromItems(
    nextItems: readonly Item[],
  ): TimelineCursorLike | null {
    return nextItems.length === 0 ? null : cursorFromItem(nextItems[0]);
  }

  function newestCursorFromItems(
    nextItems: readonly Item[],
  ): TimelineCursorLike | null {
    return nextItems.length === 0
      ? null
      : cursorFromItem(nextItems[nextItems.length - 1]);
  }

  function firstCursorAtTurn(
    nextItems: readonly Item[],
    turnIndex: number,
  ): TimelineCursorLike | null {
    const item = nextItems.find(
      (candidate) => candidate.turnIndex === turnIndex,
    );
    return item ? cursorFromItem(item) : null;
  }

  function lastCursorAtTurn(
    nextItems: readonly Item[],
    turnIndex: number,
  ): TimelineCursorLike | null {
    for (let index = nextItems.length - 1; index >= 0; index -= 1) {
      const item = nextItems[index];
      if (item.turnIndex === turnIndex) return cursorFromItem(item);
    }
    return null;
  }

  function pagedOldestCursor(
    paged: PagedItems,
    fallbackItems: readonly Item[],
  ): TimelineCursorLike | null {
    const explicit = (
      paged as PagedItems & { oldestCursor?: TimelineCursorLike }
    ).oldestCursor;
    const cloned = cloneCursor(explicit);
    if (cloned) return cloned;
    const turnIndex = (paged as PagedItems & { oldestTurnIndex?: number })
      .oldestTurnIndex;
    if (turnIndex !== undefined && turnIndex >= 0) {
      return (
        firstCursorAtTurn(fallbackItems, turnIndex) ?? {
          turnIndex,
          itemIndex: 0,
          itemId: '',
        }
      );
    }
    return oldestCursorFromItems(fallbackItems);
  }

  function pagedNewestCursor(
    paged: PagedItems,
    fallbackItems: readonly Item[],
  ): TimelineCursorLike | null {
    const explicit = (
      paged as PagedItems & { newestCursor?: TimelineCursorLike }
    ).newestCursor;
    const cloned = cloneCursor(explicit);
    if (cloned) return cloned;
    const turnIndex = (paged as PagedItems & { newestTurnIndex?: number })
      .newestTurnIndex;
    if (turnIndex !== undefined && turnIndex >= 0) {
      return (
        lastCursorAtTurn(fallbackItems, turnIndex) ?? {
          turnIndex,
          itemIndex: Number.MAX_SAFE_INTEGER,
          itemId: '',
        }
      );
    }
    return newestCursorFromItems(fallbackItems);
  }

  function pagedHasMoreOlder(paged: PagedItems): boolean {
    return (
      (paged as PagedItems & { hasMoreOlder?: boolean }).hasMoreOlder ??
      paged.hasMore ??
      false
    );
  }

  function pagedHasMoreNewer(paged: PagedItems): boolean {
    return (
      (paged as PagedItems & { hasMoreNewer?: boolean }).hasMoreNewer ?? false
    );
  }

  function setLoadedCursors(
    oldest: TimelineCursorLike | null,
    newest: TimelineCursorLike | null,
  ): void {
    oldestLoadedCursor = cloneCursor(oldest);
    newestLoadedCursor = cloneCursor(newest);
    oldestLoadedTurnIndex = oldestLoadedCursor?.turnIndex ?? null;
    newestLoadedTurnIndex = newestLoadedCursor?.turnIndex ?? null;
  }

  function setLoadedCursorsFromItems(nextItems: readonly Item[]): void {
    setLoadedCursors(
      oldestCursorFromItems(nextItems),
      newestCursorFromItems(nextItems),
    );
  }

  function applyWindowMetadataFromPaged(
    paged: PagedItems,
    nextItems: readonly Item[],
  ): void {
    setLoadedCursors(
      pagedOldestCursor(paged, nextItems),
      pagedNewestCursor(paged, nextItems),
    );
    hasMoreHistory = pagedHasMoreOlder(paged);
    hasMoreNewer = pagedHasMoreNewer(paged);
  }

  function includeAncestorClosure(
    keepIds: Set<string>,
    sourceItems: readonly Item[],
  ): void {
    const byId = new Map(sourceItems.map((item) => [item.id, item]));
    let changed = true;
    while (changed) {
      changed = false;
      for (const item of sourceItems) {
        if (!keepIds.has(item.id)) continue;
        if (!item.parentId || keepIds.has(item.parentId)) continue;
        if (!byId.has(item.parentId)) continue;
        keepIds.add(item.parentId);
        changed = true;
      }
    }
  }

  function keepRecentWindowItems(
    sourceItems: readonly Item[],
    targetCount: number,
  ): PrunedWindow {
    if (sourceItems.length <= targetCount) {
      return {
        items: sourceItems as Item[],
        oldestCursor: oldestCursorFromItems(sourceItems),
        newestCursor: newestCursorFromItems(sourceItems),
      };
    }
    const cutoffIndex = Math.max(0, sourceItems.length - targetCount);
    const cutoffItem = sourceItems[cutoffIndex] ?? sourceItems[0];
    const cutoffCursor = cursorFromItem(cutoffItem);
    const keepIds = new Set(
      sourceItems
        .filter((item) => compareItemToCursor(item, cutoffCursor) >= 0)
        .map((item) => item.id),
    );
    includeAncestorClosure(keepIds, sourceItems);
    return {
      items: sourceItems.filter((item) => keepIds.has(item.id)),
      oldestCursor: cutoffCursor,
      newestCursor: newestCursorFromItems(sourceItems),
    };
  }

  function keepHeadWindowItems(
    sourceItems: readonly Item[],
    targetCount: number,
  ): PrunedWindow {
    if (sourceItems.length <= targetCount) {
      return {
        items: sourceItems as Item[],
        oldestCursor: oldestCursorFromItems(sourceItems),
        newestCursor: newestCursorFromItems(sourceItems),
      };
    }
    const cutoffItem =
      sourceItems[Math.min(sourceItems.length - 1, targetCount - 1)];
    const cutoffCursor = cursorFromItem(cutoffItem);
    const keepIds = new Set(
      sourceItems
        .filter((item) => compareItemToCursor(item, cutoffCursor) <= 0)
        .map((item) => item.id),
    );
    includeAncestorClosure(keepIds, sourceItems);
    return {
      items: sourceItems.filter((item) => keepIds.has(item.id)),
      oldestCursor: oldestCursorFromItems(sourceItems),
      newestCursor: cutoffCursor,
    };
  }

  function pruneToRecentWindowIfNeeded(
    options: {
      hasMoreNewerAfterPrune?: boolean;
      /**
       * 'shift' is used by `loadNewer` (a paging op): the head-drop holds
       * position via the virtualizer's `shift` head-splice. 'preserve' (default) is the
       * streaming/settle path, which keeps the explicit anchor-restore
       * transaction (preserveTimelineWindowAnchor) and its active-turn defer.
       */
      positionMode?: 'shift' | 'preserve';
    } = {},
  ): void {
    if (items.length <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const activeTurn = thread !== null ? getActiveTurn(thread.id) : null;
    const exceedsHardCeiling =
      items.length > ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS;
    // A head-drop on a visible, bottom-pinned timeline repaints the whole
    // viewport: the content height collapses by the dropped rows, the
    // browser clamps scrollTop, and the virtualizer re-measures — seen as a blank
    // flash mid-stream (incident 2026-06-10). Defer the prune to turn
    // settle (settleTurn calls back in here), holding the hard ceiling
    // as the memory backstop against a runaway turn.
    if (
      !exceedsHardCeiling
      && activeTurn
    ) {
      return;
    }
    if (recentWindowPrunePending && !exceedsHardCeiling) return;
    const next = keepRecentWindowItems(
      items,
      ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
    );
    // loadNewer paging: the dropped head sits above the viewport, so the
    // virtualizer's `shift` head-splice holds the reading position — no
    // anchor transaction, no veto.
    if (options.positionMode === 'shift') {
      applyPagedPrune(next, {
        shiftAtHead: true,
        hasMoreHistoryAfterPrune: true,
        hasMoreNewerAfterPrune: options.hasMoreNewerAfterPrune ?? false,
      });
      recentWindowPrunePending = false;
      return;
    }
    const vetoPolicy = exceedsHardCeiling ? 'force' : 'defer';
    const result = applyPrunedWindow(next, {
      hasMoreHistoryAfterPrune: true,
      hasMoreNewerAfterPrune: options.hasMoreNewerAfterPrune ?? false,
      vetoPolicy,
    });
    recentWindowPrunePending = result === 'deferred';
  }

  function pruneToHeadWindowIfNeeded(): void {
    if (items.length <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const next = keepHeadWindowItems(
      items,
      ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
    );
    // loadOlder paging: the dropped tail sits below the viewport (tail change,
    // no shift, no jump), so the virtualizer leaves the reading position alone.
    applyPagedPrune(next, { shiftAtHead: false, hasMoreNewerAfterPrune: true });
  }

  // Shared window swap used by both prune paths: replace items + cursors +
  // history flags, then recompute reveal. Funnelling the mutation through one
  // place keeps the paged and preserve paths from drifting.
  function commitWindow(
    next: PrunedWindow,
    flags: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    replaceTimelineItems(next.items, { disposeDropped: true });
    setLoadedCursors(next.oldestCursor, next.newestCursor);
    if (flags.hasMoreHistoryAfterPrune !== undefined) {
      hasMoreHistory = flags.hasMoreHistoryAfterPrune;
    }
    if (flags.hasMoreNewerAfterPrune !== undefined) {
      hasMoreNewer = flags.hasMoreNewerAfterPrune;
    }
    recomputeReveal();
  }

  // Paging prune (loadOlder tail-drop / loadNewer head-drop). The dropped end
  // is always opposite the reading viewport, so there is nothing to veto and
  // no anchor to restore — the virtualizer's `shift` head-splice holds
  // position. Set the shift direction at the mutation point so the engine
  // reads it in the same flush as this length change (head-drop → splice the
  // size store from the front; tail-drop → no shift).
  function applyPagedPrune(
    next: PrunedWindow,
    options: {
      shiftAtHead: boolean;
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    if (next.items.length === items.length) return;
    pendingTimelineShiftAtHead = options.shiftAtHead;
    commitWindow(next, options);
  }

  // Streaming / settle prune. Holds position via the explicit anchor
  // transaction (preserveTimelineWindowAnchor) because it can fire under a
  // bottom-pinned, mid-turn viewport, and it can be vetoed/deferred when the
  // prune would drop the visible anchor (vetoPolicy). Leaves the shift flag
  // false — MessageTimeline owns the rendered-node head-shift hint because
  // the virtualizer receives grouped/revealed nodes, not raw pane items.
  function applyPrunedWindow(
    next: PrunedWindow,
    options: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
      vetoPolicy: PrunedWindowVetoPolicy;
    },
  ): PrunedWindowApplyResult {
    if (next.items.length === items.length) return 'applied';
    let operationApplied = false;
    const apply = (): void => {
      if (operationApplied) return;
      operationApplied = true;
      commitWindow(next, options);
    };
    const preserve = scrollController?.preserveTimelineWindowAnchor;
    if (!preserve) {
      apply();
      return 'applied';
    }
    const keptItemIds = new Set(next.items.map((item) => item.id));
    preserve({
      keepsItem: (itemId) => keptItemIds.has(itemId),
      run: apply,
    });
    if (operationApplied) return 'applied';
    if (options.vetoPolicy === 'defer') return 'deferred';
    apply();
    return 'applied';
  }

  /**
   * Eviction policy for one upserted or patched row. Returns the launch
   * anchor to fold the row under, or null when the row must stay in
   * pane memory: still active (the delta pipeline requires streaming
   * rows to exist), itself a launch anchor (anchors are the fold keys
   * and the cards), a flat non-subagent row, an orphan, or a child of
   * an inline card that is currently expanded. Retention is keyed on
   * the direct parent's expansion only; settled rows under collapsed
   * ancestors are swept by evictCollapsedSubagentSubtree when their own
   * card collapses.
   */
  function evictableAnchorIdFor(item: Item): string | null {
    if (isItemActive(item)) return null;
    const parentId = item.parentId ?? '';
    if (!parentId) return null;
    if (subagentLaunchKind(item) !== null) return null;
    const parentIndex = itemIndexById.get(parentId);
    if (parentIndex === undefined) return null;
    const parent = items[parentIndex];
    const launchKind = subagentLaunchKind(parent);
    if (launchKind === null) return null;
    if (launchKind === 'inline' && rowUiState.isSubagentGroupExpanded(parent.id)) {
      return null;
    }
    return parent.id;
  }

  /** One row leaving pane memory for the fold, keyed by its launch anchor. */
  interface SubagentEviction {
    item: Item;
    anchorId: string;
  }

  /**
   * Collect every settled non-launch descendant under `anchorId` into
   * `out`. Nested launches stay loaded (they are fold keys and render
   * as nested cards); their settled children fold under their own
   * anchor so nested entry counters stay honest. One forward pass
   * suffices because items are in (turnIndex, itemIndex) order — a
   * launch precedes its rows.
   */
  function collectSettledSubtree(anchorId: string, out: SubagentEviction[]): void {
    const launchIds = new Set([anchorId]);
    for (const item of items) {
      const parentId = item.parentId ?? '';
      if (!parentId || !launchIds.has(parentId)) continue;
      if (subagentLaunchKind(item) !== null) {
        launchIds.add(item.id);
        continue;
      }
      if (isItemActive(item)) continue;
      out.push({ item, anchorId: parentId });
    }
  }

  /**
   * Commit evictions: record each row in the fold registry, then drop
   * the rows through replaceTimelineItems with disposal so smoothers
   * and row UI state are cleaned like any other dropped row, and
   * recompute the reveal gate. Exhausted-hydration markers clear only
   * for the anchors whose transcripts changed — see
   * disposeDroppedItemState. Duplicate entries are harmless: the
   * registry and the drop set both dedupe by id.
   */
  function commitSubagentEvictions(evictions: readonly SubagentEviction[]): void {
    if (evictions.length === 0) return;
    const evictedIds = new Set<string>();
    const anchorIds = new Set<string>();
    for (const { item, anchorId } of evictions) {
      subagentFolds.recordEvicted(
        anchorId,
        item,
        normalizePreviewText(item.summary ?? ''),
      );
      evictedIds.add(item.id);
      anchorIds.add(anchorId);
    }
    replaceTimelineItems(
      items.filter((it) => !evictedIds.has(it.id)),
      { disposeDropped: true, exhaustedScope: anchorIds },
    );
    recomputeReveal();
  }

  /**
   * Fold-and-drop settled subagent children that nothing can render.
   * `candidates` is the changed-row set of the upsert batch or status
   * patch that just applied: children that arrived terminal, children
   * whose stored row just flipped terminal, and — when a launch anchor
   * itself changed — a sweep of its settled subtree (covers a
   * foreground launch being backgrounded mid-run, which flips its
   * whole transcript from expandable to suppressed).
   */
  function evictSettledSubagentChildren(candidates: readonly Item[]): void {
    let evictions: SubagentEviction[] | null = null;
    for (const candidate of candidates) {
      if (subagentLaunchKind(candidate) === 'suppressed') {
        collectSettledSubtree(candidate.id, (evictions ??= []));
        continue;
      }
      const anchorId = evictableAnchorIdFor(candidate);
      if (anchorId === null) continue;
      (evictions ??= []).push({ item: candidate, anchorId });
    }
    if (evictions) commitSubagentEvictions(evictions);
  }

  /**
   * Collapse-time eviction: fold every settled descendant under
   * `anchorId` out of pane memory (counts and preview survive in the
   * fold registry; rows re-hydrate from SQLite on the next expand).
   */
  function evictCollapsedSubagentSubtree(anchorId: string): void {
    const anchorIndex = itemIndexById.get(anchorId);
    if (anchorIndex === undefined) return;
    if (subagentLaunchKind(items[anchorIndex]) === null) return;
    const evictions: SubagentEviction[] = [];
    collectSettledSubtree(anchorId, evictions);
    commitSubagentEvictions(evictions);
  }
  // Reveal-gate invariant: any wholesale `items` replacement that can
  // change which top-level rows exist relative to a live smoother must
  // pair with `recomputeReveal()` (or `disposeAllSmoothers()`, which
  // clears the boundary). Current callers all hold this: switchThread /
  // clear → disposeAllSmoothers; removeItem / removeItemsForTurns →
  // recomputeReveal. The `loadOlder` merge is the deliberate exception —
  // it only prepends OLDER rows (before any streaming frontier by
  // (turnIndex, itemIndex)), which can be neither the frontier nor a
  // gated successor, so the boundary is unaffected and no recompute is
  // needed. A new mutation path that can append rows during a turn MUST
  // call recomputeReveal — there is no reactive backstop (a parallel
  // $effect over the timeline is forbidden; see frontend/AGENTS.md).

  function disposeSmootherFor(itemId: string): void {
    const entry = itemSmoothers.get(itemId);
    if (!entry) return;
    entry.smoother.dispose();
    itemSmoothers.delete(itemId);
    itemLiveThinkingTail.delete(itemId);
  }

  function disposeAllSmoothers(): void {
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
   *     waiting, so the next row appears within ~200ms instead of stalling
   *     behind a long (often collapsed) thinking block.
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
   * item-removal paths; `disposeAllSmoothers` clears the boundary directly.
   *
   * Reentrancy: the oversized-backlog snap below fires `onReveal`
   * synchronously, whose catch-up branch calls back into this function.
   * The guard collapses the nested call into a re-run after the current
   * pass — without it, the outer pass would overwrite the boundary and
   * pause/resume decisions the nested pass just computed from fresher
   * state.
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
    let frontier: Item | null = null;
    for (const [id, entry] of itemSmoothers) {
      const item = getItemById(id);
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
      const frontierIdx = itemIndexById.get(f.id) ?? -1;
      for (let i = frontierIdx + 1; i < items.length; i++) {
        if (!items[i].parentId) {
          hasSuccessor = true;
          break;
        }
      }
      for (const [id, entry] of itemSmoothers) {
        const item = getItemById(id);
        if (!item || item.parentId) continue;
        // Withheld successors pause; the frontier (and any earlier top-level
        // smoother, though none should outrank it) resumes.
        if (compareItemsByTimelinePosition(item, f) > 0) entry.smoother.pause();
        else entry.smoother.resume();
      }
      if (hasSuccessor) {
        const frontierSmoother = itemSmoothers.get(f.id)?.smoother;
        // A backlog too large to rush through at the drain cap would
        // hold the waiting successor for whole seconds — snap it in one
        // deliberate burst instead. Below the threshold, drain at the
        // elevated (finite) per-tick cap so the finish reads as motion
        // without a single-frame mega re-parse.
        if (frontierSmoother && frontierSmoother.getLag() > FAST_DRAIN_SNAP_LAG_CHARS) {
          frontierSmoother.snap();
        } else {
          frontierSmoother?.requestFastDrain();
        }
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
        armStructuralSpring();
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
      clock: getSmoothingClockForTest(),
      onReveal: (revealed, delta) => {
        const idx = itemIndexById.get(itemId);
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
        stampLiveContent();
        const current = items[idx];
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
        items[idx] = nextItem;
        if (isReasoningTail) {
          itemLiveThinkingTail.set(itemId, revealed);
          rowUiState.appendLivePayloadDeltaForItem(
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
    if (incoming.some((it) => subagentFolds.isEvicted(it.id))) {
      incoming = incoming.filter((it) => !subagentFolds.isEvicted(it.id));
      if (incoming.length === 0) return null;
    }

    const next = applyItemUpsertsToWindow({
      current: items,
      incoming,
      itemIndexById,
      currentThreadId: thread?.id ?? null,
      oldestLoadedCursor,
      newestLoadedCursor,
      oldestLoadedTurnIndex,
      newestLoadedTurnIndex,
      hasMoreHistory,
      hasMoreNewer,
    });
    if (!next) return null;
    if (next.droppedNewerItems) {
      hasMoreNewer = true;
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
      if (thread && !hasMoreHistory) {
        oldestLoadedCursor = oldestCursorFromItems(items);
        oldestLoadedTurnIndex = oldestLoadedCursor?.turnIndex ?? null;
      }
      if (thread) {
        newestLoadedCursor = newestCursorFromItems(items);
        newestLoadedTurnIndex = newestLoadedCursor?.turnIndex ?? null;
      }
    }
    // Live eviction runs before the window-cap check so settled subagent
    // children never count toward the prune trigger — the cap effectively
    // bounds renderable rows, matching the backend pagers' top-level-only
    // budget since 6187d039.
    evictSettledSubagentChildren(next.changedItems);
    if (next.appendedItems.length > 0 && !hasMoreNewer) {
      pruneToRecentWindowIfNeeded();
    }

    // Reconcile per-item smoothers with the upsert state. A completion /
    // failure upsert replaces items[index] entirely, so a still-running
    // smoother would write stale partial reveals back over the new
    // summary on its next tick. Dispose on any terminal-status upsert.
    // For streaming upserts whose summary extends what the smoother has
    // already received, append the suffix so the smoother continues
    // toward the new target; on a non-extending mismatch, dispose so
    // the next delta seeds a fresh smoother from the new summary.
    if (itemSmoothers.size > 0) {
      for (const it of next.changedItems) {
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
    recomputeReveal();
    return next;
  }

  function applyPendingInteractiveSnapshot(
    threadID: string,
    snapshot: PendingInteractiveRequests | null | undefined,
  ): void {
    const registrySnapshot =
      pendingInteractiveState.registrySnapshotFor(snapshot);
    pendingInteractiveState.applySnapshot(snapshot);
    replaceInteractiveRequestsForThread(threadID, registrySnapshot);
  }

  async function hydratePendingInteractiveRequests(
    threadID: string,
    gen: number,
    hydrationToken?: number,
  ): Promise<void> {
    let snapshot: PendingInteractiveRequests;
    try {
      snapshot = (await ListPendingInteractiveRequests(
        threadID,
      )) as PendingInteractiveRequests;
    } catch (err) {
      if (gen === switchGeneration && thread?.id === threadID) {
        console.error('Failed to hydrate pending interactive requests:', err);
      }
      return;
    }
    if (gen !== switchGeneration || thread?.id !== threadID) return;
    if (
      hydrationToken !== undefined &&
      !isThreadLiveStateHydrationCurrent(threadID, hydrationToken)
    )
      return;

    applyPendingInteractiveSnapshot(threadID, snapshot);
  }

  function applyThreadLiveStateSnapshot(
    snapshot: ThreadLiveState,
    threadID: string,
    guard: LiveStateHydrationGuard,
  ): void {
    if (snapshot.threadId !== threadID) return;
    const current = getActiveTurn(threadID);
    if (sameActiveTurn(current, guard.activeTurnAtRequest)) {
      const active = snapshot.activeTurn;
      if (active && active.threadId === threadID && active.turnId) {
        projectTurnStarted(
          threadID,
          active.turnId,
          active.turnIndex,
          active.startedAt,
        );
      } else if (current) {
        projectTurnCompleted(threadID, current.turnId);
      }
    }

    if (getQueueRevisionForThread(threadID) === guard.queueRevisionAtRequest) {
      const queueItems: SendQueueItem[] = (snapshot.queueItems ?? [])
        .filter((item) => item.threadId === threadID)
        .map(queueItemFromWire);
      replaceQueueForThread(threadID, queueItems);
      const flushedItems: FlushedItem[] = (snapshot.flushedItems ?? [])
        .filter((item) => item.userItemId && item.queueItemId)
        .map((item) => ({
          queueItemId: item.queueItemId,
          userItemId: item.userItemId,
          message: item.message,
          flushedAt: Date.now(),
        }));
      replaceFlushedForThread(threadID, flushedItems);
    }

    applyPendingInteractiveSnapshot(
      threadID,
      snapshot.interactive as PendingInteractiveRequests,
    );

    liveTodoState.hydrateSnapshotIfUnchanged(
      snapshot.todo,
      threadID,
      guard.liveTodoRevisionAtRequest,
    );
  }

  async function hydrateThreadLiveState(
    threadID: string,
    gen: number,
    existingHydrationToken?: number,
  ): Promise<void> {
    const hydrationToken =
      existingHydrationToken ?? beginThreadLiveStateHydration(threadID);
    const guard: LiveStateHydrationGuard = {
      activeTurnAtRequest: getActiveTurn(threadID),
      queueRevisionAtRequest: getQueueRevisionForThread(threadID),
      liveTodoRevisionAtRequest: liveTodoState.revision,
    };
    try {
      let snapshot: ThreadLiveState;
      try {
        snapshot = (await GetThreadLiveState(threadID)) as ThreadLiveState;
      } catch (err) {
        if (gen === switchGeneration && thread?.id === threadID) {
          console.error('Failed to hydrate thread live state:', err);
        }
        await hydratePendingInteractiveRequests(threadID, gen, hydrationToken);
        return;
      }
      if (gen !== switchGeneration || thread?.id !== threadID) return;
      if (!isThreadLiveStateHydrationCurrent(threadID, hydrationToken)) return;
      applyThreadLiveStateSnapshot(snapshot, threadID, guard);
    } finally {
      finishThreadLiveStateHydration(threadID, hydrationToken);
    }
  }

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

  /**
   * Apply a paged-load result to pane state. Used by `switchThread`'s
   * single initial load. Items merge additively — anything already
   * present (from cache or streamed events that landed mid-load)
   * keeps its current reference; missing rows are added and the
   * array is re-sorted by (turnIndex, itemIndex). Cursors
   * (`oldestLoadedTurnIndex` / `hasMoreHistory`) are taken straight
   * from the load — there is no second phase whose wider window
   * would need to be preserved.
   */
  function applyPagedItems(paged: PagedItems, threadID: string): void {
    const incoming = itemsForThread((paged.items ?? []) as Item[], threadID);
    const nextItems = mergeMissingItemsById(incoming, items);
    replaceTimelineItems(nextItems, { disposeDropped: true });
    applyWindowMetadataFromPaged(paged, items);
  }

  /**
   * Hydrate the child transcript under a subagent launch anchor.
   * History windows deliver only top-level rows — the collapsed
   * SubagentGroup card renders from backend-decorated aggregates, and
   * this loads the actual rows when the card expands (or when a
   * scroll-to-item target lives inside the subtree).
   *
   * Additive merge only: rows already in memory (live-streamed
   * children) keep their references, missing rows are inserted at
   * their (turnIndex, itemIndex) position. Child rows are never
   * top-level, so the reveal boundary is unaffected — same exception
   * as `loadOlder` (see the reveal-gate invariant note above).
   *
   * Returns true when new rows were merged in.
   */
  async function hydrateSubagentChildren(rootItemID: string): Promise<boolean> {
    const currentThread = thread;
    if (!currentThread || !rootItemID) return false;
    if (
      subagentHydrationInFlight.has(rootItemID) ||
      subagentHydrationExhausted.has(rootItemID)
    ) {
      return false;
    }

    const gen = switchGeneration;
    subagentHydrationInFlight.add(rootItemID);
    try {
      const children = (await ListSubagentDescendants(
        currentThread.id,
        rootItemID,
      )) as Item[];
      if (gen !== switchGeneration) return false;
      const incoming = itemsForThread(children ?? [], currentThread.id);
      // Rows coming back into memory leave the live-eviction fold first —
      // the invariant is an id is folded XOR loaded, so the card's count
      // (loaded + folded) stays exact through the hydration round-trip.
      subagentFolds.reclaim(incoming.map((child) => child.id));
      const next = mergeMissingItemsById(incoming, items);
      if (next === items) {
        subagentHydrationExhausted.add(rootItemID);
        return false;
      }
      replaceTimelineItems(next);
      return true;
    } catch (err) {
      if (gen !== switchGeneration) return false;
      console.error('hydrateSubagentChildren failed:', err);
      addToast('error', 'Failed to load subagent activity');
      return false;
    } finally {
      subagentHydrationInFlight.delete(rootItemID);
    }
  }

  async function refreshCheckpointsForThread(threadID: string): Promise<void> {
    const checkpoints = ((await ListThreadCheckpoints(threadID)) ??
      []) as Checkpoint[];
    if (thread?.id !== threadID) return;
    const sorted = [...checkpoints].sort((a, b) => a.turnIndex - b.turnIndex);
    diffPanel.setCheckpoints(sorted);
  }

  /**
   * Snapshot the outgoing thread into the LRU cache (when worth it),
   * the RHS panel slot, and the partitioned shiki token cache.
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
        oldestLoadedCursor,
        newestLoadedCursor,
        oldestLoadedTurnIndex,
        newestLoadedTurnIndex,
        hasMoreHistory,
        hasMoreNewer,
        latestSettledTurn,
        // Folded subagent children travel with the snapshot: the cached
        // items deliberately exclude evicted rows, so without the fold a
        // warm re-entry would render collapsed cards with zeroed counts
        // until the next live event or hydration.
        subagentFolds: subagentFolds.snapshot(),
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
      rhsPanelSlot.snapshotForThread(outgoingThreadId);
      // Free Shiki tokens cached against the outgoing thread. The shared
      // cache is partitioned by threadId so this is a clean segmental
      // drop; new lines tokenized for the incoming thread start from a
      // fresh per-thread namespace.
      clearTokensForThread(outgoingThreadId);
    } else {
      rhsPanelSlot.closeForThread();
    }
  }

  /**
   * Wipe pane-scoped state to the empty/default shape for the incoming
   * thread: transient fields, turn-lifecycle pointers, live-todo state,
   * and the diff panel. Pure mutation of pane state — no cache or
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
    // A should not spill into thread B. The RHS sidebar is different:
    // its active panel + width are snapshotted per thread by
    // snapshotOutgoingPane.
    showTerminal = false;

    // Turn-lifecycle reset. The active-turn registry lives in
    // threadStatuses.svelte.ts and is keyed by threadId, so a thread
    // switch does NOT clear it — a turn that's still in flight on
    // another thread keeps lighting the working indicator when the user
    // comes back. latestSettledTurn is per-pane; rehydrate it from
    // ListRecentTurns OR from the cache when available. Clear first so
    // a rehydration failure leaves the pane in a consistent state.
    latestSettledTurn = null;
    subagentNotifications = [];

    liveTodoState.resetForThread(newThread.id);
    diffPanel.clearForThread();
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
      subagentFolds.restore(cached.subagentFolds);
      setLoadedCursors(
        cached.oldestLoadedCursor ?? oldestCursorFromItems(cached.items),
        cached.newestLoadedCursor ?? newestCursorFromItems(cached.items),
      );
      if (!oldestLoadedCursor && cached.oldestLoadedTurnIndex != null) {
        oldestLoadedTurnIndex = cached.oldestLoadedTurnIndex;
      }
      if (!newestLoadedCursor && cached.newestLoadedTurnIndex != null) {
        newestLoadedTurnIndex = cached.newestLoadedTurnIndex;
      }
      hasMoreHistory = cached.hasMoreHistory;
      hasMoreNewer = cached.hasMoreNewer;
      latestSettledTurn = cached.latestSettledTurn;
      recentWindowPrunePending = false;
    } else {
      replaceTimelineItems([]);
      subagentFolds.clear();
      // Windowed-history reset. A null floor disables the upsert floor
      // check until the backend tells us otherwise — between thread
      // clear and the initial-slice response any streamed upserts are
      // already ours to append normally.
      oldestLoadedCursor = null;
      newestLoadedCursor = null;
      oldestLoadedTurnIndex = null;
      newestLoadedTurnIndex = null;
      hasMoreHistory = false;
      hasMoreNewer = false;
      recentWindowPrunePending = false;
    }
    rowUiState.clear();
    disposeAllSmoothers();
    // Reset the live-content stamp so a recent stamp from the OUTGOING
    // thread can't bleed into the incoming one. Without this, switching
    // away from an actively-streaming thread leaves `lastLiveContentAt`
    // recent; the warm gate re-flips within the 500ms hold window, and
    // the incoming (settled) thread's late async-typesetting reflow would
    // read 'spring' off the stale stamp and chase its settled content.
    // A streaming incoming thread re-stamps on its first reveal/delta.
    lastLiveContentAt = 0;
    loadingOlder = false;
    loadingNewer = false;
    subagentHydrationInFlight.clear();
    subagentHydrationExhausted.clear();
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
   * Commit the incoming thread to the pane. Sets `thread`, restores
   * the per-thread RHS panel snapshot, and re-opens the diff panel
   * when the restored panel was a diff-checkpoint.
   */
  function commitIncomingThread(newThread: Thread): void {
    draftPlaceholder = null;
    thread = newThread;
    rhsPanelSlot.restoreForThread(newThread.id);
    if (
      newThread.mode === 'design' &&
      (rhsPanelSlot.activePanel?.kind === 'diff-checkpoint' ||
        rhsPanelSlot.activePanel?.kind === 'diff-payload')
    ) {
      rhsPanelSlot.closeForThread(newThread.id);
    }
    if (rhsPanelSlot.activePanel?.kind === 'diff-checkpoint') {
      diffPanel.open_();
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
        await hydrateThreadLiveState(
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
            applyPagedItems(paged, newThread.id);
          },
          (err) => {
            // Cache miss + load failure leaves the timeline blank and
            // raises a hard error. (Cache hits skip the load entirely
            // so they can't reach this branch.)
            replaceTimelineItems([]);
            oldestLoadedCursor = null;
            newestLoadedCursor = null;
            oldestLoadedTurnIndex = null;
            newestLoadedTurnIndex = null;
            hasMoreHistory = false;
            hasMoreNewer = false;
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
        diffPanel.setError(`Failed to load checkpoints: ${errString(err)}`);
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
    get diffPanel() {
      return diffPanel;
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
      diffPanel.markCheckpointsUnavailable(payload.reason);
      diffPanel.setError(
        'Workspace is not a git repo. Checkpoint diffs are unavailable.',
      );
    },
    applyCheckpointError(payload: CheckpointErrorEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      diffPanel.setError(`Checkpoint failed: ${payload.error}`);
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
      return oldestLoadedCursor;
    },
    get newestLoadedCursor() {
      return newestLoadedCursor;
    },
    get oldestLoadedTurnIndex() {
      return oldestLoadedTurnIndex;
    },
    get newestLoadedTurnIndex() {
      return newestLoadedTurnIndex;
    },
    get pendingTimelineShiftAtHead() {
      return pendingTimelineShiftAtHead;
    },
    get hasMoreHistory() {
      return hasMoreHistory;
    },
    get hasMoreNewer() {
      return hasMoreNewer;
    },
    get hasDeferredRecentWindowPrune() {
      return recentWindowPrunePending;
    },
    retryDeferredRecentWindowPrune(): void {
      if (!recentWindowPrunePending) return;
      recentWindowPrunePending = false;
      pruneToRecentWindowIfNeeded();
    },
    get loadingOlder() {
      return loadingOlder;
    },
    get loadingNewer() {
      return loadingNewer;
    },
    debugMemoryStats() {
      return {
        itemIndexEntries: itemIndexById.size,
        rowUiState: rowUiState.debugStats(),
        itemSmoothers: itemSmoothers.size,
        liveThinkingTails: itemLiveThinkingTail.size,
        optimisticItems: optimisticItemIds.size,
        oldestLoadedCursor,
        newestLoadedCursor,
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
    get pendingClarification() {
      return designState.pendingClarification;
    },
    get activeOptionSet() {
      return designState.activeOptionSet;
    },
    get designViewport() {
      return designState.designViewport;
    },
    get activeRhsPanel() {
      return rhsPanelSlot.activePanel;
    },
    get rhsSidebarWidth() {
      return rhsPanelSlot.width;
    },
    get showPlanSidebar() {
      return rhsPanelSlot.activePanel?.kind === 'plan';
    },
    get showDesignPreviewPanel() {
      return rhsPanelSlot.activePanel?.kind === 'design-preview';
    },
    get activeDiffPayload() {
      const panel = rhsPanelSlot.activePanel;
      if (panel?.kind !== 'diff-payload') return null;
      if (panel.filePath === undefined) return { payloadId: panel.payloadId };
      return { payloadId: panel.payloadId, filePath: panel.filePath };
    },
    get diffSidebarRestoreState() {
      return rhsPanelSlot.diffPayloadRestoreState;
    },
    /** Diagnostic — total snapshots held by the RHS panel slot. */
    get rhsPanelSnapshotCount() {
      return rhsPanelSlot.snapshotCount;
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
          const anchorItemId = hasMoreNewer ? (items.at(-1)?.id ?? '') : '';
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
          applyWindowMetadataFromPaged(paged, items);
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
        await hydrateThreadLiveState(
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
      draftPlaceholder = null;
      replaceTimelineItems([]);
      subagentFolds.clear();
      rowUiState.clear();
      disposeAllSmoothers();
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
      rhsPanelSlot.reset();
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
      oldestLoadedCursor = null;
      newestLoadedCursor = null;
      oldestLoadedTurnIndex = null;
      newestLoadedTurnIndex = null;
      hasMoreHistory = false;
      hasMoreNewer = false;
      recentWindowPrunePending = false;
      loadingOlder = false;
      loadingNewer = false;
      subagentHydrationInFlight.clear();
      subagentHydrationExhausted.clear();
      // See switchThread: both `pagingGeneration` and
      // `scrollToItemRequest.nonce` stay monotonic for the pane's
      // lifetime so no consumer observes a regressed counter.
      diffPanel.clearForThread();
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

    /**
     * Fetch the next batch of older turns and prepend them to the window.
     * Respects both the switch generation (thread swapped mid-flight) and
     * a paging-specific generation (concurrent invocations from double-
     * clicks or keyboard repeats). The return value is for scroll
     * anchoring: `insertedBeforeWindow` means at least one new row sorted
     * before the current in-memory first row. Components that know the
     * actual visible anchor still restore that anchor directly.
     */
    async loadOlder(): Promise<LoadOlderResult> {
      const currentThread = thread;
      if (!currentThread) return loadOlderResult('noop');
      if (!hasMoreHistory || loadingOlder) return loadOlderResult('noop');
      const floor = cloneCursor(oldestLoadedCursor);
      if (!floor) return loadOlderResult('noop');

      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      loadingOlder = true;
      try {
        const previousNewest = cloneCursor(newestLoadedCursor);
        const paged = await ListItemsBeforeCursor(
          currentThread.id,
          cursorForBinding(floor),
          LOAD_OLDER_ITEM_BUDGET,
        );
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        const prepend = itemsForThread(
          (paged.items ?? []) as Item[],
          currentThread.id,
        );
        const currentIds = new Set(items.map((item) => item.id));
        const insertedRows = prepend.some((item) => !currentIds.has(item.id));
        const currentFirst = items[0] ?? null;
        const insertedBeforeWindow =
          currentFirst === null
            ? insertedRows
            : prepend.some(
                (item) =>
                  !currentIds.has(item.id) &&
                  compareItemsByTimelinePosition(item, currentFirst) < 0,
              );
        // Head-grow: the engine unshifts its size store and reports a
        // head-splice compensation so the reading position holds. Set
        // before the mutation so the engine reads it in the same flush.
        pendingTimelineShiftAtHead = true;
        const next = mergeItemsById(prepend, items);
        replaceTimelineItems(next, { disposeDropped: true });
        const nextFloor = pagedOldestCursor(paged, prepend) ?? floor;
        setLoadedCursors(
          nextFloor,
          previousNewest ?? newestCursorFromItems(items),
        );
        // Progress guard. If the backend returned no items AND the floor
        // didn't decrease, another click would fire the same query for
        // the same range. Force hasMore=false so the UI stops offering a
        // button that can't actually load anything. A later in-flight
        // upsert that lands an older item will re-enable paging through
        // the normal streaming path.
        if (prepend.length === 0 && compareCursors(nextFloor, floor) >= 0) {
          hasMoreHistory = false;
        } else {
          hasMoreHistory = pagedHasMoreOlder(paged);
        }
        // Let the engine process the head-grow (shift=true) before the
        // prune. The two MUST be separate flushes: coalesced, the net length
        // change can't represent "prepend at head + drop at tail" and the
        // size store scrambles (spike-verified — see frontend-scroll.md).
        await tick();
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        // Flush 2: tail-prune (shift=false). Dropped rows are below the
        // viewport, so this is transparent to the reading position.
        pruneToHeadWindowIfNeeded();
        await tick();
        return loadOlderResult('loaded', insertedBeforeWindow, insertedRows);
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        console.error('loadOlder failed:', err);
        addToast('error', 'Failed to load older messages');
        return loadOlderResult('error');
      } finally {
        // Always clear the button's busy flag. The generation guard on
        // the happy path protects state mutation from late resolutions,
        // but `loadingOlder` is a UI-only flag — leaving it stuck true
        // after a pagingGeneration bump (e.g. a concurrent
        // loadUntilItem) would greys out the Load Older button
        // indefinitely. The worst outcome of clearing unconditionally
        // is a brief flash of the non-busy state while another pager
        // is still in-flight; the concurrent call will re-raise the
        // flag on its next write.
        loadingOlder = false;
        // Both flushes have run by now; clear the one-shot shift hint so a
        // later streaming length change isn't misread as a head mutation.
        pendingTimelineShiftAtHead = false;
      }
    },

    /**
     * Ensure the item with `itemID` is present in the loaded window.
     * Used by scroll-to-item callers (search hits, plan sidebar, tray)
     * before they dispatch the scroll intent. When the item is already
     * in the window this is a cheap `Array.some` and no backend call.
     * When the item lives below the floor the pane loads every turn
     * from the item's turn_index up to the existing tail in one
     * replacement — the window grows to cover the hit, no cumulative
     * multi-page ratchet.
     *
     * Returns `true` when the item is (now) loaded and scrollable,
     * `false` when the backend reports the item doesn't exist on this
     * thread (scroll callers show a toast and abandon the request).
     */
    async loadUntilItem(itemID: string): Promise<boolean> {
      const currentThread = thread;
      if (!currentThread || !itemID) return false;
      if (items.some((it) => it.id === itemID)) return true;

      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      let fetched: Item;
      try {
        fetched = (await GetThreadItem(currentThread.id, itemID)) as Item;
      } catch (err) {
        if (gen !== switchGeneration) return false;
        console.error('loadUntilItem GetThreadItem failed:', err);
        return false;
      }
      if (gen !== switchGeneration || pageGen !== pagingGeneration)
        return false;
      if (!fetched || !fetched.id) return false;
      // Defense-in-depth: the backend already filters by threadId, but a
      // mislayered binding or a future cache that returns stale rows
      // shouldn't cross-pollute between panes.
      if (fetched.threadId !== currentThread.id) return false;

      // Race: another upsert or loadOlder might have pulled the item in
      // between our check and the backend round-trip. Re-check before
      // paging in a whole turn window we don't need.
      if (items.some((it) => it.id === itemID)) return true;

      // Subagent children never appear in history windows. Walk the
      // parent chain to the top-level launch root so the slice anchors
      // on a row the window will actually contain, then hydrate the
      // root's subtree so the scroll can resolve to the containing
      // group card. The visited set bounds corrupt parent cycles; a
      // broken chain falls back to anchoring on the child's own
      // coordinates (the slice still positions correctly — only the
      // subtree hydration is skipped, and the trailing containment
      // check reports the miss).
      let sliceAnchorID = itemID;
      let subagentRootID = '';
      if ((fetched.parentId ?? '') !== '') {
        let walker = fetched;
        const visited = new Set<string>([walker.id]);
        while ((walker.parentId ?? '') !== '' && !visited.has(walker.parentId ?? '')) {
          let parentItem: Item;
          try {
            parentItem = (await GetThreadItem(
              currentThread.id,
              walker.parentId ?? '',
            )) as Item;
          } catch (err) {
            console.error('loadUntilItem parent walk failed:', err);
            break;
          }
          if (gen !== switchGeneration || pageGen !== pagingGeneration)
            return false;
          if (!parentItem?.id || parentItem.threadId !== currentThread.id)
            break;
          visited.add(parentItem.id);
          walker = parentItem;
        }
        if ((walker.parentId ?? '') === '') {
          sliceAnchorID = walker.id;
          subagentRootID = walker.id;
        }
      }

      loadingOlder = true;
      try {
        const paged = await ListThreadSliceAround(
          currentThread.id,
          sliceAnchorID,
          ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        );
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return false;
        const next = reconcileItemWindow(
          itemsForThread((paged.items ?? []) as Item[], currentThread.id),
          items,
        );
        replaceTimelineItems(next, { disposeDropped: true });
        applyWindowMetadataFromPaged(paged, items);
        if (subagentRootID) {
          await hydrateSubagentChildren(subagentRootID);
          if (gen !== switchGeneration || pageGen !== pagingGeneration)
            return false;
        }
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return false;
        console.error('loadUntilItem ListThreadSliceAround failed:', err);
        addToast('error', 'Failed to load message');
        return false;
      } finally {
        // Match loadOlder's unconditional reset — see comment there.
        loadingOlder = false;
      }
      return items.some((it) => it.id === itemID);
    },

    /**
     * Hydrate the child transcript under a subagent launch anchor —
     * called by SubagentGroup when an expanded card's loaded children
     * trail its decorated descendant count. Deduped per anchor id;
     * see `hydrateSubagentChildren`.
     */
    ensureSubagentChildren(rootItemID: string): Promise<boolean> {
      return hydrateSubagentChildren(rootItemID);
    },

    async loadNewer(): Promise<LoadOlderResult> {
      const currentThread = thread;
      if (!currentThread) return loadOlderResult('noop');
      if (!hasMoreNewer || loadingNewer) return loadOlderResult('noop');
      const ceiling = cloneCursor(newestLoadedCursor);
      if (!ceiling) return loadOlderResult('noop');

      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      loadingNewer = true;
      try {
        const previousOldest = cloneCursor(oldestLoadedCursor);
        const paged = await ListItemsAfterCursor(
          currentThread.id,
          cursorForBinding(ceiling),
          LOAD_OLDER_ITEM_BUDGET,
        );
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        const append = itemsForThread(
          (paged.items ?? []) as Item[],
          currentThread.id,
        );
        const currentIds = new Set(items.map((item) => item.id));
        const insertedRows = append.some((item) => !currentIds.has(item.id));
        const currentLast = items.at(-1) ?? null;
        const insertedAfterWindow =
          currentLast === null
            ? insertedRows
            : append.some(
                (item) =>
                  !currentIds.has(item.id) &&
                  compareItemsByTimelinePosition(item, currentLast) > 0,
              );
        // Tail-grow: shift stays false (the engine appends size slots at the
        // end, no scroll compensation — rows arrive below the viewport).
        pendingTimelineShiftAtHead = false;
        const next = mergeItemsById(append, items);
        replaceTimelineItems(next, { disposeDropped: true });
        const nextCeiling = pagedNewestCursor(paged, append) ?? ceiling;
        setLoadedCursors(
          previousOldest ?? oldestCursorFromItems(items),
          nextCeiling,
        );
        const nextHasMoreNewer =
          append.length === 0 && compareCursors(nextCeiling, ceiling) <= 0
            ? false
            : pagedHasMoreNewer(paged);
        hasMoreNewer = nextHasMoreNewer;
        // Flush 1: the engine processes the tail-grow before the head-prune.
        // Separate flushes (see loadOlder): a coalesced tail-grow +
        // head-shrink can't be expressed by one `shift`.
        await tick();
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        // Flush 2: head-prune (shift=true) — the engine splices its size
        // store from the front and compensates scrollTop by the dropped
        // height, holding the reading position. No explicit anchor restore
        // needed.
        pruneToRecentWindowIfNeeded({
          hasMoreNewerAfterPrune: nextHasMoreNewer,
          positionMode: 'shift',
        });
        await tick();
        return loadOlderResult('loaded', insertedAfterWindow, insertedRows);
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return loadOlderResult('stale');
        console.error('loadNewer failed:', err);
        addToast('error', 'Failed to load newer messages');
        return loadOlderResult('error');
      } finally {
        loadingNewer = false;
        pendingTimelineShiftAtHead = false;
      }
    },

    async loadRecentTail(): Promise<boolean> {
      const currentThread = thread;
      if (!currentThread) return false;
      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      loadingNewer = true;
      try {
        const paged = await ListThreadSliceAround(
          currentThread.id,
          '',
          ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        );
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return false;
        const next = reconcileItemWindow(
          itemsForThread((paged.items ?? []) as Item[], currentThread.id),
          items,
        );
        replaceTimelineItems(next, { disposeDropped: true });
        applyWindowMetadataFromPaged(paged, items);
        return true;
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration)
          return false;
        console.error('loadRecentTail failed:', err);
        addToast('error', 'Failed to load latest messages');
        return false;
      } finally {
        loadingNewer = false;
      }
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
     * need to suspend auto-follow during a gesture (sidebar resizers,
     * resizable drawers). Call `pause = pane.scrollController?.pauseAutoScroll()`
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
      disposeSmootherFor(itemId);
      rowUiState.disposeItems([removed]);
      recomputeReveal();
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
      for (const r of removed) disposeSmootherFor(r.id);
      rowUiState.disposeItems(removed);
      recomputeReveal();
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
      for (const [id, entry] of itemSmoothers) {
        entry.smoother.snap();
        entry.smoother.dispose();
        itemSmoothers.delete(id);
        itemLiveThinkingTail.delete(id);
      }
    },

    /**
     * Test-only count of live per-item streaming smoothers. Lets dispose-
     * contract regressions assert directly on the map size for kinds with
     * no other observable (assistant_text has no live-tail accessor). Not
     * part of the production surface.
     */
    __itemSmootherCountForTest(): number {
      return itemSmoothers.size;
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
      // path (events.ts providerUpsertAdvancesLiveContent).
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
      const entry = getOrCreateSmoothing(evt.itemId, current.summary);
      entry.setLatestUpdatedAt(evt.updatedAt);
      entry.smoother.appendDelta(evt.delta);
      // A new smoothed row (or fresh lag on the frontier) may move the gate;
      // recompute so a withheld successor pauses and the frontier fast-drains.
      recomputeReveal();
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
        disposeSmootherFor(evt.itemId);
        return;
      }
      const current = items[index];
      const smoothing = itemSmoothers.get(evt.itemId);
      const nextStatus = evt.patch.status;
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
        disposeSmootherFor(evt.itemId);
      } else if (smoothing && evt.patch.summary !== undefined) {
        // Status flipping to completed (or any non-snap patch) may
        // carry a final summary. If it extends what the smoother has
        // already received, push the suffix as a delta so the smoother
        // finishes the reveal naturally. If it doesn't extend (an
        // overwrite or a backwards correction), snap and dispose so
        // the patch's summary wins cleanly.
        const received = smoothing.smoother.getReceived();
        const patchSummary = evt.patch.summary;
        if (patchSummary !== received && patchSummary.startsWith(received)) {
          if (evt.patch.updatedAt !== undefined) {
            smoothing.setLatestUpdatedAt(evt.patch.updatedAt);
          }
          smoothing.smoother.appendDelta(patchSummary.slice(received.length));
        } else if (patchSummary !== received) {
          smoothing.smoother.snap();
          disposeSmootherFor(evt.itemId);
        } else if (
          nextStatus !== undefined &&
          nextStatus !== 'streaming' &&
          smoothing.smoother.isCaughtUp()
        ) {
          // patchSummary === received AND a terminal status AND nothing
          // left to reveal. No further rAF tick will fire, so the
          // onReveal auto-cleanup can't dispose — do it here or the
          // smoother (and its itemLiveThinkingTail entry) leaks until the
          // next thread switch. This is the Codex completion shape:
          // content-block-stop carries ContentPresent=true, so
          // doSettleStreamingText re-asserts the full summary (== what the
          // smoother received). The bare-status branch below only covers
          // the case where that equal summary is OMITTED from the patch.
          disposeSmootherFor(evt.itemId);
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
        disposeSmootherFor(evt.itemId);
      }

      // Snap/dispose above may have cleared the frontier (interrupt, error,
      // completion); recompute so the gate drops and any withheld tail rows
      // reveal. Runs before the early `itemsAreEqual` return below.
      recomputeReveal();

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
        const stillSmoothing = itemSmoothers.has(evt.itemId);
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
      evictSettledSubagentChildren([next]);
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
      if (!willExpand) evictCollapsedSubagentSubtree(groupKey);
      return willExpand;
    },
    /** Live fold aggregate for a launch anchor — MessageTimeline threads
     *  this into the grouping pipeline. Reads are revision-driven: every
     *  fold mutation rides a timelineRevision bump. */
    subagentLiveAggregate(anchorId: string): SubagentFoldAggregate | undefined {
      return subagentFolds.aggregate(anchorId);
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
    // See `itemLiveThinkingTail` for why ThinkingBlock prefers this over
    // the trimmed-summary sliding window.
    liveThinkingTailForItem(itemId: string): string | null {
      return itemLiveThinkingTail.get(itemId) ?? null;
    },

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
    snapSmoothersToReceived(): void {
      if (itemSmoothers.size === 0) return;
      // snap() → onReveal can dispose+delete entries (terminal rows), so
      // iterate a snapshot rather than the live map.
      for (const entry of [...itemSmoothers.values()]) entry.smoother.snap();
      recomputeReveal();
    },

    /**
     * Reveal gate for the timeline. While a turn streams, this is the
     * (turnIndex, itemIndex) of the top-level item currently revealing;
     * MessageTimeline withholds nodes after it via `sliceRevealedNodes` so
     * the next row waits for the current item's reveal to drain. `null`
     * outside live streaming — render everything. See `recomputeReveal`.
     */
    get revealBoundary(): RevealBoundary | null {
      return revealBoundary;
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
     * the wire-push handler in events.ts → projectTurnStarted directly;
     * this method is the test-and-explicit-control entry point.
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
      // The wire is done; any smoother still behind is pure backlog. At
      // the adaptive ceiling (~840 cps) a multi-KB reasoning backlog
      // keeps animating for many seconds after the turn settled — drain
      // it within ~END_OF_TURN_DRAIN_MS instead. First-drain-wins
      // semantics keep an earlier sequencer drain's tighter deadline.
      for (const entry of itemSmoothers.values()) {
        entry.smoother.requestFastDrain(END_OF_TURN_DRAIN_MS);
      }
      // Run the deferred window prune now that the turn is quiet — the
      // streaming-append path skips it while a turn is active so the
      // head-drop repaint never lands mid-stream.
      if (!hasMoreNewer) {
        pruneToRecentWindowIfNeeded();
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
      // The provider:todo_update listener (events.ts:applyTodoUpdate) is
      // the wire boundary and validates `steps` is an array before
      // calling here; trust the input from that point on.
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
      thread = nextThread;
      contextWindow = seedContextWindow(nextThread);
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

    toggleDiffPanel(tab: DiffPanelTab = 'workspace'): void {
      if (thread?.mode === 'design') return;
      if (diffPanel.open) {
        activatePanel(null);
        return;
      }
      // Land on the requested tab before the panel mounts so it never
      // flashes the messages tab on the way to workspace. The header diff
      // badge and the diff.panel.toggle keybinding both open on 'workspace';
      // checkpoint-click / setDiffPanelOpen stay messages-oriented.
      diffPanel.setTabMode(tab);
      activatePanel({ kind: 'diff-checkpoint' });
    },

    togglePlanSidebar(): void {
      if (rhsPanelSlot.activePanel?.kind === 'plan') activatePanel(null);
      else activatePanel({ kind: 'plan' });
    },

    setShowPlanSidebar(value: boolean): void {
      if (value) activatePanel({ kind: 'plan' });
      else if (rhsPanelSlot.activePanel?.kind === 'plan') activatePanel(null);
    },

    toggleDesignPreviewPanel(): void {
      if (thread?.mode !== 'design') return;
      if (rhsPanelSlot.activePanel?.kind === 'design-preview')
        activatePanel(null);
      else activatePanel({ kind: 'design-preview' });
    },

    setShowDesignPreviewPanel(value: boolean): void {
      if (thread?.mode !== 'design') return;
      if (value) activatePanel({ kind: 'design-preview' });
      else if (rhsPanelSlot.activePanel?.kind === 'design-preview')
        activatePanel(null);
    },

    setDiffPanelOpen(value: boolean): void {
      if (value && thread?.mode === 'design') return;
      if (value) activatePanel({ kind: 'diff-checkpoint' });
      else if (diffPanel.open) activatePanel(null);
    },

    /**
     * Open the per-tool diff sidebar for a specific payload. Mutex with
     * PlanSidebar and DiffPanelDrawer — closes both. `filePath` is
     * optional and used by the sidebar to scroll to a file when the
     * payload contains multiple (e.g. a Claude `file_change` tool_result
     * with several files).
     */
    openDiffSidebar(payload: { payloadId: string; filePath?: string }): void {
      if (thread?.mode === 'design') return;
      activatePanel({
        kind: 'diff-payload',
        payloadId: payload.payloadId,
        filePath: payload.filePath,
      });
    },

    closeRhsPanel(): void {
      activatePanel(null);
    },

    setRhsSidebarWidthLive(next: number): void {
      rhsPanelSlot.setWidthLive(next);
    },

    persistRhsSidebarWidth(): void {
      rhsPanelSlot.persistWidthForThread(thread?.id);
    },

    getRhsSidebarMaxWidth(): number {
      return rhsPanelSlot.getMaxWidth();
    },

    /**
     * Push the sidebar's current UI state up to the pane. Called by
     * DiffSidebar whenever its viewMode / wordWrap / expandedFiles /
     * scrollTop change. Stored in memory only; snapshotted to the
     * per-thread map on the next thread switch.
     */
    recordDiffSidebarUI(state: DiffSidebarUIState): void {
      rhsPanelSlot.recordDiffPayloadUI(state);
    },

    /**
     * Atomically take the pending restore-state and clear it.
     * Returns null when no restore is pending. Called by DiffSidebar
     * exactly once on mount.
     */
    consumeDiffSidebarRestoreState(): DiffSidebarUIState | null {
      return rhsPanelSlot.consumeDiffPayloadRestore();
    },

    /**
     * Close whichever right-side panel is currently open. Idempotent —
     * safe to call when nothing is open. Explicit close keeps the
     * thread-specific width but removes the restore target.
     */
    closeActivePanel(): void {
      activatePanel(null);
    },

    mergeChannelMessages(incoming: ChannelMessage[]): void {
      channelState.mergeMessages(incoming);
    },

    setChannelStatus(status: ThreadChannelStatus): void {
      channelState.setStatus(status);
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
