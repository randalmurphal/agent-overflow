// A computer on v0.0.19, as the window's calls see it: the methods this
// build added answer `method_not_found`, and the thread actions whose result
// this build added (ArchiveThread, DeleteThread) answer a success with
// `null`, as that release's dispatcher encodes a method that returns only
// an error. The list is the generated bindings at the tag against head.

import { setBindingMock } from '../mocks/bindings-app';
import { TransportError } from '../../lib/transport/wsClient';
import type { Settings } from '../../lib/types/settings';
import { makeSettings } from './settings';

/** The App methods this build has and v0.0.19 does not. */
export const METHODS_AFTER_V0_0_19 = [
  'AgentRosterSeeds', 'CancelThreadWake', 'CheckAgentProfiles', 'CreateAssistant', 'DeleteThreads',
  'FinishProviderAccountReset',
  'GetDeleteDependents', 'GetGitBlobImage', 'GetNativeMemoryStatus', 'GetStandingAgent',
  'GetThreadAgentRows', 'GetThreadRequestStates', 'GetThreadWakeDetail',
  'GetThreadWorkerSummary', 'ListAssistantPastThreads', 'ListStandingAgentWakes',
  'ListThreadAgentRows', 'ListThreadPastAgentRows', 'ListThreadRecurringJobs',
  'ListThreadWakes', 'ListThreadWorkerSummaries', 'MarkThreadsRead', 'ResolveForgeFacts',
  'SaveGitBlobImage', 'SendExternalThreadMessage', 'SetAssistantProjects', 'SetThreadPlacement',
  'SpendProviderAccountReset',
  'StartNextAssistantThread', 'StopThreadAgentIncoming', 'StopThreadAgentRows',
  'StopThreadAgentRow', 'ThreadToolWatch', 'WakeThreadWakeNow', 'WorkflowPauseItemAndEndSessions',
  'TrackerConnections', 'AddTrackerConnection', 'RenameTrackerConnection', 'SetTrackerConnectionEnabled',
  'SetTrackerConnectionTokenExpiry', 'RemoveTrackerConnection', 'VerifyTrackerConnection',
  'EditTrackerKeyFile', 'OpenTrackerTokenPage', 'TrackerProjects', 'TrackerBoards',
  'ProjectTrackerBoards', 'SetProjectTrackerBoards', 'TrackerBoardChoices', 'ProjectTrackerSetup', 'ProjectTrackerStatuses', 'ProjectTrackerFields', 'SetProjectTrackerSetup',
  'SubscribeTrackerBoard', 'TrackerBoard', 'TrackerSectionRows', 'TrackerBoardPatch',
  'SetTrackerSubscriptionVisible', 'RefreshTrackerSubscription', 'ReleaseTrackerSubscription',
  'SubscribeTrackerTicket', 'TrackerTicket', 'TrackerComments', 'TrackerAssignableUsers', 'TrackerChoices',
  'EditTrackerIssue', 'MoveTrackerIssue', 'SetTrackerEstimate', 'AddTrackerComment', 'EditTrackerComment',
  'DeleteTrackerComment', 'TrackerMoves', 'RankTrackerIssues', 'MoveTrackerIssuesToSprint',
  'CreateTrackerIssue', 'TrackerBoardProjects', 'TrackerIssueTypes', 'TrackerCreateFields', 'CreateTrackerSprint', 'StartTrackerSprint', 'CompleteTrackerSprint',
  'EditTrackerSprint', 'EditTrackerWorklog', 'DeleteTrackerWorklog', 'TrackerWorklogs', 'RefreshTrackerWorklogs',
  'MintTrackerAttachmentUploadTicket', 'MintTrackerAttachmentDownloadTicket',
  'SaveTrackerAttachment', 'TrackerLinkTypes', 'LinkTrackerIssues', 'UnlinkTrackerIssues', 'TrackerPokerSupport',
  'FollowTrackerPoker', 'TrackerPoker', 'VoteTrackerPoker', 'RevealTrackerPoker', 'ClearTrackerPokerVotes',
  'SetThreadTicket', 'ClearThreadTicket', 'ThreadsForTicket', 'GetTicketWork', 'StartTicketThread',
  'TrackerDashboards', 'EvaluateTrackerDashboard', 'TrackerDashboardDrill',
] as const;

/**
 * What v0.0.19 grants a device it gives full access: every scope that
 * release has, which holds no tracker scope.
 */
export const SCOPES_AT_V0_0_19 = [
  'threads:read', 'files:read', 'threads:operate', 'approvals:respond', 'threads:autonomy',
  'terminal:operate', 'preview:open', 'git:operate', 'attachments:write', 'settings:read',
  'settings:write', 'access:admin',
] as const;

/** The App methods that answer a value in this build and `null` on v0.0.19. */
export const RESULTLESS_AT_V0_0_19 = ['ArchiveThread', 'DeleteThread'] as const;

/** The settings keys this build has and v0.0.19's Settings does not. */
export const SETTINGS_AFTER_V0_0_19 = [
  'agentNotes', 'agentProfiles', 'assistantsEnabled', 'claudeOwnMemoryOff',
  'codexOwnMemoryOff', 'notifyThreadAttention', 'sharedMemoryEnabled', 'subThreadLevels',
] as const;

/** A settings answer shaped like v0.0.19's: this build's, less the keys it added. */
export function settingsAsV0019(overrides: Partial<Settings> = {}): Partial<Settings> {
  const answer: Partial<Settings> = makeSettings(overrides);
  for (const key of SETTINGS_AFTER_V0_0_19) delete answer[key];
  return answer;
}

/** What v0.0.19's dispatcher answers for a method it does not register. */
export function methodNotFound(): TransportError {
  return new TransportError('method_not_found', 'method not registered');
}

/**
 * Answers every method v0.0.19 lacks with `method_not_found`, and its
 * archive, delete, mark read and workflow pause with `null`. Each mock is a
 * vi.fn, so a test reads its calls through getBindingMock.
 */
export function answerAsV0019(): void {
  for (const name of METHODS_AFTER_V0_0_19) {
    setBindingMock(name, async () => { throw methodNotFound(); });
  }
  for (const name of [...RESULTLESS_AT_V0_0_19, 'MarkThreadRead', 'WorkflowPauseItem']) {
    setBindingMock(name, async () => null);
  }
}
