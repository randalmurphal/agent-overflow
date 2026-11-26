// Fake for `bindings/agent-overflow/app.js` — the auto-generated Wails
// bindings that the real app imports via `lib/stores/bindings.ts`.
//
// Tests call `setBindingMock('GetThread', impl)` to stub specific RPC methods.
// A call to anything not mocked, except the passive loads below, throws, and
// the reset registry fails the test that made it, even when the code under
// test catches the error. A test that needs a binding absent installs a mock
// that rejects.

import { vi, type Mock } from 'vitest';
import { RESET_REGISTRY_KEY } from '../config/testResets';
import type { ResetRegistry } from '../resetRegistry';

type MockedFn = Mock<(...args: unknown[]) => unknown>;

const mocks: Map<string, MockedFn> = new Map();

const registry = (globalThis as { [key: symbol]: ResetRegistry | undefined })[Symbol.for(RESET_REGISTRY_KEY)];
if (registry === undefined) {
  throw new Error('bindings-app.ts loaded without the reset registry: the setup file must import src/test/resetRegistry.ts first');
}

/**
 * Records a call to a binding with no mock installed, which fails the test
 * that made it, and returns the error the call fails with.
 */
export function unansweredBinding(name: string): Error {
  registry!.unansweredBinding(name);
  return new Error(
    `Binding ${name} called without a mock. Install one via setBindingMock('${name}', impl) in the test.`,
  );
}

/**
 * The mock that answers `name`: the one a test installed, or the passive
 * load's default, whose spy is made on first use. Undefined when neither
 * exists. The wailsio-runtime mock dispatches through it too.
 */
export function bindingMockFor(name: string): MockedFn | undefined {
  const installed = mocks.get(name);
  if (installed !== undefined) return installed;
  if (!Object.hasOwn(PASSIVE_LOADS, name)) return undefined;
  return setBindingMock(name, PASSIVE_LOADS[name]);
}

/** Every name a mock answers or a passive load would answer. */
export function bindingMockNames(): string[] {
  return [...new Set([...mocks.keys(), ...Object.keys(PASSIVE_LOADS)])];
}

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
 * Mocks an Open*Diff binding with a function that answers the whole patch
 * text. The mock returns it as one final chunk, so the caller holds no
 * handle and never reads. Returns the vi.fn that receives the Open call's
 * arguments.
 */
export function setReviewDiffMock(
  name:
    | 'OpenWorkspaceDiff'
    | 'OpenCommitFileDiff'
    | 'OpenBranchBaseDiff'
    | 'OpenCommitDiff'
    | 'OpenPRDiff'
    | 'OpenPRCommitDiff'
    | 'OpenEditDiff'
    | 'OpenTurnEditsDiff',
  impl: (...args: never[]) => string | Promise<string>,
  opts: { headSha?: string; payloadIds?: string[] } = {},
): MockedFn {
  return setBindingMock(name, async (...args: never[]) => {
    const data = String((await impl(...args)) ?? '');
    return {
      id: '',
      chunk: { data, offset: 0, nextOffset: new TextEncoder().encode(data).length, eof: true },
      headSha: opts.headSha ?? '',
      payloadIds: opts.payloadIds ?? [],
    };
  });
}

/** GetAttachmentThumbnail's wire shape (app.AttachmentThumbnail). */
export interface AttachmentThumbnailReply {
  data: string;
  mimeType: string;
  /** The original image's pixel size; 0 when its header does not read. */
  width: number;
  height: number;
}

/**
 * Mocks GetAttachmentThumbnail with the whole wire shape. `impl` answers the
 * fields a test cares about; the rest default to a PNG thumbnail of an
 * original whose size is unknown.
 */
export function setAttachmentThumbnailMock(
  impl: (threadId: string, attachmentId: string) => Partial<AttachmentThumbnailReply> | Promise<Partial<AttachmentThumbnailReply>> = () => ({}),
): MockedFn {
  return setBindingMock('GetAttachmentThumbnail', async (threadId: string, attachmentId: string): Promise<AttachmentThumbnailReply> => ({
    data: 'iVBORw0KGgo=',
    mimeType: 'image/png',
    width: 0,
    height: 0,
    ...(await impl(threadId, attachmentId)),
  }));
}

/**
 * Direct read for assertions (call counts, args).
 */
export function getBindingMock(name: string): MockedFn | undefined {
  return bindingMockFor(name);
}

/**
 * Reset every binding between tests.
 */
export function resetBindingMocks(): void {
  mocks.clear();
}

// Loads a rendered surface or a store starts on its own, with no user
// action, that most tests never mean to exercise. Each answers as a backend
// with nothing recorded, so a test need not stub it and its store does not
// log a failed load on every test. A test that cares installs its own
// answer with setBindingMock, which holds until the next reset.
let gitStatusSubscriptions = 0;
let browserStates = 0;
/** The days from `from` to `to`, YYYY-MM-DD, both included. */
function daysFromTo(from: string, to: string): string[] {
  const out: string[] = [];
  for (let ms = Date.parse(`${from}T00:00:00Z`); ms <= Date.parse(`${to}T00:00:00Z`); ms += 86_400_000) {
    out.push(new Date(ms).toISOString().slice(0, 10));
  }
  return out;
}
const PASSIVE_LOADS: Record<string, (...args: never[]) => unknown> = {
  // A computer's sign-in state and accounts (stores/computerAccounts). With
  // no sign-in running the backend answers idle for the provider asked
  // about (app_provider_account_bindings.go GetProviderLoginState).
  GetProviderLoginState: async (provider: string) => ({ provider, phase: 'idle' }),
  ListProviderAccounts: async () => [],
  // The Issues settings pages (stores/trackerConnections,
  // stores/trackerProjectBoards). A computer with no connection and a
  // project with no board answer empty lists (app_tracker.go
  // TrackerConnections, app_tracker_boards.go ProjectTrackerBoards).
  TrackerConnections: async () => [],
  ProjectTrackerBoards: async () => [],
  // A project's tracker setup, read by a ticket of the project and the
  // Status groups page (stores/trackerSetup): a project with no setup file
  // answers no revision and every section empty, and one with no board no
  // site (app_tracker_setup.go).
  ProjectTrackerSetup: async () => ({ revision: '', setup: { status_groups: [], ticket_pins: [], start_work: [] } }),
  ProjectTrackerStatuses: async () => [],
  // Custom notification sounds (stores/sounds).
  GetSoundFiles: async () => ({ dir: '/tmp/sounds', sounds: [], warnings: [] }),
  // Rate limit snapshots read on every hello (stores/eventsRateLimits).
  GetRateLimitsSnapshots: async () => [],
  // The boards the Issues pane's picker lists, read when the pane mounts
  // (stores/trackerBoardChoices): none for a computer whose projects list
  // no board (store ListTrackerBoardChoices).
  TrackerBoardChoices: async () => [],
  // Whether a Jira ticket's site has planning poker, asked when the ticket
  // opens (stores/trackerTicket askTrackerPokerSupport): a site whose
  // add-on listing names no poker answers false (app_tracker_poker.go).
  TrackerPokerSupport: async () => ({ available: false }),
  // A week of the Time screen, read when it shows (stores/trackerTime): no
  // worklogs, and every day unread, as a computer that never read the
  // person's time answers (trackerapp Service.Worklogs).
  TrackerWorklogs: async (_connection: string, _author: string, from: string, to: string) => ({ worklogs: [], unread: daysFromTo(from, to) }),
  RefreshTrackerWorklogs: async () => ({ worklogs: [] }),
  // A ticket's Work, read when its rail shows (stores/ticketWork): no pull
  // request or branch names it, as a computer with no stored pull requests
  // and no branch of its key answers (app_ticket_work.go GetTicketWork).
  GetTicketWork: async () => ({ key: '', pullRequests: [], branches: [], problems: [] }),
  // A thread the pane opens, as the backend holds an idle thread with no
  // history: resuming it does nothing (app_session_bindings.go), and its
  // ticks, plans, turns, tray tasks, wakes and pending requests are empty
  // (app_paging.go, app_agent_wake_rows.go, app_approval.go).
  AutoResumeThread: async () => undefined,
  GetThreadUserMessageTicks: async () => [],
  ListThreadProposedPlans: async () => [],
  ListRecentTurns: async () => [],
  ListLiveBackgroundTasks: async () => [],
  ListThreadWakes: async () => ({ rows: [], moreReminders: 0 }),
  // Its window, read again on a refresh: the empty page (store/paging.go
  // emptyPagedItems).
  ListThreadSliceAround: async () => ({
    items: [],
    runs: [],
    oldestCursor: { turnIndex: -1, itemIndex: -1, itemId: '' },
    newestCursor: { turnIndex: -1, itemIndex: -1, itemId: '' },
    oldestTurnIndex: -1,
    newestTurnIndex: -1,
    hasMore: false,
    hasMoreOlder: false,
    hasMoreNewer: false,
  }),
  ListPendingInteractiveRequests: async () => ({ approvals: [], userInputs: [] }),
  // Its live state: nothing running, queued or waiting (app_live_state.go).
  GetThreadLiveState: async (threadId: string) => ({
    threadId,
    codexAgents: [],
    subagentProgress: [],
    queueItems: [],
    flushedItems: [],
    flushedThrough: 0,
    interactive: { approvals: [], userInputs: [] },
  }),
  // Its workers: none open (app_thread_agent_rows_summary.go).
  GetThreadWorkerSummary: async (threadId: string) => ({ threadId, open: 0, needYou: 0 }),
  ListThreadWorkerSummaries: async () => ({ summaries: [] }),
  ListStandingAgentWakes: async () => [],
  // Its running shell commands, async questions and agent request
  // deliveries: none (app_shell_command.go, app_async_questions.go,
  // app_thread_request_states.go).
  GetThreadShellCommands: async () => [],
  ListAsyncQuestions: async () => [],
  GetThreadRequestStates: async () => [],
  // Every thread's live activity, read on each connection: none running
  // (app_live_state.go).
  ListThreadLiveActivity: async () => [],
  // The workflow engine, not paused (app_workflow.go).
  WorkflowGetEngineState: async () => ({ paused: false }),
  // A checkout with no busy thread (app_workspace_activity.go).
  GetWorkspaceActivity: async () => ({ activeTurnThreads: 0, runningBackgroundTasks: 0, busyThreads: [] }),
  // A checkout's status stream, for a project directory that is no git
  // repository (gitapp/watch.go Subscribe, git/status.go Status). The stream
  // is keyed on the workspace path; unsubscribing is idempotent
  // (app_gitwatch.go).
  GitStatusSubscribe: async (ws: { workspacePath: string }) => ({
    id: `git-status-${++gitStatusSubscriptions}`,
    cwd: ws.workspacePath,
    status: {
      isRepo: false, branch: '', isDefaultBranch: false, hasChanges: false, insertions: 0, deletions: 0,
      fileCount: 0, hasUpstream: false, aheadCount: 0, behindCount: 0, hasOriginRemote: false,
    },
  }),
  GitStatusUnsubscribe: async () => undefined,
  // The sidebar's thread list and the composer's usage meter, with nothing
  // recorded (app_thread_bindings.go, app_usage.go).
  ListThreads: async () => [],
  GetUsageStats: async () => [],
  // The error and render trace sinks answer with the file they appended to
  // (app_observability.go, internal/uitrace).
  ReportFrontendErrorBatch: async () => '/tmp/ui-trace/frontend-errors.jsonl',
  AppendUIRenderTraceBatch: async () => '/tmp/ui-trace/render.jsonl',
  // An error report reads the backend log lines of the failure it carries:
  // none retained for it (app_observability.go GetErrorLogLines).
  GetErrorLogLines: async () => ({ lines: [], found: false }),
  // The sidebar's projects and groups: none (store/projects.go,
  // store/thread_groups.go; the generated binding reads a null list as []).
  ListProjects: async () => [],
  ListThreadGroups: async () => [],
  // A thread's requests of other threads, waiting and past: none
  // (app_thread_agent_rows.go).
  ListThreadAgentRows: async () => ({ rows: [] }),
  ListThreadPastAgentRows: async () => ({ rows: [], total: 0 }),
  // A thread's browser, with no pages opened: the state of a fresh thread
  // browser (browser/state_push.go buildThreadState, thread_life.go
  // newThreadBrowser). Seq rises with each state built.
  BrowserCompanionThreadState: async (threadId: string) => ({
    kind: 'state', threadId, visible: false, viewportWidth: 1280, viewportHeight: 720, jar: 'worktree',
    recentGen: 'test-launch.0', seq: ++browserStates,
  }),
  // Dictation offered by neither source: no Codex sign-in, no OpenAI key
  // (app_dictation.go).
  DictationSources: async () => ({ subscription: false, live: false, chatGPTSignIn: false }),
  // Dev servers: none listening, and no address to share a preview on
  // (devscan.go DevServerList).
  GetDevServers: async () => ({ servers: [], previewHost: '' }),
  // An unstamped build without an updater: version "dev", self-update not
  // supported (app_host.go Version, app_update_bindings.go CheckForUpdate).
  Version: async () => 'dev',
  CheckForUpdate: async () => ({ supported: false, available: false, currentVersion: 'dev' }),
  // A computer no service manager supervises, read on each connection
  // (app_service_update.go serviceUpdateStatusLocked).
  GetServiceUpdateStatus: async () => ({ supervised: false, available: false, currentVersion: 'dev', phase: 'idle' }),
  // A thread's or a draft's MCP menu, as the backend lists it for a thread of
  // a provider with no built-in servers and nothing configured
  // (app_mcp_bindings.go).
  ListThreadMcpServers: async () => [],
  ListWorkspaceMcpServers: async () => [],
  ...highlightingUnavailable(),
};

// Highlighting unavailable: every highlight read refuses the way the backend
// refuses a session without `files:read` (transport/authorize.go
// scopeRefusal), so code renders plain, as with no computer to highlight it.
// This stands apart from the scope posture the other answers assume, the
// owner's session with every scope. A test about highlighting installs its
// own answers.
function highlightingUnavailable(): Record<string, () => Promise<never>> {
  const names = [
    'HighlightSchemaVersion', 'HighlightClassNames', 'HighlightCode',
    'HighlightPatch', 'HighlightPatchWithContext', 'HighlightEditPatchWithContext',
  ];
  return Object.fromEntries(names.map((name) => [name, async () => {
    const { TransportError } = await import('../../lib/transport/wsClient');
    throw new TransportError('scope_required', `${name} requires the files:read scope, which this session was not granted`, { scope: 'files:read' });
  }]));
}

function dispatch(name: string) {
  return (...args: unknown[]) => {
    const fn = bindingMockFor(name);
    if (!fn) throw unansweredBinding(name);
    return fn(...args);
  };
}

export const GetComputerRoutes = dispatch('GetComputerRoutes');
export const VerifyBrowserUnlock = dispatch('VerifyBrowserUnlock');
export const GetDeviceName = dispatch('GetDeviceName');
export const SetDeviceName = dispatch('SetDeviceName');
export const UpdateClientDeviceName = dispatch('UpdateClientDeviceName');
export const MintOwnDevicePairingOnNetwork = dispatch('MintOwnDevicePairingOnNetwork');
export const OpenOwnComputerPairing = dispatch('OpenOwnComputerPairing');
export const ListOwnDevices = dispatch('ListOwnDevices');
export const SyncOwnDevices = dispatch('SyncOwnDevices');
export const IntroduceOwnDevice = dispatch('IntroduceOwnDevice');
export const MintOwnDeviceIntroduction = dispatch('MintOwnDeviceIntroduction');
export const AcceptOwnDeviceIntroduction = dispatch('AcceptOwnDeviceIntroduction');

// Every binding re-exported from `lib/stores/bindings.ts`.
// Keep this list in sync with that file.
export const ArchiveThread = dispatch('ArchiveThread');
export const UnarchiveThread = dispatch('UnarchiveThread');
export const CreateThread = dispatch('CreateThread');
export const MoveDraftToThread = dispatch('MoveDraftToThread');
export const BeginDraftProjectTransfer = dispatch('BeginDraftProjectTransfer');
export const GetThreadDefaults = dispatch('GetThreadDefaults');
// StartTerminal is imported by stores/bindings.ts. The unit (happy-dom)
// project resolves a missing named export leniently, so its absence went
// uncaught there; the browser project's real ESM loader is strict and fails
// the import. Keep the mock complete against everything bindings.ts imports.
export const StartTerminal = dispatch('StartTerminal');
export const DeleteThread = dispatch('DeleteThread');
export const DeleteThreads = dispatch('DeleteThreads');
export const ForkThread = dispatch('ForkThread');
export const ForkSideChat = dispatch('ForkSideChat');
export const PromoteScratchThread = dispatch('PromoteScratchThread');
export const GetStandingAgent = dispatch('GetStandingAgent');
export const GetDeleteDependents = dispatch('GetDeleteDependents');
export const CreateAssistant = dispatch('CreateAssistant');
export const SetAssistantProjects = dispatch('SetAssistantProjects');
export const StartNextAssistantThread = dispatch('StartNextAssistantThread');
export const ListAssistantPastThreads = dispatch('ListAssistantPastThreads');
export const ListThreadWorkerSummaries = dispatch('ListThreadWorkerSummaries');
export const GetThreadWorkerSummary = dispatch('GetThreadWorkerSummary');
export const SendExternalThreadMessage = dispatch('SendExternalThreadMessage');
export const BeginThreadTransfer = dispatch('BeginThreadTransfer');
export const CreateThreadTransferOffer = dispatch('CreateThreadTransferOffer');
export const BindThreadTransferDestination = dispatch('BindThreadTransferDestination');
export const GetThreadTransfers = dispatch('GetThreadTransfers');
export const GetThreadTransferStatus = dispatch('GetThreadTransferStatus');
export const GetThreadTransferIntent = dispatch('GetThreadTransferIntent');
export const ListThreadRecurringJobs = dispatch('ListThreadRecurringJobs');
export const GetThreadTransferDestinationProject = dispatch('GetThreadTransferDestinationProject');
export const RetryThreadTransfer = dispatch('RetryThreadTransfer');
export const CancelThreadTransfer = dispatch('CancelThreadTransfer');
export const DiscardUnpreparedThreadTransfer = dispatch('DiscardUnpreparedThreadTransfer');
export const GetThread = dispatch('GetThread');
export const ListThreads = dispatch('ListThreads');
export const MarkThreadRead = dispatch('MarkThreadRead');
export const MarkThreadsRead = dispatch('MarkThreadsRead');
export const MarkThreadUnread = dispatch('MarkThreadUnread');
export const PinThread = dispatch('PinThread');
export const SetThreadPinGroup = dispatch('SetThreadPinGroup');
export const UnpinThread = dispatch('UnpinThread');
export const ListThreadGroups = dispatch('ListThreadGroups');
export const CreateThreadGroup = dispatch('CreateThreadGroup');
export const RenameThreadGroup = dispatch('RenameThreadGroup');
export const DeleteThreadGroup = dispatch('DeleteThreadGroup');
export const SetThreadGroup = dispatch('SetThreadGroup');
export const SetThreadPlacement = dispatch('SetThreadPlacement');
export const RenameThread = dispatch('RenameThread');
export const RegenerateThreadTitle = dispatch('RegenerateThreadTitle');
export const SetThreadTicket = dispatch('SetThreadTicket');
export const ClearThreadTicket = dispatch('ClearThreadTicket');
export const ThreadsForTicket = dispatch('ThreadsForTicket');
export const GetTicketWork = dispatch('GetTicketWork');
export const StartTicketThread = dispatch('StartTicketThread');
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

export const AutoResumeThread = dispatch('AutoResumeThread');
export const StopSession = dispatch('StopSession');
export const ReconnectSession = dispatch('ReconnectSession');
export const DismissSessionBanner = dispatch('DismissSessionBanner');
export const DismissUnsavedThreadRows = dispatch('DismissUnsavedThreadRows');
/**
 * Every pane open reads it, so like SyncThreadWindow it has a DEFAULT: an
 * empty list, which is what the backend answers for a thread with nothing
 * unsaved. An explicit `setBindingMock('GetThreadUnsavedRows', …)` wins.
 */
export const GetThreadUnsavedRows = async (threadId: unknown): Promise<unknown> => {
  const explicit = mocks.get('GetThreadUnsavedRows');
  if (explicit) return explicit(threadId);
  return { threadId, rows: [] };
};
export const SendMessageWithOptions = dispatch('SendMessageWithOptions');
export const InterruptTurn = dispatch('InterruptTurn');
export const GetConversationMutationState = dispatch('GetConversationMutationState');
export const GetThreadLiveState = dispatch('GetThreadLiveState');
export const ListThreadLiveActivity = dispatch('ListThreadLiveActivity');
export const ListUnsettledFlushes = dispatch('ListUnsettledFlushes');
export const ListPendingInteractiveRequests = dispatch('ListPendingInteractiveRequests');
export const RespondToApproval = dispatch('RespondToApproval');
export const RespondToUserInput = dispatch('RespondToUserInput');
export const SendPlanRevisionComments = dispatch('SendPlanRevisionComments');

// Background tasks (per-item + thread-wide stop primitives)
export const StopClaudeTask = dispatch('StopClaudeTask');
export const TerminateCodexBackgroundTerminal = dispatch('TerminateCodexBackgroundTerminal');
export const StopCodexSubagent = dispatch('StopCodexSubagent');
export const StopBackgroundTasks = dispatch('StopBackgroundTasks');
export const BackgroundClaudeTask = dispatch('BackgroundClaudeTask');
export const WatchBackgroundTaskOutput = dispatch('WatchBackgroundTaskOutput');
export const UnwatchBackgroundTaskOutput = dispatch('UnwatchBackgroundTaskOutput');

// Live-session context breakdown (Claude's canonical /context read).
export const GetThreadContextUsage = dispatch('GetThreadContextUsage');

// Thread-scoped model / workspace / message-search / commit-message bindings
// (previously hand-wrapped with Call.ByName; now re-exported through the
// generated app.js). Tests stub these through setBindingMock by method name.
export const UpdateThreadModelSelection = dispatch('UpdateThreadModelSelection');
export const UpdateThreadMode = dispatch('UpdateThreadMode');
export const UpdateThreadReasoningEffort = dispatch('UpdateThreadReasoningEffort');
export const UpdateThreadFastMode = dispatch('UpdateThreadFastMode');
export const UpdateThreadContextWindow = dispatch('UpdateThreadContextWindow');
export const UpdateThreadRuntimeMode = dispatch('UpdateThreadRuntimeMode');
export const UpdateThreadBranch = dispatch('UpdateThreadBranch');
export const UpdateThreadWorkspace = dispatch('UpdateThreadWorkspace');
export const UpdateNewThreadDefaults = dispatch('UpdateNewThreadDefaults');
export const WriteWorkspaceFile = dispatch('WriteWorkspaceFile');
export const SearchThreadMessages = dispatch('SearchThreadMessages');
export const SearchThreadItems = dispatch('SearchThreadItems');
export const SearchTimelineItems = dispatch('SearchTimelineItems');
export const GenerateCommitMessage = dispatch('GenerateCommitMessage');

export const GetPayloadPreview = dispatch('GetPayloadPreview');
export const GetPayloadChunk = dispatch('GetPayloadChunk');
export const GetPayloadData = dispatch('GetPayloadData');
export const GetRateLimitsSnapshots = dispatch('GetRateLimitsSnapshots');
export const ListProviderAccounts = dispatch('ListProviderAccounts');
export const StartProviderLogin = dispatch('StartProviderLogin');
export const GetProviderLoginState = dispatch('GetProviderLoginState');
export const SubmitProviderLoginCode = dispatch('SubmitProviderLoginCode');
export const CancelProviderLogin = dispatch('CancelProviderLogin');
export const SwitchProviderAccount = dispatch('SwitchProviderAccount');
export const RemoveProviderAccount = dispatch('RemoveProviderAccount');
export const ReorderProviderAccounts = dispatch('ReorderProviderAccounts');
export const SetProviderAccountAutoSwitch = dispatch('SetProviderAccountAutoSwitch');
export const RefreshProviderAccountUsage = dispatch('RefreshProviderAccountUsage');
export const SpendProviderAccountReset = dispatch('SpendProviderAccountReset');
export const FinishProviderAccountReset = dispatch('FinishProviderAccountReset');

export const GetSettings = dispatch('GetSettings');
export const UpdateSettings = dispatch('UpdateSettings');
export const GetClaudeTranscriptRetention = dispatch('GetClaudeTranscriptRetention');
export const ClearBrowserSiteData = dispatch('ClearBrowserSiteData');
export const BrowserCompanionRevealPageFile = dispatch('BrowserCompanionRevealPageFile');
export const BrowserCompanionRecentAddresses = dispatch('BrowserCompanionRecentAddresses');
export const BrowserCompanionDo = dispatch('BrowserCompanionDo');
export const BrowserCompanionFind = dispatch('BrowserCompanionFind');
export const BrowserCompanionPaneAttach = dispatch('BrowserCompanionPaneAttach');
export const BrowserCompanionPaneDetach = dispatch('BrowserCompanionPaneDetach');
export const BrowserCompanionPaneRect = dispatch('BrowserCompanionPaneRect');
export const BrowserCompanionThreadState = dispatch('BrowserCompanionThreadState');
export const BrowserHostReport = dispatch('BrowserHostReport');

// Appearance / themes (stores/appearance.svelte.ts). Both writes are gated
// on the wire — `SetAppearance` needs `settings:write` and
// `SetWindowBackgroundColor` is `host`-scoped — so a test that wants the
// DEGRADED path throws from its mock rather than omitting the binding.
export const GetThemeFiles = dispatch('GetThemeFiles');
export const GetSpinnerFiles = dispatch('GetSpinnerFiles');

// The custom notification-cue library (stores/sounds.svelte.ts). The two
// writes are `settings:write`, so a test wanting a REFUSED add or delete
// throws from its mock rather than omitting the binding.
export const GetSoundFiles = dispatch('GetSoundFiles');
export const PutSoundFile = dispatch('PutSoundFile');
export const DeleteSoundFile = dispatch('DeleteSoundFile');
export const SetAppearance = dispatch('SetAppearance');
export const SetWindowBackgroundColor = dispatch('SetWindowBackgroundColor');

// The system-sound audition (settings/NotificationsSection.svelte): a real
// banner on the host, so it is `host`-scoped like SetWindowBackgroundColor
// and a test wanting the refused path throws from its mock.
export const PreviewNotificationSound = dispatch('PreviewNotificationSound');
export const SetProviderCustomEnvVar = dispatch('SetProviderCustomEnvVar');
export const DeleteProviderCustomEnvVar = dispatch('DeleteProviderCustomEnvVar');
export const GetProjectWorktreeSetup = dispatch('GetProjectWorktreeSetup');
export const SetProjectWorktreeSetup = dispatch('SetProjectWorktreeSetup');
export const DismissThreadWorktreeSetup = dispatch('DismissThreadWorktreeSetup');
export const GetThreadWorktreeSetup = dispatch('GetThreadWorktreeSetup');
export const RetryThreadWorktreeSetup = dispatch('RetryThreadWorktreeSetup');
export const AnswerThreadShellCommand = dispatch('AnswerThreadShellCommand');
export const RunThreadShellCommand = dispatch('RunThreadShellCommand');
export const StopThreadShellCommand = dispatch('StopThreadShellCommand');
export const GetThreadShellCommands = dispatch('GetThreadShellCommands');
export const ReadShellAttachmentOutput = dispatch('ReadShellAttachmentOutput');
export const Version = dispatch('Version');
export const GetErrorLogLines = dispatch('GetErrorLogLines');

// Workflow surface.
export const WorkflowAnswerQuestion = dispatch('WorkflowAnswerQuestion');
export const WorkflowBindThread = dispatch('WorkflowBindThread');
export const WorkflowCancelItem = dispatch('WorkflowCancelItem');
export const WorkflowCompleteTakeover = dispatch('WorkflowCompleteTakeover');
export const WorkflowCreateItemPR = dispatch('WorkflowCreateItemPR');
export const WorkflowDiscussPR = dispatch('WorkflowDiscussPR');
export const WorkflowDiscardItem = dispatch('WorkflowDiscardItem');
export const WorkflowDiscardPreview = dispatch('WorkflowDiscardPreview');
export const WorkflowDropUnit = dispatch('WorkflowDropUnit');
export const WorkflowFetchPRReviewComments = dispatch('WorkflowFetchPRReviewComments');
export const WorkflowGetEngineState = dispatch('WorkflowGetEngineState');
export const WorkflowGetItem = dispatch('WorkflowGetItem');
export const WorkflowGetRunMap = dispatch('WorkflowGetRunMap');
export const WorkflowListDefinitions = dispatch('WorkflowListDefinitions');
export const WorkflowListItemCosts = dispatch('WorkflowListItemCosts');
export const WorkflowListItems = dispatch('WorkflowListItems');
export const WorkflowListUnresolvedItems = dispatch('WorkflowListUnresolvedItems');
export const WorkflowMergeItem = dispatch('WorkflowMergeItem');
export const WorkflowRerunItem = dispatch('WorkflowRerunItem');
export const WorkflowSendPRReviewCommentsToThread = dispatch('WorkflowSendPRReviewCommentsToThread');
export const WorkflowPauseItem = dispatch('WorkflowPauseItem');
export const WorkflowPauseItemAndEndSessions = dispatch('WorkflowPauseItemAndEndSessions');
export const WorkflowRequestSoftStop = dispatch('WorkflowRequestSoftStop');
export const WorkflowResolveGate = dispatch('WorkflowResolveGate');
export const WorkflowResumeItem = dispatch('WorkflowResumeItem');
export const WorkflowRetryFailedUnits = dispatch('WorkflowRetryFailedUnits');
export const WorkflowRetryUnit = dispatch('WorkflowRetryUnit');
export const WorkflowSetGlobalPause = dispatch('WorkflowSetGlobalPause');
export const WorkflowStartRun = dispatch('WorkflowStartRun');
export const WorkflowTakeOverUnit = dispatch('WorkflowTakeOverUnit');
export const WorkflowUnbindThread = dispatch('WorkflowUnbindThread');

// Per-provider/model context window + auto-compact thresholds.
export const GetContextSettings = dispatch('GetContextSettings');
export const GetNetworkSettings = dispatch('GetNetworkSettings');
export const SetNetworkSettings = dispatch('SetNetworkSettings');
export const ForgetTailnetNode = dispatch('ForgetTailnetNode');
export const RenewCanonicalDomainCert = dispatch('RenewCanonicalDomainCert');

// Device access (Settings → Remote access → Devices).
export const GetAccessOverview = dispatch('GetAccessOverview');
export const MintDevicePairing = dispatch('MintDevicePairing');
export const MintDevicePairingOnNetwork = dispatch('MintDevicePairingOnNetwork');
export const DiscoverComputers = dispatch('DiscoverComputers');
export const OpenComputerPairing = dispatch('OpenComputerPairing');
export const ComputerPairingStatus = dispatch('ComputerPairingStatus');
export const CloseComputerPairing = dispatch('CloseComputerPairing');
export const DevicePairingStatus = dispatch('DevicePairingStatus');
export const ConfirmDevicePairing = dispatch('ConfirmDevicePairing');
export const CancelDevicePairing = dispatch('CancelDevicePairing');
export const RevokeAccessDevice = dispatch('RevokeAccessDevice');
export const RevokeAccessSession = dispatch('RevokeAccessSession');
export const RestoreAccessDevice = dispatch('RestoreAccessDevice');
export const ForgetAccessDevice = dispatch('ForgetAccessDevice');
export const RegisterPushToken = dispatch('RegisterPushToken');
export const UnregisterPushToken = dispatch('UnregisterPushToken');
export const GetPushSenderStatus = dispatch('GetPushSenderStatus');
export const SetPushSenderCredential = dispatch('SetPushSenderCredential');
export const ClearPushSenderCredential = dispatch('ClearPushSenderCredential');
export const BeginPasskeyRegistration = dispatch('BeginPasskeyRegistration');
export const FinishPasskeyRegistration = dispatch('FinishPasskeyRegistration');
export const ListPasskeys = dispatch('ListPasskeys');
export const DeletePasskey = dispatch('DeletePasskey');
export const BeginPasskeyStepUp = dispatch('BeginPasskeyStepUp');
export const FinishPasskeyStepUp = dispatch('FinishPasskeyStepUp');

// WSL distro switcher (Settings → Remote access → WSL distro).
// IsWSL gates whether the section renders at all; the other three
// drive the dropdown + persist on change.
export const IsWSL = dispatch('IsWSL');
export const ListWSLDistros = dispatch('ListWSLDistros');
export const GetWSLDistroPreference = dispatch('GetWSLDistroPreference');
export const SetWSLDistroPreference = dispatch('SetWSLDistroPreference');
// Launcher GPU vsync preference (Settings → Performance), same wsl.json.
export const GetWebviewGPUVsyncDisabled = dispatch('GetWebviewGPUVsyncDisabled');
export const SetWebviewGPUVsyncDisabled = dispatch('SetWebviewGPUVsyncDisabled');
export const GetObservabilityTracing = dispatch('GetObservabilityTracing');
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

// The port gateway (docs/specs/remote-access.md §7).
export const GetDevServers = dispatch('GetDevServers');
export const AllowPreviewPort = dispatch('AllowPreviewPort');
export const DisallowPreviewPort = dispatch('DisallowPreviewPort');
export const MintPreviewURL = dispatch('MintPreviewURL');
export const MintFilePreviewURL = dispatch('MintFilePreviewURL');
export const GetEditorSettings = dispatch('GetEditorSettings');
export const SetEditorSettings = dispatch('SetEditorSettings');
export const ListBackends = dispatch('ListBackends');
export const ListAgentComputers = dispatch('ListAgentComputers');
export const SetAgentComputerEnabled = dispatch('SetAgentComputerEnabled');
export const PairAgentComputer = dispatch('PairAgentComputer');
export const RemoteCommandStart = dispatch('RemoteCommandStart');
export const RemoteCommandStatus = dispatch('RemoteCommandStatus');
export const RemoteCommandCancel = dispatch('RemoteCommandCancel');
export const RemoteCommandProjects = dispatch('RemoteCommandProjects');
export const RemoteCommandLogArtifact = dispatch('RemoteCommandLogArtifact');
export const ThreadToolCall = dispatch('ThreadToolCall');
export const ThreadToolExportChunk = dispatch('ThreadToolExportChunk');
export const ThreadToolQuery = dispatch('ThreadToolQuery');
export const ThreadOwnerChanges = dispatch('ThreadOwnerChanges');
export const ThreadOwnerRecovery = dispatch('ThreadOwnerRecovery');
export const ThreadToolWatch = dispatch('ThreadToolWatch');
export const ThreadToolRequestStatus = dispatch('ThreadToolRequestStatus');
export const ThreadToolResolve = dispatch('ThreadToolResolve');
export const AgentRemoteComputers = dispatch('AgentRemoteComputers');
export const AgentRemoteStart = dispatch('AgentRemoteStart');
export const AgentRemoteStatus = dispatch('AgentRemoteStatus');
export const AgentRosterSeeds = dispatch('AgentRosterSeeds');
export const AgentRemoteCancel = dispatch('AgentRemoteCancel');
export const AgentRemoteFetchArtifact = dispatch('AgentRemoteFetchArtifact');
export const AgentRemoteFetchLog = dispatch('AgentRemoteFetchLog');
export const ListThreadRemoteCommands = dispatch('ListThreadRemoteCommands');
export const RemoteCommandArtifact = dispatch('RemoteCommandArtifact');
export const RemoteCommandEnvironment = dispatch('RemoteCommandEnvironment');
export const RemoteCommandReadLog = dispatch('RemoteCommandReadLog');
export const RemoteCommandSearchLog = dispatch('RemoteCommandSearchLog');
export const AddBackend = dispatch('AddBackend');
export const RemoveBackend = dispatch('RemoveBackend');
export const RenameBackend = dispatch('RenameBackend');
export const RepairBackendAddress = dispatch('RepairBackendAddress');
export const ShutdownBackend = dispatch('ShutdownBackend');
export const StartSSHConnection = dispatch('StartSSHConnection');
export const GetSSHConnection = dispatch('GetSSHConnection');
export const ConfirmSSHConnection = dispatch('ConfirmSSHConnection');
export const CancelSSHConnection = dispatch('CancelSSHConnection');
export const StartSSHComputer = dispatch('StartSSHComputer');
// NetworkSettings is a class re-exported alongside the bindings; the
// mock just needs a constructor-compatible stand-in so test code that
// builds `new NetworkSettings({ bindAll })` doesn't try to load the
// real generated module. Real instantiation (createFrom) is exercised
// in component tests that run against the live binding.
//
// Every persisted field is carried, because SetNetworkSettings writes
// the WHOLE record: a stand-in that dropped the domain half would let a
// test pass while the real call erased it.
export class TLSStatus {
  serving: string;
  notAfter: number;
  renewing: boolean;
  lastError: string;
  selfSignedFingerprint: string;
  constructor(s: Partial<TLSStatus> = {}) {
    this.serving = s.serving ?? '';
    this.notAfter = s.notAfter ?? 0;
    this.renewing = s.renewing ?? false;
    this.lastError = s.lastError ?? '';
    this.selfSignedFingerprint = s.selfSignedFingerprint ?? '';
  }
}
export class NetworkSettings {
  bindAll: boolean;
  canonicalDomain: string;
  acmeDnsHook: string[];
  externalCertFile: string;
  externalKeyFile: string;
  tls: TLSStatus;
  url: string;
  token: string;
  insecure: boolean;
  constructor(s: Partial<NetworkSettings> = {}) {
    this.bindAll = s.bindAll ?? false;
    this.canonicalDomain = s.canonicalDomain ?? '';
    this.acmeDnsHook = s.acmeDnsHook ?? [];
    this.externalCertFile = s.externalCertFile ?? '';
    this.externalKeyFile = s.externalKeyFile ?? '';
    this.tls = s.tls ?? new TLSStatus();
    this.url = s.url ?? '';
    this.token = s.token ?? '';
    this.insecure = s.insecure ?? false;
  }
}

export const GetProviderStatuses = dispatch('GetProviderStatuses');
export const GetModelsForProvider = dispatch('GetModelsForProvider');
export const CheckAgentProfiles = dispatch('CheckAgentProfiles');
export const GetNativeMemoryStatus = dispatch('GetNativeMemoryStatus');
// Composer command-menu sources + the two Codex thread commands the menu
// drives. All four are re-exported from stores/bindings.ts, so the mock has to
// carry them or the browser project's strict ESM loader fails the import.
export const GetClaudeSlashCommands = dispatch('GetClaudeSlashCommands');
export const GetClaudeSkills = dispatch('GetClaudeSkills');
export const GetCodexSkills = dispatch('GetCodexSkills');
export const CompactCodexThread = dispatch('CompactCodexThread');
export const RecheckClaudeAccount = dispatch('RecheckClaudeAccount');
export const RecheckCodexAccount = dispatch('RecheckCodexAccount');
export const RecheckAgyAccount = dispatch('RecheckAgyAccount');

export const GetGitStatus = dispatch('GetGitStatus');
export const GitStatusSubscribe = dispatch('GitStatusSubscribe');
export const GitStatusUnsubscribe = dispatch('GitStatusUnsubscribe');
// Class re-export mirroring the generated GitStatusSubscriptionResult.
// Tests stub GitStatusSubscribe to return a plain object literal; we
// only need the type to satisfy `import type` consumers.
export class GitStatusSubscriptionResult {
  id: string;
  cwd: string;
  status: import('../../lib/types/git').GitStatus;
  constructor(s: Partial<GitStatusSubscriptionResult> = {}) {
    this.id = s.id ?? '';
    this.cwd = s.cwd ?? '';
    this.status = s.status ?? ({} as import('../../lib/types/git').GitStatus);
  }
}
export const GitListBranches = dispatch('GitListBranches');
export const GitListTags = dispatch('GitListTags');
export const GitRefreshTags = dispatch('GitRefreshTags');
export const GitListWorktrees = dispatch('GitListWorktrees');
export const GitCommit = dispatch('GitCommit');
export const GetGitActionOutcome = dispatch('GetGitActionOutcome');
export const GitPush = dispatch('GitPush');
export const GitPull = dispatch('GitPull');
export const GitFetchOrigin = dispatch('GitFetchOrigin');
export const GitCheckout = dispatch('GitCheckout');
export const GitCreateBranchFrom = dispatch('GitCreateBranchFrom');
export const GitCreatePR = dispatch('GitCreatePR');
export const GitMaybeFetchRemotes = dispatch('GitMaybeFetchRemotes');
export const GitListBranchPruneCandidates = dispatch('GitListBranchPruneCandidates');
export const GitPruneBranches = dispatch('GitPruneBranches');
export const GitSyncBranch = dispatch('GitSyncBranch');
export const GitWorktreeStatus = dispatch('GitWorktreeStatus');
export const PrepareThreadWorktree = dispatch('PrepareThreadWorktree');
export const AttachThreadWorktree = dispatch('AttachThreadWorktree');
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
  orphanCommits: number;
  hasUpstream: boolean;
  attachedThreads: number;
  terminals: number;
  constructor(s: Partial<WorktreeStatus> = {}) {
    this.path = s.path ?? '';
    this.branch = s.branch ?? '';
    this.dirty = s.dirty ?? false;
    this.uncommittedCount = s.uncommittedCount ?? 0;
    this.unpushedCommits = s.unpushedCommits ?? 0;
    this.orphanCommits = s.orphanCommits ?? 0;
    this.hasUpstream = s.hasUpstream ?? false;
    this.attachedThreads = s.attachedThreads ?? 0;
    this.terminals = s.terminals ?? 0;
  }
}


// Composer enhancements
export const MintAttachmentUploadTicket = dispatch('MintAttachmentUploadTicket');
export const ListAttachments = dispatch('ListAttachments');
export const DeleteAttachment = dispatch('DeleteAttachment');
export const MintAttachmentDownloadTicket = dispatch('MintAttachmentDownloadTicket');
export const GetAttachmentThumbnail = dispatch('GetAttachmentThumbnail');
export const GetLocalImage = dispatch('GetLocalImage');
export const SaveLocalImage = dispatch('SaveLocalImage');
export const SaveDraft = dispatch('SaveDraft');
export const GetDraft = dispatch('GetDraft');
export const DeleteEmptyDraftThread = dispatch('DeleteEmptyDraftThread');
export const DictationSources = dispatch('DictationSources');
export const BeginDictation = dispatch('BeginDictation');
export const MintDictationChunkTicket = dispatch('MintDictationChunkTicket');
export const FinishDictation = dispatch('FinishDictation');
export const CancelDictation = dispatch('CancelDictation');
export const BeginLiveDictation = dispatch('BeginLiveDictation');
export const AppendLiveDictation = dispatch('AppendLiveDictation');
export const FinishLiveDictation = dispatch('FinishLiveDictation');
export const CancelLiveDictation = dispatch('CancelLiveDictation');
export const OpenAIKeyFile = dispatch('OpenAIKeyFile');
export const EditOpenAIKeyFile = dispatch('EditOpenAIKeyFile');
export const ReadAloudSource = dispatch('ReadAloudSource');
export const MintReadAloudTicket = dispatch('MintReadAloudTicket');
export const ReleaseReadAloudTicket = dispatch('ReleaseReadAloudTicket');
export const SearchWorkspaceFiles = dispatch('SearchWorkspaceFiles');
export const ListChatBarFavorites = dispatch('ListChatBarFavorites');
export const SetChatBarFavorite = dispatch('SetChatBarFavorite');

// Keybindings
export const GetKeybindings = dispatch('GetKeybindings');
export const UpdateKeybindings = dispatch('UpdateKeybindings');
export const ResetKeybindings = dispatch('ResetKeybindings');

// Review pane diffs
export const OpenWorkspaceDiff = dispatch('OpenWorkspaceDiff');
export const OpenCommitFileDiff = dispatch('OpenCommitFileDiff');
export const ListCommitFiles = dispatch('ListCommitFiles');
export const OpenBranchBaseDiff = dispatch('OpenBranchBaseDiff');
export const OpenCommitDiff = dispatch('OpenCommitDiff');
export const ReadReviewDiff = dispatch('ReadReviewDiff');
export const ReleaseReviewDiff = dispatch('ReleaseReviewDiff');
export const ListBranchCommits = dispatch('ListBranchCommits');
export const ListRecentCommits = dispatch('ListRecentCommits');
export const ListThreadEditDiffs = dispatch('ListThreadEditDiffs');
export const OpenEditDiff = dispatch('OpenEditDiff');
export const OpenTurnEditsDiff = dispatch('OpenTurnEditsDiff');
export const GetPayloadPatchSpans = dispatch('GetPayloadPatchSpans');
export const GetDiffContextLines = dispatch('GetDiffContextLines');
export const GetEditDiffContextLines = dispatch('GetEditDiffContextLines');
export const GetReviewDiffFileSizes = dispatch('GetReviewDiffFileSizes');
export const GetReviewDiffImage = dispatch('GetReviewDiffImage');
export const GetEditDiffImage = dispatch('GetEditDiffImage');
export const VerifyEditDiffs = dispatch('VerifyEditDiffs');
export const HighlightClassNames = dispatch('HighlightClassNames');
export const HighlightSchemaVersion = dispatch('HighlightSchemaVersion');
export const HighlightCode = dispatch('HighlightCode');
export const ResyncLiveCode = dispatch('ResyncLiveCode');
export const HighlightPatch = dispatch('HighlightPatch');
export const HighlightPatchWithContext = dispatch('HighlightPatchWithContext');
export const HighlightEditPatchWithContext = dispatch('HighlightEditPatchWithContext');
export const ForkThreadFromMessage = dispatch('ForkThreadFromMessage');
export const RevertConversationAndResendMessage = dispatch('RevertConversationAndResendMessage');

// Dev-only UI render tracing
export const AppendUIRenderTraceBatch = dispatch('AppendUIRenderTraceBatch');
export const BookmarkUIRenderTrace = dispatch('BookmarkUIRenderTrace');

// Always-on frontend runtime-error log
export const ReportFrontendErrorBatch = dispatch('ReportFrontendErrorBatch');

// Idle renderer memory trim
export const RequestWebviewMemoryTrim = dispatch('RequestWebviewMemoryTrim');

// PR-based thread creation

// Projects (sidebar)
export const ListProjects = dispatch('ListProjects');
export const CreateProject = dispatch('CreateProject');
export const InspectProjectFolder = dispatch('InspectProjectFolder');
export const CreateProjectCheckout = dispatch('CreateProjectCheckout');
export const RefreshProjectIdentity = dispatch('RefreshProjectIdentity');
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
// slices via ListThreadSliceAround and page by item-coordinate cursors.
export const ListThreadSliceAround = dispatch('ListThreadSliceAround');

/**
 * Cold-open window sync — what the pane calls where it used to call
 * ListThreadSliceAround.
 *
 * Unlike every other binding here it has a DEFAULT rather than throwing:
 * with no explicit mock it answers `stale` carrying exactly the page the
 * ListThreadSliceAround mock returns, which is what the backend does for
 * an unknown stamp. That keeps every suite that describes its initial
 * window through the slice mock describing the same window, and it lives
 * in the registry (not a setup file) so the test files that call
 * `resetBindingMocks()` themselves cannot lose it. An explicit
 * `setBindingMock('SyncThreadWindow', …)` wins; with neither mock
 * installed the call throws like any other.
 */
export const SyncThreadWindow = async (
  threadId: unknown,
  req: unknown,
): Promise<unknown> => {
  const explicit = mocks.get('SyncThreadWindow');
  if (explicit) return explicit(threadId, req);
  const request = (req ?? {}) as { anchorItemId?: string; itemBudget?: number; selection?: unknown };
  const page = await dispatch('ListThreadSliceAround')(
    threadId,
    request.anchorItemId ?? '',
    request.itemBudget ?? 0,
    request,
  );
  return { status: 'stale', epoch: 1, rev: 1, generation: 'test-generation', page };
};
export const ListActivityRunMembers = dispatch('ListActivityRunMembers');
export const ListItemsBeforeCursor = dispatch('ListItemsBeforeCursor');
export const ListItemsAfterCursor = dispatch('ListItemsAfterCursor');
export const GetThreadItemProjectionSource = dispatch('GetThreadItemProjectionSource');
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
export const OpenPRDiff = dispatch('OpenPRDiff');
export const ListPRCommits = dispatch('ListPRCommits');
export const OpenPRCommitDiff = dispatch('OpenPRCommitDiff');
export const GetPRMergeConflicts = dispatch('GetPRMergeConflicts');
export const GetMergeConflictFile = dispatch('GetMergeConflictFile');
export const SavePRCIJobLog = dispatch('SavePRCIJobLog');
export const SavePRCIJobLogSection = dispatch('SavePRCIJobLogSection');
export const GetPRCILogLines = dispatch('GetPRCILogLines');
export const SearchPRCILog = dispatch('SearchPRCILog');
export const FetchForgeAttachment = dispatch('FetchForgeAttachment');
export const ResolveForgeFacts = dispatch('ResolveForgeFacts');
export const GetGitBlobImage = dispatch('GetGitBlobImage');
export const SaveGitBlobImage = dispatch('SaveGitBlobImage');
export const SaveAttachment = dispatch('SaveAttachment');
export const SaveForgeAttachment = dispatch('SaveForgeAttachment');
export const ListPRReviewThreads = dispatch('ListPRReviewThreads');
export const SubmitPRReview = dispatch('SubmitPRReview');
export const GetPRReviewSubmitOutcome = dispatch('GetPRReviewSubmitOutcome');
export const ReplyToPRThread = dispatch('ReplyToPRThread');
export const PostPRLineComment = dispatch('PostPRLineComment');
export const PostPRComment = dispatch('PostPRComment');
export const EditPRComment = dispatch('EditPRComment');
export const DeletePRComment = dispatch('DeletePRComment');
export const SetPRThreadResolved = dispatch('SetPRThreadResolved');
export const ApprovePR = dispatch('ApprovePR');
export const UnapprovePR = dispatch('UnapprovePR');
export const MergePR = dispatch('MergePR');
export const CancelPRAutoMerge = dispatch('CancelPRAutoMerge');
export const UpdatePRBranch = dispatch('UpdatePRBranch');
export const MarkPRReady = dispatch('MarkPRReady');
export const EditPR = dispatch('EditPR');
export const SetPRReviewers = dispatch('SetPRReviewers');
export const SetPRAssignees = dispatch('SetPRAssignees');
export const SetPRLabels = dispatch('SetPRLabels');
export const SetPRState = dispatch('SetPRState');
export const RerequestPRReview = dispatch('RerequestPRReview');
export const DismissPRReview = dispatch('DismissPRReview');
export const BypassPRRequestedChanges = dispatch('BypassPRRequestedChanges');
export const SearchPRCandidates = dispatch('SearchPRCandidates');
export const GetPRMergeMessage = dispatch('GetPRMergeMessage');
export const ApplyPRSuggestion = dispatch('ApplyPRSuggestion');
export const SetPRCommentReaction = dispatch('SetPRCommentReaction');
export const ParseCommentSuggestions = dispatch('ParseCommentSuggestions');
export const SubscribePRUpdates = dispatch('SubscribePRUpdates');
export const UnsubscribePRUpdates = dispatch('UnsubscribePRUpdates');
export const SetPRUpdatesActive = dispatch('SetPRUpdatesActive');
export const SetPRCILogFollows = dispatch('SetPRCILogFollows');
export const RefreshPRCI = dispatch('RefreshPRCI');
export const ListForgeProjects = dispatch('ListForgeProjects');
export const SubscribePRList = dispatch('SubscribePRList');
export const SetPRListActive = dispatch('SetPRListActive');
export const RefreshPRList = dispatch('RefreshPRList');
export const UnsubscribePRList = dispatch('UnsubscribePRList');
export const ListThreadsOnPR = dispatch('ListThreadsOnPR');
export const OpenPRThread = dispatch('OpenPRThread');
export const ListPRDiffReviewComments = dispatch('ListPRDiffReviewComments');
export const CreatePRDiffReviewComment = dispatch('CreatePRDiffReviewComment');
export const UpdatePRDiffReviewComment = dispatch('UpdatePRDiffReviewComment');
export const DeletePRDiffReviewComment = dispatch('DeletePRDiffReviewComment');
export const MarkPRDiffReviewCommentsSent = dispatch('MarkPRDiffReviewCommentsSent');
export const SendPRDiffReviewComments = dispatch('SendPRDiffReviewComments');
export const ListProjectPipelines = dispatch('ListProjectPipelines');
export const ReadPipelineStages = dispatch('ReadPipelineStages');
export const SubscribePipeline = dispatch('SubscribePipeline');
export const SetPipelineActive = dispatch('SetPipelineActive');
export const SetPipelineFollows = dispatch('SetPipelineFollows');
export const RefreshPipeline = dispatch('RefreshPipeline');
export const RunPRCIAction = dispatch('RunPRCIAction');
export const RunPipelineAction = dispatch('RunPipelineAction');
export const SaveCIArtifacts = dispatch('SaveCIArtifacts');
export const UnsubscribePipeline = dispatch('UnsubscribePipeline');
export const CountRunningBackgroundTasks = dispatch('CountRunningBackgroundTasks');
export const RunningBackgroundAgents = dispatch('RunningBackgroundAgents');
export const ListLiveBackgroundTasks = dispatch('ListLiveBackgroundTasks');
export const ListThreadWakes = dispatch('ListThreadWakes');
export const ListStandingAgentWakes = dispatch('ListStandingAgentWakes');
export const GetThreadWakeDetail = dispatch('GetThreadWakeDetail');
export const WakeThreadWakeNow = dispatch('WakeThreadWakeNow');
export const CancelThreadWake = dispatch('CancelThreadWake');
export const ListThreadAgentRows = dispatch('ListThreadAgentRows');
export const ListThreadPastAgentRows = dispatch('ListThreadPastAgentRows');
export const GetThreadAgentRows = dispatch('GetThreadAgentRows');
export const GetThreadRequestStates = dispatch('GetThreadRequestStates');
export const StopThreadAgentRow = dispatch('StopThreadAgentRow');
export const StopThreadAgentRows = dispatch('StopThreadAgentRows');
export const StopThreadAgentIncoming = dispatch('StopThreadAgentIncoming');
export const CancelThreadRemoteCommand = dispatch('CancelThreadRemoteCommand');
export const ReadThreadRemoteLog = dispatch('ReadThreadRemoteLog');
export const GetWorkspaceActivity = dispatch('GetWorkspaceActivity');
export const GetThreadItem = dispatch('GetThreadItem');
export const GetThreadUserMessageTicks = dispatch('GetThreadUserMessageTicks');
export const GetTimelineUserMessageTicks = dispatch('GetTimelineUserMessageTicks');
export const GetThreadUserMessageHistory = dispatch('GetThreadUserMessageHistory');
export const GetThreadTurnPreview = dispatch('GetThreadTurnPreview');

// Usage accounting
export const GetUsageStats = dispatch('GetUsageStats');
export const GetCodexAccountUsage = dispatch('GetCodexAccountUsage');

// Per-client UI view state (appStorage)
export const GetUIState = dispatch('GetUIState');

// Session import (provider session files → AO threads). ImportSessions is
// async on the backend: a mock returns the run handle and the test drives
// the rest through applyImportProgress / the session-import:progress channel.
export const ListImportableSessions = dispatch('ListImportableSessions');
export const ImportSessions = dispatch('ImportSessions');
export const CancelSessionImport = dispatch('CancelSessionImport');
export const CheckThreadImportUpdates = dispatch('CheckThreadImportUpdates');
export const ImportThreadUpdates = dispatch('ImportThreadUpdates');

// MCP (provider-native state)
export const ListThreadMcpServers = dispatch('ListThreadMcpServers');
export const ListWorkspaceMcpServers = dispatch('ListWorkspaceMcpServers');
export const SetThreadMcpServerEnabled = dispatch('SetThreadMcpServerEnabled');
export const SetWorkspaceMcpServerEnabled = dispatch('SetWorkspaceMcpServerEnabled');
export const ReconnectMcpServer = dispatch('ReconnectMcpServer');
export const GetMcpServerStatus = dispatch('GetMcpServerStatus');
export const RefreshMcpServerStatus = dispatch('RefreshMcpServerStatus');
export const TriggerMcpAuth = dispatch('TriggerMcpAuth');
export const TriggerWorkspaceMcpAuth = dispatch('TriggerWorkspaceMcpAuth');

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
// drift again — `bindings-app.test.ts` beside this file fails on a missing
// or vanished name in either direction.
export const CheckForUpdate = dispatch('CheckForUpdate');
export const DownloadUpdate = dispatch('DownloadUpdate');
export const RestartToUpdate = dispatch('RestartToUpdate');
export const CancelRestartToUpdate = dispatch('CancelRestartToUpdate');
export const ListReleases = dispatch('ListReleases');
export const ListArchivedThreads = dispatch('ListArchivedThreads');
export const UpdateProjectSortPositions = dispatch('UpdateProjectSortPositions');
export const ProviderTerminalAttach = dispatch('ProviderTerminalAttach');
export const ProviderTerminalDetach = dispatch('ProviderTerminalDetach');
export const ProviderTerminalInput = dispatch('ProviderTerminalInput');
export const ProviderTerminalReplay = dispatch('ProviderTerminalReplay');
export const ProviderTerminalResize = dispatch('ProviderTerminalResize');
export const ProviderTerminalSetControl = dispatch('ProviderTerminalSetControl');
export const NotificationActivated = dispatch('NotificationActivated');
export const WorkflowScheduleResume = dispatch('WorkflowScheduleResume');
export const ReportUpdateInstallStatus = dispatch('ReportUpdateInstallStatus');
export const GetServiceUpdateStatus = dispatch('GetServiceUpdateStatus');
export const ListServiceReleases = dispatch('ListServiceReleases');
export const RequestServiceUpdate = dispatch('RequestServiceUpdate');
export const CancelServiceUpdate = dispatch('CancelServiceUpdate');

export const GetProviderUpdates = dispatch('GetProviderUpdates');
export const CheckProviderUpdates = dispatch('CheckProviderUpdates');
export const GetProviderUpdateNotes = dispatch('GetProviderUpdateNotes');
export const RunProviderUpdate = dispatch('RunProviderUpdate');

export const GetNativeNetworkConfig = dispatch('GetNativeNetworkConfig');

export const MintOwnDevicePairing = dispatch('MintOwnDevicePairing');

export const RegisterOwnDevice = dispatch('RegisterOwnDevice');

export const ReportNativeNetworkState = dispatch('ReportNativeNetworkState');

export const ListAsyncQuestions = dispatch('ListAsyncQuestions');
export const SubmitAsyncAnswers = dispatch('SubmitAsyncAnswers');
export const SetAsyncQuestionDismissed = dispatch('SetAsyncQuestionDismissed');

export const TrackerConnections = dispatch('TrackerConnections');
export const AddTrackerConnection = dispatch('AddTrackerConnection');
export const RenameTrackerConnection = dispatch('RenameTrackerConnection');
export const RemoveTrackerConnection = dispatch('RemoveTrackerConnection');
export const VerifyTrackerConnection = dispatch('VerifyTrackerConnection');
export const SetTrackerConnectionEnabled = dispatch('SetTrackerConnectionEnabled');
export const SetTrackerConnectionTokenExpiry = dispatch('SetTrackerConnectionTokenExpiry');
export const EditTrackerKeyFile = dispatch('EditTrackerKeyFile');
export const OpenTrackerTokenPage = dispatch('OpenTrackerTokenPage');

export const TrackerProjects = dispatch('TrackerProjects');
export const TrackerBoards = dispatch('TrackerBoards');
export const ProjectTrackerBoards = dispatch('ProjectTrackerBoards');
export const TrackerBoardChoices = dispatch('TrackerBoardChoices');
export const SetProjectTrackerBoards = dispatch('SetProjectTrackerBoards');
export const ProjectTrackerSetup = dispatch('ProjectTrackerSetup');
export const ProjectTrackerStatuses = dispatch('ProjectTrackerStatuses');
export const ProjectTrackerFields = dispatch('ProjectTrackerFields');
export const SetProjectTrackerSetup = dispatch('SetProjectTrackerSetup');
export const SubscribeTrackerBoard = dispatch('SubscribeTrackerBoard');
export const TrackerBoard = dispatch('TrackerBoard');
export const TrackerSectionRows = dispatch('TrackerSectionRows');
export const TrackerBoardPatch = dispatch('TrackerBoardPatch');
export const SetTrackerSubscriptionVisible = dispatch('SetTrackerSubscriptionVisible');
export const RefreshTrackerSubscription = dispatch('RefreshTrackerSubscription');
export const ReleaseTrackerSubscription = dispatch('ReleaseTrackerSubscription');
export const TrackerDashboards = dispatch('TrackerDashboards');
export const EvaluateTrackerDashboard = dispatch('EvaluateTrackerDashboard');
export const TrackerDashboardDrill = dispatch('TrackerDashboardDrill');

export const SubscribeTrackerTicket = dispatch('SubscribeTrackerTicket');
export const TrackerTicket = dispatch('TrackerTicket');
export const MintTrackerAttachmentUploadTicket = dispatch('MintTrackerAttachmentUploadTicket');
export const MintTrackerAttachmentDownloadTicket = dispatch('MintTrackerAttachmentDownloadTicket');
export const TrackerBoardProjects = dispatch('TrackerBoardProjects');
export const TrackerIssueTypes = dispatch('TrackerIssueTypes');
export const TrackerCreateFields = dispatch('TrackerCreateFields');
export const TrackerMoves = dispatch('TrackerMoves');
export const TrackerAssignableUsers = dispatch('TrackerAssignableUsers');
export const TrackerChoices = dispatch('TrackerChoices');
export const TrackerLinkTypes = dispatch('TrackerLinkTypes');
export const TrackerLookup = dispatch('TrackerLookup');
export const TrackerSearch = dispatch('TrackerSearch');
export const TrackerComments = dispatch('TrackerComments');
export const CreateTrackerIssue = dispatch('CreateTrackerIssue');
export const EditTrackerIssue = dispatch('EditTrackerIssue');
export const MoveTrackerIssue = dispatch('MoveTrackerIssue');
export const SetTrackerEstimate = dispatch('SetTrackerEstimate');
export const RankTrackerIssues = dispatch('RankTrackerIssues');
export const MoveTrackerIssuesToSprint = dispatch('MoveTrackerIssuesToSprint');
export const CreateTrackerSprint = dispatch('CreateTrackerSprint');
export const StartTrackerSprint = dispatch('StartTrackerSprint');
export const CompleteTrackerSprint = dispatch('CompleteTrackerSprint');
export const AddTrackerComment = dispatch('AddTrackerComment');
export const EditTrackerComment = dispatch('EditTrackerComment');
export const DeleteTrackerComment = dispatch('DeleteTrackerComment');
export const LinkTrackerIssues = dispatch('LinkTrackerIssues');
export const UnlinkTrackerIssues = dispatch('UnlinkTrackerIssues');
export const AddTrackerWorklog = dispatch('AddTrackerWorklog');
export const EditTrackerSprint = dispatch('EditTrackerSprint');
export const EditTrackerWorklog = dispatch('EditTrackerWorklog');
export const DeleteTrackerWorklog = dispatch('DeleteTrackerWorklog');
export const TrackerWorklogs = dispatch('TrackerWorklogs');
export const RefreshTrackerWorklogs = dispatch('RefreshTrackerWorklogs');
export const TrackerPokerSupport = dispatch('TrackerPokerSupport');
export const FollowTrackerPoker = dispatch('FollowTrackerPoker');
export const TrackerPoker = dispatch('TrackerPoker');
export const VoteTrackerPoker = dispatch('VoteTrackerPoker');
export const RevealTrackerPoker = dispatch('RevealTrackerPoker');
export const ClearTrackerPokerVotes = dispatch('ClearTrackerPokerVotes');
export const SaveTrackerAttachment = dispatch('SaveTrackerAttachment');
