import type { Item, Thread } from '../types/models';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ProviderStatusEvent,
  SubagentNotificationEvent,
  TokenUsageSummary,
  UserInputRequest,
} from '../types/events';
import type { ChannelMessage } from '../types/discussion';
import type { DesignArtifact, DesignOptionsRequest, DesignViewport } from '../types/design';
import {
  GetThreadItem,
  ListItemsBeforeTurn,
  ListRecentThreadItems,
  ListRecentTurns,
  SwitchThread,
} from './bindings';
import { replaceThread } from './threads.svelte';
import {
  createPayloadExpansion,
  type PayloadExpansionHandle,
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
import {
  buildDiffViewForItems,
  itemMayAffectDiffView,
  type TurnDiffView,
} from '../utils/turnDiffSummary';
import { errString } from '../utils/errors';
import { clearTokensForThread } from '../utils/tokenCacheReactive.svelte';

/**
 * Default batch size for "Load older" fetches. Matches the initial window
 * size so a single paging click approximately doubles the loaded history.
 * The value is a turn count, not an item count; backend-side caps keep a
 * single page from exceeding reasonable item totals even if those turns
 * are unusually large.
 */
const LOAD_OLDER_TURN_BATCH = 50;

/**
 * Soft cap on total `displayData` bytes held across a pane's expansion
 * registry. Tunable via `__setExpansionBudgetForTest` so tests can
 * exercise the LRU path without allocating real-world-sized payloads.
 * 16 MiB allows ~16 fully-expanded large tool outputs (or hundreds of
 * smaller ones) — comfortably above any realistic working set, well
 * below the point where the JS heap starts to feel it.
 */
const DEFAULT_EXPANSION_BUDGET_BYTES = 16 * 1024 * 1024;
let expansionBudgetBytes = DEFAULT_EXPANSION_BUDGET_BYTES;
function getExpansionBudgetBytes(): number {
  return expansionBudgetBytes;
}
export function setExpansionBudgetForTest(bytes: number): void {
  expansionBudgetBytes = bytes;
}
export function resetExpansionBudgetForTest(): void {
  expansionBudgetBytes = DEFAULT_EXPANSION_BUDGET_BYTES;
}

function sameRhsPanel(left: RhsPanel | null, right: RhsPanel | null): boolean {
  if (left === null || right === null) return left === right;
  if (left.kind !== right.kind) return false;
  if (left.kind !== 'diff-payload' || right.kind !== 'diff-payload') return true;
  return left.payloadId === right.payloadId && left.filePath === right.filePath;
}

/**
 * ActiveTurn is the live in-flight turn for the pane. Populated exclusively
 * from the `provider:turn_started` wire event; cleared on
 * `provider:turn_completed` or thread switch. Never hydrated from
 * persistence — see invariant 22 (turn activity is wire-pushed).
 */
export interface ActiveTurn {
  turnId: string;
  turnIndex: number;
  /** Unix-millis. Anchors the self-ticking working-indicator timer. */
  startedAt: number;
}

/**
 * SettledTurn is the most recent completed turn's projection. Used by the
 * completion divider to render "Response · Worked for Xs · Yk tokens" above
 * the final assistant item. Populated from `provider:turn_completed` pushes
 * or, on thread switch, from the most recent `ListRecentTurns` row whose
 * `completedAt` is non-null.
 */
export interface SettledTurn {
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  /** item.id of the final assistant_text; null when the provider didn't report one. */
  assistantMessageId: string | null;
  /** Parsed from triage's token_usage_json. null on malformed / missing input. */
  tokenUsage: TokenUsageSummary | null;
  aborted: boolean;
  errorMessage: string;
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
 * surfaces whose layout change isn't visible to virtua's geometry
 * (e.g. composer growth changes the inner padding-bottom but not the
 * scroll wrapper's clientHeight). The actual controller
 * (stickyBottomController for virtua, stickToBottom for DOM) has more
 * methods but they're consumed inside the timeline component directly,
 * not via this seam.
 */
export interface PaneScrollController {
  pauseAutoScroll(): () => void;
  /**
   * Nudge the controller to re-evaluate "should I scroll to the
   * bottom?". A no-op unless the user is sticky and no lease is held.
   * Use this from layout-changing surfaces outside the timeline whose
   * change isn't observable to the controller's own ResizeObserver
   * (composer overlay growth, anything that mutates inner scroll
   * padding without changing the scroll wrapper's clientHeight).
   */
  notifyContentMaybeGrew(): void;
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
  // Per-parent-itemId subagent group expand state. ChangedFilesTree and
  // ProposedPlanCard expansion state are deliberately NOT lifted — they
  // appear at end-of-turn / rare item types and the back-scroll
  // remount frequency is low in practice. Lift if profiling proves it.
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
  // Per-turn diff view index. Keyed by turnIndex. Incrementally updated on
  // upsertItem rather than rebuilt from scratch — with hundreds of items the
  // old $derived recomputation was O(turns · items) per upsert. Map presence
  // is the render gate in MessageTimeline; absent turns skip the tree+badge.
  let turnDiffViews: Map<number, TurnDiffView> = $state(new Map());
  let pendingApprovals: ApprovalRequest[] = $state([]);
  let pendingUserInputs: UserInputRequest[] = $state([]);
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
  // designArtifacts is the render+option history for the thread.
  // activeArtifactId is what the preview panel is displaying — null = show latest.
  // pendingDesignOptions is populated when an agent has blocked on present_options.
  // designViewport drives the iframe width toggle.
  let designArtifacts: DesignArtifact[] = $state([]);
  let activeArtifactId: string | null = $state(null);
  let pendingDesignOptions: DesignOptionsRequest | null = $state(null);
  let designViewport: DesignViewport = $state('desktop');

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
    // column when they open or close. Hold a brief lease so the
    // auto-follow $effect doesn't yank the timeline mid-transition.
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

  // Turn-lifecycle state. `activeTurn` is set exclusively from live
  // `provider:turn_started` wire events (invariant 22) and cleared on
  // `provider:turn_completed` or thread switch. `latestSettledTurn` is
  // populated from turn-complete events OR on thread-switch rehydration
  // from the most recent `ListRecentTurns` row with a non-null
  // `completedAt`. A crashed / in-flight historical turn is deliberately
  // NOT promoted to `activeTurn` on rehydration — only the wire can turn
  // the indicator on.
  let activeTurn: ActiveTurn | null = $state(null);
  let latestSettledTurn: SettledTurn | null = $state(null);
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
   * means "no outstanding request".
   */
  let scrollToItemRequest: { itemId: string; nonce: number } = $state({
    itemId: '',
    nonce: 0,
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

  /**
   * Rebuild the per-turn diff view for a single turnIndex from the current
   * items snapshot. Mutates the reactive Map in place — Svelte 5 tracks Map
   * mutations on $state values, so set/delete are reactive without having
   * to allocate a fresh Map per upsert (which would be O(turns) alloc per
   * mutation and defeat the point of moving this out of MessageTimeline).
   */
  function itemsForTurn(turnIndex: number): Item[] {
    const ids = itemIdsByTurn.get(turnIndex);
    if (!ids || ids.length === 0) return [];
    const turnItems: Item[] = [];
    for (const id of ids) {
      const index = itemIndexById.get(id);
      if (index === undefined) continue;
      const item = items[index];
      if (item && item.turnIndex === turnIndex) {
        turnItems.push(item);
      }
    }
    return turnItems;
  }

  function refreshTurnDiffView(turnIndex: number): void {
    const view = buildDiffViewForItems(itemsForTurn(turnIndex));
    if (view) {
      turnDiffViews.set(turnIndex, view);
    } else {
      turnDiffViews.delete(turnIndex);
    }
  }

  /**
   * Rebuild the full per-turn diff view map from an items snapshot. Used on
   * thread switch / bulk load where a single incremental pass isn't
   * appropriate. Clears the reactive map in place for the same "no
   * per-mutation alloc" reason.
   */
  function rebuildTurnDiffViews(): void {
    turnDiffViews.clear();
    for (const turnIndex of itemIdsByTurn.keys()) {
      const view = buildDiffViewForItems(itemsForTurn(turnIndex));
      if (view) turnDiffViews.set(turnIndex, view);
    }
  }

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
  // Within the lifetime of a single thread, however, the expansion
  // registry IS unbounded: each toggled tool_call holds its loaded
  // payload chunks until the user collapses it or switches threads.
  // For long sessions where the user expands many heavy outputs, that
  // can climb past sensible bounds. Cap total `displayData` bytes at
  // EXPANSION_BUDGET_BYTES; when exceeded, collapse least-recently
  // toggled handles (which drops their chunks). Map insertion order is
  // the LRU — touch on toggle/expand/showFull by delete + re-set.

  function computeExpansionBytes(): number {
    let total = 0;
    for (const handle of expansionStates.values()) {
      const data = handle.displayData;
      if (data) total += data.length;
    }
    return total;
  }

  function enforceExpansionBudget(skipKey: string): void {
    // Compute the total once, then maintain it incrementally as we
    // collapse handles. The previous approach re-summed every entry's
    // displayData on every loop iteration (O(n²) in the number of
    // expanded handles). With LRU touches keeping the touched key at
    // the tail, the iterator hits oldest-first; subtracting on collapse
    // is correct without a recompute.
    let total = computeExpansionBytes();
    const cap = getExpansionBudgetBytes();
    if (total <= cap) return;
    for (const [iterKey, handle] of expansionStates) {
      if (iterKey === skipKey) continue;
      const data = handle.displayData;
      if (!handle.expanded || !data) continue;
      const droppedBytes = data.length;
      handle.collapse();
      total -= droppedBytes;
      if (total <= cap) break;
    }
  }

  function touchExpansion(key: string): void {
    const value = expansionStates.get(key);
    if (value) {
      expansionStates.delete(key);
      expansionStates.set(key, value);
    }
  }

  function withExpansionLRU(inner: PayloadExpansionHandle, key: string): PayloadExpansionHandle {
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
      toggle: async () => {
        touchExpansion(key);
        await inner.toggle();
        enforceExpansionBudget(key);
      },
      expand: async () => {
        touchExpansion(key);
        await inner.expand();
        enforceExpansionBudget(key);
      },
      ensureLoaded: async () => {
        touchExpansion(key);
        const changed = await inner.ensureLoaded();
        if (changed) enforceExpansionBudget(key);
        return changed;
      },
      collapse: () => { inner.collapse(); },
      showFull: async () => {
        touchExpansion(key);
        await inner.showFull();
        enforceExpansionBudget(key);
      },
      retry: async () => {
        touchExpansion(key);
        await inner.retry();
        enforceExpansionBudget(key);
      },
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
  function expansionStateFor(item: Item): PayloadExpansionHandle {
    const key = 'i:' + item.id;
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
      },
    );
    cached = withExpansionLRU(inner, key);
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
    cached = withExpansionLRU(inner, key);
    expansionStates.set(key, cached);
    return cached;
  }

  function isSubagentGroupExpanded(parentId: string): boolean {
    return subagentGroupExpanded.has(parentId);
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

  function toggleSubagentGroupExpanded(parentId: string): boolean {
    const next = new Set(subagentGroupExpanded);
    const willExpand = !next.has(parentId);
    if (willExpand) next.add(parentId); else next.delete(parentId);
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

    const affectedTurns = new Set<number>();
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
        if (itemMayAffectDiffView(previous) || itemMayAffectDiffView(item)) {
          affectedTurns.add(previous.turnIndex);
          affectedTurns.add(item.turnIndex);
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
      if (itemMayAffectDiffView(item)) {
        affectedTurns.add(item.turnIndex);
      }
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
    for (const turnIndex of affectedTurns) {
      refreshTurnDiffView(turnIndex);
    }
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
    /**
     * Per-turn diff view. Keyed by `turnIndex`. Incrementally maintained by
     * `upsertItem` so MessageTimeline can render the ChangedFilesTree and
     * TurnDiffBadge without re-scanning the full items array each upsert.
     */
    get turnDiffViews(): ReadonlyMap<number, TurnDiffView> { return turnDiffViews; },
    get pendingApprovals() { return pendingApprovals; },
    get pendingUserInputs() { return pendingUserInputs; },
    get contextWindow() { return contextWindow; },
    get providerBanner() { return providerBanner; },
    get generalError() { return generalError; },
    get loading() { return loading; },
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
    /**
     * True while a provider turn is in-flight for the current pane. The
     * composer uses this to block sends and surface the interrupt
     * affordance.
     *
     * Post-turn-lifecycle refactor: this is now strictly wire-pushed —
     * `activeTurn !== null` is the single source of truth. Item-state
     * derivations (streaming text, running tool_calls) no longer drive
     * this flag; see invariant 22 (turn activity is wire-pushed).
     *
     * Pending approvals live within an active turn by construction — the
     * provider emits turn_started before any approval request and
     * turn_completed only after all approvals resolve — so they're
     * covered implicitly via activeTurn being non-null during that
     * window. If a future flow decouples approvals from turn spans,
     * re-evaluate this invariant.
     */
    get isTurnActive() {
      return activeTurn !== null;
    },
    /**
     * Live in-flight turn (wire-pushed from `provider:turn_started`).
     * Consumers drive the working indicator and the composer's mid-turn
     * guard off this value.
     */
    get activeTurn() { return activeTurn; },
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
    get designArtifacts() { return designArtifacts; },
    get activeArtifactId() { return activeArtifactId; },
    get pendingDesignOptions() { return pendingDesignOptions; },
    get designViewport() { return designViewport; },
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
      pendingApprovals = [];
      pendingUserInputs = [];
      contextWindow = seedContextWindow(newThread);
      providerBanner = null;
      generalError = null;
      sendInFlight = false;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      // Bottom-drawer state is pane-scoped: opening the terminal on thread A
      // should not spill into thread B. The RHS sidebar is different: its
      // active panel + width are snapshotted per thread below.
      showTerminal = false;
      const outgoingThreadId = thread?.id ?? null;
      if (outgoingThreadId) {
        rhsPanelSlot.snapshotForThread(outgoingThreadId);
        // Free Shiki tokens cached against the outgoing thread. The
        // shared cache is partitioned by threadId so this is a clean
        // segmental drop; new lines tokenized for the incoming thread
        // start from a fresh per-thread namespace.
        clearTokensForThread(outgoingThreadId);
      } else {
        // No outgoing thread to snapshot under — just reset.
        rhsPanelSlot.closeForThread();
      }
      // Turn-lifecycle reset. activeTurn goes to null on every switch — a
      // crashed thread's in-flight row is historical and must not light up
      // the indicator (invariant 22). latestSettledTurn gets rehydrated
      // below from ListRecentTurns; we clear it first so a rehydration
      // failure leaves the pane in a consistent "no prior turn" state.
      activeTurn = null;
      latestSettledTurn = null;
      subagentNotifications = [];
      diffPanel.clearForThread();
      loading = true;
      items = [];
      resetLiveBuffers();
      rebuildItemIndexes(items);
      clearRowUiState();
      turnDiffViews = new Map();
      // Windowed-history reset. A null floor disables the upsert floor
      // check until the backend tells us otherwise, which is correct:
      // between thread clear and the ListRecentThreadItems response any
      // streamed upserts are already ours to append normally.
      oldestLoadedTurnIndex = null;
      hasMoreHistory = false;
      loadingOlder = false;
      // `pagingGeneration` is kept monotonically increasing for the
      // pane's lifetime — same argument as `scrollToItemRequest.nonce`
      // below. A stale pre-switch loadOlder/loadUntilItem is guarded
      // by `switchGeneration`, so resetting this counter here is
      // redundant and only introduces a "same generation value before
      // and after the swap" collision risk if the guards ever get
      // reordered.

      thread = newThread;
      rhsPanelSlot.restoreForThread(newThread.id);
      if (rhsPanelSlot.activePanel?.kind === 'diff-checkpoint') {
        diffPanel.open_();
      }
      // Capture the switch generation at the top so every await below can bail
      // out if the user has already switched away (or back).
      const gen = ++switchGeneration;

      // Notify the backend so it can mark the thread read durably and
      // auto-start sessions for threads with session_ref.
      try {
        const switched = await SwitchThread(newThread.id) as Thread;
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
      if (gen !== switchGeneration) return;

      // SwitchThread persists read state for the selection itself. ChatView
      // keeps the active row read as completed turns settle.

      try {
        const paged = await ListRecentThreadItems(newThread.id, 0);
        if (gen !== switchGeneration) return;
        items = itemsForThread((paged.items ?? []) as Item[], newThread.id);
        rebuildItemIndexes(items);
        oldestLoadedTurnIndex = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
        hasMoreHistory = paged.hasMore ?? false;
        rebuildTurnDiffViews();
      } catch (err) {
        if (gen !== switchGeneration) return;
        console.error('Failed to load items:', err);
        items = [];
        rebuildItemIndexes(items);
        turnDiffViews = new Map();
        oldestLoadedTurnIndex = null;
        hasMoreHistory = false;
        generalError = `Failed to load thread items: ${errString(err)}`;
        addToast('error', 'Failed to load thread items');
      }

      if (gen !== switchGeneration) return;

      // Rehydrate latestSettledTurn from the most recent completed row.
      // We ask for the two most recent turns so a crashed-then-completed
      // sequence can skip over the in-flight row and still find the prior
      // settled one. This is a defensive fetch — the happy path is a
      // single "most recent = settled" row.
      try {
        const recent = (await ListRecentTurns(newThread.id, 2)) as TurnRow[] | null;
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
        // Rehydration is best-effort — a fetch failure shouldn't block the
        // thread from rendering items. Log and proceed with
        // latestSettledTurn=null (the completion divider just won't appear
        // for the prior turn).
        console.error('Failed to rehydrate recent turns:', err);
      }

      if (gen !== switchGeneration) return;
      loading = false;
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
      try {
        const paged = await ListRecentThreadItems(currentThread.id, 0);
        if (gen !== switchGeneration) return;
        items = itemsForThread((paged.items ?? []) as Item[], currentThread.id);
        rebuildItemIndexes(items);
        oldestLoadedTurnIndex = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
        hasMoreHistory = paged.hasMore ?? false;
        rebuildTurnDiffViews();
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
    },

    clear(): void {
      thread = null;
      items = [];
      resetLiveBuffers();
      rebuildItemIndexes(items);
      turnDiffViews = new Map();
      pendingApprovals = [];
      pendingUserInputs = [];
      contextWindow = null;
      providerBanner = null;
      generalError = null;
      loading = false;
      sendInFlight = false;
      showTerminal = false;
      rhsPanelSlot.reset();
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      activeTurn = null;
      latestSettledTurn = null;
      subagentNotifications = [];
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
     * clicks or keyboard repeats). On success the per-turn diff-view map
     * is rebuilt to include the newly loaded turns; because paging is
     * turn-boundary aligned the refresh is safe — no turn can straddle
     * the window edge and produce a partial diff. The return value is for
     * scroll anchoring: `insertedBeforeWindow` means at least one new row
     * sorted before the current in-memory first row. Components that know
     * the actual visible anchor still restore that anchor directly.
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
          rebuildTurnDiffViews();
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
          rebuildTurnDiffViews();
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
    requestScrollToItem(itemID: string): void {
      if (!itemID) return;
      scrollToItemRequest = {
        itemId: itemID,
        nonce: scrollToItemRequest.nonce + 1,
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
      pendingApprovals = [
        ...pendingApprovals.filter((a) => a.requestId !== approval.requestId),
        approval,
      ];
    },

    removeApproval(requestId: string): void {
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    addUserInput(request: UserInputRequest): void {
      pendingUserInputs = [
        ...pendingUserInputs.filter((r) => r.requestId !== request.requestId),
        request,
      ];
    },

    removeUserInput(requestId: string): void {
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
    setActiveTurn(turn: ActiveTurn): void {
      if (activeTurn !== null && activeTurn.turnId === turn.turnId) {
        // Preserve the original startedAt so the working indicator's
        // elapsed-seconds counter doesn't rewind. turnIndex can shift
        // only if the caller changed turns entirely, which the guard
        // above excludes.
        return;
      }
      activeTurn = turn;
    },

    /**
     * Settle the current turn on `provider:turn_completed`. Clears the
     * live `activeTurn` and writes `latestSettledTurn` so the completion
     * divider can render above the assistant message this settled. Safe
     * to call when `activeTurn` is already null (recovered turn that
     * the backend re-settles).
     */
    settleTurn(settled: SettledTurn): void {
      activeTurn = null;
      latestSettledTurn = settled;
    },

    /**
     * Optimistic clear used by the Esc / Stop interrupt path. Drops the
     * live `activeTurn` synchronously so the spinner / Stop button
     * flip to idle in the same render tick as the keystroke — matching
     * Claude Code's `resetLoadingState()` (REPL.tsx:2106-2163) and the
     * Codex TUI's spinner clear on `EventMsg::TurnAborted`. The real
     * `provider:turn_completed` arrives shortly after and re-runs
     * settleTurn (idempotent on already-null activeTurn). Does NOT
     * clear `latestSettledTurn` so the previous turn's completion
     * divider stays visible.
     */
    clearActiveTurn(): void {
      activeTurn = null;
    },

    /**
     * Reset both turn-lifecycle slots without rehydrating. Used by the
     * frontend on explicit "clear this pane" paths that aren't a full
     * switchThread — e.g. a user-triggered stop that leaves the pane in
     * a known-quiet state until the next wire push.
     */
    clearTurnState(): void {
      activeTurn = null;
      latestSettledTurn = null;
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
      // brief lease so the auto-follow $effect can't yank the viewport
      // while the column's clientHeight is settling.
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

    closeDiffSidebar(): void {
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
     * Replace the artifact history in one shot. Used when the panel first
     * mounts and hydrates from ListDesignArtifacts.
     */
    setDesignArtifacts(artifacts: DesignArtifact[]): void {
      designArtifacts = [...artifacts];
    },

    /**
     * Append an artifact. De-dupes by id so idempotent event replays don't
     * double-insert. New artifacts become the implicit active one (unless the
     * user has pinned a different artifact via setActiveArtifact).
     */
    appendDesignArtifact(artifact: DesignArtifact): void {
      const exists = designArtifacts.some((a) => a.id === artifact.id);
      if (exists) return;
      designArtifacts = [...designArtifacts, artifact];
    },

    setActiveArtifact(artifactId: string | null): void {
      activeArtifactId = artifactId;
    },

    setDesignOptions(request: DesignOptionsRequest | null): void {
      pendingDesignOptions = request;
    },

    clearDesignOptions(): void {
      pendingDesignOptions = null;
    },

    setDesignViewport(viewport: DesignViewport): void {
      designViewport = viewport;
    },

    clearDesign(): void {
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
