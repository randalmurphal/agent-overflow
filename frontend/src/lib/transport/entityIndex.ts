// Entity ownership for routing. Projects have globally unique IDs; moves keep
// the conversation ID and advance a durable ownership epoch. Keep backend
// identity in this index, not duplicated on every sidebar row. Thread rows carry
// the epoch because offline catalogs must remain orderable across computers.
//
// Lists, thread-row events and matching per-computer catalogs populate the
// index. Search/patch hints cannot displace an already known owner. An unknown
// entity retains the single-computer HOME fallback; contradictory owners never
// get that fallback. Higher epochs supersede old cached rows and invalidate
// thread history/read state through the ownership notification.
//
// Bounded by the metadata rows already held by the frontend. Thread/project
// ownership also drives mounted controls, so these two maps track keyed reads.

import { SvelteMap } from 'svelte/reactivity';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import type { IdFamily } from './methodFamilies';
import type { WorkflowItemDetail, WorkflowRunMapView } from '../types/workflow';

interface ThreadOwner { backend: BackendKey; epoch?: number; conflict?: boolean }
const threads = new SvelteMap<string, ThreadOwner>();
// Ownership evidence only lives as long as the metadata requests it can
// invalidate. A late list may contain an archived ID this frontend has never
// indexed; forgetting the destination must not erase evidence for that read.
interface MetadataRead { claims: Map<string, ThreadOwner>; detached: boolean }
const metadataReads = new Map<BackendKey, Set<MetadataRead>>();
export interface ThreadMetadataRead {
  verify(result: unknown): void;
  release(): void;
}
export function captureThreadMetadataRead(methodId: number, backend: BackendKey): ThreadMetadataRead | undefined {
  const single = methodId === 1098302047; // GetThread
  const ref = ROW_THREAD_REF_BY_METHOD[methodId];
  if (!single && ROW_ENTITY_BY_METHOD[methodId] !== 'thread' && ref === undefined) return undefined;
  const held: MetadataRead = { claims: new Map(), detached: false };
  let pending = metadataReads.get(backend);
  if (!pending) { pending = new Set(); metadataReads.set(backend, pending); }
  pending.add(held);
  return {
    verify(result) {
      if (held.detached) throw new Error('Computer was removed during this read.');
      if (held.claims.size === 0) return;
      const rows = single ? [result] : Array.isArray(result) ? result : [];
      for (const row of rows) {
        if (!row || typeof row !== 'object') continue;
        const id = (row as Record<string, unknown>)[ref ?? 'id'];
        const claim = typeof id === 'string' ? held.claims.get(id) : undefined;
        const epoch = (row as { ownershipEpoch?: number }).ownershipEpoch ?? 0;
        if (claim && ((claim.epoch ?? 0) > epoch
          || ((claim.epoch ?? 0) === epoch && (claim.conflict || claim.backend !== backend)))) {
          throw new Error('Conversation ownership changed during this read. Refresh to load its current computer.');
        }
      }
    },
    release() {
      pending.delete(held);
      if (pending.size === 0 && metadataReads.get(backend) === pending) metadataReads.delete(backend);
    },
  };
}

function invalidateMetadataClaim(threadId: string, claim: ThreadOwner): void {
  const snapshot = { ...claim };
  for (const pending of metadataReads.values()) {
    for (const read of pending) read.claims.set(threadId, snapshot);
  }
}

const ownershipListeners = new Set<(threadId: string, previousBackend: BackendKey) => void>();
export function onThreadOwnershipChanged(listener: (threadId: string, previousBackend: BackendKey) => void): () => void {
  ownershipListeners.add(listener);
  return () => { ownershipListeners.delete(listener); };
}
const projects = new SvelteMap<string, BackendKey>();
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
  return threads.get(threadId)?.backend;
}

/** Read-only hints cannot displace a verified move or resolve conflicting owners. */
export function resolveThreadBackend(threadId: string): BackendKey | undefined {
  const owner = threads.get(threadId);
  if (owner?.conflict) throw new Error('Two computers claim this conversation. Reconnect them to verify its owner before continuing.');
  return owner?.backend;
}

/** Thread-scoped runtime frames from a retired owner cannot alter live state. */
export function currentThreadEvent(threadId: string, backend: BackendKey): boolean {
  const owner = threads.get(threadId);
  return !owner || (!owner.conflict && owner.backend === backend);
}

export function validOwnershipEpoch(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

/** Whether a catalog row still belongs to the newest known ownership. */
export function currentThreadRow(row: { id: string; ownershipEpoch?: number }, backend?: BackendKey): boolean {
  const owner = threads.get(row.id);
  return !owner || (!owner.conflict && (backend === undefined || owner.backend === backend) && (owner.epoch ?? 0) === (row.ownershipEpoch ?? 0));
}

/** The backend that owns `projectId`, or undefined when unknown. */
export function projectBackend(projectId: string): BackendKey | undefined {
  return projects.get(projectId);
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

export function noteThread(threadId: string, backendId: BackendKey, epoch?: unknown): boolean {
  if (threadId === '' || (epoch !== undefined && !validOwnershipEpoch(epoch))) return false;
  const previous = threads.get(threadId);
  if (previous) {
    // Patches and search references are hints. They never move an indexed id.
    if (epoch === undefined) return previous.backend === backendId && !previous.conflict;
    if (previous.epoch !== undefined) {
      if (epoch < previous.epoch) return false;
      if (epoch === previous.epoch) {
        if (previous.backend !== backendId) {
          previous.conflict = true;
          invalidateMetadataClaim(threadId, previous);
        }
        return !previous.conflict;
      }
    }
  }
  const current = { backend: backendId, epoch };
  threads.set(threadId, current);
  if ((epoch ?? 0) > 0 || (previous && previous.backend !== backendId)) invalidateMetadataClaim(threadId, current);
  if (previous && (previous.backend !== backendId || (previous.epoch ?? 0) !== (epoch ?? 0))) {
    for (const listener of ownershipListeners) listener(threadId, previous.backend);
  }
  return true;
}

export function noteProject(projectId: string, backendId: BackendKey): void {
  if (projectId === '') return;
  projects.set(projectId, backendId);
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

/**
 * Every entity id a backend owned, as detaching it drops them.
 *
 * The three named sets are the ones a ROW STORE holds
 * (`stores/threads.svelte.ts`, `stores/projects.svelte.ts`,
 * `stores/threadGroups.svelte.ts`); the rest have no store to tell.
 */
export interface ForgottenEntities {
  readonly threadIds: readonly string[];
  readonly projectIds: readonly string[];
  readonly threadGroupIds: readonly string[];
  readonly workflowItemIds: readonly string[];
}

/**
 * Drop every entity a backend owned, and ANSWER with what went.
 *
 * The answer is not a convenience: it is the only ordering-free way for a
 * row store to learn which rows to drop. A listener that asked the index
 * itself would have to run before this call, and "before" is exactly the
 * kind of ordering that is correct on the machine it was written on and
 * wrong on the next one. `./backends.ts` hands this value to
 * `onBackendDetached`, so the payload IS the fact and nothing has to be
 * sequenced against it.
 *
 * Every map is swept, including the ones no store mirrors: a leftover
 * entry would resolve a terminal or a subscription to a machine this
 * client no longer holds a socket to.
 */
export function forgetBackendEntities(backendId: BackendKey): ForgottenEntities {
  for (const read of metadataReads.get(backendId) ?? []) read.detached = true;
  metadataReads.delete(backendId);
  const threadIds: string[] = [];
  for (const [id, owner] of threads) {
    if (owner.backend === backendId) { threadIds.push(id); threads.delete(id); }
  }
  const projectIds = takeOwned(projects, backendId);
  const threadGroupIds = takeOwned(threadGroups, backendId);
  const workflowItemIds = takeOwned(workflowItems, backendId);
  for (const map of [automations, terminals, subscriptions]) {
    takeOwned(map, backendId);
  }
  return { threadIds, projectIds, threadGroupIds, workflowItemIds };
}

// Delete every entry `backendId` owns and answer with their ids. One pass,
// and the ids are collected before the deletes so the iteration is not
// asked to survive its own mutation.
function takeOwned(map: Map<string, BackendKey>, backendId: BackendKey): string[] {
  const owned: string[] = [];
  for (const [id, owner] of map) {
    if (owner === backendId) owned.push(id);
  }
  for (const id of owned) map.delete(id);
  return owned;
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
      if (typeof id === 'string' && id !== '') noteThread(id, backendId, (row as Record<string, unknown>).ownershipEpoch);
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
  1009082601: { family: 'workflowItem', key: 'id', single: true }, // WorkflowStartRun
  2615697354: { family: 'workflowItem', key: 'id', single: true }, // WorkflowRunAutomationNow
  1931806823: { family: 'workflowItem', key: 'id', single: true }, // WorkflowBindThread
  2006703348: { family: 'workflowItem', key: 'id', single: true }, // WorkflowUnbindThread
  3011758347: { family: 'workflowAutomation', key: 'id', single: true }, // WorkflowCreateAutomation
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
  project: noteProject,
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
  if (methodId === 1236472344 || methodId === 1172404443) { // WorkflowDiscussPR / WorkflowSendPRReviewCommentsToThread
    const row = result as { id?: string; ownershipEpoch?: number } | null;
    if (row?.id) noteThread(row.id, backendId, row.ownershipEpoch ?? 0);
  }
  // These reads name threads excluded from the ordinary sidebar catalog.
  // Only declared metadata fields teach ownership; outputs and artifacts
  // may contain arbitrary user data and must never be traversed for IDs.
  if (methodId === 70120675 && result && typeof result === 'object') { // WorkflowGetItem
    const detail = result as WorkflowItemDetail;
    if (detail.item?.id) noteWorkflowItem(detail.item.id, backendId);
    if (detail.item?.projectId) noteProject(detail.item.projectId, backendId);
    if (detail.item?.triageThreadId) noteThread(detail.item.triageThreadId, backendId);
    noteWorkflowThreads(detail, backendId);
  } else if (methodId === 4156752389 && result && typeof result === 'object') { // WorkflowGetRunMap
    const view = result as WorkflowRunMapView;
    for (const run of view.runs ?? []) {
      if (run.itemId) noteWorkflowItem(run.itemId, backendId);
      noteWorkflowThreads(run, backendId);
    }
  }
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

function noteWorkflowThreads(row: { phases?: { threadId?: string }[]; units?: { threadId?: string }[] }, backend: BackendKey): void {
  for (const phase of row.phases ?? []) if (phase.threadId) noteThread(phase.threadId, backend);
  for (const unit of row.units ?? []) if (unit.threadId) noteThread(unit.threadId, backend);
}

/** Record a batch of thread rows from one backend's share of a list call. */
function noteThreadRows(rows: readonly unknown[], backendId: BackendKey): void {
  for (const row of rows) {
    const record = row as { id?: unknown; ownershipEpoch?: unknown };
    const id = record?.id;
    if (typeof id === 'string' && id !== '') noteThread(id, backendId, record.ownershipEpoch ?? 0);
  }
}

/**
 * Record a batch of project rows. Accepts both shapes the app carries: a
 * bare `Project` and the `ProjectWithCounts` wrapper the sidebar list
 * answers with.
 */
function noteProjectRows(rows: readonly unknown[], backendId: BackendKey): void {
  for (const row of rows) {
    const record = row as { id?: unknown; project?: { id?: unknown } };
    const id = typeof record?.project?.id === 'string' ? record.project.id : record?.id;
    if (typeof id === 'string' && id !== '') projects.set(id, backendId);
  }
}

/** Test seam: forget everything. Every map, so one case cannot leak a
 *  group or a terminal into the next. */
export function __resetEntityIndexForTest(): void {
  metadataReads.clear();
  threads.clear();
  projects.clear();
  workflowItems.clear();
  automations.clear();
  terminals.clear();
  subscriptions.clear();
  threadGroups.clear();
}

/**
 * The home backend's id, re-exported so a caller that has an origin stamp
 * and needs a fallback does not have to import two modules to say
 * "wherever this came from, or mine".
 */
export { HOME_BACKEND };
