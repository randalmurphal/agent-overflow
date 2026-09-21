import type { Item, SourceDiffReview, SourceProposedPlan } from '../types/models';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/internal/app/models';
import type { OutgoingSendOptions } from '../utils/sendOptions';
import { RegisterQueueItem } from './bindings';
import { isPendingFlushRow, parseUserMessageMeta } from '../utils/userMessageMeta';
import { createKeyedSignalRegistry, type KeyedSignalRegistry } from './keyedSignalRegistry.svelte';

/**
 * Pending send queue.
 *
 * `queueByThread` holds provisional local submissions and messages
 * registered with the backend but not yet written by the dispatch worker.
 * Provisional entries reconcile by sendId and never control dispatch.
 *
 * `flushedByThread` holds messages written to the provider whose
 * timeline row is not RENDERED anywhere yet. Both states render in the
 * same pending area above the composer.
 *
 * Zone 2's exit condition is rendering, not arrival: for one flushed
 * userItemId, "visible in the send-queue preview" XOR "visible in the
 * timeline" holds at every observable instant from `provider:queue_flushed`
 * onward. A row that reached the client but is refused window admission
 * (the pane is scrolled back, `hasMoreNewer`) or withheld by the reveal
 * gate is in neither timeline, so it keeps its Zone 2 entry. Panes call
 * `confirmFlushedByUserItemId` from their one reveal chokepoint when a
 * flush row becomes rendered; a thread with no mounted pane has no
 * timeline to be in, so the item-stream handler confirms on data arrival
 * there (eventsItemStream.ts). The other way out is the backend taking the
 * message back: a `provider:queue_state_changed` snapshot that names the
 * entry's queue id again (a requeued dispatch), or `provider:queue_restored`
 * returning it to the composer.
 *
 * There is deliberately NO memo of confirmations that arrive before their
 * Zone 2 entry exists. Such a memo has to outlive both authorities that
 * can answer "is this send still pending" — the mounted timeline and the
 * backend snapshot — and went stale exactly when a send was retried under
 * its deterministic `user:flush:<uuid5(sendId)>` id, dropping the retry
 * from Zone 1 without ever adding it to Zone 2. An entry re-added by a
 * replayed `queue_flushed` is corrected by the next render sync or by the
 * authoritative `replaceFlushedForThread` snapshot; that direction is the
 * safe one, because it can never leave a message in neither place.
 *
 * The store does not own dispatch decisions. RegisterQueueItem goes
 * through the backend RPC; replies and events confirm backend acceptance.
 */

/** Wire-side queue item shape — what the frontend stores in Zone 1
 * after `provider:queue_state_changed` arrives, and what
 * RegisterQueueItem returns. */
export interface QueueItem {
  /** Local presentation until acceptance; never sent as queue state. */
  submitting?: boolean;
  sendId?: string;
  id: string;
  threadId: string;
  message: string;
  attachmentIds: readonly string[];
  sourceProposedPlan?: SourceProposedPlan | null;
  revisionSourceProposedPlan?: SourceProposedPlan | null;
  revisionSourceCommentIds?: readonly string[];
  revisionSourceDiffReview?: SourceDiffReview | null;
  revisionSourceDiffCommentIds?: readonly string[];
  enqueuedAt: number;
}

/** What the provider has told us about a flushed message's delivery.
 *
 * Additive detail on top of Zone 2, never a precondition for it: only
 * Claude emits the acks behind this, and only on recent CLIs, so every
 * consumer must render correctly from `undefined`. See
 * docs/references/claude-wire.md §command_lifecycle. */
export interface FlushedLifecycle {
  state: 'queued' | 'started' | 'completed' | 'cancelled';
  /** Set only alongside `started`; absent when it could not be derived. */
  delivery?: 'mid_turn' | 'new_turn';
}

/** Zone 2 entry. Carries the queue id (frontend-allocated) and the
 * opaque backend-allocated user item id so the
 * "this row's Meta has provider_item_id" detection can clear the
 * marker. */
export interface FlushedItem {
  sendId?: string;
  queueItemId: string;
  userItemId: string;
  message: string;
  flushedAt: number;
  /** Undefined until the provider acks — and forever on a CLI that
   * never does. Never inferred from anything else. */
  lifecycle?: FlushedLifecycle;
}

const EMPTY_QUEUE: readonly QueueItem[] = Object.freeze([]);
const EMPTY_FLUSHED: readonly FlushedItem[] = Object.freeze([]);

// Per-thread reactive boxes rather than one SvelteMap: `hasQueueItems`
// feeds `isThreadWorking`, which every sidebar row evaluates — and for
// most rows the key is MISSING, which on a SvelteMap subscribes the
// reader to the whole-map version, so any thread's queue change
// invalidated every row. See keyedSignalRegistry.svelte.ts for the
// pattern. The empty sentinels double as the registries' empty values,
// so `{#each}` callers keep a stable identity when a zone drains.
const queueByThread = createKeyedSignalRegistry<readonly QueueItem[]>(EMPTY_QUEUE);
const flushedByThread = createKeyedSignalRegistry<readonly FlushedItem[]>(EMPTY_FLUSHED);
const queueRevisionByThread = new Map<string, number>();

// ---- Zone 1 (queued) reads ------------------------------------------

/** Read the current Zone 1 list for a thread. Stable empty array
 * sentinel when none — callers can `{#each ...}` without an
 * undefined guard. */
export function getQueueForThread(threadId: string | null | undefined): readonly QueueItem[] {
  if (!threadId) return EMPTY_QUEUE;
  return queueByThread.get(threadId);
}

/** Read the current Zone 2 list for a thread. */
export function getFlushedForThread(threadId: string | null | undefined): readonly FlushedItem[] {
  if (!threadId) return EMPTY_FLUSHED;
  return flushedByThread.get(threadId);
}

/** True when EITHER zone has at least one entry. Used by the working
 * indicator's bridge predicate so the spinner stays visible while
 * any in-flight queue activity exists. */
export function hasQueueItems(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return queueByThread.get(threadId).length > 0
    || flushedByThread.get(threadId).length > 0;
}

/** Monotonic revision for combined queued/flushed state stale-hydration guards. */
export function getQueueRevisionForThread(threadId: string | null | undefined): number {
  if (!threadId) return 0;
  return queueRevisionByThread.get(threadId) ?? 0;
}

function bumpQueueRevision(threadId: string): void {
  queueRevisionByThread.set(threadId, getQueueRevisionForThread(threadId) + 1);
}

type QueueZone<T> = KeyedSignalRegistry<readonly T[]>;

function replaceZoneItems<T>(
  zone: QueueZone<T>,
  threadId: string,
  items: readonly T[],
  empty: readonly T[],
): boolean {
  if (items.length === 0 && zone.get(threadId).length === 0) return false;
  zone.set(threadId, items.length === 0 ? empty : items);
  return true;
}

function appendZoneItems<T>(
  zone: QueueZone<T>,
  threadId: string,
  additions: readonly T[],
): boolean {
  if (additions.length === 0) return false;
  zone.set(threadId, [...zone.get(threadId), ...additions]);
  return true;
}

function filterZoneItems<T>(
  zone: QueueZone<T>,
  threadId: string,
  empty: readonly T[],
  keep: (item: T) => boolean,
): boolean {
  const current = zone.get(threadId);
  if (current.length === 0) return false;
  const next = current.filter(keep);
  if (next.length === current.length) return false;
  zone.set(threadId, next.length === 0 ? empty : next);
  return true;
}

function removeQueuedItemsById(threadId: string, queueItemIds: Set<string>): boolean {
  if (queueItemIds.size === 0) return false;
  return filterZoneItems(queueByThread, threadId, EMPTY_QUEUE, (item) => !queueItemIds.has(item.id));
}

function removeDispatchedQueueItems(
  threadId: string,
  items: readonly Pick<FlushedItem, 'queueItemId' | 'sendId'>[],
): boolean {
  const queueItemIds = new Set(items.map((item) => item.queueItemId));
  const sendIDs = new Set(items.map(item => item.sendId).filter(Boolean));
  return filterZoneItems(queueByThread, threadId, EMPTY_QUEUE,
    item => !queueItemIds.has(item.id) && !sendIDs.has(item.sendId));
}

// ---- Backend RPC mutations ------------------------------------------

/** Register a queued user message via the backend RPC. Backend
 * stores the item, emits `provider:queue_state_changed`, and the
 * event handler in events.ts updates Zone 1. Returns the
 * backend-resolved id+timestamp so the caller can reconcile
 * optimistically if needed.
 *
 * It takes the SAME `OutgoingSendOptions` the direct send path builds, from
 * the same `buildSendOptions` call. Queueing and sending are one decision
 * the composer makes about one message, so they carry one payload — which
 * is also what puts the send's idempotency id on both, since that id is
 * minted where the options are. A second option shape here was a second
 * place for the two paths to disagree. */
export async function registerQueueItem(
  threadId: string,
  message: string,
  options: OutgoingSendOptions,
  ready?: Promise<void>,
): Promise<QueueItem> {
  if (!threadId) {
    throw new Error('sendQueue.registerQueueItem: threadId is required');
  }
  const id = `submitting:${options.sendId}`;
  const pending: QueueItem = {
    id, threadId, message, enqueuedAt: Date.now(), submitting: true,
    sendId: options.sendId,
    attachmentIds: options.attachmentIds ?? [],
    sourceProposedPlan: options.sourceProposedPlan,
    revisionSourceProposedPlan: options.revisionSourceProposedPlan,
    revisionSourceCommentIds: options.revisionSourceCommentIds,
    revisionSourceDiffReview: options.revisionSourceDiffReview,
    revisionSourceDiffCommentIds: options.revisionSourceDiffCommentIds,
  };
  if (!queueByThread.get(threadId).some(item => item.sendId === options.sendId)
    && !flushedByThread.get(threadId).some(item => item.sendId === options.sendId)) {
    appendZoneItems(queueByThread, threadId, [pending]);
    bumpQueueRevision(threadId);
  }
  try {
    if (ready) await ready;
    const wire = await RegisterQueueItem(threadId, message, options);
    const item = queueItemFromWire(wire);
    // Events can acknowledge and even render the message before the RPC
    // replies. Only replace our still-present provisional entry.
    if (queueByThread.get(threadId).some(entry => entry.id === id)) {
      if (item.id.startsWith('user:')) {
        markItemsFlushed(threadId, [{ queueItemId: id, userItemId: item.id, message: item.message, sendId: options.sendId }]);
      } else {
        const current = queueByThread.get(threadId);
        queueByThread.set(threadId, current.some(entry => entry.id === item.id)
          ? current.filter(entry => entry.id !== id)
          : current.map(entry => entry.id === id ? item : entry));
        bumpQueueRevision(threadId);
      }
    }
    return item;
  } catch (err) {
    if (removeQueuedItemsById(threadId, new Set([id]))) bumpQueueRevision(threadId);
    throw err;
  }
}

// Note: the queue is READ from the backend in two places, and neither is
// here. The attach path takes it inside `GetThreadLiveState` (one round trip
// for the whole live snapshot — turn, queue, prompts, todos), and the
// transport-gap handler re-reads it alone through `GetQueueState`, because a
// gap on `provider:queue_state_changed` desynced the queue and nothing else.
// Both apply through `replaceQueueForThread` below under the same revision
// guard. A `fetchQueueState` wrapper here would be a third API for a job two
// callers already do correctly.

// ---- Event-handler surface (called from events.ts) -------------------

/** Replace the entire Zone 1 list for a thread. Called by the
 * `provider:queue_state_changed` handler — the snapshot in the event
 * payload is authoritative.
 *
 * A queue id naming a Zone 2 entry means the backend took that message
 * BACK: the eager Claude dispatch emits `provider:queue_flushed` before
 * the provider write settles, and a failed write requeues the item under
 * its original queue id (the requeue copies the input). Without this the
 * message rendered twice above the composer — once queued, once flushed —
 * which is the "never both" half of the invariant. The authoritative
 * snapshot wins, so the Zone 2 entry goes.
 *
 * A joined multi-item flush produces ONE Zone 2 entry, keyed on the first
 * member's queueItemId (markItemsFlushed dedupes on userItemId), and a
 * requeue of that batch returns every member id — including the first —
 * so the rule still maps. */
export function replaceQueueForThread(
  threadId: string,
  items: readonly QueueItem[],
): void {
  if (!threadId) return;
  const queuedIds = new Set(items.map((item) => item.id));
  const reclaimedFlushedItems = queuedIds.size > 0
    && filterZoneItems(
      flushedByThread,
      threadId,
      EMPTY_FLUSHED,
      (entry) => !queuedIds.has(entry.queueItemId),
    );
  const acceptedSendIDs = new Set(items.map(item => item.sendId).filter(Boolean));
  const submitting = queueByThread.get(threadId).filter(item => item.submitting && !acceptedSendIDs.has(item.sendId));
  const replacedQueuedItems = replaceZoneItems(queueByThread, threadId, [...items, ...submitting], EMPTY_QUEUE);
  if (reclaimedFlushedItems || replacedQueuedItems) bumpQueueRevision(threadId);
}

/** Replace Zone 2 from the backend's pending-send snapshot
 * (`GetThreadLiveState.flushedItems`). The snapshot names every send the
 * backend still considers unconfirmed — deferred rows that have no SQLite
 * row yet AND quiet rows it persisted without an item event. A loaded quiet
 * reservation remains in the preview until its pendingFlush flag clears.
 *
 * Installing it can re-add an entry whose row this client already renders
 * (the snapshot was sampled before the echo). The caller resolves that
 * immediately through its pane's render sync, in the same synchronous
 * hydration step, so no "both" instant is observable. */
export function replaceFlushedForThread(
  threadId: string,
  items: readonly FlushedItem[],
): void {
  if (!threadId) return;
  const removedQueuedItems = removeDispatchedQueueItems(threadId, items);
  const replacedFlushedItems = replaceZoneItems(flushedByThread, threadId, items, EMPTY_FLUSHED);
  if (removedQueuedItems || replacedFlushedItems) bumpQueueRevision(threadId);
}

/** Move a batch of items to Zone 2. Called by the
 * `provider:queue_flushed` handler. Items are added to Zone 2 with
 * the wall clock at handler time; the flushedAt is informational
 * only.
 *
 * Idempotent per userItemId: `provider:queue_flushed` rides the event
 * ring, so a reconnect replay re-delivers the frame, and a blind
 * append rendered the same pending message twice (and handed a
 * userItemId-keyed `{#each}` a duplicate key — an aborted flush,
 * utils/uniqueEachKeys.ts). An entry already in Zone 2 keeps its
 * original flushedAt and lifecycle; the replay carries nothing newer.
 *
 * An entry is added even when this client already holds (or renders) the
 * row: the caller runs its panes' render sync straight after this call,
 * which is the one place allowed to decide the row is on screen. Adding
 * first and letting the timeline take it back is what makes "never
 * neither" hold for a re-flushed send whose row is no longer rendered. */
export function markItemsFlushed(
  threadId: string,
  items: readonly Pick<FlushedItem, 'queueItemId' | 'userItemId' | 'message' | 'sendId'>[],
): void {
  if (!threadId || items.length === 0) return;
  const now = Date.now();
  const removedQueuedItems = removeDispatchedQueueItems(threadId, items);
  const knownUserItemIds = new Set(
    flushedByThread.get(threadId).map((entry) => entry.userItemId),
  );
  const additions: FlushedItem[] = [];
  for (const item of items) {
    if (knownUserItemIds.has(item.userItemId)) continue;
    knownUserItemIds.add(item.userItemId);
    additions.push({
      sendId: item.sendId,
      queueItemId: item.queueItemId,
      userItemId: item.userItemId,
      message: item.message,
      flushedAt: now,
    });
  }
  const appendedFlushedItems = appendZoneItems(flushedByThread, threadId, additions);
  if (removedQueuedItems || appendedFlushedItems) {
    bumpQueueRevision(threadId);
  }
}

/** A canonical echo also acknowledges a submission whose queue event was lost. */
export function markQueuedItemConsumed(item: Item): void {
  if (item.kind !== 'user_text' || isPendingFlushRow(item)) return;
  const queued = queueByThread.get(item.threadId);
  if (queued.length === 0) return;
  const meta = parseUserMessageMeta(item.meta);
  const ids = new Set([meta.sendId, ...(Array.isArray(meta.joinedSendIds) ? meta.joinedSendIds : [])]);
  const matches = queued.filter(entry => entry.sendId && ids.has(entry.sendId));
  markItemsFlushed(item.threadId, matches.map(entry => ({
    queueItemId: entry.id, userItemId: item.id, message: item.summary, sendId: entry.sendId,
  })));
}

/** Hand a flushed message over from Zone 2 to the timeline.
 *
 * The two callers are the only two ways a row becomes visible somewhere
 * else: a pane reporting that it now RENDERS the row (admitted to its
 * window and at or before its reveal boundary), and the item-stream
 * handler for a thread no pane is mounted on, where the row's arrival is
 * all the confirmation that exists. No-op when nothing matches — a
 * confirmation with no entry means the handover already happened. */
export function confirmFlushedByUserItemId(
  threadId: string,
  userItemId: string,
): void {
  if (!threadId || !userItemId) return;
  const changed = filterZoneItems(
    flushedByThread,
    threadId,
    EMPTY_FLUSHED,
    (entry) => entry.userItemId !== userItemId,
  );
  if (changed) bumpQueueRevision(threadId);
}

/** Stamp a provider delivery ack onto its Zone 2 entry.
 *
 * Keyed by the backend-resolved `userItemId`, which the backend derives
 * from the pending-send registry — the frontend never sees the wire uuid.
 * An ack for an entry that is no longer pending (its echo already cleared
 * the marker, or it belongs to a direct send that never had one) is a
 * no-op: the row it described has moved on, and re-adding state for it
 * would resurrect a marker the user already watched disappear. */
export function applyFlushedLifecycle(
  threadId: string,
  userItemId: string,
  lifecycle: FlushedLifecycle,
): void {
  if (!threadId || !userItemId) return;
  const current = flushedByThread.get(threadId);
  let changed = false;
  const next = current.map((entry) => {
    if (entry.userItemId !== userItemId) return entry;
    if (
      entry.lifecycle?.state === lifecycle.state
      && entry.lifecycle?.delivery === lifecycle.delivery
    ) {
      return entry;
    }
    changed = true;
    return { ...entry, lifecycle };
  });
  if (!changed) return;
  flushedByThread.set(threadId, next);
  bumpQueueRevision(threadId);
}

export function removeRestoredQueueItems(
  threadId: string,
  restored: {
    queueItemIds?: readonly string[];
    userItemIds?: readonly string[];
  },
): void {
  if (!threadId) return;
  const queueItemIds = new Set(restored.queueItemIds ?? []);
  const userItemIds = new Set(restored.userItemIds ?? []);
  let changed = false;
  if (queueItemIds.size > 0) {
    changed = removeQueuedItemsById(threadId, queueItemIds) || changed;
  }
  if (userItemIds.size > 0 || queueItemIds.size > 0) {
    changed = filterZoneItems(flushedByThread, threadId, EMPTY_FLUSHED, (entry) => {
      if (userItemIds.has(entry.userItemId)) return false;
      if (queueItemIds.has(entry.queueItemId)) return false;
      return true;
    }) || changed;
  }
  if (changed) {
    bumpQueueRevision(threadId);
  }
}

/** Drop every entry in both zones for a thread. Called from
 * `clearThreadStatus` on thread archive/delete; also from the
 * thread-switch path so a previously-loaded thread's queue doesn't
 * bleed into the next. */
export function clearForThread(threadId: string): void {
  if (!threadId) return;
  const hadVisibleItems = queueByThread.get(threadId).length > 0
    || flushedByThread.get(threadId).length > 0;
  if (hadVisibleItems) bumpQueueRevision(threadId);
  queueByThread.drop(threadId);
  flushedByThread.drop(threadId);
}

// ---- Wire conversion -------------------------------------------------

/** Convert the generated Wails queue DTO to the local send-queue shape. */
export function queueItemFromWire(item: WireQueuedItem): QueueItem {
  return {
    sendId: item.sendId,
    id: item.id,
    threadId: item.threadId,
    message: item.message,
    attachmentIds: item.attachmentIds ? [...item.attachmentIds] : [],
    sourceProposedPlan: item.sourceProposedPlan ?? null,
    revisionSourceProposedPlan: item.revisionSourceProposedPlan ?? null,
    revisionSourceCommentIds: item.revisionSourceCommentIds
      ? [...item.revisionSourceCommentIds]
      : undefined,
    revisionSourceDiffReview: (item.revisionSourceDiffReview as SourceDiffReview | undefined) ?? null,
    revisionSourceDiffCommentIds: item.revisionSourceDiffCommentIds
      ? [...item.revisionSourceDiffCommentIds]
      : undefined,
    enqueuedAt: item.enqueuedAt,
  };
}

// ---- Test-only helpers -----------------------------------------------

/** Wipe every thread's queue + Zone 2. Production code uses
 * `clearForThread`; tests use this for fresh-fixture isolation.
 * Named to match the `resetForTest` convention in every other store
 * in this directory (threadStatuses.svelte.ts, threads.svelte.ts). */
export function resetForTest(): void {
  queueByThread.reset();
  flushedByThread.reset();
  queueRevisionByThread.clear();
}
