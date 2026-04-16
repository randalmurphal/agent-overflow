import { Call } from '@wailsio/runtime';

// Re-export Wails v3 generated bindings used by components.
export {
  // Thread management
  CreateThread,
  ArchiveThread,
  DeleteThread,
  GetThread,
  ListThreads,
  RenameThread,
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
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
  PermissionProfile,
} from '../../../bindings/agent-overflow/internal/provider/models.js';

export function WriteThreadWorkspaceFile(
  threadID: string,
  relativePath: string,
  content: string
): Promise<string> {
  return Call.ByName('main.App.WriteThreadWorkspaceFile', threadID, relativePath, content);
}
