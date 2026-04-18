// Re-export Wails v3 generated bindings used by components.
//
// Every entry here is produced by `wails3 generate bindings`. When a new
// App method is added on the Go side, run the generator and re-export it
// from this file — do not hand-wrap bindings.
export {
  // Thread management
  CreateThread,
  ArchiveThread,
  UnarchiveThread,
  DeleteThread,
  ForkThread,
  GetThread,
  ListThreads,
  RenameThread,
  SwitchThread,
  UpdateThreadModel,

  // Session management
  StartSession,
  StopSession,
  ReconnectSession,
  SendMessage,
  InterruptTurn,
  RespondToApproval,

  // Data access
  GetPayloadData,
  ListItems,
  ListPayloadMetas,

  // Settings
  GetSettings,
  UpdateSettings,

  // Provider detection
  GetProviderStatuses,
  GetModelsForProvider,
  ProbeClaudeAccount,

  // Git operations
  GenerateCommitMessage,
  GetGitStatus,
  GetWorkingTreeDiff,
  GitListBranches,
  GitCommit,
  GitPush,
  GitPull,
  GitCheckout,
  GitCreateBranch,
  GitCreatePR,
  GitCreateWorktree,
  GitRemoveWorktree,

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
  GetTurnDiff,
  GetCheckpointToWorktreeDiff,
  RevertToTurn,
  ListThreadCheckpoints,

  // Thread interaction mode
  SetThreadInteractionMode,

  // Thread runtime mode (three-tier approval axis)
  GetThreadRuntimeMode,
  SetThreadRuntimeMode,

  // PR-based thread creation
  CreateThreadFromPR,
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
  ElicitationResolution,
  PermissionProfile,
} from '../../../bindings/agent-overflow/internal/provider/models.js';

// Structured response types surfaced to components.
export { ThreadMessageHit } from '../../../bindings/agent-overflow/internal/store/models.js';
export {
  GeneratedCommitMessage,
  Keybinding,
  TerminalOpenOptions,
} from '../../../bindings/agent-overflow/models.js';
