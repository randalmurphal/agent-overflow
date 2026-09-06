// Routing for entity IDs the generated signature table cannot infer.
// A method declares its family and, for an input object, the exact field.
// Unknown ownership is refused with multiple computers; it never redirects
// an item operation to the first paired host. Tests pin these method IDs
// against the generated table and exercise nested input routing.

import type { BackendKey } from './backendKey';
import {
  automationBackend,
  projectBackend,
  subscriptionBackend,
  terminalBackend,
  resolveThreadBackend,
  threadGroupBackend,
  workflowItemBackend,
} from './entityIndex';

export type IdFamily =
  | 'project'
  | 'workflowItem'
  | 'workflowAutomation'
  | 'terminal'
  | 'subscription'
  // A sidebar thread group — argument 0 is `groupID`. Belongs to one
  // project and so to one machine; learned from ListThreadGroups and the
  // `thread-group:updated` frames.
  | 'threadGroup'
  // A LIST of thread ids — argument 0 is `threadIDs`. Recheck every owner:
  // a conversation can move after a sidebar selection was constructed.
  | 'threadList';

export const ROUTE_BY_ID_FAMILY: Readonly<Record<number, IdFamily | { family: IdFamily; field: string }>> = {
  4000394635: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentAddMemory
  4273669366: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentAmendSeeds
  76499272: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentGuideRun
  1146143060: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentInspectRun
  1978122086: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentListMemory
  3748461612: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentRunNarrative
  2308429865: { family: 'workflowItem', field: 'itemId' }, // WorkflowAgentWatchRun
  3011758347: { family: 'project', field: 'projectId' }, // WorkflowCreateAutomation
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

  // A batch of thread ids; the atomic write must belong to one backend.
  2514763466: 'threadList', // SetThreadGroup
};

const RESOLVERS: Readonly<Record<IdFamily, (id: string) => BackendKey | undefined>> = {
  workflowItem: workflowItemBackend,
  project: projectBackend,
  workflowAutomation: automationBackend,
  terminal: terminalBackend,
  subscription: subscriptionBackend,
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
