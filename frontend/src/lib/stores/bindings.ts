// Re-export Wails v3 generated bindings used by components.
export {
  // Thread management
  CreateThread,
  ArchiveThread,
  DeleteThread,
  ListThreads,
  RenameThread,

  // Session management
  StartSession,
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

  // Git operations
  GetGitStatus,
  GitListBranches,
  GitCommit,
  GitPush,
  GitPull,
  GitCheckout,
  GitCreateBranch,
  GitCreateWorktree,
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
} from '../../../bindings/agent-overflow/internal/provider/models.js';
