import { describe, expect, it } from 'vitest';
import {
  isTerminalRunState,
  patchItemState,
  patchPhaseState,
  patchSoftStop,
} from './workflowRunMapPatch';
import type {
  WorkflowPhaseStateEvent,
  WorkflowRunMapView,
} from '../types/workflow';
import { mapRun, mapUnit, mapView, phaseAttempt } from '../../test/fixtures/runMap';

const RUN = 'run-1';

// Through the generated binding classes: the verdicts below are claims about
// the WIRE shape, so a fixture that cannot notice the wire changing would let
// every one of them keep passing against a shape the backend no longer sends.
function view(): WorkflowRunMapView {
  return mapView([mapRun(RUN, {
    skeletonMissing: true,
    startedAt: undefined,
    phases: [
      phaseAttempt('plan', { startedAt: 100, endedAt: 200 }),
      phaseAttempt('ports', { status: 'running', startedAt: 200, endedAt: undefined }),
    ],
    units: [
      mapUnit('alpha', { phaseId: 'ports', status: 'pending', startedAt: undefined, endedAt: undefined }),
      mapUnit('beta', { phaseId: 'ports', unitIndex: 1, status: 'done', startedAt: 210, endedAt: 260 }),
    ],
  })]);
}

function phase(overrides: Partial<WorkflowPhaseStateEvent> = {}): WorkflowPhaseStateEvent {
  return { itemId: RUN, phaseId: 'ports', attempt: 1, status: 'running', occurredAt: 300, ...overrides };
}

// The verdict table. Every row is a reachable event against the fixture above,
// and the verdict is the whole contract: `patched` means the payload placed the
// row exactly, `invalidate` means it did not and the key must refetch.
const VERDICTS: [name: string, event: WorkflowPhaseStateEvent, verdict: string][] = [
  ['unit starts', phase({ unitId: 'alpha', status: 'running' }), 'patched'],
  ['unit finishes', phase({ unitId: 'alpha', status: 'done' }), 'patched'],
  ['unit is dropped', phase({ unitId: 'alpha', status: 'dropped' }), 'patched'],
  ['unit is taken over', phase({ unitId: 'alpha', status: 'taken-over' }), 'patched'],
  ['unit restates its status', phase({ unitId: 'alpha', status: 'pending' }), 'unchanged'],
  ['settled unit is reopened', phase({ unitId: 'beta', status: 'pending' }), 'invalidate'],
  ['unit re-runs after ending', phase({ unitId: 'beta', status: 'running' }), 'invalidate'],
  ['unit is unknown', phase({ unitId: 'ghost', status: 'running' }), 'invalidate'],
  ['unit status is unknown', phase({ unitId: 'alpha', status: 'quarantined' }), 'invalidate'],
  ['unit is on another attempt', phase({ unitId: 'alpha', attempt: 2, status: 'done' }), 'invalidate'],
  ['attempt opens', phase({ phaseId: 'audit', attempt: 1 }), 'patched'],
  ['attempt retries', phase({ phaseId: 'ports', attempt: 2 }), 'patched'],
  ['attempt completes', phase({ status: 'completed' }), 'patched'],
  ['attempt restates running', phase({ status: 'running' }), 'unchanged'],
  ['attempt parks', phase({ status: 'parked' }), 'invalidate'],
  ['attempt fails', phase({ status: 'failed' }), 'invalidate'],
  ['attempt is cancelled', phase({ status: 'cancelled' }), 'invalidate'],
  ['attempt status is unknown', phase({ status: 'superseded' }), 'invalidate'],
  ['ended attempt runs again', phase({ phaseId: 'plan', attempt: 1 }), 'invalidate'],
  ['unseen attempt completes', phase({ phaseId: 'audit', status: 'completed' }), 'invalidate'],
  ['event has no time', phase({ unitId: 'alpha', occurredAt: 0 }), 'invalidate'],
  ['run is not in this tree', phase({ itemId: 'elsewhere', unitId: 'alpha' }), 'invalidate'],
];

describe('workflowRunMapPatch — phase-state verdicts', () => {
  for (const [name, event, verdict] of VERDICTS) {
    it(`${verdict} when a ${name}`, () => {
      expect(patchPhaseState(view(), event).kind).toBe(verdict);
    });
  }

  it('opens an attempt with the event time and leaves every other row alone', () => {
    const before = view();
    const result = patchPhaseState(before, phase({ phaseId: 'audit', attempt: 1, occurredAt: 300 }));
    expect(result).toEqual({
      kind: 'patched',
      view: {
        ...before,
        runs: [{
          ...before.runs[0]!,
          phases: [
            ...before.runs[0]!.phases,
            { phaseId: 'audit', attempt: 1, status: 'running', startedAt: 300 },
          ],
        }],
      },
    });
    // Pure: the input view is never written through.
    expect(before).toEqual(view());
  });

  it('times a unit from the event, not from arrival', () => {
    const started = patchPhaseState(view(), phase({ unitId: 'alpha', status: 'running', occurredAt: 300 }));
    expect(started.kind === 'patched' && started.view.runs[0]!.units[0]).toMatchObject({
      status: 'running', startedAt: 300,
    });
    // A `running` frame opens the span and must not close it. `endedAt` reads
    // undefined rather than being absent because the generated binding classes
    // declare every optional field — which is what the RPC's own `createFrom`
    // produces in production too, so the fixture and the wire agree.
    expect((started.kind === 'patched' ? started.view.runs[0]!.units[0]!.endedAt : -1)).toBeUndefined();
    const finished = patchPhaseState(
      started.kind === 'patched' ? started.view : view(),
      phase({ unitId: 'alpha', status: 'done', occurredAt: 900 }),
    );
    expect(finished.kind === 'patched' && finished.view.runs[0]!.units[0]).toMatchObject({
      status: 'done', startedAt: 300, endedAt: 900,
    });
  });
});

describe('workflowRunMapPatch — item-state and soft-stop', () => {
  it('writes state and reason, and drops the reason when the run leaves it behind', () => {
    const parked = patchItemState(view(), {
      itemId: RUN, projectId: 'p1', from: 'running', to: 'needs-human', reason: 'gate',
    });
    expect(parked.kind === 'patched' && parked.view.runs[0]).toMatchObject({
      state: 'needs-human', reason: 'gate',
    });

    const resumed = patchItemState(
      parked.kind === 'patched' ? parked.view : view(),
      { itemId: RUN, projectId: 'p1', from: 'needs-human', to: 'running' },
    );
    expect(resumed.kind === 'patched' && resumed.view.runs[0]).not.toHaveProperty('reason');
  });

  it('reconciles a transition for a run the view does not contain', () => {
    expect(patchItemState(view(), {
      itemId: 'newborn', projectId: 'p1', from: '' as never, to: 'running',
    }).kind).toBe('invalidate');
  });

  it('restating a run state is not a write', () => {
    expect(patchItemState(view(), {
      itemId: RUN, projectId: 'p1', from: 'running', to: 'running',
    }).kind).toBe('unchanged');
  });

  it('arms soft stop, and ignores it for a tree this view knows nothing about', () => {
    const armed = patchSoftStop(view(), { itemId: RUN, armed: true });
    expect(armed.kind === 'patched' && armed.view.runs[0]!.softStop).toBe(true);
    expect(patchSoftStop(view(), { itemId: RUN, armed: false }).kind).toBe('unchanged');
    expect(patchSoftStop(view(), { itemId: 'elsewhere', armed: true }).kind).toBe('unchanged');
  });
});

describe('workflowRunMapPatch — predicates', () => {
  it('knows which run states end a run', () => {
    expect(['done', 'failed', 'cancelled'].every(isTerminalRunState)).toBe(true);
    expect(['running', 'needs-human', ''].some(isTerminalRunState)).toBe(false);
  });

  it('accepts only a positive engine stamp as an event time', () => {
    // Driven through the public verdict rather than the predicate: what the
    // store acts on is the verdict, and a predicate asserted on its own can
    // agree with itself while the path that uses it stops calling it.
    expect(patchPhaseState(view(), phase({ unitId: 'alpha', occurredAt: 1 })).kind).toBe('patched');
    for (const occurredAt of [0, -1, Number.NaN, Number.POSITIVE_INFINITY, undefined as never]) {
      expect(patchPhaseState(view(), phase({ unitId: 'alpha', occurredAt })).kind).toBe('invalidate');
    }
  });

  it('refuses a reopen rather than leaving the failed try\'s duration on the row', () => {
    // `engine.reopenUnit` → `store.RetryWorkItemUnit` bumps `unit_attempt` and
    // zeroes both timestamps. None of that is on the wire, so a status-only
    // patch would show a queued unit still carrying its last run's ×N and span.
    const settled = view();
    settled.runs[0]!.units[1] = {
      phaseId: 'ports', attempt: 1, unitId: 'beta', unitIndex: 1, kind: 'unit',
      status: 'failed', unitAttempt: 2, startedAt: 210, endedAt: 260,
    };
    expect(patchPhaseState(settled, phase({ unitId: 'beta', status: 'pending' })).kind).toBe('invalidate');
  });
});
