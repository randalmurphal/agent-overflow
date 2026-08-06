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
export const GetThreadDefaults = dispatch('GetThreadDefaults');
// StartTerminal is imported by stores/bindings.ts. The unit (happy-dom)
// project resolves a missing named export leniently, so its absence went
// uncaught there; the browser project's real ESM loader is strict and fails
// the import. Keep the mock complete against everything bindings.ts imports.
export const StartTerminal = dispatch('StartTerminal');
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
export const CloseThreadTerminals = dispatch('CloseThreadTerminals');
export const GetTerminalReplay = dispatch('GetTerminalReplay');
export const ListTerminals = dispatch('ListTerminals');
export const OpenTerminal = dispatch('OpenTerminal');
export const MoveThreadTerminals = dispatch('MoveThreadTerminals');
export const RefreshTerminal = dispatch('RefreshTerminal');
export const ResizeTerminal = dispatch('ResizeTerminal');
export const RestartTerminal = dispatch('RestartTerminal');
export const WriteTerminal = dispatch('WriteTerminal');
// TerminalOpenOptions mirrors the generated class (internal/terminal open args).
// builtinCommands / TerminalSurface construct `new TerminalOpenOptions({ cwd })`;
// the OpenTerminal mock ignores the arg, but the class must exist so both the
// construction and `import { TerminalOpenOptions }` compile against this mock.
export class TerminalOpenOptions {
  cwd: string;
  shell?: string;
  rows?: number;
  cols?: number;
  constructor(s: Partial<TerminalOpenOptions> = {}) {
    this.cwd = s.cwd ?? '';
    this.shell = s.shell;
    this.rows = s.rows;
    this.cols = s.cols;
  }
}

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
export const TerminateCodexBackgroundTerminal = dispatch('TerminateCodexBackgroundTerminal');
export const CleanCodexBackgroundTerminals = dispatch('CleanCodexBackgroundTerminals');

// Live-session context breakdown (Claude's canonical /context read).
export const GetThreadContextUsage = dispatch('GetThreadContextUsage');

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
export const UpdateNewThreadDefaults = dispatch('UpdateNewThreadDefaults');
export const WriteThreadWorkspaceFile = dispatch('WriteThreadWorkspaceFile');
export const SearchThreadMessages = dispatch('SearchThreadMessages');
export const SearchThreadItems = dispatch('SearchThreadItems');
export const GenerateCommitMessage = dispatch('GenerateCommitMessage');

export const GetPayloadPreview = dispatch('GetPayloadPreview');
export const GetPayloadChunk = dispatch('GetPayloadChunk');
export const GetPayloadData = dispatch('GetPayloadData');
export const ListItems = dispatch('ListItems');
export const GetRateLimitsSnapshots = dispatch('GetRateLimitsSnapshots');
export const ListProviderAccounts = dispatch('ListProviderAccounts');
export const LoginProviderAccount = dispatch('LoginProviderAccount');
export const SwitchProviderAccount = dispatch('SwitchProviderAccount');
export const RemoveProviderAccount = dispatch('RemoveProviderAccount');
export const RefreshProviderAccountUsage = dispatch('RefreshProviderAccountUsage');

export const GetSettings = dispatch('GetSettings');
export const UpdateSettings = dispatch('UpdateSettings');
export const SetProviderCustomEnvVar = dispatch('SetProviderCustomEnvVar');
export const DeleteProviderCustomEnvVar = dispatch('DeleteProviderCustomEnvVar');
export const GetProjectWorktreeSetup = dispatch('GetProjectWorktreeSetup');
export const SetProjectWorktreeSetup = dispatch('SetProjectWorktreeSetup');
export const GetThreadWorktreeSetup = dispatch('GetThreadWorktreeSetup');
export const RetryThreadWorktreeSetup = dispatch('RetryThreadWorktreeSetup');
export const Version = dispatch('Version');

// Workflow surface.
export const WorkflowAnswerQuestion = dispatch('WorkflowAnswerQuestion');
export const WorkflowBindThread = dispatch('WorkflowBindThread');
export const WorkflowCancelItem = dispatch('WorkflowCancelItem');
export const WorkflowCompleteTakeover = dispatch('WorkflowCompleteTakeover');
export const WorkflowCreateAutomation = dispatch('WorkflowCreateAutomation');
export const WorkflowCreateItemPR = dispatch('WorkflowCreateItemPR');
export const WorkflowDeleteAutomation = dispatch('WorkflowDeleteAutomation');
export const WorkflowDiscussPR = dispatch('WorkflowDiscussPR');
export const WorkflowDiscardItem = dispatch('WorkflowDiscardItem');
export const WorkflowDiscardPreview = dispatch('WorkflowDiscardPreview');
export const WorkflowDropUnit = dispatch('WorkflowDropUnit');
export const WorkflowFetchPRReviewComments = dispatch('WorkflowFetchPRReviewComments');
export const WorkflowGetEngineState = dispatch('WorkflowGetEngineState');
export const WorkflowGetItem = dispatch('WorkflowGetItem');
export const WorkflowGetJobNotes = dispatch('WorkflowGetJobNotes');
export const WorkflowListAutomations = dispatch('WorkflowListAutomations');
export const WorkflowListDefinitions = dispatch('WorkflowListDefinitions');
export const WorkflowListItemCosts = dispatch('WorkflowListItemCosts');
export const WorkflowListItems = dispatch('WorkflowListItems');
export const WorkflowListUnresolvedItems = dispatch('WorkflowListUnresolvedItems');
export const WorkflowMergeItem = dispatch('WorkflowMergeItem');
export const WorkflowRerunItem = dispatch('WorkflowRerunItem');
export const WorkflowSendPRReviewCommentsToThread = dispatch('WorkflowSendPRReviewCommentsToThread');
export const WorkflowPauseItem = dispatch('WorkflowPauseItem');
export const WorkflowRequestSoftStop = dispatch('WorkflowRequestSoftStop');
export const WorkflowResolveGate = dispatch('WorkflowResolveGate');
export const WorkflowResumeItem = dispatch('WorkflowResumeItem');
export const WorkflowRetryFailedUnits = dispatch('WorkflowRetryFailedUnits');
export const WorkflowRetryUnit = dispatch('WorkflowRetryUnit');
export const WorkflowRunAutomationNow = dispatch('WorkflowRunAutomationNow');
export const WorkflowSetAutomationEnabled = dispatch('WorkflowSetAutomationEnabled');
export const WorkflowSetGlobalPause = dispatch('WorkflowSetGlobalPause');
export const WorkflowSetJobNotes = dispatch('WorkflowSetJobNotes');
export const WorkflowStartRun = dispatch('WorkflowStartRun');
export const WorkflowTakeOverUnit = dispatch('WorkflowTakeOverUnit');
export const WorkflowUnbindThread = dispatch('WorkflowUnbindThread');
export const WorkflowUpdateAutomation = dispatch('WorkflowUpdateAutomation');

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
export const ProbeDevServerURL = dispatch('ProbeDevServerURL');
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
// Composer command-menu sources + the two Codex thread commands the menu
// drives. All four are re-exported from stores/bindings.ts, so the mock has to
// carry them or the browser project's strict ESM loader fails the import.
export const GetClaudeSlashCommands = dispatch('GetClaudeSlashCommands');
export const GetClaudeSkills = dispatch('GetClaudeSkills');
export const GetCodexSkills = dispatch('GetCodexSkills');
export const CompactCodexThread = dispatch('CompactCodexThread');
export const StartCodexReview = dispatch('StartCodexReview');
export const ProbeClaudeAccount = dispatch('ProbeClaudeAccount');
export const RecheckClaudeAccount = dispatch('RecheckClaudeAccount');
export const RecheckCodexAccount = dispatch('RecheckCodexAccount');

export const GetGitStatus = dispatch('GetGitStatus');
export const GetGitStatusFast = dispatch('GetGitStatusFast');
export const GetGitStatusFastForProject = dispatch('GetGitStatusFastForProject');
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
export const GitListBranchesForProject = dispatch('GitListBranchesForProject');
export const GitListWorktrees = dispatch('GitListWorktrees');
export const GitListWorktreesForProject = dispatch('GitListWorktreesForProject');
export const GitCommit = dispatch('GitCommit');
export const GitPush = dispatch('GitPush');
export const GitPull = dispatch('GitPull');
export const GitCheckout = dispatch('GitCheckout');
export const GitCheckoutForProject = dispatch('GitCheckoutForProject');
export const GitCreateBranch = dispatch('GitCreateBranch');
export const GitCreateBranchFrom = dispatch('GitCreateBranchFrom');
export const GitCreatePR = dispatch('GitCreatePR');
export const GitCreateWorktree = dispatch('GitCreateWorktree');
export const GitMaybeFetchRemotes = dispatch('GitMaybeFetchRemotes');
export const GitMaybeFetchRemotesForProject = dispatch('GitMaybeFetchRemotesForProject');
export const GitListBranchPruneCandidates = dispatch('GitListBranchPruneCandidates');
export const GitPruneBranches = dispatch('GitPruneBranches');
export const GitSyncBranch = dispatch('GitSyncBranch');
export const GitSyncBranchForProject = dispatch('GitSyncBranchForProject');
export const GitWorktreeStatus = dispatch('GitWorktreeStatus');
export const GitWorktreeStatusForProject = dispatch('GitWorktreeStatusForProject');
export const PrepareThreadWorktree = dispatch('PrepareThreadWorktree');
export const AttachThreadWorktree = dispatch('AttachThreadWorktree');
export const GitRemoveWorktree = dispatch('GitRemoveWorktree');
export const RemoveOtherWorktree = dispatch('RemoveOtherWorktree');
export const RemoveOtherWorktreeForProject = dispatch('RemoveOtherWorktreeForProject');
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
export const GetChannelState = dispatch('GetChannelState');
export const PostChannelMessage = dispatch('PostChannelMessage');
export const ConcludeDiscussion = dispatch('ConcludeDiscussion');

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
export const DeleteEmptyDraftThread = dispatch('DeleteEmptyDraftThread');
export const SearchWorkspaceFiles = dispatch('SearchWorkspaceFiles');
export const ListChatBarFavorites = dispatch('ListChatBarFavorites');
export const SetChatBarFavorite = dispatch('SetChatBarFavorite');

// Keybindings
export const GetKeybindings = dispatch('GetKeybindings');
export const UpdateKeybindings = dispatch('UpdateKeybindings');
export const ResetKeybindings = dispatch('ResetKeybindings');

// Review pane diffs
export const GetBranchBaseDiff = dispatch('GetBranchBaseDiff');
export const GetWorkspaceCurrentDiff = dispatch('GetWorkspaceCurrentDiff');
export const ListBranchCommits = dispatch('ListBranchCommits');
export const ListRecentCommits = dispatch('ListRecentCommits');
export const GetCommitDiff = dispatch('GetCommitDiff');
export const ListThreadEditDiffs = dispatch('ListThreadEditDiffs');
export const GetTurnEditsDiff = dispatch('GetTurnEditsDiff');
export const GetDiffContextLines = dispatch('GetDiffContextLines');
export const VerifyEditDiffs = dispatch('VerifyEditDiffs');
export const HighlightClassNames = dispatch('HighlightClassNames');
export const HighlightSchemaVersion = dispatch('HighlightSchemaVersion');
export const HighlightCode = dispatch('HighlightCode');
export const HighlightPatch = dispatch('HighlightPatch');
export const HighlightPatchWithContext = dispatch('HighlightPatchWithContext');
export const ForkThreadFromMessage = dispatch('ForkThreadFromMessage');
export const RevertConversationAndResendMessage = dispatch('RevertConversationAndResendMessage');

// Thread runtime mode
export const GetThreadRuntimeMode = dispatch('GetThreadRuntimeMode');

// Dev-only UI render tracing
export const AppendUIRenderTraceBatch = dispatch('AppendUIRenderTraceBatch');
export const BookmarkUIRenderTrace = dispatch('BookmarkUIRenderTrace');
export const GetUIRenderTracePath = dispatch('GetUIRenderTracePath');

// Always-on frontend runtime-error log
export const ReportFrontendErrorBatch = dispatch('ReportFrontendErrorBatch');

// PR-based thread creation
export const CreateThreadFromPR = dispatch('CreateThreadFromPR');

// Projects (sidebar)
export const ListProjects = dispatch('ListProjects');
export const CreateProject = dispatch('CreateProject');
export const RenameProject = dispatch('RenameProject');
export const DeleteProject = dispatch('DeleteProject');
export const ProjectDeletionPreview = dispatch('ProjectDeletionPreview');
export const ArchiveProject = dispatch('ArchiveProject');
export const UnarchiveProject = dispatch('UnarchiveProject');

// Directory browser (Add Project modal)
export const BrowseDirectory = dispatch('BrowseDirectory');

// Turn lifecycle rehydration (thread-switch reads the most recent settled turn)
export const ListRecentTurns = dispatch('ListRecentTurns');

// Windowed history + thread-wide aggregates. Active panes load bounded
// slices via ListThreadSliceAround and page by item-coordinate cursors;
// ListRecentThreadItems / turn pagers remain legacy surfaces.
export const ListRecentThreadItems = dispatch('ListRecentThreadItems');
export const ListThreadSliceAround = dispatch('ListThreadSliceAround');
export const ListItemsBeforeCursor = dispatch('ListItemsBeforeCursor');
export const ListItemsBeforeTurn = dispatch('ListItemsBeforeTurn');
export const ListItemsAfterCursor = dispatch('ListItemsAfterCursor');
export const ListItemsAfterTurn = dispatch('ListItemsAfterTurn');
export const ListSubagentDescendants = dispatch('ListSubagentDescendants');
export const ListThreadProposedPlans = dispatch('ListThreadProposedPlans');
export const ListProposedPlanComments = dispatch('ListProposedPlanComments');
export const CreateProposedPlanComment = dispatch('CreateProposedPlanComment');
export const UpdateProposedPlanComment = dispatch('UpdateProposedPlanComment');
export const DeleteProposedPlanComment = dispatch('DeleteProposedPlanComment');
export const ListDiffReviewComments = dispatch('ListDiffReviewComments');
export const CreateDiffReviewComment = dispatch('CreateDiffReviewComment');
export const UpdateDiffReviewComment = dispatch('UpdateDiffReviewComment');
export const DeleteDiffReviewComment = dispatch('DeleteDiffReviewComment');
export const MarkDiffReviewCommentsSent = dispatch('MarkDiffReviewCommentsSent');
export const SendDiffReviewComments = dispatch('SendDiffReviewComments');
export const GetPRDetail = dispatch('GetPRDetail');
export const GetPRDiff = dispatch('GetPRDiff');
export const ListPRCommits = dispatch('ListPRCommits');
export const GetPRCommitDiff = dispatch('GetPRCommitDiff');
export const GetPRMergeConflicts = dispatch('GetPRMergeConflicts');
export const GetMergeConflictFile = dispatch('GetMergeConflictFile');
export const GetPRCIJobs = dispatch('GetPRCIJobs');
export const GetPRCIJobLog = dispatch('GetPRCIJobLog');
export const SavePRCIJobLog = dispatch('SavePRCIJobLog');
export const ListPRReviewThreads = dispatch('ListPRReviewThreads');
export const SubmitPRReview = dispatch('SubmitPRReview');
export const ReplyToPRThread = dispatch('ReplyToPRThread');
export const SubscribePRUpdates = dispatch('SubscribePRUpdates');
export const UnsubscribePRUpdates = dispatch('UnsubscribePRUpdates');
export const SetPRUpdatesActive = dispatch('SetPRUpdatesActive');
export const CountRunningBackgroundTasks = dispatch('CountRunningBackgroundTasks');
export const ListLiveBackgroundTasks = dispatch('ListLiveBackgroundTasks');
export const GetThreadItem = dispatch('GetThreadItem');

// Usage accounting
export const GetUsageStats = dispatch('GetUsageStats');
export const GetCodexAccountUsage = dispatch('GetCodexAccountUsage');

// Per-client UI view state (appStorage)
export const GetUIState = dispatch('GetUIState');
export const SetUIState = dispatch('SetUIState');
export const DeleteUIState = dispatch('DeleteUIState');

// MCP (provider-native state)
export const ListThreadMcpServers = dispatch('ListThreadMcpServers');
export const ListWorkspaceMcpServers = dispatch('ListWorkspaceMcpServers');
export const SetThreadMcpServerEnabled = dispatch('SetThreadMcpServerEnabled');
export const SetWorkspaceMcpServerEnabled = dispatch('SetWorkspaceMcpServerEnabled');
export const ReconnectMcpServer = dispatch('ReconnectMcpServer');
export const GetMcpServerStatus = dispatch('GetMcpServerStatus');
export const RefreshMcpServerStatus = dispatch('RefreshMcpServerStatus');
export const TriggerMcpAuth = dispatch('TriggerMcpAuth');

// Send-queue (per-thread mid-turn queue; backend-owned). Tests that
// exercise mid-turn submits must mock RegisterQueueItem to return the
// wire item AND seed the local store via replaceQueueForThread, so the
// backend echo is faithfully simulated.
export const RegisterQueueItem = dispatch('RegisterQueueItem');
export const GetQueueState = dispatch('GetQueueState');

// Browser-project loader sync. The unit (happy-dom) project resolves a missing
// named export to `undefined` (lenient), so these never broke unit tests even
// though the real bindings export them. The `browser` vitest project uses a
// real ESM loader (strict named exports) and fails any component graph that
// transitively imports one of these. They are RPCs no current test exercises;
// dispatch() makes them reject loudly if a test ever does hit them unmocked.
// Keep this block a superset of bindings/agent-overflow/app.ts so it can't
// drift again (see scripts diffing real-vs-mock exports).
export const CheckForUpdate = dispatch('CheckForUpdate');
export const DownloadUpdate = dispatch('DownloadUpdate');
export const RestartToUpdate = dispatch('RestartToUpdate');
export const ListReleases = dispatch('ListReleases');
export const ReconfigureObservability = dispatch('ReconfigureObservability');
export const ProbeCodexAccount = dispatch('ProbeCodexAccount');
export const SavePayloadToFile = dispatch('SavePayloadToFile');
export const GetWorkingTreeDiff = dispatch('GetWorkingTreeDiff');
export const GitStageAll = dispatch('GitStageAll');
export const ListArchivedThreads = dispatch('ListArchivedThreads');
export const PinThread = dispatch('PinThread');
export const UnpinThread = dispatch('UnpinThread');
export const UpdateProjectSortPositions = dispatch('UpdateProjectSortPositions');
export const ProviderTerminalAttach = dispatch('ProviderTerminalAttach');
export const ProviderTerminalDetach = dispatch('ProviderTerminalDetach');
export const ProviderTerminalInput = dispatch('ProviderTerminalInput');
export const ProviderTerminalRefresh = dispatch('ProviderTerminalRefresh');
export const ProviderTerminalReplay = dispatch('ProviderTerminalReplay');
export const ProviderTerminalResize = dispatch('ProviderTerminalResize');
export const ProviderTerminalSetControl = dispatch('ProviderTerminalSetControl');
export const NotificationActivated = dispatch('NotificationActivated');
export const WorkflowAgentGetNotes = dispatch('WorkflowAgentGetNotes');
export const WorkflowAgentListRuns = dispatch('WorkflowAgentListRuns');
export const WorkflowAgentRunOutput = dispatch('WorkflowAgentRunOutput');
export const WorkflowAgentRunStatus = dispatch('WorkflowAgentRunStatus');
export const WorkflowAgentSchedule = dispatch('WorkflowAgentSchedule');
export const WorkflowAgentSetNotes = dispatch('WorkflowAgentSetNotes');
export const WorkflowAgentStartRun = dispatch('WorkflowAgentStartRun');
