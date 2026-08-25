// User-message-revert event domain: truncating pane items on
// user_message:reverted (the Stop/Esc un-send flow and the
// edit-and-resend saga). Fan-in target of events.ts's
// setupEventListeners.
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { projectThreadReverted } from './threadStatuses.svelte';
import { adoptEventStamp, dropThreadHistoryStamp } from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { compositeKey } from '../utils/compositeKey';
import { onBackendIdentity } from '../transport/backendIdentity';
import type { ThreadPaneIngest } from './threadPaneRoles';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

// `user_message:reverted` fires after a successful conversation revert
// (Stop/Esc un-send, or the edit-and-resend saga's committed revert).
// The backend has truncated SQLite; this handler mirrors that cut exactly:
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
//
// (2) is skipped entirely for `draftPendingResend`: the edit-and-resend
// saga persists a merged crash-copy draft (edited text + the composer's
// WIP) before it truncates, and reloading would repaint the live
// composer with that transient saga row — replacing the user's untouched
// WIP with a copy of the message they are in the middle of resending.
// The saga restores the real draft row itself once the resend lands; a
// committed-then-failed resend is recovered by the edit-and-resend flow
// (`components/chat/editResendFlow.svelte.ts`) from live frontend state,
// not from this row.

// A `draftPendingResend` revert was observed for this (thread, user
// item). This is the AUTHORITATIVE "did the revert commit" signal for the
// edit-and-resend flow's failure handler. The event frame is emitted
// before the saga dispatches the resend and the RPC rejection is written
// after it, so on the FIFO WebSocket the marker is always recorded before
// the caller's promise rejects. Inferring the same answer structurally
// from `pane.items` breaks after a mid-RPC thread switch, when the pane
// holds another thread's rows.
//
// Keyed by thread AND item, not by thread alone: two panes on one thread
// can each be running a flow, and a single per-thread slot would let the
// second flow's guard rejection consume the first flow's marker and
// misreport a committed revert as "nothing happened". The map value is
// the thread id so the per-thread sweep below compares values instead of
// parsing keys — no separator can then be confused for one inside an id.
const pendingResendReverts = new Map<string, string>();
// The initiating RPC and the event bus both deliver the same committed cut.
// Their order is intentionally unspecified. The post-cut history stamp is the
// mutation identity, so applying one delivery makes the other a no-op instead
// of letting it clear a newer send that reused the reverted turn number.
const appliedRevertRevByThread = new Map<string, number>();

onBackendIdentity(() => {
  pendingResendReverts.clear();
  appliedRevertRevByThread.clear();
});

function revertRevision(payload: UserMessageRevertedEvent): number | null {
  const { historyEpoch, historyRev } = payload;
  if (typeof historyEpoch !== 'number' || typeof historyRev !== 'number') return null;
  if (!Number.isFinite(historyEpoch) || !Number.isFinite(historyRev)) return null;
  if (historyEpoch <= 0 && historyRev <= 0) return null;
  return historyRev;
}

function markerKey(threadId: string, userItemId: string): string {
  return compositeKey(threadId, userItemId);
}

// Consume-on-read from both saga outcomes, so a stale marker can never
// misclassify a later, unrelated failure. Deletes ONLY its own key: a
// concurrent flow's marker on the same thread is not this caller's to
// answer for.
export function consumeResendRevertMarker(threadId: string, userItemId: string): boolean {
  return pendingResendReverts.delete(markerKey(threadId, userItemId));
}

// Every marker on a thread is stale the moment a newer revert lands on
// it: the conversation the older saga was reverting no longer exists in
// the shape it recorded. This is also what self-heals markers set by
// reverts that originated from ANOTHER connected client — nothing local
// ever consumes those, so without a sweep they would accumulate for the
// process's lifetime and answer `true` to an unrelated later failure on
// the same anchor.
function clearResendRevertMarkersForThread(threadId: string): void {
  for (const [key, owner] of pendingResendReverts) {
    if (owner === threadId) pendingResendReverts.delete(key);
  }
}

export function resetResendRevertMarkersForTest(): void {
  pendingResendReverts.clear();
  appliedRevertRevByThread.clear();
}

export function applyUserMessageReverted(payload: UserMessageRevertedEvent | null): void {
  if (!payload?.threadId || !payload.userItemId) return;
  if (typeof payload.turnIndex !== 'number') return;
  const revision = revertRevision(payload);
  const appliedRevision = appliedRevertRevByThread.get(payload.threadId);
  // `history_rev` is monotonic for a backend generation. Ignore both the
  // second delivery of one cut and an older cut that arrived after a newer
  // one. Backend identity changes clear this map before a restored database
  // can rewind the counter.
  if (revision !== null && appliedRevision !== undefined && appliedRevision >= revision) return;
  const rehydrateDrafts = payload.draftPendingResend !== true;
  clearResendRevertMarkersForThread(payload.threadId);
  if (payload.draftPendingResend === true) {
    pendingResendReverts.set(
      markerKey(payload.threadId, payload.userItemId),
      payload.threadId,
    );
  }
  // Global settle (not per-pane): clear the active turn, pending send,
  // and both send-queue zones so an immediate resend takes the
  // direct-send path instead of queueing behind the reverted turn —
  // and so no orphaned Zone 2 chip (whose provider confirm died with
  // the reverted session) lingers under new output.
  projectThreadReverted(payload.threadId);
  // Every cached copy of this thread's window predates the cut, and the
  // per-pane patch below only reaches panes that are showing it. Drop
  // them unconditionally (and the stamp with them): a cached window
  // under a post-cut stamp is the one shape that would answer `fresh`
  // over rows the backend removed.
  dropThreadHistoryStamp(payload.threadId);
  threadItemCache.evict(payload.threadId);
  void removeReplicaWindow(payload.threadId);
  for (const pane of ingestPanes()) {
    if (pane.threadId !== payload.threadId) continue;
    pane.removeRevertedItems(payload.turnIndex, payload.keptAnchorTurnItemIds ?? []);
    if (!rehydrateDrafts) continue;
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft) {
      void draft.reloadFromBackend(payload.threadId);
    }
  }
  // After the cut has been applied everywhere, not before: the stamp
  // describes post-cut history, and adopting it while a pane still held
  // pre-cut rows would let the next sync call them fresh. In-memory
  // only, like every event-carried stamp (§3.4).
  adoptEventStamp(payload.threadId, payload.historyEpoch, payload.historyRev);
  if (revision !== null) appliedRevertRevByThread.set(payload.threadId, revision);
}
