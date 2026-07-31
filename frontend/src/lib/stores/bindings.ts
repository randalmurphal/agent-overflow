// Re-export Wails v3 generated bindings used by components.
//
// Every entry here is produced by `wails3 generate bindings -ts`. When
// a new App method is added on the Go side, run
// `wails3 task common:generate:bindings` and re-export it from this file
// -- do not hand-wrap bindings.
export {
  // Thread management
  ArchiveThread,
  UnarchiveThread,
  DeleteThread,
  ForkThread,
  ForkThreadFromMessage,
  RevertConversationToMessage,
  GetThread,
  ListArchivedThreads,
  ListThreads,
  MarkThreadRead,
  MarkThreadUnread,
  PinThread,
  UnpinThread,
  RenameThread,
  SwitchThread,
  UpdateThreadModel,
  UpdateThreadProvider,
  UpdateThreadModelSelection,
  UpdateThreadMode,
  UpdateThreadReasoningEffort,
  UpdateThreadFastMode,
  UpdateThreadContextWindow,
  UpdateThreadRuntimeMode,
  UpdateThreadBranch,
  UpdateThreadWorkspace,

  // Session management
  StartSession,
  AutoResumeThread,
  StopSession,
  ReconnectSession,
  SendMessage,
  InterruptTurn,
  InterruptAndRevertIfClean,
  GetThreadLiveState,
  ListPendingInteractiveRequests,
  RespondToApproval,
  RespondToUserInput,
  SendPlanRevisionComments,

  // Background tasks (per-item + thread-wide stop primitives)
  StopClaudeTask,
  CleanCodexBackgroundTerminals,

  // Data access
  GetPayloadPreview,
  GetPayloadChunk,
  GetPayloadData,
  ListItems,

  // Settings
  GetSettings,
  UpdateSettings,
  GetContextSettings,

  // Usage accounting (append-only ledger aggregates; costs are
  // wire-true for Claude, table-priced at read time for Codex /
  // claude-tui — see internal/usagecost).
  GetUsageStats,
  GetRateLimitsSnapshots,
  ListProviderAccounts,
  LoginProviderAccount,
  SwitchProviderAccount,
  RemoveProviderAccount,
  RefreshProviderAccountUsage,

  // Per-client UI view state (ui_state table) behind stores/appStorage.ts.
  GetUIState,
  SetUIState,
  DeleteUIState,

  // Network bindings (LAN-bind toggle for the embedded transport).
  GetNetworkSettings,
  SetNetworkSettings,

  // WSL distro switcher — exposed only when the backend is running
  // inside a WSL distribution spawned by the Windows launcher. The
  // setter mutates %APPDATA%\agent-overflow\wsl.json so the next
  // launcher boot picks the new distro.
  IsWSL,
  ListWSLDistros,
  GetWSLDistroPreference,
  SetWSLDistroPreference,

  // Host app launchers: editor, browser URL opener, catalog, persistence.
  OpenInEditor,
  OpenExternalURL,
  ListAvailableEditors,
  GetEditorSettings,
  SetEditorSettings,

  // Remote endpoint storage. Token-redacted Summary on every read
  // path; explicit GetRemoteEndpointToken for the copy-launch-command
  // flow. See app_remote.go for the threat model.
  ListRemoteEndpoints,
  AddRemoteEndpoint,
  UpdateRemoteEndpoint,
  DeleteRemoteEndpoint,
  TouchRemoteEndpoint,
  GetRemoteEndpointToken,

  // Provider detection
  GetProviderStatuses,
  GetModelsForProvider,
  ProbeClaudeAccount,
  RecheckClaudeAccount,
  RecheckCodexAccount,

  // Git operations
  GenerateCommitMessage,
  GetGitStatus,
  GetGitStatusFast,
  GetGitStatusFastForProject,
  GitStatusSubscribe,
  GitStatusUnsubscribe,
  GitListBranches,
  GitListBranchesForProject,
  GitListWorktrees,
  GitListWorktreesForProject,
  GitCommit,
  GitPush,
  GitPull,
  GitCheckout,
  GitCheckoutForProject,
  GitCreateBranch,
  GitCreateBranchFrom,
  GitCreatePR,
  GitCreateWorktree,
  GitMaybeFetchRemotes,
  GitMaybeFetchRemotesForProject,
  GitListBranchPruneCandidates,
  GitPruneBranches,
  GitSyncBranch,
  GitSyncBranchForProject,
  GitRemoveWorktree,
  GitWorktreeStatus,
  GitWorktreeStatusForProject,
  PrepareThreadWorktree,
  AttachThreadWorktree,
  RemoveOtherWorktree,
  RemoveOtherWorktreeForProject,

  // Terminal operations
  CloseTerminal,
  CloseThreadTerminals,
  GetTerminalReplay,
  ListTerminals,
  OpenTerminal,
  MoveThreadTerminals,
  RefreshTerminal,
  ResizeTerminal,
  RestartTerminal,
  WriteTerminal,

  // Provider terminal (claude-tui take-control)
  ProviderTerminalAttach,
  ProviderTerminalDetach,
  ProviderTerminalReplay,
  ProviderTerminalInput,
  ProviderTerminalResize,
  ProviderTerminalRefresh,
  ProviderTerminalSetControl,

  // Discussion operations
  ListDiscussions,
  ListDiscussionsForThread,
  GetDiscussion,
  CreateDiscussion,
  UpdateDiscussion,
  DeleteDiscussion,
  StartDiscussion,
  StartDiscussionByID,
  GetChannelMessages,
  GetChannelState,
  PostChannelMessage,
  ConcludeDiscussion,

  // Design operations
  ListDesignOptions,
  LatestDesignOptionSet,
  DismissDesignOptionSet,
  EnsureDesignWorkdir,
  GetDesignWorkdirInfo,
  IngestDiagnosticBatch,

  // Composer enhancements
  UploadAttachment,
  ListAttachments,
  DeleteAttachment,
  GetAttachmentData,
  GetAttachmentThumbnail,
  SaveDraft,
  GetDraft,
  ClearDraft,
  DeleteEmptyDraftThread,
  SearchWorkspaceFiles,
  WriteThreadWorkspaceFile,
  ListChatBarFavorites,
  SetChatBarFavorite,

  // Message search
  SearchThreadMessages,
  SearchThreadItems,

  // Keybindings
  GetKeybindings,
  UpdateKeybindings,
  ResetKeybindings,

  // Review pane diffs (workspace / branch / per-commit / edits)
  GetBranchBaseDiff,
  GetWorkspaceCurrentDiff,
  ListBranchCommits,
  GetCommitDiff,
  GetDiffContextLines,
  VerifyEditDiffs,
  ListThreadEditDiffs,
  GetTurnEditsDiff,

  // Syntax-highlight span metadata (backend tree-sitter)
  HighlightClassNames,
  HighlightSchemaVersion,
  HighlightCode,
  HighlightPatch,
  HighlightPatchWithContext,

  // Thread runtime mode (three-tier approval axis)
  GetThreadRuntimeMode,

  // Dev-only UI render tracing
  AppendUIRenderTraceBatch,
  BookmarkUIRenderTrace,
  GetUIRenderTracePath,

  // Always-on frontend runtime-error log
  ReportFrontendErrorBatch,

  // PR-based thread creation
  CreateThreadFromPR,

  // Turn lifecycle
  ListRecentTurns,

  // Windowed history + thread-wide aggregates. See /app_paging.go.
  // Active panes use bounded slice/cursor pagers; broad recent and
  // turn-based pagers remain available for legacy/full-tail surfaces.
  ListRecentThreadItems,
  ListThreadSliceAround,
  ListItemsBeforeCursor,
  ListItemsBeforeTurn,
  ListItemsAfterCursor,
  ListItemsAfterTurn,
  ListSubagentDescendants,
  ListThreadProposedPlans,
  ListProposedPlanComments,
  CountRunningBackgroundTasks,
  CreateProposedPlanComment,
  UpdateProposedPlanComment,
  DeleteProposedPlanComment,
  ListDiffReviewComments,
  CreateDiffReviewComment,
  UpdateDiffReviewComment,
  DeleteDiffReviewComment,
  MarkDiffReviewCommentsSent,
  SendDiffReviewComments,
  GetPRDetail,
  GetPRDiff,
  ListPRCommits,
  GetPRCommitDiff,
  GetPRMergeConflicts,
  GetMergeConflictFile,
  GetPRCIJobs,
  GetPRCIJobLog,
  SavePRCIJobLog,
  ListPRReviewThreads,
  SubmitPRReview,
  ReplyToPRThread,
  SubscribePRUpdates,
  UnsubscribePRUpdates,
  SetPRUpdatesActive,
  ListLiveBackgroundTasks,
  GetThreadItem,

  // Projects + directory browser
  ArchiveProject,
  BrowseDirectory,
  CreateProject,
  DeleteProject,
  ProjectDeletionPreview,
  ListProjects,
  RenameProject,
  UnarchiveProject,
  UpdateProjectSortPositions,

  // MCP library (1:1 sync with ~/.claude.json + ~/.codex/config.toml)
  ListMcpServers,
  ListMcpServersForThread,
  ListMcpServersForNewThread,
  CreateMcpServer,
  UpdateMcpServer,
  DeleteMcpServer,
  SetMcpServerEnabled,
  SetNewThreadMcpServerEnabled,
  GetMcpServerStatus,
  ListMcpServerStatuses,
  RefreshMcpServerStatus,
  TriggerMcpAuth,

  // In-app self-update (app_updater.go). LocalOnly — loopback callers only.
  CheckForUpdate,
  ListReleases,
  DownloadUpdate,
  RestartToUpdate,

  // Workflows
  WorkflowAnswerQuestion,
  WorkflowBindThread,
  WorkflowCancelItem,
  WorkflowCompleteTakeover,
  WorkflowCreateAutomation,
  WorkflowCreateItemPR,
  WorkflowDeleteAutomation,
  WorkflowDiscussPR,
  WorkflowDiscardItem,
  WorkflowDiscardPreview,
  WorkflowDropUnit,
  WorkflowFetchPRReviewComments,
  WorkflowGetEngineState,
  WorkflowGetItem,
  WorkflowGetJobNotes,
  WorkflowListAutomations,
  WorkflowListDefinitions,
  WorkflowListItemCosts,
  WorkflowListItems,
  WorkflowListUnresolvedItems,
  WorkflowMergeItem,
  WorkflowPauseItem,
  WorkflowRequestSoftStop,
  WorkflowResolveGate,
  WorkflowResumeItem,
  WorkflowRerunItem,
  WorkflowRetryFailedUnits,
  WorkflowRetryUnit,
  WorkflowRunAutomationNow,
  WorkflowSendPRReviewCommentsToThread,
  WorkflowSetAutomationEnabled,
  WorkflowSetGlobalPause,
  WorkflowSetJobNotes,
  WorkflowStartRun,
  WorkflowTakeOverUnit,
  WorkflowUnbindThread,
  WorkflowUpdateAutomation,

  // Build-time stamped binary version (Settings footer).
  Version,
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
  ElicitationResolution,
  PermissionProfile,
  UserInputResponse,
} from '../../../bindings/agent-overflow/internal/provider/models.js';

// Structured response types surfaced to components.
export {
  ChatBarFavorite,
  ThreadMessageHit,
} from '../../../bindings/agent-overflow/internal/store/models.js';
export {
  ServerStatus as MCPServerStatus,
  Provider as MCPStatusProvider,
  Status as MCPStatus,
  Source as MCPStatusSource,
} from '../../../bindings/agent-overflow/internal/mcpstatus/models.js';
export {
  MCPServer,
} from '../../../bindings/agent-overflow/models.js';
export {
  EditorSettings,
  RemoteEndpoint,
} from '../../../bindings/agent-overflow/internal/settings/models.js';
export {
  ManagedProviderAccount,
  ContextSettingsProfile,
  EditorInfo,
  GeneratedCommitMessage,
  GitWorkspaceState,
  GitStatusSubscriptionResult,
  MCPAuthInitResult,
  ReleaseSummary,
  RemoteEndpointSummary,
  TerminalOpenOptions,
  UpdateAvailability,
  WorktreeStatus,
} from '../../../bindings/agent-overflow/models.js';
export {
  Keybinding,
} from '../../../bindings/agent-overflow/internal/keybindings/models.js';
export {
  Settings as NetworkSettings,
} from '../../../bindings/agent-overflow/internal/network/models.js';
export {
  Distro as WSLDistro,
} from '../../../bindings/agent-overflow/internal/wsllauncher/models.js';

// CreateThread wrapper. The generated `CreateThread(opts: CreateThreadOptions)`
// types `opts` as a class instance; callers pass partial literals like
// `{ projectId }` which TS accepts structurally at the function boundary
// but trips when callers assign to a variable typed as the class. Keeping
// a thin interface + wrapper here lets every call site hand a plain
// object and cast the result without going through the class ceremony.
import {
  CreateThread as CreateThreadRaw,
  GetThreadDefaults as GetThreadDefaultsRaw,
  SendMessageWithOptions as SendMessageWithOptionsRaw,
  StartTerminal as StartTerminalRaw,
  UpdateContextSettingsProfile as UpdateContextSettingsProfileRaw,
  UpdateNewThreadDefaults as UpdateNewThreadDefaultsRaw,
  UpdateThreadContextSettings as UpdateThreadContextSettingsRaw,
} from '../../../bindings/agent-overflow/app.js';
import {
  CreateThreadOptions as CreateThreadOptionsClass,
  ContextSettingsUpdate as ContextSettingsUpdateClass,
  NewThreadDefaultsUpdate as NewThreadDefaultsUpdateClass,
  SendMessageOptions as SendMessageOptionsClass,
  StartTerminalOptions as StartTerminalOptionsClass,
} from '../../../bindings/agent-overflow/models.js';
import type { SourceDiffReview, SourceProposedPlan, Thread } from '../types/models';

export interface CreateThreadOptions {
  projectId: string;
  title?: string;
  provider?: 'claude' | 'codex' | string;
  model?: string;
  mode?: 'chat' | 'plan' | 'design' | 'discussion' | string;
  reasoningEffort?: 'low' | 'medium' | 'high' | 'xhigh' | 'max' | string;
  fastMode?: boolean | null;
  contextWindow?: number;
  autoCompactStandardPercent?: number | null;
  autoCompactExtendedPercent?: number | null;
  runtimeMode?: string;
  worktreeBranch?: string;
  workspaceOverride?: string;
  worktreePath?: string;
  branch?: string;
}

export type ContextSettingsUpdateInput = ConstructorParameters<
  typeof ContextSettingsUpdateClass
>[0];

export function UpdateContextSettingsProfile(update: ContextSettingsUpdateInput) {
  return UpdateContextSettingsProfileRaw(new ContextSettingsUpdateClass(update));
}

export function UpdateThreadContextSettings(
  threadId: string,
  update: ContextSettingsUpdateInput,
) {
  return UpdateThreadContextSettingsRaw(threadId, new ContextSettingsUpdateClass(update));
}

export interface NewThreadDefaultsUpdateInput {
  projectId: string;
  provider?: 'claude' | 'codex' | string;
  model?: string;
  reasoningEffort?: string;
  fastMode?: boolean | null;
  contextWindow?: number;
  autoCompactStandardPercent?: number | null;
  autoCompactExtendedPercent?: number | null;
  runtimeMode?: string;
}

export function UpdateNewThreadDefaults(
  update: NewThreadDefaultsUpdateInput,
): Promise<ThreadDefaults> {
  return UpdateNewThreadDefaultsRaw(
    new NewThreadDefaultsUpdateClass(update),
  ) as unknown as Promise<ThreadDefaults>;
}

export function CreateThread(opts: CreateThreadOptions): Promise<Thread> {
  return CreateThreadRaw(new CreateThreadOptionsClass(opts)) as unknown as Promise<Thread>;
}

// StartTerminal wrapper. Same plain-object-in / class-wrap pattern as
// CreateThread. projectId empty roots a standalone "home" terminal; cwd
// overrides the resolved root; title defaults to "Terminal" backend-side.
export interface StartTerminalOptions {
  projectId?: string;
  cwd?: string;
  title?: string;
}

export function StartTerminal(opts: StartTerminalOptions): Promise<Thread> {
  return StartTerminalRaw(new StartTerminalOptionsClass(opts)) as unknown as Promise<Thread>;
}

export interface ThreadDefaults {
  provider: string;
  model: string;
  reasoningEffort: string;
  fastMode: boolean;
  contextWindow: number;
  runtimeMode: string;
  branch: string;
  workspacePath: string;
}

export function GetThreadDefaults(opts: CreateThreadOptions): Promise<ThreadDefaults> {
  return GetThreadDefaultsRaw(new CreateThreadOptionsClass(opts)) as unknown as Promise<ThreadDefaults>;
}

export interface SendMessageOptions {
  attachmentIds?: string[];
  runtimeMode?: string;
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
  revisionSourceDiffReview?: SourceDiffReview;
  revisionSourceDiffCommentIds?: string[];
}

export function SendMessageWithOptions(
  threadId: string,
  content: string,
  opts: SendMessageOptions,
): Promise<Thread> {
  return SendMessageWithOptionsRaw(
    threadId,
    content,
    new SendMessageOptionsClass(opts),
  ) as unknown as Promise<Thread>;
}

/**
 * Send-queue surface — backend-owned per-thread queue for messages
 * the user submits while a wire round is in flight. The backend dispatch
 * worker delivers each queued message to the provider as soon as possible;
 * `GetQueueState` is the bootstrap snapshot for remote / re-attached
 * clients.
 */
import {
  RegisterQueueItem as RegisterQueueItemRaw,
  GetQueueState as GetQueueStateRaw,
} from '../../../bindings/agent-overflow/app.js';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/models';

export function RegisterQueueItem(
  threadId: string,
  message: string,
  opts: SendMessageOptions,
): Promise<WireQueuedItem> {
  return RegisterQueueItemRaw(
    threadId,
    message,
    new SendMessageOptionsClass(opts),
  ) as unknown as Promise<WireQueuedItem>;
}

export function GetQueueState(threadId: string): Promise<WireQueuedItem[]> {
  return GetQueueStateRaw(threadId) as unknown as Promise<WireQueuedItem[]>;
}

// Turn lifecycle `Turn` model is re-exported from the generated
// bindings. ListRecentTurns itself is re-exported in the main import
// block above. The frontend treats `completedAt=null` as historical
// in-flight (crash/interrupt); only a live `provider:turn_started`
// push writes into the global active-turn registry
// (threadStatuses.svelte.ts → getActiveTurn). See
// docs/architecture/invariants.md #22 and turn-lifecycle.md §Frontend
// state shape.
export { Turn } from '../../../bindings/agent-overflow/internal/store/models.js';

// Usage-accounting models. `GetUsageStats` takes a `UsageQuery` class
// instance (construct with `new UsageQuery({...})` — omitted fields
// default to zero values / '') and returns `UsageBucket[]`.
export { UsageBucket, UsageQuery } from '../../../bindings/agent-overflow/internal/store/models.js';

// Picker-facing worktree model. Unlike the lower-level internal/git shape,
// this includes backend-computed delete availability.
export { WorktreeListItem } from '../../../bindings/agent-overflow/models.js';
