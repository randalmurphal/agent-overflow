import type { ApprovalKind } from '../types/events';
import type { Item } from '../types/models';

// Global per-thread live-status projection for the sidebar. Chat state is
// authoritative in the unified item stream; this store keeps the minimal
// derived signal the thread list needs for off-pane rows (running, pending
// approval, error). No persistence: an empty map at boot is acceptable and
// rehydrates from the next live event.
//
// Running is derived from three orthogonal sources, OR'd together:
//
//   1. Optimistic send: the user hit the Send button and we've dispatched
//      SendMessage to the backend. This covers the new-thread spawn gap
//      (provider session cold-starts take multiple seconds before the
//      first turn_started event) so the pill flips the moment the user
//      clicks Send rather than waiting for the provider to come up.
//   2. Active turn: the backend emitted `provider:turn_started` for this
//      thread and hasn't emitted `provider:turn_completed` yet. This is
//      the authoritative "the provider is working right now" signal
//      (invariant 22: turn activity is wire-pushed, never derived from
//      items).
//   3. Active items: a streaming assistant_text / thinking item or a
//      running tool_call that hasn't yet settled. This catches any stray
//      running work outside a turn (e.g. a background tool outliving the
//      turn on Claude).
//
// Any of the three → `running`. All three clear → step through
// pending-approval → error → idle.

export type ThreadLiveStatus =
  | 'idle'
  | 'running'
  | 'awaiting-input'
  | 'pending-approval'
  | 'plan-ready'
  | 'error';

/**
 * Approval kinds that represent the agent asking the USER a question
 * (or requesting structured input) — distinct from a permission ask
 * for a destructive action. The sidebar renders these as
 * `awaiting-input` rather than `pending-approval` so the user can
 * distinguish "please fill in a form" from "the agent wants to rm -rf".
 * Matches forge's Sidebar.logic.ts split.
 */
const AWAITING_INPUT_KINDS: ReadonlySet<ApprovalKind> = new Set([
  'user-input',
  'mcp-elicitation',
]);

let statuses: Map<string, ThreadLiveStatus> = $state(new Map());
const activeItemIDsByThread = new Map<string, Set<string>>();
const approvalIDsByThread = new Map<string, Set<string>>();
const awaitingInputIDsByThread = new Map<string, Set<string>>();
const approvalThreadByID = new Map<string, string>();
const activeTurnIDsByThread = new Map<string, Set<string>>();
const pendingSendThreads = new Set<string>();
const planReadyThreads = new Set<string>();
const errorThreads = new Set<string>();

function trackedIDsFor(map: Map<string, Set<string>>, threadId: string): Set<string> {
  let ids = map.get(threadId);
  if (!ids) {
    ids = new Set<string>();
    map.set(threadId, ids);
  }
  return ids;
}

function removeTrackedID(map: Map<string, Set<string>>, threadId: string, id: string): void {
  const ids = map.get(threadId);
  if (!ids) return;
  ids.delete(id);
  if (ids.size === 0) {
    map.delete(threadId);
  }
}

function isActiveTimelineItem(item: Item): boolean {
  return (
    ((item.kind === 'assistant_text' || item.kind === 'thinking') && item.status === 'streaming')
    || (item.kind === 'tool_call' && item.status === 'running' && !item.isBackground)
  );
}

function recalculateThreadStatus(threadId: string): void {
  // Priority mirrors forge's Sidebar.logic.ts. Blocking approvals
  // outrank everything else because the provider can't advance until
  // the user acts; awaiting-input is a softer call-to-action that
  // still blocks progress but isn't protecting a destructive action.
  // Both sit above `running` for the same reason: the user needs to
  // see them even if a background tool is still spinning.
  if ((approvalIDsByThread.get(threadId)?.size ?? 0) > 0) {
    setThreadStatus(threadId, 'pending-approval');
    return;
  }
  if ((awaitingInputIDsByThread.get(threadId)?.size ?? 0) > 0) {
    setThreadStatus(threadId, 'awaiting-input');
    return;
  }
  if (
    pendingSendThreads.has(threadId)
    || (activeTurnIDsByThread.get(threadId)?.size ?? 0) > 0
    || (activeItemIDsByThread.get(threadId)?.size ?? 0) > 0
  ) {
    setThreadStatus(threadId, 'running');
    return;
  }
  // Plan-ready sits below running because while a plan exists, the
  // turn that produced it has settled and the user's next decision
  // drives the next turn. A fresh turn clears the plan-ready flag
  // (see projectTurnStarted).
  if (planReadyThreads.has(threadId)) {
    setThreadStatus(threadId, 'plan-ready');
    return;
  }
  if (errorThreads.has(threadId)) {
    setThreadStatus(threadId, 'error');
    return;
  }
  setThreadStatus(threadId, 'idle');
}

/**
 * Returns the live status for a thread, or 'idle' when nothing has been
 * recorded for this id yet. 'idle' is the same signal the sidebar uses
 * to render no dot, so callers don't need to special-case undefined.
 */
export function getThreadStatus(threadId: string): ThreadLiveStatus {
  return statuses.get(threadId) ?? 'idle';
}

/**
 * Set or replace a thread's live status. Writing 'idle' is equivalent
 * to clearing — we drop the entry so the map doesn't grow with stale
 * idle-only rows across long sessions.
 */
export function setThreadStatus(threadId: string, status: ThreadLiveStatus): void {
  if (status === 'idle') {
    if (!statuses.has(threadId)) return;
    const next = new Map(statuses);
    next.delete(threadId);
    statuses = next;
    return;
  }
  const current = statuses.get(threadId);
  if (current === status) return;
  statuses = new Map(statuses).set(threadId, status);
}

/**
 * Drop any status for this thread. Called when a thread is
 * archived/deleted so its dot doesn't linger in the sidebar.
 */
export function clearThreadStatus(threadId: string): void {
  activeItemIDsByThread.delete(threadId);
  activeTurnIDsByThread.delete(threadId);
  pendingSendThreads.delete(threadId);
  planReadyThreads.delete(threadId);
  for (const requestIdSet of [
    approvalIDsByThread.get(threadId),
    awaitingInputIDsByThread.get(threadId),
  ]) {
    if (!requestIdSet) continue;
    for (const requestId of requestIdSet) {
      approvalThreadByID.delete(requestId);
    }
  }
  approvalIDsByThread.delete(threadId);
  awaitingInputIDsByThread.delete(threadId);
  errorThreads.delete(threadId);
  if (!statuses.has(threadId)) return;
  const next = new Map(statuses);
  next.delete(threadId);
  statuses = next;
}

/**
 * Optimistically flip a thread to running the moment the composer
 * dispatches SendMessage. This covers the cold-start window for new
 * threads where the provider session takes multiple seconds to spawn
 * before emitting its first `provider:turn_started` — without this the
 * sidebar row would sit idle while the user is clearly waiting on the
 * provider. Paired with projectSendFailed / turn lifecycle events to
 * clear the flag.
 */
export function projectSendStarted(threadId: string): void {
  if (!threadId) return;
  // A fresh send clears any prior error so the pill stops reading
  // "Error" — the user has moved past that failure.
  errorThreads.delete(threadId);
  pendingSendThreads.add(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Clear the pending-send flag. Called by composerSend when SendMessage
 * rejects (with `error: true` so the pill flips to Error rather than
 * idle) or when turn lifecycle events arrive and take over ownership
 * of the running signal.
 */
export function projectSendResolved(threadId: string, opts: { error?: boolean } = {}): void {
  if (!threadId) return;
  pendingSendThreads.delete(threadId);
  if (opts.error) {
    errorThreads.add(threadId);
  }
  recalculateThreadStatus(threadId);
}

/**
 * Mark a turn as in-flight. Fired from applyTurnStarted, the only
 * place that can set activeTurn on a pane (invariant 22). A matching
 * projectTurnCompleted clears it.
 */
export function projectTurnStarted(threadId: string, turnId: string): void {
  if (!threadId || !turnId) return;
  // Turn-started always supersedes a prior optimistic send flag (we
  // have real backend confirmation now) AND clears any prior error
  // from an earlier turn on the same thread. It also clears the
  // plan-ready flag: if a plan was proposed and the user's acceptance
  // kicked off a new turn, the plan is no longer "awaiting a decision"
  // — the decision happened. Rejecting a plan also fires a new turn
  // (agent adjusts and re-proposes), so clearing here is correct in
  // both acceptance and rejection cases.
  pendingSendThreads.delete(threadId);
  planReadyThreads.delete(threadId);
  errorThreads.delete(threadId);
  trackedIDsFor(activeTurnIDsByThread, threadId).add(turnId);
  recalculateThreadStatus(threadId);
}

/**
 * Clear an in-flight turn. `aborted` / `errorMessage` flip the thread
 * to the error state so the sidebar pill reads "Error" until the next
 * send or turn.
 */
export function projectTurnCompleted(
  threadId: string,
  turnId: string,
  opts: { aborted?: boolean; errorMessage?: string } = {},
): void {
  if (!threadId || !turnId) return;
  removeTrackedID(activeTurnIDsByThread, threadId, turnId);
  if (opts.aborted || (opts.errorMessage && opts.errorMessage.length > 0)) {
    errorThreads.add(threadId);
  }
  recalculateThreadStatus(threadId);
}

/**
 * Feed a live item upsert into the sidebar-status projection. This is the
 * canonical chat-state path: running rows keep the dot hot, terminal rows
 * clear their own ids, and inline error rows leave the thread in an error
 * state until a new active item or user turn supersedes it.
 */
export function projectThreadItem(item: Item): void {
  if (!item?.threadId || !item.id) return;

  if (item.kind === 'error') {
    errorThreads.add(item.threadId);
  } else if (item.kind === 'user_text' || isActiveTimelineItem(item)) {
    errorThreads.delete(item.threadId);
  }

  if (isActiveTimelineItem(item)) {
    trackedIDsFor(activeItemIDsByThread, item.threadId).add(item.id);
  } else {
    removeTrackedID(activeItemIDsByThread, item.threadId, item.id);
  }

  // A completed proposed_plan item means the agent has produced a plan
  // and is waiting on the user's Accept / Edit / Reject decision. The
  // sidebar pill flips to "Plan ready" so an off-pane user doesn't have
  // to open the thread to discover the plan is sitting there. Status
  // 'errored' / 'declined' don't set plan-ready — those are terminal
  // failures, not actionable plans.
  if (item.kind === 'proposed_plan' && item.status === 'completed') {
    planReadyThreads.add(item.threadId);
  }

  recalculateThreadStatus(item.threadId);
}

/**
 * Feed an approval request into the projection. The `kind` discriminates
 * a user-input / MCP-elicitation (→ awaiting-input) from a permission /
 * command / file-change approval (→ pending-approval). An unknown or
 * missing kind defaults to pending-approval because that's the safer
 * read: a blocking approval the user didn't expect still gets their
 * attention; a soft input prompt that accidentally rendered as
 * pending-approval is lower-impact.
 */
export function projectApprovalRequest(
  threadId: string,
  requestId: string,
  kind?: ApprovalKind,
): void {
  if (!threadId || !requestId) return;
  approvalThreadByID.set(requestId, threadId);
  if (kind && AWAITING_INPUT_KINDS.has(kind)) {
    trackedIDsFor(awaitingInputIDsByThread, threadId).add(requestId);
  } else {
    trackedIDsFor(approvalIDsByThread, threadId).add(requestId);
  }
  errorThreads.delete(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Feed an approval resolution into the projection. Resolve payloads should
 * include threadId, but we also track requestId -> threadId so late or
 * partial events still clean up correctly. We remove from BOTH buckets
 * unconditionally — the caller doesn't know which bucket the request
 * landed in, and a resolution always clears the same id.
 */
export function projectApprovalResolution(
  threadId: string | null | undefined,
  requestId: string,
): void {
  if (!requestId) return;
  const ownerThreadId = threadId ?? approvalThreadByID.get(requestId);
  if (!ownerThreadId) return;
  approvalThreadByID.delete(requestId);
  removeTrackedID(approvalIDsByThread, ownerThreadId, requestId);
  removeTrackedID(awaitingInputIDsByThread, ownerThreadId, requestId);
  recalculateThreadStatus(ownerThreadId);
}

/**
 * Mark a thread as having a proposed plan that the user hasn't acted
 * on yet. Fires automatically from projectThreadItem when a
 * proposed_plan item lands with a terminal (completed) status; also
 * exposed so other call sites can force the flag if we add richer
 * plan-resolution events later. Cleared on the next turn start
 * (projectTurnStarted) or by an explicit projectPlanResolved.
 */
export function projectPlanReady(threadId: string): void {
  if (!threadId) return;
  planReadyThreads.add(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Clear the plan-ready flag. Called by projectTurnStarted (the user
 * accepted / rejected / edited the plan and a new turn is running)
 * or explicitly if the UI wires a distinct "dismiss plan" action.
 */
export function projectPlanResolved(threadId: string): void {
  if (!threadId) return;
  if (!planReadyThreads.has(threadId)) return;
  planReadyThreads.delete(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Count of threads currently in a non-idle state. Reactive via $state,
 * so any future sidebar section-header badge ("3 active") can bind to
 * this without subscribing to the whole map.
 */
export function getNonIdleThreadCount(): number {
  return statuses.size;
}

/**
 * Read-only view onto the status map. Primarily for tests that want to
 * assert "nothing other than these threads has a status".
 */
export function getAllThreadStatuses(): Map<string, ThreadLiveStatus> {
  return statuses;
}

/**
 * Wipe the entire map. Only intended for test isolation — production
 * code should use clearThreadStatus per id.
 */
export function resetForTest(): void {
  activeItemIDsByThread.clear();
  activeTurnIDsByThread.clear();
  pendingSendThreads.clear();
  planReadyThreads.clear();
  approvalIDsByThread.clear();
  awaitingInputIDsByThread.clear();
  approvalThreadByID.clear();
  errorThreads.clear();
  if (statuses.size === 0) return;
  statuses = new Map();
}
