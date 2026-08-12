import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import {
  __resetWorkflowRunMapStoreForTest,
  __workflowRunMapKeysForTest,
  applyWorkflowRunMapItemState,
  applyWorkflowRunMapPhaseState,
  applyWorkflowRunMapSoftStop,
  attachWorkflowRunMap,
  peekWorkflowRunMap,
  peekWorkflowRunMapError,
  resyncWorkflowRunMapAfterGap,
  INVALIDATE_DEBOUNCE_MS,
} from './workflowRunMap.svelte';
import { applyTransportGap } from './eventsTransportGap';
import type {
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowRunMapRun,
  WorkflowRunMapView,
} from '../types/workflow';
import {
  mapRun,
  mapUnit,
  mapView,
  phaseAttempt,
  refusedView,
  runBudget,
  runSpend,
  skeletonPhase,
} from '../../test/fixtures/runMap';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

const ROOT = 'root';
const WAVE_2 = 'wave-2';

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

/** Advance past the patch-failure debounce and let the refetch land. */
async function settleInvalidate(): Promise<void> {
  vi.advanceTimersByTime(INVALIDATE_DEBOUNCE_MS);
  await flush();
}

// Through the generated binding classes, like every other run-map fixture: a
// hand-written literal typed as the wire shape is by construction unable to
// catch the wire CHANGING under it, which is the one thing this suite's
// patch-equals-refetch claim rests on.
function run(itemId: string, overrides: Partial<WorkflowRunMapRun> = {}): WorkflowRunMapRun {
  return mapRun(itemId, {
    skeleton: [
      skeletonPhase('plan'),
      skeletonPhase('ports', { shape: 'fan-out' }),
      skeletonPhase('again', { shape: 'call', callTarget: 'campaign', maxDepth: 8 }),
    ],
    tailSelfCall: true,
    startedAt: undefined,
    ...overrides,
  });
}

/**
 * A two-wave campaign: wave 1 done, wave 2 running its fan-out with one unit
 * finished and one still queued. Deliberately the shape every patch rule has
 * to keep true — an open attempt, a terminal attempt, and both unit states.
 */
function campaign(): WorkflowRunMapView {
  return mapView([
    run(ROOT, {
      state: 'done',
      startedAt: 1_000,
      endedAt: 2_000,
      spend: runSpend({ costUsd: 1.5, wireCostUsd: 1.5 }),
      budget: runBudget({ ceilingUsd: 10, spentUsd: 1.5, percent: 15 }),
      phases: [
        phaseAttempt('plan', { startedAt: 1_000, endedAt: 1_500 }),
        phaseAttempt('again', { startedAt: 1_500, endedAt: 2_000 }),
      ],
    }),
    run(WAVE_2, {
      parentItemId: ROOT,
      parentPhaseId: 'again',
      parentAttempt: 1,
      callDepth: 1,
      startedAt: 2_000,
      phases: [
        phaseAttempt('plan', { startedAt: 2_000, endedAt: 2_400 }),
        phaseAttempt('ports', { status: 'running', startedAt: 2_400, endedAt: undefined }),
      ],
      units: [
        mapUnit('alpha', { phaseId: 'ports', status: 'done', startedAt: 2_400, endedAt: 2_600 }),
        mapUnit('beta', {
          phaseId: 'ports', unitIndex: 1, provider: 'claude', status: 'pending',
          startedAt: undefined, endedAt: undefined,
        }),
      ],
    }),
  ], ROOT);
}

function phaseEvent(overrides: Partial<WorkflowPhaseStateEvent> = {}): WorkflowPhaseStateEvent {
  return {
    itemId: WAVE_2, phaseId: 'ports', attempt: 1, status: 'running', occurredAt: 3_000, ...overrides,
  };
}

function itemEvent(overrides: Partial<WorkflowItemStateEvent> = {}): WorkflowItemStateEvent {
  return { itemId: WAVE_2, projectId: 'p1', from: 'running', to: 'needs-human', ...overrides };
}

/** The RPC mock, answering a fresh copy so a patch can never mutate the fixture. */
function installRunMapMock(view: () => WorkflowRunMapView = campaign) {
  return setBindingMock('WorkflowGetRunMap', async () => structuredClone(view()));
}

// Held so the suite releases what it attached. An attachment that outlives its
// test is re-sourced by the next test's transport reset, against binding mocks
// that no longer exist — noisy on purpose, and worth not generating here.
const holds: { release(): void }[] = [];

async function attached(key = ROOT) {
  const attachment = attachWorkflowRunMap(key);
  holds.push(attachment);
  await flush();
  return attachment;
}

beforeEach(() => {
  vi.useFakeTimers();
  installRunMapMock();
});

afterEach(() => {
  for (const hold of holds.splice(0)) hold.release();
  __resetWorkflowRunMapStoreForTest();
  vi.useRealTimers();
});

describe('workflowRunMap — attach, source, release', () => {
  it('sources the tree once for every holder and drops the entry on the last release', async () => {
    const a = await attached();
    const b = await attached();

    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledWith(ROOT);
    expect(a.current).toEqual(campaign());
    // The point of the entity store: one observation, not a copy per holder.
    expect(b.current).toBe(a.current);
    expect(peekWorkflowRunMap(ROOT)).toBe(a.current);
    expect(peekWorkflowRunMapError(ROOT)).toBeNull();
    expect(__workflowRunMapKeysForTest()).toEqual([ROOT]);

    a.release();
    expect(__workflowRunMapKeysForTest()).toEqual([ROOT]);
    b.release();
    expect(__workflowRunMapKeysForTest()).toEqual([]);
    expect(peekWorkflowRunMap(ROOT)).toBeNull();
  });

  it('keys on the id the UI asked for, whatever root the answer resolves to', async () => {
    // A deep link to a child: the server normalises to the root (§5.9), and the
    // entry stays addressable by what the caller could actually state.
    const attachment = await attached(WAVE_2);
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledWith(WAVE_2);
    expect(attachment.current?.rootItemId).toBe(ROOT);
    expect(__workflowRunMapKeysForTest()).toEqual([WAVE_2]);
  });

  it('surfaces a failed source as user-facing error state', async () => {
    setBindingMock('WorkflowGetRunMap', async () => {
      throw new Error('run map unavailable');
    });
    const attachment = await attached();
    expect(attachment.current).toBeNull();
    expect(attachment.error).toContain('run map unavailable');
  });
});

describe('workflowRunMap — patch equivalence', () => {
  it('starts a queued unit and finishes it exactly as a refetch would', async () => {
    await attached();

    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'running', occurredAt: 3_000 }));
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'done', occurredAt: 3_800 }));

    const expected = campaign();
    expected.runs[1]!.units[1] = {
      phaseId: 'ports', attempt: 1, unitId: 'beta', unitIndex: 1, kind: 'unit',
      provider: 'claude', status: 'done', unitAttempt: 1, startedAt: 3_000, endedAt: 3_800,
    };
    // Patching is the whole point: both rows are right before anything refetches.
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
    // The `running` frame schedules ONE reconcile (the thread it cannot carry);
    // the `done` frame inside the same window adds none.
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('leaves the untouched runs and rows identical objects', async () => {
    const before = (await attached()).current!;

    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'running' }));

    const after = peekWorkflowRunMap(ROOT)!;
    expect(after).not.toBe(before);
    expect(after.runs[0]).toBe(before.runs[0]);
    expect(after.runs[1]!.phases).toBe(before.runs[1]!.phases);
    expect(after.runs[1]!.units[0]).toBe(before.runs[1]!.units[0]);
  });

  it('opens a phase attempt on its running event — that IS attempt creation', async () => {
    await attached();

    applyWorkflowRunMapPhaseState(phaseEvent({ phaseId: 'ports', attempt: 2, status: 'running', occurredAt: 4_000 }));

    const expected = campaign();
    // Appended, which is where a refetch puts it too: the rows come back
    // ordered by started_at and this one's is the newest.
    expected.runs[1]!.phases.push({ phaseId: 'ports', attempt: 2, status: 'running', startedAt: 4_000 });
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);
  });

  it('completes an open attempt in place', async () => {
    await attached();

    applyWorkflowRunMapPhaseState(phaseEvent({ phaseId: 'ports', attempt: 1, status: 'completed', occurredAt: 4_200 }));

    const expected = campaign();
    expected.runs[1]!.phases[1] = {
      phaseId: 'ports', attempt: 1, status: 'completed', startedAt: 2_400, endedAt: 4_200,
    };
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);
  });

  it('patches a run state and reason, and omits the reason when the event clears it', async () => {
    await attached();

    applyWorkflowRunMapItemState(itemEvent({ to: 'needs-human', reason: 'gate' }));
    let expected = campaign();
    expected.runs[1]!.state = 'needs-human';
    expected.runs[1]!.reason = 'gate';
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);

    applyWorkflowRunMapItemState(itemEvent({ from: 'needs-human', to: 'running' }));
    expected = campaign();
    expected.runs[1]!.state = 'running';
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);
    expect(peekWorkflowRunMap(ROOT)!.runs[1]).not.toHaveProperty('reason');
  });

  it('arms and withdraws soft stop on the run it names', async () => {
    await attached();

    applyWorkflowRunMapSoftStop({ itemId: ROOT, armed: true });
    const expected = campaign();
    expected.runs[0]!.softStop = true;
    expect(peekWorkflowRunMap(ROOT)).toEqual(expected);

    applyWorkflowRunMapSoftStop({ itemId: ROOT, armed: false });
    expect(peekWorkflowRunMap(ROOT)).toEqual(campaign());
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('routes an event by tree membership, so a child run patches the map of its root', async () => {
    await attached(WAVE_2);
    // The key names wave 2; the event names the root, which the same view
    // contains. Without the member index this frame would reach nothing.
    applyWorkflowRunMapSoftStop({ itemId: ROOT, armed: true });
    expect(peekWorkflowRunMap(WAVE_2)!.runs[0]!.softStop).toBe(true);
  });
});

/**
 * The strongest equivalence there is: patch the store, then persist the SAME
 * transition into the mock's backing fixture and refetch through it. The
 * expected value is what the server would have answered, not what the test
 * author believed it would answer — a patch that quietly diverges from the
 * refetch is exactly the failure the store's whole design is guarding against.
 */
interface Equivalence {
  name: string;
  events: WorkflowPhaseStateEvent[];
  persist: (view: WorkflowRunMapView) => void;
}

const EQUIVALENT: Equivalence[] = [
  {
    name: 'a queued unit starts',
    events: [phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'running', occurredAt: 3_000 })],
    persist: (view) => {
      const unit = view.runs[1]!.units[1]!;
      unit.status = 'running';
      unit.startedAt = 3_000;
    },
  },
  {
    name: 'a unit runs and finishes',
    events: [
      phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'running', occurredAt: 3_000 }),
      phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'done', occurredAt: 3_800 }),
    ],
    persist: (view) => {
      const unit = view.runs[1]!.units[1]!;
      unit.status = 'done';
      unit.startedAt = 3_000;
      unit.endedAt = 3_800;
    },
  },
  {
    name: 'an attempt is inserted by its running frame',
    events: [phaseEvent({ phaseId: 'ports', attempt: 2, status: 'running', occurredAt: 4_000 })],
    persist: (view) => {
      view.runs[1]!.phases.push({ phaseId: 'ports', attempt: 2, status: 'running', startedAt: 4_000 });
    },
  },
];

describe('workflowRunMap — patch equals refetch', () => {
  it.each(EQUIVALENT)('$name', async ({ events, persist }) => {
    const backing = campaign();
    installRunMapMock(() => backing);
    await attached();

    for (const event of events) applyWorkflowRunMapPhaseState(event);
    const patched = peekWorkflowRunMap(ROOT);
    expect(patched).not.toEqual(campaign());

    // The engine's own write, then the answer the server would give for it.
    persist(backing);
    resyncWorkflowRunMapAfterGap();
    await flush();
    expect(patched).toEqual(peekWorkflowRunMap(ROOT));
  });
});

describe('workflowRunMap — reconciliation fallbacks', () => {
  it('reconciles a row that just opened, whose thread no event can carry', async () => {
    await attached();

    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'running', occurredAt: 3_000 }));
    // The row turns live instantly — that is what the patch is for.
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.units[1]!.status).toBe('running');
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
    // The runner attaches the thread AFTER the engine emits, and nothing
    // announces it; without this the node renders running and unclickable.
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('reconciles an attempt that just opened, for the same reason', async () => {
    await attached();

    applyWorkflowRunMapPhaseState(phaseEvent({ phaseId: 'ports', attempt: 2, status: 'running', occurredAt: 4_000 }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.phases).toHaveLength(3);
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('does not reconcile a frame that CLOSES a row — nothing follows it', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, status: 'done', occurredAt: 3_800 }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('ignores a later transition of a run in some OTHER tree', async () => {
    await attached();
    // The channel is a broadcast over the whole project. Only a BIRTH
    // (`from: ""`) can mean a watched tree grew; every later frame of a run no
    // view contains is somebody else's, and refetching for it makes the open
    // map's refetch rate proportional to the project (§4.3.3).
    applyWorkflowRunMapItemState({ itemId: 'elsewhere', projectId: 'p1', from: 'running', to: 'needs-human' });
    applyWorkflowRunMapItemState({ itemId: 'elsewhere', projectId: 'p1', from: 'needs-human', to: 'done' });
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('refetches rather than guessing at a phase status it cannot place', async () => {
    await attached();

    // Terminal statuses that carry an engine cause the event does not.
    applyWorkflowRunMapPhaseState(phaseEvent({ phaseId: 'ports', attempt: 1, status: 'parked' }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.phases[1]!.status).toBe('running');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('refetches on a non-running event for an attempt it has never seen', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ phaseId: 'ports', attempt: 3, status: 'completed' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('refetches on a unit it has never seen rather than inventing a branch', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'gamma', unitIndex: 2, status: 'running' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('refetches when a finished unit runs again — the attempt bump is unmodelable', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'alpha', unitIndex: 0, status: 'running' }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.units[0]!.status).toBe('done');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('refetches on an unstamped phase event instead of stamping a client clock', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', unitIndex: 1, occurredAt: 0 }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.units[1]!.status).toBe('pending');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('refetches every watched map on a transition for a run it does not know', async () => {
    await attached(ROOT);
    await attached(WAVE_2);
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);

    // A child's first transition (`from: ""`) is how a new wave is born; the
    // map that has to grow it is the one watching its parent, and the event
    // names an id no view contains yet.
    applyWorkflowRunMapItemState({ itemId: 'wave-3', projectId: 'p1', from: '' as never, to: 'running' });
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(4);
  });

  it('reconciles a terminal run, whose endedAt no later event can carry', async () => {
    await attached();

    applyWorkflowRunMapItemState(itemEvent({ to: 'done' }));
    // The state flips instantly — that is what the patch is for — and the
    // refetch behind it is what brings endedAt and the tree's spend.
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.state).toBe('done');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('reconciles a park: the engine wrote endedAt and a cause the frame cannot carry', async () => {
    // A park is a run-level write of state, reason, `ended_at` and (through the
    // attempt teardown) an engine-diagnosed cause. The frame carries the first
    // two, so the map flips instantly and the refetch behind it brings the rest.
    await attached();
    applyWorkflowRunMapItemState(itemEvent({ to: 'needs-human', reason: 'gate' }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.state).toBe('needs-human');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('does NOT reconcile a frame that restates the row exactly', async () => {
    // The one item-state shape that says nothing new: a running continuation
    // the view already agrees with. Refetching for it would make the map's
    // refetch rate proportional to the channel rather than to what changed.
    await attached();
    applyWorkflowRunMapItemState(itemEvent({ itemId: WAVE_2, from: 'running', to: 'running' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('reconciles a take-over: the REASON moved, and its intervention has no event', async () => {
    // `engine.takeOver` on an already-parked run writes the attempt's
    // intervention record and re-parks with a new reason — same state, no phase
    // event at all. Without a reconcile the "touched by hand" marker never
    // appeared until something unrelated refetched.
    await attached();
    applyWorkflowRunMapItemState(itemEvent({ from: 'needs-human', to: 'needs-human', reason: 'gate' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);

    applyWorkflowRunMapItemState(itemEvent({ from: 'needs-human', to: 'needs-human', reason: 'taken-over' }));
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.reason).toBe('taken-over');
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(3);
  });

  it('reconciles a disposition park, which REWRITES an endedAt the run already had', async () => {
    // `engine.parkDisposition` moves a done run to needs-human(disposition) and
    // stamps a fresh `ended_at` over the one the completion wrote. Nothing on
    // the wire says so.
    await attached();
    applyWorkflowRunMapItemState(itemEvent({ itemId: ROOT, from: 'done', to: 'needs-human', reason: 'disposition' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('reconciles a running frame the view already agrees with — that is the WORST case', async () => {
    // A fetch delivered the attempt as running and threadless (the runner
    // attaches the thread after the emit). The patch is then `unchanged`, and
    // this frame is the only prompt anything has to go back for the thread.
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({
      phaseId: 'ports', attempt: 1, status: 'running', occurredAt: 2_400,
    }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });

  it('coalesces a burst of unplaceable events into ONE refetch per key', async () => {
    await attached();

    for (let index = 0; index < 12; index += 1) {
      applyWorkflowRunMapPhaseState(phaseEvent({ unitId: `ghost-${index}`, status: 'running' }));
    }
    vi.advanceTimersByTime(INVALIDATE_DEBOUNCE_MS - 1);
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(1);
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);

    // The window is not a standing timer: the next burst opens a fresh one.
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'ghost-a', status: 'running' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(3);
  });

  it('drops a pending refetch for a run the user navigated away from', async () => {
    const attachment = await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'ghost', status: 'running' }));
    attachment.release();
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });
});

describe('workflowRunMap — the member index', () => {
  it('rebuilds from each applied view and stops routing what the tree lost', async () => {
    let view = campaign();
    installRunMapMock(() => view);
    await attached();

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(true);

    // A re-source whose answer no longer contains wave 2 (discarded run):
    // events for it must stop reaching this key.
    view = { rootItemId: ROOT, runs: [run(ROOT)] };
    resyncWorkflowRunMapAfterGap();
    await flush();
    expect(peekWorkflowRunMap(ROOT)!.runs).toHaveLength(1);

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    expect(peekWorkflowRunMap(ROOT)!.runs[0]!.softStop).toBe(false);
    // …and wave 2's BIRTH would grow the tree again, so that one reconciles.
    applyWorkflowRunMapItemState({ itemId: WAVE_2, projectId: 'p1', from: '' as never, to: 'running' });
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(3);
  });

  it('survives a superseded fetch resolving after the one that replaced it', async () => {
    const pending: ((view: WorkflowRunMapView) => void)[] = [];
    setBindingMock('WorkflowGetRunMap', () => new Promise<WorkflowRunMapView>((resolve) => {
      pending.push(resolve);
    }));
    await attached();

    // A gap re-sources while the first fetch is still out; the second answers
    // first and installs the routing.
    resyncWorkflowRunMapAfterGap();
    await flush();
    expect(pending).toHaveLength(2);
    pending[1]!(campaign());
    await flush();

    // The first fetch now lands and runs its cleanup. It belongs to a world
    // that is gone, so it must take nothing live with it.
    pending[0]!(campaign());
    await flush();

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(true);
  });

  it('clears with the entry, so a released key routes nothing', async () => {
    const attachment = await attached();
    attachment.release();

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'beta', status: 'running' }));
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
    expect(peekWorkflowRunMap(ROOT)).toBeNull();
  });
});

/**
 * The window between a fetch going out and its answer landing. Everything here
 * is about ONE hazard: the answer is a snapshot of the tree as it was when the
 * READ ran, so a frame that arrives while it is in the air describes something
 * newer than the thing about to overwrite it.
 */
describe('workflowRunMap — events during an outstanding fetch', () => {
  /** A mock whose answers are resolved by hand, so the window can be held open. */
  function deferredMock(): {
    resolve: (view: WorkflowRunMapView) => void;
    /** Remove the oldest pending answer WITHOUT settling it, to settle later. */
    take: () => ((view: WorkflowRunMapView) => void) | undefined;
    pending: number;
  } {
    const waiting: ((view: WorkflowRunMapView) => void)[] = [];
    setBindingMock('WorkflowGetRunMap', () => new Promise<WorkflowRunMapView>((res) => {
      waiting.push(res);
    }));
    return {
      resolve: (view) => waiting.shift()?.(view),
      take: () => waiting.shift(),
      get pending() { return waiting.length; },
    };
  }

  it('keeps routing events to a key whose replacement fetch is still in flight', async () => {
    // The index belongs to the ENTRY. Releasing it per source run left the key
    // deaf for the whole width of the re-source, and every frame for the tree
    // in that window was dropped with nothing to notice it.
    await attached();
    const mock = deferredMock();
    resyncWorkflowRunMapAfterGap();
    await flush();
    expect(mock.pending).toBe(1);

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(true);
  });

  it('re-sources after an answer that may have buried a patch it predates', async () => {
    await attached();
    const mock = deferredMock();
    resyncWorkflowRunMapAfterGap();
    await flush();

    // The park lands while the fetch is out; the answer, read before it
    // happened, says the run is still running.
    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    mock.resolve(campaign());
    await flush();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(false);

    // …so the apply that buried it schedules the reconcile that heals it.
    setBindingMock('WorkflowGetRunMap', async () => {
      const healed = campaign();
      healed.runs[1]!.softStop = true;
      return healed;
    });
    await settleInvalidate();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(true);
  });

  it('a run that parks mid-fetch does not stay drawn as running forever', async () => {
    // The end-to-end shape of the bug: a park emits ONE frame, so a fetch that
    // overwrites it is the last word — the map showed a parked campaign as
    // running until the user navigated away and back.
    await attached();
    const backing = campaign();
    const mock = deferredMock();
    resyncWorkflowRunMapAfterGap();
    await flush();

    applyWorkflowRunMapItemState(itemEvent({ to: 'needs-human', reason: 'gate' }));
    mock.resolve(backing);
    await flush();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.state).toBe('running');

    // The engine's own write, then the reconcile catching up to it.
    backing.runs[1]!.state = 'needs-human';
    backing.runs[1]!.reason = 'gate';
    setBindingMock('WorkflowGetRunMap', async () => structuredClone(backing));
    await settleInvalidate();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.state).toBe('needs-human');
  });

  // The reset seam clears the in-flight counters, and a fetch begun before it
  // still has a `finally` to run. Unguarded, that stale balance decremented a
  // NEWER fetch's count to zero, so every patch that landed while the newer one
  // was in the air recorded no race mark and was silently reverted by its
  // answer. Test-only in origin, a real correctness hole in the counter.
  it('a fetch outliving the reset seam cannot unbalance the counters of the next one', async () => {
    const mock = deferredMock();
    await attached();
    expect(mock.pending).toBe(1);

    // The reset lands while that first fetch is still out. It re-sources the
    // key nobody released, and THAT fetch answers.
    __resetWorkflowRunMapStoreForTest();
    await flush();
    expect(mock.pending).toBe(2);
    const abandoned = mock.take();
    mock.resolve(campaign());
    await flush();
    expect(peekWorkflowRunMap(ROOT)).not.toBeNull();

    // A third fetch goes out, and only now does the abandoned one settle. Its
    // `finally` belongs to counters that no longer exist.
    resyncWorkflowRunMapAfterGap();
    await flush();
    abandoned?.(campaign());
    await flush();

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    mock.resolve(campaign());
    await flush();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(false);

    // The mark survived, so the answer that buried the patch reconciles it.
    setBindingMock('WorkflowGetRunMap', async () => {
      const healed = campaign();
      healed.runs[1]!.softStop = true;
      return healed;
    });
    await settleInvalidate();
    expect(peekWorkflowRunMap(ROOT)!.runs[1]!.softStop).toBe(true);
  });

  it('does not re-source for a fetch nothing raced', async () => {
    await attached();
    // deferredMock installs a fresh spy, so the count below is the re-source
    // and whatever it decides to do afterwards — and nothing raced it.
    const mock = deferredMock();
    resyncWorkflowRunMapAfterGap();
    await flush();
    mock.resolve(campaign());
    await flush();
    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });
});

/**
 * A refusal (§4.2) is a SUCCESSFUL answer that will never become a different
 * one. The primitive's backoff never starts — the RPC did not throw — and the
 * store's own event-driven refetch must not stand in for it.
 */
describe('workflowRunMap — refusals', () => {
  const REFUSAL = { code: 'not-found', message: 'Run root no longer exists.' };

  it('applies a refusal as data, with no error state and no retry', async () => {
    setBindingMock('WorkflowGetRunMap', async () => refusedView(REFUSAL.code, REFUSAL.message));
    const attachment = await attached();
    expect(attachment.current?.refusal).toEqual(REFUSAL);
    expect(attachment.error).toBeNull();

    // The entity store's retry ladder tops out at 30s; nothing may re-ask.
    vi.advanceTimersByTime(120_000);
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('is not pulled back in by a birth event for some other tree', async () => {
    setBindingMock('WorkflowGetRunMap', async () => refusedView(REFUSAL.code, REFUSAL.message));
    await attached();

    // A birth normally reconciles EVERY key, because the map that has to grow a
    // wave is the one watching the parent. A refused key can grow nothing.
    for (let index = 0; index < 5; index += 1) {
      applyWorkflowRunMapItemState({ itemId: `born-${index}`, projectId: 'p1', from: '' as never, to: 'running' });
      await settleInvalidate();
    }
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  // The one path that could still walk a refused key back into the
  // event-driven refetch loop: a patch that landed while a fetch was in the
  // air, answered by a refusal. The mark has to be CONSUMED — the refetch it
  // schedules could only produce the same refusal — and the asymmetry with the
  // explicitly-guarded item-state path was unstated either way.
  it('a patch raced by a fetch that REFUSED consumes the mark without re-asking', async () => {
    await attached();
    const waiting: ((view: WorkflowRunMapView) => void)[] = [];
    setBindingMock('WorkflowGetRunMap', () => new Promise<WorkflowRunMapView>((res) => {
      waiting.push(res);
    }));
    resyncWorkflowRunMapAfterGap();
    await flush();

    applyWorkflowRunMapSoftStop({ itemId: WAVE_2, armed: true });
    waiting.shift()?.(refusedView(REFUSAL.code, REFUSAL.message));
    await flush();
    expect(peekWorkflowRunMap(ROOT)?.refusal?.code).toBe(REFUSAL.code);

    await settleInvalidate();
    // One re-source (the gap's), and nothing behind it.
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);
  });

  it('a transport gap still re-asks: the socket, not the answer, is what was suspect', async () => {
    setBindingMock('WorkflowGetRunMap', async () => refusedView(REFUSAL.code, REFUSAL.message));
    await attached();
    applyTransportGap({ channel: 'workflow:item-state', seq: 1 });
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });
});

describe('workflowRunMap — transport gap', () => {
  it('re-sources every held map on a workflow gap, keeping the last value meanwhile', async () => {
    await attached(ROOT);
    await attached(WAVE_2);
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);

    applyTransportGap({ channel: 'workflow:phase-state', seq: 12 });
    // Re-sourcing keeps what each key last observed: nothing blinks on the
    // way to the fresh answer.
    expect(peekWorkflowRunMap(ROOT)).not.toBeNull();
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(4);
  });

  it('answers every workflow channel, including ones added later', async () => {
    await attached();
    for (const channel of ['workflow:item-state', 'workflow:soft-stop', 'workflow:something-new']) {
      applyTransportGap({ channel, seq: 1 });
      await flush();
    }
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(4);
  });

  it('supersedes the patch-failure refetches it makes redundant', async () => {
    await attached();
    applyWorkflowRunMapPhaseState(phaseEvent({ unitId: 'ghost', status: 'running' }));

    applyTransportGap({ channel: 'workflow:phase-state', seq: 3 });
    await flush();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);

    await settleInvalidate();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
  });
});
