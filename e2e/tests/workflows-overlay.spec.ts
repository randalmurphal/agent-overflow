// The workflows overlay, driven as a human drives it (UI-SPEC
// docs/specs/workflows-system-ui/UI-SPEC.md). Every case here clicks and types
// in the REAL overlay — sidebar footer, home, run detail, sweep, loss preview,
// intake, the §8 keys — against the real engine with mocked providers. Backend
// setup goes through RPCs; assertions are DOM state or wire events, never a
// stubbed store.
//
// The overlay is a sibling of the pane host, so opening a thread is the ONE
// action that breaks out of it (R3) — covered here by clicking a run-map node,
// which mounts the phase's thread as a normal pane beside the tree that was
// never unmounted, and by the `/workflow` case, which never opens the overlay
// at all: it types the command in a real composer and reads what the provider
// got.
//
// The run detail's structure surface is the RUN MAP
// (docs/architecture/workflow-run-map.md): waves and nodes, position =
// progress, solid = happened, marked = now, dashed = not yet. There is no
// expandable child-row tree any more, so nothing below clicks to reveal what a
// run is doing — it reads it off the spine.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  questionResult,
  seedWorkflow,
  seedWorkflowProject,
  setClaudeScenario,
  setGlobalPause,
  singlePhaseWorkflow,
  startWorkflow,
  waitForEnginePause,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

const oneDoneTurn = [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }];

// A tail self-call: `work` decides, `again` calls this same workflow as the
// last declared phase. That is what makes a run a WAVE CHAIN (RUN-MAP §3), so
// the map draws a loop foot the stop request can annotate, and it is the only
// definition shape for which the running row offers a soft stop at all.
const loopWorkflow = `id: loop-flow
name: loop-flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: work
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: loop-flow.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - when:
            eq:
              ref: work.complete
              value: false
          to: again
        - to: done
  - id: again
    shape: call
    call: loop-flow
    args:
      goal: goal
    max_depth: 3
    gate:
      routes:
        - to: done
cleanup: manual
`;

/** The node the map drew for one declared phase of the run being looked at. */
function mapNodeOf(page: import('@playwright/test').Page, phaseId: string) {
  return page.locator(`[data-testid="workflow-map-node"][data-phase-id="${phaseId}"]`);
}

// ---------------------------------------------------------------------------
// The compensation fixture (RUN-MAP §9.7)
// ---------------------------------------------------------------------------
//
// A run engineered so that a step the ENGINE takes grows the map ABOVE a
// reader who has scrolled to its tail. Three parts, each load-bearing:
//
//   - `plan` runs first and holds on the mock's signal, so the map has a
//     stable pre-growth state to measure and the growth happens exactly when
//     the spec releases it — no timing, no polling for a moment.
//   - `port` is a fan-out whose units do not exist until `plan` completes. One
//     ghost row becomes a split bar, branch columns and a join: ~70px of
//     growth, and it sits at the TOP of the map.
//   - `rest-N` never run. A live run draws every declared phase as a dashed
//     ghost (§5.1), so they are free height BELOW the growth — enough that the
//     reader can sit at the tail with `port` far above the fold. That the
//     growth really is above the fold is asserted, not assumed: the fixture
//     drifting must fail this spec rather than quietly making it vacuous.
const COMPENSATE_HOLD = 'hold-compensate';
const COMPENSATE_UNITS = ['alpha', 'beta', 'gamma', 'delta'];
const COMPENSATE_FILLERS = 24;

function growingWorkflow(id: string): string {
  const complete = `    outputs:
      complete:
        schema:
          type: boolean
`;
  const agentPhase = (phaseId: string, next: string, inputs = ''): string => `  - id: ${phaseId}
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: ${id}.md
    access: read-only
${inputs}${complete}    gate:
      routes:
        - to: ${next}
`;
  const unit = (unitId: string): string => `      - id: ${unitId}
        provider: claude
        model: claude-opus-4-7
        prompt: ${id}.md
        access: read-only
        outputs:
          complete:
            schema:
              type: boolean
`;
  const fillers = Array.from({ length: COMPENSATE_FILLERS }, (_, index) =>
    agentPhase(`rest-${index + 1}`, index + 1 === COMPENSATE_FILLERS ? 'done' : `rest-${index + 2}`),
  ).join('');
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
${agentPhase('plan', 'port', `    inputs:
      goal:
        schema:
          type: string
`)}  - id: port
    name: Port in parallel
    shape: fan-out
${complete}    fan_out:
${COMPENSATE_UNITS.map(unit).join('')}    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: ${id}.md
      access: read-only
    gate:
      routes:
        - to: rest-1
${fillers}cleanup: manual
`;
}

test('the sidebar footer opens home and a parked gate resolves from its detail', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-gate', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-gate-project',
    'gate-flow',
    humanGateWorkflow('gate-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'gate-flow', 'Port the parser');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  await harness.open(page);
  // §6: one footer row, one count, amber only because a human is blocked.
  await expect(page.getByTestId('sidebar-workflows-attention')).toHaveText('1');

  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('workflows-overlay')).toBeVisible();
  const mapNode = (phaseId: string) => mapNodeOf(page, phaseId);

  // §3.2: project group → needs-attention list → this run, carrying its state
  // word and nothing else about the machinery behind it (R2).
  await expect(page.getByTestId('workflow-project-group')).toContainText('overlay-gate-project');
  const row = page.getByTestId('workflow-run-row').filter({ hasText: 'Port the parser' });
  await expect(row).toContainText('Review gate');
  await row.click();

  // §4.1 header + §4.2 map + §4.3 action row.
  const detail = page.getByTestId('workflow-run-detail');
  await expect(detail).toHaveAttribute('data-item-id', item.id);
  await expect(page.getByTestId('workflow-run-state')).toHaveText('Review gate');
  await expect(page.getByTestId('workflow-run-title')).toHaveText('Port the parser');
  await expect(page.getByTestId('workflow-digest')).toContainText('What happened');

  // RUN-MAP §1: position is progress. `plan` happened, `review` is where the
  // run IS, `apply` is drawn but has not happened — one wave, expanded in
  // place, no clicks to see any of it.
  await expect(page.getByTestId('workflow-run-map')).toBeVisible();
  await expect(page.getByTestId('workflow-map-wave')).toHaveCount(1);
  await expect(mapNode('plan')).toHaveAttribute('data-signal', 'done');
  await expect(mapNode('review')).toHaveAttribute('data-signal', 'parked');
  await expect(mapNode('apply')).toHaveAttribute('data-ghost', 'true');
  // The frontier strip names where the run is and what is blocking it; the
  // header's position label comes off the same frontier, not the frozen SQL
  // counter (§11.4).
  await expect(page.getByTestId('workflow-map-frontier')).toContainText('review');
  await expect(page.getByTestId('workflow-map-blocker')).toHaveText('Review gate');
  await expect(page.getByTestId('workflow-run-hint')).toContainText('review');

  // The primary names the phase the gate routes to — read off the definition,
  // not invented.
  const approve = page.getByTestId('workflow-action').filter({ hasText: 'Approve → apply' });
  const done = waitForWorkflowState(harness, item.id, 'done');
  await approve.click();
  await done;

  // §4.4: the last parked run resolved, so the sweep is exhausted.
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('1 approved');
});

test('the map tracks a run live, from held to parked, without leaving the detail', async ({
  harness,
  page,
}) => {
  // Staged under the global pause: the run is admitted and persisted `running`
  // with its first phase HELD, which is what lets this spec look at a live map
  // before anything has happened and then watch it move.
  await setGlobalPause(harness, true);
  await setClaudeScenario(harness, 'overlay-live', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-live-project',
    'live-flow',
    humanGateWorkflow('live-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'live-flow', 'Watch it move');

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Watch it move' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);

  const mapNode = (phaseId: string) => mapNodeOf(page, phaseId);
  // The held attempt is the marked node — "now" is a position, not a status —
  // and everything the frozen definition says comes after it is already drawn
  // as a ghost element rather than waiting to be inserted (§10).
  await expect(mapNode('plan')).toHaveAttribute('data-signal', 'running');
  await expect(mapNode('plan').locator('[data-run-map-now="true"]')).toBeVisible();
  await expect(mapNode('review')).toHaveAttribute('data-ghost', 'true');
  await expect(mapNode('apply')).toHaveAttribute('data-ghost', 'true');

  // Release the engine and let the real run reach its gate. Nothing below
  // navigates, reopens or reloads: what changes the map is the `workflow:*`
  // event stream patching the entity in place (RUN-MAP §4.4).
  const parked = waitForWorkflowState(harness, item.id, 'needs-human', 'gate');
  await setGlobalPause(harness, false);
  await waitForEnginePause(harness, false);
  await parked;

  await expect(mapNode('plan')).toHaveAttribute('data-signal', 'done');
  await expect(mapNode('review')).toHaveAttribute('data-signal', 'parked');
  await expect(mapNode('review').locator('[data-run-map-now="true"]')).toBeVisible();
  await expect(mapNode('apply')).toHaveAttribute('data-ghost', 'true');
  await expect(page.getByTestId('workflow-map-blocker')).toHaveText('Review gate');
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);
});

test('scrolling away holds the reader while the run moves, and only the chip brings them back', async ({
  harness,
  page,
}) => {
  // RUN-MAP §9, the intentionality contract, in a real engine: escape is
  // EVENT-SOURCED (a wheel-up is intent; a programmatic scroll never is), and
  // re-engaging is EXPLICIT ONLY. The unit tests prove the decisions against a
  // stated layout; what only a browser can prove is that a frontier moving
  // under a reader who left it does not move their viewport — and that the
  // chip still takes them back when they ask.
  await setGlobalPause(harness, true);
  await setClaudeScenario(harness, 'overlay-follow', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-follow-project',
    'follow-flow',
    humanGateWorkflow('follow-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'follow-flow', 'Follow the frontier');

  // Short enough that the run detail overflows: a surface that fits has no
  // scroll position to hold and would make every assertion below vacuous.
  await page.setViewportSize({ width: 1280, height: 320 });
  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Follow the frontier' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);

  const body = page.getByTestId('workflows-overlay-body');
  const marker = page.locator('[data-run-map-now="true"]');
  await expect(mapNodeOf(page, 'plan').locator('[data-run-map-now="true"]')).toBeVisible();
  await expect
    .poll(() => body.evaluate((el) => el.scrollHeight - el.clientHeight))
    .toBeGreaterThan(0);

  // A wheel UP is the reader going back for context above the frontier — the
  // one thing that disengages follow. The run opened running, so follow was ON
  // until this gesture (§9.4).
  await expect(page.getByTestId('workflow-map-follow')).toHaveCount(0);
  await body.hover();
  await page.mouse.wheel(0, -2000);
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();
  await expect.poll(() => body.evaluate((el) => el.scrollTop)).toBe(0);

  // The run advances to its gate: the frontier moves from `plan` to `review`,
  // which is exactly the move follow would have chased.
  const parked = waitForWorkflowState(harness, item.id, 'needs-human', 'gate');
  await setGlobalPause(harness, false);
  await waitForEnginePause(harness, false);
  await parked;
  await expect(mapNodeOf(page, 'review')).toHaveAttribute('data-signal', 'parked');
  await expect(mapNodeOf(page, 'review').locator('[data-run-map-now="true"]')).toHaveCount(1);

  // Nothing wrote a scroll position the reader did not ask for — not the
  // frontier move, not the refetch that replaced the whole view behind it.
  expect(await body.evaluate((el) => el.scrollTop)).toBe(0);
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();

  // The one way back, and it lands ON the marker rather than near it.
  await page.getByTestId('workflow-map-follow').click();
  await expect.poll(() => body.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  await expect(marker).toBeInViewport();

  // §9.10, the actionability rule, measured from the position the app itself
  // chose: where that click parked the reader IS the resting position, so a
  // second visit to it has nothing to offer. Read once the glide has settled —
  // two identical samples, because the glide is 250ms of rAF writes.
  let sample = -1;
  await expect
    .poll(async () => {
      const top = await body.evaluate((el) => el.scrollTop);
      const settled = top === sample;
      sample = top;
      return settled;
    })
    .toBe(true);
  const resting = sample;
  // A rest line with room above it, so the nudge below is a real journey
  // rather than a clamp. Asserted so fixture drift fails LOUDLY.
  expect(resting).toBeGreaterThan(40);

  // The reader leaves again — a real wheel, because escape is event-sourced —
  // and then comes back to exactly where the click had put them. The return
  // is a PROGRAMMATIC scroll on purpose: §9.2 makes that the one input which
  // changes no engagement at all, so it moves the viewport and nothing else.
  await body.hover();
  await page.mouse.wheel(0, -2000);
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();
  await expect.poll(() => body.evaluate((el) => el.scrollTop)).toBe(0);

  await body.evaluate((el, top) => { el.scrollTop = top; }, resting);
  await expect(page.getByTestId('workflow-map-follow')).toHaveCount(0);

  // And hiding it re-engaged nothing (§9.3): 40px off the rest line the offer
  // is real again while the marker is still on screen — which is exactly the
  // case an ENGAGED controller keeps the chip hidden for, so the chip coming
  // back is the reader still being disengaged.
  await body.evaluate((el, top) => { el.scrollTop = top - 40; }, resting);
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();
  const markerBox = await marker.boundingBox();
  const bodyBox = await body.boundingBox();
  if (!markerBox || !bodyBox) throw new Error('the map did not lay out');
  expect(markerBox.y).toBeGreaterThanOrEqual(bodyBox.y);
  expect(markerBox.y + markerBox.height).toBeLessThanOrEqual(bodyBox.y + bodyBox.height);
});

test('the map grows above a reader without moving what they are reading', async ({
  harness,
  page,
}) => {
  // RUN-MAP §9.7, the compensation clause: a map-initiated height change ABOVE
  // a reader who is not following is wrapped in an anchor hold, so the run
  // growing is not a reason for the page to move under them. The unit tests
  // prove the arithmetic against a stated layout; only a browser can prove the
  // whole chain — real engine growth, Svelte's flush, the anchor the descent
  // picks in the REAL DOM, and the one compensating write.
  await setClaudeScenario(harness, 'overlay-compensate', [
    {
      steps: [
        { waitSignal: { name: COMPENSATE_HOLD } },
        { emit: { lines: [doneResult({ complete: true })] } },
      ],
    },
  ]);
  const project = await seedWorkflow(
    harness,
    'overlay-compensate-project',
    'grow-flow',
    growingWorkflow('grow-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'grow-flow', 'Grow above me');

  // The run holds on its FIRST turn, so the pre-growth map is a fact rather
  // than a moment: `plan` running, `port` a one-row ghost, 24 ghosts below it.
  const planMock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'overlay-compensate' && event.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === planMock.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === COMPENSATE_HOLD,
  );

  await page.setViewportSize({ width: 1280, height: 400 });
  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Grow above me' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);
  await expect(mapNodeOf(page, 'plan')).toHaveAttribute('data-signal', 'running');
  await expect(mapNodeOf(page, 'port')).toHaveAttribute('data-ghost', 'true');
  await expect(page.getByTestId('workflow-map-fan')).toHaveCount(0);

  const body = page.getByTestId('workflows-overlay-body');
  // What the reader is reading: the last ghost on the spine, which nothing
  // this run does will touch.
  const reading = mapNodeOf(page, `rest-${COMPENSATE_FILLERS}`);

  // Wheel UP disengages follow (§9.2 — escape is event-sourced), and wheel
  // DOWN then travels to the tail without re-engaging it (§9.3 — only the chip
  // re-engages). A programmatic scroll would do neither, which is the point.
  await body.hover();
  await page.mouse.wheel(0, -2000);
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();
  await page.mouse.wheel(0, 4000);
  await expect
    .poll(() => body.evaluate((el) => el.scrollTop >= el.scrollHeight - el.clientHeight - 1))
    .toBe(true);

  const viewport = await body.boundingBox();
  const growing = await mapNodeOf(page, 'port').boundingBox();
  const readingBefore = await reading.boundingBox();
  if (!viewport || !growing || !readingBefore) throw new Error('the map did not lay out');

  // Preconditions, asserted so fixture drift fails LOUDLY instead of turning
  // this into a test of nothing: the growth is entirely above the viewport
  // top, and the element being watched is inside it.
  expect(growing.y + growing.height).toBeLessThan(viewport.y);
  expect(readingBefore.y).toBeGreaterThanOrEqual(viewport.y);
  expect(readingBefore.y + readingBefore.height).toBeLessThanOrEqual(viewport.y + viewport.height);

  const before = await body.evaluate((el) => ({ top: el.scrollTop, height: el.scrollHeight }));

  // The one step: `plan` finishes, `port` starts, and its units are stamped.
  await harness.rpc('HarnessMockCommand', planMock.mockId, {
    type: 'advance',
    name: COMPENSATE_HOLD,
  });
  await expect(mapNodeOf(page, 'plan')).toHaveAttribute('data-signal', 'done');
  await expect(page.getByTestId('workflow-map-fan')).toBeVisible();
  await expect(page.getByTestId('workflow-map-branch').first()).toBeVisible();

  // (a) The reader's line held its place on screen. Polled, because the
  // refetch behind the patch (§4.4) is a second apply and it must hold too.
  //
  // Within a pixel, not to the pixel: the anchor delta is fractional (the
  // controller ignores anything under half a pixel as noise the reader cannot
  // see) and `scrollTop` lands on the device pixel grid. A regression here is
  // the fan's whole height, ~70px, so the bound is nowhere near it.
  await expect
    .poll(async () => Math.abs(((await reading.boundingBox())?.y ?? -1e6) - readingBefore.y))
    .toBeLessThanOrEqual(1);

  const after = await body.evaluate((el) => ({ top: el.scrollTop, height: el.scrollHeight }));
  const grew = after.height - before.height;
  // (b) It held because something WROTE the scroll position: the document
  // grew, and the viewport moved by exactly that much. A guard on the size
  // keeps a fixture that stopped growing from passing this silently.
  expect(grew).toBeGreaterThan(8);
  expect(Math.abs((after.top - before.top) - grew)).toBeLessThanOrEqual(1);

  // (c) Compensation is not follow: the frontier moved from `plan` into the
  // fan and the reader was left exactly where they were, still disengaged.
  await expect(page.getByTestId('workflow-map-follow')).toBeVisible();
  await expect(page.locator('[data-run-map-now="true"]')).toHaveCount(1);
  await expect(mapNodeOf(page, 'plan').locator('[data-run-map-now="true"]')).toHaveCount(0);
});

test('a map node with a thread opens it as a pane and leaves the overlay', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-node-thread', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-node-thread-project',
    'node-flow',
    humanGateWorkflow('node-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'node-flow', 'Open from a node');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  // The thread the `plan` attempt ran in — read from the backend so the
  // assertion below names a real pane rather than whatever happened to mount.
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  const planThreadId = detail.phases.find((phase) => phase.phaseId === 'plan')?.threadId ?? '';
  expect(planThreadId).not.toBe('');

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Open from a node' }).click();
  await expect(page.getByTestId('workflow-run-map')).toBeVisible();

  // R3: a thread is the one thing that breaks out of the overlay, and the map
  // node is the affordance. The pane tree underneath was never unmounted, so
  // this is a mount beside it, not a surface swap.
  await mapNodeOf(page, 'plan').getByTestId('workflow-map-node-label').click();
  await expect(page.locator(`[data-ui-surface="chat"][data-thread-id="${planThreadId}"]`)).toBeVisible();
  await expect(page.getByTestId('workflows-overlay')).toBeHidden();
});

test('a stop request lands on the loop foot the map draws for a wave chain', async ({
  harness,
  page,
}) => {
  // Held under the global pause for the whole test: a soft stop is a standing
  // request about the NEXT call boundary, so the run only has to be running and
  // rooted for the affordance to be real.
  await setGlobalPause(harness, true);
  await setClaudeScenario(harness, 'overlay-soft-stop', oneDoneTurn);
  const project = await seedWorkflow(harness, 'overlay-loop-project', 'loop-flow', loopWorkflow);
  const item = await startWorkflow(harness, project.projectId, 'loop-flow', 'Loop until clean');

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Loop until clean' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);

  // §3: the tail self-call is not a phase node — it is the loop decision at the
  // foot of the segment, with both outcomes drawn as ghosts until it resolves.
  await expect(mapNodeOf(page, 'again')).toHaveAttribute('data-node-kind', 'decision');
  const decision = page.getByTestId('workflow-map-decision');
  await expect(decision).toContainText('lap 1');
  await expect(decision).toContainText('clean → done');
  await expect(page.getByTestId('workflow-map-soft-stop')).toHaveCount(0);

  const armed = harness.waitForEvent<{ itemId: string; armed: boolean }>(
    'workflow:soft-stop',
    (event) => event.itemId === item.id && event.armed === true,
  );
  await page.getByTestId('workflow-action').filter({ hasText: 'Stop after this wave' }).click();
  await armed;

  // The request is a fact about the loop, so the loop foot is where it reads —
  // and the action row flips to the one place its pendingness is visible.
  await expect(page.getByTestId('workflow-map-soft-stop')).toContainText('stops after this wave');
  await expect(
    page.getByTestId('workflow-action').filter({ hasText: 'Stopping after this wave' }),
  ).toBeVisible();
});

test('the sweep steps with j / k, auto-advances past a receipt, and lands on all clear', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-sweep', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-sweep-project',
    'sweep-flow',
    humanGateWorkflow('sweep-flow'),
  );
  // Sequential so the sweep order (oldest parked first) is a fact, not a race.
  const first = await startWorkflow(harness, project.projectId, 'sweep-flow', 'First parked run');
  await waitForWorkflowState(harness, first.id, 'needs-human', 'gate');
  const second = await startWorkflow(harness, project.projectId, 'sweep-flow', 'Second parked run');
  await waitForWorkflowState(harness, second.id, 'needs-human', 'gate');

  await harness.open(page);
  await expect(page.getByTestId('sidebar-workflows-attention')).toHaveText('2');
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'First parked run' }).click();
  const detail = page.getByTestId('workflow-run-detail');
  const counter = page.getByTestId('workflow-sweep-counter');
  await expect(detail).toHaveAttribute('data-item-id', first.id);
  await expect(counter).toHaveText('1 of 2');

  // §8: j / k step the sweep without touching the mouse, and wrap.
  await page.keyboard.press('j');
  await expect(detail).toHaveAttribute('data-item-id', second.id);
  await expect(counter).toHaveText('2 of 2');
  await page.keyboard.press('k');
  await expect(detail).toHaveAttribute('data-item-id', first.id);

  const firstDone = waitForWorkflowState(harness, first.id, 'done');
  await page.getByTestId('workflow-action').filter({ hasText: 'Approve' }).first().click();
  await firstDone;
  // The receipt holds the resolved run on screen long enough to read; the sweep
  // then steps itself to the run that still needs a human (§4.4).
  await expect(detail).toHaveAttribute('data-item-id', second.id);

  const secondDone = waitForWorkflowState(harness, second.id, 'done');
  await page.getByTestId('workflow-action').filter({ hasText: 'Approve' }).first().click();
  await secondDone;
  await expect(page.getByTestId('workflow-all-clear')).toBeVisible();
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('2 approved');
});

test('leaving a run returns home at the top, not where the run was scrolled', async ({
  harness,
  page,
}) => {
  // RUN-MAP §9.9: ONE scroller serves every level of the overlay, and where a
  // swap leaves the reader is a stated contract. This pins the PROMISE, not
  // one mechanism — the overlay's own reset, the branch swap's clamp and the
  // map's placement all currently deliver it, and the point is that a refactor
  // which removes any of them still has to keep the answer. Both levels here
  // are deliberately taller than the viewport, so a preserved offset would be
  // a real one rather than a clamp.
  await setClaudeScenario(harness, 'overlay-scroll', oneDoneTurn);
  const project = await seedWorkflowProject(
    harness,
    'overlay-scroll-project',
    [{ name: 'scroll-flow', yaml: singlePhaseWorkflow('scroll-flow', '        - to: done') }],
    [{ workflow: 'scroll-flow', goal: 'Scrolled run', count: 12, target: 'done' }],
  );
  expect(project.workItemIds.length).toBe(12);

  await page.setViewportSize({ width: 1280, height: 420 });
  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();

  const body = page.getByTestId('workflows-overlay-body');
  await expect(page.getByTestId('workflow-run-row').first()).toBeVisible();
  await body.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  const homeScroll = await body.evaluate((el) => el.scrollTop);
  expect(homeScroll).toBeGreaterThan(0);

  await page.getByTestId('workflow-run-row').first().click();
  await expect(page.getByTestId('workflow-run-detail')).toBeVisible();
  await body.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  expect(await body.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);

  // Back to a home that is still long enough to hold the run detail's offset.
  await page.getByTestId('workflows-back').click();
  await expect(page.getByTestId('workflow-project-group')).toBeVisible();
  await expect.poll(() => body.evaluate((el) => el.scrollTop)).toBe(0);
});

test('discard previews exactly what it would destroy before it destroys it', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-discard', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-discard-project',
    'discard-flow',
    singlePhaseWorkflow('discard-flow', '        - to: done', 'write'),
  );
  const item = await startWorkflow(harness, project.projectId, 'discard-flow', 'Finished run');
  await waitForWorkflowState(harness, item.id, 'done');

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Finished run' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);

  // §4.5 / D23: the row's Discard opens the loss preview. It does not destroy.
  await page.getByTestId('workflow-action').filter({ hasText: 'Discard' }).click();
  const dialog = page.getByTestId('workflow-discard-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('workflow-discard-worktree')).toHaveCount(1);
  await expect(dialog).toContainText('The run record is kept.');
  const stillThere = await harness.rpc<{ item: { state: string } }>('WorkflowGetItem', item.id);
  expect(stillThere.item.state).toBe('done');

  await page.getByTestId('workflow-discard-confirm').click();
  await expect(page.getByRole('alert')).toContainText('Discarded');
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('1 discarded');
});

test('New run starts a workflow from the overlay', async ({ harness, page }) => {
  await setClaudeScenario(harness, 'overlay-intake', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-intake-project',
    'intake-flow',
    singlePhaseWorkflow('intake-flow', '        - to: done'),
  );

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflows-new-run').click();

  // §5.1: Project · Goal · Workflow · Base branch · step mode, primary `Start`.
  await expect(page.getByTestId('workflow-intake-dialog')).toBeVisible();
  await expect(page.getByTestId('workflow-intake-submit')).toBeDisabled();
  await page.getByTestId('workflow-intake-goal').fill('Start from the overlay');
  await page
    .locator('[data-testid="workflow-intake-workflow"][data-workflow-id="intake-flow"]')
    .click();
  // The workflow's own declared field is a plain form field (R2), and Start
  // stays refused until it has a value — with the field named, not the schema.
  await expect(page.getByTestId('workflow-intake-error')).toContainText('goal');
  await page.getByTestId('workflow-seed-goal').fill('Start from the overlay');

  const started = harness.waitForEvent<{ projectId: string }>(
    'workflow:item-state',
    (event) => event.projectId === project.projectId,
  );
  await page.getByTestId('workflow-intake-submit').click();
  await started;
  await expect(page.getByRole('alert')).toContainText('Started — intake-flow');
  await expect(
    page.getByTestId('workflow-run-row').filter({ hasText: 'Start from the overlay' }),
  ).toBeVisible();
});

test('a question answers from the footer input, and typing there never fires the §8 keys', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-question', [
    { label: 'question', steps: [{ emit: { lines: [questionResult('Which option?')] } }] },
    { label: 'answer', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'overlay-question-project',
    'ask-flow',
    singlePhaseWorkflow('ask-flow', '        - to: done'),
  );
  const item = await startWorkflow(harness, project.projectId, 'ask-flow', 'Needs an answer');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'question');

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Needs an answer' }).click();
  await expect(page.getByTestId('workflow-question')).toContainText('Which option?');

  // §8: `a` on a question focuses the answer box rather than committing —
  // there is nothing to commit until it has text.
  const answer = page.getByTestId('workflow-answer-input');
  await page.keyboard.press('a');
  await expect(answer).toBeFocused();

  // What follows is text, not commands. Without the editable-target guard the
  // `t` in "option" would take the run over and tear this surface down.
  await page.keyboard.type('use option A');
  await expect(answer).toHaveValue('use option A');
  await expect(page.getByTestId('workflow-run-detail')).toBeVisible();

  const done = waitForWorkflowState(harness, item.id, 'done');
  await page.keyboard.press('Enter');
  await done;
});

test('/workflow completes in the composer and expands on the wire, not in the transcript', async ({
  harness,
  page,
}) => {
  // Two turns: one invoking the command at the front of the draft, one
  // invoking it mid-sentence. Both are real invocations (D31, amended).
  await setClaudeScenario(harness, 'overlay-composer', [...oneDoneTurn, ...oneDoneTurn]);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'overlay-composer-project',
        repo: {},
        workflows: {
          definitions: [
            {
              name: 'ctx-flow',
              yaml: singlePhaseWorkflow('ctx-flow', '        - to: done'),
              prompts: { 'ctx-flow.md': 'Complete this phase.' },
            },
          ],
        },
        threads: [
          { title: 'Composer thread', turns: [{ userText: 'hello there', items: [] }] },
        ],
      },
    ],
  });
  expect(seed.projects[0].threadIds).toHaveLength(1);

  await harness.open(page);
  await page.getByText('Composer thread').click();
  const composer = page.getByLabel('Message Input');
  await composer.click();

  // D31: `/` at the start of the draft offers the registered commands, and
  // selecting one completes the WORD — no token, no pasted block.
  await composer.pressSequentially('/wo');
  await expect(page.getByTestId('slash-popover')).toBeVisible();
  await page.getByTestId('slash-option').filter({ hasText: '/workflow' }).click();
  await expect(composer).toHaveValue('/workflow ');
  await composer.pressSequentially('start the release');

  // The mock reports the text it receives on the wire, which is the only
  // place the expansion exists.
  const received = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.report.kind === 'user_input',
  );
  await page.keyboard.press('Enter');
  const wireText = (await received).report.input ?? '';

  // Typed text first (it is the instruction), block second (it is context).
  // The leading "\n" is Claude's outbound slash guard
  // (internal/provider/claude/slash_guard.go): a first word that is
  // command-shaped would be routed to the CLI's own command router and never
  // reach the model, so the send prefixes one newline. `/workflow` is an AO
  // composer command, not a provider command, so it takes the guard.
  expect(wireText.startsWith('\n/workflow start the release\n\n')).toBe(true);
  expect(wireText).toContain('Agent Overflow workflows are available');
  expect(wireText).toContain('agent-overflow run start');
  expect(wireText).toContain('ctx-flow');

  // The transcript keeps exactly what was typed, with the command word in the
  // accent colour — the block is never persisted and never rendered.
  const bubble = page.getByTestId('user-message-bubble').last();
  await expect(bubble).toHaveText('/workflow start the release');
  await expect(bubble.getByTestId('user-message-command')).toHaveText('/workflow');
  expect(await bubble.textContent()).not.toContain('agent-overflow run start');

  // Let the first turn finish, so the next Enter sends rather than queues.
  await harness.waitForEvent('provider:turn_completed');

  // Same command, mid-sentence. The menu opens on a word that is not the
  // first, the completion lands in place, and the word is coloured where it
  // sits — the accent is the signal that a prose mention is live.
  await composer.click();
  await composer.pressSequentially('now check the /wo');
  await expect(page.getByTestId('slash-popover')).toBeVisible();
  await page.getByTestId('slash-option').filter({ hasText: '/workflow' }).click();
  await expect(composer).toHaveValue('now check the /workflow ');
  await composer.pressSequentially('list');
  await expect(page.getByTestId('composer-command-highlight')).toContainText('/workflow');

  const secondReceived = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.report.kind === 'user_input',
  );
  await page.keyboard.press('Enter');
  const secondWireText = (await secondReceived).report.input ?? '';

  expect(secondWireText.startsWith('now check the /workflow list\n\n')).toBe(true);
  expect(secondWireText).toContain('Agent Overflow workflows are available');

  const secondBubble = page.getByTestId('user-message-bubble').last();
  await expect(secondBubble).toHaveText('now check the /workflow list');
  await expect(secondBubble.getByTestId('user-message-command')).toHaveText('/workflow');
});

test('a view-only session disables every mutating affordance', async ({ harness, page }) => {
  await setClaudeScenario(harness, 'overlay-remote', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-remote-project',
    'remote-flow',
    humanGateWorkflow('remote-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'remote-flow', 'Remote parked run');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  // The manifest's `remote` bit is computed from the peer's locality and the
  // harness binds loopback only, so a LAN peer cannot be produced from here.
  // Patching that one field on the wire hands the SPA exactly the manifest a
  // remote browser would receive; everything downstream of it is production
  // code, including the wsClient validation that publishes the bit.
  await page.route('**/bootstrap.json*', async (route) => {
    const response = await route.fetch();
    const manifest = (await response.json()) as Record<string, unknown>;
    await route.fulfill({ response, body: JSON.stringify({ ...manifest, remote: true }) });
  });

  await harness.open(page);
  await page.getByTestId('sidebar-workflows-button').click();

  // §10: home's controls all mutate, so all of them go dead with one reason.
  await expect(page.getByTestId('workflows-pause-all')).toBeDisabled();
  const newRun = page.getByTestId('workflows-new-run');
  await expect(newRun).toBeDisabled();
  await expect(newRun).toHaveAttribute('title', 'Local only');
  // The project filter is view state, not a mutation — it stays live.
  await expect(page.getByTestId('workflows-project-filter')).toBeEnabled();

  await page.getByTestId('workflow-run-row').filter({ hasText: 'Remote parked run' }).click();
  const actions = page.getByTestId('workflow-action');
  await expect(actions.first()).toBeVisible();
  for (const action of await actions.all()) {
    await expect(action).toBeDisabled();
    await expect(action).toHaveAttribute('title', 'Local only');
  }

  // The guard is not just visual: the §8 keys are refused too, so the run is
  // still parked afterwards.
  await page.keyboard.press('a');
  const after = await harness.rpc<{ item: { state: string } }>('WorkflowGetItem', item.id);
  expect(after.item.state).toBe('needs-human');
});
