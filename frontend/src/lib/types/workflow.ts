import type {
  WorkflowArtifact,
  WorkflowDefinitionCatalog,
  WorkflowDefinitionInput,
  WorkflowDefinitionListing,
  WorkflowItemDetailView,
  WorkflowItemPhaseView,
} from '../../../bindings/agent-overflow/models';
import type {
  WorkItem,
  WorkItemUsage,
} from '../../../bindings/agent-overflow/internal/store/models';

export type WorkflowItemDetail = WorkflowItemDetailView;
export type WorkItemPhase = WorkflowItemPhaseView;

export type WorkflowRunState = 'queued' | 'running' | 'needs-human' | 'done' | 'failed' | 'cancelled';
export type WorkflowRunReason =
  | 'gate'
  | 'question'
  | 'stuck'
  | 'stalled'
  | 'budget-exhausted'
  | 'retries-exhausted'
  | 'check-failed-genuine'
  | 'agent-error'
  | 'wiring-error'
  | 'disposition'
  | 'setup-failed'
  | 'interrupted'
  | 'taken-over';

export interface WorkflowItemStateEvent {
  itemId: string;
  projectId: string;
  from: WorkflowRunState;
  to: WorkflowRunState;
  reason?: WorkflowRunReason;
}

export interface WorkflowQueueStateEvent {
  active: boolean;
  globalConcurrency: number;
  startsRemaining?: number;
}

export interface WorkflowPhaseStateEvent {
  itemId: string;
  phaseId: string;
  attempt: number;
  status: string;
}

export interface WorkflowErrorEvent {
  itemId?: string;
  error: string;
  spend?: number;
  wallClockMillis?: number;
}

export interface WorkflowDigest {
  whatHappened: string;
  whatItNeeds: string;
}

export interface WorkflowDispositionReceipt {
  action: 'merged' | 'pr' | 'discarded';
  mode?: 'ff' | 'merge';
  sha?: string;
  prRef?: string;
  base?: string;
  cleanupFailed?: boolean;
  policy: string;
  at: number;
}

export type WorkflowPaneLevel =
  | { kind: 'overview' }
  | { kind: 'workflow'; projectId: string; workflowId: string; label: string }
  | {
      kind: 'run';
      projectId: string;
      workflowId: string;
      workflowLabel: string;
      itemId: string;
      label: string;
      sweep: boolean;
    }
  | { kind: 'all-clear' };

export type WorkflowsPaneTarget =
  | { kind: 'overview' }
  | { kind: 'workflow'; projectId: string; workflowId: string; label?: string }
  | {
      kind: 'run';
      projectId: string;
      workflowId: string;
      itemId: string;
      workflowLabel?: string;
      label?: string;
    }
  | {
      kind: 'sweep-at-run';
      projectId: string;
      workflowId: string;
      itemId: string;
      workflowLabel?: string;
      label?: string;
    };

export interface WorkflowResolvedReceipt {
  itemId: string;
  kind: 'approved' | 'answered' | 'handed-off' | 're-enqueued' | 'merged' | 'pr' | 'discarded' | 'removed';
  message: string;
  costUsd: number;
}

export interface WorkflowDefinitionView {
  projectId: string;
  catalog: WorkflowDefinitionCatalog;
  definition: WorkflowDefinitionListing;
}

export type {
  WorkItem,
  WorkItemUsage,
  WorkflowArtifact,
  WorkflowDefinitionCatalog,
  WorkflowDefinitionInput,
  WorkflowDefinitionListing,
};

function parseJSONRecord(value: unknown): Record<string, unknown> | null {
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  if (typeof value !== 'string' || value.trim() === '') return null;
  try {
    const parsed = JSON.parse(value) as unknown;
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
      ? parsed as Record<string, unknown>
      : null;
  } catch {
    return null;
  }
}

export function parseWorkflowDigest(value: unknown): WorkflowDigest | null {
  const parsed = parseJSONRecord(value);
  if (!parsed || typeof parsed.whatHappened !== 'string' || typeof parsed.whatItNeeds !== 'string') {
    return null;
  }
  return { whatHappened: parsed.whatHappened, whatItNeeds: parsed.whatItNeeds };
}

export function parseWorkflowDisposition(value: unknown): WorkflowDispositionReceipt | null {
  const parsed = parseJSONRecord(value);
  if (!parsed || (parsed.action !== 'merged' && parsed.action !== 'pr' && parsed.action !== 'discarded')) {
    return null;
  }
  if (typeof parsed.policy !== 'string' || typeof parsed.at !== 'number') return null;
  return {
    action: parsed.action,
    mode: parsed.mode === 'ff' || parsed.mode === 'merge' ? parsed.mode : undefined,
    sha: typeof parsed.sha === 'string' ? parsed.sha : undefined,
    prRef: typeof parsed.prRef === 'string' ? parsed.prRef : undefined,
    base: typeof parsed.base === 'string' ? parsed.base : undefined,
    cleanupFailed: parsed.cleanupFailed === true ? true : undefined,
    policy: parsed.policy,
    at: parsed.at,
  };
}
