// Re-export Wails v3 generated bindings used by components.
//
// Every entry here is produced by `wails3 generate bindings`. When a new
// App method is added on the Go side, run the generator and re-export it
// from this file — do not hand-wrap bindings.
export {
  // Thread management
  //
  // CreateThread is NOT re-exported from the generated app.js because
  // the generated CreateThreadOptions class emits optional fields under
  // an `if (false)` pattern that TypeScript infers as required. Callers
  // pass `{ projectId }` or similar partials that the Go side treats as
  // optional, so we re-wrap below against the Wails runtime directly.
  ArchiveThread,
  UnarchiveThread,
  DeleteThread,
  ForkThread,
  GetThread,
  ListThreads,
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

  // Data access
  GetPayloadPreview,
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
  GitListWorktrees,
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

  // Thread runtime mode (three-tier approval axis)
  GetThreadRuntimeMode,
  SetThreadRuntimeMode,

  // Slash commands (Claude-only)
  GetThreadSlashCommands,

  // PR-based thread creation
  CreateThreadFromPR,

  // Turn lifecycle
  ListRecentTurns,
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

// ---------------------------------------------------------------------------
// Projects + directory-browser bindings.
//
// These landed on the Go side in Wave 1/2 (see /app_projects.go and
// /app_directory.go) but the `wails3 generate bindings` output in this
// checkout doesn't yet carry them. Hand-wrapping via Call.ByName keeps
// the sidebar compiling and unblocks Wave 3a while we wait for the
// generator to catch up. The tests drive the same Call.ByName path via
// src/test/mocks/wailsio-runtime.ts, so mock + real call paths match.
// ---------------------------------------------------------------------------

import { Call } from '@wailsio/runtime';
import type {
  DirectoryListing,
  Project,
  ProjectWithCounts,
  Thread,
} from '../types/models';

/**
 * Options the frontend passes to CreateThread. Mirrors the Go
 * CreateThreadOptions struct (projectId required; every other field
 * defaults from settings when omitted). We keep a hand-written
 * interface here because the Wails-generated CreateThreadOptions class
 * encodes optional fields under an `if (false)` pattern that the
 * TypeScript inference engine reads as required — the generated class
 * is usable at runtime but not typeable on the call site.
 */
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

/**
 * CreateThread persists a new thread. Wrapped via Call.ByName so the
 * call site can pass a partial options bag without fighting the
 * generated class's over-strict TypeScript inference.
 */
export function CreateThread(opts: CreateThreadOptions): Promise<Thread> {
  return callApp<Thread>('CreateThread', opts);
}

function callApp<T>(method: string, ...args: unknown[]): Promise<T> {
  return Call.ByName(`main.App.${method}`, ...args) as unknown as Promise<T>;
}

export function ListProjects(): Promise<ProjectWithCounts[]> {
  return callApp<ProjectWithCounts[] | null>('ListProjects').then(
    (result) => result ?? [],
  );
}

export function CreateProject(path: string): Promise<Project> {
  return callApp<Project>('CreateProject', path);
}

export function RenameProject(id: string, name: string): Promise<Project> {
  return callApp<Project>('RenameProject', id, name);
}

export function DeleteProject(id: string): Promise<string[]> {
  return callApp<string[] | null>('DeleteProject', id).then(
    (result) => result ?? [],
  );
}

export function ArchiveProject(id: string): Promise<void> {
  return callApp<void>('ArchiveProject', id);
}

export function UnarchiveProject(id: string): Promise<Project> {
  return callApp<Project>('UnarchiveProject', id);
}

export function BrowseDirectory(path: string): Promise<DirectoryListing> {
  return callApp<DirectoryListing>('BrowseDirectory', path);
}

// Turn lifecycle `Turn` model is re-exported from the generated
// bindings. ListRecentTurns itself is re-exported in the main import
// block above. The frontend treats `completedAt=null` as historical
// in-flight (crash/interrupt); only a live `provider:turn_started`
// push sets `pane.activeTurn`. See docs/architecture/invariants.md #22
// and docs/architecture/turn-lifecycle.md §Frontend state shape.
export { Turn } from '../../../bindings/agent-overflow/internal/store/models.js';
