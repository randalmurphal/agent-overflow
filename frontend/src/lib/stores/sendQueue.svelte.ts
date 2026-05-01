import { SvelteMap } from 'svelte/reactivity';
import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
import type { SourceProposedPlan } from '../types/models';

/**
 * QueueItem captures a user-typed message that the user submitted while a
 * wire round was already in flight. The frontend holds the message in an
 * ephemeral per-thread queue and dispatches the head-of-line item on the
 * next `provider:turn_completed` regardless of cause (success, error,
 * abort). Mirrors the per-message queue both reference UIs maintain —
 * Claude Code's `commandQueue` and Codex's `VecDeque<QueuedUserMessage>`.
 *
 * Attachments are captured as full `Attachment` objects (not just ids)
 * because click-to-edit pulls them back into the composer via
 * `draft.restoreDraftFor`, which expects the snapshot shape used by
 * `composerDraft.svelte.ts`. Re-fetching attachments by id at edit time
 * would round-trip through the backend and could fail if the attachment
 * record was evicted or rebound. The plan-revision metadata
 * (`revisionSourceProposedPlan`, `revisionSourceCommentIds`) is send-only:
 * it travels with the message to the backend but is intentionally NOT
 * restored to the composer on click-to-edit because it isn't part of the
 * editable composer surface.
 */
export interface QueueItem {
  id: string;
  message: string;
  attachments: readonly Attachment[];
  terminalChips: readonly TerminalChip[];
  sourceProposedPlan: SourceProposedPlan | null;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: readonly string[];
  enqueuedAt: number;
}

/**
 * Snapshot shape consumed by `draft.restoreDraftFor`. Click-to-edit pops
 * a queued item, builds this snapshot via `snapshotFromQueueItem`, and
 * pushes it back into the composer draft store. Plan-revision metadata
 * is excluded by design — it isn't editable composer state.
 */
export interface QueueItemDraftSnapshot {
  content: string;
  attachments: Attachment[];
  terminalChips: TerminalChip[];
  sourceProposedPlan: SourceProposedPlan | null;
}

// SvelteMap (`svelte/reactivity`) is the doc-recommended pattern for
// reactive Map state in Svelte 5: per-key .set / .delete are tracked
// individually so writers don't have to rebuild the binding on every
// update. Mirrors `activeTurnByThread` in `threadStatuses.svelte.ts`.
//
// Inner arrays are immutable: every mutation replaces the array via
// `.set(threadId, next)`. Readers that bind via `getQueueForThread`
// observe the swap and re-render. We never push/splice in place.
const queueByThread = new SvelteMap<string, readonly QueueItem[]>();

const EMPTY: readonly QueueItem[] = Object.freeze([]);

function nextItemId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback for environments where crypto.randomUUID is unavailable
  // (older happy-dom in the test runner). Random enough for a
  // process-local in-memory key — never persisted, never wire-stable.
  return `q-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/**
 * Read the current queue for a thread. Always returns a stable empty
 * array sentinel when the thread has no queue, so callers can safely
 * `{#each ...}` without an undefined guard.
 */
export function getQueueForThread(threadId: string): readonly QueueItem[] {
  if (!threadId) return EMPTY;
  return queueByThread.get(threadId) ?? EMPTY;
}

export function hasQueueItems(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  const items = queueByThread.get(threadId);
  return !!items && items.length > 0;
}

/**
 * Append a new queued message at the tail. Mints a fresh id and
 * timestamp; returns the id so the caller (Composer) can address the
 * item later if needed (rarely useful — cancel/edit happens through
 * the row's own button).
 */
export function enqueue(
  threadId: string,
  draft: Omit<QueueItem, 'id' | 'enqueuedAt'>,
): string {
  if (!threadId) throw new Error('sendQueue.enqueue: threadId is required');
  const id = nextItemId();
  const item: QueueItem = {
    id,
    enqueuedAt: Date.now(),
    message: draft.message,
    attachments: [...draft.attachments],
    terminalChips: [...draft.terminalChips],
    sourceProposedPlan: draft.sourceProposedPlan ?? null,
    revisionSourceProposedPlan: draft.revisionSourceProposedPlan,
    revisionSourceCommentIds: draft.revisionSourceCommentIds
      ? [...draft.revisionSourceCommentIds]
      : undefined,
  };
  const current = queueByThread.get(threadId) ?? EMPTY;
  queueByThread.set(threadId, [...current, item]);
  return id;
}

/**
 * Insert a fully-formed item at the head. Used by the drain failure
 * path to return the item that was just popped back to the front so
 * the user's next attempt picks it up first. Preserves the original
 * id/enqueuedAt so the queue preview doesn't visibly reshuffle.
 */
export function enqueueAtFront(threadId: string, item: QueueItem): void {
  if (!threadId) return;
  const current = queueByThread.get(threadId) ?? EMPTY;
  queueByThread.set(threadId, [item, ...current]);
}

/**
 * Remove and return the head-of-line item. Returns undefined when the
 * queue is empty; the drain trigger uses that to short-circuit.
 */
export function popFront(threadId: string): QueueItem | undefined {
  if (!threadId) return undefined;
  const current = queueByThread.get(threadId);
  if (!current || current.length === 0) return undefined;
  const [head, ...rest] = current;
  if (rest.length === 0) {
    queueByThread.delete(threadId);
  } else {
    queueByThread.set(threadId, rest);
  }
  return head;
}

/**
 * Remove and return a specific item by id. Click-to-edit uses this to
 * lift any queued item (not just the head) back into the composer.
 */
export function popItem(threadId: string, itemId: string): QueueItem | undefined {
  if (!threadId || !itemId) return undefined;
  const current = queueByThread.get(threadId);
  if (!current || current.length === 0) return undefined;
  const idx = current.findIndex((entry) => entry.id === itemId);
  if (idx < 0) return undefined;
  const removed = current[idx];
  const next = [...current.slice(0, idx), ...current.slice(idx + 1)];
  if (next.length === 0) {
    queueByThread.delete(threadId);
  } else {
    queueByThread.set(threadId, next);
  }
  return removed;
}

/**
 * Drop a specific item. Returns true when an item was removed, false
 * otherwise (item already gone, queue empty, ids mismatched).
 */
export function cancelItem(threadId: string, itemId: string): boolean {
  return popItem(threadId, itemId) !== undefined;
}

/**
 * Wipe the queue for a thread. Called from `clearThreadStatus` when a
 * thread is archived/deleted; the in-memory queue should not outlive
 * its thread.
 */
export function clearForThread(threadId: string): void {
  if (!threadId) return;
  queueByThread.delete(threadId);
}

/**
 * Build the snapshot a composer draft store can restore. Click-to-edit
 * pops the item, builds this, then hands it to
 * `draft.restoreDraftFor(threadId, snapshot)`. Plan-revision metadata
 * is intentionally dropped — it isn't editable composer state, it's
 * send-only routing.
 */
export function snapshotFromQueueItem(item: QueueItem): QueueItemDraftSnapshot {
  return {
    content: item.message,
    attachments: [...item.attachments],
    terminalChips: [...item.terminalChips],
    sourceProposedPlan: item.sourceProposedPlan ?? null,
  };
}

/**
 * Test-only helper. Wipes every thread's queue. Production code uses
 * `clearForThread`.
 */
export function resetSendQueueForTest(): void {
  queueByThread.clear();
}
