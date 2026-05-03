import { SvelteMap } from 'svelte/reactivity';
import type { ApprovalKind } from '../types/events';
import type { Item } from '../types/models';
import { clearForThread as clearSendQueueForThread } from './sendQueue.svelte';

/**
 * ActiveTurn is the live in-flight turn for a thread. Populated
 * exclusively from the `provider:turn_started` wire event; cleared on
 * `provider:turn_completed`. Never hydrated from persistence —
 * invariant 22 (turn activity is wire-pushed, never derived from
 * items).
 *
 * The shape lives here because this store owns the per-thread active-
 * turn map: both the sidebar pill and the chat working indicator read
 * from it (single source of truth). Survives thread switches because
 * nothing clears it on switch — that's the load-bearing fix for "I
 * switched threads and lost the working indicator on a turn that's
 * still in flight."
 */
export interface ActiveTurn {
  turnId: string;
  turnIndex: number;
  /** Unix-millis. Anchors the self-ticking working-indicator timer. */
  startedAt: number;
}

// Global per-thread live-status projection for the sidebar. Chat state is
// authoritative in the unified item stream; this store keeps the minimal
// derived signal the thread list needs for off-pane rows (running, pending
// approval, error). Durable boot status such as interrupted turns and
// actionable proposed plans is derived from Thread rows instead.
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
// Recalculation priority is: pending approval, awaiting input, running,
// error, plan-ready, interrupted, idle.

export type ThreadLiveStatus =
  | 'idle'
  | 'running'
  | 'awaiting-input'
  | 'pending-approval'
  | 'plan-ready'
  | 'error'
  | 'interrupted';

let statuses: Map<string, ThreadLiveStatus> = $state(new Map());
const activeItemIDsByThread = new Map<string, Set<string>>();
const approvalIDsByThread = new Map<string, Set<string>>();
const awaitingInputIDsByThread = new Map<string, Set<string>>();
const approvalThreadByID = new Map<string, string>();
// activeTurnByThread is the global per-thread ActiveTurn registry —
// the single source of truth for "is a turn in flight on this
// thread?". SvelteMap (from `svelte/reactivity`) is the
// doc-recommended pattern for reactive Map state in Svelte 5: .set /
// .delete are tracked individually, so writers don't have to rebuild
// the binding via `new Map(prev).set(...)` on every update. Readers
// (`getActiveTurn`, the OR projection in `recalculateThreadStatus`)
// see the same value either way; only the write hot path benefits.
//
// Under the per-wire-round emission cadence (see
// internal/triage/AGENTS.md "Wire-round vs logical-turn"), each entry
// represents the CURRENT wire round — not the user-typed logical
// turn. The two are decoupled on the backend; the frontend only
// observes one signal stream and treats each round as its own active
// turn. This is what flips the working indicator off between rounds
// in Claude's multi-result-per-turn cascade so the UI reflects "model
// is engaged right now" rather than "user-typed prompt is in flight."
const activeTurnByThread = new SvelteMap<string, ActiveTurn>();
const completedTurnIDsByThread = new Map<string, Set<string>>();
const pendingSendThreads = new Set<string>();
const planReadyThreads = new Set<string>();
const errorThreads = new Set<string>();
const interruptedThreads = new Set<string>();

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
    || activeTurnByThread.has(threadId)
    || (activeItemIDsByThread.get(threadId)?.size ?? 0) > 0
  ) {
    setThreadStatus(threadId, 'running');
    return;
  }
  if (errorThreads.has(threadId)) {
    setThreadStatus(threadId, 'error');
    return;
  }
  // Plan-ready sits below running/error because while a plan exists,
  // the turn that produced it has settled and the user's next decision
  // drives the next turn. A fresh turn clears the plan-ready flag
  // (see projectTurnStarted).
  if (planReadyThreads.has(threadId)) {
    setThreadStatus(threadId, 'plan-ready');
    return;
  }
  if (interruptedThreads.has(threadId)) {
    setThreadStatus(threadId, 'interrupted');
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
 * archived/deleted so its dot doesn't linger in the sidebar. Also
 * sweeps the per-thread send queue: queued user messages are in-memory
 * only and must not outlive their thread.
 */
export function clearThreadStatus(threadId: string): void {
  activeItemIDsByThread.delete(threadId);
  activeTurnByThread.delete(threadId);
  completedTurnIDsByThread.delete(threadId);
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
  interruptedThreads.delete(threadId);
  clearSendQueueForThread(threadId);
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
 * provider. Paired with projectSendResolved / turn lifecycle events to
 * clear the flag.
 */
export function projectSendStarted(threadId: string): void {
  if (!threadId) return;
  // A fresh send clears any prior error so the pill stops reading
  // "Failed" — the user has moved past that failure.
  errorThreads.delete(threadId);
  interruptedThreads.delete(threadId);
  pendingSendThreads.add(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Clear the pending-send flag. Called by composerSend when SendMessage
 * rejects (with `error: true` so the pill flips to Failed rather than
 * idle) or when turn lifecycle events arrive and take over ownership
 * of the running signal.
 */
export function projectSendResolved(threadId: string, opts: { error?: boolean } = {}): void {
  if (!threadId) return;
  pendingSendThreads.delete(threadId);
  interruptedThreads.delete(threadId);
  if (opts.error) {
    errorThreads.add(threadId);
  }
  recalculateThreadStatus(threadId);
}

/**
 * Read whether a `SendMessage` RPC is currently in flight for this
 * thread. Set by `projectSendStarted`; cleared by `projectSendResolved`
 * (failure path) or `projectTurnStarted` (success path: backend has
 * confirmed the round). Used by the working-indicator bridge predicate
 * to keep the spinner visible across the brief drain RPC roundtrip,
 * when activeTurn is null (round just completed) and the queue may
 * have just gone to 0 (the popped item is what's in flight).
 */
export function hasPendingSend(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return pendingSendThreads.has(threadId);
}

/**
 * Explicit clear for the pending-send flag without changing other
 * status flags. The drain failure path uses this — `projectTurnStarted`
 * clears the flag on a successful drain (the backend confirmed the new
 * round), but on a thrown SendMessage the flag would otherwise leak
 * forever. Distinct from `projectSendResolved({error:true})` because
 * we don't want to flip the thread to a Failed status pill: the queue
 * preview is still showing the user's restored item, the error banner
 * carries the failure context, and another drain attempt should be
 * possible from a clean state.
 */
export function clearPendingSend(threadId: string): void {
  if (!threadId) return;
  if (!pendingSendThreads.delete(threadId)) return;
  recalculateThreadStatus(threadId);
}

/**
 * Mark a turn as in-flight. Fired from applyTurnStarted, the only
 * place that can record a live turn (invariant 22). A matching
 * projectTurnCompleted clears it. The full {turnId, turnIndex,
 * startedAt} triple lives in the global registry so off-pane
 * surfaces (chat working indicator after thread switch, sidebar
 * pill) all read the same record without each rebuilding it.
 */
export function projectTurnStarted(
  threadId: string,
  turnId: string,
  turnIndex: number,
  startedAt: number,
): void {
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
  if (hasCompletedTurnID(threadId, turnId)) {
    recalculateThreadStatus(threadId);
    return;
  }
  errorThreads.delete(threadId);
  interruptedThreads.delete(threadId);
  // Idempotent on (threadId, turnId): under the per-wire-round
  // emission cadence each round generates a fresh `turnId`, so a
  // duplicate event for the SAME round (Codex retry-induced
  // turn/started replay, defensive double-emit) still no-ops here
  // while a new round REPLACES the prior entry. Preserving the
  // original startedAt for an exact-match round keeps the working-
  // indicator's elapsed-seconds counter monotonically increasing
  // instead of rewinding on each duplicate.
  const existing = activeTurnByThread.get(threadId);
  if (existing && existing.turnId === turnId) return;
  activeTurnByThread.set(threadId, { turnId, turnIndex, startedAt });
  recalculateThreadStatus(threadId);
}

/**
 * Clear an in-flight turn. Provider errors flip the thread to Failed;
 * clean interrupts flip it to Interrupted so user-cancelled or killed
 * work does not read as a provider failure. A complete arriving for
 * a turnId different from the current entry is a no-op against the
 * registry (defensive — turnIndex+startedAt should match), but the
 * status flags below still apply.
 */
export function projectTurnCompleted(
  threadId: string,
  turnId: string,
  opts: { aborted?: boolean; errorMessage?: string } = {},
): void {
  if (!threadId || !turnId) return;
  markCompletedTurnID(threadId, turnId);
  const current = activeTurnByThread.get(threadId);
  if (current && current.turnId === turnId) {
    activeTurnByThread.delete(threadId);
  }
  if (opts.errorMessage && opts.errorMessage.length > 0) {
    errorThreads.add(threadId);
    interruptedThreads.delete(threadId);
  } else if (opts.aborted) {
    interruptedThreads.add(threadId);
    errorThreads.delete(threadId);
  }
  recalculateThreadStatus(threadId);
}

function markCompletedTurnID(threadId: string, turnId: string): void {
  let completed = completedTurnIDsByThread.get(threadId);
  if (!completed) {
    completed = new Set<string>();
    completedTurnIDsByThread.set(threadId, completed);
  }
  completed.add(turnId);
  if (completed.size <= 128) return;
  const oldest = completed.values().next().value;
  if (typeof oldest === 'string') completed.delete(oldest);
}

function hasCompletedTurnID(threadId: string, turnId: string): boolean {
  return completedTurnIDsByThread.get(threadId)?.has(turnId) === true;
}

/**
 * Read the live in-flight turn for a thread. Returns null when no
 * turn is active or the thread id is empty/null/undefined. The single
 * source of truth for "is the working indicator on?" — both
 * ChatWorkingIndicator (chat view) and the sidebar pill consume this.
 * Survives thread switches because nothing in pane lifecycle clears
 * it. Accepts null/undefined so callers can pass `pane.threadId`
 * (which is `string | null` while the pane is empty) without a
 * fallback dance at every call site.
 */
export function getActiveTurn(threadId: string | null | undefined): ActiveTurn | null {
  if (!threadId) return null;
  return activeTurnByThread.get(threadId) ?? null;
}

/**
 * Feed a live item upsert into the sidebar-status projection. This is the
 * canonical chat-state path: running rows keep the dot hot, terminal rows
 * clear their own ids, and inline error rows leave the thread in an error
 * state until the user views the thread or a new active item / turn
 * supersedes it.
 */
export function projectThreadItem(item: Item): void {
  if (!item?.threadId || !item.id) return;

  if (item.kind === 'error') {
    errorThreads.add(item.threadId);
  } else if (item.kind === 'user_text' || isActiveTimelineItem(item)) {
    errorThreads.delete(item.threadId);
    interruptedThreads.delete(item.threadId);
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
  if (item.payloadKind === 'proposed_plan' && item.role === 'assistant' && item.status === 'completed') {
    if (isImplementedProposedPlan(item.meta)) {
      planReadyThreads.delete(item.threadId);
    } else {
      planReadyThreads.add(item.threadId);
    }
  }

  recalculateThreadStatus(item.threadId);
}

function isImplementedProposedPlan(meta: string | undefined): boolean {
  if (!meta) return false;
  try {
    const parsed = JSON.parse(meta) as { planImplementedAt?: number };
    return Number(parsed.planImplementedAt ?? 0) > 0;
  } catch {
    return false;
  }
}

/**
 * Feed an approval request into the projection. MCP elicitation,
 * permission, command, and file-change requests are blocking approvals;
 * structured user input flows through provider:user_input instead.
 * Unknown or missing kind defaults to pending-approval because that's
 * the safer read for a blocking provider request.
 */
export function projectApprovalRequest(
  threadId: string,
  requestId: string,
  kind?: ApprovalKind,
): void {
  if (!threadId || !requestId) return;
  approvalThreadByID.set(requestId, threadId);
  void kind;
  trackedIDsFor(approvalIDsByThread, threadId).add(requestId);
  errorThreads.delete(threadId);
  interruptedThreads.delete(threadId);
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

export function projectUserInputRequest(threadId: string, requestId: string): void {
  if (!threadId || !requestId) return;
  approvalThreadByID.set(requestId, threadId);
  trackedIDsFor(awaitingInputIDsByThread, threadId).add(requestId);
  errorThreads.delete(threadId);
  interruptedThreads.delete(threadId);
  recalculateThreadStatus(threadId);
}

export function projectUserInputResolution(
  threadId: string | null | undefined,
  requestId: string,
): void {
  if (!requestId) return;
  const ownerThreadId = threadId ?? approvalThreadByID.get(requestId);
  if (!ownerThreadId) return;
  approvalThreadByID.delete(requestId);
  removeTrackedID(awaitingInputIDsByThread, ownerThreadId, requestId);
  recalculateThreadStatus(ownerThreadId);
}

/**
 * Clear statuses whose only purpose is to get the user's attention for
 * unseen activity. Completed is handled by lastReadAt on the Thread row;
 * this store owns transient live-status state, so it clears stale errors
 * once the user is viewing the thread. Blocking states stay until their
 * underlying request is actually resolved.
 */
export function projectThreadViewed(threadId: string): void {
  if (!threadId) return;
  if (!errorThreads.has(threadId)) return;
  errorThreads.delete(threadId);
  recalculateThreadStatus(threadId);
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
 * code should use clearThreadStatus per id. Also wipes the per-thread
 * send queue so tests start from a clean slate.
 */
export function resetForTest(): void {
  activeItemIDsByThread.clear();
  activeTurnByThread.clear();
  completedTurnIDsByThread.clear();
  pendingSendThreads.clear();
  planReadyThreads.clear();
  approvalIDsByThread.clear();
  awaitingInputIDsByThread.clear();
  approvalThreadByID.clear();
  errorThreads.clear();
  interruptedThreads.clear();
  if (statuses.size === 0) return;
  statuses = new Map();
}
