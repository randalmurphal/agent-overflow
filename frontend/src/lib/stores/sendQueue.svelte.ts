import { SvelteMap } from 'svelte/reactivity';
import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
import type { SourceDiffReview, SourceProposedPlan } from '../types/models';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/models';
import * as bindings from './bindings';

/**
 * Two-zone send queue.
 *
 * Zone 1 — "queued, retractable". The user typed during a wire round
 * and the message is sitting in the backend's per-thread queue
 * waiting for the next safe provider boundary. The frontend MIRRORS this state via
 * `provider:queue_state_changed` events; Zone 1 is therefore a
 * reactive projection of the backend, not authoritative.
 *
 * Zone 2 — "flushed, headed to history". The trigger fired and the
 * dispatcher began writing the user message to the provider. Zone 2
 * is the brief handoff between the queue overlay and the chat row:
 * populated by `provider:queue_flushed`, cleared when the matching
 * provider-confirmed `user_text` row appears with `provider_item_id`.
 * Bulk-cleared on thread switch / session teardown.
 *
 * The store does not own dispatch decisions. RegisterQueueItem and
 * UndoQueuedItems both go through the backend RPC; the backend's
 * `provider:queue_state_changed` echo is what mutates Zone 1 here.
 * Optimistic local mutation is intentionally NOT done — the backend
 * is the source of truth and racing it would violate Core Principle
 * 1 ("Go is triage + pipe").
 */

/** Wire-side queue item shape — what the frontend stores in Zone 1
 * after `provider:queue_state_changed` arrives, and what
 * RegisterQueueItem returns. Carries attachment IDs (not full
 * Attachment records) — the attachment store keys lookups by ID. */
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

/** Snapshot shape consumed by `composerDraft.restoreDraftFor`. The
 * UP-arrow retract path collapses every Zone 1 item into one
 * snapshot — the Claude TUI's `popAllEditable` shape. Plan-revision
 * metadata is intentionally dropped (send-only routing data; not
 * part of the editable composer surface). */
export interface QueueDraftSnapshot {
  content: string;
  attachments: Attachment[];
  terminalChips: TerminalChip[];
  sourceProposedPlan: SourceProposedPlan | null;
}

const queueByThread = new SvelteMap<string, readonly QueueItem[]>();
const flushedByThread = new SvelteMap<string, readonly FlushedItem[]>();
const queueRevisionByThread = new Map<string, number>();

const EMPTY_QUEUE: readonly QueueItem[] = Object.freeze([]);
const EMPTY_FLUSHED: readonly FlushedItem[] = Object.freeze([]);

// ---- Zone 1 (queued) reads ------------------------------------------

/** Read the current Zone 1 list for a thread. Stable empty array
 * sentinel when none — callers can `{#each ...}` without an
 * undefined guard. */
export function getQueueForThread(threadId: string | null | undefined): readonly QueueItem[] {
  if (!threadId) return EMPTY_QUEUE;
  return queueByThread.get(threadId) ?? EMPTY_QUEUE;
}

/** Read the current Zone 2 list for a thread. */
export function getFlushedForThread(threadId: string | null | undefined): readonly FlushedItem[] {
  if (!threadId) return EMPTY_FLUSHED;
  return flushedByThread.get(threadId) ?? EMPTY_FLUSHED;
}

/** True when EITHER zone has at least one entry. Used by the working
 * indicator's bridge predicate so the spinner stays visible while
 * any in-flight queue activity exists. */
export function hasQueueItems(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  const q = queueByThread.get(threadId);
  if (q && q.length > 0) return true;
  const f = flushedByThread.get(threadId);
  return !!f && f.length > 0;
}

/** True only when Zone 1 (the retractable queue) has items. The
 * UP-arrow retract handler gates on this; Zone 2 items are not
 * retractable. */
export function hasRetractableQueueItems(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  const q = queueByThread.get(threadId);
  return !!q && q.length > 0;
}

/** Monotonic Zone 1 revision for stale-hydration guards. */
export function getQueueRevisionForThread(threadId: string | null | undefined): number {
  if (!threadId) return 0;
  return queueRevisionByThread.get(threadId) ?? 0;
}

function bumpQueueRevision(threadId: string): void {
  queueRevisionByThread.set(threadId, getQueueRevisionForThread(threadId) + 1);
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

/** Drop every Zone 1 item via the backend RPC. Returns the dropped
 * items so the UP-arrow retract handler can combine them into a
 * single composer draft. */
export async function undoQueuedItems(threadId: string): Promise<QueueItem[]> {
  if (!threadId) return [];
  const wire = await bindings.UndoQueuedItems(threadId);
  if (!wire || wire.length === 0) return [];
  return wire.map(queueItemFromWire);
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
  if (items.length === 0 && !queueByThread.has(threadId)) return;
  bumpQueueRevision(threadId);
  if (items.length === 0) {
    queueByThread.delete(threadId);
    return;
  }
  queueByThread.set(threadId, items);
}

export function replaceFlushedForThread(
  threadId: string,
  items: readonly FlushedItem[],
): void {
  if (!threadId) return;
  if (items.length === 0 && !flushedByThread.has(threadId)) return;
  bumpQueueRevision(threadId);
  if (items.length === 0) {
    flushedByThread.delete(threadId);
    return;
  }
  flushedByThread.set(threadId, items);
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
  const additions: FlushedItem[] = items.map((item) => ({
    queueItemId: item.queueItemId,
    userItemId: item.userItemId,
    message: item.message,
    flushedAt: now,
  }));
  const current = flushedByThread.get(threadId) ?? EMPTY_FLUSHED;
  bumpQueueRevision(threadId);
  flushedByThread.set(threadId, [...current, ...additions]);
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
  const current = flushedByThread.get(threadId);
  if (!current || current.length === 0) return;
  const next = current.filter((entry) => entry.userItemId !== userItemId);
  if (next.length === current.length) return; // no match, no mutation
  bumpQueueRevision(threadId);
  if (next.length === 0) {
    flushedByThread.delete(threadId);
  } else {
    flushedByThread.set(threadId, next);
  }
}

/** Drop every entry in both zones for a thread. Called from
 * `clearThreadStatus` on thread archive/delete; also from the
 * thread-switch path so a previously-loaded thread's queue doesn't
 * bleed into the next. */
export function clearForThread(threadId: string): void {
  if (!threadId) return;
  if (!queueByThread.has(threadId) && !flushedByThread.has(threadId)) return;
  bumpQueueRevision(threadId);
  queueByThread.delete(threadId);
  flushedByThread.delete(threadId);
}

/** Build a draft snapshot from the union of Zone 1 items, in
 * arrival order. The UP-arrow retract handler uses this — every
 * queued message becomes one editable composer entry. Plan-revision
 * metadata is dropped because it isn't part of the composer's
 * editable surface (matches the user's "retract restores text +
 * attachments" expectation).
 *
 * Resolves attachment records via the supplied lookup so the
 * snapshot carries full Attachment objects (the shape
 * `composerDraft.restoreDraftFor` expects). */
export function combineForRetract(
  items: readonly QueueItem[],
  resolveAttachment: (id: string) => Attachment | undefined,
): QueueDraftSnapshot {
  const messages = items.map((item) => item.message).filter((line) => line.length > 0);
  const content = messages.join('\n\n');

  const seen = new Set<string>();
  const attachments: Attachment[] = [];
  for (const item of items) {
    for (const id of item.attachmentIds) {
      if (seen.has(id)) continue;
      seen.add(id);
      const a = resolveAttachment(id);
      if (a) attachments.push(a);
    }
  }

  // Plan refs: take the first non-null sourceProposedPlan; combining
  // multiple is not meaningful (different plans would imply different
  // implementation contexts).
  let sourcePlan: SourceProposedPlan | null = null;
  for (const item of items) {
    if (item.sourceProposedPlan) {
      sourcePlan = item.sourceProposedPlan;
      break;
    }
  }

  return {
    content,
    attachments,
    terminalChips: [],
    sourceProposedPlan: sourcePlan,
  };
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
  queueByThread.clear();
  flushedByThread.clear();
  queueRevisionByThread.clear();
}
