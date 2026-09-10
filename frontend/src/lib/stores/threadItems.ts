import type { Item } from '../types/models';
import { userMessageIdentity } from '../utils/userMessageIdentity';

const EMPTY_ID_SET: ReadonlySet<string> = new Set<string>();
const NO_REJECTED_ITEMS: readonly Item[] = Object.freeze([]);

export interface TimelineCursorLike {
  turnIndex: number;
  itemIndex: number;
  itemId?: string;
}

export function compareItemsByTimelinePosition(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

export function compareCursors(a: TimelineCursorLike, b: TimelineCursorLike): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

export function compareItemToCursor(item: Item, cursor: TimelineCursorLike): number {
  return compareCursors(
    { turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id },
    cursor,
  );
}

export function cursorFromItem(item: Item): TimelineCursorLike {
  return {
    turnIndex: item.turnIndex,
    itemIndex: item.itemIndex,
    itemId: item.id,
  };
}

/** Reconcile page cuts before admission; moved outliers must not skip unloaded history. */
export function cursorsAfterItemUpserts(
  oldest: TimelineCursorLike | null | undefined,
  newest: TimelineCursorLike | null | undefined,
  current: readonly Item[],
  incoming: readonly Item[],
  threadId: string | null,
): { oldest: TimelineCursorLike | null; newest: TimelineCursorLike | null } {
  const result = { oldest: oldest ?? null, newest: newest ?? null };
  if (!oldest || !newest) return result;
  if (!incoming.some(item => (threadId === null || item.threadId === threadId)
    && ((item.id === oldest.itemId && compareItemToCursor(item, oldest) !== 0)
      || (item.id === newest.itemId && compareItemToCursor(item, newest) !== 0)
      || userMessageIdentity(item) !== null))) return result;
  const updates = new Map(incoming
    .filter((item) => threadId === null || item.threadId === threadId)
    .map((item) => [item.id, item]));
  const sendUpdates = new Map<string, Item>();
  for (const item of updates.values()) {
    const identity = userMessageIdentity(item);
    if (identity !== null) sendUpdates.set(identity, item);
  }
  // Confirmation can change the backend id of any covered row, including
  // interior rows needed to prove that the whole loaded span translated.
  const confirmations = new Map<string, Item>();
  if (sendUpdates.size > 0) {
    for (const item of current) {
      const identity = userMessageIdentity(item);
      const confirmed = identity === null ? undefined : sendUpdates.get(identity);
      if (confirmed && confirmed.id !== item.id) confirmations.set(item.id, confirmed);
    }
  }
  const updateFor = (id: string) => updates.get(id) ?? confirmations.get(id);
  const movesAnchor = [oldest, newest].some((cursor) => {
    const updated = cursor.itemId ? updateFor(cursor.itemId) : undefined;
    return updated && (updated.id !== cursor.itemId || compareItemToCursor(updated, cursor) !== 0);
  });
  if (!movesAnchor) return result;
  const covered = current.filter((item) => !item.parentId
    && compareItemToCursor(item, oldest) >= 0 && compareItemToCursor(item, newest) <= 0);
  const projected = covered.map((item) => updateFor(item.id) ?? item);
  // A suffix insertion translates the entire loaded span. Check every row,
  // not just the endpoints: an isolated moved prompt proves no such coverage.
  const offset = covered.length > 1 ? projected[0].itemIndex - covered[0].itemIndex : 0;
  const translated = offset !== 0 && covered.every((item, i) =>
    projected[i].turnIndex === item.turnIndex && projected[i].itemIndex - item.itemIndex === offset);
  function reconcile(cursor: TimelineCursorLike, direction: -1 | 1): TimelineCursorLike {
    const anchor = cursor.itemId ? updateFor(cursor.itemId) : undefined;
    if (!anchor || !covered.some((item) => item.id === cursor.itemId)) return cursor;
    let next = cursorFromItem(anchor);
    if (compareCursors(next, cursor) === 0 || translated) return next;
    // Keep all surviving covered rows inside the cuts when an anchor moves
    // inward. Rows that moved outside the old span are loaded outliers.
    for (const item of projected) {
      if (compareItemToCursor(item, oldest!) < 0 || compareItemToCursor(item, newest!) > 0) continue;
      if (compareItemToCursor(item, next) * direction > 0) next = cursorFromItem(item);
    }
    if (compareCursors(next, cursor) * direction <= 0) return next;
    // Extending into an unloaded gap needs every intervening coordinate in
    // this batch. Otherwise retain a numeric cut, detached from the outlier.
    const distance = (next.itemIndex - cursor.itemIndex) * direction;
    if (next.turnIndex === cursor.turnIndex && distance <= updates.size) {
      const positions = new Set([...updates.values()]
        .filter((item) => !item.parentId && item.turnIndex === cursor.turnIndex)
        .map((item) => item.itemIndex));
      let step = 1;
      while (step <= distance && positions.has(cursor.itemIndex + step * direction)) step++;
      if (step > distance) return next;
    }
    return { turnIndex: cursor.turnIndex, itemIndex: cursor.itemIndex };
  }
  result.oldest = reconcile(oldest, -1);
  result.newest = reconcile(newest, 1);
  if (compareCursors(result.oldest, result.newest) > 0) {
    result.oldest = { turnIndex: oldest.turnIndex, itemIndex: oldest.itemIndex };
    result.newest = { turnIndex: newest.turnIndex, itemIndex: newest.itemIndex };
  }
  return result;
}

// Validity keys on turnIndex alone: turn indexes are never negative,
// but item indexes can be (head-healed prompts persist at negative
// indexes), and a page bounded by one must keep paging. The backend's
// empty sentinel is turnIndex -1.
export function cursorIsValid(cursor: TimelineCursorLike | null | undefined): cursor is TimelineCursorLike {
  if (!cursor) return false;
  return Number.isFinite(cursor.turnIndex)
    && Number.isFinite(cursor.itemIndex)
    && cursor.turnIndex >= 0;
}

/**
 * Field-wise equality for two items at the same id. Returns true when the
 * incoming upsert carries no observable change vs. the existing row.
 *
 * Why this exists: the backend can re-emit the same item event with
 * unchanged content (event-loop resyncs, redundant `provider:item_event`
 * upserts on durable-status flag updates, etc.). Each such redundant
 * upsert otherwise replaces the row in `pane.items` with a new object
 * reference, which forces `groupedNodes` to rebuild, which forces
 * `<TimelineVirtualizer data={revealedNodes}>` to re-iterate, which can
 * land as a remount of the affected row's DOM. A row with async render
 * work (DiffFileBlock token dispatch, Streamdown typesetting, etc.) then
 * settles at its measured height again — and the engine compensates
 * scrollTop for the size delta. Observed in plan-ready threads as a row
 * oscillating ±103 px every ~115 ms while the user is trying to scroll.
 *
 * Compare-by-value covers the fields the row renderers actually read.
 * `id`, `threadId`, `turnIndex`, `itemIndex`, `parentId`, `completionOf`,
 * `payloadId`, `inputPayloadId`, `kind`, `role`, `payloadKind`, `toolName`
 * are part of identity / structure and would change the renderer's branch
 * if they differed — included for completeness. `summary`, `status`,
 * `decision`, `payloadMeta`, `meta`, `createdAt`, `updatedAt`,
 * `isBackground` are the streaming / mutable surface. If every one of these
 * matches, the upsert is a true no-op and we can drop it.
 */
export function itemsAreEqual(a: Item, b: Item): boolean {
  return (
    itemsRenderEqual(a, b)
    && a.createdAt === b.createdAt
    && a.updatedAt === b.updatedAt
  );
}

/**
 * `itemsAreEqual` minus the `createdAt` / `updatedAt` timestamps: value
 * equality over every field a row renders or that positions it in the
 * timeline. The events fan-out's spring-latch predicate
 * (`providerUpsertAdvancesLiveContent`) uses THIS check so that an
 * applied upsert stamps the latch exactly when something the user can
 * see changed — a timestamp-only heartbeat bump must not hold the latch
 * open. Keep exhaustive over `Item`: `itemsAreEqual` builds on it, so a
 * field added here is automatically part of both the upsert dedupe and
 * the latch decision.
 */
export function itemsRenderEqual(a: Item, b: Item): boolean {
  return (
    a.id === b.id
    && a.threadId === b.threadId
    && a.turnIndex === b.turnIndex
    && a.itemIndex === b.itemIndex
    && a.kind === b.kind
    && a.role === b.role
    && a.status === b.status
    && a.summary === b.summary
    && a.payloadId === b.payloadId
    && a.inputPayloadId === b.inputPayloadId
    && a.payloadKind === b.payloadKind
    && a.payloadMeta === b.payloadMeta
    && a.parentId === b.parentId
    && a.completionOf === b.completionOf
    && a.toolName === b.toolName
    && a.decision === b.decision
    && a.meta === b.meta
    && a.isBackground === b.isBackground
  );
}

export function itemsForThread(
  nextItems: readonly Item[] | null | undefined,
  threadId: string,
): Item[] {
  return (nextItems ?? []).filter((item) => item.threadId === threadId);
}

/**
 * Merge `incoming` into `current` by id, returning a fresh array sorted by
 * (turnIndex, itemIndex). Used by paging paths where the backend can
 * legitimately re-return ancestor rows already in the window. A top-level
 * user send also matches its provisional representation by send identity.
 *
 * Returns the original `current` reference when `incoming` is empty or every
 * incoming row is already present by the same object reference, so callers can
 * skip reactive writes and associated turn-diff rebuilds.
 */
export function mergeItemsById(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0) return current as Item[];
  const byId = new Map<string, Item>();
  const idBySend = new Map<string, string>();
  for (const it of current) {
    byId.set(it.id, it);
    const identity = userMessageIdentity(it);
    if (identity !== null) idBySend.set(identity, it.id);
  }
  let changed = false;
  for (const it of incoming) {
    const identity = userMessageIdentity(it);
    const previousId = identity === null ? undefined : idBySend.get(identity);
    const existing = byId.get(it.id) ?? (previousId ? byId.get(previousId) : undefined);
    if (existing && itemsAreEqual(existing, it)) {
      continue;
    }
    if (existing !== it) {
      const replaced = byId.get(it.id);
      const previousIdentity = replaced ? userMessageIdentity(replaced) : null;
      if (previousIdentity !== null && previousIdentity !== identity) idBySend.delete(previousIdentity);
      if (previousId && previousId !== it.id) byId.delete(previousId);
      byId.set(it.id, it);
      if (identity !== null) idBySend.set(identity, it.id);
      changed = true;
    }
  }
  if (!changed) return current as Item[];
  const merged = Array.from(byId.values());
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}

export function reconcileItemWindow(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0 && current.length === 0) return current as Item[];

  const currentById = new Map<string, Item>();
  for (const item of current) currentById.set(item.id, item);

  const next: Item[] = [];
  let changed = incoming.length !== current.length;
  for (const item of incoming) {
    const existing = currentById.get(item.id);
    if (existing && itemsAreEqual(existing, item)) {
      next.push(existing);
    } else {
      next.push(item);
      if (existing !== item) changed = true;
    }
  }

  if (!changed) {
    for (let index = 0; index < current.length; index += 1) {
      if (current[index] !== next[index]) {
        changed = true;
        break;
      }
    }
  }

  return changed ? next : current as Item[];
}

/**
 * Reconcile a backend snapshot over the current window while keeping rows
 * changed by live paths after the request began. Used for both initial
 * `SyncThreadWindow` convergence and transport-gap refresh.
 *
 * Two rules, and they pull in opposite directions:
 *
 *  - **Paint-only rows do not survive.** A cached row the page does not
 *    contain no longer exists (or no longer sits here) as of the read
 *    the stamps attest, so it is dropped. That is what makes the
 *    subsequent write-back safe: everything persisted descends from the
 *    attested page (docs/architecture/thread-replica-sync.md §6.1 step 4).
 *  - **Live rows are newer than the page.** Anything the wire touched
 *    since the switch began post-dates the page's read snapshot, so its
 *    version wins where both have the row, and it is kept where the page
 *    does not have it at all. Without this a thread opened mid-stream
 *    would lose the row it was streaming into.
 *
 * Unchanged rows keep their existing reference so the reconcile does not
 * re-render them. `items` is `current` itself when nothing moved.
 *
 * `orphanedLiveChildren` are live-touched subagent children the merge
 * dropped because their launch anchor survived in neither the page nor
 * the live set — keeping them would install exactly the unreachable
 * orphan rows the admission boundary exists to prevent (same contract as
 * `rejectedParentedItems` on the upsert merge). The caller swallows them.
 */
export function reconcileSnapshotPage(
  page: readonly Item[],
  current: readonly Item[],
  liveTouchedIds: ReadonlySet<string>,
  liveRemovedIds: ReadonlySet<string> = EMPTY_ID_SET,
): { items: Item[]; orphanedLiveChildren: readonly Item[] } {
  if (page.length === 0 && current.length === 0) {
    return { items: current as Item[], orphanedLiveChildren: NO_REJECTED_ITEMS };
  }

  const currentById = new Map<string, Item>();
  for (const item of current) currentById.set(item.id, item);

  const next: Item[] = [];
  const keptIds = new Set<string>();
  const keptSends = new Set<string>();
  for (const item of page) {
    if (liveRemovedIds.has(item.id)) continue;
    keptIds.add(item.id);
    const identity = userMessageIdentity(item);
    if (identity !== null) keptSends.add(identity);
    const existing = currentById.get(item.id);
    if (!existing) {
      next.push(item);
      continue;
    }
    if (liveTouchedIds.has(item.id) || itemsAreEqual(existing, item)) {
      next.push(existing);
      continue;
    }
    next.push(item);
  }

  let orphanedLiveChildren: Item[] | null = null;
  // `current` is window-ordered, so a live parent is decided before its
  // live children and the anchor check below is transitive.
  for (const item of current) {
    if (keptIds.has(item.id)) continue;
    const identity = userMessageIdentity(item);
    if (identity !== null && keptSends.has(identity)) continue;
    if (!liveTouchedIds.has(item.id)) continue;
    const parentId = item.parentId ?? '';
    if (parentId && !keptIds.has(parentId)) {
      (orphanedLiveChildren ??= []).push(item);
      continue;
    }
    keptIds.add(item.id);
    next.push(item);
  }
  // Always sorted, never "the page was sorted so the result is": the
  // live arrivals are appended out of position by construction, and the
  // window's order is a property callers rely on rather than something
  // the wire is trusted to have got right.
  next.sort(compareItemsByTimelinePosition);

  const orphaned = orphanedLiveChildren ?? NO_REJECTED_ITEMS;
  if (next.length !== current.length) {
    return { items: next, orphanedLiveChildren: orphaned };
  }
  for (let index = 0; index < current.length; index += 1) {
    if (current[index] !== next[index]) {
      return { items: next, orphanedLiveChildren: orphaned };
    }
  }
  return { items: current as Item[], orphanedLiveChildren: orphaned };
}

/**
 * Like `mergeItemsById` but only adds rows not already present. Existing rows
 * keep their current reference so streamed events are not clobbered by a
 * slightly older SQLite row returned by a concurrent load.
 */
export function mergeMissingItemsById(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0) return current as Item[];
  const presentIds = new Set<string>();
  const presentSends = new Set<string>();
  for (const it of current) {
    presentIds.add(it.id);
    const identity = userMessageIdentity(it);
    if (identity !== null) presentSends.add(identity);
  }
  const additions: Item[] = [];
  for (const it of incoming) {
    const identity = userMessageIdentity(it);
    if (presentIds.has(it.id) || (identity !== null && presentSends.has(identity))) continue;
    additions.push(it);
    presentIds.add(it.id);
    if (identity !== null) presentSends.add(identity);
  }
  if (additions.length === 0) return current as Item[];
  const merged = current.concat(additions);
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}
