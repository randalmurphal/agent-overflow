import type { SourceDiffReview, SourceProposedPlan } from '../types/models';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/models';
import * as bindings from './bindings';
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

/** Zone 2 entry. Carries the queue id (frontend-allocated) and the
 * backend-allocated `user:<turnIndex>:flush:<n>` id so the
 * "this row's Meta has provider_item_id" detection can clear the
 * marker. */
export interface FlushedItem {
  queueItemId: string;
  userItemId: string;
  message: string;
  flushedAt: number;
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
 * optimistically if needed. */
export async function registerQueueItem(
  threadId: string,
  message: string,
  options: {
    attachmentIds?: readonly string[];
    sourceProposedPlan?: SourceProposedPlan | null;
    revisionSourceProposedPlan?: SourceProposedPlan | null;
    revisionSourceCommentIds?: readonly string[];
    revisionSourceDiffReview?: SourceDiffReview | null;
    revisionSourceDiffCommentIds?: readonly string[];
  } = {},
): Promise<QueueItem> {
  if (!threadId) {
    throw new Error('sendQueue.registerQueueItem: threadId is required');
  }
  const wire = await bindings.RegisterQueueItem(threadId, message, {
    attachmentIds: options.attachmentIds ? [...options.attachmentIds] : undefined,
    sourceProposedPlan: options.sourceProposedPlan ?? undefined,
    revisionSourceProposedPlan: options.revisionSourceProposedPlan ?? undefined,
    revisionSourceCommentIds: options.revisionSourceCommentIds
      ? [...options.revisionSourceCommentIds]
      : undefined,
    revisionSourceDiffReview: options.revisionSourceDiffReview ?? undefined,
    revisionSourceDiffCommentIds: options.revisionSourceDiffCommentIds
      ? [...options.revisionSourceDiffCommentIds]
      : undefined,
  });
  return queueItemFromWire(wire);
}

// Note: `bindings.GetQueueState` exists for remote-client / re-attach
// bootstrap, but no caller wires it today (events drive Zone 1 in the
// running session). Re-add a `fetchQueueState` wrapper here when the
// bootstrap path needs it; keeping a dead helper around invites
// confusion about which API the composer should call.

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
 * only. */
export function markItemsFlushed(
  threadId: string,
  items: readonly { queueItemId: string; userItemId: string; message: string }[],
): void {
  if (!threadId || items.length === 0) return;
  const now = Date.now();
  const queueItemIds = new Set(items.map((item) => item.queueItemId));
  const removedQueuedItems = removeQueuedItemsById(threadId, queueItemIds);
  const additions: FlushedItem[] = [];
  for (const item of items) {
    if (isFlushedConfirmed(threadId, item.userItemId)) {
      forgetFlushedConfirmation(threadId, item.userItemId);
      continue;
    }
    additions.push({
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
