import { tick } from 'svelte';
import type { Item, Project, Thread } from '../types/models';
import { asProviderID } from '../types/providers';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
  TodoStep,
  ProviderSessionAccountEvent,
  ProviderStatusEvent,
  UserInputRequest,
} from '../types/events';
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import type {
  ActiveOptionSet,
  ClarificationRequest,
  DesignViewport,
} from '../types/design';
import { CreateThread } from './bindings';
import { getThreadById, prependThread, removeThread } from './threads.svelte';
import { leaseDuringSettle } from '../utils/scrollLeaseDuringTransition';
import {
  clearWorktreeIntent,
  migrateWorktreeIntent,
  seedDefaultWorktreeIntentForDraft,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

import { createGitStatusView, type GitStatusView } from './gitStatusStore.svelte';
import {
  closeCompanion,
  closeCompanionsForSource,
  companionForSource,
  isCompanionOpen,
  openCompanion,
  toggleCompanion,
} from './companionPanes.svelte';
import { openReviewCompanion } from './reviewPane.svelte';
import {
  agentPaneScopeTrailHolds,
  disposeAgentStateForPane,
  openAgentCompanion,
} from './agentPane.svelte';
import { errString } from '../utils/errors';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { SubagentFoldAggregate } from '../utils/subagentFold';
import { rowUiRetentionChanged } from '../utils/rowUiRetention';
import { activityRunSummaryFieldsChanged } from '../utils/activityRunGrouping';
import type { ApplyItemUpsertsToWindowResult } from './threadItems';
import { sameNormalizedPath } from '../utils/path';
import {
  getActiveTurn,
  projectTurnCompleted,
  projectTurnStarted,
  type ActiveTurn,
} from './threadStatuses.svelte';
import { createLiveTodoState } from './liveTodoState.svelte';
import { createThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { activityRunDefaultCollapsed, activityRunWindowRows } from './activityRunPrefs.svelte';
import type { SettledTurn } from './threadTurnProjection';
import { createThreadRowUiState, type RowUiStateRetention } from './threadRowUiState.svelte';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import { createThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { createThreadSubagentMemory } from './threadSubagentMemory';
import { createThreadLiveStateHydration } from './threadLiveStateHydration';
import { createThreadSwitchLoad } from './threadSwitchLoad.svelte';
import { createThreadItemStreamApply } from './threadItemStreamApply';
import {
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';
import { createThreadChannelState } from './threadChannelState.svelte';
import { createThreadDesignState } from './threadDesignState.svelte';
import {
  nowForLiveContent,
  threadUsesDiscussionSurface,
  type DraftThreadPlaceholder,
  type DraftPlaceholderDefaults,
  type DraftPlaceholderMode,
  type LoadOlderResult,
  type PaneScrollController,
  type ScrollToItemRequest,
  type ThreadPaneOptions,
} from './threadPaneShared';

/** Shared "nothing was dropped" list, so the common replacement allocates none. */
const NO_ITEMS: readonly Item[] = Object.freeze([]);

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
  PreserveViewportBottomOptions,
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
  // The pane's thread IDENTITY as a $derived primitive. `thread` is
  // replaced wholesale by every thread:updated sync (mode toggle, title
  // regen, model change), and a plain getter over the $state has no
  // equality cutoff — every effect keyed on "which thread is mounted"
  // re-ran on each replacement even though the id never moved. The nav
  // rail's baseline effect was the visible casualty: shift+tab cleared
  // and refetched the whole-thread tick list, blinking the rail
  // (2026-08-19). A $derived does not propagate while its value is
  // unchanged, so identity consumers only wake when the pane actually
  // mounts a different thread.
  // ($derived.by, not $derived: the inline form typechecks at this spot,
  // where control flow has narrowed `thread` to its `null` initializer.)
  const stableThreadId = $derived.by(() =>
    draftPlaceholder ? null : (thread?.id ?? null),
  );
  // Same cutoff for the other primitive facts served off the thread
  // object: `terminalThreadId` keys a `{#key}` and the terminal
  // placement wiring, and `activeModel` (its $derived lives beside
  // `effectiveModel` below) is one effect away from the same trap. Every
  // primitive-valued getter over `thread` goes through a $derived, so no
  // consumer can be woken by a replacement that changed nothing it reads.
  const stableTerminalThreadId = $derived.by(() => thread?.id ?? null);
  let items: Item[] = $state([]);
  // Structural revision for timeline projections that should skip
  // summary-only streaming deltas. Bump whenever the item window's array
  // changes shape or identity; `applyItemDelta` intentionally does not bump.
  let timelineRevision = $state(0);
  // Revision of the item-side inputs to offscreen row-UI-state retention
  // (`utils/rowUiRetention.ts`): bumped by an items write only when it
  // changed which rows the prune retains unconditionally, or what it
  // retains for one. The prune's no-op bail reads it as a scalar instead
  // of walking `items` per callback — that walk wedged the renderer for
  // 6-19s mid-turn, because the `$state` array is replaced on every
  // upsert batch and each walk re-created a proxy source per index.
  //
  // Deliberately NOT `$state`, same reason as `lastLiveContentAt`: the
  // only reader is the quiet scheduler's prune pass, which runs off a
  // microtask/timer and reads imperatively. Scheduling is a separate
  // concern and stays on `timelineRevision` + the other structural
  // triggers; this value only decides whether a scheduled pass is a
  // no-op.
  let rowUiRetentionRevision = 0;
  // Non-reactive timestamp of the last LIVE timeline content advance — a
  // smoother reveal, an overwrite patch, a text-like provider row, a
  // visible-field update to an already mounted row (tool output preview,
  // running→completed result chrome; see events.ts
  // providerUpsertAdvancesLiveContent), or a wire append / reveal-gate
  // release entering the loaded tail (`armLiveContentAppendSpring`
  // below — that path shares the arm's restore gates, so a switch-load
  // settle never stamps).
  // Read imperatively by the scroll controller (MessageTimeline's
  // `liveContentActive` getter) as a LIVENESS signal — it keeps the
  // spring's post-arrival sentinel alive and lets a composer resize ride
  // an in-flight glide. It does NOT choose spring vs sync-pin; growth
  // while pinned at the bottom always glides (see
  // utils/liveContentActivity.ts and utils/scroll/resolver.ts).
  // Deliberately NOT `$state`: it is stamped up to ~60×/sec during a
  // drain and is never read in a reactive scope, so `$state` would churn
  // every dependent derivation for no benefit.
  let lastLiveContentAt = 0;
  function stampLiveContent(): void {
    lastLiveContentAt = nowForLiveContent();
  }
  const itemIndexById: Map<string, number> = new Map();
  function getItemById(itemId: string): Item | undefined {
    const index = itemIndexById.get(itemId);
    return index === undefined ? undefined : items[index];
  }
  /**
   * The ONE in-place row write. Every path that replaces a single loaded
   * row (smoother reveal, delta, meta, field patch) goes through here so
   * the revisions derived from a row's fields cannot be missed by a new
   * caller: the bump is a property of writing, not something each writer
   * remembers. Both exist to keep an O(window) walk off a ~50Hz path —
   * the offscreen row-UI prune's no-op bail, and the activity-run
   * header's summary signature — so both are decided from the single
   * comparison this function is already holding.
   *
   * Wholesale replacements go through `commitTimelineItems` instead,
   * which bumps retention unconditionally; a run's membership change
   * there is re-stamped by the projection's own epoch.
   */
  function writeItemAt(index: number, next: Item): void {
    const previous = items[index];
    if (rowUiRetentionChanged(previous, next)) rowUiRetentionRevision += 1;
    // Same chokepoint logic for the activity-run header: it summarises the
    // rows in a run from five fields, and this is the write that fires at
    // reveal cadence. Comparing them here is what lets the header key on a
    // number instead of rebuilding the tuple for every member per tick.
    if (activityRunSummaryFieldsChanged(previous, next)) {
      activityRuns.noteMemberContentChanged(next.id);
    }
    items[index] = next;
  }
  const rowUiState = createThreadRowUiState({
    getItemById,
    // Read at dispose time, after the caller has already replaced `items`
    // with the surviving window — so this IS the "still loaded" set.
    loadedPayloadRefs: () => items,
  });
  // Per-item smoother + reveal-gate machinery lives in
  // threadStreamingReveal.svelte.ts. Every items-mutation path that can
  // change which top-level rows exist relative to a live smoother must
  // call `streamingReveal.recomputeReveal()` (or `.disposeAll()`).
  const streamingReveal = createThreadStreamingReveal({
    getItemById,
    getItemIndex: (itemId) => itemIndexById.get(itemId),
    getItems: () => items,
    setItemAt: writeItemAt,
    stampLiveContent,
    armStructuralSpring: armLiveContentAppendSpring,
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
  // Activity-run registry: stable run identity across window edges, plus
  // collapse overrides and inner scroll/mount state. Session-only, matching
  // item-expansion leases; the durable layer is the user setting.
  const activityRuns = createThreadActivityRuns({
    defaultCollapsed: () => activityRunDefaultCollapsed(),
    windowRows: () => activityRunWindowRows(),
    scrollController: () => scrollController,
  });
  let contextWindow: ContextWindow | null = $state(null);
  // Rate-limit snapshots live in the global `rateLimitsInfo.svelte.ts`
  // store keyed by provider and account — they are account cache state,
  // not thread state. Keeping them out of each pane means they survive
  // thread switches, turn completions, and metadata updates. The pane
  // only tracks which account its live provider session should select.
  let providerBanner: ProviderStatusEvent | null | undefined =
    $state(undefined);
  let providerSessionAccount: ProviderSessionAccountEvent | null =
    $state(null);
  let providerSessionAccountRevision = 0;
  function updateProviderSessionAccount(
    account: ProviderSessionAccountEvent | null,
  ): void {
    providerSessionAccount = account?.connected ? account : null;
    providerSessionAccountRevision += 1;
  }
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
  // `'session'` ≡ a provider session_died event; `'history-load'` ≡
  // the initial history window failed and can be retried in place.
  let generalErrorKind: 'session' | 'history-load' | null = $state(null);
  function updateHistoryLoadError(message: string | null): void {
    if (message !== null) {
      generalError = message;
      generalErrorKind = 'history-load';
    } else if (generalErrorKind === 'history-load') {
      generalError = null;
      generalErrorKind = null;
    }
  }
  let loading: boolean = $state(false);
  // The spinner-flash gate (`pastSpinnerThreshold` + its timer), the
  // in-flight live-arrival ledger, the window attestation and the
  // replica write-back timer live in threadSwitchLoad.svelte.ts as
  // `switchLoad`, which is their sole writer. The only read from out
  // here is `showLoadingSpinner`'s.

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

  // Read view onto the shared, workspace-keyed git-status store —
  // GitActionsControl, the header diff/PR badges, the Ship Changes wizard,
  // and the review pane's staleness dot all read it. It resolves its key
  // from the pane's current thread on every read, so a thread switch or a
  // worktree move re-points it with no reset of its own: the incoming
  // thread's workspace answers immediately, and the outgoing one's entry is
  // released by whoever attached it. The subscription itself is attached by
  // ChatHeaderActions (see gitStatusStore.svelte.ts).
  const gitStatus: GitStatusView = createGitStatusView(
    () => thread,
    () => (draftPlaceholder ? null : (thread?.id ?? null)),
  );

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
  // See `stableThreadId` above: the equality cutoff between thread-object
  // replacement and consumers keyed on the model string.
  const stableActiveModel = $derived.by(() => effectiveModel || thread?.model || '');
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
    getProviderSessionAccountRevision: () => providerSessionAccountRevision,
    hydrateProviderAccount: (account, expectedMutationRevision) => {
      if (providerSessionAccountRevision !== expectedMutationRevision) return;
      updateProviderSessionAccount(account);
    },
    getEffectiveModelRevision: () => effectiveModelRevision,
    hydrateEffectiveModel: (model, backendRevision, expectedMutationRevision) => {
      if (effectiveModelRevision !== expectedMutationRevision) return;
      if (backendRevision < effectiveModelBackendRevision) return;
      effectiveModelBackendRevision = backendRevision;
      updateEffectiveModel(model);
    },
  });
  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow paged fetch from thread A cannot clobber thread B's
   * items when the user flips between them quickly. Also exposed
   * publicly via the `switchGeneration` getter so MessageTimeline's
   * `$effect.pre` can detect same-thread re-switch (forced reloads
   * that mutate items in place) and re-run its restore reset path —
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
    dropTimelineItems,
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    recomputeReveal: streamingReveal.recomputeReveal,
    // An anchor the OPEN agent pane is scoped to (or holds on its trail)
    // retains its children exactly like an expanded card: the pane is a
    // live view of those rows, so folding them out from under it would
    // blank the very transcript the reader opened.
    isSubagentGroupExpanded: (groupKey: string) =>
      rowUiState.isSubagentGroupExpanded(groupKey) ||
      (thread !== null && agentPaneScopeTrailHolds(paneId, thread.id, groupKey)),
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
  // `raw`, and load-bearing: a plain `$state` PROXIES the controller on
  // assignment, so the object the pane hands back is never `===` the one that
  // registered. Every identity check against it silently fails —
  // `detachScrollController`'s "is this still mine" guard never matched, so the
  // slot was never cleared and a torn-down controller (and through it the whole
  // detached timeline subtree) stayed reachable from the pane for as long as the
  // pane lived. A controller is a handle, not reactive data: nothing reads
  // through it, and every consumer re-reads the slot itself.
  let scrollController: PaneScrollController | null = $state.raw(null);

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
   * Returns whether the gates passed and the controller was armed. The
   * pane data layer is the sole owner of this decision; the call sites
   * are `armLiveContentAppendSpring` below (wire appends to the loaded
   * tail via `applyProviderItemUpserts`, and `recomputeRevealPass`
   * releasing withheld rows) plus the composer's optimistic user-send,
   * which arms WITHOUT the live-content stamp (the send stays a
   * one-shot; see `lastLiveContentAt`). Scroll writes still belong to
   * the controller — the pane only talks to the registered
   * `PaneScrollController` surface, the same seam the
   * `scrollToItemRequest` intent publishes through when a scroll needs
   * virtualizer index resolution.
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
  function armStructuralSpring(): boolean {
    const controller = scrollController;
    if (!controller) return false;
    if (loading) return false;
    if (threadUsesDiscussionSurface(thread)) return false;
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
    return true;
  }

  /**
   * Re-close the warm-up gate for the rows an initial slice just merged
   * into an empty pane. See `PaneScrollController.armWarmup` for why the
   * switch-edge arm alone does not survive the fetch, and
   * `armStructuralSpring` above for the same synchronous-with-the-data
   * ordering contract.
   *
   * Returns whether the gate was re-armed (the cold-load trace records
   * it — a fetch that mounted rows without re-arming is this defect
   * regressing).
   *
   * Two gates:
   * - Nothing mounted (`items` still empty — a genuinely empty thread):
   *   there is no cascade to hide, and holding the gate closed would
   *   sync-pin the first streamed tokens instead of gliding them and
   *   leave the pane behind an empty 2.5s failsafe. Empty panes stay
   *   visible, exactly as the placeholder→materialized path already
   *   treats them.
   * - Discussion surface: those panes register ChannelView's controller,
   *   which owns an unrelated scroll surface — the same reason
   *   `armStructuralSpring` stands down.
   */
  function armInitialSliceWarmup(): boolean {
    if (items.length === 0) return false;
    if (threadUsesDiscussionSurface(thread)) return false;
    const controller = scrollController;
    if (!controller) return false;
    controller.armWarmup();
    return true;
  }

  /**
   * A wire append to the loaded tail (or a reveal-gate release mounting
   * withheld rows) IS live content advancing: arm the structural spring
   * AND stamp live content, sharing the arm's restore gates.
   *
   * Neither signal picks the animation — growth while pinned always
   * glides (see `utils/scroll/resolver.ts#springGateIsOpen`). They tell
   * the controller more content is expected imminently, which keeps the
   * spring sentinel alive across delivery gaps instead of cancelling on
   * each arrival, and lets the viewport-change path distinguish an
   * append from idle composer geometry. The 250ms one-shot covers only
   * the append's first growth delivery; the stamp opens the rolling
   * liveness window a background-task completion needs while its
   * payload preview / markdown / highlight spans settle after turn end.
   */
  function armLiveContentAppendSpring(): void {
    if (armStructuralSpring()) stampLiveContent();
  }

  function rebuildItemIndexes(nextItems: Item[]): void {
    itemIndexById.clear();
    for (let index = 0; index < nextItems.length; index += 1) {
      const item = nextItems[index];
      itemIndexById.set(item.id, index);
    }
  }

  function disposeDroppedItemState(
    droppedItems: readonly Item[],
    exhaustedScope?: ReadonlySet<string>,
  ): void {
    if (droppedItems.length === 0) return;
    // Dropped rows can include hydrated subagent children — re-arm their
    // anchors for hydration. See threadSubagentMemory.ts
    // `resetHydrationExhausted` for the full rationale.
    subagentMemory.resetHydrationExhausted(exhaustedScope);
    for (const item of droppedItems) streamingReveal.disposeSmootherFor(item.id);
    rowUiState.disposeItems(droppedItems);
  }

  /** Set difference, for the callers that hand over a finished array. */
  function droppedItemsBetween(
    previous: readonly Item[],
    nextItems: readonly Item[],
  ): readonly Item[] {
    if (previous.length === 0) return NO_ITEMS;
    const keptIds = new Set<string>();
    for (const item of nextItems) keptIds.add(item.id);
    const dropped: Item[] = [];
    for (const item of previous) {
      if (!keptIds.has(item.id)) dropped.push(item);
    }
    return dropped;
  }

  /**
   * The window-replacement chokepoint. `droppedItems` must be exactly the
   * rows `nextItems` lost, which is why this is private: the two public
   * entry points below each derive it, so no caller can supply a pair
   * that disagrees (a short list leaks row UI state; a long one releases
   * state a surviving row still reads).
   */
  function commitTimelineItems(
    nextItems: Item[],
    droppedItems: readonly Item[],
    exhaustedScope?: ReadonlySet<string>,
  ): boolean {
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
    disposeDroppedItemState(droppedItems, exhaustedScope);
    timelineRevision++;
    // Unconditional: a wholesale replacement can drop an active row, land
    // one, or re-link a payload, and proving otherwise would cost the very
    // walk the revision exists to remove. These paths are rare (prune,
    // reconcile, revert, cache install, eviction) — one extra prune pass
    // is cheaper than the proof.
    rowUiRetentionRevision += 1;
    // Same reasoning, the other consumer: the per-item revision the
    // activity-run headers key on is fed by `writeItemAt`, which a
    // wholesale replacement does not go through. A replace can change
    // every summary-relevant field on rows whose run membership is
    // untouched (the cache paint reconciled by `SyncThreadWindow`), and
    // that is invisible to both of the header's per-run signals.
    activityRuns.noteWholesaleReplace();
    return true;
  }

  function replaceTimelineItems(
    nextItems: Item[],
    options: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
    } = {},
  ): boolean {
    if (items === nextItems) return false;
    return commitTimelineItems(
      nextItems,
      options.disposeDropped ? droppedItemsBetween(items, nextItems) : NO_ITEMS,
      options.exhaustedScope,
    );
  }

  /**
   * Replace the window by dropping the rows `shouldDrop` selects. ONE
   * pass yields both the surviving array and the dropped rows, where
   * `replaceTimelineItems` has to diff the two arrays afterwards — a
   * second full walk plus a Set of every surviving id. Any caller that
   * already knows which rows are leaving belongs here; subagent
   * eviction, which drops a settled subtree on every settling batch, is
   * why it exists. Returns the dropped rows in their previous order; a
   * no-op drop leaves the window untouched, so it costs no revision
   * bump.
   */
  function dropTimelineItems(
    shouldDrop: (item: Item) => boolean,
    options: { exhaustedScope?: ReadonlySet<string> } = {},
  ): Item[] {
    const kept: Item[] = [];
    const dropped: Item[] = [];
    for (const item of items) {
      if (shouldDrop(item)) dropped.push(item);
      else kept.push(item);
    }
    if (dropped.length === 0) return dropped;
    commitTimelineItems(kept, dropped, options.exhaustedScope);
    return dropped;
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

  /**
   * The upsert path's commit chokepoint, and the reason
   * `threadItemStreamApply.ts` does not write `items` itself: the merge
   * in `applyItemUpsertsToWindow` never DROPS a row, so unlike
   * `commitTimelineItems` there is nothing to dispose and no fold to
   * retain — but the same three revisions still have to move, and they
   * move from what the merge already computed rather than from a fresh
   * walk. Index maintenance rides along because the result says which
   * of the two shapes it is (full rebuild vs. tail-append patch).
   */
  function commitUpsertResult(next: ApplyItemUpsertsToWindowResult): void {
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
    if (next.rowUiRetentionChanged) rowUiRetentionRevision += 1;
    for (const id of next.summaryFieldsChangedIds) {
      activityRuns.noteMemberContentChanged(id);
    }
  }

  // The streaming item-application machine (upsertItemsBatch and the
  // applyProviderItemUpserts / applyItemDelta / applyItemMeta /
  // applyItemPatch bodies) lives in threadItemStreamApply.ts as
  // `itemStream`; the thread-switch / window-sync / replica pipeline
  // (including switchThread and refreshFromBackend) lives in
  // threadSwitchLoad.svelte.ts as `switchLoad`. Both are constructed
  // below the last state they capture.

  // Thread live-state hydration protocol (applyPendingInteractiveSnapshot,
  // hydratePendingInteractiveRequests, applyThreadLiveStateSnapshot,
  // hydrateThreadLiveState) lives in threadLiveStateHydration.ts as
  // `liveStateHydration`.

  // Child-transcript hydration for a subagent launch anchor
  // (hydrateSubagentChildren) lives in threadSubagentMemory.ts as
  // `subagentMemory.hydrateChildren`.

  // Shared removal core for every path that takes rows OUT of the
  // timeline on purpose (removeItemsFromTurn / removeRevertedItems /
  // removeItemById). The drop itself and the disposal that follows it
  // belong to `dropTimelineItems` → `disposeDroppedItemState`; what is
  // left here is what removal adds on top: a reveal recompute, and
  // evicting the warm-re-entry cache so a thread-switch restore cannot
  // resurrect rows the user just destroyed.
  //
  // Routing through the chokepoint is also a behavior fix. Hand-rolling
  // the disposal skipped `subagentMemory.resetHydrationExhausted`, and a
  // removal can drop hydrated subagent children while their launch
  // anchor SURVIVES: `removeRevertedItems` keeps the anchor turn's
  // backend-enumerated survivors and drops everything else on that turn,
  // and `removeItemById` drops exactly one row. A surviving anchor still
  // marked exhausted never re-fetches, so the card wedges on its loading
  // placeholder until the thread is switched away and back.
  //
  // Returns the removed items in their previous order; [] when nothing
  // matched (idempotent).
  function removeMatchedItems(shouldRemove: (item: Item) => boolean): Item[] {
    // No `exhaustedScope`: mapping a dropped grandchild back to its launch
    // root would need an ancestor walk over rows we just dropped, and a
    // truncation is exactly the bulk case `resetHydrationExhausted`
    // documents as clearing wholesale.
    const removed = dropTimelineItems(shouldRemove);
    if (removed.length === 0) return removed;
    streamingReveal.recomputeReveal();
    if (thread) switchLoad.dropCachedWindow(thread.id);
    return removed;
  }

  // Thread-switch / window-sync / replica pipeline. Declared last
  // because the ctx captures the sub-factory handles BY VALUE — pane
  // fields it only reads through getters could be declared after it,
  // but a handle could not. Nothing calls into it during construction.
  const switchLoad = createThreadSwitchLoad({
    paneId,
    getThread: () => thread,
    setThread: (next) => {
      thread = next;
    },
    getDraftPlaceholder: () => draftPlaceholder,
    clearDraftPlaceholder: () => {
      draftPlaceholder = null;
    },
    getItems: () => items,
    replaceTimelineItems,
    getSwitchGeneration: () => switchGeneration,
    bumpSwitchGeneration: () => ++switchGeneration,
    getLoading: () => loading,
    setLoading: (value) => {
      loading = value;
    },
    resetLiveContentStamp: () => {
      lastLiveContentAt = 0;
    },
    getScrollController: () => scrollController,
    armInitialSliceWarmup,
    getLatestSettledTurn: () => latestSettledTurn,
    setLatestSettledTurn: (next) => {
      latestSettledTurn = next;
    },
    getContextWindow: () => contextWindow,
    setContextWindow: (next) => {
      contextWindow = next;
    },
    setGeneralError: (message) => {
      generalError = message;
      generalErrorKind = null;
    },
    setHistoryLoadError: updateHistoryLoadError,
    setProviderBanner: (status) => {
      providerBanner = status;
    },
    setProviderSessionAccount: updateProviderSessionAccount,
    setSendInFlight: (value) => {
      sendInFlight = value;
    },
    getShowTerminal: () => showTerminal,
    setShowTerminal: (value) => {
      showTerminal = value;
    },
    setEffectiveModel: updateEffectiveModel,
    optimisticItemIds,
    invalidatedDraftTerminalIds,
    timelineWindow,
    subagentMemory,
    rowUiState,
    activityRuns,
    streamingReveal,
    channelState,
    designState,
    pendingInteractiveState,
    liveTodoState,
    liveStateHydration,
  });

  // Streaming item-application machine. Same construction rule as
  // `switchLoad` above; it also reads that module's in-flight sync
  // ledger, so it is declared after it.
  const itemStream = createThreadItemStreamApply({
    getItems: () => items,
    itemIndexById,
    getThread: () => thread,
    writeItemAt,
    commitUpsertResult,
    getLiveTouchedDuringSync: switchLoad.getLiveTouchedDuringSync,
    stampLiveContent,
    armLiveContentAppendSpring,
    optimisticItemIds,
    timelineWindow,
    subagentMemory,
    streamingReveal,
    designState,
  });

  return {
    // --- Getters (reactive reads) ---
    get paneId() {
      return paneId;
    },
    get thread() {
      return thread;
    },
    get threadId() {
      return stableThreadId;
    },
    /**
     * Key the timeline's scroll state (snapshots, restore identity) is
     * stored under. The base pane's is its thread id; a scoped facade
     * (agentScopeView) overrides it per scope so an agent pane's scroll
     * position never clobbers the main timeline's saved position for the
     * same thread. timelineRestore must key on THIS, never on threadId.
     */
    get scrollStateKey() {
      return stableThreadId;
    },
    get activeModel() {
      return stableActiveModel;
    },
    get effectiveModel() {
      return effectiveModel;
    },
    get terminalThreadId() {
      return stableTerminalThreadId;
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
        switchLoad.closeDraftPlaceholderTerminals(draftPlaceholder.id);
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
    get rowUiRetentionRevision() {
      return rowUiRetentionRevision;
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
    get providerSessionAccount() {
      return providerSessionAccount;
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
      return loading && switchLoad.pastSpinnerThreshold && items.length === 0;
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
    /**
     * Most recent completed turn, or null if the thread has no settled
     * turns yet. Populated from `provider:turn_completed` pushes and
     * from thread-switch rehydration.
     */
    get latestSettledTurn() {
      return latestSettledTurn;
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
        liveThinkingTailChars: streamingStats.liveThinkingTailChars,
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
     * imperatively by ChannelView's scroll controller
     * `liveContentActive` — mirrors `pane.lastLiveContentAt`'s
     * chat-surface role. See
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
    /** Whether the agent companion (a subagent's scoped thread view) is
     *  open for this pane. There is at most one — opening another node
     *  swaps its scope (docs/specs/agent-visibility.md Q4b). */
    get showAgentPane() {
      return isCompanionOpen(paneId, 'agent');
    },
    /**
     * Monotonically increasing counter bumped at the top of every
     * `switchThread`, `clear`, `startDraftPlaceholder`, and
     * `adoptMaterializedDraftThread` call. Exposed so consumers can
     * detect a same-thread re-switch — the path a forced
     * `switchThread(currentThread)` reload takes to replace items
     * in place. `pane.threadId` doesn't change on that path, so
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

    /**
     * Point the pane at `newThread`. The whole pipeline — outgoing
     * snapshot, incoming reset, cache-or-replica paint, the
     * `SyncThreadWindow` convergence and the parallel hydration
     * fan-out — lives in threadSwitchLoad.svelte.ts.
     */
    switchThread(newThread: Thread): Promise<void> {
      return switchLoad.switchThread(newThread);
    },

    /**
     * Re-fetch the visible window from the backend without resetting
     * pane-scoped UI state (terminal / diff panel / draft). Used by the
     * transport-gap consumer when a missed event window forces a full
     * reconcile of the active pane. See threadSwitchLoad.svelte.ts.
     */
    refreshFromBackend(): Promise<void> {
      return switchLoad.refreshFromBackend();
    },

    retryHistoryLoad(): Promise<void> {
      return switchLoad.retryHistoryLoad();
    },

    /**
     * Pane-close counterpart of the thread-switch snapshot: cache the
     * item window (+ durable replica, size priors) so a later reopen is
     * a warm restore, not a cold fetch with an estimate→measure spring
     * (bug-report-20260822T020840Z). Called by `destroyPane` BEFORE
     * `clear()` empties the items. Skips a thread the store no longer
     * lists — deletion flows call `removeThread` (which evicts every
     * cache tier) before closing the panes, and caching here would
     * resurrect the just-evicted window.
     */
    snapshotForClose(): void {
      if (!thread || !getThreadById(thread.id)) return;
      switchLoad.snapshotPaneForClose();
    },

    clear(): void {
      // Any intent staged against the (about-to-be-discarded) placeholder
      // id must die with it — otherwise repeated "+ New" clicks, thread
      // switches, or pane closes leak entries keyed by ids the rest of
      // the app no longer reads. Cleanup is keyed on the placeholder id
      // because real threads keep their entries until the thread itself
      // is removed by the backend.
      if (draftPlaceholder) {
        switchLoad.closeDraftPlaceholderTerminals(draftPlaceholder.id);
        clearWorktreeIntent(draftPlaceholder.id);
      }
      // Companions are per-thread surfaces; an emptied pane keeps none.
      // Covers the explicit clear-pane command and startDraftPlaceholder
      // ("+ New" on a pane that was showing a thread). destroyPane's
      // cascade observer also lands here — second call is a no-op.
      closeCompanionsForSource(paneId);
      thread = null;
      updateEffectiveModel('');
      draftPlaceholder = null;
      replaceTimelineItems([]);
      subagentMemory.clearFolds();
      rowUiState.clear();
      activityRuns.clear();
      streamingReveal.disposeAll();
      // Clearing to empty: drop the live-content stamp too (see
      // installCacheOrFreshState — keeps a stale stamp from springing the
      // next thread's settled content). The switchGeneration bump below
      // also cancels any in-flight structural nudge.
      lastLiveContentAt = 0;
      pendingInteractiveState.clear();
      contextWindow = null;
      providerBanner = undefined;
      updateProviderSessionAccount(null);
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
      liveTodoState.resetForEmptyPane();
      // Same shape for everything the switch/sync pipeline holds against
      // the thread we just dropped — the spinner-threshold timer, the
      // replica write-back timer, the window attestation and the
      // in-flight live-arrival ledger. See `resetPipeline` in
      // threadSwitchLoad.svelte.ts for what each one would otherwise
      // leak into the next thread.
      switchLoad.resetPipeline();
      timelineWindow.resetForFreshThread();
      subagentMemory.clearWindowDerivedState();
      // See switchThread: both `timelineWindow`'s internal
      // `pagingGeneration` and `scrollToItemRequest.nonce` stay
      // monotonic for the pane's lifetime so no consumer observes a
      // regressed counter.
      // Git status needs no reset: it is keyed by workspace in a shared
      // store, so clearing the pane's thread already re-points the view at
      // "no workspace".
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
      // "+ New" on a pane showing a thread is a leaving-the-thread edge
      // like close: cache the window first so returning to that thread
      // is a warm restore.
      this.snapshotForClose();
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
          await switchLoad.migrateDraftPlaceholderTerminals(
            placeholderId,
            created.id,
          );
          // Re-key any intent staged against the placeholder id BEFORE we
          // adopt the real thread. Worktree/branch picks made on the
          // placeholder otherwise become orphaned when lookups switch to
          // the materialized thread id.
          migrateWorktreeIntent(placeholderId, created.id);
          seedDefaultWorktreeIntentForDraft(created);
          // An APPLIED intent was consumed by the create: the placeholder
          // was stamped with the branch/worktree it produced, and those
          // fields went out as CreateThread's worktreePath / branch, which
          // is also what makes the backend adopt the unbound setup run.
          // Anything still merely STAGED survives, so a send that fails
          // after this point re-binds on the retry. Cleared after the seed
          // so a default-worktree setting cannot re-stage on top.
          if (worktreeIntentForThread(created).applied) {
            clearWorktreeIntent(created.id);
          }
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
    requestScrollToItem(itemID: string): void {
      if (!itemID) return;
      scrollToItemRequest = {
        itemId: itemID,
        nonce: scrollToItemRequest.nonce + 1,
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
      // controller during fast thread switches. Depends on the slot being
      // `$state.raw`; see its declaration.
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
      return itemStream.upsertItemsBatch([item]) !== null;
    },

    /**
     * Merge a batch of Items from `provider:item_event` into the timeline.
     * The final state is still the backend-authored transcript, but bursts
     * only allocate/sort/bump revision once.
     */
    upsertItems(incoming: Item[]): boolean {
      return itemStream.upsertItemsBatch(incoming) !== null;
    },

    /**
     * Provider event fan-out needs the applied changed rows, not just a
     * boolean, so scroll latches are based on visible-window changes after
     * the pane has filtered below-floor history rows. See
     * threadItemStreamApply.ts.
     */
    applyProviderItemUpserts(
      incoming: Item[],
    ): ApplyItemUpsertsToWindowResult | null {
      return itemStream.applyProviderItemUpserts(incoming);
    },

    /**
     * Remove a single item from the pane's timeline by id. Returns the
     * removed Item so optimistic callers (revert-on-interrupt) can
     * re-insert it on rollback. Idempotent: returns null when the row
     * is already gone, so a late `user_message:reverted` event after
     * the optimistic remove is a no-op.
     *
     * `expectedThreadId` is required rather than optional because every
     * caller reaches here across an await or an event hop, and the pane
     * may have moved on: item ids are per-thread (`user:<n>` collides
     * across threads by construction), and the removal also drops the
     * thread's cached window. Naming the thread is the enforcement — a
     * caller cannot reach the removal without saying which conversation
     * it believes it is editing, and a mismatch is a no-op.
     */
    removeItemById(itemId: string, expectedThreadId: string): Item | null {
      if (!thread || thread.id !== expectedThreadId) return null;
      // The index answers "is it here?" in O(1); the removal itself goes
      // through the same core as the truncation paths so a one-row drop
      // cannot diverge from a bulk one. The predicate walk replaces the
      // `items.filter` this used to do — the same single pass, now
      // yielding the dropped row instead of discarding it.
      if (!itemIndexById.has(itemId)) return null;
      const [removed] = removeMatchedItems((it) => it.id === itemId);
      return removed ?? null;
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
      return removeMatchedItems((it) => it.turnIndex >= fromTurnIndex);
    },

    /**
     * Mirror a backend conversation revert exactly: remove every item
     * with `turnIndex > turnIndex`, and within the anchor turn itself
     * remove everything NOT named in `keptAnchorTurnItemIds` — the
     * survivor list the `user_message:reverted` event carries from
     * `DeleteConversationFromItem`. The kept-set formulation (rather
     * than a removed-set) is load-bearing: pane-only rows that were
     * never persisted are absent from any backend enumeration, and a
     * kept-set removes them for free. An empty list degenerates to
     * `removeItemsFromTurn(turnIndex)`. Idempotent like its sibling.
     */
    removeRevertedItems(turnIndex: number, keptAnchorTurnItemIds: string[]): Item[] {
      if (!Number.isFinite(turnIndex)) return [];
      if (keptAnchorTurnItemIds.length === 0) {
        return removeMatchedItems((it) => it.turnIndex >= turnIndex);
      }
      const kept = new Set(keptAnchorTurnItemIds);
      return removeMatchedItems(
        (it) => it.turnIndex > turnIndex || (it.turnIndex === turnIndex && !kept.has(it.id)),
      );
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

    /**
     * Test-only: is the "ids the wire touched while a window sync was in
     * flight" ledger still armed? Non-null outside a load leg means a
     * clear/throw path leaked it, which silently changes how the NEXT
     * page application classifies pre-existing rows. Not part of the
     * production surface.
     */
    __syncLedgerArmedForTest(): boolean {
      return switchLoad.getLiveTouchedDuringSync() !== null;
    },

    /** Append a streaming text delta to a loaded row. See threadItemStreamApply.ts. */
    applyItemDelta(evt: ItemDeltaEvent): void {
      itemStream.applyItemDelta(evt);
    },

    /** Replace a loaded row's re-validated meta blob. See threadItemStreamApply.ts. */
    applyItemMeta(evt: ItemMetaEvent): void {
      itemStream.applyItemMeta(evt);
    },

    /** Apply a field patch to a loaded row. See threadItemStreamApply.ts. */
    applyItemPatch(evt: ItemPatchEvent): void {
      itemStream.applyItemPatch(evt);
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
    /** Clamped user-message text the reader opened. Ephemeral per session —
     *  nothing about it goes to the backend. */
    isUserMessageExpanded: rowUiState.isUserMessageExpanded,
    setUserMessageExpanded: rowUiState.setUserMessageExpanded,
    diffCardExpandedOverride: rowUiState.diffCardExpandedOverride,
    setDiffCardExpanded: rowUiState.setDiffCardExpanded,
    /** Validity stamp for replaying a measured-size priors snapshot across a
     *  thread switch — see utils/virtual/priors.ts. */
    expansionSignature: rowUiState.expansionSignature,
    hasUserExpansionWithin: rowUiState.hasUserExpansionWithin,
    activityRuns,
    attachmentCacheFor: rowUiState.attachmentCacheFor,
    pruneRowUiState(retention: RowUiStateRetention): void {
      rowUiState.pruneRowUiState(retention);
      streamingReveal.pruneSettledThinkingTails(retention.itemIds);
    },
    // Full revealed text for a reasoning-tail row. Live while the row
    // streams and retained across a content-consistent settle (see
    // threadStreamingReveal.svelte.ts `itemLiveThinkingTail` for the
    // lifetime and the wrap-stability rationale). Returns null once the
    // entry is dropped (overwrite settle, removal, offscreen prune,
    // thread switch) — callers fall back to `item.summary`.
    liveThinkingTailForItem(itemId: string): string | null {
      return streamingReveal.liveThinkingTailFor(itemId);
    },

    /**
     * True while a per-item smoother still owns this row's summary
     * writes — i.e. the reveal is mid-drain, including the multi-second
     * tail AFTER the wire settles the item's status to terminal.
     * Reactive (SvelteMap-backed): row components derive their rendered
     * streaming mode from `status === 'streaming' || isItemSmoothing`,
     * so the streaming markdown guards hold until the drain finishes
     * rather than dropping at wire settle while text is still growing.
     */
    isItemSmoothing(itemId: string): boolean {
      return streamingReveal.isSmoothing(itemId);
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

    setHistoryLoadError(message: string | null): void {
      updateHistoryLoadError(message);
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

    /**
     * Open the one-shot structural-append spring window for a mutation
     * this pane is about to apply outside the wire-upsert path. Sole
     * external caller today: the composer's optimistic user-send, so the
     * just-sent message glides in through the spring instead of
     * sync-pinning (the send deliberately does NOT stamp the
     * live-content latch — one append wants one spring window, not
     * 500ms of spring eligibility for unrelated reflows). Owns the
     * loading/discussion gates; call it synchronously BEFORE the item
     * mutation so the window is open when the flush delivers geometry.
     */
    armStructuralSpring,

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

    setProviderSessionAccount(account: ProviderSessionAccountEvent | null): void {
      updateProviderSessionAccount(account);
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
      // end-of-turn drain, and no successor-waiting one either: rushed
      // reveal motion read as jank both times. A long final message
      // finishing a few seconds after the wire settles is the accepted
      // trade for uniform reveal speed. Nothing is skipped to shorten that
      // wait either — the bursty wire's idle gaps are what let the drain
      // catch back up, so a queued row's wait is transient without a rush.
      // The deferred window prune does NOT run here: wire settle is not
      // visual quiet — the reveal above keeps draining for seconds. A
      // mounted timeline records the prune as pending and the quiet
      // scheduler (timelineQuietWork) runs it once nothing is animating;
      // a pane with no timeline behind it prunes immediately.
      timelineWindow.settleRecentWindowPrune();
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

    /**
     * Open the agent companion scoped to `launchItemId` (an Agent/Task row,
     * a forked Skill, a Codex spawn_agent — whatever the card was for), or
     * re-scope the one already open. Opened from CARDS only; there is no
     * header button, because "which agent" is not a question a header can
     * answer.
     */
    openAgentPane(launchItemId: string, label: string): void {
      if (!thread?.id) return;
      openAgentCompanion(paneId, thread.id, launchItemId, label);
    },

    closeAgentPane(): void {
      const companion = companionForSource(paneId, 'agent');
      if (companion) closeCompanion(companion.paneId);
      // Explicitly closing drops the trail: the next open arrives with its
      // own launch row, so a retained breadcrumb could only be stale.
      disposeAgentStateForPane(paneId);
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
