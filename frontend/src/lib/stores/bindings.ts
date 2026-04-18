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

  // Thread interaction mode
  SetThreadInteractionMode,

  // PR-based thread creation
  CreateThreadFromPR,
} from '../../../bindings/agent-overflow/app.js';

// Model classes needed for constructing RPC parameters.
export {
  ApprovalResponse,
  ElicitationResolution,
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

/**
 * Global message search — returns thread-title and item-summary hits for the
 * given query. See internal/store/search.go for the ranking contract.
 *
 * Wrapped by hand so the binding is available before the next wails generate
 * pass picks up the new App method.
 */
export interface ThreadMessageHit {
  threadId: string;
  threadTitle: string;
  provider: string;
  itemId: string;
  turnIndex: number;
  itemKind: string;
  itemRole: string;
  summary: string;
  matchType: 'title' | 'item';
}

export function SearchThreadMessages(query: string, limit: number): Promise<ThreadMessageHit[]> {
  return Call.ByName('main.App.SearchThreadMessages', query, limit) as Promise<ThreadMessageHit[]>;
}

/**
 * AI-generated commit message for the thread's current working-tree diff.
 * Returns {subject, body}; subject is always non-empty on success. The
 * binding is hand-wired until the next `wails generate` pass; see
 * app_commit_message.go:GenerateCommitMessage for the Go side.
 */
export interface GeneratedCommitMessage {
  subject: string;
  body: string;
}

export function GenerateCommitMessage(threadID: string): Promise<GeneratedCommitMessage> {
  return Call.ByName('main.App.GenerateCommitMessage', threadID) as Promise<GeneratedCommitMessage>;
}
