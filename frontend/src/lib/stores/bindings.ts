// Re-export Wails v3 generated bindings used by components.
//
// Every entry here is produced by `wails3 generate bindings -ts`. When
// a new App method is added on the Go side, run
// `wails3 task common:generate:bindings` and re-export it from this file
// -- do not hand-wrap bindings.
//
// Every call below lands on the transport `lib/transport/handle.ts`
// resolves: the generated wrappers call the runtime shim's `Call.ByID`,
// which asks for the connection per call instead of importing the WS
// singleton, so a second attached backend is a change to that resolution
// and not to this file (docs/specs/remote-access.md §10).
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

  // Thread groups (sidebar grouping rows; docs/specs/sidebar-thread-groups.md)
  ListThreadGroups,
  CreateThreadGroup,
  RenameThreadGroup,
  DeleteThreadGroup,
  PinThreadGroup,
  UnpinThreadGroup,
  SetThreadGroupPinGroup,
  SetThreadGroup,

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
  BrowserCompanionThreadState,
  BrowserCompanionPaneAttach,
  BrowserCompanionPaneDetach,
  BrowserCompanionPaneRect,
  BrowserCompanionRevealPageFile,
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
  // cuts. See app_projects.go.
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
  // a brand-new account) — absence, never zeros. See app_codex_bindings.go.
  GetCodexAccountUsage,
  GetRateLimitsSnapshots,
  ListProviderAccounts,
  // A provider sign-in is a session, not one blocking call: it may be
  // finished on a different device than the one that started it, so the
  // link goes out and the answer comes back through four fast calls plus
  // the `provider:login` push. See internal/provideraccountapp/loginsession.go.
  StartProviderLogin,
  GetProviderLoginState,
  SubmitProviderLoginCode,
  CancelProviderLogin,
  SwitchProviderAccount,
  RemoveProviderAccount,
  RefreshProviderAccountUsage,

  // Per-client UI view state (ui_state table) behind stores/appStorage.ts.
  GetUIState,
  SetUIState,
  DeleteUIState,

  // Network bindings (LAN-bind toggle, canonical domain and its
  // certificate, for the embedded transport). RenewCanonicalDomainCert
  // kicks the reconciler and returns immediately: obtaining a
  // certificate outlives an RPC, so the screen polls
  // GetNetworkSettings while `tls.renewing` is set.
  GetNetworkSettings,
  SetNetworkSettings,
  RenewCanonicalDomainCert,

  // The tailnet node rides the same two calls: the toggle and its
  // coordination server are fields on the network record, so turning it
  // on is one step-up-gated write. ForgetTailnetNode is the separate act
  // that deletes the node's identity, and the backend refuses it until
  // the feature is off.
  ForgetTailnetNode,

  // Device access (Settings → Remote access → Devices): the paired-device
  // list, the pairing lifecycle, and revocation. Every one needs
  // `access:admin`; MintDevicePairing also needs a host-presence proof,
  // so a link can only be created from the backend's own screen.
  GetAccessOverview,
  MintDevicePairing,
  DevicePairingStatus,
  ConfirmDevicePairing,
  CancelDevicePairing,
  RevokeAccessDevice,
  RevokeAccessSession,
  RestoreAccessDevice,
  ForgetAccessDevice,

  // Phone push (docs/specs/remote-access.md §9). The two registrations
  // are at the SESSION FLOOR because each reaches the calling device's own
  // row and no other; the status read and the credential pair are
  // `access:admin`, the pair step-up gated on top, so both "my phone
  // stopped buzzing" and "install the key on the serve host" are done from
  // somewhere other than the machine.
  RegisterPushToken,
  UnregisterPushToken,
  GetPushSenderStatus,
  SetPushSenderCredential,
  ClearPushSenderCredential,

  // Passkeys, on the same `access:admin` surface. Registration is
  // additionally step-up gated, because it issues something that admits a
  // future caller; the two ceremony calls below are the FLOOR, since they
  // are how a session satisfies that gate rather than something it is
  // granted. Signing IN has no binding at all — its caller holds no
  // session, so it is an HTTP route (transport/deviceSession.ts).
  BeginPasskeyRegistration,
  FinishPasskeyRegistration,
  ListPasskeys,
  DeletePasskey,
  BeginPasskeyStepUp,
  FinishPasskeyStepUp,

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

  // The port gateway (docs/specs/remote-access.md §7): one machine's
  // shareable dev-server ports, and a single-use URL to open one from
  // another device. Read through stores/devServers.svelte.ts.
  GetDevServers,
  AllowPreviewPort,
  DisallowPreviewPort,
  MintPreviewURL,

  // The other machines this installation drives. Host-scoped: attaching
  // one is something only the person at this keyboard does.
  ListBackends,
  AddBackend,
  RemoveBackend,
  RenameBackend,

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
  GitStatusSubscribe,
  GitStatusUnsubscribe,
  GitListBranches,
  GitListWorktrees,
  GitCommit,
  GitPush,
  GitPull,
  GitCheckout,
  GitCreateBranchFrom,
  GitCreatePR,
  GitCreateWorktree,
  GitMaybeFetchRemotes,
  GitListBranchPruneCandidates,
  GitPruneBranches,
  GitSyncBranch,
  GitRemoveWorktree,
  GitWorktreeStatus,
  PrepareThreadWorktree,
  AttachThreadWorktree,
  RemoveOtherWorktree,

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

  // Composer enhancements.
  //
  // Attachment BYTES do not cross here: they ride HTTP, admitted by a
  // single-use ticket one of the two Mint calls returns a relative URL
  // for (lib/transport/attachmentTransfer.ts). GetAttachmentThumbnail is
  // the deliberate exception — ~10-30 KB is not a large body, and a grid
  // would pay a mint round trip per tile.
  MintAttachmentUploadTicket,
  MintAttachmentDownloadTicket,
  ListAttachments,
  DeleteAttachment,
  GetAttachmentThumbnail,
  GetLocalImageData,
  SaveDraft,
  GetDraft,
  ClearDraft,
  DeleteEmptyDraftThread,
  SearchWorkspaceFiles,
  WriteWorkspaceFile,
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
  GetEditDiffContextLines,
  VerifyEditDiffs,
  ListThreadEditDiffs,
  GetTurnEditsDiff,

  // Syntax-highlight span metadata (backend tree-sitter)
  HighlightClassNames,
  HighlightSchemaVersion,
  HighlightCode,
  HighlightPatch,
  HighlightPatchWithContext,
  HighlightEditPatchWithContext,

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
  // Active panes load a bounded slice and page by item-coordinate
  // cursor; there is no turn-based pager.
  ListThreadSliceAround,
  ListItemsBeforeCursor,
  ListItemsAfterCursor,
  ListSubagentDescendants,
  // Recovery route out of the wire projection: returns the complete
  // stored `meta` / `payloadMeta` / `payloadPreviewSpans` for one item,
  // for a consumer that reached a projection marker and now needs the
  // value behind it. Fetched on expand, never on arrival.
  GetThreadItemProjectionSource,
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

  // Session import (provider session files → AO threads). All five ride
  // `threads:operate`: they read the provider homes and name file paths,
  // so a session without that grant is refused and the surface has to
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

  // In-app self-update (internal/appupdate, via root bindings). `host`-scoped — no grant reaches them.
  CheckForUpdate,
  ListReleases,
  DownloadUpdate,
  RestartToUpdate,

  // Updating a SUPERVISED serve host from wherever you are
  // (docs/architecture/serve-mode.md § Updating over the wire). All three
  // are `access:admin` and `route selected`; the request is step-up gated
  // on top, and the interception in the dispatch path collects that proof.
  GetServiceUpdateStatus,
  ListServiceReleases,
  RequestServiceUpdate,

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
  LoginMethod as ProviderLoginMethod,
  LoginPhase as ProviderLoginPhase,
  LoginState as ProviderLoginState,
} from '../../../bindings/agent-overflow/internal/provideraccountapp/models.js';
export { EditorSettings } from '../../../bindings/agent-overflow/internal/settings/models.js';
export {
  Attached as AttachedBackend,
  Attachment as BackendAttachment,
} from '../../../bindings/agent-overflow/internal/attachedbackends/models.js';
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
  TerminalOpenOptions,
  ThreadMCPServer,
  BrowserCompanionAction,
  UpdateAvailability,
  BusyThread,
  WorkspaceActivity,
  WorktreeStatus,
} from '../../../bindings/agent-overflow/internal/app/models.js';
export {
  CompanionEvent as BrowserCompanionEvent,
  PageInfo as BrowserPageInfo,
  PaneRect as BrowserPaneRect,
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
// Dev-server rows are read-only views of one machine's scan, never
// constructed by a component.
export type {
  DevServer,
  DevServerList,
} from '../../../bindings/agent-overflow/internal/devscan/models.js';
// Device-access DTOs are read-only views; components never construct
// one, so type-only exports keep the classes out of the bundle.
export type {
  AccessOverview,
  AccessDevice,
  AccessSession,
  AccessAuditEntry,
  PendingPairing,
  PairingInvite,
  PairingStatusView,
  DeviceRevocationResult,
  PasskeySummary,
  PasskeyChallengeResult,
  PasskeyStepUpGrant,
  PushSenderStatus,
  ServiceUpdateStatus,
} from '../../../bindings/agent-overflow/internal/app/models.js';
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
} from '../../../bindings/agent-overflow/internal/app/models.js';
import type { SourceDiffReview, SourceProposedPlan, Thread } from '../types/models';
import type { ReasoningEffort } from '../types/settings';

export interface CreateThreadOptions {
  projectId: string;
  title?: string;
  provider?: 'claude' | 'codex' | string;
  model?: string;
  mode?: 'chat' | 'plan' | 'discussion' | string;
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
  /**
   * Idempotency id for this send, minted by
   * `utils/sendOptions.ts#buildSendOptions` and by nothing else. Optional
   * on the wire because it is optional on the backend: an empty one simply
   * does not dedupe. Every call site that a person can trigger twice
   * should carry one.
   */
  sendId?: string;
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
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/internal/app/models';

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
export { WorktreeListItem } from '../../../bindings/agent-overflow/internal/app/models.js';

// Composer command-menu wire shapes. `SlashCommand.name` never carries a
// leading slash on any of the three surfaces that report one, and
// `ClaudeSlashCommands.probed === false` means UNKNOWN — a menu must not
// render it as "this binary has none".
export type { ClaudeSlashCommands } from '../../../bindings/agent-overflow/internal/app/models';
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
} from '../../../bindings/agent-overflow/internal/app/models.js';
import type { CodexReviewStarted } from '../../../bindings/agent-overflow/internal/app/models';

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
} from '../../../bindings/agent-overflow/internal/app/models.js';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';

export type SyncThreadWindowStatus = 'fresh' | 'stale' | 'rewritten' | 'gone';

export interface SyncThreadWindowInput {
  /** Saved scroll anchor; empty resolves to the thread's tail. */
  anchorItemId: string;
  itemBudget: number;
  /** -1 when no cached window backs the stamp. */
  haveEpoch: number;
  haveRev: number;
  /**
   * The client's stated projection preference — `wantsInlinePreviews()`
   * in threadPaneShared, never a literal.
   *
   * Required here although the generated request class types it optional
   * (Go's `omitempty`): the positional item-window bindings make the
   * preference impossible to omit, and the one path that passes a request
   * object should be no easier to under-specify. Omitting it would ask
   * for a different projection than the rest of the window.
   */
  inlinePreviews: boolean;
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
