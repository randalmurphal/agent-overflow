import { SvelteSet } from 'svelte/reactivity';
import type { ApprovalKind } from '../types/events';
import type { Item, Thread } from '../types/models';
import {
  clearForThread as clearSendQueueForThread,
  hasQueueItems,
  resetForTest as resetSendQueueForTest,
} from './sendQueue.svelte';

/**
 * ActiveTurn is the live in-flight turn for a thread. Populated from
 * backend live signals only: `provider:turn_started` pushes during a
 * connected session, and `GetThreadLiveState` hydration after refresh.
 * Cleared on `provider:turn_completed` or an idle backend snapshot.
 * Never hydrated from durable item history — invariant 22 (turn
 * activity is live backend state, never derived from persisted items).
 *
 * The shape lives here because this store owns the per-thread active-
 * turn map used by the composite `isThreadWorking` predicate and the
 * activity rail elapsed timer. Survives thread switches because
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

export function sameActiveTurn(left: ActiveTurn | null, right: ActiveTurn | null): boolean {
  if (left === null || right === null) return left === right;
  return left.turnId === right.turnId
    && left.turnIndex === right.turnIndex
    && left.startedAt === right.startedAt;
}

// Global per-thread live-status projection for the sidebar. Chat state is
// authoritative in the unified item stream; this store keeps the minimal
// derived signal the thread list needs for off-pane rows (running, pending
// approval, error). Durable boot status such as interrupted turns and
// actionable proposed plans is derived from Thread rows instead.
//
// Running is derived from the same live sources the composer activity
// rail uses, OR'd together:
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
//   3. Send queue bridge: queued or flushed messages that have not yet
//      produced the provider-visible user-message echo. This keeps the
//      indicator hot between queue registration, provider write, and
//      round-start confirmation.
//
// Durable timeline item rows are deliberately not part of this
// calculation. A stale foreground tool_call with status=running is
// history/display state, not proof that the provider is working now.
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
let liveStateHydratingThreads: Set<string> = $state(new Set());
const approvalIDsByThread = new Map<string, Set<string>>();
const awaitingInputIDsByThread = new Map<string, Set<string>>();
const approvalThreadByID = new Map<string, string>();
// activeTurnBoxByThread is the global per-thread ActiveTurn registry.
// `isThreadWorking` combines it with pending-send and send-queue bridge
// state to answer "is this thread working?".
//
// Each thread gets its own reactive box (one `$state.raw` signal)
// rather than one SvelteMap: reading a MISSING key from a SvelteMap
// subscribes the reader to the whole-map version, and idle threads ARE
// the missing-key case here (entries clear on round completion). With
// a shared map, every turn start/complete on ANY thread invalidated
// every idle pane's working indicator and every sidebar row — at the
// per-wire-round cadence that is multiple cross-pane invalidations per
// second while any pane streams. With per-thread boxes, a reader only
// re-evaluates when ITS thread's turn changes (plus one re-run per
// box CREATION, via activeTurnBoxCreationVersion below). Boxes are
// created on first write and live for the session (bounded by distinct
// thread ids observed; `clearThreadStatus` drops the archived ones).
//
// Under the per-wire-round emission cadence (see
// internal/triage/AGENTS.md "Wire-round vs logical-turn"), each entry
// represents the CURRENT wire round — not the user-typed logical
// turn. The two are decoupled on the backend; the frontend only
// observes one signal stream and treats each round as its own active
// turn. This is what flips the working indicator off between rounds
// in Claude's multi-result-per-turn cascade so the UI reflects "model
// is engaged right now" rather than "user-typed prompt is in flight."
interface ActiveTurnBox {
  get current(): ActiveTurn | null;
  set current(turn: ActiveTurn | null);
}

function newActiveTurnBox(): ActiveTurnBox {
  // `$state.raw`: the ActiveTurn record is replaced wholesale, never
  // mutated field-by-field, so deep proxying would be pure overhead.
  let current: ActiveTurn | null = $state.raw(null);
  return {
    get current() {
      return current;
    },
    set current(turn) {
      current = turn;
    },
  };
}

const activeTurnBoxByThread = new Map<string, ActiveTurnBox>();

// Bumped when a box is CREATED — not when a turn starts or ends.
// Readers of a thread with no box yet track this instead of the box
// (see readActiveTurn); after the thread's first-ever turn creates the
// box, those readers re-run once and track the box directly.
let activeTurnBoxCreationVersion = $state(0);

// Writer-side accessor. Only writers create boxes: Svelte does not
// register state created inside the currently-running reaction as a
// dependency of that reaction, so a reader that lazily created its own
// box could never track it. Writers (projectTurnStarted etc.) run from
// event handlers, outside any reaction, where creation is safe.
function activeTurnBoxForWrite(threadId: string): ActiveTurnBox {
  let box = activeTurnBoxByThread.get(threadId);
  if (!box) {
    box = newActiveTurnBox();
    activeTurnBoxByThread.set(threadId, box);
    activeTurnBoxCreationVersion += 1;
  }
  return box;
}

function readActiveTurn(threadId: string): ActiveTurn | null {
  const box = activeTurnBoxByThread.get(threadId);
  if (!box) {
    // Track creations so this thread's first-ever turn re-runs the
    // reader; on that re-run the box exists and is tracked directly.
    void activeTurnBoxCreationVersion;
    return null;
  }
  return box.current;
}
const completedTurnIDsByThread = new Map<string, Set<string>>();
const pendingSendThreads = new SvelteSet<string>();
const planReadyThreads = new Set<string>();
const errorThreads = new Set<string>();
const interruptedThreads = new Set<string>();
const liveStateHydrationTokenByThread = new Map<string, number>();

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
  if (isThreadWorking(threadId)) {
    setThreadStatus(threadId, 'idle');
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
  const stored = statuses.get(threadId) ?? 'idle';
  if ((approvalIDsByThread.get(threadId)?.size ?? 0) > 0 || stored === 'pending-approval') {
    return 'pending-approval';
  }
  if ((awaitingInputIDsByThread.get(threadId)?.size ?? 0) > 0 || stored === 'awaiting-input') {
    return 'awaiting-input';
  }
  if (isThreadWorking(threadId)) return 'running';
  if (errorThreads.has(threadId) || stored === 'error') return 'error';
  if (planReadyThreads.has(threadId) || stored === 'plan-ready') return 'plan-ready';
  if (interruptedThreads.has(threadId) || stored === 'interrupted') return 'interrupted';
  return 'idle';
}

export function getEffectiveThreadStatus(
  thread: Pick<Thread, 'id' | 'hasIncompleteTurn' | 'hasActionableProposedPlan'>,
): ThreadLiveStatus {
  const liveStatus = getThreadStatus(thread.id);
  if (liveStatus !== 'idle') return liveStatus;
  if (thread.hasIncompleteTurn && !isThreadLiveStateHydrating(thread.id)) return 'interrupted';
  if (thread.hasActionableProposedPlan) return 'plan-ready';
  return 'idle';
}

/**
 * Mark that the frontend has asked the backend for this thread's live
 * state and is waiting on the authoritative answer. During this gap the
 * sidebar must not promote durable `hasIncompleteTurn` to Interrupted:
 * the incomplete row may simply be the current active provider turn.
 *
 * Returns a token so overlapping hydrations for the same thread cannot
 * clear each other's guard out of order.
 */
export function beginThreadLiveStateHydration(threadId: string): number {
  if (!threadId) return 0;
  const token = (liveStateHydrationTokenByThread.get(threadId) ?? 0) + 1;
  liveStateHydrationTokenByThread.set(threadId, token);
  if (!liveStateHydratingThreads.has(threadId)) {
    liveStateHydratingThreads = new Set(liveStateHydratingThreads).add(threadId);
  }
  return token;
}

export function finishThreadLiveStateHydration(threadId: string, token: number): void {
  if (!threadId || token === 0) return;
  if (liveStateHydrationTokenByThread.get(threadId) !== token) return;
  liveStateHydrationTokenByThread.delete(threadId);
  if (!liveStateHydratingThreads.has(threadId)) return;
  const next = new Set(liveStateHydratingThreads);
  next.delete(threadId);
  liveStateHydratingThreads = next;
}

export function isThreadLiveStateHydrating(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return liveStateHydratingThreads.has(threadId);
}

export function isThreadLiveStateHydrationCurrent(threadId: string, token: number): boolean {
  if (!threadId || token === 0) return false;
  return liveStateHydrationTokenByThread.get(threadId) === token;
}

/**
 * Set or replace a thread's live status. Writing 'idle' is equivalent
 * to clearing — we drop the entry so the map doesn't grow with stale
 * idle-only rows across long sessions. Runtime Working is derived by
 * `isThreadWorking`, so production projections should not persist
 * `running` in this map.
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
  const turnBox = activeTurnBoxByThread.get(threadId);
  if (turnBox) {
    // Null the signal BEFORE dropping the box: a still-mounted reader
    // re-runs off the null write, re-reads through `readActiveTurn`
    // (which tracks `activeTurnBoxCreationVersion` while box-less and
    // re-attaches when its thread's next turn creates a fresh box); the
    // orphan is GC'd with the reader.
    turnBox.current = null;
    activeTurnBoxByThread.delete(threadId);
  }
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
  liveStateHydrationTokenByThread.delete(threadId);
  if (liveStateHydratingThreads.has(threadId)) {
    const next = new Set(liveStateHydratingThreads);
    next.delete(threadId);
    liveStateHydratingThreads = next;
  }
  clearSendQueueForThread(threadId);
  if (!statuses.has(threadId)) return;
  const next = new Map(statuses);
  next.delete(threadId);
  statuses = next;
}

/**
 * Optimistically flip a thread to running when AO has started a
 * provider send/write that should open a fresh round, but the backend
 * has not emitted `provider:turn_started` yet. This covers normal
 * composer sends before the provider emits its round-start signal.
 * Paired with projectSendResolved / turn lifecycle events to clear the
 * flag.
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
 * idle), by early provider process failure before `turn_started`, or when
 * turn lifecycle events arrive and take over ownership of the running signal.
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
 * Read whether a fresh provider send/write is currently in flight for this
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

export function isThreadWorking(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  return readActiveTurn(threadId) !== null
    || pendingSendThreads.has(threadId)
    || hasQueueItems(threadId);
}

export function isSendInFlight(threadId: string | null | undefined, paneSendInFlight: boolean): boolean {
  return paneSendInFlight || hasPendingSend(threadId);
}

/**
 * Explicit clear for the pending-send flag without changing other
 * status flags. Queue-drain failure uses this because on a thrown
 * provider write the flag would otherwise leak forever. Distinct from
 * `projectSendResolved({error:true})` because we don't want to flip the
 * thread to a Failed status pill: the queue preview is still showing
 * the user's restored item, the error banner carries the failure
 * context, and another drain attempt should be possible from a clean
 * state.
 */
export function clearPendingSend(threadId: string): void {
  if (!threadId) return;
  if (!pendingSendThreads.delete(threadId)) return;
  recalculateThreadStatus(threadId);
}

export function replaceInteractiveRequestsForThread(
  threadId: string,
  snapshot: {
    approvals: readonly { requestId: string }[];
    userInputs: readonly { requestId: string }[];
  },
): void {
  if (!threadId) return;

  const previousApprovalIDs = approvalIDsByThread.get(threadId);
  if (previousApprovalIDs) {
    for (const requestId of previousApprovalIDs) {
      approvalThreadByID.delete(requestId);
    }
  }
  const previousUserInputIDs = awaitingInputIDsByThread.get(threadId);
  if (previousUserInputIDs) {
    for (const requestId of previousUserInputIDs) {
      approvalThreadByID.delete(requestId);
    }
  }

  const nextApprovalIDs = new Set<string>();
  for (const request of snapshot.approvals) {
    if (!request.requestId) continue;
    nextApprovalIDs.add(request.requestId);
    approvalThreadByID.set(request.requestId, threadId);
  }
  const nextUserInputIDs = new Set<string>();
  for (const request of snapshot.userInputs) {
    if (!request.requestId) continue;
    nextUserInputIDs.add(request.requestId);
    approvalThreadByID.set(request.requestId, threadId);
  }

  if (nextApprovalIDs.size > 0) {
    approvalIDsByThread.set(threadId, nextApprovalIDs);
  } else {
    approvalIDsByThread.delete(threadId);
  }
  if (nextUserInputIDs.size > 0) {
    awaitingInputIDsByThread.set(threadId, nextUserInputIDs);
  } else {
    awaitingInputIDsByThread.delete(threadId);
  }
  if (nextApprovalIDs.size > 0 || nextUserInputIDs.size > 0) {
    errorThreads.delete(threadId);
    interruptedThreads.delete(threadId);
  }
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
  const turnBox = activeTurnBoxForWrite(threadId);
  const existing = turnBox.current;
  if (existing && existing.turnId === turnId) return;
  turnBox.current = { turnId, turnIndex, startedAt };
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
  opts: { aborted?: boolean; errorMessage?: string; revertedUserMessage?: boolean } = {},
): void {
  if (!threadId || !turnId) return;
  markCompletedTurnID(threadId, turnId);
  const turnBox = activeTurnBoxByThread.get(threadId);
  if (turnBox && turnBox.current?.turnId === turnId) {
    turnBox.current = null;
  }
  if (opts.errorMessage && opts.errorMessage.length > 0) {
    errorThreads.add(threadId);
    interruptedThreads.delete(threadId);
  } else if (opts.aborted) {
    // Suppress the Interrupted pill when the turn ended via revert-on-
    // interrupt. The user message was undone and put back into the
    // composer; nothing happened, so the sidebar must not paint a
    // "user interrupted" status on this thread.
    if (opts.revertedUserMessage) {
      interruptedThreads.delete(threadId);
      errorThreads.delete(threadId);
    } else {
      interruptedThreads.add(threadId);
      errorThreads.delete(threadId);
    }
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
 * turn is active or the thread id is empty/null/undefined. The
 * activity rail also uses this as the elapsed-time anchor when
 * `isThreadWorking` is true. Survives thread switches because nothing
 * in pane lifecycle clears it. Accepts null/undefined so callers can
 * pass `pane.threadId` (which is `string | null` while the pane is
 * empty) without a fallback dance at every call site.
 */
export function getActiveTurn(threadId: string | null | undefined): ActiveTurn | null {
  if (!threadId) return null;
  return readActiveTurn(threadId);
}

/**
 * Feed a live item upsert into the sidebar-status projection. Item rows can
 * surface attention states such as errors and actionable plans, but they do
 * not decide whether the thread is working. Liveness belongs to backend turn
 * signals plus the send/queue bridge; persisted timeline rows can be stale.
 */
export function projectThreadItem(item: Item): void {
  if (!item?.threadId || !item.id) return;

  if (item.kind === 'error') {
    pendingSendThreads.delete(item.threadId);
    errorThreads.add(item.threadId);
  } else if (item.kind === 'user_text') {
    errorThreads.delete(item.threadId);
    interruptedThreads.delete(item.threadId);
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
 * interrupted durable state is handled the same way by MarkThreadReadNow.
 * This store owns transient live-status state, so it clears stale errors
 * and clean interrupts once the user is viewing the thread. Blocking states
 * stay until their underlying request is actually resolved.
 */
export function projectThreadViewed(threadId: string): void {
  if (!threadId) return;
  const hadAttentionStatus = errorThreads.has(threadId) || interruptedThreads.has(threadId);
  if (!hadAttentionStatus) return;
  errorThreads.delete(threadId);
  interruptedThreads.delete(threadId);
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
 * Wipe the entire map. Only intended for test isolation — production
 * code should use clearThreadStatus per id. Also wipes the per-thread
 * send queue because `isThreadWorking` reads that bridge state.
 */
export function resetForTest(): void {
  for (const box of activeTurnBoxByThread.values()) box.current = null;
  activeTurnBoxByThread.clear();
  activeTurnBoxCreationVersion = 0;
  completedTurnIDsByThread.clear();
  pendingSendThreads.clear();
  planReadyThreads.clear();
  approvalIDsByThread.clear();
  awaitingInputIDsByThread.clear();
  approvalThreadByID.clear();
  errorThreads.clear();
  interruptedThreads.clear();
  liveStateHydrationTokenByThread.clear();
  resetSendQueueForTest();
  liveStateHydratingThreads = new Set();
  if (statuses.size === 0) return;
  statuses = new Map();
}
