import type { Item, Thread } from '../types/models';
import type { Checkpoint } from '../types/checkpoint';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  PendingInteractiveRequests,
  TodoStep,
  ProviderStatusEvent,
  SubagentNotificationEvent,
  TokenUsageSummary,
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
  FeedbackBatch,
  SliderControl,
} from '../types/design';
import {
  GetThreadItem,
  GetThreadLiveState,
  LatestDesignOptionSet,
  ListPendingInteractiveRequests,
  ListItemsBeforeTurn,
  ListRecentThreadItems,
  ListRecentTurns,
  ListThreadCheckpoints,
  SwitchThread,
} from './bindings';
import {
  controlsKey,
  parseDesignAssistantPayloads,
} from '../utils/designAssistantPayload';
import { replaceThread } from './threads.svelte';
import {
  createPayloadExpansion,
  type PayloadExpansionHandle,
  type PayloadExpansionOptions,
} from '../components/chat/payloadExpansion.svelte';
import type {
  AttachmentPreviewCache,
  ImagePreviewItem,
} from '../utils/attachmentPreview.svelte';
import { leaseDuringSettle } from '../utils/scrollLeaseDuringTransition';

import { addToast } from './toast.svelte';
import { getSettings } from './settings.svelte';
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
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { ListThreadSliceAround } from './bindings';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  getActiveTurn,
  isThreadLiveStateHydrationCurrent,
  projectTurnCompleted,
  projectTurnStarted,
  replaceInteractiveRequestsForThread,
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
import type { ThreadLiveState } from '../../../bindings/agent-overflow/models';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';

/**
 * Default batch size for "Load older" fetches. Matches the initial window
 * size so a single paging click approximately doubles the loaded history.
 * The value is a turn count, not an item count; backend-side caps keep a
 * single page from exceeding reasonable item totals even if those turns
 * are unusually large.
 */
const LOAD_OLDER_TURN_BATCH = 50;

/**
 * Phase-1 fast-path slice size on `switchThread`. Roughly covers a
 * desktop viewport (10–15 rows) plus enough overscan for virtua's
 * measurement loop to land cleanly on the bottom or anchor. Phase 2
 * always fills in the rest of the window.
 */
const SLICE_AROUND_ITEM_COUNT = 50;

/**
 * Doherty perception threshold. A switch that completes inside this
 * window never paints the loading spinner — the view transitions
 * straight to the loaded content. Above the threshold, the spinner
 * fades in and stays until `loading=false`. 100ms is the standard
 * "instant to the user" budget across UX research.
 */
const SPINNER_THRESHOLD_MS = 100;

function sameRhsPanel(left: RhsPanel | null, right: RhsPanel | null): boolean {
  if (left === null || right === null) return left === right;
  if (left.kind !== right.kind) return false;
  if (left.kind !== 'diff-payload' || right.kind !== 'diff-payload') return true;
  return left.payloadId === right.payloadId && left.filePath === right.filePath;
}

function mergePendingRequests<T extends { requestId: string }>(
  snapshot: T[],
  current: T[],
  resolvedRequestIds: Set<string>,
): T[] {
  const merged: T[] = [];
  const seen = new Set<string>();
  for (const request of snapshot) {
    if (!request.requestId || resolvedRequestIds.has(request.requestId)) continue;
    merged.push(request);
    seen.add(request.requestId);
  }
  for (const request of current) {
    if (!request.requestId || resolvedRequestIds.has(request.requestId)) continue;
    if (seen.has(request.requestId)) continue;
    merged.push(request);
    seen.add(request.requestId);
  }
  return merged;
}

// ActiveTurn now lives in threadStatuses.svelte.ts (single source of
// truth for the global active-turn registry). Re-exported here so
// downstream importers (events.ts, panes, tests) don't have to rewire
// their imports for the move.
export type { ActiveTurn } from './threadStatuses.svelte';

/**
 * SettledTurn is the most recent completed turn's projection. ChatView uses
 * it to keep the active thread read, and trace/debug surfaces use it to
 * describe the current pane state. Populated from `provider:turn_completed`
 * pushes or, on thread switch, from the most recent `ListRecentTurns` row
 * whose `completedAt` is non-null.
 */
export interface SettledTurn {
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  /**
   * Provider message id of the final assistant message of this turn
   * (Claude `msg_…`, Codex equivalent). Multi-round logical turns
   * overwrite this on each round so the value is always the LAST
   * message — see backend `UpdateTurnLatePayload` per-column
   * semantics. Null when the provider didn't report one (e.g.
   * session-died synthesis before any assistant envelope).
   */
  assistantMessageId: string | null;
  /** Parsed from triage's token_usage_json. null on malformed / missing input. */
  tokenUsage: TokenUsageSummary | null;
  aborted: boolean;
  errorMessage: string;
}

/**
 * LiveTodo is the snapshot the activity rail's Todos segment renders.
 * Populated from `provider:todo_update` events (Claude TodoWrite reroute
 * + Codex update_plan, both normalised in the parser). Survives turn
 * boundaries by design: the segment keeps showing while items remain
 * incomplete and auto-hides on a timer when every step is `completed`.
 */
export interface LiveTodo {
  steps: TodoStep[];
}

/**
 * LIVE_TODO_AUTOHIDE_MS is how long the snapshot lingers after every
 * step is `completed` before the auto-hide timer clears it. Long
 * enough for the user to see the satisfying all-done state, short
 * enough that the segment doesn't squat on the rail indefinitely.
 */
export const LIVE_TODO_AUTOHIDE_MS = 5_000;

/**
 * Per-thread live-todo dropdown UI preferences (show-all reveal).
 * Module-scoped so a thread switch can save the outgoing thread's
 * state and restore the incoming thread's. Lives in process memory by
 * design — survives thread switches within a session, dies on app
 * restart, no SQLite roundtrip.
 */
interface LiveTodoUiPrefs {
  showAll: boolean;
}
const liveTodoUiPrefs = new Map<string, LiveTodoUiPrefs>();

function readLiveTodoUiPrefs(threadID: string | null): LiveTodoUiPrefs {
  if (!threadID) return { showAll: false };
  return liveTodoUiPrefs.get(threadID) ?? { showAll: false };
}

function writeLiveTodoUiPrefs(threadID: string | null, prefs: LiveTodoUiPrefs): void {
  if (!threadID) return;
  liveTodoUiPrefs.set(threadID, prefs);
}

/**
 * Drop a thread's live-todo UI prefs. Called from the thread-removal
 * path so a deleted thread doesn't leave a permanent entry in the
 * module-scoped prefs map. Bounded growth would otherwise be tied to
 * the count of distinct threads ever toggled in a session, which is
 * fine in practice but accumulates across long-running sessions.
 */
export function dropLiveTodoUiPrefs(threadID: string | null): void {
  if (!threadID) return;
  liveTodoUiPrefs.delete(threadID);
}

/**
 * Test-only reset for the live-todo UI prefs map. The map is
 * intentionally module-scoped so per-thread open/closed state survives
 * thread switches in production; tests need to clear it between cases
 * so cross-test pollution doesn't flip a fresh pane's defaults.
 * Production code never calls this — same pattern as the markdown
 * enhancement caches in `markdownEnhance.ts`.
 */
export function __resetLiveTodoUiPrefsForTest(): void {
  liveTodoUiPrefs.clear();
}

/**
 * Per-thread Activity Rail expansion state. The rail itself appears
 * only when there's active work (turn / todos / background tasks);
 * these flags govern whether the Todos and Background section bodies
 * below the rail are open. Independent toggles — both can be open at
 * once. Same shape and lifecycle rules as `liveTodoUiPrefs`: lives in
 * process memory, survives thread switches, dies on app restart.
 */
interface ActivityRailUiPrefs {
  todosOpen: boolean;
  backgroundOpen: boolean;
}
const activityRailUiPrefs = new Map<string, ActivityRailUiPrefs>();

function readActivityRailUiPrefs(threadID: string | null): ActivityRailUiPrefs {
  if (!threadID) return { todosOpen: false, backgroundOpen: false };
  return activityRailUiPrefs.get(threadID) ?? { todosOpen: false, backgroundOpen: false };
}

function writeActivityRailUiPrefs(threadID: string | null, prefs: ActivityRailUiPrefs): void {
  if (!threadID) return;
  activityRailUiPrefs.set(threadID, prefs);
}

export function dropActivityRailUiPrefs(threadID: string | null): void {
  if (!threadID) return;
  activityRailUiPrefs.delete(threadID);
}

export function __resetActivityRailUiPrefsForTest(): void {
  activityRailUiPrefs.clear();
}

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
 * `notifyContentMaybeGrew` is currently called only from chat's
 * `ChatView` (composer-overlay growth changes the timeline's bottom
 * padding without growing the contentEl). Discussion does not call it
 * today — its textarea sits in a separate `shrink-0` flex section —
 * but the seam is here so a future Discussion composer-height story
 * could reach the controller the same way chat does.
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

/**
 * TurnRow mirrors the Go `store.Turn` shape returned by the
 * `ListRecentTurns` binding. Kept as a local interface rather than an
 * import from `../types/models` because this is the only consumer and
 * inlining keeps the rehydration path self-contained. `completedAt` is
 * nullable / optional: Go's `json:"completedAt,omitempty"` omits the
 * field entirely when it's NULL in the DB, so the frontend must handle
 * both `null` and `undefined` as "in-flight / crashed."
 */
interface TurnRow {
  turnId: string;
  threadId: string;
  turnIndex: number;
  startedAt: number;
  completedAt?: number | null;
  stopReason?: string;
  assistantMessageId?: string;
  tokenUsageJson?: string;
  errorMessage?: string;
}

type LiveTodoSnapshot = NonNullable<ThreadLiveState['todo']>;

interface LiveStateHydrationGuard {
  activeTurnAtRequest: ActiveTurn | null;
  queueRevisionAtRequest: number;
  liveTodoRevisionAtRequest: number;
}

/**
 * Build a SettledTurn from a persisted TurnRow. Only called with rows
 * where `completedAt` is populated. Token usage is parsed via
 * `parseTokenUsage`, which is tolerant of malformed input.
 */
function turnRowToSettled(row: TurnRow): SettledTurn {
  return {
    turnId: row.turnId,
    turnIndex: row.turnIndex,
    startedAt: row.startedAt,
    // Narrowed by caller — `completedAt` is guaranteed non-null/undefined
    // at this point, so coerce to number with a sane fallback.
    completedAt: row.completedAt ?? 0,
    stopReason: row.stopReason ?? '',
    assistantMessageId: row.assistantMessageId && row.assistantMessageId !== ''
      ? row.assistantMessageId
      : null,
    tokenUsage: parseTokenUsage(row.tokenUsageJson),
    // Persisted rows don't carry the aborted flag as its own column; the
    // stop_reason='interrupted' value is the rehydrated signal. UI
    // consumers can branch on stopReason directly for the aborted case.
    aborted: row.stopReason === 'interrupted',
    errorMessage: row.errorMessage ?? '',
  };
}

/**
 * Parse a token-usage JSON string produced by triage into the typed
 * summary the pane exposes. Accepts either snake_case (Claude wire shape)
 * or camelCase (what triage passes through); malformed / empty input
 * returns null without throwing so the event listener can swallow
 * garbage from a misbehaving provider rather than crashing the pane.
 *
 * Exported so the `provider:turn_completed` listener in events.ts can
 * parse the wire payload's `tokenUsage` string through the same code
 * path the thread-switch rehydration uses — one parser, two call sites.
 */
export function parseTokenUsage(raw: string | null | undefined): TokenUsageSummary | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (!parsed || typeof parsed !== 'object') return null;
    const pickNumber = (...keys: string[]): number | undefined => {
      for (const key of keys) {
        const v = parsed[key];
        if (typeof v === 'number' && Number.isFinite(v)) return v;
      }
      return undefined;
    };
    const inputTokens = pickNumber('inputTokens', 'input_tokens') ?? 0;
    const outputTokens = pickNumber('outputTokens', 'output_tokens') ?? 0;
    const summary: TokenUsageSummary = { inputTokens, outputTokens };
    const cacheRead = pickNumber('cacheReadInputTokens', 'cache_read_input_tokens');
    if (cacheRead !== undefined) summary.cacheReadInputTokens = cacheRead;
    const cacheCreation = pickNumber('cacheCreationInputTokens', 'cache_creation_input_tokens');
    if (cacheCreation !== undefined) summary.cacheCreationInputTokens = cacheCreation;
    const cost = pickNumber('totalCostUsd', 'total_cost_usd');
    if (cost !== undefined) summary.totalCostUsd = cost;
    return summary;
  } catch {
    return null;
  }
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

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, unified timeline items, approvals,
 * context/banner state, and mode-specific UI. Components receive a
 * ThreadPane as a prop.
 */
export function createThreadPane() {
  let thread: Thread | null = $state(null);
  let items: Item[] = $state([]);
  let timelineRevision = $state(0);
  let liveItemSummaries: Record<string, string> = $state({});
  // Bumps once per coalesced delta flush. Auto-follow consumers depend
  // on this so a streaming row that grows in viewport (no new items, no
  // timelineRevision tick) still re-pins to bottom while sticky.
  let liveDeltaRevision = $state(0);
  const liveDeltaChunks: Map<string, string[]> = new Map();
  // Per-itemId expansion state. Survives row remount (virtua's overscan
  // eviction would otherwise reset toggle + drop loaded chunks, forcing
  // a re-fetch from Go on every back-scroll). Cleared on thread switch.
  const expansionStates: Map<string, PayloadExpansionHandle> = new Map();
  // Per-groupKey subagent group expand state. ProposedPlanCard
  // expansion state is deliberately NOT lifted — it appears on rare item
  // types and the back-scroll remount frequency is low in practice. Lift
  // if profiling proves it.
  let subagentGroupExpanded: Set<string> = $state(new Set());
  // Per-itemId attachment blob cache: outer key=itemId, inner key=attachmentId.
  // The pane owns the blob URLs so they survive virtua's overscan eviction
  // (which would otherwise revoke them in UserMessage's onDestroy and force
  // a re-fetch+re-allocate on the next back-scroll). Revoked on thread switch.
  const attachmentBlobs: Map<string, Map<string, ImagePreviewItem>> = new Map();
  const itemStatusById: Map<string, Item['status']> = new Map();
  const itemIndexById: Map<string, number> = new Map();
  const itemSummaryById: Map<string, string> = new Map();
  const itemIdsByTurn: Map<number, string[]> = new Map();
  let liveSummaryFrame: number | null = null;
  let pendingApprovals: ApprovalRequest[] = $state([]);
  let pendingUserInputs: UserInputRequest[] = $state([]);
  const resolvedApprovalIds = new Set<string>();
  const resolvedUserInputIds = new Set<string>();
  let contextWindow: ContextWindow | null = $state(null);
  let providerBanner: ProviderStatusEvent | null = $state(null);
  // generalError is the grab-bag pane-level error slot surfaced by
  // ProviderStatusBanner for non-wire failures: thread load failures,
  // composer send failures, git action failures, reconnect failures.
  // It is deliberately distinct from providerBanner (which mirrors the
  // provider's own session/auth/rate-limit state) — consumers treat
  // them as two independent reasons to show the top-of-pane banner.
  let generalError: string | null = $state(null);
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
  let showTerminal: boolean = $state(false);
  // Diff panel is per-pane; created once and reset on thread switch so its
  // caches don't leak between threads.
  const diffPanel: DiffPanelState = createDiffPanelState();

  // Channel state (only populated for discussion threads).
  let channelMessages: ChannelMessage[] = $state([]);
  let channelStatus: 'open' | 'concluded' | 'closed' | null = $state(null);

  // Design-mode state (only populated when thread.mode === 'design').
  //
  // Layout:
  //   - pendingClarification: non-null when the agent has emitted a
  //     ClarificationRequest payload and is waiting for the user to pick
  //     answers before continuing.
  //   - exposedControls: agent-published slider knobs the user can tweak
  //     after a design iteration lands. Replaces the previous control set
  //     wholesale on each ExposeControls signal.
  //   - activeOptionSet: when non-null, render the small N-up options grid
  //     instead of (or beside) the main preview iframe.
  //   - designViewport: width toggle for the main preview iframe.
  let pendingClarification: ClarificationRequest | null = $state(null);
  let exposedControls: SliderControl[] = $state([]);
  let activeOptionSet: ActiveOptionSet | null = $state(null);
  let designViewport: DesignViewport = $state('desktop');

  // Dedup keys for the assistant-payload parser. The design agent emits
  // structured `aoflow-design` blocks inside its assistant text;
  // upsertItemsBatch re-applies the same item summary on every streaming
  // delta and on retroactive replays, so we track which payload we've
  // already mapped onto pane state and skip re-fires. Keys reset in
  // clearDesign() / switchThread() so a new thread starts fresh.
  let lastClarificationRequestId: string | null = null;
  let lastExposedControlsKey: string | null = null;

  // Top-level mode tab (Chat | Design). Drives the segmented control in
  // ChatHeader and the layout decision in ChatView when no thread is loaded
  // (the empty-state pane needs to know whether the user is sitting on the
  // Chat tab or the Design tab). Once a thread loads, switchThread() syncs
  // this from thread.mode so the tab tracks the active thread's type.
  // Discussion threads bypass the top-tab UI entirely (DiscussionView owns
  // its own surface) so we leave activeTab unchanged in that case — when the
  // user navigates away the tab still carries the user's last intent.
  let activeTab: 'chat' | 'design' = $state('chat');

  // Shared right-side panel slot. The shell width and the active panel are
  // saved per thread so plan/diff/payload views swap inside one stable pane
  // instead of mounting separate sidebars with separate width stores.
  const rhsPanelSlot: RhsPanelSlot = createRhsPanelSlot();

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
    // column when they open or close. Hold a brief lease so the spring
    // controller's chase + content-RO re-pin both no-op while the
    // column's clientWidth is settling — preventing the timeline from
    // yanking mid-transition.
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
  // Live-todo panel state. Independent of activeTurn — the panel
  // persists past turn-end if items remain incomplete and only
  // disappears when the agent marks every step completed (auto-hide
  // timer below) or the user switches threads. Sourced from
  // `provider:todo_update` events; both Claude TodoWrite and Codex
  // update_plan funnel through that channel after parser
  // normalisation. Lost on app restart by design.
  let liveTodo: LiveTodo | null = $state(null);
  let liveTodoShowAll = $state(false);
  let liveTodoAutoHideTimer: ReturnType<typeof setTimeout> | null = null;
  // Activity rail per-section open flags. The rail itself derives
  // visibility from the union of working / todos / background state;
  // these flags govern only whether each accordion BODY is open.
  // Restored from `activityRailUiPrefs` on switchThread; default false.
  let activityRailTodosOpen = $state(false);
  let activityRailBackgroundOpen = $state(false);
  // Step texts that were on-screen when the previous all-completed todo list
  // auto-hid. The agent re-emits the FULL todo list on each update, so
  // without this set the next update would re-show the prior completed
  // items in the new panel. We subtract them from incoming snapshots so
  // each "logical todo cycle" cycle starts fresh from the user's viewpoint.
  // Bounded to keep long sessions with many cycles from leaking
  // memory; oldest entries drop first.
  let liveTodoCleared = new Set<string>();
  const liveTodoClearedCap = 1_000;
  let liveTodoRevision = 0;
  // Subagent notification log. The backend emits
  // `provider:subagent_notification` as a pass-through; no UI consumes it
  // today, but keeping a bounded in-pane log lets future surfaces (tray,
  // toast) subscribe without re-wiring the channel. We cap at a small
  // number of most-recent entries so the array can't grow unbounded in a
  // session that generates many notifications.
  let subagentNotifications: SubagentNotificationEvent[] = $state([]);
  const subagentNotificationLimit = 32;

  function clearLiveTodoState(): void {
    liveTodoRevision += 1;
    if (liveTodoAutoHideTimer !== null) {
      clearTimeout(liveTodoAutoHideTimer);
      liveTodoAutoHideTimer = null;
    }
    liveTodo = null;
    liveTodoCleared = new Set();
  }

  function shouldHydrateLiveTodoSnapshot(snapshot: LiveTodoSnapshot): boolean {
    if (!Array.isArray(snapshot.steps) || snapshot.steps.length === 0) {
      return false;
    }
    const allCompleted = snapshot.steps.every((step) => step.status === 'completed');
    if (!allCompleted) return true;
    const age = Date.now() - snapshot.updatedAt;
    return age >= 0 && age <= LIVE_TODO_AUTOHIDE_MS;
  }

  function setLiveTodoState(steps: TodoStep[]): void {
    liveTodoRevision += 1;
    if (liveTodoAutoHideTimer !== null) {
      clearTimeout(liveTodoAutoHideTimer);
      liveTodoAutoHideTimer = null;
    }
    const filtered = liveTodoCleared.size === 0
      ? steps
      : steps.filter(
          (s) => !(s.status === 'completed' && liveTodoCleared.has(s.step)),
        );
    if (filtered.length === 0) {
      liveTodo = null;
      return;
    }
    liveTodo = { steps: filtered };
    const allComplete = filtered.every((s) => s.status === 'completed');
    if (allComplete) {
      liveTodoAutoHideTimer = setTimeout(() => {
        if (liveTodo) {
          for (const s of liveTodo.steps) {
            liveTodoCleared.add(s.step);
          }
          if (liveTodoCleared.size > liveTodoClearedCap) {
            const arr = Array.from(liveTodoCleared);
            liveTodoCleared = new Set(arr.slice(arr.length - liveTodoClearedCap));
          }
        }
        liveTodo = null;
        liveTodoAutoHideTimer = null;
      }, LIVE_TODO_AUTOHIDE_MS);
    }
  }

  function sameActiveTurn(left: ActiveTurn | null, right: ActiveTurn | null): boolean {
    if (left === null || right === null) return left === right;
    return left.turnId === right.turnId
      && left.turnIndex === right.turnIndex
      && left.startedAt === right.startedAt;
  }

  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow ListRecentThreadItems from thread A cannot clobber
   * thread B's items when the user flips between them quickly.
   */
  let switchGeneration = 0;

  /**
   * Windowed-history state. The pane holds a contiguous tail of the
   * thread's items (last ~50 turns by default); older history loads
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

  function requestFrame(callback: () => void): number {
    if (typeof requestAnimationFrame === 'function') {
      return requestAnimationFrame(callback);
    }
    return window.setTimeout(callback, 0);
  }

  function cancelFrame(handle: number): void {
    if (typeof cancelAnimationFrame === 'function') {
      cancelAnimationFrame(handle);
    } else {
      window.clearTimeout(handle);
    }
  }

  function flushLiveDeltaChunks(): void {
    liveSummaryFrame = null;
    if (liveDeltaChunks.size === 0) return;
    const next = { ...liveItemSummaries };
    for (const [itemID, chunks] of liveDeltaChunks) {
      const persisted = itemSummaryById.get(itemID) ?? '';
      next[itemID] = (next[itemID] ?? persisted) + chunks.join('');
    }
    liveDeltaChunks.clear();
    liveItemSummaries = next;
    liveDeltaRevision++;
  }

  function scheduleLiveDeltaFlush(): void {
    if (liveSummaryFrame !== null) return;
    liveSummaryFrame = requestFrame(flushLiveDeltaChunks);
  }

  function resetLiveBuffers(): void {
    if (liveSummaryFrame !== null) {
      cancelFrame(liveSummaryFrame);
      liveSummaryFrame = null;
    }
    liveDeltaChunks.clear();
    liveItemSummaries = {};
  }

  // ---- Per-row UI state registries ----------------------------------
  //
  // virtua's overscan eviction unmounts row components when they scroll
  // far past the viewport; remounting reconstructs the snippet's local
  // state from scratch. For state the user expects to survive scrolling
  // (expand/collapse, loaded payload chunks, expanded directories), we
  // hoist it into pane-scoped registries here so the same record is
  // returned on every remount of the same itemId.
  //
  // Registries are cleared on thread switch (this is per-pane state and
  // there's no global LRU need; a single pane's max thread is bounded
  // by the thread's item count, which has its own loose memory ceiling
  // via the thread-windowing floor).
  //
  // Within the lifetime of a single thread, each expanded tool_call
  // holds its loaded payload chunks until the user collapses it or
  // switches threads. We deliberately do not auto-collapse open rows:
  // collapsing an item the user is reading changes transcript geometry
  // from outside the row's own interaction path, which fights the
  // virtualizer and creates visible jumps/flashes.

  function withExpansionRegistry(inner: PayloadExpansionHandle): PayloadExpansionHandle {
    return {
      get expanded() { return inner.expanded; },
      get loading() { return inner.loading; },
      get error() { return inner.error; },
      get previewData() { return inner.previewData; },
      get fullData() { return inner.fullData; },
      get totalSize() { return inner.totalSize; },
      get isComplete() { return inner.isComplete; },
      get payloadVersion() { return inner.payloadVersion; },
      get hasMore() { return inner.hasMore; },
      get displayData() { return inner.displayData; },
      toggle: () => inner.toggle(),
      expand: () => inner.expand(),
      ensureLoaded: () => inner.ensureLoaded(),
      collapse: () => { inner.collapse(); },
      showFull: () => inner.showFull(),
      retry: () => inner.retry(),
      reset: () => { inner.reset(); },
      setPayloadVersion: (version: unknown) => { inner.setPayloadVersion(version); },
    };
  }

  /**
   * Look up or lazily construct the PayloadExpansion handle for an
   * item. The handle's payload-id and thread-id sources read through
   * to the live `Item` reference each time, so post-mount enrichment
   * (a tool_completion gaining its `output_file` after the fact) is
   * picked up automatically without a reset.
   */
  function expansionStateFor(
    item: Item,
    options: Pick<PayloadExpansionOptions, 'loadMode'> = {},
  ): PayloadExpansionHandle {
    const key = 'i:' + item.id + ':' + (options.loadMode ?? 'preview');
    let cached = expansionStates.get(key);
    if (cached) return cached;
    const id = item.id;
    const getCurrentItem = (): Item | undefined => {
      const idx = itemIndexById.get(id);
      return idx === undefined ? undefined : items[idx];
    };
    const inner = createPayloadExpansion(
      () => getCurrentItem()?.payloadId,
      () => getCurrentItem()?.threadId,
      {
        payloadVersion: () => getCurrentItem()?.updatedAt,
        loadMode: options.loadMode,
      },
    );
    cached = withExpansionRegistry(inner);
    expansionStates.set(key, cached);
    return cached;
  }

  /**
   * Payload-keyed expansion handle. Used by sub-row components like
   * `LazyContentBlock` that operate on a payload reference without
   * needing a parent Item context. Returns a stable handle for the
   * same `(payloadId, threadId)` pair across remounts.
   */
  function expansionStateForPayload(
    payloadId: string,
    threadId: string,
    payloadVersion?: unknown,
  ): PayloadExpansionHandle {
    const key = 'p:' + payloadId;
    let cached = expansionStates.get(key);
    if (cached) {
      cached.setPayloadVersion(payloadVersion);
      return cached;
    }
    const inner = createPayloadExpansion(
      () => payloadId,
      () => threadId,
    );
    inner.setPayloadVersion(payloadVersion);
    cached = withExpansionRegistry(inner);
    expansionStates.set(key, cached);
    return cached;
  }

  function isSubagentGroupExpanded(groupKey: string): boolean {
    return subagentGroupExpanded.has(groupKey);
  }

  /**
   * Cache view scoped to a single user-message item. UserMessage uses this
   * via `createAttachmentPreviews({ cache: pane.attachmentCacheFor(item.id) })`
   * so blob URLs persist through virtua remount.
   */
  function attachmentCacheFor(itemId: string): AttachmentPreviewCache {
    let inner = attachmentBlobs.get(itemId);
    if (!inner) {
      inner = new Map<string, ImagePreviewItem>();
      attachmentBlobs.set(itemId, inner);
    }
    const innerRef = inner;
    return {
      get(attachmentId: string): ImagePreviewItem | undefined {
        return innerRef.get(attachmentId);
      },
      set(attachmentId: string, preview: ImagePreviewItem): void {
        innerRef.set(attachmentId, preview);
      },
    };
  }

  function disposeAttachmentBlobs(): void {
    for (const inner of attachmentBlobs.values()) {
      for (const preview of inner.values()) {
        if (preview.url.startsWith('blob:')) URL.revokeObjectURL(preview.url);
      }
    }
    attachmentBlobs.clear();
  }

  function toggleSubagentGroupExpanded(groupKey: string): boolean {
    const next = new Set(subagentGroupExpanded);
    const willExpand = !next.has(groupKey);
    if (willExpand) next.add(groupKey); else next.delete(groupKey);
    subagentGroupExpanded = next;
    return willExpand;
  }

  /**
   * Clears all per-row UI state registries. Called from `switchThread`.
   * Attachment blobs are explicitly revoked because they hold external
   * resources (object URLs); the other registries hold no external
   * resources and just drop their entries.
   */
  function clearRowUiState(): void {
    expansionStates.clear();
    subagentGroupExpanded = new Set();
    disposeAttachmentBlobs();
  }

  function rebuildItemIndexes(nextItems: Item[]): void {
    itemStatusById.clear();
    itemIndexById.clear();
    itemSummaryById.clear();
    itemIdsByTurn.clear();
    for (let index = 0; index < nextItems.length; index += 1) {
      const item = nextItems[index];
      itemStatusById.set(item.id, item.status);
      itemIndexById.set(item.id, index);
      itemSummaryById.set(item.id, item.summary);
      appendItemIdToTurn(item.turnIndex, item.id);
    }
  }

  function appendItemIdToTurn(turnIndex: number, itemId: string): void {
    const ids = itemIdsByTurn.get(turnIndex);
    if (ids) {
      ids.push(itemId);
      return;
    }
    itemIdsByTurn.set(turnIndex, [itemId]);
  }

  function addUniqueItemIdToTurn(turnIndex: number, itemId: string): void {
    const ids = itemIdsByTurn.get(turnIndex);
    if (ids) {
      if (!ids.includes(itemId)) ids.push(itemId);
      return;
    }
    itemIdsByTurn.set(turnIndex, [itemId]);
  }

  function removeItemIdFromTurn(turnIndex: number, itemId: string): void {
    const ids = itemIdsByTurn.get(turnIndex);
    if (!ids) return;
    const next = ids.filter((id) => id !== itemId);
    if (next.length > 0) {
      itemIdsByTurn.set(turnIndex, next);
    } else {
      itemIdsByTurn.delete(turnIndex);
    }
  }

  function compareItemsByTimelinePosition(a: Item, b: Item): number {
    if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
    if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
    return 0;
  }

  function applyLiveStateForUpsert(item: Item, nextLive: Record<string, string>): boolean {
    if (item.status !== 'streaming') {
      const hadLiveSummary = nextLive[item.id] !== undefined;
      const hadDeltaChunks = liveDeltaChunks.delete(item.id);
      if (hadLiveSummary) {
        delete nextLive[item.id];
      }
      return hadLiveSummary || hadDeltaChunks;
    }

    if (nextLive[item.id] !== undefined || !item.summary) {
      return false;
    }

    const pending = liveDeltaChunks.get(item.id)?.join('') ?? '';
    liveDeltaChunks.delete(item.id);
    nextLive[item.id] = item.summary + pending;
    return true;
  }

  function itemsForThread(nextItems: Item[] | null | undefined, threadId: string): Item[] {
    return (nextItems ?? []).filter((item) => item.threadId === threadId);
  }

  /**
   * Merge `incoming` into `current` by id, returning a fresh array
   * sorted by (turnIndex, itemIndex). Used by `loadOlder` /
   * `loadUntilItem` where the backend can legitimately re-return an
   * ancestor row that is already in the window (pulled in by the
   * initial load via the ancestor CTE). A naive prepend would either
   * duplicate the row or — if we filter dupes and still prepend —
   * reorder the timeline (a dropped ancestor that already sat above
   * the tail would leave the freshly prepended mid-turn row at
   * position 0). The sorted-merge keeps both invariants: no
   * duplicates, and stable (turnIndex, itemIndex) ordering.
   *
   * Returns the original `current` reference when `incoming` is
   * empty OR every incoming row is already present, so callers can
   * skip the reactive write and associated turn-diff rebuild.
   */
  function mergeItemsById(incoming: Item[], current: Item[]): Item[] {
    if (incoming.length === 0) return current;
    const byId = new Map<string, Item>();
    for (const it of current) byId.set(it.id, it);
    let changed = false;
    for (const it of incoming) {
      const existing = byId.get(it.id);
      if (existing !== it) {
        byId.set(it.id, it);
        changed = true;
      }
    }
    if (!changed) return current;
    const merged = Array.from(byId.values());
    merged.sort(compareItemsByTimelinePosition);
    return merged;
  }

  /**
   * Like `mergeItemsById` but only ADDS items not already present —
   * existing rows keep their current reference, and the merged result
   * is RE-SORTED by `(turnIndex, itemIndex)` so additions slot into
   * the right transcript position. Used by the two-phase `switchThread`
   * load: phase 1 (or a cache hit) seeds the timeline, then phase 2
   * fills in the remaining window items without replacing rows that
   * have already been updated by streamed events that landed mid-load.
   * Reference equality on unchanged rows keeps virtua's per-row
   * ResizeObserver from firing spuriously.
   *
   * Triage's contract is "persist then emit", so any in-flight stream
   * event during phase 2 has already been baked into SQLite by the time
   * phase 2's SQL runs. The phase-2 row therefore equals (or is older
   * than) the row already in `current`; preferring `current` is the
   * correct choice in either case.
   *
   * Returns the original `current` reference when every incoming row is
   * already present, so callers can skip the reactive write and the
   * timeline-revision bump.
   */
  function mergeMissingItemsById(incoming: Item[], current: Item[]): Item[] {
    if (incoming.length === 0) return current;
    if (current.length === 0) {
      const sorted = incoming.slice();
      sorted.sort(compareItemsByTimelinePosition);
      return sorted;
    }
    const presentIds = new Set<string>();
    for (const it of current) presentIds.add(it.id);
    const additions: Item[] = [];
    for (const it of incoming) {
      if (!presentIds.has(it.id)) {
        additions.push(it);
      }
    }
    if (additions.length === 0) return current;
    const merged = current.concat(additions);
    merged.sort(compareItemsByTimelinePosition);
    return merged;
  }

  function seedContextWindow(nextThread: Thread | null): ContextWindow | null {
    const raw = nextThread?.lastTokenUsage?.trim();
    if (!raw) {
      if (!nextThread?.contextWindow) return null;
      return normalizeContextWindowForThread({
        usedTokens: 0,
        maxTokens: nextThread.contextWindow,
        usedPercentage: 0,
      }, nextThread);
    }
    try {
      const parsed = JSON.parse(raw) as {
        usedTokens?: number;
        maxTokens?: number;
        contextPercent?: number;
        autoCompactPercent?: number;
        autoCompactTokenLimit?: number;
      };
      if (typeof parsed.usedTokens !== 'number') return null;
      return normalizeContextWindowForThread({
        usedTokens: parsed.usedTokens,
        maxTokens: parsed.maxTokens,
        usedPercentage: parsed.contextPercent,
        autoCompactPercent: parsed.autoCompactPercent,
        autoCompactTokenLimit: parsed.autoCompactTokenLimit,
      }, nextThread);
    } catch {
      return null;
    }
  }

  function normalizeContextWindowForThread(data: ContextWindow, nextThread: Thread | null): ContextWindow {
    const maxTokens = data.maxTokens || nextThread?.contextWindow || 0;
    const percent = nextThread ? activeAutoCompactPercent(nextThread, maxTokens) : (data.autoCompactPercent ?? 0);
    return {
      usedTokens: data.usedTokens,
      maxTokens,
      usedPercentage: maxTokens > 0 ? (data.usedTokens / maxTokens) * 100 : data.usedPercentage,
      ...(percent > 0 ? {
        autoCompactPercent: percent,
        autoCompactTokenLimit: maxTokens > 0 ? Math.floor(maxTokens * percent / 100) : data.autoCompactTokenLimit,
      } : {}),
    };
  }

  function activeAutoCompactPercent(nextThread: Thread, effectiveContextWindow: number = nextThread.contextWindow ?? 0): number {
    // Per-thread override wins when set (chat-meter edit flow). Otherwise
    // fall back to the per-provider Settings value, then the absolute 90%
    // safety default if Settings hasn't been loaded yet.
    const isExtended = effectiveContextWindow >= 1_000_000;
    const override = isExtended
      ? nextThread.autoCompactExtendedPercent ?? 0
      : nextThread.autoCompactStandardPercent ?? 0;
    if (override > 0) return override;
    const settings = getSettings();
    const providerSetting =
      nextThread.provider === 'codex'
        ? isExtended
          ? settings.codexAutoCompactExtendedPercent
          : settings.codexAutoCompactStandardPercent
        : isExtended
          ? settings.claudeAutoCompactExtendedPercent
          : settings.claudeAutoCompactStandardPercent;
    return providerSetting > 0 ? providerSetting : 90;
  }

  function upsertItemsBatch(incoming: Item[]): void {
    if (incoming.length === 0) return;

    const currentThreadId = thread?.id ?? null;
    const next = items.slice();

    const nextLive = { ...liveItemSummaries };
    let changed = false;
    let liveChanged = false;
    let needsSort = false;

    for (const item of incoming) {
      if (currentThreadId !== null && item.threadId !== currentThreadId) continue;

      const existingIndex = itemIndexById.get(item.id);
      if (existingIndex !== undefined) {
        liveChanged = applyLiveStateForUpsert(item, nextLive) || liveChanged;
        const previous = next[existingIndex];
        next[existingIndex] = item;
        itemStatusById.set(item.id, item.status);
        itemSummaryById.set(item.id, item.summary);
        if (previous.turnIndex !== item.turnIndex) {
          removeItemIdFromTurn(previous.turnIndex, item.id);
          addUniqueItemIdToTurn(item.turnIndex, item.id);
        }
        if (compareItemsByTimelinePosition(previous, item) !== 0) {
          needsSort = true;
        }
        changed = true;
        continue;
      }

      // Window-floor guard for NEW items. Existing-id replacements above
      // already bypass this because an in-window row can legitimately be
      // corrected below the floor.
      if (oldestLoadedTurnIndex !== null && item.turnIndex < oldestLoadedTurnIndex) {
        continue;
      }

      liveChanged = applyLiveStateForUpsert(item, nextLive) || liveChanged;
      const previousTail = next.at(-1);
      if (previousTail && compareItemsByTimelinePosition(previousTail, item) > 0) {
        needsSort = true;
      }
      itemIndexById.set(item.id, next.length);
      next.push(item);
      itemStatusById.set(item.id, item.status);
      itemSummaryById.set(item.id, item.summary);
      appendItemIdToTurn(item.turnIndex, item.id);
      changed = true;
    }

    if (liveChanged) {
      liveItemSummaries = nextLive;
    }
    if (!changed) return;

    if (needsSort) {
      next.sort(compareItemsByTimelinePosition);
      rebuildItemIndexes(next);
    }
    items = next;
    timelineRevision++;

    // Design-mode side-channel: scan assistant text for structured
    // `aoflow-design` payloads and project them onto pane state. Cheap
    // when no payload is present (the parser short-circuits on the
    // missing fence prefix); dedup keys above prevent re-fires across
    // streaming deltas.
    if (thread?.mode === 'design') {
      for (const item of incoming) {
        applyDesignAssistantPayloadsForItem(item);
      }
    }
  }

  // applyDesignAssistantPayloadsForItem is the parser-to-state shim. It
  // runs once per upserted item in a design thread; the parser short-
  // circuits on text without an `aoflow-design` fence so non-design
  // assistant messages (or design messages without structured blocks)
  // pay essentially zero cost.
  function applyDesignAssistantPayloadsForItem(item: Item): void {
    if (item.kind !== 'assistant_text') return;
    if (!item.summary) return;
    if (!thread || item.threadId !== thread.id) return;

    const payloads = parseDesignAssistantPayloads(item.summary);
    if (payloads.length === 0) return;
    for (const p of payloads) {
      if (p.kind === 'clarification_request') {
        const next = p.payload;
        if (lastClarificationRequestId === next.requestId) continue;
        if (!next.threadId) next.threadId = thread.id;
        pendingClarification = next;
        lastClarificationRequestId = next.requestId;
      } else if (p.kind === 'expose_controls') {
        const key = controlsKey(p.payload.controls);
        if (lastExposedControlsKey === key) continue;
        exposedControls = [...p.payload.controls];
        lastExposedControlsKey = key;
      }
    }
  }

  function filteredPendingInteractiveSnapshot(
    snapshot: PendingInteractiveRequests | null | undefined,
  ): PendingInteractiveRequests {
    const approvals = (snapshot?.approvals ?? [])
      .filter((request) => request.requestId && !resolvedApprovalIds.has(request.requestId));
    const userInputs = (snapshot?.userInputs ?? [])
      .filter((request) => request.requestId && !resolvedUserInputIds.has(request.requestId));
    return { approvals, userInputs };
  }

  function applyPendingInteractiveSnapshot(
    threadID: string,
    snapshot: PendingInteractiveRequests | null | undefined,
  ): void {
    const filtered = filteredPendingInteractiveSnapshot(snapshot);
    pendingApprovals = mergePendingRequests(
      filtered.approvals,
      pendingApprovals,
      resolvedApprovalIds,
    );
    pendingUserInputs = mergePendingRequests(
      filtered.userInputs,
      pendingUserInputs,
      resolvedUserInputIds,
    );
    replaceInteractiveRequestsForThread(threadID, filtered);
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

    if (liveTodoRevision === guard.liveTodoRevisionAtRequest) {
      const todo = snapshot.todo;
      if (todo && todo.threadId === threadID && shouldHydrateLiveTodoSnapshot(todo)) {
        setLiveTodoState(todo.steps as TodoStep[]);
      } else {
        clearLiveTodoState();
      }
    }
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
      liveTodoRevisionAtRequest: liveTodoRevision,
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
   * Apply a paged-load result to pane state. Used by both phases of
   * `switchThread`'s two-phase load. Items merge additively — anything
   * already present (from cache, sibling phase, or streamed events
   * that landed mid-load) keeps its current reference; missing rows
   * are added and the array is re-sorted by (turnIndex, itemIndex).
   *
   * `cursorPolicy` controls how `oldestLoadedTurnIndex` /
   * `hasMoreHistory` move:
   *
   *  - `'narrow'` — phase 1's policy. Only tighten the floor; never
   *    widen it. Phase 2's wider window is canonical when both have
   *    run, so phase 1 must not overwrite a phase-2 cursor.
   *  - `'overwrite'` — phase 2's policy. Always take the cursors from
   *    this load. Phase 2's window is the widest signal for what's
   *    actually loaded.
   */
  function applyPagedItems(
    paged: PagedItems,
    threadID: string,
    cursorPolicy: 'narrow' | 'overwrite',
  ): void {
    const incoming = itemsForThread((paged.items ?? []) as Item[], threadID);
    items = mergeMissingItemsById(incoming, items);
    rebuildItemIndexes(items);
    const pagedFloor = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
    if (cursorPolicy === 'overwrite') {
      oldestLoadedTurnIndex = pagedFloor;
      hasMoreHistory = paged.hasMore ?? false;
    } else if (
      pagedFloor !== null &&
      (oldestLoadedTurnIndex === null || pagedFloor < oldestLoadedTurnIndex)
    ) {
      oldestLoadedTurnIndex = pagedFloor;
      hasMoreHistory = paged.hasMore ?? false;
    }
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
   * `cache.get`. Streamed events evict the cache entry on every
   * `applyItemUpserts` batch in `events.ts`, so a stale snapshot can't
   * outlive a backend mutation.
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
    pendingApprovals = [];
    pendingUserInputs = [];
    resolvedApprovalIds.clear();
    resolvedUserInputIds.clear();
    contextWindow = seedContextWindow(newThread);
    providerBanner = null;
    generalError = null;
    sendInFlight = false;
    channelMessages = [];
    channelStatus = null;
    pendingClarification = null;
    exposedControls = [];
    activeOptionSet = null;
    designViewport = 'desktop';
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

    // Live-todo reset. The auto-hide timer is cancelled to avoid a stale
    // clear firing against the wrong pane. Show-all survives per-thread
    // via liveTodoUiPrefs; the rail's open/closed state lives in
    // activityRailUiPrefs.
    if (liveTodoAutoHideTimer !== null) {
      clearTimeout(liveTodoAutoHideTimer);
      liveTodoAutoHideTimer = null;
    }
    liveTodo = null;
    liveTodoCleared = new Set();
    const incomingPrefs = readLiveTodoUiPrefs(newThread.id);
    liveTodoShowAll = incomingPrefs.showAll;
    const incomingRailPrefs = readActivityRailUiPrefs(newThread.id);
    activityRailTodosOpen = incomingRailPrefs.todosOpen;
    activityRailBackgroundOpen = incomingRailPrefs.backgroundOpen;
    diffPanel.clearForThread();
  }

  /**
   * Look up the incoming thread's cached snapshot and saved scroll
   * anchor, install the snapshot (or fresh empty state) onto the pane,
   * and reset per-row UI registries. Returns the snapshot (so phase 2
   * can decide whether to suppress the empty-timeline error path) and
   * the phase-1 anchor item id (empty string means tail-load).
   */
  function installCacheOrFreshState(
    newThread: Thread,
  ): { cached: ThreadItemSnapshot | null; phase1AnchorId: string } {
    const cached = threadItemCache.get(newThread.id);
    const scrollSnapshot = getThreadScrollSnapshot(newThread.id);
    const phase1AnchorId = scrollSnapshot?.kind === 'anchor' ? scrollSnapshot.itemId : '';

    loading = true;
    if (cached) {
      items = cached.items;
      oldestLoadedTurnIndex = cached.oldestLoadedTurnIndex;
      hasMoreHistory = cached.hasMoreHistory;
      latestSettledTurn = cached.latestSettledTurn;
    } else {
      items = [];
      // Windowed-history reset. A null floor disables the upsert floor
      // check until the backend tells us otherwise — between thread
      // clear and the ListRecentThreadItems response any streamed
      // upserts are already ours to append normally.
      oldestLoadedTurnIndex = null;
      hasMoreHistory = false;
    }
    resetLiveBuffers();
    rebuildItemIndexes(items);
    clearRowUiState();
    loadingOlder = false;
    return { cached, phase1AnchorId };
  }

  /**
   * Arm the spinner-flash gate. `loading` flips true the moment
   * `switchThread` starts; `showLoadingSpinner` only resolves to true
   * after `SPINNER_THRESHOLD_MS` AND when items.length === 0. Cache
   * hits never see the spinner because items render immediately;
   * sub-100ms cache misses skip it because phase 1 populates items
   * before the timer fires.
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
   * Commit the incoming thread to the pane. Sets `thread`, syncs the
   * top tab to the thread's mode (Discussion threads bypass the tab
   * UI so we leave activeTab unchanged), restores the per-thread RHS
   * panel snapshot, and re-opens the diff panel when the restored
   * panel was a diff-checkpoint.
   */
  function commitIncomingThread(newThread: Thread): void {
    thread = newThread;
    if (newThread.mode === 'design') {
      activeTab = 'design';
    } else if (newThread.mode === 'chat' || newThread.mode === 'plan') {
      activeTab = 'chat';
    }
    rhsPanelSlot.restoreForThread(newThread.id);
    if (rhsPanelSlot.activePanel?.kind === 'diff-checkpoint') {
      diffPanel.open_();
    }
  }

  /**
   * Run the six independent backend fetches that hydrate a thread
   * switch in parallel. Serializing them was the dominant source of
   * switch latency; under `Promise.allSettled` the wall-clock cost is
   * bounded by the slowest leg, not their sum. Each leg gen-guards its
   * own pane writes so a thread swap mid-flight invalidates late
   * resolutions. `switchPromise` and `liveStatePromise` keep their
   * bespoke shapes (the former logs unconditionally; the latter
   * consumes the live-state hydration token); the four canonical
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
    phase1AnchorId: string,
    liveStateHydrationToken: number,
  ): Promise<{ liveStateHydrationConsumed: boolean }> {
    let liveStateHydrationConsumed = false;
    const switchPromise = (async () => {
      try {
        const switched = (await SwitchThread(newThread.id)) as Thread;
        if (gen !== switchGeneration) return;
        if (switched.id === newThread.id) {
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

    // Phase 1: viewport-sized fast slice. Skip on cache hit — the
    // cached items already cover the visible window; phase 2 fills in
    // the rest. Failure is non-fatal — phase 2 is the canonical
    // full-window load and will fill in.
    const phase1Promise = cached
      ? Promise.resolve()
      : withGenGuard(
          'load phase 1 slice',
          gen,
          () => ListThreadSliceAround(newThread.id, phase1AnchorId, SLICE_AROUND_ITEM_COUNT),
          (paged) => {
            applyPagedItems(paged, newThread.id, 'narrow');
          },
        );

    const phase2Promise = withGenGuard(
      'load items',
      gen,
      () => ListRecentThreadItems(newThread.id, 0),
      (paged) => {
        applyPagedItems(paged, newThread.id, 'overwrite');
      },
      (err) => {
        // Only blank the timeline AND raise a hard error when nothing
        // was painted from cache or phase 1. When something rendered
        // already, the user keeps seeing the best view we have; phase 2
        // was a refresh, and a refresh failure becomes a quiet toast (so
        // streaming events have a chance to fill the gap).
        if (!cached && items.length === 0) {
          items = [];
          rebuildItemIndexes(items);
          oldestLoadedTurnIndex = null;
          hasMoreHistory = false;
          generalError = `Failed to load thread items: ${errString(err)}`;
          addToast('error', 'Failed to load thread items');
        } else {
          addToast('warning', 'Failed to refresh thread items');
        }
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
      phase1Promise,
      phase2Promise,
      recentTurnsPromise,
      checkpointsPromise,
    ]);
    return { liveStateHydrationConsumed };
  }

  return {
    // --- Getters (reactive reads) ---
    get thread() { return thread; },
    get threadId() { return thread?.id ?? null; },
    get items() { return items; },
    get timelineRevision() { return timelineRevision; },
    get liveItemSummaries() { return liveItemSummaries; },
    /**
     * Bumps once per coalesced live-delta flush (~rAF cadence). Auto-follow
     * effects watch this so a streaming row that grows in viewport while
     * sticky still re-pins to the new bottom — `timelineRevision` only
     * ticks on item-array changes, which deltas don't trigger.
     */
    get liveDeltaRevision() { return liveDeltaRevision; },
    get pendingApprovals() { return pendingApprovals; },
    get pendingUserInputs() { return pendingUserInputs; },
    get contextWindow() { return contextWindow; },
    get providerBanner() { return providerBanner; },
    get generalError() { return generalError; },
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
      // synchronously even while phase 2 still runs (loading=true), and we
      // must not flash a spinner over visible content. Single source of
      // truth here so call sites stay simple.
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
    get channelMessages() { return channelMessages; },
    get channelStatus() { return channelStatus; },
    get pendingClarification() { return pendingClarification; },
    get exposedControls() { return exposedControls; },
    get activeOptionSet() { return activeOptionSet; },
    get designViewport() { return designViewport; },
    get activeTab() { return activeTab; },
    get activeRhsPanel() { return rhsPanelSlot.activePanel; },
    get rhsSidebarWidth() { return rhsPanelSlot.width; },
    get showPlanSidebar() { return rhsPanelSlot.activePanel?.kind === 'plan'; },
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
      // Live-state hydration token. The live-state leg always consumes
      // it through `hydrateThreadLiveState`'s own finally; the outer
      // finally below only finishes it as defense-in-depth against a
      // synchronous throw before runParallelLoad runs.
      let liveStateHydrationConsumed = false;
      let liveStateHydrationToken = 0;
      try {
        snapshotOutgoingPane(newThread.id);
        resetIncomingPaneState(newThread);
        const { cached, phase1AnchorId } = installCacheOrFreshState(newThread);
        armSpinnerThreshold();
        liveStateHydrationToken = beginThreadLiveStateHydration(newThread.id);
        commitIncomingThread(newThread);
        const result = await runParallelLoad(newThread, gen, cached, phase1AnchorId, liveStateHydrationToken);
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
          items = itemsForThread((paged.items ?? []) as Item[], currentThread.id);
          rebuildItemIndexes(items);
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
        pendingApprovals = [];
        pendingUserInputs = [];
        await hydrateThreadLiveState(currentThread.id, gen, liveStateHydrationToken);
        liveStateHydrationToken = 0;
      } finally {
        if (liveStateHydrationToken !== 0) {
          finishThreadLiveStateHydration(currentThread.id, liveStateHydrationToken);
        }
      }
    },

    clear(): void {
      thread = null;
      items = [];
      resetLiveBuffers();
      rebuildItemIndexes(items);
      pendingApprovals = [];
      pendingUserInputs = [];
      resolvedApprovalIds.clear();
      resolvedUserInputIds.clear();
      contextWindow = null;
      providerBanner = null;
      generalError = null;
      loading = false;
      sendInFlight = false;
      showTerminal = false;
      rhsPanelSlot.reset();
      channelMessages = [];
      channelStatus = null;
      pendingClarification = null;
      exposedControls = [];
      activeOptionSet = null;
      designViewport = 'desktop';
      // activeTurn lives in the global registry (threadStatuses) and is
      // cleared by projectTurnCompleted; clearing it from a pane.clear()
      // would race with an in-flight turn on the same thread that
      // belongs to a different pane. The pane's getter just stops
      // returning a value once thread is null below.
      latestSettledTurn = null;
      subagentNotifications = [];
      // Mirror the live-todo reset block in switchThread: clearing the
      // pane while a todo list is mounted otherwise leaves a stale panel
      // with a dangling auto-hide timer that can fire against an
      // unrelated subsequent thread.
      if (liveTodoAutoHideTimer !== null) {
        clearTimeout(liveTodoAutoHideTimer);
        liveTodoAutoHideTimer = null;
      }
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
      liveTodo = null;
      liveTodoCleared = new Set();
      liveTodoShowAll = false;
      activityRailTodosOpen = false;
      activityRailBackgroundOpen = false;
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
        const paged = await ListItemsBeforeTurn(currentThread.id, floor, LOAD_OLDER_TURN_BATCH);
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
        if (next !== items) {
          items = next;
          rebuildItemIndexes(items);
        }
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

      // Load every turn from the target's turn index up through the
      // existing floor. A single ListItemsBeforeTurn with a turnLimit
      // sized to cover that distance does it in one shot.
      //
      // When `currentFloor` is null (empty window — thread never loaded
      // items, or cleared pane state), ask for the target turn directly
      // with a bounded default batch. The old MAX_SAFE_INTEGER sentinel
      // made the query broad and could still miss the target depending
      // on backend paging behavior.
      const targetFloor = fetched.turnIndex;
      const beforeTurn = currentFloor ?? targetFloor + 1;
      const turnSpan = currentFloor === null
        ? LOAD_OLDER_TURN_BATCH
        : Math.max(LOAD_OLDER_TURN_BATCH, beforeTurn - targetFloor + 1);

      loadingOlder = true;
      try {
        const paged = await ListItemsBeforeTurn(currentThread.id, beforeTurn, turnSpan);
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
        const prepend = itemsForThread((paged.items ?? []) as Item[], currentThread.id);
        const next = mergeItemsById(prepend, items);
        if (next !== items) {
          items = next;
          rebuildItemIndexes(items);
        }
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
      resolvedApprovalIds.delete(approval.requestId);
      pendingApprovals = [
        ...pendingApprovals.filter((a) => a.requestId !== approval.requestId),
        approval,
      ];
    },

    removeApproval(requestId: string): void {
      resolvedApprovalIds.add(requestId);
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    addUserInput(request: UserInputRequest): void {
      resolvedUserInputIds.delete(request.requestId);
      pendingUserInputs = [
        ...pendingUserInputs.filter((r) => r.requestId !== request.requestId),
        request,
      ];
    },

    removeUserInput(requestId: string): void {
      resolvedUserInputIds.add(requestId);
      pendingUserInputs = pendingUserInputs.filter((r) => r.requestId !== requestId);
    },

    /**
     * One-item compatibility wrapper around the batched upsert path.
     * Event routing uses `upsertItems` so bursts of wait rows and payload
     * enrichments hit the timeline in one paint.
     */
    upsertItem(item: Item): void {
      upsertItemsBatch([item]);
    },

    /**
     * Merge a batch of Items from `provider:item_event` into the timeline.
     * The final state is still the backend-authored transcript, but bursts
     * only allocate/sort/bump revision once.
     */
    upsertItems(incoming: Item[]): void {
      upsertItemsBatch(incoming);
    },

    applyItemDelta(evt: ItemDeltaEvent): void {
      if (!evt.itemId || !evt.delta) return;
      if (thread && evt.threadId !== thread.id) return;
      const status = itemStatusById.get(evt.itemId);
      if (status && status !== 'streaming') return;
      const chunks = liveDeltaChunks.get(evt.itemId);
      if (chunks) {
        chunks.push(evt.delta);
      } else {
        liveDeltaChunks.set(evt.itemId, [evt.delta]);
      }
      scheduleLiveDeltaFlush();
    },

    flushLiveDeltas(): void {
      if (liveSummaryFrame !== null) {
        cancelFrame(liveSummaryFrame);
        liveSummaryFrame = null;
      }
      flushLiveDeltaChunks();
    },

    // ---- Per-row UI state (survives virtua remount) ----
    expansionStateFor,
    expansionStateForPayload,
    isSubagentGroupExpanded,
    toggleSubagentGroupExpanded,
    attachmentCacheFor,

    setGeneralError(message: string | null): void {
      generalError = message;
    },

    clearGeneralError(): void {
      generalError = null;
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

    setProviderBanner(status: ProviderStatusEvent | null): void {
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

    get liveTodo() { return liveTodo; },
    get liveTodoShowAll() { return liveTodoShowAll; },

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
      setLiveTodoState(steps);
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
      clearLiveTodoState();
    },

    /** Toggle the "Show X more…" reveal under the truncated list. */
    toggleLiveTodoShowAll(): void {
      liveTodoShowAll = !liveTodoShowAll;
      writeLiveTodoUiPrefs(thread?.id ?? null, {
        showAll: liveTodoShowAll,
      });
    },

    // --- Activity rail (consolidated working/todos/background) ---

    get activityRailTodosOpen() { return activityRailTodosOpen; },
    get activityRailBackgroundOpen() { return activityRailBackgroundOpen; },

    /** Toggle the Todos accordion body inside the activity rail. */
    toggleActivityRailTodos(): void {
      activityRailTodosOpen = !activityRailTodosOpen;
      writeActivityRailUiPrefs(thread?.id ?? null, {
        todosOpen: activityRailTodosOpen,
        backgroundOpen: activityRailBackgroundOpen,
      });
    },

    /** Toggle the Background accordion body inside the activity rail. */
    toggleActivityRailBackground(): void {
      activityRailBackgroundOpen = !activityRailBackgroundOpen;
      writeActivityRailUiPrefs(thread?.id ?? null, {
        todosOpen: activityRailTodosOpen,
        backgroundOpen: activityRailBackgroundOpen,
      });
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
      // brief lease so the spring controller's chase + content-RO
      // re-pin both no-op while the column's clientHeight is settling.
      leaseDuringSettle(scrollController);
      showTerminal = !showTerminal;
    },

    setShowTerminal(value: boolean): void {
      if (value !== showTerminal) leaseDuringSettle(scrollController);
      showTerminal = value;
    },

    toggleDiffPanel(): void {
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

    setDiffPanelOpen(value: boolean): void {
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

    /**
     * Merge channel messages into local state, de-duplicating by sequence.
     * Expected to be called with `afterSeq` set to the highest sequence we've
     * seen, so most calls append a small number of rows.
     */
    mergeChannelMessages(incoming: ChannelMessage[]): void {
      if (!incoming || incoming.length === 0) return;
      const seen = new Set(channelMessages.map((m) => m.sequence));
      const next = channelMessages.slice();
      for (const msg of incoming) {
        if (!seen.has(msg.sequence)) {
          next.push(msg);
          seen.add(msg.sequence);
        }
      }
      next.sort((a, b) => a.sequence - b.sequence);
      channelMessages = next;
    },

    setChannelStatus(status: 'open' | 'concluded' | 'closed' | null): void {
      channelStatus = status;
    },

    clearChannel(): void {
      channelMessages = [];
      channelStatus = null;
    },

    // --- Design-mode mutations ---

    /**
     * Set the agent's clarification request. Pass null when the user
     * has answered (the panel sends the answers as a regular user
     * message; it then clears local state by calling this with null).
     */
    setPendingClarification(request: ClarificationRequest | null): void {
      pendingClarification = request;
    },

    /**
     * Replace the exposed slider controls. Per the wire contract each
     * `ExposeControls` event replaces the previous set wholesale.
     */
    setExposedControls(controls: SliderControl[]): void {
      exposedControls = [...controls];
    },

    /**
     * Activate (or clear) the side-by-side options grid. `null` returns
     * the pane to the main preview.
     */
    setActiveOptionSet(set: ActiveOptionSet | null): void {
      activeOptionSet = set;
    },

    setDesignViewport(viewport: DesignViewport): void {
      designViewport = viewport;
    },

    /**
     * Set the top-level mode tab. Called from the ChatHeader segmented
     * control on user click and from switchThread() on thread load. The
     * caller owns the side effects (finding the most-recent thread of the
     * target type, switching to it, or clearing thread state for the
     * design empty-state); this only flips the slot.
     */
    setActiveTab(tab: 'chat' | 'design'): void {
      activeTab = tab;
    },

    clearDesign(): void {
      pendingClarification = null;
      exposedControls = [];
      activeOptionSet = null;
      designViewport = 'desktop';
      lastClarificationRequestId = null;
      lastExposedControlsKey = null;
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
      if (!thread || thread.id !== threadId) return;
      try {
        const latest = (await LatestDesignOptionSet(threadId)) as
          | { setId: string; optionIds: string[] }
          | null;
        if (!thread || thread.id !== threadId) return;
        if (!latest || !latest.setId || !latest.optionIds || latest.optionIds.length === 0) {
          // No active set on disk. Clear any stale panel state — this
          // is what handles the post-pick dismissal sequence (the
          // .picked marker write fires the watcher → we re-query →
          // backend returns null → panel collapses).
          activeOptionSet = null;
          return;
        }
        const optionPaths = latest.optionIds.map((id) => `options/${latest.setId}/${id}`);
        activeOptionSet = { setId: latest.setId, optionPaths };
      } catch (err) {
        // eslint-disable-next-line no-console
        console.warn('design: LatestDesignOptionSet failed:', err);
      }
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
