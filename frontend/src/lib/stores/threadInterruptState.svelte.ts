// Thread-scoped interrupt transaction state.
//
// Stop clears the active-turn projection immediately, but the backend may
// still be slicing provider history and truncating SQLite. A new send during
// that interval reuses the reverted turn identity, so the late cut deletes
// the new optimistic row. This registry keeps Send closed until the
// authoritative cut has been applied locally.

import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

const pendingByThread = createKeyedSignalRegistry<number>(0);
const pendingThreads = new Set<string>();
let nextToken = 0;

/** Claim the one interrupt transaction allowed on a thread. */
export function beginThreadInterrupt(threadId: string): number | null {
  if (!threadId || pendingByThread.get(threadId) !== 0) return null;
  const token = ++nextToken;
  pendingThreads.add(threadId);
  pendingByThread.set(threadId, token);
  return token;
}

/**
 * Finish a caller-owned interrupt transaction. The token prevents an older
 * async completion from clearing a newer transaction.
 */
export function finishThreadInterrupt(threadId: string, token: number): void {
  if (!threadId || token <= 0) return;
  if (pendingByThread.get(threadId) !== token) return;
  pendingByThread.drop(threadId);
  pendingThreads.delete(threadId);
}

export function isThreadInterruptPending(threadId: string | null | undefined): boolean {
  return Boolean(threadId && pendingByThread.get(threadId) !== 0);
}

export function resetThreadInterruptStateForTest(): void {
  pendingByThread.reset();
  pendingThreads.clear();
}

onThreadHistoryInvalidated((owns) => {
  for (const id of pendingThreads) {
    if (!owns(id)) continue;
    pendingByThread.drop(id);
    pendingThreads.delete(id);
  }
});
