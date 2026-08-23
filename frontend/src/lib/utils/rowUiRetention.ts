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

/**
 * Rows the retention pass keeps regardless of window position. Mirrored
 * by `collectTimelineRowUiRetention`'s active-item pass — both call
 * here, so the two cannot drift.
 */
export function isRowUiRetentionActive(item: Pick<Item, 'status'>): boolean {
  return item.status === 'running' || item.status === 'streaming';
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
