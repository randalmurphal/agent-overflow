// Live per-thread "the provider is compacting this thread's context"
// flag, from `provider:compacting`.
//
// Live session state, never history: the timeline's durable record of a
// compaction is its divider row; this flag only swaps the activity
// rail's "Working" label to "Compacting" while the summarization runs —
// a window that can span minutes of total wire silence. Because of that
// silence, refresh/reconnect re-learns it from
// `GetThreadLiveState.compactingSinceUnixMs` (see
// threadLiveStateHydration.ts) rather than waiting for a frame that
// won't come.
//
// One reactive box per thread (keyedSignalRegistry) for the same reason
// as fastModeState: the activity rail reads this per pane, and a missing
// key on a SvelteMap would subscribe every pane to the whole map.

import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

export interface CompactingStatePayload {
  threadId: string;
  active: boolean;
  /** Epoch ms of the frame that opened the window; absent on close frames. */
  sinceUnixMs?: number;
}

const NOT_COMPACTING: number | undefined = undefined;

const sinceByThread = createKeyedSignalRegistry<number | undefined>(NOT_COMPACTING);

/** Tracked read: is the provider compacting this thread's context right now? */
export function isThreadCompacting(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return sinceByThread.get(threadId) !== undefined;
}

/** Apply a `provider:compacting` frame. */
export function applyCompactingState(evt: CompactingStatePayload | undefined): void {
  if (!evt || !evt.threadId) return;
  if (evt.active) {
    sinceByThread.set(evt.threadId, evt.sinceUnixMs ?? 0);
  } else {
    sinceByThread.drop(evt.threadId);
  }
}

/**
 * Apply the reconnect snapshot's `compactingSinceUnixMs` (0 = not
 * compacting). Both directions matter: a refresh mid-window must set the
 * flag with no frame coming, and a stale local flag must drop when the
 * snapshot says the window closed while we were disconnected.
 */
export function hydrateCompactingState(threadId: string, sinceUnixMs: number): void {
  if (!threadId) return;
  if (sinceUnixMs > 0) {
    sinceByThread.set(threadId, sinceUnixMs);
  } else {
    sinceByThread.drop(threadId);
  }
}

/** Drop a thread's flag — session teardown, thread delete/archive. */
export function clearCompactingForThread(threadId: string): void {
  if (!threadId) return;
  sinceByThread.drop(threadId);
}

/** Test-only fixture isolation, matching the sibling stores. */
export function resetForTest(): void {
  sinceByThread.reset();
}
