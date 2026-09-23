// A typed agent facade over an independently owned timeline window.
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { errString } from '../utils/errors';
import type { ThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import { createScopedTimeline } from './scopedTimeline.svelte';
import { liveCodexAgent } from './subagentProgress.svelte';
import { subagentExecutionItem } from '../utils/codexSubagentRuntime';
import type { TimelineTurnFacet } from './threadTurnProjection';

/** The turn facet groups the scope under its own execution lifecycle. */
const AGENT_SCOPE_TURN_KEY = 0;

export interface AgentScopeView {
  /** The ThreadPane facade MessageTimeline mounts. */
  readonly pane: ThreadPane;
  /** The scope's loaded direct rows (what `pane.items` answers). */
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
  readonly root: Item | undefined;
  readonly gone: boolean;
  readonly error: string | null;
  start(): void;
  /** Release the view's own registries. Call on unmount. */
  dispose(): void;
}

/**
 * What differs between the surfaces that mount a scoped view: the agent
 * companion (`viewKey` `agent`) and a background tray row's digest
 * (`tray:<scope>`, one per expanded row).
 */
export interface AgentScopeViewOptions {
  /**
   * Distinguishes this view's pane identity from the source pane's and
   * from every other scoped view of the same source. chatDomIds scopes
   * disclosure ids by it and the row-UI expansion leases key on it, so two
   * surfaces showing the same row keep separate expansion state.
   */
  viewKey: string;
  toolsOnly?: boolean;
  digestItemId?: string;
  /**
   * Where opening a nested launch from inside this view routes. The
   * companion grows its breadcrumb (`pushScope`); the tray digest opens
   * the companion on that launch through the source pane's door.
   */
  openAgentPane(launchItemId: string, label: string): void;
}

export function createAgentScopeView(
  sourcePane: ThreadPane,
  scopeItemId: string,
  options: AgentScopeViewOptions,
): AgentScopeView {
  if (!sourcePane.thread) throw new Error('An agent pane requires a thread');
  const scrollKey = `${sourcePane.paneId}:${sourcePane.threadId}~${options.viewKey}:${scopeItemId}`;
  const owner = createScopedTimeline(sourcePane.thread, { scopeRootId: scopeItemId, tools: options.toolsOnly, digestItemId: options.digestItemId }, scrollKey);
  const { itemWindow, window, rows, reveal, runs, scroll } = owner;
  // Only presentation lifts direct children to roots; stored rows retain
  // their identity. One lifted copy per stored row, so a structural revision
  // allocates the array, not a copy of every row, and unchanged rows keep
  // their presented identity.
  const lifted = new WeakMap<Item, Item>();
  const lift = (item: Item): Item => {
    let row = lifted.get(item);
    if (!row) {
      row = { ...item, parentId: undefined };
      lifted.set(item, row);
    }
    return row;
  };
  let scopedItems = $derived.by(() => {
    void itemWindow.timelineRevision;
    return itemWindow.getItems().map(lift);
  });
  let root = $derived((owner.scope?.root as Item | undefined) ?? sourcePane.getItemById(scopeItemId));
  let lifecycle = $derived((!options.digestItemId && root ? liveCodexAgent(root.threadId, root.id) : undefined)
    ?? owner.scope?.lifecycle as Item | undefined ?? root);
  let lifecycleCompletion = $derived(owner.scope?.completion as Item | undefined);
  const timelineTurns: TimelineTurnFacet = {
    keyOf: () => AGENT_SCOPE_TURN_KEY,
    get activeKey() {
      const status = subagentExecutionItem(lifecycle, lifecycleCompletion)?.status;
      return status === 'running' || status === 'streaming' ? AGENT_SCOPE_TURN_KEY : null;
    },
    get settled() {
      const launch = lifecycle;
      const statusItem = subagentExecutionItem(launch, lifecycleCompletion);
      if (!launch || !statusItem) return null;
      if (statusItem.status === 'running' || statusItem.status === 'streaming') return null;
      return {
        key: AGENT_SCOPE_TURN_KEY,
        startedAt: subagentExecutionItem(launch)!.createdAt,
        completedAt: statusItem.updatedAt,
      };
    },
  };

  const overrides = {
    get lastLiveContentAt() { return owner.lastLiveContentAt; },
    get paneId() { return `${sourcePane.paneId}~${options.viewKey}`; },
    get scrollStateKey() { return scrollKey; },
    get agentScopeRootId() { return owner.scope?.root.id ?? scopeItemId; },
    get items() { return scopedItems; },
    get revealBoundary() { return reveal.revealBoundary; },
    get timelineTurns() { return timelineTurns; },
    get activityRuns() { return runs; },
    get scrollController() { return scroll.controller; },
    attachScrollController: scroll.attach,
    detachScrollController: scroll.detach,
    get scrollToItemRequest() { return scroll.scrollToItemRequest; },
    requestScrollToItem: scroll.requestScrollToItem,
    loadOlder: window.loadOlder,
    loadNewer: window.loadNewer,
    loadUntilItem: window.loadUntilItem,
    loadRecentTail: window.loadRecentTail,
    get hasMoreHistory() { return window.hasMoreHistory; },
    get hasMoreNewer() { return window.hasMoreNewer; },
    get loadingOlder() { return window.loadingOlder; },
    get loadingNewer() { return window.loadingNewer; },
    get oldestLoadedCursor() { return window.oldestLoadedCursor; },
    get newestLoadedCursor() { return window.newestLoadedCursor; },
    get oldestLoadedTurnIndex() { return window.oldestLoadedTurnIndex; },
    get newestLoadedTurnIndex() { return window.newestLoadedTurnIndex; },
    get hasDeferredRecentWindowPrune() { return window.hasDeferredRecentWindowPrune; },
    retryDeferredRecentWindowPrune: window.retryDeferredRecentWindowPrune,
    pruneRowUiState: (retention: Parameters<typeof rows.pruneRowUiState>[0]) => {
      rows.pruneRowUiState(retention);
      reveal.pruneSettledThinkingTails(retention.itemIds);
    },
    get loading() { return owner.loading; },
    get historyWindowPending() { return false; },
    get showLoadingSpinner() { return owner.loading && scopedItems.length === 0; },
    get historyRevision() { return itemWindow.historyRevision; },
    get timelineRevision() { return itemWindow.timelineRevision; },
    get rowUiRetentionRevision() { return itemWindow.rowUiRetentionRevision; },
    get switchGeneration() { return owner.generation; },
    getItemById: (id: string) => itemWindow.getItemById(id) ?? (id === scopeItemId || id === root?.id ? root : undefined),
    refreshFromBackend: owner.refresh,
    retryHistoryLoad: () => owner.refresh().catch(error => reportFrontendDiagnostic('Scoped history retry failed', errString(error))),
    armStructuralSpring: scroll.armStructuralSpring,
    liveThinkingTailForItem: reveal.liveThinkingTailFor,
    isItemSmoothing: reveal.isSmoothing,
    get assistantRevealRegistrationGeneration() { return reveal.assistantRevealRegistrationGeneration; },
    registerAssistantRevealSink: reveal.registerAssistantRevealSink,
    assistantMarkdownParserSource: reveal.assistantParserSource,
    assistantMarkdownSourceAppend: reveal.assistantSourceAppend,
    __flushItemSmoothersForTest: reveal.__flushForTest,
    __itemSmootherCountForTest: reveal.__smootherCountForTest,
    get smoothingItemCount() { return reveal.smootherCount(); },
    snapSmoothersToReceived: reveal.snapAllToReceived,
    openAgentPane: options.openAgentPane,
    expansionStateFor: rows.expansionStateFor,
    retainExpansionStateFor: rows.retainExpansionStateFor,
    expansionStateForPayload: rows.expansionStateForPayload,
    retainExpansionStateForPayload: rows.retainExpansionStateForPayload,
    isSubagentGroupExpanded: rows.isSubagentGroupExpanded,
    toggleSubagentGroupExpanded: rows.toggleSubagentGroupExpanded,
    isUserMessageExpanded: rows.isUserMessageExpanded,
    setUserMessageExpanded: rows.setUserMessageExpanded,
    diffCardExpandedOverride: rows.diffCardExpandedOverride,
    setDiffCardExpanded: rows.setDiffCardExpanded,
    expansionSignature: rows.expansionSignature,
    hasUserExpansionWithin: rows.hasUserExpansionWithin,
    attachmentCacheFor: rows.attachmentCacheFor,
  } satisfies Partial<ThreadPane>;

  // ---- Forwarded surface ------------------------------------------------
  // Getters preserve source reactivity; satisfies checks that every pane
  // property is either forwarded or overridden.
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
    get markLiveContentAdvanced() { return sourcePane.markLiveContentAdvanced; },
    get setDraftPlaceholderMode() { return sourcePane.setDraftPlaceholderMode; },
    get applyDraftPlaceholderDefaults() { return sourcePane.applyDraftPlaceholderDefaults; },
    get applyDraftPlaceholderWorkspace() { return sourcePane.applyDraftPlaceholderWorkspace; },
    get dematerializeEmptyDraftThread() { return sourcePane.dematerializeEmptyDraftThread; },
    get isLocked() { return sourcePane.isLocked; },
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
    get switchThread() { return sourcePane.switchThread; },
    get refreshOwnership() { return sourcePane.refreshOwnership; },
    get snapshotForClose() { return sourcePane.snapshotForClose; },
    get clear() { return sourcePane.clear; },
    get startDraftPlaceholder() { return sourcePane.startDraftPlaceholder; },
    get materializeDraftPlaceholder() { return sourcePane.materializeDraftPlaceholder; },
    get adoptMaterializedDraftThread() { return sourcePane.adoptMaterializedDraftThread; },
    get ensureMaterializedThread() { return sourcePane.ensureMaterializedThread; },
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
    get __syncLedgerArmedForTest() { return sourcePane.__syncLedgerArmedForTest; },
    get applyItemDelta() { return sourcePane.applyItemDelta; },
    get applyItemMeta() { return sourcePane.applyItemMeta; },
    get applyItemPatch() { return sourcePane.applyItemPatch; },
    get subagentLiveAggregate() { return sourcePane.subagentLiveAggregate; },
    get setPaneError() { return sourcePane.setPaneError; },
    get clearPaneError() { return sourcePane.clearPaneError; },
    get setGeneralError() { return sourcePane.setGeneralError; },
    get setSessionError() { return sourcePane.setSessionError; },
    get setHistoryLoadError() { return sourcePane.setHistoryLoadError; },
    get clearGeneralError() { return sourcePane.clearGeneralError; },
    get clearSessionError() { return sourcePane.clearSessionError; },
    get setSendInFlight() { return sourcePane.setSendInFlight; },
    get confirmOptimisticSend() { return sourcePane.confirmOptimisticSend; },
    // Forwarded, not overridden: a flushed user row is a TOP-LEVEL row of
    // the main transcript, so the source pane's window and reveal boundary
    // are the ones that decide whether it is rendered. This scope's
    // boundary describes scoped child rows only.
    get syncRenderedFlushRows() { return sourcePane.syncRenderedFlushRows; },
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
  } satisfies Pick<ThreadPane, Exclude<keyof ThreadPane, keyof typeof overrides>>;

  // Own properties, getters intact. NOT a spread of the two literals — a
  // spread would evaluate every getter once and hand the timeline a frozen
  // snapshot of the pane.
  const pane: ThreadPane = Object.defineProperties({} as ThreadPane, {
    ...Object.getOwnPropertyDescriptors(forwarded),
    ...Object.getOwnPropertyDescriptors(overrides),
  });

  return {
    pane,
    get items() { return scopedItems; },
    get lifecycle() {
      return lifecycle;
    },
    get lifecycleCompletion() {
      return lifecycleCompletion;
    },
    get root() { return root; },
    get gone() { return owner.gone; },
    get error() { return owner.error; },
    start: owner.start,
    dispose: owner.dispose,
  };
}
