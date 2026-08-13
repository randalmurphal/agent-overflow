// The per-state resolution model (UI-SPEC §4.3 + §8). One table maps a run's
// (state, reason) to the evidence block the detail opens with and to the fixed
// footer's action row, primary first, each carrying the key §8 binds it to.
//
// Pure: no RPCs, no Svelte. WorkflowRunDetail renders the rows and
// workflowActions.ts executes the ids.

import { compositeKey } from './compositeKey';
import { workflowClosedDuration } from './format';
import type { WorkItem, WorkItemPhase, WorkItemUnit, WorkflowItemDetail } from '../types/workflow';

export type WorkflowResolutionKind =
  | 'gate'
  | 'question'
  | 'failed'
  | 'blocked'
  | 'paused'
  | 'checkpoint'
  | 'unit-failed'
  | 'taken-over'
  | 'done'
  | 'running'
  | 'cancelled';

export type WorkflowActionId =
  | 'approve'
  | 'request-changes'
  | 'rerun'
  | 'resume'
  | 'retry-unit'
  | 'retry-failed-units'
  | 'drop-unit'
  | 'take-over-unit'
  | 'complete-takeover'
  | 'merge'
  | 'create-pr'
  | 'open-phase-thread'
  | 'pause'
  | 'soft-stop'
  | 'clear-soft-stop'
  | 'cancel'
  | 'discard'
  | 'back';

export type WorkflowActionKey = 'a' | 'r' | 't' | 'u';

export interface WorkflowActionButton {
  id: WorkflowActionId;
  label: string;
  /** §8 binding. Absent means mouse-only (Drop unit, Create PR, …). */
  key?: WorkflowActionKey;
  variant: 'primary' | 'secondary' | 'ghost' | 'danger-outline';
  /** Arms a confirm before it runs; Esc disarms (§2.2). */
  arms?: boolean;
}

// The two reasons that share the paused ROW. `checkpoint` takes the same engine
// edge back but gets a row of its own, so it is deliberately not here.
// Provider retry exhaustion continues the parked session, exactly as for
// `paused`/`interrupted`, so it takes the same resolution kind. The legacy
// `retries-exhausted` spelling retains that shipped behavior. A spent workflow
// loop has no dead provider turn to continue and remains blocked.
const RESUMABLE_REASONS = new Set([
  'paused',
  'interrupted',
  'provider-retries-exhausted',
  'retries-exhausted',
]);
const DONE_REASONS = new Set(['disposition']);

/**
 * Which §4.3 row a run is on. `needs-human(disposition)` shares the done row,
 * because a refused merge parks exactly where the disposition decision lives.
 *
 * `failed` and `blocked` show the same evidence — a run that could not finish —
 * but they are two rows, not one, because the engine offers each exactly one
 * edge back and they are different edges. `RerunFailed` is the only
 * `failed → running` transition and refuses anything else; `Resume` is the only
 * way out of `needs-human` and refuses a `failed` run. A single row would have
 * to offer one of them to a state that rejects it.
 */
export function workflowResolutionKind(item: Pick<WorkItem, 'state' | 'reason'>): WorkflowResolutionKind {
  switch (item.state) {
    case 'running':
      return 'running';
    case 'cancelled':
      return 'cancelled';
    case 'done':
      return 'done';
    case 'failed':
      return 'failed';
    case 'needs-human':
      break;
    default:
      // A state the backend never emits. `failed` is the row that assumes the
      // least — its actions are a hand-off and a discard, neither of which
      // pretends the run can be continued from here.
      return 'failed';
  }
  const reason = item.reason ?? '';
  if (reason === 'gate') return 'gate';
  if (reason === 'question') return 'question';
  if (reason === 'unit-failed') return 'unit-failed';
  if (reason === 'taken-over') return 'taken-over';
  // Same edge back as `paused` — resume — but its own row: this run stopped
  // where it was asked to, and reading it as a fault is exactly the confusion
  // the separate reason exists to prevent.
  if (reason === 'checkpoint') return 'checkpoint';
  if (RESUMABLE_REASONS.has(reason)) return 'paused';
  if (DONE_REASONS.has(reason)) return 'done';
  return 'blocked';
}

/**
 * The unit a `needs-human(unit-failed)` park is about — the row §4.3 highlights
 * and the id §8's unit actions operate on.
 *
 * Deliberately narrow. The evidence block reads a label, a meta line and the
 * unit's own record; the action row reads its id and its thread. Handing either
 * of them a whole timeline node would give them structure they have no use for
 * and that would still have to be kept correct.
 */
export interface WorkflowFailedUnit {
  unit: WorkItemUnit;
  /** `port-b`, or `port-b (join)` when the join itself is what failed. */
  label: string;
  /** `×2 · 1m` — retry count and elapsed span, `·`-joined, empty parts dropped. */
  meta: string;
  /** The unit's thread, openable as a normal pane (R3). */
  threadId: string;
}

/** Phase attempts in the order they ran; id and attempt break a start-time tie. */
function attemptOrder(left: WorkItemPhase, right: WorkItemPhase): number {
  return (left.startedAt || 0) - (right.startedAt || 0)
    || left.phaseId.localeCompare(right.phaseId)
    || left.attempt - right.attempt;
}

/** Units in render order: the join is always last, the rest by declared index. */
function unitOrder(left: WorkItemUnit, right: WorkItemUnit): number {
  if ((left.kind === 'join') !== (right.kind === 'join')) return left.kind === 'join' ? 1 : -1;
  return left.unitIndex - right.unitIndex || left.unitId.localeCompare(right.unitId);
}

function failedUnitRow(unit: WorkItemUnit): WorkflowFailedUnit {
  return {
    unit,
    label: unit.kind === 'join' ? `${unit.unitId} (join)` : unit.unitId,
    meta: [
      unit.unitAttempt > 1 ? `×${unit.unitAttempt}` : '',
      // No clock: the row is only ever built for a `failed` or `taken-over`
      // unit, both of which the engine writes WITH an `ended_at`, so the span
      // is closed by the record. A `nowMs` here would exist purely to be
      // wrong — its one caller read it inside a `$derived` that never re-ran.
      workflowClosedDuration(unit.startedAt ?? 0, unit.endedAt ?? 0),
    ].filter((part) => part !== '').join(' · '),
    threadId: unit.threadId ?? '',
  };
}

export function failedWorkflowUnitInDetail(
  detail: WorkflowItemDetail,
): WorkflowFailedUnit | null {
  const attempts = [...(detail.phases ?? [])].sort(attemptOrder);
  const unitsByAttempt = new Map<string, WorkItemUnit[]>();
  for (const unit of detail.units ?? []) {
    const key = compositeKey(unit.phaseId, unit.attempt);
    const bucket = unitsByAttempt.get(key);
    if (bucket) bucket.push(unit);
    else unitsByAttempt.set(key, [unit]);
  }
  for (const bucket of unitsByAttempt.values()) bucket.sort(unitOrder);
  // Newest attempt backwards: a retried fan-out leaves failed units on the
  // superseded attempt too, and the park is about the attempt the run rests on.
  const newest = (status: string): WorkItemUnit | null => {
    for (let index = attempts.length - 1; index >= 0; index -= 1) {
      const found = unitsByAttempt
        .get(compositeKey(attempts[index].phaseId, attempts[index].attempt))
        ?.find((unit) => unit.status === status);
      if (found) return found;
    }
    return null;
  };
  // `taken-over` is the fallback and never the preference: a unit under human
  // control is the answer only when nothing failed outright.
  const unit = newest('failed') ?? newest('taken-over');
  return unit === null ? null : failedUnitRow(unit);
}

export interface WorkflowActionRowInput {
  kind: WorkflowResolutionKind;
  /** Names the phase an approved gate routes to; falls back to "next phase". */
  nextPhaseId?: string;
  /**
   * The run's stop request (D36), present only where one can exist: a ROOT run
   * whose workflow has a call phase to stop at. Omitted, the running row offers
   * no stop — a request that could never fire must not be presented as a stop
   * that will happen.
   */
  softStop?: { armed: boolean };
}

export function workflowActionRow(input: WorkflowActionRowInput): WorkflowActionButton[] {
  switch (input.kind) {
    case 'gate':
      return [
        { id: 'approve', label: `Approve → ${input.nextPhaseId || 'next phase'}`, key: 'a', variant: 'primary' },
        { id: 'request-changes', label: 'Request changes', key: 'r', variant: 'secondary' },
      ];
    case 'question':
      // No buttons: a question is answered by typing, and the footer's answer
      // input is the whole affordance (D32 removed the thread escape).
      return [];
    case 'failed':
      return [
        { id: 'rerun', label: 'Rerun with guidance', key: 'a', variant: 'primary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'blocked':
      // The paused row's shape, and for the paused row's reason: both stopped
      // short of a result and both continue once the human clears the way. The
      // reasons that land here are environmental (an unbound check, a secret
      // that would not resolve, a budget ceiling, a watchdog), so resuming
      // after fixing the thing outside the run is the common case.
      return [
        { id: 'resume', label: 'Resume', key: 'a', variant: 'primary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'paused':
      return [
        { id: 'resume', label: 'Resume', key: 'a', variant: 'primary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'checkpoint':
      // The paused row's edges, its own words: this run did what it was told,
      // so "Continue the run" reads as the resumption of a plan rather than the
      // repair of a fault.
      return [
        { id: 'resume', label: 'Continue the run', key: 'a', variant: 'primary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'unit-failed':
      // Two retries, because a fan-out fails at two scales. One unit failing on
      // its own is the `a` case. One CAUSE failing many units at once — a
      // provider usage limit stopping most of a wide fan-out — is the `u` case,
      // and pressing `a` N times for it is the same repair typed N times.
      return [
        { id: 'retry-unit', label: 'Retry unit', key: 'a', variant: 'primary' },
        { id: 'retry-failed-units', label: 'Retry all failed units', key: 'u', variant: 'secondary' },
        { id: 'drop-unit', label: 'Drop unit — join proceeds without it', variant: 'secondary', arms: true },
        { id: 'take-over-unit', label: 'Take over unit', key: 't', variant: 'ghost' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'taken-over':
      // Not in the §4.3 table: a run under human control has one engine edge
      // back (`CompleteTakeover` runs the finalize turn on the steered thread),
      // and the thread it is already being steered in is where the work
      // continues — the run got here because a human sent into that thread.
      return [
        { id: 'complete-takeover', label: 'Finish takeover', key: 'a', variant: 'primary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'done':
      return [
        { id: 'merge', label: 'Merge to base', key: 'a', variant: 'primary' },
        { id: 'create-pr', label: 'Create PR', variant: 'secondary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'running': {
      // Pause stops now; the soft stop stops at the next call boundary. They sit
      // next to each other because they answer the same question ("make it
      // stop") with different costs — one interrupts a turn, the other waits for
      // the wave to finish — and the armed label is the only place a human sees
      // that the second one is pending.
      const row: WorkflowActionButton[] = [{ id: 'pause', label: 'Pause', variant: 'secondary' }];
      if (input.softStop) {
        row.push(input.softStop.armed
          ? { id: 'clear-soft-stop', label: 'Stopping after this wave — undo', variant: 'primary' }
          : { id: 'soft-stop', label: 'Stop after this wave', variant: 'secondary' });
      }
      row.push(
        { id: 'open-phase-thread', label: 'Open phase thread', variant: 'ghost' },
        { id: 'cancel', label: 'Stop this run', variant: 'danger-outline', arms: true },
      );
      return row;
    }
    case 'cancelled':
      return [
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
        { id: 'back', label: 'Back', variant: 'ghost' },
      ];
  }
}

/** The action a §8 key fires on this row, or null when the row has none. */
export function workflowActionForKey(
  row: readonly WorkflowActionButton[],
  key: WorkflowActionKey,
): WorkflowActionButton | null {
  return row.find((action) => action.key === key) ?? null;
}

/**
 * `WHAT HAPPENED` / `WHAT IT NEEDS` (§4.3). The engine seeds a digest on every
 * resting transition; this is the deterministic fallback for a run whose
 * digest has not landed yet, so the two-row block never renders empty.
 */
export function workflowDigestFallback(
  kind: WorkflowResolutionKind,
  phaseId: string,
): { whatHappened: string; whatItNeeds: string } {
  const phase = phaseId || 'the run';
  switch (kind) {
    case 'gate':
      return { whatHappened: `${phase} finished and asked for review.`, whatItNeeds: 'Approve it or request changes.' };
    case 'question':
      return { whatHappened: `${phase} paused on a question.`, whatItNeeds: 'Answer it and the phase resumes where it yielded.' };
    case 'paused':
      return { whatHappened: `${phase} stopped before it produced a result.`, whatItNeeds: 'Resume it or discard it.' };
    case 'checkpoint':
      return {
        whatHappened: 'The run stopped where you asked, before starting the next one.',
        whatItNeeds: 'Resume it to continue, or leave it stopped.',
      };
    case 'unit-failed':
      return { whatHappened: 'A fan-out unit failed; its siblings finished.', whatItNeeds: 'Retry the unit (or every failed one), drop it, or take it over.' };
    case 'taken-over':
      return { whatHappened: `${phase} is under your control.`, whatItNeeds: 'Finish the takeover to hand it back.' };
    case 'done':
      return { whatHappened: 'The run finished.', whatItNeeds: 'Decide what happens to the branch.' };
    case 'running':
      return { whatHappened: `${phase} is running.`, whatItNeeds: 'Nothing — it is still working.' };
    case 'cancelled':
      return { whatHappened: 'The run was stopped.', whatItNeeds: 'Nothing — discard it when you are done with the worktree.' };
    case 'failed':
      return { whatHappened: `${phase} could not finish.`, whatItNeeds: 'Rerun it with guidance, or discard it.' };
    case 'blocked':
      // Says that resuming starts the phase over, because the word "resume"
      // means continuing the same session one row up and the two must not be
      // confused.
      return { whatHappened: `${phase} stopped and needs you.`, whatItNeeds: 'Clear what blocked it, then resume — the phase starts over.' };
  }
}
