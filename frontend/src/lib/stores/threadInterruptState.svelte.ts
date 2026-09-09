// Thread-scoped interrupt transaction state.
//
// Early Stop hides working presentation immediately while canonical execution
// state remains live. Send stays closed until the caller applies its confirmed
// cut or reconciles an uncertain outcome. Edit/resend uses the same Send gate.

import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

const pendingByThread = createKeyedSignalRegistry<number>(0);
const restoredByThread = createKeyedSignalRegistry<number | null>(null);
const pendingThreads = new Set<string>();
const hiddenItemThreads = new Set<string>();
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
  hiddenItemThreads.delete(threadId);
  restoredByThread.drop(threadId);
  pendingByThread.drop(threadId);
  pendingThreads.delete(threadId);
}

export function isThreadInterruptCurrent(threadId: string, token: number): boolean {
  return pendingByThread.get(threadId) === token;
}

export function isThreadInterruptPending(threadId: string | null | undefined): boolean {
  return Boolean(threadId && pendingByThread.get(threadId) !== 0);
}

/** Hide only presentation while the authoritative interrupt is still settling. */
export function presentThreadInterruptAsRestored(threadId: string, token: number, turnIndex = 0): void {
  if (pendingByThread.get(threadId) === token) restoredByThread.set(threadId, turnIndex);
}

export function isThreadInterruptRestored(threadId: string | null | undefined): boolean {
  return Boolean(threadId && restoredByThread.get(threadId) !== null);
}

export function resetThreadInterruptStateForTest(): void {
  hiddenItemThreads.clear();
  restoredByThread.reset();
  pendingByThread.reset();
  pendingThreads.clear();
}

onThreadHistoryInvalidated((owns) => {
  for (const id of pendingThreads) {
    if (!owns(id)) continue;
    hiddenItemThreads.delete(id);
    restoredByThread.drop(id);
    pendingByThread.drop(id);
    pendingThreads.delete(id);
  }
});

export function optimisticInterruptCut(threadId: string): number | null { return restoredByThread.get(threadId); }

export function noteHiddenInterruptItem(threadId: string): void { hiddenItemThreads.add(threadId); }
export function restoreThreadInterruptPresentation(threadId: string, token: number): boolean {
  if (pendingByThread.get(threadId) !== token) return false;
  restoredByThread.drop(threadId);
  return hiddenItemThreads.delete(threadId);
}
