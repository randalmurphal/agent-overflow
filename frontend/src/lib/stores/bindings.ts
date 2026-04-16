// Re-export Wails v3 generated bindings used by components.
export {
  // Thread management
  CreateThread,
  ArchiveThread,
  DeleteThread,
  ListThreads,
  RenameThread,
  GetThread,
  SwitchThread,

  // Session management
  StartSession,
  StopSession,
  ReconnectSession,
  SendMessage,
  InterruptTurn,
  RespondToApproval,

  // Data access
  GetPayloadData,
  GetWorkingTreeDiff,
  ListItems,
  ListPayloadMetas,

  // Settings
  GetSettings,
  UpdateSettings,

  // Provider detection
  GetProviderStatuses,
  GetModelsForProvider,

  // Git operations
  GetGitStatus,
  GitListBranches,
  GitCommit,
  GitPush,
  GitPull,
  GitCheckout,
  GitCreateBranch,
  GitCreatePR,
  GitCreateWorktree,
  GitListWorktrees,
  GitRemoveWorktree,
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
} from '../../../bindings/agent-overflow/internal/provider/models.js';
