// Which ID FAMILY a method's first argument names, for the 59 bound
// methods keyed by an id that is neither a thread nor a project.
//
// `methodRoutes.ts` parks all of them on `home` — the generator infers
// `thread` and `project` from a first parameter it can recognise, and it
// cannot invent a family it has no vocabulary for. Home is the right
// FALLBACK (it is where they have always gone, and it is the only answer
// on a single-backend client) but it is the wrong answer once a workflow
// item, a terminal or a subscription lives on a second machine: the call
// would be refused, or worse, answered by the wrong machine's row of the
// same shape.
//
// So route resolution consults THIS table first, resolves the id through
// `entityIndex.ts`, and only falls through to the generated route when the
// index has never seen it. Hand-kept on purpose: a family is a fact about
// what an argument MEANS, which no signature scan can recover — `itemID`
// and `automationID` are both `string`. `methodFamilies.test.ts` pins every
// id here against the generated table, so a renamed method fails loudly
// rather than quietly reverting to home.
//
// Device, passkey and backend-profile admin families are deliberately
// absent: those are administration OF this client's own attachments and
// belong to the page's own backend for good.

import type { BackendKey } from './backendKey';
import {
  automationBackend,
  subscriptionBackend,
  terminalBackend,
  threadBackend,
  threadGroupBackend,
  workflowItemBackend,
} from './entityIndex';

export type IdFamily =
  | 'workflowItem'
  | 'workflowAutomation'
  | 'terminal'
  | 'subscription'
  // A sidebar thread group — argument 0 is `groupID`. Belongs to one
  // project and so to one machine; learned from ListThreadGroups and the
  // `thread-group:updated` frames.
  | 'threadGroup'
  // A LIST of thread ids — argument 0 is `threadIDs`. Every id in it lives
  // on one machine (a group gathers threads of one project), so the first
  // one names the backend for all of them.
  | 'threadList';

export const ROUTE_BY_ID_FAMILY: Readonly<Record<number, IdFamily>> = {
  // Workflow ITEM ids — argument 0 is `itemID`. An item belongs to the
  // project it was started in, and therefore to that project's machine.
  49502656: 'workflowItem', // WorkflowAgentRunStatus
  70120675: 'workflowItem', // WorkflowGetItem
  315193175: 'workflowItem', // WorkflowAgentRunOutput
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
  4150249282: 'workflowItem', // WorkflowAnswerQuestion
  4156752389: 'workflowItem', // WorkflowGetRunMap
  4158962817: 'workflowItem', // WorkflowCancelItem
  // Workflow AUTOMATION ids — argument 0 is `automationID`.
  8517788: 'workflowAutomation', // WorkflowAgentSetNotes
  536579134: 'workflowAutomation', // WorkflowUpdateAutomation
  642610548: 'workflowAutomation', // WorkflowSetAutomationEnabled
  864025136: 'workflowAutomation', // WorkflowAgentGetNotes
  1133480652: 'workflowAutomation', // WorkflowDeleteAutomation
  1934298592: 'workflowAutomation', // WorkflowSetJobNotes
  2615697354: 'workflowAutomation', // WorkflowRunAutomationNow
  3798011060: 'workflowAutomation', // WorkflowGetJobNotes
  // Terminal ids — argument 0 is `terminalID`. A terminal is a live
  // process on one machine; there is no other machine it could mean.
  146795716: 'terminal', // WriteTerminal
  1887984285: 'terminal', // ResizeTerminal
  2329592604: 'terminal', // GetTerminalReplay
  2618043580: 'terminal', // RefreshTerminal
  2702963191: 'terminal', // CloseTerminal
  4152403588: 'terminal', // RestartTerminal
  // Subscription ids, which are meaningful ONLY on the connection that
  // minted them. Recorded at subscribe time.
  1078249699: 'subscription', // SetPRUpdatesActive
  2888550814: 'subscription', // UnsubscribePRUpdates
  3263989430: 'subscription', // GitStatusUnsubscribe

  // A thread group is a sidebar row of ONE project on one backend; its id
  // is minted there and the group list is fanned out to every backend, so
  // the index learns the owner from the list answer.
  48743460: 'threadGroup', // UnpinThreadGroup
  723690026: 'threadGroup', // RenameThreadGroup
  842795367: 'threadGroup', // PinThreadGroup
  4104302889: 'threadGroup', // DeleteThreadGroup
  4218979176: 'threadGroup', // SetThreadGroupPinGroup

  // A batch of thread ids; every row of one write lives on one backend, so
  // the first id names it.
  2514763466: 'threadList', // SetThreadGroup
};

const RESOLVERS: Readonly<Record<IdFamily, (id: string) => BackendKey | undefined>> = {
  workflowItem: workflowItemBackend,
  workflowAutomation: automationBackend,
  terminal: terminalBackend,
  subscription: subscriptionBackend,
  threadGroup: threadGroupBackend,
  threadList: threadBackend,
};

/**
 * The backend a family-keyed call belongs to, or `undefined` when this
 * method names no family or the index has never seen that id. The caller
 * falls through to the generated route table, which parks these on home.
 */
export function familyBackend(methodId: number, args: readonly unknown[]): BackendKey | undefined {
  const family = ROUTE_BY_ID_FAMILY[methodId];
  if (family === undefined) return undefined;
  // A thread list is resolved through its first id; the rest are one id.
  const id = family === 'threadList' && Array.isArray(args[0]) ? args[0][0] : args[0];
  if (typeof id !== 'string' || id === '') return undefined;
  return RESOLVERS[family](id);
}
