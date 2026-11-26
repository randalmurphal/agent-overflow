// Routing for entity IDs the generated signature table cannot infer.
// A method declares its family and, for an input object, the exact field.
// Unknown ownership is refused with multiple computers; it never redirects
// an item operation to the first paired host. Tests pin these method IDs
// against the generated table and exercise nested input routing.

import type { BackendKey } from './backendKey';
import {
  projectBackend,
  terminalBackend,
  resolveThreadBackend,
  threadGroupBackend,
  workflowItemBackend,
} from './entityIndex';

export type IdFamily =
  | 'project'
  | 'workflowItem'
  | 'terminal'
  | 'subscription'
  // A tracker connection id — argument 0 is `connectionID`, minted on one
  // computer. The caller pins the computer whose answer named it
  // (withBackendTarget); an unpinned call resolves no owner.
  | 'trackerConnection'
  // A sidebar thread group — argument 0 is `groupID`. Belongs to one
  // project and so to one machine; learned from ListThreadGroups and the
  // `thread-group:updated` frames.
  | 'threadGroup'
  // A LIST of thread ids — argument 0 is `threadIDs`. Recheck every owner:
  // a conversation can move after a sidebar selection was constructed.
  | 'threadList';

export const ROUTE_BY_ID_FAMILY: Readonly<Record<number, IdFamily | { family: IdFamily; field: string }>> = {
  595194384: { family: 'project', field: 'projectId' }, // UpdateNewThreadDefaults
  // Workflow ITEM ids — argument 0 is `itemID`. An item belongs to the
  // project it was started in, and therefore to that project's machine.
  70120675: 'workflowItem', // WorkflowGetItem
  658224978: 'workflowItem', // WorkflowScheduleResume
  819019128: 'workflowItem', // WorkflowFetchPRReviewComments
  1005356607: 'workflowItem', // WorkflowDropUnit
  1172404443: 'workflowItem', // WorkflowSendPRReviewCommentsToThread
  1236472344: 'workflowItem', // WorkflowDiscussPR
  1648002260: 'workflowItem', // WorkflowRetryUnit
  1792283305: 'workflowItem', // WorkflowCreateItemPR
  1931806823: 'workflowItem', // WorkflowBindThread
  1931942299: 'workflowItem', // WorkflowTakeOverUnit
  1986594501: 'workflowItem', // WorkflowRerunItem
  2006703348: 'workflowItem', // WorkflowUnbindThread
  2163033761: 'workflowItem', // WorkflowDiscardItem
  2570221545: 'workflowItem', // WorkflowRequestSoftStop
  2659721862: 'workflowItem', // WorkflowDiscardPreview
  2846965054: 'workflowItem', // WorkflowRetryFailedUnits
  3006532931: 'workflowItem', // WorkflowMergeItem
  3138507556: 'workflowItem', // WorkflowResumeItem
  3348479803: 'workflowItem', // WorkflowResolveGate
  3393508470: 'workflowItem', // WorkflowCompleteTakeover
  3764767257: 'workflowItem', // WorkflowPauseItem
  124764894: 'workflowItem', // WorkflowPauseItemAndEndSessions
  4150249282: 'workflowItem', // WorkflowAnswerQuestion
  4156752389: 'workflowItem', // WorkflowGetRunMap
  4158962817: 'workflowItem', // WorkflowCancelItem
  // Terminal ids — argument 0 is `terminalID`. A terminal is a live
  // process on one machine; there is no other machine it could mean.
  146795716: 'terminal', // WriteTerminal
  1887984285: 'terminal', // ResizeTerminal
  2329592604: 'terminal', // GetTerminalReplay
  2618043580: 'terminal', // RefreshTerminal
  2702963191: 'terminal', // CloseTerminal
  // Subscription and review-diff handle ids, which are meaningful ONLY on
  // the connection that minted them. Every caller pins that computer
  // (withBackendTarget); an unpinned call resolves no owner.
  1078249699: 'subscription', // SetPRUpdatesActive
  2888550814: 'subscription', // UnsubscribePRUpdates
  3263989430: 'subscription', // GitStatusUnsubscribe
  2473150982: 'subscription', // ReadReviewDiff
  504085501: 'subscription', // ReleaseReviewDiff
  3524633257: 'subscription', // TrackerBoard
  4150293291: 'subscription', // TrackerSectionRows
  2847247437: 'subscription', // TrackerBoardPatch
  2154788414: 'subscription', // SetTrackerSubscriptionVisible
  247632441: 'subscription', // RefreshTrackerSubscription
  1393115131: 'subscription', // ReleaseTrackerSubscription
  105684651: 'subscription', // TrackerTicket
  3749273758: 'subscription', // MintTrackerAttachmentDownloadTicket
  2943151489: 'subscription', // FollowTrackerPoker
  3990595752: 'subscription', // TrackerPoker
  1443136041: 'subscription', // SaveTrackerAttachment
  3554577306: 'subscription', // TrackerDashboards
  2206254666: 'subscription', // EvaluateTrackerDashboard
  124644262: 'subscription', // TrackerDashboardDrill
  2951524960: 'subscription', // KeepTrackerWidget
  350315805: 'subscription', // DismissTrackerWidget
  446977083: 'subscription', // RemoveTrackerWidget
  316926314: 'subscription', // DeleteTrackerDashboard

  // Tracker connection ids: every call that names a connection belongs to
  // the computer that holds it.
  611052833: 'trackerConnection', // RenameTrackerConnection
  3921109066: 'trackerConnection', // SetTrackerConnectionEnabled
  2967721565: 'trackerConnection', // SetTrackerConnectionTokenExpiry
  21628411: 'trackerConnection', // RemoveTrackerConnection
  1424176184: 'trackerConnection', // VerifyTrackerConnection
  1347598600: 'trackerConnection', // EditTrackerKeyFile
  4215069807: 'trackerConnection', // OpenTrackerTokenPage
  2816087609: 'trackerConnection', // TrackerProjects
  2450480430: 'trackerConnection', // TrackerBoards
  3193179933: 'trackerConnection', // SubscribeTrackerTicket
  118334031: 'trackerConnection', // MintTrackerAttachmentUploadTicket
  2749961211: 'trackerConnection', // TrackerIssueTypes
  2859662243: 'trackerConnection', // TrackerBoardProjects
  440800234: 'trackerConnection', // TrackerCreateFields
  3632796459: 'trackerConnection', // TrackerMoves
  3346989894: 'trackerConnection', // TrackerAssignableUsers
  1109598697: 'trackerConnection', // TrackerChoices
  2754714292: 'trackerConnection', // TrackerLinkTypes
  2194444141: 'trackerConnection', // TrackerLookup
  1584024747: 'trackerConnection', // TrackerSearch
  1808895141: 'trackerConnection', // TrackerComments
  3981856226: 'trackerConnection', // CreateTrackerIssue
  2133356294: 'trackerConnection', // EditTrackerIssue
  3688074763: 'trackerConnection', // MoveTrackerIssue
  2301582015: 'trackerConnection', // SetTrackerEstimate
  4067581165: 'trackerConnection', // RankTrackerIssues
  3001763889: 'trackerConnection', // MoveTrackerIssuesToSprint
  759363647: 'trackerConnection', // CreateTrackerSprint
  1961738625: 'trackerConnection', // StartTrackerSprint
  3259977090: 'trackerConnection', // CompleteTrackerSprint
  790914661: 'trackerConnection', // AddTrackerComment
  3416415152: 'trackerConnection', // EditTrackerComment
  1742610839: 'trackerConnection', // DeleteTrackerComment
  4036611847: 'trackerConnection', // LinkTrackerIssues
  1599312374: 'trackerConnection', // UnlinkTrackerIssues
  2437411241: 'trackerConnection', // AddTrackerWorklog
  3891799979: 'trackerConnection', // EditTrackerSprint
  670991272: 'trackerConnection', // EditTrackerWorklog
  2679403039: 'trackerConnection', // DeleteTrackerWorklog
  1862710581: 'trackerConnection', // TrackerWorklogs
  2905282814: 'trackerConnection', // RefreshTrackerWorklogs
  4253452655: 'trackerConnection', // TrackerPokerSupport
  141171752: 'trackerConnection', // VoteTrackerPoker
  3151648721: 'trackerConnection', // RevealTrackerPoker
  3142429700: 'trackerConnection', // ClearTrackerPokerVotes

  // A thread group is a sidebar row of ONE project on one backend; its id
  // is minted there and the group list is fanned out to every backend, so
  // the index learns the owner from the list answer.
  723690026: 'threadGroup', // RenameThreadGroup
  4104302889: 'threadGroup', // DeleteThreadGroup

  // A batch of thread ids; the atomic write must belong to one backend.
  2514763466: 'threadList', // SetThreadGroup
  2294583674: 'threadList', // SetThreadPlacement
};

const RESOLVERS: Readonly<Record<IdFamily, (id: string) => BackendKey | undefined>> = {
  workflowItem: workflowItemBackend,
  project: projectBackend,
  terminal: terminalBackend,
  subscription: () => undefined,
  trackerConnection: () => undefined,
  threadGroup: threadGroupBackend,
  threadList: resolveThreadBackend,
};

/**
 * The backend a family-keyed call belongs to, or `undefined` when this
 * method names no family or the index has never seen that id. The caller
 * distinguishes those cases: unknown family IDs require a sole computer.
 */
export function familyBackend(methodId: number, args: readonly unknown[]): BackendKey | undefined {
  const spec = ROUTE_BY_ID_FAMILY[methodId];
  if (spec === undefined) return undefined;
  const family = typeof spec === 'string' ? spec : spec.family;
  if (family === 'threadList' && Array.isArray(args[0])) {
    let backend: BackendKey | undefined;
    let unknown = false;
    for (const id of args[0]) {
      const owner = typeof id === 'string' && id !== '' ? resolveThreadBackend(id) : undefined;
      if (owner === undefined) unknown = true;
      else if (backend === undefined) backend = owner;
      else if (backend !== owner) throw new Error('Select conversations on the same computer to group them.');
    }
    if (backend !== undefined && unknown) throw new Error('A selected conversation is no longer available. Refresh the selection before grouping it.');
    const groupOwner = typeof args[1] === 'string' ? threadGroupBackend(args[1]) : undefined;
    if (backend !== undefined && groupOwner !== undefined && groupOwner !== backend) {
      throw new Error('The group and its conversations must be on the same computer.');
    }
    return backend;
  }
  const argument = args[0];
  const id = typeof spec === 'string' ? argument
    : argument && typeof argument === 'object' ? (argument as Record<string, unknown>)[spec.field] : undefined;
  if (typeof id !== 'string' || id === '') return undefined;
  return RESOLVERS[family](id);
}
