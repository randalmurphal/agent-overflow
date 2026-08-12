// The header's position label (UI-SPEC §4.1, RUN-MAP §11.4).
//
// `phase N/M` is a frozen SQL ordinal: it lies for a retried run, for a looped
// one, and for a campaign root whose own first phase is `done` while the tree
// it heads is three waves in. The header therefore reads the RUN MAP whenever
// the map view is loaded and falls back to the counter only when it is not —
// so the three cases below are "no view", "a view with a frontier" and "a view
// with none", and the labels they produce are the contract.
//
// Nothing is mocked below the binding: the view goes in through the real
// entity store and comes out through the real projection, because the claim is
// that the header agrees with the map mounted directly under it — and two
// mocks agreeing with each other would prove nothing about that.

import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkflowRunMapRun, WorkflowRunMapView } from '../../../../bindings/agent-overflow/models';
import {
  campaignSkeleton as skeleton,
  mapRun,
  mapView as view,
  phaseAttempt as phase,
  skeletonPhase,
} from '../../../test/fixtures/runMap';
import WorkflowRunHeader from './WorkflowRunHeader.svelte';
import type { WorkItem } from '../../types/workflow';
import {
  attachWorkflowRunMap,
  peekWorkflowRunMap,
  __resetWorkflowRunMapStoreForTest,
} from '../../stores/workflowRunMap.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

/** Every run here is a wave of one campaign, so they all carry its skeleton. */
function run(itemId: string, over: Partial<WorkflowRunMapRun> = {}): WorkflowRunMapRun {
  return mapRun(itemId, { skeleton: skeleton(), tailSelfCall: true, ...over });
}

/** Two finished waves and a live third whose `fix` phase is the frontier. */
function campaignView(): WorkflowRunMapView {
  return view([
      run('wave-1', { state: 'done', endedAt: 200_000, phases: [phase('audit'), phase('fix'), phase('next')] }),
      run('wave-2', {
        state: 'done', endedAt: 400_000, parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
        phases: [phase('audit'), phase('fix'), phase('next')],
      }),
      run('wave-3', {
        state: 'running', parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1,
        phases: [phase('audit'), phase('fix', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
      }),
  ]);
}

/** A finished run: a loaded view with nothing left to point at. */
function finishedView(): WorkflowRunMapView {
  return view([run('wave-1', {
    state: 'done', tailSelfCall: false, endedAt: 200_000,
    skeleton: [skeletonPhase('audit')],
    phases: [phase('audit')],
  })]);
}

function item(over: Partial<WorkItem> = {}): WorkItem {
  return {
    id: 'wave-1', projectId: 'p', workflowId: 'campaign', goal: 'Sweep the ports',
    state: 'running', createdAt: 1, startedAt: 1_000,
    // The SQL counter the map is meant to replace, present in every case so a
    // missing label can never be mistaken for a missing counter.
    phaseCount: 3, currentPhaseOrdinal: 1,
    ...over,
  } as WorkItem;
}

/** Load a view into the real store, as the map's own attach would. */
async function loadMap(view: WorkflowRunMapView): Promise<() => void> {
  setBindingMock('WorkflowGetRunMap', async () => view);
  const attachment = attachWorkflowRunMap('wave-1');
  await waitFor(() => expect(peekWorkflowRunMap('wave-1')).not.toBeNull());
  return () => attachment.release();
}

let release: (() => void) | null = null;

beforeEach(() => {
  resetBindingMocks();
  __resetWorkflowRunMapStoreForTest();
  setBindingMock('GetThread', async () => null);
});

afterEach(() => {
  release?.();
  release = null;
  __resetWorkflowRunMapStoreForTest();
  resetBindingMocks();
});

/** Structurally typed: only the one query this file reads is required. */
function hint(view: { getByTestId: (id: string) => HTMLElement }): string {
  return view.getByTestId('workflow-run-hint').textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

describe('WorkflowRunHeader position', () => {
  it('falls back to the frozen phase counter while no map view is loaded', () => {
    const view = render(WorkflowRunHeader, { item: item(), costUsd: 0 });
    expect(peekWorkflowRunMap('wave-1')).toBeNull();
    expect(hint(view)).toContain('phase 1/3');
  });

  it('says nothing about position when there is no counter either', () => {
    const view = render(WorkflowRunHeader, {
      item: item({ phaseCount: 0, currentPhaseOrdinal: 0 }),
      costUsd: 0,
    });
    // Workflow, then age — the position slot is simply absent rather than
    // rendering an empty separator.
    expect(hint(view)).toMatch(/^campaign · \S+$/);
    expect(hint(view)).not.toContain('phase');
  });

  it('names the wave and the leaf off the frontier once the map is loaded', async () => {
    release = await loadMap(campaignView());
    const view = render(WorkflowRunHeader, { item: item(), costUsd: 3.1 });

    // The map says wave 3; the SQL ordinal says phase 1 of the ROOT, which is
    // exactly the lie this replaces.
    expect(hint(view)).toContain('wave 3 · fix');
    expect(hint(view)).not.toContain('phase 1/3');
    expect(hint(view)).toContain('$3.10');
  });

  // The header HOLDS the map it reads. Mounted on its own — no map component
  // beside it, nothing else attached to the key — it still reaches the frontier
  // label, because it acquired the entity rather than free-riding on a
  // sibling's reference. Peeking a key nobody here attached worked only for as
  // long as the map happened to be mounted next to it.
  it('acquires the map itself, so the label does not depend on the map being mounted', async () => {
    setBindingMock('WorkflowGetRunMap', async () => campaignView());
    const view = render(WorkflowRunHeader, { item: item(), costUsd: 0 });

    // Nothing had loaded at first paint, so the counter is what it starts from.
    expect(hint(view)).toContain('phase 1/3');
    await waitFor(() => expect(hint(view)).toContain('wave 3 · fix'));
    expect(hint(view)).not.toContain('phase 1/3');

    // And the reference is released with the component, not leaked.
    view.unmount();
    await waitFor(() => expect(peekWorkflowRunMap('wave-1')).toBeNull());
  });

  // The list cache patches a run by minting a NEW row object
  // (`patchWorkflowItems`), so this prop is replaced on every transition of the
  // run being read. An attach effect that tracked the OBJECT released and
  // re-attached each time — and an attach landing on a key in retry backoff
  // resets the curve and re-sources immediately, so a failing fetch never
  // backed off for as long as the run kept moving.
  it('a new row object for the same run does not release and re-attach the map', async () => {
    const calls = setBindingMock('WorkflowGetRunMap', async () => campaignView());
    const view = render(WorkflowRunHeader, { props: { item: item(), costUsd: 0 } });
    await waitFor(() => expect(hint(view)).toContain('wave 3 · fix'));
    expect(calls).toHaveBeenCalledTimes(1);

    for (const state of ['running', 'needs-human', 'running'] as const) {
      await view.rerender({ item: item({ state }), costUsd: 0 });
    }
    await waitFor(() => expect(hint(view)).toContain('wave 3 · fix'));
    expect(calls).toHaveBeenCalledTimes(1);

    // A DIFFERENT run is a different entity, and that one does re-source.
    setBindingMock('WorkflowGetRunMap', async () => finishedView());
    await view.rerender({ item: item({ id: 'other' }), costUsd: 0 });
    await waitFor(() => expect(peekWorkflowRunMap('other')).not.toBeNull());
    view.unmount();
  });

  it('drops the position entirely when a loaded view has no frontier left', async () => {
    release = await loadMap(finishedView());
    const view = render(WorkflowRunHeader, {
      item: item({ state: 'done', endedAt: 200_000 }),
      costUsd: 0,
    });

    // A run that has finished has no position to name, and the state word
    // already says so — the counter must NOT come back as a stand-in.
    expect(hint(view)).not.toContain('phase');
    expect(hint(view)).not.toContain('wave');
    expect(view.getByTestId('workflow-run-state').textContent).toBe('Done');
  });
});
