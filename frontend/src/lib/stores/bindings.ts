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
  GetThread,
  ListThreads,
  MarkThreadRead,
  MarkThreadUnread,
  PinThread,
  UnpinThread,
  RenameThread,
  SwitchThread,
  UpdateThreadModel,
  UpdateThreadProvider,
  UpdateThreadMode,
  UpdateThreadReasoningEffort,
  UpdateThreadFastMode,
  UpdateThreadContextWindow,
  UpdateThreadRuntimeMode,
  UpdateThreadBranch,
  UpdateThreadWorkspace,

  // Session management
  StartSession,
  StopSession,
  ReconnectSession,
  SendMessage,
  InterruptTurn,
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
  ListPayloadMetas,

  // Settings
  GetSettings,
  UpdateSettings,
  GetContextSettings,

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

  // Open-in-editor: catalog + persistence + user-facing entry point.
  OpenInEditor,
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
  GitCreateBranch,
  GitCreatePR,
  GitCreateWorktree,
  GitRemoveWorktree,
  PrepareThreadWorktree,

  // Terminal operations
  CloseTerminal,
  GetTerminalReplay,
  ListTerminals,
  OpenTerminal,
  ResizeTerminal,
  RestartTerminal,
  WriteTerminal,

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
  PostChannelMessage,

  // Design operations
  ChooseDesignOption,
  ListDesignArtifacts,
  GetDesignArtifactHTML,

  // Composer enhancements
  UploadAttachment,
  ListAttachments,
  DeleteAttachment,
  GetAttachmentData,
  GetAttachmentThumbnail,
  SaveDraft,
  GetDraft,
  ClearDraft,
  SearchWorkspaceFiles,
  WriteThreadWorkspaceFile,
  ListChatBarFavorites,
  SetChatBarFavorite,

  // Message search
  SearchThreadMessages,

  // Keybindings
  GetKeybindings,
  UpdateKeybindings,
  ResetKeybindings,

  // Checkpoints (message-keyed git-ref snapshots for diff panel + revert UX)
  GetMessageCheckpointDiff,
  GetMessageCheckpointRevertDiff,
  GetSessionAgentDiff,
  GetWorkspaceCurrentDiff,
  RevertToMessageCheckpoint,
  ListThreadCheckpoints,

  // Thread runtime mode (three-tier approval axis)
  GetThreadRuntimeMode,
  SetThreadRuntimeMode,

  // Slash commands (Claude-only)
  GetThreadSlashCommands,

  // Dev-only UI render tracing
  AppendUIRenderTraceBatch,
  GetUIRenderTracePath,

  // PR-based thread creation
  CreateThreadFromPR,

  // Turn lifecycle
  ListRecentTurns,

  // Windowed history + thread-wide aggregates. See /app_paging.go.
  // `ListRecentThreadItems` replaces `ListItems` on thread switch; the
  // others back dedicated panel/sidebar surfaces (plans, tray)
  // so they don't under-report against a partial timeline window.
  ListRecentThreadItems,
  ListItemsBeforeTurn,
  ListThreadProposedPlans,
  ListProposedPlanComments,
  CreateProposedPlanComment,
  UpdateProposedPlanComment,
  DeleteProposedPlanComment,
  ListLiveBackgroundTasks,
  GetThreadItem,

  // Projects + directory browser
  ArchiveProject,
  BrowseDirectory,
  CreateProject,
  DeleteProject,
  ListProjects,
  RenameProject,
  UnarchiveProject,
  UpdateProjectSortPositions,
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
  EditorSettings,
  RemoteEndpoint,
} from '../../../bindings/agent-overflow/internal/settings/models.js';
export {
  ContextSettingsProfile,
  EditorInfo,
  GeneratedCommitMessage,
  GitStatusSubscriptionResult,
  Keybinding,
  NetworkSettings,
  RemoteEndpointSummary,
  TerminalOpenOptions,
} from '../../../bindings/agent-overflow/models.js';
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
  SendMessageWithOptions as SendMessageWithOptionsRaw,
  SteerMessageWithOptions as SteerMessageWithOptionsRaw,
  UpdateContextSettingsProfile as UpdateContextSettingsProfileRaw,
  UpdateThreadContextSettings as UpdateThreadContextSettingsRaw,
} from '../../../bindings/agent-overflow/app.js';
import {
  CreateThreadOptions as CreateThreadOptionsClass,
  ContextSettingsUpdate as ContextSettingsUpdateClass,
  SendMessageOptions as SendMessageOptionsClass,
} from '../../../bindings/agent-overflow/models.js';
import type { SourceProposedPlan, Thread } from '../types/models';

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

export function CreateThread(opts: CreateThreadOptions): Promise<Thread> {
  return CreateThreadRaw(new CreateThreadOptionsClass(opts)) as unknown as Promise<Thread>;
}

export interface SendMessageOptions {
  attachmentIds?: string[];
  runtimeMode?: string;
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
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
 * SteerMessageWithOptions is the Codex-only mid-turn-injection
 * counterpart to SendMessageWithOptions. The active-turn check is the
 * caller's responsibility — see Composer.svelte's
 * `dispatchSteerOrEnqueue` for the routing decision (Codex active turn
 * → here; Claude active turn → enqueue path).
 */
export function SteerMessageWithOptions(
  threadId: string,
  content: string,
  opts: SendMessageOptions,
): Promise<Thread> {
  return SteerMessageWithOptionsRaw(
    threadId,
    content,
    new SendMessageOptionsClass(opts),
  ) as unknown as Promise<Thread>;
}

/**
 * Send-queue surface — backend-owned per-thread queue for messages
 * the user submits while a wire round is in flight. The trigger fires
 * on the first non-subagent tool_use of the round and the dispatcher
 * delivers each queued message to the provider; until then, items
 * sit in `RegisterQueueItem` and can be retracted via
 * `UndoQueuedItems`. `GetQueueState` is the bootstrap snapshot for
 * remote / re-attached clients.
 */
import {
  RegisterQueueItem as RegisterQueueItemRaw,
  UndoQueuedItems as UndoQueuedItemsRaw,
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

export function UndoQueuedItems(threadId: string): Promise<WireQueuedItem[]> {
  return UndoQueuedItemsRaw(threadId) as unknown as Promise<WireQueuedItem[]>;
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
