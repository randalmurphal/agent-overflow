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

  // Network bindings (LAN-bind toggle for the embedded transport).
  GetNetworkSettings,
  SetNetworkSettings,

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
  GetDiscussion,
  CreateDiscussion,
  UpdateDiscussion,
  DeleteDiscussion,
  StartDiscussion,
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
  SaveDraft,
  GetDraft,
  ClearDraft,
  SearchWorkspaceFiles,
  WriteThreadWorkspaceFile,

  // Message search
  SearchThreadMessages,

  // Keybindings
  GetKeybindings,
  UpdateKeybindings,
  ResetKeybindings,

  // Checkpoints (per-turn git-ref snapshots for diff panel + revert UX)
  GetCheckpointRangeDiff,
  RevertToCheckpoint,
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
  ThreadMessageHit,
} from '../../../bindings/agent-overflow/internal/store/models.js';
export {
  EditorSettings,
  RemoteEndpoint,
} from '../../../bindings/agent-overflow/internal/settings/models.js';
export {
  EditorInfo,
  GeneratedCommitMessage,
  Keybinding,
  NetworkSettings,
  RemoteEndpointSummary,
  TerminalOpenOptions,
} from '../../../bindings/agent-overflow/models.js';

// CreateThread wrapper. The generated `CreateThread(opts: CreateThreadOptions)`
// types `opts` as a class instance; callers pass partial literals like
// `{ projectId }` which TS accepts structurally at the function boundary
// but trips when callers assign to a variable typed as the class. Keeping
// a thin interface + wrapper here lets every call site hand a plain
// object and cast the result without going through the class ceremony.
import {
  CreateThread as CreateThreadRaw,
  SendMessageWithOptions as SendMessageWithOptionsRaw,
} from '../../../bindings/agent-overflow/app.js';
import {
  CreateThreadOptions as CreateThreadOptionsClass,
  SendMessageOptions as SendMessageOptionsClass,
} from '../../../bindings/agent-overflow/models.js';
import type { Thread } from '../types/models';

export interface CreateThreadOptions {
  projectId: string;
  provider?: 'claude' | 'codex' | string;
  model?: string;
  mode?: 'chat' | 'plan' | 'design' | 'discussion' | string;
  reasoningEffort?: 'low' | 'medium' | 'high' | 'xhigh' | 'max' | string;
  fastMode?: boolean | null;
  contextWindow?: number;
  runtimeMode?: string;
  worktreeBranch?: string;
  workspaceOverride?: string;
}

export function CreateThread(opts: CreateThreadOptions): Promise<Thread> {
  return CreateThreadRaw(new CreateThreadOptionsClass(opts)) as unknown as Promise<Thread>;
}

export interface SendMessageOptions {
  attachmentIds?: string[];
  runtimeMode?: string;
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

// Turn lifecycle `Turn` model is re-exported from the generated
// bindings. ListRecentTurns itself is re-exported in the main import
// block above. The frontend treats `completedAt=null` as historical
// in-flight (crash/interrupt); only a live `provider:turn_started`
// push sets `pane.activeTurn`. See docs/architecture/invariants.md #22
// and docs/architecture/turn-lifecycle.md §Frontend state shape.
export { Turn } from '../../../bindings/agent-overflow/internal/store/models.js';
