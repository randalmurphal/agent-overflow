// Table tests for the run-map projection. Describe names track the §7 case
// matrix of docs/architecture/workflow-run-map.md, which is binding: a row
// that is expressible at projection level has a group here, and the rows that
// are NOT expressible (driver, static fan-out width, remote posture, motion,
// theme, transport) say so where they would otherwise be missing.

import { describe, expect, it } from 'vitest';
import { WorkflowRunMapRun, WorkflowRunMapView } from '../../../bindings/agent-overflow/models';
import {
  campaignSkeleton,
  nestedFanView,
  refusedView,
  mapRun as makeRun,
  mapUnit as makeUnit,
  mapView as makeView,
  phaseAttempt as makePhase,
  runBudget,
  runSpend,
  skeletonPhase,
} from '../../test/fixtures/runMap';
import {
  buildRunMap,
  ghostAttempt,
  runMapPosition,
  runMapTone,
  runMapViewIsLive,
  RUN_MAP_INLINE_DONE_MAX,
  type RunMapCompositionNode,
  type RunMapSegmentNode,
} from './workflowRunMap';
import { branchKeyOf } from './workflowRunMapIndex';

const NOW = 10_000_000;

/**
 * One run's segment list. The model carries it: `buildRunMap` builds segments
 * for the live wave and for any folded wave the caller names, so this is a
 * lookup rather than a second walk.
 *
 * A run that is not a wave of THIS view's root chain (a called run, a wave
 * whose parent degraded to records-only) is read by building the map AT it —
 * which is what the overlay does when you open that run directly.
 */
function segmentsOf(
  view: WorkflowRunMapView,
  itemId: string,
  expanded?: string[],
  lanes?: string[],
  waves?: string[],
): RunMapSegmentNode[] {
  const options = {
    expandedWaveIds: [itemId, ...(waves ?? [])],
    expandedCompositionIds: expanded ?? [],
    expandedLaneIds: lanes ?? [],
  };
  const wave = buildRunMap(view, NOW, options).waves.find((candidate) => candidate.itemId === itemId);
  if (wave) return wave.segments ?? [];
  const rerooted = new WorkflowRunMapView({ rootItemId: itemId, runs: view.runs });
  return buildRunMap(rerooted, NOW, options).waves
    .find((candidate) => candidate.itemId === itemId)?.segments ?? [];
}

function nodeById(nodes: readonly RunMapSegmentNode[], phaseId: string): RunMapSegmentNode {
  const node = nodes.find((candidate) => candidate.phaseId === phaseId);
  if (!node) throw new Error(`no node for phase ${phaseId}: ${nodes.map((n) => n.phaseId).join(',')}`);
  return node;
}

function fanOf(nodes: readonly RunMapSegmentNode[], phaseId: string, attempt = 1) {
  const node = nodeById(nodes, phaseId);
  const found = node.attempts.find((candidate) => candidate.attempt === attempt)?.fan;
  if (!found) throw new Error(`phase ${phaseId} attempt ${attempt} has no fan`);
  return found;
}

// ---------------------------------------------------------------- base cases

describe('buildRunMap — base cases', () => {
  it('an empty view yields an empty model, not a crash', () => {
    const model = buildRunMap(makeView([], ''), NOW);
    expect(model).toEqual({
      rootItemId: '', waves: [], loop: null, frontier: [], followTarget: null,
      refusal: null, budget: null, moneyLabel: '', budgetLabel: '',
      spend: {
        totalUsd: 0, wireUsd: 0, estimatedUsd: 0, unpricedRows: 0,
        estimated: false, label: '', unpricedLabel: '',
      },
    });
    expect(segmentsOf(makeView([], ''), 'nobody')).toEqual([]);
  });

  it('a workflow with no self-call is one wave and NO loop affordance (the base case)', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
      phases: [makePhase('plan'), makePhase('build', { status: 'running', endedAt: 0, startedAt: 9_000_000 })],
    })]);
    const model = buildRunMap(view, NOW);
    expect(model.loop).toBeNull();
    expect(model.waves).toHaveLength(1);
    expect(model.waves[0].ordinal).toBe(1);
    expect(segmentsOf(view, 'root').every((node) => node.kind !== 'decision')).toBe(true);
  });

  it('exposes the root run tree money for the budget line', () => {
    const model = buildRunMap(makeView([
      makeRun('root', {
        spend: runSpend({ costUsd: 4.5, wireCostUsd: 4.5 }),
        budget: runBudget({ ceilingUsd: 10, spentUsd: 4.5, percent: 45 }),
        state: 'needs-human', reason: 'budget-exhausted',
      }),
    ]), NOW);
    expect(model.spend).toMatchObject({ totalUsd: 4.5, unpricedRows: 0, estimated: false });
    expect(model.budget).toMatchObject({ kind: 'usd', ceilingUsd: 10, percent: 45 });
    expect(model.moneyLabel).toBe('$4.50 of $10.00');
  });

  // §12 — a total carrying unpriced rows is a LOWER BOUND, and the summary must
  // say so rather than presenting it as the number.
  it('says "priced" and names the unpriced rows when the total is a lower bound', () => {
    const model = buildRunMap(makeView([makeRun('root', {
      spend: runSpend({ costUsd: 2, wireCostUsd: 1, estimatedCostUsd: 1, unpricedRows: 3 }),
    })]), NOW);
    expect(model.spend).toMatchObject({
      totalUsd: 2, wireUsd: 1, estimatedUsd: 1, unpricedRows: 3,
      estimated: true, unpricedLabel: '3 rows unpriced',
    });
    expect(model.moneyLabel).toBe('$2.00 priced · 3 rows unpriced');
  });

  it('says "spent" when nothing is missing and no ceiling stands over the tree', () => {
    const model = buildRunMap(makeView([makeRun('root', { spend: runSpend({ costUsd: 1.25, wireCostUsd: 1.25 }) })]), NOW);
    expect([model.budget, model.moneyLabel]).toEqual([null, '$1.25 spent']);
  });

  it('a ceiling that is not in dollars leaves the summary comparing nothing', () => {
    // A token bound has no dollars beside it, and "$1.25 of 400000" is a
    // comparison that does not exist — the ceiling still reaches the model.
    const model = buildRunMap(makeView([makeRun('root', {
      spend: runSpend({ costUsd: 1.25, wireCostUsd: 1.25 }),
      budget: runBudget({ kind: 'tokens', ceilingTokens: 400_000, spentTokens: 10, percent: 1 }),
    })]), NOW);
    expect(model.budget).toMatchObject({ kind: 'tokens', ceilingUsd: 0 });
    expect(model.moneyLabel).toBe('$1.25 spent');
    // …and the bound itself is stated in ITS OWN units rather than dropped:
    // "lap N" beside nothing was the whole rendering of a token-bounded run.
    expect(model.budgetLabel).toBe('10 of 400.0k tokens');
  });

  it('a wall-clock ceiling reads as elapsed against the bound, in duration shapes', () => {
    const model = buildRunMap(makeView([makeRun('root', {
      budget: runBudget({
        kind: 'wall_clock', ceilingMillis: 1_800_000, elapsedMillis: 250_000, percent: 14,
      }),
    })]), NOW);
    expect(model.budget).toMatchObject({ kind: 'wall_clock', ceilingMillis: 1_800_000, elapsedMillis: 250_000 });
    expect(model.budgetLabel).toBe('4m of 30m');
    expect(model.moneyLabel).toBe('');
  });

  it('a DOLLAR ceiling leaves the budget line empty — the money line already compares it', () => {
    const model = buildRunMap(makeView([makeRun('root', {
      spend: runSpend({ costUsd: 4.5, wireCostUsd: 4.5 }),
      budget: runBudget({ ceilingUsd: 10, spentUsd: 4.5, percent: 45 }),
    })]), NOW);
    expect([model.moneyLabel, model.budgetLabel]).toEqual(['$4.50 of $10.00', '']);
  });

  it('a ceiling whose kind this build cannot read states nothing rather than guessing', () => {
    const model = buildRunMap(makeView([makeRun('root', {
      budget: runBudget({ kind: 'moon-phases', percent: 3 }),
    })]), NOW);
    expect(model.budget?.kind).toBe('moon-phases');
    expect(model.budgetLabel).toBe('');
  });

  it('a tree that has cost nothing and stands under nothing says nothing', () => {
    expect(buildRunMap(makeView([makeRun('root')]), NOW).moneyLabel).toBe('');
  });

  // §4.2 — a refusal rides a SUCCESSFUL answer, so it is model state, not error
  // state, and every code is permanent.
  it('carries a refusal through as user-shaped state with no waves to draw', () => {
    const model = buildRunMap(refusedView('too-large', 'Run r-9 has too many runs to draw.'), NOW);
    expect(model.refusal).toEqual({ code: 'too-large', message: 'Run r-9 has too many runs to draw.' });
    expect([model.waves, model.followTarget]).toEqual([[], null]);
  });

  it('leaves refusal null for an ordinary answer', () => {
    expect(buildRunMap(campaignView(), NOW).refusal).toBeNull();
  });

  // §7 "empty / undecodable snapshot" — an ABSENT snapshot is ordinary history
  // and a CORRUPT one is a defect, so the wave carries them as two facts.
  it('projects a corrupt snapshot as its own wave-level signal, apart from mere absence', () => {
    const corrupt = buildRunMap(makeView([makeRun('root', {
      skeletonMissing: true, skeletonError: 'unmarshal snapshot: unexpected end of JSON input',
      phases: [makePhase('audit')],
    })]), NOW).waves[0];
    expect([corrupt.recordsOnly, corrupt.skeletonError])
      .toEqual([true, 'unmarshal snapshot: unexpected end of JSON input']);

    const absent = buildRunMap(makeView([makeRun('root', {
      skeletonMissing: true, phases: [makePhase('audit')],
    })]), NOW).waves[0];
    expect([absent.recordsOnly, absent.skeletonError]).toEqual([true, '']);
  });

  it('builds from the view\'s own rootItemId (server root resolution, §5.9)', () => {
    const view = makeView([
      makeRun('child', { parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 1, workflowId: 'campaign' }),
      makeRun('root', { tailSelfCall: true, skeleton: campaignSkeleton() }),
    ], 'root');
    expect(buildRunMap(view, NOW).waves.map((wave) => wave.itemId)).toEqual(['root', 'child']);
  });

  it('is deterministic: the same view builds a deeply equal model and segments', () => {
    const view = campaignView();
    expect(buildRunMap(view, NOW)).toEqual(buildRunMap(view, NOW));
    expect(segmentsOf(view, 'wave-2')).toEqual(segmentsOf(view, 'wave-2'));
  });

  it('keys are unique inside a segment list, so keyed each-blocks are safe', () => {
    const view = fanView({ unitChildren: true });
    const segments = segmentsOf(view, 'root');
    const keys = [
      ...segments.map((node) => node.key),
      ...segments.flatMap((node) => node.attempts.map((attempt) => attempt.key)),
      ...segments.flatMap((node) => node.attempts.flatMap((attempt) => [
        ...(attempt.fan ? [attempt.fan.key, attempt.fan.queued.key, attempt.fan.done.key] : []),
        ...(attempt.fan?.columns ?? []).flatMap((column) => [column.key, column.unit.key]),
        ...(attempt.fan?.queued.entries ?? []).map((chip) => chip.key),
        ...(attempt.fan?.done.entries ?? []).map((chip) => chip.key),
      ])),
    ];
    expect(new Set(keys).size).toBe(keys.length);
  });

  it('expands any run in the tree, not only a top-level wave', () => {
    const view = fanView({ unitChildren: true });
    expect(segmentsOf(view, 'port-b-child')).toEqual([]);
    const withPhases = makeView([
      ...view.runs.filter((run) => run.itemId !== 'port-b-child'),
      makeRun('port-b-child', {
        workflowId: 'porter', parentItemId: 'root', parentPhaseId: 'port',
        parentAttempt: 1, parentUnitId: 'port-b',
        skeleton: [skeletonPhase('work')], phases: [makePhase('work')],
      }),
    ], 'root');
    expect(segmentsOf(withPhases, 'port-b-child').map((node) => node.phaseId)).toEqual(['work']);
  });

  it('is lazy by WIDTH: a folded wave nobody opened carries no segments (§6)', () => {
    const view = campaignView();
    const model = buildRunMap(view, NOW);
    // wave-3 is the live one: expanded in place, so its nodes are never
    // optional. The two folded ones cost nothing until they are opened.
    expect(model.waves.map((wave) => [wave.itemId, wave.segments === null]))
      .toEqual([['wave-1', true], ['wave-2', true], ['wave-3', false]]);

    const opened = buildRunMap(view, NOW, { expandedWaveIds: ['wave-2'] });
    expect(opened.waves.map((wave) => [wave.itemId, wave.segments === null]))
      .toEqual([['wave-1', true], ['wave-2', false], ['wave-3', false]]);
    expect(opened.waves[1].segments?.map((node) => node.phaseId)).toEqual(['audit', 'fix', 'next']);
  });
});

/** Two folded waves plus a live third, with a fan-out in the live one. */
function campaignView(): WorkflowRunMapView {
  return makeView([
    makeRun('wave-1', {
      state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(), endedAt: 200_000,
      phases: [makePhase('audit'), makePhase('fix'), makePhase('next')],
    }),
    makeRun('wave-2', {
      state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(), endedAt: 400_000,
      parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
      phases: [makePhase('audit'), makePhase('fix'), makePhase('next')],
    }),
    makeRun('wave-3', {
      state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
      parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1,
      phases: [makePhase('audit'), makePhase('fix', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
    }),
  ], 'wave-1');
}

// ---------------------------------------------------------------- §3 waves

describe('§3 wave chain — what flattens, what nests', () => {
  it('tail self-call | flattens into chain-local ordinals from 1', () => {
    const model = buildRunMap(campaignView(), NOW);
    expect(model.waves.map((wave) => [wave.itemId, wave.ordinal, wave.folded]))
      .toEqual([['wave-1', 1, true], ['wave-2', 2, true], ['wave-3', 3, false]]);
  });

  it('self-call, NOT tail | composition — explicitly never flattened', () => {
    const view = makeView([
      makeRun('root', {
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('fix')],
      }),
      // Same workflow, but bound to `fix` rather than the terminal `next`.
      makeRun('inner', { parentItemId: 'root', parentPhaseId: 'fix', parentAttempt: 1, workflowId: 'campaign' }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['root']);
    const node = nodeById(segmentsOf(view, 'root'), 'fix');
    expect(node.kind).toBe('call');
    expect(node.attempts[0].chain.map((entry) => entry.itemId)).toEqual(['inner']);
  });

  it('call phase, other workflow | composition chain (CallNode)', () => {
    const view = makeView([
      makeRun('root', {
        skeleton: [skeletonPhase('delegate', { shape: 'call', callTarget: 'audit-flow' })],
        phases: [makePhase('delegate', { status: 'running', endedAt: 0 })],
      }),
      makeRun('called', {
        workflowId: 'audit-flow', parentItemId: 'root', parentPhaseId: 'delegate', parentAttempt: 1,
      }),
    ]);
    const node = nodeById(segmentsOf(view, 'root'), 'delegate');
    expect(node.kind === 'call' && node.callTarget).toBe('audit-flow');
    expect(node.attempts[0].chain[0].workflowId).toBe('audit-flow');
    expect(buildRunMap(view, NOW).waves).toHaveLength(1);
  });

  it('unit-bound call | branch chain (composition), never a wave', () => {
    const view = fanView({ unitChildren: true });
    const fan = fanOf(segmentsOf(view, 'root'), 'port');
    const branch = fan.columns.find((column) => column.unit.unitId === 'port-b');
    expect(branch?.chain.map((entry) => entry.itemId)).toEqual(['port-b-child']);
    expect(buildRunMap(view, NOW).waves).toHaveLength(1);
  });

  // §7's "unit-bound call ⇒ branch chain" is a fact about the unit, not about
  // its status. Routing a FINISHED call-bound unit into the done group — which
  // renders a label and nothing else — deleted the child run and its whole
  // composition subtree from the map the moment the branch stopped running.
  //
  // What a SETTLED lane does not do is paint that subtree forever: the lane
  // keeps its column, the column folds to its header, and the chain is one
  // click away. Reachability is the contract, not always-drawn-ness.
  it('unit-bound call | a COMPLETED call-bound unit keeps its lane, folded, and its subtree one click away', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [
          makeUnit('port-a', { unitIndex: 0, status: 'done' }),
          makeUnit('port-b', { unitIndex: 1, status: 'done' }),
        ],
      }),
      makeRun('port-b-child', {
        workflowId: 'porter', state: 'done',
        parentItemId: 'root', parentPhaseId: 'port', parentAttempt: 1, parentUnitId: 'port-b',
        skeleton: [skeletonPhase('carry')], phases: [makePhase('carry')],
      }),
    ]);
    const folded = fanOf(segmentsOf(view, 'root'), 'port');
    const laneKey = folded.columns.find((column) => column.unit.unitId === 'port-b')?.key ?? '';
    expect(laneKey).not.toBe('');
    expect(folded.columns.map((column) => [column.unit.unitId, column.collapsed, column.toggleable]))
      .toEqual([['port-b', true, true]]);
    // Collapsed means NOT BUILT, the same convention a folded wave uses: there
    // is one answer to "is this open", not a flag beside a populated chain.
    expect(folded.columns[0].chain).toEqual([]);

    // The click, and everything the lane was holding comes back.
    const opened = fanOf(segmentsOf(view, 'root', ['port-b-child'], [laneKey]), 'port');
    const branch = opened.columns.find((column) => column.unit.unitId === 'port-b');
    expect(branch?.chain.map((entry) => entry.itemId)).toEqual(['port-b-child']);
    expect(branch?.chain[0].waves.flatMap((wave) => (wave.segments ?? []).map((node) => node.phaseId)))
      .toEqual(['carry']);

    // The childless done unit still becomes arithmetic — structure is the rule,
    // not "every unit gets a column once one of them does".
    expect(folded.done.entries.map((chip) => chip.unitId)).toEqual(['port-a']);
    expect(folded.done.label).toBe('done ·1');
  });

  it('a campaign that is itself a callee still flattens internally', () => {
    const view = makeView([
      makeRun('outer', {
        workflowId: 'outer-flow',
        skeleton: [skeletonPhase('kick', { shape: 'call', callTarget: 'campaign' })],
        phases: [makePhase('kick', { status: 'running', endedAt: 0 })],
      }),
      makeRun('inner-1', {
        parentItemId: 'outer', parentPhaseId: 'kick', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(), phases: [makePhase('next')],
      }),
      makeRun('inner-2', {
        parentItemId: 'inner-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
    ], 'outer');
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['outer']);
    // Opened: the inner campaign's laps are laps, numbered from its own root.
    const composition = nodeById(segmentsOf(view, 'outer', ['inner-1']), 'kick').attempts[0].chain[0];
    expect(composition.waves.map((wave) => [wave.itemId, wave.ordinal]))
      .toEqual([['inner-1', 1], ['inner-2', 2]]);
    expect(composition.summary.waveCount).toBe(2);
  });

  it('child run failed mid-chain | chain ends there, no ghost next wave', () => {
    const view = makeView([
      makeRun('wave-1', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('next')],
      }),
      makeRun('wave-2', {
        state: 'failed', reason: 'agent-error', tailSelfCall: true, skeleton: campaignSkeleton(),
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        phases: [makePhase('audit', { status: 'failed' })],
      }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['wave-1', 'wave-2']);
    expect(model.waves[1].summary.outcome).toEqual({ kind: 'failed', reason: 'agent-error' });
    expect(model.loop).toBeNull();
    const segments = segmentsOf(view, 'wave-2');
    expect(segments.map((node) => node.phaseId)).toEqual(['audit']);
  });

  it('a retried tail call keeps BOTH child runs as waves rather than dropping one', () => {
    const view = makeView([
      makeRun('root', {
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next', { status: 'failed' }), makePhase('next', { attempt: 2 })],
      }),
      makeRun('retry-a', {
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 1, state: 'failed',
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
      makeRun('retry-b', {
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 2,
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['root', 'retry-a', 'retry-b']);
    // "Looped" is about THIS run's own tail children, never its position in the
    // chain: `retry-a` is followed in the list by a wave it did not produce,
    // and reading position labelled that dead end a loop.
    expect(model.waves.map((wave) => wave.summary.outcome))
      .toEqual([{ kind: 'looped' }, { kind: 'failed', reason: '' }, { kind: 'running' }]);
    expect(model.waves.map((wave) => wave.summary.outcomeLabel)).toEqual(['Looped', 'Failed', 'Running']);
  });

  // The chain is a TREE once a tail call is retried, so its traversal order is
  // the contract: depth-first walked one retry's grandchild before the other
  // retry and produced ordinals 1, 2, 3, 2 — a wave list that goes backwards.
  it('a retried tail call WITH grandchildren keeps the wave ordinals monotonic', () => {
    const wave = (itemId: string, over = {}) => makeRun(itemId, {
      tailSelfCall: true, skeleton: campaignSkeleton(), ...over,
    });
    const view = makeView([
      wave('root', { phases: [makePhase('next', { status: 'failed' }), makePhase('next', { attempt: 2 })] }),
      wave('retry-a', {
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 1, state: 'done',
        phases: [makePhase('next')],
      }),
      wave('retry-b', { parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 2 }),
      wave('grandchild', { parentItemId: 'retry-a', parentPhaseId: 'next', parentAttempt: 1 }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((w) => [w.itemId, w.ordinal]))
      .toEqual([['root', 1], ['retry-a', 2], ['retry-b', 2], ['grandchild', 3]]);
    const ordinals = model.waves.map((w) => w.ordinal);
    expect([...ordinals].sort((l, r) => l - r)).toEqual(ordinals);
    // Two rows both saying "wave 2" is two runs the reader cannot tell apart,
    // so a shared lap numbers its tries the way an attempt does.
    expect(model.waves.map((w) => [w.lapSeq, w.summary.label])).toEqual([
      [0, 'wave 1'], [1, 'wave 2 ·1'], [2, 'wave 2 ·2'], [0, 'wave 3'],
    ]);
  });

  it('a parent-linkage cycle terminates, and both runs still get ordinals', () => {
    const view = makeView([
      makeRun('a', { tailSelfCall: true, skeleton: campaignSkeleton(), parentItemId: 'b', parentPhaseId: 'next' }),
      makeRun('b', { tailSelfCall: true, skeleton: campaignSkeleton(), parentItemId: 'a', parentPhaseId: 'next' }),
    ], 'a');
    expect(buildRunMap(view, NOW).waves.map((wave) => [wave.itemId, wave.ordinal])).toEqual([['a', 1], ['b', 2]]);
  });
});

// ---------------------------------------------------------------- §5.1 nodes

describe('§5.1 skeleton ∪ records', () => {
  it('single-shape phase, agent driver | plain node, thread link when threadId is set', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('plan', { name: 'plan the work' })],
      phases: [makePhase('plan', { threadId: 'thread-1' })],
    })]);
    const node = nodeById(segmentsOf(view, 'root'), 'plan');
    expect([node.kind, node.label, node.ghost]).toEqual(['phase', 'plan the work', false]);
    expect(node.attempts[0].threadId).toBe('thread-1');
    expect(node.status).toEqual({ kind: 'completed' });
  });

  it('single-shape phase, tool driver | no thread link (the driver itself is not projected)', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('lint')], phases: [makePhase('lint')],
    })]);
    expect(nodeById(segmentsOf(view, 'root'), 'lint').attempts[0].threadId).toBe('');
  });

  it('every skeleton phase yields a node; skeleton-only phases are ghosts', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('plan'), skeletonPhase('build'), skeletonPhase('verify')],
      phases: [makePhase('plan'), makePhase('build', { status: 'running', endedAt: 0 })],
    })]);
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => [node.phaseId, node.ghost]))
      .toEqual([['plan', false], ['build', false], ['verify', true]]);
    expect(segments[2].status).toEqual({ kind: 'ghost' });
    expect(segments[2].signal).toBe('ghost');
    expect(runMapTone(segments[2].signal)).toBe('text-fg-hint');
  });

  it('definition drift | records missing from the skeleton append with notInDefinition', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('plan')],
      phases: [makePhase('plan'), makePhase('legacy-sweep', { startedAt: 3_000 })],
    })]);
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => [node.phaseId, node.notInDefinition]))
      .toEqual([['plan', false], ['legacy-sweep', true]]);
  });

  // §5.1 orphan records sort AFTER the whole skeleton, and §5.5's "furthest
  // point" is a DECLARED-order position — so one orphan sat past every declared
  // phase and struck out the entire future of a live run.
  it('definition drift | an orphan record does not make the live run\'s future look skipped', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('plan'), skeletonPhase('build'), skeletonPhase('verify')],
      phases: [makePhase('plan'), makePhase('legacy-sweep', { startedAt: 3_000 })],
    })]);
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => [node.phaseId, node.skipped, node.ghost])).toEqual([
      ['plan', false, false],
      ['build', false, true],
      ['verify', false, true],
      ['legacy-sweep', false, false],
    ]);
  });

  it('phases render in declared order, never started_at order', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('audit'), skeletonPhase('fix')],
      phases: [makePhase('fix', { startedAt: 1_000 }), makePhase('audit', { startedAt: 9_000 })],
    })]);
    expect(segmentsOf(view, 'root').map((node) => node.phaseId)).toEqual(['audit', 'fix']);
  });

  it('phase attempt reopened | attempts render as an ascending sequence in place', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('audit', { name: 'audit' })],
      phases: [
        makePhase('audit', { attempt: 2, status: 'running', endedAt: 0, startedAt: 9_940_000 }),
        makePhase('audit', { attempt: 1, status: 'failed' }),
      ],
    })]);
    const node = nodeById(segmentsOf(view, 'root'), 'audit');
    expect(node.attempts.map((attempt) => [attempt.attempt, attempt.label]))
      .toEqual([[1, 'audit ·1'], [2, 'audit ·2']]);
    // The node's own status is the LATEST attempt's — the phase is running again.
    expect(node.status).toEqual({ kind: 'running' });
    expect(node.attempts[1].duration).toBe('1m');
  });

  it('check phase (isCheck) | normal node rules with the flag carried through', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('audit', { isCheck: true })], phases: [makePhase('audit')],
    })]);
    expect(nodeById(segmentsOf(view, 'root'), 'audit').isCheck).toBe(true);
  });

  it('intervention recorded on an attempt | touched marker from interventionKind', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('fix')],
      phases: [makePhase('fix', { interventionKind: 'taken-over' })],
    })]);
    const attempt = nodeById(segmentsOf(view, 'root'), 'fix').attempts[0];
    expect([attempt.touched, attempt.interventionKind]).toEqual([true, 'taken-over']);
  });

  it('an unknown phase status passes through as neutral rather than being dropped', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('odd')], phases: [makePhase('odd', { status: 'superseded' })],
    })]);
    const node = nodeById(segmentsOf(view, 'root'), 'odd');
    expect(node.status).toEqual({ kind: 'unknown', raw: 'superseded' });
    // Its OWN signal, never folded into `pending`: a status this build cannot
    // name must not be drawn as a queue the phase is not in.
    expect(node.signal).toBe('unknown');
    expect(runMapTone(node.signal)).toBe('text-fg-muted');
  });

  it('§5.5 loop-back re-entry | an unrecorded phase BEFORE the furthest point is skipped, not future', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('plan'), skeletonPhase('audit'), skeletonPhase('fix'), skeletonPhase('verify')],
      phases: [makePhase('audit'), makePhase('fix', { status: 'running', endedAt: 0 })],
    })]);
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => [node.phaseId, node.ghost, node.skipped]))
      .toEqual([['plan', true, true], ['audit', false, false], ['fix', false, false], ['verify', true, false]]);
  });

  it('a skipped phase is drawn even for a terminal run — it is the past, not a future', () => {
    const view = makeView([makeRun('root', {
      state: 'done',
      skeleton: [skeletonPhase('plan'), skeletonPhase('fix'), skeletonPhase('verify')],
      phases: [makePhase('fix')],
    })]);
    expect(segmentsOf(view, 'root').map((node) => [node.phaseId, node.skipped]))
      .toEqual([['plan', true], ['fix', false]]);
  });

  it('cause text passes through verbatim (clamping is CSS)', () => {
    const cause = 'workspace would not cut: '.repeat(20);
    const view = makeView([makeRun('root', {
      state: 'needs-human', reason: 'stuck',
      skeleton: [skeletonPhase('fix')], phases: [makePhase('fix', { status: 'parked', cause, endedAt: 0 })],
    })]);
    const node = nodeById(segmentsOf(view, 'root'), 'fix');
    expect(node.status).toEqual({ kind: 'parked', cause });
    expect(node.attempts[0].cause).toBe(cause);
  });
});

describe('§5.6 ghosts exist only for live runs', () => {
  it.each([
    ['done', true],
    ['failed', true],
    ['cancelled', true],
    ['running', false],
    ['needs-human', false],
  ])('terminal run (%s) draws no future: %s', (state, terminal) => {
    const view = makeView([makeRun('root', {
      state,
      skeleton: [skeletonPhase('plan'), skeletonPhase('verify')],
      phases: [makePhase('plan')],
    })]);
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => node.phaseId)).toEqual(terminal ? ['plan'] : ['plan', 'verify']);
  });

  it('a terminal run keeps its tail-phase RECORDS even though it draws no stubs', () => {
    const view = makeView([makeRun('root', {
      state: 'failed', reason: 'wiring-error', tailSelfCall: true, skeleton: campaignSkeleton(0),
      phases: [makePhase('next', { status: 'parked', cause: 'resolve workflow "gone"', endedAt: 0 })],
    })]);
    const segments = segmentsOf(view, 'root');
    const decision = segments[segments.length - 1];
    expect(decision.kind).toBe('decision');
    expect(decision.attempts[0].cause).toBe('resolve workflow "gone"');
    expect(decision.kind === 'decision' && decision.loop.showOutcomeStubs).toBe(false);
    expect(decision.kind === 'decision' && decision.loop.decided).toBeNull();
  });

  it('a terminal tail phase with no records at all drops the loop foot entirely', () => {
    const view = makeView([makeRun('root', {
      state: 'cancelled', tailSelfCall: true, skeleton: campaignSkeleton(),
      phases: [makePhase('audit', { status: 'cancelled' })],
    })]);
    expect(segmentsOf(view, 'root').map((node) => node.kind)).toEqual(['phase']);
    expect(buildRunMap(view, NOW).loop).toBeNull();
  });
});

describe('§5.8 records-only degradation (empty / undecodable snapshot)', () => {
  const view = makeView([makeRun('root', {
    state: 'running', skeletonMissing: true, tailSelfCall: false,
    phases: [makePhase('second', { startedAt: 5_000 }), makePhase('first', { startedAt: 1_000 })],
    units: [makeUnit('u-1', { phaseId: 'second' })],
  })]);

  it('renders recorded phases in recorded order with no ghosts and no loop foot', () => {
    const segments = segmentsOf(view, 'root');
    expect(segments.map((node) => [node.phaseId, node.ghost, node.notInDefinition]))
      .toEqual([['second', false, false], ['first', false, false]]);
    expect(segments.some((node) => node.kind === 'decision')).toBe(false);
    expect(buildRunMap(view, NOW).loop).toBeNull();
  });

  it('still decides node kind from the records: units make it a fan', () => {
    expect(nodeById(segmentsOf(view, 'root'), 'second').kind).toBe('fan');
  });

  it('per-wave skeleton (§5.7): one wave degrading does not degrade the next', () => {
    const drifted = makeView([
      makeRun('wave-1', {
        state: 'done', skeletonMissing: true, phases: [makePhase('mystery')],
      }),
      makeRun('wave-2', {
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(), phases: [makePhase('audit')],
      }),
    ]);
    // wave-1 has no skeleton, so its tail edge cannot be identified: wave-2 is
    // composition, not a wave, and only wave-2 draws ghosts.
    const model = buildRunMap(drifted, NOW);
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['wave-1']);
    expect(segmentsOf(drifted, 'wave-2').map((node) => [node.phaseId, node.kind, node.ghost]))
      .toEqual([['audit', 'phase', false], ['fix', 'phase', true], ['next', 'decision', true]]);
    expect(segmentsOf(drifted, 'wave-1').map((node) => [node.phaseId, node.kind, node.ghost]))
      .toEqual([['mystery', 'phase', false]]);
  });
});

// ---------------------------------------------------------------- §6 fan

interface FanOptions {
  unitChildren?: boolean;
  width?: number;
  pre?: boolean;
}

function fanView(options: FanOptions = {}): WorkflowRunMapView {
  const width = options.width ?? 0;
  const extras = Array.from({ length: width }, (_, index) =>
    makeUnit(`bulk-${String(index).padStart(2, '0')}`, {
      unitIndex: 10 + index, status: index % 2 === 0 ? 'pending' : 'done',
    }));
  const runs = [makeRun('root', {
    state: 'running',
    skeleton: [skeletonPhase('plan'), skeletonPhase('port', { name: 'ports', shape: 'fan-out' }), skeletonPhase('verify')],
    phases: [
      makePhase('plan'),
      ...(options.pre ? [] : [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })]),
    ],
    units: options.pre ? [] : [
      makeUnit('port-a', { unitIndex: 0, status: 'done' }),
      makeUnit('port-b', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000, threadId: 't-b' }),
      makeUnit('port-c', { unitIndex: 2, status: 'failed' }),
      makeUnit('port-d', { unitIndex: 3, status: 'pending', provider: 'claude', startedAt: 0, endedAt: 0 }),
      makeUnit('port-e', { unitIndex: 4, status: 'dropped' }),
      makeUnit('port-f', { unitIndex: 5, status: 'taken-over', threadId: 't-f' }),
      ...extras,
      makeUnit('port-join', { unitIndex: 99, kind: 'join', status: 'pending' }),
    ],
  })];
  if (options.unitChildren) {
    runs.push(makeRun('port-b-child', {
      workflowId: 'porter', parentItemId: 'root', parentPhaseId: 'port',
      parentAttempt: 1, parentUnitId: 'port-b',
    }));
  }
  return makeView(runs);
}

describe('§6 fan scale', () => {
  it('fan-out expanded | columns for actionable branches, group nodes for the rest', () => {
    const fan = fanOf(segmentsOf(fanView(), 'root'), 'port');
    expect(fan.columns.map((column) => column.unit.unitId)).toEqual(['port-b', 'port-c', 'port-f']);
    // Every actionable lane is open and none of them is toggleable: there is
    // nothing to fold on the path the reader is watching.
    expect(fan.columns.every((column) => !column.collapsed && !column.toggleable)).toBe(true);
    expect(fan.done.entries.map((chip) => chip.unitId)).toEqual(['port-a', 'port-e']);
    expect(fan.done.label).toBe('done ·1');
    expect(fan.done.droppedCount).toBe(1);
  });

  // §7: a queued group names WHICH lanes it stands for, not just how many. The
  // range is the engine's own `unitIndex`, so it is the same coordinate the
  // open columns beside it carry — and it holds no entries, because a queued
  // lane has no record, no thread and no duration for a click to reveal.
  it('queued lanes group into ONE node labelled by their contiguous range', () => {
    const fan = fanOf(segmentsOf(fanView(), 'root'), 'port');
    expect([fan.queued.label, fan.queued.count, fan.queued.entries]).toEqual(['ports 3 · queued', 1, []]);
  });

  it('a queued group whose lanes are not contiguous falls back to a count it can prove', () => {
    // port-d (index 3) plus the even-indexed extras from 10 up: a range label
    // would claim lanes 4–9 that are not in the group.
    const fan = fanOf(segmentsOf(fanView({ width: 26 }), 'root'), 'port');
    expect(fan.queued.label).toBe('14 units · queued');
  });

  it('join unit | renders as the merge node, never a column or a chip', () => {
    const fan = fanOf(segmentsOf(fanView(), 'root'), 'port');
    expect(fan.join?.unitId).toBe('port-join');
    expect(fan.join?.isJoin).toBe(true);
    expect(fan.columns.concat().some((column) => column.unit.isJoin)).toBe(false);
    expect(fan.done.entries.some((chip) => chip.isJoin)).toBe(false);
  });

  it('unit `dropped` | struck entry inside the done chip expansion', () => {
    const dropped = fanOf(segmentsOf(fanView(), 'root'), 'port').done.entries.find((chip) => chip.unitId === 'port-e');
    expect([dropped?.struck, dropped?.signal, dropped?.meta])
      .toEqual([true, 'dropped', 'dropped — join proceeded without it · 1s']);
  });

  it('unit `taken-over` | amber signal and its triage thread', () => {
    const takenOver = fanOf(segmentsOf(fanView(), 'root'), 'port').columns.find((column) => column.unit.unitId === 'port-f');
    expect([takenOver?.unit.signal, takenOver?.unit.threadId]).toEqual(['parked', 't-f']);
    expect(runMapTone(takenOver?.unit.signal ?? 'done')).toBe('text-warning');
  });

  // The wording rule survives the grouping: a pending unit that EARNS a lane —
  // here because it called a run, so it has structure — still reads "queued"
  // rather than borrowing a wait state the engine never reported.
  it('`pending` + provider | meta reads "queued" (D: v1 wording)', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [makeUnit('port-d', { unitIndex: 3, status: 'pending', provider: 'claude', startedAt: 0, endedAt: 0 })],
      }),
      makeRun('port-d-child', {
        workflowId: 'porter', parentItemId: 'root', parentPhaseId: 'port',
        parentAttempt: 1, parentUnitId: 'port-d',
      }),
    ]);
    const queued = fanOf(segmentsOf(view, 'root'), 'port').columns[0].unit;
    expect([queued.meta, queued.provider, queued.signal]).toEqual(['queued', 'claude', 'pending']);
  });

  // The fan states no tally of its own any more — the wave's summary row
  // already carries the unit count, and a second one under the same node was a
  // number the reader had to reconcile with the first. What still has to hold
  // is the PARTITION the count was standing in for: every non-join unit is
  // drawn or counted exactly once, and the join is neither.
  it('every unit lands in exactly one of column / done / queued, joins apart', () => {
    const fan = fanOf(segmentsOf(fanView(), 'root'), 'port');
    const placed = [
      ...fan.columns.map((column) => column.unit.unitId),
      ...fan.done.entries.map((chip) => chip.unitId),
    ].sort();
    expect(placed).toEqual(['port-a', 'port-b', 'port-c', 'port-e', 'port-f']);
    // port-d is the sixth: queued, so it is counted rather than drawn.
    expect([fan.queued.count, placed.length + fan.queued.count]).toEqual([1, 6]);
    expect(fan.join?.unitId).toBe('port-join');
  });

  it('fan-out at the width cap (32) | columns stay bounded, the bulk becomes arithmetic', () => {
    const fan = fanOf(segmentsOf(fanView({ width: 26 }), 'root'), 'port');
    expect(fan.columns).toHaveLength(3);
    expect(fan.queued.count).toBe(14);
    expect(fan.done.count).toBe(14);
    expect(fan.done.entries).toHaveLength(15); // the 14 plus the dropped one
    // Every unit is accounted for exactly once, whether it is drawn or counted.
    expect(fan.queued.count + fan.done.entries.length + fan.columns.length).toBe(32);
  });

  it('fan-out pre-expansion | count-less ghost named from the skeleton phase', () => {
    const node = nodeById(segmentsOf(fanView({ pre: true }), 'root'), 'port');
    expect([node.kind, node.ghost]).toEqual(['fan', true]);
    expect(node.kind === 'fan' && node.ghostLabel).toBe('units — declared by ports');
    expect(node.attempts).toEqual([]);
  });

  it('unit ids of any length pass through unmodified (truncation is CSS)', () => {
    const unitId = 'section-'.repeat(40);
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [makePhase('port', { status: 'running', endedAt: 0 })],
      units: [makeUnit(unitId, { status: 'running', endedAt: 0 })],
    })]);
    expect(fanOf(segmentsOf(view, 'root'), 'port').columns[0].unit.label).toBe(unitId);
  });

  it('a unit retry surfaces as ×N in the chip meta', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [makePhase('port', { status: 'running', endedAt: 0 })],
      units: [makeUnit('u-1', { unitAttempt: 3 })],
    })]);
    expect(fanOf(segmentsOf(view, 'root'), 'port').done.entries[0].meta).toBe('×3 · 1s');
  });

  it('each attempt keeps its OWN fan, so a retried fan-out loses no units', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [
        makePhase('port', { status: 'failed' }),
        makePhase('port', { attempt: 2, status: 'running', endedAt: 0 }),
      ],
      units: [
        makeUnit('old-1', { status: 'failed' }),
        makeUnit('new-1', { attempt: 2, status: 'running', endedAt: 0 }),
      ],
    })]);
    const node = nodeById(segmentsOf(view, 'root'), 'port');
    expect(node.attempts.map((attempt) => attempt.fan?.columns.map((column) => column.unit.unitId)))
      .toEqual([['old-1'], ['new-1']]);
  });

  // §7: "what completed" is not behind a click for a small fan — the done
  // group renders inline up to `RUN_MAP_INLINE_DONE_MAX` and folds past it.
  // Queued never inlines: its entries are empty by construction, so inline
  // rendering would draw nothing.
  it('a done group inlines at the bound and folds past it; queued never inlines', () => {
    const doneFan = (count: number) => fanOf(segmentsOf(makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [makePhase('port', { status: 'running', endedAt: 0 })],
      units: Array.from({ length: count }, (_, index) =>
        makeUnit(`bulk-${String(index).padStart(2, '0')}`, { unitIndex: index, status: 'done' })),
    })]), 'root'), 'port');
    // The bound is exact: AT the constant the chips stay in the flow, one past
    // it the group folds — pinned against the constant so tuning it cannot
    // silently strand this test at the old number.
    const atBound = doneFan(RUN_MAP_INLINE_DONE_MAX);
    expect([atBound.done.entries.length, atBound.done.inline])
      .toEqual([RUN_MAP_INLINE_DONE_MAX, true]);
    const pastBound = doneFan(RUN_MAP_INLINE_DONE_MAX + 1);
    expect([pastBound.done.entries.length, pastBound.done.inline])
      .toEqual([RUN_MAP_INLINE_DONE_MAX + 1, false]);
    expect(fanOf(segmentsOf(fanView(), 'root'), 'port').queued.inline).toBe(false);
  });

  // How a fan DRAWS is the model's call, keyed by lane containment: columns
  // wherever the card's full width is available, stacked once inside a lane —
  // columns inside a column can only subdivide a width that was already
  // minimal. Depth alone is NOT the key: a spine sub-card sits below the wave
  // with the full card width and keeps columns.
  it('the top-level fan is columns, and a fan inside a lane is stacked', () => {
    const top = fanOf(segmentsOf(nestedFanView(), 'root'), 'port');
    expect(top.layout).toBe('columns');
    const lap = top.columns[0].chain[0].waves[0];
    expect(fanOf(lap.segments ?? [], 'review').layout).toBe('stacked');
  });

  it('a fan on a spine sub-card keeps columns — only a lane forces stacking', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('sub', { shape: 'call', callTarget: 'porter' })],
        phases: [makePhase('sub', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
      }),
      makeRun('child', {
        workflowId: 'porter', state: 'running',
        parentItemId: 'root', parentPhaseId: 'sub', parentAttempt: 1, callDepth: 1,
        skeleton: [skeletonPhase('review', { name: 'reviews', shape: 'fan-out' })],
        phases: [makePhase('review', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
        units: [makeUnit('rev-1', {
          phaseId: 'review', unitIndex: 0, status: 'running', endedAt: 0, startedAt: 9_960_000,
        })],
      }),
    ], 'root');
    const call = nodeById(segmentsOf(view, 'root'), 'sub');
    const lap = call.attempts[0].chain[0].waves[0];
    expect(fanOf(lap.segments ?? [], 'review').layout).toBe('columns');
  });

  // §7, sole-child merge: a lane's ONE call renders headerless (its name moves
  // onto the lane header) and arrives open — the lane toggle is the one fold
  // control, so the composition offers no second one.
  it('a sole child composition is headerless and opened by its lane', () => {
    const fan = fanOf(segmentsOf(nestedFanView(), 'root'), 'port');
    const sole = fan.columns[0].chain[0];
    expect([sole.headerless, sole.collapsed, sole.toggleable]).toEqual([true, false, false]);
    // The lane's title carries the merged workflow's name — the header is the
    // only line that can say what the lane ran.
    expect(fan.columns[0].title).toBe('port-a · porter');
  });

  it('a settled sole child still opens with the lane click alone', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [
          makeUnit('port-a', { unitIndex: 0, status: 'done' }),
          makeUnit('port-live', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000 }),
        ],
      }),
      makeRun('port-a-child', {
        workflowId: 'porter', state: 'done',
        parentItemId: 'root', parentPhaseId: 'port', parentAttempt: 1, parentUnitId: 'port-a',
        skeleton: [skeletonPhase('land')],
        phases: [makePhase('land')],
      }),
    ], 'root');
    const lane = branchKeyOf('root', 'port', 1, 'port-a');
    // FOLDED, the lane already titles itself with the child it ran — the
    // chain is not built, and the title is the only line left to say it.
    const folded = fanOf(segmentsOf(view, 'root'), 'port').columns[0];
    expect([folded.collapsed, folded.chain.length, folded.title])
      .toEqual([true, 0, 'port-a · porter']);
    const chain = fanOf(segmentsOf(view, 'root', [], [lane]), 'port').columns[0].chain;
    expect(chain.map((node) => [node.itemId, node.headerless, node.collapsed, node.waves[0].segments !== null]))
      .toEqual([['port-a-child', true, false, true]]);
  });

  // The merge's guards, each a reviewed defect when absent: a FAILED child
  // keeps its own header row (that row carries its red glyph, and the lane
  // header carries the UNIT's signal, which need not agree)…
  it('a failed sole child keeps its own row instead of merging', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [
          makeUnit('port-a', { unitIndex: 0, status: 'done' }),
          makeUnit('port-live', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000 }),
        ],
      }),
      makeRun('port-a-child', {
        workflowId: 'porter', state: 'failed', reason: 'agent-error',
        parentItemId: 'root', parentPhaseId: 'port', parentAttempt: 1, parentUnitId: 'port-a',
        skeleton: [skeletonPhase('land')],
        phases: [makePhase('land', { status: 'failed' })],
      }),
    ], 'root');
    const lane = branchKeyOf('root', 'port', 1, 'port-a');
    const column = fanOf(segmentsOf(view, 'root', [], [lane]), 'port').columns[0];
    expect(column.chain.map((node) => [node.itemId, node.headerless]))
      .toEqual([['port-a-child', false]]);
  });

  // …and an ACTIONABLE lane (failed, taken-over — always open, no fold)
  // never merges a settled child: merging force-opens, and force-opening a
  // settled subtree under a lane with no toggle leaves no collapse anywhere
  // on it.
  it('an actionable lane leaves its settled sole child its own fold', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [
          makeUnit('port-a', { unitIndex: 0, status: 'failed' }),
          makeUnit('port-live', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000 }),
        ],
      }),
      makeRun('port-a-child', {
        workflowId: 'porter', state: 'done',
        parentItemId: 'root', parentPhaseId: 'port', parentAttempt: 1, parentUnitId: 'port-a',
        skeleton: [skeletonPhase('land')],
        phases: [makePhase('land')],
      }),
    ], 'root');
    const column = fanOf(segmentsOf(view, 'root'), 'port').columns[0];
    // The lane is open (actionable), the child keeps its row and its collapse.
    expect([column.collapsed, column.toggleable]).toEqual([false, false]);
    expect(column.chain.map((node) => [node.itemId, node.headerless, node.collapsed, node.toggleable]))
      .toEqual([['port-a-child', false, true, true]]);
  });

  // …and a lane with SIBLING children keeps a row per child: the merge only
  // exists because a sole child's header repeated the lane's, which is not
  // true of two.
  it('sibling child compositions keep their own rows and folds', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('port', { name: 'ports', shape: 'fan-out' })],
        phases: [makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
        units: [
          makeUnit('port-a', { unitIndex: 0, status: 'done' }),
          makeUnit('port-live', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000 }),
        ],
      }),
      ...['c1', 'c2'].map((itemId) => makeRun(itemId, {
        workflowId: 'porter', state: 'done',
        parentItemId: 'root', parentPhaseId: 'port', parentAttempt: 1, parentUnitId: 'port-a',
        skeleton: [skeletonPhase('land')],
        phases: [makePhase('land')],
      })),
    ], 'root');
    const lane = branchKeyOf('root', 'port', 1, 'port-a');
    const chain = fanOf(segmentsOf(view, 'root', [], [lane]), 'port').columns[0].chain;
    expect(chain.map((node) => [node.itemId, node.headerless, node.collapsed, node.toggleable]))
      .toEqual([['c1', false, true, true], ['c2', false, true, true]]);
  });
});

// ---------------------------------------------------------------- §3 loop

describe('§3 loop foot', () => {
  it('tail self-call | lap counter reads "lap N of ≤M" from the tail edge maxDepth', () => {
    const model = buildRunMap(campaignView(), NOW);
    expect(model.loop).toMatchObject({
      itemId: 'wave-3', phaseId: 'next', lapCount: 3, maxDepth: 5, waveCeiling: 6,
      lapLabel: 'lap 3 of ≤6', decided: null, showOutcomeStubs: true,
    });
  });

  // `max_depth` counts EDGE TRAVERSALS: `engine/calls.go#checkCallDepth` refuses
  // the call whose ancestry already holds that many, so max_depth 2 permits the
  // root plus two more waves. Reading the raw bound called the legal last wave
  // "lap 3 of ≤2" — the map telling a reader the engine broke its own rule.
  it('maxDepth 2 | the third wave is legal and the ceiling says so', () => {
    const waves = [
      makeRun('wave-1', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(2), phases: [makePhase('next')],
      }),
      makeRun('wave-2', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(2),
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1, phases: [makePhase('next')],
      }),
      makeRun('wave-3', {
        tailSelfCall: true, skeleton: campaignSkeleton(2),
        parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1, phases: [makePhase('audit')],
      }),
    ];
    const model = buildRunMap(makeView(waves), NOW);
    expect(model.waves.map((wave) => wave.ordinal)).toEqual([1, 2, 3]);
    expect(model.loop).toMatchObject({ lapCount: 3, maxDepth: 2, waveCeiling: 3, lapLabel: 'lap 3 of ≤3' });
  });

  it('maxDepth absent on the tail edge | "lap N" plus the budget line, no ≤M', () => {
    const view = makeView([makeRun('root', {
      tailSelfCall: true, skeleton: campaignSkeleton(0), phases: [makePhase('audit')],
      budget: runBudget({ kind: 'tokens', ceilingTokens: 50_000, spentTokens: 12_300, percent: 25 }),
    })]);
    const model = buildRunMap(view, NOW);
    expect(model.loop).toMatchObject({ lapLabel: 'lap 1', maxDepth: 0, waveCeiling: 0 });
    // The bound the reader is left with when the edge declares none. The loop
    // carries no `showBudget` flag deciding it: the strip states a ceiling
    // whenever one is in force, which is a superset of §3's rule.
    expect(model.budgetLabel).toBe('12.3k of 50.0k tokens');
  });

  // The chain is level order, so its TAIL is the deepest wave — but a lap can
  // hold two of them, and which one the BFS reaches last is an accident of the
  // parents' ordering. Taking the tail outright had the strip describing a dead
  // end ("done", no stubs) while the live wave beside it was the one the run is
  // in.
  it('two waves at the deepest lap | the foot describes the LIVE one, not the tail', () => {
    const view = makeView([
      makeRun('root', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next'), makePhase('next', { attempt: 2 })],
      }),
      makeRun('wave-live', {
        state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 1,
        phases: [makePhase('audit', { status: 'running', endedAt: 0 })],
      }),
      makeRun('wave-dead', {
        state: 'failed', reason: 'agent-error', tailSelfCall: true, skeleton: campaignSkeleton(),
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 2, startedAt: 2_000,
        phases: [makePhase('audit', { status: 'failed' })],
      }),
    ]);
    const model = buildRunMap(view, NOW);
    // The dead sibling is genuinely last in the chain — this is the shape.
    expect(model.waves.map((wave) => wave.itemId)).toEqual(['root', 'wave-live', 'wave-dead']);
    expect(model.loop).toMatchObject({ itemId: 'wave-live', showOutcomeStubs: true, decided: null });
  });

  it('nothing live | the foot falls back to the last wave of the chain', () => {
    const view = makeView([
      makeRun('root', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next')],
      }),
      makeRun('wave-2', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
        parentItemId: 'root', parentPhaseId: 'next', parentAttempt: 1,
        phases: [makePhase('audit')],
      }),
    ]);
    expect(buildRunMap(view, NOW).loop).toMatchObject({ itemId: 'wave-2', decided: 'done' });
  });

  it('a decided wave reads "loop"; the wave that produced no successor does not', () => {
    const view = campaignView();
    const first = segmentsOf(view, 'wave-1');
    const decision = first[first.length - 1];
    expect(decision.kind === 'decision' && decision.loop).toMatchObject({
      decided: 'loop', lapCount: 1, showOutcomeStubs: false,
    });
    expect(decision.key).toBe(nodeById(first, 'next').key);
  });

  it('done awaiting disposition | the loop is decided "done"', () => {
    const view = makeView([makeRun('root', {
      state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
      phases: [makePhase('audit'), makePhase('next')],
    })]);
    expect(buildRunMap(view, NOW).loop).toMatchObject({ decided: 'done', showOutcomeStubs: false });
  });

  it('soft-stop armed | the loop foot carries "stops after this wave"', () => {
    const view = makeView([makeRun('root', {
      softStop: true, tailSelfCall: true, skeleton: campaignSkeleton(), phases: [makePhase('audit')],
    })]);
    expect(buildRunMap(view, NOW).loop).toMatchObject({
      softStopArmed: true, softStopNote: 'stops after this wave',
    });
    expect(buildRunMap(campaignView(), NOW).loop?.softStopNote).toBe('');
  });

  // The flag lives on the tree ROOT and on the root alone — `engine.setSoftStop`
  // refuses a called run, and every wave's call boundary reads the root's row.
  // Reading it off the chain TAIL meant the note appeared only on a one-wave
  // campaign: the one campaign nobody arms a soft stop on.
  it('soft-stop on a MULTI-WAVE campaign | the foot reads the root, not the tail', () => {
    const wave = (itemId: string, over = {}) => makeRun(itemId, {
      tailSelfCall: true, skeleton: campaignSkeleton(), phases: [makePhase('next')], ...over,
    });
    const view = makeView([
      wave('wave-1', { softStop: true, state: 'done' }),
      wave('wave-2', { state: 'done', parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1 }),
      wave('wave-3', { parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1 }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.waves.map((w) => w.softStop)).toEqual([true, false, false]);
    expect(model.loop).toMatchObject({
      itemId: 'wave-3', softStopArmed: true, softStopNote: 'stops after this wave',
    });
    // …and the same is true of the foot drawn INSIDE an expanded wave.
    const segments = segmentsOf(view, 'wave-3');
    const decision = segments[segments.length - 1];
    expect(decision.kind === 'decision' && decision.loop.softStopArmed).toBe(true);
  });

  it('the tail phase renders as the foot, never as a phase node', () => {
    const segments = segmentsOf(campaignView(), 'wave-3');
    expect(segments.map((node) => [node.phaseId, node.kind]))
      .toEqual([['audit', 'phase'], ['fix', 'phase'], ['next', 'decision']]);
  });
});

// ---------------------------------------------------------------- frontier

describe('§5.4 + §13 frontier and follow priority', () => {
  it('running units are the leaves; their running phase is not double-reported', () => {
    const model = buildRunMap(fanView(), NOW);
    expect(model.frontier.map((entry) => [entry.kind, entry.label]))
      .toEqual([['unit', 'port-f'], ['unit', 'port-b']]);
    expect(model.followTarget?.needsHuman).toBe(true);
  });

  it('needs-human sorts before running, then deepest, then most recent transition', () => {
    const view = makeView([
      makeRun('root', {
        state: 'needs-human', reason: 'gate',
        skeleton: [skeletonPhase('a'), skeletonPhase('b', { shape: 'fan-out' })],
        phases: [
          makePhase('a', { status: 'running', endedAt: 0, startedAt: 9_999_000 }),
          makePhase('b', { status: 'parked', endedAt: 0, cause: 'awaiting approval' }),
        ],
        units: [
          makeUnit('b-1', { phaseId: 'b', status: 'running', endedAt: 0, startedAt: 9_000_000 }),
          makeUnit('b-2', { phaseId: 'b', unitIndex: 1, status: 'taken-over', endedAt: 0 }),
        ],
      }),
    ]);
    const model = buildRunMap(view, NOW);
    expect(model.frontier.map((entry) => [entry.label, entry.needsHuman, entry.depth]))
      .toEqual([['b-2', true, 2], ['b', true, 1], ['b-1', false, 2], ['a', false, 1]]);
    expect(model.followTarget?.label).toBe('b-2');
  });

  it('parallel parked leaves | all amber, priority decides only the follow target', () => {
    const view = makeView([makeRun('root', {
      state: 'needs-human', reason: 'unit-failed',
      skeleton: [skeletonPhase('a'), skeletonPhase('b')],
      phases: [
        makePhase('a', { status: 'parked', endedAt: 4_000, cause: 'first' }),
        makePhase('b', { status: 'parked', endedAt: 8_000, cause: 'second' }),
      ],
    })]);
    const model = buildRunMap(view, NOW);
    expect(model.frontier.every((entry) => entry.needsHuman && runMapTone(entry.signal) === 'text-warning'))
      .toBe(true);
    expect(model.followTarget?.label).toBe('b');
  });

  it('a superseded parked attempt is history: only the latest attempt can be the frontier', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('fix', { shape: 'fan-out' })],
      phases: [
        makePhase('fix', { status: 'parked', cause: 'resolved by hand', endedAt: 5_000 }),
        makePhase('fix', { attempt: 2, status: 'running', endedAt: 0, startedAt: 9_000_000 }),
      ],
      units: [
        makeUnit('u-old', { phaseId: 'fix', status: 'running', endedAt: 0 }),
        makeUnit('u-new', { phaseId: 'fix', attempt: 2, status: 'running', endedAt: 0 }),
      ],
    })]);
    const model = buildRunMap(view, NOW);
    expect(model.frontier.map((entry) => [entry.label, entry.attempt])).toEqual([['u-new', 2]]);
    // The superseded attempt is still drawn — history is never dropped.
    expect(nodeById(segmentsOf(view, 'root'), 'fix').attempts.map((attempt) => attempt.attempt)).toEqual([1, 2]);
  });

  // §7: "all-ghost segment; follow target = SEGMENT TOP". There is no leaf to
  // report, but "no leaf" is not "no target" — `openedRunning` is decided once,
  // at open, off this value, so a null here cost the whole visit its follow for
  // a run opened in the half-second before its first attempt landed.
  it('run just created, zero attempts | all-ghost segment, follow target = segment top', () => {
    const view = makeView([makeRun('root', {
      state: 'running', skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
    })]);
    const model = buildRunMap(view, NOW);
    expect(model.frontier).toHaveLength(1);
    expect(model.followTarget).toMatchObject({
      // Keyed on the WAVE, not on a phase: no node key matches, so the follow
      // controller resolves it to the wave's own row rather than guessing which
      // ghost is next.
      key: model.waves[0].key,
      waveItemId: 'root',
      label: 'wave 1',
      phaseId: '',
      attempt: 0,
      needsHuman: false,
      status: { kind: 'ghost' },
    });
    expect(segmentsOf(view, 'root').every((node) => node.ghost)).toBe(true);
  });

  it('parked before it ran anything | the segment top carries the blocker', () => {
    const view = makeView([makeRun('root', {
      state: 'needs-human', reason: 'setup-failed',
      skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
    })]);
    const target = buildRunMap(view, NOW).followTarget;
    expect(target).toMatchObject({
      needsHuman: true, reason: 'setup-failed', status: { kind: 'parked' },
    });
    expect(target?.reasonLabel).not.toBe('');
  });

  it('a first attempt replaces the segment top — the target moves, so follow moves', () => {
    const before = makeView([makeRun('root', {
      state: 'running', skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
    })]);
    const after = makeView([makeRun('root', {
      state: 'running', skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
      phases: [makePhase('plan', { status: 'running', endedAt: 0, startedAt: 9_000_000 })],
    })]);
    const first = buildRunMap(before, NOW).followTarget;
    const second = buildRunMap(after, NOW).followTarget;
    expect([first?.label, second?.label]).toEqual(['wave 1', 'plan']);
    expect(first?.key).not.toBe(second?.key);
  });

  it('annotates each entry with a breadcrumb path and the wave that owns it', () => {
    const view = campaignView();
    const entry = buildRunMap(view, NOW).frontier[0];
    expect(entry.path.map((part) => [part.kind, part.label]))
      .toEqual([['wave', 'wave 3'], ['phase', 'fix']]);
    expect([entry.waveItemId, entry.waveOrdinal]).toEqual(['wave-3', 3]);
  });

  it('a single-wave run keeps the wave out of the breadcrumb', () => {
    const entry = buildRunMap(fanView(), NOW).frontier[0];
    expect(entry.path.map((part) => part.kind)).toEqual(['phase', 'unit']);
  });

  it('descends through composition, deepening the path per call', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('call-out', { status: 'running', endedAt: 0 })],
      }),
      makeRun('mid', {
        workflowId: 'inner', state: 'running',
        parentItemId: 'root', parentPhaseId: 'call-out', parentAttempt: 1,
        skeleton: [skeletonPhase('deep', { shape: 'call', callTarget: 'deeper' })],
        phases: [makePhase('deep', { status: 'running', endedAt: 0 })],
      }),
      makeRun('leaf', {
        workflowId: 'deeper', state: 'running',
        parentItemId: 'mid', parentPhaseId: 'deep', parentAttempt: 1,
        skeleton: [skeletonPhase('work')],
        phases: [makePhase('work', { status: 'running', endedAt: 0, startedAt: 9_000_000 })],
      }),
    ], 'root');
    const model = buildRunMap(view, NOW);
    expect(model.frontier.map((entry) => entry.label)).toEqual(['work']);
    // The breadcrumb names the call SITE and the workflow it reached, at every
    // level: "call-out → inner → deep → deeper → work".
    expect(model.frontier[0].path.map((part) => [part.kind, part.label]))
      .toEqual([
        ['phase', 'call-out'], ['call', 'inner'],
        ['phase', 'deep'], ['call', 'deeper'],
        ['phase', 'work'],
      ]);
    expect(model.frontier[0].depth).toBe(5);
  });

  it('`auto_resume_at` | raw ms reaches the frontier chip, formatting stays component-side', () => {
    const view = makeView([makeRun('root', {
      state: 'needs-human', reason: 'stalled', autoResumeAt: 12_345_678,
      skeleton: [skeletonPhase('fix')],
      phases: [makePhase('fix', { status: 'parked', endedAt: 0, cause: 'rate limited' })],
    })]);
    const entry = buildRunMap(view, NOW).frontier[0];
    expect([entry.autoResumeAt, entry.cause, entry.reason, entry.reasonLabel])
      .toEqual([12_345_678, 'rate limited', 'stalled', 'Stalled']);
  });

  it.each([
    ['gate', 'Review gate'],
    ['budget-exhausted', 'Budget spent'],
    ['wiring-error', 'Wiring error'],
    ['checkpoint', 'Stopped at checkpoint'],
    ['unit-failed', 'Unit failed'],
    ['not-a-reason', 'Needs you'],
  ])('run reason %s | frontier label "%s" via workflowRunSignal', (reason, label) => {
    const view = makeView([makeRun('root', {
      state: 'needs-human', reason,
      skeleton: [skeletonPhase('fix')],
      phases: [makePhase('fix', { status: 'parked', endedAt: 0 })],
    })]);
    expect(buildRunMap(view, NOW).frontier[0].reasonLabel).toBe(label);
  });

  it('marks every node on a frontier path onFrontierPath', () => {
    const view = fanView();
    const node = nodeById(segmentsOf(view, 'root'), 'port');
    const fan = fanOf(segmentsOf(view, 'root'), 'port');
    expect(node.onFrontierPath).toBe(true);
    expect(node.attempts[0].onFrontierPath).toBe(true);
    expect(fan.columns.map((column) => [column.unit.unitId, column.onFrontierPath]))
      .toEqual([['port-b', true], ['port-c', false], ['port-f', true]]);
    expect(nodeById(segmentsOf(view, 'root'), 'plan').onFrontierPath).toBe(false);
    expect(buildRunMap(view, NOW).waves[0].onFrontierPath).toBe(true);
  });
});

// ---------------------------------------------------------------- summaries

describe('folded wave summaries', () => {
  it('carries duration, unit totals, outcome and retry count as separate parts', () => {
    const view = makeView([makeRun('root', {
      state: 'done', startedAt: 1_000, endedAt: 3_601_000,
      skeleton: [skeletonPhase('audit'), skeletonPhase('port', { shape: 'fan-out' })],
      phases: [
        makePhase('audit', { status: 'failed' }),
        makePhase('audit', { attempt: 2 }),
        makePhase('port'),
      ],
      units: [
        makeUnit('u-1', { status: 'done' }),
        makeUnit('u-2', { unitIndex: 1, status: 'failed' }),
        makeUnit('u-3', { unitIndex: 2, status: 'dropped' }),
      ],
    })]);
    const summary = buildRunMap(view, NOW).waves[0].summary;
    expect(summary.label).toBe('wave 1');
    expect(summary.duration).toBe('1h');
    expect(summary.unitsLabel).toBe('3 units · 1 failed · 1 dropped');
    expect(summary.retriesLabel).toBe('1 retry');
    expect(summary.retries).toBe(1);
    expect(summary.outcome).toEqual({ kind: 'done' });
    expect(summary.outcomeLabel).toBe('Done');
  });

  // ATTEMPTS past the first, not phases-that-were-retried: a phase that took
  // three tries is two retries, and counting buckets read it as one.
  it('counts retry ATTEMPTS, so a three-attempt phase is two retries', () => {
    const view = makeView([makeRun('root', {
      skeleton: [skeletonPhase('audit'), skeletonPhase('fix')],
      phases: [
        makePhase('audit', { status: 'failed' }),
        makePhase('audit', { attempt: 2, status: 'failed' }),
        makePhase('audit', { attempt: 3 }),
        makePhase('fix'),
      ],
    })]);
    const summary = buildRunMap(view, NOW).waves[0].summary;
    expect([summary.retries, summary.retriesLabel]).toEqual([2, '2 retries']);
  });

  it('a wave that called the next one reads "Looped" whatever its own row says', () => {
    const model = buildRunMap(campaignView(), NOW);
    expect(model.waves.map((wave) => wave.summary.outcome))
      .toEqual([{ kind: 'looped' }, { kind: 'looped' }, { kind: 'running' }]);
    expect(model.waves[0].summary.outcomeLabel).toBe('Looped');
  });

  // §7: the ROW's glyph follows the same reinterpretation as its word. A lap
  // that handed off is still `running` in the engine — its call phase is open
  // until the whole subtree rests — and the row rendered a live spinner beside
  // the word "Looped", contradicting itself once per settled lap.
  it('a wave that handed off wears a settled glyph, not the engine’s spinner', () => {
    // The engine's own shape, not a contrived one: a lap's call phase stays
    // OPEN while its child works, so every ancestor lap of a live campaign is
    // reported `running`.
    const view = makeView([
      makeRun('wave-1', {
        state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('next', { status: 'running', endedAt: 0 })],
      }),
      makeRun('wave-2', {
        state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        phases: [makePhase('audit', { status: 'running', endedAt: 0 })],
      }),
    ], 'wave-1');
    expect(buildRunMap(view, NOW).waves.map((wave) => [wave.status.kind, wave.signal]))
      .toEqual([['running', 'done'], ['running', 'running']]);
  });

  it('attention still wins: a lap that parked keeps its hue even though it handed off', () => {
    const view = makeView([
      makeRun('wave-1', {
        state: 'needs-human', reason: 'gate', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next')],
      }),
      makeRun('wave-2', {
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
    ]);
    expect(buildRunMap(view, NOW).waves[0].signal).toBe('parked');
  });

  it('a needs-human wave summary says the reason ONCE — the outcome word IS it', () => {
    const view = makeView([makeRun('root', { state: 'needs-human', reason: 'gate' })]);
    const summary = buildRunMap(view, NOW).waves[0].summary;
    expect(summary.outcome).toEqual({ kind: 'needs-human', reason: 'gate' });
    expect([summary.outcomeLabel, summary.reasonLabel]).toEqual(['Review gate', '']);
  });

  it('keeps the reason word where the outcome says something else', () => {
    // A wave that called the next one reads "Looped", so its own park reason is
    // the part that still has something to add.
    const view = makeView([
      makeRun('wave-1', {
        state: 'needs-human', reason: 'gate', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next')],
      }),
      makeRun('wave-2', {
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
    ]);
    const summary = buildRunMap(view, NOW).waves[0].summary;
    expect([summary.outcomeLabel, summary.reasonLabel]).toEqual(['Looped', 'Review gate']);
  });

  it('never repeats a part of the summary line', () => {
    for (const state of ['needs-human', 'failed', 'done', 'running', 'cancelled']) {
      const summary = buildRunMap(makeView([makeRun('root', { state, reason: 'gate' })]), NOW).waves[0].summary;
      const parts = [summary.outcomeLabel, summary.reasonLabel, summary.unitsLabel, summary.retriesLabel]
        .filter((part) => part !== '');
      expect(new Set(parts).size).toBe(parts.length);
    }
  });

  it('a running wave duration ticks from nowMs, never an ambient clock', () => {
    const view = makeView([makeRun('root', { state: 'running', startedAt: NOW - 125_000 })]);
    expect(buildRunMap(view, NOW).waves[0].summary.duration).toBe('2m');
    expect(buildRunMap(view, NOW + 60_000).waves[0].summary.duration).toBe('3m');
  });
});

// ------------------------------------------------------- §3 collapse policy

describe('§3 composition collapse — only the live path is open', () => {
  /** root → c1 → c2 → c3, none of them on a frontier path. */
  function nestedView(leafState = 'done'): WorkflowRunMapView {
    const link = (itemId: string, parent: string, workflowId: string, state: string) => makeRun(itemId, {
      workflowId, state, parentItemId: parent, parentPhaseId: 'call-out', parentAttempt: 1,
      skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'next-level' })],
      phases: [makePhase('call-out', { status: state === 'done' ? 'completed' : 'running', endedAt: 0 })],
      units: [],
    });
    return makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'level-1' })],
        phases: [makePhase('call-out', { status: 'completed' })],
      }),
      link('c1', 'root', 'level-1', 'done'),
      link('c2', 'c1', 'level-2', 'done'),
      link('c3', 'c2', 'level-3', leafState),
    ], 'root');
  }

  /** Walk `depth` composition levels down, expanding whatever the path names. */
  function chainAt(view: WorkflowRunMapView, expanded: string[], levels: number): RunMapCompositionNode {
    let node = nodeById(segmentsOf(view, 'root', expanded), 'call-out').attempts[0].chain[0];
    for (let level = 0; level < levels; level += 1) {
      node = (node.waves[0].segments ?? [])[0].attempts[0].chain[0];
    }
    return node;
  }

  // The inversion (V1): depth is not the question. A composition off the
  // frontier is one summary row at EVERY depth — including the first, which the
  // old two-free-levels rule painted in full and which is exactly where a
  // campaign's fan lanes hang their child workflows.
  it('collapses every composition off the frontier path, starting at the first level', () => {
    const view = nestedView();
    const first = chainAt(view, [], 0);
    expect([first.depth, first.collapsed, first.toggleable, first.waves]).toEqual([1, true, true, []]);
    expect(first.summary).toMatchObject({ runCount: 3, label: '3 runs' });
  });

  it('the frontier path is always expanded, and never offers a fold', () => {
    const view = nestedView('running');
    const first = chainAt(view, [], 0);
    expect([first.collapsed, first.toggleable, first.onFrontierPath]).toEqual([false, false, true]);
    const deep = chainAt(view, [], 2);
    expect([deep.itemId, deep.depth, deep.collapsed, deep.onFrontierPath]).toEqual(['c3', 3, false, true]);
    expect((deep.waves[0].segments ?? []).map((node) => node.phaseId)).toEqual(['call-out']);
  });

  it('an expanded composition id opens exactly that row, and no other', () => {
    const view = nestedView();
    const first = chainAt(view, ['c1'], 0);
    expect([first.itemId, first.collapsed]).toEqual(['c1', false]);
    const second = chainAt(view, ['c1'], 1);
    expect([second.itemId, second.collapsed]).toEqual(['c2', true]);
  });

  // Even an OPEN composition shows only its live lap: its settled laps fold to
  // the same rows a top-level lap folds to, off the same expansion set.
  it('an open composition folds its finished laps and opens the one the reader names', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('call-out', { status: 'running', endedAt: 0 })],
      }),
      makeRun('lap-1', {
        workflowId: 'inner', state: 'done', endedAt: 200_000,
        parentItemId: 'root', parentPhaseId: 'call-out', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('next')],
      }),
      makeRun('lap-2', {
        workflowId: 'inner', state: 'running',
        parentItemId: 'lap-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
      }),
    ], 'root');

    const folded = nodeById(segmentsOf(view, 'root', ['lap-1']), 'call-out').attempts[0].chain[0];
    expect(folded.waves.map((wave) => [wave.itemId, wave.folded, wave.segments === null]))
      .toEqual([['lap-1', true, true], ['lap-2', false, false]]);
    expect(folded.waves[0].summary.label).toBe('wave 1');

    // The reader opens lap 1 through the SAME `expandedWaveIds` a top-level lap
    // uses — a lap is a lap, wherever it sits.
    const opened = buildRunMap(view, NOW, {
      expandedWaveIds: ['root', 'lap-1'],
      expandedCompositionIds: ['lap-1'],
    }).waves[0].segments ?? [];
    const composition = nodeById(opened, 'call-out').attempts[0].chain[0];
    // `next` is the tail self-call, so it renders as the lap's loop foot.
    expect((composition.waves[0].segments ?? []).map((node) => node.phaseId))
      .toEqual(['audit', 'fix', 'next']);
  });

  // A composition the reader OPENED answers with content, not another fold: a
  // settled multi-lap chain defaults its LAST lap open — the ending is the
  // summary — with the earlier laps one click away. This is the "I have to
  // click MULTIPLE times to even see what completed" complaint, fixed at the
  // model.
  it('an opened settled composition defaults its final lap open, and a click closes it again', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('call-out', { status: 'completed' })],
      }),
      makeRun('lap-1', {
        workflowId: 'inner', state: 'done', endedAt: 200_000,
        parentItemId: 'root', parentPhaseId: 'call-out', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('next')],
      }),
      makeRun('lap-2', {
        workflowId: 'inner', state: 'done', endedAt: 400_000,
        parentItemId: 'lap-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('fix')],
      }),
    ], 'root');
    // Both settled laps own a fold (`folded`); the FINAL lap — the one whose
    // outcome the composition's row quotes — arrives with its segments built.
    const opened = nodeById(segmentsOf(view, 'root', ['lap-1']), 'call-out').attempts[0].chain[0];
    expect(opened.waves.map((wave) => [wave.itemId, wave.folded, wave.segments !== null]))
      .toEqual([['lap-1', true, false], ['lap-2', true, true]]);
    // A click INVERTS a lap's default off the same expansion set: naming the
    // final lap closes it, naming a history lap opens it.
    const clicked = nodeById(segmentsOf(view, 'root', ['lap-1'], [], ['lap-2']), 'call-out')
      .attempts[0].chain[0];
    expect(clicked.waves.map((wave) => [wave.itemId, wave.segments !== null]))
      .toEqual([['lap-1', false], ['lap-2', false]]);
  });

  it('the final lap is the tail LEAF, not the last chain position', () => {
    // lap-2a is a settled dead-end (its tail call was retried as lap-2b, which
    // carried the run to completion). Both are leaves and both default open;
    // lap-1, which handed off, stays folded shut — position in the chain
    // decides nothing.
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('call-out', { status: 'completed' })],
      }),
      makeRun('lap-1', {
        workflowId: 'inner', state: 'done', endedAt: 200_000,
        parentItemId: 'root', parentPhaseId: 'call-out', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('next')],
      }),
      makeRun('lap-2a', {
        workflowId: 'inner', state: 'failed', reason: 'agent-error', endedAt: 300_000,
        parentItemId: 'lap-1', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit')],
      }),
      makeRun('lap-2b', {
        workflowId: 'inner', state: 'done', endedAt: 400_000,
        parentItemId: 'lap-1', parentPhaseId: 'next', parentAttempt: 2,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit'), makePhase('fix')],
      }),
    ], 'root');
    const opened = nodeById(segmentsOf(view, 'root', ['lap-1']), 'call-out').attempts[0].chain[0];
    expect(opened.waves.map((wave) => [wave.itemId, wave.segments !== null]))
      .toEqual([['lap-1', false], ['lap-2a', true], ['lap-2b', true]]);
  });

  it('a collapsed composition still reports its subtree counts', () => {
    const view = makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('call-out', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('call-out')],
      }),
      makeRun('c1', {
        workflowId: 'inner', state: 'done', parentItemId: 'root', parentPhaseId: 'call-out', parentAttempt: 1,
        skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
        phases: [makePhase('port')],
        units: [makeUnit('u-1'), makeUnit('u-2', { unitIndex: 1 })],
      }),
    ], 'root');
    const composition = nodeById(segmentsOf(view, 'root'), 'call-out').attempts[0].chain[0];
    expect(composition.summary).toMatchObject({ runCount: 1, waveCount: 1, unitCount: 2, attemptCount: 1 });
    expect(composition.summary.label).toBe('1 run · 2 units');
  });
});

// ---------------------------------------------------------------- adversarial

describe('adversarial shapes', () => {
  it('a 256-wave chain builds in linear time with correct ordinals', () => {
    const runs: WorkflowRunMapRun[] = [];
    for (let index = 0; index < 256; index += 1) {
      runs.push(makeRun(`wave-${String(index).padStart(3, '0')}`, {
        state: index === 255 ? 'running' : 'done',
        tailSelfCall: true, skeleton: campaignSkeleton(300),
        startedAt: 1_000 + index, endedAt: index === 255 ? 0 : 2_000 + index,
        parentItemId: index === 0 ? '' : `wave-${String(index - 1).padStart(3, '0')}`,
        parentPhaseId: index === 0 ? '' : 'next',
        parentAttempt: index === 0 ? 0 : 1,
        phases: index === 255
          ? [makePhase('audit', { status: 'running', endedAt: 0 })]
          : [makePhase('audit'), makePhase('fix'), makePhase('next')],
      }));
    }
    const view = makeView(runs, 'wave-000');
    const started = Date.now();
    const model = buildRunMap(view, NOW);
    expect(Date.now() - started).toBeLessThan(1_000);
    expect(model.waves).toHaveLength(256);
    expect(model.waves[255].ordinal).toBe(256);
    expect(model.loop).toMatchObject({ lapCount: 256, lapLabel: 'lap 256 of ≤301' });
    expect(model.frontier).toHaveLength(1);
    expect(model.frontier[0].waveItemId).toBe('wave-255');
    expect(model.waves.filter((wave) => wave.folded)).toHaveLength(255);
  });

  it('a 32-unit fan keeps a bounded column set and exact chip arithmetic', () => {
    const units = Array.from({ length: 32 }, (_, index) => makeUnit(`u-${String(index).padStart(2, '0')}`, {
      unitIndex: index,
      status: index === 0 ? 'running' : index % 3 === 0 ? 'pending' : 'done',
      endedAt: index === 0 ? 0 : 2_000,
    }));
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [makePhase('port', { status: 'running', endedAt: 0 })],
      units,
    })]);
    const fan = fanOf(segmentsOf(view, 'root'), 'port');
    expect(fan.columns).toHaveLength(1);
    expect(fan.queued.count + fan.done.count + fan.columns.length).toBe(32);
  });

  it('units sort by unitIndex then id, joins last, whatever order they arrive in', () => {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('port', { shape: 'fan-out' })],
      phases: [makePhase('port', { status: 'running', endedAt: 0 })],
      units: [
        makeUnit('z-join', { unitIndex: 0, kind: 'join', status: 'pending' }),
        makeUnit('c', { unitIndex: 2, status: 'running', endedAt: 0 }),
        makeUnit('a', { unitIndex: 1, status: 'running', endedAt: 0 }),
        makeUnit('b', { unitIndex: 1, status: 'running', endedAt: 0 }),
      ],
    })]);
    const fan = fanOf(segmentsOf(view, 'root'), 'port');
    expect(fan.columns.map((column) => column.unit.unitId)).toEqual(['a', 'b', 'c']);
    expect(fan.join?.unitId).toBe('z-join');
  });

  it('an orphaned run (parent absent from the view) still renders as its own map', () => {
    const view = makeView([makeRun('orphan', {
      parentItemId: 'gone', parentPhaseId: 'next', parentAttempt: 1,
      state: 'running', skeleton: [skeletonPhase('plan')], phases: [makePhase('plan')],
    })], 'orphan');
    expect(buildRunMap(view, NOW).waves.map((wave) => [wave.itemId, wave.ordinal])).toEqual([['orphan', 1]]);
  });
});

describe('runMapPosition — the header\'s narrow read', () => {
  it('names the wave and the deepest part of the frontier path', () => {
    expect(runMapPosition(campaignView())).toEqual({ wave: 'wave 3', leaf: 'fix' });
  });

  it('follows the same priority as the strip — the parked leaf, not the running one', () => {
    const view = makeView([
      makeRun('wave-1', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('next')],
      }),
      makeRun('wave-2', {
        state: 'needs-human', reason: 'gate', parentItemId: 'wave-1',
        parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [
          makePhase('audit', { status: 'parked', endedAt: 0, cause: 'awaiting approval' }),
          makePhase('fix', { status: 'running', endedAt: 0, startedAt: 9_000_000 }),
        ],
      }),
    ], 'wave-1');
    expect(runMapPosition(view)).toEqual({ wave: 'wave 2', leaf: 'audit' });
  });

  it('leaves the wave part empty for a single-wave run', () => {
    expect(runMapPosition(fanView())).toEqual({ wave: '', leaf: 'port-f' });
  });

  it('is null when there is nothing to name', () => {
    expect(runMapPosition(makeView([], ''))).toBeNull();
    expect(runMapPosition(makeView([makeRun('root', { state: 'done' })]))).toBeNull();
  });

  it('agrees with the full model, so the header and the strip cannot disagree', () => {
    const target = buildRunMap(campaignView(), NOW).followTarget;
    const wave = target?.path.find((part) => part.kind === 'wave');
    const leaf = target?.path[target.path.length - 1];
    expect(runMapPosition(campaignView()))
      .toEqual({ wave: wave?.label ?? '', leaf: wave === leaf ? '' : leaf?.label ?? '' });
  });
});

describe('ghostAttempt — the row a node with no records renders as', () => {
  function ghostNodes(): RunMapSegmentNode[] {
    const view = makeView([makeRun('root', {
      state: 'running',
      skeleton: [
        skeletonPhase('plan'),
        skeletonPhase('port', { shape: 'fan-out' }),
        skeletonPhase('verify'),
      ],
      phases: [makePhase('plan')],
    })]);
    return segmentsOf(view, 'root');
  }

  it('carries the node forward with nothing an attempt would have recorded', () => {
    const node = nodeById(ghostNodes(), 'verify');
    expect(node.attempts).toHaveLength(0);
    expect(ghostAttempt(node)).toEqual({
      key: node.key,
      phaseId: 'verify',
      // Records are 1-based; 0 is the ordinal no attempt can have.
      attempt: 0,
      label: node.label,
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
    });
  });

  it('names a pre-expansion fan by where its units come from, never by a count', () => {
    const fan = nodeById(ghostNodes(), 'port');
    expect(fan.kind).toBe('fan');
    expect(ghostAttempt(fan).label).toBe(fan.kind === 'fan' ? fan.ghostLabel : '');
    expect(ghostAttempt(fan).label).not.toMatch(/\d/);
  });

  it('keys the ghost row on the node, so it is the row `now ▸` can mark', () => {
    const node = nodeById(ghostNodes(), 'verify');
    expect(ghostAttempt(node).key).toBe(node.key);
    expect(ghostAttempt(node).key).not.toBe('');
  });
});

describe('runMapTone', () => {
  it.each([
    ['ghost', 'text-fg-hint'],
    ['unknown', 'text-fg-muted'],
    ['failed', 'text-error'],
    ['parked', 'text-warning'],
    ['running', 'text-fg-muted'],
    ['done', 'text-fg-muted'],
    ['pending', 'text-fg-muted'],
    ['dropped', 'text-fg-muted'],
  ] as const)('%s → %s', (signal, tone) => {
    expect(runMapTone(signal)).toBe(tone);
  });
});

/**
 * The shared 1Hz clock's gate. It is a question about CLOCK-DEPENDENT VALUES,
 * not about run state: every park writes an `ended_at` at every level
 * (`engine/fsm.go#transition`), so a tree waiting on a human has no open span
 * and nothing on it changes second to second.
 */
describe('runMapViewIsLive', () => {
  const settled = (over: Partial<WorkflowRunMapRun> = {}): WorkflowRunMapRun =>
    makeRun('root', { state: 'done', startedAt: 1_000, endedAt: 2_000, ...over });

  it('is false for a settled tree — nothing left to tick', () => {
    expect(runMapViewIsLive(makeView([settled(), settled({ itemId: 'b', state: 'cancelled' })]))).toBe(false);
    expect(runMapViewIsLive(makeView([]))).toBe(false);
  });

  it('is true while any span is still open, at any level', () => {
    expect(runMapViewIsLive(makeView([settled({ state: 'running', endedAt: 0 })]))).toBe(true);
    expect(runMapViewIsLive(makeView([settled({
      phases: [makePhase('p', { status: 'running', startedAt: 1_000, endedAt: 0 })],
    })]))).toBe(true);
    expect(runMapViewIsLive(makeView([settled({
      units: [makeUnit('u', { status: 'running', startedAt: 1_000, endedAt: 0 })],
    })]))).toBe(true);
  });

  it('is FALSE for a parked tree: the park closed every span it left behind', () => {
    // The whole point. A human reading a parked campaign was rebuilding the
    // entire model once a second, for as long as they looked at it, to redraw
    // a page on which nothing moves.
    const parked = makeView([settled({
      state: 'needs-human', reason: 'gate',
      phases: [makePhase('audit'), makePhase('gate', { status: 'parked', startedAt: 1_500, endedAt: 2_000 })],
      units: [makeUnit('u-1', { status: 'done' }), makeUnit('u-2', { unitIndex: 1, status: 'pending', startedAt: 0, endedAt: 0 })],
    })]);
    expect(runMapViewIsLive(parked)).toBe(false);
  });

  it('is true for a parked tree that is counting down to its own resume', () => {
    // `auto_resume_at` renders a countdown chip, which is clock-dependent even
    // though every span around it is closed.
    expect(runMapViewIsLive(makeView([settled({
      state: 'needs-human', reason: 'stalled', autoResumeAt: NOW + 60_000,
    })]))).toBe(true);
  });

  it('is true for a park whose unit a human took over and is still running', () => {
    // A taken-over unit's own span is closed, but a sibling that is still
    // running is a real open span and must keep the clock armed.
    expect(runMapViewIsLive(makeView([settled({
      state: 'needs-human', reason: 'taken-over',
      units: [
        makeUnit('u-1', { status: 'taken-over' }),
        makeUnit('u-2', { unitIndex: 1, status: 'running', startedAt: 1_500, endedAt: 0 }),
      ],
    })]))).toBe(true);
  });
});

describe('§5.9 orphan root', () => {
  // A root whose named parent's row is gone: the server resolves the tree to
  // the last run that exists and leaves the dangling reference on the wire,
  // because "a parent id naming no run in `runs`" IS the orphan state. The
  // projection must read it as a root rather than as a run with a parent.
  it('treats a root naming an absent parent as the root of its own chain', () => {
    const view = makeView([
      makeRun('wave-2', {
        parentItemId: 'wave-1-deleted', parentPhaseId: 'next', parentAttempt: 1,
        state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
        phases: [makePhase('audit')],
      }),
      makeRun('wave-3', {
        parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1,
        tailSelfCall: true, skeleton: campaignSkeleton(),
      }),
    ], 'wave-2');
    const model = buildRunMap(view, NOW);
    // Ordinals are chain-local and start at 1 whatever the missing ancestry
    // said: the map never invents laps it cannot see.
    expect(model.waves.map((wave) => [wave.itemId, wave.ordinal])).toEqual([['wave-2', 1], ['wave-3', 2]]);
    expect(model.rootItemId).toBe('wave-2');
    expect(segmentsOf(view, 'wave-2').map((node) => node.phaseId)).toEqual(['audit', 'fix', 'next']);
  });
});
