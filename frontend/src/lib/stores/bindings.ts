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
  RevertConversationAndResendMessage,
  GetThread,
  ListArchivedThreads,
  ListThreads,
  MarkThreadRead,
  MarkThreadUnread,
  PinThread,
  SetThreadPinGroup,
  UnpinThread,
  RenameThread,
  RegenerateThreadTitle,
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

  // Background tasks (per-item + thread-wide stop primitives). The two
  // per-row stops are deliberately separate bindings: they take
  // different id namespaces (Claude task id vs Codex PTY process id) and
  // drive different provider RPCs, so callers branch on provider.
  StopClaudeTask,
  TerminateCodexBackgroundTerminal,
  StopCodexSubagent,
  CleanCodexBackgroundTerminals,
  // The opposite direction, Claude only: detach a running foreground
  // subagent / Bash from the turn instead of killing it. Keyed by
  // tool_use_id (the launch ROW id), not the task id StopClaudeTask
  // takes — Codex has no equivalent.
  BackgroundClaudeTask,

  // Codex thread operations the composer's `/compact` and `/review`
  // commands drive. Both are LOCAL-ONLY, both need a live session, and
  // both start a NON-STEERABLE turn — callers must refuse on a busy
  // thread rather than queue.
  CompactCodexThread,

  // Live-session context breakdown (Claude's canonical /context read).
  // On-demand only — the always-on meter is fed by streaming usage events.
  GetThreadContextUsage,

  // Data access
  GetPayloadPreview,
  GetPayloadChunk,
  GetPayloadData,
  ListItems,

  // Settings
  GetSettings,
  UpdateSettings,
  ClearBrowserSiteData,
  BrowserCompanionDo,
  BrowserCompanionInput,
  BrowserCompanionNextFrame,
  BrowserCompanionResize,
  BrowserCompanionSubscribe,
  BrowserCompanionUnsubscribe,
  GetContextSettings,

  // Custom provider environment. Dedicated mutators rather than
  // UpdateSettings keys: GetSettings redacts sensitive values, so a
  // read-mutate-write patch would persist the redaction. See
  // app_provider_env.go.
  SetProviderCustomEnvVar,
  DeleteProviderCustomEnvVar,

  // Per-project worktree setup (Settings -> Projects). Dedicated
  // read/write pair rather than settings keys: the recipe lives on the
  // project row, and the setter validates before persisting because the
  // argv commands it stores run unattended on every worktree the project
  // cuts. See app_project_worktree_setup.go.
  GetProjectWorktreeSetup,
  SetProjectWorktreeSetup,

  // Chat-thread worktree setup runs. The snapshot is the reconnect
  // companion to the worktree:setup event stream (a client that missed
  // every frame converges on the same state); the retry re-reads the
  // project's recipe, so fixing it in Settings and pressing Retry does
  // what the user means. See app_worktree_setup.go.
  GetThreadWorktreeSetup,
  RetryThreadWorktreeSetup,

  // Usage accounting (append-only ledger aggregates; costs are
  // wire-true for Claude, table-priced at read time for Codex /
  // claude-tui — see internal/usagecost).
  GetUsageStats,
  // Codex's OWN account-level token report, not the AO ledger. Returns
  // null when there is nothing to report (older codex, an API-key login,
  // a brand-new account) — absence, never zeros. See app_codex_usage.go.
  GetCodexAccountUsage,
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

  // Liveness gate for the dev-server chip: loopback-only on the wire,
  // so a remote session's probe fails and the chip stays hidden there.
  ProbeDevServerURL,

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
  // Composer command menu sources. GetClaudeSlashCommands is a pure read
  // of what the zero-token account probe left behind (never spawns) and
  // is available on a cold thread; GetCodexSkills and GetClaudeSkills
  // are LOCAL-ONLY and directory-scoped, so callers pass the thread's
  // workspace path and must tolerate a remote-client refusal.
  GetClaudeSlashCommands,
  GetClaudeSkills,
  GetCodexSkills,
  ProbeClaudeAccount,
  RecheckClaudeAccount,
  RecheckCodexAccount,

  // Git operations
  GenerateCommitMessage,
  GetGitStatus,
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
  GetLocalImageData,
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

  // Theme files + appearance selection (<configDir>/themes). The read
  // is LAN-allowed like GetKeybindings; both writers are local-only.
  GetThemeFiles,
  GetSpinnerFiles,
  SetAppearance,
  SetWindowBackgroundColor,

  // Review pane diffs (workspace / branch / per-commit / edits)
  GetBranchBaseDiff,
  GetWorkspaceCurrentDiff,
  ListBranchCommits,
  ListRecentCommits,
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

  // Idle renderer memory trim (utils/idleMemoryTrim.ts)
  RequestWebviewMemoryTrim,

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
  GetWorkspaceActivity,
  GetThreadItem,
  GetThreadUserMessageTicks,
  GetThreadUserMessageHistory,
  GetThreadTurnPreview,

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

  // Session import (provider session files → AO threads). All five are
  // LOCAL-ONLY: they read the provider homes and name file paths, so a
  // remote client gets a method_not_found refusal and the surface has to
  // stay disabled there (stores/sessionImport.svelte.ts refuses first).
  // ImportSessions starts an ASYNC run — progress arrives on the
  // `session-import:progress` channel, never as a return value.
  ListImportableSessions,
  ImportSessions,
  CancelSessionImport,
  CheckThreadImportUpdates,
  ImportThreadUpdates,

  // MCP (provider-native state: live session truth per thread, config
  // + status cache for threads without a session)
  ListThreadMcpServers,
  ListWorkspaceMcpServers,
  SetThreadMcpServerEnabled,
  SetWorkspaceMcpServerEnabled,
  ReconnectMcpServer,
  GetMcpServerStatus,
  RefreshMcpServerStatus,
  TriggerMcpAuth,
  TriggerWorkspaceMcpAuth,

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
  WorkflowGetRunMap,
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
} from '../../../bindings/agent-overflow/internal/mcpstatus/models.js';
export {
  EditorSettings,
  RemoteEndpoint,
} from '../../../bindings/agent-overflow/internal/settings/models.js';
export {
  ManagedProviderAccount,
  CodexAccountUsage,
  CodexAccountUsageBucket,
  ContextSettingsProfile,
  EditorInfo,
  GeneratedCommitMessage,
  GitWorkspaceState,
  GitStatusSubscriptionResult,
  MCPAuthInitResult,
  ReleaseSummary,
  RemoteEndpointSummary,
  TerminalOpenOptions,
  ThreadMCPServer,
  BrowserCompanionAction,
  UpdateAvailability,
  BusyThread,
  WorkspaceActivity,
  WorktreeStatus,
} from '../../../bindings/agent-overflow/models.js';
export {
  CompanionEvent as BrowserCompanionEvent,
  CompanionInput as BrowserCompanionInputEvent,
  CompanionSubscription as BrowserCompanionSubscription,
  PageInfo as BrowserPageInfo,
} from '../../../bindings/agent-overflow/internal/browser/models.js';
export {
  Keybinding,
} from '../../../bindings/agent-overflow/internal/keybindings/models.js';
export {
  Appearance as ThemeAppearance,
  File as ThemeFile,
  Files as ThemeFiles,
} from '../../../bindings/agent-overflow/internal/theme/models.js';
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
import type { ReasoningEffort } from '../types/settings';

export interface CreateThreadOptions {
  projectId: string;
  title?: string;
  provider?: 'claude' | 'codex' | string;
  model?: string;
  mode?: 'chat' | 'plan' | 'design' | 'discussion' | string;
  // `| string` keeps a slug this build does not know from failing to
  // compile; the union is the canonical set (see types/settings.ts).
  reasoningEffort?: ReasoningEffort | string;
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

// Composer command-menu wire shapes. `SlashCommand.name` never carries a
// leading slash on any of the three surfaces that report one, and
// `ClaudeSlashCommands.probed === false` means UNKNOWN — a menu must not
// render it as "this binary has none".
export type { ClaudeSlashCommands } from '../../../bindings/agent-overflow/models';
export type { SlashCommand } from '../../../bindings/agent-overflow/internal/provider/models';
export type {
  CwdSkills as CodexCwdSkills,
  Skill as CodexSkill,
} from '../../../bindings/agent-overflow/internal/codexskills/models';
export type { Skill as ClaudeSkill } from '../../../bindings/agent-overflow/internal/claudeconfig/models';

// StartCodexReview wrapper. Same plain-object-in / class-wrap pattern as
// CreateThread: the generated signature types the target as a class
// instance, and every call site wants to hand a literal. The four
// variants share one flat shape discriminated by `kind`; the backend
// validates the per-variant required field and ignores the rest.
import {
  StartCodexReview as StartCodexReviewRaw,
} from '../../../bindings/agent-overflow/app.js';
import {
  CodexReviewTarget as CodexReviewTargetClass,
} from '../../../bindings/agent-overflow/models.js';
import type { CodexReviewStarted } from '../../../bindings/agent-overflow/models';

export type CodexReviewTargetKind =
  | 'uncommittedChanges'
  | 'baseBranch'
  | 'commit'
  | 'custom';

export interface CodexReviewTargetInput {
  kind: CodexReviewTargetKind;
  /** Required for `baseBranch`. */
  branch?: string;
  /** Required for `commit`. */
  sha?: string;
  /** Optional human label for a `commit` target. */
  title?: string;
  /** Required for `custom`. */
  instructions?: string;
}

export function StartCodexReview(
  threadId: string,
  target: CodexReviewTargetInput,
): Promise<CodexReviewStarted> {
  return StartCodexReviewRaw(
    threadId,
    new CodexReviewTargetClass(target),
  ) as unknown as Promise<CodexReviewStarted>;
}

// SyncThreadWindow wrapper. Same plain-object-in / class-wrap pattern as
// CreateThread: the generated signature types the request as a class
// instance and every call site wants to hand a literal. The response is
// re-typed with a narrowed `status` so callers must handle the four
// answers the store defines rather than an open string
// (docs/architecture/thread-replica-sync.md §5).
import {
  SyncThreadWindow as SyncThreadWindowRaw,
} from '../../../bindings/agent-overflow/app.js';
import {
  SyncThreadWindowRequest as SyncThreadWindowRequestClass,
} from '../../../bindings/agent-overflow/models.js';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';

export type SyncThreadWindowStatus = 'fresh' | 'stale' | 'rewritten' | 'gone';

export interface SyncThreadWindowInput {
  /** Saved scroll anchor; empty resolves to the thread's tail. */
  anchorItemId: string;
  itemBudget: number;
  /** -1 when no cached window backs the stamp. */
  haveEpoch: number;
  haveRev: number;
}

export interface SyncThreadWindowResult {
  status: SyncThreadWindowStatus | string;
  epoch: number;
  rev: number;
  generation: string;
  /** Null for `fresh` and `gone`. */
  page?: PagedItems | null;
}

export function SyncThreadWindow(
  threadId: string,
  req: SyncThreadWindowInput,
): Promise<SyncThreadWindowResult> {
  return SyncThreadWindowRaw(
    threadId,
    new SyncThreadWindowRequestClass(req),
  ) as unknown as Promise<SyncThreadWindowResult>;
}
