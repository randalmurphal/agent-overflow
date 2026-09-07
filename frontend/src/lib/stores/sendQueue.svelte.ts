import type { SourceDiffReview, SourceProposedPlan } from '../types/models';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/internal/app/models';
import type { OutgoingSendOptions } from '../utils/sendOptions';
import { RegisterQueueItem } from './bindings';
import { createKeyedSignalRegistry, type KeyedSignalRegistry } from './keyedSignalRegistry.svelte';

/**
 * Pending send queue.
 *
 * `queueByThread` holds messages registered with the backend but not
 * yet written to the provider by the dispatch worker.
 *
 * `flushedByThread` holds messages written to the provider but not
 * yet confirmed by the provider-visible user-message echo. Both states
 * render in the same pending area above the composer.
 *
 * The store does not own dispatch decisions. RegisterQueueItem goes
 * through the backend RPC; backend events are the source of truth.
 */

/** Wire-side queue item shape — what the frontend stores in Zone 1
 * after `provider:queue_state_changed` arrives, and what
 * RegisterQueueItem returns. */
export interface QueueItem {
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
const confirmedFlushedUserIdsByThread = new Map<string, Set<string>>();
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

function rememberFlushedConfirmation(threadId: string, userItemId: string): void {
  let confirmed = confirmedFlushedUserIdsByThread.get(threadId);
  if (!confirmed) {
    confirmed = new Set<string>();
    confirmedFlushedUserIdsByThread.set(threadId, confirmed);
  }
  confirmed.add(userItemId);
}

function isFlushedConfirmed(threadId: string, userItemId: string): boolean {
  return confirmedFlushedUserIdsByThread.get(threadId)?.has(userItemId) ?? false;
}

function forgetFlushedConfirmation(threadId: string, userItemId: string): void {
  const confirmed = confirmedFlushedUserIdsByThread.get(threadId);
  if (!confirmed) return;
  confirmed.delete(userItemId);
  if (confirmed.size === 0) {
    confirmedFlushedUserIdsByThread.delete(threadId);
  }
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
): Promise<QueueItem> {
  if (!threadId) {
    throw new Error('sendQueue.registerQueueItem: threadId is required');
  }
  const wire = await RegisterQueueItem(threadId, message, options);
  return queueItemFromWire(wire);
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
 * payload is authoritative. */
export function replaceQueueForThread(
  threadId: string,
  items: readonly QueueItem[],
): void {
  if (!threadId) return;
  if (!replaceZoneItems(queueByThread, threadId, items, EMPTY_QUEUE)) return;
  bumpQueueRevision(threadId);
}

export function replaceFlushedForThread(
  threadId: string,
  items: readonly FlushedItem[],
): void {
  if (!threadId) return;
  const visibleItems = items.filter((item) => {
    if (!isFlushedConfirmed(threadId, item.userItemId)) return true;
    forgetFlushedConfirmation(threadId, item.userItemId);
    return false;
  });
  if (!replaceZoneItems(flushedByThread, threadId, visibleItems, EMPTY_FLUSHED)) return;
  bumpQueueRevision(threadId);
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
 * original flushedAt and lifecycle; the replay carries nothing newer. */
export function markItemsFlushed(
  threadId: string,
  items: readonly Pick<FlushedItem, 'queueItemId' | 'userItemId' | 'message' | 'sendId'>[],
): void {
  if (!threadId || items.length === 0) return;
  const now = Date.now();
  const queueItemIds = new Set(items.map((item) => item.queueItemId));
  const removedQueuedItems = removeQueuedItemsById(threadId, queueItemIds);
  const knownUserItemIds = new Set(
    flushedByThread.get(threadId).map((entry) => entry.userItemId),
  );
  const additions: FlushedItem[] = [];
  for (const item of items) {
    if (isFlushedConfirmed(threadId, item.userItemId)) {
      forgetFlushedConfirmation(threadId, item.userItemId);
      continue;
    }
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

/** Remove a Zone 2 entry by userItemId. Called when a timeline
 * `provider:item_event` upsert arrives with the matching id and a
 * `provider_item_id`, which is the provider-confirmed signal that the
 * queued message has landed in context. */
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
  if (changed) {
    bumpQueueRevision(threadId);
    return;
  }
  rememberFlushedConfirmation(threadId, userItemId);
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
  confirmedFlushedUserIdsByThread.delete(threadId);
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
  confirmedFlushedUserIdsByThread.clear();
  queueRevisionByThread.clear();
}
