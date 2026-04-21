import type { Item, Thread } from '../types/models';
import type {
  ApprovalRequest,
  ContextWindow,
  ProviderStatusEvent,
  SubagentNotificationEvent,
  TokenUsageSummary,
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

/**
 * Default batch size for "Load older" fetches. Matches the initial window
 * size so a single paging click approximately doubles the loaded history.
 * The value is a turn count, not an item count; backend-side caps keep a
 * single page from exceeding reasonable item totals even if those turns
 * are unusually large.
 */
const LOAD_OLDER_TURN_BATCH = 50;
import { addToast } from './toast.svelte';
import { createDiffPanelState, type DiffPanelState } from './diffPanel.svelte';
import { buildTurnDiffView, type TurnDiffView } from '../utils/turnDiffSummary';

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
  // Per-turn diff view index. Keyed by turnIndex. Incrementally updated on
  // upsertItem rather than rebuilt from scratch — with hundreds of items the
  // old $derived recomputation was O(turns · items) per upsert. Map presence
  // is the render gate in MessageTimeline; absent turns skip the tree+badge.
  let turnDiffViews: Map<number, TurnDiffView> = $state(new Map());
  let pendingApprovals: ApprovalRequest[] = $state([]);
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

  // PlanSidebar toggle state. Per-pane so each pane can open/close its
  // own sidebar independently. Reset on thread switch so a new thread
  // never "remembers" whether the prior thread had the sidebar open.
  let showPlanSidebar: boolean = $state(false);

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
   * Rebuild the per-turn diff view for a single turnIndex from the current
   * items snapshot. Mutates the reactive Map in place — Svelte 5 tracks Map
   * mutations on $state values, so set/delete are reactive without having
   * to allocate a fresh Map per upsert (which would be O(turns) alloc per
   * mutation and defeat the point of moving this out of MessageTimeline).
   */
  function refreshTurnDiffView(nextItems: Item[], turnIndex: number): void {
    const view = buildTurnDiffView(nextItems, turnIndex);
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
  function rebuildTurnDiffViews(nextItems: Item[]): void {
    turnDiffViews.clear();
    const byTurn = new Set<number>();
    for (const item of nextItems) byTurn.add(item.turnIndex);
    for (const turnIndex of byTurn) {
      const view = buildTurnDiffView(nextItems, turnIndex);
      if (view) turnDiffViews.set(turnIndex, view);
    }
  }

  function seedContextWindow(nextThread: Thread | null): ContextWindow | null {
    const raw = nextThread?.lastTokenUsage?.trim();
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as {
        usedTokens?: number;
        maxTokens?: number;
        contextPercent?: number;
      };
      if (typeof parsed.usedTokens !== 'number') return null;
      return {
        usedTokens: parsed.usedTokens,
        maxTokens: parsed.maxTokens,
        usedPercentage: parsed.contextPercent,
      };
    } catch {
      return null;
    }
  }

  return {
    // --- Getters (reactive reads) ---
    get thread() { return thread; },
    get threadId() { return thread?.id ?? null; },
    get items() { return items; },
    /**
     * Per-turn diff view. Keyed by `turnIndex`. Incrementally maintained by
     * `upsertItem` so MessageTimeline can render the ChangedFilesTree and
     * TurnDiffBadge without re-scanning the full items array each upsert.
     */
    get turnDiffViews() { return turnDiffViews; },
    get pendingApprovals() { return pendingApprovals; },
    get contextWindow() { return contextWindow; },
    get providerBanner() { return providerBanner; },
    get generalError() { return generalError; },
    get loading() { return loading; },
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
    get showPlanSidebar() { return showPlanSidebar; },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      pendingApprovals = [];
      contextWindow = seedContextWindow(newThread);
      providerBanner = null;
      generalError = null;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      // Every drawer / pane-scoped UI flag should reset so switching threads
      // never "remembers" the previous thread's open drawers. diffPanel owns
      // its own reset via clearForThread (which closes the panel); showTerminal
      // is reset here so opening the terminal on thread A does not spill over
      // into thread B.
      showTerminal = false;
      // Plan-sidebar UI is pane-scoped too: never carry its open state across
      // threads.
      showPlanSidebar = false;
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
      turnDiffViews = new Map();
      // Windowed-history reset. A null floor disables the upsert floor
      // check until the backend tells us otherwise, which is correct:
      // between thread clear and the ListRecentThreadItems response any
      // streamed upserts are already ours to append normally.
      oldestLoadedTurnIndex = null;
      hasMoreHistory = false;
      loadingOlder = false;
      pagingGeneration = 0;
      // scrollToItemRequest.nonce is kept monotonically increasing for
      // the pane's lifetime so MessageTimeline's lastHandledScrollNonce
      // can't miss an intent by seeing a lower number after a switch.

      thread = newThread;
      // Capture the switch generation at the top so every await below can bail
      // out if the user has already switched away (or back).
      const gen = ++switchGeneration;

      // Notify the backend so it can auto-start sessions for threads with session_ref.
      try {
        await SwitchThread(newThread.id);
      } catch (err) {
        console.error('Failed to notify backend of thread switch:', err);
        addToast('warning', 'Backend was not notified of thread switch');
      }
      if (gen !== switchGeneration) return;

      try {
        const paged = await ListRecentThreadItems(newThread.id, 0);
        if (gen !== switchGeneration) return;
        items = (paged.items ?? []) as Item[];
        oldestLoadedTurnIndex = paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : null;
        hasMoreHistory = paged.hasMore ?? false;
        rebuildTurnDiffViews(items);
      } catch (err) {
        if (gen !== switchGeneration) return;
        console.error('Failed to load items:', err);
        items = [];
        turnDiffViews = new Map();
        oldestLoadedTurnIndex = null;
        hasMoreHistory = false;
        generalError = `Failed to load thread items: ${err}`;
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

    clear(): void {
      thread = null;
      items = [];
      turnDiffViews = new Map();
      pendingApprovals = [];
      contextWindow = null;
      providerBanner = null;
      generalError = null;
      loading = false;
      showTerminal = false;
      showPlanSidebar = false;
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
      pagingGeneration = 0;
      // See switchThread: the scroll nonce stays monotonic across thread
      // changes so no consumer observes a regressed counter.
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
     * the window edge and produce a partial diff.
     */
    async loadOlder(): Promise<void> {
      const currentThread = thread;
      if (!currentThread) return;
      if (!hasMoreHistory || loadingOlder) return;
      const floor = oldestLoadedTurnIndex;
      if (floor === null) return;

      const gen = switchGeneration;
      const pageGen = ++pagingGeneration;
      loadingOlder = true;
      try {
        const paged = await ListItemsBeforeTurn(currentThread.id, floor, LOAD_OLDER_TURN_BATCH);
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return;
        const prepend = (paged.items ?? []) as Item[];
        if (prepend.length > 0) {
          items = [...prepend, ...items];
          rebuildTurnDiffViews(items);
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
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return;
        console.error('loadOlder failed:', err);
        addToast('error', 'Failed to load older messages');
      } finally {
        if (gen === switchGeneration && pageGen === pagingGeneration) {
          loadingOlder = false;
        }
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
      const targetFloor = fetched.turnIndex;
      const beforeTurn = currentFloor ?? Number.MAX_SAFE_INTEGER;
      const turnSpan = Math.max(LOAD_OLDER_TURN_BATCH, beforeTurn - targetFloor + 1);

      loadingOlder = true;
      try {
        const paged = await ListItemsBeforeTurn(currentThread.id, beforeTurn, turnSpan);
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
        const prepend = (paged.items ?? []) as Item[];
        if (prepend.length > 0) {
          items = [...prepend, ...items];
          rebuildTurnDiffViews(items);
        }
        oldestLoadedTurnIndex =
          paged.oldestTurnIndex >= 0 ? paged.oldestTurnIndex : beforeTurn;
        hasMoreHistory = paged.hasMore ?? false;
      } catch (err) {
        if (gen !== switchGeneration || pageGen !== pagingGeneration) return false;
        console.error('loadUntilItem ListItemsBeforeTurn failed:', err);
        addToast('error', 'Failed to load older messages');
        return false;
      } finally {
        if (gen === switchGeneration && pageGen === pagingGeneration) {
          loadingOlder = false;
        }
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

    // --- Mutations (called by event router) ---

    addApproval(approval: ApprovalRequest): void {
      pendingApprovals = [...pendingApprovals, approval];
    },

    removeApproval(requestId: string): void {
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    /**
     * Merge a single Item from a `provider:item_upsert` event into the
     * timeline. New ids append in (turnIndex, itemIndex) order; existing
     * ids replace in place so the row's status/summary/payload_id can
     * mutate without losing position. The backend is authoritative for
     * ordering — we never reshuffle by anything other than those two
     * fields, so a tool_call row stays exactly where it was inserted.
     *
     * Also refreshes the turnDiffViews entry for the item's turn (and, on
     * replace, the prior item's turn if it differed — defensive against
     * cross-turn corrections). Only the affected turn(s) are recomputed,
     * keeping per-upsert work bounded by the turn's item count rather than
     * the full thread.
     */
    upsertItem(item: Item): void {
      const idx = items.findIndex((existing) => existing.id === item.id);
      if (idx >= 0) {
        // Existing-id path: always accept. If we already have this id in
        // the window, the item is in-window by construction regardless
        // of any window-floor math. This branch also handles cross-turn
        // corrections where the server reassigns turn_index on a known id.
        const prevTurnIndex = items[idx].turnIndex;
        const next = items.slice();
        next[idx] = item;
        items = next;
        refreshTurnDiffView(next, item.turnIndex);
        if (prevTurnIndex !== item.turnIndex) {
          refreshTurnDiffView(next, prevTurnIndex);
        }
        return;
      }
      // Window-floor guard for NEW items. Upserts can legitimately fire
      // for older turns (triage interrupt-queue replay, late background
      // tool_completion siblings, codex reconcile flips). If the
      // coordinate is below the loaded window, silently drop — the row
      // is safely persisted in SQLite and will pull in on the next
      // `loadOlder` click.
      if (oldestLoadedTurnIndex !== null && item.turnIndex < oldestLoadedTurnIndex) {
        return;
      }
      // Find the insertion point that preserves (turnIndex, itemIndex).
      // Linear scan is fine — the loaded window is bounded by
      // INITIAL_TURN_WINDOW (~50 turns, typically a few hundred items).
      let insertAt = items.length;
      for (let i = 0; i < items.length; i++) {
        const cur = items[i];
        if (
          cur.turnIndex > item.turnIndex
          || (cur.turnIndex === item.turnIndex && cur.itemIndex > item.itemIndex)
        ) {
          insertAt = i;
          break;
        }
      }
      const next = items.slice();
      next.splice(insertAt, 0, item);
      items = next;
      refreshTurnDiffView(next, item.turnIndex);
    },

    setGeneralError(message: string | null): void {
      generalError = message;
    },

    clearGeneralError(): void {
      generalError = null;
    },

    setContextWindow(data: ContextWindow): void {
      contextWindow = data;
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
    },

    toggleTerminal(): void {
      showTerminal = !showTerminal;
    },

    setShowTerminal(value: boolean): void {
      showTerminal = value;
    },

    toggleDiffPanel(): void {
      diffPanel.toggle();
    },

    togglePlanSidebar(): void {
      showPlanSidebar = !showPlanSidebar;
    },

    setShowPlanSidebar(value: boolean): void {
      showPlanSidebar = value;
    },

    setDiffPanelOpen(value: boolean): void {
      if (value) {
        diffPanel.open_();
      } else {
        diffPanel.close();
      }
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
