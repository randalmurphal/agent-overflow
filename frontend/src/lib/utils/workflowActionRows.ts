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
  | 'stuck'
  | 'paused'
  | 'unit-failed'
  | 'taken-over'
  | 'done'
  | 'running'
  | 'cancelled';

export type WorkflowActionId =
  | 'approve'
  | 'request-changes'
  | 'take-over'
  | 'rerun'
  | 'resume'
  | 'retry-unit'
  | 'drop-unit'
  | 'take-over-unit'
  | 'complete-takeover'
  | 'merge'
  | 'create-pr'
  | 'open-in-thread'
  | 'open-phase-thread'
  | 'pause'
  | 'cancel'
  | 'discard'
  | 'back';

export type WorkflowActionKey = 'a' | 'r' | 't';

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
 * Which §4.3 row a run is on. `failed` shares the stuck row (the table groups
 * "stuck / failed"); `needs-human(disposition)` shares the done row, because a
 * refused merge parks exactly where the disposition decision lives.
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
      return 'stuck';
    case 'needs-human':
      break;
    default:
      return 'stuck';
  }
  const reason = item.reason ?? '';
  if (reason === 'gate') return 'gate';
  if (reason === 'question') return 'question';
  if (reason === 'unit-failed') return 'unit-failed';
  if (reason === 'taken-over') return 'taken-over';
  if (RESUMABLE_REASONS.has(reason)) return 'paused';
  if (DONE_REASONS.has(reason)) return 'done';
  return 'stuck';
}

export interface WorkflowActionRowInput {
  kind: WorkflowResolutionKind;
  /** Names the phase an approved gate routes to; falls back to "next phase". */
  nextPhaseId?: string;
  /** A bound run already has a thread; an unbound one can seed one (D17). */
  bound: boolean;
  /** A child run resolves inside its parent's tree — no bind, no notify (D18). */
  isChild: boolean;
}

export function workflowActionRow(input: WorkflowActionRowInput): WorkflowActionButton[] {
  switch (input.kind) {
    case 'gate':
      return [
        { id: 'approve', label: `Approve → ${input.nextPhaseId || 'next phase'}`, key: 'a', variant: 'primary' },
        { id: 'request-changes', label: 'Request changes', key: 'r', variant: 'secondary' },
        { id: 'take-over', label: 'Take over', key: 't', variant: 'ghost' },
      ];
    case 'question':
      return [
        { id: 'take-over', label: 'Take over instead', key: 't', variant: 'ghost' },
      ];
    case 'stuck':
      return [
        { id: 'take-over', label: 'Continue with agent', key: 't', variant: 'primary' },
        { id: 'rerun', label: 'Rerun with guidance', key: 'a', variant: 'secondary' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'paused':
      return [
        { id: 'resume', label: 'Resume', key: 'a', variant: 'primary' },
        { id: 'take-over', label: 'Take over', key: 't', variant: 'ghost' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'unit-failed':
      return [
        { id: 'retry-unit', label: 'Retry unit', key: 'a', variant: 'primary' },
        { id: 'drop-unit', label: 'Drop unit — join proceeds without it', variant: 'secondary', arms: true },
        { id: 'take-over-unit', label: 'Take over unit', key: 't', variant: 'ghost' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'taken-over':
      // Not in the §4.3 table: a run under human control has one engine edge
      // back (`CompleteTakeover` runs the finalize turn on the steered thread),
      // and the thread itself is where the work continues.
      return [
        { id: 'complete-takeover', label: 'Finish takeover', key: 'a', variant: 'primary' },
        { id: 'take-over', label: 'Continue with agent', key: 't', variant: 'ghost' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
    case 'done': {
      const row: WorkflowActionButton[] = [
        { id: 'merge', label: 'Merge to base', key: 'a', variant: 'primary' },
        { id: 'create-pr', label: 'Create PR', variant: 'secondary' },
        { id: 'take-over', label: 'Continue with agent ↗', key: 't', variant: 'ghost' },
        { id: 'discard', label: 'Discard', key: 'r', variant: 'danger-outline' },
      ];
      if (!input.bound && !input.isChild) {
        row.splice(3, 0, { id: 'open-in-thread', label: 'Open in thread', variant: 'ghost' });
      }
      return row;
    }
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
      return { whatHappened: `${phase} stopped before it produced a result.`, whatItNeeds: 'Resume it or take it over.' };
    case 'unit-failed':
      return { whatHappened: 'A fan-out unit failed; its siblings finished.', whatItNeeds: 'Retry the unit, drop it, or take it over.' };
    case 'taken-over':
      return { whatHappened: `${phase} is under your control.`, whatItNeeds: 'Finish the takeover to hand it back.' };
    case 'done':
      return { whatHappened: 'The run finished.', whatItNeeds: 'Decide what happens to the branch.' };
    case 'running':
      return { whatHappened: `${phase} is running.`, whatItNeeds: 'Nothing — it is still working.' };
    case 'cancelled':
      return { whatHappened: 'The run was stopped.', whatItNeeds: 'Nothing — discard it when you are done with the worktree.' };
    case 'stuck':
      return { whatHappened: `${phase} could not finish.`, whatItNeeds: 'Continue with an agent, rerun it, or discard it.' };
  }
}
