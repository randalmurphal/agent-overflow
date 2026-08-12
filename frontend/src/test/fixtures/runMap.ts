// Run-map wire fixtures, shared by every suite that feeds a `WorkflowRunMapView`
// through the projection, the patcher, the store or the components.
//
// ALWAYS through the generated binding classes. Five suites had each grown its
// own literal builders, and one of them had drifted into hand-written object
// literals typed as the wire shape — which by construction cannot catch wire
// drift, because a field the Go side adds, renames or makes required is a field
// a literal simply does not have. The classes are regenerated from the Go
// structs, so a schema change fails the suites here rather than in production.
//
// Defaults are the NEUTRAL ones: a running run with no skeleton, no records and
// no money. Every suite states the shape its case is about and nothing else, so
// what a test is testing is what the test says.

import {
  WorkflowAgentRunBudget,
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRefusal,
  WorkflowRunMapRun,
  WorkflowRunMapSkeletonPhase,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
  WorkflowRunSpend,
} from '../../../bindings/agent-overflow/models';

/**
 * Defaults + overrides, where an override of `undefined` REMOVES the key.
 *
 * An optional wire field (`endedAt` on an open span, `startedAt` on a queued
 * unit) is ABSENT on the wire, not present-and-zero — Go omits it — and a
 * spread cannot express "unset this default". Writing zero instead is what
 * makes a fixture disagree with the payload it claims to be.
 */
function withDefaults<T extends object>(defaults: T, over: Partial<T>): T {
  const merged = { ...defaults } as Record<string, unknown>;
  for (const [key, value] of Object.entries(over)) {
    if (value === undefined) delete merged[key];
    else merged[key] = value;
  }
  return merged as T;
}

export function skeletonPhase(
  id: string,
  over: Partial<WorkflowRunMapSkeletonPhase> = {},
): WorkflowRunMapSkeletonPhase {
  return new WorkflowRunMapSkeletonPhase(
    withDefaults({ id, shape: 'single', isCheck: false }, over),
  );
}

export function phaseAttempt(
  phaseId: string,
  over: Partial<WorkflowRunMapPhaseAttempt> = {},
): WorkflowRunMapPhaseAttempt {
  return new WorkflowRunMapPhaseAttempt(withDefaults({
    phaseId, attempt: 1, status: 'completed', startedAt: 1_000, endedAt: 2_000,
  }, over));
}

export function mapUnit(
  unitId: string,
  over: Partial<WorkflowRunMapUnit> = {},
): WorkflowRunMapUnit {
  return new WorkflowRunMapUnit(withDefaults({
    phaseId: 'port', attempt: 1, unitId, unitIndex: 0, kind: 'unit',
    status: 'done', unitAttempt: 1, startedAt: 1_000, endedAt: 2_000,
  }, over));
}

export function mapRun(
  itemId: string,
  over: Partial<WorkflowRunMapRun> = {},
): WorkflowRunMapRun {
  return new WorkflowRunMapRun(withDefaults({
    itemId, workflowId: 'campaign', state: 'running', softStop: false,
    skeleton: [], skeletonMissing: false, tailSelfCall: false,
    phases: [], units: [], startedAt: 1_000,
  }, over));
}

export function mapView(
  runs: WorkflowRunMapRun[],
  rootItemId = runs[0]?.itemId ?? '',
): WorkflowRunMapView {
  return new WorkflowRunMapView({ rootItemId, runs });
}

/** A refusal answer (§4.2): the RPC succeeded, and there is no tree to draw. */
export function refusedView(code: string, message: string): WorkflowRunMapView {
  return new WorkflowRunMapView({
    rootItemId: '', runs: [], refusal: new WorkflowRunMapRefusal({ code, message }),
  });
}

/** The canonical campaign skeleton: two phases, then the tail self-call. */
export function campaignSkeleton(maxDepth = 5): WorkflowRunMapSkeletonPhase[] {
  return [
    skeletonPhase('audit', { name: 'audit', isCheck: true }),
    skeletonPhase('fix'),
    skeletonPhase('next', { shape: 'call', callTarget: 'campaign', maxDepth }),
  ];
}

export function runSpend(over: Partial<WorkflowRunSpend> = {}): WorkflowRunSpend {
  return new WorkflowRunSpend({
    costUsd: 0, wireCostUsd: 0, estimatedCostUsd: 0, unpricedRows: 0, ...over,
  });
}

export function runBudget(over: Partial<WorkflowAgentRunBudget> = {}): WorkflowAgentRunBudget {
  return new WorkflowAgentRunBudget({ kind: 'usd', percent: 0, ...over });
}
