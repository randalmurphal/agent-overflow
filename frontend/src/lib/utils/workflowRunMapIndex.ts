// The per-call index behind the run map, and the derivations that read only one
// run: node keys, status mapping, the wave chain, and the folded wave's summary.
//
// Everything here is built ONCE per `buildRunMap` call and answers in O(1): children bucketed by the attempt or unit that called them,
// attempts bucketed by phase, units bucketed by attempt, chain ordinals and
// subtree counts filled by a single post-order walk. That is what keeps the
// projection linear in runs + phases + units with no quadratic scan anywhere.

import { compositeKey } from './compositeKey';
import { workflowClosedDuration, workflowDuration } from './format';
import { workflowAttentionLabel, workflowNodeTone, workflowRunSignal } from './workflowRunSignal';
import type {
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRun,
  WorkflowRunMapSkeletonPhase,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
} from '../types/workflow';
import type {
  RunMapCompositionSummary,
  RunMapLoop,
  RunMapPhaseStatus,
  RunMapRunStatus,
  RunMapSignal,
  RunMapUnitStatus,
  RunMapUnitTotals,
  RunMapWaveOutcome,
  RunMapWaveSummary,
} from './workflowRunMapTypes';

// ---------------------------------------------------------------- keys

export const waveKey = (itemId: string) => compositeKey('wave', itemId);
export const nodeKeyOf = (itemId: string, phaseId: string) => compositeKey('node', itemId, phaseId);
export const attemptKeyOf = (itemId: string, phaseId: string, attempt: number) =>
  compositeKey('attempt', itemId, phaseId, attempt);
export const fanKeyOf = (itemId: string, phaseId: string, attempt: number) =>
  compositeKey('fan', itemId, phaseId, attempt);
export const unitKeyOf = (itemId: string, phaseId: string, attempt: number, unitId: string) =>
  compositeKey('unit', itemId, phaseId, attempt, unitId);
export const branchKeyOf = (itemId: string, phaseId: string, attempt: number, unitId: string) =>
  compositeKey('branch', itemId, phaseId, attempt, unitId);
export const groupKeyOf = (itemId: string, phaseId: string, attempt: number, kind: string) =>
  compositeKey('group', itemId, phaseId, attempt, kind);
export const compositionKeyOf = (itemId: string) => compositeKey('composition', itemId);
const decisionKeyOf = (itemId: string) => compositeKey('decision', itemId);

/** Compositions expand to two levels below their wave; deeper collapses. */
export const RUN_MAP_COMPOSITION_DEPTH = 2;

// ---------------------------------------------------------------- mapping

export const PHASE_SIGNALS: Record<RunMapPhaseStatus['kind'], RunMapSignal> = {
  ghost: 'ghost',
  running: 'running',
  completed: 'done',
  parked: 'parked',
  failed: 'failed',
  cancelled: 'dropped',
  unknown: 'unknown',
};

export const UNIT_SIGNALS: Record<RunMapUnitStatus['kind'], RunMapSignal> = {
  pending: 'pending',
  running: 'running',
  done: 'done',
  failed: 'failed',
  dropped: 'dropped',
  'taken-over': 'parked',
  unknown: 'unknown',
};

export const RUN_SIGNALS: Record<RunMapRunStatus['kind'], RunMapSignal> = {
  running: 'running',
  'needs-human': 'parked',
  done: 'done',
  failed: 'failed',
  cancelled: 'dropped',
  unknown: 'unknown',
};

/**
 * Tone for a map signal, and the ONE place the map decides a hue. R1's two
 * meanings come from the shared mapping; the two signals the tree never had
 * are neutral by construction — a state the build cannot name must not borrow
 * the colour of one it can.
 */
export function runMapTone(signal: RunMapSignal): string {
  if (signal === 'ghost') return 'text-fg-hint';
  if (signal === 'unknown') return 'text-fg-muted';
  return workflowNodeTone(signal);
}

export function phaseStatusOf(attempt: WorkflowRunMapPhaseAttempt): RunMapPhaseStatus {
  const cause = attempt.cause ?? '';
  switch (attempt.status) {
    case 'running': return { kind: 'running' };
    case 'completed': return { kind: 'completed' };
    case 'parked': return { kind: 'parked', cause };
    case 'failed': return { kind: 'failed', cause };
    case 'cancelled': return { kind: 'cancelled' };
    default: return { kind: 'unknown', raw: attempt.status };
  }
}

export function unitStatusOf(unit: WorkflowRunMapUnit): RunMapUnitStatus {
  switch (unit.status) {
    case 'pending': return { kind: 'pending', provider: unit.provider ?? '' };
    case 'running': return { kind: 'running' };
    case 'done': return { kind: 'done' };
    case 'failed': return { kind: 'failed' };
    case 'dropped': return { kind: 'dropped' };
    case 'taken-over': return { kind: 'taken-over' };
    default: return { kind: 'unknown', raw: unit.status };
  }
}

export function runStatusOf(run: WorkflowRunMapRun): RunMapRunStatus {
  const reason = run.reason ?? '';
  switch (run.state) {
    case 'running': return { kind: 'running' };
    case 'needs-human': return { kind: 'needs-human', reason };
    case 'done': return { kind: 'done' };
    case 'failed': return { kind: 'failed', reason };
    case 'cancelled': return { kind: 'cancelled' };
    default: return { kind: 'unknown', raw: run.state };
  }
}

/** Live runs are the only ones that draw a future (§5.6). */
export function isLiveRun(run: WorkflowRunMapRun): boolean {
  return run.state === 'running' || run.state === 'needs-human';
}

export function joinParts(parts: readonly string[]): string {
  return parts.filter((part) => part.trim() !== '').join(' · ');
}

export function phaseLabelOf(phaseId: string, skeleton: WorkflowRunMapSkeletonPhase | undefined): string {
  return skeleton?.name?.trim() || phaseId;
}

// ---------------------------------------------------------------- index

export interface RunIndex {
  /**
   * The clock every span in this build is measured against, or `null` for a
   * build that has NO clock (`runMapPosition`, which wants labels and nothing
   * else). Null is not "zero": a zero clock reports an open span as having run
   * since 1970, which is exactly the fabrication the null exists to refuse.
   */
  nowMs: number | null;
  runs: Map<string, WorkflowRunMapRun>;
  skeletonById: Map<string, Map<string, WorkflowRunMapSkeletonPhase>>;
  /** phaseId → attempts ascending, plus first-appearance order of recorded ids. */
  attempts: Map<string, Map<string, WorkflowRunMapPhaseAttempt[]>>;
  recordedOrder: Map<string, string[]>;
  units: Map<string, WorkflowRunMapUnit[]>;
  tailChildren: Map<string, WorkflowRunMapRun[]>;
  childrenByAttempt: Map<string, WorkflowRunMapRun[]>;
  childrenByUnit: Map<string, WorkflowRunMapRun[]>;
  ordinals: Map<string, number>;
  subtrees: Map<string, RunMapCompositionSummary>;
}

function pushInto<T>(map: Map<string, T[]>, key: string, value: T): void {
  const bucket = map.get(key);
  if (bucket) bucket.push(value);
  else map.set(key, [value]);
}

export function lastSkeletonPhaseId(run: WorkflowRunMapRun): string {
  const skeleton = run.skeleton;
  return skeleton.length > 0 ? skeleton[skeleton.length - 1].id : '';
}

/**
 * The one edge the map flattens (§3): the parent's LAST skeleton phase calling
 * the parent's own workflow, phase-bound. A non-tail self-call, a call to
 * another workflow and a unit-bound call are all composition, never waves.
 */
export function isTailChild(parent: WorkflowRunMapRun, child: WorkflowRunMapRun): boolean {
  if (!parent.tailSelfCall) return false;
  const tail = lastSkeletonPhaseId(parent);
  if (!tail) return false;
  if ((child.parentUnitId ?? '') !== '') return false;
  return child.parentPhaseId === tail && child.workflowId === parent.workflowId;
}

/**
 * One span against the index's clock, and the ONLY way the projection formats
 * one. A clockless build (`nowMs === null`) still renders every CLOSED span —
 * those need no clock — and renders nothing for an open one, so no entry point
 * can fabricate an elapsed value out of a clock it was never given.
 */
export function indexDuration(index: RunIndex, startedAt: number, endedAt: number): string {
  return index.nowMs === null
    ? workflowClosedDuration(startedAt, endedAt)
    : workflowDuration(startedAt, endedAt, index.nowMs);
}

export function buildIndex(view: WorkflowRunMapView, nowMs: number | null): RunIndex {
  const runs = new Map<string, WorkflowRunMapRun>();
  const skeletonById = new Map<string, Map<string, WorkflowRunMapSkeletonPhase>>();
  const attempts = new Map<string, Map<string, WorkflowRunMapPhaseAttempt[]>>();
  const recordedOrder = new Map<string, string[]>();
  const units = new Map<string, WorkflowRunMapUnit[]>();
  const children = new Map<string, WorkflowRunMapRun[]>();

  for (const run of view.runs) {
    runs.set(run.itemId, run);
    const skeleton = new Map<string, WorkflowRunMapSkeletonPhase>();
    for (const phase of run.skeleton) if (!skeleton.has(phase.id)) skeleton.set(phase.id, phase);
    skeletonById.set(run.itemId, skeleton);

    const byPhase = new Map<string, WorkflowRunMapPhaseAttempt[]>();
    const order: string[] = [];
    for (const attempt of run.phases) {
      const bucket = byPhase.get(attempt.phaseId);
      if (bucket) bucket.push(attempt);
      else {
        byPhase.set(attempt.phaseId, [attempt]);
        order.push(attempt.phaseId);
      }
    }
    for (const bucket of byPhase.values()) bucket.sort((left, right) => left.attempt - right.attempt);
    attempts.set(run.itemId, byPhase);
    recordedOrder.set(run.itemId, order);

    for (const unit of run.units) {
      pushInto(units, compositeKey(run.itemId, unit.phaseId, unit.attempt), unit);
    }
    if (run.parentItemId) pushInto(children, run.parentItemId, run);
  }
  for (const bucket of units.values()) {
    bucket.sort((left, right) => {
      if ((left.kind === 'join') !== (right.kind === 'join')) return left.kind === 'join' ? 1 : -1;
      return left.unitIndex - right.unitIndex || left.unitId.localeCompare(right.unitId);
    });
  }

  const tailChildren = new Map<string, WorkflowRunMapRun[]>();
  const childrenByAttempt = new Map<string, WorkflowRunMapRun[]>();
  const childrenByUnit = new Map<string, WorkflowRunMapRun[]>();
  for (const [parentId, bucket] of children) {
    bucket.sort((left, right) =>
      (left.parentAttempt ?? 0) - (right.parentAttempt ?? 0)
      || (left.startedAt ?? 0) - (right.startedAt ?? 0)
      || left.itemId.localeCompare(right.itemId));
    const parent = runs.get(parentId);
    for (const child of bucket) {
      if (parent && isTailChild(parent, child)) {
        pushInto(tailChildren, parentId, child);
        continue;
      }
      // Both keys are built by spreading ONE parts list: joining the already
      // joined attempt key in as a single part is the nesting collision
      // `compositeKey` exists to refuse.
      const parts: [string, string, number] = [parentId, child.parentPhaseId ?? '', child.parentAttempt ?? 0];
      const unitId = child.parentUnitId ?? '';
      if (unitId) pushInto(childrenByUnit, compositeKey(...parts, unitId), child);
      else pushInto(childrenByAttempt, compositeKey(...parts), child);
    }
  }

  const index: RunIndex = {
    nowMs, runs, skeletonById, attempts, recordedOrder, units,
    tailChildren, childrenByAttempt, childrenByUnit,
    ordinals: new Map(), subtrees: new Map(),
  };
  fillOrdinalsAndSubtrees(index, view, children);
  return index;
}

/**
 * Chain ordinals and subtree aggregates in one post-order walk. Iterative
 * because the engine's absolute call depth is 256 and a corrupt parent cycle
 * must terminate rather than blow a stack.
 */
function fillOrdinalsAndSubtrees(
  index: RunIndex,
  view: WorkflowRunMapView,
  children: Map<string, WorkflowRunMapRun[]>,
): void {
  // Parentless runs first, then a sweep for anything a corrupt parent cycle
  // left unreachable: every run must get an ordinal and a subtree, or a
  // consumer would silently read a default for real data.
  const parentless = view.runs.filter((run) => !run.parentItemId || !index.runs.has(run.parentItemId));
  const visited = new Set<string>();
  for (const root of [...parentless, ...view.runs]) {
    if (visited.has(root.itemId)) continue;
    const stack: { run: WorkflowRunMapRun; entered: boolean }[] = [{ run: root, entered: false }];
    while (stack.length > 0) {
      const frame = stack[stack.length - 1];
      const run = frame.run;
      if (!frame.entered) {
        frame.entered = true;
        if (visited.has(run.itemId)) { stack.pop(); continue; }
        visited.add(run.itemId);
        const parent = run.parentItemId ? index.runs.get(run.parentItemId) : undefined;
        // A parent whose own ordinal is not settled yet is a cycle the sweep
        // entered mid-ring: start the chain here rather than counting from a
        // number that does not exist.
        const parentOrdinal = parent ? index.ordinals.get(parent.itemId) : undefined;
        const ordinal = parent && parentOrdinal !== undefined && isTailChild(parent, run) ? parentOrdinal + 1 : 1;
        index.ordinals.set(run.itemId, ordinal);
        for (const child of children.get(run.itemId) ?? []) {
          if (!visited.has(child.itemId)) stack.push({ run: child, entered: false });
        }
        continue;
      }
      stack.pop();
      index.subtrees.set(run.itemId, aggregate(index, run, children.get(run.itemId) ?? []));
    }
  }
}

function aggregate(
  index: RunIndex,
  run: WorkflowRunMapRun,
  children: readonly WorkflowRunMapRun[],
): RunMapCompositionSummary {
  let runCount = 1;
  let attemptCount = 0;
  let unitCount = 0;
  let runningCount = 0;
  let parkedCount = 0;
  for (const attempt of run.phases) {
    attemptCount += 1;
    if (attempt.status === 'running') runningCount += 1;
    if (attempt.status === 'parked') parkedCount += 1;
  }
  for (const unit of run.units) {
    if (unit.kind === 'join') continue;
    unitCount += 1;
    if (unit.status === 'running') runningCount += 1;
    if (unit.status === 'taken-over') parkedCount += 1;
  }
  let waveCount = 1;
  for (const child of children) {
    const childSummary = index.subtrees.get(child.itemId);
    if (!childSummary) continue;
    runCount += childSummary.runCount;
    attemptCount += childSummary.attemptCount;
    unitCount += childSummary.unitCount;
    runningCount += childSummary.runningCount;
    parkedCount += childSummary.parkedCount;
    if (isTailChild(run, child)) waveCount += childSummary.waveCount;
  }
  const label = joinParts([
    runCount > 1 ? `${runCount} runs` : '1 run',
    waveCount > 1 ? `${waveCount} waves` : '',
    unitCount > 0 ? `${unitCount} ${unitCount === 1 ? 'unit' : 'units'}` : '',
  ]);
  return { runCount, waveCount, attemptCount, unitCount, runningCount, parkedCount, label };
}

/**
 * The wave chain rooted at `run`: itself plus every tail-self-call descendant,
 * in LAP order.
 *
 * Breadth-first, and that is the whole contract rather than a traversal
 * preference. A retried tail call spawns a SECOND child run, so the edge is not
 * guaranteed single and the chain is a tree, not a list; depth-first walked one
 * retry's grandchild before the other retry, and the ordinals it produced ran
 * 1, 2, 3, 2 — a wave list that goes backwards. Level order keeps them
 * non-decreasing, which is the only property a reader scanning "wave N" down
 * the page can rely on.
 */
export function chainOf(index: RunIndex, run: WorkflowRunMapRun): WorkflowRunMapRun[] {
  const chain: WorkflowRunMapRun[] = [];
  const seen = new Set<string>();
  const pending: WorkflowRunMapRun[] = [run];
  for (let cursor = 0; cursor < pending.length; cursor += 1) {
    const current = pending[cursor]!;
    if (seen.has(current.itemId)) continue;
    seen.add(current.itemId);
    chain.push(current);
    pending.push(...(index.tailChildren.get(current.itemId) ?? []));
  }
  return chain;
}

/**
 * One wave of a chain, with the label coordinate the ordinal alone cannot give.
 *
 * Two sibling waves at one lap (the first tail call failed and was retried) are
 * both "wave 2", and a reader looking at two rows with the same name cannot say
 * which run is which. `lapSeq` numbers them 1, 2, … and stays 0 for the lap
 * that has only one wave — the same rule an attempt's `·N` follows, so the two
 * `·N`s on the surface mean the same kind of thing.
 */
export interface RunMapChainStep {
  run: WorkflowRunMapRun;
  ordinal: number;
  lapSeq: number;
}

/** `chainOf` plus each wave's ordinal and its retry position within that lap. */
export function chainSteps(index: RunIndex, run: WorkflowRunMapRun): RunMapChainStep[] {
  const chain = chainOf(index, run);
  const perLap = new Map<number, number>();
  for (const wave of chain) {
    const ordinal = index.ordinals.get(wave.itemId) ?? 1;
    perLap.set(ordinal, (perLap.get(ordinal) ?? 0) + 1);
  }
  const seen = new Map<number, number>();
  return chain.map((wave, position) => {
    const ordinal = index.ordinals.get(wave.itemId) ?? position + 1;
    const nth = (seen.get(ordinal) ?? 0) + 1;
    seen.set(ordinal, nth);
    return { run: wave, ordinal, lapSeq: (perLap.get(ordinal) ?? 1) > 1 ? nth : 0 };
  });
}

export function orderedPhaseIds(index: RunIndex, run: WorkflowRunMapRun): string[] {
  const recorded = index.recordedOrder.get(run.itemId) ?? [];
  if (run.skeletonMissing || run.skeleton.length === 0) return recorded;
  const declared = run.skeleton.map((phase) => phase.id);
  const known = new Set(declared);
  return [...declared, ...recorded.filter((phaseId) => !known.has(phaseId))];
}

/**
 * One run's subtree counts. The walk fills every run in the view, so the empty
 * default is only ever reached by an id that is not in the view at all —
 * defined here rather than at the call site so a caller cannot invent its own.
 */
export function subtreeOf(index: RunIndex, itemId: string): RunMapCompositionSummary {
  return index.subtrees.get(itemId)
    ?? { runCount: 0, waveCount: 0, attemptCount: 0, unitCount: 0, runningCount: 0, parkedCount: 0, label: '' };
}

export function attemptsOf(index: RunIndex, itemId: string, phaseId: string): WorkflowRunMapPhaseAttempt[] {
  return index.attempts.get(itemId)?.get(phaseId) ?? [];
}

export function unitsOf(index: RunIndex, itemId: string, phaseId: string, attempt: number): WorkflowRunMapUnit[] {
  return index.units.get(compositeKey(itemId, phaseId, attempt)) ?? [];
}

// ---------------------------------------------------------------- summaries

/**
 * A zeroed tally. Defined once because the nine fields are the shape of the
 * type, not of either caller: a literal per counting site is a field somebody
 * forgets to add when the union grows.
 */
export function emptyUnitTotals(): RunMapUnitTotals {
  return {
    total: 0, pending: 0, running: 0, done: 0, failed: 0, dropped: 0, takenOver: 0, unknown: 0, joins: 0,
  };
}

/**
 * Count one unit into `totals`. A join is counted apart and NEVER as a unit —
 * it is the merge, not a branch — which is the rule both the fold's tally and
 * the fan's own have to agree on for their numbers to mean the same thing.
 */
export function countUnit(
  totals: RunMapUnitTotals,
  kind: RunMapUnitStatus['kind'],
  isJoin: boolean,
): void {
  if (isJoin) {
    totals.joins += 1;
    return;
  }
  totals.total += 1;
  if (kind === 'taken-over') totals.takenOver += 1;
  else totals[kind] += 1;
}

function unitTotals(index: RunIndex, run: WorkflowRunMapRun): RunMapUnitTotals {
  const totals = emptyUnitTotals();
  for (const phaseId of orderedPhaseIds(index, run)) {
    const attempts = attemptsOf(index, run.itemId, phaseId);
    if (attempts.length === 0) continue;
    // The latest attempt is what the fan renders, so it is what the fold counts.
    const latest = attempts[attempts.length - 1];
    for (const unit of unitsOf(index, run.itemId, phaseId, latest.attempt)) {
      countUnit(totals, unitStatusOf(unit).kind, unit.kind === 'join');
    }
  }
  return totals;
}

function unitTotalsLabel(totals: RunMapUnitTotals): string {
  if (totals.total === 0) return '';
  return joinParts([
    `${totals.total} ${totals.total === 1 ? 'unit' : 'units'}`,
    totals.failed > 0 ? `${totals.failed} failed` : '',
    totals.dropped > 0 ? `${totals.dropped} dropped` : '',
    totals.takenOver > 0 ? `${totals.takenOver} taken over` : '',
  ]);
}

/**
 * A wave that called the next one LOOPED, whatever its own row now says.
 *
 * "Called the next one" is a fact about THIS run's own tail children, never
 * about its position in the chain: a retried tail call puts two sibling waves
 * in one chain, and the failed first one is followed in the list by a wave it
 * did not produce. Reading position there labelled a dead end "looped".
 */
function waveOutcome(index: RunIndex, run: WorkflowRunMapRun): RunMapWaveOutcome {
  const looped = (index.tailChildren.get(run.itemId) ?? []).length > 0;
  return looped ? { kind: 'looped' } : runStatusOf(run);
}

export function waveSummary(
  index: RunIndex,
  run: WorkflowRunMapRun,
  ordinal: number,
  lapSeq: number,
): RunMapWaveSummary {
  const totals = unitTotals(index, run);
  const outcome = waveOutcome(index, run);
  const reason = run.reason ?? '';
  // ATTEMPTS past the first, summed over the wave — not the number of phases
  // that were retried at all. A phase that took three tries is two retries;
  // counting the bucket reported it as one, so a wave that fought hard read
  // exactly like one that stumbled once.
  let retries = 0;
  for (const bucket of index.attempts.get(run.itemId)?.values() ?? []) retries += bucket.length - 1;
  // The outcome word for a parked wave IS the reason word (`workflowRunSignal`
  // reads the same reason), so carrying both would render "Review gate ·
  // Review gate". The reason part survives only where the outcome says
  // something else — a looped wave that is itself parked, above all.
  const reasonLabel = reason && outcome.kind !== 'needs-human' ? workflowAttentionLabel(reason) : '';
  return {
    label: lapSeq > 0 ? `wave ${ordinal} ·${lapSeq}` : `wave ${ordinal}`,
    duration: indexDuration(index, run.startedAt ?? 0, run.endedAt ?? 0),
    units: totals,
    unitsLabel: unitTotalsLabel(totals),
    outcome,
    outcomeLabel: outcome.kind === 'looped' ? 'Looped' : (workflowRunSignal(run.state, reason).label || run.state),
    reasonLabel,
    retries,
    retriesLabel: retries === 0 ? '' : `${retries} ${retries === 1 ? 'retry' : 'retries'}`,
  };
}

/**
 * The loop foot of one wave (§3). Absent for a workflow that does not
 * tail-self-call — that is the base case, not a special case — and absent for a
 * records-only wave, which has no definition to promise a next lap with.
 */
export function loopFootOf(
  index: RunIndex,
  run: WorkflowRunMapRun,
  treeRoot: WorkflowRunMapRun,
): RunMapLoop | null {
  if (!run.tailSelfCall || run.skeletonMissing) return null;
  const skeleton = run.skeleton;
  const tail = skeleton[skeleton.length - 1];
  if (!tail) return null;
  const attempts = attemptsOf(index, run.itemId, tail.id);
  const hasNextWave = (index.tailChildren.get(run.itemId) ?? []).length > 0;
  const live = isLiveRun(run);
  const decided: RunMapLoop['decided'] = hasNextWave ? 'loop' : run.state === 'done' ? 'done' : null;
  // A terminal run draws no future: no records, nothing decided and nothing
  // still to decide means there is no foot to draw at all.
  if (decided === null && !live && attempts.length === 0) return null;
  const maxDepth = tail.maxDepth ?? 0;
  // `max_depth` bounds EDGE TRAVERSALS, so the root plus that many child waves
  // are all legal (`engine/calls.go#checkCallDepth`). Comparing a lap number
  // against the raw bound rendered the legal final wave as "lap 3 of ≤2".
  const waveCeiling = maxDepth > 0 ? maxDepth + 1 : 0;
  const lapCount = index.ordinals.get(run.itemId) ?? 1;
  // Soft stop lives on the tree ROOT and on the root alone — `engine.setSoftStop`
  // REFUSES a called run, and every wave's call boundary reads the root's row
  // (`engine/soft_stop.go`). Reading the flag off the wave that draws the foot
  // meant the note only ever appeared on a single-wave campaign, which is the
  // one campaign nobody arms a soft stop on.
  const softStopArmed = treeRoot.softStop;
  return {
    key: decisionKeyOf(run.itemId),
    itemId: run.itemId,
    phaseId: tail.id,
    label: phaseLabelOf(tail.id, tail),
    lapCount,
    maxDepth,
    waveCeiling,
    // No `showBudget` beside this: §3 asks for the budget line when the edge
    // declares no depth bound, and the strip renders it whenever a ceiling
    // exists at all — a superset, so a flag deciding it had no reader.
    lapLabel: waveCeiling < 1 ? `lap ${lapCount}` : `lap ${lapCount} of ≤${waveCeiling}`,
    softStopArmed,
    softStopNote: softStopArmed ? 'stops after this wave' : '',
    decided,
    showOutcomeStubs: decided === null && live,
  };
}
