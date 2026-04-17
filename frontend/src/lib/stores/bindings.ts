import { Call } from '@wailsio/runtime';
import type { Thread } from '../types/models';

// Re-export Wails v3 generated bindings used by components.
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

  // Keybindings
  GetKeybindings,
  UpdateKeybindings,
  ResetKeybindings,

  // Checkpoints (per-turn git-ref snapshots for diff panel + revert UX)
  GetTurnDiff,
  GetCheckpointToWorktreeDiff,
  RevertToTurn,
  ListThreadCheckpoints,
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

export function UpdateThreadModel(threadID: string, model: string): Promise<Thread> {
  return Call.ByName('main.App.UpdateThreadModel', threadID, model) as Promise<Thread>;
}
