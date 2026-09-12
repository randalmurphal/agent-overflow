// Sidebar-grade live activity for every thread of one computer, read as one
// snapshot and applied to the global registries the pills read from
// (threadStatuses, compactingState).
//
// The threads:read push channels (provider:turn_started / turn_completed,
// provider:approval, provider:user_input, provider:compacting) keep a
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
// The active-turn write keeps the same race guard as the pane path: a push
// that landed while the snapshot was in flight is newer than the snapshot
// and wins.

import { pendingBackendReplay } from './transportRecovery';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { threadBackend, threadIdsForBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import type { ThreadLiveActivity } from '../../../bindings/agent-overflow/internal/app/models';
import { ListThreadLiveActivity } from './bindings';
import { compactingRevision, hydrateCompactingState } from './compactingState.svelte';
import {
  getCanonicalActiveTurn as getActiveTurn,
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
  return { turn: getActiveTurn(threadId), interactive: interactiveRequestsRevision(threadId), compacting: compactingRevision(threadId) };
}
function applyRow(row: ThreadLiveActivity, guard: ActivityGuard): void {
  applyActiveTurn(row.threadId, row.activeTurn, guard.turn);
  if (interactiveRequestsRevision(row.threadId) === guard.interactive) {
    replaceInteractiveRequestsForThread(row.threadId, {
      approvals: (row.approvalRequestIds ?? []).map((requestId) => ({ requestId })),
      userInputs: (row.userInputRequestIds ?? []).map((requestId) => ({ requestId })),
    });
  }
  hydrateCompactingState(row.threadId, row.compactingSinceUnixMs ?? 0, guard.compacting);
}

/**
 * Read `backend`'s live activity and reconcile every thread attributed to
 * it. Resolves without reading when the session lacks threads:read there:
 * the channels this mirrors would not reach it either. Rejects with the
 * transport error otherwise, for the caller to report.
 */
export async function reconcileThreadLiveActivity(backend: BackendKey): Promise<void> {
  const replay = pendingBackendReplay(backend);
  if (replay) await replay;
  if (!hasScope('threads:read', backend)) return;
  // Captured before the read leaves: the guard tells a snapshot that
  // crossed a push apart from one that did not. The entity index rather
  // than the sidebar list, because it holds every thread this client has
  // attributed to the computer (rows, panes, search results, archived).
  const atRequest = new Map<string, ActivityGuard>();
  for (const threadId of threadIdsForBackend(backend)) atRequest.set(threadId, captureActivity(threadId));
  const rows = (await withBackendTarget(backend, () => ListThreadLiveActivity())) as ThreadLiveActivity[] | null;
  const named = new Set<string>();
  for (const row of rows ?? []) {
    if (!row?.threadId) continue;
    named.add(row.threadId);
    const owner = threadBackend(row.threadId);
    if (owner !== undefined && owner !== backend) continue;
    applyRow(row, atRequest.get(row.threadId) ?? { turn: null, interactive: 0, compacting: 0 });
  }
  for (const [threadId, guard] of atRequest) {
    if (!named.has(threadId) && threadBackend(threadId) === backend) {
      applyRow({ threadId, activeTurn: null, approvalRequestIds: [], userInputRequestIds: [], compactingSinceUnixMs: 0 } as ThreadLiveActivity, guard);
    }
  }
}
