import type { Item, Project, Thread } from '../types/models';
import { asProviderID } from '../types/providers';
import type { Checkpoint } from '../types/checkpoint';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
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
  CreateThread,
  GetThreadItem,
  GetThreadLiveState,
  ListPendingInteractiveRequests,
  ListItemsBeforeTurn,
  ListRecentThreadItems,
  ListRecentTurns,
  ListThreadCheckpoints,
  SwitchThread,
  AutoResumeThread,
} from './bindings';
import { prependThread, replaceThread } from './threads.svelte';
import { leaseDuringSettle } from '../utils/scrollLeaseDuringTransition';
import {
  clearWorktreeIntent,
  migrateWorktreeIntent,
  seedDefaultWorktreeIntentForDraft,
} from './worktreeIntent.svelte';
import {
  clearRuntimeModeDraft,
  migrateRuntimeModeDraft,
} from './runtimeModeDraft.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

import { addToast } from './toast.svelte';
import { createDiffPanelState, type DiffPanelState } from './diffPanel.svelte';
import {
  createRhsPanelSlot,
  type DiffSidebarUIState,
  type RhsPanel,
  type RhsPanelSlot,
} from './rhsPanelSlot.svelte';
import { errString } from '../utils/errors';
import { clearTokensForThread } from '../utils/tokenCacheReactive.svelte';
import {
  MAX_CACHED_SNAPSHOT_ITEMS,
  threadItemCache,
  type ThreadItemSnapshot,
} from './threadItemCache';
import {
  type ApplyItemUpsertsToWindowResult,
  applyItemUpsertsToWindow,
  compareItemsByTimelinePosition,
  itemsForThread,
  mergeItemsById,
  mergeMissingItemsById,
  reconcileItemWindow,
} from './threadItems';
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { ListThreadSliceAround } from './bindings';
import {
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

/**
 * Default raw-item budget passed to `ListItemsBeforeTurn` for an
 * explicit "Load older" page. The backend walks turns DESC summing
 * each turn's item count (excluding plan_update notifications) until
 * cumulative ≥ this budget, then returns that turn's items plus every
 * newer one strictly below the caller's floor. One click = ~this many
 * items prepended, regardless of per-turn density. Matches the initial
 * slice size so the user sees a consistent "page size."
 */
const LOAD_OLDER_ITEM_BUDGET = 200;

/**
 * Hard cap on the item budget passed to `loadUntilItem` for explicit
 * jump paths (search hits, plan sidebar clicks, checkpoint jumps). A
 * fixed literal — independent of `LOAD_OLDER_ITEM_BUDGET` — so tuning
 * the per-click page size for UX doesn't silently shrink the search-
 * reachability budget. Sized at roughly 5× the normal page so a search
 * hit deep in a long thread is reachable in one round-trip without
 * unbounded fetches; if a target lives below this cap, the load fails
 * cleanly and the caller shows the existing "couldn't locate that
 * message" toast.
 */
const LOAD_UNTIL_ITEM_HARD_CAP = 1000;

/**
 * Initial-load slice size on `switchThread`. Sized to cover a desktop
 * viewport (10–15 rendered cards) with several screens of overscan,
 * and large enough that one heavy subagent turn collapsing to a
 * single SubagentGroup card doesn't leave the timeline visually empty.
 * Older items page in lazily as the user scrolls up.
 */
const SLICE_AROUND_ITEM_BUDGET = 200;

/**
 * Doherty perception threshold. A switch that completes inside this
 * window never paints the loading spinner — the view transitions
 * straight to the loaded content. Above the threshold, the spinner
 * fades in and stays until `loading=false`. 100ms is the standard
 * "instant to the user" budget across UX research.
 */
const SPINNER_THRESHOLD_MS = 100;

/**
 * Maximum runes the frontend keeps in `items[i].summary` for a streaming
 * thinking row. Mirrors `thinkingPreviewRunes` in
 * `internal/triage/stream_items.go` — the server-side tail cap on the
 * persisted thinking summary. Matching the cap means the completion
 * upsert (which carries the same tail) does not visibly shrink the row
 * at settle. Full thinking content stays on-demand via the payload
 * table, fetched when the user expands the row.
 *
 * Sized to overflow the 3-line collapsed-view box (`max-h-[3lh]` at
 * 12px italic with `leading-relaxed`) at realistic chat-pane widths so
 * the CSS clip + tail scroll-pin show a consistent 3 lines regardless
 * of pane width.
 */
const THINKING_TAIL_RUNES = 400;

/**
 * Returns the tail of `text` containing at most `maxRunes` Unicode code
 * points. Surrogate-pair safe — walks code points from the end and slices
 * once. Cheap on the common case where `text.length <= maxRunes`.
 */
function trimToTailRunes(text: string, maxRunes: number): string {
  if (text.length <= maxRunes) return text;
  let runes = 0;
  for (let i = text.length; i > 0; ) {
    const cp = text.codePointAt(i - 1)!;
    i -= cp > 0xffff ? 2 : 1;
    runes += 1;
    if (runes >= maxRunes) return text.slice(i);
  }
  return text;
}

function sameRhsPanel(left: RhsPanel | null, right: RhsPanel | null): boolean {
  if (left === null || right === null) return left === right;
  if (left.kind !== right.kind) return false;
  if (left.kind !== 'diff-payload' || right.kind !== 'diff-payload') return true;
  return left.payloadId === right.payloadId && left.filePath === right.filePath;
}

// ActiveTurn now lives in threadStatuses.svelte.ts (single source of
// truth for the global active-turn registry). Re-exported here so
// downstream importers (events.ts, panes, tests) don't have to rewire
// their imports for the move.
export type { ActiveTurn } from './threadStatuses.svelte';

export { parseTokenUsage } from './threadTurnProjection';
export type { SettledTurn } from './threadTurnProjection';

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
export type {
  DiffSidebarUIState,
  RhsPanel,
} from './rhsPanelSlot.svelte';

export type LoadOlderResult = {
  insertedBeforeWindow: boolean;
  insertedRows: boolean;
  status: 'loaded' | 'noop' | 'stale' | 'error';
};

/**
 * Minimal surface a registered scroll controller exposes to the pane.
 * Kept narrow on purpose: the pane brokers a `pauseAutoScroll()` lease
 * for outside surfaces (resizers, drawers) and a re-pin nudge for
 * surfaces whose layout change isn't visible to the controller's own
 * content ResizeObserver (e.g. composer growth changes the outer
 * padding-bottom but not the contentEl's scrollHeight). The concrete
 * controller (`useStickToBottom`) has more methods, but only this
 * narrow seam crosses the pane boundary — chat MessageTimeline and
 * Discussion ChannelView both register the same controller shape so
 * one set of resizer/drawer hooks works on both surfaces.
 *
 * `notifyContentMaybeGrew` is called by chat's `ChatView`
 * (composer-overlay growth changes the timeline's bottom padding
 * without growing the contentEl) and as a fallback for host-layout
 * nudges when a controller has not implemented `notifyHostLayoutSettled`.
 * Discussion does not call it today — its textarea sits in a separate
 * `shrink-0` flex section — but the seam is here so a future Discussion
 * composer-height story could reach the controller the same way chat does.
 */
export interface PaneScrollController {
  pauseAutoScroll(): () => void;
  /**
   * Nudge the controller to re-evaluate "should I scroll to the
   * bottom?". A no-op unless the user is sticky and no lease is held.
   * Use this from layout-changing surfaces outside the timeline whose
   * change isn't observable to the controller's own ResizeObserver
   * (composer overlay growth, anything that mutates outer scroll
   * padding without changing the contentEl's scrollHeight).
   */
  notifyContentMaybeGrew(): void;
  /**
   * Notify the timeline that its pane was moved or reflowed by the host
   * without any transcript content change. Reconcile the virtualizer against
   * the settled layout; panes without explicit user escape should restore
   * bottom intent, while escaped panes should keep their existing virtual
   * scroll offset.
   */
  notifyHostLayoutSettled?(): void;
  /**
   * Preserve a clicked disclosure header's viewport position while the
   * row expands or collapses. Optional so Discussion and simple test
   * doubles can keep the minimal pause/notify-only shape.
   */
  preserveScrollAnchor?(anchor: HTMLElement, action: () => void | Promise<void>): Promise<void>;
  /**
   * Optional. True when the timeline should behave as bottom-present:
   * sticky by intent, or geometrically near the bottom while not escaped.
   * Explicit user escape returns false even inside the near-bottom band.
   * Lifecycle-aware rows read this before transitioning their own height
   * on settle — e.g. ThinkingBlock auto-collapses its body on the
   * streaming -> settled boundary only when the user is at the bottom,
   * so a user mid-read of the streamed thinking text doesn't have content
   * yanked out from under them. Optional so test mocks that don't care
   * about lifecycle transitions can omit it; rows treat `undefined` as
   * "at bottom" (the common sticky-mode default).
   */
  readonly isAtBottom?: boolean;
}

export interface ScrollToItemOptions {
  behavior?: 'instant' | 'animated';
  flash?: boolean;
}

interface ScrollToItemRequest {
  itemId: string;
  nonce: number;
  behavior: 'instant' | 'animated';
  flash: boolean;
}

function loadOlderResult(
  status: LoadOlderResult['status'],
  insertedBeforeWindow = false,
  insertedRows = false,
): LoadOlderResult {
  return { status, insertedBeforeWindow, insertedRows };
}

interface LiveStateHydrationGuard {
  activeTurnAtRequest: ActiveTurn | null;
  queueRevisionAtRequest: number;
  liveTodoRevisionAtRequest: number;
}

/**
 * Returns the absolute workspace path of a pane's active thread, or
 * '' when the pane is undefined / has no thread / has an empty
 * workspacePath. Lets every chat surface that drives `OpenInEditor`
 * (or threads workspacePath into ChatMarkdown / EditorLink) read
 * through one accessor instead of repeating `pane?.thread?.workspacePath ?? ''`.
 *
 * Centralising the lookup also gives us one place to teach the app
 * about future workspace-source preferences (e.g. preferring
 * thread.worktreePath when set).
 */
export function paneWorkspacePath(pane: ThreadPane | undefined): string {
  return pane?.thread?.workspacePath ?? '';
}

export type DraftPlaceholderMode = 'chat' | 'design';

export interface DraftThreadPlaceholder {
  id: string;
  projectId: string;
  projectName: string;
  projectPath: string;
  mode: DraftPlaceholderMode;
  createdAt: number;
}

/**
 * Seed values for the synthetic placeholder thread. Callers fetch
 * these via the `GetThreadDefaults` binding so the placeholder's
 * toolbar (model name, effort, runtime mode) and workspace strip
 * (current git branch) render the same values a freshly-created
 * thread would. All fields optional — when omitted, the placeholder
 * falls back to the previous "no model selected / no branch" surface.
 */
export interface DraftPlaceholderDefaults {
  provider?: string;
  model?: string;
  reasoningEffort?: string;
  fastMode?: boolean;
  contextWindow?: number;
  runtimeMode?: string;
  branch?: string;
  workspacePath?: string;
}

interface ThreadPaneOptions {
  paneId?: string;
}

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
  const itemIndexById: Map<string, number> = new Map();
  const rowUiState = createThreadRowUiState({
    getItemById(itemId: string): Item | undefined {
      const index = itemIndexById.get(itemId);
      return index === undefined ? undefined : items[index];
    },
  });
  const pendingInteractiveState = createThreadPendingInteractiveState();
  let contextWindow: ContextWindow | null = $state(null);
  // Rate-limit snapshots live in the global `rateLimitsInfo.svelte.ts`
  // store keyed by provider — they are an account property, not a
  // thread property. Components read via `getProviderRateLimit(provider,
  // windowMins)` directly. Keeping them out of per-pane state means
  // they survive thread switches, turn completions, and metadata
  // updates with no defensive logic on the pane side.
  let providerBanner: ProviderStatusEvent | null | undefined = $state(undefined);
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
  // materializingThreadPromise coalesces concurrent ensureMaterializedThread
  // callers — composer input, paste/upload, send, toolbar pickers — into a
  // single CreateThread call. Cleared in `finally` so a subsequent
  // placeholder can materialize on its own.
  let materializingThreadPromise: Promise<string | null> | null = null;
  let showTerminal: boolean = $state(false);
  // Diff panel is per-pane; created once and reset on thread switch so its
  // caches don't leak between threads.
  const diffPanel: DiffPanelState = createDiffPanelState();

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
   * items when the user flips between them quickly.
   */
  let switchGeneration = 0;

  /**
   * Windowed-history state. The pane holds a contiguous tail of the
   * thread's items (~50 items on initial load); older history loads
   * on demand via `loadOlder()` or `loadUntilItem()`.
   *
   *  - `oldestLoadedTurnIndex` is the inclusive floor of the window.
   *    `null` when nothing is loaded (empty thread / fresh pane).
   *  - `hasMoreHistory` drives the "Load older" button's visibility.
   *  - `loadingOlder` disables the button while a fetch is in flight.
   *
   * Upsert events whose item coordinates fall below the window floor
   * are silently dropped — the canonical copy lives in SQLite and will
   * be pulled in the next time the user loads older history. See
   * `upsertItem` below.
   */
  let oldestLoadedTurnIndex: number | null = $state(null);
  let hasMoreHistory: boolean = $state(false);
  let loadingOlder: boolean = $state(false);

  /**
   * Separate generation counter for `loadOlder` / `loadUntilItem` so a
   * second click doesn't race with a slow first fetch. `switchGeneration`
   * covers thread swaps; this guards against same-thread concurrent
   * paging fetches (double-click, keyboard repeat).
   */
  let pagingGeneration = 0;

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
    behavior: 'instant',
    flash: false,
  });

  /**
   * Live registration slot for the timeline's sticky-bottom controller.
   * MessageTimeline registers its controller on mount so external surfaces
   * (sidebar resizers, inspector panels, anything that opens a drawer over
   * the chat column) can acquire a `pauseAutoScroll()` lease while a
   * gesture is in flight, preventing auto-follow from yanking the view
   * mid-drag. The factory only knows about the minimal surface
   * (`PaneScrollController`) — it never depends on virtua or the DOM
   * controller's full type, so the contract stays cheap to honour.
   */
  let scrollController: PaneScrollController | null = $state(null);

  function rebuildItemIndexes(nextItems: Item[]): void {
    itemIndexById.clear();
    for (let index = 0; index < nextItems.length; index += 1) {
      const item = nextItems[index];
      itemIndexById.set(item.id, index);
    }
  }

  function replaceTimelineItems(nextItems: Item[]): boolean {
    if (items === nextItems) return false;
    items = nextItems;
    rebuildItemIndexes(items);
    timelineRevision++;
    return true;
  }

  function upsertItemsBatch(incoming: Item[]): ApplyItemUpsertsToWindowResult | null {
    if (incoming.length === 0) return null;

    const next = applyItemUpsertsToWindow({
      current: items,
      incoming,
      itemIndexById,
      currentThreadId: thread?.id ?? null,
      oldestLoadedTurnIndex,
    });
    if (!next) return null;
    items = next.items;
    if (next.indexesNeedRebuild) {
      rebuildItemIndexes(items);
    } else {
      const firstAppendIndex = items.length - next.appendedItems.length;
      for (let index = 0; index < next.appendedItems.length; index += 1) {
        itemIndexById.set(next.appendedItems[index].id, firstAppendIndex + index);
      }
    }
    timelineRevision++;

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
    return next;
  }

  function applyPendingInteractiveSnapshot(
    threadID: string,
    snapshot: PendingInteractiveRequests | null | undefined,
  ): void {
    const registrySnapshot = pendingInteractiveState.registrySnapshotFor(snapshot);
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
      snapshot = (await ListPendingInteractiveRequests(threadID)) as PendingInteractiveRequests;
    } catch (err) {
      if (gen === switchGeneration && thread?.id === threadID) {
        console.error('Failed to hydrate pending interactive requests:', err);
      }
      return;
    }
    if (gen !== switchGeneration || thread?.id !== threadID) return;
    if (hydrationToken !== undefined && !isThreadLiveStateHydrationCurrent(threadID, hydrationToken)) return;

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
        projectTurnStarted(threadID, active.turnId, active.turnIndex, active.startedAt);
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

    applyPendingInteractiveSnapshot(threadID, snapshot.interactive as PendingInteractiveRequests);

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
    const hydrationToken = existingHydrationToken ?? beginThreadLiveStateHydration(threadID);
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
    replaceTimelineItems(mergeMissingItemsById(incoming, items));
    const pagedFloor = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
    oldestLoadedTurnIndex = pagedFloor;
    hasMoreHistory = paged.hasMore ?? false;
  }

  async function refreshCheckpointsForThread(threadID: string): Promise<void> {
    const checkpoints = ((await ListThreadCheckpoints(threadID)) ?? []) as Checkpoint[];
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
      // Deliberately do not snapshot virtua's row-size cache here. Those
      // sizes are only valid with the row UI state that produced them
      // (expanded payloads, loaded thumbnails, nested bodies). switchThread
      // clears that state for the incoming thread, so replaying old row
      // sizes can make virtua trust geometry the DOM cannot reproduce.
      threadItemCache.set(outgoingThreadId, {
        items,
        oldestLoadedTurnIndex,
        hasMoreHistory,
        latestSettledTurn,
      });
    }
    if (sameThreadReswitch) {
      threadItemCache.evict(incomingThreadId);
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
  }

  /**
   * Look up the incoming thread's cached snapshot and saved scroll
   * anchor, install the snapshot (or fresh empty state) onto the pane,
   * and reset per-row UI registries. Returns the snapshot (so the
   * initial load can decide to skip the fetch on cache hit) and the
   * anchor item id (empty string means tail-load).
   */
  function installCacheOrFreshState(
    newThread: Thread,
  ): { cached: ThreadItemSnapshot | null; sliceAnchorId: string } {
    const cached = threadItemCache.get(newThread.id);
    const scrollSnapshot = getThreadScrollSnapshot(newThread.id);
    const sliceAnchorId = scrollSnapshot?.kind === 'anchor' ? scrollSnapshot.itemId : '';

    loading = true;
    if (cached) {
      replaceTimelineItems(cached.items);
      oldestLoadedTurnIndex = cached.oldestLoadedTurnIndex;
      hasMoreHistory = cached.hasMoreHistory;
      latestSettledTurn = cached.latestSettledTurn;
    } else {
      replaceTimelineItems([]);
      // Windowed-history reset. A null floor disables the upsert floor
      // check until the backend tells us otherwise — between thread
      // clear and the initial-slice response any streamed upserts are
      // already ours to append normally.
      oldestLoadedTurnIndex = null;
      hasMoreHistory = false;
    }
    rowUiState.clear();
    loadingOlder = false;
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
      newThread.mode === 'design'
      && (
        rhsPanelSlot.activePanel?.kind === 'diff-checkpoint'
        || rhsPanelSlot.activePanel?.kind === 'diff-payload'
      )
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
        const switched = (await SwitchThread(newThread.id)) as Thread | undefined;
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
        await hydrateThreadLiveState(newThread.id, gen, liveStateHydrationToken);
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
          () => ListThreadSliceAround(newThread.id, sliceAnchorId, SLICE_AROUND_ITEM_BUDGET),
          (paged) => {
            applyPagedItems(paged, newThread.id);
          },
          (err) => {
            // Cache miss + load failure leaves the timeline blank and
            // raises a hard error. (Cache hits skip the load entirely
            // so they can't reach this branch.)
            replaceTimelineItems([]);
            oldestLoadedTurnIndex = null;
            hasMoreHistory = false;
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
    get paneId() { return paneId; },
    get thread() { return thread; },
    get threadId() { return draftPlaceholder ? null : thread?.id ?? null; },
    get draftPlaceholder() { return draftPlaceholder; },
    get hasDraftPlaceholder() { return draftPlaceholder !== null; },
    get canCompose() { return Boolean(thread || draftPlaceholder); },
    get items() { return items; },
    /**
     * "Locked in" — the user has sent at least one message, so the
     * provider/model selection is committed for this thread. UI
     * affordances that should hide while the thread is still in its
     * pre-send configuration phase (rate-limit rings, model picker
     * disable) read this getter rather than re-deriving from
     * `items.length`.
     */
    get isLocked() { return items.length > 0; },
    get timelineRevision() { return timelineRevision; },
    get pendingApprovals() { return pendingInteractiveState.approvals; },
    get pendingUserInputs() { return pendingInteractiveState.userInputs; },
    get contextWindow() { return contextWindow; },
    get providerBanner() { return providerBanner; },
    get generalError() { return generalError; },
    get generalErrorKind() { return generalErrorKind; },
    get loading() { return loading; },
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
    get sendInFlight() { return sendInFlight; },
    get showTerminal() { return showTerminal; },
    get diffPanel() { return diffPanel; },
    refreshCheckpoints: refreshCheckpointsForThread,
    applyCheckpointCaptured(payload: CheckpointCapturedEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      void refreshCheckpointsForThread(payload.threadId);
    },
    applyCheckpointUnavailable(payload: CheckpointUnavailableEvent | null): void {
      if (!payload || payload.threadId !== thread?.id) return;
      diffPanel.markCheckpointsUnavailable(payload.reason);
      diffPanel.setError('Workspace is not a git repo. Checkpoint diffs are unavailable.');
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
    get latestSettledTurn() { return latestSettledTurn; },
    /**
     * Bounded recent subagent notifications. No UI consumer today; stored
     * so a future tray / toast surface can subscribe without the pane
     * needing a new channel.
     */
    get subagentNotifications() { return subagentNotifications; },
    /**
     * Inclusive floor of the loaded history window. Consumers use this
     * to render "Load older messages" and, in scroll-to-item flows, to
     * decide whether a target coordinate is already in view.
     */
    get oldestLoadedTurnIndex() { return oldestLoadedTurnIndex; },
    get hasMoreHistory() { return hasMoreHistory; },
    get loadingOlder() { return loadingOlder; },
    /**
     * Scroll-to-item intent published by pane-level callers (search
     * hits, plan sidebar clicks, tray rows). MessageTimeline reacts to
     * nonce changes — the timeline compares the observed nonce against
     * the current value and runs `scrollToItem(itemId)` when it
     * advances. `itemId === ''` means "no request".
     */
    get scrollToItemRequest() { return scrollToItemRequest; },
    get channelMessages() { return channelState.messages; },
    get channelStatus() { return channelState.status; },
    get pendingClarification() { return designState.pendingClarification; },
    get activeOptionSet() { return designState.activeOptionSet; },
    get designViewport() { return designState.designViewport; },
    get activeRhsPanel() { return rhsPanelSlot.activePanel; },
    get rhsSidebarWidth() { return rhsPanelSlot.width; },
    get showPlanSidebar() { return rhsPanelSlot.activePanel?.kind === 'plan'; },
    get showDesignPreviewPanel() { return rhsPanelSlot.activePanel?.kind === 'design-preview'; },
    get activeDiffPayload() {
      const panel = rhsPanelSlot.activePanel;
      if (panel?.kind !== 'diff-payload') return null;
      if (panel.filePath === undefined) return { payloadId: panel.payloadId };
      return { payloadId: panel.payloadId, filePath: panel.filePath };
    },
    get diffSidebarRestoreState() { return rhsPanelSlot.diffPayloadRestoreState; },
    /** Diagnostic — total snapshots held by the RHS panel slot. */
    get rhsPanelSnapshotCount() { return rhsPanelSlot.snapshotCount; },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      // Bump the switch generation BEFORE any synchronous mutation so
      // any in-flight prior switch's late resolutions are invalidated
      // before we touch pane state. `gen` is read by every async leg
      // below and by the outer finally to decide whether the spinner
      // can be cleared (a concurrent switch keeps it up).
      const gen = ++switchGeneration;
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
        const result = await runParallelLoad(newThread, gen, cached, sliceAnchorId, liveStateHydrationToken);
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
      let liveStateHydrationToken = beginThreadLiveStateHydration(currentThread.id);
      try {
        try {
          const paged = await ListRecentThreadItems(currentThread.id, 0);
          if (gen !== switchGeneration) return;
          const nextItems = reconcileItemWindow(
            itemsForThread((paged.items ?? []) as Item[], currentThread.id),
            items,
          );
          replaceTimelineItems(nextItems);
          oldestLoadedTurnIndex = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
          hasMoreHistory = paged.hasMore ?? false;
        } catch (err) {
          if (gen !== switchGeneration) return;
          console.error('Failed to refresh thread items after gap:', err);
          return;
        }
        try {
          const recent = (await ListRecentTurns(currentThread.id, 2)) as TurnRow[] | null;
          if (gen !== switchGeneration) return;
          if (recent && recent.length > 0) {
            const settled = recent.find(
              (row) => row.completedAt !== null && row.completedAt !== undefined,
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
        await hydrateThreadLiveState(currentThread.id, gen, liveStateHydrationToken);
        liveStateHydrationToken = 0;
      } finally {
        if (liveStateHydrationToken !== 0) {
          finishThreadLiveStateHydration(currentThread.id, liveStateHydrationToken);
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
        clearWorktreeIntent(draftPlaceholder.id);
        clearRuntimeModeDraft(draftPlaceholder.id);
      }
      thread = null;
      draftPlaceholder = null;
      replaceTimelineItems([]);
      rowUiState.clear();
      pendingInteractiveState.clear();
      contextWindow = null;
      providerBanner = undefined;
      generalError = null;
      generalErrorKind = null;
      loading = false;
      sendInFlight = false;
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
      oldestLoadedTurnIndex = null;
      hasMoreHistory = false;
      loadingOlder = false;
      // See switchThread: both `pagingGeneration` and
      // `scrollToItemRequest.nonce` stay monotonic for the pane's
      // lifetime so no consumer observes a regressed counter.
      diffPanel.clearForThread();
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
      const created = (await CreateThread({
        projectId: placeholder.projectId,
        mode: placeholder.mode,
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
     * callers so composer-input, paste/upload, send, and toolbar pickers
     * don't each race to `CreateThread`. Resolves to null when the pane
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
      const existingId = draftPlaceholder ? null : thread?.id ?? null;
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
          // Re-key any intent staged against the placeholder id BEFORE we
          // adopt the real thread — worktree/branch picks and runtime-mode
          // toggles made on the placeholder otherwise become orphaned when
          // the lookups switch to the materialized thread id.
          migrateWorktreeIntent(placeholderId, created.id);
          migrateRuntimeModeDraft(placeholderId, created.id);
          seedDefaultWorktreeIntentForDraft(created);
          prependThread(created);
          this.adoptMaterializedDraftThread(created);
          const draftStore = getComposerDraftForPane(paneId);
          if (draftStore) await draftStore.adoptThread(created.id);
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
      const floor = oldestLoadedTurnIndex;
      if (floor === null) return loadOlderResult('noop');

      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      loadingOlder = true;
      try {
        const paged = await ListItemsBeforeTurn(currentThread.id, floor, LOAD_OLDER_ITEM_BUDGET);
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return loadOlderResult('stale');
        const prepend = itemsForThread((paged.items ?? []) as Item[], currentThread.id);
        const currentIds = new Set(items.map((item) => item.id));
        const insertedRows = prepend.some((item) => !currentIds.has(item.id));
        const currentFirst = items[0] ?? null;
        const insertedBeforeWindow = currentFirst === null
          ? insertedRows
          : prepend.some((item) => (
              !currentIds.has(item.id)
              && compareItemsByTimelinePosition(item, currentFirst) < 0
            ));
        const next = mergeItemsById(prepend, items);
        replaceTimelineItems(next);
        const nextFloor =
          paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : floor;
        oldestLoadedTurnIndex = nextFloor;
        // Progress guard. If the backend returned no items AND the floor
        // didn't decrease, another click would fire the same query for
        // the same range. Force hasMore=false so the UI stops offering a
        // button that can't actually load anything. A later in-flight
        // upsert that lands an older item will re-enable paging through
        // the normal streaming path.
        if (prepend.length === 0 && nextFloor >= floor) {
          hasMoreHistory = false;
        } else {
          hasMoreHistory = paged.hasMore ?? false;
        }
        return loadOlderResult('loaded', insertedBeforeWindow, insertedRows);
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return loadOlderResult('stale');
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
      if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
      if (!fetched || !fetched.id) return false;
      // Defense-in-depth: the backend already filters by threadId, but a
      // mislayered binding or a future cache that returns stale rows
      // shouldn't cross-pollute between panes.
      if (fetched.threadId !== currentThread.id) return false;

      // Race: another upsert or loadOlder might have pulled the item in
      // between our check and the backend round-trip. Re-check before
      // paging in a whole turn window we don't need.
      if (items.some((it) => it.id === itemID)) return true;

      const currentFloor = oldestLoadedTurnIndex;
      if (currentFloor !== null && fetched.turnIndex >= currentFloor) {
        // Nominally in-window per the floor invariant. Double-check the
        // in-memory state in case an upsert got dropped — never claim
        // success without a row the DOM can actually scroll to.
        return items.some((it) => it.id === itemID);
      }

      // Load items between the target turn and the existing floor.
      // The third parameter is an item-budget; cap at
      // LOAD_UNTIL_ITEM_HARD_CAP so a search hit deep in a long
      // thread is bounded to one round-trip. If the target lives
      // below the cap, this load returns false and callers show the
      // existing "couldn't locate that message" toast — the
      // alternative (unbounded item budget) ran the risk of a single
      // jump pulling the entire history.
      const beforeTurn = currentFloor ?? fetched.turnIndex + 1;
      const itemBudget = LOAD_UNTIL_ITEM_HARD_CAP;

      loadingOlder = true;
      try {
        const paged = await ListItemsBeforeTurn(currentThread.id, beforeTurn, itemBudget);
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
        const prepend = itemsForThread((paged.items ?? []) as Item[], currentThread.id);
        const next = mergeItemsById(prepend, items);
        replaceTimelineItems(next);
        oldestLoadedTurnIndex =
          paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : currentFloor;
        hasMoreHistory = paged.hasMore ?? false;
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
        console.error('loadUntilItem ListItemsBeforeTurn failed:', err);
        addToast('error', 'Failed to load older messages');
        return false;
      } finally {
        // Match loadOlder's unconditional reset — see comment there.
        loadingOlder = false;
      }
      return items.some((it) => it.id === itemID);
    },

    /**
     * Publish a scroll-to-item intent for the MessageTimeline to pick
     * up. Consumers call this instead of reaching into the timeline
     * directly — keeps DOM operations inside the component that owns
     * the scroll container, and lets the pane mediate window loading
     * if the target isn't visible yet. The timeline handler is
     * responsible for awaiting `loadUntilItem` before scrolling.
     */
    requestScrollToItem(itemID: string, options: ScrollToItemOptions = {}): void {
      if (!itemID) return;
      scrollToItemRequest = {
        itemId: itemID,
        nonce: scrollToItemRequest.nonce + 1,
        behavior: options.behavior ?? 'instant',
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
      if (thread) {
        threadItemCache.evict(thread.id);
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
      if (thread) {
        threadItemCache.evict(thread.id);
      }
      return removed;
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

      let nextSummary = current.summary + evt.delta;
      if (current.kind === 'thinking') {
        nextSummary = trimToTailRunes(nextSummary, THINKING_TAIL_RUNES);
      }
      // Replace the entry rather than mutating in place: threadItemCache
      // snapshots items by reference and the chat surface depends on
      // reference equality for virtua's per-row ResizeObserver to stay
      // quiet on unchanged rows. The streaming row is genuinely growing,
      // so a fresh reference for the row whose content changed is the
      // correct signal.
      const nextItem = { ...current, summary: nextSummary, updatedAt: evt.updatedAt };
      items[index] = nextItem;
      if (nextItem.kind === 'thinking') {
        rowUiState.appendLivePayloadDeltaForItem(
          nextItem.id,
          THINKING_PAYLOAD_EXPANSION_STATE_KEY,
          evt.delta,
          thinkingPayloadVersionForItem(nextItem),
          current.summary,
        );
      }
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
      // like a content change to virtua / threadItemCache.
      items[index] = { ...current, meta: evt.meta };
    },

    // ---- Per-row UI state (survives virtua remount) ----
    expansionStateFor: rowUiState.expansionStateFor,
    expansionStateForPayload: rowUiState.expansionStateForPayload,
    isSubagentGroupExpanded: rowUiState.isSubagentGroupExpanded,
    toggleSubagentGroupExpanded: rowUiState.toggleSubagentGroupExpanded,
    attachmentCacheFor: rowUiState.attachmentCacheFor,

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

    get liveTodo() { return liveTodoState.liveTodo; },
    get liveTodoShowAll() { return liveTodoState.liveTodoShowAll; },

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

    get activityRailTodosOpen() { return liveTodoState.activityRailTodosOpen; },
    get activityRailBackgroundOpen() { return liveTodoState.activityRailBackgroundOpen; },

    /** Toggle the Todos accordion body inside the activity rail. */
    toggleActivityRailTodos(): void {
      liveTodoState.toggleActivityRailTodos(thread?.id ?? null);
    },

    /** Toggle the Background accordion body inside the activity rail. */
    toggleActivityRailBackground(): void {
      liveTodoState.toggleActivityRailBackground(thread?.id ?? null);
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
        subagentNotifications = next.slice(next.length - subagentNotificationLimit);
      } else {
        subagentNotifications = next;
      }
    },

    replaceThread(nextThread: Thread): void {
      thread = nextThread;
      contextWindow = seedContextWindow(nextThread);
    },

    toggleTerminal(): void {
      // Bottom drawer mount/unmount reflows the chat column. Hold a
      // brief lease so the controller's content-RO sync-pin no-ops
      // while the column's clientHeight is settling.
      leaseDuringSettle(scrollController);
      showTerminal = !showTerminal;
    },

    setShowTerminal(value: boolean): void {
      if (value !== showTerminal) leaseDuringSettle(scrollController);
      showTerminal = value;
    },

    toggleDiffPanel(): void {
      if (thread?.mode === 'design') return;
      if (diffPanel.open) activatePanel(null);
      else activatePanel({ kind: 'diff-checkpoint' });
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
      if (rhsPanelSlot.activePanel?.kind === 'design-preview') activatePanel(null);
      else activatePanel({ kind: 'design-preview' });
    },

    setShowDesignPreviewPanel(value: boolean): void {
      if (thread?.mode !== 'design') return;
      if (value) activatePanel({ kind: 'design-preview' });
      else if (rhsPanelSlot.activePanel?.kind === 'design-preview') activatePanel(null);
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
      activatePanel({ kind: 'diff-payload', payloadId: payload.payloadId, filePath: payload.filePath });
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
    async applyDesignOptionsUpdate(threadId: string, _setId: string): Promise<void> {
      await designState.applyDesignOptionsUpdate(() => thread, threadId);
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
