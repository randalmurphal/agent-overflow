// The run map's patch algebra: view + one `workflow:*` event → the next view,
// or the verdict that this event cannot be placed and the key must refetch
// (RUN-MAP §4.4).
//
// Pure and Svelte-free, next to the store the way `workflowData.ts` sits next
// to `workflowRuns.svelte.ts`. The store owns the entry, the member index and
// the debounce; this owns the ONE question that decides whether the map stays
// honest: does the payload determine the new row exactly?
//
// The standing rule is that it usually does not. Every event carries strictly
// less than the view — a parked attempt's engine cause, a unit retry's attempt
// number, a run's endedAt and the tree's rolled-up spend are all absent from
// the wire — so anything short of an exact placement returns `invalidate`
// rather than a plausible guess. A patch is a latency optimization; a wrong
// patch is a map that lies until something else happens to refetch it.

import type {
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRun,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
  WorkflowSoftStopEvent,
} from '../types/workflow';

// Phase-attempt statuses the engine persists (`teardownRequest.phaseStatus`).
// `completed` is the only terminal one a phase event describes COMPLETELY: the
// other three are written together with an engine-diagnosed cause (and, for a
// take-over, an intervention record) that the event does not carry.
const PHASE_STATUS_RUNNING = 'running';
const PHASE_STATUS_COMPLETED = 'completed';

// Unit statuses (`workflowRunMapIndex.ts`'s UNIT_SIGNALS vocabulary). A unit row
// carries no cause, so every one of these patches in full — which matters,
// because a 32-branch fan-out is where the event rate actually lives.
const UNIT_STATUS_RUNNING = 'running';
const UNIT_STATUS_PENDING = 'pending';
const UNIT_STATUSES_TERMINAL = new Set(['done', 'failed', 'dropped', 'taken-over']);

export type PatchResult =
  | { kind: 'patched'; view: WorkflowRunMapView }
  /** The event restated something the view already said. */
  | { kind: 'unchanged' }
  /** The event cannot be placed exactly — refetch instead of guessing. */
  | { kind: 'invalidate' };

// Shared singletons rather than fresh objects: a verdict carries no data, and
// the store only ever reads `kind`. Not exported — a test asserts the verdict
// a public patch function returns, never one it built itself.
const UNCHANGED: PatchResult = { kind: 'unchanged' };
const INVALIDATE: PatchResult = { kind: 'invalidate' };

/** Run states after which nothing further is emitted for that run. */
export function isTerminalRunState(state: string): boolean {
  return state === 'done' || state === 'failed' || state === 'cancelled';
}

/** Whether a phase event carries the engine's own event time (§4.3.1). */
function hasEventTime(event: WorkflowPhaseStateEvent): boolean {
  return typeof event.occurredAt === 'number'
    && Number.isFinite(event.occurredAt)
    && event.occurredAt > 0;
}

// Only the touched run is rebuilt; every other run, and every untouched row of
// the touched one, stays the same object. Consumers `$derived` off this view,
// and a wholesale clone would invalidate the projection of a tree where one
// unit moved.
function replaceRun(
  view: WorkflowRunMapView,
  index: number,
  run: WorkflowRunMapRun,
): WorkflowRunMapView {
  const runs = view.runs.slice();
  runs[index] = run;
  return { ...view, runs };
}

/**
 * Patch one unit row. Identity is (phaseId, attempt, unitId): a unit RETRY
 * reuses the row and bumps `unitAttempt`, which the event does not carry — so
 * a `running` arriving at a row that has already ended is the reopen this
 * cannot model.
 *
 * Returns the run unchanged when the event restates the row, and null when the
 * key must refetch.
 */
function patchUnitRow(
  run: WorkflowRunMapRun,
  event: WorkflowPhaseStateEvent,
  occurredAt: number,
): WorkflowRunMapRun | null {
  const index = run.units.findIndex((unit) => (
    unit.phaseId === event.phaseId
    && unit.attempt === event.attempt
    && unit.unitId === event.unitId
  ));
  // Every unit of an attempt is persisted `pending` when the attempt expands
  // (engine/units.go), so an unknown unit means this view predates that
  // expansion — the rest of the branch column is missing too, and inserting
  // one row would draw a fan-out that never had the others.
  if (index < 0) return null;
  const unit = run.units[index]!;
  const next: WorkflowRunMapUnit = { ...unit, status: event.status };
  if (event.status === UNIT_STATUS_RUNNING) {
    if (unit.endedAt) return null;
    if (!unit.startedAt) next.startedAt = occurredAt;
  } else if (UNIT_STATUSES_TERMINAL.has(event.status)) {
    next.endedAt = occurredAt;
  } else if (event.status === UNIT_STATUS_PENDING) {
    // `pending` at a unit that is ALREADY pending is the expansion restating
    // itself. At a settled one it is a REOPEN (`engine.reopenUnit` →
    // `store.RetryWorkItemUnit`), which bumps `unit_attempt` and zeroes both
    // timestamps — none of which is on the wire. Patching the status alone
    // would leave the row queued again while still showing the failed try's
    // duration and its old ×N.
    if (unit.status !== UNIT_STATUS_PENDING) return null;
  } else {
    // A status this build does not know: it may well be terminal, and
    // guessing its timing is how a finished branch keeps ticking.
    return null;
  }
  if (next.status === unit.status && next.startedAt === unit.startedAt && next.endedAt === unit.endedAt) {
    return run;
  }
  const units = run.units.slice();
  units[index] = next;
  return { ...run, units };
}

/**
 * Patch (or open) one phase attempt. A `running` event for an attempt the view
 * has never seen is exactly how an attempt comes into existence, so it INSERTS
 * — appended, which is where the refetch would put it too (the rows come back
 * ordered by `started_at`, and this row's is the newest).
 */
function patchAttemptRow(
  run: WorkflowRunMapRun,
  event: WorkflowPhaseStateEvent,
  occurredAt: number,
): WorkflowRunMapRun | null {
  const index = run.phases.findIndex(
    (phase) => phase.phaseId === event.phaseId && phase.attempt === event.attempt,
  );
  if (index < 0) {
    if (event.status !== PHASE_STATUS_RUNNING) return null;
    const opened: WorkflowRunMapPhaseAttempt = {
      phaseId: event.phaseId,
      attempt: event.attempt,
      status: PHASE_STATUS_RUNNING,
      startedAt: occurredAt,
    };
    return { ...run, phases: [...run.phases, opened] };
  }
  const phase = run.phases[index]!;
  const next: WorkflowRunMapPhaseAttempt = { ...phase, status: event.status };
  if (event.status === PHASE_STATUS_RUNNING) {
    // An ended attempt that runs again is a resume re-entering the row; its
    // cause and intervention would have to be cleared, and only the record
    // knows whether they were.
    if (phase.endedAt) return null;
    if (!phase.startedAt) next.startedAt = occurredAt;
  } else if (event.status === PHASE_STATUS_COMPLETED) {
    next.endedAt = occurredAt;
  } else {
    // Everything else: `failed` / `parked` / `cancelled` are persisted WITH the
    // engine's diagnosis (park cause, take-over intervention), and the event
    // carries the status alone — patching one would paint a park with no reason
    // for it, which is the one moment the map is being read for exactly that.
    // A status this build does not know is the same answer for a stronger
    // reason: nothing here can say what it means.
    return null;
  }
  if (next.status === phase.status && next.startedAt === phase.startedAt && next.endedAt === phase.endedAt) {
    return run;
  }
  const phases = run.phases.slice();
  phases[index] = next;
  return { ...run, phases };
}

/** `workflow:phase-state` — one attempt's, or one unit's, status moved. */
export function patchPhaseState(
  view: WorkflowRunMapView,
  event: WorkflowPhaseStateEvent,
): PatchResult {
  if (!hasEventTime(event)) return INVALIDATE;
  const index = view.runs.findIndex((run) => run.itemId === event.itemId);
  if (index < 0) return INVALIDATE;
  const run = view.runs[index]!;
  const patched = event.unitId
    ? patchUnitRow(run, event, event.occurredAt)
    : patchAttemptRow(run, event, event.occurredAt);
  if (patched === null) return INVALIDATE;
  if (patched === run) return UNCHANGED;
  return { kind: 'patched', view: replaceRun(view, index, patched) };
}

/**
 * `workflow:item-state` — a run's state moved. State and reason are all the
 * payload determines; `endedAt`, `autoResumeAt` and the tree's spend are the
 * store's to reconcile.
 */
export function patchItemState(
  view: WorkflowRunMapView,
  event: WorkflowItemStateEvent,
): PatchResult {
  const index = view.runs.findIndex((run) => run.itemId === event.itemId);
  if (index < 0) return INVALIDATE;
  const run = view.runs[index]!;
  const reason = event.reason ?? '';
  if (run.state === event.to && (run.reason ?? '') === reason) return UNCHANGED;
  const next: WorkflowRunMapRun = { ...run, state: event.to };
  // `reason` is omitted rather than blanked, so a patched run and a refetched
  // one are the same object shape.
  if (reason) next.reason = reason;
  else delete next.reason;
  return { kind: 'patched', view: replaceRun(view, index, next) };
}

/**
 * `workflow:soft-stop` — the run tree's standing stop request was armed or
 * withdrawn. A run this view does not contain is not ambiguity: soft stop is
 * armed on trees the overlay may have nothing open for.
 */
export function patchSoftStop(
  view: WorkflowRunMapView,
  event: WorkflowSoftStopEvent,
): PatchResult {
  const index = view.runs.findIndex((run) => run.itemId === event.itemId);
  if (index < 0) return UNCHANGED;
  const run = view.runs[index]!;
  if (run.softStop === event.armed) return UNCHANGED;
  return { kind: 'patched', view: replaceRun(view, index, { ...run, softStop: event.armed }) };
}
