import type { UnsavedThreadRowsEvent } from '../../../bindings/agent-overflow/internal/app/models';

/**
 * A pane's thread errors and notices that the backend could not save to
 * history (`thread:unsaved_rows`, read back by `GetThreadUnsavedRows`). Live
 * state, not history: every frame is the thread's whole list, newest last
 * and bounded by the backend, and the pane drops it when it leaves or
 * reloads the thread. Only backend answers write it: a dismiss
 * (`DismissUnsavedThreadRows`) arrives back as a push.
 */
export interface ThreadUnsavedRows {
  /** The thread's current list, null when it has none. */
  readonly current: UnsavedThreadRowsEvent | null;
  /** Bumped by every push and clear; a snapshot read before it moved is stale. */
  readonly revision: number;
  /** Apply a `thread:unsaved_rows` frame (an empty list clears). */
  apply(evt: UnsavedThreadRowsEvent): void;
  /** Apply a `GetThreadLiveState` snapshot unless a push landed during the read. */
  hydrateSnapshotIfUnchanged(snapshot: UnsavedThreadRowsEvent | null | undefined, revisionAtRequest: number): void;
  clear(): void;
}

export function createThreadUnsavedRows(): ThreadUnsavedRows {
  let current = $state<UnsavedThreadRowsEvent | null>(null);
  let revision = 0;

  function replace(next: UnsavedThreadRowsEvent | null | undefined): void {
    current = next && Array.isArray(next.rows) && next.rows.length > 0
      ? { threadId: next.threadId, rows: [...next.rows], omitted: next.omitted ?? 0 }
      : null;
  }

  return {
    get current() {
      return current;
    },
    get revision() {
      return revision;
    },
    apply(evt) {
      revision += 1;
      replace(evt);
    },
    hydrateSnapshotIfUnchanged(snapshot, revisionAtRequest) {
      if (revision !== revisionAtRequest) return;
      replace(snapshot);
    },
    clear() {
      revision += 1;
      current = null;
    },
  };
}
