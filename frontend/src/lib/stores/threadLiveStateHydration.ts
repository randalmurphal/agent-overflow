import type { Item, Thread } from '../types/models';
import type {
  PendingInteractiveRequests,
  ProviderSessionAccountEvent,
} from '../types/events';
import type { ThreadLiveState } from '../../../bindings/agent-overflow/internal/app/models';
import { GetThreadLiveState, ListPendingInteractiveRequests } from './bindings';
import { hasScope } from '../transport/scopes';
import type { LiveStateHydrationGuard } from './threadPaneShared';
import {
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
import { hydrateCompactingState } from './compactingState.svelte';

export interface ThreadLiveStateHydrationOptions {
  getThread(): Thread | null;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** The pane's createThreadPendingInteractiveState instance. */
  pendingInteractiveState: ThreadPendingInteractiveState;
  /** The pane's createLiveTodoState instance. */
  liveTodoState: LiveTodoState;
  getProviderSessionAccountRevision(): number;
  hydrateProviderAccount(
    account: ProviderSessionAccountEvent | null,
    expectedMutationRevision: number,
  ): void;
  getEffectiveModelRevision(): number;
  hydrateEffectiveModel(
    model: string,
    backendRevision: number,
    expectedMutationRevision: number,
  ): void;
}

export interface LiveStateFetchResult {
  /**
   * Pending-send timeline rows the backend has NOT persisted to SQLite
   * yet (a pending send's row lands on its wire echo). A caller
   * reconciling a SQLite page merges these in — the page is
   * structurally blind to them. Empty when the fetch failed.
   */
  deferredItems: Item[];
  /**
   * Apply the fetched snapshot to the pane and the global registries,
   * gen/token-guarded, entirely synchronously. Consumes the hydration
   * token (idempotent: second call no-ops). If never called, the caller
   * owns finishing the token.
   */
  apply(): void;
}

export interface ThreadLiveStateHydration {
  /**
   * Fetch the thread's live state (active turn, send queue, pending
   * interactive requests, live todos, deferred pending-send rows);
   * apply when the caller says so. Both authoritative install paths —
   * the cold-open sync leg and `refreshFromBackend` — fetch in
   * parallel with their SQLite page, merge the result's
   * `deferredItems` into the page, and commit the install and the
   * live-state apply back-to-back with no await between them, so the
   * timeline never paints the slice-only intermediate state (which is
   * missing pending sends and streaming partials).
   */
  startLiveStateFetch(
    threadID: string,
    gen: number,
    hydrationToken: number,
  ): Promise<LiveStateFetchResult>;
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

  function deferredItemsForThread(
    snapshot: ThreadLiveState,
    threadID: string,
  ): Item[] {
    const rows = (snapshot.deferredItems ?? []) as Item[];
    if (rows.length === 0) return rows;
    return rows.filter((row) => row.threadId === threadID);
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
    options.hydrateProviderAccount(
      (snapshot.providerAccount as ProviderSessionAccountEvent | undefined) ?? null,
      guard.providerSessionAccountRevisionAtRequest,
    );
    options.hydrateEffectiveModel(
      snapshot.effectiveModel ?? '',
      snapshot.effectiveModelRevision ?? 0,
      guard.effectiveModelRevisionAtRequest,
    );
    // Compacting can span minutes of wire silence, so a refresh inside the
    // window has no upcoming frame to learn it from — the snapshot is the
    // only source. 0 clears a flag the window's close outran.
    hydrateCompactingState(threadID, snapshot.compactingSinceUnixMs ?? 0);
  }

  async function startLiveStateFetch(
    threadID: string,
    gen: number,
    hydrationToken: number,
  ): Promise<LiveStateFetchResult> {
    // Guard values captured BEFORE the RPC leaves, exactly like the
    // single-phase form: apply-time comparisons against these detect
    // registries that moved while the snapshot was in flight.
    const guard: LiveStateHydrationGuard = {
      activeTurnAtRequest: getActiveTurn(threadID),
      queueRevisionAtRequest: getQueueRevisionForThread(threadID),
      liveTodoRevisionAtRequest: options.liveTodoState.revision,
      providerSessionAccountRevisionAtRequest:
        options.getProviderSessionAccountRevision(),
      effectiveModelRevisionAtRequest: options.getEffectiveModelRevision(),
    };
    const currentTarget = (): boolean =>
      gen === options.getSwitchGeneration() &&
      options.getThread()?.id === threadID;

    let snapshot: ThreadLiveState | null = null;
    let fallbackInteractive: PendingInteractiveRequests | null = null;
    // Opening a thread is a READ, so neither leg may be issued
    // speculatively. This runs on every thread switch, so a session that
    // holds neither grant would spend two refusals per open on state it
    // was never going to be shown. The snapshot rides `threads:operate`
    // and the fallback rides `approvals:respond`
    // (internal/transport/methods_gen.go), so each is asked for on its
    // own — a session may hold one and not the other.
    //
    // Only the SNAPSHOT is lost. The channels that keep an open thread
    // current are threads:read and reach a view-only session normally,
    // so a thread opened mid-turn still fills in as the turn streams.
    if (hasScope('threads:operate')) {
      try {
        snapshot = (await GetThreadLiveState(threadID)) as ThreadLiveState;
      } catch (err) {
        if (currentTarget()) {
          console.error('Failed to hydrate thread live state:', err);
        }
      }
    }
    // Degraded leg: pending approvals/questions block the user, so they
    // get their own fetch when the full snapshot did not land.
    if (snapshot === null && hasScope('approvals:respond')) {
      try {
        fallbackInteractive = (await ListPendingInteractiveRequests(
          threadID,
        )) as PendingInteractiveRequests;
      } catch (fallbackErr) {
        if (currentTarget()) {
          console.error(
            'Failed to hydrate pending interactive requests:',
            fallbackErr,
          );
        }
      }
    }

    let tokenConsumed = false;
    return {
      deferredItems: snapshot ? deferredItemsForThread(snapshot, threadID) : [],
      apply(): void {
        if (tokenConsumed) return;
        try {
          if (!currentTarget()) return;
          if (!isThreadLiveStateHydrationCurrent(threadID, hydrationToken)) {
            return;
          }
          if (snapshot) {
            applyThreadLiveStateSnapshot(snapshot, threadID, guard);
          } else if (fallbackInteractive) {
            applyPendingInteractiveSnapshot(threadID, fallbackInteractive);
          }
        } finally {
          tokenConsumed = true;
          finishThreadLiveStateHydration(threadID, hydrationToken);
        }
      },
    };
  }

  return { startLiveStateFetch };
}
