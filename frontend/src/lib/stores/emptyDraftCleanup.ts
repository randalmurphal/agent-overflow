// The initiating composer owns the return to a placeholder, including text
// typed while deletion was in flight. Its broadcast still evicts the sidebar
// row, but must not destroy that composer before the RPC result is applied.
const pending = new Map<string, { count: number; afterDeletion?: () => void }>();

export function deferEmptyDraftDeletion(threadId: string, afterDeletion: () => void): boolean {
  const cleanup = pending.get(threadId);
  if (!cleanup) return false;
  cleanup.afterDeletion = afterDeletion;
  return true;
}

export async function withEmptyDraftCleanup(
  threadId: string,
  remove: () => Promise<boolean>,
  restore: (deleted: boolean) => Promise<void>,
): Promise<void> {
  const claim = pending.get(threadId) ?? { count: 0 };
  claim.count++;
  pending.set(threadId, claim);
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
    await restore(deleted);
  } finally {
    if (--claim.count === 0) {
      pending.delete(threadId);
      // Usually restoration has already moved the pane off the deleted ID.
      // A lost RPC reply must not strand it on a row proven gone by the event.
      claim.afterDeletion?.();
    }
  }
}
