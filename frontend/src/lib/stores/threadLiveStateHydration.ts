import { isPendingFlushRow } from '../utils/userMessageMeta';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import {
  codexAgentRevision,
  hydrateCodexAgents,
  hydrateSubagentProgress,
  subagentProgressRevision,
} from './subagentProgress.svelte';
import { threadHasScope } from '../transport/entityScopes';
import type { Item, Thread } from '../types/models';
import type {
  PendingInteractiveRequests,
  ProviderSessionAccountEvent,
  SubagentProgressEvent,
} from '../types/events';
import type { ThreadLiveState } from '../../../bindings/agent-overflow/internal/app/models';
import { GetThreadItem, GetThreadLiveState, ListPendingInteractiveRequests } from './bindings';
import type { LiveStateHydrationGuard } from './threadPaneShared';
import {
  finishThreadLiveStateHydration,
  getCanonicalActiveTurn as getActiveTurn,
  isThreadLiveStateHydrationCurrent,
  projectTurnCompleted,
  projectTurnStarted,
  replaceInteractiveRequestsForThread,
  sameActiveTurn,
  type ActiveTurn,
} from './threadStatuses.svelte';
import {
  getQueueRevisionForThread,
  getFlushedForThread,
  getQueueForThread,
  markQueuedItemConsumed,
  queueItemFromWire,
  replaceFlushedForThread,
  replaceQueueForThread,
  type FlushedItem,
  type QueueItem as SendQueueItem,
} from './sendQueue.svelte';
import type { ThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import type { LiveTodoState } from './liveTodoState.svelte';
import { compactingRevision, hydrateCompactingState } from './compactingState.svelte';

export interface ThreadLiveStateHydrationOptions {
  getThread(): Thread | null;
  getItems(): readonly Item[];
  isOptimisticItem(itemId: string): boolean;
  confirmOptimisticSend(threadId: string, sendId: string | undefined, canonicalItemId?: string): void;
  /** The pane's send-queue render handover (thread.svelte.ts). */
  syncRenderedFlushRows(): void;
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
  /**
   * Snapshot leg of the stale-binary banner (`sessionCliVersion` /
   * `installedCliVersion`, both set only while the thread's live session
   * runs an older CLI than the installed binary). Set-only by contract:
   * clears stay with the `provider:status` / disconnect events.
   */
  hydrateBinaryStaleBanner(sessionVersion: string, installedVersion: string): void;
}

export interface LiveStateFetchResult {
  /** Present when the full execution snapshot could not be fetched. */
  error?: unknown;
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
   * owns finishing the token. onStaleQueue requests a fresh snapshot when
   * queue events overtook this read without restating pending dispatches.
   */
  apply(onStaleQueue: () => void): void;
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
  function deferredItemsForThread(
    snapshot: ThreadLiveState,
    threadID: string,
  ): Item[] {
    const rows = (snapshot.deferredItems ?? []) as Item[];
    if (rows.length === 0) return rows;
    return rows.filter((row) => row.threadId === threadID);
  }

  function applyActiveTurnSnapshot(
    snapshot: ThreadLiveState,
    threadID: string,
    activeTurnAtRequest: ActiveTurn | null,
  ): void {
    if (snapshot.threadId !== threadID) return;
    const current = getActiveTurn(threadID);
    if (!sameActiveTurn(current, activeTurnAtRequest)) return;
    const active = snapshot.activeTurn;
    if (active && active.threadId === threadID && active.turnId) {
      projectTurnStarted(threadID, active.turnId, active.turnIndex, active.startedAt);
    } else if (current) {
      projectTurnCompleted(threadID, current.turnId);
    }
  }

  function applyThreadLiveStateSnapshot(
    snapshot: ThreadLiveState,
    threadID: string,
    guard: LiveStateHydrationGuard,
    applyInteractive: (snapshot: PendingInteractiveRequests | null | undefined) => void,
  ): void {
    if (snapshot.threadId !== threadID) return;
    hydrateCodexAgents(threadID, (snapshot.codexAgents ?? []) as Item[], guard.codexAgentRevisionAtRequest);
    hydrateSubagentProgress(
      threadID,
      (snapshot.subagentProgress ?? []) as SubagentProgressEvent[],
      guard.subagentProgressRevisionAtRequest,
    );
    applyActiveTurnSnapshot(snapshot, threadID, guard.activeTurnAtRequest);

    if (getQueueRevisionForThread(threadID) === guard.queueRevisionAtRequest) {
      const queueItems: SendQueueItem[] = (snapshot.queueItems ?? [])
        .filter((item) => item.threadId === threadID)
        .map(queueItemFromWire);
      replaceQueueForThread(threadID, queueItems);
      const flushedItems: FlushedItem[] = (snapshot.flushedItems ?? [])
        .filter((item) => item.userItemId && item.queueItemId)
        .map((item) => ({
          sendId: item.sendId,
          queueItemId: item.queueItemId,
          userItemId: item.userItemId,
          message: item.message,
          flushedAt: Date.now(),
        }));
      replaceFlushedForThread(threadID, flushedItems);
      // The snapshot lists every send the backend still holds unconfirmed,
      // including quiet reservations loaded by this pane's history read.
      // Only confirmed rows may hand over to the timeline; pendingFlush
      // keeps reservations in the preview across navigation and refresh.
      for (const item of queueItems) options.confirmOptimisticSend(threadID, item.sendId);
      for (const item of flushedItems) {
        options.confirmOptimisticSend(threadID, item.sendId, item.userItemId);
      }
    }

    // Recovery may learn consumption from history before the admission
    // reply or echo reaches this client. Provisional timeline rows cannot
    // acknowledge their own submission.
    if (getQueueForThread(threadID).length > 0) {
      for (const item of options.getItems()) {
        if (!options.isOptimisticItem(item.id)) markQueuedItemConsumed(item);
      }
    }
    options.syncRenderedFlushRows();

    applyInteractive(snapshot.interactive as PendingInteractiveRequests);

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
    hydrateCompactingState(threadID, snapshot.compactingSinceUnixMs ?? 0, guard.compactingRevisionAtRequest);
    options.hydrateBinaryStaleBanner(
      snapshot.sessionCliVersion ?? '',
      snapshot.installedCliVersion ?? '',
    );
  }

  async function startLiveStateFetch(
    threadID: string,
    gen: number,
    hydrationToken: number,
  ): Promise<LiveStateFetchResult> {
    // Guard values captured BEFORE the RPC leaves, exactly like the
    // single-phase form: apply-time comparisons against these detect
    // registries that moved while the snapshot was in flight.
    const backend = requireEntityBackend(threadBackend(threadID));
    const reconcileInteractive = options.pendingInteractiveState.beginSnapshot();
    const applyInteractive = (snapshot: PendingInteractiveRequests | null | undefined): void => {
      replaceInteractiveRequestsForThread(threadID, reconcileInteractive(snapshot));
    };
    const guard: LiveStateHydrationGuard = {
      compactingRevisionAtRequest: compactingRevision(threadID),
      codexAgentRevisionAtRequest: codexAgentRevision(threadID),
      subagentProgressRevisionAtRequest: subagentProgressRevision(threadID),
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

    const previousFlushed = getFlushedForThread(threadID);
    let snapshot: ThreadLiveState | null = null;
    let snapshotError: unknown;
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
    if (threadHasScope('threads:operate', threadID)) {
      try {
        snapshot = (await withBackendTarget(backend, () => GetThreadLiveState(threadID))) as ThreadLiveState;
        if (snapshot && previousFlushed.length > 0) {
          // The backend stops tracking a send on consumption; this screen
          // keeps its preview until rendering. Resolve only missing markers
          // against history so recovery distinguishes a consumed row outside
          // the window from a message restored or removed while disconnected.
          const queuedIDs = new Set((snapshot.queueItems ?? []).map(item => item.id));
          const flushedIDs = new Set((snapshot.flushedItems ?? []).map(item => item.userItemId));
          const missing = previousFlushed.filter(item => !queuedIDs.has(item.queueItemId) && !flushedIDs.has(item.userItemId));
          const retained = await Promise.all(missing.map(async item => {
            const row = await withBackendTarget(backend, () => GetThreadItem(threadID, item.userItemId));
            return row?.id === item.userItemId && row.kind === 'user_text' && !isPendingFlushRow(row as Item) ? item : null;
          }));
          snapshot = { ...snapshot, flushedItems: [
            ...(snapshot.flushedItems ?? []),
            ...retained.filter((item): item is FlushedItem => item !== null),
          ] } as ThreadLiveState;
        }
      } catch (err) {
        snapshot = null;
        snapshotError = err;
        if (currentTarget()) {
          console.error('Failed to hydrate thread live state:', err);
        }
      }
    }
    // Degraded leg: pending approvals/questions block the user, so they
    // get their own fetch when the full snapshot did not land.
    if (snapshot === null && threadHasScope('approvals:respond', threadID)) {
      try {
        fallbackInteractive = (await withBackendTarget(backend, () => ListPendingInteractiveRequests(threadID))) as PendingInteractiveRequests;
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
      error: snapshotError,
      deferredItems: snapshot ? deferredItemsForThread(snapshot, threadID) : [],
      apply(onStaleQueue): void {
        if (tokenConsumed) return;
        try {
          if (!currentTarget()) return;
          if (!isThreadLiveStateHydrationCurrent(threadID, hydrationToken)) {
            return;
          }
          if (snapshot) {
            const queueStale = getQueueRevisionForThread(threadID) !== guard.queueRevisionAtRequest;
            applyThreadLiveStateSnapshot(snapshot, threadID, guard, applyInteractive);
            // Queue events do not restate pending dispatches. A superseded
            // snapshot still owes recovery of a potentially missed flush.
            if (queueStale) onStaleQueue();
          } else if (fallbackInteractive) {
            applyInteractive(fallbackInteractive);
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
