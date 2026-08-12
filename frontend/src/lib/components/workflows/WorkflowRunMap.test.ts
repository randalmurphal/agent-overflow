// Component coverage for the run map (RUN-MAP §7 case matrix, visual half).
//
// The projection is NOT mocked: every case feeds a real `WorkflowRunMapView`
// through `buildRunMap`, so a component test that passes
// proves the rendered thing is what the projection actually says — the two
// cannot drift into agreeing with each other about a shape neither produces.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkflowRunMapView } from '../../../../bindings/agent-overflow/models';
import {
  campaignSkeleton,
  mapRun as makeRun,
  mapUnit as makeUnit,
  mapView as makeView,
  phaseAttempt as makePhase,
  refusedView,
  skeletonPhase,
} from '../../../test/fixtures/runMap';
import WorkflowRunMap from './WorkflowRunMap.svelte';
import WorkflowRunMapWave from './WorkflowRunMapWave.svelte';
import { WORKFLOWS_OVERLAY_SCROLLER_KEY } from './overlayScroller';
import { buildRunMap } from '../../utils/workflowRunMap';
import { __resetWorkflowRunMapStoreForTest } from '../../stores/workflowRunMap.svelte';
import {
  getWorkflowRunMapExpansion,
  resetWorkflowsOverlayForTest,
} from '../../stores/workflowsOverlay.svelte';
import { __resetRunningElapsedTickerForTest } from '../chat/useRunningElapsed.svelte';
import { getSettings } from '../../stores/settings.svelte';
import { runMapNodeStyle } from '../../utils/workflowRunMapStyle';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

const NOW = 10_000_000;

/** Two folded waves plus a live one whose `fix` phase is the frontier. */
function campaignView(): WorkflowRunMapView {
  return makeView([
    makeRun('wave-1', {
      state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(), endedAt: 200_000,
      phases: [makePhase('audit'), makePhase('fix'), makePhase('next')],
      spend: { costUsd: 2.5, wireCostUsd: 2.5, estimatedCostUsd: 0, unpricedRows: 0 },
      budget: { kind: 'usd', ceilingUsd: 10, spentUsd: 2.5, percent: 25 },
    }),
    makeRun('wave-2', {
      state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(), endedAt: 400_000,
      parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
      phases: [makePhase('audit'), makePhase('fix'), makePhase('next')],
    }),
    makeRun('wave-3', {
      state: 'running', tailSelfCall: true, skeleton: campaignSkeleton(),
      parentItemId: 'wave-2', parentPhaseId: 'next', parentAttempt: 1,
      phases: [
        makePhase('audit', { threadId: 'thread-audit' }),
        makePhase('fix', { status: 'running', endedAt: 0, startedAt: 9_900_000, threadId: 'thread-fix' }),
      ],
    }),
  ], 'wave-1');
}

const PARK_CAUSE = 'provision worktree: branch "ao/wave-3" already exists and points at a different commit';

/** One live run parked on a gate, with an intervention recorded on the attempt. */
function parkedView(): WorkflowRunMapView {
  return makeView([makeRun('root', {
    // Real wall clock: the strip's countdown rides the shared 1Hz clock, so a
    // fixture-time target would already have passed by the time it renders.
    state: 'needs-human', reason: 'gate', autoResumeAt: Date.now() + 15 * 60_000,
    skeleton: [skeletonPhase('plan'), skeletonPhase('review'), skeletonPhase('ship')],
    phases: [
      makePhase('plan', { interventionKind: 'by-hand' }),
      makePhase('review', {
        status: 'parked', endedAt: 0, startedAt: 9_400_000, cause: PARK_CAUSE, threadId: 'thread-review',
      }),
    ],
  })]);
}

/** A fan-out with actionable columns, scalar bulk, dropped units and a join. */
function fanView(): WorkflowRunMapView {
  return makeView([makeRun('root', {
    state: 'running',
    skeleton: [
      skeletonPhase('plan'),
      skeletonPhase('port', { name: 'ports', shape: 'fan-out' }),
      skeletonPhase('verify'),
    ],
    phases: [makePhase('plan'), makePhase('port', { status: 'running', endedAt: 0, startedAt: 9_880_000 })],
    units: [
      makeUnit('port-a', { unitIndex: 0, status: 'done' }),
      makeUnit('port-b', { unitIndex: 1, status: 'running', endedAt: 0, startedAt: 9_970_000, threadId: 'thread-b' }),
      makeUnit('port-c', { unitIndex: 2, status: 'failed' }),
      makeUnit('port-d', { unitIndex: 3, status: 'pending', provider: 'claude', startedAt: 0, endedAt: 0 }),
      makeUnit('port-e', { unitIndex: 4, status: 'dropped' }),
      makeUnit('port-join', { unitIndex: 99, kind: 'join', status: 'pending', startedAt: 0, endedAt: 0 }),
    ],
  })]);
}

/**
 * The overlay owns the scroller and hands it down (§9.9), so a map mounted on
 * its own has to provide one — deliberately: the map throws without a provider
 * rather than walking the DOM for something that happens to scroll. The
 * container the map renders into plays the part, so the follow controller sees
 * the real containment relationship.
 *
 * happy-dom lays nothing out — every rect is 0×0 at the origin — so the
 * stand-in states a viewport that CONTAINS the origin. Without that, every
 * unlaid-out row reads as off-screen and the chip appears in states it never
 * would in a browser. Geometry decisions are proved against a stated layout in
 * `runMapFollow.svelte.test.ts`; what these tests prove is the wiring.
 */
function asScroller(el: HTMLElement): HTMLElement {
  el.getBoundingClientRect = () => ({
    x: 0, y: -1000, top: -1000, bottom: 1000, left: 0, right: 1000,
    width: 1000, height: 2000, toJSON: () => ({}),
  }) as DOMRect;
  for (const key of ['clientHeight', 'scrollHeight']) {
    Object.defineProperty(el, key, { get: () => 2000, configurable: true });
  }
  return el;
}

function renderMap(itemId: string) {
  // The getter is what the overlay hands down, so it can answer with an
  // element that does not exist yet — which is exactly the case here.
  const scroller: { el: HTMLElement | null } = { el: null };
  const rendered = render(WorkflowRunMap, {
    props: { itemId },
    context: new Map<symbol, () => HTMLElement | null>([
      [WORKFLOWS_OVERLAY_SCROLLER_KEY, () => scroller.el],
    ]),
  });
  scroller.el = asScroller(rendered.container as HTMLElement);
  return rendered;
}

function mountMap(view: WorkflowRunMapView, itemId = view.rootItemId) {
  setBindingMock('WorkflowGetRunMap', async () => view);
  return renderMap(itemId);
}

/** One wave rendered on its own, for the node/fan cases the map only hosts. */
function mountWave(view: WorkflowRunMapView, waveItemId = view.rootItemId) {
  const model = buildRunMap(view, NOW, { expandedWaveIds: [waveItemId] });
  const wave = model.waves.find((candidate) => candidate.itemId === waveItemId);
  if (!wave) throw new Error(`no wave ${waveItemId}`);
  // The scroller context is required here too: a wave's fold asks whether its
  // region is on screen before it animates (§9.8), which is the same "the
  // overlay provides, the map requires" contract the map itself holds to.
  const scroller: { el: HTMLElement | null } = { el: null };
  // No `open` prop: a wave is open exactly when the projection built its
  // segments, which naming the wave in `expandedWaveIds` above is what does.
  const rendered = render(WorkflowRunMapWave, {
    props: {
      wave,
      nowKey: model.followTarget?.key ?? '',
      onOpenThread: () => {},
      onToggleComposition: () => {},
    },
    context: new Map<symbol, () => HTMLElement | null>([
      [WORKFLOWS_OVERLAY_SCROLLER_KEY, () => scroller.el],
    ]),
  });
  scroller.el = asScroller(rendered.container as HTMLElement);
  return rendered;
}

beforeEach(() => {
  resetBindingMocks();
  resetWorkflowsOverlayForTest();
  __resetWorkflowRunMapStoreForTest();
  __resetRunningElapsedTickerForTest();
  setBindingMock('GetThread', async () => null);
});

afterEach(() => {
  __resetWorkflowRunMapStoreForTest();
  __resetRunningElapsedTickerForTest();
  resetBindingMocks();
});

describe('WorkflowRunMap — the surface', () => {
  it('renders the wave list, folding what is terminal and expanding what is live', async () => {
    const view = mountMap(campaignView());

    await waitFor(() => expect(view.getAllByTestId('workflow-map-wave')).toHaveLength(3));
    expect(view.getAllByTestId('workflow-map-wave').map((node) => node.dataset.waveExpanded))
      .toEqual(['false', 'false', 'true']);
    expect(view.getAllByTestId('workflow-map-summary').map((node) => node.textContent?.trim().replace(/\s+/g, ' ')))
      .toEqual([
        expect.stringContaining('wave 1'),
        expect.stringContaining('wave 2'),
        expect.stringContaining('wave 3'),
      ]);
    // A wave that called the next one LOOPED, whatever its own row says.
    expect(view.getAllByTestId('workflow-map-summary')[0].textContent).toContain('Looped');
    // Chain-local ordinals, on the row, so a numbering regression is caught as
    // a wrong number rather than as a label string that happens to still read.
    expect(view.getAllByTestId('workflow-map-summary').map((row) => row.dataset.waveOrdinal))
      .toEqual(['1', '2', '3']);
    // §2: the wave the run is IN gets the emphasis, and it is the rail's weight
    // rather than a hue. Only the live wave on the frontier path is current.
    expect(view.getAllByTestId('workflow-map-wave-body').map((body) => body.dataset.current))
      .toEqual(['false', 'false', 'true']);
    expect(view.getAllByTestId('workflow-map-wave-body')[2].className).toContain('border-border-strong');
    // Only the live wave draws nodes; the folded ones cost nothing.
    expect(view.getAllByTestId('workflow-map-node').length).toBeGreaterThan(0);
    expect(view.getAllByTestId('workflow-map-node').every((node) => node.closest('[data-wave-expanded="true"]') !== null))
      .toBe(true);
  });

  // A THROWN error is transient — the store answers it with a retry ladder —
  // so it renders as the error state, and the surface keeps whatever it had.
  it('states loading, then the error loudly rather than blanking the surface', async () => {
    setBindingMock('WorkflowGetRunMap', async () => { throw new Error('map fetch exploded'); });
    const view = renderMap('root');

    expect(view.getByTestId('workflow-map-loading')).toBeTruthy();
    await waitFor(() => expect(view.getByTestId('workflow-map-error')).toBeTruthy());
    expect(view.queryByTestId('workflow-map-refusal')).toBeNull();
  });

  // A REFUSAL is the opposite answer (§4.2): the RPC succeeded, the answer is
  // "never", and re-asking cannot change it. It is data, so it renders as its
  // own state — headline for what it means here, the backend's sentence for
  // which run it happened to — and never through the error path.
  it.each([
    ['not-found', 'Run wave-1 no longer exists.', 'This run is gone'],
    ['too-large', 'This campaign has more than 4096 runs, which is more than the map can draw at once.',
      'This campaign is too big to draw'],
    ['corrupt-linkage', 'The call linkage around run wave-1 does not describe a tree, so its map cannot be drawn.',
      'This run’s call linkage is broken'],
  ])('renders the %s refusal as permanent state, not as a failed fetch', async (code, message, headline) => {
    setBindingMock('WorkflowGetRunMap', async () => refusedView(code, message));
    const view = renderMap('wave-1');

    const refusal = await waitFor(() => view.getByTestId('workflow-map-refusal'));
    expect(refusal.dataset.refusalCode).toBe(code);
    expect(refusal.textContent).toContain(headline);
    expect(refusal.textContent).toContain(message);
    // Neither the error state nor the empty-map sentence: a refused map is a
    // third thing, and saying "nothing to show yet" would promise a later yes.
    expect(view.queryByTestId('workflow-map-error')).toBeNull();
    expect(view.queryByTestId('workflow-map-empty')).toBeNull();
    expect(view.queryByTestId('workflow-map-waves')).toBeNull();
  });

  // A code this build has not learnt still gets a heading rather than a bare
  // sentence — the honest one: the map cannot be drawn and we cannot say why.
  it('heads an unrecognised refusal code without inventing a meaning for it', async () => {
    setBindingMock('WorkflowGetRunMap', async () => refusedView('from-the-future', 'Something new happened.'));
    const view = renderMap('wave-1');

    const refusal = await waitFor(() => view.getByTestId('workflow-map-refusal'));
    expect(refusal.textContent).toContain('This run’s map cannot be drawn');
    expect(refusal.textContent).toContain('Something new happened.');
  });

  // The empty map is a different sentence from a refusal, and it is reachable:
  // a view carrying no runs at all has no root to build a wave from.
  it('says a run has nothing to show when the answer names no runs', async () => {
    const view = mountMap(makeView([]), 'root');
    await waitFor(() => expect(view.getByTestId('workflow-map-empty')).toBeTruthy());
    expect(view.queryByTestId('workflow-map-refusal')).toBeNull();
  });

  it('names where the run is, what blocks it, when it resumes, and what it has spent', async () => {
    const view = mountMap(parkedView());

    const strip = await waitFor(() => view.getByTestId('workflow-map-frontier'));
    expect(strip.textContent).toContain('review');
    expect(view.getByTestId('workflow-map-blocker').textContent).toContain('Review gate');
    // The chip's amber is the map's one amber: glow, border and tone all come
    // from the shared style table, so the strip and the node it points at
    // cannot drift into two different ambers.
    const blocker = runMapNodeStyle('parked');
    const chip = view.getByTestId('workflow-map-blocker');
    for (const part of [blocker.glow, blocker.border, blocker.tone]) {
      expect(chip.className).toContain(part);
    }
    expect(view.getByTestId('workflow-map-resume').textContent).toContain('resumes in');
  });

  it('shows the lap counter and the budget line for a campaign with a loop foot', async () => {
    const view = mountMap(campaignView());
    const lap = await waitFor(() => view.getByTestId('workflow-map-lap'));
    expect(lap.textContent).toContain('lap 3 of ≤6');
    expect(lap.textContent).toContain('$2.50 of $10.00');
  });

  it('expands a folded wave through the overlay store, and keeps it across a remount', async () => {
    const view = mountMap(campaignView());
    await waitFor(() => expect(view.getAllByTestId('workflow-map-summary')).toHaveLength(3));
    // Closed means the fold is closed AND its body is empty: an open flag that
    // could disagree with the model is what put "Nothing recorded in this wave
    // yet." inside every folded wave's DOM.
    expect(view.getAllByTestId('workflow-map-wave-fold').map((fold) => fold.dataset.open))
      .toEqual(['false', 'false', 'true']);
    expect(view.queryAllByTestId('workflow-map-wave-empty')).toHaveLength(0);

    await fireEvent.click(view.getAllByTestId('workflow-map-summary')[1]);
    await waitFor(() => expect(view.getAllByTestId('workflow-map-wave')[1].dataset.waveExpanded).toBe('true'));
    expect(view.getAllByTestId('workflow-map-wave-fold')[1].dataset.open).toBe('true');
    expect([...getWorkflowRunMapExpansion('wave-1').waves]).toEqual(['wave-2']);

    view.unmount();
    const remounted = mountMap(campaignView());
    await waitFor(() => expect(remounted.getAllByTestId('workflow-map-wave')).toHaveLength(3));
    expect(remounted.getAllByTestId('workflow-map-wave')[1].dataset.waveExpanded).toBe('true');
  });

  // The one state the empty sentence is true of: an opened wave whose run
  // recorded nothing and declared nothing (records-only, §5.8, with no records).
  it('says a wave recorded nothing only once that wave is actually open', async () => {
    const view = mountMap(makeView([
      makeRun('wave-1', {
        state: 'done', tailSelfCall: true, skeleton: campaignSkeleton(), endedAt: 200_000,
        phases: [makePhase('audit'), makePhase('next')],
      }),
      makeRun('wave-2', {
        state: 'cancelled', endedAt: 400_000, skeletonMissing: true,
        parentItemId: 'wave-1', parentPhaseId: 'next', parentAttempt: 1,
      }),
    ], 'wave-1'));

    await waitFor(() => expect(view.getAllByTestId('workflow-map-summary')).toHaveLength(2));
    expect(view.queryByTestId('workflow-map-wave-empty')).toBeNull();

    await fireEvent.click(view.getAllByTestId('workflow-map-summary')[1]);
    await waitFor(() => expect(view.getByTestId('workflow-map-wave-empty')).toBeTruthy());
  });

  // §5.8's two causes are not the same news. An absent snapshot is ordinary
  // history and renders silently; one that would not DECODE is a defect in a
  // stored record, so it is stated — in the failure hue, outside the fold.
  it('states a corrupt frozen definition as a failure, not as ordinary history', async () => {
    const corrupt = mountMap(makeView([makeRun('root', {
      state: 'done', endedAt: 400_000, skeletonMissing: true,
      skeletonError: 'decode work item snapshot: unexpected EOF',
      phases: [makePhase('plan')],
    })]));

    const notice = await waitFor(() => corrupt.getByTestId('workflow-map-wave-skeleton-error'));
    const failed = runMapNodeStyle('failed');
    expect(notice.className).toContain(failed.border);
    expect(notice.className).toContain(failed.tone);
    // R2: the decode failure itself never reaches the surface.
    expect(notice.textContent).not.toContain('EOF');
    corrupt.unmount();

    // Records-only through mere ABSENCE says nothing at all.
    const absent = mountMap(makeView([makeRun('root', {
      state: 'done', endedAt: 400_000, skeletonMissing: true, phases: [makePhase('plan')],
    })]));
    await waitFor(() => expect(absent.getAllByTestId('workflow-map-summary')).toHaveLength(1));
    expect(absent.queryByTestId('workflow-map-wave-skeleton-error')).toBeNull();
  });

  it('leaves a live wave with no fold affordance — it is expanded in place', async () => {
    const view = mountMap(campaignView());
    await waitFor(() => expect(view.getAllByTestId('workflow-map-summary')).toHaveLength(3));
    const rows = view.getAllByTestId('workflow-map-summary') as HTMLButtonElement[];
    expect([rows[0].disabled, rows[2].disabled]).toEqual([false, true]);
  });

  // §9.8. The pairing with §9.7 is the whole point: compensation is measured
  // ONCE, at the flush, so a fold that keeps changing height for 200ms
  // afterwards drifts the viewport of a reader who is not following. In view
  // the reader sees the motion and there is nothing to cancel; out of view the
  // whole delta has to land inside the hold.
  it('animates a fold in view and applies an off-screen one instantly', async () => {
    const view = mountMap(campaignView());
    await waitFor(() => expect(view.getAllByTestId('workflow-map-summary')).toHaveLength(3));
    const folds = view.getAllByTestId('workflow-map-wave-fold');
    expect(folds[0].dataset.foldAnimated).toBe('true');

    // Park wave 1's region above the scroller's viewport — the auto-fold above
    // a disengaged reader — and open it.
    folds[0].getBoundingClientRect = () => ({
      x: 0, y: -4000, top: -4000, bottom: -3900, left: 0, right: 600,
      width: 600, height: 100, toJSON: () => ({}),
    }) as DOMRect;
    await fireEvent.click(view.getAllByTestId('workflow-map-summary')[0]);
    await waitFor(() => expect(view.getAllByTestId('workflow-map-wave-fold')[0].dataset.open).toBe('true'));
    const opened = view.getAllByTestId('workflow-map-wave-fold')[0];
    expect(opened.dataset.foldAnimated).toBe('false');
    expect(opened.className).not.toContain('transition-');
  });

  it('renders an all-ghost segment for a run with zero attempts', async () => {
    const view = mountMap(makeView([makeRun('root', {
      skeleton: [skeletonPhase('plan'), skeletonPhase('build')],
    })]));

    await waitFor(() => expect(view.getAllByTestId('workflow-map-node')).toHaveLength(2));
    expect(view.getAllByTestId('workflow-map-node').every((node) => node.dataset.ghost === 'true')).toBe(true);
    // §7 wants a follow target here — the segment top — and the strip that
    // names it comes with it. What it must NOT do is mark a ghost as `now ▸`:
    // the target is the wave, so no node key matches it.
    expect(view.getByTestId('workflow-map-frontier').textContent).toContain('wave 1');
    expect(view.container.querySelector('[data-run-map-now="true"]')).toBeNull();
  });

  // §9.9: the frame that owns the scroller hands it down. Mounted anywhere
  // that does not, the map has nothing to place, jump, follow or compensate
  // against — and it says so instead of silently doing none of the four.
  it('refuses to mount without the overlay scroller its writes need', () => {
    setBindingMock('WorkflowGetRunMap', async () => campaignView());
    expect(() => render(WorkflowRunMap, { itemId: 'wave-1' })).toThrow(/scroller context is missing/);
  });

  it('renders under reduced motion without branching on it', async () => {
    const original = window.matchMedia;
    window.matchMedia = ((query: string) => ({
      matches: query.includes('reduce'),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    })) as unknown as typeof window.matchMedia;
    try {
      const view = mountMap(campaignView());
      await waitFor(() => expect(view.getAllByTestId('workflow-map-wave')).toHaveLength(3));
    } finally {
      window.matchMedia = original;
    }
  });
});

describe('WorkflowRunMap — the follow chip', () => {
  it('offers the jump when a parked run opens with follow off, and engages on click', async () => {
    const view = mountMap(parkedView());
    await waitFor(() => expect(view.getByTestId('workflow-map-follow')).toBeTruthy());

    await fireEvent.click(view.getByTestId('workflow-map-follow'));
    await waitFor(() => expect(view.queryByTestId('workflow-map-follow')).toBeNull());
  });

  it('stays out of the way when a running run opens already following', async () => {
    const view = mountMap(campaignView());
    await waitFor(() => expect(view.getAllByTestId('workflow-map-wave')).toHaveLength(3));
    expect(view.queryByTestId('workflow-map-follow')).toBeNull();
  });

  it('sits outside the scrolled flow rather than inside the wave list', async () => {
    const view = mountMap(parkedView());
    const chip = await waitFor(() => view.getByTestId('workflow-map-follow'));
    expect(chip.closest('[data-testid="workflow-map-waves"]')).toBeNull();
    expect(chip.parentElement?.className).toContain('sticky');
  });
});

describe('WorkflowRunMapNode — the state table', () => {
  it('draws the running node with the standing spinner and no pulse', () => {
    const view = mountWave(campaignView(), 'wave-3');
    const running = view.container.querySelector('[data-signal="running"]');
    expect(running).not.toBeNull();
    expect(running?.querySelector('[data-testid="stepped-spinner"]')).not.toBeNull();
    expect(view.container.querySelector('.animate-pulse')).toBeNull();
  });

  it('marks the follow target with the one accent on the surface', () => {
    const view = mountWave(campaignView(), 'wave-3');
    const marked = view.container.querySelectorAll('[data-run-map-now="true"]');
    expect(marked).toHaveLength(1);
    expect(marked[0].textContent).toContain('now ▸');
    expect(marked[0].querySelector('.text-accent')).not.toBeNull();
  });

  it('draws what has not happened yet as a dashed ghost', () => {
    const view = mountWave(campaignView(), 'wave-3');
    const ghost = view.container.querySelector('[data-testid="workflow-map-node"][data-ghost="true"]');
    expect(ghost).not.toBeNull();
    expect(ghost?.querySelector('.border-dashed')).not.toBeNull();
  });

  it('parks amber, clamps the cause to two lines, and expands it inline', async () => {
    const view = mountWave(parkedView());
    const cause = view.getByTestId('workflow-map-node-cause');
    expect(cause.textContent).toContain(PARK_CAUSE);
    expect(cause.className).toContain('line-clamp-2');
    expect(cause.className).toContain('text-warning');

    await fireEvent.click(cause);
    expect(view.getByTestId('workflow-map-node-cause').className).not.toContain('line-clamp-2');
  });

  it('marks an attempt someone touched by hand', () => {
    const view = mountWave(parkedView());
    expect(view.container.textContent).toContain('touched by hand');
  });

  it('renders a failed wave node in the failure hue', () => {
    const failed = makeView([makeRun('root', {
      state: 'failed', reason: 'agent-error',
      skeleton: [skeletonPhase('plan')],
      phases: [makePhase('plan', { status: 'failed', cause: 'agent exited 1' })],
    })]);
    const view = mountWave(failed);
    const node = view.container.querySelector('[data-signal="failed"]');
    expect(node).not.toBeNull();
    expect(node?.querySelector('.border-error')).not.toBeNull();
  });

  // Asserted against the real open path rather than a mocked module: the map
  // wires `openWorkflowThreadById` itself, and a resolver mock would only prove
  // the test's own wiring. `GetThread` is the first thing that path reaches.
  it('opens the thread of a node that has one, and leaves the rest inert', async () => {
    const view = mountMap(campaignView());
    await waitFor(() => expect(view.getAllByTestId('workflow-map-node-label').length).toBeGreaterThan(0));
    const labels = view.getAllByTestId('workflow-map-node-label') as HTMLButtonElement[];
    const openable = labels.filter((label) => !label.disabled);
    expect(openable.length).toBeGreaterThan(0);

    await fireEvent.click(openable[0]);
    await waitFor(() => expect(getBindingMock('GetThread')?.mock.calls).toEqual([['thread-audit']]));
    expect(labels.some((label) => label.disabled)).toBe(true);
  });

  it('appends a record whose phase left the definition, with a note and never dropped', () => {
    const drifted = makeView([makeRun('root', {
      state: 'running',
      skeleton: [skeletonPhase('audit'), skeletonPhase('fix')],
      phases: [makePhase('audit'), makePhase('mystery', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
    })]);
    const view = mountWave(drifted);
    const nodes = view.getAllByTestId('workflow-map-node');
    expect(nodes.map((node) => node.dataset.phaseId)).toEqual(['audit', 'fix', 'mystery']);
    expect(nodes[2].querySelector('[data-testid="workflow-map-node-note"]')?.textContent)
      .toContain('not in current definition');
  });

  // R1: amber means a human is BLOCKED. A standing stop request is a fact
  // about what the loop does next, and nothing is waiting on anyone.
  it('states a standing stop request neutrally, not as an attention hue', () => {
    const view = campaignView();
    // On the tree ROOT: `engine.setSoftStop` refuses a called run, and every
    // wave's loop foot reads the root's row.
    view.runs[0].softStop = true;
    const rendered = mountWave(view, 'wave-3');
    const note = rendered.getByTestId('workflow-map-soft-stop');
    expect(note.textContent).toContain('stops after this wave');
    expect(note.className).toContain('text-fg-muted');
    expect(note.className).not.toContain('text-warning');
  });

  it('draws the loop foot with its undecided outcome stubs', () => {
    const view = mountWave(campaignView(), 'wave-3');
    const decision = view.getByTestId('workflow-map-decision');
    expect(decision.textContent).toContain('lap 3 of ≤6');
    expect(decision.textContent).toContain('issues → wave 4');
    expect(decision.textContent).toContain('clean → done');
  });
});

describe('WorkflowRunMapFan', () => {
  it('gives columns to the actionable branches and arithmetic to the rest', () => {
    const view = mountWave(fanView());
    expect(view.getAllByTestId('workflow-map-branch').map((node) => node.dataset.unitId))
      .toEqual(['port-b', 'port-c']);
    expect(view.getAllByTestId('workflow-map-group').map((node) => node.textContent?.trim()))
      .toEqual(['queued ·1', 'done ·1 · 1 dropped']);
    expect(view.getByTestId('workflow-map-join').textContent).toContain('port-join');
  });

  // The fan's two shapes carry their state on the element that drew it, so a
  // regression that moves a unit between column, chip and group is caught as a
  // WRONG STATE rather than as a count that happens to still add up.
  it('states each unit\'s signal and each group\'s kind on the element that drew it', () => {
    const view = mountWave(fanView());
    expect(view.getAllByTestId('workflow-map-group').map((node) => node.dataset.groupKind))
      .toEqual(['queued', 'done']);
    // Columns first, then the join; the scalar bulk is inside the closed folds.
    expect([...view.container.querySelectorAll<HTMLElement>('[data-unit-signal]')]
      .map((node) => [node.dataset.unitId, node.dataset.unitSignal]))
      .toEqual([
        ['port-b', 'running'],
        ['port-c', 'failed'],
        ['port-join', 'pending'],
      ]);
  });

  it('expands a group chip into a wrapping unit grid, dropped entries struck', async () => {
    const view = mountWave(fanView());
    const done = view.getAllByTestId('workflow-map-group')[1];
    expect(view.container.querySelector('[data-unit-id="port-e"]')).toBeNull();

    await fireEvent.click(done);
    await waitFor(() => expect(view.container.querySelector('[data-unit-id="port-e"]')).not.toBeNull());
    const dropped = view.container.querySelector('[data-unit-id="port-e"] button');
    expect(dropped?.className).toContain('line-through');
  });

  // §6: a column's resting width, its enter animation and its leaving
  // transition are three renderings of two numbers, so the numbers live in one
  // place and the markup reads them.
  it('declares the lane geometry once, on the fan container', () => {
    const view = mountWave(fanView());
    const fan = view.getByTestId('workflow-map-fan');
    expect(fan.style.getPropertyValue('--run-map-lane-min')).toBe('120px');
    expect(fan.style.getPropertyValue('--run-map-lane-max')).toBe('200px');

    const column = view.getAllByTestId('workflow-map-branch')[0];
    expect(column.className).toContain('min-w-[var(--run-map-lane-min)]');
    expect(column.className).not.toMatch(/\[\d+px\]/);
  });

  // §10: every structural motion gates on `motionReduced()`, whose second half
  // — the app's low-power setting — no CSS reset can see.
  it('drops the column animation and the fold transition under low power', () => {
    const lit = mountWave(fanView());
    expect(lit.getAllByTestId('workflow-map-branch')[0].className).toContain('run-map-column');
    expect(lit.getAllByTestId('workflow-map-group-fold')[0].className).toContain('transition-');

    getSettings().lowPowerMode = true;
    try {
      const dark = mountWave(fanView());
      expect(dark.getAllByTestId('workflow-map-branch')[0].className).not.toContain('run-map-column');
      expect(dark.getAllByTestId('workflow-map-group-fold')[0].className).not.toContain('transition-');
    } finally {
      getSettings().lowPowerMode = false;
    }
  });

  it('scrolls the fan region alone — the spine never goes sideways', () => {
    const view = mountWave(fanView());
    const fan = view.getByTestId('workflow-map-fan');
    expect(fan.querySelector('.overflow-x-auto')).not.toBeNull();
    expect(view.container.querySelector('[data-testid="workflow-map-wave-body"].overflow-x-auto')).toBeNull();
  });
});

describe('composition — a called run inside its caller', () => {
  /** A root whose `sub` phase calls a DIFFERENT workflow: composition, not a wave. */
  function compositionView(): WorkflowRunMapView {
    return makeView([
      makeRun('root', {
        state: 'running',
        skeleton: [skeletonPhase('plan'), skeletonPhase('sub', { shape: 'call', callTarget: 'inner' })],
        phases: [makePhase('plan'), makePhase('sub', { status: 'running', endedAt: 0, startedAt: 9_900_000 })],
      }),
      makeRun('child', {
        workflowId: 'inner', state: 'running',
        parentItemId: 'root', parentPhaseId: 'sub', parentAttempt: 1, callDepth: 1,
        skeleton: [skeletonPhase('build')],
        phases: [makePhase('build', { status: 'running', endedAt: 0, startedAt: 9_950_000 })],
      }),
    ], 'root');
  }

  it('renders the called run as a chain row keyed by its own id', () => {
    const view = mountWave(compositionView());
    const rows = view.getAllByTestId('workflow-map-composition');
    expect(rows.map((row) => row.dataset.compositionItemId)).toEqual(['child']);
    expect(rows[0].textContent).toContain('inner');
    // `data-item-id` means three different things elsewhere in the app and is
    // walked app-wide as "a timeline row"; a called run is none of them.
    expect(rows[0].dataset.itemId).toBeUndefined();
  });

  it('draws the called run\'s own phases inline — no clicks to see what is running', () => {
    const view = mountWave(compositionView());
    expect(view.getAllByTestId('workflow-map-node').map((node) => node.dataset.phaseId))
      .toEqual(['plan', 'sub', 'build']);
  });
});
