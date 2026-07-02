// Checkpoint + user-message-revert event domain: fanning
// checkpoint:captured/unavailable/error/reverted straight into the
// matching panes, and truncating pane items on user_message:reverted.
// Fan-in target of events.ts's setupEventListeners.
import type {
  CheckpointCapturedEvent,
  CheckpointErrorEvent,
  CheckpointRevertedEvent,
  CheckpointUnavailableEvent,
  UserMessageRevertedEvent,
} from '../types/checkpoint';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

export function applyCheckpointCaptured(payload: CheckpointCapturedEvent | null): void {
  for (const pane of iterPanes()) {
    pane.applyCheckpointCaptured(payload);
  }
}

export function applyCheckpointUnavailable(payload: CheckpointUnavailableEvent | null): void {
  for (const pane of iterPanes()) {
    pane.applyCheckpointUnavailable(payload);
  }
}

export function applyCheckpointError(payload: CheckpointErrorEvent | null): void {
  for (const pane of iterPanes()) {
    pane.applyCheckpointError(payload);
  }
}

export function applyCheckpointReverted(payload: CheckpointRevertedEvent | null): void {
  for (const pane of iterPanes()) {
    pane.applyCheckpointReverted(payload);
  }
}

// `user_message:reverted` fires after InterruptAndRevertIfClean rolls
// back the most-recent user message. Backend truncates SQLite via
// `DeleteConversationFromTurn(threadId, turnIndex)` — inclusive — so
// synthetic siblings on the same turn (thinking, api_retry, error,
// notification, terminal_interaction waits) all go with the user row.
// This handler mirrors that truncate on the frontend: removing only
// the user item would strand orphans in `pane.items` that no longer
// back any SQLite row, surviving until thread switch / cache evict.
//
// Responsibilities: (1) idempotently remove every pane item at
// `>= turnIndex` for any pane viewing the thread (matches backend
// truncate; defends against a stale optimistic miss / cross-pane
// reflection); (2) refresh the composer draft from disk so the
// user's typed text reappears in the input. `reloadFromBackend` is
// a no-op when the draft store is not pointed at this thread, so we
// just fire it for every active draft.
export function applyUserMessageReverted(payload: UserMessageRevertedEvent | null): void {
  if (!payload?.threadId || !payload.userItemId) return;
  if (typeof payload.turnIndex !== 'number') return;
  for (const pane of iterPanes()) {
    if (pane.threadId !== payload.threadId) continue;
    pane.removeItemsFromTurn(payload.turnIndex);
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft) {
      void draft.reloadFromBackend(payload.threadId);
    }
  }
}
