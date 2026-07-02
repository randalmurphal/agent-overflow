// Item-stream event batching: the provider:item_event ordered mutation
// queue (upsert/delta/meta/patch actions sharing one wire channel), its
// rAF-scheduled flush, per-item upsert validation, and the item-upsert
// subscriber fan-out consumed by activityRailBackground and
// workspaceChangeLock. Fan-in target of events.ts's setupEventListeners.
import type { ItemDeltaEvent, ItemStreamEvent } from '../types/events';
import type { Item } from '../types/models';
import { iterPanes } from './panes.svelte';
import { confirmFlushedByUserItemId } from './sendQueue.svelte';
import { itemsRenderEqual } from './threadItems';
import { threadItemCache } from './threadItemCache';
import { isSmoothLiveContentKind } from './threadPaneShared';
import { projectThreadItem } from './threadStatuses.svelte';
import { syncProposedPlanStatus, syncThreadActivity, userTextCountsAsActivity } from './eventsThreadRows';

const itemUpsertSubscribers: Set<(item: Item) => void> = new Set();
const ITEM_EVENT_FLUSH_MAX_DELAY_MS = 50;
const ITEM_EVENT_FLUSH_MAX_EVENTS = 500;
const ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS = 2_000;
const ITEM_EVENT_TEXT_FIELD_MAX_CHARS = 2_000_000;
let itemEventQueue: ItemStreamEvent[] = [];
let itemEventQueueStart = 0;
let itemEventFlushFrame: number | null = null;
let itemEventFlushTimeout: number | null = null;

interface PendingItemUpsert {
  item: Item;
  countsAsActivity?: boolean;
}

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
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

function isBoundedString(value: unknown, maxChars = ITEM_EVENT_TEXT_FIELD_MAX_CHARS): value is string {
  return typeof value === 'string' && value.length <= maxChars;
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

function itemUpsertCountsAsActivity(upsert: PendingItemUpsert): boolean {
  if (upsert.countsAsActivity !== undefined) return upsert.countsAsActivity;
  return userTextCountsAsActivity(upsert.item);
}

function providerUpsertAdvancesLiveContent(existing: Item | undefined, incoming: Item): boolean {
  // A brand-new row opens the spring latch only for text-like kinds: tool
  // rows enter the timeline at a virtual size estimate and remeasure a few
  // milliseconds later, and spring-chasing those transient targets is
  // visible WebKit stutter (the structural-append one-shot covers genuine
  // tail appends instead — see markStructuralContentPending).
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

function applyItemUpserts(upserts: PendingItemUpsert[]): void {
  if (upserts.length === 0) return;
  const itemsByThread = new Map<string, Item[]>();
  const userTextActivityByThread = new Map<string, number>();
  for (const upsert of upserts) {
    const { item } = upsert;
    const list = itemsByThread.get(item.threadId);
    if (list) {
      list.push(item);
    } else {
      itemsByThread.set(item.threadId, [item]);
    }
    // Zone 2 clears when a flush user_text row arrives in the
    // timeline — either via the normal deferred echo path (which
    // carries provider_item_id) or via the eager persist on
    // interrupt (which appears before the echo).
    if (item.kind === 'user_text' && item.id.includes(':flush:')) {
      confirmFlushedByUserItemId(item.threadId, item.id);
    }
    // user_text upserts are one of three sidebar-bump boundaries —
    // alongside provider:turn_completed and approval / user-input
    // request creation. assistant_text / thinking / tool_call / etc.
    // upserts deliberately do NOT advance the sidebar timestamp.
    if (itemUpsertCountsAsActivity(upsert) && Number.isFinite(item.updatedAt)) {
      const existing = userTextActivityByThread.get(item.threadId) ?? Number.NEGATIVE_INFINITY;
      if (item.updatedAt > existing) {
        userTextActivityByThread.set(item.threadId, item.updatedAt);
      }
    }
  }
  const changedThreadIds = new Set<string>();
  const activeThreadIds = new Set<string>();
  for (const pane of iterPanes()) {
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
      const hasLiveContentAdvance = applied.changedItems.some((item) =>
        providerUpsertAdvancesLiveContent(previousItemsById.get(item.id), item),
      );
      // A provider upsert that advances live content marks the
      // scroll-animation latch so the controller spring-chases. New
      // text-like rows and visible-field updates to any mounted row
      // stamp; new tool rows (estimate→remeasure churn) and
      // timestamp-only bumps deliberately do not — see
      // providerUpsertAdvancesLiveContent.
      if (hasLiveContentAdvance) pane.markLiveContentAdvanced();
    }
  }
  // Evict cached snapshots only when this batch produced an observable
  // active-pane change. Inactive threads still evict defensively because
  // we do not have their current item window available for value dedupe.
  // This keeps redundant active-thread echoes from invalidating the warm
  // re-entry cache and rebuilding rows for no visible data change.
  for (const threadId of itemsByThread.keys()) {
    if (changedThreadIds.has(threadId) || !activeThreadIds.has(threadId)) {
      threadItemCache.evict(threadId);
    }
  }
  for (const [threadId, updatedAt] of userTextActivityByThread) {
    syncThreadActivity(threadId, updatedAt);
  }
}

function applyItemDelta(evt: ItemDeltaEvent): void {
  if (!evt || !evt.threadId || !evt.itemId || !evt.delta) return;
  if (!isBoundedString(evt.threadId, 512) || !isBoundedString(evt.itemId, 512)) return;
  if (!isBoundedString(evt.kind, 128) || !isBoundedString(evt.delta)) return;
  if (!isFiniteNumber(evt.updatedAt)) return;

  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.applyItemDelta(evt);
  }
}

export function applyItemStreamEvent(evt: ItemStreamEvent): void {
  if (!evt || !evt.threadId) return;
  if (evt.action === 'upsert' && evt.item) {
    // Boundary validation only. Thread-status projection and
    // proposed-plan sync happen at flush time alongside the pane
    // apply — running them here gave every upsert WS message its own
    // global-store write + effect flush outside the rAF batch.
    if (!isValidItemForThread(evt.item, evt.threadId)) return;
  } else if (evt.action === 'delta') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (!isBoundedString(evt.kind, 128)) return;
    if (!isBoundedString(evt.delta) || evt.delta === '') return;
    if (!isFiniteNumber(evt.updatedAt)) return;
  } else if (evt.action === 'meta') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (!isBoundedString(evt.kind, 128)) return;
    if (!isBoundedString(evt.meta)) return;
    if (!isFiniteNumber(evt.updatedAt)) return;
  } else if (evt.action === 'patch') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (!evt.patch || typeof evt.patch !== 'object') return;
    if (evt.patch.status !== undefined && !isBoundedString(evt.patch.status, 128)) return;
    if (evt.patch.summary !== undefined && !isBoundedString(evt.patch.summary)) return;
    if (evt.patch.meta !== undefined && !isBoundedString(evt.patch.meta)) return;
    if (evt.patch.decision !== undefined && !isBoundedString(evt.patch.decision, 128)) return;
    if (evt.patch.updatedAt !== undefined && !isFiniteNumber(evt.patch.updatedAt)) return;
  } else {
    return;
  }
  if (itemEventQueue.length - itemEventQueueStart >= ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS) {
    flushItemEventQueue();
  }
  itemEventQueue.push(evt);
  scheduleItemEventFlush();
}

export function flushItemEventQueue(): void {
  cancelItemEventFlushSchedule();
  if (itemEventQueueStart >= itemEventQueue.length) {
    itemEventQueue = [];
    itemEventQueueStart = 0;
    return;
  }

  const itemEventQueueEnd = Math.min(
    itemEventQueueStart + ITEM_EVENT_FLUSH_MAX_EVENTS,
    itemEventQueue.length,
  );
  const events = itemEventQueue.slice(itemEventQueueStart, itemEventQueueEnd);
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
  const pendingUpserts: PendingItemUpsert[] = [];
  const pendingUpsertItemKeys = new Set<string>();
  const notifiedUpserts: Item[] = [];
  const pendingDeltas = new Map<string, ItemDeltaEvent & { chunks: string[] }>();
  const pendingDeltaItemKeys = new Set<string>();

  const itemConflictKey = (threadId: string, itemId: string): string =>
    `${threadId}\u0000${itemId}`;

  const flushPendingUpserts = () => {
    if (pendingUpserts.length === 0) return;
    const semanticUpserts = pendingUpserts.map((upsert) => upsert.item);
    applyItemUpserts(pendingUpserts);
    notifiedUpserts.push(...semanticUpserts);
    pendingUpserts.length = 0;
    pendingUpsertItemKeys.clear();
  };

  const queueDelta = (evt: ItemDeltaEvent) => {
    // Coalescing key includes kind (a row's text and thinking streams
    // coalesce separately); the per-item conflict key does not.
    const key = `${evt.threadId}\u0000${evt.itemId}\u0000${evt.kind}`;
    const existing = pendingDeltas.get(key);
    if (existing) {
      existing.chunks.push(evt.delta);
      existing.updatedAt = Math.max(existing.updatedAt, evt.updatedAt);
      return;
    }
    pendingDeltas.set(key, { ...evt, delta: '', chunks: [evt.delta] });
    pendingDeltaItemKeys.add(itemConflictKey(evt.threadId, evt.itemId));
  };

  const flushPendingDeltas = () => {
    if (pendingDeltas.size === 0) return;
    for (const delta of pendingDeltas.values()) {
      const coalesced: ItemDeltaEvent = {
        threadId: delta.threadId,
        itemId: delta.itemId,
        kind: delta.kind,
        delta: delta.chunks.join(''),
        updatedAt: delta.updatedAt,
      };
      applyItemDelta(coalesced);
    }
    pendingDeltas.clear();
    pendingDeltaItemKeys.clear();
  };

  // Apply order is preserved PER ITEM, not globally: a pending buffer
  // only flushes early when the incoming event targets an item that
  // buffer already holds. Items are independent in pane state, so
  // cross-item reordering inside one rAF flush is safe — and it is what
  // keeps a tool burst (upserts, patches, and deltas of many different
  // rows interleaved on the wire) applying as one upsert batch -> one
  // items-array swap -> one structural re-derive, instead of
  // fragmenting into per-transition micro-batches that each paid an
  // O(window) array copy and a full timeline regroup.
  for (const evt of events) {
    if (!evt || !evt.threadId) continue;
    if (evt.action === 'upsert') {
      if (!isValidItemForThread(evt.item, evt.threadId)) continue;
      const itemKey = itemConflictKey(evt.threadId, evt.item.id);
      // A queued delta for this row must land before the upsert
      // replaces the row's summary wholesale.
      if (pendingDeltaItemKeys.has(itemKey)) flushPendingDeltas();
      // Global projections ride the batched flush (one macrotask, one
      // effect flush) instead of the per-message WS handler.
      projectThreadItem(evt.item);
      syncProposedPlanStatus(evt.item);
      pendingUpserts.push({ item: evt.item, countsAsActivity: evt.countsAsActivity });
      pendingUpsertItemKeys.add(itemKey);
      continue;
    }
    if (evt.action === 'meta') {
      // Re-validated meta blob (e.g. live path-link allowlist for an
      // in-flight assistant_text row). Pending deltas for the same row
      // must apply FIRST so the new meta lands against text the user
      // has already seen; a pending upsert for the same row must apply
      // too so the row exists by the time we set its meta.
      const itemKey = itemConflictKey(evt.threadId, evt.itemId);
      if (pendingDeltaItemKeys.has(itemKey)) flushPendingDeltas();
      if (pendingUpsertItemKeys.has(itemKey)) flushPendingUpserts();
      for (const pane of iterPanes()) {
        if (pane.threadId !== evt.threadId) continue;
        pane.applyItemMeta(evt);
      }
      continue;
    }
    if (evt.action === 'patch') {
      const itemKey = itemConflictKey(evt.threadId, evt.itemId);
      if (pendingDeltaItemKeys.has(itemKey)) flushPendingDeltas();
      if (pendingUpsertItemKeys.has(itemKey)) flushPendingUpserts();
      for (const pane of iterPanes()) {
        if (pane.threadId !== evt.threadId) continue;
        pane.applyItemPatch(evt);
      }
      continue;
    }
    if (evt.action !== 'delta') continue;

    // A pending upsert for this row carries the full summary; the
    // delta extends it, so the upsert must land first.
    if (pendingUpsertItemKeys.has(itemConflictKey(evt.threadId, evt.itemId))) {
      flushPendingUpserts();
    }
    queueDelta(evt);
  }

  // Tail order is safe: no item can be pending in BOTH buffers here.
  // Queuing a delta flushes that row's pending upsert first, and
  // buffering an upsert flushes that row's pending delta first (the
  // per-item conflict checks above), so the two pending sets are always
  // disjoint per item by the time we reach the tail. Draining deltas
  // then upserts therefore can't reorder any single row's events.
  flushPendingDeltas();
  flushPendingUpserts();
  // Sidebar activity is bumped only at meaningful interaction
  // boundaries: user_text upsert (handled in applyItemUpserts),
  // provider:turn_completed (applyTurnCompleted), and approval /
  // user-input request creation (applyApprovalEvent /
  // applyUserInputEvent). Streaming deltas and assistant / tool /
  // thinking upserts deliberately do NOT advance the timestamp —
  // that used to make the sidebar reshuffle every chunk.
  notifyItemUpserts(notifiedUpserts);
  if (itemEventQueueStart < itemEventQueue.length) {
    scheduleItemEventFlush();
  }
}
