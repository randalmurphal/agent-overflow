// Narrow role interfaces over the wide `ThreadPane` object.
//
// `ThreadPane` (thread.svelte.ts) is the pane's whole API — 172 members
// covering the timeline, the scroll host, per-row UI state, streaming
// reveal, the error banner, discussion, design, terminal placement,
// companions and the draft-thread lifecycle. No consumer uses more than
// a slice of it, but every consumer that types a prop or a parameter as
// `ThreadPane` takes a dependency on all of it: a signature says nothing
// about what the callee reads, and a change anywhere in the object looks
// like a change to everyone.
//
// The interfaces below name the slices that consumers actually take.
// Each one lists ONLY the members its consumer group uses today, derived
// from real call sites — not from what "belongs together" on the pane.
// Roles deliberately overlap where consumers do (a prune pass reads both
// the timeline and the row-UI registry); they are views, not a partition.
//
// This module is types only. It declares no values, so it erases
// completely at compile time and adds nothing to any bundle.
//
// `ThreadPaneRoleConformance` at the bottom is the drift tripwire: it
// fails to compile the moment `ThreadPane` stops satisfying a role, and
// the error names the role.

import type { Item, Thread } from '../types/models';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
  ProviderSessionAccountEvent,
  ProviderStatusEvent,
  TodoStep,
  UserInputRequest,
} from '../types/events';
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import type { AttachmentPreviewCache } from '../utils/attachmentPreview.svelte';
import type { PayloadExpansionHandle } from '../utils/payloadExpansion.svelte';
import type { SubagentFoldAggregate } from '../utils/subagentFold';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { ApplyItemUpsertsToWindowResult, TimelineCursorLike } from './threadItems';
import type {
  LoadOlderResult,
  PaneScrollController,
  ScrollToItemRequest,
} from './threadPaneShared';
import type {
  PayloadExpansionLease,
  RowExpansionStateOptions,
  RowUiStateRetention,
} from './threadRowUiState.svelte';
import type { SettledTurn } from './threadTurnProjection';
import type { ThreadPane } from './thread.svelte';

/**
 * What the timeline projection / retention / trace passes read: row
 * membership plus the two revisions they invalidate on.
 *
 * Consumers: `chat/timelineRowProjection.svelte.ts`,
 * `chat/timelineRowUiRetention.ts`, `chat/timelineRowUiPrune.ts`,
 * `chat/messageTimelineTrace.ts`, `chat/MessageNavRail.svelte` +
 * `chat/messageNavRail.ts`, `stores/agentScopeView.svelte.ts`,
 * `utils/rowUiRetention.ts`, `utils/subagentFold.ts`,
 * `utils/timelineStructureSignature.ts`.
 */
export interface TimelineSource {
  readonly threadId: string | null;
  /** `$state.raw` array — membership and order only. */
  readonly items: Item[];
  /** Per-row signal box; a field patch wakes this and not `items`. */
  readonly getItemById: (itemId: string) => Item | undefined;
  readonly timelineRevision: number;
  readonly rowUiRetentionRevision: number;
  readonly activityRuns: ThreadActivityRuns;
}

/**
 * The loaded window's bounds and the calls that move them. Everything
 * here is about which slice of history is in memory — nothing about how
 * it is rendered or scrolled.
 *
 * Consumers: `chat/timelinePaging.ts`, `chat/timelineDiagnostics.ts`,
 * `chat/timelineScroll.ts`, `chat/MessageTimeline.svelte`,
 * `chat/timelineRestore.svelte.ts`, `chat/MessageNavRail.svelte`,
 * `chat/SubagentGroup.svelte`, `utils/subagentGrouping.ts`.
 */
export interface TimelineWindow {
  readonly hasMoreHistory: boolean;
  readonly hasMoreNewer: boolean;
  readonly loadingOlder: boolean;
  readonly loadingNewer: boolean;
  readonly oldestLoadedCursor: TimelineCursorLike | null;
  readonly newestLoadedCursor: TimelineCursorLike | null;
  readonly oldestLoadedTurnIndex: number | null;
  readonly newestLoadedTurnIndex: number | null;
  readonly loadOlder: () => Promise<LoadOlderResult>;
  readonly loadNewer: () => Promise<LoadOlderResult>;
  readonly loadRecentTail: () => Promise<boolean>;
  readonly loadUntilItem: (itemID: string) => Promise<boolean>;
  readonly ensureSubagentChildren: (rootItemID: string) => Promise<boolean>;
  /** Set when a wire settle deferred its window prune to visual quiet. */
  readonly hasDeferredRecentWindowPrune: boolean;
  readonly retryDeferredRecentWindowPrune: () => void;
}

/**
 * Scroll-controller registration and scroll intent. The pane is the
 * rendezvous point between whoever owns the scroller (MessageTimeline,
 * ChannelView, ActivityRun) and everyone who needs to reach it.
 *
 * Consumers: `chat/MessageTimeline.svelte`, `chat/ChatView.svelte`,
 * `chat/ActivityRun.svelte`, `chat/preserveScrollAnchor.ts`,
 * `chat/editResendFlow.svelte.ts`, `chat/timelinePaging.ts`,
 * `chat/timelineRestore.svelte.ts`, `chat/timelineSizePriors.svelte.ts`,
 * `discussion/ChannelView.svelte`, `panes/PaneHost.svelte`,
 * `sidebar/SidebarResizer.svelte`,
 * `terminal/ThreadTerminalPlacement.svelte`,
 * `composer/ActivityRailBackgroundBody.svelte`,
 * `palette/MessageSearch.svelte`, `stores/threadItemStreamApply.ts`.
 */
export interface ScrollHost {
  /** Snapshot key for scroll position + size priors; null while draft. */
  readonly scrollStateKey: string | null;
  readonly scrollController: PaneScrollController | null;
  readonly attachScrollController: (controller: PaneScrollController) => void;
  readonly detachScrollController: (controller: PaneScrollController) => void;
  readonly scrollToItemRequest: ScrollToItemRequest;
  readonly requestScrollToItem: (itemID: string) => void;
  /** One-shot structural-append spring window; call before the mutation. */
  readonly armStructuralSpring: () => boolean;
}

/**
 * Per-row view state that outlives a row's mount: payload expansion
 * handles and leases, subagent/wait group folds, user-message clamps,
 * diff-card overrides, attachment previews — plus the prune pass that
 * bounds them.
 *
 * Consumers: `chat/useLeasedPayloadExpansion.svelte.ts`,
 * `chat/UserMessage.svelte`, `chat/UserMessageBody.svelte`,
 * `chat/UserMessageEditor.svelte`, `chat/DiffFileBlock.svelte`,
 * `chat/SubagentGroup.svelte`, `chat/WaitGroup.svelte`,
 * `chat/timelineRowUiPrune.ts`, `chat/timelineActivityRunAutoCollapse.ts`,
 * `composer/composerInputSurface.ts`, `utils/virtual/priors.ts`.
 */
export interface RowUiRegistry {
  readonly expansionStateFor: (
    item: Item,
    options?: RowExpansionStateOptions,
  ) => PayloadExpansionHandle;
  readonly retainExpansionStateFor: (
    item: Item,
    options?: RowExpansionStateOptions,
  ) => PayloadExpansionLease;
  readonly expansionStateForPayload: (
    payloadId: string,
    threadId: string,
    options?: unknown,
  ) => PayloadExpansionHandle;
  readonly retainExpansionStateForPayload: (
    payloadId: string,
    threadId: string,
    options?: unknown,
  ) => PayloadExpansionLease;
  readonly isSubagentGroupExpanded: (groupKey: string) => boolean;
  readonly toggleSubagentGroupExpanded: (groupKey: string) => boolean;
  readonly subagentLiveAggregate: (anchorId: string) => SubagentFoldAggregate | undefined;
  readonly isUserMessageExpanded: (itemId: string) => boolean;
  readonly setUserMessageExpanded: (itemId: string, expanded: boolean) => void;
  readonly diffCardExpandedOverride: (itemId: string, filePath: string) => boolean | undefined;
  readonly setDiffCardExpanded: (itemId: string, filePath: string, expanded: boolean) => void;
  readonly attachmentCacheFor: (itemId: string) => AttachmentPreviewCache;
  /** Validity stamp for replaying a measured-size priors snapshot. */
  readonly expansionSignature: () => string;
  readonly hasUserExpansionWithin: (itemIds: Iterable<string>) => boolean;
  readonly pruneRowUiState: (retention: RowUiStateRetention) => void;
}

/**
 * Read side of the streaming reveal: what a row renders while text is
 * still draining, and the gate that withholds rows after the one
 * currently revealing.
 *
 * Consumers: `chat/timelineRowProjection.svelte.ts`,
 * `chat/timelineRowUiPrune.ts`, `chat/MessageTimeline.svelte`,
 * `chat/AssistantMessage.svelte`, `chat/ReasoningTailRow.svelte`,
 * `chat/ActivityRun.svelte`, `App.svelte`.
 */
export interface RevealRead {
  readonly revealBoundary: RevealBoundary | null;
  readonly liveThinkingTailForItem: (itemId: string) => string | null;
  readonly isItemSmoothing: (itemId: string) => boolean;
  readonly snapSmoothersToReceived: () => void;
  readonly lastLiveContentAt: number;
}

/**
 * The pane's one user-facing error slot plus the provider banner that
 * shares it. Writers are tag-aware: a session-kind message is
 * auto-dismissible, an orthogonal one is not.
 *
 * Consumers: `chat/ProviderStatusBanner.svelte`,
 * `stores/eventsProvider.ts`, `stores/interruptErrors.ts`,
 * `stores/threadTitleGeneration.svelte.ts`, `stores/panes.svelte.ts`,
 * `composer/Composer.svelte`, `git/GitActionsControl.svelte`,
 * `panes/PaneTitleHandle.svelte`, `sidebar/ThreadRow.svelte`,
 * `sidebar/ThreadContextMenu.svelte`. `setHistoryLoadError` is the
 * writer behind the banner's `history-load` retry affordance.
 */
export interface ErrorSurface {
  readonly generalError: string | null;
  readonly generalErrorKind: 'session' | 'history-load' | null;
  readonly providerBanner: ProviderStatusEvent | null | undefined;
  readonly setGeneralError: (message: string | null) => void;
  readonly setSessionError: (message: string) => void;
  readonly setHistoryLoadError: (message: string | null) => void;
  readonly clearGeneralError: () => void;
  readonly clearSessionError: () => void;
  readonly setProviderBanner: (status: ProviderStatusEvent | null | undefined) => void;
  readonly retryHistoryLoad: () => Promise<void>;
}

/**
 * The write surface the backend-event fan-out lands on: item stream,
 * approvals, turn lifecycle, context window, channel and design pushes.
 * Reads are limited to what those handlers need to address or dedupe
 * against (`paneId`/`threadId`/`thread`, the pending queues, `items`).
 *
 * Consumers: `stores/eventsItemStream.ts`, `stores/eventsProvider.ts`,
 * `stores/eventsMessageRevert.ts`, `stores/eventsQueue.ts`,
 * `stores/eventsDiscussion.ts`, `stores/eventsDesign.ts`,
 * `stores/eventsTransportGap.ts`, `stores/eventsThreadRows.ts`,
 * `stores/revertOnInterrupt.svelte.ts`, `stores/threadContextWindow.ts`.
 */
export interface ThreadPaneIngest {
  readonly paneId: string;
  readonly threadId: string | null;
  readonly thread: Thread | null;
  readonly items: Item[];
  readonly getItemById: (itemId: string) => Item | undefined;
  readonly pendingApprovals: ApprovalRequest[];
  readonly pendingUserInputs: UserInputRequest[];
  readonly markLiveContentAdvanced: () => void;
  readonly applyProviderItemUpserts: (
    incoming: Item[],
  ) => ApplyItemUpsertsToWindowResult | null;
  readonly applyItemDelta: (evt: ItemDeltaEvent) => void;
  readonly applyItemMeta: (evt: ItemMetaEvent) => void;
  readonly applyItemPatch: (evt: ItemPatchEvent) => void;
  readonly upsertItems: (incoming: Item[]) => boolean;
  readonly removeItemById: (itemId: string, expectedThreadId: string) => Item | null;
  readonly removeItemsFromTurn: (fromTurnIndex: number) => Item[];
  readonly removeRevertedItems: (turnIndex: number, keptAnchorTurnItemIds: string[]) => Item[];
  readonly addApproval: (approval: ApprovalRequest) => void;
  readonly removeApproval: (requestId: string) => void;
  readonly addUserInput: (request: UserInputRequest) => void;
  readonly removeUserInput: (requestId: string) => void;
  readonly setContextWindow: (data: ContextWindow) => void;
  readonly clearContextWindow: () => void;
  readonly setProviderSessionAccount: (account: ProviderSessionAccountEvent | null) => void;
  readonly setLiveTodo: (steps: TodoStep[]) => void;
  readonly settleTurn: (settled: SettledTurn) => void;
  readonly applyEffectiveModel: (model: string, revision: number) => void;
  readonly replaceThread: (nextThread: Thread) => void;
  readonly refreshFromBackend: () => Promise<void>;
  readonly applyChannelMessage: (message: ChannelMessage) => void;
  readonly applyChannelMessages: (messages: ChannelMessage[]) => void;
  readonly applyChannelState: (payload: ChannelStatePayload) => void;
  readonly applyDesignOptionsUpdate: (threadId: string, setId: string) => Promise<void>;
}

/**
 * `Impl` must satisfy `Role`, and evaluates to `Impl`. Used only to
 * force the constraint check below; instantiating it is the assertion.
 */
type Conforms<Role, Impl extends Role> = Impl;

/**
 * Compile-time proof that `ThreadPane` still satisfies every role above.
 * Purely a type — nothing constructs this. If a role drifts away from
 * the pane (a member renamed, a signature changed, a getter dropped),
 * the failing field names the role and the error names the member.
 */
export type ThreadPaneRoleConformance = {
  timelineSource: Conforms<TimelineSource, ThreadPane>;
  timelineWindow: Conforms<TimelineWindow, ThreadPane>;
  scrollHost: Conforms<ScrollHost, ThreadPane>;
  rowUiRegistry: Conforms<RowUiRegistry, ThreadPane>;
  revealRead: Conforms<RevealRead, ThreadPane>;
  errorSurface: Conforms<ErrorSurface, ThreadPane>;
  ingest: Conforms<ThreadPaneIngest, ThreadPane>;
};
