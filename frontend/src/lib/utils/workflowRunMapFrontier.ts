// Frontier extraction (spec §5.4 + §13 priority): the leaves of the whole tree
// that are running or waiting on a person, path-annotated so the strip can name
// where they are and the segments can mark themselves expanded.
//
// The leaf rule is what makes this useful rather than noisy: a running attempt
// is reported only when nothing deeper is running, because the unit or the
// called run below it is what a person would actually open. A parked attempt is
// always reported — it is the thing that needs someone, whatever sits under it.

import { compositeKey } from './compositeKey';
import { workflowAttentionLabel } from './workflowRunSignal';
import type { WorkflowRunMapRun, WorkflowRunMapUnit } from '../types/workflow';
import {
  attemptKeyOf,
  attemptsOf,
  branchKeyOf,
  chainOf,
  compositionKeyOf,
  fanKeyOf,
  indexDuration,
  isLiveRun,
  nodeKeyOf,
  orderedPhaseIds,
  phaseLabelOf,
  phaseStatusOf,
  runStatusOf,
  unitKeyOf,
  unitStatusOf,
  unitsOf,
  waveKey,
  PHASE_SIGNALS,
  RUN_SIGNALS,
  UNIT_SIGNALS,
  type RunIndex,
} from './workflowRunMapIndex';
import type {
  RunMapFrontierBase,
  RunMapFrontierEntry,
  RunMapPathPart,
  RunMapPhaseStatus,
  RunMapSignal,
} from './workflowRunMapTypes';

interface FrontierContext {
  waveItemId: string;
  waveOrdinal: number;
  path: RunMapPathPart[];
  keys: string[];
}

interface FrontierLeaf {
  key: string;
  phaseId: string;
  attempt: number;
  label: string;
  /** The leaf's own breadcrumb part and the keys of every node above it. */
  path: RunMapPathPart[];
  nodeKeys: string[];
  needsHuman: boolean;
  signal: RunMapSignal;
  cause: string;
  startedAt: number;
  endedAt: number;
  threadId: string;
}

/**
 * The half of an entry that is the same for a phase and a unit. Built in one
 * shot from a fully described leaf: a partially initialised entry that callers
 * then patch is how a field ends up carrying the default for the case nobody
 * remembered.
 */
function frontierBase(
  run: WorkflowRunMapRun,
  index: RunIndex,
  context: FrontierContext,
  leaf: FrontierLeaf,
): RunMapFrontierBase {
  return {
    key: leaf.key,
    itemId: run.itemId,
    phaseId: leaf.phaseId,
    attempt: leaf.attempt,
    label: leaf.label,
    waveItemId: context.waveItemId,
    waveOrdinal: context.waveOrdinal,
    depth: leaf.path.length,
    needsHuman: leaf.needsHuman,
    signal: leaf.signal,
    cause: leaf.cause,
    reason: run.reason ?? '',
    reasonLabel: run.reason ? workflowAttentionLabel(run.reason) : '',
    autoResumeAt: run.autoResumeAt ?? 0,
    duration: indexDuration(index, leaf.startedAt, leaf.endedAt),
    threadId: leaf.threadId,
    transitionAt: Math.max(leaf.startedAt, leaf.endedAt),
    path: leaf.path,
    nodeKeys: leaf.nodeKeys,
  };
}

/**
 * One unit's contribution to the frontier: the unit itself when it is a leaf
 * the strip should name, plus everything the runs it called are doing.
 *
 * The early return is the allocation rule, not a micro-optimisation. A 32-wide
 * fan-out is mostly units that are done, queued or dropped and have called
 * nothing; those can yield nothing here, and building a path and a key list for
 * each of them was two array copies per unit per frontier walk — paid on every
 * tick of the shared clock, for rows that never existed.
 */
function collectUnitFrontier(
  index: RunIndex,
  run: WorkflowRunMapRun,
  coordinate: [string, string, number],
  unit: WorkflowRunMapUnit,
  attemptContext: FrontierContext,
  latest: boolean,
): RunMapFrontierEntry[] {
  const [itemId, phaseId, attempt] = coordinate;
  const children = index.childrenByUnit.get(compositeKey(...coordinate, unit.unitId)) ?? [];
  const status = unitStatusOf(unit);
  // A running unit reports only when nothing deeper is; a taken-over one always
  // does, so it can report without waiting on the children walk below.
  const reports = latest && (status.kind === 'running' || status.kind === 'taken-over');
  if (children.length === 0 && !reports) return [];

  const unitKey = unitKeyOf(itemId, phaseId, attempt, unit.unitId);
  const unitContext: FrontierContext = {
    ...attemptContext,
    path: [...attemptContext.path, { kind: 'unit', label: unit.unitId, key: unitKey }],
    keys: [...attemptContext.keys, fanKeyOf(itemId, phaseId, attempt),
      branchKeyOf(itemId, phaseId, attempt, unit.unitId), unitKey],
  };
  const nested: RunMapFrontierEntry[] = [];
  for (const child of children) {
    nested.push(...collectCompositionFrontier(index, child, unitContext));
  }
  if (!reports || (status.kind === 'running' && nested.length > 0)) return nested;
  const base = frontierBase(run, index, unitContext, {
    key: unitKey,
    phaseId,
    attempt,
    label: unit.unitId,
    path: unitContext.path,
    nodeKeys: unitContext.keys,
    needsHuman: status.kind === 'taken-over',
    signal: UNIT_SIGNALS[status.kind],
    cause: '',
    startedAt: unit.startedAt ?? 0,
    endedAt: unit.endedAt ?? 0,
    threadId: unit.threadId ?? '',
  });
  return [{ ...base, kind: 'unit', status, unitId: unit.unitId }, ...nested];
}

/**
 * Frontier leaves of one run and everything called below it (§5.4). A running
 * attempt is a leaf only when nothing deeper is running: the unit or the child
 * run is what the human would follow. A parked attempt is always emitted —
 * it is the thing that needs a person, whatever sits under it.
 */
function collectRunFrontier(
  index: RunIndex,
  run: WorkflowRunMapRun,
  context: FrontierContext,
): RunMapFrontierEntry[] {
  const entries: RunMapFrontierEntry[] = [];
  const skeleton = index.skeletonById.get(run.itemId);
  for (const phaseId of orderedPhaseIds(index, run)) {
    const label = phaseLabelOf(phaseId, skeleton?.get(phaseId));
    const phaseKey = nodeKeyOf(run.itemId, phaseId);
    const records = attemptsOf(index, run.itemId, phaseId);
    for (const attempt of records) {
      // Only the phase's latest attempt can BE the frontier: an earlier attempt
      // that parked or was still marked running when a retry superseded it is
      // history, and reporting it would put a resolved park back on the strip.
      const latest = attempt === records[records.length - 1];
      const attemptKey = attemptKeyOf(run.itemId, phaseId, attempt.attempt);
      const coordinate: [string, string, number] = [run.itemId, phaseId, attempt.attempt];
      const below: RunMapFrontierEntry[] = [];
      const attemptContext: FrontierContext = {
        ...context,
        path: [...context.path, { kind: 'phase', label, key: phaseKey }],
        keys: [...context.keys, phaseKey, attemptKey],
      };
      for (const unit of unitsOf(index, run.itemId, phaseId, attempt.attempt)) {
        below.push(...collectUnitFrontier(index, run, coordinate, unit, attemptContext, latest));
      }
      for (const child of index.childrenByAttempt.get(compositeKey(...coordinate)) ?? []) {
        below.push(...collectCompositionFrontier(index, child, attemptContext));
      }
      const status = phaseStatusOf(attempt);
      if (latest && (status.kind === 'parked' || (status.kind === 'running' && below.length === 0))) {
        const base = frontierBase(run, index, context, {
          key: attemptKey,
          phaseId,
          attempt: attempt.attempt,
          label,
          path: attemptContext.path,
          nodeKeys: attemptContext.keys,
          needsHuman: status.kind === 'parked',
          signal: PHASE_SIGNALS[status.kind],
          cause: status.kind === 'parked' ? status.cause : '',
          startedAt: attempt.startedAt ?? 0,
          endedAt: attempt.endedAt ?? 0,
          threadId: attempt.threadId ?? '',
        });
        entries.push({ ...base, kind: 'phase', status });
      }
      entries.push(...below);
    }
  }
  return entries;
}

/** A called run is its own wave chain; its waves are visited in chain order. */
function collectCompositionFrontier(
  index: RunIndex,
  root: WorkflowRunMapRun,
  context: FrontierContext,
): RunMapFrontierEntry[] {
  const key = compositionKeyOf(root.itemId);
  const chain = chainOf(index, root);
  const entries: RunMapFrontierEntry[] = [];
  for (const run of chain) {
    const label = chain.length > 1 ? `${root.workflowId} · wave ${index.ordinals.get(run.itemId) ?? 1}` : root.workflowId;
    entries.push(...collectRunFrontier(index, run, {
      ...context,
      path: [...context.path, { kind: 'call', label, key }],
      keys: [...context.keys, key, waveKey(run.itemId)],
    }));
  }
  return entries;
}

function sortFrontier(entries: RunMapFrontierEntry[]): RunMapFrontierEntry[] {
  return entries.sort((left, right) =>
    Number(right.needsHuman) - Number(left.needsHuman)
    || right.depth - left.depth
    || right.transitionAt - left.transitionAt
    || left.key.localeCompare(right.key));
}

/**
 * Where a live run is when it has recorded NOTHING yet (§7, "run just created,
 * zero attempts"): its segment top.
 *
 * There is no leaf to report — the segment is all ghosts — but "no frontier" and
 * "no follow target" are not the same statement, and conflating them cost the
 * visit its follow: `openedRunning` is decided once, at open, from the target,
 * so a run opened in the half-second before its first attempt landed followed
 * nothing for the rest of the visit however far it then advanced.
 *
 * The entry is keyed on the WAVE, which is what makes it the segment top rather
 * than a guess at which ghost is next: no node key matches it, so the follow
 * controller resolves it to the wave's own row.
 */
function openingLeaf(index: RunIndex, waves: readonly WorkflowRunMapRun[]): RunMapFrontierEntry | null {
  let live: WorkflowRunMapRun | null = null;
  for (const run of waves) if (isLiveRun(run)) live = run;
  if (!live) return null;
  const ordinal = index.ordinals.get(live.itemId) ?? 1;
  const label = `wave ${ordinal}`;
  const key = waveKey(live.itemId);
  const runStatus = runStatusOf(live);
  const needsHuman = runStatus.kind === 'needs-human';
  const base = frontierBase(live, index, {
    waveItemId: live.itemId, waveOrdinal: ordinal, path: [], keys: [],
  }, {
    key,
    // No attempt exists, and 0 is the ordinal no record can have — the same
    // convention `ghostAttempt` uses for the row a ghost node draws.
    phaseId: '',
    attempt: 0,
    label,
    path: [{ kind: 'wave', label, key }],
    nodeKeys: [key],
    needsHuman,
    signal: RUN_SIGNALS[runStatus.kind],
    cause: '',
    startedAt: live.startedAt ?? 0,
    endedAt: live.endedAt ?? 0,
    threadId: '',
  });
  // A run parked before it ran anything (a setup failure, a pre-flight gate) is
  // parked, and the strip's blocker chip reads the run's own reason off `base`.
  const status: RunMapPhaseStatus = needsHuman ? { kind: 'parked', cause: '' } : { kind: 'ghost' };
  return { ...base, kind: 'phase', status };
}

export function collectFrontier(index: RunIndex, waves: readonly WorkflowRunMapRun[]): RunMapFrontierEntry[] {
  const entries: RunMapFrontierEntry[] = [];
  for (const run of waves) {
    const ordinal = index.ordinals.get(run.itemId) ?? 1;
    const label = `wave ${ordinal}`;
    const key = waveKey(run.itemId);
    entries.push(...collectRunFrontier(index, run, {
      waveItemId: run.itemId,
      waveOrdinal: ordinal,
      path: waves.length > 1 ? [{ kind: 'wave', label, key }] : [],
      keys: [key],
    }));
  }
  if (entries.length > 0) return sortFrontier(entries);
  const opening = openingLeaf(index, waves);
  return opening === null ? [] : [opening];
}

export function frontierKeySet(entries: readonly RunMapFrontierEntry[]): Set<string> {
  const keys = new Set<string>();
  for (const entry of entries) for (const key of entry.nodeKeys) keys.add(key);
  return keys;
}
