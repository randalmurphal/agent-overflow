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
export const MarkThreadRead = dispatch('MarkThreadRead');
export const MarkThreadUnread = dispatch('MarkThreadUnread');
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
export const AutoResumeThread = dispatch('AutoResumeThread');
export const StopSession = dispatch('StopSession');
export const ReconnectSession = dispatch('ReconnectSession');
export const SendMessage = dispatch('SendMessage');
export const SendMessageWithOptions = dispatch('SendMessageWithOptions');
export const SteerMessageWithOptions = dispatch('SteerMessageWithOptions');
export const InterruptTurn = dispatch('InterruptTurn');
export const InterruptAndRevertIfClean = dispatch('InterruptAndRevertIfClean');
export const GetThreadLiveState = dispatch('GetThreadLiveState');
export const ListPendingInteractiveRequests = dispatch('ListPendingInteractiveRequests');
export const RespondToApproval = dispatch('RespondToApproval');
export const RespondToUserInput = dispatch('RespondToUserInput');
export const SendPlanRevisionComments = dispatch('SendPlanRevisionComments');

// Background tasks (per-item + thread-wide stop primitives)
export const StopClaudeTask = dispatch('StopClaudeTask');
export const CleanCodexBackgroundTerminals = dispatch('CleanCodexBackgroundTerminals');

// Thread-scoped model / workspace / message-search / commit-message bindings
// (previously hand-wrapped with Call.ByName; now re-exported through the
// generated app.js). Tests stub these through setBindingMock by method name.
export const UpdateThreadModel = dispatch('UpdateThreadModel');
export const UpdateThreadProvider = dispatch('UpdateThreadProvider');
export const UpdateThreadModelSelection = dispatch('UpdateThreadModelSelection');
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
export const GetPayloadChunk = dispatch('GetPayloadChunk');
export const GetPayloadData = dispatch('GetPayloadData');
export const ListItems = dispatch('ListItems');
export const ListPayloadMetas = dispatch('ListPayloadMetas');

export const GetSettings = dispatch('GetSettings');
export const UpdateSettings = dispatch('UpdateSettings');

// Per-provider/model context window + auto-compact thresholds.
// GetContextSettings hydrates the form; the two updates persist either
// the model default (UpdateContextSettingsProfile) or a thread-scoped
// override (UpdateThreadContextSettings).
export const GetContextSettings = dispatch('GetContextSettings');
export const UpdateContextSettingsProfile = dispatch('UpdateContextSettingsProfile');
export const UpdateThreadContextSettings = dispatch('UpdateThreadContextSettings');
export const GetNetworkSettings = dispatch('GetNetworkSettings');
export const SetNetworkSettings = dispatch('SetNetworkSettings');

// WSL distro switcher (Settings → Network → WSL Distro section).
// IsWSL gates whether the section renders at all; the other three
// drive the dropdown + persist on change.
export const IsWSL = dispatch('IsWSL');
export const ListWSLDistros = dispatch('ListWSLDistros');
export const GetWSLDistroPreference = dispatch('GetWSLDistroPreference');
export const SetWSLDistroPreference = dispatch('SetWSLDistroPreference');
// WSLDistro mirrors the generated Distro class from
// internal/wsllauncher/models — tests pass plain object literals to
// the mocks, but the class shape is needed so `import type { WSLDistro }`
// in components compiles against the test mock.
export class WSLDistro {
  name: string;
  default: boolean;
  version: number;
  state: string;
  constructor(d: Partial<WSLDistro> = {}) {
    this.name = d.name ?? '';
    this.default = d.default ?? false;
    this.version = d.version ?? 2;
    this.state = d.state ?? '';
  }
}

// Host app launchers. OpenInEditor and OpenExternalURL are user-facing
// launchers; the catalog + persistence pair powers the settings picker. The
// EditorInfo / EditorSettings classes are re-exported from
// internal/settings/models.js + models.js (not aliased), so tests use
// the real generated classes — only the RPC functions need stubs.
export const OpenInEditor = dispatch('OpenInEditor');
export const OpenExternalURL = dispatch('OpenExternalURL');
export const ListAvailableEditors = dispatch('ListAvailableEditors');
export const GetEditorSettings = dispatch('GetEditorSettings');
export const SetEditorSettings = dispatch('SetEditorSettings');
export const ListRemoteEndpoints = dispatch('ListRemoteEndpoints');
export const AddRemoteEndpoint = dispatch('AddRemoteEndpoint');
export const UpdateRemoteEndpoint = dispatch('UpdateRemoteEndpoint');
export const DeleteRemoteEndpoint = dispatch('DeleteRemoteEndpoint');
export const TouchRemoteEndpoint = dispatch('TouchRemoteEndpoint');
export const GetRemoteEndpointToken = dispatch('GetRemoteEndpointToken');
// RemoteEndpoint mirrors the Phase F generated class; tests stub the
// list/add/update calls and read fields off the returned objects.
export class RemoteEndpoint {
  id: string;
  name: string;
  url: string;
  token: string;
  lastUsedAt?: number;
  constructor(s: Partial<RemoteEndpoint> = {}) {
    this.id = s.id ?? '';
    this.name = s.name ?? '';
    this.url = s.url ?? '';
    this.token = s.token ?? '';
    this.lastUsedAt = s.lastUsedAt;
  }
}
// NetworkSettings is a class re-exported alongside the bindings; the
// mock just needs a constructor-compatible stand-in so test code that
// builds `new NetworkSettings({ bindAll })` doesn't try to load the
// real generated module. Real instantiation (createFrom) is exercised
// in component tests that run against the live binding.
export class NetworkSettings {
  bindAll: boolean;
  url: string;
  token: string;
  insecure: boolean;
  constructor(s: Partial<NetworkSettings> = {}) {
    this.bindAll = s.bindAll ?? false;
    this.url = s.url ?? '';
    this.token = s.token ?? '';
    this.insecure = s.insecure ?? false;
  }
}

export const GetProviderStatuses = dispatch('GetProviderStatuses');
export const GetModelsForProvider = dispatch('GetModelsForProvider');
export const ProbeClaudeAccount = dispatch('ProbeClaudeAccount');
export const RecheckClaudeAccount = dispatch('RecheckClaudeAccount');
export const RecheckCodexAccount = dispatch('RecheckCodexAccount');

export const GetGitStatus = dispatch('GetGitStatus');
export const GitStatusSubscribe = dispatch('GitStatusSubscribe');
export const GitStatusUnsubscribe = dispatch('GitStatusUnsubscribe');
// Class re-export mirroring the generated GitStatusSubscriptionResult.
// Tests stub GitStatusSubscribe to return a plain object literal; we
// only need the type to satisfy `import type` consumers.
export class GitStatusSubscriptionResult {
  id: string;
  status: import('../../lib/types/git').GitStatus;
  constructor(s: Partial<GitStatusSubscriptionResult> = {}) {
    this.id = s.id ?? '';
    this.status = s.status ?? ({} as import('../../lib/types/git').GitStatus);
  }
}
export const GitListBranches = dispatch('GitListBranches');
export const GitListWorktrees = dispatch('GitListWorktrees');
export const GitCommit = dispatch('GitCommit');
export const GitPush = dispatch('GitPush');
export const GitPull = dispatch('GitPull');
export const GitCheckout = dispatch('GitCheckout');
export const GitCreateBranch = dispatch('GitCreateBranch');
export const GitCreateBranchFrom = dispatch('GitCreateBranchFrom');
export const GitCreatePR = dispatch('GitCreatePR');
export const GitCreateWorktree = dispatch('GitCreateWorktree');
export const GitMaybeFetchRemotes = dispatch('GitMaybeFetchRemotes');
export const GitPruneRemotes = dispatch('GitPruneRemotes');
export const GitSyncBranch = dispatch('GitSyncBranch');
export const GitWorktreeStatus = dispatch('GitWorktreeStatus');
export const PrepareThreadWorktree = dispatch('PrepareThreadWorktree');
export const AttachThreadWorktree = dispatch('AttachThreadWorktree');
export const GitRemoveWorktree = dispatch('GitRemoveWorktree');
export const RemoveOtherWorktree = dispatch('RemoveOtherWorktree');
// WorktreeStatus mirrors the generated class; tests stub the binding
// to return plain object literals, so the class shape is only here so
// `import type { WorktreeStatus }` resolves through the mock module.
export class WorktreeStatus {
  path: string;
  branch: string;
  dirty: boolean;
  uncommittedCount: number;
  unpushedCommits: number;
  hasUpstream: boolean;
  attachedThreads: number;
  constructor(s: Partial<WorktreeStatus> = {}) {
    this.path = s.path ?? '';
    this.branch = s.branch ?? '';
    this.dirty = s.dirty ?? false;
    this.uncommittedCount = s.uncommittedCount ?? 0;
    this.unpushedCommits = s.unpushedCommits ?? 0;
    this.hasUpstream = s.hasUpstream ?? false;
    this.attachedThreads = s.attachedThreads ?? 0;
  }
}

export const ListDiscussions = dispatch('ListDiscussions');
export const ListDiscussionsForThread = dispatch('ListDiscussionsForThread');
export const GetDiscussion = dispatch('GetDiscussion');
export const CreateDiscussion = dispatch('CreateDiscussion');
export const UpdateDiscussion = dispatch('UpdateDiscussion');
export const DeleteDiscussion = dispatch('DeleteDiscussion');
export const StartDiscussion = dispatch('StartDiscussion');
export const StartDiscussionByID = dispatch('StartDiscussionByID');
export const GetChannelMessages = dispatch('GetChannelMessages');
export const PostChannelMessage = dispatch('PostChannelMessage');

export const ListDesignOptions = dispatch('ListDesignOptions');
export const LatestDesignOptionSet = dispatch('LatestDesignOptionSet');
export const DismissDesignOptionSet = dispatch('DismissDesignOptionSet');
export const EnsureDesignWorkdir = dispatch('EnsureDesignWorkdir');
export const GetDesignWorkdirInfo = dispatch('GetDesignWorkdirInfo');
export const IngestDiagnosticBatch = dispatch('IngestDiagnosticBatch');

// Composer enhancements
export const UploadAttachment = dispatch('UploadAttachment');
export const ListAttachments = dispatch('ListAttachments');
export const DeleteAttachment = dispatch('DeleteAttachment');
export const GetAttachmentData = dispatch('GetAttachmentData');
export const GetAttachmentThumbnail = dispatch('GetAttachmentThumbnail');
export const SaveDraft = dispatch('SaveDraft');
export const GetDraft = dispatch('GetDraft');
export const ClearDraft = dispatch('ClearDraft');
export const SearchWorkspaceFiles = dispatch('SearchWorkspaceFiles');
export const ListChatBarFavorites = dispatch('ListChatBarFavorites');
export const SetChatBarFavorite = dispatch('SetChatBarFavorite');

// Keybindings
export const GetKeybindings = dispatch('GetKeybindings');
export const UpdateKeybindings = dispatch('UpdateKeybindings');
export const ResetKeybindings = dispatch('ResetKeybindings');

// Checkpoints
export const GetMessageCheckpointDiff = dispatch('GetMessageCheckpointDiff');
export const GetMessageCheckpointRevertDiff = dispatch('GetMessageCheckpointRevertDiff');
export const GetSessionAgentDiff = dispatch('GetSessionAgentDiff');
export const GetWorkspaceCurrentDiff = dispatch('GetWorkspaceCurrentDiff');
export const RevertToMessageCheckpoint = dispatch('RevertToMessageCheckpoint');
export const ForkThreadFromMessage = dispatch('ForkThreadFromMessage');
export const ListThreadCheckpoints = dispatch('ListThreadCheckpoints');

// Thread runtime mode
export const GetThreadRuntimeMode = dispatch('GetThreadRuntimeMode');
export const SetThreadRuntimeMode = dispatch('SetThreadRuntimeMode');

// Dev-only UI render tracing
export const AppendUIRenderTraceBatch = dispatch('AppendUIRenderTraceBatch');
export const GetUIRenderTracePath = dispatch('GetUIRenderTracePath');

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

// Windowed history + thread-wide aggregates. The frontend reads the
// tail of a thread via ListRecentThreadItems on switch and pages
// backward via ListItemsBeforeTurn; the three thread-wide bindings
// back dedicated sidebar / tray surfaces that need the full
// thread regardless of the timeline window.
export const ListRecentThreadItems = dispatch('ListRecentThreadItems');
export const ListThreadSliceAround = dispatch('ListThreadSliceAround');
export const ListItemsBeforeTurn = dispatch('ListItemsBeforeTurn');
export const ListThreadProposedPlans = dispatch('ListThreadProposedPlans');
export const ListProposedPlanComments = dispatch('ListProposedPlanComments');
export const CreateProposedPlanComment = dispatch('CreateProposedPlanComment');
export const UpdateProposedPlanComment = dispatch('UpdateProposedPlanComment');
export const DeleteProposedPlanComment = dispatch('DeleteProposedPlanComment');
export const ListDiffReviewComments = dispatch('ListDiffReviewComments');
export const CreateDiffReviewComment = dispatch('CreateDiffReviewComment');
export const UpdateDiffReviewComment = dispatch('UpdateDiffReviewComment');
export const DeleteDiffReviewComment = dispatch('DeleteDiffReviewComment');
export const SendDiffReviewComments = dispatch('SendDiffReviewComments');
export const ListLiveBackgroundTasks = dispatch('ListLiveBackgroundTasks');
export const GetThreadItem = dispatch('GetThreadItem');

// Send-queue (per-thread mid-turn queue; backend-owned). Tests that
// exercise mid-turn submits must mock RegisterQueueItem to return the
// wire item AND seed the local store via replaceQueueForThread, so the
// backend echo is faithfully simulated.
export const RegisterQueueItem = dispatch('RegisterQueueItem');
export const GetQueueState = dispatch('GetQueueState');
