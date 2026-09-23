import { applyTimelineMutation } from './timelineSurfaces';
import { optimisticInterruptCut, noteHiddenInterruptItem } from './threadInterruptState.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';
import { threadBackend, HOME_BACKEND } from '../transport/entityIndex';
import { getUndoableSend, retireUndoableSend } from './composerSendUndo';
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
// Item-stream event batching: the provider:item_event ordered mutation
// queue (upsert/delta/meta/patch actions sharing one wire channel), its
// rAF-scheduled flush, per-item upsert validation, the item-upsert
// subscriber fan-out consumed by activityRailBackground and
// proposedPlans, and the discussion live-tail side-channel feed
// (assistant_text upserts/deltas from unmounted participant child
// threads routed through discussionLiveTail.ts — see
// feedDiscussionLiveTailUpserts). Fan-in target of events.ts's
// setupEventListeners.
import type { ItemDeltaEvent, ItemStreamEvent } from '../types/events';
import type { Item } from '../types/models';
import { iterPanes } from './panes.svelte';
import { confirmFlushedByUserItemId, markQueuedItemConsumed } from './sendQueue.svelte';
import { itemsRenderEqual } from './threadItems';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { isSmoothLiveContentKind } from './threadPaneShared';
import { lookupDiscussionLiveTail } from './discussionLiveTail';
import { isBoundedString, isFiniteNumber } from './eventsGuards';
import { compositeKey } from '../utils/compositeKey';
import { isPendingFlushRow } from '../utils/userMessageMeta';
import type { ThreadPaneIngest } from './threadPaneRoles';
import { itemEventQueued, itemEventsSettled, resetItemEventSettlement } from './itemEventSettlement';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

const itemUpsertSubscribers: Set<(item: Item) => void> = new Set();
const ITEM_EVENT_FLUSH_MAX_DELAY_MS = 50;
const ITEM_EVENT_FLUSH_MAX_EVENTS = 500;
const ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS = 2_000;
// Code-unit budgets supplement event counts: one event can contain far more
// text than hundreds of ordinary deltas. A single oversized event progresses
// alone; accepted events are never truncated or dropped.
const ITEM_EVENT_FLUSH_MAX_CHARS = 256 * 1024;
const ITEM_EVENT_QUEUE_FORCE_FLUSH_CHARS = 2 * 1024 * 1024;
/**
 * An accepted item event with what ingest derived from it, so the flush
 * reuses them instead of deriving them again: `chars` for the text budgets,
 * `rowKey` for the per-row ordering sets and delta coalescing, and the
 * transport `sequence` the revert fence compares. `sequence` is the one
 * mutable field: a revert stamps entries that arrived unsequenced.
 */
interface QueuedItemEvent {
  readonly evt: ItemStreamEvent;
  readonly chars: number;
  readonly rowKey: string;
  sequence: number | undefined;
}
let itemEventQueue: (QueuedItemEvent | undefined)[] = [];
let itemEventQueueChars = 0;

const cuts = new Map<string, { sequence: number; turnIndex: number; kept: Set<string>; launchId?: string; turnStartedSequence?: number; turnCompletedSequence?: number }>();
onThreadHistoryInvalidated((owns) => {
  for (const id of cuts.keys()) if (owns(id)) cuts.delete(id);
});

export function fenceConversationSnapshot(threadId: string, sequence: number, turns?: { turnStartedSequence?: number; turnCompletedSequence?: number }): void {
  cuts.set(threadId, { sequence, turnIndex: -1, kept: new Set(), ...turns, launchId: getTransportHelloFor(threadBackend(threadId) ?? HOME_BACKEND)?.launchId });
}

export function survivesRevertedTurnEvent(threadId: string, kind: 'turnStartedSequence' | 'turnCompletedSequence', sequence?: number): boolean {
  const cut = cuts.get(threadId);
  const launch = getTransportHelloFor(threadBackend(threadId) ?? HOME_BACKEND)?.launchId;
  if (cut?.launchId && launch && cut.launchId !== launch) return true;
  const boundary = cut?.[kind];
  return boundary === undefined || sequence === undefined || sequence > boundary;
}

function survivesCut(entry: QueuedItemEvent): boolean {
  const evt = entry.evt;
  const cut = cuts.get(evt.threadId);
  if (!cut) return true;
  const launch = getTransportHelloFor(threadBackend(evt.threadId) ?? HOME_BACKEND)?.launchId;
  if (cut.launchId && launch && cut.launchId !== launch) {
    cuts.delete(evt.threadId);
    return true;
  }
  if (entry.sequence === undefined || entry.sequence > cut.sequence) return true;
  if (evt.action === 'upsert') {
    return evt.item.turnIndex < cut.turnIndex
      || (evt.item.turnIndex === cut.turnIndex && cut.kept.has(evt.item.id));
  }
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    const item = pane.getItemById(evt.itemId);
    if (item && (item.turnIndex < cut.turnIndex || cut.kept.has(item.id))) return true;
  }
  return false;
}

/** Fence queued and not-yet-delivered pre-cut frames without flushing other threads. */
export function fenceRevertedItemEvents(cut: UserMessageRevertedEvent): void {
  if (typeof cut.itemEventSequence === 'number') {
    cuts.set(cut.threadId, {
      sequence: cut.itemEventSequence, turnIndex: cut.turnIndex,
      turnStartedSequence: cut.turnStartedSequence, turnCompletedSequence: cut.turnCompletedSequence,
      launchId: getTransportHelloFor(threadBackend(cut.threadId) ?? HOME_BACKEND)?.launchId,
      kept: new Set(cut.keptAnchorTurnItemIds ?? []),
    });
  }
  // Unsequenced test/legacy deliveries still have a known ordering boundary
  // once they are in this queue. Stamp only those already received.
  for (let i = itemEventQueueStart; i < itemEventQueue.length; i++) {
    const entry = itemEventQueue[i];
    if (entry?.evt.threadId !== cut.threadId || entry.sequence !== undefined) continue;
    entry.sequence = cut.itemEventSequence ?? 0;
  }
  if (!cuts.has(cut.threadId)) cuts.set(cut.threadId, {
    sequence: 0, turnIndex: cut.turnIndex, kept: new Set(cut.keptAnchorTurnItemIds ?? []),
  });
}

function itemEventChars(evt: ItemStreamEvent): number {
  const fields = evt.action === 'upsert' ? evt.item : evt.action === 'patch' ? evt.patch : evt;
  let chars = 0;
  for (const key in fields) {
    const value = (fields as unknown as Record<string, unknown>)[key];
    if (typeof value === 'string') chars += value.length;
  }
  return chars;
}
let itemEventQueueStart = 0;
let itemEventFlushFrame: number | null = null;
let itemEventFlushTimeout: number | null = null;

function requestFrame(callback: () => void): number {
  if (typeof requestAnimationFrame === 'function') {
    return requestAnimationFrame(callback);
  }
  return window.setTimeout(callback, 0);
}

function cancelFrame(handle: number): void {
  if (typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(handle);
  } else {
    window.clearTimeout(handle);
  }
}

function cancelItemEventFlushSchedule(): void {
  if (itemEventFlushFrame !== null) {
    cancelFrame(itemEventFlushFrame);
    itemEventFlushFrame = null;
  }
  if (itemEventFlushTimeout !== null) {
    window.clearTimeout(itemEventFlushTimeout);
    itemEventFlushTimeout = null;
  }
}

function scheduleItemEventFlush(): void {
  if (itemEventFlushFrame !== null || itemEventFlushTimeout !== null) return;
  itemEventFlushFrame = requestFrame(flushItemEventQueue);
  itemEventFlushTimeout = window.setTimeout(flushItemEventQueue, ITEM_EVENT_FLUSH_MAX_DELAY_MS);
}

export function resetItemEventQueue(): void {
  cancelItemEventFlushSchedule();
  itemEventQueue = [];
  itemEventQueueStart = 0;
  itemEventQueueChars = 0;
  resetItemEventSettlement();
  cuts.clear();
}

function isValidItemForThread(item: Item | null | undefined, threadId: string): item is Item {
  if (!item || item.threadId !== threadId) return false;
  if (!isBoundedString(item.id, 512) || item.id.trim() === '') return false;
  if (!isBoundedString(item.threadId, 512) || item.threadId.trim() === '') return false;
  if (!isFiniteNumber(item.turnIndex) || !isFiniteNumber(item.itemIndex)) return false;
  if (!isBoundedString(item.kind, 128)) return false;
  if (!isBoundedString(item.role, 128)) return false;
  if (!isBoundedString(item.status, 128)) return false;
  if (!isBoundedString(item.summary)) return false;
  if (item.payloadId !== undefined && !isBoundedString(item.payloadId, 512)) return false;
  if (item.payloadKind !== undefined && !isBoundedString(item.payloadKind, 128)) return false;
  if (item.payloadMeta !== undefined && !isBoundedString(item.payloadMeta)) return false;
  if (item.parentId !== undefined && !isBoundedString(item.parentId, 512)) return false;
  if (item.completionOf !== undefined && !isBoundedString(item.completionOf, 512)) return false;
  if (item.toolName !== undefined && !isBoundedString(item.toolName, 256)) return false;
  if (item.decision !== undefined && !isBoundedString(item.decision, 128)) return false;
  if (item.inputPayloadId !== undefined && !isBoundedString(item.inputPayloadId, 512)) return false;
  if (item.meta !== undefined && !isBoundedString(item.meta)) return false;
  if (!isFiniteNumber(item.createdAt) || !isFiniteNumber(item.updatedAt)) return false;
  if (!Number.isInteger(item.rev)) return false;
  return true;
}

export function onItemUpsert(handler: (item: Item) => void): () => void {
  itemUpsertSubscribers.add(handler);
  return () => {
    itemUpsertSubscribers.delete(handler);
  };
}

function notifyItemUpserts(items: Item[]): void {
  if (items.length === 0 || itemUpsertSubscribers.size === 0) return;
  const subscribers = [...itemUpsertSubscribers];
  for (const item of items) {
    for (const handler of subscribers) {
      handler(item);
    }
  }
}

function providerUpsertAdvancesLiveContent(existing: Item | undefined, incoming: Item): boolean {
  // A brand-new row opens the spring latch THROUGH THIS PREDICATE only
  // for text-like kinds. Non-text appends still stamp — but at the
  // pane's arm site (`armLiveContentAppendSpring` in threadPaneScroll.svelte.ts),
  // which shares the structural arm's restore gates (loading /
  // discussion / controller-attached); this ungated per-row predicate
  // must not duplicate that decision without them.
  //
  // `existing` comes from a snapshot taken BEFORE the batch applies, so a
  // same-batch insert+update burst for one row resolves both upserts down
  // this insert path — correct, because that row is still in its estimate
  // phase for the whole flush.
  if (!existing) return isSmoothLiveContentKind(incoming.kind);
  // An update to an existing row has no estimate phase — the row is
  // mounted and measured, so a change to any rendered field (status dot,
  // summary, tool input in meta, output preview in payloadMeta, approval
  // decision, backgrounded-launch chrome) is genuine content advancing
  // the bottom, whatever the kind. Sync-pinning those growths lands
  // whole-viewport teleports between spring glides: running Bash rows
  // growing their output preview per flush window and running→completed
  // result chrome both jumped (bug-report-20260702T184236Z). Render
  // equality deliberately ignores `createdAt`/`updatedAt` — a bump with
  // no rendered field change must not hold the latch open.
  return !itemsRenderEqual(existing, incoming);
}

/**
 * Feed a discussion child thread's `assistant_text` upserts to any
 * registered live-tail handlers, keyed by the item's OWN threadId — not
 * the pane-matching loop below it. Discussion participant threads have
 * no mounted pane (only the parent thread gets a ChannelView), so
 * without this side-channel their streaming text never reaches anyone.
 * `lookupDiscussionLiveTail` returns `undefined` for every ordinary chat
 * thread, so this is a single Map miss per thread on the common path.
 */
function feedDiscussionLiveTailUpserts(itemsByThread: Map<string, Item[]>): void {
  for (const [threadId, threadItems] of itemsByThread) {
    const handlers = lookupDiscussionLiveTail(threadId);
    if (!handlers || handlers.size === 0) continue;
    for (const item of threadItems) {
      if (item.kind !== 'assistant_text') continue;
      for (const handler of handlers) {
        handler.applyTailUpsert(threadId, item.id, item.summary);
      }
    }
  }
}

function applyItemUpserts(upserts: Item[]): void {
  if (upserts.length === 0) return;
  const itemsByThread = new Map<string, Item[]>();
  // Flushed user rows in this batch, per thread. The arrival of one is
  // NOT what clears its send-queue Zone 2 marker any more: the row lands
  // at the turn tail, behind any prose the reveal gate is still draining,
  // and a scrolled-back window (`hasMoreNewer`) refuses it outright — in
  // both cases the message would be in neither place. A pane decides for
  // its own thread, from its reveal chokepoint (`syncRenderedFlushRows`),
  // which `applyProviderItemUpserts` below always runs. The list here is
  // only for threads NOBODY has mounted.
  const flushRowIdsByThread = new Map<string, string[]>();
  for (const item of upserts) {
    markQueuedItemConsumed(item);
    const list = itemsByThread.get(item.threadId);
    if (list) {
      list.push(item);
    } else {
      itemsByThread.set(item.threadId, [item]);
    }
    if (item.kind === 'user_text' && item.id.includes(':flush:') && !isPendingFlushRow(item)) {
      const flushRowIds = flushRowIdsByThread.get(item.threadId);
      if (flushRowIds) flushRowIds.push(item.id);
      else flushRowIdsByThread.set(item.threadId, [item.id]);
    }
  }
  feedDiscussionLiveTailUpserts(itemsByThread);
  const changedThreadIds = new Set<string>();
  const activeThreadIds = new Set<string>();
  for (const pane of ingestPanes()) {
    const threadId = pane.threadId;
    if (!threadId) continue;
    const threadItems = itemsByThread.get(threadId);
    if (!threadItems) continue;
    activeThreadIds.add(threadId);
    const previousItemsById = new Map(
      threadItems.map((item) => [item.id, pane.getItemById(item.id)] as const),
    );
    const applied = pane.applyProviderItemUpserts(threadItems);
    if (applied) {
      changedThreadIds.add(threadId);
      for (const item of applied.changedItems) {
        const previous = previousItemsById.get(item.id);
        if (providerUpsertAdvancesLiveContent(previous, item)) {
          pane.markLiveContentAdvanced(item);
        }
      }
    }
  }
  for (const [threadId, items] of itemsByThread) applyTimelineMutation(threadId, { kind: 'upsert', items });
  // A thread with no mounted pane has no timeline for the row to be
  // visible in, and no window or reveal state to ask. Its Zone 2 entries
  // are unrendered by construction, so arrival is the only confirmation
  // that exists there — and the one the sidebar's working indicator
  // (`hasQueueItems`) needs, or a delivered message would keep that
  // thread spinning until someone opened it. Panes re-decide from their
  // own window when the thread is next mounted (hydration installs the
  // window and runs the same sync).
  if (flushRowIdsByThread.size > 0) {
    for (const [threadId, flushRowIds] of flushRowIdsByThread) {
      if (activeThreadIds.has(threadId)) continue;
      for (const itemId of flushRowIds) confirmFlushedByUserItemId(threadId, itemId);
    }
  }
  // Evict cached snapshots only when this batch produced an observable
  // active-pane change. Inactive threads still evict defensively because
  // we do not have their current item window available for value dedupe.
  // This keeps redundant active-thread echoes from invalidating the warm
  // re-entry cache and rebuilding rows for no visible data change.
  for (const threadId of itemsByThread.keys()) {
    const paneOpen = activeThreadIds.has(threadId);
    if (changedThreadIds.has(threadId) || !paneOpen) {
      evictStaleWindowCaches(threadId, paneOpen);
    }
  }
}

/**
 * Drop the cached copies of a thread's window once the wire shows the
 * thread has moved past them: the L1 snapshot always, the durable replica
 * only when no pane shows the thread.
 *
 * A mounted thread's replica entry stays: at ~10 Hz streaming, a readwrite
 * IndexedDB transaction per flush is exactly the per-frame cost the backend
 * contract was shaped to avoid (§14), and the envelope it would drop is a
 * valid window under its own older stamp. The switch-away snapshot and the
 * debounced write-back own the mounted thread's entry, and the next open
 * describes the rows this activity produced to the backend directly
 * (thread-replica-sync.md §3.4), so the newer window earns its own stamp.
 *
 * Item events reach only watched threads (`provider:item_event` is
 * entity-filtered), so a thread with no pane never evicts through them;
 * turn lifecycle events reach every client and cover that thread from
 * `applyTurnStarted` / `applyTurnCompleted`.
 */
export function evictStaleWindowCaches(threadId: string, paneOpen: boolean): void {
  threadItemCache.evict(threadId);
  if (!paneOpen) void removeReplicaWindow(threadId);
}

/**
 * Drop a row the backend deleted from every mounted pane, and retire the
 * send-queue entry that was waiting for it to render.
 *
 * The Zone 2 clear is not optional: the entry's exit condition is "its row is
 * rendered", and a row that no longer exists can never satisfy it, so without
 * this the message would sit above the composer forever. Its text is not lost
 * — the queue-boundary merge fold that removed the row put it in the surviving
 * row's summary, which arrives as an upsert in this same batch.
 *
 * The cache eviction mirrors the upsert path: a window that changed shape must
 * not be served from a warm snapshot that still holds the row.
 */
function applyItemRemoval(threadId: string, itemId: string): void {
  applyTimelineMutation(threadId, { kind: 'remove', itemId });
  let removed = false;
  for (const pane of ingestPanes()) {
    if (pane.threadId !== threadId) continue;
    if (pane.removeItemById(itemId, threadId)) removed = true;
  }
  confirmFlushedByUserItemId(threadId, itemId);
  if (removed) threadItemCache.evict(threadId);
}

function applyItemDelta(evt: ItemDeltaEvent): void {
  if (!evt || !evt.threadId || !evt.itemId || !evt.delta) return;
  if (!isBoundedString(evt.threadId, 512) || !isBoundedString(evt.itemId, 512)) return;
  if (!isBoundedString(evt.kind, 128) || !isBoundedString(evt.delta)) return;
  if (!isFiniteNumber(evt.updatedAt)) return;

  // Same discussion live-tail side-channel as feedDiscussionLiveTailUpserts
  // above, for the delta half of the wire. A no-op Map lookup for every
  // ordinary chat thread.
  if (evt.kind === 'assistant_text') {
    const handlers = lookupDiscussionLiveTail(evt.threadId);
    if (handlers) {
      for (const handler of handlers) {
        handler.applyTailDelta(evt.threadId, evt.itemId, evt.delta);
      }
    }
  }

  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.applyItemDelta(evt);
  }
}

/**
 * Validate one `provider:item_event` frame and queue it for the next flush.
 * Everything the flush needs from the event is derived here, once; the
 * flush trusts the queue and repeats none of it. `sequence` is the
 * frame's transport sequence, absent for an unsequenced delivery.
 */
export function applyItemStreamEvent(evt: ItemStreamEvent, sequence?: number): void {
  if (!evt || !evt.threadId) return;
  if (evt.action === 'upsert' && evt.item) {
    // Boundary validation only, and now the ONLY global work this
    // channel does: thread status, sidebar activity, and the durable
    // proposed-plan column all moved to wildcard channels, because this
    // channel is narrowed to the threads a client watches and a client
    // that is not watching would never have learned any of them.
    if (!isValidItemForThread(evt.item, evt.threadId)) return;
    if ((evt.item.kind === 'assistant_text' || evt.item.kind === 'tool_call')
      && getUndoableSend(evt.threadId)?.turnIndex === evt.item.turnIndex) retireUndoableSend(evt.threadId);
  } else if (evt.action === 'delta') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (evt.parentId !== undefined && !isBoundedString(evt.parentId, 512)) return;
    if (!isBoundedString(evt.kind, 128)) return;
    if (!isBoundedString(evt.delta) || evt.delta === '') return;
    if (!isFiniteNumber(evt.updatedAt)) return;
  } else if (evt.action === 'meta') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (!isBoundedString(evt.kind, 128)) return;
    if (!isBoundedString(evt.meta)) return;
    if (!isFiniteNumber(evt.updatedAt)) return;
  } else if (evt.action === 'remove') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
  } else if (evt.action === 'patch') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (evt.parentId !== undefined && !isBoundedString(evt.parentId, 512)) return;
    if (!evt.patch || typeof evt.patch !== 'object') return;
    if (evt.patch.status !== undefined && !isBoundedString(evt.patch.status, 128)) return;
    if (evt.patch.summary !== undefined && !isBoundedString(evt.patch.summary)) return;
    if (evt.patch.meta !== undefined && !isBoundedString(evt.patch.meta)) return;
    if (evt.patch.decision !== undefined && !isBoundedString(evt.patch.decision, 128)) return;
    if (evt.patch.updatedAt !== undefined && !isFiniteNumber(evt.patch.updatedAt)) return;
    // Required, unlike the fields above: the patch is what stamps a
    // settled streaming row's revision (types/events.ts ItemPatchEvent).
    if (!Number.isInteger(evt.patch.rev)) return;
  } else {
    return;
  }
  // The row key throws for an id carrying its separator, which fails this
  // one frame here rather than the flush batch it would have joined.
  const entry: QueuedItemEvent = {
    evt,
    chars: itemEventChars(evt),
    rowKey: compositeKey(evt.threadId, evt.action === 'upsert' ? evt.item.id : evt.itemId),
    sequence,
  };
  while (itemEventQueueStart < itemEventQueue.length &&
    (itemEventQueue.length - itemEventQueueStart >= ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS ||
      itemEventQueueChars + entry.chars > ITEM_EVENT_QUEUE_FORCE_FLUSH_CHARS)) {
    flushItemEventQueue();
  }
  itemEventQueue.push(entry);
  itemEventQueued();
  itemEventQueueChars += entry.chars;
  scheduleItemEventFlush();
}

type CoalescedDelta = ItemDeltaEvent & { chunks: string[] };
type DeferredDelta = { action: 'delta' } & CoalescedDelta;
type ItemRowEvent = Extract<ItemStreamEvent, { action: 'meta' | 'patch' }>;
type DeferredRowEvent = ItemRowEvent | DeferredDelta;

function coalescedDelta(evt: ItemDeltaEvent): CoalescedDelta {
  return {
    threadId: evt.threadId,
    itemId: evt.itemId,
    parentId: evt.parentId,
    kind: evt.kind,
    delta: '',
    updatedAt: evt.updatedAt,
    chunks: [evt.delta],
  };
}

function applyCoalescedDelta(delta: CoalescedDelta): void {
  const coalesced: ItemDeltaEvent = {
    threadId: delta.threadId,
    itemId: delta.itemId,
    parentId: delta.parentId,
    kind: delta.kind,
    delta: delta.chunks.join(''),
    updatedAt: delta.updatedAt,
  };
  applyItemDelta(coalesced);
  applyTimelineMutation(coalesced.threadId, { kind: 'delta', event: coalesced });
}

function applyItemRowEvent(evt: ItemRowEvent): void {
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    if (evt.action === 'meta') pane.applyItemMeta(evt);
    else pane.applyItemPatch(evt);
  }
  applyTimelineMutation(evt.threadId, evt.action === 'meta'
    ? { kind: 'meta', event: evt }
    : { kind: 'patch', event: evt });
}

export function flushItemEventQueue(): void {
  cancelItemEventFlushSchedule();
  if (itemEventQueueStart >= itemEventQueue.length) {
    itemEventQueue = [];
    itemEventQueueStart = 0;
    return;
  }

  const events: QueuedItemEvent[] = [];
  let chars = 0;
  let itemEventQueueEnd = itemEventQueueStart;
  while (itemEventQueueEnd < itemEventQueue.length && events.length < ITEM_EVENT_FLUSH_MAX_EVENTS) {
    const entry = itemEventQueue[itemEventQueueEnd]!;
    if (events.length > 0 && chars + entry.chars > ITEM_EVENT_FLUSH_MAX_CHARS) break;
    events.push(entry);
    chars += entry.chars;
    // Retire processed payload references immediately, even while a small
    // unprocessed tail keeps the backing queue array alive.
    itemEventQueue[itemEventQueueEnd++] = undefined;
  }
  itemEventQueueChars -= chars;
  if (itemEventQueueEnd >= itemEventQueue.length) {
    itemEventQueue = [];
    itemEventQueueStart = 0;
  } else {
    itemEventQueueStart = itemEventQueueEnd;
    if (itemEventQueueStart > ITEM_EVENT_FLUSH_MAX_EVENTS * 4) {
      itemEventQueue = itemEventQueue.slice(itemEventQueueStart);
      itemEventQueueStart = 0;
    }
  }
  // NOTE (2026-08-26, do not rebuild): rotating this flush across
  // mounted threads (one pane's commit per rAF when several stream at
  // once) was built and REFUTED by a controlled 3-pane clone-replay
  // A/B/A/B: busy p95 3.0/3.0ms merged vs 5.0/5.5ms rotated, worst
  // frame no better. The tall frames are flood-shaped. One pane's own
  // beat dominates them, and un-merging multiplies the per-flush fixed
  // costs this single batch amortizes. Ruling in
  // .claude/skills/perf-investigation/REFERENCE.md.
  const pendingUpserts: Item[] = [];
  const pendingUpsertItemKeys = new Set<string>();
  const notifiedUpserts: Item[] = [];
  // Each row's pending deltas, one coalesced lane per kind (a row's text
  // and thinking streams coalesce separately), keyed by row.
  const pendingDeltas = new Map<string, CoalescedDelta[]>();
  // Row events that arrived after a pending upsert of their row. They apply
  // right after that upsert batch, in arrival order, so rows created and then
  // patched, re-tagged or streamed within one flush still commit once.
  const deferredRowEvents: DeferredRowEvent[] = [];
  const deferredRowItemKeys = new Set<string>();
  // Each row's newest deferred event when it is a delta, so consecutive
  // chunks for that row coalesce into it.
  const deferredDeltaTails = new Map<string, DeferredDelta>();

  const flushPendingUpserts = () => {
    if (pendingUpserts.length === 0) return;
    applyItemUpserts(pendingUpserts);
    notifiedUpserts.push(...pendingUpserts);
    pendingUpserts.length = 0;
    pendingUpsertItemKeys.clear();
    if (deferredRowEvents.length === 0) return;
    const deferred = deferredRowEvents.splice(0);
    deferredRowItemKeys.clear();
    deferredDeltaTails.clear();
    for (const evt of deferred) {
      if (evt.action === 'delta') applyCoalescedDelta(evt);
      else applyItemRowEvent(evt);
    }
  };

  const deferRowEvent = (evt: DeferredRowEvent, itemKey: string) => {
    deferredRowEvents.push(evt);
    deferredRowItemKeys.add(itemKey);
    if (evt.action === 'delta') deferredDeltaTails.set(itemKey, evt);
    else deferredDeltaTails.delete(itemKey);
  };

  const deferDelta = (evt: ItemDeltaEvent, itemKey: string) => {
    const tail = deferredDeltaTails.get(itemKey);
    if (tail?.kind === evt.kind) {
      tail.chunks.push(evt.delta);
      tail.updatedAt = Math.max(tail.updatedAt, evt.updatedAt);
      return;
    }
    deferRowEvent({ action: 'delta', ...coalescedDelta(evt) }, itemKey);
  };

  const queueDelta = (evt: ItemDeltaEvent, itemKey: string) => {
    const lanes = pendingDeltas.get(itemKey);
    if (!lanes) {
      pendingDeltas.set(itemKey, [coalescedDelta(evt)]);
      return;
    }
    for (const lane of lanes) {
      if (lane.kind !== evt.kind) continue;
      lane.chunks.push(evt.delta);
      lane.updatedAt = Math.max(lane.updatedAt, evt.updatedAt);
      return;
    }
    lanes.push(coalescedDelta(evt));
  };

  const flushPendingDeltas = () => {
    if (pendingDeltas.size === 0) return;
    for (const lanes of pendingDeltas.values()) {
      for (const delta of lanes) applyCoalescedDelta(delta);
    }
    pendingDeltas.clear();
  };

  // Apply order is preserved PER ITEM, not globally. Items are independent
  // in pane state, so cross-item reordering inside one rAF flush is safe,
  // and it is what keeps a tool burst (upserts, patches, metas and deltas of
  // many rows interleaved on the wire) applying as one upsert batch -> one
  // items-array swap -> one structural re-derive. A row event behind a
  // pending upsert of its row waits for that batch (`deferredRowEvents`);
  // the batch applies early only when a row it holds is upserted again
  // after such an event, or removed. A pending delta applies before any
  // later event of its row. A row's delta lanes apply together, in the
  // order the row first arrived, which reorders only across rows.
  try {
    for (const entry of events) {
      if (!survivesCut(entry)) continue;
      const { evt, rowKey: itemKey } = entry;
      const hiddenFrom = optimisticInterruptCut(evt.threadId);
      if (hiddenFrom !== null && evt.action === 'upsert' && evt.item.turnIndex >= hiddenFrom) {
        noteHiddenInterruptItem(evt.threadId);
        continue;
      }
      if (evt.action === 'upsert') {
        // A queued delta for this row must land before the upsert
        // replaces the row's summary wholesale, and so must an event
        // already waiting behind this row's pending upsert.
        if (pendingDeltas.has(itemKey)) flushPendingDeltas();
        if (deferredRowItemKeys.has(itemKey)) flushPendingUpserts();
        pendingUpserts.push(evt.item);
        pendingUpsertItemKeys.add(itemKey);
        continue;
      }
      if (evt.action === 'meta' || evt.action === 'patch') {
        // Meta is a re-validated blob (e.g. the live path-link allowlist
        // for an in-flight assistant_text row) and must land against text
        // the user has already seen, so the row's pending deltas apply
        // first. Both need the row to exist, so behind a pending upsert of
        // the row they wait for that batch.
        if (pendingDeltas.has(itemKey)) flushPendingDeltas();
        if (pendingUpsertItemKeys.has(itemKey)) deferRowEvent(evt, itemKey);
        else applyItemRowEvent(evt);
        continue;
      }
      if (evt.action === 'remove') {
        // The row is gone from the store. Anything this flush still holds
        // for it describes a row that no longer exists, so both buffers
        // drain first and the removal is applied last.
        if (pendingDeltas.has(itemKey)) flushPendingDeltas();
        if (pendingUpsertItemKeys.has(itemKey)) flushPendingUpserts();
        applyItemRemoval(evt.threadId, evt.itemId);
        continue;
      }
      if (evt.action !== 'delta') continue;

      // A pending upsert for this row carries the full summary; the
      // delta extends it, so it waits for that batch.
      if (pendingUpsertItemKeys.has(itemKey)) deferDelta(evt, itemKey);
      else queueDelta(evt, itemKey);
    }

    // Tail order is safe: no item can be pending in BOTH buffers here.
    // A delta is queued only for a row with no pending upsert, and
    // buffering an upsert flushes that row's pending delta first, so the
    // two pending sets are disjoint per item by the time we reach the
    // tail. Deferred events belong to rows of the upsert batch and apply
    // with it. Draining deltas then upserts therefore can't reorder any
    // single row's events.
    flushPendingDeltas();
    flushPendingUpserts();
    // Sidebar activity is bumped only at meaningful interaction
    // boundaries, and none of them is here any more: the reader's own
    // message arrives as a thread:updated `updatedAt` patch, and turn
    // completion / approval / user-input creation each ride their own
    // wildcard channel. Streaming deltas and assistant / tool / thinking
    // upserts never advanced the timestamp — that used to make the
    // sidebar reshuffle every chunk — so this channel now bumps nothing.
    notifyItemUpserts(notifiedUpserts);
  } finally {
    // One failed mutation/subscriber must not strand the untouched queue tail
    // after this flush cancelled both of its wakeups. The error still surfaces.
    if (itemEventQueueStart < itemEventQueue.length) scheduleItemEventFlush();
    itemEventsSettled(events.length);
  }
}
