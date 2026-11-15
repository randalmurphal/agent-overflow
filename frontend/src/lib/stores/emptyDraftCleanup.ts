// The initiating composer owns the return to a placeholder, including text
// typed while deletion was in flight. Its broadcast still evicts the sidebar
// row, but must not destroy that composer before the RPC result is applied.
//
// Anything else that writes to the thread row while the delete is in flight
// (a picker, a command, a sidebar action) waits for the cleanup to settle and
// then acts on what the pane shows: the kept thread, or the placeholder.
interface Claim {
  count: number;
  afterDeletion?: () => void;
  deleted: boolean;
  settled: Promise<boolean>;
  settle: (deleted: boolean) => void;
}
const pending = new Map<string, Claim>();

export function deferEmptyDraftDeletion(threadId: string, afterDeletion: () => void): boolean {
  const cleanup = pending.get(threadId);
  if (!cleanup) return false;
  cleanup.afterDeletion = afterDeletion;
  return true;
}

export function isEmptyDraftCleanupPending(threadId: string | null | undefined): boolean {
  return Boolean(threadId && pending.has(threadId));
}

/**
 * Resolves once no empty-draft cleanup of `threadId` is in flight, and the
 * pane has moved to the kept thread or the placeholder: true when it
 * deleted the thread.
 */
export async function settleEmptyDraftCleanup(threadId: string | null | undefined): Promise<boolean> {
  const cleanup = threadId ? pending.get(threadId) : undefined;
  return cleanup ? cleanup.settled : false;
}

export async function withEmptyDraftCleanup(
  threadId: string,
  remove: () => Promise<boolean>,
  restore: (deleted: boolean) => Promise<void>,
): Promise<void> {
  let claim = pending.get(threadId);
  if (!claim) {
    let settle!: (deleted: boolean) => void;
    const settled = new Promise<boolean>((resolve) => { settle = resolve; });
    claim = { count: 0, deleted: false, settled, settle };
    pending.set(threadId, claim);
  }
  claim.count++;
  try {
    let deleted: boolean;
    try {
      deleted = await remove();
    } catch (error) {
      // The event proves the deletion committed even if its RPC reply was
      // lost. Use the ordinary restoration path to retain newly typed text.
      if (!claim.afterDeletion) throw error;
      deleted = true;
    }
    claim.deleted ||= deleted;
    await restore(deleted);
  } finally {
    if (--claim.count === 0) {
      pending.delete(threadId);
      // Usually restoration has already moved the pane off the deleted ID.
      // A lost RPC reply must not strand it on a row proven gone by the event.
      claim.afterDeletion?.();
      claim.settle(claim.deleted);
    }
  }
}
