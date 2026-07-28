// Activity-run auto-collapse against a real layout engine: the zero-motion
// proof. The unit suites verify the registry hold, the geometry predicate,
// and the gate's refusals — what only a real browser can verify is the claim
// the whole design stands on: releasing a held-open run moves NOTHING the
// reader can see, whether the run's row is still mounted (row-observer
// compensation) or long unmounted (estimate swap + the anchored
// transaction's restore).
import { describe, expect, it } from 'vitest';
// Real production cascade: the clip's cap and every row height come from
// app.css, and eligibility distances are measured against them.
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { updateSetting } from '../../stores/settings.svelte';
import {
  distanceToBottom,
  mountTimeline,
  setupTimelineHarness,
  userScrollTo,
  waitForQuietBottom,
  type MountedTimeline,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
/** Enough rows to overflow the clip's cap, so collapsing removes real height. */
const RUN_ROWS = 20;
/** Rects are fractional (fonts, borders, DPR). */
const DRIFT_PX = 1;

function tool(id: string, index: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    summary: `Bash: inspect fixture ${id}`,
    createdAt: index,
    updatedAt: index,
  });
}

function prose(id: string, index: number, threadId: string, tall = false): Item {
  const lead = `Reply ${id}: prose breaks the rail, so the run before it can never grow again.`;
  const summary = tall
    ? [
        lead,
        `The gate measures distance from the tail in content space, so filler like this is what pushes a settled run past a viewport of scrollback — paragraph two of reply ${id} exists purely to take up honest rendered height.`,
        `And a third paragraph, because the fixture needs each of these rows to cost real pixels against the production cascade rather than a one-line estimate — reply ${id} closes here.`,
      ].join('\n\n')
    : lead;
  return makeItem({
    id,
    threadId,
    itemIndex: index,
    summary,
    createdAt: index,
    updatedAt: index,
  });
}

describe('activity run — auto-collapse off-screen', () => {
  const THREAD_ID = 'thread-run-autocollapse';

  // Two runs: a finished one — the reference for what a header-only run
  // measures — and the live one under test, separated by prose.
  const REFERENCE_ROWS = 3;
  const FIRST_RUN_INDEX = REFERENCE_ROWS + 1;
  const AFTER_RUN_INDEX = FIRST_RUN_INDEX + RUN_ROWS;

  function items(): Item[] {
    const built: Item[] = [];
    for (let i = 0; i < REFERENCE_ROWS; i += 1) built.push(tool(`b${i}`, i, THREAD_ID));
    built.push(prose('p0', REFERENCE_ROWS, THREAD_ID));
    for (let i = 0; i < RUN_ROWS; i += 1) {
      built.push(tool(`a${i}`, FIRST_RUN_INDEX + i, THREAD_ID));
    }
    return built;
  }

  function clip(scrollEl: HTMLElement): HTMLElement | null {
    return scrollEl.querySelector('[data-testid="activity-run-clip"]');
  }

  function runEl(scrollEl: HTMLElement, runId: string): HTMLElement | null {
    return scrollEl.querySelector(`[data-run-id="${runId}"]`);
  }

  async function mountHeldOpenSettledRun(): Promise<
    MountedTimeline & { runId: string; lastProse: Item }
  > {
    // The harness installs GetSettings only; changing one needs the write
    // side too, and a missing mock means the default silently stands — a
    // header-only run and a vacuous suite.
    setBindingMock('UpdateSettings', async (patch: unknown) =>
      makeSettings(patch as Parameters<typeof makeSettings>[0]));
    await updateSetting('activityRunDefault', 'collapsed');
    const mountedTimeline = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const { pane, scrollEl } = mountedTimeline;
    // Collapsed default, live run: open clip, exactly one (the reference run
    // is header-only). If this renders header-only, everything below is
    // vacuous.
    expect(scrollEl.querySelectorAll('[data-testid="activity-run-clip"]')).toHaveLength(1);
    const subject = clip(scrollEl)!.closest('[data-run-id]') as HTMLElement;
    const runId = subject.dataset.runId!;
    expect(subject.dataset.live).toBe('true');

    // Prose settles the run. The old behavior closed the clip here; the new
    // one holds it open — nobody has moved on yet.
    const lastProse = prose('p1', AFTER_RUN_INDEX, THREAD_ID);
    pane.upsertItem(lastProse);
    await waitForQuietBottom(scrollEl, 'settle after closing prose', QUIET_BOTTOM);
    expect(subject.dataset.live).toBe('false');
    expect(subject.dataset.collapsed).toBe('false');
    expect(clip(scrollEl)).not.toBeNull();
    return { ...mountedTimeline, runId, lastProse };
  }

  /** Append `count` prose rows after the run and settle back to the bottom. */
  async function growTail(
    mounted: MountedTimeline,
    count: number,
    tall: boolean,
  ): Promise<Item> {
    let last: Item | undefined;
    for (let i = 0; i < count; i += 1) {
      last = prose(`t${i}`, AFTER_RUN_INDEX + 1 + i, THREAD_ID, tall);
      mounted.pane.upsertItem(last);
    }
    await waitForQuietBottom(mounted.scrollEl, 'tail growth settle', QUIET_BOTTOM);
    return last!;
  }

  /**
   * Record the anchor row's viewport position, fire `trigger`, then keep
   * sampling every frame until `done` has held for ten frames. The
   * zero-motion assertions run over exactly this record — the first sample
   * predates the trigger, so a jump on the trigger's own flush cannot hide.
   */
  async function sampleTopsUntil(
    anchorEl: () => HTMLElement,
    trigger: () => void,
    done: () => boolean,
    label: string,
  ): Promise<number[]> {
    const tops: number[] = [anchorEl().getBoundingClientRect().top];
    trigger();
    let doneFrames = 0;
    for (let i = 0; i < 300 && doneFrames < 10; i += 1) {
      await raf();
      tops.push(anchorEl().getBoundingClientRect().top);
      if (done()) doneFrames += 1;
    }
    if (doneFrames === 0) throw new Error(`timed out sampling: ${label}`);
    return tops;
  }

  function expectNoFrameMotion(tops: number[]): void {
    for (let i = 1; i < tops.length; i += 1) {
      expect(Math.abs(tops[i] - tops[i - 1])).toBeLessThanOrEqual(DRIFT_PX);
    }
    expect(Math.abs(tops[tops.length - 1] - tops[0])).toBeLessThanOrEqual(DRIFT_PX);
  }

  it('collapses a mounted off-screen run without moving the reader a pixel', async () => {
    const mounted = await mountHeldOpenSettledRun();
    const { pane, scrollEl, runId } = mounted;
    // An engagement blocker, so the release fires when THIS test says so —
    // not mid-glide during the growth below, where the spring's own motion
    // would drown the measurement.
    pane.setDiffCardExpanded('a5', 'src/blocker.ts', true);

    // Enough scrollback to be eligible (~1000px, past the viewport and the
    // tail-distance threshold), little enough that the run's row stays well
    // inside the virtualizer's 1800px buffer: this is the mounted-row
    // compensation path.
    const lastProse = await growTail(mounted, 6, true);
    const subject = runEl(scrollEl, runId);
    expect(subject).not.toBeNull();
    // Off-screen above, and still open: engagement blocked the release
    // through the real trigger cascade.
    expect(subject!.getBoundingClientRect().bottom).toBeLessThan(
      scrollEl.getBoundingClientRect().top,
    );
    expect(subject!.dataset.collapsed).toBe('false');

    // Unblock, then hand the gate its trigger. The scroll-end path is the
    // one that reaches it with no geometry change: dispatching `scroll` at
    // the current offset re-arms the virtualizer's write-settle timer,
    // exactly what the pin's own writes do after any tail activity. (A
    // bumped item stamp is NOT a trigger — `updatedAt` sits outside
    // `itemTimelineStructureKey`, so it bumps nothing the gate's effects
    // watch.)
    pane.setDiffCardExpanded('a5', 'src/blocker.ts', undefined);
    const anchor = () =>
      scrollEl.querySelector(`[data-item-id="${lastProse.id}"]`) as HTMLElement;
    const tops = await sampleTopsUntil(
      anchor,
      () => scrollEl.dispatchEvent(new Event('scroll')),
      () => runEl(scrollEl, runId)?.dataset.collapsed === 'true',
      'mounted run to auto-collapse',
    );

    expectNoFrameMotion(tops);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(QUIET_BOTTOM.epsilonPx);
    expect(clip(scrollEl)).toBeNull();
    // The end state is indistinguishable from a run that was never open —
    // priced against the reference run in the same fixture, because the
    // run's own box adds a line of its own either way.
    const reference = scrollEl.querySelector(
      '[data-testid="activity-run"]',
    ) as HTMLElement;
    expect(reference.dataset.runId).not.toBe(runId);
    expect(
      Math.abs(
        runEl(scrollEl, runId)!.getBoundingClientRect().height
          - reference.getBoundingClientRect().height,
      ),
    ).toBeLessThanOrEqual(DRIFT_PX);
  });

  it('collapses an unmounted run purely in estimate space, still moving nothing', async () => {
    const mounted = await mountHeldOpenSettledRun();
    const { pane, scrollEl, runId } = mounted;
    pane.setDiffCardExpanded('a5', 'src/blocker.ts', true);

    // Tall scrollback: the run's row leaves the virtualizer's buffer
    // entirely, so its collapse can only be an engine-side estimate change —
    // the path with no ResizeObserver to catch a mistake.
    const lastProse = await growTail(mounted, 14, true);
    expect(runEl(scrollEl, runId)).toBeNull();
    expect(pane.activityRuns.openedLiveRunIds()).toContain(runId);

    // Same scroll-end trigger as the mounted test: no geometry may change,
    // so the release below is attributable to the gate alone.
    pane.setDiffCardExpanded('a5', 'src/blocker.ts', undefined);
    const anchor = () =>
      scrollEl.querySelector(`[data-item-id="${lastProse.id}"]`) as HTMLElement;
    const tops = await sampleTopsUntil(
      anchor,
      () => scrollEl.dispatchEvent(new Event('scroll')),
      () => pane.activityRuns.openedLiveRunIds().length === 0,
      'unmounted run to auto-collapse',
    );

    expectNoFrameMotion(tops);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(QUIET_BOTTOM.epsilonPx);

    // Scrolling back up finds a chip, not a reopened clip.
    await userScrollTo(scrollEl, 0);
    await waitFor(
      () => runEl(scrollEl, runId) !== null,
      'released run to remount as a chip',
    );
    expect(runEl(scrollEl, runId)!.dataset.collapsed).toBe('true');
    expect(clip(scrollEl)).toBeNull();
  });

  it('defers a release that becomes eligible mid-glide until the spring settles', async () => {
    const mounted = await mountHeldOpenSettledRun();
    const { pane, scrollEl, runId } = mounted;

    // One unwaited burst: the appends arm the structural spring and the
    // viewport glides down ~a thousand pixels — and that same growth is
    // what pushes the held run past the tail-distance threshold. The
    // gate's structural passes land while the glide is still running; a
    // release from one of them would reach `restoreBottomEdge` as a
    // direct write and snap the animation. The gate must stand down until
    // the settle's scrollend instead.
    for (let i = 0; i < 6; i += 1) {
      pane.upsertItem(prose(`t${i}`, AFTER_RUN_INDEX + 1 + i, THREAD_ID, true));
    }

    // Sample every frame until the hold is gone AND the viewport has been
    // at rest on the bottom for ten frames (or the budget runs out).
    const samples: { distance: number; held: boolean }[] = [];
    let quietFrames = 0;
    for (let i = 0; i < 600 && quietFrames < 10; i += 1) {
      await raf();
      const held = pane.activityRuns.openedLiveRunIds().length > 0;
      const distance = distanceToBottom(scrollEl);
      samples.push({ distance, held });
      if (!held && distance <= QUIET_BOTTOM.epsilonPx) quietFrames += 1;
      else quietFrames = 0;
    }
    expect(quietFrames).toBe(10);

    // The glide really happened — the burst was not absorbed in a single
    // frame — and no frame ever observed the hold released while the
    // viewport was still in flight. A release seen mid-flight is exactly
    // the animation-to-snap regression this test guards.
    expect(samples.some((s) => s.distance > QUIET_BOTTOM.epsilonPx)).toBe(true);
    for (const sample of samples) {
      if (!sample.held) {
        expect(sample.distance).toBeLessThanOrEqual(QUIET_BOTTOM.epsilonPx);
      }
    }
    expect(runEl(scrollEl, runId)!.dataset.collapsed).toBe('true');
  });

  it('never collapses a run the reader is looking at, and waits for them to leave', async () => {
    const mounted = await mountHeldOpenSettledRun();
    const { pane, scrollEl, runId } = mounted;

    // The reader scrolls up to the run BEFORE it is eligible, then the tail
    // grows past the threshold underneath them. Every trigger fires; the
    // gate must refuse while any part of the run is on screen.
    await userScrollTo(scrollEl, 0);
    const subject = runEl(scrollEl, runId)!;
    expect(subject.getBoundingClientRect().bottom).toBeGreaterThan(
      scrollEl.getBoundingClientRect().top,
    );

    for (let i = 0; i < 10; i += 1) {
      pane.upsertItem(prose(`t${i}`, AFTER_RUN_INDEX + 1 + i, THREAD_ID, true));
    }
    // Quiet, but NOT at the bottom: the reader is parked on the run, and an
    // escaped controller must not be scrolled by the growth.
    for (let i = 0; i < 30; i += 1) await raf();
    expect(runEl(scrollEl, runId)!.dataset.collapsed).toBe('false');
    expect(pane.activityRuns.openedLiveRunIds()).toContain(runId);

    // Leaving is what licenses the release: back at the bottom, the run is
    // off-screen and far from the tail, and the scroll-end trigger alone —
    // no structural nudge — must be enough.
    await userScrollTo(scrollEl, scrollEl.scrollHeight);
    await waitForQuietBottom(scrollEl, 'return to bottom', QUIET_BOTTOM);
    await waitFor(
      () => pane.activityRuns.openedLiveRunIds().length === 0,
      'release after the reader moves on',
    );
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(QUIET_BOTTOM.epsilonPx);
  });
});
