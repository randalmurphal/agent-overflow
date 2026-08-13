// R1 — two-hue attention. Amber (`--warning`) ONLY for states a human must
// unblock; red (`--error`) ONLY for `failed`. Everything else is typographic:
// no dot, no badge. Done-awaiting-disposition is deliberately neutral and has
// no time-based escalation.
//
// Amber rows reuse the sidebar's existing `status-glow-warning` ring + pulse
// conventions (threadStatusPill.ts / app.css) so the whole app has one
// vocabulary for "this is blocked on you".

import type { WorkflowRunReason, WorkflowRunState } from '../types/workflow';

/**
 * What a single node on a run's timeline is doing — a phase attempt, a fan-out
 * unit, a called run. The RUN state above and the NODE state below are separate
 * vocabularies (a run is `needs-human`, a unit is `failed`), but they answer to
 * the same two-hue rule, so both live here rather than in whatever module
 * happened to need one first.
 */
export type WorkflowNodeSignal = 'done' | 'running' | 'pending' | 'failed' | 'dropped' | 'parked';

/**
 * Tailwind tone per node signal. A total table rather than an if-chain with a
 * neutral default: a signal added to the union without a hue decision here is a
 * COMPILE error, where the default silently painted it neutral — which is a
 * decision about R1 made by omission.
 */
const NODE_TONES: Record<WorkflowNodeSignal, string> = {
  done: 'text-fg-muted',
  running: 'text-fg-muted',
  pending: 'text-fg-muted',
  dropped: 'text-fg-muted',
  failed: 'text-error',
  parked: 'text-warning',
};

/** Tailwind tone for a node signal. R1 keeps everything but failure neutral. */
export function workflowNodeTone(signal: WorkflowNodeSignal): string {
  return NODE_TONES[signal];
}

export interface WorkflowRunSignal {
  signal: 'attention' | 'failed' | 'none';
  label: string;
  tone: string;
  dotClass: string;
  pulse: boolean;
  glowClass?: string;
}

/**
 * The states that get a plain word. `needs-human` and `failed` are excluded by
 * TYPE, not by omission: both are answered above this table, and an entry for
 * either would be a second place their word is decided.
 */
type WorkflowNeutralRunState = Exclude<WorkflowRunState, 'needs-human' | 'failed'>;

const neutralLabels: Record<WorkflowNeutralRunState, string> = {
  done: 'Done',
  running: 'Running',
  cancelled: 'Cancelled',
};

// The state word (§4.1). Every typed reason gets a human word; an unknown
// reason falls back to the generic "Needs you" rather than leaking the token.
//
// Keyed on the reason UNION, so a reason the engine grows and this build has
// not learnt fails `pnpm run check` here instead of silently rendering as
// "Needs you" in the field. Both tables are read through the boundary lookups
// below, which are the one place a genuinely-unknown WIRE string is allowed to
// reach them.
const attentionLabels: Record<WorkflowRunReason, string> = {
  gate: 'Review gate',
  question: 'Question',
  stuck: 'Stuck',
  stalled: 'Stalled',
  paused: 'Paused',
  interrupted: 'Interrupted',
  checkpoint: 'Stopped at checkpoint',
  'unit-failed': 'Unit failed',
  'child-failed': 'Child failed',
  disposition: 'Disposition',
  'budget-exhausted': 'Budget spent',
  'retries-exhausted': 'Retries spent',
  'provider-retries-exhausted': 'Provider retries spent',
  'loop-limit-exhausted': 'Loop limit spent',
  'check-failed-genuine': 'Check failed',
  'agent-error': 'Agent error',
  'wiring-error': 'Wiring error',
  'setup-failed': 'Setup failed',
  'taken-over': 'Taken over',
};

/**
 * The ONE crossing from wire string to typed reason. A payload is bytes until
 * something checks it, and every other read of the table goes through here —
 * which is what lets the table itself stay total over the union.
 */
export function workflowAttentionLabel(reason?: WorkflowRunReason | string): string {
  const key = String(reason ?? '');
  return Object.hasOwn(attentionLabels, key)
    ? attentionLabels[key as WorkflowRunReason]
    : 'Needs you';
}

/** The same crossing for the neutral state words; '' for anything unlisted. */
function workflowNeutralLabel(state: WorkflowRunState | string): string {
  const key = String(state);
  return Object.hasOwn(neutralLabels, key) ? neutralLabels[key as WorkflowNeutralRunState] : '';
}

export function workflowRunSignal(
  state: WorkflowRunState | string,
  reason?: WorkflowRunReason | string,
): WorkflowRunSignal {
  if (state === 'needs-human') {
    return {
      signal: 'attention',
      label: workflowAttentionLabel(reason),
      tone: 'text-warning',
      dotClass: 'bg-warning',
      pulse: true,
      glowClass: 'status-glow-warning',
    };
  }
  if (state === 'failed') {
    return {
      signal: 'failed', label: 'Failed', tone: 'text-error', dotClass: 'bg-error', pulse: false,
    };
  }
  return {
    signal: 'none', label: workflowNeutralLabel(state), tone: 'text-fg-muted', dotClass: '', pulse: false,
  };
}
