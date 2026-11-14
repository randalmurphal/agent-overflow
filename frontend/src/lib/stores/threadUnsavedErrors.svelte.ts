import type { UnsavedThreadErrorsEvent } from '../../../bindings/agent-overflow/internal/app/models';

/**
 * A pane's thread errors that the backend could not save to history
 * (`thread:unsaved_error`, mirrored in `GetThreadLiveState.unsavedErrors`).
 * Live state, not history: every frame is the thread's whole list, newest
 * last and bounded by the backend, and the pane drops it when it leaves or
 * reloads the thread. Dismissal is backend-owned (`DismissSessionBanner`
 * with kind `unsaved_error`), so every screen on the thread agrees.
 */
export interface ThreadUnsavedErrors {
  /** The thread's current list, null when it has none. */
  readonly current: UnsavedThreadErrorsEvent | null;
  /** Bumped by every push and clear; a snapshot read before it moved is stale. */
  readonly revision: number;
  /** Apply a `thread:unsaved_error` frame (an empty list clears). */
  apply(evt: UnsavedThreadErrorsEvent): void;
  /** Apply a `GetThreadLiveState` snapshot unless a push landed during the read. */
  hydrateSnapshotIfUnchanged(snapshot: UnsavedThreadErrorsEvent | null | undefined, revisionAtRequest: number): void;
  clear(): void;
}

export const UNSAVED_ERROR_BANNER_KIND = 'unsaved_error';

export function createThreadUnsavedErrors(): ThreadUnsavedErrors {
  let current = $state<UnsavedThreadErrorsEvent | null>(null);
  let revision = 0;

  function replace(next: UnsavedThreadErrorsEvent | null | undefined): void {
    current = next && Array.isArray(next.errors) && next.errors.length > 0
      ? { threadId: next.threadId, errors: [...next.errors], omitted: next.omitted ?? 0 }
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
