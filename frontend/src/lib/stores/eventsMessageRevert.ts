import { fenceRevertedItemEvents } from './eventsItemStream';
// User-message-revert event domain: truncating pane items on
// user_message:reverted (the Stop/Esc un-send flow and the
// edit-and-resend saga). Fan-in target of events.ts's
// setupEventListeners.
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { projectSendStarted, projectThreadReverted } from './threadStatuses.svelte';
import { adoptEventStamp, dropThreadHistoryStamp } from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { compositeKey } from '../utils/compositeKey';
import { getConnectionId } from '../transport/clientIdentity';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
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
// Replacement operations leave the composer draft alone. Their cut and prepared
// replacement are applied in one task. Early un-send rehydrates other clients;
// its initiating composer holds a local restoration until cleanup settles.

// Event markers supplement the structured RPC outcome. The event and reply may
// arrive in either order; neither depends on the pane still showing this thread.
// Keyed by thread AND item, not by thread alone: two panes on one thread
// can each be running a flow, and a single per-thread slot would let the
// second flow's guard rejection consume the first flow's marker and
// misreport a committed revert as "nothing happened". The map value is
// the thread id so the per-thread sweep below compares values instead of
// parsing keys — no separator can then be confused for one inside an id.
//
// Recorded only for a saga THIS page load started (`connectionId`). The
// cut is a fact about the thread and every client applies it; the marker
// answers a different question — "did MY revert commit" — and a second
// client's saga on the same anchor would otherwise answer it yes for a
// call that never got that far. The CONNECTION and not the device: two
// tabs of one browser run independent flows.
const revertSubscribers = new Set<(cut: UserMessageRevertedEvent) => void>();
export function onUserMessageReverted(handler: (cut: UserMessageRevertedEvent) => void): () => void {
  revertSubscribers.add(handler);
  return () => { revertSubscribers.delete(handler); };
}

const pendingResendReverts = new Map<string, string>();
// The initiating RPC and the event bus both deliver the same committed cut.
// Their order is intentionally unspecified. The post-cut history stamp is the
// mutation identity, so applying one delivery makes the other a no-op instead
// of letting it clear a newer send that reused the reverted turn number.
const appliedRevertRevByThread = new Map<string, number>();

onThreadHistoryInvalidated((owns) => {
  for (const [key, id] of pendingResendReverts) if (owns(id)) pendingResendReverts.delete(key);
  for (const id of appliedRevertRevByThread.keys()) if (owns(id)) appliedRevertRevByThread.delete(id);
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

// An UNSTAMPED saga frame is recorded, which is the pre-stamp behaviour
// kept verbatim: the stamp is additive, and a bundle running against a
// backend too old to send it must not stop recording its OWN markers —
// that would turn every committed revert into "nothing happened" and send
// the failure handler down the wrong recovery branch.
function resendIsOurs(connectionId: string | undefined): boolean {
  return !connectionId || connectionId === getConnectionId();
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
// the shape it recorded. It runs on EVERY revert, including one another
// client made, because such a revert invalidates our own markers just as
// surely as one of ours does — it is only the RECORDING below that is
// ours alone.
function clearResendRevertMarkersForThread(threadId: string): void {
  for (const [key, owner] of pendingResendReverts) {
    if (owner === threadId) pendingResendReverts.delete(key);
  }
}

/** A locked reconciliation has superseded earlier cut notifications. */
export function acknowledgeConversationRevision(threadId: string, revision: number): void {
  if (Number.isFinite(revision)) appliedRevertRevByThread.set(threadId, Math.max(revision, appliedRevertRevByThread.get(threadId) ?? 0));
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
  fenceRevertedItemEvents(payload);
  const rehydrateDrafts = payload.draftPendingResend !== true;
  clearResendRevertMarkersForThread(payload.threadId);
  if (payload.draftPendingResend === true && resendIsOurs(payload.connectionId)) {
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
  for (const handler of revertSubscribers) handler(payload);
  if (payload.replacement) projectSendStarted(payload.threadId);
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
    if (payload.replacement) pane.armStructuralSpring();
    pane.removeRevertedItems(payload.turnIndex, payload.keptAnchorTurnItemIds ?? []);
    if (payload.replacement?.threadId === payload.threadId) pane.applyProviderItemUpserts([payload.replacement]);
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
