import type { Thread } from '../types/models';
import type { PendingInteractiveRequests } from '../types/events';
import type { ThreadLiveState } from '../../../bindings/agent-overflow/models';
import { GetThreadLiveState, ListPendingInteractiveRequests } from './bindings';
import type { LiveStateHydrationGuard } from './threadPaneShared';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  getActiveTurn,
  isThreadLiveStateHydrationCurrent,
  projectTurnCompleted,
  projectTurnStarted,
  replaceInteractiveRequestsForThread,
  sameActiveTurn,
} from './threadStatuses.svelte';
import {
  getQueueRevisionForThread,
  queueItemFromWire,
  replaceFlushedForThread,
  replaceQueueForThread,
  type FlushedItem,
  type QueueItem as SendQueueItem,
} from './sendQueue.svelte';
import type { ThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import type { LiveTodoState } from './liveTodoState.svelte';

export interface ThreadLiveStateHydrationOptions {
  getThread(): Thread | null;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** The pane's createThreadPendingInteractiveState instance. */
  pendingInteractiveState: ThreadPendingInteractiveState;
  /** The pane's createLiveTodoState instance. */
  liveTodoState: LiveTodoState;
}

export interface ThreadLiveStateHydration {
  /** Fetch and apply the thread's live state (active turn, send queue, pending interactive requests, live todos), gen-guarded throughout. */
  hydrateThreadLiveState(
    threadID: string,
    gen: number,
    existingHydrationToken?: number,
  ): Promise<void>;
}

/**
 * Owns a thread pane's live-state hydration protocol: fetching
 * `GetThreadLiveState` (with a `ListPendingInteractiveRequests`
 * fallback leg), projecting the snapshot onto the global active-turn /
 * send-queue registries, and applying the pending-interactive and
 * live-todo snapshots onto the pane's own state slots. Every leg is
 * gen-guarded against the pane's switch generation and the live-state
 * hydration token so a thread swap or a superseding hydration mid-flight
 * discards the late resolution.
 */
export function createThreadLiveStateHydration(
  options: ThreadLiveStateHydrationOptions,
): ThreadLiveStateHydration {
  function applyPendingInteractiveSnapshot(
    threadID: string,
    snapshot: PendingInteractiveRequests | null | undefined,
  ): void {
    const registrySnapshot =
      options.pendingInteractiveState.registrySnapshotFor(snapshot);
    options.pendingInteractiveState.applySnapshot(snapshot);
    replaceInteractiveRequestsForThread(threadID, registrySnapshot);
  }

  async function hydratePendingInteractiveRequests(
    threadID: string,
    gen: number,
    hydrationToken?: number,
  ): Promise<void> {
    let snapshot: PendingInteractiveRequests;
    try {
      snapshot = (await ListPendingInteractiveRequests(
        threadID,
      )) as PendingInteractiveRequests;
    } catch (err) {
      if (gen === options.getSwitchGeneration() && options.getThread()?.id === threadID) {
        console.error('Failed to hydrate pending interactive requests:', err);
      }
      return;
    }
    if (gen !== options.getSwitchGeneration() || options.getThread()?.id !== threadID) return;
    if (
      hydrationToken !== undefined &&
      !isThreadLiveStateHydrationCurrent(threadID, hydrationToken)
    )
      return;

    applyPendingInteractiveSnapshot(threadID, snapshot);
  }

  function applyThreadLiveStateSnapshot(
    snapshot: ThreadLiveState,
    threadID: string,
    guard: LiveStateHydrationGuard,
  ): void {
    if (snapshot.threadId !== threadID) return;
    const current = getActiveTurn(threadID);
    if (sameActiveTurn(current, guard.activeTurnAtRequest)) {
      const active = snapshot.activeTurn;
      if (active && active.threadId === threadID && active.turnId) {
        projectTurnStarted(
          threadID,
          active.turnId,
          active.turnIndex,
          active.startedAt,
        );
      } else if (current) {
        projectTurnCompleted(threadID, current.turnId);
      }
    }

    if (getQueueRevisionForThread(threadID) === guard.queueRevisionAtRequest) {
      const queueItems: SendQueueItem[] = (snapshot.queueItems ?? [])
        .filter((item) => item.threadId === threadID)
        .map(queueItemFromWire);
      replaceQueueForThread(threadID, queueItems);
      const flushedItems: FlushedItem[] = (snapshot.flushedItems ?? [])
        .filter((item) => item.userItemId && item.queueItemId)
        .map((item) => ({
          queueItemId: item.queueItemId,
          userItemId: item.userItemId,
          message: item.message,
          flushedAt: Date.now(),
        }));
      replaceFlushedForThread(threadID, flushedItems);
    }

    applyPendingInteractiveSnapshot(
      threadID,
      snapshot.interactive as PendingInteractiveRequests,
    );

    options.liveTodoState.hydrateSnapshotIfUnchanged(
      snapshot.todo,
      threadID,
      guard.liveTodoRevisionAtRequest,
    );
  }

  async function hydrateThreadLiveState(
    threadID: string,
    gen: number,
    existingHydrationToken?: number,
  ): Promise<void> {
    const hydrationToken =
      existingHydrationToken ?? beginThreadLiveStateHydration(threadID);
    const guard: LiveStateHydrationGuard = {
      activeTurnAtRequest: getActiveTurn(threadID),
      queueRevisionAtRequest: getQueueRevisionForThread(threadID),
      liveTodoRevisionAtRequest: options.liveTodoState.revision,
    };
    try {
      let snapshot: ThreadLiveState;
      try {
        snapshot = (await GetThreadLiveState(threadID)) as ThreadLiveState;
      } catch (err) {
        if (gen === options.getSwitchGeneration() && options.getThread()?.id === threadID) {
          console.error('Failed to hydrate thread live state:', err);
        }
        await hydratePendingInteractiveRequests(threadID, gen, hydrationToken);
        return;
      }
      if (gen !== options.getSwitchGeneration() || options.getThread()?.id !== threadID) return;
      if (!isThreadLiveStateHydrationCurrent(threadID, hydrationToken)) return;
      applyThreadLiveStateSnapshot(snapshot, threadID, guard);
    } finally {
      finishThreadLiveStateHydration(threadID, hydrationToken);
    }
  }

  return { hydrateThreadLiveState };
}
