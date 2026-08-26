// The item-side inputs to offscreen row-UI-state retention
// (`components/chat/timelineRowUiPrune.ts`), as a pure predicate pair so
// the store and the prune share ONE definition of "active".
//
// The prune retains every active row unconditionally, wherever it sits
// relative to the viewport, because a running/streaming row's expansion
// handle is about to be written to. Which rows those are, and what the
// prune retains for each, is exactly this tuple — so a write that leaves
// it untouched cannot change the prune's answer.
//
// `rowUiRetentionChanged` is what the store calls at write time to
// maintain `pane.rowUiRetentionRevision`. It compares one row against
// its predecessor, so it costs O(changed) per batch; the prune then
// proves a no-op from that scalar instead of walking every loaded item.
// A full walk at prune cadence is what wedged the renderer for 6-19s
// mid-turn: `pane.items` was then a deep `$state` array replaced on every
// upsert batch, so each walk re-created ~800 lazy per-index sources in
// the proxy's get trap (incident 2026-08-10). The array is `$state.raw`
// since 2026-08-23; the scalar bail still keeps an O(window) walk off
// the reveal cadence.

import type { Item } from '../types/models';
import { compositeKey } from './compositeKey';

/**
 * Rows the retention pass keeps regardless of window position. Mirrored
 * by `collectTimelineRowUiRetention`'s active-item pass — both call
 * here, so the two cannot drift.
 */
export function isRowUiRetentionActive(item: Pick<Item, 'status'>): boolean {
  return item.status === 'running' || item.status === 'streaming';
}

/**
 * THE registry key for a payload's row-UI state — the expansion
 * registry files payload-owned entries under it and the retention pass
 * names retained payloads by it. One helper, shared by the store and
 * the prune, so the two sides cannot drift; retention itself carries
 * these strings (`RowUiStateRetention.payloads`) rather than
 * `{threadId, payloadId}` pairs the pruner would have to re-join.
 */
export function payloadRetentionKey(threadId: string, payloadId: string): string {
  return compositeKey(threadId, payloadId);
}

// Keyed by the Item OBJECT: `writeItemAt` replaces a row's Item on every
// content write, so `threadId`/`payloadId` are immutable for the object's
// lifetime and the memo needs no invalidation. The prune collects
// retention at quiet-work cadence over a ~300-row band; joining the key
// fresh per item per pass was part of the 26.8MB/30s the prune pipeline
// allocated during two-pane streaming (2026-08-25 alloc profile).
const payloadKeyByItem = new WeakMap<object, string | null>();

/** `payloadRetentionKey` for an item's payload, memoized per Item; null when it has none. */
export function itemPayloadRetentionKey(
  item: Pick<Item, 'threadId' | 'payloadId'>,
): string | null {
  const cached = payloadKeyByItem.get(item);
  if (cached !== undefined) return cached;
  const key = item.payloadId ? payloadRetentionKey(item.threadId, item.payloadId) : null;
  payloadKeyByItem.set(item, key);
  return key;
}

/**
 * Whether replacing `previous` with `next` changes what the row-UI prune
 * would retain. `undefined` means the row is absent on that side — an
 * append, an eviction, or a removal.
 *
 * Two rows that are both inactive contribute nothing either way, so
 * their fields are not compared at all: that is the streaming hot case
 * (a summary delta on a settled row) and the common one (a text delta
 * on a streaming row changes no field here either).
 */
export function rowUiRetentionChanged(
  previous: Item | undefined,
  next: Item | undefined,
): boolean {
  if (previous === undefined) return next !== undefined && isRowUiRetentionActive(next);
  if (next === undefined) return isRowUiRetentionActive(previous);
  const previousActive = isRowUiRetentionActive(previous);
  const nextActive = isRowUiRetentionActive(next);
  if (!previousActive && !nextActive) return false;
  if (previousActive !== nextActive) return true;
  // Both active: the retention entry itself can still move. `id` and
  // `threadId` key the retained item, `payloadId` the retained payload
  // handle, and `status` distinguishes the two active statuses.
  return previous.id !== next.id
    || previous.threadId !== next.threadId
    || (previous.payloadId ?? '') !== (next.payloadId ?? '')
    || previous.status !== next.status;
}
