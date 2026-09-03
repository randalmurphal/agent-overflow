// Scoped ThreadPane facade for the agent companion pane.
//
// The agent pane is a NORMAL thread pane — the real MessageTimeline with
// its virtualizer, scroll physics, activity runs, paging plumbing — whose
// visible window is one subagent launch's subtree. Rather than teach the
// timeline a "scope mode", this module answers the ThreadPane surface
// with a PLAIN OBJECT over the source pane that overrides exactly the
// members where a scoped view legitimately differs, and forwards
// everything else (item resolution, payload loads, approvals, expansion
// registries, live-aggregate reads) so nothing here can drift from the
// chat surface.
//
// Plain object, not a Proxy, and that is a type-checking decision. A
// Proxy forwards whatever it is asked for, so neither half of the
// forward set is checkable: a member added to `ThreadPane` silently
// forwards (nobody is told the scoped view now answers something it has
// never thought about), and a member REMOVED from `ThreadPane` leaves a
// dead override entry that compiles forever. Here the forward set is a
// literal of getters checked with `satisfies Pick<ThreadPane, …>`, which
// fails in both directions: a missing member is a missing property and a
// stale one is an excess property. `AgentScopeOverride` names the
// divergences and `AgentScopeForwarded` is everything else, so adding a
// pane member forces a deliberate choice between the two lists.
//
// Every forward is a GETTER, never a copied value. The source pane's
// fields are `$state`; delegating at READ time is what preserves their
// reactivity, exactly as the Proxy's property-access delegation did.
// Nothing here may spread `forwarded` — a spread evaluates every getter
// once and freezes the result.
//
// The view's TYPE stays the whole `ThreadPane` rather than a union of
// the narrow roles in `threadPaneRoles.ts`, and that is forced rather
// than lazy: this object is mounted as `MessageTimeline`'s `pane` prop,
// and the timeline hands the same object down to `TimelineLeaf` →
// `UserMessage` → `UserMessageEditor` → `ComposerInputSurface`, one of
// the two places that guide names as deliberately staying on
// `ThreadPane` (the surface runs `/` commands and reaches
// `makeCommandContext` plus the whole command-action surface). Narrowing
// here would mean widening a role until it re-described the pane. The
// exhaustiveness check below is what the roles would have bought.
//
// The override table IS the design — each entry names why the scoped
// view diverges:
//
// - `paneId` / `scrollStateKey`: distinct identity. chatDomIds scopes
//   disclosure ids by paneId (the same item can be mounted in both
//   surfaces at once), and timelineRestore keys scroll snapshots and
//   restore bookkeeping by scrollStateKey (per SCOPE, so an agent pane's
//   position never clobbers the main timeline's saved position).
// - `agentScopeRootId`: the launch this view is scoped to. Empty on the
//   thread timeline; rows read it to know which surface they are on. It
//   is always a TRANSCRIPT ROOT: a §E6 resume carrier's rows are parented
//   to the original launch, so every opener resolves through
//   `utils/subagentLaunch.ts#agentScopeRootId` before scoping here.
// - `items`: the scope's loaded subtree. Direct children get their
//   `parentId` LIFTED (cleared) so the grouping treats them as this
//   surface's top level. A direct child launch remains a card, but its own
//   transcript stays outside this scope. Opening that row changes scope
//   through the breadcrumb instead of recursively embedding panes. Read
//   groups still group and activity runs still wrap. Completion siblings of
//   direct child launches ride along by `completionOf` (they carry no
//   parentId of their own), so nested cards fold status correctly.
// - `revealBoundary`: null. The reveal gate sequences TOP-LEVEL rows of
//   the main transcript; child rows were never reveal-sequenced, and a
//   boundary id from the main thread must not withhold scoped rows.
// - `timelineTurns`: the scope IS one turn. A subagent's rows all carry
//   the LAUNCH's turn (invariants.md §10) across however many provider
//   turns the agent outlives, so keying the response decorations on
//   `item.turnIndex` plus the thread's active/settled turn put a
//   "Response 1m 58s" pill on a still-running agent the moment the
//   launching turn settled (live regression 2026-08-22). Here every
//   scoped row shares one key, whatever turn it carries; the
//   turn is active while the scope's LIFECYCLE row runs and settles on
//   that row's own completion, with the agent's own duration. The
//   lifecycle row is the launch, or the latest resume carrier bound to
//   it — the root settled when round one did.
// - `activityRuns`: an own registry. Run membership differs per surface
//   (the scoped list has different top-level rows), and collapse state
//   is a view concern, so sharing the source registry would let one
//   surface's collapse mutate the other's geometry.
// - scroll controller / scroll-to-item: own slots, same reason — the
//   source pane's slots belong to the main MessageTimeline instance.
// - paging (`loadOlder`/`loadNewer`/`loadUntilItem`, the `hasMore*` and
//   `loading*` flags): a scope is not a turn window. Everything the
//   scope can show is either loaded or fetched wholesale through
//   `ensureSubagentChildren` (the pane body drives that; see
//   AgentPane.svelte), so the timeline's edge-paging must never fire.
//   `hasDeferredRecentWindowPrune` / `retryDeferredRecentWindowPrune`
//   ride with them: the deferred prune describes the SOURCE window, and
//   a scoped instance retrying it would run the host's bookkeeping.
// - `loading` / `showLoadingSpinner`: false. Both describe the source
//   window's switch/page load; the scope's rows are already local or
//   arriving through hydration, which the pane body renders its own
//   states for.
// - `openAgentPane`: descend-in-place. Inside the pane, opening a child
//   card grows the breadcrumb (`pushScope`) instead of re-seeding the
//   companion from the outside.
// - `pruneRowUiState`: no-op. Row-UI state (expansion handles, attachment
//   blob caches, thinking tails) is SHARED with the source pane by
//   design, and each MessageTimeline instance's prune pass computes
//   retention from ITS OWN revealed rows — a scoped instance's retention
//   describes one subtree, so letting it reach the shared store would
//   revoke the attachment blobs and expansion state of every main-
//   timeline row (live incident 2026-08-22: pasted screenshots went
//   dead the moment the agent pane's prune ran). The host pane's own
//   prune stays the one bounded-memory owner, and it spares the open
//   scope's rows in return (thread.svelte.ts widens its retention via
//   `collectAgentScopeRetainedIds`).
//
// One instance per (pane body mount × scope): AgentPane keys the
// timeline on the scope id, so a scope swap builds a fresh view and a
// fresh MessageTimeline — scroll restore, warmup, and run identity all
// start clean, exactly like a thread switch.

import type { ThreadPane } from './thread.svelte';
import type { AgentPaneState } from './agentPane.svelte';
import type { Item } from '../types/models';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import {
  activityRunDefaultCollapsed,
  activityRunWindowRows,
} from './activityRunPrefs.svelte';
import type {
  LoadOlderResult,
  PaneScrollController,
  ScrollToItemRequest,
} from './threadPaneShared';
import type { TimelineTurnFacet } from './threadTurnProjection';
import { claudeResumeTranscriptRootId } from '../utils/subagentLaunch';

/** The one turn key every scoped row shares (see `timelineTurns` above). */
const AGENT_SCOPE_TURN_KEY = 0;

export interface AgentScopeView {
  /** The ThreadPane facade MessageTimeline mounts. */
  readonly pane: ThreadPane;
  /** The scope's loaded subtree (what `pane.items` answers). */
  readonly items: Item[];
  /**
   * The row whose STATUS is the scope's: the launch, or the latest §E6
   * resume carrier bound to it. The turn facet, the composer shell's
   * run state, its elapsed timer, its progress ticks and its Stop target
   * all read this row; identity (name, model, description) reads the
   * launch. One resolver so the two can never disagree.
   */
  readonly lifecycle: Item | undefined;
  /** The `completionOf` sibling of `lifecycle`, once one has landed. */
  readonly lifecycleCompletion: Item | undefined;
  /** Release the view's own registries. Call on unmount. */
  dispose(): void;
}

/**
 * The members the scoped view answers ITSELF. One entry per paragraph of
 * the override table in the module header; adding to this list without a
 * matching `overrides` entry is a compile error, and so is the reverse.
 */
type AgentScopeOverride =
  | 'paneId'
  | 'scrollStateKey'
  | 'agentScopeRootId'
  | 'items'
  | 'revealBoundary'
  | 'timelineTurns'
  | 'activityRuns'
  | 'scrollController'
  | 'attachScrollController'
  | 'detachScrollController'
  | 'scrollToItemRequest'
  | 'requestScrollToItem'
  | 'loadOlder'
  | 'loadNewer'
  | 'loadUntilItem'
  | 'hasMoreHistory'
  | 'hasMoreNewer'
  | 'loadingOlder'
  | 'loadingNewer'
  | 'hasDeferredRecentWindowPrune'
  | 'retryDeferredRecentWindowPrune'
  | 'pruneRowUiState'
  | 'loading'
  | 'showLoadingSpinner'
  | 'openAgentPane';

/**
 * Everything else on the pane. Forwarded verbatim, and exhaustively:
 * `ThreadPane` gaining a member puts it here, where the `forwarded`
 * literal below has to grow an entry or fail to compile.
 */
type AgentScopeForwarded = Exclude<keyof ThreadPane, AgentScopeOverride>;

/** Settled no-op paging result: a scope window has no edges to page. */
const NO_PAGE: Promise<LoadOlderResult> = Promise.resolve({
  insertedBeforeWindow: false,
  insertedRows: false,
  status: 'noop',
});

/**
 * Every loaded row the scope at `scopeItemId` renders or needs as a direct
 * navigation edge: the scope row itself, its direct children, and the
 * completion siblings of those rows. A child's descendants belong to the
 * child's own pane scope and are deliberately not retained here.
 *
 * Two consumers, one truth: the facade's item window filters through
 * this set, and the source pane's row-UI prune widens its retention
 * with it so the chat timeline's bounded-memory pass cannot dispose
 * state under rows the agent pane has mounted.
 */
export function collectAgentScopeRetainedIds(
  items: readonly Item[],
  scopeItemId: string,
): Set<string> {
  const retained = new Set<string>();
  if (!scopeItemId) return retained;
  for (const item of items) {
    if (item.parentId === scopeItemId) retained.add(item.id);
  }
  retained.add(scopeItemId);
  for (const item of items) {
    if (item.completionOf && retained.has(item.completionOf)) {
      retained.add(item.id);
    }
  }
  return retained;
}

export function createAgentScopeView(
  sourcePane: ThreadPane,
  agent: AgentPaneState,
  scopeItemId: string,
): AgentScopeView {
  // ---- Scoped item window ---------------------------------------------
  // Recomputed per source timelineRevision (the projection reads items
  // untracked behind that revision, so identity churn outside a revision
  // bump would be invisible anyway — matching the source pane's own
  // contract). Direct children are cloned with parentId lifted. A nested
  // launch's descendants do not enter this window.
  //
  // One ordered pass over the source items, so the window keeps the
  // timeline's document order exactly. The membership set comes from
  // `collectAgentScopeRetainedIds` (direct rows + completion siblings); the
  // SCOPE's own row and its completion sibling stay out — they feed the
  // pane's breadcrumb and status line, not the transcript.
  // The same pass collects the scope's §E6 resume CARRIERS — the rows
  // Claude rebinds the agent's task onto to resume it. They are top-level
  // rows of the source timeline (nothing is ever parented to one), so
  // they are deliberately not in the window; they are the scope's
  // lifecycle rows, read below.
  let scopeWindow = $derived.by<{ items: Item[]; carriers: Item[] }>(() => {
    void sourcePane.timelineRevision;
    if (!scopeItemId) return { items: [], carriers: [] };
    const retained = collectAgentScopeRetainedIds(sourcePane.items, scopeItemId);
    const items: Item[] = [];
    const carriers: Item[] = [];
    for (const item of sourcePane.items) {
      if (claudeResumeTranscriptRootId(item) === scopeItemId) carriers.push(item);
      if (item.id === scopeItemId || !retained.has(item.id)) continue;
      if (item.completionOf === scopeItemId) continue;
      items.push(item.parentId === scopeItemId ? { ...item, parentId: undefined } : item);
    }
    return { items, carriers };
  });
  let scopedItems = $derived(scopeWindow.items);

  // ---- Scope lifecycle as the timeline's turn ---------------------------
  // Status reads go through the SOURCE pane's live row (`getItemById`,
  // the row's own box), so a launch flipping to terminal re-derives
  // without a structural revision. The completion sibling is the status
  // source once it exists — same rule the card and the composer shell
  // follow. Its MEMBERSHIP comes from the array (structure); its fields
  // must not, because a patch to the row is written in place and the
  // array signal stays silent for it.
  //
  // The LIFECYCLE row is the launch for an ordinary agent and the LATEST
  // resume carrier for a resumed one (claude-wire.md §E6): the scope root
  // settled when its first round did, so reading status from it would
  // settle the pane's turn while round two runs. Elapsed counts from the
  // resume, which is what the reader is watching (user ruling).
  let lifecycle = $derived.by<Item | undefined>(() => {
    const root = scopeItemId ? sourcePane.getItemById(scopeItemId) : undefined;
    let latest = root;
    // `>=` with the carriers in timeline order: the latest carrier wins,
    // and a carrier always outranks the root at an equal timestamp
    // because it is written after it.
    for (const carrier of scopeWindow.carriers) {
      const live = sourcePane.getItemById(carrier.id) ?? carrier;
      if (!latest || live.createdAt >= latest.createdAt) latest = live;
    }
    return latest;
  });
  let lifecycleCompletion = $derived.by<Item | undefined>(() => {
    const lifecycleId = lifecycle?.id;
    if (!lifecycleId) return undefined;
    const completion = sourcePane.items.find((item) => item.completionOf === lifecycleId);
    return completion ? (sourcePane.getItemById(completion.id) ?? completion) : undefined;
  });
  const timelineTurns: TimelineTurnFacet = {
    keyOf: () => AGENT_SCOPE_TURN_KEY,
    get activeKey() {
      const status = (lifecycleCompletion ?? lifecycle)?.status;
      return status === 'running' || status === 'streaming' ? AGENT_SCOPE_TURN_KEY : null;
    },
    get settled() {
      const launch = lifecycle;
      const statusItem = lifecycleCompletion ?? launch;
      if (!launch || !statusItem) return null;
      if (statusItem.status === 'running' || statusItem.status === 'streaming') return null;
      return {
        key: AGENT_SCOPE_TURN_KEY,
        startedAt: launch.createdAt,
        completedAt: statusItem.updatedAt,
      };
    },
  };

  // ---- Own view registries ---------------------------------------------
  let scrollController: PaneScrollController | null = $state.raw(null);
  let scrollToItemRequest = $state.raw<ScrollToItemRequest>({ itemId: '', nonce: 0 });
  const activityRuns = createThreadActivityRuns({
    defaultCollapsed: () => activityRunDefaultCollapsed(),
    windowRows: () => activityRunWindowRows(),
    scrollController: () => scrollController,
  });

  const overrides = {
    get paneId() {
      return `${sourcePane.paneId}~agent`;
    },
    get scrollStateKey() {
      return `${sourcePane.threadId ?? ''}~agent:${scopeItemId}`;
    },
    get agentScopeRootId() {
      return scopeItemId;
    },
    get items() {
      return scopedItems;
    },
    get revealBoundary() {
      return null;
    },
    get timelineTurns() {
      return timelineTurns;
    },
    get activityRuns() {
      return activityRuns;
    },
    get scrollController() {
      return scrollController;
    },
    attachScrollController(controller: PaneScrollController): void {
      scrollController = controller;
    },
    detachScrollController(controller: PaneScrollController): void {
      if (scrollController === controller) scrollController = null;
    },
    get scrollToItemRequest() {
      return scrollToItemRequest;
    },
    requestScrollToItem(itemID: string): void {
      if (!itemID) return;
      scrollToItemRequest = { itemId: itemID, nonce: scrollToItemRequest.nonce + 1 };
    },
    // A scope never edge-pages; its rows arrive wholesale via
    // ensureSubagentChildren (driven by the pane body).
    loadOlder: () => NO_PAGE,
    loadNewer: () => NO_PAGE,
    loadUntilItem: (itemID: string) =>
      Promise.resolve(scopedItems.some((item) => item.id === itemID)),
    get hasMoreHistory() {
      return false;
    },
    get hasMoreNewer() {
      return false;
    },
    get loadingOlder() {
      return false;
    },
    get loadingNewer() {
      return false;
    },
    get hasDeferredRecentWindowPrune() {
      return false;
    },
    retryDeferredRecentWindowPrune(): void {},
    // See the module header: a scoped instance's retention describes one
    // subtree, so its prune pass must never reach the SHARED row-UI
    // store. The host pane's own prune is the one bounded-memory owner.
    pruneRowUiState(): void {},
    // The scope's rows are already local (or arriving via hydration);
    // the thread-level loading states describe the SOURCE window.
    get loading() {
      return false;
    },
    get showLoadingSpinner() {
      return false;
    },
    openAgentPane(launchItemId: string, label: string): void {
      agent.pushScope(launchItemId, label);
    },
  } satisfies Pick<ThreadPane, AgentScopeOverride>;

  // ---- Forwarded surface ------------------------------------------------
  // Read the module header before touching this: every entry is a getter
  // that reaches the source pane at READ time (reactivity), and the
  // `satisfies` is the exhaustiveness check in both directions. Order
  // follows `thread.svelte.ts`'s own return object, so the two read as a
  // pair.
  const forwarded = {
    get thread() { return sourcePane.thread; },
    get threadId() { return sourcePane.threadId; },
    get workspace() { return sourcePane.workspace; },
    get activeModel() { return sourcePane.activeModel; },
    get effectiveModel() { return sourcePane.effectiveModel; },
    get terminalThreadId() { return sourcePane.terminalThreadId; },
    get draftPlaceholder() { return sourcePane.draftPlaceholder; },
    get hasDraftPlaceholder() { return sourcePane.hasDraftPlaceholder; },
    get canCompose() { return sourcePane.canCompose; },
    get lastLiveContentAt() { return sourcePane.lastLiveContentAt; },
    get markLiveContentAdvanced() { return sourcePane.markLiveContentAdvanced; },
    get setDraftPlaceholderMode() { return sourcePane.setDraftPlaceholderMode; },
    get applyDraftPlaceholderDefaults() { return sourcePane.applyDraftPlaceholderDefaults; },
    get applyDraftPlaceholderWorkspace() { return sourcePane.applyDraftPlaceholderWorkspace; },
    get dematerializeEmptyDraftThread() { return sourcePane.dematerializeEmptyDraftThread; },
    get isLocked() { return sourcePane.isLocked; },
    get timelineRevision() { return sourcePane.timelineRevision; },
    get rowUiRetentionRevision() { return sourcePane.rowUiRetentionRevision; },
    get getItemById() { return sourcePane.getItemById; },
    get pendingApprovals() { return sourcePane.pendingApprovals; },
    get pendingUserInputs() { return sourcePane.pendingUserInputs; },
    get contextWindow() { return sourcePane.contextWindow; },
    get providerBanner() { return sourcePane.providerBanner; },
    get providerSessionAccount() { return sourcePane.providerSessionAccount; },
    get generalError() { return sourcePane.generalError; },
    get generalErrorKind() { return sourcePane.generalErrorKind; },
    get paneErrorList() { return sourcePane.paneErrorList; },
    get sendInFlight() { return sourcePane.sendInFlight; },
    get showTerminal() { return sourcePane.showTerminal; },
    get gitStatus() { return sourcePane.gitStatus; },
    get canAdoptOpenedTerminal() { return sourcePane.canAdoptOpenedTerminal; },
    get latestSettledTurn() { return sourcePane.latestSettledTurn; },
    get oldestLoadedCursor() { return sourcePane.oldestLoadedCursor; },
    get newestLoadedCursor() { return sourcePane.newestLoadedCursor; },
    get oldestLoadedTurnIndex() { return sourcePane.oldestLoadedTurnIndex; },
    get newestLoadedTurnIndex() { return sourcePane.newestLoadedTurnIndex; },
    get debugMemoryStats() { return sourcePane.debugMemoryStats; },
    get channelMessages() { return sourcePane.channelMessages; },
    get channelStatus() { return sourcePane.channelStatus; },
    get channelTurnCount() { return sourcePane.channelTurnCount; },
    get channelMaxTurns() { return sourcePane.channelMaxTurns; },
    get channelAwaitingResponse() { return sourcePane.channelAwaitingResponse; },
    get channelCurrentSpeakerRole() { return sourcePane.channelCurrentSpeakerRole; },
    get channelParticipants() { return sourcePane.channelParticipants; },
    get channelLiveTail() { return sourcePane.channelLiveTail; },
    get channelLastLiveContentAt() { return sourcePane.channelLastLiveContentAt; },
    get showPlanSidebar() { return sourcePane.showPlanSidebar; },
    get showReviewPane() { return sourcePane.showReviewPane; },
    get showAgentPane() { return sourcePane.showAgentPane; },
    get switchGeneration() { return sourcePane.switchGeneration; },
    get switchThread() { return sourcePane.switchThread; },
    get refreshFromBackend() { return sourcePane.refreshFromBackend; },
    get retryHistoryLoad() { return sourcePane.retryHistoryLoad; },
    get snapshotForClose() { return sourcePane.snapshotForClose; },
    get clear() { return sourcePane.clear; },
    get startDraftPlaceholder() { return sourcePane.startDraftPlaceholder; },
    get materializeDraftPlaceholder() { return sourcePane.materializeDraftPlaceholder; },
    get adoptMaterializedDraftThread() { return sourcePane.adoptMaterializedDraftThread; },
    get ensureMaterializedThread() { return sourcePane.ensureMaterializedThread; },
    get ensureSubagentChildren() { return sourcePane.ensureSubagentChildren; },
    get loadRecentTail() { return sourcePane.loadRecentTail; },
    get addApproval() { return sourcePane.addApproval; },
    get removeApproval() { return sourcePane.removeApproval; },
    get addUserInput() { return sourcePane.addUserInput; },
    get removeUserInput() { return sourcePane.removeUserInput; },
    get upsertItem() { return sourcePane.upsertItem; },
    get upsertItems() { return sourcePane.upsertItems; },
    get applyProviderItemUpserts() { return sourcePane.applyProviderItemUpserts; },
    get removeItemById() { return sourcePane.removeItemById; },
    get removeItemsFromTurn() { return sourcePane.removeItemsFromTurn; },
    get removeRevertedItems() { return sourcePane.removeRevertedItems; },
    get __flushItemSmoothersForTest() { return sourcePane.__flushItemSmoothersForTest; },
    get __itemSmootherCountForTest() { return sourcePane.__itemSmootherCountForTest; },
    get __syncLedgerArmedForTest() { return sourcePane.__syncLedgerArmedForTest; },
    get applyItemDelta() { return sourcePane.applyItemDelta; },
    get applyItemMeta() { return sourcePane.applyItemMeta; },
    get applyItemPatch() { return sourcePane.applyItemPatch; },
    get expansionStateFor() { return sourcePane.expansionStateFor; },
    get retainExpansionStateFor() { return sourcePane.retainExpansionStateFor; },
    get expansionStateForPayload() { return sourcePane.expansionStateForPayload; },
    get retainExpansionStateForPayload() { return sourcePane.retainExpansionStateForPayload; },
    get isSubagentGroupExpanded() { return sourcePane.isSubagentGroupExpanded; },
    get toggleSubagentGroupExpanded() { return sourcePane.toggleSubagentGroupExpanded; },
    get subagentLiveAggregate() { return sourcePane.subagentLiveAggregate; },
    get isUserMessageExpanded() { return sourcePane.isUserMessageExpanded; },
    get setUserMessageExpanded() { return sourcePane.setUserMessageExpanded; },
    get diffCardExpandedOverride() { return sourcePane.diffCardExpandedOverride; },
    get setDiffCardExpanded() { return sourcePane.setDiffCardExpanded; },
    get expansionSignature() { return sourcePane.expansionSignature; },
    get hasUserExpansionWithin() { return sourcePane.hasUserExpansionWithin; },
    get attachmentCacheFor() { return sourcePane.attachmentCacheFor; },
    get liveThinkingTailForItem() { return sourcePane.liveThinkingTailForItem; },
    get isItemSmoothing() { return sourcePane.isItemSmoothing; },
    get assistantRevealRegistrationGeneration() {
      return sourcePane.assistantRevealRegistrationGeneration;
    },
    get registerAssistantRevealSink() { return sourcePane.registerAssistantRevealSink; },
    get assistantMarkdownParserSource() { return sourcePane.assistantMarkdownParserSource; },
    get assistantMarkdownSourceAppend() { return sourcePane.assistantMarkdownSourceAppend; },
    // Forwarded, not overridden: the source pane owns the smoothers, and
    // the scope view is a projection of the same drain, not a second one.
    get smoothingItemCount() { return sourcePane.smoothingItemCount; },
    get snapSmoothersToReceived() { return sourcePane.snapSmoothersToReceived; },
    get setPaneError() { return sourcePane.setPaneError; },
    get clearPaneError() { return sourcePane.clearPaneError; },
    get setGeneralError() { return sourcePane.setGeneralError; },
    get setSessionError() { return sourcePane.setSessionError; },
    get setHistoryLoadError() { return sourcePane.setHistoryLoadError; },
    get clearGeneralError() { return sourcePane.clearGeneralError; },
    get clearSessionError() { return sourcePane.clearSessionError; },
    get setSendInFlight() { return sourcePane.setSendInFlight; },
    get armStructuralSpring() { return sourcePane.armStructuralSpring; },
    get trackOptimisticItem() { return sourcePane.trackOptimisticItem; },
    get isOptimisticItem() { return sourcePane.isOptimisticItem; },
    get untrackOptimisticItem() { return sourcePane.untrackOptimisticItem; },
    get setContextWindow() { return sourcePane.setContextWindow; },
    get clearContextWindow() { return sourcePane.clearContextWindow; },
    get setProviderBanner() { return sourcePane.setProviderBanner; },
    get setProviderSessionAccount() { return sourcePane.setProviderSessionAccount; },
    get setActiveTurn() { return sourcePane.setActiveTurn; },
    get settleTurn() { return sourcePane.settleTurn; },
    get clearActiveTurn() { return sourcePane.clearActiveTurn; },
    get clearTurnState() { return sourcePane.clearTurnState; },
    get liveTodo() { return sourcePane.liveTodo; },
    get liveTodoShowAll() { return sourcePane.liveTodoShowAll; },
    get setLiveTodo() { return sourcePane.setLiveTodo; },
    get clearLiveTodo() { return sourcePane.clearLiveTodo; },
    get toggleLiveTodoShowAll() { return sourcePane.toggleLiveTodoShowAll; },
    get activityRailTodosOpen() { return sourcePane.activityRailTodosOpen; },
    get activityRailBackgroundOpen() { return sourcePane.activityRailBackgroundOpen; },
    get activityRailInputCollapsed() { return sourcePane.activityRailInputCollapsed; },
    get toggleActivityRailTodos() { return sourcePane.toggleActivityRailTodos; },
    get toggleActivityRailBackground() { return sourcePane.toggleActivityRailBackground; },
    get toggleActivityRailInputCollapsed() { return sourcePane.toggleActivityRailInputCollapsed; },
    get replaceThread() { return sourcePane.replaceThread; },
    get setEffectiveModel() { return sourcePane.setEffectiveModel; },
    get applyEffectiveModel() { return sourcePane.applyEffectiveModel; },
    get setShowTerminal() { return sourcePane.setShowTerminal; },
    get requestTerminalFocus() { return sourcePane.requestTerminalFocus; },
    get consumeTerminalFocusRequest() { return sourcePane.consumeTerminalFocusRequest; },
    get togglePlanSidebar() { return sourcePane.togglePlanSidebar; },
    get setShowPlanSidebar() { return sourcePane.setShowPlanSidebar; },
    get toggleReviewPane() { return sourcePane.toggleReviewPane; },
    get setShowReviewPane() { return sourcePane.setShowReviewPane; },
    get closeAgentPane() { return sourcePane.closeAgentPane; },
    get applyChannelMessage() { return sourcePane.applyChannelMessage; },
    get applyChannelMessages() { return sourcePane.applyChannelMessages; },
    get applyChannelState() { return sourcePane.applyChannelState; },
    get clearChannel() { return sourcePane.clearChannel; },
  } satisfies Pick<ThreadPane, AgentScopeForwarded>;

  // Own properties, getters intact. NOT a spread of the two literals — a
  // spread would evaluate every getter once and hand the timeline a frozen
  // snapshot of the pane.
  const pane: ThreadPane = Object.defineProperties({} as ThreadPane, {
    ...Object.getOwnPropertyDescriptors(forwarded),
    ...Object.getOwnPropertyDescriptors(overrides),
  });

  return {
    pane,
    get items() {
      return scopedItems;
    },
    get lifecycle() {
      return lifecycle;
    },
    get lifecycleCompletion() {
      return lifecycleCompletion;
    },
    dispose() {
      activityRuns.clear();
    },
  };
}
