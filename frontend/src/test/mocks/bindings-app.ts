// Fake for `bindings/agent-overflow/app.js` — the auto-generated Wails
// bindings that the real app imports via `lib/stores/bindings.ts`.
//
// Tests call `setBindingMock('ListItems', impl)` to stub specific RPC methods.
// Anything not explicitly mocked rejects with a clear error so an untested
// code path can't silently no-op.

import { vi, type Mock } from 'vitest';

type MockedFn = Mock<(...args: unknown[]) => unknown>;

const mocks: Map<string, MockedFn> = new Map();

/**
 * Internal export of the mocks registry. Imported by the wailsio-runtime
 * mock so Call.ByName dispatches to the same per-name handlers that
 * setBindingMock installs. Not part of the public test surface — prefer
 * setBindingMock / getBindingMock in tests.
 */
export const __bindingMocksInternal = mocks;

/**
 * Install (or replace) a mock implementation for a binding.
 * Returns the underlying vi.fn so tests can inspect call args.
 */
export function setBindingMock(
  name: string,
  impl: (...args: never[]) => unknown,
): MockedFn {
  const fn = vi.fn(impl as (...args: unknown[]) => unknown);
  mocks.set(name, fn);
  return fn;
}

/**
 * Direct read for assertions (call counts, args).
 */
export function getBindingMock(name: string): MockedFn | undefined {
  return mocks.get(name);
}

/**
 * Reset every binding between tests.
 */
export function resetBindingMocks(): void {
  mocks.clear();
}

function dispatch(name: string) {
  return (...args: unknown[]) => {
    const fn = mocks.get(name);
    if (!fn) {
      throw new Error(
        `Binding ${name} called without a mock. Install one via setBindingMock('${name}', impl) in the test.`,
      );
    }
    return fn(...args);
  };
}

// Every binding re-exported from `lib/stores/bindings.ts`.
// Keep this list in sync with that file.
export const ArchiveThread = dispatch('ArchiveThread');
export const UnarchiveThread = dispatch('UnarchiveThread');
export const CreateThread = dispatch('CreateThread');
export const DeleteThread = dispatch('DeleteThread');
export const ForkThread = dispatch('ForkThread');
export const GetThread = dispatch('GetThread');
export const ListThreads = dispatch('ListThreads');
export const RenameThread = dispatch('RenameThread');
export const SwitchThread = dispatch('SwitchThread');

// Terminal operations (ThreadTerminalDrawer, TerminalBody, etc).
export const CloseTerminal = dispatch('CloseTerminal');
export const GetTerminalReplay = dispatch('GetTerminalReplay');
export const ListTerminals = dispatch('ListTerminals');
export const OpenTerminal = dispatch('OpenTerminal');
export const ResizeTerminal = dispatch('ResizeTerminal');
export const RestartTerminal = dispatch('RestartTerminal');
export const WriteTerminal = dispatch('WriteTerminal');

export const StartSession = dispatch('StartSession');
export const StopSession = dispatch('StopSession');
export const ReconnectSession = dispatch('ReconnectSession');
export const SendMessage = dispatch('SendMessage');
export const InterruptTurn = dispatch('InterruptTurn');
export const RespondToApproval = dispatch('RespondToApproval');

// Thread-scoped model / workspace / message-search / commit-message bindings
// (previously hand-wrapped with Call.ByName; now re-exported through the
// generated app.js). Tests stub these through setBindingMock by method name.
export const UpdateThreadModel = dispatch('UpdateThreadModel');
export const UpdateThreadProvider = dispatch('UpdateThreadProvider');
export const UpdateThreadMode = dispatch('UpdateThreadMode');
export const UpdateThreadReasoningEffort = dispatch('UpdateThreadReasoningEffort');
export const UpdateThreadFastMode = dispatch('UpdateThreadFastMode');
export const UpdateThreadContextWindow = dispatch('UpdateThreadContextWindow');
export const UpdateThreadRuntimeMode = dispatch('UpdateThreadRuntimeMode');
export const UpdateThreadBranch = dispatch('UpdateThreadBranch');
export const UpdateThreadWorkspace = dispatch('UpdateThreadWorkspace');
export const WriteThreadWorkspaceFile = dispatch('WriteThreadWorkspaceFile');
export const SearchThreadMessages = dispatch('SearchThreadMessages');
export const GenerateCommitMessage = dispatch('GenerateCommitMessage');

export const GetPayloadPreview = dispatch('GetPayloadPreview');
export const GetPayloadData = dispatch('GetPayloadData');
export const ListItems = dispatch('ListItems');
export const ListPayloadMetas = dispatch('ListPayloadMetas');

export const GetSettings = dispatch('GetSettings');
export const UpdateSettings = dispatch('UpdateSettings');

export const GetProviderStatuses = dispatch('GetProviderStatuses');
export const GetModelsForProvider = dispatch('GetModelsForProvider');
export const ProbeClaudeAccount = dispatch('ProbeClaudeAccount');

export const GetGitStatus = dispatch('GetGitStatus');
export const GetWorkingTreeDiff = dispatch('GetWorkingTreeDiff');
export const GitListBranches = dispatch('GitListBranches');
export const GitListWorktrees = dispatch('GitListWorktrees');
export const GitCommit = dispatch('GitCommit');
export const GitPush = dispatch('GitPush');
export const GitPull = dispatch('GitPull');
export const GitCheckout = dispatch('GitCheckout');
export const GitCreateBranch = dispatch('GitCreateBranch');
export const GitCreatePR = dispatch('GitCreatePR');
export const GitCreateWorktree = dispatch('GitCreateWorktree');
export const GitRemoveWorktree = dispatch('GitRemoveWorktree');

export const ListDiscussions = dispatch('ListDiscussions');
export const GetDiscussion = dispatch('GetDiscussion');
export const CreateDiscussion = dispatch('CreateDiscussion');
export const UpdateDiscussion = dispatch('UpdateDiscussion');
export const DeleteDiscussion = dispatch('DeleteDiscussion');
export const StartDiscussion = dispatch('StartDiscussion');
export const GetChannelMessages = dispatch('GetChannelMessages');
export const PostChannelMessage = dispatch('PostChannelMessage');

export const ChooseDesignOption = dispatch('ChooseDesignOption');
export const ListDesignArtifacts = dispatch('ListDesignArtifacts');
export const GetDesignArtifactHTML = dispatch('GetDesignArtifactHTML');

// Composer enhancements
export const UploadAttachment = dispatch('UploadAttachment');
export const ListAttachments = dispatch('ListAttachments');
export const DeleteAttachment = dispatch('DeleteAttachment');
export const GetAttachmentData = dispatch('GetAttachmentData');
export const SaveDraft = dispatch('SaveDraft');
export const GetDraft = dispatch('GetDraft');
export const ClearDraft = dispatch('ClearDraft');
export const SearchWorkspaceFiles = dispatch('SearchWorkspaceFiles');

// Keybindings
export const GetKeybindings = dispatch('GetKeybindings');
export const UpdateKeybindings = dispatch('UpdateKeybindings');
export const ResetKeybindings = dispatch('ResetKeybindings');

// Checkpoints
export const GetTurnDiff = dispatch('GetTurnDiff');
export const GetCheckpointToWorktreeDiff = dispatch('GetCheckpointToWorktreeDiff');
export const RevertToTurn = dispatch('RevertToTurn');
export const ListThreadCheckpoints = dispatch('ListThreadCheckpoints');

// Thread runtime mode
export const GetThreadRuntimeMode = dispatch('GetThreadRuntimeMode');
export const SetThreadRuntimeMode = dispatch('SetThreadRuntimeMode');

// Slash commands (Claude-only)
export const GetThreadSlashCommands = dispatch('GetThreadSlashCommands');

// PR-based thread creation
export const CreateThreadFromPR = dispatch('CreateThreadFromPR');

// Projects (sidebar)
export const ListProjects = dispatch('ListProjects');
export const CreateProject = dispatch('CreateProject');
export const RenameProject = dispatch('RenameProject');
export const DeleteProject = dispatch('DeleteProject');
export const ArchiveProject = dispatch('ArchiveProject');
export const UnarchiveProject = dispatch('UnarchiveProject');

// Directory browser (Add Project modal)
export const BrowseDirectory = dispatch('BrowseDirectory');

// Turn lifecycle rehydration (thread-switch reads the most recent settled turn)
export const ListRecentTurns = dispatch('ListRecentTurns');
