// R1 — two-hue attention. Amber (`--warning`) ONLY for states a human must
// unblock; red (`--error`) ONLY for `failed`. Everything else is typographic:
// no dot, no badge. Done-awaiting-disposition is deliberately neutral and has
// no time-based escalation.
//
// Amber rows reuse the sidebar's existing `status-glow-warning` ring + pulse
// conventions (threadStatusPill.ts / app.css) so the whole app has one
// vocabulary for "this is blocked on you".

import type { WorkflowRunReason, WorkflowRunState } from '../types/workflow';

export interface WorkflowRunSignal {
  signal: 'attention' | 'failed' | 'none';
  label: string;
  tone: string;
  dotClass: string;
  pulse: boolean;
  glowClass?: string;
}

const neutralLabels: Record<string, string> = {
  done: 'Done',
  running: 'Running',
  cancelled: 'Cancelled',
};

// The state word (§4.1). Every typed reason gets a human word; an unknown
// reason falls back to the generic "Needs you" rather than leaking the token.
const attentionLabels: Record<string, string> = {
  gate: 'Review gate',
  question: 'Question',
  stuck: 'Stuck',
  stalled: 'Stalled',
  paused: 'Paused',
  interrupted: 'Interrupted',
  'unit-failed': 'Unit failed',
  'child-failed': 'Child failed',
  disposition: 'Disposition',
  'budget-exhausted': 'Budget spent',
  'retries-exhausted': 'Retries spent',
  'check-failed-genuine': 'Check failed',
  'agent-error': 'Agent error',
  'wiring-error': 'Wiring error',
  'setup-failed': 'Setup failed',
  'taken-over': 'Taken over',
};

export function workflowAttentionLabel(reason?: WorkflowRunReason | string): string {
  return attentionLabels[String(reason ?? '')] ?? 'Needs you';
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
    signal: 'none', label: neutralLabels[state] ?? '', tone: 'text-fg-muted', dotClass: '', pulse: false,
  };
}
