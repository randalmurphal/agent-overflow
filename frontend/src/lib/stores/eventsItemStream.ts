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
import { confirmFlushedByUserItemId } from './sendQueue.svelte';
import { itemsRenderEqual } from './threadItems';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { isSmoothLiveContentKind } from './threadPaneShared';
import { lookupDiscussionLiveTail } from './discussionLiveTail';
import { isBoundedString, isFiniteNumber } from './eventsGuards';
import { compositeKey } from '../utils/compositeKey';
import type { ThreadPaneIngest } from './threadPaneRoles';

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
let itemEventQueue: ItemStreamEvent[] = [];
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
  for (const item of upserts) {
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
      const hasLiveContentAdvance = applied.changedItems.some((item) =>
        providerUpsertAdvancesLiveContent(previousItemsById.get(item.id), item),
      );
      // A provider upsert that advances live content marks the
      // scroll-animation latch so the controller spring-chases. New
      // text-like rows and visible-field updates to any mounted row
      // stamp here; timestamp-only bumps deliberately do not — see
      // providerUpsertAdvancesLiveContent. New non-text rows stamp via
      // the pane's gated append arm inside applyProviderItemUpserts
      // instead of this ungated path.
      if (hasLiveContentAdvance) pane.markLiveContentAdvanced();
    }
  }
  // Evict cached snapshots only when this batch produced an observable
  // active-pane change. Inactive threads still evict defensively because
  // we do not have their current item window available for value dedupe.
  // This keeps redundant active-thread echoes from invalidating the warm
  // re-entry cache and rebuilding rows for no visible data change.
  for (const threadId of itemsByThread.keys()) {
    const inactive = !activeThreadIds.has(threadId);
    if (changedThreadIds.has(threadId) || inactive) {
      threadItemCache.evict(threadId);
    }
    // The durable copy is dropped only for INACTIVE threads, whose
    // window nobody owns. A mounted thread's replica entry stays: at
    // ~10 Hz streaming, a readwrite IndexedDB transaction per flush is
    // exactly the per-frame cost the backend contract was shaped to
    // avoid (§14), and it buys nothing — the envelope's attested stamp
    // already trails these writes, so the next open answers `stale`
    // and replaces the window regardless. The switch-away snapshot and
    // the debounced write-back own the mounted thread's entry.
    if (inactive) void removeReplicaWindow(threadId);
  }
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

export function applyItemStreamEvent(evt: ItemStreamEvent): void {
  if (!evt || !evt.threadId) return;
  if (evt.action === 'upsert' && evt.item) {
    // Boundary validation only, and now the ONLY global work this
    // channel does: thread status, sidebar activity, and the durable
    // proposed-plan column all moved to wildcard channels, because this
    // channel is narrowed to the threads a client watches and a client
    // that is not watching would never have learned any of them.
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
  // NOTE (2026-08-26, do not rebuild): rotating this flush across
  // mounted threads (one pane's commit per rAF when several stream at
  // once) was built and REFUTED by a controlled 3-pane clone-replay
  // A/B/A/B: busy p95 3.0/3.0ms merged vs 5.0/5.5ms rotated, worst
  // frame no better. The tall frames are flood-shaped. One pane's own
  // beat dominates them, and un-merging multiplies the per-flush fixed
  // costs this single batch amortizes. Numbers in
  // .claude/skills/perf-investigation/REFERENCE.md.
  const pendingUpserts: Item[] = [];
  const pendingUpsertItemKeys = new Set<string>();
  const notifiedUpserts: Item[] = [];
  const pendingDeltas = new Map<string, ItemDeltaEvent & { chunks: string[] }>();
  const pendingDeltaItemKeys = new Set<string>();

  const itemConflictKey = (threadId: string, itemId: string): string =>
    compositeKey(threadId, itemId);

  const flushPendingUpserts = () => {
    if (pendingUpserts.length === 0) return;
    applyItemUpserts(pendingUpserts);
    notifiedUpserts.push(...pendingUpserts);
    pendingUpserts.length = 0;
    pendingUpsertItemKeys.clear();
  };

  const queueDelta = (evt: ItemDeltaEvent) => {
    // Coalescing key includes kind (a row's text and thinking streams
    // coalesce separately); the per-item conflict key does not.
    const key = compositeKey(evt.threadId, evt.itemId, evt.kind);
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
      pendingUpserts.push(evt.item);
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
      for (const pane of ingestPanes()) {
        if (pane.threadId !== evt.threadId) continue;
        pane.applyItemMeta(evt);
      }
      continue;
    }
    if (evt.action === 'patch') {
      const itemKey = itemConflictKey(evt.threadId, evt.itemId);
      if (pendingDeltaItemKeys.has(itemKey)) flushPendingDeltas();
      if (pendingUpsertItemKeys.has(itemKey)) flushPendingUpserts();
      for (const pane of ingestPanes()) {
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
  // boundaries, and none of them is here any more: the reader's own
  // message arrives as a thread:updated `updatedAt` patch, and turn
  // completion / approval / user-input creation each ride their own
  // wildcard channel. Streaming deltas and assistant / tool / thinking
  // upserts never advanced the timestamp — that used to make the
  // sidebar reshuffle every chunk — so this channel now bumps nothing.
  notifyItemUpserts(notifiedUpserts);
  if (itemEventQueueStart < itemEventQueue.length) {
    scheduleItemEventFlush();
  }
}
