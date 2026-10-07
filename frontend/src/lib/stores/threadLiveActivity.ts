// Sidebar-grade live activity for every thread of one computer, read as one
// snapshot and applied to the global registries the pills read from
// (threadStatuses, compactingState).
//
// The threads:read push channels (provider:turn_started / turn_completed,
// provider:approval, provider:user_input, provider:compacting,
// provider:sends_pending) keep a
// connected client current. A client that connects mid-turn, reloads, or
// drops frames learns nothing from them about threads it has no pane on:
// the pane path (threadLiveStateHydration.ts) reads one thread, and only
// with threads:operate. This is the snapshot leg for everything else, and
// it runs from computerHydration on every connection edge and from the gap
// handler when one of those channels dropped frames.
//
// The answer is authoritative in both directions for the computer it came
// from: a thread it names is projected, and a thread this client attributes
// to that computer (transport/entityIndex) which it does not name is idle.
// Every write keeps the same race guard as the pane path: a push that
// landed while the snapshot was in flight is newer than the snapshot and
// wins. Reads of one computer run one at a time (`reconcileThreadLiveActivity`).
// A thread the answer names that this client had not attributed to any
// computer is attributed to this one, so a later answer that omits it, or
// the computer's removal, settles it too.

import { awaitBackendReplay } from './transportRecovery';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { noteThread, threadBackend, threadIdsForBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import type { ThreadLiveActivity } from '../../../bindings/agent-overflow/internal/app/models';
import { ListThreadLiveActivity } from './bindings';
import { compactingRevision, hydrateCompactingState } from './compactingState.svelte';
import {
  beginSendsPendingRead,
  endSendsPendingRead,
  getCanonicalActiveTurn as getActiveTurn,
  hydrateSendsPending,
  interactiveRequestsRevision,
  projectTurnCompleted,
  projectTurnStarted,
  replaceInteractiveRequestsForThread,
  sameActiveTurn,
  type ActiveTurn,
} from './threadStatuses.svelte';

function applyActiveTurn(threadId: string, active: ThreadLiveActivity['activeTurn'], atRequest: ActiveTurn | null): void {
  const current = getActiveTurn(threadId);
  if (!sameActiveTurn(current, atRequest)) return;
  if (active && active.threadId === threadId && active.turnId) {
    projectTurnStarted(threadId, active.turnId, active.turnIndex, active.startedAt);
  } else if (current) {
    projectTurnCompleted(threadId, current.turnId);
  }
}

interface ActivityGuard {
  turn: ActiveTurn | null;
  interactive: number;
  compacting: number;
}
function captureActivity(threadId: string): ActivityGuard {
  return {
    turn: getActiveTurn(threadId),
    interactive: interactiveRequestsRevision(threadId),
    compacting: compactingRevision(threadId),
  };
}
function applyRow(row: ThreadLiveActivity, guard: ActivityGuard, sendsPendingChanged: ReadonlySet<string>): void {
  applyActiveTurn(row.threadId, row.activeTurn, guard.turn);
  if (interactiveRequestsRevision(row.threadId) === guard.interactive) {
    replaceInteractiveRequestsForThread(row.threadId, {
      approvals: (row.approvalRequestIds ?? []).map((requestId) => ({ requestId })),
      userInputs: (row.userInputRequestIds ?? []).map((requestId) => ({ requestId })),
    });
  }
  hydrateCompactingState(row.threadId, row.compactingSinceUnixMs ?? 0, guard.compacting);
  hydrateSendsPending(row.threadId, row.sendsPending === true, sendsPendingChanged);
}

// Per computer, the read in flight and the one queued behind it. A request
// made while a read is out shares the queued read, which starts once the
// read ahead of it settles, so every request is answered by a read that
// began after it and no two answers of one computer cross. Detaching drops
// both: an answer that arrives after it is not applied.
interface Reads {
  current: Promise<void> | null;
  queued: Promise<void> | null;
}
const readsByBackend = new Map<BackendKey, Reads>();
onBackendDetached(({ backendId }) => {
  readsByBackend.delete(backendId);
});

/**
 * Read `backend`'s live activity and reconcile every thread attributed to
 * it. Resolves without reading when the session lacks threads:read there:
 * the channels this mirrors would not reach it either. Rejects with the
 * transport error otherwise, for the caller to report.
 */
export function reconcileThreadLiveActivity(backend: BackendKey): Promise<void> {
  let reads = readsByBackend.get(backend);
  if (!reads) readsByBackend.set(backend, reads = { current: null, queued: null });
  if (reads.queued) return reads.queued;
  const own = reads;
  const ahead = own.current;
  const read: Promise<void> = ahead
    ? ahead.then(noop, noop).then(() => {
      own.queued = null;
      if (readsByBackend.get(backend) !== own) return;
      own.current = read;
      return readThreadLiveActivity(backend, own);
    })
    : readThreadLiveActivity(backend, own);
  if (ahead) own.queued = read;
  else own.current = read;
  const settle = () => {
    if (own.current !== read) return;
    own.current = null;
    if (!own.queued && readsByBackend.get(backend) === own) readsByBackend.delete(backend);
  };
  read.then(settle, settle);
  return read;
}

function noop(): void {}

async function readThreadLiveActivity(backend: BackendKey, own: Reads): Promise<void> {
  const replay = awaitBackendReplay(backend);
  if (replay) await replay;
  if (!hasScope('threads:read', backend)) return;
  // Captured before the read leaves: the guard tells a snapshot that
  // crossed a push apart from one that did not. The entity index rather
  // than the sidebar list, because it holds every thread this client has
  // attributed to the computer (rows, panes, search results, archived).
  const atRequest = new Map<string, ActivityGuard>();
  for (const threadId of threadIdsForBackend(backend)) atRequest.set(threadId, captureActivity(threadId));
  const sendsPendingChanged = beginSendsPendingRead();
  let rows: ThreadLiveActivity[] | null;
  try {
    rows = (await withBackendTarget(backend, () => ListThreadLiveActivity())) as ThreadLiveActivity[] | null;
  } finally {
    endSendsPendingRead(sendsPendingChanged);
  }
  if (readsByBackend.get(backend) !== own) return;
  const named = new Set<string>();
  for (const row of rows ?? []) {
    if (!row?.threadId) continue;
    named.add(row.threadId);
    const owner = threadBackend(row.threadId);
    if (owner === undefined) noteThread(row.threadId, backend);
    else if (owner !== backend) continue;
    applyRow(row, atRequest.get(row.threadId) ?? { turn: null, interactive: 0, compacting: 0 }, sendsPendingChanged);
  }
  for (const [threadId, guard] of atRequest) {
    if (!named.has(threadId) && threadBackend(threadId) === backend) {
      applyRow({ threadId, activeTurn: null, approvalRequestIds: [], userInputRequestIds: [], compactingSinceUnixMs: 0 } as ThreadLiveActivity, guard, sendsPendingChanged);
    }
  }
}
