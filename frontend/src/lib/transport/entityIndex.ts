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

const threads = new Map<string, BackendKey>();
const projects = new Map<string, BackendKey>();
// An approval is answered against the backend that raised it, and the
// answer RPC names the approval rather than the thread. One extra map
// rather than a second lookup through the thread, because an approval can
// outlive the moment its thread row was loaded.
const approvalThreads = new Map<string, string>();

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
 * `methodRoutes.test.ts` pins every id here against its `Call.ByID(<n>`
 * site in the generated bindings, so a regeneration that moves an id fails
 * the suite rather than quietly emptying the index. When wave 7a's
 * `methodgen` emits the route table it can emit this column too, and this
 * constant goes with it.
 */
const ROW_ENTITY_BY_METHOD: Readonly<Record<number, 'thread' | 'project'>> = {
  1090132042: 'thread', // ListThreads
  2721360259: 'project', // ListProjects
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
  if (kind === undefined || !Array.isArray(result)) return;
  if (kind === 'thread') noteThreadRows(result, backendId);
  else noteProjectRows(result, backendId);
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
export function entityIndexSize(): { threads: number; projects: number } {
  return { threads: threads.size, projects: projects.size };
}

/** Test seam: forget everything. */
export function __resetEntityIndexForTest(): void {
  threads.clear();
  projects.clear();
  approvalThreads.clear();
}

/**
 * The home backend's id, re-exported so a caller that has an origin stamp
 * and needs a fallback does not have to import two modules to say
 * "wherever this came from, or mine".
 */
export { HOME_BACKEND };
