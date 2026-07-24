// User-message-revert event domain: truncating pane items on
// user_message:reverted (the Stop/Esc un-send flow). Fan-in target of
// events.ts's setupEventListeners.
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { projectThreadReverted } from './threadStatuses.svelte';

// `user_message:reverted` fires after a successful conversation revert
// (Stop/Esc un-send, or the explicit revert-to-message button). The
// backend has truncated SQLite; this handler mirrors that cut exactly:
// every turn after the anchor turn goes, and within the anchor turn
// only the event's `keptAnchorTurnItemIds` survive (empty = whole turn
// gone — the common case; non-empty = Claude's item-granular cut to a
// mid-turn anchor kept the turn's prefix). Removing only the user item
// would strand orphans in `pane.items` that no longer back any SQLite
// row; removing the whole anchor turn unconditionally would hide rows
// SQLite kept.
//
// Responsibilities: (1) idempotently apply the cut for any pane viewing
// the thread (confirms the un-send path's optimistic removal; defends
// against a stale optimistic miss / cross-pane reflection); (2) refresh
// the composer draft from disk so the reverted text reappears in the
// input. `reloadFromBackend` is a no-op when the draft store is not
// pointed at this thread, so we just fire it for every active draft.
export function applyUserMessageReverted(payload: UserMessageRevertedEvent | null): void {
  if (!payload?.threadId || !payload.userItemId) return;
  if (typeof payload.turnIndex !== 'number') return;
  // Global settle (not per-pane): clear the active turn, pending send,
  // and both send-queue zones so an immediate resend takes the
  // direct-send path instead of queueing behind the reverted turn —
  // and so no orphaned Zone 2 chip (whose provider confirm died with
  // the reverted session) lingers under new output.
  projectThreadReverted(payload.threadId);
  for (const pane of iterPanes()) {
    if (pane.threadId !== payload.threadId) continue;
    pane.removeRevertedItems(payload.turnIndex, payload.keptAnchorTurnItemIds ?? []);
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft) {
      void draft.reloadFromBackend(payload.threadId);
    }
  }
}
