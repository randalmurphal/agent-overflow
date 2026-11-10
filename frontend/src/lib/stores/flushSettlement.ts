// Release of the Zone 2 entries no row will confirm.
//
// A thread this client does not watch is sent no rows, so the entries its
// `provider:queue_flushed` frames add are confirmed by the backend instead:
// an answer that none of the thread's sends is pending, as of a
// `provider:queue_flushed` sequence, settles every message the thread
// flushed up to that sequence. The answer has two legs.
//
// - A `provider:sends_pending` frame that reads nothing pending carries the
//   sequence as `flushedThrough`. Live frames arrive in the order the
//   backend emitted them, so the flush frames it covers have arrived.
// - `ListUnsettledFlushes` restates it for a whole computer: every thread
//   it does not name is settled through its sequence. It is read on every
//   connection edge and after a gap on `provider:sends_pending`, which is
//   what repairs a settle frame that was lost or that a replay delivered
//   ahead of the flush frames it covers. Its reply can overtake flush
//   frames emitted before it, so the newest answer per computer stays as a
//   floor that late flush frames are checked against.
//
// Both are facts about one backend's stream position, so applying one late
// or twice changes nothing, and neither says anything about an entry
// another backend flushed. `settleFlushed` (sendQueue.svelte.ts) owns the
// entries and what a mounted pane does with an answer.
import { getTransportHelloFor } from './transportStatus.svelte';
import { iterPanes } from './panes.svelte';
import {
  flushMarkAt,
  releaseFlushedOn,
  sameFlushStream,
  settleFlushed,
  threadsWithFlushedOn,
  type FlushMark,
} from './sendQueue.svelte';
import { ListUnsettledFlushes } from './bindings';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import type { FlushSettlement } from '../../../bindings/agent-overflow/internal/app/models';

const FLUSH_SETTLEMENT = 'flush-settlement.v1';

interface SettlementFloor {
  at: FlushMark;
  unsettled: ReadonlySet<string>;
}
const floors = new Map<BackendKey, SettlementFloor>();
onBackendDetached(({ backendId }) => {
  floors.delete(backendId);
  for (const threadId of threadsWithFlushedOn(backendId)) {
    releaseFlushedOn(threadId, backendId, threadHasPane(threadId));
  }
});

/** How many computers' settlement answers are retained. */
export function __flushSettlementFloorsForTest(): number {
  return floors.size;
}

export function __resetFlushSettlementForTest(): void {
  floors.clear();
}

function threadHasPane(threadId: string): boolean {
  for (const pane of iterPanes()) {
    if (pane.threadId === threadId) return true;
  }
  return false;
}

function settle(threadId: string, settled: FlushMark): void {
  settleFlushed(threadId, settled, threadHasPane(threadId));
}

/** A `provider:sends_pending` frame from `backend` that reads nothing
 * pending. Whoever owns the thread now, it settles what `backend` flushed. */
export function applySettledFrame(threadId: string, backend: BackendKey, flushedThrough: number | undefined): void {
  const settled = flushMarkAt(backend, flushedThrough);
  if (settled) settle(threadId, settled);
}

/** A `provider:queue_flushed` frame at `mark` added entries to the thread. */
export function applySettlementToFlush(threadId: string, mark: FlushMark): void {
  const floor = floors.get(mark.backend);
  if (!floor || !sameFlushStream(floor.at, mark)) return;
  if (!floor.unsettled.has(threadId)) settle(threadId, floor.at);
}

/**
 * Read which of `backend`'s threads still hold an unsettled flush and
 * settle the rest. Resolves without reading when the backend does not
 * report settlement or the session cannot operate its threads, which is
 * also when no flush frame reaches it. Rejects with the transport error.
 */
export async function reconcileSettledFlushes(backend: BackendKey): Promise<void> {
  const launchId = getTransportHelloFor(backend)?.launchId;
  if (!launchId || !getTransportHelloFor(backend)?.capabilities?.includes(FLUSH_SETTLEMENT)) return;
  if (!hasScope('threads:operate', backend)) return;
  const answer = (await withBackendTarget(backend, () => ListUnsettledFlushes())) as FlushSettlement | null;
  const at = flushMarkAt(backend, answer?.flushedThrough);
  // An answer from a launch that is no longer the backend's says nothing
  // about the stream its frames are numbered in now.
  if (!at || at.launchId !== launchId) return;
  const unsettled = new Set(answer?.unsettledThreadIds ?? []);
  const floor = floors.get(backend);
  if (!floor || !sameFlushStream(floor.at, at) || floor.at.sequence <= at.sequence) floors.set(backend, { at, unsettled });
  for (const threadId of threadsWithFlushedOn(backend)) {
    if (!unsettled.has(threadId)) settle(threadId, at);
  }
}
