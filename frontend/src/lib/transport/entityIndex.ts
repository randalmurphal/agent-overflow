// Which backend owns which entity.
//
// Once a client is attached to more than one backend, "send this message
// to thread X" needs a machine as well as an id. Thread and project ids
// are globally unique (`internal/entityid`, the contract spec §10 states
// for exactly this reason), so nothing about a row has to change to make
// that resolvable — the client only has to REMEMBER where each row came
// from. This module is that memory, and ./runtime.ts's `thread` and
// `project` routes are its only hot reader.
//
// **Rows gain no field.** The alternative — stamping `backendId` onto
// every Thread and Project — costs a property on every row in the sidebar,
// makes two copies of one fact (the index would still be needed for ids
// whose row is not loaded), and puts a transport concern in a store type.
// A row that genuinely needs to RENDER its machine reads the index.
//
// Plain `Map`s and nothing else: no reactivity, no eviction policy beyond
// the explicit forget calls, no allocation on read. The bound is the
// number of threads and projects this client has seen, which is the same
// bound the sidebar already carries.
//
// **An id this index does not know resolves HOME**, not an error
// (./handle.ts states that fallback). That is what makes a single-backend
// app behave identically: it never populates the index for its own rows
// beyond what the fan-out notes, and every lookup that misses lands where
// it always did.

import { HOME_BACKEND, type BackendKey } from './backendKey';
import type { IdFamily } from './methodFamilies';

const threads = new Map<string, BackendKey>();
const projects = new Map<string, BackendKey>();
// An approval is answered against the backend that raised it, and the
// answer RPC names the approval rather than the thread. One extra map
// rather than a second lookup through the thread, because an approval can
// outlive the moment its thread row was loaded.
const approvalThreads = new Map<string, string>();
// The id families that are neither thread nor project and cannot be
// resolved through one: a workflow item and an automation belong to a
// project the caller may never have listed, a terminal is a live process
// on one machine, and a subscription id means nothing off the connection
// that minted it. ./methodFamilies.ts names which methods take each.
const workflowItems = new Map<string, BackendKey>();
const automations = new Map<string, BackendKey>();
const terminals = new Map<string, BackendKey>();
const subscriptions = new Map<string, BackendKey>();
// A thread group belongs to one project and so to one machine, but the
// group RPCs name the GROUP: resolved here rather than through the
// project because a group id arrives in the sidebar list before any
// caller has reason to look its project up.
const threadGroups = new Map<string, BackendKey>();

/** The backend that owns `threadId`, or undefined when unknown. */
export function threadBackend(threadId: string): BackendKey | undefined {
  return threads.get(threadId);
}

/** The backend that owns `projectId`, or undefined when unknown. */
export function projectBackend(projectId: string): BackendKey | undefined {
  return projects.get(projectId);
}

/**
 * The backend that raised `approvalId`, resolved through the thread it
 * belongs to. Undefined when either link is unknown.
 */
export function approvalBackend(approvalId: string): BackendKey | undefined {
  const threadId = approvalThreads.get(approvalId);
  return threadId === undefined ? undefined : threads.get(threadId);
}

/** The backend that owns `itemId` (a workflow item), or undefined. */
export function workflowItemBackend(itemId: string): BackendKey | undefined {
  return workflowItems.get(itemId);
}

/** The backend that owns `automationId`, or undefined. */
export function automationBackend(automationId: string): BackendKey | undefined {
  return automations.get(automationId);
}

/** The backend running `terminalId`, or undefined. */
export function terminalBackend(terminalId: string): BackendKey | undefined {
  return terminals.get(terminalId);
}

/** The backend that minted `subscriptionId`, or undefined. */
export function subscriptionBackend(subscriptionId: string): BackendKey | undefined {
  return subscriptions.get(subscriptionId);
}

export function noteThread(threadId: string, backendId: BackendKey): void {
  if (threadId === '') return;
  threads.set(threadId, backendId);
}

export function noteProject(projectId: string, backendId: BackendKey): void {
  if (projectId === '') return;
  projects.set(projectId, backendId);
}

export function noteApproval(approvalId: string, threadId: string): void {
  if (approvalId === '' || threadId === '') return;
  approvalThreads.set(approvalId, threadId);
}

export function noteWorkflowItem(itemId: string, backendId: BackendKey): void {
  if (itemId === '') return;
  workflowItems.set(itemId, backendId);
}

export function noteAutomation(automationId: string, backendId: BackendKey): void {
  if (automationId === '') return;
  automations.set(automationId, backendId);
}

export function noteTerminal(terminalId: string, backendId: BackendKey): void {
  if (terminalId === '') return;
  terminals.set(terminalId, backendId);
}

/**
 * Record where a subscription was OPENED. The only fact that can answer
 * "unsubscribe on which connection" — the id is minted by one backend and
 * is not a name any other would recognise.
 */
export function noteSubscription(subscriptionId: string, backendId: BackendKey): void {
  if (subscriptionId === '') return;
  subscriptions.set(subscriptionId, backendId);
}

/** The backend that owns thread group `groupId`, or undefined when unknown. */
export function threadGroupBackend(groupId: string): BackendKey | undefined {
  return threadGroups.get(groupId);
}

export function noteThreadGroup(groupId: string, backendId: BackendKey): void {
  if (groupId === '') return;
  threadGroups.set(groupId, backendId);
}

export function forgetThreadGroup(groupId: string): void {
  threadGroups.delete(groupId);
}

/** Release a subscription id once it has been closed. Unlike the entity
 *  maps these are unbounded in TIME rather than in row count, so the one
 *  path that ends a subscription is the one that must forget it. */
export function forgetSubscription(subscriptionId: string): void {
  subscriptions.delete(subscriptionId);
}

export function forgetThread(threadId: string): void {
  threads.delete(threadId);
}

export function forgetProject(projectId: string): void {
  projects.delete(projectId);
}

/** Drop every entity a backend owned. Called when it detaches. */
export function forgetBackendEntities(backendId: BackendKey): void {
  for (const [id, owner] of threads) {
    if (owner === backendId) threads.delete(id);
  }
  for (const [id, owner] of projects) {
    if (owner === backendId) projects.delete(id);
  }
  for (const map of [workflowItems, automations, terminals, subscriptions, threadGroups]) {
    for (const [id, owner] of map) {
      if (owner === backendId) map.delete(id);
    }
  }
}

// ---------------------------------------------------------------------------
// Population from the `all` fan-out
// ---------------------------------------------------------------------------

/**
 * Which entity a list method's rows are, keyed by the numeric method id.
 *
 * Hand-written, closed, and SHORT on purpose. The alternative considered
 * and rejected was sniffing each row's shape: a shape-based walker is
 * wrong the first time an unrelated payload happens to carry an `id` and a
 * `projectId`, and here being wrong means routing somebody's next message
 * to the wrong machine. Keying on the METHOD is keying on what was asked,
 * which cannot be ambiguous.
 *
 * `methodFamilies.test.ts` pins every id here against the generated route
 * table, so a regeneration that moves an id fails the suite rather than
 * quietly emptying the index.
 */
const ROW_ENTITY_BY_METHOD: Readonly<Record<number, 'thread' | 'project'>> = {
  1090132042: 'thread', // ListThreads
  2451527188: 'thread', // ListArchivedThreads
  2721360259: 'project', // ListProjects
};

/**
 * The `all` methods whose rows POINT AT a thread rather than being one.
 * A search hit carries `threadId`, and it is the only place a client
 * learns which machine holds a thread it has never listed — the archive
 * and the search reach further back than the sidebar does.
 */
const ROW_THREAD_REF_BY_METHOD: Readonly<Record<number, string>> = {
  3644945077: 'threadId', // SearchThreadMessages
};

/**
 * Record which backend a list call's rows came from.
 *
 * Called by the `all` fan-out with each backend's OWN share, before the
 * shares are merged — the merged value can no longer say which machine
 * each row belongs to, which is the whole reason the fan-out hands the
 * shares out one at a time.
 *
 * A method with no entry does nothing at all: most `all` methods answer
 * something that is not a row list, and guessing would be worse than
 * knowing nothing.
 */
export function noteRowsFromCall(
  methodId: number,
  result: unknown,
  backendId: BackendKey,
): void {
  const kind = ROW_ENTITY_BY_METHOD[methodId];
  if (kind !== undefined && Array.isArray(result)) {
    if (kind === 'thread') noteThreadRows(result, backendId);
    else noteProjectRows(result, backendId);
  }
  const ref = ROW_THREAD_REF_BY_METHOD[methodId];
  if (ref !== undefined && Array.isArray(result)) {
    for (const row of result) {
      const id = (row as Record<string, unknown> | null)?.[ref];
      if (typeof id === 'string' && id !== '') threads.set(id, backendId);
    }
  }
  noteFamilyRowsFromCall(methodId, result, backendId);
}

/**
 * Which family the rows a method ANSWERS with belong to, and how to read
 * their id. The other half of `methodFamilies.ts`'s routing table: a
 * family id can only route once something has told the index where it
 * lives, and the only place that is knowable is the call that listed or
 * minted it. It lives HERE rather than beside that table so the import
 * runs one way — `methodFamilies` reads this module, never the reverse.
 *
 * Keyed by method, never sniffed from the row's shape — the same rule
 * `entityIndex.ts` states, and for the same reason: `id` on an unrelated
 * payload would silently index the wrong thing, and being wrong here sends
 * somebody's cancel to another machine.
 */
interface ResultFamily {
  readonly family: IdFamily;
  /** The row property carrying the id. */
  readonly key: string;
  /** True when the method answers one row rather than a list. */
  readonly single?: boolean;
}

const RESULT_FAMILIES: Readonly<Record<number, ResultFamily>> = {
  2247958725: { family: 'terminal', key: 'terminalID', single: true }, // OpenTerminal
  2319799628: { family: 'workflowAutomation', key: 'id' }, // WorkflowListAutomations
  2445206506: { family: 'terminal', key: 'terminalID' }, // ListTerminals
  3037887964: { family: 'workflowItem', key: 'id' }, // WorkflowListItems
  3272491649: { family: 'subscription', key: 'id', single: true }, // SubscribePRUpdates
  3282404643: { family: 'subscription', key: 'id', single: true }, // GitStatusSubscribe
  3613211765: { family: 'workflowItem', key: 'id' }, // WorkflowListUnresolvedItems
  1478438024: { family: 'threadGroup', key: 'id', single: true }, // CreateThreadGroup
  2176447381: { family: 'threadGroup', key: 'id' }, // ListThreadGroups
};

const FAMILY_NOTERS: Readonly<Record<IdFamily, (id: string, backendId: BackendKey) => void>> = {
  workflowItem: noteWorkflowItem,
  workflowAutomation: noteAutomation,
  terminal: noteTerminal,
  subscription: noteSubscription,
  threadGroup: noteThreadGroup,
  // A thread list names threads, which the thread registry already
  // notes; no call answers with one.
  threadList: () => {},
};

/**
 * Record the ids a call ANSWERED with, for the families that have no row
 * in any list this client already fans out (`methodFamilies.ts`'s
 * `RESULT_FAMILIES` below). A workflow item, an automation and a terminal are
 * only ever learned from the call that listed or opened them; a
 * subscription id only from the subscribe that minted it.
 */
export function noteFamilyRowsFromCall(
  methodId: number,
  result: unknown,
  backendId: BackendKey,
): void {
  const spec = RESULT_FAMILIES[methodId];
  if (spec === undefined || result === null || typeof result !== 'object') return;
  const note = FAMILY_NOTERS[spec.family];
  if (spec.single) {
    const id = (result as Record<string, unknown>)[spec.key];
    if (typeof id === 'string' && id !== '') note(id, backendId);
    return;
  }
  if (!Array.isArray(result)) return;
  for (const row of result) {
    if (row === null || typeof row !== 'object') continue;
    const id = (row as Record<string, unknown>)[spec.key];
    if (typeof id === 'string' && id !== '') note(id, backendId);
  }
}

/** Record a batch of thread rows. Exported for the stores that receive
 *  rows outside a list call (a resync, a replica cold open). */
export function noteThreadRows(rows: readonly unknown[], backendId: BackendKey): void {
  for (const row of rows) {
    const id = (row as { id?: unknown })?.id;
    if (typeof id === 'string' && id !== '') threads.set(id, backendId);
  }
}

/**
 * Record a batch of project rows. Accepts both shapes the app carries: a
 * bare `Project` and the `ProjectWithCounts` wrapper the sidebar list
 * answers with.
 */
export function noteProjectRows(rows: readonly unknown[], backendId: BackendKey): void {
  for (const row of rows) {
    const record = row as { id?: unknown; project?: { id?: unknown } };
    const id = typeof record?.project?.id === 'string' ? record.project.id : record?.id;
    if (typeof id === 'string' && id !== '') projects.set(id, backendId);
  }
}

/**
 * Record the backend a single thread row arrived from, for the paths that
 * carry one row rather than a list: a `thread:updated` event (whose origin
 * stamp names the connection), a create, a replica cold open (whose
 * database is per backend, so the stamp is known).
 *
 * Takes the origin's `backendId`, which may be the backend's UUID rather
 * than its registry id; both resolve through ./backends.ts, and storing
 * whichever arrived avoids a lookup on an event path.
 */
export function noteThreadRow(row: unknown, backendId: BackendKey): void {
  const id = (row as { id?: unknown })?.id;
  if (typeof id === 'string' && id !== '') threads.set(id, backendId);
}

/** Diagnostics and tests: how many entities are indexed. */
export function entityIndexSize(): {
  threads: number;
  projects: number;
  workflowItems: number;
  automations: number;
  terminals: number;
  subscriptions: number;
} {
  return {
    threads: threads.size,
    projects: projects.size,
    workflowItems: workflowItems.size,
    automations: automations.size,
    terminals: terminals.size,
    subscriptions: subscriptions.size,
  };
}

/** Test seam: forget everything. */
export function __resetEntityIndexForTest(): void {
  threads.clear();
  projects.clear();
  approvalThreads.clear();
  workflowItems.clear();
  automations.clear();
  terminals.clear();
  subscriptions.clear();
}

/**
 * The home backend's id, re-exported so a caller that has an origin stamp
 * and needs a fallback does not have to import two modules to say
 * "wherever this came from, or mine".
 */
export { HOME_BACKEND };
