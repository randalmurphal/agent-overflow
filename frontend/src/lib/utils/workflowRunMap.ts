// Run-map projection (spec: docs/specs/workflows-system-ui/RUN-MAP.md §5) — the
// public entry points, and the node builders behind them.
//
// A run tree arrives as one flat, parent-linked list of runs, each carrying its
// OWN frozen skeleton plus its recorded attempts and units. The map is the
// projection that answers three questions the records alone cannot: which calls
// are waves of one campaign and which are composition (§3), what has not
// happened yet (skeleton ∪ records, §5.1), and where the run actually IS
// (the frontier, §5.4).
//
// ONE walk per call, and laziness is about WIDTH, not about entry points.
// `buildRunMap` builds the index and collects the frontier exactly once and
// then fills in `segments` only for the waves the caller named plus the live
// one, so a 256-lap campaign still costs O(1) per folded wave — but a caller
// that also wants a wave's nodes does not pay for a second index and a second
// frontier to get them. A per-wave builder made both of those per-wave, and
// the surface derives once a second off the shared clock.
//
// The narrow read is `runMapPosition`, for the header's one label. It is the
// only entry point that answers less than the whole model, and it is deliberate:
// it does not build segments at all.
//
// Pure on purpose: no Svelte imports, no ambient clock. `nowMs` is always an
// explicit parameter (the shared 1Hz clock threads it in), which is what makes
// every rule here exhaustively table-testable.

import { compositeKey } from './compositeKey';
import { formatTokens, formatUsd, workflowSpanMs } from './format';
import type {
  WorkflowAgentRunBudget,
  WorkflowRunMapPhaseAttempt,
  WorkflowRunMapRun,
  WorkflowRunMapSkeletonPhase,
  WorkflowRunMapUnit,
  WorkflowRunMapView,
  WorkflowRunSpend,
} from '../types/workflow';
import { collectFrontier, frontierKeySet } from './workflowRunMapFrontier';
import {
  attemptKeyOf,
  attemptsOf,
  blockerLabelOf,
  branchKeyOf,
  buildIndex,
  chainOf,
  chainSteps,
  compositionKeyOf,
  fanKeyOf,
  groupKeyOf,
  indexDuration,
  isLiveRun,
  joinParts,
  lastSkeletonPhaseId,
  loopFootOf,
  nodeKeyOf,
  orderedPhaseIds,
  phaseLabelOf,
  phaseStatusOf,
  runStatusOf,
  subtreeOf,
  unitKeyOf,
  unitRangeLabel,
  unitStatusOf,
  unitsOf,
  waveIsSettled,
  waveKey,
  waveSignalOf,
  waveSummary,
  PHASE_SIGNALS,
  RUN_SIGNALS,
  UNIT_SIGNALS,
  type RunIndex,
} from './workflowRunMapIndex';
import type {
  RunMapAttempt,
  RunMapBranch,
  RunMapBudget,
  RunMapBuildOptions,
  RunMapCompositionNode,
  RunMapCompositionWave,
  RunMapFan,
  RunMapModel,
  RunMapNodeBase,
  RunMapPhaseStatus,
  RunMapPosition,
  RunMapRefusal,
  RunMapSegmentNode,
  RunMapSpend,
  RunMapUnitChip,
  RunMapUnitStatus,
  RunMapWave,
} from './workflowRunMapTypes';

export * from './workflowRunMapTypes';
export { runMapTone } from './workflowRunMapIndex';

// ---------------------------------------------------------------- build

export function buildRunMap(
  view: WorkflowRunMapView,
  nowMs: number,
  options: RunMapBuildOptions = {},
): RunMapModel {
  const refusal = refusalOf(view);
  const index = buildIndex(view, nowMs);
  const root = index.runs.get(view.rootItemId) ?? view.runs[0];
  if (!root) {
    return {
      rootItemId: view.rootItemId, waves: [], loop: null,
      frontier: [], followTarget: null, refusal,
      spend: spendOf(undefined), budget: null, moneyLabel: '', budgetLabel: '',
    };
  }
  const steps = chainSteps(index, root);
  const chain = steps.map((step) => step.run);
  const frontier = collectFrontier(index, chain);
  const keys = frontierKeySet(frontier);
  const expandedWaves = new Set(options.expandedWaveIds ?? []);
  const context: SegmentContext = {
    frontierKeys: keys,
    expandedWaves,
    expandedCompositions: new Set(options.expandedCompositionIds ?? []),
    expandedLanes: new Set(options.expandedLaneIds ?? []),
    depth: 0,
    treeRoot: root,
  };
  const waves = steps.map(({ run, ordinal, lapSeq }) => {
    const status = runStatusOf(run);
    // A live wave is expanded in place and has nothing to fold back into, so
    // its segments are never optional; a folded one is built only when the
    // reader has opened it. "Live" here means the run has not yet HANDED OFF —
    // a lap that called the next one stays `running` while its child works, and
    // folding on run state alone left every ancestor lap fully expanded.
    const folded = waveIsSettled(index, run);
    return {
      key: waveKey(run.itemId),
      itemId: run.itemId,
      workflowId: run.workflowId,
      ordinal,
      lapSeq,
      recordsOnly: run.skeletonMissing,
      skeletonError: run.skeletonError ?? '',
      folded,
      status,
      signal: waveSignalOf(index, run),
      summary: waveSummary(index, run, ordinal, lapSeq),
      segments: !folded || expandedWaves.has(run.itemId) ? runSegments(index, run, context) : null,
      startedAt: run.startedAt ?? 0,
      endedAt: run.endedAt ?? 0,
      autoResumeAt: run.autoResumeAt ?? 0,
      softStop: run.softStop,
      onFrontierPath: keys.has(waveKey(run.itemId)),
    } satisfies RunMapWave;
  });
  const spend = spendOf(root.spend);
  const budget = budgetOf(root.budget);
  return {
    rootItemId: root.itemId,
    waves,
    loop: loopFootOf(index, loopWave(chain), root),
    frontier,
    followTarget: frontier[0] ?? null,
    refusal,
    spend,
    budget,
    moneyLabel: moneyLabelOf(spend, budget),
    budgetLabel: budgetLabelOf(budget),
  };
}

/**
 * Which wave the strip's loop foot describes: the DEEPEST LIVE one, falling
 * back to the last of the chain when nothing is live.
 *
 * The chain is level order, so its tail is the deepest wave — but a lap can
 * hold two waves (a retried tail call, §7), and the deepest lap's tail is
 * whichever of those the BFS happened to reach last. Taking it outright made
 * the strip describe a dead-end sibling — "lap 3, done" off a wave that failed
 * — while the live wave beside it was the one the run is actually in.
 */
function loopWave(chain: readonly WorkflowRunMapRun[]): WorkflowRunMapRun {
  for (let position = chain.length - 1; position >= 0; position -= 1) {
    const wave = chain[position]!;
    if (isLiveRun(wave)) return wave;
  }
  return chain[chain.length - 1]!;
}

/**
 * Is this wave's body on screen? The projection ALREADY decided, when it chose
 * whether to walk the wave's nodes (§6, vertical scale): `segments === null`
 * means "not built", and a wave whose nodes were not built has nothing to show.
 *
 * It is a function rather than a field so there is exactly one place the
 * question is answered. A separate `open` flag — on the model or as a component
 * prop — is a second answer, and the surface only stays honest while the two
 * agree: open with `segments: null` renders "Nothing recorded in this wave yet."
 * over a wave full of records.
 */
export function runMapWaveIsOpen(wave: Pick<RunMapWave, 'segments'>): boolean {
  return wave.segments !== null;
}

function refusalOf(view: WorkflowRunMapView): RunMapRefusal | null {
  const refusal = view.refusal;
  if (!refusal || !refusal.code) return null;
  return { code: refusal.code, message: refusal.message };
}

/**
 * The tree's dollars, said honestly. The wire keeps the halves apart precisely
 * so a surface can distinguish "this is what it cost" from "this is what we
 * could price" — a Codex phase reports tokens and no cost at all, and rows
 * whose model has no rate are priced by nothing.
 */
function spendOf(spend: WorkflowRunSpend | null | undefined): RunMapSpend {
  const totalUsd = spend?.costUsd ?? 0;
  const wireUsd = spend?.wireCostUsd ?? 0;
  const estimatedUsd = spend?.estimatedCostUsd ?? 0;
  const unpricedRows = spend?.unpricedRows ?? 0;
  return {
    totalUsd,
    wireUsd,
    estimatedUsd,
    unpricedRows,
    estimated: estimatedUsd > 0,
    label: totalUsd > 0 ? formatUsd(totalUsd) : '',
    unpricedLabel: unpricedRows > 0
      ? `${unpricedRows} ${unpricedRows === 1 ? 'row' : 'rows'} unpriced`
      : '',
  };
}

/**
 * The ceiling in force, ALL THREE pairs carried across. Reading only the dollar
 * pair meant a run bounded by tokens or by wall-clock arrived here with its
 * bound already discarded, and the strip could say nothing about the one thing
 * that was going to stop the run.
 */
function budgetOf(budget: WorkflowAgentRunBudget | null | undefined): RunMapBudget | null {
  if (!budget) return null;
  return {
    kind: budget.kind,
    ceilingUsd: budget.ceilingUsd ?? 0,
    spentUsd: budget.spentUsd ?? 0,
    ceilingTokens: budget.ceilingTokens ?? 0,
    spentTokens: budget.spentTokens ?? 0,
    ceilingMillis: budget.ceilingMillis ?? 0,
    elapsedMillis: budget.elapsedMillis ?? 0,
    percent: budget.percent,
    exhausted: budget.exhausted === true,
    unpricedRows: budget.unpricedRows ?? 0,
    estimated: budget.estimated === true,
    rootItemId: budget.rootItemId ?? '',
  };
}

/**
 * The ceiling said in ITS OWN units, for the two kinds the money line cannot
 * speak for. Dollars are `moneyLabelOf`'s — it already renders `$4.12 of
 * $10.00` — so this answers `''` for them rather than repeating the comparison
 * beside itself.
 *
 * A kind this build does not know answers `''` too: there is no pair to read,
 * and inventing one from the dollar fields would compare a number against a
 * ceiling that is not in that unit — the exact mistake the `usd`-only money
 * line already refuses to make.
 */
function budgetLabelOf(budget: RunMapBudget | null): string {
  if (!budget) return '';
  if (budget.kind === 'tokens') {
    if (budget.ceilingTokens <= 0) return '';
    return `${formatTokens(budget.spentTokens)} of ${formatTokens(budget.ceilingTokens)} tokens`;
  }
  if (budget.kind === 'wall_clock') {
    if (budget.ceilingMillis <= 0) return '';
    return `${workflowSpanMs(budget.elapsedMillis)} of ${workflowSpanMs(budget.ceilingMillis)}`;
  }
  return '';
}

/**
 * The one-line money summary, and the ONE place the lower-bound distinction is
 * worded. Three statements, in the order a reader needs them:
 *
 *   - `$4.12 of $10.00` — there IS a dollar ceiling, so the total is a share.
 *   - `$4.12 priced` — some rows could not be priced, so the number is a floor
 *     and the `unpriced` part beside it says why.
 *   - `$4.12 spent` — nothing is missing and there is no ceiling to compare to.
 *
 * A token or wall-clock ceiling contributes nothing: the map has no tokens or
 * elapsed to put beside it, and a dollar figure "of" a token bound is a
 * comparison that does not exist.
 */
function moneyLabelOf(spend: RunMapSpend, budget: RunMapBudget | null): string {
  const ceilingUsd = budget && budget.kind === 'usd' ? budget.ceilingUsd : 0;
  if (spend.label === '' && ceilingUsd <= 0) return '';
  const amount = spend.label || formatUsd(0);
  if (ceilingUsd > 0) return joinParts([`${amount} of ${formatUsd(ceilingUsd)}`, spend.unpricedLabel]);
  if (spend.unpricedLabel !== '') return joinParts([`${amount} priced`, spend.unpricedLabel]);
  return `${amount} spent`;
}

/**
 * The row a node with no attempts renders as (§2, the ghost). A node draws one
 * row per attempt, and a ghost has none — but a ghost is a REAL element from
 * first render (§10: a status change is a class swap, never a DOM insertion),
 * so it needs a row of the same shape rather than a parallel one. The node
 * carries every field an attempt would have supplied for it: nothing happened,
 * so there is no duration, no cause, no thread and no fan.
 *
 * `attempt: 0` is the honest ordinal — attempts are 1-based, and 0 is the
 * value no record can have.
 */
export function ghostAttempt(node: RunMapSegmentNode): RunMapAttempt {
  return {
    key: node.key,
    phaseId: node.phaseId,
    attempt: 0,
    label: node.kind === 'fan' ? node.ghostLabel : node.label,
    status: node.status,
    signal: node.signal,
    duration: '',
    cause: '',
    touched: false,
    interventionKind: '',
    threadId: '',
    startedAt: 0,
    endedAt: 0,
    fan: null,
    chain: [],
    onFrontierPath: node.onFrontierPath,
  };
}

/**
 * Where the run IS, as the two label parts the run header shows (§11.4) — and
 * NOTHING else. The header sits above the map and needs one short label, so it
 * reads this instead of building a model it would throw away: no segments, no
 * summaries, no loop foot.
 *
 * `nowMs` is deliberately absent, and the index is built with NO clock rather
 * than with a zero one. A position label reads no duration, so there is nothing
 * to measure — and a zero clock would have every open span on the path
 * formatted as decades of elapsed time, correct only because nothing here
 * happens to read it. `null` makes that unreadable value unbuildable instead.
 */
export function runMapPosition(view: WorkflowRunMapView): RunMapPosition | null {
  const index = buildIndex(view, null);
  const root = index.runs.get(view.rootItemId) ?? view.runs[0];
  if (!root) return null;
  const target = collectFrontier(index, chainOf(index, root))[0];
  if (!target) return null;
  const wave = target.path.find((part) => part.kind === 'wave');
  const leaf = target.path[target.path.length - 1];
  return {
    wave: wave?.label ?? '',
    // A leaf that IS the wave part would render the wave label twice.
    leaf: wave === leaf ? '' : leaf?.label ?? '',
  };
}

/** An open span: started and not yet ended, so its rendered length ticks. */
function isOpenSpan(startedAt: number | undefined, endedAt: number | undefined): boolean {
  return (startedAt ?? 0) > 0 && (endedAt ?? 0) === 0;
}

/**
 * Does anything in this tree still move? The shared 1Hz clock gates on THIS and
 * never on the model: the model is built FROM the clock, so a predicate that
 * read it would re-arm the ticker on every tick.
 *
 * The answer is about CLOCK-DEPENDENT VALUES, not about run state, and the two
 * are not the same question. Every park writes an `ended_at` — on the run
 * (`engine/fsm.go#transition` stamps one for every non-running state), on the
 * attempt it tore down, and on each unit it settled — so a tree waiting on a
 * human has no open span anywhere and nothing on screen changes second to
 * second. Gating on `needs-human` rebuilt the whole model once a second, for
 * hours, while a person read a stationary page. What genuinely ticks is an open
 * duration at any level, and an `auto_resume_at` countdown chip.
 */
export function runMapViewIsLive(view: WorkflowRunMapView): boolean {
  for (const run of view.runs) {
    if ((run.autoResumeAt ?? 0) > 0) return true;
    if (isOpenSpan(run.startedAt, run.endedAt)) return true;
    for (const attempt of run.phases) if (isOpenSpan(attempt.startedAt, attempt.endedAt)) return true;
    for (const unit of run.units) if (isOpenSpan(unit.startedAt, unit.endedAt)) return true;
  }
  return false;
}

interface SegmentContext {
  frontierKeys: Set<string>;
  /** Wave item ids the reader opened — top-level laps and composition laps alike. */
  expandedWaves: Set<string>;
  /** Called-run ids the reader opened; everything off the frontier is folded by default. */
  expandedCompositions: Set<string>;
  /** Branch keys of settled fan lanes the reader opened. */
  expandedLanes: Set<string>;
  /** Composition levels below the owning wave; 0 while inside the wave itself. */
  depth: number;
  /**
   * The tree's root run. Soft stop is armed on it and on it alone
   * (`engine/soft_stop.go` refuses a called run), and every loop foot anywhere
   * in the tree draws the note from that one row.
   */
  treeRoot: WorkflowRunMapRun;
}

function runSegments(
  index: RunIndex,
  run: WorkflowRunMapRun,
  context: SegmentContext,
): RunMapSegmentNode[] {
  const skeleton = index.skeletonById.get(run.itemId);
  const declared = run.skeletonMissing ? new Set<string>() : new Set(run.skeleton.map((phase) => phase.id));
  const tailPhaseId = run.tailSelfCall && !run.skeletonMissing ? lastSkeletonPhaseId(run) : '';
  const live = isLiveRun(run);
  const ordered = orderedPhaseIds(index, run);
  // How far the run got, in DECLARED order. Everything past it is the future;
  // an unrecorded phase before it was skipped and never will run.
  //
  // Only a DECLARED phase moves it. `ordered` appends the run's orphan records
  // — phases a rerun's definition no longer declares (§5.1) — after the whole
  // skeleton, so a single orphan sat past every declared position and marked
  // every phase the LIVE run had not reached yet as skipped: a running campaign
  // rendered its entire future struck through.
  let furthest = -1;
  ordered.forEach((phaseId, position) => {
    if (!declared.has(phaseId)) return;
    if (attemptsOf(index, run.itemId, phaseId).length > 0) furthest = position;
  });
  const nodes: RunMapSegmentNode[] = [];
  for (const [position, phaseId] of ordered.entries()) {
    // The terminal tail-self-call phase is the loop foot, not a phase node (§3).
    if (phaseId === tailPhaseId) continue;
    const records = attemptsOf(index, run.itemId, phaseId);
    const skipped = records.length === 0 && position < furthest;
    // A recordless phase past the furthest point is the future, and a terminal
    // run has none (§5.6). A skipped one is the past, so it is always drawn.
    if (records.length === 0 && !skipped && !live) continue;
    const definitionPhase = skeleton?.get(phaseId);
    const base = nodeBase(index, run, phaseId, records, definitionPhase,
      declared.size > 0 && !declared.has(phaseId), skipped, context);
    nodes.push(shapedNode(base, definitionPhase));
  }
  if (tailPhaseId) {
    const loop = loopFootOf(index, run, context.treeRoot);
    if (loop) {
      const tail = run.skeleton[run.skeleton.length - 1];
      const base = nodeBase(index, run, tailPhaseId,
        attemptsOf(index, run.itemId, tailPhaseId), tail, false, false, context);
      nodes.push({ ...base, kind: 'decision', loop });
    }
  }
  return nodes;
}

function nodeBase(
  index: RunIndex,
  run: WorkflowRunMapRun,
  phaseId: string,
  records: readonly WorkflowRunMapPhaseAttempt[],
  skeleton: WorkflowRunMapSkeletonPhase | undefined,
  notInDefinition: boolean,
  skipped: boolean,
  context: SegmentContext,
): RunMapNodeBase {
  const label = phaseLabelOf(phaseId, skeleton);
  const key = nodeKeyOf(run.itemId, phaseId);
  const attempts = records.map((record) =>
    attemptNode(index, run, record, label, records.length > 1, context));
  const ghost = attempts.length === 0;
  const status: RunMapPhaseStatus = ghost ? { kind: 'ghost' } : attempts[attempts.length - 1].status;
  const signal = PHASE_SIGNALS[status.kind];
  return {
    key,
    itemId: run.itemId,
    phaseId,
    label,
    shape: skeleton?.shape ?? '',
    isCheck: skeleton?.isCheck === true,
    ghost,
    skipped,
    notInDefinition,
    status,
    signal,
    attempts,
    onFrontierPath: context.frontierKeys.has(key),
  };
}

/**
 * The node kind a phase renders as. Shape comes from the definition when there
 * is one; records-only mode (§5.8) has no shape, so what the records contain —
 * units, called runs — is what decides.
 */
function shapedNode(
  base: RunMapNodeBase,
  skeleton: WorkflowRunMapSkeletonPhase | undefined,
): RunMapSegmentNode {
  const hasUnits = base.attempts.some((attempt) => attempt.fan !== null);
  const hasChain = base.attempts.some((attempt) => attempt.chain.length > 0);
  if (base.shape === 'fan-out' || hasUnits) {
    // The skeleton carries shape, never a fan-out WIDTH, so a pre-expansion
    // ghost states where the units come from and never guesses how many.
    return { ...base, kind: 'fan', ghostLabel: `units — declared by ${base.label}` };
  }
  if (base.shape === 'call' || hasChain) {
    return { ...base, kind: 'call', callTarget: skeleton?.callTarget ?? '' };
  }
  return { ...base, kind: 'phase' };
}

function attemptNode(
  index: RunIndex,
  run: WorkflowRunMapRun,
  record: WorkflowRunMapPhaseAttempt,
  phaseLabel: string,
  retried: boolean,
  context: SegmentContext,
): RunMapAttempt {
  const status = phaseStatusOf(record);
  const signal = PHASE_SIGNALS[status.kind];
  const key = attemptKeyOf(run.itemId, record.phaseId, record.attempt);
  const units = unitsOf(index, run.itemId, record.phaseId, record.attempt);
  const chain = (index.childrenByAttempt.get(compositeKey(run.itemId, record.phaseId, record.attempt)) ?? [])
    .map((child) => compositionNode(index, child, context));
  return {
    key,
    phaseId: record.phaseId,
    attempt: record.attempt,
    label: retried ? `${phaseLabel} ·${record.attempt}` : phaseLabel,
    status,
    signal,
    duration: indexDuration(index, record.startedAt, record.endedAt ?? 0),
    cause: record.cause ?? '',
    touched: (record.interventionKind ?? '') !== '',
    interventionKind: record.interventionKind ?? '',
    threadId: record.threadId ?? '',
    startedAt: record.startedAt ?? 0,
    endedAt: record.endedAt ?? 0,
    fan: units.length > 0 ? fanNode(index, run, record, units, phaseLabel, context) : null,
    chain,
    onFrontierPath: context.frontierKeys.has(key),
  };
}

function unitChip(
  index: RunIndex,
  run: WorkflowRunMapRun,
  record: WorkflowRunMapPhaseAttempt,
  unit: WorkflowRunMapUnit,
  childRunCount: number,
  context: SegmentContext,
): RunMapUnitChip {
  const status = unitStatusOf(unit);
  const signal = UNIT_SIGNALS[status.kind];
  const key = unitKeyOf(run.itemId, record.phaseId, record.attempt, unit.unitId);
  const duration = indexDuration(index, unit.startedAt ?? 0, unit.endedAt ?? 0);
  return {
    key,
    unitId: unit.unitId,
    unitIndex: unit.unitIndex,
    label: unit.unitId,
    isJoin: unit.kind === 'join',
    status,
    signal,
    provider: unit.provider ?? '',
    duration,
    meta: joinParts([
      status.kind === 'pending' ? 'queued' : '',
      // A status this build has no word for is stated, never relabelled: the
      // reader is told the engine said something we cannot name, which is the
      // truth, rather than being shown a queue this unit is not in.
      status.kind === 'unknown' ? `status ${status.raw}` : '',
      status.kind === 'dropped' ? 'dropped — join proceeded without it' : '',
      status.kind === 'taken-over' ? 'taken over' : '',
      unit.unitAttempt > 1 ? `×${unit.unitAttempt}` : '',
      duration,
    ]),
    struck: status.kind === 'dropped',
    unitAttempt: unit.unitAttempt,
    threadId: unit.threadId ?? '',
    childRunCount,
    startedAt: unit.startedAt ?? 0,
    endedAt: unit.endedAt ?? 0,
    onFrontierPath: context.frontierKeys.has(key),
  };
}

/** Lanes that are still ACTIONABLE: a reader can do something about them now. */
const COLUMN_STATUSES = new Set<RunMapUnitStatus['kind']>(['running', 'failed', 'taken-over', 'unknown']);

function fanNode(
  index: RunIndex,
  run: WorkflowRunMapRun,
  record: WorkflowRunMapPhaseAttempt,
  units: readonly WorkflowRunMapUnit[],
  phaseLabel: string,
  context: SegmentContext,
): RunMapFan {
  const coordinate: [string, string, number] = [run.itemId, record.phaseId, record.attempt];
  const columns: RunMapBranch[] = [];
  const queued: RunMapUnitChip[] = [];
  const done: RunMapUnitChip[] = [];
  let join: RunMapUnitChip | null = null;
  let droppedCount = 0;
  for (const unit of units) {
    const children = index.childrenByUnit.get(compositeKey(...coordinate, unit.unitId)) ?? [];
    const chip = unitChip(index, run, record, unit, children.length, context);
    const isJoin = unit.kind === 'join';
    if (isJoin) {
      join = chip;
      continue;
    }
    const kind = chip.status.kind;
    // Columns are reserved for branches with STRUCTURE or actionability (§6);
    // everything scalar becomes arithmetic in a group node.
    //
    // A unit that CALLED something has structure whatever its own status now
    // says, and the group nodes render a label and nothing else. Routing a
    // finished call-bound unit into the done group therefore deleted the child
    // run and its whole composition subtree from the map — so it keeps its
    // lane. What it does NOT keep is the painted subtree: a SETTLED lane folds
    // to its header, which already carries the glyph, the id and the duration,
    // and one click puts the chain back (§7).
    const actionable = COLUMN_STATUSES.has(kind) || chip.onFrontierPath;
    if (actionable || children.length > 0) {
      const branchKey = branchKeyOf(run.itemId, record.phaseId, record.attempt, unit.unitId);
      const toggleable = !actionable && children.length > 0;
      const collapsed = toggleable && !context.expandedLanes.has(branchKey);
      columns.push({
        key: branchKey,
        unit: chip,
        chain: collapsed ? [] : children.map((child) => compositionNode(index, child, context)),
        collapsed,
        toggleable,
        onFrontierPath: chip.onFrontierPath || context.frontierKeys.has(branchKey),
      });
      continue;
    }
    if (kind === 'pending') queued.push(chip);
    else {
      if (kind === 'dropped') droppedCount += 1;
      done.push(chip);
    }
  }
  const doneCount = done.length - droppedCount;
  const queuedRange = unitRangeLabel(queued.map((chip) => chip.unitIndex), phaseLabel);
  return {
    key: fanKeyOf(run.itemId, record.phaseId, record.attempt),
    attempt: record.attempt,
    columns,
    queued: {
      kind: 'queued',
      key: groupKeyOf(run.itemId, record.phaseId, record.attempt, 'queued'),
      count: queued.length,
      droppedCount: 0,
      label: joinParts([
        queuedRange || `${queued.length} ${queued.length === 1 ? 'unit' : 'units'}`,
        'queued',
      ]),
      // Nothing a click would add: a queued lane has no record, no thread and
      // no duration, so its chip would repeat the label beside it.
      entries: [],
    },
    done: {
      kind: 'done',
      key: groupKeyOf(run.itemId, record.phaseId, record.attempt, 'done'),
      count: doneCount,
      droppedCount,
      label: `done ·${doneCount}`,
      entries: done,
    },
    join,
  };
}

/**
 * One called run as a chain inside its caller's node or branch (§3), and the
 * one place the map's reading rule turns into structure: **only the live path
 * is open.**
 *
 * Every composition OFF the frontier path collapses to a single summary row —
 * glyph, workflow, duration, subtree counts — at every depth, and the reader's
 * `expandedCompositionIds` is what opens one. A depth rule used to decide this
 * instead (two levels free, deeper folded), which meant a campaign's three fan
 * lanes each painted a child workflow's whole adjudication history the moment
 * it started: sixty rows of settled work with the live step somewhere inside
 * them. Depth is not the question. Whether this call is where the run IS, is.
 *
 * Even an OPEN composition shows only its live lap: its finished laps fold to
 * the same compact wave rows a top-level lap folds to, off the same
 * `expandedWaveIds` set, because a lap is a lap wherever it sits.
 *
 * Nothing bounds the painted depth any more because nothing needs to: the
 * frontier is a PATH, so force-opening it costs O(depth) rows, and everything
 * beside it is one row until someone asks. The old bound existed to stop a
 * pathological definition painting unboundedly, which the default now does.
 */
function compositionNode(
  index: RunIndex,
  root: WorkflowRunMapRun,
  context: SegmentContext,
): RunMapCompositionNode {
  const depth = context.depth + 1;
  const key = compositionKeyOf(root.itemId);
  const onFrontierPath = context.frontierKeys.has(key);
  // The frontier path is force-open, so there is nothing there to fold; every
  // other composition owns a collapse the reader can work.
  const toggleable = !onFrontierPath;
  const collapsed = toggleable && !context.expandedCompositions.has(root.itemId);
  const status = runStatusOf(root);
  const signal = RUN_SIGNALS[status.kind];
  const summary = subtreeOf(index, root.itemId);
  const steps = collapsed ? [] : chainSteps(index, root);
  const inner: SegmentContext = { ...context, depth };
  // A chain of ONE lap has nothing to fold: the composition's own row is
  // already that lap's summary, and folding it too would put two clicks between
  // the reader and a single called run. The rule is for CAMPAIGNS — a child
  // workflow that looped eleven times — which is where the wall was.
  const multiLap = steps.length > 1;
  return {
    key,
    itemId: root.itemId,
    workflowId: root.workflowId,
    label: root.workflowId,
    depth,
    status,
    signal,
    duration: indexDuration(index, root.startedAt ?? 0, root.endedAt ?? 0),
    blockerLabel: blockerLabelOf(status),
    collapsed,
    toggleable,
    summary,
    waves: steps.map(({ run, ordinal, lapSeq }) => {
      const folded = multiLap && waveIsSettled(index, run);
      const waveStatus = runStatusOf(run);
      return {
        key: waveKey(run.itemId),
        itemId: run.itemId,
        ordinal,
        lapSeq,
        folded,
        status: waveStatus,
        signal: waveSignalOf(index, run),
        summary: waveSummary(index, run, ordinal, lapSeq),
        segments: !folded || context.expandedWaves.has(run.itemId)
          ? runSegments(index, run, inner)
          : null,
        onFrontierPath: context.frontierKeys.has(waveKey(run.itemId)),
      } satisfies RunMapCompositionWave;
    }),
    onFrontierPath,
  };
}
