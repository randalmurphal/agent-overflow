import type {
  ProjectCleanupWorktree,
  ProjectDeletionPreview,
  ProjectDeletionResult,
  RetainedWorktree,
  WorkflowArtifact,
  WorkflowAutomationView,
  WorkflowDefinitionCatalog,
  WorkflowDefinitionInput,
  WorkflowDefinitionListing,
  WorkflowDiscardPreview,
  WorkflowDiscardWorktree,
  WorkflowItemDetailView,
  WorkflowItemPhaseView,
  WorkflowItemUnitView,
  WorkflowAgentRunBudget,
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRefusal,
  WorkflowRunMapRun,
  WorkflowRunMapSkeletonPhase,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
  WorkflowRunSpend,
} from '../../../bindings/agent-overflow/internal/app/models';
import type {
  WorkItem,
  WorkItemUsage,
} from '../../../bindings/agent-overflow/internal/store/models';

export type WorkflowItemDetail = WorkflowItemDetailView;
export type WorkItemPhase = WorkflowItemPhaseView;
export type WorkItemUnit = WorkflowItemUnitView;

export type WorkflowRunState = 'running' | 'needs-human' | 'done' | 'failed' | 'cancelled';
export type WorkflowRunReason =
  | 'gate'
  | 'question'
  | 'stuck'
  | 'stalled'
  | 'budget-exhausted'
  | 'retries-exhausted'
  | 'provider-retries-exhausted'
  | 'provider-usage-limited'
  | 'loop-limit-exhausted'
  | 'check-failed-genuine'
  | 'agent-error'
  | 'wiring-error'
  | 'disposition'
  | 'setup-failed'
  | 'interrupted'
  | 'paused'
  | 'unit-failed'
  | 'child-failed'
  | 'checkpoint'
  | 'taken-over';

export interface WorkflowItemStateEvent {
  itemId: string;
  projectId: string;
  from: WorkflowRunState;
  to: WorkflowRunState;
  reason?: WorkflowRunReason;
  /**
   * Where the run WAS when it transitioned (`engine.StateEvent`) — the attempt
   * a park rests on, which is the coordinate its cause and its narrative are
   * filed under. Absent on a run that has not entered a phase yet, and on the
   * transitions taken with the run non-resident.
   */
  phaseId?: string;
  attempt?: number;
}

/**
 * WorkflowSoftStopEvent is the `workflow:soft-stop` payload: a run tree's
 * standing request to stop at its next call boundary was armed or withdrawn.
 * It is not an item-state transition because nothing about the run's state
 * changed — the run is still running, it simply now has an appointment.
 */
export interface WorkflowSoftStopEvent {
  itemId: string;
  armed: boolean;
}

/**
 * WorkflowEngineStateEvent is the `workflow:engine-state` payload: the live
 * global pause flag. There is no queue, so this is the whole engine-wide
 * control surface.
 */
export interface WorkflowEngineStateEvent {
  paused: boolean;
}

export interface WorkflowPhaseStateEvent {
  itemId: string;
  phaseId: string;
  attempt: number;
  status: string;
  /** Set when the event reports one fan-out unit inside the attempt. */
  unitId?: string;
  unitIndex?: number;
  unitKind?: string;
  /**
   * The engine's own event time in Unix milliseconds, stamped on the single
   * emit path (`engine.emitPhaseState`) so no site can forget it. A consumer
   * patching a live view reads it as the transition's moment: a `running`
   * status starts the attempt or unit, a terminal one ends it. Client time
   * would drift across reconnects and replay, where an event's ARRIVAL says
   * nothing about when it happened.
   */
  occurredAt: number;
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

export interface WorkflowResolvedReceipt {
  itemId: string;
  kind: 'approved' | 'answered' | 'handed-off' | 'restarted' | 'merged' | 'pr' | 'discarded';
  message: string;
  costUsd: number;
}

export interface WorkflowDefinitionView {
  projectId: string;
  catalog: WorkflowDefinitionCatalog;
  definition: WorkflowDefinitionListing;
}

export type {
  // Project deletion (D25) walks the same run trees the workflow surfaces do,
  // so its shapes live here rather than growing a parallel definition next to
  // the project types. What it reports is its own: a cleanup, not a loss.
  ProjectCleanupWorktree,
  ProjectDeletionPreview,
  ProjectDeletionResult,
  RetainedWorktree,
  WorkItem,
  WorkItemUsage,
  WorkflowArtifact,
  WorkflowAutomationView,
  WorkflowDefinitionCatalog,
  WorkflowDefinitionInput,
  WorkflowDefinitionListing,
  WorkflowDiscardPreview,
  WorkflowDiscardWorktree,
  // The run map (RUN-MAP §4.2): one whole run TREE as metadata. Re-exported
  // here so the store, the projection and the components name these shapes
  // through the same module as every other workflow type.
  WorkflowAgentRunBudget,
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRefusal,
  WorkflowRunMapRun,
  WorkflowRunMapSkeletonPhase,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
  WorkflowRunSpend,
};

/**
 * Why a run map will never be answered. Every code is PERMANENT: retrying
 * cannot make a tree smaller, a linkage acyclic, or a deleted run exist, so a
 * consumer that sees one must stop re-sourcing rather than back off. They
 * arrive on `WorkflowRunMapView.refusal` with the RPC itself succeeding —
 * the transport strips a method error's text for remote clients, so a refusal
 * returned as an error could not carry its own sentence there.
 *
 * Mirrors the Go constants in `app_workflow_runmap.go`; the binding generator
 * emits the struct but not an untyped string const block.
 */
export type WorkflowRunMapRefusalCode = 'not-found' | 'too-large' | 'corrupt-linkage';

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
