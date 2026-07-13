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
  queued: 'Queued',
  running: 'Running',
  cancelled: 'Cancelled',
};

function attentionLabel(reason?: WorkflowRunReason): string {
  if (reason === 'gate') return 'Review gate';
  if (reason === 'question') return 'Question';
  if (reason === 'disposition') return 'Disposition';
  return 'Needs you';
}

export function workflowRunSignal(
  state: WorkflowRunState | string,
  reason?: WorkflowRunReason | string,
): WorkflowRunSignal {
  if (state === 'needs-human') {
    return {
      signal: 'attention',
      label: attentionLabel(reason as WorkflowRunReason | undefined),
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
