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
// Consumers state the intersection they need
// (`PaneSession & RowUiRegistry & ScrollHost` for a typical timeline
// row). `ThreadPane` satisfies every role, so narrowing a CHILD never
// forces its parent to narrow — the retypings can proceed bottom-up,
// one component at a time.
//
// Two places deliberately stay on `ThreadPane` and are not oversights:
// `composer/ComposerInputSurface` (and the row that hosts it,
// `chat/UserMessageEditor` → `chat/UserMessage` → `chat/TimelineLeaf` →
// `chat/WaitGroup`), because the surface runs `/` commands and reaches
// `makeCommandContext` plus the whole command-action surface; and
// `utils/uiRenderTrace.ts#summarizePaneForTrace`, which exists to
// summarise the pane and whose honest narrowest type IS the pane. A role
// for either would just re-describe `ThreadPane` under a new name.
// `stores/agentScopeView.svelte.ts` inherits that: it BUILDS a pane the
// timeline mounts, and the timeline hands it down to that same composer
// surface, so its facade is typed as the whole pane. It gets the
// property the roles give a consumer — an exhaustive, compile-checked
// member list — from a `satisfies Pick<ThreadPane, …>` over its
// forwarded getters instead.
//
// `ThreadPaneRoleConformance` at the bottom is the drift tripwire: it
// fails to compile the moment `ThreadPane` stops satisfying a role, and
// the error names the role.

import type { Item, Thread } from '../types/models';
import type {
  StreamingAssistantRenderContext,
  StreamingAssistantRevealSink,
} from './streamingAssistantReveal';
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
  PaneErrorKind,
  PaneScrollController,
  ScrollToItemRequest,
} from './threadPaneShared';
import type {
  PayloadExpansionLease,
  RowExpansionStateOptions,
  RowUiStateRetention,
} from './threadRowUiState.svelte';
import type { SettledTurn, TimelineTurnFacet } from './threadTurnProjection';
import type { ThreadPane } from './thread.svelte';
import type { ProvenAppend } from '../markdown';

/**
 * Pane identity, the thread it is holding, and the switch-load
 * lifecycle around that thread. These key every per-pane registry and
 * every "is this still the load I started against?" guard in the
 * timeline's session modules; `switchGeneration` moves on a same-thread
 * reload where `threadId` does not, which is exactly why both are here.
 * `ensureMaterializedThread` belongs to the same question — it is how a
 * draft placeholder becomes the thread the pane holds — and
 * `debugMemoryStats` rides along as the pane's diagnostics probe, an
 * opaque trace payload and the only reason the trace passes need a pane
 * beyond its identity.
 *
 * Consumers: `chat/timelineWindowAnchor.svelte.ts`,
 * `chat/timelineJump.svelte.ts`, `chat/timelineRestore.svelte.ts`,
 * `chat/timelineDiagnostics.ts`, `chat/timelineSizePriors.svelte.ts`,
 * `chat/timelinePaging.ts`, `chat/useLeasedPayloadExpansion.svelte.ts`,
 * and every timeline row (workspace path, thread id, agent scope).
 */
export interface PaneSession {
  readonly paneId: string;
  readonly threadId: string | null;
  /** The mounted thread row — read for its provider/mode/workspace, never mutated here. */
  readonly thread: Thread | null;
  /** Bumped by every switch, including a same-thread in-place reload. */
  readonly switchGeneration: number;
  /** A switch/window load is in flight. */
  readonly loading: boolean;
  /** Empty on the thread timeline; an agent-scope facade names its root launch. */
  readonly agentScopeRootId: string;
  /** Model the thread is SET to (identity-stable across thread replacement). */
  readonly activeModel: string;
  /** Model the pane's next turn will actually run on. */
  readonly effectiveModel: string;
  /** Turn a draft placeholder into a real thread; resolves its id, or null. */
  readonly ensureMaterializedThread: () => Promise<string | null>;
  /** Dev-only memory probe; opaque to every consumer, which just records it. */
  readonly debugMemoryStats: () => unknown;
}

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
  /**
   * Turn identity for the rows above, taken from the PANE rather than the
   * thread's turn store (the agent pane's facade keys its scoped window
   * as one run). Added for `chat/timelineRowProjection.svelte.ts`, which
   * reads it for the turn-boundary decorations and the response pill.
   */
  readonly timelineTurns: TimelineTurnFacet;
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
 * `chat/ActivityRun.svelte`, `chat/preserveScrollAnchor.ts` — and
 * therefore every row that holds a viewport anchor across a height
 * change, which is most of them —
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
 * `chat/timelineRowUiPrune.ts`, `chat/timelineSizePriors.svelte.ts`,
 * `chat/timelineActivityRunAutoCollapse.ts`, `utils/virtual/priors.ts`,
 * and every timeline ROW that owns expansion state or hands the pane to
 * one that does (`UserMessageBody`, `DiffFileBlock`, `SubagentGroup`,
 * `ExpandablePayloadBody`, the tool-row family, …).
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
 * `chat/ThinkingBlock.svelte`, `chat/CompactionReasoning.svelte`,
 * `chat/ToolCallCard.svelte`, `chat/ActivityRun.svelte`, `App.svelte`.
 */
export interface RevealRead {
  readonly revealBoundary: RevealBoundary | null;
  readonly liveThinkingTailForItem: (itemId: string) => string | null;
  readonly isItemSmoothing: (itemId: string) => boolean;
  /** Changes after pane-wide reveal disposal so mounted rows re-register. */
  readonly assistantRevealRegistrationGeneration: number;
  readonly registerAssistantRevealSink: (
    itemId: string,
    sink: StreamingAssistantRevealSink,
  ) => () => void;
  readonly assistantMarkdownParserSource: (
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ) => string;
  /** Opaque lineage proof for the append that produced this exact parser source. */
  readonly assistantMarkdownSourceAppend: (
    itemId: string,
    parserSource: string,
  ) => ProvenAppend | undefined;
  readonly snapSmoothersToReceived: () => void;
  readonly lastLiveContentAt: number;
}

/**
 * The pane's one user-facing error surface plus the provider banner that
 * shares the top of the pane. `setPaneError` / `clearPaneError` are the
 * chokepoint — the message is stored per KIND, every stored kind renders
 * as its own banner row (`paneErrorList`, fixed display order), and the
 * kind decides the affordance each row offers. The named writers below
 * are thin wrappers kept for their call sites. `thread` / `threadId` /
 * `refreshFromBackend` are here because the banner's affordances are
 * per-thread actions: a retry row refreshes THIS thread, and the
 * provider banner's action needs the thread's provider.
 *
 * Consumers: `chat/ProviderStatusBanner.svelte` (the whole role),
 * `stores/interruptErrors.ts` (`Pick<…, 'setGeneralError'>`), and
 * `stores/eventsProvider.ts` (a `Pick` of the writers, intersected with
 * `ThreadPaneIngest`). `composer/Composer.svelte` also reaches
 * `setGeneralError`, but through the whole pane — it is one of the
 * documented whole-pane holdouts above.
 */
export interface ErrorSurface {
  readonly threadId: string | null;
  /** Read for its provider/mode when a banner row's affordance needs it. */
  readonly thread: Thread | null;
  /** Every stored error in banner-stack order; one row each. */
  readonly paneErrorList: readonly { kind: PaneErrorKind; message: string }[];
  /** Newest stored error's message; presence-check convenience. */
  readonly generalError: string | null;
  /** Its kind, with `'general'` reported as `null` — an untagged error has no action. */
  readonly generalErrorKind: 'session' | 'history-load' | null;
  readonly providerBanner: ProviderStatusEvent | null | undefined;
  readonly setPaneError: (message: string, kind?: PaneErrorKind) => void;
  readonly clearPaneError: (kind?: PaneErrorKind) => void;
  readonly setGeneralError: (message: string | null) => void;
  readonly setSessionError: (message: string) => void;
  readonly setHistoryLoadError: (message: string | null) => void;
  readonly clearGeneralError: () => void;
  readonly clearSessionError: () => void;
  readonly setProviderBanner: (status: ProviderStatusEvent | null | undefined) => void;
  readonly retryHistoryLoad: () => Promise<void>;
  readonly refreshFromBackend: () => Promise<void>;
}

/**
 * The doors a ROW can open onto another surface. Opening a companion or
 * a scoped pane is normally the header's or a store helper's job; two
 * affordances live inside the timeline itself — the proposed-plan card's
 * "View plan" and a subagent launch's "open in pane" — and the pane is
 * the one place that decides where either routes. Deliberately the
 * narrowest possible door rather than the pane's whole companion
 * surface.
 *
 * Consumers: `chat/ProposedPlanCard.svelte`, `chat/SubagentGroup.svelte`,
 * `chat/AgentRow.svelte`, `chat/CollabToolRow.svelte`.
 */
export interface PaneDoors {
  readonly showPlanSidebar: boolean;
  readonly setShowPlanSidebar: (open: boolean) => void;
  readonly openAgentPane: (launchItemId: string, label: string) => void;
}

/**
 * The write surface the backend-event fan-out lands on: item stream,
 * approvals, turn lifecycle, context window, channel and design pushes.
 * Reads are limited to what those handlers need to address or dedupe
 * against (`paneId`/`threadId`/`thread`, the pending queues, `items`,
 * `channelMessages` for the discussion refresh diff).
 *
 * Consumers: `stores/eventsItemStream.ts`, `stores/eventsProvider.ts`
 * (intersected with a `Pick` of `ErrorSurface`'s writers),
 * `stores/eventsQueue.ts`, `stores/eventsDiscussion.ts`,
 * `stores/eventsDesign.ts`, `stores/eventsMessageRevert.ts`,
 * `stores/eventsThreadRows.ts`, `stores/eventsTransportGap.ts`,
 * `stores/revertOnInterrupt.svelte.ts`. Each narrows at its pane
 * acquisition point (the registry hands out whole `ThreadPane`s), so a
 * new member use in any of them fails to compile until it is added
 * here. `stores/eventsTerminal.ts` stays off this role on purpose: its
 * two members (`paneId`, `requestTerminalFocus`) are a focus request,
 * not ingest, and it states its own `Pick`.
 */
export interface ThreadPaneIngest {
  readonly paneId: string;
  readonly threadId: string | null;
  readonly thread: Thread | null;
  readonly items: Item[];
  readonly channelMessages: ChannelMessage[];
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
  paneSession: Conforms<PaneSession, ThreadPane>;
  timelineSource: Conforms<TimelineSource, ThreadPane>;
  timelineWindow: Conforms<TimelineWindow, ThreadPane>;
  scrollHost: Conforms<ScrollHost, ThreadPane>;
  rowUiRegistry: Conforms<RowUiRegistry, ThreadPane>;
  revealRead: Conforms<RevealRead, ThreadPane>;
  paneDoors: Conforms<PaneDoors, ThreadPane>;
  errorSurface: Conforms<ErrorSurface, ThreadPane>;
  ingest: Conforms<ThreadPaneIngest, ThreadPane>;
};
