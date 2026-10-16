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
  4152403588: 'terminal', // RestartTerminal
  // Subscription and review-diff handle ids, which are meaningful ONLY on
  // the connection that minted them. Every caller pins that computer
  // (withBackendTarget); an unpinned call resolves no owner.
  1078249699: 'subscription', // SetPRUpdatesActive
  2888550814: 'subscription', // UnsubscribePRUpdates
  3263989430: 'subscription', // GitStatusUnsubscribe
  2473150982: 'subscription', // ReadReviewDiff
  504085501: 'subscription', // ReleaseReviewDiff

  // A thread group is a sidebar row of ONE project on one backend; its id
  // is minted there and the group list is fanned out to every backend, so
  // the index learns the owner from the list answer.
  723690026: 'threadGroup', // RenameThreadGroup
  4104302889: 'threadGroup', // DeleteThreadGroup

  // A batch of thread ids; the atomic write must belong to one backend.
  2514763466: 'threadList', // SetThreadGroup
};

const RESOLVERS: Readonly<Record<IdFamily, (id: string) => BackendKey | undefined>> = {
  workflowItem: workflowItemBackend,
  project: projectBackend,
  terminal: terminalBackend,
  subscription: () => undefined,
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
