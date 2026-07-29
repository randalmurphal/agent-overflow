// The per-state resolution model (UI-SPEC §4.3 + §8). One table maps a run's
// (state, reason) to the evidence block the detail opens with and to the fixed
// footer's action row, primary first, each carrying the key §8 binds it to.
//
// Pure: no RPCs, no Svelte. WorkflowRunDetail renders the rows and
// workflowActions.ts executes the ids.

import type { WorkItem } from '../types/workflow';

export type WorkflowResolutionKind =
  | 'gate'
  | 'question'
  | 'failed'
  | 'blocked'
  | 'paused'
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

const RESUMABLE_REASONS = new Set(['paused', 'interrupted']);
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
  if (RESUMABLE_REASONS.has(reason)) return 'paused';
  if (DONE_REASONS.has(reason)) return 'done';
  return 'blocked';
}

export interface WorkflowActionRowInput {
  kind: WorkflowResolutionKind;
  /** Names the phase an approved gate routes to; falls back to "next phase". */
  nextPhaseId?: string;
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
    case 'running':
      return [
        { id: 'pause', label: 'Pause', variant: 'secondary' },
        { id: 'open-phase-thread', label: 'Open phase thread', variant: 'ghost' },
        { id: 'cancel', label: 'Stop this run', variant: 'danger-outline', arms: true },
      ];
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
